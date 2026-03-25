// Package chaos provides thread-safe anomaly/chaos injection state
// management and HTTP endpoints for triggering chaos scenarios.
package chaos

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

// ChaosType enumerates the supported chaos scenarios.
type ChaosType string

const (
	CPUStress        ChaosType = "cpu_stress"
	MemoryLeak       ChaosType = "memory_leak"
	ErrorInjection   ChaosType = "error_injection"
	LatencyInjection ChaosType = "latency_injection"
	LogStorm         ChaosType = "log_storm"
	DBSlow           ChaosType = "db_slow"
	QueueBacklog     ChaosType = "queue_backlog"
	PodCrash         ChaosType = "pod_crash"
)

// State holds the runtime state of a single chaos scenario.
type State struct {
	Enabled   bool          `json:"enabled"`
	Intensity float64       `json:"intensity"`
	Duration  time.Duration `json:"-"`
	DurationS float64       `json:"duration_seconds"`
	StartedAt time.Time     `json:"started_at,omitempty"`
}

var (
	mu     sync.RWMutex
	states = map[ChaosType]*State{
		CPUStress:        {},
		MemoryLeak:       {},
		ErrorInjection:   {},
		LatencyInjection: {},
		LogStorm:         {},
		DBSlow:           {},
		QueueBacklog:     {},
		PodCrash:         {},
	}

	// memoryBallast holds references so the GC cannot reclaim them.
	memoryBallast [][]byte
	memMu         sync.Mutex

	// cpuCancel is closed to stop CPU-burn goroutines.
	cpuCancel chan struct{}
)

// Enable activates a chaos scenario with the given intensity (0-1) and duration.
func Enable(ct ChaosType, intensity float64, duration time.Duration) {
	if intensity < 0 {
		intensity = 0
	}
	if intensity > 1 {
		intensity = 1
	}

	mu.Lock()
	s, ok := states[ct]
	if !ok {
		mu.Unlock()
		return
	}
	s.Enabled = true
	s.Intensity = intensity
	s.Duration = duration
	s.DurationS = duration.Seconds()
	s.StartedAt = time.Now()
	mu.Unlock()

	slog.Warn("chaos enabled", "type", string(ct), "intensity", intensity, "duration", duration.String())

	// Fire side-effects.
	switch ct {
	case CPUStress:
		startCPUStress(intensity)
	case MemoryLeak:
		startMemoryLeak(intensity, duration)
	case LogStorm:
		startLogStorm(intensity, duration)
	case PodCrash:
		startPodCrash(duration)
	}

	// Auto-disable after duration.
	go func() {
		time.Sleep(duration)
		Disable(ct)
	}()
}

// Disable deactivates a chaos scenario.
func Disable(ct ChaosType) {
	mu.Lock()
	s, ok := states[ct]
	if !ok {
		mu.Unlock()
		return
	}
	wasEnabled := s.Enabled
	s.Enabled = false
	s.Intensity = 0
	s.Duration = 0
	s.DurationS = 0
	s.StartedAt = time.Time{}
	mu.Unlock()

	if wasEnabled {
		slog.Info("chaos disabled", "type", string(ct))
	}

	if ct == CPUStress {
		stopCPUStress()
	}
	if ct == MemoryLeak {
		releaseMemory()
	}
}

// IsActive returns true if the given chaos type is currently enabled and
// has not exceeded its duration.
func IsActive(ct ChaosType) bool {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := states[ct]
	if !ok {
		return false
	}
	if !s.Enabled {
		return false
	}
	if s.Duration > 0 && time.Since(s.StartedAt) > s.Duration {
		return false
	}
	return true
}

// GetIntensity returns the intensity (0-1) of the given chaos type, or 0 if
// it is not active.
func GetIntensity(ct ChaosType) float64 {
	if !IsActive(ct) {
		return 0
	}
	mu.RLock()
	defer mu.RUnlock()
	return states[ct].Intensity
}

