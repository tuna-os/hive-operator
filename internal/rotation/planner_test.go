package rotation

import (
	"strings"
	"testing"
	"time"
)

// bashTiers is hive-rotate.sh's built-in TIERS table (lines 191-208), in
// order, with the defaults T1=claude-opus-5, T2=claude-sonnet-5 and
// META_MODEL=muse-spark-1.3-contributor.
func bashTiers() []Rung {
	var out []Rung
	add := func(tier, p, b, m string) { out = append(out, Rung{tier, p, b, m}) }
	add("T1", "google", "agy", "gemini-3.8-flash-high")
	add("T1", "deepseek", "pi", "deepseek-flash")
	add("T1", "openai", "codex", "gpt-5.6-sol")
	add("T1", "anthropic", "claude", "claude-opus-5")
	for _, t := range []string{"T2", "T3"} {
		add(t, "google", "agy", "gemini-3.8-flash-low")
		add(t, "deepseek", "pi", "deepseek-flash")
		add(t, "openai", "codex", "gpt-5.6-luna")
		if t == "T2" {
			add(t, "anthropic", "claude", "claude-sonnet-5")
		} else {
			add(t, "anthropic", "claude", "claude-haiku-4-5-20251001")
		}
		add(t, "google", "agy", "gemini-3.6-flash-low")
		add(t, "meta", "muse", "muse-spark-1.3-contributor")
	}
	return out
}

// primaryRungs is tier_members on the primary hive on 2026-09-24: the
// hive-tiers cache (Artificial Analysis) UNION the built-in table, cache
// first, de-duplicated on provider|model, gated by the live inventory
// (claude-opus-5-xhigh and claude-fable-5-1-low are not ids the CLI offers;
// openai was not inventoried, so all its cached rungs survive).
func primaryRungs() []Rung {
	cache := []Rung{
		{"T1", "anthropic", "claude", "claude-fable-5-1"},
		{"T1", "openai", "codex", "gpt-6-astra"},
		{"T1", "openai", "codex", "gpt-6-astra-xhigh"},
		{"T2", "openai", "codex", "gpt-5-6-luna"},
		{"T2", "openai", "codex", "gpt-5-5"},
	}
	return append(cache, bashTiers()...)
}

var fleetTiers = map[string]string{
	"supervisor": "T2", "scanner": "T2", "ci-maintainer": "T2", "quality": "T2", "guide": "T2",
	"outreach": "T2", "operations": "T2", "telemetry": "T2",
	"architect": "T1", "sec-check": "T1", "strategist": "T1",
}

// The school spoke as /api/status showed it at the 17:00Z tick.
func schoolAgents() []Agent {
	agy := func(n, m string) Agent { return Agent{Name: n, CLI: "agy", Model: m, Cadence: "1h"} }
	return []Agent{
		agy("supervisor", "gemini-3.6-flash-low"),
		agy("scanner", "gemini-3.6-flash-low"),
		{Name: "reviewer", CLI: "copilot", Model: "claude-fable-5", Paused: true, PausedTrigger: "dashboard-api"},
		agy("ci-maintainer", "gemini-3.6-flash-low"),
		agy("quality", "gemini-3.6-flash-low"),
		agy("architect", "gemini-3.8-flash-high"),
		agy("guide", "gemini-3.6-flash-low"),
		agy("outreach", "gemini-3.6-flash-low"),
		{Name: "sec-check", CLI: "claude", Model: "claude-sonnet-5", Cadence: "2h"},
		agy("telemetry", "gemini-3.6-flash-low"),
		agy("operations", "gemini-3.6-flash-low"),
		{Name: "strategist", CLI: "claude", Model: "claude-sonnet-5", Cadence: "2h"},
		{Name: "brainstorm", CLI: "muse", Model: "muse-spark-1.3-contributor", Paused: true, PausedTrigger: "startup", OnDemand: true},
	}
}

