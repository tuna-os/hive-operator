package rotation

// Pace: a port of hive-pace.sh (live ConfigMap hive/hive-ops-scripts,
// 2026-10-01, 863 lines). Thresholds say whether a pool CAN serve; pacing
// says how FAST to spend it so it lands near-empty exactly at its reset:
//
//	pressure = max over a provider's limits of observed_rate / allowed_rate
//	allowed  = (100 - pct) / hours_to_reset
//
// fitted by least squares over the sample history (≥ MinSamples spanning
// ≥ MinSpan, else "learning"), with a deadband around 1.0. Hot → demote ONE
// agent ONE notch per provider per tick (RungDown); cold → restore ONE agent
// the pacer itself demoted. Kiro (monthly credits, overage disabled, nothing
// but these agents draws on it) is paced against its exact credit budget
// instead (KiroBudget / kiro levers below).

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/hive-operator/internal/usage"
)

// PacedPools are the pools the pacer controls (PACED_POOLS).
var PacedPools = []string{"anthropic", "google", "openai", "kiro"}

// pyRound approximates Python's round(x, n) (half-to-even).
func pyRound(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.RoundToEven(x*p) / p
}

func fptr(v float64) *float64 { return &v }

// SlotFit is one limit's fit (compute(), per provider/slot).
type SlotFit struct {
	Pct          float64
	Reset        int64 // 0 = no deadline
	Samples      int
	SpanS        int64
	HoursLeft    *float64
	AllowedRate  *float64
	ObservedRate *float64
	Ratio        *float64
	ExhaustsInH  *float64
}

// PaceVerdict is one provider's verdict: hot, cold, on-pace or learning.
type PaceVerdict struct {
	Verdict     string
	Pressure    *float64
	BindingSlot string
	Slots       map[string]SlotFit
}

// FitConfig holds HIVE_PACE_DEADBAND / _MIN_SAMPLES / _MIN_SPAN_S.
type FitConfig struct {
	Deadband   float64
	MinSamples int
	MinSpan    time.Duration
}

// DefaultFitConfig mirrors hive-pace.sh.
func DefaultFitConfig() FitConfig {
	return FitConfig{Deadband: 0.25, MinSamples: 3, MinSpan: time.Hour}
}

