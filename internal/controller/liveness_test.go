package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/liveness"
	"github.com/tuna-os/hive-operator/internal/rotation"
)

// fakeLiveness records every call; mutating calls answer like v6.
type fakeLiveness struct {
	calls    []string
	panes    map[string]string
	governor string
	kick     map[string]string
}

func (f *fakeLiveness) Place(_ context.Context, _, agent string, p hiveclient.Placement) (string, error) {
	f.calls = append(f.calls, "place "+agent+" "+p.Backend+" "+p.Model)
	return `{"ok":true,"applied":true,"status":"updated"}`, nil
}
func (f *fakeLiveness) Post(_ context.Context, _, path string) (string, error) {
	f.calls = append(f.calls, "POST "+path)
	if strings.HasPrefix(path, "/api/kick/") {
		if b, ok := f.kick[strings.TrimPrefix(path, "/api/kick/")]; ok {
			return b, nil
		}
		return `{"ok":true,"status":"queued"}`, nil
	}
	if strings.HasPrefix(path, "/api/effort/") {
		return `{"ok":true,"status":"effort_set"}`, nil
	}
	return `{"ok":true}`, nil
}
func (f *fakeLiveness) Pane(_ context.Context, _, agent string) (string, error) {
	f.calls = append(f.calls, "GET pane "+agent)
	return f.panes[agent], nil
}
func (f *fakeLiveness) Governor(context.Context, string) (string, error) {
	f.calls = append(f.calls, "GET governor")
	return f.governor, nil
}
func (f *fakeLiveness) Restart(_ context.Context, _, agent string, fixPerms bool) (string, error) {
	c := "restart " + agent
	if fixPerms {
		c += " +perms"
	}
	f.calls = append(f.calls, c)
	return `{"ok":true,"status":"restarted","agent":"` + agent + `"}`, nil
}
func (f *fakeLiveness) Hygiene(context.Context, string) error {
	f.calls = append(f.calls, "hygiene")
	return nil
}

func (f *fakeLiveness) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

const govYAML = `governor:
    modes:
        busy:
            guide: 30m
            scanner: 30m
            quality: 30m
`

func livenessLadder() *hivev1.ModelLadder {
	l := &hivev1.ModelLadder{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}}
	for _, r := range [][4]string{
		{"T2", "google", "agy", "gemini-3.8-flash-low"},
		{"T2", "kiro", "pi", "kiro-api-key/claude-sonnet-5:medium"},
		{"T2", "anthropic", "claude", "claude-sonnet-5"},
		{"T2", "meta", "muse", "muse-spark-1.3-contributor"},
	} {
		l.Status.Effective = append(l.Status.Effective, hivev1.Rung{Tier: r[0], Provider: r[1], Backend: r[2], Model: r[3], Available: true})
	}
	return l
}

// A spoke with: guide on claude parked on a login prompt (rotate-off
// candidate), scanner a dead shell on pi... and quality ready but idle 2 h
// (a nudge candidate).
func livenessSpoke(livenessMode, rotationMode hivev1.ReconcileMode) (*hivev1.HiveSpoke, livenessInputs) {
	sp := &hivev1.HiveSpoke{ObjectMeta: metav1.ObjectMeta{Name: "reef"},
		Spec: hivev1.HiveSpokeSpec{Namespace: "hive-reef", LadderRef: "fleet", LivenessMode: livenessMode, RotationMode: rotationMode},
		Status: hivev1.HiveSpokeStatus{Agents: []hivev1.AgentState{
			{Name: "guide", Backend: "claude", Model: "claude-sonnet-5", Cadence: "1h"},
			{Name: "scanner", Backend: "muse", Model: "muse-spark-1.3-contributor", Cadence: "1h"},
			{Name: "quality", Backend: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "1h"},
		}}}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	li := livenessInputs{snapshot: now.Add(-time.Minute), ages: map[string]int64{"guide": 60, "scanner": 60, "quality": 7200},
		budget:   map[string]any{"BUDGET_EXHAUSTED": false},
		readings: map[string]rotation.Reading{"google": {Percent: 10}, "kiro": {Percent: 5}, "anthropic": {Percent: 99}, "openai": {Percent: 99}, "meta": {Percent: -1, Note: "no-usage-api"}},
		sources:  map[string]string{},
		agents: []liveness.Agent{
			{Name: "guide", CLI: "claude", Model: "claude-sonnet-5", Busy: "idle", LiveSummary: "Select login method:"},
			{Name: "scanner", CLI: "muse", Model: "muse-spark-1.3-contributor", Busy: "idle", LiveSummary: "hive-scanner@h:/data/agents/scanner$"},
			{Name: "quality", CLI: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Busy: "idle", LiveSummary: "(kiro-api-key) claude-sonnet-5 • medium"},
		},
		nudge: []liveness.NudgeAgent{{Name: "guide"}, {Name: "scanner"}, {Name: "quality"}},
	}
	return sp, li
}

