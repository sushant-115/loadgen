// Package sysstate provides a time-deterministic system health state machine.
//
// Every service binary independently derives an identical health score by
// computing it from the current wall-clock epoch number (no IPC required).
// Because all services use the same formula and the same time source, their
// health scores evolve in lockstep, producing correlated anomalies across
// logs, metrics, and traces instead of independent per-request coin flips.
//
// Timeline structure inside a 30-minute epoch:
//
//	55% chance: full epoch is quiet (health stays at 1.0)
//	45% chance: incident epoch
//	  └── [quiet period] → [ramp-down] → [peak] → [recovery] → [quiet]
//
// Manual injection: call ForceScenario(name, duration) to immediately override
// the epoch-based computation with a specific incident. All probe functions
// (HealthScore, ActiveFaultNames, etc.) honour the forced scenario until its
// deadline. ClearForce() cancels it early.
package sysstate

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
)

// autoIncidents gates the epoch-scheduled (periodic/automated) anomaly engine.
// It is OFF by default: anomalies only occur when explicitly invoked via the
// injection scripts (ForceScenario / the chaos API). Set
// LOADGEN_AUTO_INCIDENTS=true to restore the periodic epoch schedule.
var (
	autoIncidentsOnce sync.Once
	autoIncidentsVal  bool
)

func autoIncidentsEnabled() bool {
	autoIncidentsOnce.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("LOADGEN_AUTO_INCIDENTS")))
		autoIncidentsVal = v == "true" || v == "1" || v == "yes"
	})
	return autoIncidentsVal
}

// ---------------------------------------------------------------------------
// Force override — in-process manual anomaly injection
// ---------------------------------------------------------------------------

// forcedState holds an active manual override. It is nil when the system runs
// in normal epoch-based mode. intensity multiplies the configured PeakHealth
// drop — intensity=1.0 yields the scenario's stock amplitude (e.g. PeakHealth
// 0.20 → health drops from 1.0 to 0.20), intensity=10.0 drives PeakHealth far
// lower and is the founder's "demo mode" setting that guarantees the
// watchdog organically crosses its alert threshold within ~90s on young
// tenants where stock amplitudes barely register.
type forcedState struct {
	inc       *incident
	until     time.Time
	intensity float64
}

var (
	forceMu sync.RWMutex
	forced  *forcedState
)

// ForceScenario locks the system into a named incident scenario for the
// specified duration, overriding the normal epoch-based calculation. All
// telemetry signals (health, faults, latency scaling, error rates) reflect the
// forced scenario immediately.
//
// intensity multiplies the amplitude of the fault: 1.0 is the scenario's
// configured PeakHealth (current behavior); >1.0 drives health LOWER (more
// severe). Clamped to [0.1, 10.0]. Pass 1.0 (or 0/negative) for the legacy
// amplitude.
//
// Valid scenario names: db_contention, payment_degradation,
// auth_service_degradation, network_saturation, memory_pressure.
func ForceScenario(name string, duration time.Duration, intensity float64) error {
	var found *incident
	for i := range incidents {
		if incidents[i].Name == name {
			found = &incidents[i]
			break
		}
	}
	if found == nil {
		names := make([]string, len(incidents))
		for i, inc := range incidents {
			names[i] = inc.Name
		}
		return fmt.Errorf("unknown scenario %q; valid names: %s", name, strings.Join(names, ", "))
	}

	if intensity <= 0 {
		intensity = 1.0
	}
	if intensity > 10.0 {
		intensity = 10.0
	}
	if intensity < 0.1 {
		intensity = 0.1
	}

	forceMu.Lock()
	forced = &forcedState{inc: found, until: time.Now().Add(duration), intensity: intensity}
	forceMu.Unlock()
	return nil
}

// ClearForce cancels any active manual override and returns the system to
// epoch-based health computation.
func ClearForce() {
	forceMu.Lock()
	forced = nil
	forceMu.Unlock()
}

// ForceStatus returns a snapshot of the current manual override state for the
// /anomaly/status API response.
func ForceStatus() map[string]any {
	forceMu.RLock()
	defer forceMu.RUnlock()
	if forced == nil || time.Now().After(forced.until) {
		return map[string]any{
			"active":   false,
			"scenario": "",
			"expires":  "",
		}
	}
	return map[string]any{
		"active":               true,
		"scenario":             forced.inc.Name,
		"expires":              forced.until.UTC().Format(time.RFC3339),
		"remaining_sec":        int(time.Until(forced.until).Seconds()),
		"intensity":            forced.intensity,
		"effective_peak_health": forcedEffectivePeakHealthLocked(),
	}
}