// Fit ports compute(): per provider, the verdict over its limits.
func Fit(history []usage.PaceSample, now time.Time, cfg FitConfig) map[string]PaceVerdict {
	type key struct{ p, s string }
	series := map[key][]usage.PaceSample{}
	var order []key
	for _, r := range history {
		k := key{r.Provider, r.Slot}
		if _, ok := series[k]; !ok {
			order = append(order, k)
		}
		series[k] = append(series[k], r)
	}
	out := map[string]map[string]SlotFit{}
	slotOrder := map[string][]string{}
	nowS := now.Unix()
	for _, k := range order {
		pts := append([]usage.PaceSample(nil), series[k]...)
		sort.SliceStable(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })
		// Window rollover: a drop of more than 5 points, or the reset moving
		// forward, starts a new window.
		cut := 0
		for i := 1; i < len(pts); i++ {
			if pts[i].Pct < pts[i-1].Pct-5 {
				cut = i
			} else if pts[i].Reset != 0 && pts[i-1].Reset != 0 && pts[i].Reset > pts[i-1].Reset+60 {
				cut = i
			}
		}
		pts = pts[cut:]
		latest := pts[len(pts)-1]
		var span int64
		if len(pts) > 1 {
			span = pts[len(pts)-1].TS - pts[0].TS
		}
		e := SlotFit{Pct: latest.Pct, Reset: latest.Reset, Samples: len(pts), SpanS: span}
		if latest.Reset != 0 {
			hrs := float64(latest.Reset-nowS) / 3600
			e.HoursLeft = fptr(pyRound(hrs, 2))
			if hrs > 0.05 {
				e.AllowedRate = fptr(pyRound((100-latest.Pct)/hrs, 2))
			}
		}
		if len(pts) >= cfg.MinSamples && span >= int64(cfg.MinSpan/time.Second) {
			n := float64(len(pts))
			var mx, my float64
			xs := make([]float64, len(pts))
			for i, p := range pts {
				xs[i] = float64(p.TS-pts[0].TS) / 3600
				mx += xs[i]
				my += p.Pct
			}
			mx, my = mx/n, my/n
			var num, den float64
			for i, p := range pts {
				num += (xs[i] - mx) * (p.Pct - my)
				den += (xs[i] - mx) * (xs[i] - mx)
			}
			slope := 0.0
			if den > 1e-9 {
				slope = num / den
			}
			e.ObservedRate = fptr(pyRound(slope, 2))
			if e.AllowedRate != nil && *e.AllowedRate > 0 {
				e.Ratio = fptr(pyRound(slope / *e.AllowedRate, 2))
			}
			if slope > 0.01 {
				e.ExhaustsInH = fptr(pyRound((100-latest.Pct)/slope, 2))
			}
		}
		if out[k.p] == nil {
			out[k.p] = map[string]SlotFit{}
		}
		out[k.p][k.s] = e
		slotOrder[k.p] = append(slotOrder[k.p], k.s)
	}
	verdicts := map[string]PaceVerdict{}
	for prov, slots := range out {
		var ratios []float64
		for _, s := range slotOrder[prov] {
			if r := slots[s].Ratio; r != nil {
				ratios = append(ratios, *r)
			}
		}
		if len(ratios) == 0 {
			verdicts[prov] = PaceVerdict{Verdict: "learning", Slots: slots}
			continue
		}
		pressure := ratios[0]
		for _, r := range ratios[1:] {
			pressure = math.Max(pressure, r)
		}
		// binding: max by (ratio or -1) — a 0.0 ratio counts as -1, as in
		// Python's `or`.
		binding, best := "", math.Inf(-1)
		for _, s := range slotOrder[prov] {
			v := -1.0
			if r := slots[s].Ratio; r != nil && *r != 0 {
				v = *r
			}
			if v > best {
				best, binding = v, s
			}
		}
		v := "on-pace"
		switch {
		case pressure > 1+cfg.Deadband:
			v = "hot"
		case pressure < 1-cfg.Deadband:
			v = "cold"
		}
		verdicts[prov] = PaceVerdict{Verdict: v, Pressure: fptr(pyRound(pressure, 2)), BindingSlot: binding, Slots: slots}
	}
	return verdicts
}

// KiroBudgetConfig holds the HIVE_PACE_KIRO_* budget knobs.
type KiroBudgetConfig struct {
	Window        time.Duration // _WINDOW_S, 3600
	MinSamples    int           // _MIN_SAMPLES, 3
	MinSpan       time.Duration // _MIN_SPAN_S, 1200
	Safety        float64       // _SAFETY, 0.85
	Hot           float64       // _HOT, 1.0
	Cold          float64       // _COLD, 0.6
	MaxReadingAge time.Duration // _MAX_READING_AGE_S, 1800
}

// DefaultKiroBudgetConfig mirrors the live hive-pace CronJob env.
func DefaultKiroBudgetConfig() KiroBudgetConfig {
	return KiroBudgetConfig{Window: time.Hour, MinSamples: 3, MinSpan: 20 * time.Minute,
		Safety: 0.85, Hot: 1.0, Cold: 0.6, MaxReadingAge: 30 * time.Minute}
}

// KiroBudget is kiro_budget()'s verdict: over, under, on-budget, settling,
// learning, stale, no-deadline or no-data.
type KiroBudget struct {
	Verdict     string
	Used, Limit float64
	Remaining   float64
	Reset       int64
	ReadingAgeS int64
	Safety      float64
	HoursLeft   *float64
	Allowed     *float64
	Samples     int
	SpanS       int64
	WindowStart int64
	Burn6h      *float64
	Burn        *float64
	Ratio       *float64
}

