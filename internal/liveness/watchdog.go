package liveness

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// Agent is one agent as /api/status shows it (model overlaid with the
// hive-state.json override, as rotation reads it).
type Agent struct {
	Name   string
	CLI    string
	Model  string // govModel (overlaid)
	Effort string // reasoningEffort ("" = never set → agy default low)
	Paused bool
	// Busy is /api/status `busy`: "working" while a turn is open.
	Busy          string
	NeedsLogin    bool
	AuthKnown     bool
	AuthAvailable bool
	// LiveSummary is the hive's own last pane capture (/api/status lags it
	// by minutes; the live pane is re-read before judging — NeedsLivePane).
	LiveSummary string
}

// Action kinds.
const (
	KindHygiene   = "hygiene"
	KindWake      = "wake"
	KindRepair    = "repair"
	KindEffort    = "effort"
	KindRestart   = "restart"
	KindRotateOff = "rotate-off"
	KindHuman     = "needs-human"
	KindKick      = "kick"
)

// Action is one mutation the watchdog (or nudge) would make.
type Action struct {
	Agent string
	Kind  string
	// State is the pane classification that triggered a heal.
	State string
	// CLI is the agent's backend at decision time (restart of agy/muse
	// first reopens the shared first-run state, in the same exec).
	CLI string
	// To is the destination of a repair or rotate-off.
	To rotation.Rung
	// Effort to set (KindEffort).
	Effort string
	// HealCount is the restart number this heal records (1-based).
	HealCount int
	// Providers whose renewal triggered a wake.
	Providers []string
	Reason    string
	// Anchor is the index in Plan.Lines after which the executor's outcome
	// lines go (bash prints them after the action line). -1: none.
	Anchor int
}

// Mutating reports whether applying the action changes the spoke (the
// MAX_MUTATIONS budget counts these).
func (a Action) Mutating() bool {
	switch a.Kind {
	case KindRepair, KindEffort, KindRestart, KindRotateOff:
		return true
	}
	return false
}

// Heal is one agent's backoff record (watchdog-last-kick-<agent>:
// "<epoch> <count>").
type Heal struct {
	Count int
	Last  time.Time
}

// PaneSeen is the stall detector's record (watchdog-pane-<agent>:
// "<hash> <first-seen>").
type PaneSeen struct {
	Hash  string
	Since time.Time
}

// Journal is the watchdog's memory between passes. It replaces the bash
// state files on the hive-ops-state PVC.
type Journal struct {
	Heals map[string]Heal
	Panes map[string]PaneSeen
	// Resets: provider → when its exhausted window renews (resets.d/<p>).
	Resets    map[string]time.Time
	HygieneAt time.Time
}

func (j Journal) clone() Journal {
	out := Journal{Heals: map[string]Heal{}, Panes: map[string]PaneSeen{}, Resets: map[string]time.Time{}, HygieneAt: j.HygieneAt}
	for k, v := range j.Heals {
		out.Heals[k] = v
	}
	for k, v := range j.Panes {
		out.Panes[k] = v
	}
	for k, v := range j.Resets {
		out.Resets[k] = v
	}
	return out
}

// Policy holds the watchdog knobs, defaulted to the bash values.
type Policy struct {
	MaxMutations   int           // HIVE_WATCHDOG_MAX_MUTATIONS, 3
	BackoffBaseMin int           // HIVE_WATCHDOG_BASE_MIN, 5
	BackoffMaxMin  int           // HIVE_WATCHDOG_MAX_MIN, 120
	StallMin       int           // HIVE_WATCHDOG_STALL_MIN, 60
	MaxSnapshotAge time.Duration // HIVE_WATCHDOG_MAX_SNAPSHOT_AGE_S, 900 s
	HygieneEvery   time.Duration // hourly
}

// DefaultPolicy mirrors the bash defaults.
func DefaultPolicy() Policy {
	return Policy{MaxMutations: 3, BackoffBaseMin: 5, BackoffMaxMin: 120, StallMin: 60,
		MaxSnapshotAge: 900 * time.Second, HygieneEvery: time.Hour}
}

