package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HiveHousekeepingSpec is the desired state of the hive-ops CronJobs that stay
// shell: the data collectors and small reconcilers (tiers, inventory, pi-kiro,
// cli-update, repo-sync, metrics, activity, shared-auth) that a Go rewrite
// would not make better, only different.
//
// The controller RENDERS each job into a CronJob from Template + the job
// entry, diffs it against the live object and — in Enforce — adopts it
// (ownerReference + label, a metadata-only patch that keeps the CronJob's
// UID and so its Job history) and converges any drift back. The scripts stay
// in the scripts ConfigMap, which the controller hashes but never writes: it
// is shared with the rotate/watchdog/pace/nudge jobs and its live copy, not
// git, is the authoritative one (docs/housekeeping.md).
type HiveHousekeepingSpec struct {
	// Namespace the CronJobs live in.
	// +optional
	// +kubebuilder:default=hive
	Namespace string `json:"namespace,omitempty"`

	// Mode gates every write.
	//   Observe: report presence and script hashes only, no diff.
	//   Shadow:  render and diff; status says what Enforce would do.
	//   Enforce: adopt, create, converge drift, retire.
	// +optional
	// +kubebuilder:default=Shadow
	Mode ReconcileMode `json:"mode,omitempty"`

	// ScriptsConfigMap holds the scripts (mounted at /scripts). Observed only.
	// +optional
	// +kubebuilder:default=hive-ops-scripts
	ScriptsConfigMap string `json:"scriptsConfigMap,omitempty"`

	// Template is what every job shares.
	Template HousekeepingTemplate `json:"template"`

	// Jobs, one CronJob each. A CronJob this object owns that is no longer
	// listed is deleted in Enforce; one it does not own is never touched.
	// +listType=map
	// +listMapKey=name
	Jobs []HousekeepingJob `json:"jobs"`

	// Retire lists legacy CronJobs (NOT rendered by this object) to delete.
	// Gated: only a CronJob that is suspended and has no active Job is
	// deleted, and only in Enforce. Use it for the per-spoke rotate/watchdog
	// jobs once the operator owns that spoke's rotation and liveness.
	// +optional
	Retire []RetiredCronJob `json:"retire,omitempty"`
}

// HousekeepingTemplate is the pod and CronJob shape every hive-ops job shares.
type HousekeepingTemplate struct {
	// +kubebuilder:default="alpine/k8s:1.37.1"
	Image string `json:"image,omitempty"`
	// +optional
	// +kubebuilder:default=IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// +optional
	// +kubebuilder:default=hive-ops
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// StateClaimName is the PVC mounted at /state (the bash journals).
	// +optional
	// +kubebuilder:default=hive-ops-state
	StateClaimName string `json:"stateClaimName,omitempty"`
	// Labels go on the CronJob and its pod template, plus
	// app.kubernetes.io/component=<job component>.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Env is the container env unless a job sets its own (which REPLACES it,
	// so the order of the live objects can be reproduced exactly).
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	// +kubebuilder:default=Forbid
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
	// +optional
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`
	// +optional
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// HousekeepingJob is one CronJob: `bash /scripts/<script> <args...>`.
type HousekeepingJob struct {
	// Name of the CronJob.
	Name string `json:"name"`
	// Component label; defaults to Name without its "hive-" prefix.
	// +optional
	Component string `json:"component,omitempty"`
	Schedule  string `json:"schedule"`
	// +optional
	TimeZone *string `json:"timeZone,omitempty"`
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// Script is the key in the scripts ConfigMap.
	Script string `json:"script"`
	// +optional
	Args []string `json:"args,omitempty"`
	// Env replaces Template.Env when set.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Resources replaces Template.Resources when set.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// Mode overrides spec.mode for this job (promote one job at a time).
	// +optional
	Mode ReconcileMode `json:"mode,omitempty"`
}

// RetiredCronJob names a legacy CronJob to delete once it is safe to.
type RetiredCronJob struct {
	Name string `json:"name"`
	// Reason is recorded in status (why it is safe to delete).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// HousekeepingJobStatus is one job's observed state.
type HousekeepingJobStatus struct {
	Name string `json:"name"`
	// State: InSync, Drifted, Missing, Unadopted (exists, not owned yet),
	// Conflict (owned by another controller), Pruned.
	State string `json:"state"`
	// Owned is true once the CronJob carries this object's ownerReference.
	// +optional
	Owned bool `json:"owned,omitempty"`
	// Drift lists the fields where live differs from the render.
	// +optional
	Drift []string `json:"drift,omitempty"`
	// Action is what this reconcile did (Enforce) or would do (Shadow).
	// +optional
	Action string `json:"action,omitempty"`
	// Script SHA-256 (first 12 hex) of the ConfigMap key; "missing" if absent.
	// +optional
	Script string `json:"script,omitempty"`
	// +optional
	Suspended bool `json:"suspended,omitempty"`
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
}

// RetiredCronJobStatus is one retirement's state.
type RetiredCronJobStatus struct {
	Name string `json:"name"`
	// State: Gone, Blocked (not suspended / active jobs), Deleted, WouldDelete.
	State string `json:"state"`
	// +optional
	Message string `json:"message,omitempty"`
}

// HiveHousekeepingStatus is the observed state.
type HiveHousekeepingStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Jobs []HousekeepingJobStatus `json:"jobs,omitempty"`
	// +optional
	Retired []RetiredCronJobStatus `json:"retired,omitempty"`
	// PendingActions is what Enforce would do. Empty when in sync.
	// +optional
	PendingActions []string `json:"pendingActions,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hhk
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="InSync")].status`
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`

// HiveHousekeeping owns the hive-ops CronJobs that remain shell scripts.
type HiveHousekeeping struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HiveHousekeepingSpec   `json:"spec,omitempty"`
	Status HiveHousekeepingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HiveHousekeepingList contains a list of HiveHousekeeping.
type HiveHousekeepingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HiveHousekeeping `json:"items"`
}

func init() { SchemeBuilder.Register(&HiveHousekeeping{}, &HiveHousekeepingList{}) }
