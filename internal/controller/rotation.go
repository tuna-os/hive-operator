package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// Contributor Deployments are scaled by the primary spoke in Enforce, and a
// primary in Enforce republishes hive/hive-provider-usage for the consumers
// that still read it (hive-console, the bash jobs of spokes not yet
// promoted).
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=usagepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=usagepools/status,verbs=get;update;patch

const (
	usageCM = "hive-provider-usage"
	// spokeUsageMaxAge is how old a published ConfigMap reading a spoke may
	// reuse (hive-rotate.sh USAGE_MAX_AGE for spoke apply: 1200 s).
	spokeUsageMaxAge = 20 * time.Minute
	// poolReadingMaxAge: a pool reading older than this is ignored (the pool
	// reconciles every 2 minutes; a wedged pool must not serve yesterday).
	poolReadingMaxAge = 10 * time.Minute
)

// boundAPI binds hiveclient to one spoke's pod and dashboard token.
type boundAPI struct {
	c          *hiveclient.Client
	pod, token string
}

func (b boundAPI) Place(ctx context.Context, ns, agent string, p hiveclient.Placement) (string, error) {
	return b.c.Place(ctx, ns, b.pod, b.token, agent, p)
}

func (b boundAPI) Post(ctx context.Context, ns, path string) (string, error) {
	return b.c.Post(ctx, ns, b.pod, b.token, path)
}

// rotationReadings returns provider → reading in the probe shape, from each
// provider's UsagePool (ccleft, reduced as ccleft_probe does) and, per
// provider, the hive-provider-usage ConfigMap as the fallback.
func (r *HiveSpokeReconciler) rotationReadings(ctx context.Context, sp *hivev1.HiveSpoke, now time.Time) (map[string]rotation.Reading, map[string]string) {
	pools := map[string]*hivev1.UsagePool{}
	if sp.Spec.RotationUsageSource != "Probe" {
		var list hivev1.UsagePoolList
		if err := r.List(ctx, &list); err == nil {
			sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
			for i := range list.Items {
				p := &list.Items[i]
				if _, dup := pools[p.Spec.Provider]; !dup {
					pools[p.Spec.Provider] = p
				}
			}
		}
	}
	var cm corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: sp.Spec.Namespace, Name: usageCM}, &cm)
	if err != nil && sp.Spec.Namespace != "hive" {
		err = r.Get(ctx, client.ObjectKey{Namespace: "hive", Name: usageCM}, &cm)
	}
	cmFresh := false
	if err == nil {
		if at, perr := time.Parse(time.RFC3339, cm.Data["updated_at"]); perr == nil && now.Sub(at) <= spokeUsageMaxAge {
			cmFresh = true
		}
	}
	readings, sources := map[string]rotation.Reading{}, map[string]string{}
	for _, p := range rotation.Providers {
		if pool := pools[p]; pool != nil {
			rr := pool.Status.RotationReading
			if rr != nil && rr.Source != "none" && rr.ComputedAt != nil && now.Sub(rr.ComputedAt.Time) <= poolReadingMaxAge {
				readings[p] = rotation.Reading{Percent: float64(rr.Percent), Note: rr.Note}
				sources[p] = rr.Source
				continue
			}
		}
		if cmFresh {
			v, ok := cm.Data[p]
			if !ok {
				v = "unknown unpublished"
			}
			l := usage.PublishedToProbe(v)
			readings[p] = rotation.Reading{Percent: float64(l.Percent), Note: l.Note}
			sources[p] = "configmap"
			continue
		}
		readings[p] = rotation.Reading{Percent: -1, Note: "unmeasured (no fresh UsagePool or ConfigMap reading)"}
		sources[p] = "none"
	}
	return readings, sources
}