func runLiveness(t *testing.T, sp *hivev1.HiveSpoke, li livenessInputs, api *fakeLiveness, now time.Time) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(livenessLadder()).Build()
	r := &HiveSpokeReconciler{Client: c}
	r.liveness(context.Background(), sp, li, api, now)
}

var lnow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Shadow plans the exact pass, reads the live panes and governor, and
// mutates NOTHING.
func TestLivenessShadowNeverMutates(t *testing.T) {
	sp, li := livenessSpoke(hivev1.ModeShadow, hivev1.ModeShadow)
	api := &fakeLiveness{panes: map[string]string{"guide": "Select login method:", "scanner": "hive-scanner@h:/data/agents/scanner$"}, governor: govYAML}
	runLiveness(t, sp, li, api, lnow)
	if m := api.mutations(); len(m) != 0 {
		t.Fatalf("Shadow mutated: %v", m)
	}
	l := sp.Status.Liveness
	want := []string{
		"guide          auth     -> healing (restart #1)",
		"guide          auth     rotating off -> agy gemini-3.8-flash-low",
		"guide          auth     restarted (would restart)",
		"scanner        shell    -> healing (restart #1)",
		"scanner        shell    rotating off -> agy gemini-3.8-flash-low",
		"scanner        shell    restarted (would restart)",
		"quality        liveness ok",
		"watchdog: 2 agent(s) healed",
	}
	if strings.Join(l.WatchdogPlanText, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan text\n%s", strings.Join(l.WatchdogPlanText, "\n"))
	}
	// rotationMode Shadow: the rotate-off is reported, not applied.
	if len(l.WatchdogPlan) != 2 || l.WatchdogPlan[0].Kind != liveness.KindRotateOff || l.WatchdogPlan[0].Applied ||
		!strings.Contains(l.WatchdogPlan[0].Error, "rotationMode is Shadow") {
		t.Fatalf("%+v", l.WatchdogPlan)
	}
	// Journal kept as if applied: backoff ladder warm at promotion.
	if l.Journal == nil || len(l.Journal.Heals) != 2 || l.Journal.Heals[0].Count != 1 {
		t.Fatalf("%+v", l.Journal)
	}
	if strings.Join(l.NudgePlanText, "\n") != "hive-reef      quality        idle 120m > 60m (longest cadence 30m x2) — would nudge\nnudge: 1 agent(s) would be nudged" {
		t.Fatalf("%v", l.NudgePlanText)
	}
	if l.WatchdogAt == nil || l.NudgeAt == nil {
		t.Fatal("timestamps")
	}
}

// Enforce with rotation still Shadow (bash-owned placement): the rotate-off
// is only reported and the agent restarted in place; muse/agy restarts
// reopen the shared first-run state in the same exec; nudge kicks.
func TestLivenessEnforceRotationShadow(t *testing.T) {
	sp, li := livenessSpoke(hivev1.ModeEnforce, hivev1.ModeShadow)
	api := &fakeLiveness{panes: map[string]string{"guide": "Select login method:", "scanner": "hive-scanner@h:/data/agents/scanner$"}, governor: govYAML}
	runLiveness(t, sp, li, api, lnow)
	want := "hygiene,restart guide,restart scanner +perms,POST /api/kick/quality"
	if got := strings.Join(api.mutations(), ","); got != want {
		t.Fatalf("calls %s\nwant  %s", got, want)
	}
	l := sp.Status.Liveness
	if !strings.Contains(strings.Join(l.WatchdogPlanText, "\n"), "guide          auth     restarted (restarted)") {
		t.Fatalf("%v", l.WatchdogPlanText)
	}
	if l.NudgePlanText[0] != "hive-reef      quality        idle 120m > 60m (longest cadence 30m x2) — kicked (queued)" ||
		!l.NudgePlan[0].Applied {
		t.Fatalf("%v %+v", l.NudgePlanText, l.NudgePlan)
	}
}

// Enforce with rotation Enforce: the rotate-off is a placement through the
// rotation actuator (atomic PUT), not a restart.
func TestLivenessEnforceRotateOff(t *testing.T) {
	sp, li := livenessSpoke(hivev1.ModeEnforce, hivev1.ModeEnforce)
	api := &fakeLiveness{panes: map[string]string{"guide": "Select login method:", "scanner": "hive-scanner@h:/data/agents/scanner$"}, governor: govYAML}
	runLiveness(t, sp, li, api, lnow)
	want := "hygiene,place guide agy gemini-3.8-flash-low,place scanner agy gemini-3.8-flash-low,POST /api/kick/quality"
	if got := strings.Join(api.mutations(), ","); got != want {
		t.Fatalf("calls %s\nwant  %s", got, want)
	}
	if !sp.Status.Liveness.WatchdogPlan[0].Applied {
		t.Fatalf("%+v", sp.Status.Liveness.WatchdogPlan)
	}
}

