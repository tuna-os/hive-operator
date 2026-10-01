package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// ── Pool side: history, verdicts (hive-pace record + compute) ───────────

func fstr(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func fparse(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

func decimal(s string, def float64) float64 {
	if v := fparse(s); v != nil {
		return *v
	}
	return def
}

// encodeSample / decodeSample: "<ts> <slot> <pct> <reset|-> [<used> <limit>]".
func encodeSample(s usage.PaceSample) string {
	reset := "-"
	if s.Reset != 0 {
		reset = strconv.FormatInt(s.Reset, 10)
	}
	out := fmt.Sprintf("%d %s %s %s", s.TS, s.Slot, strconv.FormatFloat(s.Pct, 'f', -1, 64), reset)
	if s.HasUsed {
		out += " " + strconv.FormatFloat(s.Used, 'f', -1, 64) + " " + strconv.FormatFloat(s.Limit, 'f', -1, 64)
	}
	return out
}

func decodeSample(provider, line string) (usage.PaceSample, bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return usage.PaceSample{}, false
	}
	ts, err1 := strconv.ParseInt(f[0], 10, 64)
	pct, err2 := strconv.ParseFloat(f[2], 64)
	if err1 != nil || err2 != nil {
		return usage.PaceSample{}, false
	}
	s := usage.PaceSample{TS: ts, Provider: provider, Slot: f[1], Pct: pct}
	if f[3] != "-" {
		s.Reset, _ = strconv.ParseInt(f[3], 10, 64)
	}
	if len(f) >= 6 {
		s.Used, _ = strconv.ParseFloat(f[4], 64)
		s.Limit, _ = strconv.ParseFloat(f[5], 64)
		s.HasUsed = true
	}
	return s, true
}

func poolHistory(pool *hivev1.UsagePool) []usage.PaceSample {
	var out []usage.PaceSample
	for _, l := range pool.Status.PaceHistory {
		if s, ok := decodeSample(pool.Spec.Provider, l); ok {
			out = append(out, s)
		}
	}
	return out
}

func paceSpec(pool *hivev1.UsagePool) hivev1.PoolPaceSpec {
	if pool.Spec.Pace != nil {
		return *pool.Spec.Pace
	}
	return hivev1.PoolPaceSpec{}
}

func fitConfig(ps hivev1.PoolPaceSpec) rotation.FitConfig {
	c := rotation.DefaultFitConfig()
	c.Deadband = decimal(ps.Deadband, c.Deadband)
	if ps.MinSamples > 0 {
		c.MinSamples = int(ps.MinSamples)
	}
	if ps.MinSpanSeconds > 0 {
		c.MinSpan = time.Duration(ps.MinSpanSeconds) * time.Second
	}
	return c
}

func kiroConfig(ps hivev1.PoolPaceSpec) rotation.KiroBudgetConfig {
	c := rotation.DefaultKiroBudgetConfig()
	c.Safety = decimal(ps.KiroSafety, c.Safety)
	c.Hot = decimal(ps.KiroHot, c.Hot)
	c.Cold = decimal(ps.KiroCold, c.Cold)
	if ps.KiroWindowSeconds > 0 {
		c.Window = time.Duration(ps.KiroWindowSeconds) * time.Second
	}
	if ps.KiroMinSamples > 0 {
		c.MinSamples = int(ps.KiroMinSamples)
	}
	if ps.KiroMinSpanSeconds > 0 {
		c.MinSpan = time.Duration(ps.KiroMinSpanSeconds) * time.Second
	}
	if ps.KiroMaxReadingAgeSeconds > 0 {
		c.MaxReadingAge = time.Duration(ps.KiroMaxReadingAgeSeconds) * time.Second
	}
	return c
}

// recordSamples appends new samples (pace_history_add: one row per
// provider/slot/ts), at most one per SampleInterval per slot, and prunes to
// HistoryHours.
func recordSamples(pool *hivev1.UsagePool, samples []usage.PaceSample, now time.Time) {
	ps := paceSpec(pool)
	interval := int64(1200)
	if pool.Spec.Provider == "kiro" {
		interval = 0
	}
	if ps.SampleIntervalSeconds != nil {
		interval = int64(*ps.SampleIntervalSeconds)
	}
	hist := poolHistory(pool)
	last := map[string]int64{}
	seen := map[string]bool{}
	for _, s := range hist {
		seen[fmt.Sprintf("%d|%s", s.TS, s.Slot)] = true
		if s.TS > last[s.Slot] {
			last[s.Slot] = s.TS
		}
	}
	for _, s := range samples {
		k := fmt.Sprintf("%d|%s", s.TS, s.Slot)
		if seen[k] || (last[s.Slot] != 0 && s.TS-last[s.Slot] < interval) || s.TS <= last[s.Slot] && last[s.Slot] != 0 {
			continue
		}
		seen[k] = true
		last[s.Slot] = s.TS
		hist = append(hist, s)
	}
	hours := int64(60)
	if ps.HistoryHours > 0 {
		hours = int64(ps.HistoryHours)
	}
	cutoff := now.Unix() - hours*3600
	sort.SliceStable(hist, func(i, j int) bool { return hist[i].TS < hist[j].TS })
	pool.Status.PaceHistory = nil
	for _, s := range hist {
		if s.TS >= cutoff {
			pool.Status.PaceHistory = append(pool.Status.PaceHistory, encodeSample(s))
		}
	}
	if n := len(pool.Status.PaceHistory); n > 1200 {
		pool.Status.PaceHistory = pool.Status.PaceHistory[n-1200:]
	}
}

// computePace fills status.pace from the history (compute() + kiro_budget()).
func computePace(pool *hivev1.UsagePool, now time.Time) {
	ps := paceSpec(pool)
	hist := poolHistory(pool)
	prev := pool.Status.Pace
	st := &hivev1.PoolPaceStatus{Verdict: "no-data", ComputedAt: metaTime(now)}
	if prev != nil {
		st.LastActuation = prev.LastActuation
	}
	v := rotation.Fit(hist, now, fitConfig(ps))
	if pv, ok := v[pool.Spec.Provider]; ok {
		st.Verdict, st.Pressure, st.BindingSlot = pv.Verdict, fstr(pv.Pressure), pv.BindingSlot
		slots := make([]string, 0, len(pv.Slots))
		for s := range pv.Slots {
			slots = append(slots, s)
		}
		sort.Strings(slots)
		for _, s := range slots {
			f := pv.Slots[s]
			st.Slots = append(st.Slots, hivev1.PaceSlotStatus{Slot: s, Pct: strconv.FormatFloat(f.Pct, 'f', -1, 64), Reset: f.Reset,
				Samples: int32(f.Samples), SpanSeconds: f.SpanS, HoursLeft: fstr(f.HoursLeft), AllowedRate: fstr(f.AllowedRate),
				ObservedRate: fstr(f.ObservedRate), Ratio: fstr(f.Ratio)})
		}
	}
	for _, l := range rotation.VerdictTable(v)[1:] {
		if strings.HasPrefix(l, pool.Spec.Provider+" ") {
			st.Row = l
		}
	}
	if pool.Spec.Provider == "kiro" {
		var lastAct int64
		if st.LastActuation != nil {
			lastAct = st.LastActuation.Unix()
		}
		kb := rotation.ComputeKiroBudget(hist, now, lastAct, kiroConfig(ps))
		st.KiroBudget = &hivev1.KiroBudgetStatus{Verdict: kb.Verdict, Used: strconv.FormatFloat(kb.Used, 'f', -1, 64),
			Limit: strconv.FormatFloat(kb.Limit, 'f', -1, 64), Remaining: strconv.FormatFloat(kb.Remaining, 'f', -1, 64),
			Reset: kb.Reset, ReadingAgeSeconds: kb.ReadingAgeS, Safety: strconv.FormatFloat(kb.Safety, 'f', -1, 64),
			HoursLeft: fstr(kb.HoursLeft), Allowed: fstr(kb.Allowed), Burn: fstr(kb.Burn), Burn6h: fstr(kb.Burn6h),
			Ratio: fstr(kb.Ratio), Samples: int32(kb.Samples), SpanSeconds: kb.SpanS, WindowStart: kb.WindowStart, Line: kb.Line()}
	}
	pool.Status.Pace = st
}

func verdictFromStatus(provider string, st *hivev1.PoolPaceStatus) (rotation.PaceVerdict, bool) {
	if st == nil || st.Verdict == "" || st.Verdict == "no-data" {
		return rotation.PaceVerdict{}, false
	}
	pv := rotation.PaceVerdict{Verdict: st.Verdict, Pressure: fparse(st.Pressure), BindingSlot: st.BindingSlot, Slots: map[string]rotation.SlotFit{}}
	for _, s := range st.Slots {
		pv.Slots[s.Slot] = rotation.SlotFit{Pct: decimal(s.Pct, 0), Reset: s.Reset, Samples: int(s.Samples), SpanS: s.SpanSeconds,
			HoursLeft: fparse(s.HoursLeft), AllowedRate: fparse(s.AllowedRate), ObservedRate: fparse(s.ObservedRate), Ratio: fparse(s.Ratio)}
	}
	return pv, true
}

func kiroFromStatus(st *hivev1.PoolPaceStatus) rotation.KiroBudget {
	if st == nil || st.KiroBudget == nil {
		return rotation.KiroBudget{Verdict: "no-data"}
	}
	k := st.KiroBudget
	return rotation.KiroBudget{Verdict: k.Verdict, Used: decimal(k.Used, 0), Limit: decimal(k.Limit, 0), Remaining: decimal(k.Remaining, 0),
		Reset: k.Reset, ReadingAgeS: k.ReadingAgeSeconds, Safety: decimal(k.Safety, 0), HoursLeft: fparse(k.HoursLeft),
		Allowed: fparse(k.Allowed), Burn: fparse(k.Burn), Burn6h: fparse(k.Burn6h), Ratio: fparse(k.Ratio),
		Samples: int(k.Samples), SpanS: k.SpanSeconds, WindowStart: k.WindowStart}
}

// ── Spoke side: the fleet plan and this spoke's share of it ─────────────

func paceConfig(sp *hivev1.HiveSpoke) (rotation.PaceConfig, time.Duration) {
	c := rotation.DefaultPaceConfig()
	iv := 20 * time.Minute
	p := sp.Spec.Pace
	if p == nil {
		return c, iv
	}
	if p.IntervalMinutes > 0 {
		iv = time.Duration(p.IntervalMinutes) * time.Minute
	}
	c.KiroBudget = !p.DisableKiroBudget
	c.KiroPromoteMax = decimal(p.KiroPromoteMax, c.KiroPromoteMax)
	if p.KiroMaxDemote > 0 {
		c.KiroMaxDemote = int(p.KiroMaxDemote)
	}
	if p.KiroMaxPromote > 0 {
		c.KiroMaxPromote = int(p.KiroMaxPromote)
	}
	if p.KiroMaxEvict > 0 {
		c.KiroMaxEvict = int(p.KiroMaxEvict)
	}
	if p.KiroEvictTTLSeconds > 0 {
		c.KiroEvictTTL = time.Duration(p.KiroEvictTTLSeconds) * time.Second
	}
	if p.EvictTargetMaxPct > 0 {
		c.EvictTargetMaxPct = int(p.EvictTargetMaxPct)
	}
	return c, iv
}

func spokeMode(sp *hivev1.HiveSpoke) hivev1.ReconcileMode {
	if sp.Spec.RotationMode == "" {
		return hivev1.ModeShadow
	}
	return sp.Spec.RotationMode
}

// pace runs one pace tick when due: the FLEET-wide plan (hive-pace.sh runs
// once over every hive) from every spoke's observed agents and journals,
// the pools' verdicts and Kiro budget; this spoke records the full text and
// — in Enforce — applies the decisions for its own agents.
func (r *HiveSpokeReconciler) pace(ctx context.Context, sp *hivev1.HiveSpoke, mode hivev1.ReconcileMode, rin rotation.Input,
	rungs []rotation.Rung, act rotation.Actuator, now time.Time) {
	if sp.Spec.Pace != nil && sp.Spec.Pace.Disabled {
		sp.Status.PacePlan, sp.Status.PacePlanText, sp.Status.PaceTickAt = nil, nil, nil
		return
	}
	cfg, iv := paceConfig(sp)
	if sp.Status.PaceTickAt != nil && now.Sub(sp.Status.PaceTickAt.Time) < iv-time.Minute {
		return
	}
	var spokes hivev1.HiveSpokeList
	if err := r.List(ctx, &spokes); err != nil {
		return
	}
	items := spokes.Items
	for i := range items {
		if items[i].Name == sp.Name {
			items[i] = *sp // our freshest observation and journal
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		oi, oj := int32(0), int32(0)
		if items[i].Spec.Pace != nil {
			oi = items[i].Spec.Pace.FleetOrder
		}
		if items[j].Spec.Pace != nil {
			oj = items[j].Spec.Pace.FleetOrder
		}
		if oi != oj {
			return oi < oj
		}
		return items[i].Spec.Namespace < items[j].Spec.Namespace
	})
	in := rotation.PaceInput{Now: now, Config: cfg, Readings: rin.Providers, Verdicts: map[string]rotation.PaceVerdict{},
		Demoted: map[string]rotation.Placement{}, KiroEvict: map[string]rotation.KiroEvict{}, Pins: map[string]bool{}}
	for i := range items {
		s := &items[i]
		if !s.Status.Reachable && s.Name != sp.Name {
			continue
		}
		tiers := spokeTiers(s)
		enforce := spokeMode(s) == hivev1.ModeEnforce
		for _, a := range s.Status.Agents {
			fa := rotation.FleetAgent{Namespace: s.Spec.Namespace, Agent: rotation.Agent{Name: a.Name, CLI: a.Backend, Model: a.Model,
				Effort: a.Effort, Paused: a.Paused, OnDemand: a.OnDemand, PausedTrigger: a.PausedTrigger, Cadence: a.Cadence}}
			// Inference stands in for bash's journal on Shadow spokes only;
			// an Enforce spoke's journal is authoritative ("restore only
			// own demotions"). No members = no inference.
			if !enforce && tiers[a.Name] != "" {
				for _, r := range rungs {
					if r.Tier == tiers[a.Name] {
						fa.Members = append(fa.Members, r)
					}
				}
			}
			in.Fleet = append(in.Fleet, fa)
		}
		for _, p := range s.Spec.Pins {
			in.Pins[s.Spec.Namespace+"/"+p.Agent] = true
		}
		if j := s.Status.Journal; j != nil {
			for _, row := range j.PaceDemoted {
				in.Demoted[s.Spec.Namespace+"/"+row.Agent] = rotation.Placement{Provider: row.Provider, Backend: row.Backend, Model: row.Model}
			}
			for _, e := range j.KiroEvict {
				in.KiroEvict[s.Spec.Namespace+"/"+e.Agent] = rotation.KiroEvict{Expiry: e.Expiry.Time, Targets: e.Targets}
			}
		}
	}
	var pools hivev1.UsagePoolList
	poolOf := map[string]*hivev1.UsagePool{}
	if err := r.List(ctx, &pools); err == nil {
		sort.Slice(pools.Items, func(i, j int) bool { return pools.Items[i].Name < pools.Items[j].Name })
		for i := range pools.Items {
			p := &pools.Items[i]
			if _, dup := poolOf[p.Spec.Provider]; dup {
				continue
			}
			poolOf[p.Spec.Provider] = p
			if v, ok := verdictFromStatus(p.Spec.Provider, p.Status.Pace); ok {
				in.Verdicts[p.Spec.Provider] = v
			}
		}
	}
	in.Kiro = rotation.KiroBudget{Verdict: "no-data"}
	if kp := poolOf["kiro"]; kp != nil {
		in.Kiro = kiroFromStatus(kp.Status.Pace)
	}
	plan := rotation.PlanPace(in)

	text := rotation.VerdictTable(in.Verdicts)
	text = append(text, fmt.Sprintf("controls %d agents across: %s", len(in.Fleet), fleetNamespaces(items)))
	if cfg.KiroBudget {
		text = append(text, in.Kiro.Line())
	}
	sp.Status.PacePlanText = append(text, plan.Text()...)
	sp.Status.PacePlan = nil
	t := metav1.NewTime(now)
	sp.Status.PaceTickAt = &t

	apply := mode == hivev1.ModeEnforce && act != nil
	kiroApplied := false
	clearedEvicts := false
	for _, d := range plan.Decisions {
		if d.Namespace != sp.Spec.Namespace {
			continue
		}
		rd := decisionStatus(d.Decision)
		if apply {
			prov := d.From.Provider
			generic := !(prov == "kiro" && cfg.KiroBudget)
			switch {
			case generic && !r.claimPaceNotch(ctx, poolOf[prov], now, iv):
				rd.Error = prov + " already took its pace notch this interval (another spoke)"
			case d.Action == rotation.ActionCap:
				rd.Applied = true
			default:
				if err := act.Apply(ctx, sp.Spec.Namespace, d.Decision); err != nil {
					rd.Error = err.Error()
				} else {
					rd.Applied = true
				}
			}
			if rd.Applied {
				applyPaceJournal(sp, d, now)
				if !generic {
					kiroApplied = true
				}
			}
		}
		sp.Status.PacePlan = append(sp.Status.PacePlan, rd)
		metrics.Action(spokeController, sp.Name, "pace-"+d.Action, rd.Applied)
	}
	if apply && plan.ClearEvicts {
		journal(sp).KiroEvict = nil
		clearedEvicts = true
	}
	if apply && kiroApplied && plan.KiroActed {
		r.touchPoolActuation(ctx, poolOf["kiro"], now)
	}
	if apply {
		msg := fmt.Sprintf("%d fleet change(s), %d for this spoke", plan.Changes, len(sp.Status.PacePlan))
		if clearedEvicts {
			msg += "; kiro cap requests withdrawn"
		}
		apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "PaceEnforced", Status: metav1.ConditionTrue,
			Reason: "Ticked", Message: msg, ObservedGeneration: sp.Generation})
	}
}

