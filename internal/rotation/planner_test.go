package rotation

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/hive-operator/internal/usage"
)

// bashTiers is the LIVE hive-rotate.sh TIERS table (ConfigMap
// hive/hive-ops-scripts, 2026-10-01), in order, with the defaults
// T1_ANTHROPIC_MODEL=claude-opus-5, T2_ANTHROPIC_MODEL=claude-sonnet-5 and
// META_MODEL=muse-spark-1.3-contributor. DeepSeek is gone; Kiro (pi +
// kiro-api-key/…:<thinking>) took its place.
func bashTiers() []Rung {
	var out []Rung
	add := func(tier, p, b, m string) { out = append(out, Rung{tier, p, b, m}) }
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

// fleetTiers is AGENT_TIERS.
var fleetTiers = map[string]string{
	"supervisor": "T2", "scanner": "T2", "ci-maintainer": "T2", "quality": "T2", "guide": "T2",
	"outreach": "T2", "sec-check": "T1", "architect": "T1", "strategist": "T1",
	"operations": "T2", "telemetry": "T2",
}

// readings20261001 is hive/hive-provider-usage as the primary rotate
// published it from ccleft at 2026-10-01T14:25:00Z (published_to_probe).
func readings20261001() map[string]Reading {
	cm := map[string]string{
		"anthropic": "100% used no-credential (ccleft auth_required cause=no_credentials: needs an interactive login)",
		"google":    "95% used resets=2026-10-07T03:17:08Z",
		"kiro":      "4% used credits=458.85/10000 resets=2026-11-01T00:00:00Z",
		"meta":      "unknown no-usage-api (ccleft unsupported cause=api_key_login)",
		"openai":    "100% used weekly=100% resets=2026-10-03T22:41:44Z",
	}
	out := map[string]Reading{}
	for p, v := range cm {
		l := usage.PublishedToProbe(v)
		out[p] = Reading{Percent: float64(l.Percent), Note: l.Note}
	}
	return out
}

// loadStatus reads a slimmed /api/status snapshot (testdata) into Agents,
// govModel first, exactly as hive-rotate.sh reads it.
func loadStatus(t *testing.T, path string) []Agent {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Agents []struct {
			Name, CLI, GovModel, Model, ReasoningEffort, Cadence, PausedTrigger string
			Paused                                                              bool
			OnDemand                                                            *bool
		}
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	var out []Agent
	for _, a := range st.Agents {
		m := a.GovModel
		if m == "" {
			m = a.Model
		}
		out = append(out, Agent{Name: a.Name, CLI: a.CLI, Model: m, Effort: a.ReasoningEffort, Cadence: a.Cadence,
			Paused: a.Paused, PausedTrigger: a.PausedTrigger, OnDemand: a.OnDemand != nil && *a.OnDemand})
	}
	return out
}

// bashPlan reads a hive-rotate job log: the agent lines (no usage banner, no
// contributors section) plus the footer.
func bashPlan(t *testing.T, path string) (agents, contributors []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inContrib := false
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "usage: "), l == "":
			continue
		case l == "contributors:":
			inContrib = true
			continue
		case strings.HasPrefix(l, "fleet already") || strings.Contains(l, " change(s) — run "):
			agents = append(agents, l)
			inContrib = false
			continue
		}
		if inContrib {
			contributors = append(contributors, l)
		} else {
			agents = append(agents, l)
		}
	}
	return agents, contributors
}

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func liveInput(t *testing.T, spoke, ns string, primary bool, pins, holds map[string]bool, now time.Time) Input {
	return Input{Now: now, Namespace: ns, Primary: primary, Agents: loadStatus(t, "testdata/status-"+spoke+"-20261001.json"),
		Providers: readings20261001(), UsageSource: "ccleft", Tiers: fleetTiers, Rungs: bashTiers(),
		Pins: pins, Holds: holds, Policy: DefaultPolicy()}
}

func diffLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}
	t.Errorf("plan differs from the bash job log\n--- bash\n%s\n--- operator\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
}

