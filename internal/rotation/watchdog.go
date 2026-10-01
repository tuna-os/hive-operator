package rotation

import (
	"math"
	"sort"
	"strings"
	"time"
)

// The watchdog (internal/liveness) places agents in two cases, and both are
// rotation decisions made with rotation's own rules, so they live here rather
// than being re-derived there:
//
//   - rotate OFF a backend failing liveness (auth chrome, a CLI that dies to a
//     bare shell, muse parked on an approval prompt): ChooseRungHealthy;
//   - repair a half-placed backend/model pair that cannot launch:
//     MismatchRepair.

// Members is tier_members for one tier, in preference order.
func (in Input) Members(tier string) []Rung {
	var out []Rung
	for _, r := range in.Rungs {
		if r.Tier == tier {
			out = append(out, r)
		}
	}
	return out
}

// Exhausted ports provider_exhausted against this Input's readings and
// thresholds: a POSITIVE reading at or over the threshold.
func (in Input) Exhausted(provider string) bool { return newState(in).exhausted(provider) }

// ChooseRungHealthy ports choose_rung_healthy: like choose_rung but ONLY
// rungs whose provider is positively MEASURED healthy (a known percentage
// below its threshold), never the agent's current provider, with the
// subscription escape hatch always granted (provider_ok … 1). Used by the
// watchdog to rotate OFF a backend failing liveness, where restarting on the
// same rung just re-breaks it (RFC #4665).
//
// Ranked "cost pct p|b|m" with `sort -k1,1n -k2,2n | head -1`: no -s, so a
// tie falls back to the whole line (provider, then backend, then model) —
// the same QUIRK as chooseRung. Unlike choose_rung there is no +5 per
// placement: the watchdog never calls note_placement, so ASSIGNED and
// AGY_HV_PLACED stay 0 for the whole pass.
func ChooseRungHealthy(in Input, agent string) (Rung, bool) {
	s := newState(in)
	a := s.byName[agent]
	tier := in.Tiers[agent]
	if a == nil || tier == "" {
		return Rung{}, false
	}
	curp := s.providerOf(a)
	type cand struct {
		cost, pct int
		tail      string
		r         Rung
	}
	var cs []cand
	for _, r := range s.members(tier) {
		p := r.Provider
		if p == "" || p == curp {
			continue
		}
		if !s.providerOK(p, a, true) || s.pct(p) == -1 || s.exhausted(p) {
			continue
		}
		cs = append(cs, cand{s.costRank(p), int(math.Round(s.pct(p))), p + "|" + r.Backend + "|" + r.Model, r})
	}
	if len(cs) == 0 {
		return Rung{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].cost != cs[j].cost {
			return cs[i].cost < cs[j].cost
		}
		if cs[i].pct != cs[j].pct {
			return cs[i].pct < cs[j].pct
		}
		return cs[i].tail < cs[j].tail
	})
	return cs[0].r, true
}

// BackendModelMismatch ports backend_model_mismatch: true when the pair
// CANNOT launch. Only single-provider CLIs are judged; pi is Kiro-only in
// this fleet, so a pi agent on a non-kiro id is the half-placed
// `pi --model gemini-…` the old /api/switch + /api/model pair left behind.
func BackendModelMismatch(backend, model string) bool {
	if backend == "" || model == "" {
		return false
	}
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(model, x) {
				return true
			}
		}
		return false
	}
	switch backend {
	case "claude":
		return !has("claude", "opus", "sonnet", "haiku")
	case "agy":
		return !has("gemini")
	case "codex":
		return !has("gpt-", "codex")
	case "pi":
		return !strings.HasPrefix(model, "kiro-api-key/")
	}
	return false
}

// MismatchRepair ports repair_mismatch's decision: for an agent whose
// backend cannot run its model, the first rung of its own tier on its
// CURRENT backend. A pinned agent is never repaired — repair only ever picks
// a tier member, so it could never restore a deliberate off-ladder pin.
func MismatchRepair(in Input, a Agent, pinned bool) (Rung, bool) {
	if pinned || !BackendModelMismatch(a.CLI, a.Model) {
		return Rung{}, false
	}
	tier := in.Tiers[a.Name]
	if tier == "" {
		return Rung{}, false
	}
	for _, r := range in.Members(tier) {
		if r.Backend == a.CLI {
			return r, true
		}
	}
	return Rung{}, false
}

// ResetsAt extracts the first ISO-8601 instant from a probe note, as
// gather() does with `grep -oE '[0-9]{4}-…T…:…:…' | head -1` (read as UTC).
func ResetsAt(note string) (time.Time, bool) {
	for i := 0; i+19 <= len(note); i++ {
		t, err := time.Parse("2006-01-02T15:04:05", note[i:i+19])
		if err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
