package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
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

const serviceName = "auth-service"

var (
	db     *platform.DB
	cache  *platform.Cache
	tracer trace.Tracer
)

func main() {
	// Structured JSON logging.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Initialise telemetry.
	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		slog.Error("failed to init telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdown()

	// Platform simulators.
	db = platform.NewDB()
	cache = platform.NewCache()
	tracer = otel.Tracer(serviceName)

	// Seed some users in the simulated DB for credential lookups.
	seedUsers()

	mux := http.NewServeMux()

	// Health & metrics.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok","service":"auth-service"}`)
	})
	mux.Handle("GET /metrics", telemetry.PrometheusHandler())

	// Chaos endpoints.
	chaos.RegisterChaosEndpoints(mux)

	// Auth endpoints.
	mux.HandleFunc("POST /login", handleLogin)
	mux.HandleFunc("POST /verify", handleVerify)

	// Apply middleware chain.
	handler := middleware.Chain(serviceName, slog.Default(), mux)

	srv := &http.Server{
		Addr:         ":8081",
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting auth-service", "port", 8081)
		errCh <- srv.ListenAndServe()
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	slog.Info("auth-service stopped")
}

// --------------------------------------------------------------------------
// Data types
// --------------------------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
}

type verifyRequest struct {
	Token string `json:"token"`
}

type verifyResponse struct {
	Valid  bool   `json:"valid"`
	UserID string `json:"user_id"`
}