// ComputeKiroBudget ports kiro_budget(): burn (least squares over the last
// Window, never across the last Kiro actuation) against
// allowed = remaining / hours_left × Safety.
func ComputeKiroBudget(history []usage.PaceSample, now time.Time, lastAct int64, cfg KiroBudgetConfig) KiroBudget {
	type pt struct {
		ts          int64
		used, limit float64
		reset       int64
	}
	byTS := map[int64]pt{}
	for _, r := range history {
		if r.Provider != "kiro" || (r.Slot != "" && r.Slot != "slot0") {
			continue
		}
		lim := r.Limit
		if lim == 0 {
			lim = 10000
		}
		used := r.Used
		if !r.HasUsed {
			used = r.Pct * lim / 100
		}
		// prefer the exact row (with "used") when two share a timestamp
		if _, ok := byTS[r.TS]; !ok || r.HasUsed {
			byTS[r.TS] = pt{r.TS, used, lim, r.Reset}
		}
	}
	if len(byTS) == 0 {
		return KiroBudget{Verdict: "no-data"}
	}
	pts := make([]pt, 0, len(byTS))
	for _, p := range byTS {
		pts = append(pts, p)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].ts < pts[j].ts })
	cut := 0
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		if b.used < a.used-50 || (a.reset != 0 && b.reset != 0 && b.reset > a.reset+60) {
			cut = i
		}
	}
	pts = pts[cut:]
	latest := pts[len(pts)-1]
	nowS := now.Unix()
	out := KiroBudget{Used: pyRound(latest.used, 2), Limit: latest.limit, Remaining: pyRound(latest.limit-latest.used, 2),
		Reset: latest.reset, ReadingAgeS: nowS - latest.ts, Safety: cfg.Safety}
	var allowed *float64
	if latest.reset != 0 {
		hrs := float64(latest.reset-nowS) / 3600
		out.HoursLeft = fptr(pyRound(hrs, 2))
		if hrs > 0.05 {
			allowed = fptr(out.Remaining / hrs * cfg.Safety)
			out.Allowed = fptr(pyRound(*allowed, 1))
		}
	}
	start := nowS - int64(cfg.Window/time.Second)
	if lastAct > start {
		start = lastAct
	}
	var w []pt
	for _, p := range pts {
		if p.ts >= start {
			w = append(w, p)
		}
	}
	var span int64
	if len(w) > 1 {
		span = w[len(w)-1].ts - w[0].ts
	}
	out.Samples, out.SpanS, out.WindowStart = len(w), span, start
	var w6 []pt
	for _, p := range pts {
		if p.ts >= nowS-6*3600 {
			w6 = append(w6, p)
		}
	}
	if len(w6) >= 2 && w6[len(w6)-1].ts-w6[0].ts >= 1800 {
		out.Burn6h = fptr(pyRound((w6[len(w6)-1].used-w6[0].used)/(float64(w6[len(w6)-1].ts-w6[0].ts)/3600), 1))
	}
	switch {
	case allowed == nil:
		out.Verdict = "no-deadline"
	case out.ReadingAgeS > int64(cfg.MaxReadingAge/time.Second):
		out.Verdict = "stale"
	case len(w) >= cfg.MinSamples && span >= int64(cfg.MinSpan/time.Second):
		n := float64(len(w))
		var mx, my, num, den float64
		xs := make([]float64, len(w))
		for i, p := range w {
			xs[i] = float64(p.ts-w[0].ts) / 3600
			mx += xs[i]
			my += p.used
		}
		mx, my = mx/n, my/n
		for i, p := range w {
			num += (xs[i] - mx) * (p.used - my)
			den += (xs[i] - mx) * (xs[i] - mx)
		}
		burn := 0.0
		if den > 1e-9 {
			burn = num / den
		}
		out.Burn = fptr(pyRound(burn, 1))
		if *allowed > 0 {
			out.Ratio = fptr(pyRound(burn / *allowed, 2))
		}
		switch {
		case out.Ratio == nil || *out.Ratio > cfg.Hot:
			out.Verdict = "over"
		case *out.Ratio < cfg.Cold:
			out.Verdict = "under"
		default:
			out.Verdict = "on-budget"
		}
	default:
		if lastAct > nowS-int64(cfg.Window/time.Second) {
			out.Verdict = "settling"
		} else {
			out.Verdict = "learning"
		}
	}
	return out
}