// StatusSnapshot returns a copy of all chaos states for reporting.
func StatusSnapshot() map[ChaosType]State {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[ChaosType]State, len(states))
	for k, v := range states {
		out[k] = *v
	}
	return out
}

// ---- side-effect implementations ----

func startCPUStress(intensity float64) {
	stopCPUStress()
	ch := make(chan struct{})

	mu.Lock()
	cpuCancel = ch
	mu.Unlock()

	numCPU := runtime.NumCPU()
	goroutines := int(float64(numCPU) * intensity)
	if goroutines < 1 {
		goroutines = 1
	}

	for i := 0; i < goroutines; i++ {
		go func() {
			for {
				select {
				case <-ch:
					return
				default:
					// Burn CPU.
					_ = rand.IntN(1000) * rand.IntN(1000)
				}
			}
		}()
	}
}

func stopCPUStress() {
	mu.Lock()
	ch := cpuCancel
	cpuCancel = nil
	mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func startMemoryLeak(intensity float64, duration time.Duration) {
	// Allocate chunks over the duration. Total target: intensity * 512 MB.
	totalBytes := int(intensity * 512 * 1024 * 1024)
	chunks := 100
	chunkSize := totalBytes / chunks
	if chunkSize < 1024 {
		chunkSize = 1024
	}
	interval := duration / time.Duration(chunks)

	go func() {
		for i := 0; i < chunks; i++ {
			if !IsActive(MemoryLeak) {
				return
			}
			buf := make([]byte, chunkSize)
			// Touch memory so it is actually allocated.
			for j := range buf {
				buf[j] = byte(j % 256)
			}
			memMu.Lock()
			memoryBallast = append(memoryBallast, buf)
			memMu.Unlock()
			time.Sleep(interval)
		}
	}()
}

func releaseMemory() {
	memMu.Lock()
	memoryBallast = nil
	memMu.Unlock()
}

func startLogStorm(intensity float64, duration time.Duration) {
	count := int(intensity * 1000) // logs per second
	if count < 1 {
		count = 1
	}
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(count))
		defer ticker.Stop()
		deadline := time.After(duration)
		for {
			select {
			case <-deadline:
				return
			case <-ticker.C:
				slog.Error("log_storm: simulated error event",
					"chaos", true,
					"random_id", rand.IntN(1_000_000),
					"message", "This is a synthetic error log generated by chaos log storm",
				)
			}
		}
	}()
}

func startPodCrash(delay time.Duration) {
	go func() {
		slog.Error("pod_crash: process will exit", "delay", delay.String())
		time.Sleep(delay)
		os.Exit(1)
	}()
}

// ---- HTTP endpoints ----

type chaosRequest struct {
	Intensity       float64 `json:"intensity"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// RegisterChaosEndpoints adds chaos control handlers to the given ServeMux.
func RegisterChaosEndpoints(mux *http.ServeMux) {
	endpoints := map[string]ChaosType{
		"/chaos/cpu":           CPUStress,
		"/chaos/memory":        MemoryLeak,
		"/chaos/errors":        ErrorInjection,
		"/chaos/latency":       LatencyInjection,
		"/chaos/logstorm":      LogStorm,
		"/chaos/db-slow":       DBSlow,
		"/chaos/queue-backlog": QueueBacklog,
		"/chaos/pod-crash":     PodCrash,
	}

	for path, ct := range endpoints {
		mux.HandleFunc(path, makeChaosHandler(ct))
	}

	mux.HandleFunc("/chaos/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := StatusSnapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap)
	})
}

func makeChaosHandler(ct ChaosType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			Disable(ct)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"disabled","type":"%s"}`, ct)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req chaosRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if req.DurationSeconds <= 0 {
			req.DurationSeconds = 60
		}
		if req.Intensity <= 0 {
			req.Intensity = 0.5
		}

		dur := time.Duration(req.DurationSeconds * float64(time.Second))
		Enable(ct, req.Intensity, dur)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"enabled","type":"%s","intensity":%.2f,"duration_seconds":%.0f}`,
			ct, req.Intensity, req.DurationSeconds)
	}
}