// Input is one watchdog pass's world.
type Input struct {
	Now time.Time
	// Snapshot is /api/status `timestamp`. Zero (unparseable) reads as
	// epoch 0 in bash, i.e. ancient: stall detection is skipped.
	Snapshot time.Time
	Agents   []Agent // /api/status order
	// LivePane: GET /api/pane/<agent>?lines=60, joined with "\n", for the
	// agents NeedsLivePane picked. Absent or "" = unavailable (bash keeps
	// liveSummary then).
	LivePane map[string]string
	Pins     map[string]bool
	// Rotation is the spoke's rotation input (readings, thresholds, tiers,
	// rungs, agents). Repair and rotate-off are decided by internal/rotation.
	Rotation rotation.Input
	Journal  Journal
	Policy   Policy
}

// NeedsLivePane: the live pane is fetched only for agents that look
// unhealthy in the snapshot or are mid-turn (stall candidates). /api/status
// lagged 9+ minutes on school (2026-09-24): an agent the previous pass had
// already healed still showed its old wizard there.
func NeedsLivePane(a Agent) bool {
	return !a.Paused && (Classify(a.LiveSummary) != StateReady || a.Busy == "working")
}

// Plan is one watchdog pass.
type Plan struct {
	// Lines as `hive-rotate.sh watchdog` prints them, WITHOUT the outcome
	// lines of actions (the executor appends those at Action.Anchor).
	Lines   []string
	Actions []Action
	// Journal is the next journal, assuming every action succeeds (bash
	// records the backoff whether or not the restart worked).
	Journal Journal
	Healed  int
	Human   []string
	// Woke: the pass ended at the renewal wake-up (no per-agent checks).
	Woke bool
	// StallChecked is false when the snapshot was too old to judge stalls.
	StallChecked bool
}

