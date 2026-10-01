package rotation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/hive-operator/internal/usage"
)

// paceCM reads the hive/hive-pace ConfigMap the bash pacer published at the
// 14:25Z tick (pace.json + kiro-budget.json) into the planner's types.
func paceCM(t *testing.T, tick string) (map[string]PaceVerdict, KiroBudget) {
	t.Helper()
	b, err := os.ReadFile("testdata/hive-pace-cm-" + tick + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var cm map[string]string
	if err := json.Unmarshal(b, &cm); err != nil {
		t.Fatal(err)
	}
	var raw map[string]struct {
		Verdict     string
		Pressure    *float64
		BindingSlot string `json:"binding_slot"`
		Slots       map[string]struct {
			Pct          float64
			Reset        *int64
			Samples      int
			SpanS        int64    `json:"span_s"`
			HoursLeft    *float64 `json:"hours_left"`
			AllowedRate  *float64 `json:"allowed_rate"`
			ObservedRate *float64 `json:"observed_rate"`
			Ratio        *float64
		}
	}
	if err := json.Unmarshal([]byte(cm["pace.json"]), &raw); err != nil {
		t.Fatal(err)
	}
	v := map[string]PaceVerdict{}
	for p, r := range raw {
		pv := PaceVerdict{Verdict: r.Verdict, Pressure: r.Pressure, BindingSlot: r.BindingSlot, Slots: map[string]SlotFit{}}
		for s, f := range r.Slots {
			sf := SlotFit{Pct: f.Pct, Samples: f.Samples, SpanS: f.SpanS, HoursLeft: f.HoursLeft, AllowedRate: f.AllowedRate,
				ObservedRate: f.ObservedRate, Ratio: f.Ratio}
			if f.Reset != nil {
				sf.Reset = *f.Reset
			}
			pv.Slots[s] = sf
		}
		v[p] = pv
	}
	var kb struct {
		Verdict                        string
		Used, Limit, Remaining, Safety float64
		Reset                          int64
		ReadingAgeS                    int64 `json:"reading_age_s"`
		Allowed, Burn, Ratio           *float64
		HoursLeft                      *float64 `json:"hours_left"`
		Burn6h                         *float64 `json:"burn_6h"`
		Samples                        int
		SpanS                          int64 `json:"span_s"`
		WindowStart                    int64 `json:"window_start"`
	}
	if err := json.Unmarshal([]byte(cm["kiro-budget.json"]), &kb); err != nil {
		t.Fatal(err)
	}
	return v, KiroBudget{Verdict: kb.Verdict, Used: kb.Used, Limit: kb.Limit, Remaining: kb.Remaining, Reset: kb.Reset,
		ReadingAgeS: kb.ReadingAgeS, Safety: kb.Safety, HoursLeft: kb.HoursLeft, Allowed: kb.Allowed, Samples: kb.Samples,
		SpanS: kb.SpanS, WindowStart: kb.WindowStart, Burn6h: kb.Burn6h, Burn: kb.Burn, Ratio: kb.Ratio}
}

func membersOf(tier string) []Rung {
	var out []Rung
	for _, r := range bashTiers() {
		if r.Tier == tier {
			out = append(out, r)
		}
	}
	return out
}

func liveFleet(t *testing.T, snap string) []FleetAgent {
	var fleet []FleetAgent
	for _, s := range []struct{ spoke, ns string }{{"school", "hive"}, {"reef", "hive-reef"}, {"hanthor", "hive-hanthor"}} {
		for _, a := range loadStatus(t, "testdata/status-"+s.spoke+"-"+snap+".json") {
			fleet = append(fleet, FleetAgent{Namespace: s.ns, Agent: a, Members: membersOf(fleetTiers[a.Name])})
		}
	}
	return fleet
}

func paceLog(t *testing.T, tick string) (table, kiroLine string, actions []string) {
	t.Helper()
	f, err := os.Open("testdata/hive-pace-" + tick + ".log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var tab []string
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == "":
		case strings.HasPrefix(l, "PROVIDER") || (len(tab) > 0 && len(tab) < 5):
			tab = append(tab, l)
		case strings.HasPrefix(l, "kiro budget:"):
			kiroLine = l
		case strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "pace: "):
			actions = append(actions, l)
		}
	}
	return strings.Join(tab, "\n"), kiroLine, actions
}

