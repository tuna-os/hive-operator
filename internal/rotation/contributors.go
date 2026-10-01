package rotation

import (
	"fmt"

	"github.com/tuna-os/hive-operator/internal/usage"
)

// ContribDeploy is one hive-contributors Deployment as reconcile_contributors
// reads it: the pinned AGENT_BACKEND/AGENT_MODEL and the replica count.
type ContribDeploy struct {
	Name, Backend, Model string
	Replicas             int32
}

// Contributors ports reconcile_contributors (hive-rotate.sh): park a
// contributor worker (replicas 0) while its provider is POSITIVELY
// exhausted, restore it (1) once POSITIVELY recovered, and restore one
// parked worker on an unmeasured provider (several providers can only be
// read through a live consumer — parking the last one made them
// unmeasurable forever). Fleet-wide: only the primary spoke may scale; any
// other spoke prints the skip line and plans nothing.
//
// Decisions carry Action "scale" with To.Model holding the wanted replica
// count ("0"/"1"); keep lines are Action "keep".
func Contributors(in Input, deploys []ContribDeploy, contribNS string) []Decision {
	pol := in.Policy
	if pol.Thresholds == nil {
		pol = DefaultPolicy()
	}
	s := &state{in: in, pol: pol}
	if !in.Primary {
		return []Decision{{Action: ActionKeep, Reason: "not primary",
			Line: fmt.Sprintf("contributors: skipped — pool is managed by the primary hive (%s), not %s", primaryNS(in), in.Namespace)}}
	}
	if len(deploys) == 0 {
		return []Decision{{Action: ActionKeep, Line: "no contributor deployments in " + contribNS}}
	}
	var out []Decision
	for _, d := range deploys {
		p := "unknown"
		if d.Backend != "" {
			p = usage.ProviderOf(d.Backend, d.Model)
		}
		have := d.Replicas
		var want int32
		switch {
		case s.exhausted(p):
			want = 0
		case s.recovered(p):
			want = 1
		case have == 0:
			want = 1
			out = append(out, Decision{Agent: d.Name, Action: ActionScale, From: Placement{Provider: p, Model: "0"},
				To: Placement{Provider: p, Model: "1"}, Reason: "unmeasurable while parked",
				Line: fmt.Sprintf("%-24s %-9s unmeasurable while parked -> restoring 1 to re-probe", d.Name, p)})
			continue
		default:
			out = append(out, Decision{Agent: d.Name, Action: ActionKeep, Reason: "unmeasured",
				Line: fmt.Sprintf("%-24s %-9s %s (unmeasured — left at %d)", d.Name, p, in.Providers[p].Note, have)})
			continue
		}
		if have == want {
			out = append(out, Decision{Agent: d.Name, Action: ActionKeep, Reason: "ok",
				Line: fmt.Sprintf("%-24s %-9s ok (replicas=%d)", d.Name, p, have)})
			continue
		}
		l := fmt.Sprintf("%-24s %-9s recovered -> restoring (replicas %d->1)", d.Name, p, have)
		reason := p + " recovered"
		if want == 0 {
			l = fmt.Sprintf("%-24s %-9s EXHAUSTED -> parking (replicas %d->0)", d.Name, p, have)
			reason = p + " exhausted"
		}
		out = append(out, Decision{Agent: d.Name, Action: ActionScale, From: Placement{Provider: p, Model: fmt.Sprint(have)},
			To: Placement{Provider: p, Model: fmt.Sprint(want)}, Reason: reason, Line: l})
	}
	return out
}

func primaryNS(in Input) string {
	if in.PrimaryNamespace != "" {
		return in.PrimaryNamespace
	}
	return "hive"
}
