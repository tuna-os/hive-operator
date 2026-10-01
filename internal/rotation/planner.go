// Package rotation is the placement and pacing planner that replaces
// hive-rotate.sh's apply/plan path and hive-pace.sh. It is a pure function of
// its Input — no I/O — so Shadow output can be diffed line for line against
// the bash jobs, and the same code produces the Enforce plan once promoted.
//
// PORTED, NOT REDESIGNED
// ----------------------
// Every rule below exists in the LIVE hive-rotate.sh (ConfigMap
// hive/hive-ops-scripts, 2026-10-01, 1922 lines; it is ahead of dotfiles git)
// and was earned by an incident; the comments name the bash function. Where
// the bash behaviour differs from its own comments, the planner reproduces
// the BEHAVIOUR (that is what the diff compares) and says so — see QUIRK
// notes. Fix quirks after cut-over, one at a time, never during shadowing.
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
	// Cadence as /api/status renders it ("5m", "2h", "paused"); "" = unknown.
	Cadence string
}

// awkNum is awk's string→number: the longest numeric prefix, else 0.
func awkNum(s string) float64 {
	end := 0
	for i, c := range s {
		if (c >= '0' && c <= '9') || c == '.' || (i == 0 && (c == '-' || c == '+')) {
			end = i + 1
			continue
		}
		break
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0
	}
	return v
}

// CadenceSeconds ports cadence_s: Nh/Nm/Ns, anything else ("paused", "idle",
// "") is 999999. Like awk, a non-numeric count reads as 0.
func (a Agent) CadenceSeconds() int {
	s := a.Cadence
	if s == "" {
		return 999999
	}
	n := s
	if c := s[len(s)-1]; c >= 'a' && c <= 'z' {
		n = s[:len(s)-1]
	}
	switch s[len(s)-1] {
	case 'h':
		return int(awkNum(n) * 3600)
	case 'm':
		return int(awkNum(n) * 60)
	case 's':
		return int(awkNum(n))
	}
	return 999999
}

// Reading is a provider measurement in the probe shape: Percent -1 is
// UNMEASURED, never exhausted. Note carries the probe note ("no-usage-api",
// "resets=…").
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
	KiroMinCadenceS    int                // HIVE_ROTATE_KIRO_MIN_CADENCE_S, 900
	MeteredFailover    bool               // HIVE_ROTATE_METERED_FAILOVER, on
	Canaries           bool               // HIVE_ROTATE_CANARIES, on
	CanaryProviders    []string           // bash: openai only
	CanaryCooldown     time.Duration      // HIVE_ROTATE_CANARY_EXHAUSTED_COOLDOWN_MIN, 720m
	AutoResume         bool               // HIVE_ROTATE_AUTORESUME, on
	PeakProviders      []string           // HIVE_PEAK_PROVIDERS, none since DeepSeek left
	PeakWindows        string             // HIVE_PEAK_WINDOWS, 01:00-04:00,06:00-10:00 (UTC, weekdays)
	CostRank           map[string]int     // google 0, kiro 1, anthropic 2, openai 3, other 4
	// InferPaceDemotion treats "agent sits below X on X's demotion chain, for
	// an X in its tier" as pace-demoted from X when the journal has no row
	// for it. Shadow cannot read hive-pace's journal (a file on the ops PVC);
	// without this every pace-demoted agent would read as off-tier.
	InferPaceDemotion bool
}

// DefaultPolicy mirrors the bash defaults.
func DefaultPolicy() Policy {
	return Policy{
		Thresholds:         map[string]float64{"openai": 85, "anthropic": 90, "google": 90, "kiro": 95},
		DefaultThreshold:   85,
		HighVolumeCadenceS: 1800,
		AgyMaxHighVolume:   5,
		KiroMinCadenceS:    900,
		MeteredFailover:    true,
		Canaries:           true,
		CanaryProviders:    []string{"openai"},
		CanaryCooldown:     720 * time.Minute,
		AutoResume:         true,
		PeakWindows:        "01:00-04:00,06:00-10:00",
		CostRank:           map[string]int{"google": 0, "kiro": 1, "anthropic": 2, "openai": 3},
		InferPaceDemotion:  true,
	}
}

