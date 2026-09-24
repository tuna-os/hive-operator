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

	// UsageRatio is a pool window's consumption over its limit (configured or
	// learned). -1 means there is no limit to compare against — NOT exhausted.
	UsageRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_usage_ratio",
		Help: "Pool window consumption / limit, from session logs (ccusage). -1: no limit known.",
	}, []string{"pool", "provider", "window"})

	// UsedPercentPool is what rotation would read: the provider's own
	// reading when fresh, else 100×ratio, else -1.
	UsedPercentPool = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_pool_used_percent",
		Help: "Pool window used percent as rotation consumes it; source=reading|ccusage. -1 unmeasured.",
	}, []string{"pool", "provider", "window", "source"})

	// ReadingPercent is the provider-reported figure alone, for comparing
	// against the ccusage-derived ratio (the shadow check for the probes).
	ReadingPercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_reading_percent",
		Help: "Provider-reported used percent for the window (probe/headroom). -1 absent or stale.",
	}, []string{"pool", "provider", "window"})

	// Remaining is limit − consumed, in the pool's unit.
	Remaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_remaining",
		Help: "Pool window limit minus consumption, in the pool's unit. -1: no limit known.",
	}, []string{"pool", "provider", "window", "unit"})

	// Consumed is the window's consumption, in the pool's unit.
	Consumed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_consumed",
		Help: "Pool window consumption from session logs, in the pool's unit.",
	}, []string{"pool", "provider", "window", "unit"})

	// Limit is the limit in force and where it came from.
	Limit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_limit",
		Help: "Pool window limit in the pool's unit; source=configured|learned.",
	}, []string{"pool", "provider", "window", "unit", "source"})

	// BurnPerHour is recent consumption per hour.
	BurnPerHour = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_burn_per_hour",
		Help: "Pool consumption over the last hour, per hour, in the pool's unit.",
	}, []string{"pool", "provider", "window", "unit"})

	// ExhaustionETA is seconds until the window's limit is reached at the
	// current burn; -1 when it will not be reached before the window resets.
	ExhaustionETA = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_provider_exhaustion_eta_seconds",
		Help: "Seconds until the pool window is exhausted at the current burn. -1: not before reset / unknown.",
	}, []string{"pool", "provider", "window"})

	// AgentUsage is per-agent consumption within a pool window.
	AgentUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_agent_usage",
		Help: "Per-agent consumption in the pool window, in the pool's unit (fleet-wide for shared stores).",
	}, []string{"pool", "provider", "agent", "window", "unit"})

	// UsageSourceUp is 1 when a sidecar source was read successfully.
	UsageSourceUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_source_up",
		Help: "1 if the hive-usage sidecar source answered; counted=false marks a duplicate shared store.",
	}, []string{"pool", "namespace", "source", "counted"})

	// RotationDecisions is the current plan, by action. In Shadow these are
	// what WOULD happen; hive_actions_total{applied} counts what did.
	RotationDecisions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_rotation_plan_decisions",
		Help: "Decisions in the spoke's current rotation plan, by action (move/strand/resume/canary).",
	}, []string{"spoke", "action", "applied"})

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
		UsageRatio, UsedPercentPool, ReadingPercent, Remaining, Consumed, Limit,
		BurnPerHour, ExhaustionETA, AgentUsage, UsageSourceUp, RotationDecisions,
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