// forcedEffectivePeakHealthLocked computes the peak health value the
// healthAt path will return under the current forced override + intensity.
// Caller MUST already hold forceMu (read or write). Returns 1.0 when no
// override is active.
func forcedEffectivePeakHealthLocked() float64 {
	if forced == nil || time.Now().After(forced.until) {
		return 1.0
	}
	pk := forced.inc.PeakHealth / forced.intensity
	if pk < 0.01 {
		pk = 0.01
	}
	if pk > 1.0 {
		pk = 1.0
	}
	return pk
}

// ListScenarios returns the names of all available scenarios.
func ListScenarios() []string {
	names := make([]string, len(incidents))
	for i, inc := range incidents {
		names[i] = inc.Name
	}
	return names
}

// forcedIncident returns the active forced incident if one is set and not
// expired; otherwise returns nil.
func forcedIncident() *incident {
	forceMu.RLock()
	defer forceMu.RUnlock()
	return forcedIncidentLocked()
}

// forcedIncidentLocked is the lock-already-held variant used by callers
// that need to consume `forced.intensity` in the same critical section.
func forcedIncidentLocked() *incident {
	if forced == nil || time.Now().After(forced.until) {
		return nil
	}
	return forced.inc
}

// FaultType is a bitmask of named fault conditions.
type FaultType uint32

const (
	FaultNone              FaultType = 0
	FaultDBContention      FaultType = 1 << iota
	FaultPaymentGateway
	FaultAuthService
	FaultNetworkCongestion
	FaultMemoryPressure
	// New complex fault types — each requires correlating multiple signal kinds.
	FaultCacheStampede    // hot-key eviction storm → DB read amplification
	FaultConnectionPool   // DB connection pool exhaustion → bimodal p50/p99 latency
	FaultRetryStorm       // payment intermittent failure → order retry amplification
	FaultClockSkew        // NTP drift → JWT nbf validation fails ~35% of verify calls
	FaultQueueBackpressure // slow notification consumer → queue depth grows → order latency
)

var faultNames = map[FaultType]string{
	FaultDBContention:      "db_contention",
	FaultPaymentGateway:    "payment_gateway",
	FaultAuthService:       "auth_service",
	FaultNetworkCongestion: "network_congestion",
	FaultMemoryPressure:    "memory_pressure",
	FaultCacheStampede:     "cache_stampede",
	FaultConnectionPool:    "connection_pool",
	FaultRetryStorm:        "retry_storm",
	FaultClockSkew:         "clock_skew",
	FaultQueueBackpressure: "queue_backpressure",
}

// incident describes one named fault scenario with causal structure.
type incident struct {
	Name            string
	PrimaryFault    FaultType // service most affected
	CascadeFaults   FaultType // secondary services drawn in after ramp midpoint
	RampDuration    time.Duration
	PeakDuration    time.Duration
	RecoverDuration time.Duration
	PeakHealth      float64 // minimum health at peak (0.10=terrible, 0.30=bad)
}

