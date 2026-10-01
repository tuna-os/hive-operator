package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Window.TimeZone must work in a distroless image.

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/registry"
)

const releaseController = "hiverelease"

// Annotations written on every managed Deployment, compatible with the ones
// hive-upgrade.sh wrote so a manual rollback recipe keeps working.
const (
	AnnVersion        = "hive.tunaos.org/version"
	AnnPreviousImage  = "hive.tunaos.org/previous-image"
	AnnPreviousVer    = "hive.tunaos.org/previous-version"
	AnnUpgradedAt     = "hive.tunaos.org/upgraded-at"
	AnnRolledBackFrom = "hive.tunaos.org/rolled-back-from"
	AnnRolledBackAt   = "hive.tunaos.org/rolled-back-at"
	AnnManagedBy      = "hive.tunaos.org/managed-by"
	managedByValue    = "hive-operator"
)

// versionAnnotations are captured before a change and restored by a rollback.
var versionAnnotations = []string{AnnVersion, AnnPreviousImage, AnnPreviousVer, AnnUpgradedAt}

// Condition types.
const (
	CondReady    = "Ready"
	CondDegraded = "Degraded"
)

// eventRecorder is the subset of events.EventRecorder used here.
type eventRecorder interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{})
}

// HiveReleaseReconciler owns the hive container image of every target.
//
// The reconcile is a persisted state machine: each call does at most one step
// of the active rollout, writes Status.Rollout, and requeues. Nothing blocks
// inside Reconcile, and an operator restart resumes exactly where Status says.
type HiveReleaseReconciler struct {
	client.Client
	// APIReader reads Deployments, Pods and Nodes uncached, so a step never
	// judges a Deployment it just patched from a stale cache.
	APIReader client.Reader
	Registry  registry.Client
	Probe     HiveProbe
	HTTP      HTTPProber
	Recorder  eventRecorder
	Now       func() time.Time

	// RolloutTimeout bounds waiting for the new pod (default 7m, as the script).
	RolloutTimeout time.Duration
	// VerifyTimeout bounds retrying the health gate once the rollout is done (default 3m).
	VerifyTimeout time.Duration
	// StepInterval is the requeue while rolling (default 15s).
	StepInterval time.Duration
	// SoakInterval is the re-check cadence while soaking (default 60s).
	SoakInterval time.Duration
}

// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivereleases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=hivereleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *HiveReleaseReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func durOr(d *metav1.Duration, def time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return def
	}
	return d.Duration
}

func orDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (r *HiveReleaseReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *HiveReleaseReconciler) event(hr *hivev1.HiveRelease, typ, reason, msg string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(hr, nil, typ, reason, reason, msg, args...)
	}
}

// target is a ReleaseTarget with everything resolved.
type target struct {
	spec       hivev1.ReleaseTarget
	index      int
	ns, deploy string
	container  string
	repo       string
	track      hivev1.ReleaseTrack
	pin        string
	spoke      bool
	err        error
}

func (t target) key() string { return t.repo + "|" + string(t.track) + "|" + t.pin }

func (r *HiveReleaseReconciler) targets(ctx context.Context, hr *hivev1.HiveRelease) []target {
	out := make([]target, 0, len(hr.Spec.Targets))
	for i, ts := range hr.Spec.Targets {
		t := target{spec: ts, index: i, ns: ts.Namespace, deploy: ts.Deployment, container: ts.Container,
			repo: ts.Image, track: ts.Track, pin: ts.Pin}
		if t.repo == "" {
			t.repo = hr.Spec.Image
		}
		if t.track == "" {
			t.track = hr.Spec.Track
		}
		if ts.Pin == "" && ts.Image == "" && ts.Track == "" {
			// Spec.Pin applies to targets that follow the default image line.
			t.pin = hr.Spec.Pin
		}
		if ts.Spoke != "" {
			t.spoke = true
			var sp hivev1.HiveSpoke
			if err := r.Get(ctx, client.ObjectKey{Name: ts.Spoke}, &sp); err != nil {
				t.err = fmt.Errorf("HiveSpoke %s: %w", ts.Spoke, err)
			} else if t.ns == "" {
				t.ns = sp.Spec.Namespace
			}
			if t.deploy == "" {
				t.deploy = "hive"
			}
			if t.container == "" {
				t.container = "hive"
			}
		}
		if t.err == nil && (t.ns == "" || t.deploy == "" || t.container == "") {
			t.err = errors.New("target needs spoke, or namespace + deployment + container")
		}
		out = append(out, t)
	}
	return out
}

