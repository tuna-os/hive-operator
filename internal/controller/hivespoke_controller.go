package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/metrics"
)

const spokeController = "hivespoke"

// HiveSpokeReconciler observes a spoke and publishes its state as metrics.
//
// Observe-only by design in this pass: it never mutates a spoke. Placement,
// pausing and kicking are the incumbent CronJobs' job until a controller has
// been shadowed against them.
type HiveSpokeReconciler struct {
	client.Client
	Hive     *hiveclient.Client
	Interval time.Duration
}

// statusResponse is the subset of /api/status this operator reads.
type statusResponse struct {
	Agents []struct {
		Name          string `json:"name"`
		CLI           string `json:"cli"`
		Model         string `json:"model"`
		Mode          string `json:"mode"`
		Paused        bool   `json:"paused"`
		PausedTrigger string `json:"pausedTrigger"`
		PausedReason  string `json:"pausedReason"`
		OnDemand      *bool  `json:"onDemand"`
	} `json:"agents"`
	Budget map[string]any `json:"budget"`
}

// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivespokes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivespokes/status,verbs=get;update;patch

// Reconcile reads spoke state and publishes metrics.
func (r *HiveSpokeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sp hivev1.HiveSpoke
	if err := r.Get(ctx, req.NamespacedName, &sp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	metrics.SetMode(spokeController, sp.Name, string(hivev1.ModeObserve))

	iv := r.Interval
	if iv == 0 {
		iv = 2 * time.Minute
	}
	fail := func(err error) (ctrl.Result, error) {
		metrics.SpokeReachable.WithLabelValues(sp.Name, sp.Spec.Namespace).Set(0)
		metrics.ReconcileErrors.WithLabelValues(spokeController, sp.Name).Inc()
		sp.Status.Reachable = false
		now := metav1.Now()
		sp.Status.ObservedAt = &now
		_ = r.Status().Update(ctx, &sp)
		return ctrl.Result{RequeueAfter: iv}, nil
	}

	pod, err := r.Hive.Pod(ctx, sp.Spec.Namespace)
	if err != nil {
		return fail(err)
	}
	token, err := r.dashboardToken(ctx, sp.Spec.Namespace)
	if err != nil {
		return fail(err)
	}

	var sr statusResponse
	if err := r.Hive.GetJSON(ctx, sp.Spec.Namespace, pod, token, "/api/status", &sr); err != nil {
		return fail(err)
	}

	// last_kick lives in the state file as ISO-8601 with fractional seconds and
	// a numeric offset. /api/status renders it as a human string ("9/6 9:35 AM
	// EDT") that is not safely parseable, so read the file. Ages are computed in
	// the pod because busybox `date -d` rejects that format outright.
	ages := map[string]int64{}
	if out, err := r.Hive.Sh(ctx, sp.Spec.Namespace, pod, `now=$(date -u +%s); jq -r '.agents | to_entries[] | "\(.key) \(.value.last_kick // "never")"' /data/hive-state.json 2>/dev/null | while read -r a t; do if [ "$t" = "never" ]; then echo "$a -1"; else e=$(date -u -d "$t" +%s 2>/dev/null) && echo "$a $((now-e))" || echo "$a -1"; fi; done`); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 {
				if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					ages[f[0]] = v
				}
			}
		}
	}

	pinned := map[string]bool{}
	for _, p := range sp.Spec.Pins {
		pinned[p.Agent] = true
	}

	var running, paused, onDemand int
	sp.Status.Agents = nil
	metrics.AgentIdleSeconds.Reset()
	for _, a := range sr.Agents {
		od := a.OnDemand != nil && *a.OnDemand
		st := hivev1.AgentState{
			Name: a.Name, Backend: a.CLI, Model: a.Model, Mode: a.Mode,
			Paused: a.Paused, PausedTrigger: a.PausedTrigger, PausedReason: a.PausedReason,
			OnDemand: od, IdleSeconds: ages[a.Name], Pinned: pinned[a.Name],
		}
		sp.Status.Agents = append(sp.Status.Agents, st)

		switch {
		case od:
			onDemand++
		case a.Paused:
			paused++
		default:
			running++
		}
		metrics.AgentPaused.WithLabelValues(sp.Name, a.Name, a.PausedTrigger).Set(b2f(a.Paused))
		if v, ok := ages[a.Name]; ok && v >= 0 {
			metrics.AgentIdleSeconds.WithLabelValues(sp.Name, a.Name, a.CLI, a.Model).Set(float64(v))
		}
	}
	metrics.AgentsTotal.WithLabelValues(sp.Name, "running").Set(float64(running))
	metrics.AgentsTotal.WithLabelValues(sp.Name, "paused").Set(float64(paused))
	metrics.AgentsTotal.WithLabelValues(sp.Name, "on_demand").Set(float64(onDemand))

	// Budget. BUDGET_EXHAUSTED is the governor's own suppression gate: when it
	// is true, an empty agents_due list means "told not to spend", not "broken".
	if v, ok := numFrom(sr.Budget, "BUDGET_USED"); ok {
		sp.Status.BudgetUsedTokens = int64(v)
		metrics.BudgetUsedTokens.WithLabelValues(sp.Name).Set(v)
	}
	if v, ok := numFrom(sr.Budget, "BUDGET_WEEKLY"); ok {
		metrics.BudgetLimitTokens.WithLabelValues(sp.Name).Set(v)
	}
	if v, ok := numFrom(sr.Budget, "BUDGET_PCT_USED"); ok {
		sp.Status.BudgetPctUsed = fmt.Sprintf("%.1f%%", v)
	}
	if b, ok := sr.Budget["BUDGET_EXHAUSTED"].(bool); ok {
		sp.Status.BudgetExhausted = b
		metrics.BudgetExhausted.WithLabelValues(sp.Name).Set(b2f(b))
	}

	metrics.SpokeReachable.WithLabelValues(sp.Name, sp.Spec.Namespace).Set(1)
	sp.Status.Reachable = true
	now := metav1.Now()
	sp.Status.ObservedAt = &now
	if err := r.Status().Update(ctx, &sp); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: iv}, nil
}

func numFrom(m map[string]any, k string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[k].(float64)
	return v, ok
}

func (r *HiveSpokeReconciler) dashboardToken(ctx context.Context, ns string) (string, error) {
	var sec struct {
		Data map[string][]byte
	}
	_ = sec
	s, err := r.Hive.Secret(ctx, ns, "hive-secrets", "HIVE_DASHBOARD_TOKEN")
	if err != nil {
		return "", err
	}
	return s, nil
}

// SetupWithManager wires the controller.
func (r *HiveSpokeReconciler) SetupWithManager(mgr ctrl.Manager, cs kubernetes.Interface, cfg *rest.Config) error {
	r.Hive = hiveclient.New(cs, cfg)
	return ctrl.NewControllerManagedBy(mgr).For(&hivev1.HiveSpoke{}).Complete(r)
}