// GOLDEN: hive-pace apply at 2026-10-01T14:25Z. Given the verdicts and Kiro
// budget it published, the fleet from the same hour (41 agents across three
// spokes) and HIVE_PACE_PIN=hive/supervisor, the operator plans exactly the
// bash actions: google hot with nothing demotable, Kiro on budget.
func TestGoldenPace20261001(t *testing.T) {
	verdicts, kb := paceCM(t, "20261001T1425")
	wantTable, wantKiro, wantActions := paceLog(t, "20261001T1425")
	if got := strings.Join(VerdictTable(verdicts), "\n"); got != wantTable {
		t.Errorf("verdict table\n--- bash\n%s\n--- operator\n%s", wantTable, got)
	}
	if got := kb.Line(); got != wantKiro {
		t.Errorf("kiro line\n bash     %s\n operator %s", wantKiro, got)
	}
	in := PaceInput{Now: time.Date(2026, 10, 1, 14, 25, 1, 0, time.UTC), Fleet: liveFleet(t, "20261001"), Verdicts: verdicts, Kiro: kb,
		Readings: readings20261001(), Pins: set("hive/supervisor"), Config: DefaultPaceConfig()}
	if len(in.Fleet) != 41 {
		t.Fatalf("controls %d agents, bash said 41", len(in.Fleet))
	}
	diffLines(t, PlanPace(in).Text(), wantActions)
}

func sample(p string, ts int64, pct float64, reset int64) usage.PaceSample {
	return usage.PaceSample{TS: ts, Provider: p, Slot: "slot0", Pct: pct, Reset: reset}
}

func TestFitVerdicts(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	reset := now.Unix() + 10*3600 // 10 h left
	// ser: n samples every everyMin minutes, rising perHour, ending at
	// `latest` percent now.
	ser := func(p string, latest float64, perHour float64, n int, everyMin int) []usage.PaceSample {
		var out []usage.PaceSample
		for i := 0; i < n; i++ {
			before := float64((n-1-i)*everyMin) / 60
			out = append(out, sample(p, now.Unix()-int64(before*3600), latest-perHour*before, reset))
		}
		return out
	}
	// latest 50%: allowed = 50/10 = 5 %/h.
	hot := ser("anthropic", 50, 10, 7, 20)  // 10 %/h → pressure 2 → hot
	cold := ser("google", 50, 2, 7, 20)     // 2 %/h → 0.4 → cold
	onpace := ser("openai", 50, 5.5, 7, 20) // 1.1 → on-pace (deadband 0.25)
	learning := ser("kiro", 50, 5, 2, 20)   // 2 samples
	short := ser("github", 50, 5, 5, 10)    // spans 40 min < 1 h
	var hist []usage.PaceSample
	for _, s := range [][]usage.PaceSample{hot, cold, onpace, learning, short} {
		hist = append(hist, s...)
	}
	v := Fit(hist, now, DefaultFitConfig())
	for p, want := range map[string]string{"anthropic": "hot", "google": "cold", "openai": "on-pace", "kiro": "learning", "github": "learning"} {
		if v[p].Verdict != want {
			t.Errorf("%s: %s, want %s (%+v)", p, v[p].Verdict, want, v[p])
		}
	}
	if pr := v["anthropic"].Pressure; pr == nil || *pr != 2 {
		t.Fatalf("pressure %v", pr)
	}
	// Rollover: a drop of >5 points starts a fresh window (no negative burn).
	roll := append(ser("anthropic", 90, 5, 4, 20), sample("anthropic", now.Unix()+60, 2, reset))
	if v := Fit(roll, now.Add(time.Minute), DefaultFitConfig())["anthropic"]; v.Verdict != "learning" || v.Slots["slot0"].Samples != 1 {
		t.Fatalf("rollover: %+v", v)
	}
	// pressure is the MAX over limits, not the largest percent.
	two := append(ser("anthropic", 30, 2, 7, 20), func() []usage.PaceSample {
		var o []usage.PaceSample
		for _, s := range ser("anthropic", 30, 30, 7, 20) {
			s.Slot, s.Reset = "slot1", now.Unix()+3600*3
			o = append(o, s)
		}
		return o
	}()...)
	if v := Fit(two, now, DefaultFitConfig())["anthropic"]; v.Verdict != "hot" || v.BindingSlot != "slot1" {
		t.Fatalf("binding limit: %+v", v)
	}
}

