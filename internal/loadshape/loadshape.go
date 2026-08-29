// Package loadshape turns a flat requests-per-second knob into a
// production-shaped load curve. Anomaly baselines are only as honest as
// the traffic they learn from: a flat line teaches a detector nothing
// about mornings, weekends, or lunch dips — and makes every real
// deviation trivially detectable. This curve is what InfraSage's
// seasonality-aware baselines deserve to train on.
//
// Target RPS at time t:
//
//		rps(t) = base × diurnal(t) × weekly(t) × noise(t)
//
//	  - diurnal: two-humped business-day curve (morning + afternoon peaks,
//	    lunch dip, deep night trough) built from cosine components.
//	  - weekly: weekday factor (weekends at a configurable fraction).
//	  - noise: an Ornstein-Uhlenbeck process — mean-reverting wander, so
//	    load looks organic minute-to-minute without drifting unbounded.
//
// TimeCompression accelerates the clock (compression 24 = a full diurnal
// cycle every hour) for local smoke tests; production runs use 1.
package loadshape

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

type Config struct {
	BaseRPS          float64 `json:"base_rps"`
	DiurnalAmplitude float64 `json:"diurnal_amplitude"` // 0..1; 0.6 → night ≈ 40% of base-hump
	WeekendFactor    float64 `json:"weekend_factor"`    // weekend load fraction (default 0.45)
	LunchDip         float64 `json:"lunch_dip"`         // 0..1 dip depth at ~13:00 (default 0.15)
	NoiseSigma       float64 `json:"noise_sigma"`       // OU stddev as fraction of rps (default 0.08)
	NoiseReversion   float64 `json:"noise_reversion"`   // OU mean-reversion rate per minute (default 0.2)
	TimeCompression  float64 `json:"time_compression"`  // 1 = real time; 24 = day-per-hour
	MinRPS           float64 `json:"min_rps"`           // floor so the system never goes fully silent
}

func DefaultConfig(baseRPS float64) Config {
	return Config{
		BaseRPS:          baseRPS,
		DiurnalAmplitude: 0.6,
		WeekendFactor:    0.45,
		LunchDip:         0.15,
		NoiseSigma:       0.08,
		NoiseReversion:   0.2,
		TimeCompression:  1,
		MinRPS:           0.5,
	}
}

type Shape struct {
	cfg   Config
	epoch time.Time

	mu        sync.Mutex
	noise     float64 // current OU deviation (multiplicative, around 0)
	lastNoise time.Time
}

func New(cfg Config) *Shape {
	if cfg.BaseRPS <= 0 {
		cfg.BaseRPS = 10
	}
	if cfg.TimeCompression <= 0 {
		cfg.TimeCompression = 1
	}
	if cfg.WeekendFactor <= 0 {
		cfg.WeekendFactor = 0.45
	}
	if cfg.NoiseReversion <= 0 {
		cfg.NoiseReversion = 0.2
	}
	return &Shape{cfg: cfg, epoch: time.Now(), lastNoise: time.Now()}
}

// Compression returns the configured time compression (1 = real time).
func (s *Shape) Compression() float64 { return s.cfg.TimeCompression }

// RPSAt returns the target RPS for wall-clock time t.
func (s *Shape) RPSAt(t time.Time) float64 {
	vt := s.virtualTime(t)
	rps := s.cfg.BaseRPS * s.diurnal(vt) * s.weekly(vt) * s.noiseFactor(t)
	if rps < s.cfg.MinRPS {
		return s.cfg.MinRPS
	}
	return rps
}

// DeterministicRPSAt is RPSAt without the stochastic noise term — used by
// tests and by anyone reasoning about expected load at a given hour.
func (s *Shape) DeterministicRPSAt(t time.Time) float64 {
	vt := s.virtualTime(t)
	rps := s.cfg.BaseRPS * s.diurnal(vt) * s.weekly(vt)
	if rps < s.cfg.MinRPS {
		return s.cfg.MinRPS
	}
	return rps
}

// virtualTime applies TimeCompression around the shape's epoch.
func (s *Shape) virtualTime(t time.Time) time.Time {
	if s.cfg.TimeCompression == 1 {
		return t
	}
	elapsed := t.Sub(s.epoch)
	return s.epoch.Add(time.Duration(float64(elapsed) * s.cfg.TimeCompression))
}

// diurnal: 1.0-centered two-humped business curve.
//
//	base hump: cosine with trough ~03:30, peak ~15:30
//	second harmonic: sharpens morning (≈10:30) and afternoon (≈16:00) peaks
//	lunch dip: narrow gaussian at 13:00
func (s *Shape) diurnal(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60.0
	a := s.cfg.DiurnalAmplitude
	if a <= 0 {
		return 1
	}
	main := math.Cos((h - 15.5) / 24.0 * 2 * math.Pi)          // -1..1, peak 15:30
	second := 0.35 * math.Cos((h-10.5)/6.0*2*math.Pi)          // 6h harmonic: humps 10:30 & 16:30, trough 13:30
	dip := s.cfg.LunchDip * math.Exp(-math.Pow(h-13.0, 2)/1.2) // lunch gaussian
	f := 1 + a*(0.7*main+second)/1.05 - dip
	if f < 0.05 {
		f = 0.05
	}
	return f
}

func (s *Shape) weekly(t time.Time) float64 {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return s.cfg.WeekendFactor
	case time.Friday:
		return 0.92 // Fridays taper
	case time.Monday:
		return 1.05 // Monday catch-up bump
	default:
		return 1
	}
}

// noiseFactor advances the OU process to now and returns 1+deviation.
func (s *Shape) noiseFactor(t time.Time) float64 {
	if s.cfg.NoiseSigma <= 0 {
		return 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dtMin := t.Sub(s.lastNoise).Minutes()
	if dtMin > 0 {
		if dtMin > 30 {
			dtMin = 30
		}
		theta := s.cfg.NoiseReversion
		s.noise += -theta*s.noise*dtMin +
			s.cfg.NoiseSigma*math.Sqrt(dtMin)*rand.NormFloat64()
		// clamp deviation to ±3σ-ish so a random walk can't fake an anomaly
		limit := 3 * s.cfg.NoiseSigma
		if s.noise > limit {
			s.noise = limit
		}
		if s.noise < -limit {
			s.noise = -limit
		}
		s.lastNoise = t
	}
	return 1 + s.noise
}
