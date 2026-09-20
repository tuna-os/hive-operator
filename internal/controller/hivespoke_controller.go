package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
}

// statusResponse is the subset of /api/status this operator reads.
type statusResponse struct {
	Agents []struct {
		Name          string `json:"name"`
		CLI           string `json:"cli"`
		Model         string `json:"model"`
		Effort        string `json:"reasoningEffort"`
		Mode          string `json:"mode"`
		Paused        bool   `json:"paused"`
		PausedTrigger string `json:"pausedTrigger"`
		PausedReason  string `json:"pausedReason"`
		OnDemand      *bool  `json:"onDemand"`
	} `json:"agents"`
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
			Name: a.Name, Backend: a.CLI, Model: a.Model, Effort: a.Effort, Mode: a.Mode,
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

	// Build the same placement decision set in Shadow and Enforce. Keeping one
	// planner is what makes a week of shadow output meaningful: promotion only
	// changes whether the already-visible actions are applied.
	sp.Status.Providers = r.providerUsage(ctx, &sp)
	sp.Status.RotationPlan = nil
	if rotationMode != hivev1.ModeObserve {
		ladderName := sp.Spec.LadderRef
		if ladderName == "" {
			ladderName = "fleet"
		}
		var ladder hivev1.ModelLadder
		if err := r.Get(ctx, client.ObjectKey{Name: ladderName}, &ladder); err == nil {
			sp.Status.RotationPlan = planRotation(sp.Status.Agents, sp.Status.Providers, ladder.Status.Effective)
			for i := range sp.Status.RotationPlan {
				decision := &sp.Status.RotationPlan[i]
				applied := false
				if rotationMode == hivev1.ModeEnforce {
					decision.Error = r.applyRotation(ctx, &sp, pod, decision)
					applied = decision.Error == ""
					decision.Applied = applied
				}
				metrics.Action(spokeController, sp.Name, "rotate", applied)
			}
		}
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
	now := metav1.Now()
	sp.Status.ObservedAt = &now
	if err := r.Status().Update(ctx, &sp); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: iv}, nil
}

func (r *HiveSpokeReconciler) providerUsage(ctx context.Context, sp *hivev1.HiveSpoke) []hivev1.ProviderState {
	var cm corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: sp.Spec.Namespace, Name: "hive-provider-usage"}, &cm)
	if err != nil && sp.Spec.Namespace != "hive" {
		err = r.Get(ctx, client.ObjectKey{Namespace: "hive", Name: "hive-provider-usage"}, &cm)
	}
	providers := []string{"google", "meta", "deepseek", "openai", "anthropic"}
	result := make([]hivev1.ProviderState, 0, len(providers))
	for _, provider := range providers {
		state := hivev1.ProviderState{Provider: provider, UsedPercent: -1, Note: "no reading"}
		if err == nil {
			state.Note = cm.Data[provider]
			if fields := strings.Fields(state.Note); len(fields) > 0 && strings.HasSuffix(fields[0], "%") {
				if n, parseErr := strconv.Atoi(strings.TrimSuffix(fields[0], "%")); parseErr == nil {
					state.UsedPercent = int32(n)
				}
			}
		}
		metrics.ProviderUsedPercent.WithLabelValues(sp.Name, provider).Set(float64(state.UsedPercent))
		result = append(result, state)
	}
	return result
}

func planRotation(agents []hivev1.AgentState, providers []hivev1.ProviderState, rungs []hivev1.Rung) []hivev1.RotationDecision {
	usage := map[string]int32{}
	for _, p := range providers {
		usage[p.Provider] = p.UsedPercent
	}
	providerFor := func(backend, model string) string {
		for _, rung := range rungs {
			if rung.Backend == backend && rung.Model == model {
				return rung.Provider
			}
		}
		switch backend {
		case "codex":
			return "openai"
		case "claude":
			return "anthropic"
		case "agy":
			return "google"
		case "muse":
			return "meta"
		case "pi":
			return "deepseek"
		}
		return "unknown"
	}
	var plan []hivev1.RotationDecision
	for _, agent := range agents {
		if agent.Pinned || agent.OnDemand || agent.Paused {
			continue
		}
		tier := agentTiers[agent.Name]
		if tier == "" {
			continue
		}
		currentProvider := providerFor(agent.Backend, agent.Model)
		currentValid := false
		for _, rung := range rungs {
			if rung.Available && rung.Tier == tier && rung.Backend == agent.Backend && rung.Model == agent.Model && (agent.Effort == "" || rung.Effort == agent.Effort) {
				currentValid = true
				break
			}
		}
		if currentValid && usage[currentProvider] < 100 {
			continue
		}
		for _, rung := range rungs {
			if !rung.Available || rung.Tier != tier || usage[rung.Provider] >= 100 {
				continue
			}
			reason := "current rung is not in the effective ladder"
			if usage[currentProvider] >= 100 {
				reason = currentProvider + " usage is exhausted"
			}
			plan = append(plan, hivev1.RotationDecision{Agent: agent.Name, FromBackend: agent.Backend, FromModel: agent.Model, ToProvider: rung.Provider, ToBackend: rung.Backend, ToModel: rung.Model, ToEffort: rung.Effort, Reason: reason})
			break
		}
	}
	return plan
}

func (r *HiveSpokeReconciler) applyRotation(ctx context.Context, sp *hivev1.HiveSpoke, pod string, d *hivev1.RotationDecision) string {
	session, err := r.Hive.OwnerSession(ctx, sp.Spec.Namespace, pod)
	if err != nil || session == "" {
		return "no usable owner session"
	}
	post := func(path, want string) error {
		out, err := r.Hive.Post(ctx, sp.Spec.Namespace, pod, session, path)
		if err != nil {
			return err
		}
		var response map[string]any
		if json.Unmarshal([]byte(out), &response) != nil || response["status"] != want {
			return fmt.Errorf("unexpected response: %.200s", out)
		}
		return nil
	}
	if err := post("/api/switch/"+hiveclient.EscapePath(d.Agent)+"/"+hiveclient.EscapePath(d.ToBackend), "switched"); err != nil {
		return err.Error()
	}
	if err := post("/api/model/"+hiveclient.EscapePath(d.Agent)+"/"+hiveclient.EscapePath(d.ToModel), "model_set"); err != nil {
		_, _ = r.Hive.Post(ctx, sp.Spec.Namespace, pod, session, "/api/switch/"+hiveclient.EscapePath(d.Agent)+"/"+hiveclient.EscapePath(d.FromBackend))
		return "model change failed and backend was rolled back: " + err.Error()
	}
	if d.ToEffort != "" {
		if err := post("/api/effort/"+hiveclient.EscapePath(d.Agent)+"/"+hiveclient.EscapePath(d.ToEffort), "effort_set"); err != nil {
			return "placement changed but effort change failed: " + err.Error()
		}
	}
	if err := post("/api/kick/"+hiveclient.EscapePath(d.Agent), "kicked"); err != nil {
		return "placement changed but kick failed: " + err.Error()
	}
	return ""
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
