package liveness

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// ── fixtures ──────────────────────────────────────────────────────────────

// bashTiers is hive-rotate.sh's live TIERS table (2026-10-01).
func bashTiers() []rotation.Rung {
	var out []rotation.Rung
	add := func(tier, p, b, m string) {
		out = append(out, rotation.Rung{Tier: tier, Provider: p, Backend: b, Model: m})
	}
	add("T1", "google", "agy", "gemini-3.8-flash-high")
	add("T1", "kiro", "pi", "kiro-api-key/claude-opus-5:high")
	add("T1", "kiro", "pi", "kiro-api-key/gpt-5-6-sol:high")
	add("T1", "openai", "codex", "gpt-5.6-sol")
	add("T1", "anthropic", "claude", "claude-opus-5")
	add("T2", "google", "agy", "gemini-3.8-flash-low")
	add("T2", "kiro", "pi", "kiro-api-key/claude-sonnet-5:medium")
	add("T2", "kiro", "pi", "kiro-api-key/gpt-5-6-luna:medium")
	add("T2", "openai", "codex", "gpt-5.6-luna")
	add("T2", "anthropic", "claude", "claude-sonnet-5")
	add("T2", "google", "agy", "gemini-3.6-flash-low")
	add("T2", "meta", "muse", "muse-spark-1.3-contributor")
	add("T3", "google", "agy", "gemini-3.8-flash-low")
	add("T3", "kiro", "pi", "kiro-api-key/claude-haiku-4-5:low")
	add("T3", "openai", "codex", "gpt-5.6-luna")
	add("T3", "anthropic", "claude", "claude-haiku-4-5-20251001")
	add("T3", "google", "agy", "gemini-3.6-flash-low")
	add("T3", "meta", "muse", "muse-spark-1.3-contributor")
	return out
}

var fleetTiers = map[string]string{
	"supervisor": "T2", "scanner": "T2", "ci-maintainer": "T2", "quality": "T2", "guide": "T2",
	"outreach": "T2", "sec-check": "T1", "architect": "T1", "strategist": "T1",
	"operations": "T2", "telemetry": "T2",
}

// readings is hive/hive-provider-usage as published at 2026-10-01T14:25Z.
func readings(over map[string]string) map[string]rotation.Reading {
	cm := map[string]string{
		"anthropic": "100% used no-credential (ccleft auth_required cause=no_credentials: needs an interactive login)",
		"google":    "95% used resets=2026-10-07T03:17:08Z",
		"kiro":      "4% used credits=458.85/10000 resets=2026-11-01T00:00:00Z",
		"meta":      "unknown no-usage-api (ccleft unsupported cause=api_key_login)",
		"openai":    "100% used weekly=100% resets=2026-10-03T22:41:44Z",
	}
	for k, v := range over {
		cm[k] = v
	}
	out := map[string]rotation.Reading{}
	for p, v := range cm {
		l := usage.PublishedToProbe(v)
		out[p] = rotation.Reading{Percent: float64(l.Percent), Note: l.Note}
	}
	return out
}

type statusFixture struct {
	Timestamp string `json:"timestamp"`
	Budget    struct {
		Exhausted bool    `json:"BUDGET_EXHAUSTED"`
		Pct       float64 `json:"BUDGET_PCT_USED"`
		Weekly    float64 `json:"BUDGET_WEEKLY"`
	} `json:"budget"`
	Agents []struct {
		Name, CLI, GovModel, Model, Cadence, Busy string
		ReasoningEffort                           string `json:"reasoningEffort"`
		PausedTrigger                             string `json:"pausedTrigger"`
		Paused                                    bool
		OnDemand                                  *bool  `json:"onDemand"`
		Enabled                                   *bool  `json:"enabled"`
		NeedsLogin                                bool   `json:"needsLogin"`
		AuthKnown                                 bool   `json:"authKnown"`
		AuthAvailable                             bool   `json:"authAvailable"`
		LiveSummary                               string `json:"liveSummary"`
	} `json:"agents"`
}

