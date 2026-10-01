package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// HiveRelease makes the operator the single writer of the hive container image.
//
// WHY THIS EXISTS
// ---------------
// The nightly bash CronJob hive/hive-upgrade tracked `^v5\.` releases and
// decided "ahead or behind" from the hive.tunaos.org/version annotation. After
// the fleet moved to v6 (published only as the moving tags v6-latest/edge, no
// semver), that annotation was stale and the semver-only comparison read v6 as
// behind, so the job rolled every spoke back to v5 every night. Two writers of
// one field is the bug; this resource is the one writer.
//
// Rules carried over from that script, each learned from an incident:
//   - Deploy a digest, never a moving tag (repo:tag@sha256:…).
//   - Every node architecture must be in the image index before anything is
//     touched (an arm64-only image once took a Recreate deployment down).
//   - "Could not measure" is never "unhealthy": an exec/API error aborts and
//     reports, it never rolls back or blocklists.
//   - The rollback target is written in the same patch that changes the image.
//   - Preflight the current image first: a failure after the change proves
//     nothing if the target was already unhealthy.
//   - One target at a time, canary first, with a soak between targets.

// ReleaseTrack is how a desired image is chosen.
//
// A value starting with "^" is a regular expression over semver tags (for
// example `^v5\.`); the newest non-prerelease semver tag that matches wins.
// Anything else is a tag (for example `v6-latest`) resolved to its digest.
type ReleaseTrack string

// ReleasePhase is a target's position in the rollout state machine.
// +kubebuilder:validation:Enum=Current;Pending;Held;Rolling;Soaking;RolledBack;Failed;Drifted;Unknown
type ReleasePhase string

const (
	// PhaseCurrent: the target runs the desired digest.
	PhaseCurrent ReleasePhase = "Current"
	// PhasePending: the target differs from desired and will be rolled (Enforce)
	// or would be rolled (Shadow).
	PhasePending ReleasePhase = "Pending"
	// PhaseHeld: the target differs but is deliberately not changed (major
	// version change without opt-in, blocklisted desired, architecture gap).
	PhaseHeld ReleasePhase = "Held"
	// PhaseRolling: patched, waiting for the new pod and the health gate.
	PhaseRolling ReleasePhase = "Rolling"
	// PhaseSoaking: gate passed, being watched for Spec.Soak.
	PhaseSoaking ReleasePhase = "Soaking"
	// PhaseRolledBack: the gate failed and the previous image was restored.
	PhaseRolledBack ReleasePhase = "RolledBack"
	// PhaseFailed: could not verify, or the rollback itself did not recover.
	PhaseFailed ReleasePhase = "Failed"
	// PhaseDrifted: the image was changed by someone other than the operator.
	PhaseDrifted ReleasePhase = "Drifted"
	// PhaseUnknown: the target could not be read.
	PhaseUnknown ReleasePhase = "Unknown"
)

// ReleaseWindow restricts when a rollout may START. A rollout already in
// progress always finishes (or rolls back) outside the window.
type ReleaseWindow struct {
	// Start is HH:MM local time, inclusive.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`
	// End is HH:MM local time, exclusive. End before Start wraps midnight.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	End string `json:"end"`
	// TimeZone is an IANA zone name. Default UTC.
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
}

// ReleaseTarget is one Deployment whose image this release owns. Targets are
// rolled strictly in list order; the first is the canary.
type ReleaseTarget struct {
	// Name identifies the target in status and events.
	Name string `json:"name"`
	// Spoke names a HiveSpoke. Its namespace is used, with deployment "hive"
	// and container "hive", and the full spoke health gate applies (in-pod
	// /api/health plus the agent placement snapshot).
	// +optional
	Spoke string `json:"spoke,omitempty"`
	// Namespace, Deployment and Container address a target explicitly. They
	// override what Spoke implies.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Deployment string `json:"deployment,omitempty"`
	// +optional
	Container string `json:"container,omitempty"`
	// Image overrides Spec.Image for this target (e.g. ghcr.io/hivecommons/hive-hub).
	// +optional
	Image string `json:"image,omitempty"`
	// Track overrides Spec.Track for this target, so the hub can follow its own line.
	// +optional
	Track ReleaseTrack `json:"track,omitempty"`
	// Pin overrides Spec.Pin for this target.
	// +optional
	Pin string `json:"pin,omitempty"`
	// HealthURL is fetched by the operator (HTTP 200 required) as the health
	// gate for non-spoke targets such as the hub. Spoke targets are checked
	// in-pod instead.
	// +optional
	HealthURL string `json:"healthURL,omitempty"`
	// AllowMajorChange permits moving this target across a major version (v5→v6
	// or back). Without it, or a pin, such a change is Held. This is the guard
	// against the nightly v6→v5 rollback.
	// +optional
	AllowMajorChange bool `json:"allowMajorChange,omitempty"`
}

