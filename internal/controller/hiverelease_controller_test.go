package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/registry"
)

const (
	repoHive = "ghcr.io/hivecommons/hive"
	repoHub  = "ghcr.io/hivecommons/hive-hub"
	digOld   = "sha256:f33662b37d2abf51816b81a04c05bfaba1ae63e38da8d062105d0e3283f23b16"
	digNew   = "sha256:8e137eb2d1af4b0929286f1a6845a18fa5abde1b52161f9a9e99ea27fff161b6"
	digHubV6 = "sha256:38af8ad25b598a7bbd76b746de62e4fc4c9a9e23ca041154d4ba59ea36681661"
	digHubV5 = "sha256:797b4035b2bc9f9f3b6f344ce60cb7a5b3c6d638fa4988f6bdf2d7f12889db8a"
	digV5    = "sha256:6bcfc09e7fcfd10ca128d4ac7f9c41bea44bb32e4f92d088812e7892a9762de0"
	revOld   = "459e63d58696687aaf7c579a01bff3dcfb16a553"
	revNew   = "5d26aa998c80311040acd5a0279090b2a451b5e1"
)

var oldImage = repoHive + ":v6-latest@" + digOld
var newImage = repoHive + ":v6-latest@" + digNew

// ── fakes ──────────────────────────────────────────────────────────────────

type fakeRegistry struct {
	images map[string]registry.Image // repo|ref
	tags   map[string][]string
	calls  int
}

func (f *fakeRegistry) add(repo, tag, digest, rev string) {
	if f.images == nil {
		f.images = map[string]registry.Image{}
	}
	img := registry.Image{Repo: repo, Tag: tag, Digest: digest, Revision: rev, Platforms: []string{"linux/amd64", "linux/arm64"}}
	if tag != "" {
		f.images[repo+"|"+tag] = img
	}
	byDigest := img
	byDigest.Tag = ""
	f.images[repo+"|"+digest] = byDigest
}

func (f *fakeRegistry) Resolve(_ context.Context, repo, ref string) (registry.Image, error) {
	f.calls++
	if img, ok := f.images[repo+"|"+ref]; ok {
		return img, nil
	}
	return registry.Image{}, fmt.Errorf("%s:%s: %w", repo, ref, registry.ErrNotFound)
}

func (f *fakeRegistry) Tags(_ context.Context, repo string) ([]string, error) {
	return f.tags[repo], nil
}

type placeCall struct {
	ns, agent string
	p         hiveclient.Placement
}

type fakeProbe struct {
	health    map[string]int   // ns -> code (default 200)
	healthErr map[string]error // ns -> not measured
	agents    map[string][]hivev1.AgentPlacementSnapshot
	placed    []placeCall
}

func (f *fakeProbe) Health(_ context.Context, ns, _ string) (int, error) {
	if err := f.healthErr[ns]; err != nil {
		return 0, err
	}
	if c, ok := f.health[ns]; ok {
		return c, nil
	}
	return 200, nil
}

func (f *fakeProbe) Agents(_ context.Context, ns, _ string) ([]hivev1.AgentPlacementSnapshot, error) {
	return append([]hivev1.AgentPlacementSnapshot(nil), f.agents[ns]...), nil
}

func (f *fakeProbe) Place(_ context.Context, ns, _, agent string, p hiveclient.Placement) error {
	f.placed = append(f.placed, placeCall{ns, agent, p})
	// The placement takes effect.
	for i, a := range f.agents[ns] {
		if a.Name == agent {
			f.agents[ns][i].Backend, f.agents[ns][i].Model = p.Backend, p.Model
		}
	}
	return nil
}

type fakeHTTP struct{ code int }

func (f fakeHTTP) Get(context.Context, string) (int, error) { return f.code, nil }

// ── harness ────────────────────────────────────────────────────────────────

type harness struct {
	t     *testing.T
	c     client.Client
	r     *HiveReleaseReconciler
	reg   *fakeRegistry
	probe *fakeProbe
	rec   *events.FakeRecorder
	now   time.Time
	podN  int
}

var spokeNS = map[string]string{"hanthor": "hive-hanthor", "reef": "hive-reef", "school": "hive"}

func defaultAgents() []hivev1.AgentPlacementSnapshot {
	return []hivev1.AgentPlacementSnapshot{
		{Name: "scanner", Backend: "pi", Model: "deepseek-flash"},
		{Name: "supervisor", Backend: "codex", Model: "gpt-5.4-mini"},
		{Name: "architect", Backend: "claude", Model: "claude-opus-5", Paused: true},
	}
}