func loadFixture(t *testing.T, spoke, stamp string) statusFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/status-" + spoke + "-" + stamp + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var st statusFixture
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// liveInputFrom builds a watchdog Input from a captured /api/status, the
// live panes captured with it (GET /api/pane/<a>?lines=60) and the 14:25Z
// readings.
func liveInputFrom(t *testing.T, spoke, stamp string, now time.Time) Input {
	t.Helper()
	st := loadFixture(t, spoke, stamp)
	var panes map[string]string
	b, err := os.ReadFile("testdata/panes-" + spoke + "-" + stamp + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &panes); err != nil {
		t.Fatal(err)
	}
	snap, _ := time.Parse(time.RFC3339, st.Timestamp)
	in := Input{Now: now, Snapshot: snap, LivePane: map[string]string{}, Pins: map[string]bool{},
		Journal: Journal{}, Policy: DefaultPolicy(),
		Rotation: rotation.Input{Now: now, Providers: readings(nil), Tiers: fleetTiers, Rungs: bashTiers(), Policy: rotation.DefaultPolicy()}}
	for _, a := range st.Agents {
		m := a.GovModel
		if m == "" {
			m = a.Model
		}
		la := Agent{Name: a.Name, CLI: a.CLI, Model: m, Effort: a.ReasoningEffort, Paused: a.Paused, Busy: a.Busy,
			NeedsLogin: a.NeedsLogin, AuthKnown: a.AuthKnown, AuthAvailable: a.AuthAvailable, LiveSummary: a.LiveSummary}
		in.Agents = append(in.Agents, la)
		in.Rotation.Agents = append(in.Rotation.Agents, rotation.Agent{Name: a.Name, CLI: a.CLI, Model: m, Effort: a.ReasoningEffort,
			Cadence: a.Cadence, Paused: a.Paused, PausedTrigger: a.PausedTrigger, OnDemand: a.OnDemand != nil && *a.OnDemand})
		// The controller fetches the live pane only for the agents it must.
		if NeedsLivePane(la) {
			in.LivePane[a.Name] = panes[a.Name]
		}
	}
	return in
}

// bashLog reads a job log without the lines the operator never prints
// (the ccleft banner).
func bashLog(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); !strings.HasPrefix(l, "usage: ") {
			out = append(out, l)
		}
	}
	return out
}

func diffLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("differs from the bash job log\n--- bash\n%s\n--- operator\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

// enforceOutcomes renders a pass as an Enforce run whose every restart the
// hive answered `{"ok":true,"status":"restarted"}`.
func enforceOutcomes(p Plan) []string {
	out := map[int][]string{}
	for i, a := range p.Actions {
		if a.Kind == KindRestart {
			out[i] = []string{RestartLine(a.Agent, a.State, "restarted")}
		}
	}
	return Render(p.Lines, p.Actions, out)
}

// ── golden: the live watchdog job logs ───────────────────────────────────

// GOLDEN: hive-watchdog (school) 15:25Z, hive-watchdog-hanthor 15:24Z and
// hive-watchdog-reef 15:27Z, from the /api/status snapshots and live panes
// read at 15:23Z. Every unpaused agent is ready; hanthor/guide and the
// busy=working agents get their live pane re-read and a first stall-clock
// stamp (no stall on a first sighting).
func TestGoldenWatchdogAllReady20261001(t *testing.T) {
	for _, c := range []struct {
		spoke, log string
		now        time.Time
	}{
		{"school", "testdata/hive-watchdog-school-20261001T1525.log", time.Date(2026, 10, 1, 15, 25, 4, 0, time.UTC)},
		{"hanthor", "testdata/hive-watchdog-hanthor-20261001T1524.log", time.Date(2026, 10, 1, 15, 24, 4, 0, time.UTC)},
		{"reef", "testdata/hive-watchdog-reef-20261001T1527.log", time.Date(2026, 10, 1, 15, 27, 4, 0, time.UTC)},
	} {
		t.Run(c.spoke, func(t *testing.T) {
			in := liveInputFrom(t, c.spoke, "20261001T1523", c.now)
			p := Watchdog(in)
			diffLines(t, enforceOutcomes(p), bashLog(t, c.log))
			for _, a := range p.Actions {
				if a.Mutating() {
					t.Errorf("all-ready pass planned %+v", a)
				}
			}
			for _, a := range in.Agents {
				_, clock := p.Journal.Panes[a.Name]
				if want := !a.Paused && a.Busy == "working"; clock != want {
					t.Errorf("%s: stall clock %v, want %v", a.Name, clock, want)
				}
			}
		})
	}
}

// GOLDEN: hive-watchdog-hanthor 15:14Z healed supervisor as STALLED (a turn
// open with a byte-identical pane for ≥ 60 min) with restart #1, and the
// 15:19Z pass saw it ready again, which clears the backoff record.
//
// The 15:14Z /api/status was not captured; supervisor's pane and turn are
// reconstructed from the log (busy=working, the same pane first seen at
// 14:13Z), every other agent is the 15:23Z capture (all ready then too).
func TestGoldenWatchdogHanthorStall20261001T1514(t *testing.T) {
	now := time.Date(2026, 10, 1, 15, 14, 3, 0, time.UTC)
	in := liveInputFrom(t, "hanthor", "20261001T1523", now)
	in.Snapshot = time.Date(2026, 10, 1, 15, 10, 0, 0, time.UTC)
	pane := "  muse-spark-1.3-contributor · max · /data/agents/supervisor · Auto-review"
	in.Agents[0].Busy, in.Agents[0].LiveSummary = "working", pane
	in.LivePane["supervisor"] = pane
	// guide was idle at 15:14 (its turn opened later).
	for i := range in.Agents {
		if in.Agents[i].Name == "guide" {
			in.Agents[i].Busy = "idle"
			delete(in.LivePane, "guide")
		}
	}
	in.Journal = Journal{Panes: map[string]PaneSeen{"supervisor": {PaneHash(pane), now.Add(-61 * time.Minute)}}}
	p := Watchdog(in)
	diffLines(t, enforceOutcomes(p), bashLog(t, "testdata/hive-watchdog-hanthor-20261001T1514.log"))
	if len(p.Actions) != 2 || p.Actions[1].Kind != KindRestart || p.Actions[1].State != StateStalled || p.Actions[1].CLI != "muse" {
		t.Fatalf("%+v", p.Actions)
	}
	if h := p.Journal.Heals["supervisor"]; h.Count != 1 || !h.Last.Equal(now) {
		t.Fatalf("backoff record %+v", h)
	}
	if _, ok := p.Journal.Panes["supervisor"]; ok {
		t.Fatal("a restart must drop the stall clock")
	}

	// 15:19Z: ready → backoff record cleared.
	now2 := time.Date(2026, 10, 1, 15, 19, 3, 0, time.UTC)
	in2 := liveInputFrom(t, "hanthor", "20261001T1523", now2)
	in2.Snapshot = time.Date(2026, 10, 1, 15, 15, 0, 0, time.UTC)
	for i := range in2.Agents {
		if in2.Agents[i].Name == "guide" {
			in2.Agents[i].Busy = "idle"
			delete(in2.LivePane, "guide")
		}
	}
	in2.Journal = p.Journal
	p2 := Watchdog(in2)
	diffLines(t, enforceOutcomes(p2), bashLog(t, "testdata/hive-watchdog-hanthor-20261001T1519.log"))
	if _, ok := p2.Journal.Heals["supervisor"]; ok {
		t.Fatal("ready must clear the backoff record")
	}
}

// GOLDEN: hive-watchdog (school) 15:15Z printed the stale-snapshot line —
// /api/status was 1790 s old — and judged no stalls.
func TestGoldenWatchdogStaleSnapshot20261001T1515(t *testing.T) {
	now := time.Date(2026, 10, 1, 15, 15, 3, 0, time.UTC)
	in := liveInputFrom(t, "school", "20261001T1523", now)
	in.Snapshot = now.Add(-1790 * time.Second)
	// A long-frozen busy pane must NOT read as stalled on a stale snapshot.
	for i := range in.Agents {
		if in.Agents[i].Busy == "working" {
			in.Journal.Panes = map[string]PaneSeen{in.Agents[i].Name: {PaneHash(in.LivePane[in.Agents[i].Name]), now.Add(-3 * time.Hour)}}
			break
		}
	}
	p := Watchdog(in)
	diffLines(t, enforceOutcomes(p), bashLog(t, "testdata/hive-watchdog-school-20261001T1515.log"))
	if p.StallChecked {
		t.Fatal("stall detection must be skipped")
	}
}

// ── table tests: classification → action, limits, ladders ────────────────

func baseInput(now time.Time, agents ...Agent) Input {
	in := Input{Now: now, Snapshot: now.Add(-time.Minute), Agents: agents, LivePane: map[string]string{}, Pins: map[string]bool{},
		Policy: DefaultPolicy(), Journal: Journal{HygieneAt: now.Add(-time.Minute)},
		Rotation: rotation.Input{Now: now, Providers: readings(map[string]string{"google": "12% used resets=2026-10-07T03:17:08Z",
			"openai": "20% used weekly=20%"}), Tiers: fleetTiers, Rungs: bashTiers(), Policy: rotation.DefaultPolicy()}}
	for _, a := range agents {
		in.Rotation.Agents = append(in.Rotation.Agents, rotation.Agent{Name: a.Name, CLI: a.CLI, Model: a.Model, Cadence: "1h"})
	}
	return in
}

const readyPane = "  (kiro-api-key) claude-sonnet-5 • medium"

func ag(name, cli, model, pane string) Agent {
	return Agent{Name: name, CLI: cli, Model: model, Busy: "idle", LiveSummary: pane}
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestWatchdogHealPerClassification(t *testing.T) {
	cases := []struct {
		state, pane, kind, rotateTo string
		cli, model                  string
	}{
		// Dialogs: repair perms and relaunch IN PLACE (never rotate).
		{StateWizard, "Choose your color scheme\n[Next]", KindRestart, "", "agy", "gemini-3.8-flash-low"},
		{StateEmpty, "", KindRestart, "", "pi", "kiro-api-key/claude-sonnet-5:medium"},
		// Backend-level: rotate onto a positively-measured healthy rung.
		// With google 12 % and openai 20 % measured, kiro 4 %: cost rank
		// google 0 wins (and for a google agent, kiro 1). Within google the
		// whole-line sort tie-break (bash QUIRK) puts gemini-3.6 before 3.8.
		{StateAuth, "Select login method:", KindRotateOff, "agy gemini-3.6-flash-low", "claude", "claude-sonnet-5"},
		{StateShell, "hive-guide@hive-1:/data/agents/guide$", KindRotateOff, "pi kiro-api-key/claude-sonnet-5:medium", "agy", "gemini-3.8-flash-low"},
		{StateApproval, "Would you like to run the following command?\n› 1. Yes, proceed (y)", KindRotateOff, "agy gemini-3.6-flash-low", "muse", "muse-spark-1.3-contributor"},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			in := baseInput(t0, ag("guide", c.cli, c.model, c.pane))
			if c.cli == "agy" {
				in.Agents[0].Effort = "low"
			}
			p := Watchdog(in)
			var heal *Action
			for i := range p.Actions {
				if p.Actions[i].Kind == KindRestart || p.Actions[i].Kind == KindRotateOff {
					heal = &p.Actions[i]
				}
			}
			if heal == nil || heal.Kind != c.kind || heal.State != c.state || heal.HealCount != 1 {
				t.Fatalf("%+v\n%s", p.Actions, strings.Join(p.Lines, "\n"))
			}
			if c.rotateTo != "" && heal.To.Backend+" "+heal.To.Model != c.rotateTo {
				t.Fatalf("rotate-off to %s %s, want %s", heal.To.Backend, heal.To.Model, c.rotateTo)
			}
			if !strings.Contains(p.Lines[0], "-> healing (restart #1)") {
				t.Fatalf("%v", p.Lines)
			}
			if c.state == StateWizard && (p.Actions[0].Kind != KindHygiene || p.Journal.HygieneAt != t0) {
				t.Fatalf("a wizard heal repairs the shared first-run state first: %+v", p.Actions)
			}
		})
	}
}

