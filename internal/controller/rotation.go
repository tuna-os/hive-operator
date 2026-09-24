package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// rotationProviders is hive-rotate.sh's HIVE_ROTATE_PROVIDERS default. A
// provider outside it (github, ibm) has no reading and is never placed on.
var rotationProviders = []string{"google", "meta", "deepseek", "openai", "anthropic"}

// providerUsage reads the bash probe's ConfigMap: the spoke namespace first,
// then the primary (spokes publish into hive/ with measured_by=<ns>).
func (r *HiveSpokeReconciler) providerUsage(ctx context.Context, sp *hivev1.HiveSpoke) []hivev1.ProviderState {
	var cm corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: sp.Spec.Namespace, Name: "hive-provider-usage"}, &cm)
	if err != nil && sp.Spec.Namespace != "hive" {
		err = r.Get(ctx, client.ObjectKey{Namespace: "hive", Name: "hive-provider-usage"}, &cm)
	}
	result := make([]hivev1.ProviderState, 0, len(rotationProviders))
	for _, provider := range rotationProviders {
		state := hivev1.ProviderState{Provider: provider, UsedPercent: -1, Note: "no reading"}
		if err == nil {
			if v, ok := cm.Data[provider]; ok {
				pr := usage.ParseProbeValue(v)
				state.Note = v
				state.UsedPercent = int32(pr.Percent)
				if !pr.ResetsAt.IsZero() {
					state.ResetsAt = &metav1.Time{Time: pr.ResetsAt}
				}
			}
		}
		metrics.ProviderUsedPercent.WithLabelValues(sp.Name, provider).Set(float64(state.UsedPercent))
		result = append(result, state)
	}
	return result
}

// poolReadings returns provider → worst window UsedPercent from UsagePools.
func (r *HiveSpokeReconciler) poolReadings(ctx context.Context) map[string]float64 {
	var pools hivev1.UsagePoolList
	out := map[string]float64{}
	if err := r.List(ctx, &pools); err != nil {
		return out
	}
	for _, p := range pools.Items {
		worst := -1.0
		for _, w := range p.Status.Windows {
			if v, err := strconv.ParseFloat(w.UsedPercent, 64); err == nil && v > worst {
				worst = v
			}
		}
		if worst >= 0 {
			if cur, ok := out[p.Spec.Provider]; !ok || worst > cur {
				out[p.Spec.Provider] = worst
			}
		}
	}
	return out
}

func policyFrom(spec *hivev1.RotationPolicySpec) rotation.Policy {
	pol := rotation.DefaultPolicy()
	if spec == nil {
		return pol
	}
	for p, t := range spec.Thresholds {
		pol.Thresholds[p] = float64(t)
	}
	if spec.HighVolumeCadenceSeconds > 0 {
		pol.HighVolumeCadenceS = int(spec.HighVolumeCadenceSeconds)
	}
	if spec.AgyMaxHighVolume > 0 {
		pol.AgyMaxHighVolume = int(spec.AgyMaxHighVolume)
	}
	pol.Canaries = !spec.DisableCanaries
	pol.AutoResume = !spec.DisableAutoResume
	pol.MeteredFailover = !spec.DisableMeteredFailover
	if len(spec.PeakProviders) > 0 {
		pol.PeakProviders = spec.PeakProviders
	}
	if spec.PeakWindows != "" {
		pol.PeakWindows = spec.PeakWindows
	}
	return pol
}

// ladderRungs flattens the effective ladder into tier members, de-duplicated
// on tier+provider+model like tier_members (effort variants of one model are
// one rung for placement; effort is derived from the agy model suffix).
func ladderRungs(effective []hivev1.Rung) []rotation.Rung {
	seen := map[string]bool{}
	var out []rotation.Rung
	for _, r := range effective {
		if !r.Available {
			continue
		}
		k := r.Tier + "|" + r.Provider + "|" + r.Model
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, rotation.Rung{Tier: r.Tier, Provider: r.Provider, Backend: r.Backend, Model: r.Model})
	}
	return out
}