// Reconcile drives one step.
func (r *HiveReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var hr hivev1.HiveRelease
	// Uncached: the rollout step lives in status, and acting on a stale step
	// could patch a Deployment twice or record the wrong rollback target.
	if err := r.reader().Get(ctx, req.NamespacedName, &hr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	mode := hr.Spec.Mode
	if mode == "" {
		mode = hivev1.ModeShadow
	}
	metrics.SetMode(releaseController, hr.Name, string(mode))
	poll := durOr(hr.Spec.PollInterval, time.Hour)
	now := r.now()
	st := &hr.Status

	if hr.Spec.Suspend {
		r.setCond(&hr, CondReady, metav1.ConditionFalse, "Suspended", "spec.suspend is true; nothing is evaluated")
		return ctrl.Result{RequeueAfter: poll}, r.persist(ctx, &hr)
	}

	tgts := r.targets(ctx, &hr)

	// Resolve desired images. Never mid-rollout: one rollout deploys one
	// resolution, or a moving tag could change under a half-rolled fleet.
	needResolve := st.Rollout == nil && (st.LastCheck == nil || now.Sub(st.LastCheck.Time) >= poll ||
		st.ObservedGeneration != hr.Generation)
	for _, t := range tgts {
		if _, ok := st.Resolved[t.key()]; !ok && t.err == nil && st.Rollout == nil {
			needResolve = true
		}
	}
	nodeArches, archErr := r.nodeArches(ctx)
	if needResolve {
		fresh := map[string]hivev1.ResolvedImage{}
		var errs []string
		for _, t := range tgts {
			if t.err != nil {
				continue
			}
			if _, done := fresh[t.key()]; done {
				continue
			}
			img, err := r.resolve(ctx, &hr, t)
			if err != nil {
				errs = append(errs, err.Error())
				if old, ok := st.Resolved[t.key()]; ok {
					fresh[t.key()] = old // keep the last good answer; never guess
				}
				continue
			}
			fresh[t.key()] = img
		}
		st.Resolved = fresh
		nowT := metav1.NewTime(now)
		st.LastCheck = &nowT
		st.ObservedGeneration = hr.Generation
		if len(errs) > 0 {
			r.setCond(&hr, "RegistryReachable", metav1.ConditionFalse, "ResolveFailed", strings.Join(errs, "; "))
		} else {
			r.setCond(&hr, "RegistryReachable", metav1.ConditionTrue, "Resolved", "all tracks resolved")
		}
	}
	if d, ok := st.Resolved[hr.Spec.Image+"|"+string(hr.Spec.Track)+"|"+hr.Spec.Pin]; ok {
		dd := d
		st.Desired = &dd
	}

	// Observe every target and compute its phase.
	plans := r.observe(ctx, &hr, tgts, nodeArches, archErr, needResolve)

	res := ctrl.Result{RequeueAfter: poll}
	switch mode {
	case hivev1.ModeEnforce:
		var err error
		res, err = r.enforce(ctx, &hr, tgts, plans, poll)
		if err != nil {
			logger.Error(err, "release step failed")
			metrics.ReconcileErrors.WithLabelValues(releaseController, hr.Name).Inc()
		}
	default:
		r.shadow(&hr, plans, mode)
	}
	for i := range st.Targets {
		metrics.SetReleasePhase(hr.Name, st.Targets[i].Name, string(st.Targets[i].Phase))
	}
	if err := r.persist(ctx, &hr); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *HiveReleaseReconciler) persist(ctx context.Context, hr *hivev1.HiveRelease) error {
	if err := r.Status().Update(ctx, hr); err != nil {
		if apierrors.IsConflict(err) {
			return err // requeued with backoff; the next pass re-reads
		}
		return err
	}
	return nil
}

func (r *HiveReleaseReconciler) setCond(hr *hivev1.HiveRelease, typ string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&hr.Status.Conditions, metav1.Condition{
		Type: typ, Status: s, Reason: reason, Message: truncate(msg, 1000), ObservedGeneration: hr.Generation,
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (r *HiveReleaseReconciler) nodeArches(ctx context.Context) ([]string, error) {
	var nodes corev1.NodeList
	if err := r.reader().List(ctx, &nodes); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, n := range nodes.Items {
		if a := n.Status.NodeInfo.Architecture; a != "" {
			set[a] = true
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// ── desired-image resolution ────────────────────────────────────────────────

func (r *HiveReleaseReconciler) blocked(hr *hivev1.HiveRelease, digest, version string) bool {
	for _, l := range [][]string{hr.Spec.Blocklist, hr.Status.Blocklist} {
		for _, b := range l {
			if b != "" && (b == digest || b == version) {
				return true
			}
		}
	}
	return false
}

// VersionLabel names a resolved image for humans: the semver tag itself, or
// for a moving tag the line plus the source revision (v6-latest → v6-5d26aa998c80),
// matching the hive.tunaos.org/version values already on the cluster.
func VersionLabel(tag, revision, digest string) string {
	if _, ok := registry.ParseSemver(tag); ok {
		return tag
	}
	rev := revision
	if len(rev) > 12 {
		rev = rev[:12]
	}
	line := strings.TrimSuffix(tag, "-latest")
	switch {
	case rev != "" && line != "":
		return line + "-" + rev
	case rev != "":
		return "rev-" + rev
	case tag != "":
		return tag + "@" + shortDigest(digest)
	default:
		return shortDigest(digest)
	}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func toResolved(img registry.Image, pinned bool) hivev1.ResolvedImage {
	return hivev1.ResolvedImage{Repo: img.Repo, Tag: img.Tag, Digest: img.Digest, Revision: img.Revision,
		Platforms: img.Platforms, Version: VersionLabel(img.Tag, img.Revision, img.Digest), Pinned: pinned}
}

func (r *HiveReleaseReconciler) resolve(ctx context.Context, hr *hivev1.HiveRelease, t target) (hivev1.ResolvedImage, error) {
	if r.Registry == nil {
		return hivev1.ResolvedImage{}, errors.New("no registry client")
	}
	if p := t.pin; p != "" {
		tag, dig := p, ""
		if i := strings.Index(p, "@"); i >= 0 {
			tag, dig = p[:i], p[i+1:]
		} else if strings.HasPrefix(p, "sha256:") {
			tag, dig = "", p
		}
		ref := tag
		if dig != "" {
			ref = dig
		}
		img, err := r.Registry.Resolve(ctx, t.repo, ref)
		if err != nil {
			return hivev1.ResolvedImage{}, fmt.Errorf("pin %s:%s: %w", t.repo, p, err)
		}
		img.Tag = tag
		return toResolved(img, true), nil
	}
	tr := string(t.track)
	if strings.HasPrefix(tr, "^") {
		re, err := regexp.Compile(tr)
		if err != nil {
			return hivev1.ResolvedImage{}, fmt.Errorf("track %q: %w", tr, err)
		}
		tags, err := r.Registry.Tags(ctx, t.repo)
		if err != nil {
			return hivev1.ResolvedImage{}, fmt.Errorf("%s tags: %w", t.repo, err)
		}
		cands := registry.NewestMatching(tags, re)
		if len(cands) == 0 {
			return hivev1.ResolvedImage{}, fmt.Errorf("%s: no tag matches %s", t.repo, tr)
		}
		for i, tag := range cands {
			if i >= 10 {
				break
			}
			img, err := r.Registry.Resolve(ctx, t.repo, tag)
			if errors.Is(err, registry.ErrNotFound) {
				continue
			}
			if err != nil {
				return hivev1.ResolvedImage{}, err
			}
			res := toResolved(img, false)
			if r.blocked(hr, res.Digest, res.Version) {
				continue
			}
			return res, nil
		}
		return hivev1.ResolvedImage{}, fmt.Errorf("%s: every recent %s release is blocklisted or unpublished", t.repo, tr)
	}
	img, err := r.Registry.Resolve(ctx, t.repo, tr)
	if err != nil {
		return hivev1.ResolvedImage{}, fmt.Errorf("%s:%s: %w", t.repo, tr, err)
	}
	return toResolved(img, false), nil
}

// ── observation and planning ───────────────────────────────────────────────

// plan is one target's computed position.
type plan struct {
	t       target
	dep     *appsv1.Deployment
	desired *hivev1.ResolvedImage
	// change is true when this target should be rolled now.
	change bool
}

func splitImage(img string) (repo, tag, digest string) {
	rest := img
	if i := strings.Index(rest, "@"); i >= 0 {
		digest = rest[i+1:]
		rest = rest[:i]
	}
	slash := strings.LastIndex(rest, "/")
	if c := strings.LastIndex(rest, ":"); c > slash {
		tag = rest[c+1:]
		rest = rest[:c]
	}
	return rest, tag, digest
}

var majorRe = regexp.MustCompile(`^v?(\d+)(?:[.-]|$)`)

// MajorOf returns the major version of a tag or version label (v6-latest → 6,
// v5.105.10 → 5), or -1 when it cannot tell.
func MajorOf(v string) int {
	m := majorRe.FindStringSubmatch(v)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func containerImage(dep *appsv1.Deployment, name string) (string, bool) {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c.Image, true
		}
	}
	return "", false
}

func (r *HiveReleaseReconciler) statusFor(hr *hivev1.HiveRelease, name string) *hivev1.ReleaseTargetStatus {
	for i := range hr.Status.Targets {
		if hr.Status.Targets[i].Name == name {
			return &hr.Status.Targets[i]
		}
	}
	hr.Status.Targets = append(hr.Status.Targets, hivev1.ReleaseTargetStatus{Name: name})
	return &hr.Status.Targets[len(hr.Status.Targets)-1]
}

func setPhase(ts *hivev1.ReleaseTargetStatus, p hivev1.ReleasePhase, msg string, now time.Time) {
	if ts.Phase != p || ts.Since == nil {
		t := metav1.NewTime(now)
		ts.Since = &t
	}
	ts.Phase = p
	ts.Message = truncate(msg, 500)
}

func (r *HiveReleaseReconciler) observe(ctx context.Context, hr *hivev1.HiveRelease, tgts []target,
	arches []string, archErr error, refreshVersions bool) []plan {
	now := r.now()
	plans := make([]plan, len(tgts))
	// Drop status rows for targets no longer in spec, keep order of spec.
	ordered := make([]hivev1.ReleaseTargetStatus, 0, len(tgts))
	for _, t := range tgts {
		ordered = append(ordered, *r.statusFor(hr, t.spec.Name))
	}
	hr.Status.Targets = ordered

	active := ""
	if hr.Status.Rollout != nil {
		active = hr.Status.Rollout.Target
	}
	for i, t := range tgts {
		ts := &hr.Status.Targets[i]
		plans[i] = plan{t: t}
		ts.Namespace, ts.Deployment, ts.Container = t.ns, t.deploy, t.container
		if t.err != nil {
			setPhase(ts, hivev1.PhaseUnknown, t.err.Error(), now)
			continue
		}
		var dep appsv1.Deployment
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: t.ns, Name: t.deploy}, &dep); err != nil {
			setPhase(ts, hivev1.PhaseUnknown, "cannot read deployment: "+err.Error(), now)
			continue
		}
		plans[i].dep = &dep
		cur, ok := containerImage(&dep, t.container)
		if !ok {
			setPhase(ts, hivev1.PhaseUnknown, "container "+t.container+" not found", now)
			continue
		}
		curRepo, curTag, curDigest := splitImage(cur)
		if d, ok := hr.Status.Resolved[t.key()]; ok {
			dd := d
			plans[i].desired = &dd
			ts.DesiredImage, ts.DesiredVersion = d.Image(), d.Version
		}
		desired := plans[i].desired

		// Current version comes from the running image, never from the
		// version annotation alone: a stale annotation is exactly how the bash
		// job concluded v6 was behind v5.
		switch {
		case desired != nil && curDigest != "" && curDigest == desired.Digest:
			ts.CurrentVersion = desired.Version
		case curDigest != "" && curDigest == ts.CurrentDigest && ts.CurrentVersion != "" && !refreshVersions:
			// unchanged; keep the cached label
		default:
			ts.CurrentVersion = VersionLabel(curTag, "", curDigest)
			if _, semver := registry.ParseSemver(curTag); !semver && curDigest != "" && r.Registry != nil {
				if img, err := r.Registry.Resolve(ctx, curRepo, curDigest); err == nil {
					ts.CurrentVersion = VersionLabel(curTag, img.Revision, curDigest)
				}
			}
		}
		ts.CurrentImage, ts.CurrentDigest = cur, curDigest

		if t.spec.Name == active {
			continue // the rollout engine owns this row's phase
		}
		if desired == nil {
			setPhase(ts, hivev1.PhaseUnknown, "desired image not resolved yet", now)
			continue
		}
		// A rollback or unverified upgrade stays visible for the cooldown, as
		// long as nobody has changed the image since.
		if (ts.Phase == hivev1.PhaseRolledBack || ts.Phase == hivev1.PhaseFailed) && cur == ts.AppliedImage &&
			hr.Status.NextAttempt != nil && now.Before(hr.Status.NextAttempt.Time) {
			continue
		}
		if curDigest == desired.Digest {
			ts.AppliedImage = cur // adopt: from here on any other image is drift
			if ts.Phase != hivev1.PhaseCurrent {
				setPhase(ts, hivev1.PhaseCurrent, "running "+desired.Version, now)
			}
			continue
		}
		drifted := ts.AppliedImage != "" && cur != ts.AppliedImage
		why := ""
		curMajor, desMajor := MajorOf(curTag), MajorOf(desired.Version)
		if curMajor < 0 {
			curMajor = MajorOf(ts.CurrentVersion)
		}
		if drifted {
			// Judge the major line against what the operator last applied, not
			// the out-of-band image: converging an unwanted v6→v5 swap back to
			// v6 is the whole point of being the single writer.
			_, appliedTag, _ := splitImage(ts.AppliedImage)
			curMajor = MajorOf(appliedTag)
		}
		switch {
		case !desired.Pinned && r.blocked(hr, desired.Digest, desired.Version):
			why = "desired " + desired.Version + " is blocklisted; holding until the track moves"
		case archErr != nil:
			why = "cannot read node architectures: " + archErr.Error()
		case missingArch(desired.Platforms, arches) != "":
			why = "image index lacks linux/" + missingArch(desired.Platforms, arches)
		case !desired.Pinned && !t.spec.AllowMajorChange && (curMajor < 0 || desMajor < 0 || curMajor != desMajor):
			why = fmt.Sprintf("major version change %s → %s needs allowMajorChange or a pin", ts.CurrentVersion, desired.Version)
		case !desired.Pinned && strings.HasPrefix(string(t.track), "^") && semverAhead(curTag, desired.Tag) && !r.blocked(hr, curDigest, curTag):
			why = fmt.Sprintf("running %s, ahead of %s; not downgrading without a pin", curTag, desired.Tag)
		}
		if why != "" {
			p := hivev1.PhaseHeld
			if drifted {
				p = hivev1.PhaseDrifted
				why = "image changed out of band from " + ts.AppliedImage + "; " + why
			}
			setPhase(ts, p, why, now)
			continue
		}
		plans[i].change = true
		if drifted {
			setPhase(ts, hivev1.PhaseDrifted, fmt.Sprintf("image changed out of band from %s to %s; will converge to %s", ts.AppliedImage, cur, desired.Version), now)
		} else {
			setPhase(ts, hivev1.PhasePending, fmt.Sprintf("%s → %s", ts.CurrentVersion, desired.Version), now)
		}
	}
	return plans
}

func semverAhead(cur, des string) bool {
	a, ok1 := registry.ParseSemver(cur)
	b, ok2 := registry.ParseSemver(des)
	return ok1 && ok2 && b.Less(a)
}

func missingArch(platforms, arches []string) string {
	if len(platforms) == 0 {
		return ""
	}
	var miss []string
	for _, a := range arches {
		found := false
		for _, p := range platforms {
			if p == "linux/"+a {
				found = true
			}
		}
		if !found {
			miss = append(miss, a)
		}
	}
	return strings.Join(miss, ",")
}

// shadow reports the plan without touching anything.
func (r *HiveReleaseReconciler) shadow(hr *hivev1.HiveRelease, plans []plan, mode hivev1.ReconcileMode) {
	var would []string
	for i, p := range plans {
		if !p.change {
			continue
		}
		ts := &hr.Status.Targets[i]
		would = append(would, fmt.Sprintf("%s %s → %s", ts.Name, ts.CurrentVersion, p.desired.Version))
		if mode == hivev1.ModeShadow {
			metrics.Action(releaseController, hr.Name, "rollout", false)
		}
	}
	msg := "fleet current"
	if len(would) > 0 {
		msg = string(mode) + ": would roll in order: " + strings.Join(would, ", ")
	}
	if hr.Status.LastResult != msg && mode == hivev1.ModeShadow && len(would) > 0 {
		r.event(hr, corev1.EventTypeNormal, "WouldRollout", "%s", msg)
	}
	hr.Status.LastResult = msg
	r.setCond(hr, CondReady, metav1.ConditionTrue, string(mode), msg)
}

// ── enforcement: the rollout state machine ─────────────────────────────────

func inWindow(w *hivev1.ReleaseWindow, now time.Time) (bool, time.Duration, error) {
	if w == nil {
		return true, 0, nil
	}
	loc := time.UTC
	if w.TimeZone != "" {
		l, err := time.LoadLocation(w.TimeZone)
		if err != nil {
			return false, 0, err
		}
		loc = l
	}
	parse := func(s string) (int, error) {
		t, err := time.Parse("15:04", s)
		if err != nil {
			return 0, err
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	s, err := parse(w.Start)
	if err != nil {
		return false, 0, err
	}
	e, err := parse(w.End)
	if err != nil {
		return false, 0, err
	}
	l := now.In(loc)
	cur := l.Hour()*60 + l.Minute()
	in := (s <= e && cur >= s && cur < e) || (s > e && (cur >= s || cur < e))
	if in {
		return true, 0, nil
	}
	wait := s - cur
	if wait <= 0 {
		wait += 24 * 60
	}
	return false, time.Duration(wait)*time.Minute - time.Duration(l.Second())*time.Second, nil
}

func (r *HiveReleaseReconciler) enforce(ctx context.Context, hr *hivev1.HiveRelease, tgts []target, plans []plan, poll time.Duration) (ctrl.Result, error) {
	now := r.now()
	st := &hr.Status
	step := orDur(r.StepInterval, 15*time.Second)

	if st.Rollout == nil {
		first := -1
		var todo []string
		for i, p := range plans {
			if p.change {
				if first < 0 {
					first = i
				}
				todo = append(todo, p.t.spec.Name)
			}
		}
		if first < 0 {
			if !hasPhase(st.Targets, hivev1.PhaseRolledBack, hivev1.PhaseFailed) {
				r.setCond(hr, CondDegraded, metav1.ConditionFalse, "Healthy", "no failed targets")
			}
			r.setCond(hr, CondReady, metav1.ConditionTrue, "Idle", "nothing to roll")
			return ctrl.Result{RequeueAfter: poll}, nil
		}
		if st.NextAttempt != nil {
			if left := st.NextAttempt.Sub(now); left > 0 {
				r.setCond(hr, CondReady, metav1.ConditionFalse, "Cooldown",
					fmt.Sprintf("%s pending; next rollout not before %s (%s: %s)", strings.Join(todo, ", "),
						st.NextAttempt.UTC().Format(time.RFC3339), "last result", st.LastResult))
				return ctrl.Result{RequeueAfter: min(poll, left)}, nil
			}
		}
		ok, wait, err := inWindow(hr.Spec.Window, now)
		if err != nil {
			r.setCond(hr, CondReady, metav1.ConditionFalse, "BadWindow", err.Error())
			return ctrl.Result{RequeueAfter: poll}, nil
		}
		if !ok {
			r.setCond(hr, CondReady, metav1.ConditionFalse, "OutsideWindow",
				fmt.Sprintf("%s pending; window opens in %s", strings.Join(todo, ", "), wait.Round(time.Minute)))
			return ctrl.Result{RequeueAfter: min(poll, wait)}, nil
		}
		r.startTarget(hr, plans[first], nil)
		r.event(hr, corev1.EventTypeNormal, "RolloutStarted", "rolling %s in order: %s", plans[first].desired.Version, strings.Join(todo, ", "))
		r.setCond(hr, CondReady, metav1.ConditionFalse, "Rolling", "rolling "+strings.Join(todo, ", "))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	rp := st.Rollout
	idx := -1
	for i, t := range tgts {
		if t.spec.Name == rp.Target {
			idx = i
		}
	}
	if idx < 0 {
		r.endRollout(hr, "abort: target "+rp.Target+" was removed from spec mid-rollout")
		return ctrl.Result{RequeueAfter: poll}, nil
	}
	if plans[idx].dep == nil {
		// Transient read failure: keep the persisted step and try again.
		return ctrl.Result{RequeueAfter: step}, nil
	}
	rp.Index = idx
	p := plans[idx]
	ts := &st.Targets[idx]

	switch rp.Step {
	case hivev1.StepPreflight:
		return r.stepPreflight(ctx, hr, plans, p, ts, poll)
	case hivev1.StepPatching:
		return r.stepPatch(ctx, hr, p, ts, poll)
	case hivev1.StepRolling:
		return r.stepRolling(ctx, hr, p, ts, step)
	case hivev1.StepSoaking:
		return r.stepSoaking(ctx, hr, plans, p, ts)
	case hivev1.StepRollingBack:
		return r.stepRollingBack(ctx, hr, p, ts, step)
	}
	r.endRollout(hr, "abort: unknown step "+string(rp.Step))
	return ctrl.Result{RequeueAfter: poll}, nil
}

func hasPhase(ts []hivev1.ReleaseTargetStatus, ps ...hivev1.ReleasePhase) bool {
	for _, t := range ts {
		for _, p := range ps {
			if t.Phase == p {
				return true
			}
		}
	}
	return false
}

func (r *HiveReleaseReconciler) startTarget(hr *hivev1.HiveRelease, p plan, prev *hivev1.RolloutProgress) {
	now := metav1.NewTime(r.now())
	rp := &hivev1.RolloutProgress{StartedAt: now}
	if prev != nil {
		rp.StartedAt, rp.Done = prev.StartedAt, prev.Done
	}
	rp.Index, rp.Target = p.t.index, p.t.spec.Name
	rp.Step, rp.StepSince = hivev1.StepPreflight, now
	rp.Image, rp.Digest, rp.Version = p.desired.Image(), p.desired.Digest, p.desired.Version
	hr.Status.Rollout = rp
	ts := &hr.Status.Targets[p.t.index]
	setPhase(ts, hivev1.PhasePending, "preflight before "+ts.CurrentVersion+" → "+rp.Version, r.now())
}

func (r *HiveReleaseReconciler) holdUntil(hr *hivev1.HiveRelease, t time.Time) {
	if hr.Status.NextAttempt == nil || hr.Status.NextAttempt.Time.Before(t) {
		tt := metav1.NewTime(t)
		hr.Status.NextAttempt = &tt
	}
}

func (r *HiveReleaseReconciler) endRollout(hr *hivev1.HiveRelease, result string) {
	hr.Status.LastResult = truncate(result, 1000)
	hr.Status.Rollout = nil
}

func (r *HiveReleaseReconciler) enterStep(rp *hivev1.RolloutProgress, s hivev1.RolloutStep) {
	rp.Step = s
	rp.StepSince = metav1.NewTime(r.now())
}

func (r *HiveReleaseReconciler) stepPreflight(ctx context.Context, hr *hivev1.HiveRelease, plans []plan, p plan, ts *hivev1.ReleaseTargetStatus, poll time.Duration) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	now := r.now()
	cur, _ := containerImage(p.dep, p.t.container)
	if _, _, d := splitImage(cur); d == rp.Digest {
		// Already there (someone else set it, or a status write was lost):
		// never record the new image as its own rollback target.
		return r.finishTarget(hr, plans, p, ts, "already at "+rp.Version)
	}
	g := r.gate(ctx, p, -1)
	if !g.ok {
		kind := "UNHEALTHY"
		if g.unknown {
			kind = "UNKNOWN"
		}
		msg := fmt.Sprintf("preflight on current %s %s (%s); left untouched", ts.CurrentVersion, kind, g.msg)
		setPhase(ts, hivev1.PhasePending, msg, now)
		r.event(hr, corev1.EventTypeWarning, "PreflightFailed", "%s: %s", ts.Name, msg)
		r.setCond(hr, CondReady, metav1.ConditionFalse, "PreflightFailed", ts.Name+": "+msg)
		r.endRollout(hr, "abort at "+ts.Name+" (nothing changed): "+msg)
		// The next attempt waits a poll interval, not a hot loop of preflights.
		r.holdUntil(hr, now.Add(poll))
		return ctrl.Result{RequeueAfter: poll}, nil
	}
	prev := cur
	if _, _, d := splitImage(cur); d == "" {
		// Not digest-pinned: roll back to the digest the pod actually runs.
		prev = ""
		if g.pod != nil {
			for _, cs := range g.pod.Status.ContainerStatuses {
				if cs.Name == p.t.container {
					if i := strings.Index(cs.ImageID, "@sha256:"); i >= 0 {
						repo, tag, _ := splitImage(cur)
						prev = repo
						if tag != "" {
							prev += ":" + tag
						}
						prev += cs.ImageID[i:]
					}
				}
			}
		}
		if prev == "" {
			msg := "cannot resolve a rollback digest for " + cur + "; left untouched"
			setPhase(ts, hivev1.PhasePending, msg, now)
			r.event(hr, corev1.EventTypeWarning, "PreflightFailed", "%s: %s", ts.Name, msg)
			r.endRollout(hr, "abort at "+ts.Name+" (nothing changed): "+msg)
			r.holdUntil(hr, now.Add(poll))
			return ctrl.Result{RequeueAfter: poll}, nil
		}
	}
	rp.PreviousImage, rp.PreviousVersion = prev, ts.CurrentVersion
	rp.PreviousAnnotations = map[string]string{}
	for _, k := range versionAnnotations {
		if v, ok := p.dep.Annotations[k]; ok {
			rp.PreviousAnnotations[k] = v
		}
	}
	rp.Snapshot = g.agents
	rp.Failures = 0
	r.enterStep(rp, hivev1.StepPatching)
	setPhase(ts, hivev1.PhaseRolling, fmt.Sprintf("preflight OK (%s); patching %s → %s", g.msg, ts.CurrentVersion, rp.Version), now)
	// Persist the rollback target and snapshot BEFORE touching the Deployment.
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

// patchImage sets the container image and annotations in one strategic-merge
// patch, so the change and its way back land atomically. A nil annotation
// value deletes the key.
func (r *HiveReleaseReconciler) patchImage(ctx context.Context, dep *appsv1.Deployment, container, image string, ann map[string]*string) error {
	orig := dep.DeepCopy()
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	for k, v := range ann {
		if v == nil {
			delete(dep.Annotations, k)
		} else {
			dep.Annotations[k] = *v
		}
	}
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == container {
			dep.Spec.Template.Spec.Containers[i].Image = image
		}
	}
	return r.Patch(ctx, dep, client.StrategicMergeFrom(orig, client.MergeFromWithOptimisticLock{}))
}

func sp(s string) *string { return &s }

func (r *HiveReleaseReconciler) stepPatch(ctx context.Context, hr *hivev1.HiveRelease, p plan, ts *hivev1.ReleaseTargetStatus, poll time.Duration) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	now := r.now()
	cur, _ := containerImage(p.dep, p.t.container)
	if cur != rp.Image {
		ann := map[string]*string{
			AnnVersion:        sp(rp.Version),
			AnnPreviousImage:  sp(rp.PreviousImage),
			AnnPreviousVer:    sp(rp.PreviousVersion),
			AnnUpgradedAt:     sp(now.UTC().Format(time.RFC3339)),
			AnnManagedBy:      sp(managedByValue),
			AnnRolledBackFrom: nil,
			AnnRolledBackAt:   nil,
		}
		if err := r.patchImage(ctx, p.dep, p.t.container, rp.Image, ann); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			msg := "patch failed: " + err.Error()
			setPhase(ts, hivev1.PhaseFailed, msg, now)
			r.event(hr, corev1.EventTypeWarning, "PatchFailed", "%s: %s", ts.Name, msg)
			r.endRollout(hr, "abort at "+ts.Name+": "+msg)
			return ctrl.Result{RequeueAfter: poll}, err
		}
		metrics.Action(releaseController, hr.Name, "upgrade", true)
		r.event(hr, corev1.EventTypeNormal, "Upgrading", "%s: %s → %s (rollback target %s)", ts.Name, rp.PreviousVersion, rp.Version, rp.PreviousImage)
	}
	ts.AppliedImage = rp.Image
	ts.PlacementsRestored = nil
	r.enterStep(rp, hivev1.StepRolling)
	setPhase(ts, hivev1.PhaseRolling, fmt.Sprintf("%s → %s: waiting for the new pod", rp.PreviousVersion, rp.Version), now)
	return ctrl.Result{RequeueAfter: orDur(r.StepInterval, 15*time.Second)}, nil
}

func (r *HiveReleaseReconciler) stepRolling(ctx context.Context, hr *hivev1.HiveRelease, p plan, ts *hivev1.ReleaseTargetStatus, step time.Duration) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	now := r.now()
	if cur, _ := containerImage(p.dep, p.t.container); cur != rp.Image {
		return r.outOfBand(hr, ts, cur)
	}
	elapsed := now.Sub(rp.StepSince.Time)
	rollTO := orDur(r.RolloutTimeout, 7*time.Minute)
	verifyTO := orDur(r.VerifyTimeout, 3*time.Minute)
	if done, detail := rolloutDone(p.dep); !done {
		if elapsed > rollTO {
			return r.rollback(ctx, hr, p, ts, "rollout did not complete in "+rollTO.String()+": "+detail)
		}
		setPhase(ts, hivev1.PhaseRolling, "waiting for rollout: "+detail, now)
		return ctrl.Result{RequeueAfter: step}, nil
	}
	g := r.gate(ctx, p, -1)
	if g.ok && p.t.spoke {
		restored, lost, rerr := r.restorePlacements(ctx, p, g.pod, rp.Snapshot, g.agents)
		switch {
		case len(lost) > 0:
			g.ok, g.msg = false, "agents lost after the image change: "+strings.Join(lost, ", ")
		case rerr != nil:
			g.ok, g.unknown, g.msg = false, true, "could not restore placements: "+rerr.Error()
		}
		if len(restored) > 0 {
			ts.PlacementsRestored = appendUnique(ts.PlacementsRestored, restored...)
			metrics.Action(releaseController, hr.Name, "restore-placement", rerr == nil)
			r.event(hr, corev1.EventTypeWarning, "PlacementsRestored", "%s: image change reset placements; re-applied %s", ts.Name, strings.Join(restored, ", "))
		}
	}
	if g.ok {
		rp.BaselineRestarts = g.restarts
		rp.Failures = 0
		r.enterStep(rp, hivev1.StepSoaking)
		msg := "gate passed (" + g.msg + "); soaking " + durOr(hr.Spec.Soak, 10*time.Minute).String()
		if len(ts.PlacementsRestored) > 0 {
			msg += "; restored placements: " + strings.Join(ts.PlacementsRestored, ", ")
		}
		setPhase(ts, hivev1.PhaseSoaking, msg, now)
		return ctrl.Result{RequeueAfter: orDur(r.SoakInterval, time.Minute)}, nil
	}
	if elapsed <= rollTO+verifyTO {
		setPhase(ts, hivev1.PhaseRolling, "gate not yet passing: "+g.msg, now)
		return ctrl.Result{RequeueAfter: step}, nil
	}
	if g.unknown {
		return r.failUnmeasured(hr, ts, g.msg)
	}
	return r.rollback(ctx, hr, p, ts, g.msg)
}

func appendUnique(l []string, add ...string) []string {
	for _, a := range add {
		found := false
		for _, x := range l {
			if x == a {
				found = true
			}
		}
		if !found {
			l = append(l, a)
		}
	}
	return l
}

// failUnmeasured stops without rolling back: "could not measure" is never
// "unhealthy". The target stays on the new image with its rollback target in
// the previous-image annotation.
func (r *HiveReleaseReconciler) failUnmeasured(hr *hivev1.HiveRelease, ts *hivev1.ReleaseTargetStatus, why string) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	msg := fmt.Sprintf("could not verify %s after upgrade (%s). NOT rolled back — unmeasured is not unhealthy. Rollback target: %s", rp.Version, why, rp.PreviousImage)
	setPhase(ts, hivev1.PhaseFailed, msg, r.now())
	r.event(hr, corev1.EventTypeWarning, "VerifyUnknown", "%s: %s", ts.Name, msg)
	r.setCond(hr, CondDegraded, metav1.ConditionTrue, "VerifyUnknown", ts.Name+": "+msg)
	r.endRollout(hr, "abort at "+ts.Name+": "+msg)
	r.holdUntil(hr, r.now().Add(durOr(hr.Spec.Cooldown, 20*time.Hour)))
	metrics.ReleaseRollbacks.WithLabelValues(hr.Name, ts.Name, "unverified").Inc()
	return ctrl.Result{RequeueAfter: durOr(hr.Spec.PollInterval, time.Hour)}, nil
}

func (r *HiveReleaseReconciler) outOfBand(hr *hivev1.HiveRelease, ts *hivev1.ReleaseTargetStatus, cur string) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	msg := fmt.Sprintf("image changed out of band to %s during the rollout of %s; rollout stopped", cur, rp.Version)
	setPhase(ts, hivev1.PhaseDrifted, msg, r.now())
	r.event(hr, corev1.EventTypeWarning, "Drifted", "%s: %s", ts.Name, msg)
	r.endRollout(hr, "abort at "+ts.Name+": "+msg)
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *HiveReleaseReconciler) stepSoaking(ctx context.Context, hr *hivev1.HiveRelease, plans []plan, p plan, ts *hivev1.ReleaseTargetStatus) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	now := r.now()
	if cur, _ := containerImage(p.dep, p.t.container); cur != rp.Image {
		return r.outOfBand(hr, ts, cur)
	}
	soak := durOr(hr.Spec.Soak, 10*time.Minute)
	interval := orDur(r.SoakInterval, time.Minute)
	g := r.gate(ctx, p, rp.BaselineRestarts)
	if g.ok && p.t.spoke && len(rp.Snapshot) > 0 {
		if lost := lostAgents(rp.Snapshot, g.agents); len(lost) > 0 {
			g.ok, g.msg = false, "agents lost: "+strings.Join(lost, ", ")
		}
	}
	switch {
	case g.ok:
		rp.Failures = 0
	case g.unknown:
		// not a failure, not a pass
	default:
		rp.Failures++
		if rp.Failures >= 2 {
			return r.rollback(ctx, hr, p, ts, "soak: "+g.msg)
		}
	}
	elapsed := now.Sub(rp.StepSince.Time)
	if elapsed < soak {
		setPhase(ts, hivev1.PhaseSoaking, fmt.Sprintf("soaking %s/%s: %s", elapsed.Round(time.Second), soak, g.msg), now)
		return ctrl.Result{RequeueAfter: min(interval, soak-elapsed)}, nil
	}
	if !g.ok {
		if elapsed < soak+orDur(r.VerifyTimeout, 3*time.Minute) {
			return ctrl.Result{RequeueAfter: interval}, nil
		}
		if g.unknown {
			return r.failUnmeasured(hr, ts, g.msg)
		}
		return r.rollback(ctx, hr, p, ts, "after soak: "+g.msg)
	}
	// Target done.
	msg := fmt.Sprintf("upgraded %s → %s; soak passed", rp.PreviousVersion, rp.Version)
	if len(ts.PlacementsRestored) > 0 {
		msg += "; restored placements: " + strings.Join(ts.PlacementsRestored, ", ")
	}
	r.event(hr, corev1.EventTypeNormal, "Upgraded", "%s: %s", ts.Name, msg)
	return r.finishTarget(hr, plans, p, ts, msg)
}

