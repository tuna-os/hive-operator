// Package rotation is the placement planner that replaces hive-rotate.sh's
// apply/plan path. It is a pure function of its Input — no I/O — so a week of
// Shadow output can be diffed line for line against the bash job, and the
// same code produces the Enforce plan once promoted.
//
// PORTED, NOT REDESIGNED
// ----------------------
// Every rule below exists in hive-rotate.sh (branch hive-ops-k8s, 2026-09-24)
// and was earned by an incident; the comments name the bash lines. Where the
// bash behaviour differs from its own comments, the planner reproduces the
// BEHAVIOUR (that is what the diff compares) and says so — see QUIRK notes.
// Fix quirks after cut-over, one at a time, never during shadowing.
//
// Output lines use the bash printf formats byte for byte, so
// `diff <(bash plan) <(operator plan)` is the shadow check.
package rotation

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/hive-operator/internal/usage"
)

// Agent is one agent as /api/status (overlaid with hive-state.json) shows it.
type Agent struct {
	Name          string
	CLI           string // backend
	Model         string // govModel: what the governor launches
	Effort        string
	Paused        bool
	OnDemand      bool
	PausedTrigger string
	// Cadence as /api/status renders it ("5m", "2h"); "" = unknown.
	Cadence string
}

// CadenceSeconds parses Nh/Nm/Ns; anything else is 999999 (hive-rotate.sh:923).
func (a Agent) CadenceSeconds() int {
	s := strings.TrimSpace(a.Cadence)
	if len(s) < 2 {
		return 999999
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 999999
	}
	switch s[len(s)-1] {
	case 'h':
		return int(n * 3600)
	case 'm':
		return int(n * 60)
	case 's':
		return int(n)
	}
	return 999999
}

// Reading is a provider measurement: Percent -1 is UNMEASURED, never
// exhausted. Note carries the probe note ("no-usage-api", "resets=…").
type Reading struct {
	Percent float64
	Note    string
}

// Rung is one tier member, in preference order.
type Rung struct {
	Tier, Provider, Backend, Model string
}

// Policy holds the tunables, defaulted to hive-rotate.sh's values.
type Policy struct {
	Thresholds         map[string]float64 // provider → exhausted-at percent
	DefaultThreshold   float64            // HIVE_ROTATE_THRESHOLD, 85
	HighVolumeCadenceS int                // HIVE_ROTATE_HIGH_VOLUME_S, 1800
	AgyMaxHighVolume   int                // HIVE_ROTATE_AGY_MAX_HIGH_VOLUME, 5
	MeteredFailover    bool               // HIVE_ROTATE_METERED_FAILOVER, on
	Canaries           bool               // HIVE_ROTATE_CANARIES, on
	CanaryProviders    []string           // bash: openai only
	AutoResume         bool               // HIVE_ROTATE_AUTORESUME, on
	PeakProviders      []string           // HIVE_PEAK_PROVIDERS, deepseek
	PeakWindows        string             // HIVE_PEAK_WINDOWS, 01:00-04:00,06:00-10:00 (UTC, weekdays)
	CostRank           map[string]int     // google 0, anthropic 1, openai 2, deepseek 3, other 4
	// InferPaceDemotion treats "agent sits on rung_down(X) for an X in its
	// tier" as pace-demoted from X when no journal row says so. The operator
	// cannot read hive-pace's journal (a file on the ops PVC); without this
	// every pace-demoted T1 agent would read as off-tier and be moved.
	InferPaceDemotion bool
}

// DefaultPolicy mirrors the bash defaults.
func DefaultPolicy() Policy {
	return Policy{
		Thresholds:         map[string]float64{"openai": 85, "anthropic": 90, "google": 90, "deepseek": 100},
		DefaultThreshold:   85,
		HighVolumeCadenceS: 1800,
		AgyMaxHighVolume:   5,
		MeteredFailover:    true,
		Canaries:           true,
		CanaryProviders:    []string{"openai"},
		AutoResume:         true,
		PeakProviders:      []string{"deepseek"},
		PeakWindows:        "01:00-04:00,06:00-10:00",
		CostRank:           map[string]int{"google": 0, "anthropic": 1, "openai": 2, "deepseek": 3},
		InferPaceDemotion:  true,
	}
}

// Placement is a backend/model pair recorded in a journal.
type Placement struct {
	Provider, Backend, Model string
}

