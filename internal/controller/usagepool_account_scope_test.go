package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tuna-os/ccleft"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// withKiroDecoy adds a second kiro account with more homes and zero spend. The
// bash-parity reduction (most homes wins) would pick it over the pinned one.
func withKiroDecoy(out *usage.CcleftOutput) {
	for _, r := range out.Readings {
		if r.Provider != ccleft.Kiro {
			continue
		}
		d := r
		d.Account = "decoy000000000000"
		d.Homes = []string{"a", "b", "c", "d", "e"}
		zero := 0.0
		d.Windows = append([]ccleft.Window(nil), r.Windows...)
		d.Windows[0].Used = &zero
		out.Readings = append(out.Readings, d)
		return
	}
}

// ccleft → UsagePool → rotation: a pinned account is the only one that drives
// the rotation reading and the plan; an absent pinned account is unmeasured
// (never free quota, never exhausted); recovery changes the decision once.
func TestPinnedCcleftAccountDrivesRotation(t *testing.T) {
	out := liveCcleft(t)
	withKiroDecoy(out)
	rf := &fakeReadings{out: out}
	pool := ccleftPool("kiro", "kiro", &hivev1.CcleftRef{URL: "http://ccleft", Account: "000000000000a005"},
		win("monthly", 31*24*time.Hour, "", "plan"))
	got := reconcilePool(t, pool, rf, liveProbeCM())
	if rr := got.Status.RotationReading; rr == nil || rr.Source != "ccleft" || rr.Percent != 38 {
		t.Fatalf("pinned account must set the rotation reading (38), got %+v", rr)
	}

	// The pinned account is not served at all: unmeasured, not the decoy's 0%.
	pool = ccleftPool("kiro", "kiro", &hivev1.CcleftRef{URL: "http://ccleft", Account: "0000000000000000"},
		win("monthly", 31*24*time.Hour, "", "plan"))
	absent := reconcilePool(t, pool, rf, liveProbeCM())
	if rr := absent.Status.RotationReading; rr == nil || rr.Percent != -1 {
		t.Fatalf("absent pinned account must be unmeasured, got %+v", rr)
	}

	// Through the spoke: the pool's reading reaches rotation, and unmeasured
	// strands nobody while a high reading does.
	now := ccleftNow
	hot := got.DeepCopy()
	hot.Status.RotationReading.Percent = 96
	hot.Status.RotationReading.ComputedAt = metaTime(now)
	for name, tc := range map[string]struct {
		pool   *hivev1.UsagePool
		strand bool
	}{"hot": {hot, true}, "unmeasured": {absent.DeepCopy(), false}, "cool": {got.DeepCopy(), false}} {
		tc.pool.Status.RotationReading.ComputedAt = metaTime(now)
		sp := enforceSpoke("hanthor", "hive-hanthor",
			hivev1.AgentState{Name: "architect", Backend: "pi", Model: "kiro-api-key/claude-opus-5:high", Cadence: "4h"})
		tc.pool.ObjectMeta = metav1.ObjectMeta{Name: "kiro"}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fleetLadder(), sp, tc.pool).WithStatusSubresource(sp).Build()
		r := &HiveSpokeReconciler{Client: c}
		rd, src := r.rotationReadings(context.Background(), sp, now)
		act := &recActuator{fail: map[string]bool{}}
		r.rotate(context.Background(), sp, hivev1.ModeEnforce, rd, src, act, now)
		strands := false
		for _, call := range act.calls {
			if call == "hive-hanthor strand architect " {
				strands = true
			}
		}
		if strands != tc.strand {
			t.Fatalf("%s: kiro %+v calls %v", name, rd["kiro"], act.calls)
		}
		if name == "unmeasured" && (rd["kiro"].Percent != -1 || src["kiro"] != "ccleft") {
			t.Fatalf("unmeasured must stay -1: %+v %s", rd["kiro"], src["kiro"])
		}
	}
}