// HiveReleaseSpec is the desired image policy.
type HiveReleaseSpec struct {
	// Image is the default repository, e.g. ghcr.io/hivecommons/hive.
	Image string `json:"image"`
	// Track is a tag (v6-latest) or, when it starts with "^", a semver tag regex.
	Track ReleaseTrack `json:"track"`
	// Pin freezes the desired image: a tag, "sha256:<digest>" or
	// "tag@sha256:<digest>". A pin bypasses the blocklist and the major-version
	// guard, so pinning an older version is how the fleet is rolled back on
	// purpose (through the same canary, gate and soak). It applies to targets
	// that do not override image, track or pin.
	// +optional
	Pin string `json:"pin,omitempty"`
	// Targets in rollout order. The first is the canary.
	// +kubebuilder:validation:MinItems=1
	Targets []ReleaseTarget `json:"targets"`
	// Soak is how long each target is watched after its gate passes before the
	// next target is touched. Default 10m.
	// +optional
	Soak *metav1.Duration `json:"soak,omitempty"`
	// PollInterval is how often the registry is consulted. Default 1h.
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`
	// Cooldown blocks starting a new rollout after a rollback. Default 20h.
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`
	// Window restricts when a rollout may start. Omit for any time.
	// +optional
	Window *ReleaseWindow `json:"window,omitempty"`
	// Mode: Observe reports current images only; Shadow also computes and
	// reports the plan; Enforce applies it. Default Shadow.
	// +optional
	// +kubebuilder:default=Shadow
	// +kubebuilder:validation:Enum=Observe;Shadow;Enforce
	Mode ReconcileMode `json:"mode,omitempty"`
	// Blocklist holds digests (sha256:…) or version labels never to deploy.
	// Digests that fail the gate are added to Status.Blocklist automatically.
	// +optional
	Blocklist []string `json:"blocklist,omitempty"`
	// Suspend stops all evaluation. An in-progress rollout is left where it is
	// and resumes when unsuspended.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ResolvedImage is a track resolved against the registry.
type ResolvedImage struct {
	Repo string `json:"repo"`
	Tag  string `json:"tag,omitempty"`
	// Digest is the image index digest.
	Digest string `json:"digest"`
	// Version is a human label: the semver tag, or <line>-<revision12> for a
	// moving tag (v6-latest → v6-5d26aa998c80).
	Version string `json:"version,omitempty"`
	// Revision is the org.opencontainers.image.revision label.
	// +optional
	Revision string `json:"revision,omitempty"`
	// Platforms in the index, os/arch.
	// +optional
	Platforms []string `json:"platforms,omitempty"`
	// Pinned is true when this came from a pin.
	// +optional
	Pinned bool `json:"pinned,omitempty"`
}

// Image renders repo:tag@digest.
func (r ResolvedImage) Image() string {
	s := r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	return s + "@" + r.Digest
}

// AgentPlacementSnapshot is one agent's placement captured before a rollout.
type AgentPlacementSnapshot struct {
	Name    string `json:"name"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	// +optional
	Effort string `json:"effort,omitempty"`
	Paused bool   `json:"paused"`
}

// ReleaseTargetStatus is the observed state of one target.
type ReleaseTargetStatus struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	Container  string `json:"container,omitempty"`
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`
	// +optional
	CurrentDigest string `json:"currentDigest,omitempty"`
	// CurrentVersion is derived from the running image (tag, or registry
	// revision for moving tags), never from the version annotation alone.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`
	// +optional
	DesiredImage string `json:"desiredImage,omitempty"`
	// +optional
	DesiredVersion string       `json:"desiredVersion,omitempty"`
	Phase          ReleasePhase `json:"phase"`
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// AppliedImage is the last image the operator wrote. A different live image
	// is drift.
	// +optional
	AppliedImage string `json:"appliedImage,omitempty"`
	// PlacementsRestored lists agents whose backend/model the operator re-applied
	// after the image change reset them.
	// +optional
	PlacementsRestored []string `json:"placementsRestored,omitempty"`
}

// RolloutStep is where an active rollout is for its current target.
// +kubebuilder:validation:Enum=Preflight;Patching;Rolling;Soaking;RollingBack
type RolloutStep string

const (
	StepPreflight   RolloutStep = "Preflight"
	StepPatching    RolloutStep = "Patching"
	StepRolling     RolloutStep = "Rolling"
	StepSoaking     RolloutStep = "Soaking"
	StepRollingBack RolloutStep = "RollingBack"
)

// RolloutProgress is persisted so an operator restart resumes mid-rollout.
type RolloutProgress struct {
	StartedAt metav1.Time `json:"startedAt"`
	// Index into Spec.Targets of the target being worked on.
	Index int `json:"index"`
	// Target is the name at Index, to detect a reordered spec.
	Target string      `json:"target"`
	Step   RolloutStep `json:"step"`
	// StepSince is when Step was entered.
	StepSince metav1.Time `json:"stepSince"`
	// Image is what this target is being moved to.
	Image   string `json:"image"`
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
	// PreviousImage is the rollback target (digest-pinned).
	// +optional
	PreviousImage string `json:"previousImage,omitempty"`
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`
	// PreviousAnnotations are the version annotations before the change, so a
	// rollback restores them exactly.
	// +optional
	PreviousAnnotations map[string]string `json:"previousAnnotations,omitempty"`
	// Snapshot is every agent's placement before the change.
	// +optional
	Snapshot []AgentPlacementSnapshot `json:"snapshot,omitempty"`
	// BaselineRestarts is the container restart count when the gate passed.
	// +optional
	BaselineRestarts int32 `json:"baselineRestarts,omitempty"`
	// Failures counts consecutive measured soak failures (two fail the soak).
	// +optional
	Failures int `json:"failures,omitempty"`
	// Reason records why a rollback is in progress.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Done lists targets finished in this rollout.
	// +optional
	Done []string `json:"done,omitempty"`
}

// HiveReleaseStatus is the observed state.
type HiveReleaseStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Desired is Spec.Image/Track resolved. Targets with overrides carry their
	// own desired image in Targets[].
	// +optional
	Desired *ResolvedImage `json:"desired,omitempty"`
	// Resolved caches every distinct track resolution, keyed by repo|track|pin.
	// +optional
	Resolved map[string]ResolvedImage `json:"resolved,omitempty"`
	// +optional
	Targets []ReleaseTargetStatus `json:"targets,omitempty"`
	// Rollout is non-nil while a rollout is in progress.
	// +optional
	Rollout *RolloutProgress `json:"rollout,omitempty"`
	// Blocklist holds digests that failed the gate.
	// +optional
	Blocklist []string `json:"blocklist,omitempty"`
	// +optional
	LastCheck *metav1.Time `json:"lastCheck,omitempty"`
	// +optional
	LastRollbackAt *metav1.Time `json:"lastRollbackAt,omitempty"`
	// NextAttempt: no rollout starts before this. Set to LastRollbackAt +
	// Spec.Cooldown after a rollback or an unverifiable upgrade, and to one
	// poll interval after an aborted preflight. Clear it to retry sooner.
	// +optional
	NextAttempt *metav1.Time `json:"nextAttempt,omitempty"`
	// +optional
	LastResult string `json:"lastResult,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hrel
// +kubebuilder:printcolumn:name="Track",type=string,JSONPath=`.spec.track`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.status.desired.version`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Rolling",type=string,JSONPath=`.status.rollout.target`
// +kubebuilder:printcolumn:name="Result",type=string,JSONPath=`.status.lastResult`,priority=1

// HiveRelease owns the image version of a set of hive Deployments.
type HiveRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HiveReleaseSpec   `json:"spec,omitempty"`
	Status HiveReleaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HiveReleaseList contains a list of HiveRelease.
type HiveReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HiveRelease `json:"items"`
}

func init() { SchemeBuilder.Register(&HiveRelease{}, &HiveReleaseList{}) }
