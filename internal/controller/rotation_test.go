package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// The pool reduces ccleft exactly as hive-lib.sh's ccleft_probe does, records
// one pace sample per ccleft fetch, and falls back to the ConfigMap.
func TestPoolRotationReadingAndHistory(t *testing.T) {
	pool := ccleftPool("kiro", "kiro", &hivev1.CcleftRef{URL: "http://ccleft"})
	got := reconcilePool(t, pool, &fakeReadings{out: liveCcleft(t)}, liveProbeCM())
	rr := got.Status.RotationReading
	if rr == nil || rr.Source != "ccleft" || rr.Percent != 38 || !strings.HasPrefix(rr.Note, "credits=3864.61/10000 resets=2026-10-01T00:00:00Z") {
		t.Fatalf("rotation reading %+v", rr)
	}
	if len(got.Status.PaceHistory) != 1 || !strings.HasSuffix(got.Status.PaceHistory[0], " 3864.61 10000") {
		t.Fatalf("history %v", got.Status.PaceHistory)
	}
	if got.Status.Pace == nil || got.Status.Pace.Verdict != "learning" || got.Status.Pace.KiroBudget == nil ||
		got.Status.Pace.KiroBudget.Verdict != "learning" {
		t.Fatalf("pace %+v", got.Status.Pace)
	}

	// ccleft down: the fresh ConfigMap (11:27Z, 23 minutes old) serves.
	got = reconcilePool(t, pool, &fakeReadings{err: errors.New("dial tcp: refused")}, liveProbeCM())
	rr = got.Status.RotationReading
	if rr == nil || rr.Source != "configmap" || rr.Percent != 37 || rr.Note != "credits=3777.17/10000 resets=2026-10-01T00:00:00Z" {
		t.Fatalf("fallback %+v", rr)
	}
	// Neither: unmeasured, never exhausted.
	got = reconcilePool(t, pool, &fakeReadings{err: errors.New("down")})
	if rr = got.Status.RotationReading; rr.Source != "none" || rr.Percent != -1 {
		t.Fatalf("none %+v", rr)
	}
}

func TestRecordSamplesDedupesAndThrottles(t *testing.T) {
	pool := &hivev1.UsagePool{Spec: hivev1.UsagePoolSpec{Provider: "google"}}
	now := time.Unix(10_000, 0)
	s := func(ts int64) []rotationSample { return []rotationSample{{ts, 50}} }
	for _, ts := range []int64{1000, 1000, 1600, 2200, 2300} {
		recordSamples(pool, toSamples("google", s(ts)), now)
	}
	// 1000 kept; 1000 duplicate; 1600 (< 1200 s after the last kept)
	// skipped; 2200 kept; 2300 skipped.
	if len(pool.Status.PaceHistory) != 2 || !strings.HasPrefix(pool.Status.PaceHistory[1], "2200 ") {
		t.Fatalf("%v", pool.Status.PaceHistory)
	}
	recordSamples(pool, nil, now.Add(61*time.Hour))
	if len(pool.Status.PaceHistory) != 0 {
		t.Fatalf("history beyond 60 h must be pruned: %v", pool.Status.PaceHistory)
	}
}

type rotationSample struct {
	ts  int64
	pct float64
}

func toSamples(p string, in []rotationSample) []usage.PaceSample {
	var out []usage.PaceSample
	for _, s := range in {
		out = append(out, usage.PaceSample{TS: s.ts, Provider: p, Slot: "slot0", Pct: s.pct})
	}
	return out
}

func usageCMAt(at time.Time) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "hive", Name: "hive-provider-usage"},
		Data: map[string]string{
			"anthropic":  "100% used no-credential (ccleft auth_required cause=no_credentials: needs an interactive login)",
			"google":     "95% used resets=2026-10-07T03:17:08Z",
			"kiro":       "4% used credits=458.85/10000 resets=2026-11-01T00:00:00Z",
			"meta":       "unknown no-usage-api (ccleft unsupported cause=api_key_login)",
			"openai":     "100% used weekly=100% resets=2026-10-03T22:41:44Z",
			"updated_at": at.UTC().Format(time.RFC3339),
		}}
}

