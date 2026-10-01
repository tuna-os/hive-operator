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
	// Backend is the CLI that runs it: claude, codex, agy, pi. Optional:
	// rotation only needs to know the agent is pinned (HIVE_ROTATE_PIN names
	// agents, not placements); Backend/Model document the intended pair.
	// +optional
	Backend string `json:"backend,omitempty"`
	// Model is the exact id the backend accepts. It need NOT be on the ladder —
	// pinning supervisor to gpt-5.4-mini (the only codex model with no service
	// tier, and so unmetered) is the motivating case.
	// +optional
	Model string `json:"model,omitempty"`
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
	//   UsagePool  (default) each provider's UsagePool .status.rotationReading:
	//              ccleft's /readings reduced exactly as hive-lib.sh's
	//              ccleft_probe does (HIVE_PROBE_SOURCE=ccleft, the bash
	//              default), falling back per provider to the ConfigMap below
	//              when the pool has no fresh reading. The ccusage sidecar is
	//              NOT needed for this.
	//   Probe      the hive/hive-provider-usage ConfigMap only.
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

	// Pace tunes the burn-rate pacer (hive-pace.sh). It runs under
	// RotationMode: Shadow plans, Enforce actuates this spoke's agents.
	// +optional
	Pace *PaceSpec `json:"pace,omitempty"`

	// ContributorNamespace holds the fleet-wide contributor Deployments the
	// PRIMARY spoke scales with provider headroom. Default hive-contributors.
	// +optional
	ContributorNamespace string `json:"contributorNamespace,omitempty"`

	// LivenessMode controls the watchdog (`hive-rotate.sh watchdog`: pane
	// classification and healing) and the nudge backstop (`hive-nudge.sh`)
	// for this spoke, independently of RotationMode. Observe computes
	// nothing; Shadow records the exact pass in .status.liveness without
	// acting; Enforce restarts, repairs, fixes efforts and kicks. A rotate-off
	// (moving an agent off a failing backend) is a placement and is applied
	// only when RotationMode is ALSO Enforce; otherwise it is reported and the
	// agent is restarted in place.
	// +optional
	// +kubebuilder:default=Shadow
	LivenessMode ReconcileMode `json:"livenessMode,omitempty"`

	// Liveness tunes the watchdog and nudge. Every default is the bash one.
	// +optional
	Liveness *LivenessPolicySpec `json:"liveness,omitempty"`
}

// LivenessPolicySpec mirrors the HIVE_WATCHDOG_* and HIVE_NUDGE_* knobs.
type LivenessPolicySpec struct {
	// WatchdogIntervalMinutes between watchdog passes (the CronJobs ran */5).
	// Default 5.
	// +optional
	WatchdogIntervalMinutes int32 `json:"watchdogIntervalMinutes,omitempty"`
	// MaxMutations: restart-causing actions (heal, repair, effort fix) per
	// pass. Each restarts an agent inside the API request, 30-60 s on the
	// loaded node. Default 3.
	// +optional
	MaxMutations int32 `json:"maxMutations,omitempty"`
	// BackoffBaseMinutes/BackoffMaxMinutes: per-agent exponential heal
	// backoff, base, 2×base … capped. Defaults 5 and 120.
	// +optional
	BackoffBaseMinutes int32 `json:"backoffBaseMinutes,omitempty"`
	// +optional
	BackoffMaxMinutes int32 `json:"backoffMaxMinutes,omitempty"`
	// StallMinutes: a turn open (busy=working) with a byte-identical pane for
	// this long is a hang. Default 60.
	// +optional
	StallMinutes int32 `json:"stallMinutes,omitempty"`
	// MaxSnapshotAgeSeconds: stall detection is skipped when /api/status's
	// snapshot is older than this (a frozen snapshot reads as a frozen
	// pane). Default 900.
	// +optional
	MaxSnapshotAgeSeconds int32 `json:"maxSnapshotAgeSeconds,omitempty"`
	// DisableWatchdog turns the watchdog pass off for this spoke.
	// +optional
	DisableWatchdog bool `json:"disableWatchdog,omitempty"`

	// NudgeIntervalMinutes between nudge passes (the CronJob ran :13,:43).
	// Default 30.
	// +optional
	NudgeIntervalMinutes int32 `json:"nudgeIntervalMinutes,omitempty"`
	// NudgeGrace: an agent is overdue when idle longer than Grace × its
	// LONGEST cadence across all governor modes. Default 2.
	// +optional
	NudgeGrace int32 `json:"nudgeGrace,omitempty"`
	// NudgeFloorSeconds: never nudge anything idle less than this. Default 1800.
	// +optional
	NudgeFloorSeconds int32 `json:"nudgeFloorSeconds,omitempty"`
	// NudgeMaxPerSpoke: successful kicks per pass. Default 4.
	// +optional
	NudgeMaxPerSpoke int32 `json:"nudgeMaxPerSpoke,omitempty"`
	// DisableNudge turns the nudge pass off for this spoke.
	// +optional
	DisableNudge bool `json:"disableNudge,omitempty"`
}

