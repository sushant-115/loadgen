// shape.go — chaos v2: anomaly SHAPES, not just switches.
//
// The original chaos API was a step function: intensity 0.9 for 300s, on a
// hard edge — the kind of fault any detector catches. Real production
// failures ramp (a deploy warms bad code paths), leak (memory over an
// hour), flap (a sick node in and out of rotation), or degrade partially
// (only some requests hit the bad pool). This file adds those shapes as a
// property of every chaos type, computed inside GetIntensity so EVERY
// existing call site in the services inherits them with zero changes.
//
// It also adds `sticky` faults: duration is ignored and the fault holds
// until an explicit /ops/* remediation (or kill-switch) clears it — the
// primitive that lets an InfraSage runbook GENUINELY fix an incident and
// lets the verdict harness confirm recovery followed remediation, not a
// timer.
package chaos

import (
	"log/slog"
	"math"
	"sync/atomic"
	"time"
)

// Onset kinds.
const (
	OnsetStep     = "step"     // full intensity immediately (legacy behavior)
	OnsetRamp     = "ramp"     // linear 0→intensity over ramp_seconds
	OnsetSlowLeak = "slowleak" // linear 0→intensity over the WHOLE duration
	OnsetFlap     = "flap"     // square wave: on/off every flap_period_seconds
)

// Spec is the extended enable request.
type Spec struct {
	Type              ChaosType     `json:"type"`
	Intensity         float64       `json:"intensity"`
	Duration          time.Duration `json:"-"`
	Onset             string        `json:"onset"`               // step|ramp|slowleak|flap (default step)
	RampSeconds       float64       `json:"ramp_seconds"`        // for onset=ramp (default 120)
	FlapPeriodSeconds float64       `json:"flap_period_seconds"` // for onset=flap (default 60)
	Sticky            bool          `json:"sticky"`              // ignore duration; cleared only by /ops or kill-switch
	ScopePercent      float64       `json:"scope_percent"`       // 0..1 fraction of traffic affected (default 1)
}

// EnableSpec activates a chaos scenario with full shape control. Enable()
// remains as the legacy step-onset path and now routes through here.
func EnableSpec(spec Spec) {
	spec.Intensity = normalizeIntensity(spec.Intensity)
	if !spec.Sticky {
		spec.Duration = normalizeDuration(spec.Duration)
	}
	if spec.Onset == "" {
		spec.Onset = OnsetStep
	}
	if spec.RampSeconds <= 0 {
		spec.RampSeconds = 120
	}
	if spec.FlapPeriodSeconds <= 0 {
		spec.FlapPeriodSeconds = 60
	}
	if spec.ScopePercent <= 0 || spec.ScopePercent > 1 {
		spec.ScopePercent = 1
	}

	mu.Lock()
	s, ok := states[spec.Type]
	if !ok {
		mu.Unlock()
		return
	}
	s.Enabled = true
	s.Intensity = spec.Intensity
	s.Duration = spec.Duration
	s.DurationS = spec.Duration.Seconds()
	s.StartedAt = time.Now()
	s.Onset = spec.Onset
	s.RampSeconds = spec.RampSeconds
	s.FlapPeriodSeconds = spec.FlapPeriodSeconds
	s.Sticky = spec.Sticky
	s.ScopePercent = spec.ScopePercent
	mu.Unlock()

	slog.Warn("chaos enabled",
		"type", string(spec.Type), "intensity", spec.Intensity,
		"onset", spec.Onset, "sticky", spec.Sticky,
		"scope_percent", spec.ScopePercent, "duration", spec.Duration.String())

	// Side-effect starters (same as legacy Enable).
	switch spec.Type {
	case CPUStress:
		startCPUStress(spec.Intensity)
	case MemoryLeak:
		startMemoryLeak(spec.Intensity, spec.Duration)
	case LogStorm:
		startLogStorm(spec.Intensity, spec.Duration)
	case NovelLog:
		startNovelLog(spec.Intensity, spec.Duration)
	case PodCrash:
		startPodCrash(spec.Duration)
	}

	if !spec.Sticky {
		go func(ct ChaosType, d time.Duration, startedAt time.Time) {
			time.Sleep(d)
			// Only auto-disable if this same activation is still current —
			// a re-enable resets StartedAt, invalidating the old timer.
			mu.RLock()
			cur, ok := states[ct]
			still := ok && cur.Enabled && cur.StartedAt.Equal(startedAt)
			mu.RUnlock()
			if still {
				Disable(ct)
			}
		}(spec.Type, spec.Duration, timeNowFor(spec.Type))
	}
}

// timeNowFor reads the StartedAt just written (under lock) so the
// auto-disable closure can validate it hasn't been superseded.
func timeNowFor(ct ChaosType) time.Time {
	mu.RLock()
	defer mu.RUnlock()
	if s, ok := states[ct]; ok {
		return s.StartedAt
	}
	return time.Time{}
}

// onsetFactor returns the 0..1 multiplier the shape applies at time t.
func onsetFactor(s *State, now time.Time) float64 {
	elapsed := now.Sub(s.StartedAt).Seconds()
	switch s.Onset {
	case OnsetRamp:
		if s.RampSeconds <= 0 {
			return 1
		}
		return math.Min(1, elapsed/s.RampSeconds)
	case OnsetSlowLeak:
		total := s.Duration.Seconds()
		if s.Sticky || total <= 0 {
			// Sticky slow-leak: climb over 30 minutes, then hold.
			total = (30 * time.Minute).Seconds()
		}
		return math.Min(1, elapsed/total)
	case OnsetFlap:
		period := s.FlapPeriodSeconds
		if period <= 0 {
			period = 60
		}
		if int(elapsed/period)%2 == 0 {
			return 1
		}
		return 0
	default: // OnsetStep or unset (legacy states)
		return 1
	}
}

// capacityFactor is the simulated replica capacity relative to baseline
// (1.0 = one replica). /ops/scale raises it; capacity-sensitive faults
// (latency, db-slow, queue-backlog, cpu) divide by it — scaling out
// genuinely relieves them, which is what makes the remediation loop real.
var capacityBits atomic.Uint64

func init() { capacityBits.Store(math.Float64bits(1.0)) }

func capacityFor(ct ChaosType) float64 {
	switch ct {
	case LatencyInjection, DBSlow, QueueBacklog, CPUStress:
		c := math.Float64frombits(capacityBits.Load())
		if c < 1 {
			return 1
		}
		return c
	default:
		return 1
	}
}

// SetCapacity is called by /ops/scale. Baseline is 1 replica.
func SetCapacity(replicas float64) {
	if replicas < 1 {
		replicas = 1
	}
	capacityBits.Store(math.Float64bits(replicas))
	slog.Warn("ops: capacity set", "replicas", replicas)
}

func Capacity() float64 { return math.Float64frombits(capacityBits.Load()) }
