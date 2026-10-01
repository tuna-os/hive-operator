package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/housekeeping"
	"github.com/tuna-os/hive-operator/internal/metrics"
)

const housekeepingController = "housekeeping"

// Job states in status.
const (
	hkInSync    = "InSync"
	hkDrifted   = "Drifted"
	hkMissing   = "Missing"
	hkUnadopted = "Unadopted"
	hkConflict  = "Conflict"
	hkPruned    = "Pruned"
)

// HiveHousekeepingReconciler owns the hive-ops CronJobs that stay shell.
type HiveHousekeepingReconciler struct {
	client.Client
	Interval time.Duration
}

// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivehousekeepings,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivehousekeepings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile renders, diffs and — in Enforce — adopts, converges and retires.
func (r *HiveHousekeepingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	var hk hivev1.HiveHousekeeping
	if err := r.Get(ctx, req.NamespacedName, &hk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	mode := hk.Spec.Mode
	if mode == "" {
		mode = hivev1.ModeShadow
	}
	metrics.SetMode(housekeepingController, hk.Name, string(mode))
	ns := housekeeping.Namespace(&hk)

	scripts := map[string]string{}
	var cm corev1.ConfigMap
	scriptsErr := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: housekeeping.ScriptsConfigMap(&hk)}, &cm)
	if scriptsErr == nil {
		scripts = cm.Data
	}

	st := hivev1.HiveHousekeepingStatus{}
	var errs []string
	listed := map[string]bool{}
	for _, j := range hk.Spec.Jobs {
		listed[j.Name] = true
		jm := mode
		if j.Mode != "" {
			jm = j.Mode
		}
		js, err := r.reconcileJob(ctx, &hk, j, jm)
		if err != nil {
			lg.Error(err, "job", "cronjob", j.Name)
			errs = append(errs, fmt.Sprintf("%s: %v", j.Name, err))
		}
		if s, ok := scripts[j.Script]; ok {
			js.Script = housekeeping.ScriptHash(s)
		} else {
			js.Script = "missing"
			errs = append(errs, fmt.Sprintf("%s: script %s not in ConfigMap %s/%s", j.Name, j.Script, ns, housekeeping.ScriptsConfigMap(&hk)))
		}
		if js.Action != "" && js.State != hkInSync {
			st.PendingActions = append(st.PendingActions, fmt.Sprintf("%s: %s", j.Name, js.Action))
		}
		st.Jobs = append(st.Jobs, js)
	}

	// Prune: a CronJob THIS object owns that is no longer listed.
	var all batchv1.CronJobList
	if err := r.List(ctx, &all, client.InNamespace(ns), client.MatchingLabels{housekeeping.OwnerLabel: hk.Name}); err != nil {
		errs = append(errs, "list: "+err.Error())
	}
	for i := range all.Items {
		cj := &all.Items[i]
		if listed[cj.Name] || !ownedBy(cj, &hk) {
			continue
		}
		js := hivev1.HousekeepingJobStatus{Name: cj.Name, State: hkPruned, Owned: true}
		if mode == hivev1.ModeEnforce {
			if err := r.Delete(ctx, cj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Sprintf("%s: prune: %v", cj.Name, err))
			}
			js.Action = "deleted (no longer listed)"
			metrics.Action(housekeepingController, hk.Name, "prune", true)
		} else {
			js.Action = "would delete (owned, no longer listed)"
			st.PendingActions = append(st.PendingActions, cj.Name+": "+js.Action)
			metrics.Action(housekeepingController, hk.Name, "prune", false)
		}
		st.Jobs = append(st.Jobs, js)
	}

	for _, rt := range hk.Spec.Retire {
		rs := r.retire(ctx, &hk, ns, rt, mode)
		if rs.State == "WouldDelete" {
			st.PendingActions = append(st.PendingActions, rt.Name+": would delete (retired)")
		}
		st.Retired = append(st.Retired, rs)
	}

	now := metav1.Now()
	st.ObservedAt = &now
	cond := metav1.Condition{Type: "InSync", Status: metav1.ConditionTrue, Reason: "Rendered",
		Message: "every listed CronJob matches its render", LastTransitionTime: now}
	var out []string
	for _, j := range st.Jobs {
		if j.State != hkInSync {
			out = append(out, j.Name+"="+j.State)
		}
	}
	if len(out) > 0 {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NotInSync", strings.Join(out, ", ")
	}
	if scriptsErr != nil {
		errs = append(errs, "scripts ConfigMap: "+scriptsErr.Error())
	}
	st.Conditions = []metav1.Condition{cond}
	if len(errs) > 0 {
		st.Conditions = append(st.Conditions, metav1.Condition{Type: "Degraded", Status: metav1.ConditionTrue,
			Reason: "Errors", Message: strings.Join(errs, "; "), LastTransitionTime: now})
		metrics.ReconcileErrors.WithLabelValues(housekeepingController, hk.Name).Inc()
	}
	hk.Status = st
	if err := r.Status().Update(ctx, &hk); err != nil {
		return ctrl.Result{}, err
	}
	iv := r.Interval
	if iv == 0 {
		iv = 10 * time.Minute
	}
	return ctrl.Result{RequeueAfter: iv}, nil
}

