package rotation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tuna-os/hive-operator/internal/hiveclient"
)

// Actuator applies one decision to a spoke. Only Enforce mode calls it.
type Actuator interface {
	Apply(ctx context.Context, namespace string, d Decision) error
}

// HiveAPI is the slice of hiveclient the HiveActuator needs, already bound to
// the spoke's pod and X-Hive-Internal token: the atomic placement PUT and a
// body-less POST, each returning the raw JSON body.
type HiveAPI interface {
	Place(ctx context.Context, namespace, agent string, p hiveclient.Placement) (string, error)
	Post(ctx context.Context, namespace, path string) (string, error)
}

// HiveActuator applies decisions through the hive dashboard API exactly as
// hive-rotate.sh / hive-pace.sh do since the atomic endpoint (v5.35,
// hivecommons/hive#7374): ONE PUT /api/config/agent/{name}/models carrying
// backend + model (+ the effort an agy model requires), then pause / resume
// for strands and resumes. The old /api/switch then /api/model pair could
// leave an unlaunchable half-placed agent; there is nothing to roll back now.
//
// It does not kick after placing: bash does not either, and the governor
// launches the agent on its new rung at its next cadence.
type HiveActuator struct {
	API HiveAPI
}

// PlacementOK ports hive_placement_ok: the atomic endpoint confirmed it.
//
//	.ok == true and (if has("applied") then .applied == true else true end)
//	and ((.status // "") | startswith("updated"))
func PlacementOK(body string) bool {
	var r map[string]any
	if json.Unmarshal([]byte(body), &r) != nil {
		return false
	}
	if ok, _ := r["ok"].(bool); !ok {
		return false
	}
	if v, has := r["applied"]; has {
		if b, _ := v.(bool); !b {
			return false
		}
	}
	st, _ := r["status"].(string)
	return strings.HasPrefix(st, "updated")
}

// placement builds the body hive_placement_body sends: backend, model and —
// for agy only — the reasoning effort its suffixed model requires. Other
// backends get no effort key ("absent" = leave unchanged).
func placement(backend, model string) hiveclient.Placement {
	p := hiveclient.Placement{Backend: backend, Model: model}
	if e := AgyEffort(backend, model); e != "" {
		p.ReasoningEffort = &e
	}
	return p
}

func (h *HiveActuator) post(ctx context.Context, ns, path string, want ...string) error {
	out, err := h.API.Post(ctx, ns, path)
	if err != nil {
		return err
	}
	var resp map[string]any
	if json.Unmarshal([]byte(out), &resp) != nil {
		return fmt.Errorf("%s: unparseable response %.200q", path, out)
	}
	st, _ := resp["status"].(string)
	for _, w := range want {
		if st == w {
			return nil
		}
	}
	if ok, _ := resp["ok"].(bool); ok {
		return nil
	}
	return fmt.Errorf("%s: unexpected response %.200q", path, out)
}

// Place performs one atomic placement and checks the confirmation.
func (h *HiveActuator) Place(ctx context.Context, ns, agent, backend, model string) error {
	if backend == "" || model == "" {
		// Never write a blank: an empty backend/model unseats a working agent.
		return fmt.Errorf("refusing to place %s with empty backend/model", agent)
	}
	out, err := h.API.Place(ctx, ns, agent, placement(backend, model))
	if err != nil {
		return err
	}
	if !PlacementOK(out) {
		return fmt.Errorf("placement failed: %.200s", out)
	}
	return nil
}

// Apply implements Actuator.
func (h *HiveActuator) Apply(ctx context.Context, ns string, d Decision) error {
	if d.SkipActuation {
		return nil
	}
	a := url.PathEscape(d.Agent)
	switch d.Action {
	case ActionMove, ActionCanary, ActionDemote, ActionRestore:
		if err := h.Place(ctx, ns, d.Agent, d.To.Backend, d.To.Model); err != nil {
			return err
		}
		if d.ResumeAfter {
			if err := h.post(ctx, ns, "/api/resume/"+a, "resumed"); err != nil {
				return fmt.Errorf("placed, but resume failed: %w", err)
			}
		}
		return nil
	case ActionStrand:
		return h.post(ctx, ns, "/api/pause/"+a, "paused")
	case ActionResume:
		return h.post(ctx, ns, "/api/resume/"+a, "resumed")
	}
	return nil
}
