package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/housekeeping"
)

func hkSample(t *testing.T, mode hivev1.ReconcileMode) *hivev1.HiveHousekeeping {
	t.Helper()
	b, err := os.ReadFile("../../config/samples/housekeeping.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var hk hivev1.HiveHousekeeping
	if err := yaml.UnmarshalStrict(b, &hk); err != nil {
		t.Fatal(err)
	}
	hk.UID = "hk-uid"
	hk.Spec.Mode = mode
	return &hk
}

// liveCronJobs is the 2026-10-01T18:00Z snapshot of ns hive, with UIDs.
func liveCronJobs(t *testing.T) []client.Object {
	t.Helper()
	b, err := os.ReadFile("../housekeeping/testdata/live-cronjobs-20261001T1800.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var l batchv1.CronJobList
	if err := yaml.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	var out []client.Object
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out
}

func scriptsCM() *corev1.ConfigMap {
	d := map[string]string{}
	for _, s := range []string{"hive-shared-auth.sh", "hive-tiers.sh", "hive-inventory.sh", "hive-pi-kiro.sh",
		"hive-cli-update.sh", "hive-repo-sync.sh", "hive-metrics.sh", "hive-activity.sh"} {
		d[s] = "#!/bin/bash\n# " + s
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "hive", Name: "hive-ops-scripts"}, Data: d}
}

func runHK(t *testing.T, c client.Client, name string) *hivev1.HiveHousekeeping {
	t.Helper()
	r := &HiveHousekeepingReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatal(err)
	}
	var got hivev1.HiveHousekeeping
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func hkClient(t *testing.T, hk *hivev1.HiveHousekeeping, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&hivev1.HiveHousekeeping{}).
		WithObjects(append(append(objs, hk, scriptsCM()), liveCronJobs(t)...)...).Build()
}

func getCJ(t *testing.T, c client.Client, name string) *batchv1.CronJob {
	t.Helper()
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "hive", Name: name}, &cj); err != nil {
		t.Fatal(err)
	}
	return &cj
}

// Shadow against the live fleet: every job is Unadopted with ZERO drift, and
// nothing is written.
func TestHousekeepingShadowOnLiveFleet(t *testing.T) {
	hk := hkSample(t, hivev1.ModeShadow)
	c := hkClient(t, hk)
	before := getCJ(t, c, "hive-tiers").ResourceVersion
	got := runHK(t, c, "fleet")
	if len(got.Status.Jobs) != 8 {
		t.Fatalf("jobs: %+v", got.Status.Jobs)
	}
	for _, j := range got.Status.Jobs {
		if j.State != hkUnadopted || len(j.Drift) != 0 || j.Owned {
			t.Errorf("%s: state %s drift %v owned %v", j.Name, j.State, j.Drift, j.Owned)
		}
		if j.Script == "missing" || len(j.Script) != 12 {
			t.Errorf("%s: script hash %q", j.Name, j.Script)
		}
	}
	if getCJ(t, c, "hive-tiers").ResourceVersion != before {
		t.Fatal("Shadow wrote a CronJob")
	}
	if len(got.Status.PendingActions) != 8 {
		t.Errorf("pending: %v", got.Status.PendingActions)
	}
}