// hive/hive-provider-usage at 2026-09-24T17:12:32Z.
func probeReadings() map[string]Reading {
	return map[string]Reading{
		"anthropic": {34, "resets=2026-09-24T17:40:00.326149+00:00"},
		"deepseek":  {100, "balance=-1.23"},
		"google":    {52, "resets=2026-09-24T19:17:07Z"},
		"meta":      {-1, "no-usage-api"},
		"openai":    {100, "weekly=100% resets=2026-09-26T08:15:18Z"},
	}
}

// Thursday 17:20Z: outside the DeepSeek peak windows.
var tick = time.Date(2026, 9, 24, 17, 20, 0, 0, time.UTC)

func schoolInput() Input {
	return Input{
		Now: tick, Namespace: "hive", Primary: true,
		Agents: schoolAgents(), Providers: probeReadings(), Tiers: fleetTiers, Rungs: primaryRungs(),
		Pins: map[string]bool{"supervisor": true}, Holds: map[string]bool{"reviewer": true},
		Policy: DefaultPolicy(),
	}
}

// GOLDEN: the agent lines of the hive-rotate-29837820 job log (school,
// 17:00Z) — the shadow planner must reproduce them byte for byte.
func TestGoldenSchoolTick(t *testing.T) {
	want := `reviewer       operator  held paused by HIVE_ROTATE_HOLD (declared in git)
supervisor     google    pinned by HIVE_ROTATE_PIN — placement left alone
scanner        google    gemini-3.6-flash-low ok
ci-maintainer  google    gemini-3.6-flash-low ok
quality        google    gemini-3.6-flash-low ok
architect      google    gemini-3.8-flash-high ok
guide          google    gemini-3.6-flash-low ok
outreach       google    gemini-3.6-flash-low ok
sec-check      anthropic claude-sonnet-5 ok (pace-demoted from claude-fable-5-1)
telemetry      google    gemini-3.6-flash-low ok
operations     google    gemini-3.6-flash-low ok
strategist     anthropic claude-sonnet-5 ok (pace-demoted from claude-fable-5-1)
fleet already on the best available rung`
	got := strings.Join(Compute(schoolInput()).Lines(true), "\n")
	if got != want {
		t.Fatalf("plan differs from the bash job\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// GOLDEN: hive-rotate-reef-29837827 (reef runs the built-in table only: its
// state dir has no tiers cache, so strategist was demoted from claude-opus-5).
func TestGoldenReefTick(t *testing.T) {
	agy := func(n, m string) Agent { return Agent{Name: n, CLI: "agy", Model: m, Cadence: "1h"} }
	in := Input{
		Now: tick, Namespace: "hive-reef", Tiers: fleetTiers, Rungs: bashTiers(),
		Providers: probeReadings(), Policy: DefaultPolicy(),
		Agents: []Agent{
			agy("supervisor", "gemini-3.6-flash-low"), agy("scanner", "gemini-3.6-flash-low"),
			{Name: "reviewer", CLI: "copilot", Model: "claude-fable-5"},
			{Name: "brainstorm", CLI: "muse", Model: "muse-spark-1.3-contributor", Paused: true, PausedTrigger: "startup", OnDemand: true},
			agy("ci-maintainer", "gemini-3.6-flash-low"), agy("quality", "gemini-3.6-flash-low"),
			agy("architect", "gemini-3.8-flash-high"), agy("guide", "gemini-3.6-flash-low"),
			{Name: "outreach", CLI: "muse", Model: "muse-spark-1.3-contributor", Cadence: "4h"},
			agy("sec-check", "gemini-3.8-flash-high"), agy("telemetry", "gemini-3.6-flash-low"),
			agy("operations", "gemini-3.6-flash-low"),
			{Name: "strategist", CLI: "claude", Model: "claude-sonnet-5", Cadence: "2h"},
		},
	}
	want := `supervisor     google    gemini-3.6-flash-low ok
scanner        google    gemini-3.6-flash-low ok
ci-maintainer  google    gemini-3.6-flash-low ok
quality        google    gemini-3.6-flash-low ok
architect      google    gemini-3.8-flash-high ok
guide          google    gemini-3.6-flash-low ok
outreach       meta      muse-spark-1.3-contributor ok
sec-check      google    gemini-3.8-flash-high ok
telemetry      google    gemini-3.6-flash-low ok
operations     google    gemini-3.6-flash-low ok
strategist     anthropic claude-sonnet-5 ok (pace-demoted from claude-opus-5)
fleet already on the best available rung`
	if got := strings.Join(Compute(in).Lines(true), "\n"); got != want {
		t.Fatalf("plan differs from the bash job\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// google hits its 90% threshold: every T2 agent fails over. openai and
// deepseek are exhausted too, so anthropic (cost rank 1) takes them —
// spreading is by +5 per assignment, not enough to beat meta's rank 4.
// sec-check and strategist stay: anthropic is their own, healthy, provider.
func TestGoogleExhaustedFailsOverToAnthropic(t *testing.T) {
	in := schoolInput()
	in.Providers["google"] = Reading{95, "resets=2026-09-24T19:17:07Z"}
	p := Compute(in)
	moves := 0
	for _, d := range p.Decisions {
		if d.Action != ActionMove {
			continue
		}
		moves++
		if d.To.Provider != "anthropic" || d.Reason != "google exhausted" {
			t.Errorf("%s → %+v (%s)", d.Agent, d.To, d.Reason)
		}
	}
	if moves != 8 { // 7 unpinned T2 agents on google + architect (T1)
		t.Fatalf("moves = %d\n%s", moves, strings.Join(p.Lines(true), "\n"))
	}
	for _, d := range p.Decisions {
		if d.Agent == "architect" && d.To.Model != "claude-fable-5-1" {
			t.Fatalf("architect should land on the first-sorting T1 anthropic rung, got %s", d.To.Model)
		}
		if d.Agent == "scanner" && d.Line != "scanner        google    gemini-3.6-flash-low  ->  anthropic claude-sonnet-5" {
			t.Fatalf("line %q", d.Line)
		}
	}
	if !strings.HasPrefix(p.Lines(true)[len(p.Lines(true))-1], "8 change(s)") {
		t.Fatalf("footer: %v", p.Lines(true))
	}
}

// Nothing in T1 has headroom: the agent is stranded, not left running into a
// backend that cannot serve.
func TestStrandWhenNoRungHasHeadroom(t *testing.T) {
	in := schoolInput()
	in.Providers["google"] = Reading{100, ""}
	in.Providers["anthropic"] = Reading{97, ""}
	p := Compute(in)
	var strands []string
	for _, d := range p.Decisions {
		if d.Action == ActionStrand {
			strands = append(strands, d.Line)
		}
	}
	// T1 has no meta rung; T2 agents fail over to meta (no-usage-api is an
	// allowed unknown).
	if len(strands) != 3 || strands[0] != "architect      google    STRANDED (no rung at T1) -> pausing" {
		t.Fatalf("strands: %q", strands)
	}
	for _, d := range p.Decisions {
		if d.Agent == "scanner" && (d.Action != ActionMove || d.To.Provider != "meta") {
			t.Fatalf("scanner: %+v", d)
		}
	}
}

// Unknown readings never evict, and a FAILED measurement is not a
// destination.
func TestUnknownNeverEvictsAndFailedProbeIsNotADestination(t *testing.T) {
	in := schoolInput()
	in.Providers["google"] = Reading{-1, "unparsed"}
	for _, d := range Compute(in).Decisions {
		if d.Action == ActionMove || d.Action == ActionStrand {
			t.Fatalf("unknown google reading moved %s: %s", d.Agent, d.Line)
		}
	}
	in = schoolInput()
	in.Providers["anthropic"] = Reading{97, ""}
	in.Providers["google"] = Reading{-1, "unparsed"}
	for _, d := range Compute(in).Decisions {
		if d.Action == ActionMove && d.To.Provider == "google" {
			t.Fatalf("placed onto an unmeasurable provider: %s", d.Line)
		}
	}
}

// QUIRK reproduced: within a provider the alphabetically-first model wins.
func TestTieBreakIsAlphabetical(t *testing.T) {
	in := schoolInput()
	in.Agents = []Agent{{Name: "scanner", CLI: "codex", Model: "gpt-5.6-luna", Cadence: "1h"}}
	in.Providers["openai"] = Reading{100, ""}
	in.Providers["anthropic"] = Reading{95, ""}
	in.Rungs = bashTiers()
	d := Compute(in).Decisions[0]
	if d.To.Model != "gemini-3.6-flash-low" || d.ToEffort != "low" {
		t.Fatalf("got %+v", d)
	}
}

// Canary: codex usage is readable only through a live pane, so one
// low-cadence agent is parked on a recovered openai pool.
func TestCanaryOnRecoveredOpenAI(t *testing.T) {
	in := schoolInput()
	in.Providers["openai"] = Reading{12, "weekly=12%"}
	for i := range in.Agents {
		if in.Agents[i].Name == "guide" {
			in.Agents[i].Cadence = "6h"
		}
		if in.Agents[i].Name == "scanner" {
			in.Agents[i].Cadence = "5m" // high-volume: never a codex canary
		}
	}
	p := Compute(in)
	var c *Decision
	for i := range p.Decisions {
		if p.Decisions[i].Action == ActionCanary {
			c = &p.Decisions[i]
		}
	}
	if c == nil || c.Agent != "guide" || c.Line != "guide          openai    canary -> codex     gpt-5-6-luna (probe visibility)" {
		t.Fatalf("canary: %+v", c)
	}
	in.CanaryCooldown = map[string]time.Time{"openai": tick.Add(time.Hour)}
	for _, d := range Compute(in).Decisions {
		if d.Action == ActionCanary {
			t.Fatal("canary placed during cooldown")
		}
	}
}

// Auto-resume: an undeclared dashboard pause is resumed; a declared hold and
// on-demand agents are left alone; login-detector pauses block the provider.
func TestAutoResumeAndLoginBlock(t *testing.T) {
	in := schoolInput()
	in.Holds = nil
	in.Agents = append(in.Agents, Agent{Name: "quality2", CLI: "claude", Model: "claude-sonnet-5", Paused: true, PausedTrigger: "login-detector"})
	p := Compute(in)
	var resumed []string
	for _, d := range p.Decisions {
		if d.Action == ActionResume {
			resumed = append(resumed, d.Line)
		}
	}
	if len(resumed) != 1 || resumed[0] != "reviewer       operator  operator pause -> resuming (not declared in HIVE_ROTATE_HOLD)" {
		t.Fatalf("resumes: %q", resumed)
	}
	// anthropic is login-blocked now: the sticky check must not keep
	// sec-check there.
	for _, d := range p.Decisions {
		if d.Agent == "sec-check" && d.Action != ActionMove {
			t.Fatalf("sec-check stayed on a login-blocked provider: %s", d.Line)
		}
	}
}

func TestStrandedJournalRecovery(t *testing.T) {
	in := schoolInput()
	in.Stranded = map[string]Placement{"operations": {"deepseek", "pi", "deepseek-flash"}}
	in.Providers["deepseek"] = Reading{0, "balance=$12.3"}
	p := Compute(in)
	if p.Decisions[0].Line != "operations     deepseek  recovered -> resuming" {
		t.Fatalf("first line %q", p.Decisions[0].Line)
	}
	if p.Changes != 0 {
		t.Fatal("resumes are not counted as changes (bash parity)")
	}
}

func TestPeakWindowIsSoft(t *testing.T) {
	if !InPeakWindow(time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC), "01:00-04:00,06:00-10:00") {
		t.Fatal("07:00 Thursday is peak")
	}
	if InPeakWindow(time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC), "01:00-04:00,06:00-10:00") {
		t.Fatal("Saturday is never peak")
	}
	if !InPeakWindow(time.Date(2026, 9, 24, 23, 30, 0, 0, time.UTC), "23:00-01:00") {
		t.Fatal("wrapping window")
	}
}

func TestCadenceSeconds(t *testing.T) {
	for in, want := range map[string]int{"5m": 300, "2h": 7200, "45s": 45, "": 999999, "daily": 999999} {
		if got := (Agent{Cadence: in}).CadenceSeconds(); got != want {
			t.Errorf("%q → %d, want %d", in, got, want)
		}
	}
}