// Line renders the budget as hive-pace prints it.
func (k KiroBudget) Line() string {
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return pyFloat(*p)
	}
	q := func(p *float64) string {
		if p == nil {
			return "?"
		}
		return pyFloat(*p)
	}
	if k.Verdict == "no-data" {
		return "kiro budget: no-data — used ?/? cr, ? left, ?h to reset -> allowed - cr/h (safety -); burn - cr/h over 0 sample(s)/0m (6h: -) -> ratio -"
	}
	return fmt.Sprintf("kiro budget: %s — used %s/%s cr, %s left, %sh to reset -> allowed %s cr/h (safety %s); burn %s cr/h over %d sample(s)/%dm (6h: %s) -> ratio %s",
		k.Verdict, pyFloat(k.Used), pyFloat(k.Limit), pyFloat(k.Remaining), q(k.HoursLeft), f(k.Allowed), pyFloat(k.Safety),
		f(k.Burn), k.Samples, k.SpanS/60, f(k.Burn6h), f(k.Ratio))
}

// FleetAgent is one agent of the paced fleet (fleet()), in HIVE_PACE_NAMESPACES
// order then /api/status order.
type FleetAgent struct {
	Namespace string
	Agent
	// Tier members of the agent's tier, for inferring a pace demotion when
	// no journal row exists (Shadow, or the first Enforce tick).
	Members []Rung
}

// PaceConfig holds the actuation knobs (HIVE_PACE_KIRO_* levers).
type PaceConfig struct {
	KiroBudget        bool    // HIVE_PACE_KIRO_BUDGET, on
	KiroPromoteMax    float64 // _PROMOTE_MAX, 0.8
	KiroMaxDemote     int     // _MAX_DEMOTE, 4
	KiroMaxPromote    int     // _MAX_PROMOTE, 1
	KiroMaxEvict      int     // _MAX_EVICT, 2
	KiroEvictTTL      time.Duration
	EvictTargetMaxPct int // HIVE_PACE_EVICT_TARGET_MAX_PCT, 85
	// InferDemotions stands an inferred demotion in for a missing journal row.
	InferDemotions bool
}

// DefaultPaceConfig mirrors the live hive-pace CronJob env.
func DefaultPaceConfig() PaceConfig {
	return PaceConfig{KiroBudget: true, KiroPromoteMax: 0.8, KiroMaxDemote: 4, KiroMaxPromote: 1, KiroMaxEvict: 2,
		KiroEvictTTL: 6 * time.Hour, EvictTargetMaxPct: 85, InferDemotions: true}
}

// PaceInput is one pace tick's world.
type PaceInput struct {
	Now      time.Time
	Fleet    []FleetAgent
	Verdicts map[string]PaceVerdict
	Kiro     KiroBudget
	// Readings are the rotation readings (published shape), for the Kiro
	// cap's target check.
	Readings map[string]Reading
	// Demoted: "ns/agent" → ORIGINAL placement (pace-demoted).
	Demoted map[string]Placement
	// KiroEvict: "ns/agent" → pending cap request.
	KiroEvict map[string]KiroEvict
	// Pins: "ns/agent" never re-ruled (HIVE_PACE_PIN).
	Pins   map[string]bool
	Config PaceConfig
}

// PaceDecision is one pace action, for one namespace.
type PaceDecision struct {
	Namespace string
	Decision
	// Journal effects on success.
	SetDemoted   *Placement // write "ns/agent|orig" (nil: leave)
	ClearDemoted bool
	SetEvict     *KiroEvict
}