// LivenessAction is one auditable watchdog or nudge action.
type LivenessAction struct {
	// Agent is empty for spoke-wide actions (hygiene, wake).
	// +optional
	Agent string `json:"agent,omitempty"`
	// Kind: restart, rotate-off, repair, effort, hygiene, wake, needs-human, kick.
	Kind string `json:"kind"`
	// State is the pane classification that triggered a heal: wizard, auth,
	// approval, shell, empty, stalled.
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	ToBackend string `json:"toBackend,omitempty"`
	// +optional
	ToModel string `json:"toModel,omitempty"`
	// +optional
	Effort string `json:"effort,omitempty"`
	// HealCount is the restart number this heal records.
	// +optional
	HealCount int32  `json:"healCount,omitempty"`
	Reason    string `json:"reason"`
	Applied   bool   `json:"applied"`
	// +optional
	Error string `json:"error,omitempty"`
}

// LivenessStatus is the watchdog's and nudge's last passes and memory.
type LivenessStatus struct {
	// WatchdogAt is the last watchdog pass.
	// +optional
	WatchdogAt *metav1.Time `json:"watchdogAt,omitempty"`
	// WatchdogPlan is that pass's actions. In Shadow it is the exact action
	// set Enforce would take.
	// +optional
	WatchdogPlan []LivenessAction `json:"watchdogPlan,omitempty"`
	// WatchdogPlanText is the pass as `hive-rotate.sh watchdog` prints it
	// (Shadow shows "restarted (would restart)" where bash shows the hive's
	// answer), for diffing against the hive-watchdog job log.
	// +optional
	WatchdogPlanText []string `json:"watchdogPlanText,omitempty"`
	// NudgeAt is the last nudge pass.
	// +optional
	NudgeAt *metav1.Time `json:"nudgeAt,omitempty"`
	// +optional
	NudgePlan []LivenessAction `json:"nudgePlan,omitempty"`
	// NudgePlanText is this spoke's share of `hive-nudge.sh` output (Shadow
	// prints the check-mode suffix " — would nudge").
	// +optional
	NudgePlanText []string `json:"nudgePlanText,omitempty"`
	// Journal is the watchdog's memory. It is kept in Shadow too, as if
	// every planned action had succeeded, so the backoff ladder and stall
	// clocks are already warm (and comparable with bash's) at promotion.
	// +optional
	Journal *LivenessJournal `json:"journal,omitempty"`
}