func pf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// Watchdog plans one pass (the `[ "$ACTION" = watchdog ]` block).
func Watchdog(in Input) Plan {
	pol := in.Policy
	if pol.MaxMutations == 0 {
		pol = DefaultPolicy()
	}
	j := in.Journal.clone()
	p := Plan{StallChecked: true}
	add := func(l string) int { p.Lines = append(p.Lines, l); return len(p.Lines) - 1 }

	// gather(): remember WHEN an exhausted provider comes back. An exhausted
	// reading with a reset instant (re)stamps it; a provider no longer
	// exhausted drops its stamp, so a stale one cannot re-fire.
	//
	// QUIRK (bash behaviour, kept): gather runs BEFORE the check in the same
	// pass, so a renewal that ccleft already reflects (reading below
	// threshold) clears its own stamp and never wakes anything — the wake
	// fires only while the reading still shows the pool exhausted past its
	// reset. The 20-minute rotation tick re-decides either way.
	for _, prov := range rotation.Providers {
		rd, ok := in.Rotation.Providers[prov]
		if ok && in.Rotation.Exhausted(prov) {
			if t, ok := rotation.ResetsAt(rd.Note); ok {
				j.Resets[prov] = t
			}
		} else {
			delete(j.Resets, prov)
		}
	}

	// 1) Shared-state hygiene: hourly, and again on any wizard heal.
	if in.Now.Sub(j.HygieneAt) >= pol.HygieneEvery {
		p.Actions = append(p.Actions, Action{Kind: KindHygiene, Reason: "hourly shared-home permission repair", Anchor: -1})
		j.HygieneAt = in.Now
	}

	// 1b) RENEWAL WAKE-UP: once a recorded reset passes, re-derive placement
	// from scratch (bash runs a full `hive-rotate.sh apply`) and end the pass.
	var renewed []string
	for prov, due := range j.Resets {
		if !in.Now.Before(due) {
			renewed = append(renewed, prov)
		}
	}
	sort.Strings(renewed) // resets.d/* glob order
	if len(renewed) > 0 {
		for _, prov := range renewed {
			delete(j.Resets, prov)
		}
		at := add(pf("renewal reached for: %s — re-deciding placement from current credits", strings.Join(renewed, " ")))
		p.Actions = append(p.Actions, Action{Kind: KindWake, Providers: renewed, Reason: "provider reset passed", Anchor: at})
		p.Woke = true
		p.Journal = j
		return p
	}

	// v5/v6's /api/status is a periodically rebuilt snapshot that can lag
	// minutes; a frozen snapshot would read as a frozen pane.
	snapAge := in.Now.Unix() - in.Snapshot.Unix()
	if in.Snapshot.IsZero() {
		snapAge = in.Now.Unix()
	}
	if snapAge > int64(pol.MaxSnapshotAge/time.Second) {
		p.StallChecked = false
		add(pf("status snapshot is %ds old — stall detection skipped this pass", snapAge))
	}

	mutations := 0
	budgetOK := func() bool { return mutations < pol.MaxMutations }
	byName := map[string]rotation.Agent{}
	for _, a := range in.Rotation.Agents {
		byName[a.Name] = a
	}

	for _, a := range in.Agents {
		if a.Paused {
			continue
		}
		ra, ok := byName[a.Name]
		if !ok {
			ra = rotation.Agent{Name: a.Name, CLI: a.CLI, Model: a.Model}
		}
		// Half-placed pair that cannot launch: put it back on a model its
		// CURRENT backend runs. Counted as a heal; ends this agent's checks.
		if budgetOK() {
			if want, ok := rotation.MismatchRepair(in.Rotation, ra, in.Pins[a.Name]); ok {
				at := add(pf("%-14s MISMATCH %s/%s -> setting model %s", a.Name, a.CLI, a.Model, want.Model))
				p.Actions = append(p.Actions, Action{Agent: a.Name, Kind: KindRepair, CLI: a.CLI, To: want,
					Reason: "backend cannot run its model", Anchor: at})
				p.Healed++
				mutations++
				continue
			}
		}
		// agy effort drift: a model set by hand/pace/older rotate without
		// the effort its suffix requires runs Gemini 3.6 Flash Low instead.
		if want := EffortChange(a.CLI, a.Model, a.Effort); want != "" {
			if budgetOK() {
				at := add(pf("%-14s EFFORT %s needs --effort %s -> setting", a.Name, a.Model, want))
				p.Actions = append(p.Actions, Action{Agent: a.Name, Kind: KindEffort, CLI: a.CLI, Effort: want,
					Reason: "agy model suffix and --effort disagree", Anchor: at})
				mutations++
			} else {
				add(pf("%-14s effort fix deferred to the next pass (budget)", a.Name))
			}
		}

		pane := a.LiveSummary
		state := Classify(pane)
		// CONFIRM ON THE LIVE PANE before judging.
		if state != StateReady || a.Busy == "working" {
			if live := in.LivePane[a.Name]; live != "" {
				pane = live
				state = Classify(pane)
			}
		}
		if state == StateReady && p.StallChecked && stalled(&j, a.Name, a.Busy, pane, in.Now, pol.StallMin) {
			state = StateStalled
		}
		if state == StateReady {
			add(pf("%-14s liveness ok", a.Name))
			delete(j.Heals, a.Name)
			continue
		}
		// Copilot's device flow is never headlessly recoverable: restarting
		// just burns the crash-loop budget.
		if a.CLI == "copilot" && (state == StateAuth || a.NeedsLogin || (a.AuthKnown && !a.AuthAvailable)) {
			add(pf("%-14s %-8s NEEDS HUMAN LOGIN (copilot device flow) — not restarting", a.Name, state))
			p.Human = append(p.Human, a.Name)
			p.Actions = append(p.Actions, Action{Agent: a.Name, Kind: KindHuman, State: state, CLI: a.CLI,
				Reason: "copilot device-flow login", Anchor: -1})
			continue
		}
		h := j.Heals[a.Name]
		wait := BackoffSeconds(h.Count, pol.BackoffBaseMin, pol.BackoffMaxMin)
		if h.Count > 0 && in.Now.Sub(h.Last) < time.Duration(wait)*time.Second {
			add(pf("%-14s %-8s (healed %dx, backing off %dm)", a.Name, state, h.Count, wait/60))
			continue
		}
		if !budgetOK() {
			add(pf("%-14s %-8s heal deferred to the next pass (budget)", a.Name, state))
			continue
		}
		mutations++
		at := add(pf("%-14s %-8s -> healing (restart #%d)", a.Name, state, h.Count+1))
		if state == StateWizard {
			p.Actions = append(p.Actions, Action{Agent: a.Name, Kind: KindHygiene, State: state,
				Reason: "wizard: shared first-run state drifted", Anchor: -1})
			j.HygieneAt = in.Now
		}
		act := Action{Agent: a.Name, Kind: KindRestart, State: state, CLI: a.CLI, HealCount: h.Count + 1,
			Reason: "pane " + state, Anchor: at}
		// Backend-level failure: restarting on the same rung just re-breaks
		// it — rotate onto a positively-measured-healthy rung first.
		if (state == StateAuth || state == StateShell || state == StateApproval) && !in.Pins[a.Name] {
			if want, ok := rotation.ChooseRungHealthy(in.Rotation, a.Name); ok && (want.Backend != a.CLI || want.Model != a.Model) {
				act.Anchor = add(pf("%-14s %-8s rotating off -> %s %s", a.Name, state, want.Backend, want.Model))
				act.Kind, act.To = KindRotateOff, want
				act.Reason = fmt.Sprintf("pane %s on %s: rotate onto a measured-healthy rung", state, usage.ProviderOf(a.CLI, a.Model))
			}
		}
		if act.Kind == KindRestart {
			delete(j.Panes, a.Name)
		}
		p.Actions = append(p.Actions, act)
		j.Heals[a.Name] = Heal{Count: h.Count + 1, Last: in.Now}
		p.Healed++
	}
	p.Lines = append(p.Lines, pf("watchdog: %d agent(s) healed", p.Healed))
	if len(p.Human) > 0 {
		p.Lines = append(p.Lines, "watchdog: needs a human login: "+strings.Join(p.Human, " "))
	}
	p.Journal = j
	return p
}