// Input is one tick's world.
type Input struct {
	Now       time.Time
	Namespace string
	// Primary is true for the spoke that owns fleet-wide state (peak holds).
	Primary   bool
	Agents    []Agent // /api/status order — placement is order-dependent
	Providers map[string]Reading
	// Tiers maps agent → tier; an agent with no tier is skipped everywhere.
	Tiers map[string]string
	// Rungs is tier_members for every tier, in preference order.
	Rungs []Rung
	Pins  map[string]bool
	Holds map[string]bool
	// PeakPaused: agents hive-peak paused for a peak window (declared holds).
	PeakPaused map[string]bool
	// Stranded is the strand journal: agent → where it was parked.
	Stranded map[string]Placement
	// PaceDemoted: agent → the backend/model the pacer demoted it FROM.
	PaceDemoted map[string]Placement
	// CanaryCooldown: provider → cooldown expiry.
	CanaryCooldown map[string]time.Time
	Policy         Policy
}

// Action kinds.
const (
	ActionMove    = "move"
	ActionStrand  = "strand"
	ActionResume  = "resume"
	ActionCanary  = "canary"
	ActionHold    = "hold"
	ActionPin     = "pin"
	ActionKeep    = "keep"
	ActionUnknown = "skip"
)

// Decision is one line of the plan.
type Decision struct {
	Agent  string
	Action string
	From   Placement
	To     Placement
	// ToEffort is set when the destination is an agy model whose suffix
	// dictates effort (sync_effort, hive-rotate.sh:471-494).
	ToEffort string
	Reason   string
	// Line is the exact text hive-rotate.sh prints for this decision.
	Line string
}

// Mutates reports whether applying the decision changes the hive.
func (d Decision) Mutates() bool {
	switch d.Action {
	case ActionMove, ActionStrand, ActionResume, ActionCanary:
		return true
	}
	return false
}

// Plan is the planner's output.
type Plan struct {
	Decisions []Decision
	// Changes counts moves, strands and canaries — resumes are NOT counted,
	// matching bash's `changed`.
	Changes int
}

// Lines renders the plan as hive-rotate.sh prints it (before the
// contributors section), including the footer.
func (p Plan) Lines(dry bool) []string {
	var out []string
	for _, d := range p.Decisions {
		if d.Line != "" {
			out = append(out, d.Line)
		}
	}
	if dry && p.Changes > 0 {
		out = append(out, fmt.Sprintf("%d change(s) — run 'hive-rotate.sh apply' to perform them", p.Changes))
	}
	if p.Changes == 0 {
		out = append(out, "fleet already on the best available rung")
	}
	return out
}

type state struct {
	in           Input
	pol          Policy
	loginBlocked map[string]bool
	assigned     map[string]int
	agyHVPlaced  int
	peakNow      bool
}

func (s *state) threshold(p string) float64 {
	if t, ok := s.pol.Thresholds[p]; ok {
		return t
	}
	if s.pol.DefaultThreshold > 0 {
		return s.pol.DefaultThreshold
	}
	return 85
}

func (s *state) pct(p string) float64 {
	r, ok := s.in.Providers[p]
	if !ok {
		return -1
	}
	return r.Percent
}

// exhausted: a POSITIVE reading at or over threshold. Unknown is never
// exhausted (hive-rotate.sh:945-950).
//
// QUIRK: bash compares integers; a fractional reading makes `-ge` error and
// read as "not exhausted". Readings are integers today; the port compares
// numerically and rounds nothing.
func (s *state) exhausted(p string) bool {
	v := s.pct(p)
	return v >= 0 && v >= s.threshold(p)
}

// recovered: POSITIVE reading below threshold — stricter than !exhausted,
// because treating unknown as recovered made stranded agents flap.
func (s *state) recovered(p string) bool {
	v := s.pct(p)
	return v >= 0 && v < s.threshold(p)
}

func (s *state) providerOK(p string, a *Agent, allowSub bool) bool {
	if s.loginBlocked[p] || s.exhausted(p) {
		return false
	}
	if s.pct(p) < 0 {
		// Unknown is OK only when the probe says it CANNOT measure (no pane,
		// no usage API) — a FAILED measurement keeps agents off.
		note := s.in.Providers[p].Note
		if !strings.Contains(note, "no-agent") && !strings.Contains(note, "no-usage-api") {
			return false
		}
	}
	if a != nil {
		c := a.CadenceSeconds()
		if p == "openai" && c <= s.pol.HighVolumeCadenceS && !allowSub {
			return false
		}
		if p == "google" && c <= s.pol.HighVolumeCadenceS && s.agyHVPlaced >= s.pol.AgyMaxHighVolume {
			return false
		}
	}
	return true
}

