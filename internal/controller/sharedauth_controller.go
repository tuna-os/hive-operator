package controller

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/metrics"
)

const sharedAuthController = "sharedauth"

// SharedAuthReconciler verifies that a credential store is genuinely shared.
type SharedAuthReconciler struct {
	client.Client
	Hive     *hiveclient.Client
	Interval time.Duration
}

// +kubebuilder:rbac:groups=hive.tunaos.org,resources=sharedauths,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=sharedauths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// Reconcile verifies and, in Enforce mode, repairs.
func (r *SharedAuthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var sa hivev1.SharedAuth
	if err := r.Get(ctx, req.NamespacedName, &sa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	mode := sa.Spec.Mode
	if mode == "" {
		mode = hivev1.ModeShadow
	}
	metrics.SetMode(sharedAuthController, sa.Name, string(mode))

	home := sa.Spec.AgentHome
	if home == "" {
		home = "/data/home"
	}
	dirs := sa.Spec.Dirs
	if len(dirs) == 0 {
		dirs = []string{".claude", ".gemini", ".codex"}
	}

	status := hivev1.SharedAuthStatus{Consistent: true}
	byNS := map[string]*hivev1.SharedAuthNamespaceStatus{}
	for _, ns := range sa.Spec.Namespaces {
		byNS[ns] = &hivev1.SharedAuthNamespaceStatus{Namespace: ns, Shared: map[string]bool{}}
	}

	primaryPod, err := r.Hive.Pod(ctx, sa.Spec.PrimaryNamespace)
	if err != nil {
		metrics.ReconcileErrors.WithLabelValues(sharedAuthController, sa.Name).Inc()
		return r.finish(ctx, &sa, status, fmt.Errorf("primary namespace %s: %w", sa.Spec.PrimaryNamespace, err))
	}

	// WRITE-THROUGH, not inspection. On this cluster the same hostPath string
	// can resolve to a different filesystem for a freshly created mount; the
	// wrong one is an empty DirectoryOrCreate that `ls` renders as a perfectly
	// plausible directory. A marker written here and read there is the only
	// thing that distinguishes them.
	for _, dir := range dirs {
		stamp := fmt.Sprintf("probe-%d-%d", time.Now().UnixNano(), rand.Int31())
		marker := fmt.Sprintf("%s/%s/.shared-auth-probe", home, dir)

		if _, err := r.Hive.Sh(ctx, sa.Spec.PrimaryNamespace, primaryPod,
			fmt.Sprintf("printf '%%s' %q > %q", stamp, marker)); err != nil {
			lg.Error(err, "primary not writable", "dir", dir)
			status.Consistent = false
			continue
		}

		for _, ns := range sa.Spec.Namespaces {
			if ns == sa.Spec.PrimaryNamespace {
				byNS[ns].Shared[dir] = true
				continue
			}
			pod, err := r.Hive.Pod(ctx, ns)
			if err != nil {
				byNS[ns].Message = err.Error()
				continue
			}
			got, _ := r.Hive.Sh(ctx, ns, pod, fmt.Sprintf("cat %q 2>/dev/null", marker))
			ok := strings.TrimSpace(got) == stamp
			byNS[ns].Shared[dir] = ok
			metrics.SharedAuthConsistent.WithLabelValues(sa.Name, ns, dir).Set(b2f(ok))
			if !ok {
				status.Consistent = false
				// Deliberately NOT auto-repaired: this needs mount surgery and no
				// controller should guess at hostPath topology.
				status.PendingRepairs = append(status.PendingRepairs,
					fmt.Sprintf("%s/%s is NOT shared — this spoke has a private copy; a login here will not propagate (needs a mount fix, not a script)", ns, dir))
			}
		}
		metrics.SharedAuthConsistent.WithLabelValues(sa.Name, sa.Spec.PrimaryNamespace, dir).Set(1)
		_, _ = r.Hive.Sh(ctx, sa.Spec.PrimaryNamespace, primaryPod, fmt.Sprintf("rm -f %q", marker))
	}

	// Credential health and the two repairable causes.
	for _, ns := range sa.Spec.Namespaces {
		pod, err := r.Hive.Pod(ctx, ns)
		if err != nil {
			continue
		}
		st := byNS[ns]

		tok, _ := r.Hive.Sh(ctx, ns, pod, fmt.Sprintf(
			`jq -r 'if ((.claudeAiOauth.accessToken // "") == "") then "EMPTY" else "ok" end' %q 2>/dev/null`,
			home+"/.claude/.credentials.json"))
		st.CredentialPresent = strings.TrimSpace(tok) == "ok"
		metrics.CredentialPresent.WithLabelValues(sa.Name, ns).Set(b2f(st.CredentialPresent))
		if !st.CredentialPresent {
			status.Consistent = false
			status.PendingRepairs = append(status.PendingRepairs,
				fmt.Sprintf("%s: claude token empty — needs an interactive `claude auth login` (one login covers the fleet)", ns))
		}

		// theme:null parks the CLI at the picker forever while
		// hasCompletedOnboarding reads true. A MISSING file is not "fine"
		// either: the CLI authors one with no theme and wedges the same way.
		theme, _ := r.Hive.Sh(ctx, ns, pod, fmt.Sprintf(
			`jq -r '.theme // "null"' %q 2>/dev/null`, home+"/.claude.json"))
		st.ThemeSet = strings.TrimSpace(theme) != "null" && strings.TrimSpace(theme) != ""
		if !st.ThemeSet && sa.Spec.RepairTheme {
			action := fmt.Sprintf("%s: set .claude.json theme (unset — CLI stops at the theme picker)", ns)
			if mode == hivev1.ModeEnforce {
				script := fmt.Sprintf(`f=%q
if [ ! -f "$f" ]; then printf '%%s' '{"theme":"dark","hasCompletedOnboarding":true}' > "$f" || exit 3
else t=$(mktemp) && jq '.theme = "dark" | .hasCompletedOnboarding = true' "$f" > "$t" && cat "$t" > "$f" && rm -f "$t"; fi
chgrp node "$f" 2>/dev/null; chmod 664 "$f" 2>/dev/null`, home+"/.claude.json")
				if _, err := r.Hive.Sh(ctx, ns, pod, script); err != nil {
					status.PendingRepairs = append(status.PendingRepairs, action+" — REPAIR FAILED: "+err.Error())
					metrics.Action(sharedAuthController, sa.Name, "repair_theme", false)
				} else {
					st.ThemeSet = true
					metrics.Action(sharedAuthController, sa.Name, "repair_theme", true)
				}
			} else {
				status.PendingRepairs = append(status.PendingRepairs, action)
				metrics.Action(sharedAuthController, sa.Name, "repair_theme", false)
			}
		}

		// Token refresh REWRITES the credential file; without group write the
		// refresh fails and looks exactly like an expired subscription.
		if sa.Spec.RepairPermissions {
			if mode == hivev1.ModeEnforce {
				script := fmt.Sprintf(`for d in %s; do p=%s/$d; [ -e "$p" ] || continue; chgrp -R node "$p" 2>/dev/null; chmod -R g+rwX "$p" 2>/dev/null; done`,
					strings.Join(dirs, " "), home)
				_, _ = r.Hive.Sh(ctx, ns, pod, script)
				metrics.Action(sharedAuthController, sa.Name, "repair_perms", true)
			} else {
				metrics.Action(sharedAuthController, sa.Name, "repair_perms", false)
			}
		}
	}

	for _, ns := range sa.Spec.Namespaces {
		status.Namespaces = append(status.Namespaces, *byNS[ns])
	}
	return r.finish(ctx, &sa, status, nil)
}

func (r *SharedAuthReconciler) finish(ctx context.Context, sa *hivev1.SharedAuth, st hivev1.SharedAuthStatus, cause error) (ctrl.Result, error) {
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
	st.Conditions = []metav1.Condition{cond}
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
	r.Hive = hiveclient.New(cs, cfg)
	return ctrl.NewControllerManagedBy(mgr).For(&hivev1.SharedAuth{}).Complete(r)
}
