package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// fakeFetcher serves canned sidecar reports. Each namespace's rows are
// returned for every requested window start, so a test controls exactly what
// each window sums to via fixed per-namespace rows.
type fakeFetcher struct {
	reports map[string]usage.Report
	rows    map[string]map[string][]usage.Row // ns → window label ("recent" or window name) → rows
	starts  map[string]string                 // RFC3339 start → label
	calls   int
}

func (f *fakeFetcher) Fetch(_ context.Context, ns string, _ int32, since []time.Time) (*usage.Report, error) {
	f.calls++
	base, ok := f.reports[ns]
	if !ok {
		return nil, fmt.Errorf("connection refused")
	}
	r := base
	r.Windows = nil
	for _, s := range since {
		label := f.starts[s.Format(time.RFC3339)]
		r.Windows = append(r.Windows, usage.WindowUsage{Since: s, Rows: f.rows[ns][label]})
	}
	return &r, nil
}

func row(source, provider, agent, model string, cost float64) usage.Row {
	return usage.Row{Key: usage.Key{Source: source, Provider: provider, Agent: agent, Model: model},
		Tokens: usage.Tokens{CostUSD: cost, Total: int64(cost * 1e6)}}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := hivev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The fleet's Claude store is one PVC mounted by every spoke: three sidecars
// report the same fingerprint and the pool must count it once. The window is
// anchored on Anthropic's reported reset and the limit learned from 34%.
func TestUsagePoolDedupesSharedStoreAndLearns(t *testing.T) {
	now := time.Date(2026, 9, 24, 17, 12, 32, 0, time.UTC)
	pool := &hivev1.UsagePool{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic"},
		Spec: hivev1.UsagePoolSpec{
			Provider: "anthropic",
			Windows: []hivev1.UsageWindowSpec{
				{Name: "5h", Duration: metav1.Duration{Duration: 5 * time.Hour}, ReadingSlot: "slot0"},
				{Name: "weekly", Duration: metav1.Duration{Duration: 7 * 24 * time.Hour}, ReadingSlot: "slot1"},
			},
			Reading: &hivev1.ProbeConfigMapRef{Namespace: "hive", Name: "hive-provider-usage"},
		},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "hive", Name: "hive-provider-usage"},
		Data: map[string]string{
			"anthropic":        "34% used resets=2026-09-24T17:40:00.326149+00:00",
			"anthropic_limits": `[{"slot":"slot0","percent":34,"resets_at":"2026-09-24T17:40:00.326149+00:00"},{"slot":"slot1","percent":26,"resets_at":"2026-09-30T23:00:00.326175+00:00"}]`,
			"updated_at":       "2026-09-24T17:12:32Z",
		}}
	spokes := []hivev1.HiveSpoke{
		{ObjectMeta: metav1.ObjectMeta{Name: "school"}, Spec: hivev1.HiveSpokeSpec{Namespace: "hive"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "reef"}, Spec: hivev1.HiveSpokeSpec{Namespace: "hive-reef"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hanthor"}, Spec: hivev1.HiveSpokeSpec{Namespace: "hive-hanthor"}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&hivev1.UsagePool{}).
		WithObjects(pool, cm, &spokes[0], &spokes[1], &spokes[2]).Build()

	shared := usage.SourceStatus{Source: "claude", Fingerprint: "c1a0de", OK: true, Primed: true,
		UnpricedModels: []string{"claude-opus-5-5"}}
	// Phase 0 measurements: $22.64 in the 5h window, $36.85 in the week.
	sharedRows := map[string][]usage.Row{
		"5h":     {row("claude", "anthropic", "sec-check", "claude-fable-5-1", 11.42), row("claude", "anthropic", "strategist", "claude-fable-5-1", 11.22)},
		"weekly": {row("claude", "anthropic", "sec-check", "claude-fable-5-1", 21.66), row("claude", "anthropic", "strategist", "claude-fable-5-1", 15.19)},
		"recent": {row("claude", "anthropic", "strategist", "claude-sonnet-5", 0.9)},
	}
	withNoise := func(m map[string][]usage.Row) map[string][]usage.Row {
		out := map[string][]usage.Row{}
		for k, v := range m {
			// Rows from other providers/sources must never leak into the pool.
			out[k] = append(append([]usage.Row(nil), v...), row("antigravity", "google", "scanner", "gemini-3.6-flash-low", 99))
		}
		return out
	}
	start5h := time.Date(2026, 9, 24, 12, 40, 0, 0, time.UTC)
	startWeek := time.Date(2026, 9, 23, 23, 0, 0, 0, time.UTC)
	f := &fakeFetcher{
		reports: map[string]usage.Report{
			"hive":         {Schema: 1, Sources: []usage.SourceStatus{shared}},
			"hive-reef":    {Schema: 1, Sources: []usage.SourceStatus{shared}},
			"hive-hanthor": {Schema: 1, Sources: []usage.SourceStatus{shared}},
		},
		rows: map[string]map[string][]usage.Row{"hive": withNoise(sharedRows), "hive-reef": withNoise(sharedRows), "hive-hanthor": withNoise(sharedRows)},
		starts: map[string]string{
			start5h.Format(time.RFC3339):                                   "5h",
			startWeek.Format(time.RFC3339):                                 "weekly",
			now.Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339): "recent",
		},
	}
	r := &UsagePoolReconciler{Client: c, Fetch: f, Now: func() time.Time { return now }}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "anthropic"}}); err != nil {
		t.Fatal(err)
	}
	var got hivev1.UsagePool
	if err := c.Get(context.Background(), types.NamespacedName{Name: "anthropic"}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Windows) != 2 {
		t.Fatalf("windows %+v", got.Status.Windows)
	}
	w := got.Status.Windows[0]
	if w.Consumed != "22.6400" {
		t.Fatalf("5h consumed %s — shared store counted more than once, or noise leaked", w.Consumed)
	}
	if !w.Start.Time.Equal(start5h) || w.LimitSource != "learned" || w.Limit != "66.5882" || w.UsedPercent != "34.0000" {
		t.Fatalf("5h window %+v", w)
	}
	wk := got.Status.Windows[1]
	if wk.Consumed != "36.8500" || wk.Limit != "141.7308" {
		t.Fatalf("weekly %+v", wk)
	}
	counted := 0
	for _, s := range got.Status.Sources {
		if s.Counted {
			counted++
		}
	}
	if counted != 1 || len(got.Status.Sources) != 3 {
		t.Fatalf("sources %+v", got.Status.Sources)
	}
	if len(got.Status.Agents) != 4 || got.Status.Agents[0].Agent != "sec-check" || got.Status.Agents[0].Window != "5h" {
		t.Fatalf("agents %+v", got.Status.Agents)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, "Priced"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("unpriced models must surface: %+v", got.Status.Conditions)
	}

	// Second pass with a stale reading: the learned limit is carried, used%
	// falls back to consumed/limit.
	later := now.Add(45 * time.Minute)
	r.Now = func() time.Time { return later }
	f.starts = map[string]string{
		later.Add(-5 * time.Hour).Truncate(time.Second).Format(time.RFC3339):      "5h",
		later.Add(-7 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339): "weekly",
		later.Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339):          "recent",
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "anthropic"}}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "anthropic"}, &got)
	w = got.Status.Windows[0]
	if w.ReadingPercent != "-1" || w.Limit != "66.5882" || w.UsedPercent != "34.0000" {
		t.Fatalf("stale-reading pass %+v", w)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, "Reading"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("stale reading must be flagged: %+v", got.Status.Conditions)
	}
}

func TestUsagePoolNoSidecar(t *testing.T) {
	pool := &hivev1.UsagePool{ObjectMeta: metav1.ObjectMeta{Name: "openai"},
		Spec: hivev1.UsagePoolSpec{Provider: "openai", Namespaces: []string{"hive"},
			Windows: []hivev1.UsageWindowSpec{{Name: "weekly", Duration: metav1.Duration{Duration: 7 * 24 * time.Hour}}}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&hivev1.UsagePool{}).WithObjects(pool).Build()
	r := &UsagePoolReconciler{Client: c, Fetch: &fakeFetcher{}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "openai"}}); err != nil {
		t.Fatal(err)
	}
	var got hivev1.UsagePool
	_ = c.Get(context.Background(), types.NamespacedName{Name: "openai"}, &got)
	if cnd := apiMeta.FindStatusCondition(got.Status.Conditions, "Ready"); cnd == nil || cnd.Reason != "NoSidecar" {
		t.Fatalf("conditions %+v", got.Status.Conditions)
	}
	if got.Status.Windows[0].UsedPercent != "-1.0000" || got.Status.Windows[0].LimitSource != "none" {
		t.Fatalf("no data must read unmeasured (-1), got %+v", got.Status.Windows[0])
	}
}
