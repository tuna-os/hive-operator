package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/sharedauth"
)

const sharedAuthController = "sharedauth"

// SharedAuthReconciler verifies that a credential store is genuinely shared
// and, in Enforce, repairs what hive-shared-auth.sh repairs. The pass itself
// is internal/sharedauth (a rule-for-rule port, diffed against the bash).
type SharedAuthReconciler struct {
	client.Client
	Exec     sharedauth.Execer
	Interval time.Duration
}

// +kubebuilder:rbac:groups=hive.tunaos.org,resources=sharedauths,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=sharedauths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch

// Reconcile runs one pass.
func (r *SharedAuthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sa hivev1.SharedAuth
	if err := r.Get(ctx, req.NamespacedName, &sa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	mode := sa.Spec.Mode
	if mode == "" {
		mode = hivev1.ModeShadow
	}
	metrics.SetMode(sharedAuthController, sa.Name, string(mode))

	// Enforce is interlocked on the bash CronJob: never two writers.
	effective := mode
	incumbent := ""
	if mode == hivev1.ModeEnforce {
		if active, why := r.incumbentActive(ctx, sa.Spec.Incumbent); active {
			effective = hivev1.ModeShadow
			incumbent = why
		}
	}

	cfg := sharedAuthConfig(&sa, effective)
	// Every mode verifies: the write-through probe IS the observation.
	res := sharedauth.Run(ctx, r.Exec, cfg)
	st := sharedAuthStatus(&sa, cfg, res, effective)
	r.recordMetrics(&sa, cfg, res, effective)
	return r.finish(ctx, &sa, st, res.PrimaryErr, incumbent)
}

// sharedAuthConfig maps the spec onto one pass. Repairs run only when the
// EFFECTIVE mode is Enforce; otherwise the pass is the bash `check`.
func sharedAuthConfig(sa *hivev1.SharedAuth, effective hivev1.ReconcileMode) sharedauth.Config {
	home := sa.Spec.AgentHome
	if home == "" {
		home = "/data/home"
	}
	dirs := sa.Spec.Dirs
	if len(dirs) == 0 {
		dirs = []string{".claude", ".gemini", ".codex"}
	}
	enf := effective == hivev1.ModeEnforce
	return sharedauth.Config{
		Namespaces:    sa.Spec.Namespaces,
		Primary:       sa.Spec.PrimaryNamespace,
		Home:          home,
		Dirs:          dirs,
		FixTheme:      enf && sa.Spec.RepairTheme,
		FixStatusLine: enf && sa.Spec.RepairAgyStatusLine,
		FixPerms:      enf && sa.Spec.RepairPermissions,
	}
}

// incumbentActive reports whether the bash CronJob still runs. A read error
// counts as active: the interlock fails closed.
func (r *SharedAuthReconciler) incumbentActive(ctx context.Context, ref *hivev1.CronJobRef) (bool, string) {
	if ref == nil {
		ref = &hivev1.CronJobRef{Namespace: "hive", Name: "hive-shared-auth"}
	}
	if ref.Name == "" {
		return false, ""
	}
	var cj batchv1.CronJob
	err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &cj)
	switch {
	case apierrors.IsNotFound(err):
		return false, ""
	case err != nil:
		return true, fmt.Sprintf("cannot read CronJob %s/%s (%v) — not repairing", ref.Namespace, ref.Name, err)
	case cj.Spec.Suspend != nil && *cj.Spec.Suspend:
		return false, ""
	}
	return true, fmt.Sprintf("CronJob %s/%s is not suspended — Enforce runs as Shadow until it is (suspend it in the same change: docs/housekeeping.md)", ref.Namespace, ref.Name)
}

func sharedAuthStatus(sa *hivev1.SharedAuth, cfg sharedauth.Config, res sharedauth.Result, effective hivev1.ReconcileMode) hivev1.SharedAuthStatus {
	st := hivev1.SharedAuthStatus{Consistent: res.OK, Report: res.Lines, EffectiveMode: effective}
	unwritable := map[string]bool{}
	for _, d := range res.PrimaryUnwritable {
		unwritable[d] = true
	}
	if len(res.PrimaryUnwritable) > 0 {
		st.PendingRepairs = append(st.PendingRepairs,
			fmt.Sprintf("%s: %s not writable — cannot verify", cfg.Primary, strings.Join(res.PrimaryUnwritable, " ")))
	}
	for _, ns := range cfg.Namespaces {
		n := hivev1.SharedAuthNamespaceStatus{Namespace: ns, Shared: map[string]bool{}}
		if ns == cfg.Primary {
			for _, d := range cfg.Dirs {
				n.Shared[d] = !unwritable[d] && res.PrimaryErr == nil
			}
		} else {
			for d, ok := range res.Shared[ns] {
				n.Shared[d] = ok
				if !ok {
					// Deliberately NOT auto-repaired: this needs mount surgery and no
					// controller should guess at hostPath topology.
					st.PendingRepairs = append(st.PendingRepairs,
						fmt.Sprintf("%s/%s is NOT shared — this spoke has a private copy; a login here will not propagate (needs a mount fix, not a script)", ns, d))
				}
			}
			if _, judged := res.Shared[ns]; !judged && res.PrimaryErr == nil {
				n.Message = "no hive pod — skipped"
			}
		}
		sp := res.Spokes[ns]
		if sp == nil {
			if n.Message == "" && res.PrimaryErr == nil {
				n.Message = "no hive pod — skipped"
			}
			st.Namespaces = append(st.Namespaces, n)
			continue
		}
		n.CredentialPresent = sp.Token == "ok"
		n.ThemeSet = sp.Theme == "" || sp.Theme == "repaired"
		switch sp.Token {
		case "EMPTY":
			st.PendingRepairs = append(st.PendingRepairs,
				fmt.Sprintf("%s: claude token empty — needs an interactive `claude auth login` (one login covers the fleet)", ns))
		case "unreadable":
			st.PendingRepairs = append(st.PendingRepairs, fmt.Sprintf("%s: claude credential unreadable", ns))
		}
		var msgs []string
		switch sp.Theme {
		case "unset":
			st.PendingRepairs = append(st.PendingRepairs, fmt.Sprintf("%s: set .claude.json theme (unset — CLI stops at the theme picker)", ns))
		case "repair-failed":
			st.PendingRepairs = append(st.PendingRepairs, fmt.Sprintf("%s: .claude.json theme unset — REPAIR FAILED", ns))
		case "repaired":
			msgs = append(msgs, "theme repaired")
		}
		switch sp.StatusLine {
		case "broken":
			st.PendingRepairs = append(st.PendingRepairs, fmt.Sprintf(`%s: remove the broken agy statusLine ("/status")`, ns))
		case "repair-failed":
			st.PendingRepairs = append(st.PendingRepairs, fmt.Sprintf("%s: agy statusLine REPAIR FAILED", ns))
		case "repaired":
			msgs = append(msgs, "agy statusLine repaired")
		}
		if sp.Perms {
			msgs = append(msgs, "group perms repaired")
		}
		n.Message = strings.Join(msgs, "; ")
		st.Namespaces = append(st.Namespaces, n)
	}
	return st
}