func providerStates(sp *hivev1.HiveSpoke, readings map[string]rotation.Reading) []hivev1.ProviderState {
	out := make([]hivev1.ProviderState, 0, len(rotation.Providers))
	for _, p := range rotation.Providers {
		rd := readings[p]
		st := hivev1.ProviderState{Provider: p, UsedPercent: int32(rd.Percent), Note: rd.Note}
		if pr := usage.ParseProbeValue("0% used " + rd.Note); !pr.ResetsAt.IsZero() {
			st.ResetsAt = &metav1.Time{Time: pr.ResetsAt}
		}
		metrics.ProviderUsedPercent.WithLabelValues(sp.Name, p).Set(rd.Percent)
		out = append(out, st)
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
	if spec.KiroMinCadenceSeconds > 0 {
		pol.KiroMinCadenceS = int(spec.KiroMinCadenceSeconds)
	}
	if spec.CanaryCooldownMinutes > 0 {
		pol.CanaryCooldown = time.Duration(spec.CanaryCooldownMinutes) * time.Minute
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

func rotationInterval(sp *hivev1.HiveSpoke) time.Duration {
	if sp.Spec.Rotation != nil && sp.Spec.Rotation.IntervalMinutes > 0 {
		return time.Duration(sp.Spec.Rotation.IntervalMinutes) * time.Minute
	}
	return 20 * time.Minute
}

// ladderRungs flattens the effective ladder into tier members, de-duplicated
// on tier+provider+model like tier_members.
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

func isPrimary(sp *hivev1.HiveSpoke) bool {
	return sp.Spec.PrimarySpoke == "" || sp.Spec.PrimarySpoke == sp.Spec.Namespace || sp.Spec.PrimarySpoke == sp.Name
}

func primaryNamespace(sp *hivev1.HiveSpoke) string {
	if sp.Spec.PrimarySpoke == "" || sp.Spec.PrimarySpoke == sp.Name {
		return sp.Spec.Namespace
	}
	return sp.Spec.PrimarySpoke
}

func spokeTiers(sp *hivev1.HiveSpoke) map[string]string {
	if len(sp.Spec.AgentTiers) > 0 {
		return sp.Spec.AgentTiers
	}
	return agentTiers
}

func journal(sp *hivev1.HiveSpoke) *hivev1.RotationJournal {
	if sp.Status.Journal == nil {
		sp.Status.Journal = &hivev1.RotationJournal{}
	}
	return sp.Status.Journal
}

func placementsOf(rows []hivev1.JournalPlacement) map[string]rotation.Placement {
	out := map[string]rotation.Placement{}
	for _, r := range rows {
		out[r.Agent] = rotation.Placement{Provider: r.Provider, Backend: r.Backend, Model: r.Model}
	}
	return out
}

func setRow(rows []hivev1.JournalPlacement, row hivev1.JournalPlacement) []hivev1.JournalPlacement {
	return append(dropRow(rows, row.Agent), row)
}

func dropRow(rows []hivev1.JournalPlacement, agent string) []hivev1.JournalPlacement {
	out := rows[:0:0]
	for _, r := range rows {
		if r.Agent != agent {
			out = append(out, r)
		}
	}
	return out
}

// buildRotationInput assembles the planner's world from spoke status.
func buildRotationInput(sp *hivev1.HiveSpoke, readings map[string]rotation.Reading, sources map[string]string,
	rungs []rotation.Rung, now time.Time, mode hivev1.ReconcileMode) rotation.Input {
	in := rotation.Input{
		Now: now, Namespace: sp.Spec.Namespace, Primary: isPrimary(sp), PrimaryNamespace: primaryNamespace(sp),
		Providers: readings, Tiers: spokeTiers(sp), Rungs: rungs,
		Pins: map[string]bool{}, Holds: map[string]bool{}, Policy: policyFrom(sp.Spec.Rotation),
		Stranded: map[string]rotation.Placement{}, PaceDemoted: map[string]rotation.Placement{},
		KiroEvict: map[string]rotation.KiroEvict{}, CanaryCooldown: map[string]time.Time{},
	}
	if sources["openai"] == "ccleft" {
		in.UsageSource = "ccleft"
	}
	// Shadow cannot see bash's journals, so it infers pace demotions; an
	// Enforce spoke owns its journal (seeded from inference at promotion).
	in.Policy.InferPaceDemotion = mode != hivev1.ModeEnforce
	for _, a := range sp.Status.Agents {
		in.Agents = append(in.Agents, rotation.Agent{Name: a.Name, CLI: a.Backend, Model: a.Model, Effort: a.Effort,
			Paused: a.Paused, OnDemand: a.OnDemand, PausedTrigger: a.PausedTrigger, Cadence: a.Cadence})
	}
	for _, p := range sp.Spec.Pins {
		in.Pins[p.Agent] = true
	}
	for _, h := range sp.Spec.Holds {
		in.Holds[h] = true
	}
	if j := sp.Status.Journal; j != nil {
		in.Stranded = placementsOf(j.Stranded)
		in.PaceDemoted = placementsOf(j.PaceDemoted)
		for _, e := range j.KiroEvict {
			in.KiroEvict[e.Agent] = rotation.KiroEvict{Expiry: e.Expiry.Time, Targets: e.Targets}
		}
		for _, c := range j.CanaryCooldown {
			in.CanaryCooldown[c.Provider] = c.Until.Time
		}
	}
	return in
}

// seedPaceJournal: on an Enforce spoke, an agent sitting below a tier rung
// on that rung's demotion chain with no journal row gets an inferred row —
// the bash pace-demoted file is not imported, and without a row the pacer
// could never restore it and rotation would read it as off-tier.
func seedPaceJournal(sp *hivev1.HiveSpoke, rungs []rotation.Rung, now time.Time) int {
	j := journal(sp)
	have := placementsOf(j.PaceDemoted)
	tiers := spokeTiers(sp)
	n := 0
	for _, a := range sp.Status.Agents {
		if _, ok := have[a.Name]; ok || tiers[a.Name] == "" {
			continue
		}
		var members []rotation.Rung
		for _, r := range rungs {
			if r.Tier == tiers[a.Name] {
				members = append(members, r)
			}
		}
		p, ok := rotation.PaceDemotedFrom(nil, true, members, rotation.Agent{Name: a.Name, CLI: a.Backend, Model: a.Model})
		if !ok {
			continue
		}
		at := metav1.NewTime(now)
		j.PaceDemoted = append(j.PaceDemoted, hivev1.JournalPlacement{Agent: a.Name, Provider: p.Provider, Backend: p.Backend,
			Model: p.Model, At: &at, Inferred: true})
		n++
	}
	return n
}

func decisionStatus(d rotation.Decision) hivev1.RotationDecision {
	return hivev1.RotationDecision{Agent: d.Agent, Action: d.Action,
		FromProvider: d.From.Provider, FromBackend: d.From.Backend, FromModel: d.From.Model,
		ToProvider: d.To.Provider, ToBackend: d.To.Backend, ToModel: d.To.Model, ToEffort: d.ToEffort,
		Reason: d.Reason}
}

// rotate computes the plan and, in Enforce, applies it at most once per
// rotation interval.
func (r *HiveSpokeReconciler) rotate(ctx context.Context, sp *hivev1.HiveSpoke, mode hivev1.ReconcileMode,
	readings map[string]rotation.Reading, sources map[string]string, act rotation.Actuator, now time.Time) {
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
	rungs := ladderRungs(ladder.Status.Effective)
	enforce := mode == hivev1.ModeEnforce && act != nil
	if enforce {
		if n := seedPaceJournal(sp, rungs, now); n > 0 {
			apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "PaceJournalSeeded", Status: metav1.ConditionTrue,
				Reason: "Inferred", ObservedGeneration: sp.Generation,
				Message: fmt.Sprintf("%d pace-demoted row(s) inferred from the live placement at promotion", n)})
		}
	}
	in := buildRotationInput(sp, readings, sources, rungs, now, mode)
	plan := rotation.Compute(in)
	sp.Status.RotationPlanText = plan.Lines(true)
	srcCount := map[string]int{}
	for _, s := range sources {
		srcCount[s]++
	}
	sp.Status.RotationInputs = fmt.Sprintf("readings %s, ladder %s (%d rungs)", fmtCounts(srcCount), ladderName, len(rungs))

	if mode == hivev1.ModeEnforce && act == nil {
		apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "RotationEnforced", Status: metav1.ConditionFalse,
			Reason: "NoActuator", ObservedGeneration: sp.Generation,
			Message: "rotationMode is Enforce but the hive API is unreachable this reconcile; planned as Shadow"})
	}
	due := sp.Status.RotationAppliedAt == nil || now.Sub(sp.Status.RotationAppliedAt.Time) >= rotationInterval(sp)-time.Minute
	apply := enforce && due
	counts := map[string]int{}
	var failed int
	for _, d := range plan.Decisions {
		if !d.Mutates() {
			continue
		}
		rd := decisionStatus(d)
		if apply {
			if err := act.Apply(ctx, sp.Spec.Namespace, d); err != nil {
				rd.Error = err.Error()
				failed++
			} else {
				rd.Applied = true
				applyRotationJournal(sp, in, d, now)
			}
		}
		counts[d.Action]++
		sp.Status.RotationPlan = append(sp.Status.RotationPlan, rd)
		metrics.Action(spokeController, sp.Name, "rotate-"+d.Action, rd.Applied)
	}

	sp.Status.ContributorPlanText = nil
	if in.Primary {
		r.contributors(ctx, sp, in, apply)
	}
	if apply {
		t := metav1.NewTime(now)
		sp.Status.RotationAppliedAt = &t
		pruneCooldowns(sp, now)
		r.publishUsage(ctx, sp, readings, sources, now)
		st, reason, msg := metav1.ConditionTrue, "Applied", fmt.Sprintf("%d change(s) applied", plan.Changes)
		if failed > 0 {
			st, reason, msg = metav1.ConditionFalse, "PartialFailure", fmt.Sprintf("%d of %d mutation(s) failed", failed, len(sp.Status.RotationPlan))
		}
		apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "RotationEnforced", Status: st,
			Reason: reason, Message: msg, ObservedGeneration: sp.Generation})
	}
	metrics.RotationDecisions.DeletePartialMatch(map[string]string{"spoke": sp.Name})
	for _, a := range []string{rotation.ActionMove, rotation.ActionStrand, rotation.ActionResume, rotation.ActionCanary} {
		metrics.RotationDecisions.WithLabelValues(sp.Name, a, strconv.FormatBool(apply)).Set(float64(counts[a]))
	}
	apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "RotationPlanned", Status: metav1.ConditionTrue,
		Reason: "Planned", Message: fmt.Sprintf("%d change(s) from %s", plan.Changes, sp.Status.RotationInputs),
		ObservedGeneration: sp.Generation})

	r.pace(ctx, sp, mode, in, rungs, act, now)
}

func fmtCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// applyRotationJournal records a successfully applied decision's journal
// effects, as hive-rotate.sh does after each API call.
func applyRotationJournal(sp *hivev1.HiveSpoke, in rotation.Input, d rotation.Decision, now time.Time) {
	j := journal(sp)
	at := metav1.NewTime(now)
	if d.ClearsStrand {
		j.Stranded = dropRow(j.Stranded, d.Agent)
	}
	if d.RecordsStrand {
		j.Stranded = setRow(j.Stranded, hivev1.JournalPlacement{Agent: d.Agent, Provider: d.From.Provider,
			Backend: d.From.Backend, Model: d.From.Model, At: &at})
	}
	if d.CooldownProvider != "" {
		until := metav1.NewTime(now.Add(in.Policy.CanaryCooldown))
		var out []hivev1.ProviderCooldown
		for _, c := range j.CanaryCooldown {
			if c.Provider != d.CooldownProvider {
				out = append(out, c)
			}
		}
		j.CanaryCooldown = append(out, hivev1.ProviderCooldown{Provider: d.CooldownProvider, Until: until})
	}
}

func pruneCooldowns(sp *hivev1.HiveSpoke, now time.Time) {
	j := journal(sp)
	var cc []hivev1.ProviderCooldown
	for _, c := range j.CanaryCooldown {
		if c.Until.After(now) {
			cc = append(cc, c)
		}
	}
	j.CanaryCooldown = cc
	var ev []hivev1.KiroEvictRequest
	for _, e := range j.KiroEvict {
		if e.Expiry.After(now) {
			ev = append(ev, e)
		}
	}
	j.KiroEvict = ev
}

