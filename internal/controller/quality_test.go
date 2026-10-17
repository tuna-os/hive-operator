package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/rotation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLadderMinimumScoreFiltersWithoutInventingEvidence(t *testing.T) {
	for _, minimum := range []string{"40.2", "", "NaN"} {
		t.Run("floor="+minimum, func(t *testing.T) {
			ladder := &hivev1.ModelLadder{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}, Spec: hivev1.ModelLadderSpec{MinimumScore: minimum}}
			for i, score := range []string{"40.2", "39.7", "", "NaN", "+Inf"} {
				ladder.Spec.Builtin = append(ladder.Spec.Builtin, hivev1.Rung{Tier: "T2", Provider: "openai", Backend: "codex", Model: "gpt-5.6-luna", Effort: []string{"max", "xhigh", "high", "medium", "low"}[i], Score: score})
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ladder).WithStatusSubresource(ladder).Build()
			r := &ModelLadderReconciler{Client: c}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "fleet"}}); err != nil {
				t.Fatal(err)
			}
			var got hivev1.ModelLadder
			if err := c.Get(context.Background(), client.ObjectKey{Name: "fleet"}, &got); err != nil {
				t.Fatal(err)
			}
			want := 1
			if minimum == "" {
				want = 5
			}
			if minimum == "NaN" {
				want = 0
			}
			if len(got.Status.Effective) != want || len(got.Status.Dropped) != 5-want {
				t.Fatalf("effective=%+v dropped=%v", got.Status.Effective, got.Status.Dropped)
			}
		})
	}
}

func TestRotationReadsFloorEvenBeforeLadderStatusRefresh(t *testing.T) {
	ladder := &hivev1.ModelLadder{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}, Spec: hivev1.ModelLadderSpec{MinimumScore: "40.2"},
		Status: hivev1.ModelLadderStatus{Effective: []hivev1.Rung{
			{Tier: "T2", Provider: "anthropic", Backend: "claude", Model: "claude-haiku-4-5", Effort: "max", Available: true},
			{Tier: "T2", Provider: "anthropic", Backend: "claude", Model: "claude-sonnet-5", Effort: "max", Score: "43.6", Available: true},
		}}}
	sp := &hivev1.HiveSpoke{ObjectMeta: metav1.ObjectMeta{Name: "reef"},
		Spec:   hivev1.HiveSpokeSpec{Namespace: "hive-reef", PrimarySpoke: "school", Pace: &hivev1.PaceSpec{Disabled: true}},
		Status: hivev1.HiveSpokeStatus{Agents: []hivev1.AgentState{{Name: "guide", Backend: "claude", Model: "claude-haiku-4-5", Effort: "max", Cadence: "1h"}}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ladder).Build()
	r := &HiveSpokeReconciler{Client: c}
	act := &recActuator{}
	r.rotate(context.Background(), sp, hivev1.ModeEnforce, map[string]rotation.Reading{"anthropic": {Percent: 10}}, nil, act, time.Now())
	if len(act.calls) != 1 || act.calls[0] != "hive-reef move guide claude-sonnet-5" {
		t.Fatalf("stale effective status must not preserve unscored current model: %v", act.calls)
	}
	if len(sp.Status.RotationPlan) != 1 || sp.Status.RotationPlan[0].ToEffort != "max" {
		t.Fatalf("exact scored effort must survive controller conversion: %+v", sp.Status.RotationPlan)
	}
}

func TestQualityFloorHoldsWatchdogAndNudgeOnUnscoredPlacements(t *testing.T) {
	sp, li := livenessSpoke(hivev1.ModeEnforce, hivev1.ModeEnforce)
	ladder := livenessLadder()
	ladder.Spec.MinimumScore = "40.2"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ladder).Build()
	r := &HiveSpokeReconciler{Client: c}
	api := &fakeLiveness{governor: govYAML}
	r.liveness(context.Background(), sp, li, api, li.snapshot.Add(time.Minute))
	for _, mutation := range api.mutations() {
		if mutation != "hygiene" {
			t.Fatalf("unscored placements must not restart, re-place or kick: %v", api.mutations())
		}
	}
}

func TestReleaseDoesNotRestoreSnapshotInvalidatedByQualityFloor(t *testing.T) {
	h := newHarness(t, hivev1.ModeEnforce, nil)
	h.untilStep("hanthor", hivev1.StepRolling, 10)
	// Enable policy after the old release snapshot was captured.
	ladder := &hivev1.ModelLadder{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}, Spec: hivev1.ModelLadderSpec{MinimumScore: "40.2"},
		Status: hivev1.ModelLadderStatus{Effective: []hivev1.Rung{
			{Tier: "T2", Provider: "anthropic", Backend: "claude", Model: "claude-sonnet-5", Effort: "max", Score: "43.6", Available: true},
		}}}
	if err := h.c.Create(context.Background(), ladder); err != nil {
		t.Fatal(err)
	}
	h.settle("hive-hanthor")
	current := &h.probe.agents["hive-hanthor"][0]
	current.Backend, current.Model, current.Effort = "claude", "claude-sonnet-5", "max"
	h.reconcile()
	if rp := h.rollout(); rp == nil || rp.Step != hivev1.StepSoaking {
		t.Fatalf("stale lower-quality snapshot alone must not cause release rollback: %+v", rp)
	}
	if len(h.probe.placed) != 0 {
		t.Fatalf("release restored a snapshot invalidated by the floor: %+v", h.probe.placed)
	}
	if len(h.target("hanthor").PlacementsRestored) != 0 || !strings.Contains(h.events(), "PlacementRestoreHeld") {
		t.Fatal("held snapshot must be reported without claiming it was restored")
	}
}