// GOLDEN: the live hive-rotate job logs of 2026-10-01, reproduced byte for
// byte from the /api/status snapshot of the same hour, the published
// readings, the live TIERS table and each CronJob's env (school:
// HIVE_ROTATE_PIN=supervisor, HIVE_ROTATE_HOLD=reviewer; hanthor:
// HIVE_ROTATE_HOLD=reviewer; reef: neither). The pace-demoted journal is
// inferred — every "(pace-demoted from …)" suffix below is bash reading its
// journal file, which the operator never sees in Shadow.
func TestGoldenSchool20261001(t *testing.T) {
	in := liveInput(t, "school", "hive", true, set("supervisor"), set("reviewer"),
		time.Date(2026, 10, 1, 14, 20, 3, 0, time.UTC))
	want, wantContrib := bashPlan(t, "testdata/hive-rotate-school-20261001T1420.log")
	diffLines(t, Compute(in).Lines(false), want)

	deploys := []ContribDeploy{
		{Name: "agy-contributor", Backend: "agy", Model: "gemini-3.7-flash-high"},
		{Name: "claude-contributor", Backend: "claude"},
		{Name: "pi-codex-contributor", Backend: "pi", Model: "openai-codex/gpt-5.6-sol"},
	}
	var got []string
	for _, d := range Contributors(in, deploys, "hive-contributors") {
		got = append(got, d.Line)
	}
	diffLines(t, got, wantContrib)
}

func TestGoldenHanthor20261001(t *testing.T) {
	in := liveInput(t, "hanthor", "hive-hanthor", false, nil, set("reviewer"),
		time.Date(2026, 10, 1, 14, 14, 3, 0, time.UTC))
	want, wantContrib := bashPlan(t, "testdata/hive-rotate-hanthor-20261001T1414.log")
	diffLines(t, Compute(in).Lines(false), want)
	var got []string
	for _, d := range Contributors(in, nil, "hive-contributors") {
		got = append(got, d.Line)
	}
	diffLines(t, got, wantContrib)
}

func TestGoldenReef20261001(t *testing.T) {
	in := liveInput(t, "reef", "hive-reef", false, nil, nil, time.Date(2026, 10, 1, 14, 27, 3, 0, time.UTC))
	want, _ := bashPlan(t, "testdata/hive-rotate-reef-20261001T1427.log")
	diffLines(t, Compute(in).Lines(false), want)
}

// ── Rule tables ─────────────────────────────────────────────────────────

func mkIn(agents []Agent, readings map[string]Reading) Input {
	return Input{Now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Namespace: "hive", Primary: true,
		Agents: agents, Providers: readings, UsageSource: "ccleft", Tiers: fleetTiers, Rungs: bashTiers(), Policy: DefaultPolicy()}
}

func healthy() map[string]Reading {
	return map[string]Reading{
		"google": {10, "resets=x"}, "kiro": {10, "credits=1/10"}, "anthropic": {10, "resets=x"},
		"openai": {10, "resets=x"}, "meta": {-1, "no-usage-api"},
	}
}

func decisionFor(p Plan, agent string) Decision {
	for _, d := range p.Decisions {
		if d.Agent == agent && d.Action != ActionResume && d.Action != ActionHold {
			return d
		}
	}
	return Decision{}
}

func canaryFor(p Plan, agent string) Decision {
	for _, d := range p.Decisions {
		if d.Agent == agent && d.Action == ActionCanary {
			return d
		}
	}
	return Decision{}
}

func TestThresholdsPerProvider(t *testing.T) {
	pol := DefaultPolicy()
	for p, want := range map[string]float64{"openai": 85, "anthropic": 90, "google": 90, "kiro": 95, "meta": 85, "github": 85} {
		if got := pol.Threshold(p); got != want {
			t.Errorf("%s threshold %v, want %v", p, got, want)
		}
	}
	// 94% kiro is still usable (overage disabled: 95 leaves a margin), 95 is not.
	for _, tc := range []struct {
		pct  float64
		move bool
	}{{94, false}, {95, true}} {
		r := healthy()
		r["kiro"] = Reading{tc.pct, "credits"}
		p := Compute(mkIn([]Agent{{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "4h"}}, r))
		if got := decisionFor(p, "guide").Action == ActionMove; got != tc.move {
			t.Errorf("kiro at %v%%: moved=%v, want %v (%s)", tc.pct, got, tc.move, decisionFor(p, "guide").Line)
		}
	}
	// openai trips at 85, anthropic at 90.
	r := healthy()
	r["openai"], r["anthropic"] = Reading{85, "x"}, Reading{89, "x"}
	p := Compute(mkIn([]Agent{
		{Name: "guide", CLI: "codex", Model: "gpt-5.6-luna", Cadence: "4h"},
		{Name: "quality", CLI: "claude", Model: "claude-sonnet-5", Cadence: "4h"},
	}, r))
	if decisionFor(p, "guide").Action != ActionMove || decisionFor(p, "quality").Action != ActionKeep {
		t.Fatalf("thresholds: %v", p.Lines(true))
	}
}

