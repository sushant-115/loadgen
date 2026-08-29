package chaos

import (
	"testing"
	"time"
)

func reset() {
	DisableAll()
	SetCapacity(1)
}

func TestRampOnset(t *testing.T) {
	reset()
	defer reset()
	EnableSpec(Spec{Type: ErrorInjection, Intensity: 0.8, Duration: 10 * time.Minute,
		Onset: OnsetRamp, RampSeconds: 100})
	// Immediately after enable the ramp should be near zero, far below full.
	if eff := GetIntensity(ErrorInjection); eff > 0.15 {
		t.Fatalf("ramp should start low, got %f", eff)
	}
	// Simulate elapsed time by backdating StartedAt.
	mu.Lock()
	states[ErrorInjection].StartedAt = time.Now().Add(-50 * time.Second)
	mu.Unlock()
	if eff := GetIntensity(ErrorInjection); eff < 0.3 || eff > 0.5 {
		t.Fatalf("halfway up the ramp expected ~0.4, got %f", eff)
	}
	mu.Lock()
	states[ErrorInjection].StartedAt = time.Now().Add(-200 * time.Second)
	mu.Unlock()
	if eff := GetIntensity(ErrorInjection); eff < 0.75 {
		t.Fatalf("past ramp end expected full 0.8, got %f", eff)
	}
}

func TestFlapOnset(t *testing.T) {
	reset()
	defer reset()
	EnableSpec(Spec{Type: ErrorInjection, Intensity: 0.6, Duration: 10 * time.Minute,
		Onset: OnsetFlap, FlapPeriodSeconds: 60})
	mu.Lock()
	states[ErrorInjection].StartedAt = time.Now().Add(-30 * time.Second) // first half-period: ON
	mu.Unlock()
	if eff := GetIntensity(ErrorInjection); eff < 0.55 {
		t.Fatalf("flap ON phase expected 0.6, got %f", eff)
	}
	mu.Lock()
	states[ErrorInjection].StartedAt = time.Now().Add(-90 * time.Second) // second half-period: OFF
	mu.Unlock()
	if eff := GetIntensity(ErrorInjection); eff != 0 {
		t.Fatalf("flap OFF phase expected 0, got %f", eff)
	}
}

func TestStickySurvivesDurationAndClearsViaOps(t *testing.T) {
	reset()
	defer reset()
	EnableSpec(Spec{Type: DBSlow, Intensity: 0.7, Sticky: true, Duration: time.Millisecond})
	time.Sleep(20 * time.Millisecond)
	if !IsActive(DBSlow) {
		t.Fatal("sticky fault expired by time — sticky must only clear via remediation")
	}
	// The reset-pool remediation clears db_slow…
	for _, ct := range opsClears["reset-pool"] {
		Disable(ct)
	}
	if IsActive(DBSlow) {
		t.Fatal("reset-pool should have cleared db_slow")
	}
}

func TestCapacityRelief(t *testing.T) {
	reset()
	defer reset()
	EnableSpec(Spec{Type: LatencyInjection, Intensity: 0.8, Duration: 10 * time.Minute})
	base := GetIntensity(LatencyInjection)
	SetCapacity(4)
	scaled := GetIntensity(LatencyInjection)
	if scaled >= base/3 {
		t.Fatalf("4× capacity should quarter latency intensity: base=%f scaled=%f", base, scaled)
	}
	// Error injection is NOT capacity-sensitive — scaling out doesn't fix bugs.
	EnableSpec(Spec{Type: ErrorInjection, Intensity: 0.5, Duration: 10 * time.Minute})
	if eff := GetIntensity(ErrorInjection); eff < 0.45 {
		t.Fatalf("error injection must ignore capacity, got %f", eff)
	}
}

func TestScopePercentFolds(t *testing.T) {
	reset()
	defer reset()
	EnableSpec(Spec{Type: ErrorInjection, Intensity: 0.8, Duration: 10 * time.Minute, ScopePercent: 0.25})
	if eff := GetIntensity(ErrorInjection); eff < 0.15 || eff > 0.25 {
		t.Fatalf("scope 0.25 of 0.8 expected ~0.2, got %f", eff)
	}
}

func TestLegacyEnableUnchanged(t *testing.T) {
	reset()
	defer reset()
	Enable(ErrorInjection, 0.9, 5*time.Minute)
	if eff := GetIntensity(ErrorInjection); eff < 0.85 {
		t.Fatalf("legacy Enable must be a full-intensity step, got %f", eff)
	}
}