// A PINNED agent is healed in place: rotate-off and mismatch repair both
// only ever pick tier members, so neither may touch a deliberate pin.
func TestWatchdogPinnedNeverRotatedOrRepaired(t *testing.T) {
	in := baseInput(t0, ag("supervisor", "claude", "claude-sonnet-5", "Select login method:"))
	in.Pins["supervisor"] = true
	p := Watchdog(in)
	if len(p.Actions) != 1 || p.Actions[0].Kind != KindRestart {
		t.Fatalf("%+v", p.Actions)
	}
	in = baseInput(t0, ag("supervisor", "codex", "gpt-5.4-mini-but-claude-opus", readyPane))
	in.Agents[0].Model = "claude-opus-5" // codex cannot run it
	in.Rotation.Agents[0].Model = "claude-opus-5"
	in.Pins["supervisor"] = true
	if p := Watchdog(in); len(p.Actions) != 0 {
		t.Fatalf("pinned mismatch repaired: %+v", p.Actions)
	}
	in.Pins = map[string]bool{}
	p = Watchdog(in)
	if len(p.Actions) != 1 || p.Actions[0].Kind != KindRepair || p.Actions[0].To.Model != "gpt-5.6-luna" ||
		p.Lines[0] != "supervisor     MISMATCH codex/claude-opus-5 -> setting model gpt-5.6-luna" {
		t.Fatalf("%+v %v", p.Actions, p.Lines)
	}
}

