package shopify

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const maxIncidentDuration = 15 * time.Minute

// IncidentState models an injectable payment-gateway meltdown scoped to one
// shop. Mirrors the conventions of internal/chaos (RWMutex, duration-aware
// Active, auto-expiry).
type IncidentState struct {
	mu              sync.RWMutex
	enabled         bool
	shop            string
	declineFraction float64
	cancelBoost     float64
	refundBoost     float64
	startedAt       time.Time
	duration        time.Duration
}

// Enable starts an incident for the given shop.
func (i *IncidentState) Enable(shop string, decline, cancel, refund float64, dur time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enabled = true
	i.shop = shop
	i.declineFraction = clamp01(decline)
	i.cancelBoost = clamp01(cancel)
	i.refundBoost = clamp01(refund)
	i.startedAt = time.Now()
	i.duration = dur
}

// Disable clears the incident.
func (i *IncidentState) Disable() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enabled = false
	i.shop = ""
	i.declineFraction = 0
	i.cancelBoost = 0
	i.refundBoost = 0
	i.duration = 0
}

// Active reports whether an incident is currently in effect (duration-aware).
func (i *IncidentState) Active() (active bool, shop string, decline, cancel, refund float64) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.enabled {
		return false, "", 0, 0, 0
	}
	if i.duration > 0 && time.Since(i.startedAt) > i.duration {
		return false, "", 0, 0, 0
	}
	return true, i.shop, i.declineFraction, i.cancelBoost, i.refundBoost
}

func (i *IncidentState) snapshot() map[string]any {
	i.mu.RLock()
	defer i.mu.RUnlock()
	active := i.enabled && (i.duration == 0 || time.Since(i.startedAt) <= i.duration)
	remaining := 0.0
	if active && i.duration > 0 {
		remaining = (i.duration - time.Since(i.startedAt)).Seconds()
	}
	return map[string]any{
		"active":           active,
		"shop":             i.shop,
		"decline_fraction": i.declineFraction,
		"cancel_boost":     i.cancelBoost,
		"refund_boost":     i.refundBoost,
		"remaining_seconds": remaining,
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// incidentRequest is the POST body for /incident/payment-meltdown.
type incidentRequest struct {
	DurationSeconds float64 `json:"duration_seconds"`
	DeclineFraction float64 `json:"decline_fraction"`
	Shop            string  `json:"shop"`
	CancelBoost     float64 `json:"cancel_boost"`
	RefundBoost     float64 `json:"refund_boost"`
}

// RegisterIncidentEndpoints wires the incident knob onto the mux.
func RegisterIncidentEndpoints(mux *http.ServeMux, e *Emitter) {
	mux.HandleFunc("/incident/payment-meltdown", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			e.incident.Disable()
			writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
		case http.MethodPost:
			var req incidentRequest
			_ = json.NewDecoder(r.Body).Decode(&req) // all fields optional → defaults
			if req.DurationSeconds <= 0 {
				req.DurationSeconds = 300
			}
			if req.DeclineFraction <= 0 {
				req.DeclineFraction = 0.6
			}
			if req.CancelBoost <= 0 {
				req.CancelBoost = 0.25
			}
			if req.RefundBoost <= 0 {
				req.RefundBoost = 0.2
			}
			if req.Shop == "" {
				req.Shop = e.cfg.Shops[0].Domain
			}
			dur := time.Duration(req.DurationSeconds * float64(time.Second))
			if dur > maxIncidentDuration {
				dur = maxIncidentDuration
			}
			e.incident.Enable(req.Shop, req.DeclineFraction, req.CancelBoost, req.RefundBoost, dur)
			slog.Warn("shopify incident enabled", "shop", req.Shop,
				"decline_fraction", req.DeclineFraction, "duration", dur.String())
			go func() {
				time.Sleep(dur)
				e.incident.Disable()
				slog.Info("shopify incident auto-disabled", "shop", req.Shop)
			}()
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "enabled", "shop": req.Shop,
				"decline_fraction": req.DeclineFraction,
				"duration_seconds": dur.Seconds(),
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("GET /incident/status", func(w http.ResponseWriter, _ *http.Request) {
		snap := e.incident.snapshot()
		snap["emitted"] = e.emitted.Load()
		snap["declines"] = e.declines.Load()
		snap["errors"] = e.errors.Load()
		snap["webhooks_per_minute"] = e.cfg.WebhooksPerMinute
		writeJSON(w, http.StatusOK, snap)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
