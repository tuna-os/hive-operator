package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

// TestB2F pins the gauge encoding: metrics consumers alert on == 0, so an
// accidental inversion here would silently invert every alert.
func TestB2F(t *testing.T) {
	if b2f(true) != 1 || b2f(false) != 0 {
		t.Fatalf("b2f encoding changed: true=%v false=%v", b2f(true), b2f(false))
	}
}

// recExec records what each spoke exec was asked to do and answers like a
// healthy fleet whose reef .claude.json has theme:null.
type recExec struct{ spokeArgs [][]string }

func (r *recExec) Pod(_ context.Context, ns string) (string, error) { return "hive-" + ns, nil }
func (r *recExec) Exec(_ context.Context, ns, _ string, argv []string) (string, string, error) {
	if len(argv) > 3 {
		r.spokeArgs = append(r.spokeArgs, argv)
		out := "token ok\n"
		if ns == "hive-reef" {
			if argv[5] == "reconcile" {
				out += "theme repaired\n"
			} else {
				out += "theme unset\n"
			}
		}
		if argv[7] == "reconcile" {
			out += "perms repaired\n"
		}
		return out, "", nil
	}
	if strings.Contains(argv[2], "$(cat ") {
		// echo back the stamp the write used: it is the marker suffix.
		i := strings.Index(argv[2], ".shared-auth-probe.")
		stamp := strings.Trim(strings.Fields(argv[2][i+len(".shared-auth-probe."):])[0], "';")
		return ".claude " + stamp + "\n.gemini " + stamp + "\n.codex " + stamp + "\n", "", nil
	}
	return "", "", nil
}

func sharedAuthFixture(mode hivev1.ReconcileMode) *hivev1.SharedAuth {
	return &hivev1.SharedAuth{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: hivev1.SharedAuthSpec{
			Namespaces: []string{"hive", "hive-reef", "hive-hanthor"}, PrimaryNamespace: "hive",
			Mode: mode, RepairPermissions: true, RepairTheme: true, RepairAgyStatusLine: true,
		},
	}
}

func incumbentCJ(suspend bool) *batchv1.CronJob {
	return &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "hive", Name: "hive-shared-auth"},
		Spec: batchv1.CronJobSpec{Schedule: "*/30 * * * *", Suspend: &suspend}}
}

func runSharedAuth(t *testing.T, sa *hivev1.SharedAuth, objs ...client.Object) (*hivev1.SharedAuth, *recExec) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&hivev1.SharedAuth{}).
		WithObjects(append(objs, sa)...).Build()
	ex := &recExec{}
	r := &SharedAuthReconciler{Client: c, Exec: ex}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: sa.Name}}); err != nil {
		t.Fatal(err)
	}
	var got hivev1.SharedAuth
	if err := c.Get(context.Background(), types.NamespacedName{Name: sa.Name}, &got); err != nil {
		t.Fatal(err)
	}
	return &got, ex
}

func repaired(ex *recExec) bool {
	for _, a := range ex.spokeArgs {
		for _, v := range a[5:8] {
			if v == "reconcile" {
				return true
			}
		}
	}
	return false
}

// Enforce never repairs while the bash CronJob is still scheduled — two
// writers on the same shared files is worse than either one.
func TestSharedAuthEnforceInterlockedOnIncumbent(t *testing.T) {
	got, ex := runSharedAuth(t, sharedAuthFixture(hivev1.ModeEnforce), incumbentCJ(false))
	if repaired(ex) {
		t.Fatal("repaired while hive-shared-auth is not suspended")
	}
	if got.Status.EffectiveMode != hivev1.ModeShadow {
		t.Errorf("effectiveMode %q, want Shadow", got.Status.EffectiveMode)
	}
	var inc *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "IncumbentActive" {
			inc = &got.Status.Conditions[i]
		}
	}
	if inc == nil || inc.Status != metav1.ConditionTrue {
		t.Fatalf("IncumbentActive not raised: %+v", got.Status.Conditions)
	}
	if !strings.Contains(strings.Join(got.Status.PendingRepairs, "\n"), "hive-reef: set .claude.json theme") {
		t.Errorf("theme repair not reported as pending: %v", got.Status.PendingRepairs)
	}
}

func TestSharedAuthEnforceRepairsOnceIncumbentSuspended(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"suspended": {incumbentCJ(true)},
		"deleted":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			got, ex := runSharedAuth(t, sharedAuthFixture(hivev1.ModeEnforce), objs...)
			if !repaired(ex) {
				t.Fatal("Enforce did not repair")
			}
			if got.Status.EffectiveMode != hivev1.ModeEnforce || !got.Status.Consistent {
				t.Errorf("effective %q consistent %v; report:\n%s", got.Status.EffectiveMode, got.Status.Consistent,
					strings.Join(got.Status.Report, "\n"))
			}
			want := "theme      hive-reef      theme was unset -> set to dark"
			if !strings.Contains(strings.Join(got.Status.Report, "\n"), want) {
				t.Errorf("report lacks %q:\n%s", want, strings.Join(got.Status.Report, "\n"))
			}
		})
	}
}

func TestSharedAuthShadowNeverRepairs(t *testing.T) {
	got, ex := runSharedAuth(t, sharedAuthFixture(hivev1.ModeShadow))
	if repaired(ex) {
		t.Fatal("Shadow repaired")
	}
	if got.Status.Consistent {
		t.Error("theme unset is exit 1 in bash check mode")
	}
	if len(got.Status.Report) == 0 || got.Status.Report[len(got.Status.Report)-1] != "shared-auth: PROBLEMS ABOVE" {
		t.Errorf("report: %v", got.Status.Report)
	}
}