// -1 is UNMEASURED: never exhausted (nobody is moved off it), never
// recovered (nobody is resumed onto it), and enterable only when the note
// says nothing CAN measure it (no-agent / no-usage-api).
func TestUnmeasuredIsNotExhausted(t *testing.T) {
	r := healthy()
	r["kiro"] = Reading{-1, "ccleft-stale age=45m"}
	p := Compute(mkIn([]Agent{{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.Action != ActionKeep || d.Line != "guide          kiro      kiro-api-key/claude-sonnet-5:medium ok" {
		t.Fatalf("an unmeasured provider must keep its agents: %q", d.Line)
	}
	// ...but a failed measurement is not a destination.
	r["google"] = Reading{95, "resets=x"}
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider == "kiro" {
		t.Fatalf("moved onto an unmeasured (failed) pool: %q", d.Line)
	}
	// meta's "no-usage-api" -1 IS enterable.
	r = healthy()
	for _, p := range []string{"google", "kiro", "anthropic", "openai"} {
		r[p] = Reading{100, "x"}
	}
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "meta" {
		t.Fatalf("no-usage-api must permit arrival: %q", d.Line)
	}
	// A stranded agent is not resumed on an unknown reading.
	in := mkIn(nil, map[string]Reading{"kiro": {-1, "ccleft-stale"}})
	in.Stranded = map[string]Placement{"guide": {"kiro", "pi", "kiro-api-key/claude-sonnet-5:medium"}}
	if len(Compute(in).Decisions) != 0 {
		t.Fatal("unknown is not recovery")
	}
	in.Providers["kiro"] = Reading{20, "credits"}
	if d := Compute(in).Decisions[0]; d.Action != ActionResume || d.Line != "guide          kiro      recovered -> resuming" || !d.ClearsStrand {
		t.Fatalf("recovered strand: %+v", d)
	}
}

// Cost order agy > kiro > claude > codex, and failover not optimiser.
func TestCostOrderAndStickiness(t *testing.T) {
	r := healthy()
	// Off-tier agent: lands on the cheapest provider with room.
	p := Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "claude-fable-5", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.To != (Placement{"google", "agy", "gemini-3.6-flash-low"}) {
		// QUIRK: alphabetically-first model wins within google.
		t.Fatalf("cheapest: %q", d.Line)
	}
	r["google"] = Reading{90, "x"}
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "claude-fable-5", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "kiro" {
		t.Fatalf("kiro is rank 1: %q", d.Line)
	}
	r["kiro"] = Reading{95, "x"}
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "claude-fable-5", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "anthropic" {
		t.Fatalf("anthropic is rank 2: %q", d.Line)
	}
	r["anthropic"] = Reading{90, "x"}
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "claude-fable-5", Cadence: "4h"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "openai" {
		t.Fatalf("openai is rank 3: %q", d.Line)
	}
	// Sticky: an in-tier agent on a usable dearer pool stays even though
	// google has room.
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "codex", Model: "gpt-5.6-luna", Cadence: "4h"}}, healthy()))
	if d := decisionFor(p, "guide"); d.Action != ActionKeep {
		t.Fatalf("failover, not optimiser: %q", d.Line)
	}
	// Spreading: +5 per agent placed this run.
	agents := []Agent{}
	for _, n := range []string{"scanner", "quality", "guide"} {
		agents = append(agents, Agent{Name: n, CLI: "copilot", Model: "x", Cadence: "4h"})
	}
	r = healthy()
	r["google"] = Reading{100, "x"}
	r["kiro"] = Reading{1, "x"}
	r["anthropic"] = Reading{5, "x"}
	p = Compute(mkIn(agents, r))
	// all kiro (rank 1 beats anthropic rank 2 regardless of load)
	for _, n := range []string{"scanner", "quality", "guide"} {
		if decisionFor(p, n).To.Provider != "kiro" {
			t.Fatalf("cost rank dominates load: %v", p.Lines(true))
		}
	}
	// within kiro the +5 spreading is per PROVIDER, so the model tie-break
	// (alphabetical) still picks claude-sonnet for each.
	if decisionFor(p, "guide").To.Model != "kiro-api-key/claude-sonnet-5:medium" {
		t.Fatalf("%v", p.Lines(true))
	}
}

