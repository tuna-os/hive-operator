package liveness

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loadAges reads the in-pod last_kick ages (`<agent> <seconds>`; -1 = never).
func loadAges(t *testing.T, p string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(readFile(t, p)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[0] != "now" {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			out[f[0]] = v
		}
	}
	return out
}

func nudgeAgents(st statusFixture) []NudgeAgent {
	var out []NudgeAgent
	for _, a := range st.Agents {
		out = append(out, NudgeAgent{Name: a.Name, Paused: a.Paused, OnDemand: a.OnDemand != nil && *a.OnDemand, Enabled: a.Enabled})
	}
	return out
}

func nudgeIn(t *testing.T, spoke, ns, stamp string) NudgeInput {
	st := loadFixture(t, spoke, stamp)
	return NudgeInput{Namespace: ns, Cadences: LongestCadences(readFile(t, "testdata/governor-"+spoke+"-"+stamp+".yaml")),
		Agents: nudgeAgents(st), Idle: loadAges(t, "testdata/ages-"+spoke+"-"+stamp+".txt"),
		BudgetExhausted: st.Budget.Exhausted, BudgetPctUsed: st.Budget.Pct, BudgetWeekly: st.Budget.Weekly, Policy: DefaultNudgePolicy()}
}

func okKicks(n int) []KickResult {
	var out []KickResult
	for i := 0; i < n; i++ {
		out = append(out, KickResult{Outcome: KickOK, Detail: "queued"})
	}
	return out
}

// GOLDEN (reconstructed idle): hive-nudge 14:43Z kicked school's
// adjudicator (idle 66m) and reviewer (61m) — both 30m at their slowest, so
// overdue past 60m — and 15:13Z kicked hanthor's supervisor (59m against
// 5m × 2 = 10m, floored to 30m). Cadences are the live governor blocks; the
// /api/status and ages of those minutes were not captured, so each kicked
// agent's idle is the log's and every other agent is inside its threshold.
func TestGoldenNudgeReconstructed(t *testing.T) {
	log := map[string][]string{}
	for _, f := range []string{"testdata/hive-nudge-20261001T1443.log", "testdata/hive-nudge-20261001T1513.log"} {
		log[f] = bashLog(t, f)
	}
	// 14:43: school.
	in := nudgeIn(t, "school", "hive", "20261001T1523")
	for a := range in.Idle {
		in.Idle[a] = 0
	}
	in.Idle["adjudicator"], in.Idle["reviewer"] = 66*60+10, 61*60+5
	p := Nudge(in)
	got := append(NudgeLines(p, okKicks(len(p.Candidates)), 30), NudgeFooter(okKicks(len(p.Candidates))))
	diffLines(t, got, log["testdata/hive-nudge-20261001T1443.log"])

	// 15:13: hanthor (school and reef had nothing overdue).
	in = nudgeIn(t, "hanthor", "hive-hanthor", "20261001T1523")
	for a := range in.Idle {
		in.Idle[a] = 0
	}
	in.Idle["supervisor"] = 59*60 + 40
	p = Nudge(in)
	got = append(NudgeLines(p, okKicks(len(p.Candidates)), 30), NudgeFooter(okKicks(len(p.Candidates))))
	diffLines(t, got, log["testdata/hive-nudge-20261001T1513.log"])
}

// Thresholds from the live governor blocks: the LONGEST cadence across
// every mode, so a governor in a slow mode is never pre-empted.
func TestNudgeLongestCadenceLive(t *testing.T) {
	c := map[string]int{}
	for _, x := range LongestCadences(readFile(t, "testdata/governor-reef-20261001T1523.yaml")) {
		c[x.Agent] = x.Seconds
	}
	// reef architect: 15m (surge) … 3h (quiet) → 3h. supervisor/outreach/
	// strategist/telemetry/operations are paused in every mode → absent.
	if c["architect"] != 3*3600 || c["guide"] != 4*3600 || c["adjudicator"] != 1800 {
		t.Fatalf("%v", c)
	}
	for _, a := range []string{"supervisor", "outreach", "strategist", "telemetry", "operations", "threshold"} {
		if _, ok := c[a]; ok {
			t.Fatalf("%s must have no entry: %v", a, c)
		}
	}
}

func nudgeTable(idle map[string]int64, agents ...NudgeAgent) NudgeInput {
	var cad []Cadence
	for _, a := range agents {
		cad = append(cad, Cadence{a.Name, 1800})
	}
	return NudgeInput{Namespace: "hive", Cadences: cad, Agents: agents, Idle: idle, Policy: DefaultNudgePolicy()}
}

// NEVER nudge past an exhausted budget: the governor is suppressing kicks
// on purpose (school, 190 % of a 50M weekly budget, looked exactly like a
// stalled scheduler).
func TestNudgeBudgetExhaustedGate(t *testing.T) {
	in := nudgeTable(map[string]int64{"guide": 99999}, NudgeAgent{Name: "guide"})
	in.BudgetExhausted, in.BudgetPctUsed, in.BudgetWeekly = true, 190.4, 50000000
	p := Nudge(in)
	if len(p.Candidates) != 0 ||
		p.Skip != "hive           budget exhausted (190% of 50000000) — NOT nudging; this is a cost control, not a stall" {
		t.Fatalf("%+v", p)
	}
	if l := NudgeLines(p, nil, 30); len(l) != 1 || l[0] != p.Skip {
		t.Fatalf("%v", l)
	}
}

