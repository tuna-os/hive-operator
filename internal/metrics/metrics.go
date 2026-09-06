// Package metrics exposes fleet state as Prometheus series.
//
// WHY THIS IS THE POINT OF THE PROJECT
// ------------------------------------
// Every incident this operator exists to prevent was detectable and undetected,
// because each one left the obvious signals green:
//
//   - a fleet idle for ~8 hours: pods Running, panes healthy, providers with
//     headroom, credentials valid. The only tell was the last-kick age.
//   - a spoke at 190% of its token budget: the governor stopped scheduling ON
//     PURPOSE, which reads identically to a broken scheduler.
//   - four agents on a dead credential for two days, on a spoke that no
//     rotation job covered.
//   - 609 evicted pods from a node filling up, which took the ingress down.
//
// None of those needed a cleverer controller. They needed a number someone
// could alert on. That is what lives here.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SpokeReachable is 1 when the operator can read the spoke's API.
	SpokeReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_spoke_reachable",
		Help: "1 if the spoke's dashboard API answered on the last reconcile, else 0.",
	}, []string{"spoke", "namespace"})

	// AgentsTotal counts agents by state.
	AgentsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_agents",
		Help: "Agents per spoke, split by state (running, paused, on_demand).",
	}, []string{"spoke", "state"})

	// AgentIdleSeconds is time since an agent was last kicked.
	//
	// The highest-value series here. A fleet that has quietly stopped shows up
	// nowhere else: alert on this exceeding an agent's slowest cadence.
	AgentIdleSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_agent_idle_seconds",
		Help: "Seconds since this agent was last kicked.",
	}, []string{"spoke", "agent", "backend", "model"})

	// AgentPaused is 1 when paused, labelled with the trigger.
	//
	// NOTE the trigger does NOT separate an operator's pause from rotation's
	// own: rotation authenticates with the owner session cookie, so its strands
	// record as "manual pause" / "dashboard-api" byte-for-byte.
	AgentPaused = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_agent_paused",
		Help: "1 if the agent is paused. Trigger 'startup' is on-demand and expected.",
	}, []string{"spoke", "agent", "trigger"})

	// ProviderUsedPercent is a measured provider reading.
	//
	// -1 means UNMEASURED, which is not the same as exhausted and must not be
	// alerted as such — an unmeasured provider stays eligible by design.
	ProviderUsedPercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_used_percent",
		Help: "Provider quota used, 0-100. -1 means unmeasured (NOT exhausted).",
	}, []string{"spoke", "provider"})

	// BudgetUsedTokens and friends make the suppression gate visible.
	BudgetUsedTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_budget_used_tokens",
		Help: "Tokens spent in the current budget window.",
	}, []string{"spoke"})

	BudgetLimitTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_budget_limit_tokens",
		Help: "Weekly token limit. 0 means uncapped.",
	}, []string{"spoke"})

	// BudgetExhausted is the one to alert on alongside idle time: together they
	// separate "stopped because we told it to" from "stopped for no reason".
	BudgetExhausted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_budget_exhausted",
		Help: "1 when the governor is suppressing kicks because the budget is spent.",
	}, []string{"spoke"})

	// SharedAuthConsistent is the write-through result, per namespace and dir.
	SharedAuthConsistent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_shared_auth_consistent",
		Help: "1 when this namespace genuinely shares the credential dir (write-through verified).",
	}, []string{"store", "namespace", "dir"})

	// CredentialPresent catches an empty or zeroed token before agents do.
	CredentialPresent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_credential_present",
		Help: "1 when the shared credential file holds a non-empty token.",
	}, []string{"store", "namespace"})

	// ReconcileMode records how far each controller is allowed to act, so a
	// dashboard never implies an action was taken when it was only logged.
	ReconcileMode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_reconcile_mode",
		Help: "1 for the active mode of a controller (Observe/Shadow/Enforce).",
	}, []string{"controller", "object", "mode"})

	// ActionsTotal counts what a controller decided, split by whether it was
	// actually applied. In Shadow every action lands in applied="false".
	ActionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_actions_total",
		Help: "Actions a controller decided on, by kind and whether they were applied.",
	}, []string{"controller", "object", "action", "applied"})

	// ReconcileErrors counts failures per controller.
	ReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_reconcile_errors_total",
		Help: "Reconcile errors per controller.",
	}, []string{"controller", "object"})
)

// Register adds every collector to the controller-runtime registry, which is
// what /metrics serves.
func Register() {
	metrics.Registry.MustRegister(
		SpokeReachable, AgentsTotal, AgentIdleSeconds, AgentPaused,
		ProviderUsedPercent, BudgetUsedTokens, BudgetLimitTokens, BudgetExhausted,
		SharedAuthConsistent, CredentialPresent,
		ReconcileMode, ActionsTotal, ReconcileErrors,
	)
}

// SetMode records the active mode as a one-hot set of gauges.
func SetMode(controller, object, mode string) {
	for _, m := range []string{"Observe", "Shadow", "Enforce"} {
		v := 0.0
		if m == mode {
			v = 1.0
		}
		ReconcileMode.WithLabelValues(controller, object, m).Set(v)
	}
}

// Action records a decision. applied is false in Observe and Shadow.
func Action(controller, object, action string, applied bool) {
	a := "false"
	if applied {
		a = "true"
	}
	ActionsTotal.WithLabelValues(controller, object, action, a).Inc()
}
