// Package telemetry sets up OpenTelemetry tracing, metrics, logs, and Prometheus
// exposition for each microservice in the loadgen project.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

var (
	// Common metrics registered once during Init.
	RequestCounter    metric.Int64Counter
	RequestDuration   metric.Float64Histogram
	ErrorCounter      metric.Int64Counter
	ActiveRequests    metric.Int64UpDownCounter

	promHandler http.Handler
)

// Init initialises OpenTelemetry tracing and metrics for the given service.
// It returns a shutdown function that should be called on process exit.
func Init(serviceName string) (shutdown func(), err error) {
	ctx := context.Background()

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "otel-collector:4317"
	}

	// Build common resource.
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithProcessRuntimeDescription(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, err
	}

	// --- Trace provider (OTLP gRPC) ---
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// --- Meter provider (OTLP gRPC + Prometheus) ---
	otlpMetricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	promExporter, err := promexporter.New()
	if err != nil {
		return nil, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(otlpMetricExporter, sdkmetric.WithInterval(10*time.Second))),
		sdkmetric.WithReader(promExporter),
	)
	otel.SetMeterProvider(mp)

	// --- Log provider (OTLP gRPC) ---
	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter, sdklog.WithExportInterval(5*time.Second))),
	)

	// Bridge slog to OTEL log provider while keeping stdout output for kubectl logs.
	otelHandler := otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(lp))
	stdoutHandler := slog.NewJSONHandler(os.Stdout, nil)
	slog.SetDefault(slog.New(&fanoutHandler{handlers: []slog.Handler{stdoutHandler, otelHandler}}))

	// Prometheus HTTP handler.
	promHandler = promhttp.Handler()

	// Register common metrics.
	m := mp.Meter(serviceName)

	RequestCounter, err = m.Int64Counter("http_requests_total",
		metric.WithDescription("Total number of HTTP requests"),
	)
	if err != nil {
		return nil, err
	}

	RequestDuration, err = m.Float64Histogram("http_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return nil, err
	}

	ErrorCounter, err = m.Int64Counter("http_errors_total",
		metric.WithDescription("Total number of HTTP errors"),
	)
	if err != nil {
		return nil, err
	}

	ActiveRequests, err = m.Int64UpDownCounter("http_active_requests",
		metric.WithDescription("Number of in-flight HTTP requests"),
	)
	if err != nil {
		return nil, err
	}

	// Shutdown tears down all providers.
	shutdown = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lp.Shutdown(ctx)
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
	}
	return shutdown, nil
}

// Tracer returns a named tracer from the global TracerProvider.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// Meter returns a named meter from the global MeterProvider.
func Meter(name string) metric.Meter {
	return otel.Meter(name)
}

// PrometheusHandler returns an http.Handler that serves the /metrics endpoint.
func PrometheusHandler() http.Handler {
	if promHandler != nil {
		return promHandler
	}
	// Fallback if Init has not been called yet.
	return promhttp.Handler()
}

// fanoutHandler sends each log record to multiple slog.Handlers.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, r.Level) {
			_ = hh.Handle(ctx, r)
		}
	}
	return nil
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		cloned[i] = hh.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: cloned}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	cloned := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		cloned[i] = hh.WithGroup(name)
	}
	return &fanoutHandler{handlers: cloned}
}