// High-cadence (≤30m) agents are barred from codex unless their current
// provider is POSITIVELY exhausted.
func TestHighCadenceCodexBar(t *testing.T) {
	r := healthy()
	for _, p := range []string{"google", "kiro", "anthropic"} {
		r[p] = Reading{100, "x"}
	}
	r["meta"] = Reading{100, "x"}
	// Current provider (github/copilot) is unmeasured, not exhausted: no
	// escape hatch, so a 30m agent cannot go to codex → no rung → stays.
	p := Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "claude-fable-5", Cadence: "30m"}}, r))
	if d := decisionFor(p, "guide"); d.Line != "guide          github    claude-fable-5 ok (no better rung available)" {
		t.Fatalf("barred: %q", d.Line)
	}
	// 31m is not high volume.
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "claude-fable-5", Cadence: "31m"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "openai" {
		t.Fatalf("31m may use codex: %q", d.Line)
	}
	// Exhausted current provider opens codex to a 5m agent.
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "5m"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "openai" || d.CooldownProvider != "google" {
		t.Fatalf("escape hatch: %+v", d)
	}
	// Metered failover off: the hatch closes → STRANDED.
	in := mkIn([]Agent{{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "5m"}}, r)
	in.Policy.MeteredFailover = false
	if d := decisionFor(Compute(in), "guide"); d.Action != ActionStrand || d.Line != "guide          google    STRANDED (no rung at T2) -> pausing" || !d.RecordsStrand {
		t.Fatalf("strand: %+v", d)
	}
}

// Kiro cadence guard: no agent kicked more often than every 900 s goes on
// Kiro (and one already there is not sticky), unless its current provider
// is positively exhausted.
func TestKiroCadenceGuard(t *testing.T) {
	r := healthy()
	r["google"] = Reading{100, "x"}
	// 10m agent on copilot (unmeasured): kiro barred → anthropic.
	p := Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "x", Cadence: "10m"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "anthropic" {
		t.Fatalf("kiro barred for 10m: %q", d.Line)
	}
	// 15m (=900 s) is allowed: the guard is strictly "<".
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "copilot", Model: "x", Cadence: "15m"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "kiro" {
		t.Fatalf("15m may use kiro: %q", d.Line)
	}
	// A 10m agent ALREADY on kiro is not sticky: moved off.
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "10m"}}, r))
	if d := decisionFor(p, "guide"); d.Action != ActionMove || d.To.Provider != "anthropic" {
		t.Fatalf("high-cadence kiro agent must move: %q", d.Line)
	}
	// ...unless nothing else will take it: then it stays ("no better rung").
	r2 := healthy()
	for _, x := range []string{"google", "anthropic", "openai", "meta"} {
		r2[x] = Reading{100, "x"}
	}
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "10m"}}, r2))
	if d := decisionFor(p, "guide"); d.Line != "guide          kiro      kiro-api-key/claude-sonnet-5:medium ok (no better rung available)" {
		t.Fatalf("%q", d.Line)
	}
	// Positively exhausted current provider waives the guard.
	p = Compute(mkIn([]Agent{{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "5m"}}, r))
	if d := decisionFor(p, "guide"); d.To.Provider != "kiro" {
		t.Fatalf("exhausted current waives the kiro guard: %q", d.Line)
	}
}

// ≤5 high-volume agents on agy per tick.
func TestAgyHighVolumeCap(t *testing.T) {
	r := healthy()
	var agents []Agent
	for _, n := range []string{"supervisor", "scanner", "ci-maintainer", "quality", "guide", "outreach", "operations"} {
		agents = append(agents, Agent{Name: n, CLI: "copilot", Model: "x", Cadence: "5m"})
	}
	p := Compute(mkIn(agents, r))
	onGoogle := 0
	for _, d := range p.Decisions {
		if d.To.Provider == "google" {
			onGoogle++
		}
	}
	if onGoogle != 5 {
		t.Fatalf("agy cap: %d on google\n%s", onGoogle, strings.Join(p.Lines(true), "\n"))
	}
	// Sticky high-volume agents already on agy count toward the cap.
	agents = []Agent{}
	for _, n := range []string{"supervisor", "scanner", "ci-maintainer", "quality", "guide"} {
		agents = append(agents, Agent{Name: n, CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "5m"})
	}
	agents = append(agents, Agent{Name: "outreach", CLI: "copilot", Model: "x", Cadence: "5m"})
	p = Compute(mkIn(agents, r))
	if d := decisionFor(p, "outreach"); d.To.Provider == "google" {
		t.Fatalf("sixth high-volume agent on agy: %q", d.Line)
	}
}