func TestComputeKiroBudget(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	reset := now.Unix() + 100*3600
	mk := func(perHour float64) []usage.PaceSample {
		var out []usage.PaceSample
		for i := 0; i < 7; i++ {
			ts := now.Unix() - int64((6-i)*10*60)
			used := 1000 + perHour*float64(i*10)/60
			out = append(out, usage.PaceSample{TS: ts, Provider: "kiro", Slot: "slot0", Used: used, Limit: 10000, HasUsed: true, Reset: reset})
		}
		return out
	}
	cfg := DefaultKiroBudgetConfig()
	// remaining ≈ 9000 over 100 h × 0.85 ≈ 76.5 cr/h allowed.
	for burn, want := range map[float64]string{150: "over", 20: "under", 60: "on-budget"} {
		kb := ComputeKiroBudget(mk(burn), now, 0, cfg)
		if kb.Verdict != want {
			t.Errorf("burn %v: %s, want %s (%s)", burn, kb.Verdict, want, kb.Line())
		}
	}
	// Never fitted across the last actuation: settling.
	if kb := ComputeKiroBudget(mk(150), now, now.Unix()-5*60, cfg); kb.Verdict != "settling" {
		t.Fatalf("settling: %s", kb.Line())
	}
	// A stale reading (> 30 min) is not acted on.
	if kb := ComputeKiroBudget(mk(150), now.Add(time.Hour), 0, cfg); kb.Verdict != "stale" {
		t.Fatalf("stale: %s", kb.Line())
	}
	if kb := ComputeKiroBudget(nil, now, 0, cfg); kb.Verdict != "no-data" {
		t.Fatal(kb.Verdict)
	}
}

func fleetAgent(ns, name, cli, model, cadence string) FleetAgent {
	return FleetAgent{Namespace: ns, Agent: Agent{Name: name, CLI: cli, Model: model, Cadence: cadence}, Members: membersOf(fleetTiers[name])}
}

// Generic pacing: one notch per provider per tick, only agents with a
// cheaper rung, pins skipped, cold restores only own demotions.
func TestPaceGenericOneNotch(t *testing.T) {
	fleet := []FleetAgent{
		fleetAgent("hive", "supervisor", "claude", "claude-opus-5", "5m"),
		fleetAgent("hive", "guide", "claude", "claude-sonnet-5", "1h"),
		fleetAgent("hive-reef", "architect", "claude", "claude-opus-5", "1h"),
		fleetAgent("hive-hanthor", "strategist", "claude", "claude-fable-5-1", "1h"),
	}
	in := PaceInput{Now: time.Unix(1, 0), Fleet: fleet, Pins: set("hive/supervisor"), Config: DefaultPaceConfig(),
		Verdicts: map[string]PaceVerdict{"anthropic": {Verdict: "hot"}}}
	p := PlanPace(in)
	diffLines(t, p.Text(), []string{"  demote  hive-reef/architect  claude-opus-5 -> claude-sonnet-5  (anthropic hot)", "pace: 1 change(s)"})
	if d := p.Decisions[0]; d.Namespace != "hive-reef" || d.SetDemoted == nil || d.SetDemoted.Model != "claude-opus-5" {
		t.Fatalf("%+v", d)
	}
	// cold: restore ONLY a journaled (or inferred in-tier) demotion.
	in.Verdicts["anthropic"] = PaceVerdict{Verdict: "cold"}
	in.Config.InferDemotions = false
	in.Fleet[2].Model = "claude-sonnet-5"
	if p := PlanPace(in); p.Changes != 0 {
		t.Fatalf("restored a rung it never demoted: %v", p.Text())
	}
	in.Demoted = map[string]Placement{"hive-reef/architect": {"anthropic", "claude", "claude-opus-5"}}
	p = PlanPace(in)
	diffLines(t, p.Text(), []string{"  restore hive-reef/architect  claude-sonnet-5 -> claude-opus-5  (anthropic cold)", "pace: 1 change(s)"})
	// hot with nothing demotable → SATURATED (seated counts unpinned only).
	in.Verdicts["anthropic"] = PaceVerdict{Verdict: "hot"}
	in.Fleet = fleet[:2]
	in.Fleet[1].Model = "claude-sonnet-5"
	diffLines(t, PlanPace(in).Text(), []string{
		"  SATURATED: anthropic is hot but no notch was available (0 of 1 seated agents demotable) — model-rung pacing is exhausted; needs cadence or capacity",
		"pace: 0 change(s)"})
}

