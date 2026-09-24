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
	// Windows are the provider's quota windows.
	Windows []UsageWindowSpec `json:"windows"`
	// Reading is the provider-reported "% used" used for calibration and,
	// while it exists, as the authoritative UsedPercent. Transitional: today
	// it is the bash probe's ConfigMap; see DESIGN.md for retiring it.
	// +optional
	Reading *ProbeConfigMapRef `json:"reading,omitempty"`
	// SidecarPort is where hive-usage listens in each hive pod. Default 9464.
	// +optional
	SidecarPort int32 `json:"sidecarPort,omitempty"`
	// Mode: only Observe is meaningful — a pool measures, it never acts.
	// Rotation consumes pools; see HiveSpoke.spec.rotationUsageSource.
	// +optional
	// +kubebuilder:default=Observe
	Mode ReconcileMode `json:"mode,omitempty"`
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