// stalled ports pane_stalled: a turn is open (busy=working) and the pane
// has not changed for StallMin minutes. Any other busy value clears the
// record; a changed pane restarts the clock.
func stalled(j *Journal, agent, busy, text string, now time.Time, min int) bool {
	if busy != "working" {
		delete(j.Panes, agent)
		return false
	}
	h := PaneHash(text)
	prev, ok := j.Panes[agent]
	if !ok || prev.Hash != h || prev.Since.IsZero() {
		j.Panes[agent] = PaneSeen{Hash: h, Since: now}
		return false
	}
	return now.Sub(prev.Since) >= time.Duration(min)*time.Minute
}

// RestartLine is bash's outcome line for a restart: `restarted (<status>)`.
func RestartLine(agent, state, status string) string {
	return pf("%-14s %-8s restarted (%s)", agent, state, status)
}

// Render inserts outcome lines (by action index) after each action's
// anchor line.
func Render(lines []string, actions []Action, outcomes map[int][]string) []string {
	after := map[int][]string{}
	for i, a := range actions {
		if a.Anchor >= 0 {
			after[a.Anchor] = append(after[a.Anchor], outcomes[i]...)
		}
	}
	var out []string
	for i, l := range lines {
		out = append(out, l)
		out = append(out, after[i]...)
	}
	return out
}

// ShadowOutcomes are the outcome lines a Shadow plan shows: what bash
// prints when every call succeeds, with the hive's response replaced by
// "would restart".
func ShadowOutcomes(actions []Action) map[int][]string {
	out := map[int][]string{}
	for i, a := range actions {
		if a.Kind == KindRestart {
			out[i] = []string{RestartLine(a.Agent, a.State, "would restart")}
		}
	}
	return out
}
