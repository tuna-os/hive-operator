package main

import (
	"testing"
	"time"
)

// The live 15:40Z school watchdog healed a stalled scanner; the operator,
// reading a minute later, sees it ready: snapshot skew, not logic.
func TestCompareWatchdogClasses(t *testing.T) {
	bash := watchdogLines(`usage: from ccleft (generated 2026-10-01T15:38:49Z)
supervisor     liveness ok
scanner        stalled  -> healing (restart #1)
scanner        stalled  restarted (restarted)
guide          empty    (healed 2x, backing off 10m)
quality        liveness ok
architect      auth     -> healing (restart #1)
architect      auth     rotating off -> agy gemini-3.8-flash-high
watchdog: 2 agent(s) healed`)
	op := watchdogLines(`supervisor     liveness ok
scanner        liveness ok
guide          empty    -> healing (restart #1)
guide          empty    restarted (would restart)
quality        shell    -> healing (restart #1)
quality        shell    restarted (would restart)
architect      auth     -> healing (restart #1)
architect      auth     rotating off -> agy gemini-3.8-flash-high
watchdog: 3 agent(s) healed`)
	same, res := compareWatchdog(bash, op)
	if same != 2 {
		t.Fatalf("same %d %+v", same, res)
	}
	want := map[string]string{"scanner": "skew/acted", "guide": "journal", "quality": "skew/time"}
	if len(res) != len(want) {
		t.Fatalf("%+v", res)
	}
	for _, r := range res {
		if want[r.agent] != r.class {
			t.Errorf("%s: %s, want %s", r.agent, r.class, want[r.agent])
		}
	}
	// A rotate-off target that differs is logic.
	op["architect"][1] = "architect      auth     rotating off -> pi kiro-api-key/claude-opus-5:high"
	if _, res := compareWatchdog(bash, op); res[0].agent != "architect" || res[0].class != "logic" {
		t.Fatalf("%+v", res)
	}
}

func TestCompareNudge(t *testing.T) {
	bash := nudgeEntries(`hive           adjudicator    idle 66m > 60m (longest cadence 30m x2) — kicked (queued)
hive           reviewer       idle 61m > 60m (longest cadence 30m x2) — kicked (queued)
nudge: 2 agent(s) nudged`)
	op := nudgeEntries(`hive           reviewer       idle 75m > 60m (longest cadence 30m x2) — would nudge
hive           guide          idle 62m > 60m (longest cadence 30m x2) — would nudge
hive-reef      quality        idle 400m > 360m (longest cadence 180m x2) — would nudge`)
	idle := map[string]int64{"hive/adjudicator": 600, "hive/reviewer": 4500, "hive/guide": 3720, "hive-reef/quality": 24000}
	same, res := compareNudge(bash, op, idle, 14*time.Minute)
	if same != 1 {
		t.Fatalf("%d %+v", same, res)
	}
	want := map[string]string{"hive/adjudicator": "skew/acted", "hive/guide": "skew/time", "hive-reef/quality": "logic"}
	for _, r := range res {
		if want[r.agent] != r.class {
			t.Errorf("%s: %s, want %s", r.agent, r.class, want[r.agent])
		}
	}
}