func deployment(ns, name, container, image string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1,
			Annotations: map[string]string{AnnVersion: "v6-459e63d58696", AnnPreviousImage: repoHive + ":v5.105.10@" + digV5}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
	}
}

func newHarness(t *testing.T, mode hivev1.ReconcileMode, mutate func(*hivev1.HiveRelease)) *harness {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = hivev1.AddToScheme(s)

	hr := &hivev1.HiveRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "hive", Generation: 1},
		Spec: hivev1.HiveReleaseSpec{
			Image: repoHive, Track: "v6-latest", Mode: mode,
			Targets: []hivev1.ReleaseTarget{
				{Name: "hanthor", Spoke: "hanthor"},
				{Name: "reef", Spoke: "reef"},
				{Name: "school", Spoke: "school"},
				{Name: "hub", Namespace: "hive-hub", Deployment: "hive-hub", Container: "hub", Image: repoHub,
					HealthURL: "http://hive-hub.hive-hub.svc:3001/"},
			},
		},
	}
	if mutate != nil {
		mutate(hr)
	}
	objs := []client.Object{hr,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "amd64"}}},
		deployment("hive-hub", "hive-hub", "hub", repoHub+":v5.105.10@"+digHubV5),
	}
	for name, ns := range spokeNS {
		objs = append(objs,
			&hivev1.HiveSpoke{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: hivev1.HiveSpokeSpec{Namespace: ns}},
			deployment(ns, "hive", "hive", oldImage))
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&hivev1.HiveRelease{}).Build()

	reg := &fakeRegistry{}
	reg.add(repoHive, "v6-latest", digNew, revNew)
	reg.add(repoHive, "", digOld, revOld)
	reg.add(repoHive, "v5.105.10", digV5, "ccf32c80fb1c8ec7d2387f12db0559e706deeae6")
	reg.add(repoHub, "v6-latest", digHubV6, revNew)
	reg.add(repoHub, "v5.105.10", digHubV5, "ccf32c80fb1c8ec7d2387f12db0559e706deeae6")

	probe := &fakeProbe{health: map[string]int{}, healthErr: map[string]error{}, agents: map[string][]hivev1.AgentPlacementSnapshot{}}
	for _, ns := range spokeNS {
		probe.agents[ns] = defaultAgents()
	}
	h := &harness{t: t, c: c, reg: reg, probe: probe, rec: events.NewFakeRecorder(200),
		now: time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)}
	h.r = h.newReconciler()
	for _, ns := range append([]string{"hive-hub"}, "hive-hanthor", "hive-reef", "hive") {
		h.settle(ns)
	}
	return h
}

// newReconciler builds a fresh reconciler over the same cluster and fakes —
// as after an operator restart, with no in-memory state.
func (h *harness) newReconciler() *HiveReleaseReconciler {
	return &HiveReleaseReconciler{Client: h.c, APIReader: h.c, Registry: h.reg, Probe: h.probe,
		HTTP: fakeHTTP{200}, Recorder: h.rec, Now: func() time.Time { return h.now },
		RolloutTimeout: 7 * time.Minute, VerifyTimeout: 3 * time.Minute, StepInterval: 15 * time.Second, SoakInterval: time.Minute}
}

func (h *harness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "hive"}})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (h *harness) release() *hivev1.HiveRelease {
	h.t.Helper()
	var hr hivev1.HiveRelease
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "hive"}, &hr); err != nil {
		h.t.Fatal(err)
	}
	return &hr
}

func (h *harness) target(name string) hivev1.ReleaseTargetStatus {
	for _, t := range h.release().Status.Targets {
		if t.Name == name {
			return t
		}
	}
	h.t.Fatalf("no status for %s", name)
	return hivev1.ReleaseTargetStatus{}
}

func (h *harness) dep(ns string) *appsv1.Deployment {
	name := "hive"
	if ns == "hive-hub" {
		name = "hive-hub"
	}
	var d appsv1.Deployment
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
		h.t.Fatal(err)
	}
	return &d
}

func (h *harness) image(ns string) string {
	return h.dep(ns).Spec.Template.Spec.Containers[0].Image
}