// LivenessJournal replaces the watchdog's bash state files.
type LivenessJournal struct {
	// Heals: per-agent backoff (watchdog-last-kick-<agent>). Cleared the
	// first time the agent is seen ready.
	// +optional
	Heals []HealRecord `json:"heals,omitempty"`
	// Panes: stall clocks (watchdog-pane-<agent>) for agents mid-turn.
	// +optional
	Panes []PaneRecord `json:"panes,omitempty"`
	// Resets: when each exhausted provider renews (resets.d/<provider>).
	// +optional
	Resets []ProviderReset `json:"resets,omitempty"`
	// HygieneAt: the last shared-home permission repair (hygiene-last).
	// +optional
	HygieneAt *metav1.Time `json:"hygieneAt,omitempty"`
}

// HealRecord is one agent's heal backoff.
type HealRecord struct {
	Agent string      `json:"agent"`
	Count int32       `json:"count"`
	Last  metav1.Time `json:"last"`
}

// PaneRecord is one agent's stall clock: the pane fingerprint and when it
// was first seen unchanged.
type PaneRecord struct {
	Agent string      `json:"agent"`
	Hash  string      `json:"hash"`
	Since metav1.Time `json:"since"`
}

// ProviderReset is a pending renewal wake-up.
type ProviderReset struct {
	Provider string      `json:"provider"`
	At       metav1.Time `json:"at"`
}

// PaceSpec mirrors hive-pace.sh's HIVE_PACE_* actuation knobs. The verdict
// knobs (deadband, sample counts, Kiro safety/hot/cold) live on the
// UsagePool that computes the verdict.
type PaceSpec struct {
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// IntervalMinutes between pace ticks (the CronJob ran 5,25,45). Default 20.
	// +optional
	IntervalMinutes int32 `json:"intervalMinutes,omitempty"`
	// FleetOrder places this spoke in the fleet-wide pace order
	// (HIVE_PACE_NAMESPACES "hive hive-reef hive-hanthor"): one notch per
	// provider per tick goes to the FIRST eligible agent in that order. Lower
	// first; ties by namespace.
	// +optional
	FleetOrder int32 `json:"fleetOrder,omitempty"`
	// DisableKiroBudget paces Kiro with the generic fit instead of its credit
	// budget (HIVE_PACE_KIRO_BUDGET=0).
	// +optional
	DisableKiroBudget bool `json:"disableKiroBudget,omitempty"`
	// KiroPromoteMax: promote only if the projected burn/allowed stays at or
	// under this. Decimal string, default "0.8".
	// +optional
	KiroPromoteMax string `json:"kiroPromoteMax,omitempty"`
	// Kiro demotions per tick. Default 4.
	// +optional
	KiroMaxDemote int32 `json:"kiroMaxDemote,omitempty"`
	// Kiro promotions per tick. Default 1.
	// +optional
	KiroMaxPromote int32 `json:"kiroMaxPromote,omitempty"`
	// Kiro cap requests (move off Kiro) per tick. Default 2.
	// +optional
	KiroMaxEvict int32 `json:"kiroMaxEvict,omitempty"`
	// KiroEvictTTLSeconds: how long a cap request stands. Default 21600.
	// +optional
	KiroEvictTTLSeconds int32 `json:"kiroEvictTTLSeconds,omitempty"`
	// EvictTargetMaxPct: a pool is a cap target only below this. Default 85.
	// +optional
	EvictTargetMaxPct int32 `json:"evictTargetMaxPct,omitempty"`
}

