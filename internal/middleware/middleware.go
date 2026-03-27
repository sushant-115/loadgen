// Package middleware provides HTTP middleware for tracing, metrics, and
// structured logging used by all loadgen microservices.
package middleware

import (
	"log/slog"
	"net/http"
	"time"

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
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.String()),
				attribute.String("http.target", r.URL.Path),
			))
		defer span.End()

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.status_code", rw.statusCode))
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
		attrs := attribute.NewSet(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", r.URL.Path),
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

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", sc.TraceID().String(),
			"span_id", sc.SpanID().String(),
		)
	})
}

// Chain applies middleware in order: Logging -> Metrics -> Tracing (outermost
// first so tracing context is available to inner layers).
func Chain(serviceName string, logger *slog.Logger, handler http.Handler) http.Handler {
	return Logging(logger, Metrics(Tracing(serviceName, handler)))
}