// Pins, holds, undeclared pauses, the recovery net and on-demand agents.
func TestPinsHoldsAndAutoResume(t *testing.T) {
	r := healthy()
	agents := []Agent{
		{Name: "supervisor", CLI: "agy", Model: "gemini-3.8-flash-medium", Cadence: "paused"},
		{Name: "reviewer", CLI: "agy", Model: "gemini-3.8-flash-medium", Paused: true, PausedTrigger: "dashboard-api"},
		{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Paused: true, PausedTrigger: "dashboard-api"},
		{Name: "quality", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Paused: true, PausedTrigger: "governor"},
		{Name: "scanner", CLI: "copilot", Model: "x", Paused: true, PausedTrigger: "login-detector"},
		{Name: "brainstorm", CLI: "agy", Model: "gemini-3.8-flash-low", Paused: true, PausedTrigger: "startup", OnDemand: true},
	}
	in := mkIn(agents, r)
	in.Pins, in.Holds = set("supervisor"), set("reviewer")
	got := Compute(in).Lines(true)
	want := []string{
		"reviewer       operator  held paused by HIVE_ROTATE_HOLD (declared in git)",
		"guide          operator  operator pause -> resuming (not declared in HIVE_ROTATE_HOLD)",
		"quality        kiro      paused on a healthy provider -> resuming",
		"supervisor     google    pinned by HIVE_ROTATE_PIN — placement left alone",
		"guide          kiro      kiro-api-key/claude-sonnet-5:medium ok",
		"quality        kiro      kiro-api-key/claude-sonnet-5:medium ok",
		"scanner        github    x  ->  google    gemini-3.6-flash-low",
		"1 change(s) — run 'hive-rotate.sh apply' to perform them",
	}
	diffLines(t, got, want)
	d := decisionFor(Compute(in), "scanner")
	if !d.ResumeAfter || d.Reason != "github login-blocked" {
		t.Fatalf("a login-detector agent moved off its provider is resumed after: %+v", d)
	}
	// An undeclared pause on an agent WE stranded is ours, not the operator's.
	in.Stranded = map[string]Placement{"guide": {"kiro", "pi", "kiro-api-key/claude-sonnet-5:medium"}}
	in.Providers["kiro"] = Reading{96, "x"}
	lines := strings.Join(Compute(in).Lines(true), "\n")
	if strings.Contains(lines, "guide          operator") {
		t.Fatalf("stranded journal row makes the pause ours:\n%s", lines)
	}
	// Auto-resume off.
	in = mkIn(agents, r)
	in.Policy.AutoResume = false
	if strings.Contains(strings.Join(Compute(in).Lines(true), "\n"), "resuming") {
		t.Fatal("HIVE_ROTATE_AUTORESUME=0 must hold every pause")
	}
}

// Stranding: no rung fits and the current provider is exhausted → pause,
// journal; an already-paused agent is reported but not re-paused/journaled.
func TestStranding(t *testing.T) {
	r := healthy()
	for _, p := range []string{"google", "kiro", "anthropic", "openai", "meta"} {
		r[p] = Reading{100, "x"}
	}
	in := mkIn([]Agent{
		{Name: "architect", CLI: "pi", Model: "kiro-api-key/claude-opus-5:high", Cadence: "4h"},
		{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "4h", Paused: true, PausedTrigger: "governor"},
	}, r)
	p := Compute(in)
	a, g := decisionFor(p, "architect"), decisionFor(p, "guide")
	if a.Action != ActionStrand || !a.RecordsStrand || a.SkipActuation || a.Line != "architect      kiro      STRANDED (no rung at T1) -> pausing" {
		t.Fatalf("strand: %+v", a)
	}
	if g.Action != ActionStrand || !g.SkipActuation || g.RecordsStrand || g.Mutates() {
		t.Fatalf("already paused: %+v", g)
	}
	if p.Changes != 2 {
		t.Fatalf("both count as changes in bash: %d", p.Changes)
	}
	// A stranded agent moved onto a recovered pool is cleared and resumed.
	r["google"] = Reading{10, "x"}
	in = mkIn([]Agent{{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "4h", Paused: true, PausedTrigger: "dashboard-api"}}, r)
	in.Stranded = map[string]Placement{"guide": {"kiro", "pi", "kiro-api-key/claude-sonnet-5:medium"}}
	d := decisionFor(Compute(in), "guide")
	if d.Action != ActionMove || !d.ResumeAfter || !d.ClearsStrand || d.CooldownProvider != "kiro" {
		t.Fatalf("recovery by move: %+v", d)
	}
}

