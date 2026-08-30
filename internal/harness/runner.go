// runner.go — the phase machine. One scenario run:
//
//	preflight → setup(changes) → inject → detect → rca → remediation
//	          → business → cooldown → verdict
//
// Absent expectation sections are SKIPPED (marked, not failed); a degraded
// (rule-based-fallback) RCA is recorded as its own outcome so an engine
// outage never masquerades as an accuracy failure — the honesty the
// analysis_mode field exists for.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Runner struct {
	Infra   *InfraSage
	Targets map[string]string // loadgen service name → base URL
	client  *http.Client
	// SkipRemediation lets CI run detection-only when no runbooks are
	// promoted yet on the target InfraSage.
	SkipRemediation bool
	Poll            time.Duration
}

func NewRunner(infra *InfraSage, targets map[string]string) *Runner {
	return &Runner{
		Infra:   infra,
		Targets: targets,
		client:  &http.Client{Timeout: 15 * time.Second},
		Poll:    15 * time.Second,
	}
}

// PhaseResult is one graded phase.
type PhaseResult struct {
	Phase    string        `json:"phase"`
	Outcome  string        `json:"outcome"` // pass | fail | skip | degraded
	Detail   string        `json:"detail"`
	Elapsed  time.Duration `json:"-"`
	ElapsedS float64       `json:"elapsed_seconds"`
}

// RunResult is one scenario's verdict.
type RunResult struct {
	ScenarioID string        `json:"scenario_id"`
	Title      string        `json:"title"`
	StartedAt  time.Time     `json:"started_at"`
	Phases     []PhaseResult `json:"phases"`
	Passed     bool          `json:"passed"`
}

