// Package distribution provides statistically realistic latency sampling
// functions based on the log-normal distribution.
//
// Log-normal is the standard model for real-world service latency: it has a
// fast median (most requests are quick) with a long tail (a minority of
// requests are much slower). Unlike uniform random(min, max), it produces
// realistic histogram shapes that match what anomaly detectors expect.
//
// The ScaledDuration function additionally degrades the latency as a health
// score drops, enabling correlated slowdowns driven by sysstate.FaultHealth.
package distribution

import (
	"math"
	"math/rand"
	"time"
)

// ScaledDuration returns a log-normally distributed duration that worsens as
// health drops. baseMu is the desired median latency in milliseconds at
// full health (1.0). sigma is the log-space standard deviation controlling
// tail spread (0.3 = tight, 0.8 = fat tail).
//
// Health scaling:
//   - health=1.0 → median ≈ baseMu ms
//   - health=0.5 → median ≈ baseMu×5.5 ms, wider tail
//   - health=0.0 → median ≈ baseMu×10 ms, very wide tail
func ScaledDuration(baseMu, sigma, health float64) time.Duration {
	// Scale the log-space mean: health=1 → baseMu, health=0 → baseMu×10.
	scaleFactor := 1.0 + 9.0*(1.0-health)
	logMu := math.Log(baseMu * scaleFactor)
	// Widen the tail at low health — a degraded service has worse p95/p99.
	scaledSigma := sigma * (1.0 + 1.5*(1.0-health))

	sample := logNormalSample(logMu, scaledSigma)
	ms := math.Max(sample, 0.1)
	return time.Duration(ms * float64(time.Millisecond))
}

// logNormalSample draws from LN(mu, sigma²) using the Box-Muller transform.
// No external dependencies — stdlib math/rand only.
func logNormalSample(mu, sigma float64) float64 {
	u1 := rand.Float64()
	if u1 == 0 {
		u1 = 1e-10 // avoid log(0)
	}
	u2 := rand.Float64()
	z := math.Sqrt(-2.0*math.Log(u1)) * math.Cos(2.0*math.Pi*u2)
	return math.Exp(mu + sigma*z)
}
