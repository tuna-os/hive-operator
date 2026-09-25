package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tuna-os/ccleft"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/usage"
)

type fakeReadings struct {
	out   *usage.CcleftOutput
	err   error
	urls  []string
	edits func(*usage.CcleftOutput)
}

func (f *fakeReadings) Readings(_ context.Context, url string) (*usage.CcleftOutput, error) {
	f.urls = append(f.urls, url)
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

// liveCcleft loads the real (redacted) GET /readings captured from the Hive's
// ccleft at 2026-09-25 11:47Z.
func liveCcleft(t *testing.T) *usage.CcleftOutput {
	t.Helper()
	b, err := os.ReadFile("../usage/testdata/ccleft.readings.json")
	if err != nil {
		t.Fatal(err)
	}
	var out usage.CcleftOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

var ccleftNow = time.Date(2026, 9, 25, 11, 50, 0, 0, time.UTC)

// The bash ConfigMap as published at 11:27Z the same day.
func liveProbeCM() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "hive", Name: "hive-provider-usage"},
		Data: map[string]string{
			"anthropic":        "10% used resets=2026-09-25T15:40:00.200682+00:00",
			"anthropic_limits": `[{"slot":"slot0","percent":10,"resets_at":"2026-09-25T15:40:00.200682+00:00"},{"slot":"slot1","percent":3,"resets_at":"2026-09-30T23:00:00.200709+00:00"}]`,
			"google":           "74% used resets=2026-09-30T03:17:08Z",
			"kiro":             "37% used credits=3777.17/10000 resets=2026-10-01T00:00:00Z",
			"openai":           "100% used weekly=100% resets=2026-09-26T08:15:18Z",
			"updated_at":       "2026-09-25T11:27:19Z",
		}}
}

func reconcilePool(t *testing.T, pool *hivev1.UsagePool, rf usage.ReadingsFetcher, objs ...interface{}) hivev1.UsagePool {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&hivev1.UsagePool{}).WithObjects(pool)
	for _, o := range objs {
		b = b.WithObjects(o.(*corev1.ConfigMap))
	}
	c := b.Build()
	r := &UsagePoolReconciler{Client: c, Fetch: &fakeFetcher{}, Readings: rf, Now: func() time.Time { return ccleftNow }}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pool.Name}}); err != nil {
		t.Fatal(err)
	}
	var got hivev1.UsagePool
	if err := c.Get(context.Background(), types.NamespacedName{Name: pool.Name}, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func ccleftPool(name, provider string, cc *hivev1.CcleftRef, windows ...hivev1.UsageWindowSpec) *hivev1.UsagePool {
	return &hivev1.UsagePool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: hivev1.UsagePoolSpec{
		Provider: provider, Namespaces: []string{"hive"}, Windows: windows, Ccleft: cc,
		Reading: &hivev1.ProbeConfigMapRef{Namespace: "hive", Name: "hive-provider-usage"},
	}}
}

func win(name string, d time.Duration, slot, cw string) hivev1.UsageWindowSpec {
	return hivev1.UsageWindowSpec{Name: name, Duration: metav1.Duration{Duration: d}, ReadingSlot: slot, CcleftWindow: cw}
}

