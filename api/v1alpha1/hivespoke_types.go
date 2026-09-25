package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AgentPin fixes one agent's placement. Rotation must leave a pinned agent's
// rung alone, and healing must not "repair" it back onto the ladder.
//
// This exists because a pin that anything else can overwrite is not a pin. The
// hive config carries a `cli_pinned` field that the rotation script never read,
// so a model set through the dashboard was reverted within 20 minutes; the
// watchdog then rewrote it again, because its repair path only ever selects a
// tier member and therefore can never restore a deliberately off-ladder choice.
type AgentPin struct {
	// Agent is the agent name, e.g. "supervisor".
	Agent string `json:"agent"`
	// Backend is the CLI that runs it: claude, codex, agy, pi.
	Backend string `json:"backend"`
	// Model is the exact id the backend accepts. It need NOT be on the ladder —
	// pinning supervisor to gpt-5.4-mini (the only codex model with no service
	// tier, and so unmetered) is the motivating case.
	Model string `json:"model"`
	// Reason is free text, surfaced in the dashboard and in events.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// BudgetSpec caps token spend for a spoke.
//
// WeeklyTokens == 0 means uncapped. This is load-bearing: when a limit is set
// and exceeded, the hive governor stops marking agents due, which is
// indistinguishable from a stalled scheduler unless you look at the budget.
// An operator that nudges agents MUST consult Status.BudgetExhausted first, or
// it spends money the operator explicitly capped.
type BudgetSpec struct {
	// +optional
	WeeklyTokens int64 `json:"weeklyTokens,omitempty"`
	// +optional
	// +kubebuilder:default=7
	PeriodDays int32 `json:"periodDays,omitempty"`
	// +optional
	// +kubebuilder:default=90
	CriticalPct int32 `json:"criticalPct,omitempty"`
}

// HiveSpokeSpec is the desired state of one hive instance.
type HiveSpokeSpec struct {
	// Namespace the hive Deployment runs in.
	Namespace string `json:"namespace"`

	// Org is the GitHub account or organisation this spoke acts on.
	Org string `json:"org"`

	// InstallationID is the GitHub App installation. Exactly one per spoke:
	// a repo under an account this installation does not cover 401-loops, and
	// that is a hard constraint of the hive config, not something to route
	// around per-repo.
	// +optional
	InstallationID string `json:"installationID,omitempty"`

	// Repos this spoke manages. Curated deliberately — a repo enumerator cannot
	// infer "the handful I am actively committing to".
	// +optional
	Repos []string `json:"repos,omitempty"`

	// PrimarySpoke names the spoke that owns fleet-wide resources (the
	// contributor pool). A non-primary spoke must skip contributor
	// reconciliation or two controllers fight over the same replicas.
	// +optional
	PrimarySpoke string `json:"primarySpoke,omitempty"`

	// Budget caps weekly token spend. Omit or set 0 for uncapped.
	// +optional
	Budget *BudgetSpec `json:"budget,omitempty"`

	// Pins fix individual agents' placement against rotation and healing.
	// +optional
	Pins []AgentPin `json:"pins,omitempty"`

	// Holds lists agents allowed to remain paused. Anything else found paused
	// through the dashboard API is resumed: an undeclared hand pause is not
	// durable state, it is how a spoke goes silently idle. Declaring a hold
	// here makes it reviewable and survives a pod rebuild.
	// +optional
	Holds []string `json:"holds,omitempty"`

	// LadderRef selects the ModelLadder this spoke places agents from.
	// +optional
	LadderRef string `json:"ladderRef,omitempty"`

	// RotationMode controls placement decisions for this spoke. Shadow records
	// the exact changes without applying them; Enforce performs the same plan.
	// +optional
	// +kubebuilder:default=Shadow
	RotationMode ReconcileMode `json:"rotationMode,omitempty"`

	// RotationUsageSource selects the provider readings rotation plans from:
	//   Probe      the hive-provider-usage ConfigMap the bash probes publish
	//              (default — shadow must compare decision logic on the SAME
	//              inputs the bash job sees, or every diff is ambiguous);
	//   UsagePool  UsagePool status (ccusage consumption vs limit), falling
	//              back to Probe for a provider with no pool.
	// +optional
	// +kubebuilder:validation:Enum=Probe;UsagePool
	RotationUsageSource string `json:"rotationUsageSource,omitempty"`

	// AgentTiers maps agent → tier (T1/T2/T3). An agent with no tier is never
	// placed. Default: hive-rotate.sh's AGENT_TIERS table.
	// +optional
	AgentTiers map[string]string `json:"agentTiers,omitempty"`

	// Rotation tunes the placement policy. Every default is hive-rotate.sh's.
	// +optional
	Rotation *RotationPolicySpec `json:"rotation,omitempty"`
}

// RotationPolicySpec mirrors hive-rotate.sh's HIVE_ROTATE_* knobs.
type RotationPolicySpec struct {
	// Thresholds: provider → percent at which it counts as exhausted.
	// Defaults: openai 85, anthropic 90, google 90, deepseek 100, other 85.
	// +optional
	Thresholds map[string]int32 `json:"thresholds,omitempty"`
	// HighVolumeCadenceSeconds: agents kicked at least this often are
	// high-volume — kept off openai (unless their provider is exhausted) and
	// capped on google. Default 1800.
	// +optional
	HighVolumeCadenceSeconds int32 `json:"highVolumeCadenceSeconds,omitempty"`
	// AgyMaxHighVolume caps high-volume agents on google per tick. Default 5.
	// +optional
	AgyMaxHighVolume int32 `json:"agyMaxHighVolume,omitempty"`
	// +optional
	DisableCanaries bool `json:"disableCanaries,omitempty"`
	// +optional
	DisableAutoResume bool `json:"disableAutoResume,omitempty"`
	// +optional
	DisableMeteredFailover bool `json:"disableMeteredFailover,omitempty"`
	// PeakProviders are avoided (softly) during PeakWindows. Default deepseek.
	// +optional
	PeakProviders []string `json:"peakProviders,omitempty"`
	// PeakWindows, UTC weekdays, "HH:MM-HH:MM,…". Default 01:00-04:00,06:00-10:00.
	// +optional
	PeakWindows string `json:"peakWindows,omitempty"`
}

// RotationDecision is one auditable placement decision.
type RotationDecision struct {
	Agent string `json:"agent"`
	// Action: move, strand, resume or canary.
	// +optional
	Action string `json:"action,omitempty"`
	// +optional
	FromProvider string `json:"fromProvider,omitempty"`
	FromBackend  string `json:"fromBackend,omitempty"`
	FromModel    string `json:"fromModel,omitempty"`
	// +optional
	FromEffort string `json:"fromEffort,omitempty"`
	ToProvider string `json:"toProvider"`
	ToBackend  string `json:"toBackend"`
	ToModel    string `json:"toModel"`
	// +optional
	ToEffort string `json:"toEffort,omitempty"`
	Reason   string `json:"reason"`
	Applied  bool   `json:"applied"`
	// +optional
	Error string `json:"error,omitempty"`
}

// ProviderState is a measured reading for one backend provider.
type ProviderState struct {
	Provider string `json:"provider"`
	// UsedPercent is 0-100, or -1 when genuinely unmeasured.
	//
	// The distinction matters more than it looks: "unmeasured" is not evidence
	// of exhaustion and must keep a provider eligible, while a positive reading
	// at 100 must evacuate it. Conflating them either strands a healthy fleet
	// or keeps filling a dead pool.
	UsedPercent int32 `json:"usedPercent"`
	// +optional
	Note string `json:"note,omitempty"`
	// +optional
	ResetsAt *metav1.Time `json:"resetsAt,omitempty"`
}

// AgentState is the observed state of one agent.
type AgentState struct {
	Name string `json:"name"`
	// +optional
	Backend string `json:"backend,omitempty"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Effort string `json:"effort,omitempty"`
	// +optional
	Mode string `json:"mode,omitempty"`
	// Cadence as /api/status renders it ("5m", "2h"). Rotation keeps
	// high-volume agents (≤30m) off the subscription pools.
	// +optional
	Cadence string `json:"cadence,omitempty"`
	Paused  bool   `json:"paused"`
	// PausedTrigger distinguishes why. NOTE it does NOT distinguish an operator
	// pause from rotation's own: rotation authenticates with the owner session
	// cookie, so a strand records exactly as a human pause does
	// (reason "manual pause", trigger "dashboard-api"). The stranded journal is
	// the only reliable discriminator.
	// +optional
	PausedTrigger string `json:"pausedTrigger,omitempty"`
	// +optional
	PausedReason string `json:"pausedReason,omitempty"`
	// +optional
	OnDemand bool `json:"onDemand,omitempty"`
	// +optional
	LastKick *metav1.Time `json:"lastKick,omitempty"`
	// IdleSeconds since the last kick. The single most useful number for
	// noticing a fleet that has quietly stopped: every other signal stayed green
	// through an eight-hour outage.
	// +optional
	IdleSeconds int64 `json:"idleSeconds,omitempty"`
	// Pinned is true when Spec.Pins covers this agent.
	// +optional
	Pinned bool `json:"pinned,omitempty"`
}

// HiveSpokeStatus is the observed state.
type HiveSpokeStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	HiveID string `json:"hiveID,omitempty"`
	// +optional
	Reachable bool `json:"reachable"`
	// +optional
	Agents []AgentState `json:"agents,omitempty"`
	// +optional
	Providers []ProviderState `json:"providers,omitempty"`
	// RotationPlan is replaced on every reconcile. In Shadow it is the exact
	// action set the controller would apply if promoted to Enforce.
	// +optional
	RotationPlan []RotationDecision `json:"rotationPlan,omitempty"`
	// RotationPlanText is the plan rendered exactly as `hive-rotate.sh plan`
	// prints it (agent lines + footer, without the contributors section), so
	// shadow output can be diffed against the bash job's log with plain diff.
	// +optional
	RotationPlanText []string `json:"rotationPlanText,omitempty"`
	// RotationInputs records what the plan was computed from ("Probe
	// hive/hive-provider-usage @ 2026-09-24T17:12:32Z"), so a disagreement
	// can be attributed to inputs rather than logic.
	// +optional
	RotationInputs string `json:"rotationInputs,omitempty"`
	// +optional
	BudgetUsedTokens int64 `json:"budgetUsedTokens,omitempty"`
	// +optional
	BudgetPctUsed string `json:"budgetPctUsed,omitempty"`
	// BudgetExhausted mirrors the governor's own suppression gate. When true the
	// governor stops kicking on purpose, and nothing should nudge past it.
	// +optional
	BudgetExhausted bool `json:"budgetExhausted,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=spoke
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.org`
// +kubebuilder:printcolumn:name="Agents",type=integer,JSONPath=`.status.agents[*]`,priority=1
// +kubebuilder:printcolumn:name="Budget%",type=string,JSONPath=`.status.budgetPctUsed`
// +kubebuilder:printcolumn:name="Exhausted",type=boolean,JSONPath=`.status.budgetExhausted`
// +kubebuilder:printcolumn:name="Reachable",type=boolean,JSONPath=`.status.reachable`

// HiveSpoke is one hive instance in the fleet.
type HiveSpoke struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HiveSpokeSpec   `json:"spec,omitempty"`
	Status HiveSpokeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HiveSpokeList contains a list of HiveSpoke.
type HiveSpokeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HiveSpoke `json:"items"`
}

func init() { SchemeBuilder.Register(&HiveSpoke{}, &HiveSpokeList{}) }
