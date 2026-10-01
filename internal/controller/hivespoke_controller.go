package controller

import (
	"context"
	"encoding/json"
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
	"github.com/tuna-os/hive-operator/internal/rotation"
)

const spokeController = "hivespoke"

var agentTiers = map[string]string{
	"supervisor": "T2", "scanner": "T2", "ci-maintainer": "T2",
	"quality": "T2", "guide": "T2", "outreach": "T2",
	"operations": "T2", "telemetry": "T2", "architect": "T1",
	"sec-check": "T1", "strategist": "T1",
}

// HiveSpokeReconciler observes a spoke and publishes its state as metrics.
//
// Observe-only by design in this pass: it never mutates a spoke. Placement,
// pausing and kicking are the incumbent CronJobs' job until a controller has
// been shadowed against them.
type HiveSpokeReconciler struct {
	client.Client
	Hive     *hiveclient.Client
	Interval time.Duration
	// Actuator overrides how Enforce decisions are applied (tests). Nil:
	// a HiveActuator bound to the spoke's pod and X-Hive-Internal token.
	// Nothing is ever applied unless the spoke's rotationMode is Enforce.
	Actuator rotation.Actuator
	// Now overrides the clock (tests).
	Now func() time.Time
}

// rawAgent is one agent entry as /api/status reports it, before folding in
// the runtime override journal, pins, or idle ages.
type rawAgent struct {
	Name          string `json:"name"`
	CLI           string `json:"cli"`
	Model         string `json:"model"`
	GovModel      string `json:"govModel"`
	Cadence       string `json:"cadence"`
	Effort        string `json:"reasoningEffort"`
	Mode          string `json:"mode"`
	Paused        bool   `json:"paused"`
	PausedTrigger string `json:"pausedTrigger"`
	PausedReason  string `json:"pausedReason"`
	OnDemand      *bool  `json:"onDemand"`
}

// statusResponse is the subset of /api/status this operator reads.
type statusResponse struct {
	Agents []rawAgent     `json:"agents"`
	Budget map[string]any `json:"budget"`
}

// Reading /api/status needs the spoke's dashboard token, which lives in the
// hive-secrets Secret in each spoke namespace. Without this rule the pod exec
// succeeds and the token read fails, so a healthy spoke reports unreachable.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivespokes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivespokes/status,verbs=get;update;patch

// Reconcile reads spoke state and publishes metrics.
func (r *HiveSpokeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sp hivev1.HiveSpoke
	if err := r.Get(ctx, req.NamespacedName, &sp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	rotationMode := sp.Spec.RotationMode
	if rotationMode == "" {
		rotationMode = hivev1.ModeShadow
	}
	metrics.SetMode(spokeController, sp.Name, string(rotationMode))

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

	// /api/status can lag switch/model writes. The persisted override journal is
	// the authority used at the next launch, so shadow decisions must overlay it
	// or they will certify invalid pairs such as codex + muse-spark as healthy.
	var runtimeState runtimeStateFile
	if out, err := r.Hive.Sh(ctx, sp.Spec.Namespace, pod, "cat /data/hive-state.json 2>/dev/null"); err == nil {
		_ = json.Unmarshal([]byte(out), &runtimeState)
	}

	pinned := map[string]bool{}
	for _, p := range sp.Spec.Pins {
		pinned[p.Agent] = true
	}

	obs := observeAgents(sr.Agents, runtimeState, ages, pinned)
	sp.Status.Agents = obs.Agents
	metrics.AgentIdleSeconds.Reset()
	for _, a := range obs.Agents {
		metrics.AgentPaused.WithLabelValues(sp.Name, a.Name, a.PausedTrigger).Set(b2f(a.Paused))
		if v, ok := ages[a.Name]; ok && v >= 0 {
			metrics.AgentIdleSeconds.WithLabelValues(sp.Name, a.Name, a.Backend, a.Model).Set(float64(v))
		}
	}
	metrics.AgentsTotal.WithLabelValues(sp.Name, "running").Set(float64(obs.Running))
	metrics.AgentsTotal.WithLabelValues(sp.Name, "paused").Set(float64(obs.Paused))
	metrics.AgentsTotal.WithLabelValues(sp.Name, "on_demand").Set(float64(obs.OnDemand))

	// Build the same placement decision set in Shadow and Enforce. Keeping one
	// planner is what makes a week of shadow output meaningful: promotion only
	// changes whether the already-visible actions are applied.
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	readings, sources := r.rotationReadings(ctx, &sp, now)
	sp.Status.Providers = providerStates(&sp, readings)
	sp.Status.RotationPlan, sp.Status.RotationPlanText, sp.Status.RotationInputs = nil, nil, ""
	if rotationMode != hivev1.ModeObserve {
		act := r.Actuator
		if act == nil && r.Hive != nil {
			act = &rotation.HiveActuator{API: boundAPI{c: r.Hive, pod: pod, token: token}}
		}
		r.rotate(ctx, &sp, rotationMode, readings, sources, act, now)
	}

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
	observed := metav1.NewTime(now)
	sp.Status.ObservedAt = &observed
	if err := r.Status().Update(ctx, &sp); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: iv}, nil
}