// Canaries: openai only, never while ccleft measures codex, never on an
// exhausted pool, never during the cooldown, longest-cadence agent, never a
// high-volume one.
func TestCanaries(t *testing.T) {
	r := healthy()
	r["openai"] = Reading{-1, "no-agent"}
	agents := []Agent{
		{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "4h"},
		{Name: "operations", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "24h"},
		{Name: "scanner", CLI: "agy", Model: "gemini-3.8-flash-low", Cadence: "5m"},
	}
	in := mkIn(agents, r)
	in.UsageSource = "direct"
	p := Compute(in)
	d := canaryFor(p, "operations")
	if d.Action != ActionCanary || d.Line != "operations     openai    canary -> codex     gpt-5.6-luna (probe visibility)" {
		t.Fatalf("canary: %+v\n%v", d, p.Lines(true))
	}
	in.UsageSource = "ccleft"
	if canaryFor(Compute(in), "operations").Action != ActionCanary {
		t.Fatal("unmeasured codex via ccleft still canaries (the pane fallback is then the only signal)")
	}
	in.Providers["openai"] = Reading{40, "weekly=40%"}
	if canaryFor(Compute(in), "operations").Action == ActionCanary {
		t.Fatal("ccleft measures codex headlessly: no canary")
	}
	in.UsageSource = "direct"
	in.CanaryCooldown = map[string]time.Time{"openai": in.Now.Add(time.Hour)}
	if canaryFor(Compute(in), "operations").Action == ActionCanary {
		t.Fatal("cooldown")
	}
	in.CanaryCooldown = map[string]time.Time{"openai": in.Now.Add(-time.Minute)}
	if canaryFor(Compute(in), "operations").Action != ActionCanary {
		t.Fatal("expired cooldown")
	}
	in.Providers["openai"] = Reading{85, "x"}
	if canaryFor(Compute(in), "operations").Action == ActionCanary {
		t.Fatal("exhausted pool")
	}
}

