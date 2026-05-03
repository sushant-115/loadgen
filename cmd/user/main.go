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
	"github.com/loadgen/internal/sysstate"
	"github.com/loadgen/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "user-service"

// User represents a user record.
type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

var (
	db     = platform.NewDB()
	cache  = platform.NewCache()
	tracer trace.Tracer
	logger *slog.Logger
)

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		logger.Error("failed to initialise telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdown()

	tracer = otel.Tracer(serviceName)

	// Seed some mock users.
	seedUsers()

	mux := http.NewServeMux()
	mux.HandleFunc("/users", usersHandler)
	mux.HandleFunc("/users/", userByIDHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/metrics", telemetry.PrometheusHandler())

	chaos.RegisterChaosEndpoints(mux)

	handler := middleware.Chain(serviceName, logger, mux)

	srv := &http.Server{
		Addr:    ":8082",
		Handler: handler,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		logger.Info("starting user-service", "port", 8082)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down user-service")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}
}

func seedUsers() {
	ctx := context.Background()
	users := []User{
		{ID: "1", Name: "Alice Johnson", Email: "alice@example.com", CreatedAt: "2025-01-15T10:30:00Z"},
		{ID: "2", Name: "Bob Smith", Email: "bob@example.com", CreatedAt: "2025-02-20T14:45:00Z"},
		{ID: "3", Name: "Charlie Brown", Email: "charlie@example.com", CreatedAt: "2025-03-10T09:00:00Z"},
	}
	for _, u := range users {
		_ = db.Insert(ctx, "users", u.ID, u)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok","service":"user-service"}`))
}

func usersHandler(w http.ResponseWriter, r *http.Request) {
	if chaos.IsActive(chaos.LatencyInjection) {
		delay := time.Duration(float64(200*time.Millisecond) * chaos.GetIntensity(chaos.LatencyInjection))
		time.Sleep(delay)
	}
	switch r.Method {
	case http.MethodGet:
		listUsers(w, r)
	case http.MethodPost:
		createUser(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func userByIDHandler(w http.ResponseWriter, r *http.Request) {
	if chaos.IsActive(chaos.LatencyInjection) {
		delay := time.Duration(float64(200*time.Millisecond) * chaos.GetIntensity(chaos.LatencyInjection))
		time.Sleep(delay)
	}
	id := strings.TrimPrefix(r.URL.Path, "/users/")
	if id == "" {
		http.Error(w, `{"error":"missing user id"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		getUser(w, r, id)
	case http.MethodPut:
		updateUser(w, r, id)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// CRUD operations
// ---------------------------------------------------------------------------

func listUsers(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "listUsers")
	defer span.End()

	if simulateDBError(span) {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	rows, err := db.List(ctx, "users")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"failed to list users"}`, http.StatusInternalServerError)
		return
	}

	logger.InfoContext(ctx, "listed users", "count", len(rows),
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusOK, rows)
}

func getUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx, span := tracer.Start(r.Context(), "getUser",
		trace.WithAttributes(attribute.String("user.id", id)))
	defer span.End()

	if simulateDBError(span) {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	// Cache-aside: check cache first.
	cacheKey := "user:" + id
	if data, ok := cache.Get(ctx, cacheKey); ok {
		logger.InfoContext(ctx, "cache hit for user", "user_id", id,
			"trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	// Cache miss – query DB.
	var user User
	if err := db.Get(ctx, "users", id, &user); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	// Populate cache.
	raw, _ := json.Marshal(user)
	cache.Set(ctx, cacheKey, raw)

	logger.InfoContext(ctx, "fetched user", "user_id", id,
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusOK, user)
}

func createUser(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "createUser")
	defer span.End()

	if simulateDBError(span) {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	var input struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		span.RecordError(err)
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	user := User{
		ID:        fmt.Sprintf("%d", 1000+rand.Intn(9000)),
		Name:      input.Name,
		Email:     input.Email,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	span.SetAttributes(attribute.String("user.id", user.ID))

	if err := db.Insert(ctx, "users", user.ID, user); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"failed to create user"}`, http.StatusInternalServerError)
		return
	}

	logger.InfoContext(ctx, "created user", "user_id", user.ID, "email", user.Email,
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusCreated, user)
}

func updateUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx, span := tracer.Start(r.Context(), "updateUser",
		trace.WithAttributes(attribute.String("user.id", id)))
	defer span.End()

	if simulateDBError(span) {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	// Read existing user.
	var existing User
	if err := db.Get(ctx, "users", id, &existing); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	var input struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		span.RecordError(err)
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if input.Name != "" {
		existing.Name = input.Name
	}
	if input.Email != "" {
		existing.Email = input.Email
	}

	if err := db.Update(ctx, "users", id, existing); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, `{"error":"failed to update user"}`, http.StatusInternalServerError)
		return
	}

	// Invalidate cache.
	cache.Delete(ctx, "user:"+id)

	logger.InfoContext(ctx, "updated user", "user_id", id,
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String())

	writeJSON(w, http.StatusOK, existing)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// simulateDBError returns true at a rate that scales with DB contention health.
// At full health the rate is ~2%; during a db_contention incident it climbs
// to ~40%, matching the behaviour visible in traces and metrics.
func simulateDBError(span trace.Span) bool {
	dbHealth := sysstate.FaultHealth(sysstate.FaultDBContention)
	errorRate := sysstate.ScaledErrorRate(0.02, 0.40, dbHealth)
	if rand.Float64() < errorRate {
		err := fmt.Errorf("database connection error: timeout after 30s")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.Error("simulated DB error", "error", err, "db_health", dbHealth)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