// ccleft is preferred: remaining/limit/resets come straight from the provider.
func TestUsagePoolReadsCcleft(t *testing.T) {
	rf := &fakeReadings{out: liveCcleft(t)}
	pool := ccleftPool("google", "google", &hivev1.CcleftRef{URL: "http://ccleft.hive.svc:9464", Scope: "gemini"},
		win("5h", 5*time.Hour, "", ""), win("weekly", 168*time.Hour, "", ""))
	got := reconcilePool(t, pool, rf, liveProbeCM())

	if len(rf.urls) != 1 || rf.urls[0] != "http://ccleft.hive.svc:9464" {
		t.Fatalf("ccleft urls %v", rf.urls)
	}
	a := got.Status.Account
	if a == nil || a.Source != "ccleft" || a.Provider != "agy" || a.State != "ok" || a.Account != "000000000000a003" || len(a.Homes) != 14 || a.Error != "" {
		t.Fatalf("account %+v", a)
	}
	w5, wk := got.Status.Windows[0], got.Status.Windows[1]
	if w5.ReadingSource != "ccleft" || w5.ProviderWindow == nil || w5.ProviderWindow.ID != "gemini-5h" ||
		w5.ResetsAt == nil || w5.ResetsAt.UTC().Format(time.RFC3339) != "2026-09-25T15:17:07Z" {
		t.Fatalf("5h %+v", w5)
	}
	// ccleft's reading (78.3%, 11:47Z) wins over the ConfigMap's (74%, 11:27Z).
	if wk.ReadingSource != "ccleft" || !strings.HasPrefix(wk.ReadingPercent, "78.3") || wk.UsedPercent != wk.ReadingPercent ||
		wk.ProviderWindow.ID != "gemini-weekly" || wk.ProviderWindow.Scope != "gemini" || wk.ProviderWindow.Unit != "percent" ||
		!strings.HasPrefix(wk.ProviderWindow.RemainingPercent, "21.6") || wk.ProviderWindow.Limit != "100.0000" {
		t.Fatalf("weekly %+v / %+v", wk, wk.ProviderWindow)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, "Reading"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Ccleft" {
		t.Fatalf("Reading condition %+v", c)
	}
}

// Credits in the provider's own unit (Kiro) land in providerWindow.
func TestUsagePoolCcleftCredits(t *testing.T) {
	rf := &fakeReadings{out: liveCcleft(t)}
	pool := ccleftPool("kiro", "kiro", &hivev1.CcleftRef{URL: "http://ccleft"}, win("monthly", 31*24*time.Hour, "", "plan"))
	got := reconcilePool(t, pool, rf, liveProbeCM())
	w := got.Status.Windows[0]
	if w.ReadingSource != "ccleft" || w.ProviderWindow.Unit != "credits" || w.ProviderWindow.Limit != "10000.0000" ||
		w.ProviderWindow.Remaining != "6135.3900" || w.ReadingPercent != "38.6461" || w.ResetsAt.UTC().Format(time.RFC3339) != "2026-10-01T00:00:00Z" {
		t.Fatalf("kiro %+v / %+v", w, w.ProviderWindow)
	}
}

// Claude 429 with no last-good yet: ccleft has no verdict, so every window
// falls back to the ConfigMap (and status says why).
func TestUsagePoolCcleftFallsBackOnNoVerdict(t *testing.T) {
	rf := &fakeReadings{out: liveCcleft(t)}
	pool := ccleftPool("anthropic", "anthropic", &hivev1.CcleftRef{URL: "http://ccleft"},
		win("5h", 5*time.Hour, "slot0", ""), win("weekly", 168*time.Hour, "slot1", ""))
	got := reconcilePool(t, pool, rf, liveProbeCM())
	a := got.Status.Account
	if a == nil || a.Source != "configmap" || a.State != "rate_limited" || a.Cause != "http_429" || !strings.Contains(a.Error, "no quota verdict") || a.RetryAt == nil {
		t.Fatalf("account %+v", a)
	}
	w5, wk := got.Status.Windows[0], got.Status.Windows[1]
	if w5.ReadingSource != "configmap" || w5.ReadingPercent != "10.0000" || wk.ReadingSource != "configmap" || wk.ReadingPercent != "3.0000" || w5.ProviderWindow != nil {
		t.Fatalf("fallback windows %+v %+v", w5, wk)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, "Reading"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Fallback" || !strings.Contains(c.Message, "http_429") {
		t.Fatalf("Reading condition %+v", c)
	}
}