var incidents = []incident{
	{
		Name:            "db_contention",
		PrimaryFault:    FaultDBContention,
		CascadeFaults:   FaultNetworkCongestion,
		RampDuration:    3 * time.Minute,
		PeakDuration:    8 * time.Minute,
		RecoverDuration: 7 * time.Minute,
		PeakHealth:      0.15,
	},
	{
		Name:            "payment_degradation",
		PrimaryFault:    FaultPaymentGateway,
		CascadeFaults:   FaultNone,
		RampDuration:    2 * time.Minute,
		PeakDuration:    6 * time.Minute,
		RecoverDuration: 8 * time.Minute,
		PeakHealth:      0.20,
	},
	{
		Name:            "auth_service_degradation",
		PrimaryFault:    FaultAuthService,
		CascadeFaults:   FaultNone,
		RampDuration:    90 * time.Second,
		PeakDuration:    5 * time.Minute,
		RecoverDuration: 5 * time.Minute,
		PeakHealth:      0.25,
	},
	{
		Name:            "network_saturation",
		PrimaryFault:    FaultNetworkCongestion,
		CascadeFaults:   FaultPaymentGateway | FaultDBContention,
		RampDuration:    4 * time.Minute,
		PeakDuration:    10 * time.Minute,
		RecoverDuration: 10 * time.Minute,
		PeakHealth:      0.10,
	},
	{
		Name:            "memory_pressure",
		PrimaryFault:    FaultMemoryPressure,
		CascadeFaults:   FaultNetworkCongestion,
		RampDuration:    5 * time.Minute,
		PeakDuration:    7 * time.Minute,
		RecoverDuration: 12 * time.Minute,
		PeakHealth:      0.30,
	},
	// ---- Complex multi-signal scenarios ----
	//
	// cache_stampede: hot-key eviction storm.
	//   Clues: cache.hit drops to ~0 (metrics) + DB read latency/errors spike (traces)
	//          + cache latency DROPS (empty = fast, which is counter-intuitive)
	//          + memory metrics look normal (data was evicted, not OOM)
	//   Root cause: cache layer, NOT the DB itself.
	{
		Name:            "cache_stampede",
		PrimaryFault:    FaultCacheStampede,
		CascadeFaults:   FaultDBContention,
		RampDuration:    2 * time.Minute,
		PeakDuration:    8 * time.Minute,
		RecoverDuration: 6 * time.Minute,
		PeakHealth:      0.20,
	},
	// connection_pool_exhaustion: DB connection pool saturated.
	//   Clues: p50 DB latency normal (lucky callers get a free slot)
	//          + p99/p999 extreme ~10-30s (callers stuck waiting for slot)
	//          + error rate low initially then climbing as wait timeouts hit
	//          + throughput normal → then drops suddenly when timeouts cascade
	//          + logs show "connection pool wait" warnings
	//   Root cause: pool sized too small, NOT a slow DB or bad query.
	{
		Name:            "connection_pool_exhaustion",
		PrimaryFault:    FaultConnectionPool,
		CascadeFaults:   FaultDBContention,
		RampDuration:    3 * time.Minute,
		PeakDuration:    10 * time.Minute,
		RecoverDuration: 8 * time.Minute,
		PeakHealth:      0.18,
	},
	// retry_amplification: partial payment failures trigger order-service retries.
	//   Clues: order-service QPS normal; payment-service QPS 3× higher (retry fan-out)
	//          + trace spans show 3 payment_attempt events per order root span
	//          + payment spans carry retry_attempt=1/2/3 attributes
	//          + payment error rate climbs OVER TIME as service drowns under retry load
	//          + order latency high only because of backoff sleep between retries
	//   Root cause: aggressive retry policy in order-service, NOT a payment infrastructure failure.
	{
		Name:            "retry_amplification",
		PrimaryFault:    FaultRetryStorm,
		CascadeFaults:   FaultPaymentGateway,
		RampDuration:    3 * time.Minute,
		PeakDuration:    9 * time.Minute,
		RecoverDuration: 6 * time.Minute,
		PeakHealth:      0.22,
	},
	// clock_skew_auth: NTP desync causes JWT "not-before" check to fail intermittently.
	//   Clues: auth error rate ~35% but NOT 100% (would be 100% for a real outage)
	//          + ONLY /verify-token endpoint affected; /login path error-free
	//          + errors are HTTP 401 (Unauthorized), NOT 500 (Internal Server Error)
	//          + gateway logs show intermittent 401s but no 5xx
	//          + user/order services see 401s on auth calls, not on their own DB calls
	//          + log event message: "token not yet valid: clock skew detected"
	//   Root cause: time drift on auth service host, NOT bad credentials or code bug.
	{
		Name:            "clock_skew_auth",
		PrimaryFault:    FaultClockSkew,
		CascadeFaults:   FaultAuthService,
		RampDuration:    90 * time.Second,
		PeakDuration:    6 * time.Minute,
		RecoverDuration: 4 * time.Minute,
		PeakHealth:      0.28,
	},
	// queue_consumer_lag: notification worker GC pressure → slow message processing.
	//   Clues: notification processing latency high (traces on consumer side)
	//          + queue depth metric rising (queue.depth gauge)
	//          + order-service p99 latency climbs even though DB + payment are healthy
	//          + order spans show queue.publish span taking seconds (backpressure)
	//          + notification logs show "slow consumer: processing backlog"
	//          + DB latency, payment latency: both normal (eliminates those suspects)
	//   Root cause: notification consumer throughput collapse, NOT order or DB.
	{
		Name:            "queue_consumer_lag",
		PrimaryFault:    FaultQueueBackpressure,
		CascadeFaults:   FaultMemoryPressure,
		RampDuration:    4 * time.Minute,
		PeakDuration:    8 * time.Minute,
		RecoverDuration: 10 * time.Minute,
		PeakHealth:      0.25,
	},
}

