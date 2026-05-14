package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loadgen/internal/chaos"
	"github.com/loadgen/internal/dimensions"
	"github.com/loadgen/internal/middleware"
	"github.com/loadgen/internal/sysstate"
	"github.com/loadgen/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "api-gateway"

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

	// Read downstream service URLs from env (with defaults).
	authURL := envOrDefault("AUTH_SERVICE_URL", "http://auth-service:8081")
	userURL := envOrDefault("USER_SERVICE_URL", "http://user-service:8082")
	orderURL := envOrDefault("ORDER_SERVICE_URL", "http://order-service:8083")

	// Build reverse proxies.
	authProxy := newProxy(authURL)
	userProxy := newProxy(userURL)
	orderProxy := newProxy(orderURL)

	// Rate-limit state: simple sliding-window counter.
	var requestCount atomic.Int64
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			requestCount.Store(0)
		}
	}()

	tracer := otel.Tracer(serviceName)

	mux := http.NewServeMux()

	// Health & metrics endpoints.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok","service":"api-gateway"}`)
	})
	mux.Handle("GET /metrics", telemetry.PrometheusHandler())

	// Chaos and anomaly injection endpoints.
	chaos.RegisterChaosEndpoints(mux)
	sysstate.RegisterEndpoints(mux)

	// Proxy routes.
	proxyHandler := func(pathPrefix, stripPrefix, targetBase, peerService string, proxy *httputil.ReverseProxy) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Rate limiting.
			current := requestCount.Add(1)
			if current > 1000 {
				slog.WarnContext(r.Context(), "rate limit exceeded", "rps", current)
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}

			ctx := r.Context()

			// Add X-Request-ID if not present.
			reqID := r.Header.Get("X-Request-ID")
			if reqID == "" {
				reqID = fmt.Sprintf("gw-%d", time.Now().UnixNano())
				r.Header.Set("X-Request-ID", reqID)
			}

			// Carry business-dimension tags onto the proxy span so the cross-service
			// trace view shows tenant / region / customer_tier / plan / gateway
			// alongside the standard http/peer attributes.
			dims := dimensions.FromHeaders(r)

			// Start a proxy span marking the downstream peer service.
			ctx, span := tracer.Start(ctx, "gateway.proxy",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(
					attribute.String("proxy.target", targetBase),
					attribute.String("http.path", r.URL.Path),
					attribute.String("request.id", reqID),
					attribute.String("peer.service", peerService),
					attribute.String(dimensions.AttrTenantID, dims.TenantID),
					attribute.String(dimensions.AttrRegion, dims.Region),
					attribute.String(dimensions.AttrCustomerTier, dims.CustomerTier),
					attribute.String(dimensions.AttrPlan, dims.Plan),
					attribute.String(dimensions.AttrPaymentGateway, dims.PaymentGateway),
				),
			)
			defer span.End()

			// Log the proxied request.
			traceID := span.SpanContext().TraceID().String()
			slog.InfoContext(ctx, "proxying request",
				"method", r.Method,
				"path", r.URL.Path,
				"target", targetBase,
				"trace_id", traceID,
				"request_id", reqID,
			)

			// Rewrite path: strip the API prefix so downstream sees its own routes.
			r.URL.Path = strings.TrimPrefix(r.URL.Path, stripPrefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}

			// Propagate trace context to downstream.
			r = r.WithContext(ctx)

			proxy.ServeHTTP(w, r)
		}
	}

	// Auth routes.
	mux.HandleFunc("POST /api/auth/login", proxyHandler("/api/auth/login", "/api/auth", authURL, "auth-service", authProxy))
	mux.HandleFunc("POST /api/auth/verify", proxyHandler("/api/auth/verify", "/api/auth", authURL, "auth-service", authProxy))

	// User routes.
	mux.HandleFunc("POST /api/users", proxyHandler("/api/users", "/api", userURL, "user-service", userProxy))
	mux.HandleFunc("GET /api/users/", proxyHandler("/api/users/", "/api", userURL, "user-service", userProxy))
	mux.HandleFunc("GET /api/users", proxyHandler("/api/users", "/api", userURL, "user-service", userProxy))
	mux.HandleFunc("POST /api/users/{id}/upgrade", proxyHandler("/api/users/", "/api", userURL, "user-service", userProxy))

	// Order routes.
	mux.HandleFunc("POST /api/orders", proxyHandler("/api/orders", "/api", orderURL, "order-service", orderProxy))
	mux.HandleFunc("GET /api/orders/", proxyHandler("/api/orders/", "/api", orderURL, "order-service", orderProxy))
	mux.HandleFunc("GET /api/orders", proxyHandler("/api/orders", "/api", orderURL, "order-service", orderProxy))

	// Apply middleware chain.
	handler := middleware.Chain(serviceName, slog.Default(), mux)

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting api-gateway", "port", 8080)
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
	slog.Info("api-gateway stopped")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newProxy(target string) *httputil.ReverseProxy {
	u, err := url.Parse(target)
	if err != nil {
		slog.Error("failed to parse proxy target", "target", target, "error", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)

	// Inject trace context into outgoing requests.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.ErrorContext(r.Context(), "proxy error",
			"target", target,
			"path", r.URL.Path,
			"error", err,
		)
		http.Error(w, `{"error":"upstream service unavailable"}`, http.StatusBadGateway)
	}
	return proxy
}
