package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// UsageWindowSpec is one quota window of a provider account.
type UsageWindowSpec struct {
	// Name labels the window in status and metrics: "5h", "weekly", "daily".
	Name string `json:"name"`
	// Duration of the window. When the provider reading carries a reset time
	// the window is anchored on it (start = resetsAt − duration); otherwise it
	// rolls back from now.
	Duration metav1.Duration `json:"duration"`
	// Limit, in the pool's Unit, as a decimal string. Empty means learn it by
	// calibrating consumption against the provider reading. A configured limit
	// always wins; configure one only when the provider documents it — a
	// guessed limit is worse than a learned one.
	// +optional
	Limit string `json:"limit,omitempty"`
	// ReadingSlot selects which of the provider reading's windows calibrates
	// this one. Anthropic publishes several (anthropic_limits, ordered by
	// reset: slot0 = 5-hour session, slot1 = weekly); others publish one
	// (leave empty).
	// +optional
	ReadingSlot string `json:"readingSlot,omitempty"`
	// CcleftWindow selects the ccleft window (Reading.windows[].id, e.g.
	// "five_hour", "seven_day", "weekly", "gemini-5h", "plan") that feeds this
	// window when spec.ccleft is set. Empty: the binding window whose kind
	// matches Duration (≤6h five_hour, ≤48h daily, ≤14d weekly, else
	// monthly), restricted to spec.ccleft.scope.
	// +optional
	CcleftWindow string `json:"ccleftWindow,omitempty"`
}

