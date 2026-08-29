// ops.go — the remediation surface. These endpoints exist so an InfraSage
// runbook (a legacy:http step) can GENUINELY fix a fault, closing the full
// loop the verdict harness grades: sticky chaos → alert → RCA → runbook
// proposed → human/harness approves → http step hits an /ops endpoint here
// → the fault clears → telemetry recovers → recovery is attributable to
// the remediation, not a timer.
//
// Every action logs a structured ops event (visible in InfraSage's log
// stream) so the remediation itself shows up on the incident timeline.
package chaos

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// opsClears maps each remediation to the fault kinds it clears. A restart
// clears process-local pathologies; targeted ops clear their one fault —
// so a runbook that "fixes" the wrong thing genuinely fails to recover,
// which is exactly what the harness needs to observe.
var opsClears = map[string][]ChaosType{
	"restart":       {CPUStress, MemoryLeak, ErrorInjection, LatencyInjection, LogStorm, NovelLog, ErrorBudget, LatencyTail},
	"reset-pool":    {DBSlow},
	"clear-backlog": {QueueBacklog},
}

// RegisterOpsEndpoints wires the remediation handlers. Called from
// RegisterChaosEndpoints so every service exposes them automatically.
func RegisterOpsEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/ops/restart", makeOpsHandler("restart", func() {
		// A "restart" also resets simulated capacity to baseline —
		// restarts don't scale you out.
	}))
	mux.HandleFunc("/ops/reset-pool", makeOpsHandler("reset-pool", nil))
	mux.HandleFunc("/ops/clear-backlog", makeOpsHandler("clear-backlog", nil))

	mux.HandleFunc("/ops/scale", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Replicas float64 `json:"replicas"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Replicas < 1 {
			http.Error(w, `{"error":"replicas (>=1) required"}`, http.StatusBadRequest)
			return
		}
		SetCapacity(req.Replicas)
		slog.Warn("ops: scaled",
			"ops_event", true, "action", "scale", "replicas", req.Replicas,
			"message", fmt.Sprintf("simulated scale-out to %.0f replicas — capacity-sensitive fault intensity divides by this", req.Replicas))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "scaled", "replicas": req.Replicas, "at": time.Now().UTC(),
		})
	})

	mux.HandleFunc("/ops/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capacity_replicas": Capacity(),
			"active_faults":     ActiveTypes(),
			"remediations": map[string]any{
				"restart":       "clears process-local faults (errors, latency, cpu, memory, log storms)",
				"reset-pool":    "clears db_slow",
				"clear-backlog": "clears queue_backlog",
				"scale":         "raises simulated capacity; divides capacity-sensitive fault intensity",
			},
		})
	})
}

func makeOpsHandler(action string, extra func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cleared := []string{}
		for _, ct := range opsClears[action] {
			if IsActive(ct) {
				Disable(ct)
				cleared = append(cleared, string(ct))
			}
		}
		if action == "restart" {
			SetCapacity(1)
		}
		if extra != nil {
			extra()
		}
		slog.Warn("ops: remediation executed",
			"ops_event", true, "action", action, "cleared", cleared,
			"message", fmt.Sprintf("remediation %q executed; cleared faults: %v", action, cleared))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "action": action, "cleared": cleared, "at": time.Now().UTC(),
		})
	}
}

// orStep normalizes an empty onset for response rendering.
func orStep(onset string) string {
	if onset == "" {
		return OnsetStep
	}
	return onset
}
