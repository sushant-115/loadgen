// Payment Service – processes payments for orders.
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
	"syscall"
	"time"

	"github.com/loadgen/internal/chaos"
	"github.com/loadgen/internal/middleware"
	"github.com/loadgen/internal/platform"
	"github.com/loadgen/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "payment-service"

// Custom metrics.
var (
	paymentAmountTotal     metric.Float64Counter
	paymentProcessDuration metric.Float64Histogram
	tracer                 trace.Tracer
	db                     *platform.DB
)

// ---------- request / response types ----------

type processRequest struct {
	OrderID  string  `json:"order_id"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Method   string  `json:"method"`
}

type processResponse struct {
	PaymentID     string `json:"payment_id"`
	Status        string `json:"status"`
	TransactionID string `json:"transaction_id"`
}

type paymentRecord struct {
	PaymentID     string  `json:"payment_id"`
	OrderID       string  `json:"order_id"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	Method        string  `json:"method"`
	Status        string  `json:"status"`
	TransactionID string  `json:"transaction_id"`
	CreatedAt     string  `json:"created_at"`
}

func main() {
	// Structured JSON logging.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// Telemetry.
	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		slog.Error("telemetry init failed", "error", err)
		os.Exit(1)
	}
	defer shutdown()

	tracer = otel.Tracer(serviceName)
	db = platform.SharedDB

	// Register custom metrics.
	m := telemetry.Meter(serviceName)
	paymentAmountTotal, err = m.Float64Counter("payment_amount_total",
		metric.WithDescription("Total payment amount processed"),
	)
	if err != nil {
		slog.Error("metric init failed", "error", err)
		os.Exit(1)
	}
	paymentProcessDuration, err = m.Float64Histogram("payment_processing_duration_seconds",
		metric.WithDescription("Payment processing duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.2, 0.5, 1, 2, 5),
	)
	if err != nil {
		slog.Error("metric init failed", "error", err)
		os.Exit(1)
	}

	// Router.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /process", handleProcess)
	mux.HandleFunc("GET /payments/{id}", handleGetPayment)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", telemetry.PrometheusHandler())
	chaos.RegisterRoutes(mux)

	handler := middleware.Chain(mux, middleware.Tracing(serviceName))

	srv := &http.Server{
		Addr:    ":8084",
		Handler: handler,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("payment-service starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down payment-service")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// ---------- handlers ----------

func handleProcess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req processRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(ctx, "process_payment",
		trace.WithAttributes(
			attribute.String("payment.order_id", req.OrderID),
			attribute.Float64("payment.amount", req.Amount),
			attribute.String("payment.currency", req.Currency),
			attribute.String("payment.method", req.Method),
		),
	)
	defer span.End()

	// Inject chaos.
	chaos.Get().InjectLatency()

	paymentID := fmt.Sprintf("pay_%s", randomID(12))
	txnID := fmt.Sprintf("txn_%s", randomID(16))

	slog.InfoContext(ctx, "processing payment",
		"payment_id", paymentID,
		"order_id", req.OrderID,
		"amount", req.Amount,
		"currency", req.Currency,
		"method", req.Method,
	)

	start := time.Now()

	// Determine outcome.
	roll := rand.Float64()
	var status string
	switch {
	case roll < 0.01:
		// ~1% timeout simulation.
		slog.WarnContext(ctx, "payment timeout", "payment_id", paymentID)
		span.SetAttributes(attribute.String("payment.status", "timeout"))
		time.Sleep(5 * time.Second)
		status = "failed"
	case roll < 0.06:
		// ~5% failure.
		reasons := []string{"insufficient_funds", "card_declined", "expired_card", "processing_error"}
		reason := reasons[rand.Intn(len(reasons))]
		slog.ErrorContext(ctx, "payment failed",
			"payment_id", paymentID,
			"reason", reason,
		)
		span.SetAttributes(
			attribute.String("payment.status", "failed"),
			attribute.String("payment.failure_reason", reason),
		)
		simulateLatency(50, 200)
		status = "failed"
	default:
		// Success.
		simulateLatency(50, 200)
		status = "completed"
		slog.InfoContext(ctx, "payment completed",
			"payment_id", paymentID,
			"transaction_id", txnID,
		)
		span.SetAttributes(attribute.String("payment.status", "completed"))
	}

	duration := time.Since(start).Seconds()
	paymentProcessDuration.Record(ctx, duration,
		metric.WithAttributes(
			attribute.String("payment.method", req.Method),
			attribute.String("payment.status", status),
		),
	)
	if status == "completed" {
		paymentAmountTotal.Add(ctx, req.Amount,
			metric.WithAttributes(
				attribute.String("payment.currency", req.Currency),
				attribute.String("payment.method", req.Method),
			),
		)
	}

	// Persist record.
	rec := paymentRecord{
		PaymentID:     paymentID,
		OrderID:       req.OrderID,
		Amount:        req.Amount,
		Currency:      req.Currency,
		Method:        req.Method,
		Status:        status,
		TransactionID: txnID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := db.Insert(ctx, "payments", paymentID, rec); err != nil {
		slog.ErrorContext(ctx, "db insert failed", "error", err)
	}

	resp := processResponse{
		PaymentID:     paymentID,
		Status:        status,
		TransactionID: txnID,
	}

	statusCode := http.StatusOK
	if status == "failed" {
		statusCode = http.StatusPaymentRequired
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(resp)
}

func handleGetPayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	ctx, span := tracer.Start(ctx, "get_payment",
		trace.WithAttributes(attribute.String("payment.id", id)),
	)
	defer span.End()

	var rec paymentRecord
	if err := db.Get(ctx, "payments", id, &rec); err != nil {
		http.Error(w, `{"error":"payment not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rec)
}

// ---------- helpers ----------

func simulateLatency(minMs, maxMs int) {
	time.Sleep(time.Duration(minMs+rand.Intn(maxMs-minMs)) * time.Millisecond)
}

func randomID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}
