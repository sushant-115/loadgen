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
package sysstate

import (
	"math"
	"math/rand"
	"strings"
	"time"
)

// FaultType is a bitmask of named fault conditions.
type FaultType uint32

const (
	FaultNone              FaultType = 0
	FaultDBContention      FaultType = 1 << iota
	FaultPaymentGateway
	FaultAuthService
	FaultNetworkCongestion
	FaultMemoryPressure
)

var faultNames = map[FaultType]string{
	FaultDBContention:      "db_contention",
	FaultPaymentGateway:    "payment_gateway",
	FaultAuthService:       "auth_service",
	FaultNetworkCongestion: "network_congestion",
	FaultMemoryPressure:    "memory_pressure",
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
// All services independently compute the same value for any given timestamp.
func healthAt(t time.Time) float64 {
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
