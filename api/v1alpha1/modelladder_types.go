package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Rung is one placeable (provider, backend, model) at a tier.
type Rung struct {
	Tier     string `json:"tier"`
	Provider string `json:"provider"`
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	// Effort is part of the rung because inference quality and consumption
	// change materially with reasoning effort.
	// +optional
	Effort string `json:"effort,omitempty"`
	// +optional
	Score string `json:"score,omitempty"`
	// Source is where the rung came from: "benchmark" or "builtin".
	// +optional
	Source string `json:"source,omitempty"`
	// Available is false when the backend's own model list does not offer this
	// id. Such a rung must never be placed: an agent launched on an id the CLI
	// rejects dies at startup and reads as a dead agent, not a bad config.
	// +optional
	Available bool `json:"available,omitempty"`
}

// BandSpec maps a benchmark score to a tier.
//
// Bands are scale-specific and MUST be derived from the feed's own
// distribution. Carrying thresholds across benchmarks is how T1 came back empty:
// cut-offs taken from a suite whose field runs to 89.5 matched nothing on an
// index that tops out at 58.2, and every T1 agent would have stranded.
type BandSpec struct {
	Tier     string `json:"tier"`
	MinScore string `json:"minScore"`
}

// ModelLadderSpec describes how to build the placement ladder.
type ModelLadderSpec struct {
	// BenchmarkURL supplies the ordering signal.
	// +optional
	BenchmarkURL string `json:"benchmarkURL,omitempty"`
	// BenchmarkSecretRef names a Secret key holding the benchmark API key.
	// +optional
	BenchmarkSecretRef *SecretKeyRef `json:"benchmarkSecretRef,omitempty"`

	// InventoryConfigMapRef points at the live backend model inventory. When
	// omitted it defaults to hive/hive-model-inventory, key inventory.tsv.
	// +optional
	InventoryConfigMapRef *ConfigMapKeyRef `json:"inventoryConfigMapRef,omitempty"`

	// Bands map scores to tiers, highest first.
	// +optional
	Bands []BandSpec `json:"bands,omitempty"`

	// Builtin rungs are the fallback and the UNION partner — not merely a
	// fallback. A benchmark feed does not cover every provider a fleet runs
	// (measured: zero DeepSeek entries, and the only Google entries were models
	// the CLI does not offer), so letting the feed REPLACE this list deletes
	// whole providers from the ladder the moment a refresh succeeds.
	// +optional
	Builtin []Rung `json:"builtin,omitempty"`

	// +optional
	// +kubebuilder:default=Shadow
	Mode ReconcileMode `json:"mode,omitempty"`
}

// ConfigMapKeyRef points at one key in a ConfigMap.
type ConfigMapKeyRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

// SecretKeyRef points at one key in a Secret.
type SecretKeyRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

// ModelLadderStatus is the computed ladder.
type ModelLadderStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Effective is rank(benchmark) UNION builtin, then gated by the live
	// inventory. De-duplicated on provider+model+effort: the same model at low
	// and high effort is a different capacity and cost choice.
	// +optional
	Effective []Rung `json:"effective,omitempty"`
	// Dropped rungs and why — the audit trail for "why is nothing at T1".
	// +optional
	Dropped []string `json:"dropped,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ladder
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`

// ModelLadder is the capability ladder agents are placed on.
type ModelLadder struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelLadderSpec   `json:"spec,omitempty"`
	Status ModelLadderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelLadderList contains a list of ModelLadder.
type ModelLadderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelLadder `json:"items"`
}

func init() { SchemeBuilder.Register(&ModelLadder{}, &ModelLadderList{}) }