func (r *Runner) Run(ctx context.Context, sc Scenario) RunResult {
	res := RunResult{ScenarioID: sc.ID, Title: sc.Title, StartedAt: time.Now().UTC()}
	add := func(phase, outcome, detail string, since time.Time) {
		el := time.Since(since)
		res.Phases = append(res.Phases, PhaseResult{
			Phase: phase, Outcome: outcome, Detail: detail,
			Elapsed: el, ElapsedS: el.Seconds(),
		})
		slog.Info("harness phase", "scenario", sc.ID, "phase", phase, "outcome", outcome, "detail", detail)
	}

	// ── preflight ───────────────────────────────────────────────────────
	t0 := time.Now()
	if err := r.preflight(ctx, sc); err != nil {
		add("preflight", "fail", err.Error(), t0)
		return finish(res)
	}
	add("preflight", "pass", "engine healthy, targets reachable", t0)

	// ── setup: change events ────────────────────────────────────────────
	changeVersions := []string{}
	if len(sc.Setup.Changes) > 0 {
		t := time.Now()
		maxLead := 0
		for _, ch := range sc.Setup.Changes {
			if err := r.Infra.PostChange(ctx, ch.Service, orDeploy(ch.Kind), ch.Version, ch.Ref, ch.Summary); err != nil {
				add("setup", "fail", "change event: "+err.Error(), t)
				return finish(res)
			}
			if ch.Version != "" {
				changeVersions = append(changeVersions, ch.Version)
			}
			if ch.LeadSeconds > maxLead {
				maxLead = ch.LeadSeconds
			}
		}
		add("setup", "pass", fmt.Sprintf("%d change event(s) emitted, leading by %ds", len(sc.Setup.Changes), maxLead), t)
		sleepCtx(ctx, time.Duration(maxLead)*time.Second)
	}

	// ── inject ──────────────────────────────────────────────────────────
	tInject := time.Now()
	injectAt := time.Now().UTC()
	for _, inj := range sc.Inject {
		inj := inj
		go func() {
			sleepCtx(ctx, time.Duration(inj.DelaySeconds)*time.Second)
			if err := r.inject(ctx, inj); err != nil {
				slog.Error("harness inject failed", "scenario", sc.ID, "target", inj.Target, "type", inj.Type, "error", err)
			}
		}()
	}
	add("inject", "pass", fmt.Sprintf("%d injection(s) dispatched", len(sc.Inject)), tInject)

	// ── quiet (false-positive control) ──────────────────────────────────
	if q := sc.Expect.Quiet; q != nil {
		t := time.Now()
		window := time.Duration(q.WithinSeconds) * time.Second
		if window <= 0 {
			window = 10 * time.Minute
		}
		fired := r.waitForAlert(ctx, q.Service, injectAt, window)
		if fired != nil {
			add("quiet", "fail", fmt.Sprintf("false positive: %q alerted (%s)", fired.Title, fired.Severity), t)
		} else {
			add("quiet", "pass", fmt.Sprintf("no alert on %s for %s — correctly quiet", q.Service, window), t)
		}
	}

	// ── detect ──────────────────────────────────────────────────────────
	var detected *Alert
	if d := sc.Expect.Detect; d != nil {
		t := time.Now()
		window := time.Duration(d.WithinSeconds) * time.Second
		if window <= 0 {
			window = 10 * time.Minute
		}
		detected = r.waitForAlert(ctx, d.Service, injectAt, window)
		switch {
		case detected == nil:
			add("detect", "fail", fmt.Sprintf("no alert on %s within %s", d.Service, window), t)
		case len(d.SeveritiesAny) > 0 && !containsFold(d.SeveritiesAny, detected.Severity):
			add("detect", "fail", fmt.Sprintf("alerted (%.0fs) but severity %q not in %v",
				time.Since(t).Seconds(), detected.Severity, d.SeveritiesAny), t)
		default:
			add("detect", "pass", fmt.Sprintf("%q (%s) after %.0fs", detected.Title, detected.Severity, time.Since(t).Seconds()), t)
		}
	}

	// ── rca ─────────────────────────────────────────────────────────────
	if e := sc.Expect.RCA; e != nil {
		t := time.Now()
		svc := ""
		if sc.Expect.Detect != nil {
			svc = sc.Expect.Detect.Service
		} else if len(e.OriginAny) > 0 {
			svc = e.OriginAny[0]
		}
		analysis, err := r.runRCA(ctx, svc, injectAt, e, detected)
		switch {
		case err != nil:
			add("rca", "fail", err.Error(), t)
		case analysis.Degraded:
			add("rca", "degraded",
				"engine served a rule-based fallback — not graded for accuracy (mode="+analysis.AnalysisMode+")", t)
		default:
			outcome, detail := gradeRCA(analysis, e, changeVersions)
			add("rca", outcome, detail, t)
		}
	}

	// ── remediation ─────────────────────────────────────────────────────
	if rem := sc.Expect.Remediation; rem != nil {
		t := time.Now()
		if r.SkipRemediation {
			add("remediation", "skip", "disabled by --skip-remediation", t)
		} else {
			outcome, detail := r.runRemediation(ctx, sc, rem, detected)
			add("remediation", outcome, detail, t)
		}
	}

	// ── business ────────────────────────────────────────────────────────
	if b := sc.Expect.Business; b != nil {
		t := time.Now()
		timeout := time.Duration(b.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 8 * time.Minute
		}
		deadline := time.Now().Add(timeout)
		hit := false
		for time.Now().Before(deadline) && !hit {
			raw, _ := r.Infra.BusinessAnomaliesRaw(ctx)
			for _, kw := range b.KeywordAny {
				if strings.Contains(strings.ToLower(raw), strings.ToLower(kw)) {
					hit = true
					break
				}
			}
			if !hit {
				sleepCtx(ctx, r.Poll)
			}
		}
		if hit {
			add("business", "pass", "business-anomaly surface reflects the fault", t)
		} else {
			add("business", "fail", fmt.Sprintf("no business anomaly matching %v within %s", b.KeywordAny, timeout), t)
		}
	}

	// ── cleanup + cooldown ──────────────────────────────────────────────
	tc := time.Now()
	r.killAll(ctx, sc)
	r.resolveRunAlerts(ctx, injectAt)
	r.cooldown(ctx, sc)
	add("cooldown", "pass", "chaos cleared, alerts resolved, engine drained", tc)

	return finish(res)
}

// ── phase helpers ───────────────────────────────────────────────────────