// epochDuration is the length of one time epoch. Scenarios are scheduled
// within epochs using the epoch number as a deterministic seed so that all
// services independently reach the same state.
const epochDuration = 30 * time.Minute

type epochData struct {
	isIncident    bool
	inc           *incident
	quietDuration time.Duration // calm period before incident onset
}

// epochInfo returns the plan for a given epoch number. It is deterministic:
// same epochNum always yields the same result regardless of which service
// calls it or when.
func epochInfo(epochNum int64) epochData {
	rng := rand.New(rand.NewSource(epochNum))
	// 55% of epochs are quiet — no incident.
	if rng.Float64() < 0.55 {
		return epochData{}
	}
	inc := &incidents[rng.Intn(len(incidents))]
	// Quiet fraction: 20-45% of the epoch elapses before the incident starts.
	quietFrac := 0.20 + rng.Float64()*0.25
	quietDur := time.Duration(float64(epochDuration) * quietFrac)
	return epochData{isIncident: true, inc: inc, quietDuration: quietDur}
}

// healthAt computes the system health score in [0.0, 1.0] for a moment in time.
// If a manual force override is active it takes priority over the epoch schedule.
func healthAt(t time.Time) float64 {
	forceMu.RLock()
	pk := forcedEffectivePeakHealthLocked()
	hasForce := forced != nil && !time.Now().After(forced.until)
	forceMu.RUnlock()
	if hasForce {
		// Forced: expose intensity-adjusted peak health immediately (no
		// ramp, already in crisis). intensity=10 + scenario PeakHealth=0.20
		// → pk=0.02, well below any threshold the watchdog uses.
		return pk
	}
	if !autoIncidentsEnabled() {
		return 1.0 // periodic injection disabled — only manual/script anomalies
	}
	epochStart := t.Truncate(epochDuration)
	epochNum := epochStart.Unix() / int64(epochDuration.Seconds())
	ed := epochInfo(epochNum)
	if !ed.isIncident {
		return 1.0
	}

	inc := ed.inc
	incidentStart := epochStart.Add(ed.quietDuration)
	rampEnd := incidentStart.Add(inc.RampDuration)
	peakEnd := rampEnd.Add(inc.PeakDuration)
	recoverEnd := peakEnd.Add(inc.RecoverDuration)

	switch {
	case t.Before(incidentStart):
		return 1.0
	case t.Before(rampEnd):
		progress := float64(t.Sub(incidentStart)) / float64(inc.RampDuration)
		return 1.0 - progress*(1.0-inc.PeakHealth)
	case t.Before(peakEnd):
		return inc.PeakHealth
	case t.Before(recoverEnd):
		progress := float64(t.Sub(peakEnd)) / float64(inc.RecoverDuration)
		return inc.PeakHealth + progress*(1.0-inc.PeakHealth)
	default:
		return 1.0
	}
}

// activeFaultsAt returns the active fault bitmask at time t.
func activeFaultsAt(t time.Time) FaultType {
	if fi := forcedIncident(); fi != nil {
		return fi.PrimaryFault | fi.CascadeFaults
	}
	health := healthAt(t)
	if health >= 0.95 {
		return FaultNone
	}
	epochStart := t.Truncate(epochDuration)
	epochNum := epochStart.Unix() / int64(epochDuration.Seconds())
	ed := epochInfo(epochNum)
	if !ed.isIncident {
		return FaultNone
	}
	inc := ed.inc
	incidentStart := epochStart.Add(ed.quietDuration)
	// Cascade faults activate once the ramp is 50% complete.
	rampMidpoint := incidentStart.Add(inc.RampDuration / 2)
	if t.After(rampMidpoint) {
		return inc.PrimaryFault | inc.CascadeFaults
	}
	return inc.PrimaryFault
}

