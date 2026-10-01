package housekeeping

import (
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/yaml"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

func loadSample(t *testing.T) *hivev1.HiveHousekeeping {
	t.Helper()
	b, err := os.ReadFile("../../config/samples/housekeeping.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var hk hivev1.HiveHousekeeping
	if err := yaml.UnmarshalStrict(b, &hk); err != nil {
		t.Fatal(err)
	}
	return &hk
}

func loadLive(t *testing.T) map[string]*batchv1.CronJob {
	t.Helper()
	b, err := os.ReadFile("testdata/live-cronjobs-20261001T1915.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var l batchv1.CronJobList
	if err := yaml.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	out := map[string]*batchv1.CronJob{}
	for i := range l.Items {
		out[l.Items[i].Name] = &l.Items[i]
	}
	return out
}

// TestHousekeepingSampleRendersLive: the sample IS the live fleet. Every job
// renders to exactly the live CronJob (the snapshot of 2026-10-01T19:15Z),
// so adopting it in Enforce changes nothing but metadata.
func TestHousekeepingSampleRendersLive(t *testing.T) {
	hk := loadSample(t)
	live := loadLive(t)
	// The sample records what is deployed: Enforce since 2026-10-01 (#50).
	if hk.Spec.Mode != hivev1.ModeEnforce {
		t.Errorf("sample mode %q, want Enforce (the deployed state)", hk.Spec.Mode)
	}
	want := []string{"hive-shared-auth", "hive-tiers", "hive-inventory", "hive-pi-kiro",
		"hive-cli-update", "hive-repo-sync", "hive-metrics", "hive-activity"}
	var got []string
	for _, j := range hk.Spec.Jobs {
		got = append(got, j.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("jobs %v, want %v", got, want)
	}
	for _, j := range hk.Spec.Jobs {
		l, ok := live[j.Name]
		if !ok {
			t.Errorf("%s: not in the live snapshot", j.Name)
			continue
		}
		r := Render(hk, j)
		if d := Diff(r, l); len(d) > 0 {
			ry, _ := yaml.Marshal(r.Spec)
			ly, _ := yaml.Marshal(l.Spec)
			t.Errorf("%s drifts from live at %v\n--- render\n%s--- live\n%s", j.Name, d, ry, ly)
		}
		if l.Namespace != "hive" {
			t.Errorf("%s: namespace %s", j.Name, l.Namespace)
		}
	}
	// school's jobs are someone else's promotion: the sample must not list them.
	for _, j := range hk.Spec.Jobs {
		switch j.Name {
		case "hive-rotate", "hive-watchdog", "hive-pace", "hive-nudge":
			t.Errorf("%s must not be housekept: it belongs to HiveSpoke school's promotion", j.Name)
		}
	}
}

// TestDiffNamesTheDriftedField: a hand edit is reported where it happened.
func TestDiffNamesTheDriftedField(t *testing.T) {
	hk := loadSample(t)
	live := loadLive(t)
	var tiers hivev1.HousekeepingJob
	for _, j := range hk.Spec.Jobs {
		if j.Name == "hive-tiers" {
			tiers = j
		}
	}
	l := live["hive-tiers"].DeepCopy()
	l.Spec.Schedule = "0 6 * * *"
	l.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = "alpine/k8s:1.38.0"
	l.Labels["app.kubernetes.io/component"] = "x"
	l.Labels["extra"] = "fine"
	d := Diff(Render(hk, tiers), l)
	want := []string{
		"metadata.labels.app.kubernetes.io/component",
		"spec.jobTemplate.spec.template.spec.containers[0].image",
		"spec.schedule",
	}
	if strings.Join(d, "\n") != strings.Join(want, "\n") {
		t.Fatalf("diff:\n%s\nwant:\n%s", strings.Join(d, "\n"), strings.Join(want, "\n"))
	}
}

// An omitted template falls back to the live defaults, not to Go zero values.
func TestRenderDefaults(t *testing.T) {
	hk := &hivev1.HiveHousekeeping{}
	cj := Render(hk, hivev1.HousekeepingJob{Name: "hive-x", Schedule: "* * * * *", Script: "x.sh"})
	ps := cj.Spec.JobTemplate.Spec.Template.Spec
	if cj.Namespace != "hive" || ps.Containers[0].Image != DefaultImage || ps.ServiceAccountName != "hive-ops" ||
		ps.Volumes[0].ConfigMap.Name != "hive-ops-scripts" || ps.Volumes[1].PersistentVolumeClaim.ClaimName != "hive-ops-state" ||
		cj.Labels["app.kubernetes.io/component"] != "x" || cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Fatalf("defaults: %+v", cj)
	}
	if got := strings.Join(ps.Containers[0].Command, " "); got != "bash /scripts/x.sh" {
		t.Errorf("command %q", got)
	}
}