func (r *Runner) preflight(ctx context.Context, sc Scenario) error {
	if sat, err := r.Infra.Saturation(ctx); err == nil {
		if sat.LLMDegraded {
			return fmt.Errorf("engine already degraded before the run — fix that first, results would be meaningless")
		}
	} // saturation endpoint absent (older server) is tolerated
	for _, inj := range sc.Inject {
		base, ok := r.Targets[inj.Target]
		if !ok {
			return fmt.Errorf("no target URL for %q (pass --targets)", inj.Target)
		}
		resp, err := r.client.Get(base + "/chaos/status")
		if err != nil {
			return fmt.Errorf("target %s unreachable: %w", inj.Target, err)
		}
		resp.Body.Close()
	}
	return nil
}

func (r *Runner) inject(ctx context.Context, inj Injection) error {
	base := r.Targets[inj.Target]
	body := map[string]any{
		"intensity":           inj.Intensity,
		"duration_seconds":    inj.DurationSeconds,
		"onset":               inj.Onset,
		"ramp_seconds":        inj.RampSeconds,
		"flap_period_seconds": inj.FlapPeriodSeconds,
		"sticky":              inj.Sticky,
		"scope_percent":       inj.ScopePercent,
	}
	if inj.Payload != nil {
		body = inj.Payload
	}
	path := "/chaos/" + inj.Type
	if inj.Path != "" {
		path = inj.Path
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("chaos %s on %s: HTTP %d", inj.Type, inj.Target, resp.StatusCode)
	}
	slog.Info("harness injected", "target", inj.Target, "type", inj.Type,
		"onset", inj.Onset, "sticky", inj.Sticky, "intensity", inj.Intensity)
	return nil
}

// waitForAlert polls for a firing alert on a service that fired after t0.
// waitForAlert accepts two shapes of "the operator got paged": a fresh
// alert (fired_at after injection), OR an existing firing alert absorbing
// the incident as dedup occurrences — fingerprints are service:severity,
// so a pre-incident alert on the same service soaks subsequent fires with
// fired_at frozen (observed live: 5 in-fault fires deduped into a row
// fired 2 minutes before injection). The operator sees the alert re-light
// either way; the harness snapshots dedup counts at phase start and
// counts an increment as detection.
func (r *Runner) waitForAlert(ctx context.Context, service string, after time.Time, window time.Duration) *Alert {
	baselineDedup := map[string]uint32{}
	if alerts, err := r.Infra.FiringAlerts(ctx); err == nil {
		for _, a := range alerts {
			if serviceMatches(a.ServiceID, service) {
				baselineDedup[a.ID] = a.DedupCount
			}
		}
	}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		alerts, err := r.Infra.FiringAlerts(ctx)
		if err != nil {
			slog.Warn("harness: alerts poll failed", "error", err)
		}
		for i := range alerts {
			a := alerts[i]
			if !serviceMatches(a.ServiceID, service) {
				continue
			}
			if a.FiredAt.After(after.Add(-time.Minute)) {
				return &a
			}
			base, seen := baselineDedup[a.ID]
			switch {
			case seen && a.DedupCount > base:
				slog.Info("harness: detection via dedup occurrence on pre-existing alert",
					"alert", a.ID, "dedup", a.DedupCount, "baseline", base)
				return &a
			case !seen:
				// Transitioned to firing after phase start despite a stale
				// fired_at (resurrect/race) — the pager lit up now.
				slog.Info("harness: detection via firing transition of stale-fired_at alert",
					"alert", a.ID, "fired_at", a.FiredAt)
				return &a
			}
		}
		if !sleepCtx(ctx, r.Poll) {
			return nil
		}
	}
	return nil
}