// Kiro budget cap (hive-pace's kiro-evict request): only onto the named
// pools, only onto the agent's own tier; expired requests are ignored.
func TestKiroEvictRequest(t *testing.T) {
	r := healthy()
	agents := []Agent{{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-haiku-4-5:low", Cadence: "1h"}}
	in := mkIn(agents, r)
	in.PaceDemoted = map[string]Placement{"guide": {"kiro", "pi", "kiro-api-key/claude-sonnet-5:medium"}}
	in.KiroEvict = map[string]KiroEvict{"guide": {Expiry: in.Now.Add(time.Hour), Targets: []string{"anthropic"}}}
	p := Compute(in)
	d := decisionFor(p, "guide")
	if d.Line != "guide          kiro      kiro-api-key/claude-haiku-4-5:low  ->  anthropic claude-sonnet-5  (kiro budget cap, hive-pace)" || p.Changes != 1 {
		t.Fatalf("%q", d.Line)
	}
	in.Providers["anthropic"] = Reading{100, "x"}
	lines := Compute(in).Lines(true)
	if lines[0] != "guide          kiro      kiro budget cap requested, but no rung on [anthropic] in T2 — stays" ||
		lines[1] != "guide          kiro      kiro-api-key/claude-haiku-4-5:low ok (pace-demoted from kiro-api-key/claude-sonnet-5:medium)" {
		t.Fatalf("%q", lines)
	}
	in.KiroEvict["guide"] = KiroEvict{Expiry: in.Now.Add(-time.Second), Targets: []string{"anthropic"}}
	if l := Compute(in).Lines(true)[0]; strings.Contains(l, "cap requested") {
		t.Fatalf("expired request: %q", l)
	}
}

// pace_demoted_from follows the chain (any notch below the original) and a
// journal row wins over inference.
func TestPaceDemotedChain(t *testing.T) {
	members := bashTiers()[:5]
	a := Agent{Name: "architect", CLI: "pi", Model: "kiro-api-key/claude-haiku-4-5:low"}
	if p, ok := PaceDemotedFrom(nil, true, members, a); !ok || p.Model != "kiro-api-key/claude-opus-5:high" {
		t.Fatalf("two notches: %+v", p)
	}
	j := map[string]Placement{"architect": {"kiro", "pi", "kiro-api-key/gpt-5-6-sol:high"}}
	if p, ok := PaceDemotedFrom(j, true, members, a); !ok || p.Model != "kiro-api-key/gpt-5-6-sol:high" {
		t.Fatalf("journal row: %+v", p)
	}
	a.Model = "kiro-api-key/claude-opus-5:high"
	if _, ok := PaceDemotedFrom(j, true, members, a); ok {
		t.Fatal("sitting ON the original is not demoted")
	}
	if _, ok := PaceDemotedFrom(nil, false, members, Agent{CLI: "pi", Model: "kiro-api-key/claude-haiku-4-5:low"}); ok {
		t.Fatal("no inference when disabled")
	}
}

func TestRungChains(t *testing.T) {
	for in, want := range map[string]string{
		"kiro-api-key/claude-opus-5:high":     "kiro-api-key/claude-sonnet-5:high",
		"kiro-api-key/gpt-5-6-sol:high":       "kiro-api-key/gpt-5-6-luna:high",
		"kiro-api-key/gpt-5-6-terra":          "kiro-api-key/gpt-5-6-luna",
		"kiro-api-key/claude-sonnet-5:medium": "kiro-api-key/claude-haiku-4-5:low",
		"kiro-api-key/gpt-5-6-luna:medium":    "kiro-api-key/claude-haiku-4-5:low",
		"kiro-api-key/claude-haiku-4-5:low":   "",
		"claude-fable-5-1":                    "claude-sonnet-5",
		"gemini-3.8-flash-high":               "gemini-3.8-flash-low",
		"gemini-3.8-flash-medium":             "",
		"gpt-6-astra":                         "gpt-5.6-luna",
	} {
		if got := RungDown(in); got != want {
			t.Errorf("RungDown(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RungUpToward("kiro-api-key/claude-opus-5:high", "kiro-api-key/claude-haiku-4-5:low"); got != "kiro-api-key/claude-sonnet-5:high" {
		t.Fatalf("up toward: %q", got)
	}
	if got := RungUpToward("kiro-api-key/claude-opus-5:high", "kiro-api-key/claude-sonnet-5:high"); got != "kiro-api-key/claude-opus-5:high" {
		t.Fatalf("up toward: %q", got)
	}
}

func TestCadenceSeconds(t *testing.T) {
	for c, want := range map[string]int{"5m": 300, "1h": 3600, "30s": 30, "24h": 86400, "1.5h": 5400,
		"paused": 999999, "idle": 999999, "": 999999} {
		if got := (Agent{Cadence: c}).CadenceSeconds(); got != want {
			t.Errorf("%q → %d, want %d", c, got, want)
		}
	}
}

func TestContributorsScale(t *testing.T) {
	r := healthy()
	r["google"] = Reading{95, "x"}
	in := mkIn(nil, r)
	ds := []ContribDeploy{
		{Name: "agy-contributor", Backend: "agy", Model: "gemini-3.7-flash-high", Replicas: 1},
		{Name: "claude-contributor", Backend: "claude", Replicas: 0},
		{Name: "pi-codex-contributor", Backend: "pi", Model: "openai-codex/gpt-5.6-sol", Replicas: 1},
		{Name: "mystery", Backend: "", Replicas: 1},
	}
	var got []string
	for _, d := range Contributors(in, ds, "hive-contributors") {
		got = append(got, d.Line)
	}
	diffLines(t, got, []string{
		"agy-contributor          google    EXHAUSTED -> parking (replicas 1->0)",
		"claude-contributor       anthropic recovered -> restoring (replicas 0->1)",
		"pi-codex-contributor     openai    ok (replicas=1)",
		"mystery                  unknown    (unmeasured — left at 1)",
	})
}
