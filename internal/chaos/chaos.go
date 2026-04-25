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
	"sort"
	"strings"
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

	campaignMu     sync.RWMutex
	activeCampaign *CampaignState
)

const (
	maxDuration        = 15 * time.Minute
	maxCampaignSteps   = 32
	defaultStepSeconds = 60
)

// CampaignStep defines one anomaly action in a campaign timeline.
type CampaignStep struct {
	Type              ChaosType `json:"type"`
	Intensity         float64   `json:"intensity"`
	DurationSeconds   float64   `json:"duration_seconds"`
	StartAfterSeconds float64   `json:"start_after_seconds"`
}

// CampaignRequest defines an API payload to run or dry-run a campaign.
type CampaignRequest struct {
	CampaignID string         `json:"campaign_id"`
	DryRun     bool           `json:"dry_run"`
	Steps      []CampaignStep `json:"steps"`
}

// CampaignState tracks campaign lifecycle for status and telemetry annotations.
type CampaignState struct {
	CampaignID string    `json:"campaign_id"`
	StartedAt  time.Time `json:"started_at"`
	DryRun     bool      `json:"dry_run"`
	StepCount  int       `json:"step_count"`
	Running    bool      `json:"running"`
}

// Enable activates a chaos scenario with the given intensity (0-1) and duration.
func Enable(ct ChaosType, intensity float64, duration time.Duration) {
	intensity = normalizeIntensity(intensity)
	duration = normalizeDuration(duration)

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

// ActiveTypes returns active chaos types sorted by name.
func ActiveTypes() []ChaosType {
	mu.RLock()
	defer mu.RUnlock()
	active := make([]ChaosType, 0, len(states))
	for ct, s := range states {
		if s.Enabled {
			active = append(active, ct)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i] < active[j] })
	return active
}

// ActiveMetadata returns campaign ID and active chaos labels for telemetry.
func ActiveMetadata() (bool, string, string) {
	types := ActiveTypes()
	if len(types) == 0 {
		return false, currentCampaignID(), ""
	}
	values := make([]string, 0, len(types))
	for _, t := range types {
		values = append(values, string(t))
	}
	return true, currentCampaignID(), strings.Join(values, ",")
}

// DisableAll disables all active chaos types.
func DisableAll() {
	for ct := range states {
		Disable(ct)
	}
	campaignMu.Lock()
	if activeCampaign != nil {
		activeCampaign.Running = false
	}
	campaignMu.Unlock()
}

// StartCampaign validates and executes a campaign timeline.
func StartCampaign(req CampaignRequest) error {
	if req.CampaignID == "" {
		req.CampaignID = fmt.Sprintf("campaign-%d", time.Now().UnixNano())
	}
	if len(req.Steps) == 0 {
		return fmt.Errorf("campaign must include at least one step")
	}
	if len(req.Steps) > maxCampaignSteps {
		return fmt.Errorf("campaign has too many steps: %d > %d", len(req.Steps), maxCampaignSteps)
	}

	for i := range req.Steps {
		step := &req.Steps[i]
		if !isValidChaosType(step.Type) {
			return fmt.Errorf("invalid chaos type in step %d: %s", i, step.Type)
		}
		if step.DurationSeconds <= 0 {
			step.DurationSeconds = defaultStepSeconds
		}
		step.Intensity = normalizeIntensity(step.Intensity)
		if step.StartAfterSeconds < 0 {
			step.StartAfterSeconds = 0
		}
	}

	campaignMu.Lock()
	activeCampaign = &CampaignState{
		CampaignID: req.CampaignID,
		StartedAt:  time.Now(),
		DryRun:     req.DryRun,
		StepCount:  len(req.Steps),
		Running:    !req.DryRun,
	}
	campaignMu.Unlock()

	if req.DryRun {
		slog.Info("chaos campaign dry-run validated", "campaign_id", req.CampaignID, "steps", len(req.Steps))
		return nil
	}

	slog.Warn("chaos campaign started", "campaign_id", req.CampaignID, "steps", len(req.Steps))
	for _, step := range req.Steps {
		step := step
		go func() {
			time.Sleep(time.Duration(step.StartAfterSeconds * float64(time.Second)))
			Enable(step.Type, step.Intensity, time.Duration(step.DurationSeconds*float64(time.Second)))
		}()
	}

	go func() {
		endAfter := campaignEndDuration(req.Steps)
		time.Sleep(endAfter)
		campaignMu.Lock()
		if activeCampaign != nil && activeCampaign.CampaignID == req.CampaignID {
			activeCampaign.Running = false
		}
		campaignMu.Unlock()
		slog.Info("chaos campaign finished", "campaign_id", req.CampaignID)
	}()

	return nil
}

func campaignEndDuration(steps []CampaignStep) time.Duration {
	maxSeconds := 0.0
	for _, step := range steps {
		candidate := step.StartAfterSeconds + step.DurationSeconds
		if candidate > maxSeconds {
			maxSeconds = candidate
		}
	}
	if maxSeconds <= 0 {
		maxSeconds = defaultStepSeconds
	}
	return time.Duration(maxSeconds*float64(time.Second)) + time.Second
}

func normalizeIntensity(v float64) float64 {
	if v > 1 {
		v = v / 100.0
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v
}

func normalizeDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultStepSeconds * time.Second
	}
	if d > maxDuration {
		return maxDuration
	}
	return d
}

func isValidChaosType(ct ChaosType) bool {
	_, ok := states[ct]
	return ok
}

func currentCampaignID() string {
	campaignMu.RLock()
	defer campaignMu.RUnlock()
	if activeCampaign == nil {
		return ""
	}
	return activeCampaign.CampaignID
}

func campaignSnapshot() *CampaignState {
	campaignMu.RLock()
	defer campaignMu.RUnlock()
	if activeCampaign == nil {
		return nil
	}
	cp := *activeCampaign
	return &cp
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
		active, campaignID, types := ActiveMetadata()
		resp := map[string]any{
			"states":       snap,
			"active":       active,
			"types":        types,
			"campaign_id":  campaignID,
			"campaign":     campaignSnapshot(),
			"generated_at": time.Now().UTC(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/chaos/campaign", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req CampaignRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if err := StartCampaign(req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "accepted",
			"campaign_id": req.CampaignID,
			"dry_run":     req.DryRun,
			"step_count":  len(req.Steps),
		})
	})

	mux.HandleFunc("/chaos/kill-switch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		DisableAll()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "all chaos disabled"})
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
		Enable(ct, normalizeIntensity(req.Intensity), dur)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"enabled","type":"%s","intensity":%.2f,"duration_seconds":%.0f}`,
			ct, req.Intensity, req.DurationSeconds)
	}
}
