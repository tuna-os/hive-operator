package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
)

// errNotMeasured marks "could not measure". It is never treated as unhealthy:
// an exec, RBAC or API failure must not roll a target back or blocklist a
// release (a lesson from the predecessor scripts).
var errNotMeasured = errors.New("could not measure")

// HiveProbe is the in-pod surface the release gate needs. Kept small so tests
// can fake it; the real implementation wraps hiveclient unchanged.
type HiveProbe interface {
	// Health returns the HTTP status of GET /api/health inside the pod. An
	// error means the probe could not run, not that the spoke is unhealthy.
	Health(ctx context.Context, ns, pod string) (int, error)
	// Agents returns every agent's placement from /api/status, overlaid with
	// the persisted override journal. An error (including a still-initializing
	// status) means not measured.
	Agents(ctx context.Context, ns, pod string) ([]hivev1.AgentPlacementSnapshot, error)
	// Place re-applies a placement atomically.
	Place(ctx context.Context, ns, pod, agent string, p hiveclient.Placement) error
}

// HTTPProber fetches a URL from the operator (hub health).
type HTTPProber interface {
	Get(ctx context.Context, url string) (int, error)
}

// execProbe implements HiveProbe over pod exec with X-Hive-Internal.
type execProbe struct{ c *hiveclient.Client }

func (p execProbe) token(ctx context.Context, ns string) (string, error) {
	return p.c.Secret(ctx, ns, "hive-secrets", "HIVE_DASHBOARD_TOKEN")
}

func (p execProbe) Health(ctx context.Context, ns, pod string) (int, error) {
	out, err := p.c.Sh(ctx, ns, pod, "curl -s -o /dev/null -m 20 -w '%{http_code}' "+hiveclient.ShellQuote(hiveclient.APIAddr+"/api/health"))
	if err != nil {
		return 0, fmt.Errorf("%w: exec health probe: %v", errNotMeasured, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || code == 0 {
		// curl prints 000 when the port is not listening yet: the pod is up but
		// the API is not, which is a measured failure, not a probe failure.
		if strings.TrimSpace(out) == "000" {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: health probe printed %q", errNotMeasured, out)
	}
	return code, nil
}

type releaseStatusAgent struct {
	Name            string `json:"name"`
	CLI             string `json:"cli"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	Paused          bool   `json:"paused"`
}

func (p execProbe) Agents(ctx context.Context, ns, pod string) ([]hivev1.AgentPlacementSnapshot, error) {
	tok, err := p.token(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("%w: dashboard token: %v", errNotMeasured, err)
	}
	var sr struct {
		Status string               `json:"status"`
		Agents []releaseStatusAgent `json:"agents"`
	}
	if err := p.c.GetJSON(ctx, ns, pod, tok, "/api/status", &sr); err != nil {
		return nil, fmt.Errorf("%w: /api/status: %v", errNotMeasured, err)
	}
	if sr.Agents == nil {
		return nil, fmt.Errorf("%w: /api/status has no agents yet (status %q)", errNotMeasured, sr.Status)
	}
	// /api/status can lag a placement write; the override journal is what the
	// next launch uses, so it wins (same rule as the HiveSpoke observer).
	var journal struct {
		Agents map[string]struct {
			Backend string `json:"backend_override"`
			Model   string `json:"model_override"`
		} `json:"agents"`
	}
	if out, err := p.c.Sh(ctx, ns, pod, "cat /data/hive-state.json 2>/dev/null"); err == nil {
		_ = json.Unmarshal([]byte(out), &journal)
	}
	snap := make([]hivev1.AgentPlacementSnapshot, 0, len(sr.Agents))
	for _, a := range sr.Agents {
		if o, ok := journal.Agents[a.Name]; ok {
			if o.Backend != "" {
				a.CLI = o.Backend
			}
			if o.Model != "" {
				a.Model = o.Model
			}
		}
		snap = append(snap, hivev1.AgentPlacementSnapshot{Name: a.Name, Backend: a.CLI, Model: a.Model, Effort: a.ReasoningEffort, Paused: a.Paused})
	}
	return snap, nil
}

func (p execProbe) Place(ctx context.Context, ns, pod, agent string, pl hiveclient.Placement) error {
	tok, err := p.token(ctx, ns)
	if err != nil {
		return err
	}
	out, err := p.c.Place(ctx, ns, pod, tok, agent, pl)
	if err != nil {
		return err
	}
	var resp map[string]any
	if json.Unmarshal([]byte(out), &resp) != nil || resp["error"] != nil {
		return fmt.Errorf("unexpected response: %.200s", out)
	}
	return nil
}

// netProber implements HTTPProber with a plain HTTP client.
type netProber struct{ c *http.Client }

func (n netProber) Get(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	c := n.c
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: GET %s: %v", errNotMeasured, url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