// settle plays the Deployment controller and kubelet: the rollout completes
// and a fresh, ready pod runs the spec image.
func (h *harness) settle(ns string) {
	h.t.Helper()
	ctx := context.Background()
	d := h.dep(ns)
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}
	if err := h.c.Status().Update(ctx, d); err != nil {
		h.t.Fatal(err)
	}
	var pods corev1.PodList
	_ = h.c.List(ctx, &pods, client.InNamespace(ns))
	for i := range pods.Items {
		_ = h.c.Delete(ctx, &pods.Items[i])
	}
	h.podN++
	c := d.Spec.Template.Spec.Containers[0]
	_, _, dig := splitImage(c.Image)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("%s-%d", d.Name, h.podN), Labels: d.Spec.Selector.MatchLabels,
			CreationTimestamp: metav1.NewTime(h.now)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: c.Name, Ready: true, ImageID: "ghcr.io/x@" + dig, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	if err := h.c.Create(ctx, pod); err != nil {
		h.t.Fatal(err)
	}
}

// unsettle simulates a patched Deployment whose new pod has not come up.
func (h *harness) unsettle(ns string) {
	d := h.dep(ns)
	d.Status.UpdatedReplicas = 0
	d.Status.ReadyReplicas = 0
	if err := h.c.Status().Update(context.Background(), d); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) rollout() *hivev1.RolloutProgress { return h.release().Status.Rollout }

// drive reconciles, settling any patched Deployment, until the rollout ends or
// the step budget is exhausted.
func (h *harness) drive(maxSteps int) {
	h.t.Helper()
	for i := 0; i < maxSteps; i++ {
		before := map[string]string{}
		for _, ns := range []string{"hive-hanthor", "hive-reef", "hive", "hive-hub"} {
			before[ns] = h.image(ns)
		}
		res := h.reconcile()
		for ns, img := range before {
			if h.image(ns) != img {
				h.settle(ns)
			}
		}
		if h.rollout() == nil && i > 0 {
			return
		}
		h.advance(res.RequeueAfter)
	}
}

func (h *harness) events() string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// ── tests ──────────────────────────────────────────────────────────────────

func TestShadowReportsPlanAndMutatesNothing(t *testing.T) {
	h := newHarness(t, hivev1.ModeShadow, nil)
	h.reconcile()
	hr := h.release()
	if hr.Status.Desired == nil || hr.Status.Desired.Digest != digNew || hr.Status.Desired.Version != "v6-5d26aa998c80" {
		t.Fatalf("desired = %+v", hr.Status.Desired)
	}
	for _, n := range []string{"hanthor", "reef", "school"} {
		ts := h.target(n)
		if ts.Phase != hivev1.PhasePending {
			t.Fatalf("%s phase %s (%s)", n, ts.Phase, ts.Message)
		}
		if ts.CurrentVersion != "v6-459e63d58696" {
			t.Fatalf("%s current version %q must come from the image revision", n, ts.CurrentVersion)
		}
	}
	hub := h.target("hub")
	if hub.Phase != hivev1.PhaseHeld || !strings.Contains(hub.Message, "major version change") {
		t.Fatalf("hub v5→v6 must be held without opt-in: %s %s", hub.Phase, hub.Message)
	}
	if !strings.Contains(hr.Status.LastResult, "hanthor v6-459e63d58696 → v6-5d26aa998c80, reef") {
		t.Fatalf("plan = %q", hr.Status.LastResult)
	}
	for _, ns := range []string{"hive-hanthor", "hive-reef", "hive"} {
		if h.image(ns) != oldImage {
			t.Fatalf("shadow mutated %s", ns)
		}
	}
	if !strings.Contains(h.events(), "WouldRollout") {
		t.Fatal("expected a WouldRollout event")
	}
	// A second pass with the same plan does not repeat the event.
	h.advance(time.Minute)
	h.reconcile()
	if strings.Contains(h.events(), "WouldRollout") {
		t.Fatal("WouldRollout repeated for an unchanged plan")
	}
}