// Providers is HIVE_ROTATE_PROVIDERS: the pools this fleet measures and
// places on. A provider outside it has no reading.
var Providers = []string{"kiro", "anthropic", "openai", "google", "meta"}

// Placement is a backend/model pair recorded in a journal.
type Placement struct {
	Provider, Backend, Model string
}

// KiroEvict is one hive-pace cap request: move this agent off Kiro, onto
// one of Targets, before Expiry.
type KiroEvict struct {
	Expiry  time.Time
	Targets []string
}

// Input is one tick's world.
type Input struct {
	Now       time.Time
	Namespace string
	// Primary is true for the spoke that owns fleet-wide state (peak holds,
	// contributors).
	Primary bool
	// PrimaryNamespace is HIVE_PRIMARY_NS (default "hive").
	PrimaryNamespace string
	Agents           []Agent // /api/status order — placement is order-dependent
	Providers        map[string]Reading
	// UsageSource is where the readings came from: "ccleft" disables the
	// openai canary while codex is measured (ccleft needs no pane).
	UsageSource string
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
	// PaceDemoted: agent → the ORIGINAL backend/model the pacer demoted it
	// from (however many notches it has taken since).
	PaceDemoted map[string]Placement
	// KiroEvict: hive-pace's cap requests for this spoke's agents.
	KiroEvict map[string]KiroEvict
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
	// Pace actions.
	ActionDemote  = "demote"
	ActionRestore = "restore"
	ActionCap     = "kiro-cap"
	// Contributor replicas.
	ActionScale = "scale"
)

// Decision is one line of the plan.
type Decision struct {
	Agent  string
	Action string
	From   Placement
	To     Placement
	// ToEffort is set when the destination is an agy model whose suffix
	// dictates effort (hive_placement_body).
	ToEffort string
	Reason   string
	// Line is the exact text hive-rotate.sh prints for this decision.
	Line string
	// SkipActuation: bash prints the line but makes no call (a strand of an
	// agent that is already paused).
	SkipActuation bool
	// ResumeAfter: after a successful move, resume the agent (a
	// login-detector pause, or an agent this controller stranded).
	ResumeAfter bool
	// CooldownProvider: after a successful move off an exhausted provider,
	// keep canaries off it for Policy.CanaryCooldown.
	CooldownProvider string
	// ClearsStrand: on apply, drop the agent's stranded journal row.
	ClearsStrand bool
	// RecordsStrand: on apply, add a stranded journal row (From).
	RecordsStrand bool
}