// rotate-off never picks an UNMEASURED provider (meta) or the current one,
// and the Kiro cadence guard holds even with the escape hatch: a 5-minute
// driver is restarted in place rather than moved onto kiro.
func TestWatchdogRotateOffNeedsMeasuredHealthy(t *testing.T) {
	// muse/meta is unmeasured (never "exhausted"); google 95, openai and
	// anthropic 100 are exhausted; kiro 4 is the only healthy pool.
	in := baseInput(t0, ag("scanner", "muse", "muse-spark-1.3-contributor", "hive-scanner@h:/x$"))
	in.Rotation.Providers = readings(nil)
	in.Rotation.Agents[0].Cadence = "5m"
	p := Watchdog(in)
	if len(p.Actions) != 1 || p.Actions[0].Kind != KindRestart {
		t.Fatalf("5m agent must not rotate onto kiro: %+v", p.Actions)
	}
	in.Rotation.Agents[0].Cadence = "1h"
	p = Watchdog(in)
	if p.Actions[0].Kind != KindRotateOff || p.Actions[0].To.Provider != "kiro" {
		t.Fatalf("%+v", p.Actions)
	}
	// An exhausted CURRENT provider waives the kiro guard (provider_ok's
	// kiro rule is waived by that alone, not by the escape hatch).
	in = baseInput(t0, ag("scanner", "agy", "gemini-3.8-flash-low", "hive-scanner@h:/x$"))
	in.Agents[0].Effort = "low"
	in.Rotation.Providers = readings(nil)
	in.Rotation.Agents[0].Cadence = "5m"
	p = Watchdog(in)
	if p.Actions[0].Kind != KindRotateOff || p.Actions[0].To.Provider != "kiro" {
		t.Fatalf("%+v", p.Actions)
	}
	// Never onto an unmeasured pool: with kiro unmeasured too, nothing.
	in.Rotation.Providers["kiro"] = rotation.Reading{Percent: -1, Note: "no-usage-api"}
	p = Watchdog(in)
	if p.Actions[0].Kind != KindRestart {
		t.Fatalf("%+v", p.Actions)
	}
}