func TestRotationReadingsPreferPoolThenConfigMap(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 30, 0, 0, time.UTC)
	kiro := &hivev1.UsagePool{ObjectMeta: metav1.ObjectMeta{Name: "kiro"}, Spec: hivev1.UsagePoolSpec{Provider: "kiro"},
		Status: hivev1.UsagePoolStatus{RotationReading: &hivev1.PoolRotationReading{Percent: 5, Note: "credits=500/10000 resets=x",
			Source: "ccleft", ComputedAt: metaTime(now.Add(-time.Minute))}}}
	stale := &hivev1.UsagePool{ObjectMeta: metav1.ObjectMeta{Name: "openai"}, Spec: hivev1.UsagePoolSpec{Provider: "openai"},
		Status: hivev1.UsagePoolStatus{RotationReading: &hivev1.PoolRotationReading{Percent: 1, Source: "ccleft",
			ComputedAt: metaTime(now.Add(-time.Hour))}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(kiro, stale, usageCMAt(now.Add(-5*time.Minute))).Build()
	r := &HiveSpokeReconciler{Client: c}
	sp := &hivev1.HiveSpoke{Spec: hivev1.HiveSpokeSpec{Namespace: "hive-hanthor"}}
	rd, src := r.rotationReadings(context.Background(), sp, now)
	if rd["kiro"].Percent != 5 || src["kiro"] != "ccleft" {
		t.Fatalf("kiro from the pool: %+v %s", rd["kiro"], src["kiro"])
	}
	if rd["openai"].Percent != 100 || src["openai"] != "configmap" {
		t.Fatalf("a stale pool falls back to the ConfigMap: %+v %s", rd["openai"], src["openai"])
	}
	if rd["meta"].Percent != -1 || rd["meta"].Note != "no-usage-api (ccleft unsupported cause=api_key_login)" {
		t.Fatalf("meta %+v", rd["meta"])
	}
	// A ConfigMap older than 20 minutes is not reused: unmeasured.
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(usageCMAt(now.Add(-25 * time.Minute))).Build()
	r = &HiveSpokeReconciler{Client: c}
	rd, src = r.rotationReadings(context.Background(), sp, now)
	if rd["google"].Percent != -1 || src["google"] != "none" {
		t.Fatalf("stale ConfigMap: %+v %s", rd["google"], src["google"])
	}
}

type recActuator struct {
	calls []string
	fail  map[string]bool
}

func (a *recActuator) Apply(_ context.Context, ns string, d rotation.Decision) error {
	a.calls = append(a.calls, ns+" "+d.Action+" "+d.Agent+" "+d.To.Model)
	if a.fail[d.Agent] {
		return errors.New("refused")
	}
	return nil
}

func fleetLadder() *hivev1.ModelLadder {
	l := &hivev1.ModelLadder{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}}
	add := func(tier, p, b, m string) {
		l.Status.Effective = append(l.Status.Effective, hivev1.Rung{Tier: tier, Provider: p, Backend: b, Model: m, Available: true})
	}
	add("T1", "google", "agy", "gemini-3.8-flash-high")
	add("T1", "kiro", "pi", "kiro-api-key/claude-opus-5:high")
	add("T2", "google", "agy", "gemini-3.8-flash-low")
	add("T2", "kiro", "pi", "kiro-api-key/claude-sonnet-5:medium")
	add("T2", "meta", "muse", "muse-spark-1.3-contributor")
	return l
}

func enforceSpoke(name, ns string, agents ...hivev1.AgentState) *hivev1.HiveSpoke {
	return &hivev1.HiveSpoke{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   hivev1.HiveSpokeSpec{Namespace: ns, PrimarySpoke: "hive", RotationMode: hivev1.ModeEnforce},
		Status: hivev1.HiveSpokeStatus{Reachable: true, Agents: agents}}
}