// The watchdog runs every 5 minutes and nudge every 30, whatever the
// reconcile cadence; nudge never kicks past an exhausted budget.
func TestLivenessIntervalsAndBudgetGate(t *testing.T) {
	sp, li := livenessSpoke(hivev1.ModeEnforce, hivev1.ModeShadow)
	li.agents = li.agents[2:] // only the healthy, overdue quality
	sp.Status.BudgetExhausted = true
	li.budget = map[string]any{"BUDGET_EXHAUSTED": true, "BUDGET_PCT_USED": 190.4, "BUDGET_WEEKLY": 5e7}
	api := &fakeLiveness{governor: govYAML}
	runLiveness(t, sp, li, api, lnow)
	if got := strings.Join(api.mutations(), ","); got != "hygiene" {
		t.Fatalf("kicked past an exhausted budget: %s", got)
	}
	if sp.Status.Liveness.NudgePlanText[0] != "hive-reef      budget exhausted (190% of 50000000) — NOT nudging; this is a cost control, not a stall" {
		t.Fatalf("%v", sp.Status.Liveness.NudgePlanText)
	}
	sp.Status.BudgetExhausted = false
	for _, step := range []struct {
		after          time.Duration
		watchdog, kick bool
	}{
		{2 * time.Minute, false, false},
		{4 * time.Minute, false, false},
		{5 * time.Minute, true, false},
		{8 * time.Minute, false, false},
		{10 * time.Minute, true, false},
		{30 * time.Minute, true, true},
	} {
		api.calls = nil
		prevW := sp.Status.Liveness.WatchdogAt.Time
		runLiveness(t, sp, li, api, lnow.Add(step.after))
		ran := !sp.Status.Liveness.WatchdogAt.Time.Equal(prevW)
		kicked := strings.Contains(strings.Join(api.calls, ","), "/api/kick/")
		if ran != step.watchdog || kicked != step.kick {
			t.Fatalf("+%v: watchdog %v kick %v (%v)", step.after, ran, kicked, api.calls)
		}
	}
	if d := livenessNextDue(sp); !d.Equal(lnow.Add(35*time.Minute - livenessSlack)) {
		t.Fatalf("next due %v", d)
	}
}

// A renewal wake-up in Enforce makes an Enforce rotation due at once; with
// rotation in Shadow it is only reported.
func TestLivenessWakeMakesRotationDue(t *testing.T) {
	for _, rot := range []hivev1.ReconcileMode{hivev1.ModeEnforce, hivev1.ModeShadow} {
		sp, li := livenessSpoke(hivev1.ModeEnforce, rot)
		applied := metav1.NewTime(lnow.Add(-10 * time.Minute))
		sp.Status.RotationAppliedAt = &applied
		li.readings["anthropic"] = rotation.Reading{Percent: 100, Note: "resets=2026-10-01T11:59:00Z"}
		sp.Status.Liveness = &hivev1.LivenessStatus{Journal: &hivev1.LivenessJournal{
			Resets: []hivev1.ProviderReset{{Provider: "anthropic", At: metav1.NewTime(lnow.Add(-time.Minute))}}}}
		api := &fakeLiveness{governor: govYAML}
		runLiveness(t, sp, li, api, lnow)
		l := sp.Status.Liveness
		if l.WatchdogPlanText[0] != "renewal reached for: anthropic — re-deciding placement from current credits" {
			t.Fatalf("%v", l.WatchdogPlanText)
		}
		var wake hivev1.LivenessAction
		for _, a := range l.WatchdogPlan {
			if a.Kind == liveness.KindWake {
				wake = a
			}
		}
		if rot == hivev1.ModeEnforce {
			if !wake.Applied || sp.Status.RotationAppliedAt != nil {
				t.Fatalf("enforce: %+v %v", wake, sp.Status.RotationAppliedAt)
			}
			if requeueAfter(sp, rot, 2*time.Minute, lnow) != 10*time.Second {
				t.Fatal("a wake-up must requeue promptly")
			}
		} else if wake.Applied || sp.Status.RotationAppliedAt == nil || !strings.Contains(wake.Error, "reported only") {
			t.Fatalf("shadow rotation: %+v", wake)
		}
	}
}

// Observe computes nothing; a rotation apply in this reconcile defers the
// watchdog (placements /api/status does not show yet).
func TestLivenessObserveAndDeferAfterRotate(t *testing.T) {
	sp, li := livenessSpoke(hivev1.ModeObserve, hivev1.ModeShadow)
	api := &fakeLiveness{governor: govYAML}
	runLiveness(t, sp, li, api, lnow)
	if len(api.calls) != 0 || sp.Status.Liveness != nil {
		t.Fatalf("Observe: %v %+v", api.calls, sp.Status.Liveness)
	}
	sp, li = livenessSpoke(hivev1.ModeShadow, hivev1.ModeEnforce)
	t0 := metav1.NewTime(lnow)
	sp.Status.RotationAppliedAt = &t0
	runLiveness(t, sp, li, api, lnow)
	if sp.Status.Liveness.WatchdogAt != nil {
		t.Fatal("watchdog must wait for the next reconcile after a rotation apply")
	}
}