func TestEnforceRollsInCanaryOrderWithSoak(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	var order []string
	seen := map[string]bool{}
	for i := 0; i < 400; i++ {
		res := h.reconcile()
		for _, ns := range []string{"hive-hanthor", "hive-reef", "hive"} {
			if h.image(ns) == newImage && !seen[ns] {
				seen[ns] = true
				order = append(order, ns)
				// Nothing after the canary may move before the canary's soak.
				h.settle(ns)
			}
		}
		if rp := h.rollout(); rp == nil && i > 0 {
			break
		}
		h.advance(res.RequeueAfter)
	}
	if strings.Join(order, ",") != "hive-hanthor,hive-reef,hive" {
		t.Fatalf("order %v", order)
	}
	hr := h.release()
	if hr.Status.Rollout != nil || !strings.HasPrefix(hr.Status.LastResult, "success") {
		t.Fatalf("rollout not finished: %q %+v", hr.Status.LastResult, hr.Status.Rollout)
	}
	d := h.dep("hive-reef")
	if d.Annotations[AnnManagedBy] != managedByValue || d.Annotations[AnnPreviousImage] != oldImage ||
		d.Annotations[AnnVersion] != "v6-5d26aa998c80" || d.Annotations[AnnPreviousVer] != "v6-459e63d58696" ||
		d.Annotations[AnnUpgradedAt] == "" {
		t.Fatalf("annotations %v", d.Annotations)
	}
	if h.image("hive-hub") != repoHub+":v5.105.10@"+digHubV5 {
		t.Fatal("held hub was changed")
	}
	for _, n := range []string{"hanthor", "reef", "school"} {
		if ts := h.target(n); ts.Phase != hivev1.PhaseCurrent || ts.AppliedImage != newImage {
			t.Fatalf("%s: %s %s applied=%s", n, ts.Phase, ts.Message, ts.AppliedImage)
		}
	}
	// Soak: 3 targets × 10m at least.
	if h.now.Sub(time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)) < 30*time.Minute {
		t.Fatalf("finished in %s; soak was skipped", h.now.Sub(time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)))
	}
}

// soakUntil drives until the named target is soaking.
func (h *harness) untilStep(target string, step hivev1.RolloutStep, max int) {
	h.t.Helper()
	for i := 0; i < max; i++ {
		if rp := h.rollout(); rp != nil && rp.Target == target && rp.Step == step {
			return
		}
		res := h.reconcile()
		h.advance(res.RequeueAfter)
	}
	h.t.Fatalf("never reached %s/%s; rollout=%+v", target, step, h.rollout())
}

func TestGateFailureRollsBackBlocklistsAndStops(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	if h.image("hive-hanthor") != newImage {
		t.Fatal("canary not patched")
	}
	h.settle("hive-hanthor")
	h.probe.health["hive-hanthor"] = 503
	// Retries within the verify window, then rolls back.
	for i := 0; i < 100 && h.rollout() != nil && h.rollout().Step == hivev1.StepRolling; i++ {
		h.advance(h.reconcile().RequeueAfter)
	}
	if rp := h.rollout(); rp == nil || rp.Step != hivev1.StepRollingBack {
		t.Fatalf("expected RollingBack, got %+v", rp)
	}
	d := h.dep("hive-hanthor")
	if d.Spec.Template.Spec.Containers[0].Image != oldImage {
		t.Fatalf("not rolled back: %s", d.Spec.Template.Spec.Containers[0].Image)
	}
	if d.Annotations[AnnRolledBackFrom] != newImage || d.Annotations[AnnVersion] != "v6-459e63d58696" {
		t.Fatalf("rollback annotations %v", d.Annotations)
	}
	h.probe.health["hive-hanthor"] = 200
	h.settle("hive-hanthor")
	h.reconcile()
	hr := h.release()
	if hr.Status.Rollout != nil {
		t.Fatalf("rollout should have stopped: %+v", hr.Status.Rollout)
	}
	if ts := h.target("hanthor"); ts.Phase != hivev1.PhaseRolledBack {
		t.Fatalf("hanthor %s: %s", ts.Phase, ts.Message)
	}
	if len(hr.Status.Blocklist) != 1 || hr.Status.Blocklist[0] != digNew {
		t.Fatalf("blocklist %v", hr.Status.Blocklist)
	}
	if hr.Status.NextAttempt == nil {
		t.Fatal("no cooldown recorded")
	}
	if h.image("hive-reef") != oldImage || h.image("hive") != oldImage {
		t.Fatal("later targets were touched")
	}
	ev := h.events()
	if !strings.Contains(ev, "RollingBack") || !strings.Contains(ev, "RolledBack") {
		t.Fatalf("events: %s", ev)
	}
	// After the cooldown the blocklisted digest is held, not retried.
	h.advance(21 * time.Hour)
	h.reconcile()
	if h.rollout() != nil || h.image("hive-hanthor") != oldImage {
		t.Fatal("re-rolled a blocklisted digest")
	}
	if ts := h.target("reef"); ts.Phase != hivev1.PhaseHeld || !strings.Contains(ts.Message, "blocklisted") {
		t.Fatalf("reef %s %s", ts.Phase, ts.Message)
	}
}

