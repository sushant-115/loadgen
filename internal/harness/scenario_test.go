package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRepoScenarios(t *testing.T) {
	// The shipped library must always parse and validate.
	dir := filepath.Join("..", "..", "scenarios")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("scenarios dir not present in this checkout")
	}
	scenarios, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("shipped scenarios failed to load: %v", err)
	}
	if len(scenarios) < 8 {
		t.Fatalf("expected the full library, got %d", len(scenarios))
	}
	ids := map[string]bool{}
	for _, s := range scenarios {
		if ids[s.ID] {
			t.Fatalf("duplicate scenario id %s", s.ID)
		}
		ids[s.ID] = true
	}
}

func TestValidateRejectsGroundTruthlessScenario(t *testing.T) {
	s := Scenario{ID: "x", Inject: []Injection{{Target: "payment", Type: "latency"}}}
	if err := s.Validate(); err == nil {
		t.Fatal("a scenario with no expectations must be rejected — it grades nothing")
	}
}

func TestServiceMatchesTenantQualified(t *testing.T) {
	cases := []struct {
		got, want string
		match     bool
	}{
		{"payment-service", "payment-service", true},
		{"tenant-a/payment-service", "payment-service", true},
		{"acme:payment-service", "payment-service", true},
		{"auth-service", "payment-service", false},
	}
	for _, c := range cases {
		if serviceMatches(c.got, c.want) != c.match {
			t.Fatalf("serviceMatches(%q,%q) != %v", c.got, c.want, c.match)
		}
	}
}
