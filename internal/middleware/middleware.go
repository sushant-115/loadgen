// Package middleware provides HTTP middleware for tracing, metrics, and
// structured logging used by all loadgen microservices.
package middleware

import (
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/loadgen/internal/chaos"
	"github.com/loadgen/internal/dimensions"
	"github.com/loadgen/internal/sysstate"
	"github.com/loadgen/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// routeBucket collapses a request path to a low-cardinality bucket
// suitable for use as a metric attribute. It returns the first path
// segment (e.g. "/orders/ORD-12345" → "/orders"). Empty path → "/".
// Without this collapse, every unique order/product/user ID in the URL
// would create a new attribute set in the OTel SDK and explode the
// number of cumulative-counter data points emitted per export tick.
func routeBucket(path string) string {
	p := strings.TrimLeft(path, "/")
	if p == "" {
		return "/"
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return "/" + p
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Tracing extracts incoming trace context and creates a server span for every
// request.
func Tracing(serviceName string, next http.Handler) http.Handler {
	tracer := otel.Tracer(serviceName)
	propagator := otel.GetTextMapPropagator()
	if propagator == nil {
		otel.SetTextMapPropagator(propagation.TraceContext{})
		propagator = otel.GetTextMapPropagator()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chaosActive, campaignID, chaosTypes := chaos.ActiveMetadata()
		dims := dimensions.FromHeaders(r)

		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.String()),
				attribute.String("http.target", r.URL.Path),
				attribute.Bool("chaos.active", chaosActive),
				attribute.String("chaos.types", chaosTypes),
				attribute.String("chaos.campaign_id", campaignID),
			))
		defer span.End()

		// Annotate every span with the live system health so anomaly detectors
		// can correlate metric/trace anomalies to the active fault scenario.
		span.SetAttributes(
			attribute.Float64("system.health_score", sysstate.HealthScore()),
			attribute.String("system.state", sysstate.CurrentStateName()),
			attribute.String("system.active_faults", sysstate.ActiveFaultNames()),
			attribute.String("infrasage_synthetic", "true"),
		)

		// Attach business-dimension attributes — only when present, so internal
		// health checks/probes don't pollute the trace stream with empty tags.
		if !dims.IsEmpty() {
			span.SetAttributes(
				attribute.String(dimensions.AttrTenantID, dims.TenantID),
				attribute.String(dimensions.AttrRegion, dims.Region),
				attribute.String(dimensions.AttrCustomerTier, dims.CustomerTier),
				attribute.String(dimensions.AttrPlan, dims.Plan),
				attribute.String(dimensions.AttrPaymentGateway, dims.PaymentGateway),
			)
		}

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.status_code", rw.statusCode))
		rw.Header().Set("X-Chaos-Active", boolToString(chaosActive))
		if campaignID != "" {
			rw.Header().Set("X-Chaos-Campaign", campaignID)
		}
		if chaosTypes != "" {
			rw.Header().Set("X-Chaos-Types", chaosTypes)
		}
		if rw.statusCode >= 400 {
			span.SetStatus(codes.Error, http.StatusText(rw.statusCode))
		}
	})
}

// Metrics records request count, duration, error count, and active requests
// using counters registered by telemetry.Init.
//
// NOTE: do NOT add the raw request path (r.URL.Path) here — the loadgen
// uses unique IDs in paths (/orders/ORD-12345, /products/PROD-42…),
// so attaching the path would create a unique attribute set per
// request and cause the OTel SDK to emit thousands of cumulative-counter
// data points per export tick (we saw 6k+ rows / 10s / metric in CH).
// Use a coarse route bucket (the first path segment) instead.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		chaosActive, _, _ := chaos.ActiveMetadata()
		attrs := attribute.NewSet(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", routeBucket(r.URL.Path)),
			attribute.Bool("chaos.active", chaosActive),
		)

		telemetry.ActiveRequests.Add(ctx, 1, metric.WithAttributeSet(attrs))
		start := time.Now()

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))

		duration := time.Since(start).Seconds()
		telemetry.ActiveRequests.Add(ctx, -1, metric.WithAttributeSet(attrs))
		telemetry.RequestCounter.Add(ctx, 1, metric.WithAttributeSet(attrs))
		telemetry.RequestDuration.Record(ctx, duration, metric.WithAttributeSet(attrs))

		if rw.statusCode >= 400 {
			telemetry.ErrorCounter.Add(ctx, 1, metric.WithAttributeSet(attrs))
		}
	})
}

