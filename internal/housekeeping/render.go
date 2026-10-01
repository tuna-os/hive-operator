// Package housekeeping renders the hive-ops CronJobs from a HiveHousekeeping
// and diffs a render against the live object.
//
// The render is fully DEFAULTED — it spells out every field the API server
// would otherwise fill in (dnsPolicy, schedulerName, terminationMessagePath,
// the deprecated serviceAccount mirror, …) — so that "render == live" is a
// plain structural comparison and a CronJob that has not drifted produces no
// update at all. An update the server would immediately normalise back would
// otherwise be a write on every reconcile.
package housekeeping

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

// OwnerLabel marks a CronJob as rendered by a HiveHousekeeping (value: its
// name). Added on adoption together with the ownerReference.
const OwnerLabel = "hive.tunaos.org/housekeeping"

// Defaults: the shape every live hive-ops job had on 2026-10-01.
const (
	DefaultNamespace = "hive"
	DefaultScripts   = "hive-ops-scripts"
	DefaultImage     = "alpine/k8s:1.37.1"
	DefaultSA        = "hive-ops"
	DefaultState     = "hive-ops-state"
)

// Component is the app.kubernetes.io/component of a job.
func Component(j hivev1.HousekeepingJob) string {
	if j.Component != "" {
		return j.Component
	}
	return strings.TrimPrefix(j.Name, "hive-")
}

// Namespace is the spec namespace, defaulted.
func Namespace(hk *hivev1.HiveHousekeeping) string {
	if hk.Spec.Namespace != "" {
		return hk.Spec.Namespace
	}
	return DefaultNamespace
}

// ScriptsConfigMap is the spec scripts ConfigMap, defaulted.
func ScriptsConfigMap(hk *hivev1.HiveHousekeeping) string {
	if hk.Spec.ScriptsConfigMap != "" {
		return hk.Spec.ScriptsConfigMap
	}
	return DefaultScripts
}

func or(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

func i64(v int64) *int64 { return &v }
func i32(v int32) *int32 { return &v }
func bp(v bool) *bool    { return &v }

func firstI64(vs ...*int64) *int64 {
	for _, v := range vs {
		if v != nil {
			x := *v
			return &x
		}
	}
	return nil
}

func firstI32(vs ...*int32) *int32 {
	for _, v := range vs {
		if v != nil {
			x := *v
			return &x
		}
	}
	return nil
}

// Labels is what the CronJob and its pod template carry.
func Labels(hk *hivev1.HiveHousekeeping, j hivev1.HousekeepingJob) map[string]string {
	l := map[string]string{"app.kubernetes.io/name": "hive-ops"}
	for k, v := range hk.Spec.Template.Labels {
		l[k] = v
	}
	l["app.kubernetes.io/component"] = Component(j)
	return l
}

// Render builds one job's CronJob (metadata name/namespace/labels + spec).
func Render(hk *hivev1.HiveHousekeeping, j hivev1.HousekeepingJob) *batchv1.CronJob {
	t := hk.Spec.Template
	sa := or(t.ServiceAccountName, DefaultSA)
	env := t.Env
	if j.Env != nil {
		env = j.Env
	}
	res := t.Resources
	if j.Resources != nil {
		res = *j.Resources
	}
	cp := batchv1.ConcurrencyPolicy(or(t.ConcurrencyPolicy, string(batchv1.ForbidConcurrent)))
	labels := Labels(hk, j)
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	cmd := append([]string{"bash", "/scripts/" + j.Script}, j.Args...)
	pull := t.ImagePullPolicy
	if pull == "" {
		pull = corev1.PullIfNotPresent
	}

	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name: j.Name, Namespace: Namespace(hk), Labels: labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   j.Schedule,
			TimeZone:                   j.TimeZone,
			StartingDeadlineSeconds:    firstI64(j.StartingDeadlineSeconds, t.StartingDeadlineSeconds),
			ConcurrencyPolicy:          cp,
			Suspend:                    bp(j.Suspend),
			SuccessfulJobsHistoryLimit: firstI32(t.SuccessfulJobsHistoryLimit, i32(3)),
			FailedJobsHistoryLimit:     firstI32(t.FailedJobsHistoryLimit, i32(1)),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: firstI64(j.ActiveDeadlineSeconds, t.ActiveDeadlineSeconds),
					BackoffLimit:          firstI32(t.BackoffLimit, i32(6)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:            "ops",
								Image:           or(t.Image, DefaultImage),
								ImagePullPolicy: pull,
								Command:         cmd,
								Env:             env,
								Resources:       res,
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: bp(false),
									Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
									ReadOnlyRootFilesystem:   bp(true),
								},
								TerminationMessagePath:   corev1.TerminationMessagePathDefault,
								TerminationMessagePolicy: corev1.TerminationMessageReadFile,
								VolumeMounts: []corev1.VolumeMount{
									{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
									{Name: "state", MountPath: "/state"},
									{Name: "tmp", MountPath: "/tmp"},
								},
							}},
							DNSPolicy:     corev1.DNSClusterFirst,
							NodeSelector:  t.NodeSelector,
							RestartPolicy: corev1.RestartPolicyNever,
							SchedulerName: corev1.DefaultSchedulerName,
							SecurityContext: &corev1.PodSecurityContext{
								FSGroup: i64(1000), RunAsGroup: i64(1000), RunAsNonRoot: bp(true), RunAsUser: i64(1000),
								SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
							},
							DeprecatedServiceAccount:      sa,
							ServiceAccountName:            sa,
							TerminationGracePeriodSeconds: i64(corev1.DefaultTerminationGracePeriodSeconds),
							Tolerations:                   t.Tolerations,
							Volumes: []corev1.Volume{
								{Name: "scripts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: ScriptsConfigMap(hk)},
									DefaultMode:          func() *int32 { m := int32(0o755); return &m }(),
								}}},
								{Name: "state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: or(t.StateClaimName, DefaultState),
								}}},
								{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
							},
						},
					},
				},
			},
		},
	}
}

// Diff lists the paths where live differs from the render: the spec in full,
// and the render's labels (extra live labels are not drift). Empty = in sync.
func Diff(want, live *batchv1.CronJob) []string {
	var out []string
	for k, v := range want.Labels {
		if live.Labels[k] != v {
			out = append(out, "metadata.labels."+k)
		}
	}
	sort.Strings(out)
	a, b := toMap(want.Spec), toMap(live.Spec)
	diffPaths("spec", a, b, &out)
	return out
}

func toMap(v any) any {
	raw, _ := json.Marshal(v)
	var m any
	_ = json.Unmarshal(raw, &m)
	return m
}

// diffPaths walks two JSON trees. Lists are compared whole, except
// containers/volumes/env (by index, to name the element that moved).
func diffPaths(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			diffPaths(path+"."+k, am[k], bm[k], out)
		}
		return
	}
	al, alok := a.([]any)
	bl, blok := b.([]any)
	if alok && blok && len(al) == len(bl) && (strings.HasSuffix(path, ".containers")) {
		for i := range al {
			diffPaths(fmt.Sprintf("%s[%d]", path, i), al[i], bl[i], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}

// ScriptHash is the first 12 hex of the script's SHA-256.
func ScriptHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}