// buildRotationInput assembles the planner's world from spoke status.
func buildRotationInput(sp *hivev1.HiveSpoke, probe []hivev1.ProviderState, pools map[string]float64,
	rungs []rotation.Rung, now time.Time) (rotation.Input, string) {
	in := rotation.Input{
		Now: now, Namespace: sp.Spec.Namespace,
		Primary:   sp.Spec.PrimarySpoke == "" || sp.Spec.PrimarySpoke == sp.Spec.Namespace || sp.Spec.PrimarySpoke == sp.Name,
		Providers: map[string]rotation.Reading{},
		Tiers:     map[string]string{},
		Rungs:     rungs,
		Pins:      map[string]bool{},
		Holds:     map[string]bool{},
		Policy:    policyFrom(sp.Spec.Rotation),
	}
	for _, a := range sp.Status.Agents {
		in.Agents = append(in.Agents, rotation.Agent{Name: a.Name, CLI: a.Backend, Model: a.Model, Effort: a.Effort,
			Paused: a.Paused, OnDemand: a.OnDemand, PausedTrigger: a.PausedTrigger, Cadence: a.Cadence})
	}
	tiers := sp.Spec.AgentTiers
	if len(tiers) == 0 {
		tiers = agentTiers
	}
	for a, t := range tiers {
		in.Tiers[a] = t
	}
	for _, p := range sp.Spec.Pins {
		in.Pins[p.Agent] = true
	}
	for _, h := range sp.Spec.Holds {
		in.Holds[h] = true
	}
	src := "Probe"
	for _, p := range probe {
		in.Providers[p.Provider] = rotation.Reading{Percent: float64(p.UsedPercent), Note: p.Note}
	}
	if sp.Spec.RotationUsageSource == "UsagePool" {
		src = "UsagePool"
		for prov, pct := range pools {
			rd := in.Providers[prov]
			rd.Percent, rd.Note = pct, "usagepool"
			in.Providers[prov] = rd
		}
	}
	return in, src
}

// rotate computes the plan and, in Enforce with an Actuator, applies it.
func (r *HiveSpokeReconciler) rotate(ctx context.Context, sp *hivev1.HiveSpoke, mode hivev1.ReconcileMode, probe []hivev1.ProviderState) {
	ladderName := sp.Spec.LadderRef
	if ladderName == "" {
		ladderName = "fleet"
	}
	var ladder hivev1.ModelLadder
	if err := r.Get(ctx, client.ObjectKey{Name: ladderName}, &ladder); err != nil {
		apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "RotationPlanned", Status: metav1.ConditionFalse,
			Reason: "NoLadder", Message: err.Error(), ObservedGeneration: sp.Generation})
		return
	}
	in, src := buildRotationInput(sp, probe, r.poolReadings(ctx), ladderRungs(ladder.Status.Effective), time.Now())
	plan := rotation.Compute(in)
	sp.Status.RotationPlanText = plan.Lines(true)
	sp.Status.RotationInputs = fmt.Sprintf("%s readings, ladder %s (%d rungs)", src, ladderName, len(in.Rungs))

	enforce := mode == hivev1.ModeEnforce && r.Actuator != nil
	if mode == hivev1.ModeEnforce && r.Actuator == nil {
		apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "RotationEnforced", Status: metav1.ConditionFalse,
			Reason: "NoActuator", ObservedGeneration: sp.Generation,
			Message: "rotationMode is Enforce but no actuator is wired in this build; planning as Shadow"})
	}
	counts := map[string]int{}
	for _, d := range plan.Decisions {
		if !d.Mutates() {
			continue
		}
		rd := hivev1.RotationDecision{Agent: d.Agent, Action: d.Action,
			FromProvider: d.From.Provider, FromBackend: d.From.Backend, FromModel: d.From.Model,
			ToProvider: d.To.Provider, ToBackend: d.To.Backend, ToModel: d.To.Model, ToEffort: d.ToEffort,
			Reason: d.Reason}
		if enforce {
			if err := r.Actuator.Apply(ctx, sp.Spec.Namespace, d); err != nil {
				rd.Error = err.Error()
			} else {
				rd.Applied = true
			}
		}
		counts[d.Action]++
		sp.Status.RotationPlan = append(sp.Status.RotationPlan, rd)
		metrics.Action(spokeController, sp.Name, "rotate-"+d.Action, rd.Applied)
	}
	metrics.RotationDecisions.DeletePartialMatch(map[string]string{"spoke": sp.Name})
	for _, a := range []string{rotation.ActionMove, rotation.ActionStrand, rotation.ActionResume, rotation.ActionCanary} {
		metrics.RotationDecisions.WithLabelValues(sp.Name, a, strconv.FormatBool(enforce)).Set(float64(counts[a]))
	}
	apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "RotationPlanned", Status: metav1.ConditionTrue,
		Reason: "Planned", Message: fmt.Sprintf("%d change(s) from %s", plan.Changes, sp.Status.RotationInputs),
		ObservedGeneration: sp.Generation})
}