// runtimeAgentOverride is one agent's entry in the persisted override journal
// (hive-state.json). backend_override/model_override are the authority used
// at the next launch, ahead of whatever /api/status currently reports.
type runtimeAgentOverride struct {
	Backend string `json:"backend_override"`
	Model   string `json:"model_override"`
}

// runtimeStateFile is the subset of hive-state.json this controller reads.
type runtimeStateFile struct {
	Agents map[string]runtimeAgentOverride `json:"agents"`
}

// observation is the result of folding /api/status against the runtime
// override journal, pins, and idle ages — everything Reconcile needs to
// populate HiveSpokeStatus.Agents and the per-agent count gauges, without
// touching the Kubernetes API or emitting metrics itself.
type observation struct {
	Agents                    []hivev1.AgentState
	Running, Paused, OnDemand int
}

// observeAgents folds the raw /api/status agents against the persisted
// override journal (authoritative over /api/status, which can lag a
// switch/model write), spec-declared pins, and previously computed idle
// ages, into the AgentState list Reconcile publishes to status.
//
// Pure and side-effect free so it is testable independently of a live
// cluster or pod exec — the seam the observe phase lacked before this
// extraction.
func observeAgents(agents []rawAgent, runtimeState runtimeStateFile, ages map[string]int64, pinned map[string]bool) observation {
	var obs observation
	for _, a := range agents {
		// govModel is what the governor launches and what hive-rotate.sh
		// reads; model is the display field.
		if a.GovModel != "" {
			a.Model = a.GovModel
		}
		if persisted, ok := runtimeState.Agents[a.Name]; ok {
			if persisted.Backend != "" {
				a.CLI = persisted.Backend
			}
			if persisted.Model != "" {
				a.Model = persisted.Model
			}
		}
		od := a.OnDemand != nil && *a.OnDemand
		st := hivev1.AgentState{
			Name: a.Name, Backend: a.CLI, Model: a.Model, Effort: a.Effort, Mode: a.Mode, Cadence: a.Cadence,
			Paused: a.Paused, PausedTrigger: a.PausedTrigger, PausedReason: a.PausedReason,
			OnDemand: od, IdleSeconds: ages[a.Name], Pinned: pinned[a.Name],
		}
		obs.Agents = append(obs.Agents, st)

		switch {
		case od:
			obs.OnDemand++
		case a.Paused:
			obs.Paused++
		default:
			obs.Running++
		}
	}
	return obs
}

func numFrom(m map[string]any, k string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[k].(float64)
	return v, ok
}

func (r *HiveSpokeReconciler) dashboardToken(ctx context.Context, ns string) (string, error) {
	return r.Hive.Secret(ctx, ns, "hive-secrets", "HIVE_DASHBOARD_TOKEN")
}

// SetupWithManager wires the controller.
func (r *HiveSpokeReconciler) SetupWithManager(mgr ctrl.Manager, cs kubernetes.Interface, cfg *rest.Config) error {
	r.Hive = hiveclient.New(cs, cfg)
	return ctrl.NewControllerManagedBy(mgr).For(&hivev1.HiveSpoke{}).Complete(r)
}