func ownedBy(cj *batchv1.CronJob, hk *hivev1.HiveHousekeeping) bool {
	for _, o := range cj.OwnerReferences {
		if o.UID == hk.UID {
			return true
		}
	}
	return false
}

// foreignController is another controller's ownerReference, if any.
func foreignController(cj *batchv1.CronJob, hk *hivev1.HiveHousekeeping) *metav1.OwnerReference {
	for i, o := range cj.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.UID != hk.UID {
			return &cj.OwnerReferences[i]
		}
	}
	return nil
}

func (r *HiveHousekeepingReconciler) reconcileJob(ctx context.Context, hk *hivev1.HiveHousekeeping, j hivev1.HousekeepingJob, mode hivev1.ReconcileMode) (hivev1.HousekeepingJobStatus, error) {
	js := hivev1.HousekeepingJobStatus{Name: j.Name}
	want := housekeeping.Render(hk, j)
	var live batchv1.CronJob
	err := r.Get(ctx, types.NamespacedName{Namespace: want.Namespace, Name: want.Name}, &live)
	if apierrors.IsNotFound(err) {
		js.State = hkMissing
		if mode != hivev1.ModeEnforce {
			js.Action = "would create"
			metrics.Action(housekeepingController, hk.Name, "create", false)
			return js, nil
		}
		want.Labels[housekeeping.OwnerLabel] = hk.Name
		if err := controllerutil.SetControllerReference(hk, want, r.Scheme()); err != nil {
			return js, err
		}
		if err := r.Create(ctx, want); err != nil {
			return js, err
		}
		metrics.Action(housekeepingController, hk.Name, "create", true)
		js.State, js.Owned, js.Action = hkInSync, true, "created"
		js.Suspended = j.Suspend
		return js, nil
	}
	if err != nil {
		js.State = "Unknown"
		return js, err
	}

	js.Owned = ownedBy(&live, hk)
	js.Suspended = live.Spec.Suspend != nil && *live.Spec.Suspend
	js.LastScheduleTime = live.Status.LastScheduleTime
	js.LastSuccessfulTime = live.Status.LastSuccessfulTime
	if mode == hivev1.ModeObserve {
		js.State = "Observed"
		return js, nil
	}
	if fc := foreignController(&live, hk); fc != nil {
		js.State = hkConflict
		js.Action = fmt.Sprintf("left alone: controlled by %s/%s", fc.Kind, fc.Name)
		return js, nil
	}
	js.Drift = housekeeping.Diff(want, &live)
	adopted := js.Owned && live.Labels[housekeeping.OwnerLabel] == hk.Name

	switch {
	case !adopted && len(js.Drift) > 0:
		js.State = hkUnadopted
		js.Action = fmt.Sprintf("would adopt (ownerReference + label) and revert drift in %d field(s)", len(js.Drift))
	case !adopted:
		js.State = hkUnadopted
		js.Action = "would adopt (ownerReference + label; spec already matches — Job history kept)"
	case len(js.Drift) > 0:
		js.State = hkDrifted
		js.Action = fmt.Sprintf("would revert drift in %d field(s)", len(js.Drift))
	default:
		js.State = hkInSync
		return js, nil
	}
	if mode != hivev1.ModeEnforce {
		metrics.Action(housekeepingController, hk.Name, "converge", false)
		return js, nil
	}

	// Adopt in place: metadata only, same object (UID), so the Jobs it owns —
	// the history — stay attached. Then converge the spec if it drifted.
	upd := live.DeepCopy()
	if upd.Labels == nil {
		upd.Labels = map[string]string{}
	}
	for k, v := range want.Labels {
		upd.Labels[k] = v
	}
	upd.Labels[housekeeping.OwnerLabel] = hk.Name
	if err := controllerutil.SetControllerReference(hk, upd, r.Scheme()); err != nil {
		return js, err
	}
	if len(js.Drift) > 0 {
		upd.Spec = want.Spec
	}
	if err := r.Update(ctx, upd); err != nil {
		return js, err
	}
	metrics.Action(housekeepingController, hk.Name, "converge", true)
	if !adopted {
		js.Action = "adopted"
	} else {
		js.Action = "reverted drift"
	}
	if len(js.Drift) > 0 {
		js.Action += fmt.Sprintf(" (%s)", strings.Join(js.Drift, ", "))
	}
	js.State, js.Owned = hkInSync, true
	js.Suspended = j.Suspend
	return js, nil
}