func TestRolloutTimeoutRollsBack(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	h.unsettle("hive-hanthor") // e.g. ImagePullBackOff under Recreate: no pod at all
	h.reconcile()
	if rp := h.rollout(); rp.Step != hivev1.StepRolling {
		t.Fatalf("gave up before the rollout timeout: %+v", rp)
	}
	h.advance(8 * time.Minute)
	h.reconcile()
	rp := h.rollout()
	if rp == nil || rp.Step != hivev1.StepRollingBack || !strings.Contains(rp.Reason, "did not complete") {
		t.Fatalf("expected rollback on rollout timeout, got %+v", rp)
	}
	if h.image("hive-hanthor") != oldImage {
		t.Fatal("not rolled back")
	}
	// The rollback itself never recovers: reported loudly, rollout stopped.
	h.unsettle("hive-hanthor")
	h.advance(11 * time.Minute)
	h.reconcile()
	if ts := h.target("hanthor"); ts.Phase != hivev1.PhaseFailed || !strings.Contains(ts.Message, "ROLLBACK DID NOT RECOVER") {
		t.Fatalf("hanthor %s %s", ts.Phase, ts.Message)
	}
	if h.rollout() != nil || h.image("hive-reef") != oldImage {
		t.Fatal("continued after a failed rollback")
	}
}

func TestPlacementResetIsRestored(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	h.settle("hive-hanthor")
	// The v6 swap reset the pi lane to the default backend.
	h.probe.agents["hive-hanthor"][0].Backend, h.probe.agents["hive-hanthor"][0].Model = "claude", "claude-sonnet-5"
	h.reconcile()
	if rp := h.rollout(); rp == nil || rp.Step != hivev1.StepSoaking {
		t.Fatalf("expected soaking after restore, got %+v", rp)
	}
	if len(h.probe.placed) != 1 || h.probe.placed[0].agent != "scanner" ||
		h.probe.placed[0].p.Backend != "pi" || h.probe.placed[0].p.Model != "deepseek-flash" {
		t.Fatalf("placements %+v", h.probe.placed)
	}
	ts := h.target("hanthor")
	if len(ts.PlacementsRestored) != 1 || !strings.Contains(ts.PlacementsRestored[0], "scanner") {
		t.Fatalf("placementsRestored %v", ts.PlacementsRestored)
	}
	if !strings.Contains(h.events(), "PlacementsRestored") {
		t.Fatal("no PlacementsRestored event")
	}
}

func TestLostAgentFailsGate(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	h.settle("hive-hanthor")
	h.probe.agents["hive-hanthor"] = h.probe.agents["hive-hanthor"][:2] // architect vanished
	for i := 0; i < 100 && h.rollout() != nil && h.rollout().Step == hivev1.StepRolling; i++ {
		h.advance(h.reconcile().RequeueAfter)
	}
	rp := h.rollout()
	if rp == nil || rp.Step != hivev1.StepRollingBack || !strings.Contains(rp.Reason, "architect") {
		t.Fatalf("expected rollback for lost agent, got %+v", rp)
	}
}

func TestUnmeasuredIsNotRolledBack(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	h.settle("hive-hanthor")
	h.probe.healthErr["hive-hanthor"] = fmt.Errorf("%w: exec: forbidden", errNotMeasured)
	for i := 0; i < 100 && h.rollout() != nil; i++ {
		h.advance(h.reconcile().RequeueAfter)
	}
	if h.image("hive-hanthor") != newImage {
		t.Fatal("rolled back on an unmeasured gate")
	}
	hr := h.release()
	if len(hr.Status.Blocklist) != 0 {
		t.Fatalf("blocklisted on an unmeasured gate: %v", hr.Status.Blocklist)
	}
	if ts := h.target("hanthor"); ts.Phase != hivev1.PhaseFailed || !strings.Contains(ts.Message, "NOT rolled back") {
		t.Fatalf("hanthor %s %s", ts.Phase, ts.Message)
	}
	if h.image("hive-reef") != oldImage {
		t.Fatal("continued past an unverified target")
	}
}