// finishTarget marks the current target done and moves to the next target in
// order that needs a change, or completes the rollout.
func (r *HiveReleaseReconciler) finishTarget(hr *hivev1.HiveRelease, plans []plan, p plan, ts *hivev1.ReleaseTargetStatus, msg string) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	setPhase(ts, hivev1.PhaseCurrent, msg, r.now())
	rp.Done = append(rp.Done, ts.Name)
	for j := p.t.index + 1; j < len(plans); j++ {
		if plans[j].change {
			r.startTarget(hr, plans[j], rp)
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	res := fmt.Sprintf("success: %s (%s)", rp.Version, strings.Join(rp.Done, ", "))
	r.event(hr, corev1.EventTypeNormal, "RolloutComplete", "%s", res)
	r.endRollout(hr, res)
	r.setCond(hr, CondReady, metav1.ConditionTrue, "Complete", res)
	r.setCond(hr, CondDegraded, metav1.ConditionFalse, "Healthy", "last rollout succeeded")
	return ctrl.Result{RequeueAfter: durOr(hr.Spec.PollInterval, time.Hour)}, nil
}

// rollback restores the previous image and annotations, blocklists the digest,
// and stops the rollout. Later targets are never touched; earlier ones keep
// the version they passed their own gate on.
func (r *HiveReleaseReconciler) rollback(ctx context.Context, hr *hivev1.HiveRelease, p plan, ts *hivev1.ReleaseTargetStatus, reason string) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	now := r.now()
	hr.Status.Blocklist = appendUnique(hr.Status.Blocklist, rp.Digest)
	nowT := metav1.NewTime(now)
	hr.Status.LastRollbackAt = &nowT
	r.holdUntil(hr, now.Add(durOr(hr.Spec.Cooldown, 20*time.Hour)))
	ann := map[string]*string{
		AnnRolledBackFrom: sp(rp.Image),
		AnnRolledBackAt:   sp(now.UTC().Format(time.RFC3339)),
	}
	for _, k := range versionAnnotations {
		if v, ok := rp.PreviousAnnotations[k]; ok {
			ann[k] = sp(v)
		} else {
			ann[k] = nil
		}
	}
	rp.Reason = truncate(reason, 500)
	if err := r.patchImage(ctx, p.dep, p.t.container, rp.PreviousImage, ann); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		msg := fmt.Sprintf("%s failed (%s) AND the rollback patch failed: %v. Manual action: kubectl -n %s set image deploy/%s %s=%s",
			rp.Version, reason, err, p.t.ns, p.t.deploy, p.t.container, rp.PreviousImage)
		setPhase(ts, hivev1.PhaseFailed, msg, now)
		r.event(hr, corev1.EventTypeWarning, "RollbackFailed", "%s: %s", ts.Name, msg)
		r.setCond(hr, CondDegraded, metav1.ConditionTrue, "RollbackFailed", ts.Name+": "+msg)
		r.endRollout(hr, msg)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	ts.AppliedImage = rp.PreviousImage
	metrics.Action(releaseController, hr.Name, "rollback", true)
	metrics.ReleaseRollbacks.WithLabelValues(hr.Name, ts.Name, "gate").Inc()
	r.event(hr, corev1.EventTypeWarning, "RollingBack", "%s: %s failed (%s); rolling back to %s and blocklisting %s",
		ts.Name, rp.Version, reason, rp.PreviousImage, rp.Digest)
	r.enterStep(rp, hivev1.StepRollingBack)
	setPhase(ts, hivev1.PhaseRolling, "rolling back: "+reason, now)
	return ctrl.Result{RequeueAfter: orDur(r.StepInterval, 15*time.Second)}, nil
}

