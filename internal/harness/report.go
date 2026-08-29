// report.go — verdicts you can put in front of a design partner: one JSON
// per run (machine-readable, diffable across InfraSage versions) and one
// markdown scorecard per suite.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SuiteReport struct {
	StartedAt   time.Time   `json:"started_at"`
	FinishedAt  time.Time   `json:"finished_at"`
	InfraSage   string      `json:"infrasage_base_url"`
	Runs        []RunResult `json:"runs"`
	Passed      int         `json:"passed"`
	Failed      int         `json:"failed"`
	DegradedRCA int         `json:"degraded_rca_runs"`
}

func BuildSuiteReport(baseURL string, started time.Time, runs []RunResult) SuiteReport {
	rep := SuiteReport{StartedAt: started, FinishedAt: time.Now().UTC(), InfraSage: baseURL, Runs: runs}
	for _, r := range runs {
		if r.Passed {
			rep.Passed++
		} else {
			rep.Failed++
		}
		for _, p := range r.Phases {
			if p.Outcome == "degraded" {
				rep.DegradedRCA++
			}
		}
	}
	return rep
}

// Write drops report.json + report.md into dir (created if absent).
func (rep SuiteReport) Write(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stamp := rep.StartedAt.Format("20060102-150405")
	jsonPath := filepath.Join(dir, "verdict-"+stamp+".json")
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		return "", err
	}
	mdPath := filepath.Join(dir, "verdict-"+stamp+".md")
	if err := os.WriteFile(mdPath, []byte(rep.Markdown()), 0o644); err != nil {
		return "", err
	}
	return mdPath, nil
}

func (rep SuiteReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Loadgen Verdict — %s\n\n", rep.StartedAt.Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "Target: `%s`  \nScenarios: **%d passed / %d failed**",
		rep.InfraSage, rep.Passed, rep.Failed)
	if rep.DegradedRCA > 0 {
		fmt.Fprintf(&b, "  \n⚠ %d RCA phase(s) ran on the rule-based fallback and were excluded from accuracy grading.", rep.DegradedRCA)
	}
	b.WriteString("\n\n")
	for _, run := range rep.Runs {
		mark := "✅"
		if !run.Passed {
			mark = "❌"
		}
		fmt.Fprintf(&b, "## %s %s — %s\n\n", mark, run.ScenarioID, run.Title)
		b.WriteString("| Phase | Outcome | Elapsed | Detail |\n|---|---|---|---|\n")
		for _, p := range run.Phases {
			fmt.Fprintf(&b, "| %s | %s | %.0fs | %s |\n",
				p.Phase, p.Outcome, p.ElapsedS, strings.ReplaceAll(p.Detail, "|", "\\|"))
		}
		b.WriteString("\n")
	}
	return b.String()
}
