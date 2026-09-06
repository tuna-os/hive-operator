package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ReconcileMode controls how far a controller is allowed to go.
//
// Every controller starts at Shadow and graduates. This is not caution for its
// own sake: the hive scripts warn in capitals that two control planes issuing
// the same mutations is worse than none, and a shadow pass is the only way to
// diff a new decision engine against the one already running.
// +kubebuilder:validation:Enum=Observe;Shadow;Enforce
type ReconcileMode string

const (
	// ModeObserve reads state and emits metrics only. It never computes actions.
	ModeObserve ReconcileMode = "Observe"
	// ModeShadow computes the full action set and records it — as events, logs
	// and metrics — without applying any of it. Diff this against the CronJobs.
	ModeShadow ReconcileMode = "Shadow"
	// ModeEnforce applies actions. Only move a controller here once its shadow
	// output has matched the incumbent, and suspend the incumbent CronJob in the
	// same change.
	ModeEnforce ReconcileMode = "Enforce"
)

// SharedAuthSpec describes one credential store shared across spokes.
//
// A single device-flow login should cover the whole fleet. Assembling that from
// hostPath PVs and symlinks has failed silently three distinct ways, none of
// which are visible by inspection — hence VerifyByWriteThrough, which is not
// optional.
type SharedAuthSpec struct {
	// Namespaces expected to see the same storage.
	Namespaces []string `json:"namespaces"`

	// PrimaryNamespace is where the probe marker is written.
	PrimaryNamespace string `json:"primaryNamespace"`

	// AgentHome is the agents' home directory inside the hive pod.
	//
	// Spell it out. Do NOT use $HOME: `kubectl exec` runs as root and $HOME is
	// /root, so every check silently inspects the wrong directory and reports a
	// healthy fleet as broken — and a broken one as repaired.
	// +optional
	// +kubebuilder:default="/data/home"
	AgentHome string `json:"agentHome,omitempty"`

	// Dirs under AgentHome that must resolve to the same storage everywhere.
	// +optional
	// +kubebuilder:default={".claude",".gemini",".codex"}
	Dirs []string `json:"dirs,omitempty"`

	// Mode gates repairs.
	// +optional
	// +kubebuilder:default=Shadow
	Mode ReconcileMode `json:"mode,omitempty"`

	// RepairPermissions regroups and group-writes the shared dirs. Token refresh
	// REWRITES the credential file; without group write the refresh fails and
	// presents as an expired subscription.
	// +optional
	// +kubebuilder:default=true
	RepairPermissions bool `json:"repairPermissions,omitempty"`

	// RepairTheme sets a theme in .claude.json when unset, and creates the file
	// when missing. A null theme leaves hasCompletedOnboarding true while the
	// CLI stops at the theme picker on every launch, and the watchdog
	// kill+restarts it forever.
	// +optional
	// +kubebuilder:default=true
	RepairTheme bool `json:"repairTheme,omitempty"`
}

// SharedAuthNamespaceStatus is the per-namespace verification result.
type SharedAuthNamespaceStatus struct {
	Namespace string `json:"namespace"`
	// Shared is the result of an actual write-through test, never of an `ls`.
	// The same hostPath string can resolve to a different filesystem for a
	// freshly created mount, and the wrong one looks perfectly plausible.
	Shared map[string]bool `json:"shared,omitempty"`
	// +optional
	CredentialPresent bool `json:"credentialPresent,omitempty"`
	// +optional
	ThemeSet bool `json:"themeSet,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// SharedAuthStatus is the observed state.
type SharedAuthStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Namespaces []SharedAuthNamespaceStatus `json:"namespaces,omitempty"`
	// +optional
	Consistent bool `json:"consistent"`
	// PendingRepairs is what Enforce mode would do. In Shadow this is the whole
	// output of the controller.
	// +optional
	PendingRepairs []string `json:"pendingRepairs,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sauth
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Consistent",type=boolean,JSONPath=`.status.consistent`
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`

// SharedAuth is one credential store shared across spokes.
type SharedAuth struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SharedAuthSpec   `json:"spec,omitempty"`
	Status SharedAuthStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SharedAuthList contains a list of SharedAuth.
type SharedAuthList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SharedAuth `json:"items"`
}

func init() { SchemeBuilder.Register(&SharedAuth{}, &SharedAuthList{}) }