func (r *HiveReleaseReconciler) stepRollingBack(ctx context.Context, hr *hivev1.HiveRelease, p plan, ts *hivev1.ReleaseTargetStatus, step time.Duration) (ctrl.Result, error) {
	rp := hr.Status.Rollout
	now := r.now()
	elapsed := now.Sub(rp.StepSince.Time)
	limit := orDur(r.RolloutTimeout, 7*time.Minute) + orDur(r.VerifyTimeout, 3*time.Minute)
	g := gateResult{msg: "rollout not complete"}
	if done, detail := rolloutDone(p.dep); done {
		g = r.gate(ctx, p, -1)
		if g.ok && p.t.spoke {
			restored, _, rerr := r.restorePlacements(ctx, p, g.pod, rp.Snapshot, g.agents)
			if len(restored) > 0 {
				ts.PlacementsRestored = appendUnique(ts.PlacementsRestored, restored...)
				r.event(hr, corev1.EventTypeWarning, "PlacementsRestored", "%s: rollback reset placements; re-applied %s", ts.Name, strings.Join(restored, ", "))
			}
			if rerr != nil {
				g.ok, g.msg = false, "could not restore placements: "+rerr.Error()
			}
		}
	} else {
		g.msg = detail
	}
	if g.ok {
		msg := fmt.Sprintf("rolled back from %s to %s: %s; %s blocklisted", rp.Version, rp.PreviousVersion, rp.Reason, shortDigest(rp.Digest))
		if len(ts.PlacementsRestored) > 0 {
			msg += "; restored placements: " + strings.Join(ts.PlacementsRestored, ", ")
		}
		setPhase(ts, hivev1.PhaseRolledBack, msg, now)
		r.event(hr, corev1.EventTypeWarning, "RolledBack", "%s: %s. Stopped; later targets untouched.", ts.Name, msg)
		r.setCond(hr, CondDegraded, metav1.ConditionTrue, "RolledBack", ts.Name+": "+msg)
		r.endRollout(hr, "rolled back "+ts.Name+": "+msg)
		return ctrl.Result{RequeueAfter: durOr(hr.Spec.PollInterval, time.Hour)}, nil
	}
	if elapsed <= limit {
		setPhase(ts, hivev1.PhaseRolling, "rolling back ("+rp.Reason+"): "+g.msg, now)
		return ctrl.Result{RequeueAfter: step}, nil
	}
	msg := fmt.Sprintf("ROLLBACK DID NOT RECOVER (%s) after: %s. Manual action: kubectl -n %s get pods; kubectl -n %s rollout status deploy/%s",
		g.msg, rp.Reason, p.t.ns, p.t.ns, p.t.deploy)
	setPhase(ts, hivev1.PhaseFailed, msg, now)
	r.event(hr, corev1.EventTypeWarning, "RollbackFailed", "%s: %s", ts.Name, msg)
	r.setCond(hr, CondDegraded, metav1.ConditionTrue, "RollbackFailed", ts.Name+": "+msg)
	r.endRollout(hr, msg)
	return ctrl.Result{RequeueAfter: durOr(hr.Spec.PollInterval, time.Hour)}, nil
}