func TestPreflightFailureChangesNothing(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.probe.health["hive-hanthor"] = 500
	for i := 0; i < 5; i++ {
		h.advance(h.reconcile().RequeueAfter)
	}
	if h.image("hive-hanthor") != oldImage {
		t.Fatal("patched a target that failed preflight")
	}
	hr := h.release()
	if hr.Status.NextAttempt == nil || len(hr.Status.Blocklist) != 0 {
		t.Fatalf("nextAttempt=%v blocklist=%v", hr.Status.NextAttempt, hr.Status.Blocklist)
	}
	if !strings.Contains(h.events(), "PreflightFailed") {
		t.Fatal("no PreflightFailed event")
	}
}

func TestResumesAfterOperatorRestart(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	h.settle("hive-hanthor")
	h.untilStep("hanthor", hivev1.StepSoaking, 10)
	// Restart: a brand-new reconciler with nothing in memory.
	h.r = h.newReconciler()
	h.drive(400)
	if ts := h.target("school"); ts.Phase != hivev1.PhaseCurrent {
		t.Fatalf("school %s %s", ts.Phase, ts.Message)
	}
	// hanthor was patched exactly once (no second Upgrading event for it).
	if n := strings.Count(h.events(), "Upgrading hanthor"); n > 1 {
		t.Fatalf("hanthor upgraded %d times", n)
	}
}

func TestSoakRestartTriggersRollback(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	h.settle("hive-hanthor")
	h.untilStep("hanthor", hivev1.StepSoaking, 10)
	var pods corev1.PodList
	_ = h.c.List(context.Background(), &pods, client.InNamespace("hive-hanthor"))
	p := pods.Items[0]
	p.Status.ContainerStatuses[0].RestartCount = 2
	if err := h.c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.reconcile()
	h.advance(time.Minute)
	h.reconcile()
	if rp := h.rollout(); rp == nil || rp.Step != hivev1.StepRollingBack {
		t.Fatalf("expected rollback after two failed soak checks, got %+v", rp)
	}
}

func TestDriftIsReportedAndConverged(t *testing.T) {
	h := newHarness(t, hivev1.ModeShadow, nil)
	// Fleet already on the desired digest: adopted.
	h.reg.add(repoHive, "v6-latest", digOld, revOld)
	h.reconcile()
	if ts := h.target("reef"); ts.Phase != hivev1.PhaseCurrent || ts.AppliedImage != oldImage {
		t.Fatalf("reef %s applied=%s", ts.Phase, ts.AppliedImage)
	}
	// The old cron swaps reef back to v5 behind our back.
	d := h.dep("hive-reef")
	d.Spec.Template.Spec.Containers[0].Image = repoHive + ":v5.105.10@" + digV5
	_ = h.c.Update(context.Background(), d)
	h.settle("hive-reef")
	h.advance(time.Minute)
	h.reconcile()
	if ts := h.target("reef"); ts.Phase != hivev1.PhaseDrifted || !strings.Contains(ts.Message, "out of band") {
		t.Fatalf("reef %s: %s", ts.Phase, ts.Message)
	}
	if h.image("hive-reef") == oldImage {
		t.Fatal("shadow converged drift")
	}
	// Enforce converges it back despite the v5→v6 major change.
	hr := h.release()
	hr.Spec.Mode = hivev1.ModeEnforce
	if err := h.c.Update(context.Background(), hr); err != nil {
		t.Fatal(err)
	}
	h.drive(200)
	if h.image("hive-reef") != oldImage {
		t.Fatalf("drift not converged: %s", h.image("hive-reef"))
	}
	if h.image("hive-hanthor") != oldImage || h.image("hive") != oldImage {
		t.Fatal("non-drifted targets touched")
	}
}

func TestWindowDefersStart(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, func(hr *hivev1.HiveRelease) {
		hr.Spec.Window = &hivev1.ReleaseWindow{Start: "04:30", End: "06:00", TimeZone: "America/New_York"}
	})
	// 08:30 UTC is 04:30 EDT: inside. Move to 12:00 UTC (08:00 EDT): outside.
	h.now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	res := h.reconcile()
	if h.rollout() != nil {
		t.Fatal("started outside the window")
	}
	if res.RequeueAfter > time.Hour {
		t.Fatalf("requeue %s exceeds poll", res.RequeueAfter)
	}
	h.now = time.Date(2026, 10, 3, 8, 31, 0, 0, time.UTC)
	h.reconcile()
	if h.rollout() == nil {
		t.Fatal("did not start inside the window")
	}
}