// PacePlan is the fleet-wide pace plan.
type PacePlan struct {
	Decisions []PaceDecision
	// Lines in hive-pace apply's order (actions, SATURATED, kiro lines).
	Lines []string
	// ClearEvicts: kiro under budget withdraws every pending cap request.
	ClearEvicts bool
	// KiroActed: the budget levers moved something (KIRO_LAST_ACT = now).
	KiroActed bool
	// MovedOn: providers that got their one generic notch this tick.
	MovedOn map[string]bool
	Changes int
}

// kicksPerHour ports kicks_per_hour, including awk's %.6g output (the value
// is printed and re-read by the next awk, so its precision is what bash
// computes with).
func kicksPerHour(c string) float64 {
	if c == "" {
		return 0
	}
	n := c
	if ch := c[len(c)-1]; ch >= 'a' && ch <= 'z' {
		n = c[:len(c)-1]
	}
	for _, ch := range n {
		if (ch < '0' || ch > '9') && ch != '.' {
			return 0
		}
	}
	v := awkNum(n)
	if n == "" || v <= 0 {
		return 0
	}
	var k float64
	switch c[len(c)-1] {
	case 'h':
		k = 1 / v
	case 'm':
		k = 60 / v
	case 's':
		k = 3600 / v
	default:
		return 0
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(k, 'g', 6, 64), 64)
	return r
}

// awkG is awk's default number output (OFMT %.6g).
func awkG(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

// pyFloat renders a float as Python's json.dumps (and jq 1.7 echoing it)
// does: integral values keep ".0".
func pyFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if v == math.Trunc(v) && !strings.ContainsAny(s, "e.") {
		s += ".0"
	}
	return s
}

// PlanPace ports hive-pace.sh apply: the generic one-notch pass, the
// saturation report, then the Kiro budget levers. Every set_model is assumed
// to succeed (Shadow); the Enforce caller drops the effects of a failed one.
func PlanPace(in PaceInput) PacePlan {
	cfg := in.Config
	plan := PacePlan{MovedOn: map[string]bool{}}
	key := func(a FleetAgent) string { return a.Namespace + "/" + a.Name }
	demoted := map[string]Placement{}
	for k, v := range in.Demoted {
		demoted[k] = v
	}
	demotedFrom := func(a FleetAgent) (Placement, bool) {
		j := map[string]Placement{}
		if p, ok := demoted[key(a)]; ok {
			j[a.Name] = p
		}
		return PaceDemotedFrom(j, cfg.InferDemotions, a.Members, a.Agent)
	}
	paced := func(a FleetAgent) string {
		p := usage.ProviderOf(a.CLI, a.Model)
		if contains(PacedPools, p) {
			return p
		}
		return ""
	}
	verdict := func(p string) string {
		if v, ok := in.Verdicts[p]; ok {
			return v.Verdict
		}
		return "no-data"
	}
	seated, demotable := map[string]int{}, map[string]int{}

	for _, a := range in.Fleet {
		if a.Paused {
			continue
		}
		prov := paced(a)
		if prov == "" || (prov == "kiro" && cfg.KiroBudget) || in.Pins[key(a)] {
			continue
		}
		seated[prov]++
		cheap := RungDown(a.Model)
		if cheap != "" {
			demotable[prov]++
		}
		df, isDF := demotedFrom(a)
		if plan.MovedOn[prov] {
			continue
		}
		switch verdict(prov) {
		case "hot":
			if cheap == "" {
				continue
			}
			orig := Placement{Provider: prov, Backend: a.CLI, Model: a.Model}
			if isDF {
				orig = df
			}
			plan.Decisions = append(plan.Decisions, PaceDecision{Namespace: a.Namespace, SetDemoted: &orig,
				Decision: Decision{Agent: a.Name, Action: ActionDemote, Reason: prov + " hot",
					From: Placement{prov, a.CLI, a.Model}, To: Placement{prov, a.CLI, cheap}, ToEffort: AgyEffort(a.CLI, cheap),
					Line: fmt.Sprintf("  demote  %s  %s -> %s  (%s hot)", key(a), a.Model, cheap, prov)}})
			plan.Lines = append(plan.Lines, plan.Decisions[len(plan.Decisions)-1].Line)
			demoted[key(a)] = orig
			plan.MovedOn[prov] = true
			plan.Changes++
		case "cold":
			if !isDF {
				continue
			}
			plan.Decisions = append(plan.Decisions, PaceDecision{Namespace: a.Namespace, ClearDemoted: true,
				Decision: Decision{Agent: a.Name, Action: ActionRestore, Reason: prov + " cold",
					From: Placement{prov, a.CLI, a.Model}, To: Placement{df.Provider, df.Backend, df.Model}, ToEffort: AgyEffort(df.Backend, df.Model),
					Line: fmt.Sprintf("  restore %s  %s -> %s  (%s cold)", key(a), a.Model, df.Model, prov)}})
			plan.Lines = append(plan.Lines, plan.Decisions[len(plan.Decisions)-1].Line)
			delete(demoted, key(a))
			plan.MovedOn[prov] = true
			plan.Changes++
		}
	}
	for _, p := range PacedPools {
		if p == "kiro" && cfg.KiroBudget {
			continue
		}
		if verdict(p) != "hot" || plan.MovedOn[p] || seated[p] == 0 {
			continue
		}
		plan.Lines = append(plan.Lines, fmt.Sprintf("  SATURATED: %s is hot but no notch was available (%d of %d seated agents demotable) — model-rung pacing is exhausted; needs cadence or capacity",
			p, demotable[p], seated[p]))
	}
	if cfg.KiroBudget && in.Kiro.Verdict != "" {
		planKiro(in, &plan, demoted, demotedFrom)
	}
	return plan
}