// stateNameAt returns the human-readable phase name at time t.
func stateNameAt(t time.Time) string {
	if fi := forcedIncident(); fi != nil {
		return "critical" // forced scenarios are immediately at peak
	}
	if !autoIncidentsEnabled() {
		return "normal" // periodic injection disabled
	}
	epochStart := t.Truncate(epochDuration)
	epochNum := epochStart.Unix() / int64(epochDuration.Seconds())
	ed := epochInfo(epochNum)
	if !ed.isIncident {
		return "normal"
	}
	inc := ed.inc
	incidentStart := epochStart.Add(ed.quietDuration)
	rampEnd := incidentStart.Add(inc.RampDuration)
	peakEnd := rampEnd.Add(inc.PeakDuration)
	recoverEnd := peakEnd.Add(inc.RecoverDuration)

	switch {
	case t.Before(incidentStart):
		return "normal"
	case t.Before(rampEnd):
		return "degraded"
	case t.Before(peakEnd):
		return "critical"
	case t.Before(recoverEnd):
		return "recovering"
	default:
		return "normal"
	}
}

// HealthScore returns the global system health in [0.0, 1.0].
// 1.0 = fully healthy. 0.0 = total failure.
// All services independently compute the same value at any given moment.
func HealthScore() float64 {
	return healthAt(time.Now())
}

// FaultHealth returns the health score specific to one fault type.
//   - If ft is the primary fault of the current incident, returns health × 0.5
//     (the directly affected service sees the worst degradation).
//   - If ft is a cascade fault, returns health × 0.75.
//   - If ft is not involved in the current incident, returns 1.0 (fully healthy).
//
// Use this instead of HealthScore() for per-service probability scaling so that
// a payment degradation incident does not erroneously degrade the DB or auth.
func FaultHealth(ft FaultType) float64 {
	// Check forced override first. Intensity-aware: the same divisor used in
	// healthAt is applied here so per-service signals scale with the demo
	// intensity setting (otherwise services would degrade by stock amplitude
	// even when the founder fired the demo at 10×).
	forceMu.RLock()
	fi := forcedIncidentLocked()
	intensity := 1.0
	if forced != nil {
		intensity = forced.intensity
	}
	forceMu.RUnlock()
	if fi != nil {
		pk := fi.PeakHealth / intensity
		if pk < 0.01 {
			pk = 0.01
		}
		if fi.PrimaryFault == ft {
			return math.Max(pk*0.5, 0.01)
		}
		if fi.CascadeFaults&ft != 0 {
			return math.Max(pk*0.75, 0.01)
		}
		return 1.0
	}
	t := time.Now()
	health := healthAt(t)
	if health >= 0.95 {
		return 1.0
	}
	epochStart := t.Truncate(epochDuration)
	epochNum := epochStart.Unix() / int64(epochDuration.Seconds())
	ed := epochInfo(epochNum)
	if !ed.isIncident || ed.inc == nil {
		return 1.0
	}
	if ed.inc.PrimaryFault == ft {
		return math.Max(health*0.5, 0.01)
	}
	if ed.inc.CascadeFaults&ft != 0 {
		return math.Max(health*0.75, 0.01)
	}
	return 1.0
}

// CurrentStateName returns the human-readable system state.
func CurrentStateName() string {
	return stateNameAt(time.Now())
}

// IsActive reports whether a fault type is currently active.
func IsActive(ft FaultType) bool {
	return activeFaultsAt(time.Now())&ft != 0
}

// ActiveFaultNames returns a comma-separated list of active fault names,
// or an empty string when the system is healthy.
func ActiveFaultNames() string {
	active := activeFaultsAt(time.Now())
	if active == FaultNone {
		return ""
	}
	var names []string
	for ft, name := range faultNames {
		if active&ft != 0 {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}

// ScaledErrorRate interpolates between baseRate (at health=1.0) and maxRate
// (at health=0.0). Pass FaultHealth or HealthScore as the health argument.
func ScaledErrorRate(baseRate, maxRate, health float64) float64 {
	rate := baseRate + (maxRate-baseRate)*(1.0-health)
	if rate < 0 {
		return 0
	}
	if rate > 1 {
		return 1
	}
	return rate
}
