// Traffic Generator – sends continuous HTTP traffic to the API gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loadgen/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "traffic-generator"

var tracer trace.Tracer

// ---------- config ----------

type config struct {
	TargetURL            string
	RequestsPerSecond    int
	BurstIntervalSeconds int
}

func loadConfig() config {
	c := config{
		TargetURL:            envOrDefault("TARGET_URL", "http://gateway:8080"),
		RequestsPerSecond:    envIntOrDefault("REQUESTS_PER_SECOND", 10),
		BurstIntervalSeconds: envIntOrDefault("BURST_INTERVAL_SECONDS", 300),
	}
	return c
}

// ---------- statistics ----------

type statistics struct {
	mu        sync.Mutex
	total     int64
	successes int64
	failures  int64
	latencies []time.Duration
}

var globalStats statistics

func (s *statistics) record(statusCode int, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddInt64(&s.total, 1)
	if statusCode >= 200 && statusCode < 400 {
		atomic.AddInt64(&s.successes, 1)
	} else {
		atomic.AddInt64(&s.failures, 1)
	}
	s.latencies = append(s.latencies, latency)
}

func (s *statistics) summarize() {
	s.mu.Lock()
	total := s.total
	successes := s.successes
	failures := s.failures
	latencies := make([]time.Duration, len(s.latencies))
	copy(latencies, s.latencies)
	// Reset latencies for next window.
	s.latencies = s.latencies[:0]
	s.mu.Unlock()

	if total == 0 {
		slog.Info("traffic summary: no requests in this period")
		return
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	var sum time.Duration
	for _, d := range latencies {
		sum += d
	}
	avg := sum / time.Duration(len(latencies))

	p99 := latencies[int(math.Ceil(float64(len(latencies))*0.99))-1]
	successRate := float64(successes) / float64(total) * 100

	slog.Info("traffic summary",
		"total", total,
		"success_rate_pct", fmt.Sprintf("%.1f", successRate),
		"failures", failures,
		"avg_latency_ms", avg.Milliseconds(),
		"p99_latency_ms", p99.Milliseconds(),
	)
}

// ---------- main ----------

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		slog.Error("telemetry init failed", "error", err)
		os.Exit(1)
	}
	defer shutdown()

	tracer = otel.Tracer(serviceName)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	cfg := loadConfig()
	slog.Info("traffic-generator starting",
		"target", cfg.TargetURL,
		"rps", cfg.RequestsPerSecond,
		"burst_interval_s", cfg.BurstIntervalSeconds,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 10 * time.Second}

	// Summary printer.
	summaryTicker := time.NewTicker(30 * time.Second)
	defer summaryTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-summaryTicker.C:
				globalStats.summarize()
			}
		}
	}()

	// Main traffic loop.
	burstTimer := time.NewTimer(time.Duration(cfg.BurstIntervalSeconds) * time.Second)
	defer burstTimer.Stop()

	rps := cfg.RequestsPerSecond
	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("traffic-generator stopping")
			globalStats.summarize()
			return

		case <-burstTimer.C:
			// Burst mode: 5x rate for 30 seconds.
			burstRPS := cfg.RequestsPerSecond * 5
			slog.Info("burst mode activated", "rps", burstRPS)
			ticker.Reset(time.Second / time.Duration(burstRPS))
			go func() {
				time.Sleep(30 * time.Second)
				slog.Info("burst mode deactivated", "rps", cfg.RequestsPerSecond)
				ticker.Reset(time.Second / time.Duration(cfg.RequestsPerSecond))
			}()
			burstTimer.Reset(time.Duration(cfg.BurstIntervalSeconds) * time.Second)

		case <-ticker.C:
			go sendRequest(ctx, client, cfg.TargetURL)
		}
	}
}

// ---------- request generation ----------

func sendRequest(ctx context.Context, client *http.Client, baseURL string) {
	roll := rand.Float64()
	var method, path string
	var body []byte

	switch {
	case roll < 0.40:
		// 40% GET /api/users
		method = http.MethodGet
		if rand.Float64() < 0.5 {
			path = "/api/users"
		} else {
			path = fmt.Sprintf("/api/users/user_%s", randomID(8))
		}

	case roll < 0.60:
		// 20% POST /api/auth/login
		method = http.MethodPost
		path = "/api/auth/login"
		body, _ = json.Marshal(map[string]string{
			"username": fmt.Sprintf("user_%s", randomID(6)),
			"password": "testpassword123",
		})

	case roll < 0.85:
		// 25% POST /api/orders
		method = http.MethodPost
		path = "/api/orders"
		items := rand.Intn(5) + 1
		orderItems := make([]map[string]any, items)
		for i := range orderItems {
			orderItems[i] = map[string]any{
				"product_id": fmt.Sprintf("prod_%s", randomID(6)),
				"quantity":   rand.Intn(3) + 1,
				"price":      float64(rand.Intn(9900)+100) / 100.0,
			}
		}
		body, _ = json.Marshal(map[string]any{
			"user_id": fmt.Sprintf("user_%s", randomID(8)),
			"items":   orderItems,
		})

	case roll < 0.95:
		// 10% GET /api/orders
		method = http.MethodGet
		if rand.Float64() < 0.5 {
			path = "/api/orders"
		} else {
			path = fmt.Sprintf("/api/orders/order_%s", randomID(8))
		}

	default:
		// 5% POST /api/users (create)
		method = http.MethodPost
		path = "/api/users"
		body, _ = json.Marshal(map[string]string{
			"name":  fmt.Sprintf("User %s", randomID(4)),
			"email": fmt.Sprintf("%s@example.com", randomID(8)),
		})
	}

	url := baseURL + path

	// Create a span so traffic generator shows up in traces.
	ctx, span := tracer.Start(ctx, fmt.Sprintf("%s %s", method, path),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", method),
			attribute.String("http.url", url),
		),
	)
	defer span.End()

	var req *http.Request
	var reqErr error
	if body != nil {
		req, reqErr = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	} else {
		req, reqErr = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if reqErr != nil {
		slog.ErrorContext(ctx, "failed to create request", "error", reqErr)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	// Propagate trace context.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		slog.WarnContext(ctx, "request failed",
			"method", method,
			"path", path,
			"error", err,
			"latency_ms", latency.Milliseconds(),
		)
		span.SetAttributes(attribute.String("error", err.Error()))
		globalStats.record(0, latency)
		return
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	globalStats.record(resp.StatusCode, latency)

	slog.DebugContext(ctx, "request completed",
		"method", method,
		"path", path,
		"status", resp.StatusCode,
		"latency_ms", latency.Milliseconds(),
	)
}

// ---------- helpers ----------

func randomID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
