// Traffic Generator – sends continuous HTTP traffic to the API gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
var actors = newActorState(2000)

// ---------- config ----------

type config struct {
	TargetURL            string
	RequestsPerSecond    int
	BurstIntervalSeconds int
	ScenarioFile         string
}

func loadConfig() config {
	c := config{
		TargetURL:            envOrDefault("TARGET_URL", "http://gateway:8080"),
		RequestsPerSecond:    envIntOrDefault("REQUESTS_PER_SECOND", 10),
		BurstIntervalSeconds: envIntOrDefault("BURST_INTERVAL_SECONDS", 300),
		ScenarioFile:         envOrDefault("TRAFFIC_SCENARIO_FILE", ""),
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
	scenarioCfg, err := loadScenario(cfg.ScenarioFile)
	if err != nil {
		slog.Error("failed to load scenario", "error", err, "file", cfg.ScenarioFile)
		os.Exit(1)
	}

	picker, err := newActionPicker(scenarioCfg)
	if err != nil {
		slog.Error("invalid scenario", "error", err)
		os.Exit(1)
	}

	slog.Info("traffic-generator starting",
		"target", cfg.TargetURL,
		"rps", cfg.RequestsPerSecond,
		"burst_interval_s", cfg.BurstIntervalSeconds,
		"scenario", scenarioCfg.Name,
		"scenario_file", cfg.ScenarioFile,
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
			go sendRequest(ctx, client, cfg.TargetURL, picker)
		}
	}
}

// ---------- request generation ----------

func sendRequest(ctx context.Context, client *http.Client, baseURL string, picker *actionPicker) {
	plan := buildPlan(picker.pick(), actors)
	if plan.Action == "" {
		return
	}

	url := baseURL + plan.Path

	// Create a span so traffic generator shows up in traces.
	ctx, span := tracer.Start(ctx, fmt.Sprintf("%s %s", plan.Method, plan.Path),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", plan.Method),
			attribute.String("http.url", url),
			attribute.String("peer.service", "api-gateway"),
			attribute.String("traffic.action", plan.Action),
		),
	)
	defer span.End()

	var req *http.Request
	var reqErr error
	if plan.Body != nil {
		req, reqErr = http.NewRequestWithContext(ctx, plan.Method, url, bytes.NewReader(plan.Body))
	} else {
		req, reqErr = http.NewRequestWithContext(ctx, plan.Method, url, nil)
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
			"method", plan.Method,
			"path", plan.Path,
			"action", plan.Action,
			"error", err,
			"latency_ms", latency.Milliseconds(),
		)
		span.SetAttributes(attribute.String("error", err.Error()))
		globalStats.record(0, latency)
		return
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	if resp.StatusCode < http.StatusBadRequest {
		trackState(plan.Action, resp.Body)
	}
	globalStats.record(resp.StatusCode, latency)

	slog.DebugContext(ctx, "request completed",
		"method", plan.Method,
		"path", plan.Path,
		"action", plan.Action,
		"status", resp.StatusCode,
		"latency_ms", latency.Milliseconds(),
	)
}

func trackState(action string, body io.Reader) {
	raw, err := io.ReadAll(body)
	if err != nil || len(raw) == 0 {
		return
	}

	switch action {
	case "users_create", "users_get":
		var user struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &user); err == nil {
			actors.addUser(user.ID)
		}
	case "auth_login":
		var login struct {
			Token  string `json:"token"`
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(raw, &login); err == nil {
			actors.addToken(login.Token)
			actors.addUser(login.UserID)
		}
	case "orders_create", "orders_get":
		var order struct {
			OrderID string `json:"order_id"`
			UserID  string `json:"user_id"`
		}
		if err := json.Unmarshal(raw, &order); err == nil {
			actors.addOrder(order.OrderID)
			actors.addUser(order.UserID)
		}
	}
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