// Enforce applies the same plan Shadow shows, journals strands and canary
// cooldowns, and does not re-apply inside the rotation interval.
func TestRotateEnforceAppliesAndJournals(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 30, 0, 0, time.UTC)
	sp := enforceSpoke("hanthor", "hive-hanthor",
		hivev1.AgentState{Name: "architect", Backend: "pi", Model: "kiro-api-key/claude-opus-5:high", Cadence: "4h"},
		hivev1.AgentState{Name: "guide", Backend: "pi", Model: "kiro-api-key/claude-sonnet-5:medium", Cadence: "4h"},
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fleetLadder(), sp).WithStatusSubresource(sp).Build()
	r := &HiveSpokeReconciler{Client: c}
	act := &recActuator{fail: map[string]bool{}}
	readings := map[string]rotation.Reading{"kiro": {Percent: 96, Note: "credits"}, "google": {Percent: 100, Note: "x"}, "meta": {Percent: 100, Note: "x"},
		"anthropic": {Percent: 100, Note: "x"}, "openai": {Percent: 100, Note: "x"}}
	src := map[string]string{"kiro": "ccleft", "google": "ccleft", "meta": "ccleft", "anthropic": "ccleft", "openai": "ccleft"}
	r.rotate(context.Background(), sp, hivev1.ModeEnforce, readings, src, act, now)
	if strings.Join(act.calls, "|") != "hive-hanthor strand architect |hive-hanthor strand guide " {
		t.Fatalf("calls %v", act.calls)
	}
	if j := sp.Status.Journal; j == nil || len(j.Stranded) != 2 || j.Stranded[0].Provider != "kiro" {
		t.Fatalf("journal %+v", sp.Status.Journal)
	}
	// Inside the interval nothing is applied again (status lags mutations).
	act.calls = nil
	sp.Status.RotationPlan = nil
	r.rotate(context.Background(), sp, hivev1.ModeEnforce, readings, src, act, now.Add(5*time.Minute))
	if len(act.calls) != 0 {
		t.Fatalf("re-applied inside the interval: %v", act.calls)
	}
	// After it, with kiro recovered: the journal resumes them — and, as in
	// bash apply (the row is gone by then), the undeclared-pause pass
	// resumes them again.
	sp.Status.RotationPlan = nil
	for i := range sp.Status.Agents {
		sp.Status.Agents[i].Paused, sp.Status.Agents[i].PausedTrigger = true, "dashboard-api"
	}
	readings["kiro"] = rotation.Reading{Percent: 20, Note: "credits"}
	r.rotate(context.Background(), sp, hivev1.ModeEnforce, readings, src, act, now.Add(21*time.Minute))
	if strings.Join(act.calls, "|") != "hive-hanthor resume architect |hive-hanthor resume guide |hive-hanthor resume architect |hive-hanthor resume guide " ||
		len(sp.Status.Journal.Stranded) != 0 {
		t.Fatalf("un-strand: %v %+v", act.calls, sp.Status.Journal)
	}
	// Shadow never calls the actuator.
	act.calls = nil
	sp.Status.RotationAppliedAt = nil
	r.rotate(context.Background(), sp, hivev1.ModeShadow, readings, src, act, now.Add(60*time.Minute))
	if len(act.calls) != 0 {
		t.Fatalf("shadow applied: %v", act.calls)
	}
}

