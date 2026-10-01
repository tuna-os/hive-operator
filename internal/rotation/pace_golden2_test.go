package rotation

import (
	"strings"
	"testing"
	"time"
)

// GOLDEN: hive-pace apply at 2026-10-01T14:45Z — Kiro went over budget
// (burn 11.7 > allowed 11.1 cr/h) and the pacer demoted two agents, chosen by
// estimated savings with bash sort's tie-break (three candidates at ~0.4
// cr/h; whole-line order puts hive/guide, then hive-hanthor/sec-check, before
// hive-hanthor/strategist). The fleet is the 15:01Z snapshot with those two
// demotions undone: what the job saw.
func TestGoldenPaceKiroDemotion20261001T1445(t *testing.T) {
	verdicts, kb := paceCM(t, "20261001T1445")
	wantTable, wantKiro, wantActions := paceLog(t, "20261001T1445")
	if got := strings.Join(VerdictTable(verdicts), "\n"); got != wantTable {
		t.Errorf("verdict table\n--- bash\n%s\n--- operator\n%s", wantTable, got)
	}
	if got := kb.Line(); got != wantKiro {
		t.Errorf("kiro line\n bash     %s\n operator %s", wantKiro, got)
	}
	fleet := liveFleet(t, "20261001T1501")
	undo := map[string]string{
		"hive/guide":             "kiro-api-key/claude-sonnet-5:medium",
		"hive-hanthor/sec-check": "kiro-api-key/claude-sonnet-5:high",
	}
	for i := range fleet {
		if m, ok := undo[fleet[i].Namespace+"/"+fleet[i].Name]; ok {
			fleet[i].Model = m
		}
	}
	in := PaceInput{Now: time.Date(2026, 10, 1, 14, 45, 1, 0, time.UTC), Fleet: fleet, Verdicts: verdicts, Kiro: kb,
		Readings: readings20261001(), Pins: set("hive/supervisor"), Config: DefaultPaceConfig()}
	p := PlanPace(in)
	diffLines(t, p.Text(), wantActions)
	// sec-check (T1) sits on sonnet-5:high: the journal keeps the ORIGINAL
	// opus-5:high (bash's `${df:-…}`), so rotation still sees T1.
	if !p.KiroActed || len(p.Decisions) != 2 || p.Decisions[1].Namespace != "hive-hanthor" ||
		p.Decisions[1].SetDemoted == nil || p.Decisions[1].SetDemoted.Model != "kiro-api-key/claude-opus-5:high" {
		t.Fatalf("%+v", p.Decisions)
	}
}

// GOLDEN: the school rotate at 15:00Z, after that pace tick — guide now
// reads as pace-demoted (inferred) and stays put.
func TestGoldenSchool20261001T1500(t *testing.T) {
	in := liveInput(t, "school", "hive", true, set("supervisor"), set("reviewer"), time.Date(2026, 10, 1, 15, 0, 3, 0, time.UTC))
	in.Agents = loadStatus(t, "testdata/status-school-20261001T1501.json")
	want, wantContrib := bashPlan(t, "testdata/hive-rotate-school-20261001T1500.log")
	diffLines(t, Compute(in).Lines(false), want)
	var got []string
	for _, d := range Contributors(in, []ContribDeploy{
		{Name: "agy-contributor", Backend: "agy", Model: "gemini-3.7-flash-high"},
		{Name: "claude-contributor", Backend: "claude"},
		{Name: "pi-codex-contributor", Backend: "pi", Model: "openai-codex/gpt-5.6-sol"},
	}, "hive-contributors") {
		got = append(got, d.Line)
	}
	diffLines(t, got, wantContrib)
}