// ── the health gate ────────────────────────────────────────────────────────

type gateResult struct {
	ok, unknown bool
	msg         string
	pod         *corev1.Pod
	restarts    int32
	agents      []hivev1.AgentPlacementSnapshot
}

func rolloutDone(dep *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	s := dep.Status
	switch {
	case s.ObservedGeneration < dep.Generation:
		return false, fmt.Sprintf("generation %d/%d observed", s.ObservedGeneration, dep.Generation)
	case s.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d/%d updated", s.UpdatedReplicas, want)
	case s.Replicas > want:
		return false, fmt.Sprintf("%d old replicas terminating", s.Replicas-want)
	case s.ReadyReplicas < want:
		return false, fmt.Sprintf("%d/%d ready", s.ReadyReplicas, want)
	}
	return true, ""
}

var badWaiting = map[string]bool{
	"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true, "InvalidImageName": true,
}

// gate measures a target. ok=false,unknown=false is a measured failure;
// unknown=true means it could not be measured. baseline<0 skips the restart check.
func (r *HiveReleaseReconciler) gate(ctx context.Context, p plan, baseline int32) gateResult {
	dep := p.dep
	if done, detail := rolloutDone(dep); !done {
		return gateResult{msg: "deployment not ready: " + detail}
	}
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas < 1 {
		return gateResult{msg: "deployment scaled to 0"}
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return gateResult{unknown: true, msg: "bad selector: " + err.Error()}
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return gateResult{unknown: true, msg: "cannot list pods: " + err.Error()}
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		pp := &pods.Items[i]
		if pp.DeletionTimestamp != nil {
			continue
		}
		if pod == nil || pod.CreationTimestamp.Before(&pp.CreationTimestamp) {
			pod = pp
		}
	}
	if pod == nil {
		return gateResult{msg: "no live pod"}
	}
	g := gateResult{pod: pod}
	var cs *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == p.t.container {
			cs = &pod.Status.ContainerStatuses[i]
		}
	}
	if cs == nil {
		g.msg = "pod " + pod.Name + " has no status for container " + p.t.container
		return g
	}
	g.restarts = cs.RestartCount
	if w := cs.State.Waiting; w != nil && badWaiting[w.Reason] {
		g.msg = "pod " + pod.Name + ": " + w.Reason
		return g
	}
	if baseline >= 0 && cs.RestartCount > baseline {
		g.msg = fmt.Sprintf("pod %s restarted (%d → %d)", pod.Name, baseline, cs.RestartCount)
		return g
	}
	if !cs.Ready {
		g.msg = "pod " + pod.Name + " not ready"
		return g
	}
	switch {
	case p.t.spoke:
		if r.Probe == nil {
			return gateResult{unknown: true, msg: "no hive probe"}
		}
		code, err := r.Probe.Health(ctx, dep.Namespace, pod.Name)
		if err != nil {
			g.unknown, g.msg = true, err.Error()
			return g
		}
		if code != 200 {
			g.msg = fmt.Sprintf("/api/health HTTP %d", code)
			return g
		}
		agents, err := r.Probe.Agents(ctx, dep.Namespace, pod.Name)
		if err != nil {
			g.unknown, g.msg = true, err.Error()
			return g
		}
		g.agents = agents
		g.ok, g.msg = true, fmt.Sprintf("health 200, %d agents", len(agents))
	case p.t.spec.HealthURL != "":
		if r.HTTP == nil {
			return gateResult{unknown: true, msg: "no http prober"}
		}
		code, err := r.HTTP.Get(ctx, p.t.spec.HealthURL)
		if err != nil {
			g.unknown, g.msg = true, err.Error()
			return g
		}
		if code != 200 {
			g.msg = fmt.Sprintf("%s HTTP %d", p.t.spec.HealthURL, code)
			return g
		}
		g.ok, g.msg = true, p.t.spec.HealthURL+" 200"
	default:
		g.ok, g.msg = true, "pod ready"
	}
	return g
}