func (r *Runner) runRCA(ctx context.Context, service string, injectAt time.Time, e *ExpectRCA, detected *Alert) (*RCAAnalysis, error) {
	if service == "" {
		return nil, fmt.Errorf("rca expectation needs detect.service or origin_any")
	}
	// Behave like an operator, not a race: wait for the ingest pipeline to
	// land the onset windows before asking for RCA (see TriggerDelaySeconds).
	// 240s default: onset windows aggregate ~2.5 min behind wall clock and
	// the named-metric scorer persists them on its next cycle (~1 min more).
	// Triggering earlier anchors the origin resolver on score rows that
	// don't exist yet — verified empirically: identical anchor, RCA at
	// onset+3min abstained (anchor_no_onset), at onset+9min resolved the
	// correct origin at 0.6 confidence.
	delay := time.Duration(e.TriggerDelaySeconds) * time.Second
	if e.TriggerDelaySeconds == 0 {
		delay = 240 * time.Second
	} else if e.TriggerDelaySeconds < 0 {
		delay = 0
	}
	if delay > 0 {
		slog.Info("harness: waiting before RCA trigger so aggregates cover the onset", "delay", delay)
		if !sleepCtx(ctx, delay) {
			return nil, ctx.Err()
		}
	}
	// Anchor the analysis at the alert's fired_at — that's the timestamp an
	// operator clicks Analyze from. time.Now() would anchor past the onset.
	anchor := time.Now().UTC()
	if detected != nil && !detected.FiredAt.IsZero() {
		anchor = detected.FiredAt.UTC()
	}
	if err := r.Infra.TriggerRCA(ctx, service, anchor); err != nil {
		return nil, fmt.Errorf("trigger rca: %w", err)
	}
	timeout := time.Duration(e.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 6 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		a, err := r.Infra.LatestRCA(ctx, service)
		if err == nil && a != nil {
			if at, perr := time.Parse(time.RFC3339, a.AnalyzedAt); perr == nil && at.After(injectAt) {
				return a, nil
			}
		}
		if !sleepCtx(ctx, r.Poll) {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("no fresh RCA within %s", timeout)
}

func gradeRCA(a *RCAAnalysis, e *ExpectRCA, changeVersions []string) (string, string) {
	var fails []string
	if len(e.OriginAny) > 0 {
		ok := a.Origin != nil && func() bool {
			for _, want := range e.OriginAny {
				if serviceMatches(a.Origin.ServiceID, want) {
					return true
				}
			}
			return false
		}()
		if !ok {
			got := "<none>"
			if a.Origin != nil {
				got = a.Origin.ServiceID
			}
			fails = append(fails, fmt.Sprintf("origin=%s want one of %v", got, e.OriginAny))
		} else if e.OriginConfidenceMin > 0 && a.Origin.Confidence < e.OriginConfidenceMin {
			fails = append(fails, fmt.Sprintf("origin confidence %.2f < %.2f", a.Origin.Confidence, e.OriginConfidenceMin))
		}
	}
	text := strings.ToLower(a.AnalysisText + " " + a.LikelyRootCause)
	if len(e.EvidenceKeywordsAny) > 0 {
		hit := false
		for _, kw := range e.EvidenceKeywordsAny {
			if strings.Contains(text, strings.ToLower(kw)) {
				hit = true
				break
			}
		}
		if !hit {
			fails = append(fails, fmt.Sprintf("analysis mentions none of %v", e.EvidenceKeywordsAny))
		}
	}
	if e.RequireChangeEvidence {
		hit := false
		for _, v := range changeVersions {
			if v != "" && strings.Contains(text, strings.ToLower(v)) {
				hit = true
				break
			}
		}
		if !hit {
			fails = append(fails, fmt.Sprintf("change evidence absent (versions %v not referenced)", changeVersions))
		}
	}
	if len(fails) > 0 {
		return "fail", strings.Join(fails, " · ")
	}
	return "pass", fmt.Sprintf("origin + evidence graded clean (mode=%s)", a.AnalysisMode)
}

func (r *Runner) runRemediation(ctx context.Context, sc Scenario, rem *ExpectRemediation, detected *Alert) (string, string) {
	alertID := ""
	if detected != nil {
		alertID = detected.ID
	}
	actionID, err := r.Infra.ManualTriggerRunbook(ctx, rem.RunbookID, alertID, map[string]string{
		"scenario": sc.ID,
	})
	if err != nil {
		return "fail", "manual-trigger: " + err.Error()
	}
	dec, err := r.Infra.ApproveAction(ctx, actionID)
	if err != nil {
		return "fail", "approve: " + err.Error()
	}
	if dec.Resume != "runbook_dispatched" {
		return "fail", fmt.Sprintf("approved but resume=%s (%s) — the fire-on-approve fix isn't holding", dec.Resume, dec.ResumeError)
	}

	// Recovery: the target's sticky fault must clear via /ops (the runbook's
	// http step), observed on the loadgen side — attribution, not luck.
	target := rem.Target
	if target == "" && len(sc.Inject) > 0 {
		target = sc.Inject[0].Target
	}
	within := time.Duration(rem.RecoversWithinSeconds) * time.Second
	if within <= 0 {
		within = 5 * time.Minute
	}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !r.anyFaultActive(ctx, target) {
			return "pass", fmt.Sprintf("runbook %s executed (exec %s) and the fault cleared", rem.RunbookID, dec.ExecutionID)
		}
		if !sleepCtx(ctx, 5*time.Second) {
			return "fail", "cancelled"
		}
	}
	return "fail", fmt.Sprintf("runbook executed but fault still active on %s after %s — remediation didn't remediate", target, within)
}

func (r *Runner) anyFaultActive(ctx context.Context, target string) bool {
	base, ok := r.Targets[target]
	if !ok {
		return false
	}
	resp, err := r.client.Get(base + "/chaos/status")
	if err != nil {
		return true // unreachable counts as unhealthy
	}
	defer resp.Body.Close()
	var st struct {
		Active bool `json:"active"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	return st.Active
}

// killAll clears every fault on every scenario target — no scenario may
// leak chaos into the next.
func (r *Runner) killAll(ctx context.Context, sc Scenario) {
	seen := map[string]bool{}
	for _, inj := range sc.Inject {
		if seen[inj.Target] {
			continue
		}
		seen[inj.Target] = true
		if base, ok := r.Targets[inj.Target]; ok {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chaos/kill-switch", nil)
			if resp, err := r.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
}

// cooldown paces the suite: fixed floor + wait for the RCA queue to drain.
// resolveRunAlerts plays the operator closing the page after recovery: every
// alert this run caused (fired after injection, blast radius included) is
// PATCHed to resolved. Without this, alert fingerprints (service:severity)
// dedup the NEXT run's identical incident into the lingering row — its
// fired_at never moves and the detect phase times out on a phantom miss.
func (r *Runner) resolveRunAlerts(ctx context.Context, injectAt time.Time) {
	alerts, err := r.Infra.FiringAlerts(ctx)
	if err != nil {
		slog.Warn("harness: could not list alerts to resolve", "error", err)
		return
	}
	for i := range alerts {
		a := alerts[i]
		if !a.FiredAt.After(injectAt.Add(-time.Minute)) {
			continue
		}
		if err := r.Infra.ResolveAlert(ctx, a.ID); err != nil {
			slog.Warn("harness: resolve alert failed", "alert", a.ID, "error", err)
			continue
		}
		slog.Info("harness: resolved run-scoped alert", "alert", a.ID, "service", a.ServiceID)
	}
}

func (r *Runner) cooldown(ctx context.Context, sc Scenario) {
	floor := time.Duration(sc.Cooldown) * time.Second
	if floor <= 0 {
		floor = 3 * time.Minute
	}
	sleepCtx(ctx, floor)
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		sat, err := r.Infra.Saturation(ctx)
		if err != nil || sat.RCAQueue == nil || sat.RCAQueue.Depth < 3 {
			return
		}
		slog.Info("harness cooldown: waiting for RCA queue to drain", "depth", sat.RCAQueue.Depth)
		if !sleepCtx(ctx, 20*time.Second) {
			return
		}
	}
}

// ── tiny utilities ──────────────────────────────────────────────────────

func finish(res RunResult) RunResult {
	res.Passed = true
	for _, p := range res.Phases {
		if p.Outcome == "fail" {
			res.Passed = false
		}
	}
	return res
}

// serviceMatches: InfraSage ids may be tenant-qualified — suffix match on
// the bare name keeps scenarios portable across tenants.
func serviceMatches(got, want string) bool {
	g, w := strings.ToLower(got), strings.ToLower(want)
	return g == w || strings.HasSuffix(g, "/"+w) || strings.HasSuffix(g, ":"+w) || strings.Contains(g, w)
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func orDeploy(kind string) string {
	if kind == "" {
		return "deploy"
	}
	return kind
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
