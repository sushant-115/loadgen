// Package harness is the verdict engine: it runs a chaos scenario against
// the loadgen fleet, then interrogates InfraSage's own APIs to grade the
// outcome — did it detect, how fast, did the RCA name the right origin
// with the right evidence, did the change get attributed, did the runbook
// loop genuinely remediate. Scenarios carry their ground truth; the
// harness turns "I stared at the console and it seemed fine" into a
// pass/fail scorecard.
//
// Posture notes:
//   - The harness plays the HUMAN. It triggers RCA explicitly and approves
//     runbook executions through the normal approval API — InfraSage's
//     no-auto-RCA / human-decides posture is exercised, never bypassed.
//   - Runs are PACED: between scenarios the harness polls InfraSage's
//     saturation endpoint and waits for the engine to breathe. Back-to-back
//     hammering is how the RE2-OB evaluation poisoned its own tail.
package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is one graded test case.
type Scenario struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`

	Setup    Setup       `yaml:"setup"`
	Inject   []Injection `yaml:"inject"`
	Expect   Expect      `yaml:"expect"`
	Cooldown int         `yaml:"cooldown_seconds"`
}

// Setup runs before injection — today: change events (deploys, config,
// flags) emitted to InfraSage so change-intelligence has something to
// attribute. lead_seconds gaps the change from the fault onset.
type Setup struct {
	Changes []ChangeEvent `yaml:"changes"`
}

type ChangeEvent struct {
	Service     string `yaml:"service"` // InfraSage service id (suffix-matched)
	Kind        string `yaml:"kind"`    // deploy | config | flag | scale | migration
	Version     string `yaml:"version"`
	Ref         string `yaml:"ref"`
	Summary     string `yaml:"summary"`
	LeadSeconds int    `yaml:"lead_seconds"`
}

// Injection is one chaos activation on one loadgen service.
type Injection struct {
	Target            string  `yaml:"target"` // loadgen service name (resolved via targets map)
	Type              string  `yaml:"type"`   // chaos endpoint suffix: db-slow, latency, error-budget…
	Intensity         float64 `yaml:"intensity"`
	DurationSeconds   float64 `yaml:"duration_seconds"`
	Onset             string  `yaml:"onset"`
	RampSeconds       float64 `yaml:"ramp_seconds"`
	FlapPeriodSeconds float64 `yaml:"flap_period_seconds"`
	Sticky            bool    `yaml:"sticky"`
	ScopePercent      float64 `yaml:"scope_percent"`
	DelaySeconds      int     `yaml:"delay_seconds"` // stagger multi-injection scenarios

	// Escape hatch for non-/chaos endpoints (mock-shopify's /incident/*):
	// Path overrides "/chaos/<type>"; Payload replaces the standard chaos
	// body when set.
	Path    string         `yaml:"path"`
	Payload map[string]any `yaml:"payload"`
}

// Expect is the ground truth the run is graded against. Absent sections
// are skipped, not failed — a pure-detection scenario needn't grade RCA.
type Expect struct {
	Detect      *ExpectDetect      `yaml:"detect"`
	RCA         *ExpectRCA         `yaml:"rca"`
	Remediation *ExpectRemediation `yaml:"remediation"`
	Business    *ExpectBusiness    `yaml:"business"`
	// Quiet inverts detection: PASS means InfraSage did NOT alert on the
	// named service within the window — the false-positive control.
	Quiet *ExpectQuiet `yaml:"quiet"`
}

type ExpectDetect struct {
	Service       string   `yaml:"service"`
	WithinSeconds int      `yaml:"within_seconds"`
	SeveritiesAny []string `yaml:"severities_any"`
}

type ExpectRCA struct {
	OriginAny           []string `yaml:"origin_any"`
	OriginConfidenceMin float64  `yaml:"origin_confidence_min"`
	EvidenceKeywordsAny []string `yaml:"evidence_keywords_any"`
	// RequireChangeEvidence passes only if the analysis text references
	// the version string emitted in setup.changes — proof the WS2 join
	// reached the model, not just the database.
	RequireChangeEvidence bool `yaml:"require_change_evidence"`
	TimeoutSeconds        int  `yaml:"timeout_seconds"`
	// TriggerDelaySeconds is how long the harness waits after detection
	// before triggering RCA (default 150). Aggregates land ~2.5 minutes
	// behind wall clock; triggering instantly anchors the origin resolver
	// on windows that don't exist yet and it abstains with "no onset" —
	// a failure mode no human operator reproduces, since nobody clicks
	// Analyze within one second of the alert. 0 means default; -1 means
	// trigger immediately (to deliberately test the too-early path).
	TriggerDelaySeconds int `yaml:"trigger_delay_seconds"`
}

type ExpectRemediation struct {
	RunbookID             string `yaml:"runbook_id"`
	Target                string `yaml:"target"` // loadgen service whose fault must clear
	RecoversWithinSeconds int    `yaml:"recovers_within_seconds"`
}

type ExpectBusiness struct {
	// KeywordAny is matched against the business-anomalies API response —
	// loose by design; KPI naming is tenant-configured.
	KeywordAny     []string `yaml:"keyword_any"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

type ExpectQuiet struct {
	Service       string `yaml:"service"`
	WithinSeconds int    `yaml:"within_seconds"`
}

// LoadDir reads every *.yaml scenario in dir, sorted by id.
func LoadDir(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scenarios dir: %w", err)
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		s, lerr := LoadFile(filepath.Join(dir, e.Name()))
		if lerr != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), lerr)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func LoadFile(path string) (Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	var s Scenario
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return Scenario{}, fmt.Errorf("yaml: %w", err)
	}
	return s, s.Validate()
}

func (s Scenario) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("scenario id required")
	}
	if len(s.Inject) == 0 && s.Setup.Changes == nil {
		return fmt.Errorf("%s: nothing to do (no inject, no setup)", s.ID)
	}
	for i, inj := range s.Inject {
		if inj.Target == "" || (inj.Type == "" && inj.Path == "") {
			return fmt.Errorf("%s: inject[%d] needs target and type (or path)", s.ID, i)
		}
	}
	if s.Expect.Detect == nil && s.Expect.Quiet == nil && s.Expect.RCA == nil &&
		s.Expect.Remediation == nil && s.Expect.Business == nil {
		return fmt.Errorf("%s: no expectations — a scenario with no ground truth grades nothing", s.ID)
	}
	if s.Cooldown <= 0 {
		s.Cooldown = 180
	}
	return nil
}

// TotalInjectionWindow returns the longest injection horizon — used to
// bound the watch phases.
func (s Scenario) TotalInjectionWindow() time.Duration {
	max := 0.0
	for _, inj := range s.Inject {
		d := inj.DurationSeconds + float64(inj.DelaySeconds)
		if inj.Sticky {
			d = float64(inj.DelaySeconds) + 1800 // sticky: bounded by remediation phase
		}
		if d > max {
			max = d
		}
	}
	return time.Duration(max) * time.Second
}