func lostAgents(before, after []hivev1.AgentPlacementSnapshot) []string {
	have := map[string]bool{}
	for _, a := range after {
		have[a.Name] = true
	}
	var lost []string
	for _, b := range before {
		if !have[b.Name] {
			lost = append(lost, b.Name)
		}
	}
	return lost
}

// restorePlacements re-applies any backend/model/effort the image change reset
// (v5↔v6 swaps have reset the pi lane). Paused state is compared and reported
// but not changed: on-demand agents come back paused by design, and resuming
// one would spend budget nobody asked for.
func (r *HiveReleaseReconciler) restorePlacements(ctx context.Context, p plan, pod *corev1.Pod,
	before, after []hivev1.AgentPlacementSnapshot) (restored, lost []string, err error) {
	lost = lostAgents(before, after)
	now := map[string]hivev1.AgentPlacementSnapshot{}
	for _, a := range after {
		now[a.Name] = a
	}
	var errs []string
	for _, b := range before {
		a, ok := now[b.Name]
		if !ok {
			continue
		}
		if a.Backend == b.Backend && a.Model == b.Model && a.Effort == b.Effort {
			continue
		}
		pl := hiveclient.Placement{Backend: b.Backend, Model: b.Model}
		if a.Effort != b.Effort {
			e := b.Effort
			pl.ReasoningEffort = &e
		}
		if perr := r.Probe.Place(ctx, p.t.ns, pod.Name, b.Name, pl); perr != nil {
			errs = append(errs, b.Name+": "+perr.Error())
			continue
		}
		restored = append(restored, fmt.Sprintf("%s (%s/%s → %s/%s)", b.Name, a.Backend, a.Model, b.Backend, b.Model))
	}
	if len(errs) > 0 {
		err = errors.New(strings.Join(errs, "; "))
	}
	return restored, lost, err
}

// ── wiring ──────────────────────────────────────────────────────────────────

// SetupWithManager wires the controller. Deployment changes enqueue every
// HiveRelease that names them, which is what makes drift visible immediately.
func (r *HiveReleaseReconciler) SetupWithManager(mgr ctrl.Manager, cs kubernetes.Interface, cfg *rest.Config) error {
	if r.Probe == nil {
		r.Probe = execProbe{c: hiveclient.New(cs, cfg)}
	}
	if r.HTTP == nil {
		r.HTTP = netProber{}
	}
	if r.Registry == nil {
		r.Registry = registry.New()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder(releaseController)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&hivev1.HiveRelease{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.releasesForDeployment)).
		Named(releaseController).
		Complete(r)
}

func (r *HiveReleaseReconciler) releasesForDeployment(ctx context.Context, o client.Object) []reconcile.Request {
	var list hivev1.HiveReleaseList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, hr := range list.Items {
		for _, t := range hr.Status.Targets {
			if t.Namespace == o.GetNamespace() && t.Deployment == o.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: hr.Name}})
				break
			}
		}
	}
	return reqs
}