type userRecord struct {
	UserID       string `json:"user_id"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// --------------------------------------------------------------------------
// Handlers
// --------------------------------------------------------------------------

func handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx, span := tracer.Start(ctx, "auth.login",
		trace.WithAttributes(attribute.String("auth.operation", "login")),
	)
	defer span.End()

	// Internal error rate scales from ~1% (healthy) to ~20% during auth degradation.
	if rand.Float64() < sysstate.ScaledErrorRate(0.01, 0.20, sysstate.FaultHealth(sysstate.FaultAuthService)) {
		span.SetStatus(codes.Error, "internal error")
		span.RecordError(fmt.Errorf("simulated internal error"))
		slog.ErrorContext(ctx, "internal error during login", "error", "simulated")
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("auth.username", req.Username))

	// Simulate credential verification via DB.
	ctx, err := verifyCredentials(ctx, req.Username, req.Password)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		slog.WarnContext(ctx, "login failed",
			"username", req.Username,
			"reason", err.Error(),
		)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	// Wrong-password rate scales from ~5% (healthy) to ~35% during auth degradation.
	if rand.Float64() < sysstate.ScaledErrorRate(0.05, 0.35, sysstate.FaultHealth(sysstate.FaultAuthService)) {
		span.SetStatus(codes.Error, "simulated wrong password")
		slog.WarnContext(ctx, "login failed",
			"username", req.Username,
			"reason", "simulated wrong password",
		)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	// Generate a token.
	userID := fmt.Sprintf("user-%s", req.Username)
	token := generateToken(req.Username)

	// Cache the token.
	ctx, cacheErr := cacheToken(ctx, token, userID)
	if cacheErr != nil {
		slog.WarnContext(ctx, "failed to cache token", "error", cacheErr)
		// Non-fatal: continue anyway.
	}

	slog.InfoContext(ctx, "login success",
		"username", req.Username,
		"user_id", userID,
		"trace_id", span.SpanContext().TraceID().String(),
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(loginResponse{
		Token:  token,
		UserID: userID,
	})
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx, span := tracer.Start(ctx, "auth.verify",
		trace.WithAttributes(attribute.String("auth.operation", "verify")),
	)
	defer span.End()

	// Internal error rate scales from ~1% (healthy) to ~20% during auth degradation.
	if rand.Float64() < sysstate.ScaledErrorRate(0.01, 0.20, sysstate.FaultHealth(sysstate.FaultAuthService)) {
		span.SetStatus(codes.Error, "internal error")
		span.RecordError(fmt.Errorf("simulated internal error"))
		slog.ErrorContext(ctx, "internal error during verify", "error", "simulated")
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Check cache for token.
	ctx, userID, valid := lookupToken(ctx, req.Token)
	span.SetAttributes(
		attribute.Bool("auth.token_valid", valid),
		attribute.String("auth.token_prefix", truncateToken(req.Token)),
	)

	if !valid {
		slog.WarnContext(ctx, "token invalid",
			"token_prefix", truncateToken(req.Token),
			"trace_id", span.SpanContext().TraceID().String(),
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(verifyResponse{Valid: false})
		return
	}

	slog.InfoContext(ctx, "token verified",
		"user_id", userID,
		"trace_id", span.SpanContext().TraceID().String(),
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(verifyResponse{
		Valid:  true,
		UserID: userID,
	})
}

// --------------------------------------------------------------------------
// Business logic helpers
// --------------------------------------------------------------------------

func verifyCredentials(ctx context.Context, username, password string) (context.Context, error) {
	ctx, span := tracer.Start(ctx, "auth.verify_credentials")
	defer span.End()

	// Simulate DB lookup for user credentials.
	var user userRecord
	err := db.Get(ctx, "users", username, &user)
	if err != nil {
		span.RecordError(err)
		// For loadgen, treat missing users as valid (they just haven't been seeded).
		// The simulated failure rates handle realistic error scenarios.
		slog.DebugContext(ctx, "user not in DB, proceeding with simulated auth", "username", username)
	}

	// Simulate password hash verification latency.
	time.Sleep(time.Duration(10+rand.Intn(20)) * time.Millisecond)

	return ctx, nil
}

func cacheToken(ctx context.Context, token, userID string) (context.Context, error) {
	ctx, span := tracer.Start(ctx, "auth.cache_token")
	defer span.End()

	cache.Set(ctx, "token:"+token, []byte(userID))
	return ctx, nil
}

func lookupToken(ctx context.Context, token string) (context.Context, string, bool) {
	ctx, span := tracer.Start(ctx, "auth.lookup_token")
	defer span.End()

	// Try cache first.
	data, hit := cache.Get(ctx, "token:"+token)
	if hit {
		span.SetAttributes(attribute.Bool("cache.hit", true))
		return ctx, string(data), true
	}

	span.SetAttributes(attribute.Bool("cache.hit", false))

	// Simulate a DB fallback lookup. For loadgen, tokens not in cache are
	// considered invalid ~50% of the time to produce a mix of outcomes.
	time.Sleep(time.Duration(5+rand.Intn(10)) * time.Millisecond)
	if rand.Float64() < 0.5 {
		userID := fmt.Sprintf("user-%d", rand.Intn(1000))
		// Re-cache for next time.
		cache.Set(ctx, "token:"+token, []byte(userID))
		return ctx, userID, true
	}

	return ctx, "", false
}

func generateToken(username string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", username, time.Now().UnixNano(), rand.Int63())))
	return fmt.Sprintf("%x", h[:16])
}

func truncateToken(token string) string {
	if len(token) > 8 {
		return token[:8] + "..."
	}
	return token
}

func seedUsers() {
	ctx := context.Background()
	users := []userRecord{
		{UserID: "user-alice", Username: "alice", PasswordHash: "hashed_password_alice"},
		{UserID: "user-bob", Username: "bob", PasswordHash: "hashed_password_bob"},
		{UserID: "user-charlie", Username: "charlie", PasswordHash: "hashed_password_charlie"},
		{UserID: "user-demo", Username: "demo", PasswordHash: "hashed_password_demo"},
	}
	for _, u := range users {
		if err := db.Insert(ctx, "users", u.Username, u); err != nil {
			slog.Warn("failed to seed user", "username", u.Username, "error", err)
		}
	}
	slog.Info("seeded users in simulated DB", "count", len(users))
}