func TestNudgeEligibilityAndThreshold(t *testing.T) {
	f := false
	idle := map[string]int64{"ok": 3599, "over": 3601, "paused": 99999, "ondemand": 99999, "disabled": 99999, "never": -1}
	p := Nudge(nudgeTable(idle,
		NudgeAgent{Name: "ok"}, NudgeAgent{Name: "over"}, NudgeAgent{Name: "paused", Paused: true},
		NudgeAgent{Name: "ondemand", OnDemand: true}, NudgeAgent{Name: "disabled", Enabled: &f},
		NudgeAgent{Name: "never"}, NudgeAgent{Name: "noidle"}))
	var names []string
	for _, c := range p.Candidates {
		names = append(names, c.Agent)
	}
	// 1800 × 2 = 3600: strictly greater is overdue; "never" kicked = 999999;
	// an agent with no last_kick row at all is skipped.
	if strings.Join(names, " ") != "over never" {
		t.Fatalf("%v", names)
	}
	if p.Candidates[1].Line != "hive           never          idle 16666m > 60m (longest cadence 30m x2)" {
		t.Fatalf("%q", p.Candidates[1].Line)
	}
	// The floor: a 5m agent is overdue only past 30m, not 10m.
	in := NudgeInput{Namespace: "hive", Cadences: []Cadence{{"supervisor", 300}}, Agents: []NudgeAgent{{Name: "supervisor"}},
		Idle: map[string]int64{"supervisor": 1700}, Policy: DefaultNudgePolicy()}
	if p := Nudge(in); len(p.Candidates) != 0 {
		t.Fatalf("%+v", p)
	}
	in.Idle["supervisor"] = 1801
	if p := Nudge(in); len(p.Candidates) != 1 || p.Candidates[0].ThreshS != 1800 {
		t.Fatalf("%+v", p)
	}
}

// At most 4 SUCCESSFUL kicks per spoke; unanswered/failed kicks do not
// count, and the rest are not attempted (bash `break`s).
func TestNudgeMaxFourPerSpoke(t *testing.T) {
	idle := map[string]int64{}
	var agents []NudgeAgent
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		idle[n] = 99999
		agents = append(agents, NudgeAgent{Name: n})
	}
	p := Nudge(nudgeTable(idle, agents...))
	if len(p.Candidates) != 6 {
		t.Fatalf("%d", len(p.Candidates))
	}
	sh := ShadowKicks(p, 4)
	lines := NudgeLines(p, sh, 30)
	if len(lines) != 4 || !strings.HasSuffix(lines[3], " — would nudge") || NudgeFooter(sh) != "nudge: 4 agent(s) would be nudged" {
		t.Fatalf("%v %s", lines, NudgeFooter(sh))
	}
	// Enforce-shaped: a2 wedged, a4 refused → a1 a3 a5 a6 kicked.
	res := []KickResult{{KickOK, "queued"}, {KickUnanswered, "timeout"}, {KickOK, "in-flight"}, {KickFailed, "agent not found"},
		{KickOK, "queued"}, {KickOK, "queued"}}
	lines = NudgeLines(p, res, 30)
	want := []string{
		"hive           a1             idle 1666m > 60m (longest cadence 30m x2) — kicked (queued)",
		"hive           a2             idle 1666m > 60m (longest cadence 30m x2) — kick not answered in 30s (agent likely wedged mid-delivery); skipped",
		"hive           a3             idle 1666m > 60m (longest cadence 30m x2) — kicked (in-flight)",
		"hive           a4             idle 1666m > 60m (longest cadence 30m x2) — KICK FAILED: agent not found",
		"hive           a5             idle 1666m > 60m (longest cadence 30m x2) — kicked (queued)",
		"hive           a6             idle 1666m > 60m (longest cadence 30m x2) — kicked (queued)",
	}
	diffLines(t, lines, want)
	if NudgeFooter(res) != "nudge: 4 kicked, 1 not answered" {
		t.Fatal(NudgeFooter(res))
	}
	if NudgeFooter(nil) != "nudge: nothing overdue" {
		t.Fatal(NudgeFooter(nil))
	}
}

func TestNudgeCadencesUnreadable(t *testing.T) {
	p := Nudge(NudgeInput{Namespace: "hive-reef", CadencesErr: true})
	if p.Skip != "hive-reef      could not read cadences — skipped" {
		t.Fatalf("%q", p.Skip)
	}
}

// GOLDEN: hive-nudge 15:43Z found nothing overdue on any spoke, from the
// governor blocks, /api/status and last_kick ages read at 15:41Z.
func TestGoldenNudge20261001T1543(t *testing.T) {
	var all []KickResult
	for _, s := range [][2]string{{"school", "hive"}, {"reef", "hive-reef"}, {"hanthor", "hive-hanthor"}} {
		p := Nudge(nudgeIn(t, s[0], s[1], "20261001T1541"))
		if p.Skip != "" || len(p.Candidates) != 0 {
			t.Fatalf("%s: %+v", s[0], p)
		}
		all = append(all, ShadowKicks(p, 4)...)
	}
	diffLines(t, []string{NudgeFooter(all)}, bashLog(t, "testdata/hive-nudge-20261001T1543.log"))
}
