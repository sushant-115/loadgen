package ecom

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	maxInjectDuration = 30 * time.Minute
	defaultDuration   = 10 * time.Minute
	defaultIntensity  = 0.85
)

// state is one scenario's injection state. Mirrors internal/shopify
// IncidentState conventions (RWMutex, duration-aware Active, auto-expiry).
type state struct {
	enabled   bool
	intensity float64
	dimension string
	startedAt time.Time
	duration  time.Duration
}

// Injector holds per-scenario injection state. The metrics emitter consults it
// every collection (~10s) to perturb the matching series.
type Injector struct {
	mu     sync.RWMutex
	states map[Scenario]*state
}

func NewInjector() *Injector {
	m := make(map[Scenario]*state, len(AllScenarios))
	for _, s := range AllScenarios {
		m[s] = &state{}
	}
	return &Injector{states: m}
}

// Enable starts a scenario. dim "" falls back to the scenario default.
func (in *Injector) Enable(s Scenario, intensity float64, dim string, dur time.Duration) {
	if intensity <= 0 {
		intensity = defaultIntensity
	}
	if intensity > 1 {
		intensity = 1
	}
	if dur <= 0 {
		dur = defaultDuration
	}
	if dur > maxInjectDuration {
		dur = maxInjectDuration
	}
	if dim == "" {
		dim = DefaultDimension(s)
	}
	in.mu.Lock()
	st := in.states[s]
	st.enabled = true
	st.intensity = intensity
	st.dimension = dim
	st.startedAt = time.Now()
	st.duration = dur
	in.mu.Unlock()
	slog.Warn("ecom anomaly injected", "scenario", string(s),
		"intensity", intensity, "dimension", dim, "duration", dur.String())
}

func (in *Injector) Disable(s Scenario) {
	in.mu.Lock()
	if st, ok := in.states[s]; ok {
		*st = state{}
	}
	in.mu.Unlock()
}

func (in *Injector) Reset() {
	in.mu.Lock()
	for _, st := range in.states {
		*st = state{}
	}
	in.mu.Unlock()
	slog.Info("ecom anomalies reset — back to normal baseline")
}

// active returns (enabled, intensity, dimension) for s, honouring auto-expiry.
func (in *Injector) active(s Scenario) (bool, float64, string) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	st := in.states[s]
	if st == nil || !st.enabled {
		return false, 0, ""
	}
	if st.duration > 0 && time.Since(st.startedAt) > st.duration {
		return false, 0, ""
	}
	return true, st.intensity, st.dimension
}

func (in *Injector) snapshot() []map[string]any {
	in.mu.RLock()
	defer in.mu.RUnlock()
	out := make([]map[string]any, 0, len(AllScenarios))
	for _, s := range AllScenarios {
		st := in.states[s]
		active := st.enabled && (st.duration == 0 || time.Since(st.startedAt) <= st.duration)
		remaining := 0.0
		if active && st.duration > 0 {
			remaining = (st.duration - time.Since(st.startedAt)).Seconds()
		}
		out = append(out, map[string]any{
			"scenario":          string(s),
			"active":            active,
			"intensity":         st.intensity,
			"dimension":         st.dimension,
			"remaining_seconds": remaining,
		})
	}
	return out
}

// RegisterEndpoints wires the injector control surface onto mux:
//
//	POST   /ecom/inject?scenario=NAME&intensity=0.85&dimension=Footwear&duration=600
//	DELETE /ecom/inject?scenario=NAME
//	POST   /ecom/reset
//	GET    /ecom/status
//	GET    /ecom/scenarios
func RegisterEndpoints(mux *http.ServeMux, in *Injector) {
	mux.HandleFunc("/ecom/inject", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		name := q.Get("scenario")
		if !IsScenario(name) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "unknown scenario", "valid": AllScenarios})
			return
		}
		s := Scenario(name)
		switch r.Method {
		case http.MethodDelete:
			in.Disable(s)
			writeJSON(w, http.StatusOK, map[string]string{"status": "disabled", "scenario": name})
		case http.MethodPost:
			intensity, _ := strconv.ParseFloat(q.Get("intensity"), 64)
			durSec, _ := strconv.Atoi(q.Get("duration"))
			dur := time.Duration(durSec) * time.Second
			in.Enable(s, intensity, q.Get("dimension"), dur)
			_, gotI, gotD := in.active(s)
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "enabled", "scenario": name,
				"intensity": gotI, "dimension": gotD})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("POST /ecom/reset", func(w http.ResponseWriter, _ *http.Request) {
		in.Reset()
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})

	mux.HandleFunc("GET /ecom/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"scenarios": in.snapshot()})
	})

	mux.HandleFunc("GET /ecom/scenarios", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"scenarios": AllScenarios})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