func TestSemverTrackPicksNewestUnblockedAndPinBypasses(t *testing.T) {
	h := newHarness(t, hivev1.ModeShadow, func(hr *hivev1.HiveRelease) {
		hr.Spec.Track = `^v5\.`
		hr.Spec.Targets = hr.Spec.Targets[:1]
		hr.Spec.Blocklist = []string{"v5.105.11"}
	})
	h.reg.tags = map[string][]string{repoHive: {"v5.105.9", "v5.105.10", "v5.105.11", "v6-latest", "v5.106.0-rc.1"}}
	h.reg.add(repoHive, "v5.105.11", "sha256:"+strings.Repeat("b", 64), "")
	h.reg.add(repoHive, "v5.105.9", "sha256:"+strings.Repeat("c", 64), "")
	h.reconcile()
	hr := h.release()
	if hr.Status.Desired == nil || hr.Status.Desired.Version != "v5.105.10" {
		t.Fatalf("desired %+v", hr.Status.Desired)
	}
	// The v6 canary must not be "upgraded" to v5: that is the nightly-rollback bug.
	if ts := h.target("hanthor"); ts.Phase != hivev1.PhaseHeld {
		t.Fatalf("v6 target on a v5 track must be held, got %s %s", ts.Phase, ts.Message)
	}
	// A pin is a deliberate choice and bypasses the guard.
	hr.Spec.Pin = "v5.105.10"
	hr.Generation = 2
	_ = h.c.Update(context.Background(), hr)
	h.advance(time.Minute)
	h.reconcile()
	if ts := h.target("hanthor"); ts.Phase != hivev1.PhasePending {
		t.Fatalf("pinned target %s %s", ts.Phase, ts.Message)
	}
}

func TestArchitectureGateHolds(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	img := h.reg.images[repoHive+"|v6-latest"]
	img.Platforms = []string{"linux/arm64"}
	h.reg.images[repoHive+"|v6-latest"] = img
	h.reconcile()
	if ts := h.target("hanthor"); ts.Phase != hivev1.PhaseHeld || !strings.Contains(ts.Message, "amd64") {
		t.Fatalf("hanthor %s %s", ts.Phase, ts.Message)
	}
	if h.rollout() != nil {
		t.Fatal("started a rollout of an image missing the node architecture")
	}
}

func TestSuspendDoesNothing(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, func(hr *hivev1.HiveRelease) { hr.Spec.Suspend = true })
	h.reconcile()
	if h.reg.calls != 0 || h.rollout() != nil {
		t.Fatal("suspended release did work")
	}
}

func TestVersionLabelAndMajor(t *testing.T) {
	cases := map[[3]string]string{
		{"v6-latest", revNew, digNew}: "v6-5d26aa998c80",
		{"v5.105.10", "x", digV5}:     "v5.105.10",
		{"edge", revNew, digNew}:      "edge-5d26aa998c80",
		{"", revNew, digNew}:          "rev-5d26aa998c80",
		{"v6-latest", "", digNew}:     "v6-latest@8e137eb2d1af",
	}
	for in, want := range cases {
		if got := VersionLabel(in[0], in[1], in[2]); got != want {
			t.Errorf("VersionLabel%v = %q, want %q", in, got, want)
		}
	}
	for v, want := range map[string]int{"v6-latest": 6, "v6-459e63d58696": 6, "v5.105.10": 5, "edge": -1, "": -1} {
		if got := MajorOf(v); got != want {
			t.Errorf("MajorOf(%q) = %d, want %d", v, got, want)
		}
	}
	if _, err := (&HiveReleaseReconciler{}).resolve(context.Background(), &hivev1.HiveRelease{}, target{}); err == nil {
		t.Fatal("resolve without a registry must fail, never guess")
	}
	r, tg, d := splitImage("ghcr.io/hivecommons/hive:v6-latest@" + digOld)
	if r != repoHive || tg != "v6-latest" || d != digOld {
		t.Fatalf("splitImage = %q %q %q", r, tg, d)
	}
	if r, tg, _ := splitImage("localhost:5000/hive"); r != "localhost:5000/hive" || tg != "" {
		t.Fatalf("registry port parsed as tag: %q %q", r, tg)
	}
	if !errors.Is(fmt.Errorf("%w: x", errNotMeasured), errNotMeasured) {
		t.Fatal("errNotMeasured must wrap")
	}
}