func (r *SharedAuthReconciler) recordMetrics(sa *hivev1.SharedAuth, cfg sharedauth.Config, res sharedauth.Result, effective hivev1.ReconcileMode) {
	if res.PrimaryErr != nil {
		metrics.ReconcileErrors.WithLabelValues(sharedAuthController, sa.Name).Inc()
		return
	}
	applied := effective == hivev1.ModeEnforce
	for _, d := range cfg.Dirs {
		ok := true
		for _, u := range res.PrimaryUnwritable {
			if u == d {
				ok = false
			}
		}
		metrics.SharedAuthConsistent.WithLabelValues(sa.Name, cfg.Primary, d).Set(b2f(ok))
	}
	for ns, m := range res.Shared {
		for d, ok := range m {
			metrics.SharedAuthConsistent.WithLabelValues(sa.Name, ns, d).Set(b2f(ok))
		}
	}
	for ns, sp := range res.Spokes {
		metrics.CredentialPresent.WithLabelValues(sa.Name, ns).Set(b2f(sp.Token == "ok"))
		switch sp.Theme {
		case "unset":
			metrics.Action(sharedAuthController, sa.Name, "repair_theme", false)
		case "repaired":
			metrics.Action(sharedAuthController, sa.Name, "repair_theme", true)
		}
		switch sp.StatusLine {
		case "broken":
			metrics.Action(sharedAuthController, sa.Name, "repair_statusline", false)
		case "repaired":
			metrics.Action(sharedAuthController, sa.Name, "repair_statusline", true)
		}
		if sa.Spec.RepairPermissions {
			metrics.Action(sharedAuthController, sa.Name, "repair_perms", applied && sp.Perms)
		}
	}
}

func (r *SharedAuthReconciler) finish(ctx context.Context, sa *hivev1.SharedAuth, st hivev1.SharedAuthStatus, cause error, incumbent string) (ctrl.Result, error) {
	now := metav1.Now()
	st.ObservedAt = &now
	cond := metav1.Condition{
		Type:               "Consistent",
		Status:             metav1.ConditionTrue,
		Reason:             "WriteThroughVerified",
		Message:            "every namespace sees the same credential storage",
		LastTransitionTime: now,
	}
	if cause != nil {
		st.Consistent = false
		cond.Status = metav1.ConditionFalse
		cond.Reason = "VerificationFailed"
		cond.Message = cause.Error()
	} else if !st.Consistent {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NotShared"
		cond.Message = strings.Join(st.PendingRepairs, "; ")
	}
	inc := metav1.Condition{Type: "IncumbentActive", Status: metav1.ConditionFalse, Reason: "Clear",
		Message: "no unsuspended incumbent CronJob", LastTransitionTime: now}
	if incumbent != "" {
		inc.Status, inc.Reason, inc.Message = metav1.ConditionTrue, "EnforceInterlocked", incumbent
	}
	st.Conditions = []metav1.Condition{cond, inc}
	sa.Status = st
	if err := r.Status().Update(ctx, sa); err != nil {
		return ctrl.Result{}, err
	}
	iv := r.Interval
	if iv == 0 {
		iv = 30 * time.Minute
	}
	return ctrl.Result{RequeueAfter: iv}, cause
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// SetupWithManager wires the controller.
func (r *SharedAuthReconciler) SetupWithManager(mgr ctrl.Manager, cs kubernetes.Interface, cfg *rest.Config) error {
	if r.Exec == nil {
		r.Exec = hiveclient.New(cs, cfg)
	}
	// GenerationChanged: without it every status write re-triggers a pass, and
	// the pass (a dozen execs plus probe writes into the shared store) ran
	// back to back — observedAt advanced every ~20 s on the live cluster.
	return ctrl.NewControllerManagedBy(mgr).
		For(&hivev1.SharedAuth{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