// RotationPolicySpec mirrors hive-rotate.sh's HIVE_ROTATE_* knobs.
type RotationPolicySpec struct {
	// Thresholds: provider → percent at which it counts as exhausted.
	// Defaults: openai 85, anthropic 90, google 90, kiro 95, other 85.
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
	// KiroMinCadenceSeconds: agents kicked more often than this never go on
	// kiro, and are not sticky there, unless their current provider is
	// positively exhausted. Default 900.
	// +optional
	KiroMinCadenceSeconds int32 `json:"kiroMinCadenceSeconds,omitempty"`
	// CanaryCooldownMinutes keeps canaries off a pool rotation just left
	// because it was exhausted. Default 720.
	// +optional
	CanaryCooldownMinutes int32 `json:"canaryCooldownMinutes,omitempty"`
	// IntervalMinutes between Enforce applies (the CronJobs ran every 20
	// minutes; /api/status lags a mutation by minutes, so chained applies
	// re-decide on stale state). The plan itself is recomputed every
	// reconcile. Default 20.
	// +optional
	IntervalMinutes int32 `json:"intervalMinutes,omitempty"`
	// PeakProviders are avoided (softly) during PeakWindows. Default none
	// (DeepSeek, the only peak-priced pool, was dropped 2026-09-24).
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

	// Journal is rotation's and the pacer's memory, owned by this
	// controller once the spoke is in Enforce (the bash jobs kept it on the
	// hive-ops-state PVC). In Shadow it stays empty and the planner infers
	// pace demotions instead.
	// +optional
	Journal *RotationJournal `json:"journal,omitempty"`
	// RotationAppliedAt is the last Enforce apply (rotation.intervalMinutes).
	// +optional
	RotationAppliedAt *metav1.Time `json:"rotationAppliedAt,omitempty"`
	// ContributorPlanText is the contributors section of the plan (primary
	// spoke only), as hive-rotate.sh prints it.
	// +optional
	ContributorPlanText []string `json:"contributorPlanText,omitempty"`
	// PacePlan is the pacer's actions for THIS spoke's agents at the last
	// pace tick.
	// +optional
	PacePlan []RotationDecision `json:"pacePlan,omitempty"`
	// PacePlanText is the FLEET-wide pace plan as hive-pace apply prints it
	// (verdict table, kiro budget, actions, footer), for diffing against the
	// hive-pace job log.
	// +optional
	PacePlanText []string `json:"pacePlanText,omitempty"`
	// PaceTickAt is the last pace tick.
	// +optional
	PaceTickAt *metav1.Time `json:"paceTickAt,omitempty"`

	// Liveness is the watchdog's and nudge's last passes, under
	// spec.livenessMode.
	// +optional
	Liveness *LivenessStatus `json:"liveness,omitempty"`
}

// RotationJournal replaces the bash state files.
type RotationJournal struct {
	// Stranded: agents rotation paused because their provider ran dry and
	// no rung would take them (/state/…/stranded).
	// +optional
	Stranded []JournalPlacement `json:"stranded,omitempty"`
	// PaceDemoted: the ORIGINAL rung the pacer demoted each agent from
	// (pace-demoted). Rotation counts a demoted rung as in-tier; the pacer
	// restores only these.
	// +optional
	PaceDemoted []JournalPlacement `json:"paceDemoted,omitempty"`
	// KiroEvict: the pacer's requests to move agents off Kiro (kiro-evict).
	// +optional
	KiroEvict []KiroEvictRequest `json:"kiroEvict,omitempty"`
	// CanaryCooldown: pools rotation evicted because they were exhausted
	// (canary-cool-<provider>).
	// +optional
	CanaryCooldown []ProviderCooldown `json:"canaryCooldown,omitempty"`
}

// JournalPlacement is one journal row.
type JournalPlacement struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider,omitempty"`
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	// +optional
	At *metav1.Time `json:"at,omitempty"`
	// Inferred: seeded from the placement itself when the spoke was
	// promoted (the bash journal was not imported), not written by an
	// action of this controller.
	// +optional
	Inferred bool `json:"inferred,omitempty"`
}

// KiroEvictRequest asks rotation to move an agent off Kiro onto Targets.
type KiroEvictRequest struct {
	Agent   string      `json:"agent"`
	Expiry  metav1.Time `json:"expiry"`
	Targets []string    `json:"targets"`
}

// ProviderCooldown keeps canaries off a provider until Until.
type ProviderCooldown struct {
	Provider string      `json:"provider"`
	Until    metav1.Time `json:"until"`
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