type kiroRow struct {
	a         FleetAgent
	kph, mult float64
}

func planKiro(in PaceInput, plan *PacePlan, demoted map[string]Placement, demotedFrom func(FleetAgent) (Placement, bool)) {
	cfg := in.Config
	key := func(a FleetAgent) string { return a.Namespace + "/" + a.Name }
	kb := in.Kiro
	burn, allowed := 0.0, 0.0
	if kb.Burn != nil {
		burn = *kb.Burn
	}
	if kb.Allowed != nil {
		allowed = *kb.Allowed
	}
	var rows []kiroRow
	total := 0.0
	for _, a := range in.Fleet {
		if a.Paused || usage.ProviderOf(a.CLI, a.Model) != "kiro" {
			continue
		}
		r := kiroRow{a, kicksPerHour(a.Cadence), KiroCreditMult(a.Model)}
		rows = append(rows, r)
		total += r.kph * r.mult
	}
	n := 0
	switch kb.Verdict {
	case "over":
		need := burn - allowed
		type cand struct {
			sav   float64
			savS  string
			line  string
			r     kiroRow
			cheap string
		}
		var cs []cand
		for _, r := range rows {
			if in.Pins[key(r.a)] {
				continue
			}
			cheap := RungDown(r.a.Model)
			if cheap == "" || r.kph <= 0 || total <= 0 {
				continue
			}
			nm := KiroCreditMult(cheap)
			sav := burn * (r.kph * r.mult / total) * (1 - nm/r.mult)
			s := fmt.Sprintf("%.1f", sav)
			v, _ := strconv.ParseFloat(s, 64)
			cs = append(cs, cand{v, s, strings.Join([]string{s, r.a.Namespace, r.a.Name, r.a.CLI, r.a.Model, cheap}, "\t"), r, cheap})
		}
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].sav != cs[j].sav {
				return cs[i].sav > cs[j].sav
			}
			return cs[i].line < cs[j].line
		})
		cum := 0.0
		for _, c := range cs {
			if n >= cfg.KiroMaxDemote || cum >= need {
				break
			}
			a := c.r.a
			orig := Placement{Provider: "kiro", Backend: a.CLI, Model: a.Model}
			if df, ok := demotedFrom(a); ok {
				orig = df
			}
			plan.Decisions = append(plan.Decisions, PaceDecision{Namespace: a.Namespace, SetDemoted: &orig,
				Decision: Decision{Agent: a.Name, Action: ActionDemote, Reason: "kiro over budget",
					From: Placement{"kiro", a.CLI, a.Model}, To: Placement{"kiro", a.CLI, c.cheap},
					Line: fmt.Sprintf("  kiro-demote %s  %s -> %s  (saves ~%s cr/h; need %s)", key(a), a.Model, c.cheap, c.savS, awkG(need))}})
			plan.Lines = append(plan.Lines, plan.Decisions[len(plan.Decisions)-1].Line)
			demoted[key(a)] = orig
			n++
			plan.Changes++
			cum += c.sav
		}
		if n > 0 {
			plan.KiroActed = true
			return
		}
		var targets []string
		for _, p := range []string{"google", "anthropic"} {
			if v, ok := in.Verdicts[p]; ok && v.Verdict == "hot" {
				continue
			}
			r, ok := in.Readings[p]
			if !ok || r.Percent < 0 || int(r.Percent) >= cfg.EvictTargetMaxPct {
				continue
			}
			targets = append(targets, p)
		}
		if len(targets) == 0 {
			bs, as := "0", "0"
			if kb.Burn != nil {
				bs = pyFloat(burn)
			}
			if kb.Allowed != nil {
				as = pyFloat(allowed)
			}
			plan.Lines = append(plan.Lines, fmt.Sprintf("  SATURATED: kiro over budget (burn %s > allowed %s cr/h), every kicked Kiro agent is on the cheapest rung, and neither agy nor claude has headroom — needs cadence (operator)",
				bs, as))
			return
		}
		type wc struct {
			w    float64
			line string
			r    kiroRow
		}
		var ws []wc
		for _, r := range rows {
			if r.kph <= 0 || in.Pins[key(r.a)] {
				continue
			}
			s := fmt.Sprintf("%.3f", r.kph*r.mult)
			v, _ := strconv.ParseFloat(s, 64)
			ws = append(ws, wc{v, s + "\t" + r.a.Namespace + "\t" + r.a.Name, r})
		}
		sort.SliceStable(ws, func(i, j int) bool {
			if ws[i].w != ws[j].w {
				return ws[i].w > ws[j].w
			}
			return ws[i].line < ws[j].line
		})
		for _, w := range ws {
			if n >= cfg.KiroMaxEvict {
				break
			}
			a := w.r.a
			if ev, ok := in.KiroEvict[key(a)]; ok && ev.Expiry.After(in.Now) && len(ev.Targets) > 0 {
				continue
			}
			ev := KiroEvict{Expiry: in.Now.Add(cfg.KiroEvictTTL).Truncate(time.Second), Targets: targets}
			plan.Decisions = append(plan.Decisions, PaceDecision{Namespace: a.Namespace, SetEvict: &ev,
				Decision: Decision{Agent: a.Name, Action: ActionCap, Reason: "kiro over budget, nothing left to demote",
					From: Placement{"kiro", a.CLI, a.Model},
					Line: fmt.Sprintf("  kiro-cap    %s  -> off Kiro onto [%s] (hive-rotate enacts on its next tick)", key(a), strings.Join(targets, ","))}})
			plan.Lines = append(plan.Lines, plan.Decisions[len(plan.Decisions)-1].Line)
			n++
			plan.Changes++
		}
		plan.KiroActed = n > 0
	case "under":
		if len(in.KiroEvict) > 0 {
			plan.ClearEvicts = true
			plan.Lines = append(plan.Lines, "  kiro under budget: pending cap requests withdrawn")
		}
		type pc struct {
			add  float64
			addS string
			line string
			r    kiroRow
			up   string
			df   Placement
		}
		var cs []pc
		for _, r := range rows {
			if in.Pins[key(r.a)] {
				continue
			}
			df, ok := demotedFrom(r.a)
			if !ok {
				continue
			}
			up := RungUpToward(df.Model, r.a.Model)
			if up == "" {
				continue
			}
			s := "0"
			if total > 0 && r.kph > 0 {
				s = fmt.Sprintf("%.1f", burn*(r.kph*r.mult/total)*(KiroCreditMult(up)/r.mult-1))
			}
			v, _ := strconv.ParseFloat(s, 64)
			cs = append(cs, pc{v, s, strings.Join([]string{s, r.a.Namespace, r.a.Name, r.a.Model, up}, "\t"), r, up, df})
		}
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].add != cs[j].add {
				return cs[i].add < cs[j].add
			}
			return cs[i].line < cs[j].line
		})
		for _, c := range cs {
			if n >= cfg.KiroMaxPromote {
				break
			}
			if !(allowed > 0 && (burn+c.add)/allowed <= cfg.KiroPromoteMax) {
				continue
			}
			a := c.r.a
			d := PaceDecision{Namespace: a.Namespace,
				Decision: Decision{Agent: a.Name, Action: ActionRestore, Reason: "kiro under budget",
					From: Placement{"kiro", a.CLI, a.Model}, To: Placement{"kiro", c.df.Backend, c.up},
					Line: fmt.Sprintf("  kiro-promote %s  %s -> %s  (adds ~%s cr/h; kiro under budget)", key(a), a.Model, c.up, c.addS)}}
			if c.up == c.df.Model {
				d.ClearDemoted = true
				delete(demoted, key(a))
			}
			plan.Decisions = append(plan.Decisions, d)
			plan.Lines = append(plan.Lines, d.Line)
			n++
			plan.Changes++
		}
		plan.KiroActed = n > 0
	}
}