// Enforce adopts IN PLACE (same object, so the Jobs it owns stay attached),
// leaves the spec byte-identical when it already matches, then reverts a
// hand edit and recreates a deleted job.
func TestHousekeepingEnforceAdoptsInPlaceAndRevertsDrift(t *testing.T) {
	hk := hkSample(t, hivev1.ModeEnforce)
	c := hkClient(t, hk)
	pre := getCJ(t, c, "hive-metrics")
	got := runHK(t, c, "fleet")
	for _, j := range got.Status.Jobs {
		if j.State != hkInSync || !j.Owned || !strings.HasPrefix(j.Action, "adopted") {
			t.Errorf("%s: %+v", j.Name, j)
		}
	}
	post := getCJ(t, c, "hive-metrics")
	if post.UID != pre.UID {
		t.Fatal("adoption replaced the object")
	}
	if post.Labels[housekeeping.OwnerLabel] != "fleet" || len(post.OwnerReferences) != 1 ||
		post.OwnerReferences[0].UID != "hk-uid" || post.OwnerReferences[0].Kind != "HiveHousekeeping" {
		t.Fatalf("not adopted: labels %v owners %+v", post.Labels, post.OwnerReferences)
	}
	pj, _ := yaml.Marshal(pre.Spec)
	qj, _ := yaml.Marshal(post.Spec)
	if string(pj) != string(qj) {
		t.Fatalf("adoption changed the spec:\n%s\n---\n%s", pj, qj)
	}
	// Untouched: school's jobs are not listed.
	if rot := getCJ(t, c, "hive-rotate"); len(rot.OwnerReferences) != 0 {
		t.Fatal("touched hive-rotate")
	}

	// A hand edit is reverted.
	post.Spec.Schedule = "0 * * * *"
	if err := c.Update(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	got = runHK(t, c, "fleet")
	if s := getCJ(t, c, "hive-metrics").Spec.Schedule; s != "23 * * * *" {
		t.Fatalf("drift not reverted: %s", s)
	}
	for _, j := range got.Status.Jobs {
		if j.Name == "hive-metrics" && !strings.Contains(j.Action, "spec.schedule") {
			t.Errorf("action does not name the field: %q", j.Action)
		}
	}

	// A deleted job is recreated, owned.
	if err := c.Delete(context.Background(), getCJ(t, c, "hive-activity")); err != nil {
		t.Fatal(err)
	}
	runHK(t, c, "fleet")
	if a := getCJ(t, c, "hive-activity"); a.Labels[housekeeping.OwnerLabel] != "fleet" || a.Spec.Schedule != "*/5 * * * *" {
		t.Fatalf("not recreated: %+v", a.ObjectMeta)
	}

	// Unlisting an owned job prunes it.
	var fresh hivev1.HiveHousekeeping
	_ = c.Get(context.Background(), types.NamespacedName{Name: "fleet"}, &fresh)
	fresh.Spec.Jobs = fresh.Spec.Jobs[:len(fresh.Spec.Jobs)-1] // drop hive-activity
	if err := c.Update(context.Background(), &fresh); err != nil {
		t.Fatal(err)
	}
	runHK(t, c, "fleet")
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "hive", Name: "hive-activity"}, &cj); err == nil {
		t.Fatal("owned, unlisted job not pruned")
	}
}

// Per-job mode: promote one job at a time.
func TestHousekeepingPerJobMode(t *testing.T) {
	hk := hkSample(t, hivev1.ModeShadow)
	hk.Spec.Jobs[1].Mode = hivev1.ModeEnforce // hive-tiers
	c := hkClient(t, hk)
	runHK(t, c, "fleet")
	if len(getCJ(t, c, "hive-tiers").OwnerReferences) != 1 {
		t.Error("hive-tiers (Enforce) not adopted")
	}
	if len(getCJ(t, c, "hive-inventory").OwnerReferences) != 0 {
		t.Error("hive-inventory (Shadow) adopted")
	}
}

// Retirement is gated: never an unsuspended job, never in Shadow.
func TestHousekeepingRetireGated(t *testing.T) {
	hk := hkSample(t, hivev1.ModeEnforce)
	hk.Spec.Retire = []hivev1.RetiredCronJob{
		{Name: "hive-rotate-hanthor"}, // suspended in the snapshot
		{Name: "hive-rotate"},         // NOT suspended
		{Name: "hive-gone"},
	}
	c := hkClient(t, hk)
	got := runHK(t, c, "fleet")
	states := map[string]string{}
	for _, r := range got.Status.Retired {
		states[r.Name] = r.State
	}
	if states["hive-rotate-hanthor"] != "Deleted" || states["hive-rotate"] != "Blocked" || states["hive-gone"] != "Gone" {
		t.Fatalf("retire states %v", states)
	}
	getCJ(t, c, "hive-rotate") // still there

	hk2 := hkSample(t, hivev1.ModeShadow)
	hk2.Spec.Retire = []hivev1.RetiredCronJob{{Name: "hive-watchdog-reef"}}
	c2 := hkClient(t, hk2)
	got = runHK(t, c2, "fleet")
	if got.Status.Retired[0].State != "WouldDelete" {
		t.Fatalf("shadow retire: %+v", got.Status.Retired)
	}
	getCJ(t, c2, "hive-watchdog-reef")
}

// A CronJob another controller owns is never taken over.
func TestHousekeepingLeavesForeignControllerAlone(t *testing.T) {
	hk := hkSample(t, hivev1.ModeEnforce)
	c := hkClient(t, hk)
	cj := getCJ(t, c, "hive-tiers")
	tr := true
	cj.OwnerReferences = []metav1.OwnerReference{{APIVersion: "x/v1", Kind: "Other", Name: "o", UID: "other", Controller: &tr}}
	if err := c.Update(context.Background(), cj); err != nil {
		t.Fatal(err)
	}
	got := runHK(t, c, "fleet")
	for _, j := range got.Status.Jobs {
		if j.Name == "hive-tiers" && j.State != hkConflict {
			t.Fatalf("hive-tiers: %+v", j)
		}
	}
	if o := getCJ(t, c, "hive-tiers").OwnerReferences; len(o) != 1 || o[0].UID != "other" {
		t.Fatalf("owners changed: %+v", o)
	}
}