func TestWatchdogCopilotNeedsHuman(t *testing.T) {
	for _, mut := range []func(*Agent){
		func(a *Agent) { a.LiveSummary = "Sign in to use Copilot" },
		func(a *Agent) { a.LiveSummary = ""; a.NeedsLogin = true },
		func(a *Agent) { a.LiveSummary = "hive-r@h:/x$"; a.AuthKnown = true; a.AuthAvailable = false },
	} {
		a := ag("reviewer", "copilot", "claude-fable-5", "")
		mut(&a)
		p := Watchdog(baseInput(t0, a))
		if len(p.Actions) != 1 || p.Actions[0].Kind != KindHuman || p.Healed != 0 {
			t.Fatalf("%+v", p.Actions)
		}
		if p.Lines[len(p.Lines)-1] != "watchdog: needs a human login: reviewer" {
			t.Fatalf("%v", p.Lines)
		}
	}
	// copilot that is merely ready is fine; copilot with a dead shell and
	// valid auth is restarted like anyone else.
	a := ag("reviewer", "copilot", "claude-fable-5", "hive-r@h:/x$")
	a.AuthKnown, a.AuthAvailable = true, true
	if p := Watchdog(baseInput(t0, a)); p.Actions[0].Kind == KindHuman {
		t.Fatalf("%+v", p.Actions)
	}
}