// Mutates reports whether applying the decision changes the hive.
func (d Decision) Mutates() bool {
	if d.SkipActuation {
		return false
	}
	switch d.Action {
	case ActionMove, ActionStrand, ActionResume, ActionCanary, ActionDemote, ActionRestore, ActionCap, ActionScale:
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
	byName       map[string]*Agent
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

// exhausted ports provider_exhausted: a POSITIVE reading at or over the
// provider's threshold. Unknown (-1) is never exhausted.
func (s *state) exhausted(p string) bool {
	v := s.pct(p)
	return v != -1 && v >= s.threshold(p)
}

// recovered ports provider_recovered: a POSITIVE reading below threshold —
// stricter than !exhausted, because treating unknown as recovered made
// stranded agents flap.
func (s *state) recovered(p string) bool {
	v := s.pct(p)
	return v != -1 && v < s.threshold(p)
}

func (s *state) providerOf(a *Agent) string { return usage.ProviderOf(a.CLI, a.Model) }

// providerOK ports provider_ok: may agent a be moved ONTO provider p?
func (s *state) providerOK(p string, a *Agent, allowSub bool) bool {
	if s.loginBlocked[p] || s.exhausted(p) {
		return false
	}
	if s.pct(p) == -1 {
		// Unknown is OK only when nothing CAN measure it (no agent, no usage
		// API) — a FAILED measurement keeps agents off.
		note := s.in.Providers[p].Note
		if !strings.Contains(note, "no-agent") && !strings.Contains(note, "no-usage-api") {
			return false
		}
	}
	if a != nil {
		c := a.CadenceSeconds()
		// High-volume guard: codex only (its low limit is shared with the
		// operator's own CLI), waived by the exhausted-current escape hatch.
		if p == "openai" && c <= s.pol.HighVolumeCadenceS && !allowSub {
			return false
		}
		// Kiro cadence guard: pi spends credits per STEP, so an agent kicked
		// more often than KiroMinCadenceS never goes on kiro — waived ONLY
		// when its current provider is positively exhausted (not by
		// allowSub, which the watchdog always passes).
		if p == "kiro" && c < s.pol.KiroMinCadenceS && !s.exhausted(s.providerOf(a)) {
			return false
		}
		// agy 5h-window stewardship: cap concurrent high-volume placements.
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

// chooseRung ports choose_rung: rank eligible tier members by cost rank,
// then peak, then pct used (unknown = 99) + 5 per agent already placed there
// this run. restrict, when non-nil, limits candidates to those providers
// (hive-pace's Kiro cap targets).
//
// QUIRK: bash sorts "cost peak pct+assigned*5 p|b|m" on the three numeric
// keys without -s, so ties fall back to comparing the whole line: WITHIN a
// provider the alphabetically-first model wins, not the first-listed one.
// Reproduced here.
func (s *state) chooseRung(tier string, a *Agent, restrict []string) (Rung, bool) {
	allowSub := false
	if s.pol.MeteredFailover && s.exhausted(s.providerOf(a)) {
		allowSub = true
	}
	type cand struct {
		k1, k2, k3 int
		tail       string
		r          Rung
	}
	var cs []cand
	for _, r := range s.members(tier) {
		if restrict != nil && !contains(restrict, r.Provider) {
			continue
		}
		if !s.providerOK(r.Provider, a, allowSub) {
			continue
		}
		rank := 99
		if pct, ok := s.in.Providers[r.Provider]; ok && pct.Percent != -1 {
			rank = int(math.Round(pct.Percent))
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

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// PaceDemotedFrom ports pace_demoted_from: when the pacer demoted this agent
// and it still sits BELOW the original on that demotion chain, the original
// placement. With no journal row and InferPaceDemotion, the first member of
// the agent's tier on the same backend whose chain contains the current
// model stands in for the row.
func PaceDemotedFrom(journal map[string]Placement, infer bool, members []Rung, a Agent) (Placement, bool) {
	if p, ok := journal[a.Name]; ok {
		if demotedFrom(p.Model, a.Model) {
			return p, true
		}
		return Placement{}, false
	}
	if !infer {
		return Placement{}, false
	}
	for _, r := range members {
		if r.Backend == a.CLI && demotedFrom(r.Model, a.Model) {
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
		if s < e {
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

// Compute builds the placement plan. It never mutates Input.
func Compute(in Input) Plan {
	pol := in.Policy
	if pol.Thresholds == nil {
		pol = DefaultPolicy()
	}
	if pol.CanaryCooldown == 0 {
		pol.CanaryCooldown = 720 * time.Minute
	}
	s := &state{in: in, pol: pol, loginBlocked: map[string]bool{}, assigned: map[string]int{}, byName: map[string]*Agent{}}
	s.peakNow = len(pol.PeakProviders) > 0 && InPeakWindow(in.Now, pol.PeakWindows)
	for i := range in.Agents {
		a := &in.Agents[i]
		s.byName[a.Name] = a
		// A paused login-detector agent is positive evidence of a provider
		// outage (LOGIN_BLOCKED).
		if a.Paused && a.PausedTrigger == "login-detector" {
			s.loginBlocked[s.providerOf(a)] = true
		}
	}
	var plan Plan
	add := func(d Decision) { plan.Decisions = append(plan.Decisions, d) }

	// The strand journal as apply mode sees it at each step: bash rewrites
	// the file after the un-strand pass and deletes a row on every resume,
	// so a later step no longer finds those rows. (The CronJobs run apply;
	// `plan` keeps the rows and would print the recovery-net line instead.)
	cleared := map[string]bool{}
	strandedNow := func(a string) bool {
		_, ok := in.Stranded[a]
		return ok && !cleared[a]
	}

	// 1. Un-strand journal: a stranded agent comes back by itself once its
	// provider is POSITIVELY below threshold. Map order is not the file
	// order bash walks; sort by agent for a stable diff.
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
		cleared[a] = true
		add(Decision{Agent: a, Action: ActionResume, From: row, Reason: row.Provider + " recovered", ClearsStrand: true,
			Line: line("%-14s %-9s recovered -> resuming", a, row.Provider)})
	}

	// 2. Auto-resume. An undeclared dashboard-api pause is resumed
	// unconditionally (HIVE_ROTATE_HOLD is the only durable pause); the
	// recovery net resumes an agent paused on a positively healthy provider
	// whose rung is in its tier.
	if pol.AutoResume {
		for i := range in.Agents {
			a := &in.Agents[i]
			if !a.Paused || a.OnDemand {
				continue
			}
			ours := strandedNow(a.Name)
			if a.PausedTrigger == "dashboard-api" && !ours {
				switch {
				case in.Holds[a.Name]:
					add(Decision{Agent: a.Name, Action: ActionHold, Reason: "declared hold",
						Line: line("%-14s %-9s held paused by HIVE_ROTATE_HOLD (declared in git)", a.Name, "operator")})
				case in.Primary && in.PeakPaused[a.Name]:
					add(Decision{Agent: a.Name, Action: ActionHold, Reason: "peak window",
						Line: line("%-14s %-9s held paused by the peak window (hive-peak-resume restores it)", a.Name, "peak")})
				default:
					cleared[a.Name] = true
					add(Decision{Agent: a.Name, Action: ActionResume, Reason: "undeclared operator pause", ClearsStrand: true,
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
			curp := s.providerOf(a)
			if !s.recovered(curp) || !s.inTier(tier, a.CLI, a.Model) {
				continue
			}
			cleared[a.Name] = true
			add(Decision{Agent: a.Name, Action: ActionResume, Reason: "paused on a healthy provider", ClearsStrand: true,
				From: Placement{curp, a.CLI, a.Model},
				Line: line("%-14s %-9s paused on a healthy provider -> resuming", a.Name, curp)})
		}
	}

	// 3. Placement.
	for i := range in.Agents {
		a := &in.Agents[i]
		curp := s.providerOf(a)
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
		demoted, isDemoted := PaceDemotedFrom(in.PaceDemoted, pol.InferPaceDemotion, s.members(tier), *a)

		// Kiro budget cap (hive-pace's kiro-evict request): move off Kiro,
		// only onto the pools pace named, only onto this agent's tier.
		if curp == "kiro" {
			if ev, ok := in.KiroEvict[a.Name]; ok && ev.Expiry.After(in.Now) && len(ev.Targets) > 0 {
				if want, ok := s.chooseRung(tier, a, ev.Targets); ok {
					s.notePlacement(want.Provider, a)
					plan.Changes++
					add(Decision{Agent: a.Name, Action: ActionMove, From: cur,
						To:       Placement{want.Provider, want.Backend, want.Model},
						ToEffort: AgyEffort(want.Backend, want.Model), Reason: "kiro budget cap, hive-pace",
						Line: line("%-14s %-9s %s  ->  %-9s %s  (kiro budget cap, hive-pace)", a.Name, curp, a.Model, want.Provider, want.Model)})
					continue
				}
				add(Decision{Agent: a.Name, Action: ActionKeep, From: cur, To: cur, Reason: "kiro cap: no rung on targets",
					Line: line("%-14s %-9s kiro budget cap requested, but no rung on [%s] in %s — stays", a.Name, curp, strings.Join(ev.Targets, " "), tier)})
			}
		}

		// A high-cadence agent sitting on kiro is NOT sticky.
		kiroEvict := curp == "kiro" && a.CadenceSeconds() < pol.KiroMinCadenceS

		// Stickiness: a failover system, not an optimiser. Only POSITIVE
		// exhaustion (or a login-blocked provider) moves anyone; a rung the
		// pacer demoted this agent onto counts as in-tier.
		if !kiroEvict && !s.exhausted(curp) && !s.loginBlocked[curp] &&
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
		want, ok := s.chooseRung(tier, a, nil)
		if !ok {
			if s.exhausted(curp) {
				// Pausing is honest degradation; bash pauses (and journals)
				// only an agent that is not already paused.
				plan.Changes++
				add(Decision{Agent: a.Name, Action: ActionStrand, From: cur,
					Reason:        fmt.Sprintf("%s exhausted and no rung at %s", curp, tier),
					SkipActuation: a.Paused, RecordsStrand: !a.Paused,
					Line: line("%-14s %-9s STRANDED (no rung at %s) -> pausing", a.Name, curp, tier)})
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
		switch {
		case s.exhausted(curp):
			reason = curp + " exhausted"
		case s.loginBlocked[curp]:
			reason = curp + " login-blocked"
		case kiroEvict:
			reason = fmt.Sprintf("cadence %s is under the kiro minimum", a.Cadence)
		}
		wasStranded := strandedNow(a.Name)
		d := Decision{Agent: a.Name, Action: ActionMove, From: cur, To: to, ToEffort: AgyEffort(want.Backend, want.Model),
			Reason:       reason,
			ResumeAfter:  a.Paused && (a.PausedTrigger == "login-detector" || wasStranded),
			ClearsStrand: wasStranded,
			Line:         line("%-14s %-9s %s  ->  %-9s %s", a.Name, curp, a.Model, want.Provider, want.Model)}
		if s.exhausted(curp) {
			d.CooldownProvider = curp
		}
		plan.Changes++
		add(d)
	}

	// 4. Canaries: keep one low-cadence agent on each pool whose usage is
	// readable only through a live pane (codex). ccleft reads codex
	// headlessly, so while its reading is usable there is no canary.
	//
	// NOTE: first_agent_on/canary_eligible read the snapshot, so an agent
	// moved above still counts on its OLD provider this tick — as in bash.
	if pol.Canaries {
		for _, p := range pol.CanaryProviders {
			if in.UsageSource == "ccleft" && s.pct(p) != -1 {
				continue
			}
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
				From:     Placement{s.providerOf(a), a.CLI, a.Model},
				To:       Placement{p, r.Backend, r.Model},
				ToEffort: AgyEffort(r.Backend, r.Model), Reason: "probe visibility",
				Line: line("%-14s %-9s canary -> %-9s %s (probe visibility)", a.Name,
					usage.ProviderOf(r.Backend, r.Model), r.Backend, r.Model)})
		}
	}
	return plan
}

func (s *state) firstAgentOn(p string) bool {
	for i := range s.in.Agents {
		a := &s.in.Agents[i]
		if !a.Paused && s.providerOf(a) == p {
			return true
		}
	}
	return false
}

// canaryEligible: the LONGEST-cadence unpaused agent whose tier has a rung
// on p (first wins a tie); high-volume agents never canary openai.
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

// Threshold exposes the effective exhausted-at percent (for contributors and
// status).
func (p Policy) Threshold(provider string) float64 {
	if t, ok := p.Thresholds[provider]; ok {
		return t
	}
	if p.DefaultThreshold > 0 {
		return p.DefaultThreshold
	}
	return 85
}
