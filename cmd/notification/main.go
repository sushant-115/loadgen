// Notification Worker – consumes order.created events and sends notifications.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loadgen/internal/chaos"
	"github.com/loadgen/internal/distribution"
	"github.com/loadgen/internal/platform"
	"github.com/loadgen/internal/sysstate"
	"github.com/loadgen/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "notification-worker"

var (
	tracer trace.Tracer
	db     *platform.DB
	queue  *platform.Queue

	// Custom metrics.
	notifSentTotal    metric.Int64Counter
	notifFailedTotal  metric.Int64Counter
	notifProcessDur   metric.Float64Histogram

	// Stats tracking.
	stats Stats
)

// Stats tracks processing statistics exposed via GET /stats.
type Stats struct {
	mu            sync.Mutex
	Total         int64   `json:"total"`
	Success       int64   `json:"success"`
	Failed        int64   `json:"failed"`
	TotalDuration float64 `json:"-"`
}

func (s *Stats) record(success bool, dur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Total++
	if success {
		s.Success++
	} else {
		s.Failed++
	}
	s.TotalDuration += dur.Seconds()
}

func (s *Stats) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	avg := 0.0
	if s.Total > 0 {
		avg = s.TotalDuration / float64(s.Total)
	}
	return map[string]any{
		"total":               s.Total,
		"success":             s.Success,
		"failed":              s.Failed,
		"avg_duration_seconds": avg,
	}
}

type notificationRecord struct {
	ID        string `json:"id"`
	OrderID   string `json:"order_id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type orderEvent struct {
	OrderID string `json:"order_id"`
	UserID  string `json:"user_id"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		slog.Error("telemetry init failed", "error", err)
		os.Exit(1)
	}
	defer shutdown()

	tracer = otel.Tracer(serviceName)
	db = platform.SharedDB
	queue = platform.SharedQueue

	m := telemetry.Meter(serviceName)
	notifSentTotal, err = m.Int64Counter("notifications_sent_total",
		metric.WithDescription("Total notifications sent"),
	)
	if err != nil {
		slog.Error("metric init failed", "error", err)
		os.Exit(1)
	}
	notifFailedTotal, err = m.Int64Counter("notifications_failed_total",
		metric.WithDescription("Total notifications failed"),
	)
	if err != nil {
		slog.Error("metric init failed", "error", err)
		os.Exit(1)
	}
	notifProcessDur, err = m.Float64Histogram("notification_processing_duration_seconds",
		metric.WithDescription("Notification processing duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2, 5),
	)
	if err != nil {
		slog.Error("metric init failed", "error", err)
		os.Exit(1)
	}

	// HTTP server for health, metrics, stats, and chaos.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", telemetry.PrometheusHandler())
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats.snapshot())
	})
	chaos.RegisterChaosEndpoints(mux)

	srv := &http.Server{
		Addr:    ":8085",
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start queue consumer.
	var workerRunning atomic.Bool
	workerRunning.Store(true)
	cancelSub := queue.Subscribe(ctx, "order.created", handleOrderCreated)

	go func() {
		slog.Info("notification-worker HTTP starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	slog.Info("notification-worker consuming from order.created")

	<-ctx.Done()
	slog.Info("shutting down notification-worker")
	workerRunning.Store(false)
	cancelSub()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func handleOrderCreated(ctx context.Context, msg platform.Message) error {
	// Restore the distributed trace context injected by the order-service
	// publisher so this consumer span becomes a child of the order span,
	// completing the full traffic → gateway → order → payment → notification
	// trace tree.
	if len(msg.TraceCarrier) > 0 {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(msg.TraceCarrier))
	}
	ctx, span := tracer.Start(ctx, "process_notification",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("queue.topic", msg.Topic)),
	)
	defer span.End()

	var evt orderEvent
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal order event", "error", err)
		return err
	}

	// Pick a notification type.
	types := []string{"email", "sms", "push"}
	notifType := types[rand.Intn(len(types))]

	span.SetAttributes(
		attribute.String("notification.type", notifType),
		attribute.String("notification.order_id", evt.OrderID),
		attribute.String("notification.user_id", evt.UserID),
	)

	slog.InfoContext(ctx, "processing notification",
		"order_id", evt.OrderID,
		"type", notifType,
	)

	// Inject chaos latency.
	if chaos.IsActive(chaos.LatencyInjection) {
		delay := time.Duration(float64(500*time.Millisecond) * chaos.GetIntensity(chaos.LatencyInjection))
		time.Sleep(delay)
	}

	// Processing latency scales with overall system health.
	health := sysstate.HealthScore()
	start := time.Now()
	time.Sleep(distribution.ScaledDuration(150, 0.8, health))

	// Failure rate scales from ~3% (healthy) to ~30% (degraded).
	success := rand.Float64() >= sysstate.ScaledErrorRate(0.03, 0.30, health)
	dur := time.Since(start)
	status := "sent"

	if !success || chaos.IsActive(chaos.ErrorInjection) {
		success = false
		status = "failed"
		slog.ErrorContext(ctx, "notification failed",
			"order_id", evt.OrderID,
			"type", notifType,
		)
		span.SetAttributes(attribute.String("notification.status", "failed"))
		notifFailedTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("notification.type", notifType)),
		)
	} else {
		slog.InfoContext(ctx, "notification sent",
			"order_id", evt.OrderID,
			"type", notifType,
			"duration_ms", dur.Milliseconds(),
		)
		span.SetAttributes(attribute.String("notification.status", "sent"))
		notifSentTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("notification.type", notifType)),
		)
	}

	notifProcessDur.Record(ctx, dur.Seconds(),
		metric.WithAttributes(attribute.String("notification.type", notifType)),
	)

	stats.record(success, dur)

	// Persist notification record.
	rec := notificationRecord{
		ID:        fmt.Sprintf("notif_%s", randomID(12)),
		OrderID:   evt.OrderID,
		Type:      notifType,
		Status:    status,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := db.Insert(ctx, "notifications", rec.ID, rec); err != nil {
		slog.ErrorContext(ctx, "db insert failed", "error", err)
	}

	return nil
}

func randomID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}
