// Package middleware provides HTTP middleware for tracing, metrics, and
// structured logging used by all loadgen microservices.
package middleware

import (
	"log/slog"
	"net/http"
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
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		chaosActive, _, _ := chaos.ActiveMetadata()
		attrs := attribute.NewSet(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", r.URL.Path),
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

// Chain applies middleware in order: Logging -> Metrics -> Tracing (outermost
// first so tracing context is available to inner layers).
func Chain(serviceName string, logger *slog.Logger, handler http.Handler) http.Handler {
	return Logging(logger, Metrics(Tracing(serviceName, handler)))
}
