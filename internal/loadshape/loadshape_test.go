package loadshape

import (
	"testing"
	"time"
)

func at(wd time.Weekday, hour, min int) time.Time {
	// 2026-08-24 is a Monday.
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	d := base.AddDate(0, 0, int(wd-time.Monday))
	return time.Date(d.Year(), d.Month(), d.Day(), hour, min, 0, 0, time.UTC)
}

func TestDiurnalShape(t *testing.T) {
	s := New(DefaultConfig(100))

	night := s.DeterministicRPSAt(at(time.Tuesday, 3, 30))
	morning := s.DeterministicRPSAt(at(time.Tuesday, 10, 30))
	lunch := s.DeterministicRPSAt(at(time.Tuesday, 13, 0))
	afternoon := s.DeterministicRPSAt(at(time.Tuesday, 15, 30))

	if night >= morning {
		t.Fatalf("night (%f) should be well below morning (%f)", night, morning)
	}
	if afternoon <= night*1.5 {
		t.Fatalf("afternoon peak (%f) should dominate night trough (%f)", afternoon, night)
	}
	if lunch >= morning && lunch >= afternoon {
		t.Fatalf("lunch (%f) should dip below surrounding peaks (%f / %f)", lunch, morning, afternoon)
	}
}

func TestWeekendFactor(t *testing.T) {
	s := New(DefaultConfig(100))
	tue := s.DeterministicRPSAt(at(time.Tuesday, 15, 30))
	sun := s.DeterministicRPSAt(at(time.Sunday, 15, 30))
	if sun >= tue*0.6 {
		t.Fatalf("sunday (%f) should be well below tuesday (%f)", sun, tue)
	}
}

func TestFloorAndNoiseBounds(t *testing.T) {
	cfg := DefaultConfig(10)
	cfg.MinRPS = 2
	s := New(cfg)
	if got := s.DeterministicRPSAt(at(time.Sunday, 3, 30)); got < 2 {
		t.Fatalf("floor violated: %f", got)
	}
	// Noise stays within its clamp across many samples.
	for i := 0; i < 500; i++ {
		f := s.noiseFactor(time.Now().Add(time.Duration(i) * time.Minute))
		if f < 1-3.1*cfg.NoiseSigma || f > 1+3.1*cfg.NoiseSigma {
			t.Fatalf("noise factor out of clamp: %f", f)
		}
	}
}