// At most MaxMutations (3) restart-causing actions per pass; the rest are
// deferred with bash's line, and record nothing.
func TestWatchdogMaxThreeMutationsPerPass(t *testing.T) {
	var agents []Agent
	for _, n := range []string{"scanner", "quality", "guide", "outreach", "operations"} {
		agents = append(agents, ag(n, "pi", "kiro-api-key/claude-sonnet-5:medium", ""))
	}
	p := Watchdog(baseInput(t0, agents...))
	muts := 0
	for _, a := range p.Actions {
		if a.Mutating() {
			muts++
		}
	}
	if muts != 3 || p.Healed != 3 {
		t.Fatalf("%d mutations, %d healed", muts, p.Healed)
	}
	want := []string{
		"outreach       empty    heal deferred to the next pass (budget)",
		"operations     empty    heal deferred to the next pass (budget)",
		"watchdog: 3 agent(s) healed",
	}
	diffLines(t, p.Lines[len(p.Lines)-3:], want)
	if _, ok := p.Journal.Heals["outreach"]; ok {
		t.Fatal("a deferred heal must not record a backoff")
	}
	// Effort fixes count too, and are deferred with their own line.
	var agy []Agent
	for _, n := range []string{"scanner", "quality", "guide", "outreach"} {
		a := ag(n, "agy", "gemini-3.8-flash-high", readyPane)
		a.Effort = "low"
		agy = append(agy, a)
	}
	p = Watchdog(baseInput(t0, agy...))
	if !contains(p.Lines, "outreach       effort fix deferred to the next pass (budget)") ||
		!contains(p.Lines, "scanner        EFFORT gemini-3.8-flash-high needs --effort high -> setting") {
		t.Fatalf("%v", p.Lines)
	}
	// A mismatch repair is skipped (not deferred) when the budget is spent.
	agents = append(agents[:3], ag("ci-maintainer", "pi", "gemini-3.8-flash-low", readyPane))
	p = Watchdog(baseInput(t0, agents...))
	for _, a := range p.Actions {
		if a.Kind == KindRepair {
			t.Fatalf("repair past the budget: %+v", a)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// The backoff ladder in action: heal #1, then back off 5, 10, 20 … minutes.
func TestWatchdogBackoff(t *testing.T) {
	a := ag("guide", "pi", "kiro-api-key/claude-sonnet-5:medium", "")
	j := Journal{}
	now := t0
	var counts []string
	for step := 0; step < 12; step++ {
		in := baseInput(now, a)
		in.Journal = j
		in.Journal.HygieneAt = now
		p := Watchdog(in)
		if p.Healed == 1 {
			counts = append(counts, strconv.Itoa(int(now.Sub(t0).Minutes())))
		} else if !strings.Contains(p.Lines[0], "backing off") {
			t.Fatalf("%v", p.Lines)
		}
		j = p.Journal
		now = now.Add(5 * time.Minute)
	}
	// Heals at 0, then +5, +10, +20 (cumulative 0 5 15 35).
	if strings.Join(counts, " ") != "0 5 15 35" {
		t.Fatalf("heals at minutes %v", counts)
	}
	in := baseInput(now, a)
	in.Journal = j
	p := Watchdog(in)
	if p.Lines[0] != "guide          empty    (healed 4x, backing off 40m)" {
		t.Fatalf("%v", p.Lines)
	}
}

// The live pane wins over the lagging snapshot: an agent the previous pass
// healed still shows its old wizard in /api/status.
func TestWatchdogLivePaneRecheck(t *testing.T) {
	a := ag("guide", "agy", "gemini-3.8-flash-low", "Choose your color scheme\n[Next]")
	a.Effort = "low"
	if !NeedsLivePane(a) {
		t.Fatal("an unhealthy snapshot needs the live pane")
	}
	in := baseInput(t0, a)
	in.LivePane["guide"] = readyPane
	if p := Watchdog(in); len(p.Actions) != 0 || p.Lines[0] != "guide          liveness ok" {
		t.Fatalf("%+v %v", p.Actions, p.Lines)
	}
	// And the other way: a ready snapshot of a mid-turn agent whose live
	// pane is a login prompt.
	b := ag("guide", "claude", "claude-sonnet-5", readyPane)
	b.Busy = "working"
	if !NeedsLivePane(b) {
		t.Fatal("a mid-turn agent needs the live pane")
	}
	in = baseInput(t0, b)
	in.LivePane["guide"] = "Login expired · Please run /login"
	if p := Watchdog(in); p.Actions[0].State != StateAuth {
		t.Fatalf("%+v", p.Actions)
	}
	// An empty live pane (fetch failed) keeps the snapshot's verdict.
	in.LivePane["guide"] = ""
	if p := Watchdog(in); len(p.Actions) != 0 {
		t.Fatalf("%+v", p.Actions)
	}
	if NeedsLivePane(ag("x", "pi", "m", readyPane)) {
		t.Fatal("an idle ready agent needs no pane fetch")
	}
}

func TestWatchdogStall(t *testing.T) {
	a := ag("architect", "pi", "kiro-api-key/claude-opus-5:high", readyPane)
	a.Busy = "working"
	in := baseInput(t0, a)
	in.LivePane["architect"] = readyPane
	p := Watchdog(in)
	if len(p.Actions) != 0 || p.Journal.Panes["architect"].Since != t0 {
		t.Fatalf("first sighting stamps the clock: %+v", p.Journal.Panes)
	}
	// 59 minutes unchanged: still ok; 60: stalled.
	in.Journal, in.Now = p.Journal, t0.Add(59*time.Minute)
	in.Snapshot, in.Journal.HygieneAt = in.Now, in.Now
	if p2 := Watchdog(in); len(p2.Actions) != 0 {
		t.Fatalf("%+v", p2.Actions)
	}
	in.Now, in.Snapshot = t0.Add(60*time.Minute), t0.Add(60*time.Minute)
	in.Journal.HygieneAt = in.Now
	p3 := Watchdog(in)
	if len(p3.Actions) != 1 || p3.Actions[0].State != StateStalled || p3.Actions[0].Kind != KindRestart {
		t.Fatalf("%+v", p3.Actions)
	}
	// A changed pane restarts the clock; a turn that closed clears it.
	in.LivePane["architect"] = readyPane + "\n⠋ Working (61m)"
	if p4 := Watchdog(in); len(p4.Actions) != 0 || p4.Journal.Panes["architect"].Since != in.Now {
		t.Fatalf("%+v", p4.Journal.Panes)
	}
	in.Agents[0].Busy = "idle"
	if p5 := Watchdog(in); len(p5.Journal.Panes) != 0 {
		t.Fatalf("%+v", p5.Journal.Panes)
	}
}

// Renewal wake-up: gather() stamps an exhausted provider's reset; once it
// passes (and the reading STILL shows it exhausted — see the QUIRK) the
// pass re-decides placement and ends.
func TestWatchdogRenewalWake(t *testing.T) {
	in := baseInput(t0, ag("guide", "pi", "kiro-api-key/claude-sonnet-5:medium", readyPane))
	in.Rotation.Providers = readings(map[string]string{"openai": "100% used weekly=100% resets=2026-10-01T11:55:00Z"})
	p := Watchdog(in)
	if !p.Woke || p.Lines[0] != "renewal reached for: openai — re-deciding placement from current credits" ||
		len(p.Lines) != 1 || p.Actions[len(p.Actions)-1].Kind != KindWake {
		t.Fatalf("%v %+v", p.Lines, p.Actions)
	}
	if _, ok := p.Journal.Resets["openai"]; ok {
		t.Fatal("a fired stamp is removed")
	}
	// google (95 %, resets 10-07) stays pending.
	if r := p.Journal.Resets["google"]; !r.Equal(time.Date(2026, 10, 7, 3, 17, 8, 0, time.UTC)) {
		t.Fatalf("%+v", p.Journal.Resets)
	}
	// QUIRK: a provider no longer exhausted drops its stamp before the
	// check, so a renewal ccleft already reflects never wakes anything.
	in.Journal.Resets = map[string]time.Time{"kiro": t0.Add(-time.Hour)}
	in.Rotation.Providers = readings(nil)
	if p := Watchdog(in); p.Woke {
		t.Fatalf("%v", p.Lines)
	}
}

func TestWatchdogHygieneHourly(t *testing.T) {
	in := baseInput(t0, ag("guide", "pi", "kiro-api-key/claude-sonnet-5:medium", readyPane))
	in.Journal.HygieneAt = t0.Add(-59 * time.Minute)
	if p := Watchdog(in); len(p.Actions) != 0 {
		t.Fatalf("%+v", p.Actions)
	}
	in.Journal.HygieneAt = t0.Add(-time.Hour)
	if p := Watchdog(in); len(p.Actions) != 1 || p.Actions[0].Kind != KindHygiene || p.Journal.HygieneAt != t0 {
		t.Fatalf("%+v", p.Actions)
	}
}

func TestWatchdogSkipsPaused(t *testing.T) {
	a := ag("brainstorm", "muse", "muse-spark-1.3-contributor", "")
	a.Paused = true
	if NeedsLivePane(a) {
		t.Fatal("paused agents are never probed")
	}
	if p := Watchdog(baseInput(t0, a)); len(p.Lines) != 1 || p.Lines[0] != "watchdog: 0 agent(s) healed" {
		t.Fatalf("%v", p.Lines)
	}
}

// GOLDEN: the 15:41Z capture against hive-watchdog-reef 15:42Z and
// hive-watchdog-hanthor 15:44Z (all ready), and hive-watchdog (school)
// 15:40Z, which healed scanner (muse) as STALLED. The capture is a minute
// after that heal; scanner's open turn and unchanged pane are restored from
// the log (its stall clock first stamped an hour before).
func TestGoldenWatchdog20261001T1540(t *testing.T) {
	for _, c := range []struct {
		spoke, log string
		now        time.Time
	}{
		{"reef", "testdata/hive-watchdog-reef-20261001T1542.log", time.Date(2026, 10, 1, 15, 42, 4, 0, time.UTC)},
		{"hanthor", "testdata/hive-watchdog-hanthor-20261001T1544.log", time.Date(2026, 10, 1, 15, 44, 4, 0, time.UTC)},
	} {
		in := liveInputFrom(t, c.spoke, "20261001T1541", c.now)
		diffLines(t, enforceOutcomes(Watchdog(in)), bashLog(t, c.log))
	}

	now := time.Date(2026, 10, 1, 15, 40, 4, 0, time.UTC)
	in := liveInputFrom(t, "school", "20261001T1541", now)
	in.Snapshot = time.Date(2026, 10, 1, 15, 36, 0, 0, time.UTC)
	pane := in.LivePane["scanner"]
	if pane == "" {
		t.Fatal("fixture: scanner was mid-turn at 15:41")
	}
	in.Journal = Journal{Panes: map[string]PaneSeen{"scanner": {PaneHash(pane), now.Add(-62 * time.Minute)}}}
	p := Watchdog(in)
	diffLines(t, enforceOutcomes(p), bashLog(t, "testdata/hive-watchdog-school-20261001T1540.log"))
	var heal Action
	for _, a := range p.Actions {
		if a.Agent == "scanner" {
			heal = a
		}
	}
	// muse: the restart reopens the shared first-run state in the same exec.
	if heal.Kind != KindRestart || heal.CLI != "muse" || heal.State != StateStalled {
		t.Fatalf("%+v", heal)
	}
}