func (s *state) notePlacement(p string, a *Agent) {
	s.assigned[p]++
	if p == "google" && a.CadenceSeconds() <= s.pol.HighVolumeCadenceS {
		s.agyHVPlaced++
	}
}

func (s *state) members(tier string) []Rung {
	var out []Rung
	for _, r := range s.in.Rungs {
		if r.Tier == tier {
			out = append(out, r)
		}
	}
	return out
}

func (s *state) inTier(tier, b, m string) bool {
	for _, r := range s.members(tier) {
		if r.Backend == b && r.Model == m {
			return true
		}
	}
	return false
}

func (s *state) costRank(p string) int {
	if r, ok := s.pol.CostRank[p]; ok {
		return r
	}
	return 4
}

func (s *state) isPeak(p string) bool {
	for _, x := range s.pol.PeakProviders {
		if x == p {
			return true
		}
	}
	return false
}

// chooseRung ports choose_rung (hive-rotate.sh:1049-1077).
//
// QUIRK: bash sorts "cost peak pct+assigned*5 p|b|m" on the three numeric keys
// without -s, so ties fall back to comparing the whole line: WITHIN a
// provider the alphabetically-first model wins, not the first-listed one
// (gemini-3.6-flash-low beats gemini-3.8-flash-low in T2). Reproduced here.
func (s *state) chooseRung(tier string, a *Agent) (Rung, bool) {
	allowSub := false
	if s.pol.MeteredFailover {
		if s.exhausted(usage.ProviderOf(a.CLI, a.Model)) {
			allowSub = true
		}
	}
	type cand struct {
		k1, k2, k3 int
		tail       string
		r          Rung
	}
	var cs []cand
	for _, r := range s.members(tier) {
		if !s.providerOK(r.Provider, a, allowSub) {
			continue
		}
		pct := s.pct(r.Provider)
		rank := 99
		if _, ok := s.in.Providers[r.Provider]; ok && pct >= 0 {
			rank = int(math.Round(pct))
		}
		peak := 0
		if s.peakNow && s.isPeak(r.Provider) {
			peak = 1
		}
		cs = append(cs, cand{s.costRank(r.Provider), peak, rank + s.assigned[r.Provider]*5,
			r.Provider + "|" + r.Backend + "|" + r.Model, r})
	}
	if len(cs) == 0 {
		return Rung{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.k1 != b.k1 {
			return a.k1 < b.k1
		}
		if a.k2 != b.k2 {
			return a.k2 < b.k2
		}
		if a.k3 != b.k3 {
			return a.k3 < b.k3
		}
		return a.tail < b.tail
	})
	return cs[0].r, true
}

// RungDown ports rung_down (hive-lib.sh:327): the pacer's one-notch demotion.
func RungDown(model string) string {
	switch model {
	case "claude-fable-5-1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5":
		return "claude-sonnet-5"
	case "gemini-3.8-flash-high":
		return "gemini-3.8-flash-low"
	case "gemini-3.7-flash-high":
		return "gemini-3.7-flash-low"
	case "gpt-6-astra", "gpt-5.6-sol":
		return "gpt-5.6-luna"
	}
	return ""
}

// agyEffort ports sync_effort's rule: only agy, only a -high/-medium/-low
// suffix — agy silently runs 3.6 Flash Low when --effort disagrees.
func agyEffort(backend, model string) string {
	if backend != "agy" {
		return ""
	}
	for _, e := range []string{"high", "medium", "low"} {
		if strings.HasSuffix(model, "-"+e) {
			return e
		}
	}
	return ""
}

func (s *state) paceDemotedFrom(a *Agent, tier string) (Placement, bool) {
	if p, ok := s.in.PaceDemoted[a.Name]; ok {
		if RungDown(p.Model) == a.Model {
			return p, true
		}
		return Placement{}, false
	}
	if !s.pol.InferPaceDemotion {
		return Placement{}, false
	}
	for _, r := range s.members(tier) {
		if r.Backend == a.CLI && RungDown(r.Model) == a.Model {
			return Placement{Provider: r.Provider, Backend: r.Backend, Model: r.Model}, true
		}
	}
	return Placement{}, false
}