// retire deletes a legacy CronJob — only if it is suspended, has no active
// Job, and is not one this object renders.
func (r *HiveHousekeepingReconciler) retire(ctx context.Context, hk *hivev1.HiveHousekeeping, ns string, rt hivev1.RetiredCronJob, mode hivev1.ReconcileMode) hivev1.RetiredCronJobStatus {
	rs := hivev1.RetiredCronJobStatus{Name: rt.Name, Message: rt.Reason}
	for _, j := range hk.Spec.Jobs {
		if j.Name == rt.Name {
			rs.State, rs.Message = "Blocked", "also listed in spec.jobs"
			return rs
		}
	}
	var cj batchv1.CronJob
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: rt.Name}, &cj)
	switch {
	case apierrors.IsNotFound(err):
		rs.State = "Gone"
		return rs
	case err != nil:
		rs.State, rs.Message = "Blocked", err.Error()
		return rs
	case cj.Spec.Suspend == nil || !*cj.Spec.Suspend:
		rs.State, rs.Message = "Blocked", "not suspended — suspend it first (that is the promotion step), then it is retired"
		return rs
	case len(cj.Status.Active) > 0:
		rs.State, rs.Message = "Blocked", fmt.Sprintf("%d active Job(s)", len(cj.Status.Active))
		return rs
	}
	if mode != hivev1.ModeEnforce {
		rs.State = "WouldDelete"
		metrics.Action(housekeepingController, hk.Name, "retire", false)
		return rs
	}
	if err := r.Delete(ctx, &cj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		rs.State, rs.Message = "Blocked", err.Error()
		return rs
	}
	metrics.Action(housekeepingController, hk.Name, "retire", true)
	rs.State = "Deleted"
	return rs
}

// SetupWithManager wires the controller. CronJob events reach it through the
// ownerReference (adopted jobs) — a hand edit to an adopted CronJob is seen
// at once; an unadopted one is re-diffed every Interval.
func (r *HiveHousekeepingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hivev1.HiveHousekeeping{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.CronJob{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{}))).
		Complete(r)
}
