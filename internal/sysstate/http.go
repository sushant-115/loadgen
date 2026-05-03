package sysstate

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// RegisterEndpoints adds anomaly-injection HTTP endpoints to mux.
//
//	POST /anomaly/force?scenario=NAME&duration=SECONDS
//	     Immediately forces the named scenario for the given duration.
//	     All health/fault probe functions honour the override until expiry.
//
//	POST /anomaly/clear
//	     Cancels the active forced scenario and returns to epoch-based mode.
//
//	GET  /anomaly/status
//	     Returns the current forced-scenario state as JSON.
//
//	GET  /anomaly/scenarios
//	     Lists all available scenario names.
func RegisterEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("POST /anomaly/force", handleForce)
	mux.HandleFunc("POST /anomaly/clear", handleClear)
	mux.HandleFunc("GET /anomaly/status", handleStatus)
	mux.HandleFunc("GET /anomaly/scenarios", handleScenarios)
}

func handleForce(w http.ResponseWriter, r *http.Request) {
	scenario := r.URL.Query().Get("scenario")
	if scenario == "" {
		http.Error(w, `{"error":"missing scenario query param"}`, http.StatusBadRequest)
		return
	}

	durSec := 300 // default 5 min
	if s := r.URL.Query().Get("duration"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			http.Error(w, `{"error":"duration must be a positive integer (seconds)"}`, http.StatusBadRequest)
			return
		}
		durSec = n
	}

	if err := ForceScenario(scenario, time.Duration(durSec)*time.Second); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"scenario":      scenario,
		"duration_sec":  durSec,
		"expires":       time.Now().Add(time.Duration(durSec) * time.Second).UTC().Format(time.RFC3339),
		"health_score":  HealthScore(),
		"active_faults": ActiveFaultNames(),
		"state":         CurrentStateName(),
	})
}

func handleClear(w http.ResponseWriter, r *http.Request) {
	ClearForce()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"cleared": true,
		"state":   CurrentStateName(),
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	status := ForceStatus()
	status["health_score"] = HealthScore()
	status["state"] = CurrentStateName()
	status["active_faults"] = ActiveFaultNames()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func handleScenarios(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"scenarios": ListScenarios(),
	})
}