// ccleft down: ConfigMap fallback; ccleft down AND ConfigMap stale: unknown
// (-1), never exhausted.
func TestUsagePoolCcleftUnreachable(t *testing.T) {
	rf := &fakeReadings{err: errors.New("dial tcp 10.0.0.1:9464: connect: connection refused")}
	pool := ccleftPool("openai", "openai", &hivev1.CcleftRef{URL: "http://ccleft"}, win("weekly", 168*time.Hour, "", ""))
	got := reconcilePool(t, pool, rf, liveProbeCM())
	if got.Status.Account.Source != "configmap" || !strings.Contains(got.Status.Account.Error, "ccleft unavailable") {
		t.Fatalf("account %+v", got.Status.Account)
	}
	if w := got.Status.Windows[0]; w.ReadingSource != "configmap" || w.UsedPercent != "100.0000" {
		t.Fatalf("fallback %+v", w)
	}

	stale := liveProbeCM()
	stale.Data["updated_at"] = "2026-09-25T08:00:00Z"
	got = reconcilePool(t, pool, rf, stale)
	if w := got.Status.Windows[0]; w.ReadingSource != "none" || w.UsedPercent != "-1.0000" {
		t.Fatalf("no reading anywhere must be unmeasured: %+v", w)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, "Reading"); c == nil || c.Status != metav1.ConditionFalse ||
		!strings.Contains(c.Message, "ccleft unavailable") || !strings.Contains(c.Message, "stale") {
		t.Fatalf("Reading condition %+v", c)
	}
}

// A stale last-good served during 429s is used while young enough, and
// refused (fallback) once older than maxAge.
func TestUsagePoolCcleftStaleLastGood(t *testing.T) {
	out := liveCcleft(t)
	five, seven := 90.0, 97.0
	r5 := time.Date(2026, 9, 25, 15, 40, 0, 0, time.UTC)
	r7 := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	for i := range out.Readings {
		if out.Readings[i].Provider != ccleft.Claude {
			continue
		}
		c := &out.Readings[i]
		c.State, c.Stale, c.Cause = ccleft.StateOK, true, "http_429"
		c.FetchedAt = ccleftNow.Add(-12 * time.Minute)
		c.Windows = []ccleft.Window{
			{ID: "five_hour", Kind: ccleft.KindFiveHour, Binding: true, RemainingPct: &five, ResetsAt: &r5, Unit: "percent"},
			{ID: "seven_day", Kind: ccleft.KindWeekly, Binding: true, RemainingPct: &seven, ResetsAt: &r7, Unit: "percent"},
			{ID: "seven_day_opus", Kind: ccleft.KindWeekly, Scope: "opus", RemainingPct: new(float64), ResetsAt: &r7, Unit: "percent"},
		}
	}
	rf := &fakeReadings{out: out}
	pool := ccleftPool("anthropic", "anthropic", &hivev1.CcleftRef{URL: "http://ccleft"},
		win("5h", 5*time.Hour, "slot0", ""), win("weekly", 168*time.Hour, "slot1", ""))
	got := reconcilePool(t, pool, rf, liveProbeCM())
	w5, wk := got.Status.Windows[0], got.Status.Windows[1]
	// The Opus-scoped cap at 0% left must not be picked for the account window.
	if w5.ReadingSource != "ccleft" || w5.ReadingPercent != "10.0000" || wk.ReadingPercent != "3.0000" || wk.ProviderWindow.ID != "seven_day" {
		t.Fatalf("stale last-good %+v %+v", w5, wk)
	}
	if !got.Status.Account.Stale || got.Status.Account.Source != "ccleft" {
		t.Fatalf("account %+v", got.Status.Account)
	}

	pool.Spec.Ccleft.MaxAge = &metav1.Duration{Duration: 10 * time.Minute}
	got = reconcilePool(t, pool, rf, liveProbeCM())
	if w := got.Status.Windows[0]; w.ReadingSource != "configmap" || !strings.Contains(got.Status.Account.Error, "older than 10m0s") {
		t.Fatalf("over-age last-good must fall back: %+v %+v", w, got.Status.Account)
	}
}

// No spec.ccleft: behaviour is exactly the ConfigMap path, status.account nil.
func TestUsagePoolWithoutCcleftUnchanged(t *testing.T) {
	pool := ccleftPool("openai", "openai", nil, win("weekly", 168*time.Hour, "", ""))
	got := reconcilePool(t, pool, nil, liveProbeCM())
	if got.Status.Account != nil || got.Status.Windows[0].ReadingSource != "configmap" {
		t.Fatalf("%+v %+v", got.Status.Account, got.Status.Windows[0])
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, "Reading"); c == nil || c.Reason != "Fresh" {
		t.Fatalf("Reading condition %+v", c)
	}
}