// Text renders the pace plan as hive-pace apply's action section, footer
// included ("pace: N change(s)").
func (p PacePlan) Text() []string {
	return append(append([]string(nil), p.Lines...), fmt.Sprintf("pace: %d change(s)", p.Changes))
}

// VerdictTable renders hive-pace's provider table (header included).
func VerdictTable(v map[string]PaceVerdict) []string {
	out := []string{fmt.Sprintf("%-11s %-9s %-9s %-11s %-11s %s", "PROVIDER", "VERDICT", "PRESSURE", "OBSERVED", "ALLOWED", "DETAIL")}
	for _, p := range PacedPools {
		pv, ok := v[p]
		if !ok {
			out = append(out, fmt.Sprintf("%-11s %-9s %-9s %-11s %-11s %s", p, "no-data", "-", "-", "-", "not measured"))
			continue
		}
		bs := pv.BindingSlot
		if bs == "" {
			bs = "slot0"
		}
		b, ok := pv.Slots[bs]
		pr, ob, al := "-", "-", "-"
		if pv.Pressure != nil {
			pr = pyFloat(*pv.Pressure)
		}
		det := fmt.Sprintf("null%% used, ?h left, %d limit(s), null sample(s)", len(pv.Slots))
		if ok {
			if b.ObservedRate != nil {
				ob = pyFloat(*b.ObservedRate) + " %/hr"
			}
			if b.AllowedRate != nil {
				al = pyFloat(*b.AllowedRate) + " %/hr"
			}
			pct := pyFloat(b.Pct)
			if p != "kiro" && b.Pct == math.Trunc(b.Pct) {
				pct = strconv.FormatFloat(b.Pct, 'f', 0, 64)
			}
			hl := "?"
			if b.HoursLeft != nil {
				hl = pyFloat(*b.HoursLeft)
			}
			det = fmt.Sprintf("%s%% used, %sh left, %d limit(s), %d sample(s)", pct, hl, len(pv.Slots), b.Samples)
		}
		out = append(out, fmt.Sprintf("%-11s %-9s %-9s %-11s %-11s %s", p, pv.Verdict, pr, ob, al, det))
	}
	return out
}