// InPeakWindow ports in_peak_window: weekdays (UTC) only, windows may wrap.
func InPeakWindow(now time.Time, windows string) bool {
	now = now.UTC()
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	mins := now.Hour()*60 + now.Minute()
	for _, w := range strings.Split(windows, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(w), "-")
		if !ok {
			continue
		}
		s, e := hhmm(a), hhmm(b)
		if s < 0 || e < 0 {
			continue
		}
		if s <= e {
			if mins >= s && mins < e {
				return true
			}
		} else if mins >= s || mins < e {
			return true
		}
	}
	return false
}

func hhmm(s string) int {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return -1
	}
	hi, err1 := strconv.Atoi(h)
	mi, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil {
		return -1
	}
	return hi*60 + mi
}

func line(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// Compute builds the plan. It never mutates Input.
func Compute(in Input) Plan {
	pol := in.Policy
	if pol.Thresholds == nil {
		pol = DefaultPolicy()
	}
	s := &state{in: in, pol: pol, loginBlocked: map[string]bool{}, assigned: map[string]int{}}
	s.peakNow = InPeakWindow(in.Now, pol.PeakWindows)
	for _, a := range in.Agents {
		if a.Paused && a.PausedTrigger == "login-detector" {
			s.loginBlocked[usage.ProviderOf(a.CLI, a.Model)] = true
		}
	}
	var plan Plan
	add := func(d Decision) {
		plan.Decisions = append(plan.Decisions, d)
	}

	// 1. Un-strand journal (hive-rotate.sh:1461-1476). Map order is not the
	// file order bash walks; sort by agent for a stable diff.
	stranded := make([]string, 0, len(in.Stranded))
	for a := range in.Stranded {
		stranded = append(stranded, a)
	}
	sort.Strings(stranded)
	for _, a := range stranded {
		row := in.Stranded[a]
		if !s.recovered(row.Provider) {
			continue
		}
		add(Decision{Agent: a, Action: ActionResume, From: row, Reason: row.Provider + " recovered",
			Line: line("%-14s %-9s recovered -> resuming", a, row.Provider)})
	}

	// 2. Auto-resume (1537-1594).
	if pol.AutoResume {
		for i := range in.Agents {
			a := &in.Agents[i]
			if !a.Paused || a.OnDemand {
				continue
			}
			_, ours := in.Stranded[a.Name]
			if a.PausedTrigger == "dashboard-api" && !ours {
				switch {
				case in.Holds[a.Name]:
					add(Decision{Agent: a.Name, Action: ActionHold, Reason: "declared hold",
						Line: line("%-14s %-9s held paused by HIVE_ROTATE_HOLD (declared in git)", a.Name, "operator")})
				case in.Primary && in.PeakPaused[a.Name]:
					add(Decision{Agent: a.Name, Action: ActionHold, Reason: "peak window",
						Line: line("%-14s %-9s held paused by the peak window (hive-peak-resume restores it)", a.Name, "peak")})
				default:
					add(Decision{Agent: a.Name, Action: ActionResume, Reason: "undeclared operator pause",
						Line: line("%-14s %-9s operator pause -> resuming (not declared in HIVE_ROTATE_HOLD)", a.Name, "operator")})
				}
				continue
			}
			if a.PausedTrigger == "login-detector" {
				continue
			}
			tier := in.Tiers[a.Name]
			if tier == "" {
				continue
			}
			curp := usage.ProviderOf(a.CLI, a.Model)
			if !s.recovered(curp) || !s.inTier(tier, a.CLI, a.Model) {
				continue
			}
			add(Decision{Agent: a.Name, Action: ActionResume, Reason: "paused on a healthy provider",
				From: Placement{curp, a.CLI, a.Model},
				Line: line("%-14s %-9s paused on a healthy provider -> resuming", a.Name, curp)})
		}
	}

	// 3. Placement (1600-1710).
	for i := range in.Agents {
		a := &in.Agents[i]
		curp := usage.ProviderOf(a.CLI, a.Model)
		cur := Placement{curp, a.CLI, a.Model}
		if in.Pins[a.Name] {
			add(Decision{Agent: a.Name, Action: ActionPin, From: cur, Reason: "pinned",
				Line: line("%-14s %-9s pinned by HIVE_ROTATE_PIN — placement left alone", a.Name, curp)})
			continue
		}
		tier := in.Tiers[a.Name]
		if tier == "" {
			continue
		}
		// Stickiness: a failover system, not an optimiser. Only POSITIVE
		// exhaustion (or a login-blocked provider) moves anyone.
		demoted, isDemoted := s.paceDemotedFrom(a, tier)
		if !s.exhausted(curp) && !s.loginBlocked[curp] &&
			(s.inTier(tier, a.CLI, a.Model) || (isDemoted && s.inTier(tier, demoted.Backend, demoted.Model))) {
			s.notePlacement(curp, a)
			suffix := ""
			if isDemoted {
				suffix = " (pace-demoted from " + demoted.Model + ")"
			}
			add(Decision{Agent: a.Name, Action: ActionKeep, From: cur, To: cur, Reason: "sticky",
				Line: line("%-14s %-9s %s ok%s", a.Name, curp, a.Model, suffix)})
			continue
		}
		want, ok := s.chooseRung(tier, a)
		if !ok {
			if s.exhausted(curp) {
				plan.Changes++
				add(Decision{Agent: a.Name, Action: ActionStrand, From: cur,
					Reason: fmt.Sprintf("%s exhausted and no rung at %s", curp, tier),
					Line:   line("%-14s %-9s STRANDED (no rung at %s) -> pausing", a.Name, curp, tier)})
			} else {
				add(Decision{Agent: a.Name, Action: ActionKeep, From: cur, To: cur, Reason: "no better rung",
					Line: line("%-14s %-9s %s ok (no better rung available)", a.Name, curp, a.Model)})
			}
			continue
		}
		s.notePlacement(want.Provider, a)
		to := Placement{want.Provider, want.Backend, want.Model}
		if want.Backend == a.CLI && want.Model == a.Model {
			add(Decision{Agent: a.Name, Action: ActionKeep, From: cur, To: to, Reason: "already best",
				Line: line("%-14s %-9s %s ok", a.Name, curp, a.Model)})
			continue
		}
		reason := "current rung is not in tier " + tier
		if s.exhausted(curp) {
			reason = curp + " exhausted"
		} else if s.loginBlocked[curp] {
			reason = curp + " login-blocked"
		}
		plan.Changes++
		add(Decision{Agent: a.Name, Action: ActionMove, From: cur, To: to, ToEffort: agyEffort(want.Backend, want.Model),
			Reason: reason,
			Line:   line("%-14s %-9s %s  ->  %-9s %s", a.Name, curp, a.Model, want.Provider, want.Model)})
	}

	// 4. Canaries (1712-1773): keep one low-cadence agent on each pool whose
	// usage is readable only through a live pane.
	//
	// NOTE: first_agent_on/canary_eligible read the snapshot, so an agent
	// moved above still counts on its OLD provider this tick — as in bash.
	if pol.Canaries {
		for _, p := range pol.CanaryProviders {
			if s.firstAgentOn(p) || s.exhausted(p) {
				continue
			}
			if exp, ok := in.CanaryCooldown[p]; ok && exp.After(in.Now) {
				continue
			}
			a := s.canaryEligible(p)
			if a == nil {
				continue
			}
			var r Rung
			for _, m := range s.members(in.Tiers[a.Name]) {
				if m.Provider == p {
					r = m
					break
				}
			}
			plan.Changes++
			add(Decision{Agent: a.Name, Action: ActionCanary,
				From:     Placement{usage.ProviderOf(a.CLI, a.Model), a.CLI, a.Model},
				To:       Placement{p, r.Backend, r.Model},
				ToEffort: agyEffort(r.Backend, r.Model), Reason: "probe visibility",
				Line: line("%-14s %-9s canary -> %-9s %s (probe visibility)", a.Name,
					usage.ProviderOf(r.Backend, r.Model), r.Backend, r.Model)})
		}
	}
	return plan
}

func (s *state) firstAgentOn(p string) bool {
	for _, a := range s.in.Agents {
		if !a.Paused && usage.ProviderOf(a.CLI, a.Model) == p {
			return true
		}
	}
	return false
}

func (s *state) canaryEligible(p string) *Agent {
	var best *Agent
	bestC := -1
	for i := range s.in.Agents {
		a := &s.in.Agents[i]
		if a.Paused {
			continue
		}
		tier := s.in.Tiers[a.Name]
		if tier == "" {
			continue
		}
		has := false
		for _, m := range s.members(tier) {
			if m.Provider == p {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		c := a.CadenceSeconds()
		if p == "openai" && c <= s.pol.HighVolumeCadenceS {
			continue
		}
		if c > bestC {
			bestC, best = c, a
		}
	}
	return best
}
