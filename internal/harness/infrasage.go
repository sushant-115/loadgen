// infrasage.go — the harness's view of InfraSage: a thin client over the
// handful of APIs grading needs. Auth is a bearer token + tenant header
// (the harness is an ordinary operator as far as InfraSage is concerned).
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type InfraSage struct {
	BaseURL string
	Token   string
	Tenant  string
	client  *http.Client
}

func NewInfraSage(baseURL, token, tenant string) *InfraSage {
	return &InfraSage{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Tenant:  tenant,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *InfraSage) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Tenant != "" {
		req.Header.Set("X-Tenant-ID", c.Tenant)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, clip(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// ── Alerts ──────────────────────────────────────────────────────────────

type Alert struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Severity  string    `json:"severity"`
	Status    string    `json:"status"`
	ServiceID string    `json:"service_id"`
	FiredAt   time.Time `json:"fired_at"`
}

// FiringAlerts lists current alerts; the harness filters client-side.
func (c *InfraSage) FiringAlerts(ctx context.Context) ([]Alert, error) {
	var resp struct {
		Alerts []Alert `json:"alerts"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/alerts?status=firing&limit=200", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Alerts, nil
}

// ── Changes (WS2) ───────────────────────────────────────────────────────

func (c *InfraSage) PostChange(ctx context.Context, serviceID, kind, version, ref, summary string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/changes", map[string]any{
		"service_id": serviceID,
		"kind":       kind,
		"version":    version,
		"ref":        ref,
		"summary":    summary,
		"actor":      "loadgen-harness",
	}, nil)
}

// ── RCA ─────────────────────────────────────────────────────────────────

// ResolveAlert marks one alert resolved — the harness playing the operator
// closing the page after recovery (also keeps service:severity fingerprint
// dedup from swallowing the next run's alerts).
func (c *InfraSage) ResolveAlert(ctx context.Context, alertID string) error {
	return c.do(ctx, http.MethodPatch, "/api/v1/alerts/"+alertID,
		map[string]any{"status": "resolved", "actor": "verdict-harness"}, nil)
}

func (c *InfraSage) TriggerRCA(ctx context.Context, serviceID string, at time.Time) error {
	return c.do(ctx, http.MethodPost, "/api/v1/rca/analyze", map[string]any{
		"service_id": serviceID,
		"timestamp":  at.UTC().Format(time.RFC3339),
	}, nil)
}

type RCAAnalysis struct {
	ServiceID        string `json:"service_id"`
	AnalyzedAt       string `json:"analyzed_at"`
	AnalysisText     string `json:"analysis_text"`
	LikelyRootCause  string `json:"likely_root_cause"`
	AnalysisMode     string `json:"analysis_mode"`
	Degraded         bool   `json:"degraded"`
	PublicationState string `json:"publication_state"`
	Origin           *struct {
		ServiceID  string  `json:"service_id"`
		Kind       string  `json:"kind"`
		Confidence float64 `json:"confidence"`
	} `json:"origin"`
}

func (c *InfraSage) LatestRCA(ctx context.Context, serviceID string) (*RCAAnalysis, error) {
	var out RCAAnalysis
	if err := c.do(ctx, http.MethodGet,
		"/api/v1/rca/analysis?service="+serviceID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Runbooks + approvals (the human loop, played by the harness) ────────

func (c *InfraSage) ManualTriggerRunbook(ctx context.Context, runbookID, alertID string, triggerCtx map[string]string) (string, error) {
	var resp struct {
		ActionID string `json:"action_id"`
	}
	err := c.do(ctx, http.MethodPost,
		"/api/v1/runbooks/"+runbookID+"/manual-trigger",
		map[string]any{"alert_id": alertID, "trigger_context": triggerCtx}, &resp)
	return resp.ActionID, err
}

type decisionResult struct {
	Status      string `json:"status"`
	Resume      string `json:"resume"`
	ExecutionID string `json:"execution_id"`
	ResumeError string `json:"resume_error"`
}

func (c *InfraSage) ApproveAction(ctx context.Context, actionID string) (decisionResult, error) {
	var out decisionResult
	err := c.do(ctx, http.MethodPost,
		"/api/v1/agent/actions/"+actionID+"/approve",
		map[string]any{"surface": "console", "reason": "loadgen verdict harness"}, &out)
	return out, err
}

// ── Business + saturation ───────────────────────────────────────────────

func (c *InfraSage) BusinessAnomaliesRaw(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/business/anomalies", nil)
	if err != nil {
		return "", err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Tenant != "" {
		req.Header.Set("X-Tenant-ID", c.Tenant)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(raw), nil
}

type Saturation struct {
	RCAQueue *struct {
		Depth    int `json:"depth"`
		Capacity int `json:"capacity"`
	} `json:"rca_queue"`
	LLMDegraded bool `json:"llm_degraded"`
}

func (c *InfraSage) Saturation(ctx context.Context) (*Saturation, error) {
	var out Saturation
	if err := c.do(ctx, http.MethodGet, "/api/v1/engine/saturation", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
