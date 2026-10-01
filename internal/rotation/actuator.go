package rotation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Actuator applies one decision to a spoke. Only Enforce mode calls it, and
// cmd/main.go deliberately wires none yet: promotion to Enforce is a separate,
// reviewed change that also suspends the matching CronJob (see DESIGN.md).
// With no Actuator, an Enforce spoke plans exactly as Shadow does and says so
// in a condition.
type Actuator interface {
	Apply(ctx context.Context, namespace string, d Decision) error
}

// HiveAPI is the slice of hiveclient the HiveActuator needs: an authenticated
// POST (owner session cookie) returning the raw JSON body.
type HiveAPI interface {
	Post(ctx context.Context, namespace, path string) (string, error)
}

// HiveActuator applies decisions through the hive dashboard API with the
// same call sequence as hive-rotate.sh's place_agent (switch → model →
// effort, with backend rollback when the model call fails), pause for a
// strand, and resume.
//
// It does not kick after placing: bash does not either, and the governor
// launches the agent on its new rung at its next cadence.
type HiveActuator struct {
	API HiveAPI
}

func esc(s string) string { return url.PathEscape(s) }

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

// Apply implements Actuator.
//
// Switching backend and model is two calls with no transaction: between them
// the agent sits on the new backend with the old model (codex/claude-sonnet-5
// cannot launch). A failed model call therefore rolls the backend back, and
// the caller must re-read state before trusting the result — /api/status lags
// writes, so verify against hive-state.json.
func (h *HiveActuator) Apply(ctx context.Context, ns string, d Decision) error {
	a := esc(d.Agent)
	switch d.Action {
	case ActionMove, ActionCanary:
		if d.To.Backend != d.From.Backend {
			if err := h.post(ctx, ns, "/api/switch/"+a+"/"+esc(d.To.Backend), "switched"); err != nil {
				return err
			}
		}
		if d.To.Model != d.From.Model {
			if err := h.post(ctx, ns, "/api/model/"+a+"/"+esc(d.To.Model), "model_set"); err != nil {
				if d.To.Backend != d.From.Backend {
					_ = h.post(ctx, ns, "/api/switch/"+a+"/"+esc(d.From.Backend), "switched")
				}
				return fmt.Errorf("model change failed, backend rolled back: %w", err)
			}
		}
		if d.ToEffort != "" {
			if err := h.post(ctx, ns, "/api/effort/"+a+"/"+esc(d.ToEffort), "effort_set"); err != nil {
				return fmt.Errorf("placed, but effort change failed: %w", err)
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