// Promotion seeds the pace journal from the live placement, so rotation
// keeps a demoted agent and the pacer can restore it.
func TestEnforceSeedsPaceJournal(t *testing.T) {
	sp := enforceSpoke("hanthor", "hive-hanthor",
		hivev1.AgentState{Name: "architect", Backend: "pi", Model: "kiro-api-key/claude-haiku-4-5:low", Cadence: "4h"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fleetLadder(), sp).WithStatusSubresource(sp).Build()
	r := &HiveSpokeReconciler{Client: c}
	readings := map[string]rotation.Reading{"kiro": {Percent: 4, Note: "credits"}}
	r.rotate(context.Background(), sp, hivev1.ModeEnforce, readings, map[string]string{}, &recActuator{}, time.Now())
	j := sp.Status.Journal
	if j == nil || len(j.PaceDemoted) != 1 || !j.PaceDemoted[0].Inferred || j.PaceDemoted[0].Model != "kiro-api-key/claude-opus-5:high" {
		t.Fatalf("journal %+v", j)
	}
	if !strings.Contains(strings.Join(sp.Status.RotationPlanText, "\n"), "ok (pace-demoted from kiro-api-key/claude-opus-5:high)") {
		t.Fatalf("plan %v", sp.Status.RotationPlanText)
	}
}

// Pace is fleet-wide: one notch per provider per interval across every
// Enforce spoke, each spoke applying only its own agents.
func TestPaceOneNotchFleetWide(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 30, 0, 0, time.UTC)
	school := enforceSpoke("school", "hive",
		hivev1.AgentState{Name: "guide", Backend: "agy", Model: "gemini-3.8-flash-high", Cadence: "1h"})
	school.Spec.Pace = &hivev1.PaceSpec{FleetOrder: 0}
	hanthor := enforceSpoke("hanthor", "hive-hanthor",
		hivev1.AgentState{Name: "quality", Backend: "agy", Model: "gemini-3.8-flash-high", Cadence: "1h"})
	hanthor.Spec.Pace = &hivev1.PaceSpec{FleetOrder: 2}
	google := &hivev1.UsagePool{ObjectMeta: metav1.ObjectMeta{Name: "google"}, Spec: hivev1.UsagePoolSpec{Provider: "google"},
		Status: hivev1.UsagePoolStatus{Pace: &hivev1.PoolPaceStatus{Verdict: "hot", Pressure: "2"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fleetLadder(), school, hanthor, google).
		WithStatusSubresource(school, hanthor, google).Build()
	r := &HiveSpokeReconciler{Client: c}
	act := &recActuator{}
	in := rotation.Input{Providers: map[string]rotation.Reading{}}
	rungs := ladderRungs(fleetLadder().Status.Effective)

	// hanthor ticks first: the fleet's notch belongs to school/guide.
	r.pace(context.Background(), hanthor, hivev1.ModeEnforce, in, rungs, act, now)
	if len(act.calls) != 0 || len(hanthor.Status.PacePlan) != 0 ||
		!strings.Contains(strings.Join(hanthor.Status.PacePlanText, "\n"), "  demote  hive/guide  gemini-3.8-flash-high -> gemini-3.8-flash-low  (google hot)") {
		t.Fatalf("hanthor: %v %v", act.calls, hanthor.Status.PacePlanText)
	}
	r.pace(context.Background(), school, hivev1.ModeEnforce, in, rungs, act, now)
	if strings.Join(act.calls, "|") != "hive demote guide gemini-3.8-flash-low" {
		t.Fatalf("school: %v", act.calls)
	}
	if j := school.Status.Journal; j == nil || len(j.PaceDemoted) != 1 || j.PaceDemoted[0].Model != "gemini-3.8-flash-high" {
		t.Fatalf("journal %+v", school.Status.Journal)
	}
	if err := c.Status().Update(context.Background(), school); err != nil {
		t.Fatal(err)
	}
	// school's agent is now demoted; hanthor's next tick would pick its own
	// agent — but google already took its notch this interval.
	school.Status.Agents[0].Model = "gemini-3.8-flash-low"
	_ = c.Status().Update(context.Background(), school)
	hanthor.Status.PaceTickAt = nil
	act.calls = nil
	r.pace(context.Background(), hanthor, hivev1.ModeEnforce, in, rungs, act, now.Add(2*time.Minute))
	if len(act.calls) != 0 || len(hanthor.Status.PacePlan) != 1 || hanthor.Status.PacePlan[0].Error == "" {
		t.Fatalf("second notch in one interval: %v %+v", act.calls, hanthor.Status.PacePlan)
	}
	var g hivev1.UsagePool
	_ = c.Get(context.Background(), client.ObjectKey{Name: "google"}, &g)
	if g.Status.Pace.LastActuation == nil {
		t.Fatal("lastActuation not recorded")
	}
	_ = types.NamespacedName{}
}