// Kiro budget levers: demote by savings until the gap closes (≤4), then cap
// requests (≤2) onto pools with headroom, else SATURATED; under budget
// withdraws caps and promotes ONE demoted agent one notch if the projected
// ratio stays ≤ 0.8.
func TestPaceKiroLevers(t *testing.T) {
	fleet := []FleetAgent{
		fleetAgent("hive", "architect", "pi", "kiro-api-key/claude-opus-5:high", "15m"),    // 4/h × 2.2
		fleetAgent("hive", "guide", "pi", "kiro-api-key/claude-sonnet-5:medium", "1h"),     // 1/h × 1.3
		fleetAgent("hive-reef", "strategist", "pi", "kiro-api-key/gpt-5-6-sol:high", "2h"), // 0.5/h × 4.4
		fleetAgent("hive-reef", "telemetry", "pi", "kiro-api-key/claude-sonnet-5:medium", "paused"),
	}
	over := KiroBudget{Verdict: "over", Burn: fptr(30), Allowed: fptr(11.1)}
	in := PaceInput{Now: time.Unix(1000, 0), Fleet: fleet, Kiro: over, Config: DefaultPaceConfig(), Readings: readings20261001()}
	p := PlanPace(in)
	// total weight 8.8+1.3+2.2 = 12.3; savings: architect 30×8.8/12.3×(1-1.3/2.2)=8.8,
	// strategist 30×2.2/12.3×(1-1.1/4.4)=4.0, guide 30×1.3/12.3×(1-0.4/1.3)=2.2;
	// need 18.9: all three (cum 15.0 < 18.9 after three).
	diffLines(t, p.Text(), []string{
		"  kiro-demote hive/architect  kiro-api-key/claude-opus-5:high -> kiro-api-key/claude-sonnet-5:high  (saves ~8.8 cr/h; need 18.9)",
		"  kiro-demote hive-reef/strategist  kiro-api-key/gpt-5-6-sol:high -> kiro-api-key/gpt-5-6-luna:high  (saves ~4.0 cr/h; need 18.9)",
		"  kiro-demote hive/guide  kiro-api-key/claude-sonnet-5:medium -> kiro-api-key/claude-haiku-4-5:low  (saves ~2.2 cr/h; need 18.9)",
		"pace: 3 change(s)"})
	if !p.KiroActed {
		t.Fatal("KIRO_LAST_ACT")
	}
	// Everything on haiku: cap requests, onto pools with headroom only.
	for i := range in.Fleet {
		in.Fleet[i].Model = "kiro-api-key/claude-haiku-4-5:low"
	}
	r := readings20261001()
	r["anthropic"] = Reading{40, "x"}
	in.Readings = r
	p = PlanPace(in)
	diffLines(t, p.Text(), []string{
		"  kiro-cap    hive/architect  -> off Kiro onto [anthropic] (hive-rotate enacts on its next tick)",
		"  kiro-cap    hive/guide  -> off Kiro onto [anthropic] (hive-rotate enacts on its next tick)",
		"pace: 2 change(s)"})
	if ev := p.Decisions[0].SetEvict; ev == nil || !ev.Expiry.Equal(time.Unix(1000, 0).Add(6*time.Hour)) {
		t.Fatalf("evict TTL: %+v", p.Decisions[0])
	}
	in.Readings = readings20261001() // google 95 / anthropic 100: no headroom
	diffLines(t, PlanPace(in).Text(), []string{
		"  SATURATED: kiro over budget (burn 30.0 > allowed 11.1 cr/h), every kicked Kiro agent is on the cheapest rung, and neither agy nor claude has headroom — needs cadence (operator)",
		"pace: 0 change(s)"})
	// under: withdraw caps, promote the cheapest one notch.
	in.Kiro = KiroBudget{Verdict: "under", Burn: fptr(2), Allowed: fptr(11.1)}
	in.Config.InferDemotions = false // only the journal: "restore only own demotions"
	in.KiroEvict = map[string]KiroEvict{"hive/guide": {Expiry: time.Unix(5000, 0), Targets: []string{"anthropic"}}}
	in.Demoted = map[string]Placement{
		"hive/architect":       {"kiro", "pi", "kiro-api-key/claude-opus-5:high"},
		"hive-reef/strategist": {"kiro", "pi", "kiro-api-key/gpt-5-6-sol:high"},
	}
	p = PlanPace(in)
	want := []string{
		"  kiro under budget: pending cap requests withdrawn",
		// strategist: 2×(0.5×0.4/total)×(1.1/0.4-1); total = (4+1+0.5)×0.4 = 2.2
		fmt.Sprintf("  kiro-promote hive-reef/strategist  kiro-api-key/claude-haiku-4-5:low -> kiro-api-key/gpt-5-6-luna:high  (adds ~%.1f cr/h; kiro under budget)", 2*(0.5*0.4/2.2)*(1.1/0.4-1)),
		"pace: 1 change(s)",
	}
	diffLines(t, p.Text(), want)
	if !p.ClearEvicts || p.Decisions[0].ClearDemoted {
		t.Fatalf("first notch up keeps the journal row: %+v", p.Decisions[0])
	}
}