// CcleftRef points at a `ccleft serve` instance (github.com/tuna-os/ccleft),
// which reports each provider account's REMAINING quota straight from the
// provider (remaining, limit, reset) and dedupes homes sharing one account.
type CcleftRef struct {
	// URL of ccleft serve, e.g. http://ccleft.hive.svc:9464. GET /readings is
	// read from it.
	URL string `json:"url"`
	// Provider is ccleft's provider name. Default derived from the pool's:
	// anthropic→claude, openai→codex, google→agy, github→copilot,
	// meta→muse, kiro→kiro, deepseek→deepseek.
	// +optional
	Provider string `json:"provider,omitempty"`
	// Account pins one account fingerprint when ccleft serves several for
	// this provider. Empty: exactly one account is required.
	// +optional
	Account string `json:"account,omitempty"`
	// Scope restricts automatic window matching to one ccleft scope, e.g.
	// "gemini" for agy's Gemini pool (agy also reports a "3p" pool for
	// Claude/GPT models on the same account).
	// +optional
	Scope string `json:"scope,omitempty"`
	// MaxAge beyond which a reading's measurement (fetched_at — for a stale
	// last-good reading, the time of the last good measurement) is ignored.
	// Default 30m.
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

// ProbeConfigMapRef points at a provider-reading ConfigMap in the
// hive-provider-usage format ("<N>% used resets=<RFC3339> …").
type ProbeConfigMapRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Key is the provider key; defaults to the pool's provider.
	// +optional
	Key string `json:"key,omitempty"`
	// MaxAge beyond which a reading is ignored as stale. Default 30m.
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

// UsagePoolSpec describes one provider account whose quota several agents —
// possibly on several spokes — draw on.
//
// A pool is an ACCOUNT, not a spoke: the fleet's Claude, Gemini/Antigravity and
// Codex logins live on RWX PVCs shared by every spoke (and by the
// hive-contributors pods), so one 5-hour Claude window is spent by all of them
// together. The sidecar in each spoke reads the same shared store; the
// controller counts each store once, by content fingerprint.
type UsagePoolSpec struct {
	// Provider: anthropic, openai, google, deepseek, meta, github.
	Provider string `json:"provider"`
	// Sources are the ccusage sources whose rows count against this pool;
	// rows are further filtered to this provider by model (pi fronts several).
	// Default: derived from the provider (anthropic→claude, openai→codex,
	// google→antigravity+gemini, deepseek→pi+goose).
	// +optional
	Sources []string `json:"sources,omitempty"`
	// Namespaces whose hive-usage sidecars are read. Default: every
	// HiveSpoke's namespace.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// Unit the limits are in: costUSD (default), tokens, outputTokens.
	// +optional
	// +kubebuilder:validation:Enum=costUSD;tokens;outputTokens
	Unit string `json:"unit,omitempty"`
	// Windows are the provider's quota windows. Optional: a pool with none
	// still publishes its rotation reading and pace verdict (meta has no
	// quota windows at all).
	// +optional
	Windows []UsageWindowSpec `json:"windows,omitempty"`
	// Reading is the provider-reported "% used" used for calibration and,
	// while it exists, as the authoritative UsedPercent. Transitional: today
	// it is the bash probe's ConfigMap; see DESIGN.md for retiring it.
	// +optional
	Reading *ProbeConfigMapRef `json:"reading,omitempty"`
	// Ccleft reads the provider's own remaining/limit/reset per window from
	// ccleft. When set it is preferred; Reading (the ConfigMap) is the
	// per-window fallback while ccleft is unreachable, has no usable reading
	// for this account, or has no matching window.
	// +optional
	Ccleft *CcleftRef `json:"ccleft,omitempty"`
	// Pace tunes the burn-rate verdict computed from the pool's reading
	// history (hive-pace.sh's fit, and the Kiro credit budget).
	// +optional
	Pace *PoolPaceSpec `json:"pace,omitempty"`
	// SidecarPort is where hive-usage listens in each hive pod. Default 9464.
	// +optional
	SidecarPort int32 `json:"sidecarPort,omitempty"`
	// Mode: only Observe is meaningful — a pool measures, it never acts.
	// Rotation consumes pools; see HiveSpoke.spec.rotationUsageSource.
	// +optional
	// +kubebuilder:default=Observe
	Mode ReconcileMode `json:"mode,omitempty"`
}

// PoolPaceSpec mirrors hive-pace.sh's verdict knobs. Decimals are strings.
type PoolPaceSpec struct {
	// Deadband around 1.0 that counts as on-pace. Default "0.25".
	// +optional
	Deadband string `json:"deadband,omitempty"`
	// MinSamples before any verdict. Default 3.
	// +optional
	MinSamples int32 `json:"minSamples,omitempty"`
	// MinSpanSeconds the samples must span. Default 3600.
	// +optional
	MinSpanSeconds int32 `json:"minSpanSeconds,omitempty"`
	// SampleIntervalSeconds: keep at most one sample per this interval
	// (hive-pace sampled each 20-minute tick; Kiro also took every reading
	// rotate/watchdog saw). Default 1200; 0 for the kiro pool.
	// +optional
	SampleIntervalSeconds *int32 `json:"sampleIntervalSeconds,omitempty"`
	// HistoryHours of samples kept. Default 60 (≈ hive-pace's 1200 lines).
	// +optional
	HistoryHours int32 `json:"historyHours,omitempty"`
	// Kiro credit budget (kiro pool only): allowed = remaining/hours × Safety.
	// +optional
	KiroSafety string `json:"kiroSafety,omitempty"`
	// +optional
	KiroHot string `json:"kiroHot,omitempty"`
	// +optional
	KiroCold string `json:"kiroCold,omitempty"`
	// +optional
	KiroWindowSeconds int32 `json:"kiroWindowSeconds,omitempty"`
	// +optional
	KiroMinSamples int32 `json:"kiroMinSamples,omitempty"`
	// +optional
	KiroMinSpanSeconds int32 `json:"kiroMinSpanSeconds,omitempty"`
	// +optional
	KiroMaxReadingAgeSeconds int32 `json:"kiroMaxReadingAgeSeconds,omitempty"`
}

// PoolRotationReading is the pool's reading in hive-rotate.sh's probe shape.
type PoolRotationReading struct {
	// Percent 0-100, or -1 UNMEASURED (never exhausted).
	Percent int32 `json:"percent"`
	// Note is the probe note (resets=…, no-usage-api (…), credits=U/L …).
	// +optional
	Note string `json:"note,omitempty"`
	// Source: ccleft, configmap or none.
	Source string `json:"source"`
	// MeasuredAt: ccleft's fetched_at, or the ConfigMap's updated_at.
	// +optional
	MeasuredAt *metav1.Time `json:"measuredAt,omitempty"`
	// ComputedAt: when this controller derived it.
	// +optional
	ComputedAt *metav1.Time `json:"computedAt,omitempty"`
}

// PaceSlotStatus is one limit's fit. Decimals are strings ("" = null).
type PaceSlotStatus struct {
	Slot string `json:"slot"`
	Pct  string `json:"pct"`
	// +optional
	Reset   int64 `json:"reset,omitempty"`
	Samples int32 `json:"samples"`
	// +optional
	SpanSeconds int64 `json:"spanSeconds,omitempty"`
	// +optional
	HoursLeft string `json:"hoursLeft,omitempty"`
	// +optional
	AllowedRate string `json:"allowedRate,omitempty"`
	// +optional
	ObservedRate string `json:"observedRate,omitempty"`
	// +optional
	Ratio string `json:"ratio,omitempty"`
}

// KiroBudgetStatus is the Kiro credit budget verdict. Decimals are strings.
type KiroBudgetStatus struct {
	// Verdict: over, under, on-budget, settling, learning, stale,
	// no-deadline, no-data.
	Verdict string `json:"verdict"`
	// +optional
	Used string `json:"used,omitempty"`
	// +optional
	Limit string `json:"limit,omitempty"`
	// +optional
	Remaining string `json:"remaining,omitempty"`
	// +optional
	Reset int64 `json:"reset,omitempty"`
	// +optional
	ReadingAgeSeconds int64 `json:"readingAgeSeconds,omitempty"`
	// +optional
	Safety string `json:"safety,omitempty"`
	// +optional
	HoursLeft string `json:"hoursLeft,omitempty"`
	// +optional
	Allowed string `json:"allowed,omitempty"`
	// +optional
	Burn string `json:"burn,omitempty"`
	// +optional
	Burn6h string `json:"burn6h,omitempty"`
	// +optional
	Ratio string `json:"ratio,omitempty"`
	// +optional
	Samples int32 `json:"samples,omitempty"`
	// +optional
	SpanSeconds int64 `json:"spanSeconds,omitempty"`
	// +optional
	WindowStart int64 `json:"windowStart,omitempty"`
	// Line is the verdict as hive-pace prints it.
	// +optional
	Line string `json:"line,omitempty"`
}

// PoolPaceStatus is the pool's pace verdict (hive-pace's pace.json entry).
type PoolPaceStatus struct {
	// Verdict: hot, cold, on-pace, learning.
	Verdict string `json:"verdict"`
	// +optional
	Pressure string `json:"pressure,omitempty"`
	// +optional
	BindingSlot string `json:"bindingSlot,omitempty"`
	// +optional
	Slots []PaceSlotStatus `json:"slots,omitempty"`
	// Row is the provider's line of hive-pace's verdict table.
	// +optional
	Row string `json:"row,omitempty"`
	// +optional
	KiroBudget *KiroBudgetStatus `json:"kiroBudget,omitempty"`
	// LastActuation: the last pace notch on this provider, fleet-wide (one
	// notch per provider per tick across every Enforce spoke; for kiro,
	// KIRO_LAST_ACT — the budget never fits across it).
	// +optional
	LastActuation *metav1.Time `json:"lastActuation,omitempty"`
	// +optional
	ComputedAt *metav1.Time `json:"computedAt,omitempty"`
}

// UsageWindowStatus is one evaluated window. Numbers are decimal strings in
// the pool's unit, like the ladder's scores.
type UsageWindowStatus struct {
	Name  string       `json:"name"`
	Start *metav1.Time `json:"start,omitempty"`
	// +optional
	ResetsAt *metav1.Time `json:"resetsAt,omitempty"`
	Consumed string       `json:"consumed"`
	// +optional
	Limit string `json:"limit,omitempty"`
	// LimitSource: configured, learned, none.
	LimitSource string `json:"limitSource"`
	// Learned is the calibrated limit, kept even when a configured limit
	// wins, so the two can be compared.
	// +optional
	Learned string `json:"learned,omitempty"`
	// UsedPercent: the provider reading when fresh, else consumed/limit,
	// else -1 (unmeasured — never "exhausted").
	UsedPercent string `json:"usedPercent"`
	// ReadingPercent is the provider's own figure, -1 when absent/stale.
	ReadingPercent string `json:"readingPercent"`
	// +optional
	Remaining string `json:"remaining,omitempty"`
	// +optional
	BurnPerHour string `json:"burnPerHour,omitempty"`
	// +optional
	ExhaustionETA *metav1.Time `json:"exhaustionETA,omitempty"`
	// ReadingSource is where ReadingPercent came from: ccleft, configmap or
	// none.
	// +optional
	ReadingSource string `json:"readingSource,omitempty"`
	// ProviderWindow is the provider's own view of this window as ccleft
	// reported it, in the provider's unit (percent, credits, requests, usd) —
	// unlike Limit/Remaining above, which are in the pool's Unit.
	// +optional
	ProviderWindow *ProviderWindowStatus `json:"providerWindow,omitempty"`
}

// ProviderWindowStatus is one ccleft window. Numbers are decimal strings.
type ProviderWindowStatus struct {
	// ID is ccleft's window id (five_hour, seven_day, gemini-weekly, plan...).
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"`
	// +optional
	Scope string `json:"scope,omitempty"`
	Unit  string `json:"unit,omitempty"`
	// +optional
	Used string `json:"used,omitempty"`
	// +optional
	Limit string `json:"limit,omitempty"`
	// +optional
	Remaining string `json:"remaining,omitempty"`
	// +optional
	RemainingPercent string `json:"remainingPercent,omitempty"`
	// +optional
	ResetsAt *metav1.Time `json:"resetsAt,omitempty"`
}

// ProviderAccountStatus is the pool's account as ccleft last reported it.
type ProviderAccountStatus struct {
	// Source of the reading: ccleft, or configmap when ccleft was unusable.
	Source string `json:"source"`
	// +optional
	Provider string `json:"provider,omitempty"`
	// Account is ccleft's non-secret account fingerprint.
	// +optional
	Account string `json:"account,omitempty"`
	// State: ok, limited, exhausted, rate_limited, auth_required,
	// unsupported, error.
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Cause string `json:"cause,omitempty"`
	// +optional
	Plan string `json:"plan,omitempty"`
	// Stale: ccleft served its last good measurement during an upstream
	// failure (429 etc.); FetchedAt is that measurement's time.
	Stale bool `json:"stale"`
	// +optional
	FetchedAt *metav1.Time `json:"fetchedAt,omitempty"`
	// +optional
	RetryAt *metav1.Time `json:"retryAt,omitempty"`
	// Homes that ccleft resolved to this account.
	// +optional
	Homes []string `json:"homes,omitempty"`
	// Error explains why ccleft could not be used (the fallback reason).
	// +optional
	Error string `json:"error,omitempty"`
}

// AgentUsage is one agent's consumption in a pool window.
type AgentUsage struct {
	Agent  string `json:"agent"`
	Window string `json:"window"`
	// Consumed in the pool's unit.
	Consumed string `json:"consumed"`
	// Share of the pool's consumption in this window, 0-1.
	Share string `json:"share"`
	// Models seen, most-consuming first.
	// +optional
	Models []string `json:"models,omitempty"`
}

// UsageSourceStatus is one sidecar source as the controller last read it.
type UsageSourceStatus struct {
	Namespace   string `json:"namespace"`
	Source      string `json:"source"`
	Fingerprint string `json:"fingerprint,omitempty"`
	OK          bool   `json:"ok"`
	// Counted is false when another namespace's sidecar already supplied the
	// same store (same fingerprint): counting it again would double the pool.
	Counted bool `json:"counted"`
	Primed  bool `json:"primed"`
	// +optional
	Error string `json:"error,omitempty"`
	// +optional
	LastSuccess *metav1.Time `json:"lastSuccess,omitempty"`
	// +optional
	UnpricedModels []string `json:"unpricedModels,omitempty"`
}

// UsagePoolStatus is the measured state of the pool.
type UsagePoolStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Windows []UsageWindowStatus `json:"windows,omitempty"`
	// +optional
	Agents []AgentUsage `json:"agents,omitempty"`
	// +optional
	Sources []UsageSourceStatus `json:"sources,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// Account is the provider account behind the pool, from ccleft.
	// +optional
	Account *ProviderAccountStatus `json:"account,omitempty"`
	// RotationReading is what rotation plans from (HiveSpoke
	// rotationUsageSource UsagePool): ccleft first, the ConfigMap fallback.
	// +optional
	RotationReading *PoolRotationReading `json:"rotationReading,omitempty"`
	// PaceHistory is the pacer's sample history, oldest first, one sample
	// per line: "<ts> <slot> <pct> <reset|-> [<used> <limit>]".
	// +optional
	PaceHistory []string `json:"paceHistory,omitempty"`
	// Pace is the burn-rate verdict over PaceHistory.
	// +optional
	Pace *PoolPaceStatus `json:"pace,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pool
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Used%",type=string,JSONPath=`.status.windows[0].usedPercent`
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.status.windows[0].name`
// +kubebuilder:printcolumn:name="Limit",type=string,JSONPath=`.status.windows[0].limitSource`
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`

// UsagePool is one provider account's quota, measured from session logs.
type UsagePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UsagePoolSpec   `json:"spec,omitempty"`
	Status UsagePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UsagePoolList contains a list of UsagePool.
type UsagePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UsagePool `json:"items"`
}

func init() { SchemeBuilder.Register(&UsagePool{}, &UsagePoolList{}) }
