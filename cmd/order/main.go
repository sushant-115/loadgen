package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/loadgen/internal/sysstate"
	"github.com/loadgen/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "order-service"

// Order represents an order record.
type Order struct {
	OrderID   string      `json:"order_id"`
	UserID    string      `json:"user_id"`
	Items     []OrderItem `json:"items"`
	Total     float64     `json:"total"`
	Status    string      `json:"status"`
	CreatedAt string      `json:"created_at"`
}

// OrderItem represents a single line item in an order.
type OrderItem struct {
	ProductID string  `json:"product_id"`
	Name      string  `json:"name"`
	Quantity  int     `json:"quantity"`
	Price     float64 `json:"price"`
}

var (
	db                = platform.NewDB()
	cache             = platform.NewCache()
	queue             = platform.NewQueue()
	tracer            trace.Tracer
	logger            *slog.Logger
	paymentServiceURL string
)

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	paymentServiceURL = os.Getenv("PAYMENT_SERVICE_URL")
	if paymentServiceURL == "" {
		paymentServiceURL = "http://payment-service:8084"
	}

	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		logger.Error("failed to initialise telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdown()

	tracer = otel.Tracer(serviceName)

	// Seed some mock orders.
	seedOrders()

	mux := http.NewServeMux()
	mux.HandleFunc("/orders", ordersHandler)
	mux.HandleFunc("/orders/", orderByIDHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/metrics", telemetry.PrometheusHandler())

	chaos.RegisterChaosEndpoints(mux)
	sysstate.RegisterEndpoints(mux)

	handler := middleware.Chain(serviceName, logger, mux)

	srv := &http.Server{
		Addr:    ":8083",
		Handler: handler,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		logger.Info("starting order-service", "port", 8083)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down order-service")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}
}

func seedOrders() {
	ctx := context.Background()
	orders := []Order{
		{
			OrderID: "ORD-001", UserID: "1",
			Items:     []OrderItem{{ProductID: "P100", Name: "Widget", Quantity: 2, Price: 19.99}},
			Total:     39.98,
			Status:    "completed",
			CreatedAt: "2025-06-01T08:00:00Z",
		},
		{
			OrderID: "ORD-002", UserID: "2",
			Items:     []OrderItem{{ProductID: "P200", Name: "Gadget", Quantity: 1, Price: 49.99}},
			Total:     49.99,
			Status:    "completed",
			CreatedAt: "2025-06-15T12:30:00Z",
		},
	}
	for _, o := range orders {
		_ = db.Insert(ctx, "orders", o.OrderID, o)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok","service":"order-service"}`))
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	if chaos.IsActive(chaos.LatencyInjection) {
		delay := time.Duration(float64(200*time.Millisecond) * chaos.GetIntensity(chaos.LatencyInjection))
		time.Sleep(delay)
	}
	switch r.Method {
	case http.MethodGet:
		listOrders(w, r)
	case http.MethodPost:
		createOrder(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func orderByIDHandler(w http.ResponseWriter, r *http.Request) {
	if chaos.IsActive(chaos.LatencyInjection) {
		delay := time.Duration(float64(200*time.Millisecond) * chaos.GetIntensity(chaos.LatencyInjection))
		time.Sleep(delay)
	}
	id := strings.TrimPrefix(r.URL.Path, "/orders/")
	if id == "" {
		http.Error(w, `{"error":"missing order id"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		getOrder(w, r, id)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// CRUD operations
// ---------------------------------------------------------------------------

func listOrders(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "listOrders")
	defer span.End()

	rows, err := db.List(ctx, "orders")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"failed to list orders"}`, http.StatusInternalServerError)
		return
	}

	logger.InfoContext(ctx, "listed orders", "count", len(rows),
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusOK, rows)
}

func getOrder(w http.ResponseWriter, r *http.Request, id string) {
	ctx, span := tracer.Start(r.Context(), "getOrder",
		trace.WithAttributes(attribute.String("order.id", id)))
	defer span.End()

	// Cache-aside: check cache first.
	cacheKey := "order:" + id
	if data, ok := cache.Get(ctx, cacheKey); ok {
		logger.InfoContext(ctx, "cache hit for order", "order_id", id,
			"trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	// Cache miss - query DB.
	var order Order
	if err := db.Get(ctx, "orders", id, &order); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"order not found"}`, http.StatusNotFound)
		return
	}

	// Populate cache.
	raw, _ := json.Marshal(order)
	cache.Set(ctx, cacheKey, raw)

	logger.InfoContext(ctx, "fetched order", "order_id", id,
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusOK, order)
}

func createOrder(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "createOrder")
	defer span.End()

	var input struct {
		UserID string      `json:"user_id"`
		Items  []OrderItem `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		span.RecordError(err)
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Compute total.
	var total float64
	for _, item := range input.Items {
		total += float64(item.Quantity) * item.Price
	}

	order := Order{
		OrderID:   fmt.Sprintf("ORD-%d", 10000+rand.Intn(90000)),
		UserID:    input.UserID,
		Items:     input.Items,
		Total:     total,
		Status:    "pending",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	span.SetAttributes(
		attribute.String("order.id", order.OrderID),
		attribute.String("order.user_id", order.UserID),
		attribute.Float64("order.total", order.Total),
	)

	// Store order in DB.
	if err := db.Insert(ctx, "orders", order.OrderID, order); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"failed to store order"}`, http.StatusInternalServerError)
		return
	}

	// Call payment service with retry logic.
	paymentErr := callPaymentService(ctx, order)
	if paymentErr != nil {
		order.Status = "payment_failed"
		_ = db.Update(ctx, "orders", order.OrderID, order)
		span.RecordError(paymentErr)
		span.SetStatus(codes.Error, paymentErr.Error())

		logger.ErrorContext(ctx, "payment failed", "order_id", order.OrderID, "error", paymentErr,
			"trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String())

		http.Error(w, `{"error":"payment processing failed"}`, http.StatusPaymentRequired)
		return
	}

	order.Status = "confirmed"
	_ = db.Update(ctx, "orders", order.OrderID, order)

	// Publish order.created event to queue.
	if err := queue.Publish(ctx, "order.created", order); err != nil {
		logger.WarnContext(ctx, "failed to publish order.created event",
			"order_id", order.OrderID, "error", err,
			"trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String())
		// Non-fatal: order was already confirmed.
	}

	logger.InfoContext(ctx, "created order", "order_id", order.OrderID, "total", order.Total,
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusCreated, order)
}

// ---------------------------------------------------------------------------
// Payment service call with retry
// ---------------------------------------------------------------------------

func callPaymentService(ctx context.Context, order Order) error {
	ctx, span := tracer.Start(ctx, "callPaymentService",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("peer.service", "payment-service"),
			attribute.String("order.id", order.OrderID),
			attribute.Float64("order.total", order.Total),
		))
	defer span.End()

	payload, _ := json.Marshal(map[string]any{
		"order_id": order.OrderID,
		"amount":   order.Total,
		"user_id":  order.UserID,
	})

	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		span.AddEvent("payment_attempt", trace.WithAttributes(
			attribute.Int("attempt", attempt),
		))

		err := doPaymentRequest(ctx, payload, attempt)
		if err == nil {
			return nil
		}

		lastErr = err
		logFields := []any{
			"attempt", attempt,
			"max_retries", maxRetries,
			"error", err,
			"order_id", order.OrderID,
			"trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String(),
		}
		if attempt > 1 && sysstate.IsActive(sysstate.FaultRetryStorm) {
			logFields = append(logFields, "retry_amplification", true,
				"diagnostic_hint", "high retry rate amplifying payment load — check payment QPS vs order QPS ratio")
		}
		logger.WarnContext(ctx, "payment call failed, retrying", logFields...)

		if attempt < maxRetries {
			// Exponential backoff: 100ms, 200ms.
			backoff := time.Duration(attempt*100) * time.Millisecond
			time.Sleep(backoff)
		}
	}

	span.RecordError(lastErr)
	span.SetStatus(codes.Error, lastErr.Error())
	return fmt.Errorf("payment failed after %d attempts: %w", maxRetries, lastErr)
}

func doPaymentRequest(ctx context.Context, payload []byte, attempt int) error {
	url := paymentServiceURL + "/process"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Carry retry attempt number so payment service can tag its own spans.
	if attempt > 1 {
		req.Header.Set("X-Retry-Attempt", fmt.Sprintf("%d", attempt))
	}

	// Propagate trace context to downstream service.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("payment request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("payment service returned %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