// contributors plans (and in Enforce applies) reconcile_contributors.
func (r *HiveSpokeReconciler) contributors(ctx context.Context, sp *hivev1.HiveSpoke, in rotation.Input, apply bool) {
	ns := sp.Spec.ContributorNamespace
	if ns == "" {
		ns = "hive-contributors"
	}
	var deps []rotation.ContribDeploy
	if r.Hive != nil && r.Hive.Clientset() != nil {
		list, err := r.Hive.Clientset().AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			sp.Status.ContributorPlanText = []string{"contributors: " + err.Error()}
			return
		}
		for _, d := range list.Items {
			cd := rotation.ContribDeploy{Name: d.Name}
			if d.Spec.Replicas != nil {
				cd.Replicas = *d.Spec.Replicas
			}
			if cs := d.Spec.Template.Spec.Containers; len(cs) > 0 {
				for _, e := range cs[0].Env {
					switch e.Name {
					case "AGENT_BACKEND":
						cd.Backend = e.Value
					case "AGENT_MODEL":
						cd.Model = e.Value
					}
				}
			}
			deps = append(deps, cd)
		}
	}
	for _, d := range rotation.Contributors(in, deps, ns) {
		line := d.Line
		if d.Action == rotation.ActionScale && apply {
			want, _ := strconv.Atoi(d.To.Model)
			if err := r.Hive.Scale(ctx, ns, d.Agent, int32(want)); err != nil {
				line += "\n    ! scale failed for " + d.Agent + ": " + err.Error()
			}
		}
		sp.Status.ContributorPlanText = append(sp.Status.ContributorPlanText, line)
	}
}

// publishUsage mirrors the readings into hive/hive-provider-usage (primary,
// Enforce only), as the primary hive-rotate job did, for the consumers that
// still read it. Best-effort, and only when the readings came from ccleft.
func (r *HiveSpokeReconciler) publishUsage(ctx context.Context, sp *hivev1.HiveSpoke, readings map[string]rotation.Reading, sources map[string]string, now time.Time) {
	if !isPrimary(sp) {
		return
	}
	data := map[string]string{}
	for _, p := range rotation.Providers {
		if sources[p] != "ccleft" {
			return
		}
		rd := readings[p]
		data[p] = usage.ProbeLine{Percent: int(rd.Percent), Note: rd.Note}.Published()
	}
	data["updated_at"] = now.UTC().Format(time.RFC3339)
	data["measured_by"] = sp.Spec.Namespace
	data["source"] = "ccleft"
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: "hive", Name: usageCM}
	if err := r.Get(ctx, key, &cm); err != nil {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: data}
		_ = r.Create(ctx, &cm)
		return
	}
	if v, ok := cm.Data["anthropic_limits"]; ok {
		data["anthropic_limits"] = v
	}
	cm.Data = data
	_ = r.Update(ctx, &cm)
}