func fleetNamespaces(items []hivev1.HiveSpoke) string {
	var ns []string
	for _, s := range items {
		ns = append(ns, s.Spec.Namespace)
	}
	return strings.Join(ns, " ")
}

func applyPaceJournal(sp *hivev1.HiveSpoke, d rotation.PaceDecision, now time.Time) {
	j := journal(sp)
	at := metav1.NewTime(now)
	if d.SetDemoted != nil {
		j.PaceDemoted = setRow(j.PaceDemoted, hivev1.JournalPlacement{Agent: d.Agent, Provider: d.SetDemoted.Provider,
			Backend: d.SetDemoted.Backend, Model: d.SetDemoted.Model, At: &at})
	}
	if d.ClearDemoted {
		j.PaceDemoted = dropRow(j.PaceDemoted, d.Agent)
	}
	if d.SetEvict != nil {
		var out []hivev1.KiroEvictRequest
		for _, e := range j.KiroEvict {
			if e.Agent != d.Agent {
				out = append(out, e)
			}
		}
		j.KiroEvict = append(out, hivev1.KiroEvictRequest{Agent: d.Agent, Expiry: metav1.NewTime(d.SetEvict.Expiry), Targets: d.SetEvict.Targets})
	}
}

// claimPaceNotch enforces ONE notch per provider per pace interval across
// every Enforce spoke: the pool's lastActuation is the fleet-wide marker.
func (r *HiveSpokeReconciler) claimPaceNotch(ctx context.Context, pool *hivev1.UsagePool, now time.Time, iv time.Duration) bool {
	if pool == nil {
		return true
	}
	claimed := false
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur hivev1.UsagePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), &cur); err != nil {
			return err
		}
		if cur.Status.Pace == nil {
			cur.Status.Pace = &hivev1.PoolPaceStatus{Verdict: "no-data"}
		}
		if la := cur.Status.Pace.LastActuation; la != nil && now.Sub(la.Time) < iv-time.Minute {
			return nil
		}
		t := metav1.NewTime(now)
		cur.Status.Pace.LastActuation = &t
		if err := r.Status().Update(ctx, &cur); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed
}

// touchPoolActuation records KIRO_LAST_ACT on the kiro pool.
func (r *HiveSpokeReconciler) touchPoolActuation(ctx context.Context, pool *hivev1.UsagePool, now time.Time) {
	if pool == nil {
		return
	}
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur hivev1.UsagePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), &cur); err != nil {
			return err
		}
		if cur.Status.Pace == nil {
			cur.Status.Pace = &hivev1.PoolPaceStatus{Verdict: "no-data"}
		}
		t := metav1.NewTime(now)
		cur.Status.Pace.LastActuation = &t
		return r.Status().Update(ctx, &cur)
	})
}