// Logging emits a structured JSON log line for every request, including
// trace_id and span_id when available.
func Logging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)

		span := trace.SpanFromContext(r.Context())
		sc := span.SpanContext()
		chaosActive, campaignID, chaosTypes := chaos.ActiveMetadata()
		dims := dimensions.FromHeaders(r)
		durationMs := time.Since(start).Milliseconds()

		logArgs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", durationMs,
			"chaos_active", chaosActive,
			"chaos_types", chaosTypes,
			"chaos_campaign", campaignID,
			"trace_id", sc.TraceID().String(),
			"span_id", sc.SpanID().String(),
			"system_state", sysstate.CurrentStateName(),
			"system_health", sysstate.HealthScore(),
		}
		if !dims.IsEmpty() {
			logArgs = append(logArgs,
				"tenant_id", dims.TenantID,
				"region", dims.Region,
				"customer_tier", dims.CustomerTier,
				"plan", dims.Plan,
				"payment_gateway", dims.PaymentGateway,
			)
		}
		switch {
		case rw.statusCode >= 500:
			logger.ErrorContext(r.Context(), "request error", logArgs...)
		case rw.statusCode >= 400 || durationMs > 2000:
			logger.WarnContext(r.Context(), "request slow or failed", logArgs...)
		default:
			logger.InfoContext(r.Context(), "request", logArgs...)
		}
	})
}

func boolToString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// ChaosFaults applies the request-path v2 faults every service inherits
// through Chain:
//
//   - error_budget: a LOW-grade failure rate — effective intensity is the
//     probability a request 500s (0.02 = 2%). The slow-burn failure that
//     threshold alerts miss and error-budget detection should catch.
//   - latency_tail: only the TAIL suffers — scope_percent of requests (set
//     small, e.g. 0.02) get a large delay scaled by intensity. Moves p99
//     while barely moving the average; catches detectors that only watch
//     means.
//
// Both read effective intensity through chaos.GetIntensity, so onset
// shapes (ramp/leak/flap) and /ops remediation apply automatically.
func ChaosFaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never fault the control planes — chaos must stay steerable and
		// health checks honest-to-purpose.
		p := r.URL.Path
		if strings.HasPrefix(p, "/chaos") || strings.HasPrefix(p, "/ops") || p == "/health" || p == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if eff := chaos.GetIntensity(chaos.LatencyTail); eff > 0 {
			// GetIntensity already folded scope_percent in; treat the
			// effective value as (hit probability × severity). Split it:
			// small chance, big delay.
			if rand.Float64() < math.Min(0.10, eff) {
				delay := time.Duration(1500+rand.IntN(2500)) * time.Millisecond
				time.Sleep(delay)
			}
		}
		if eff := chaos.GetIntensity(chaos.ErrorBudget); eff > 0 {
			if rand.Float64() < eff {
				slog.Warn("request failed: upstream dependency returned malformed response",
					"chaos", true, "fault", "error_budget", "path", routeBucket(p))
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Chain applies middleware in order: Logging -> Metrics -> Tracing -> ChaosFaults
// (outermost first so tracing context is available to inner layers, and the
// injected faults are recorded by metrics/tracing like real failures).
func Chain(serviceName string, logger *slog.Logger, handler http.Handler) http.Handler {
	return Logging(logger, Metrics(Tracing(serviceName, ChaosFaults(handler))))
}
