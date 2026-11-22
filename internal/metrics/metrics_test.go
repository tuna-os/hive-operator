package metrics

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

// series gathers one metric family from reg and returns its samples keyed by
// the label that varies ("mode", "phase", ...) joined to the fixed labels.
// Counters and gauges both read through GetValue, which is all these tests need.
func series(t *testing.T, reg prometheus.Gatherer, name, keyLabel string) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var key string
			for _, l := range m.GetLabel() {
				if l.GetName() == keyLabel {
					key = l.GetValue()
				}
			}
			if m.GetGauge() != nil {
				out[key] = m.GetGauge().GetValue()
			} else {
				out[key] = m.GetCounter().GetValue()
			}
		}
	}
	return out
}

func registryWith(t *testing.T, cs ...prometheus.Collector) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(cs...)
	return reg
}

func TestSetModeIsOneHot(t *testing.T) {
	ReconcileMode.Reset()
	t.Cleanup(ReconcileMode.Reset)
	reg := registryWith(t, ReconcileMode)

	SetMode("sharedauth", "fleet", string(v1alpha1.ModeShadow))
	got := series(t, reg, "hive_reconcile_mode", "mode")
	want := map[string]float64{"Observe": 0, "Shadow": 1, "Enforce": 0}
	if len(got) != len(want) {
		t.Fatalf("series = %v, want exactly %v", got, want)
	}
	for mode, v := range want {
		if got[mode] != v {
			t.Errorf("mode %s = %v, want %v (all: %v)", mode, got[mode], v, got)
		}
	}

	// Changing mode must move the 1, not add a second one: two active modes
	// would let a dashboard imply Enforce while the operator only logged.
	SetMode("sharedauth", "fleet", string(v1alpha1.ModeEnforce))
	got = series(t, reg, "hive_reconcile_mode", "mode")
	if got["Enforce"] != 1 || got["Shadow"] != 0 || got["Observe"] != 0 {
		t.Errorf("after Enforce: %v, want only Enforce=1", got)
	}
}

func TestSetModeUnknownModeClearsEveryMode(t *testing.T) {
	ReconcileMode.Reset()
	t.Cleanup(ReconcileMode.Reset)
	reg := registryWith(t, ReconcileMode)

	SetMode("c", "o", string(v1alpha1.ModeEnforce))
	SetMode("c", "o", "")
	for mode, v := range series(t, reg, "hive_reconcile_mode", "mode") {
		if v != 0 {
			t.Errorf("mode %s = %v after an unrecognised mode, want 0", mode, v)
		}
	}
}

func TestSetModeKeepsObjectsApart(t *testing.T) {
	ReconcileMode.Reset()
	t.Cleanup(ReconcileMode.Reset)
	reg := registryWith(t, ReconcileMode)

	SetMode("pool", "a", string(v1alpha1.ModeEnforce))
	SetMode("pool", "b", string(v1alpha1.ModeObserve))

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]string{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			if m.GetGauge().GetValue() != 1 {
				continue
			}
			var obj, mode string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "object":
					obj = l.GetValue()
				case "mode":
					mode = l.GetValue()
				}
			}
			active[obj] = mode
		}
	}
	if active["a"] != "Enforce" || active["b"] != "Observe" || len(active) != 2 {
		t.Errorf("active modes = %v, want a=Enforce b=Observe", active)
	}
}

func TestActionSplitsAppliedFromDecided(t *testing.T) {
	ActionsTotal.Reset()
	t.Cleanup(ActionsTotal.Reset)
	reg := registryWith(t, ActionsTotal)

	Action("liveness", "spoke", "kick", false)
	Action("liveness", "spoke", "kick", false)
	Action("liveness", "spoke", "kick", true)

	got := series(t, reg, "hive_actions_total", "applied")
	if got["false"] != 2 || got["true"] != 1 || len(got) != 2 {
		t.Errorf("applied series = %v, want false=2 true=1", got)
	}
}

func TestSetReleasePhaseIsOneHotOverEveryPhase(t *testing.T) {
	ReleaseTargetPhase.Reset()
	t.Cleanup(ReleaseTargetPhase.Reset)
	reg := registryWith(t, ReleaseTargetPhase)

	// The phase list in SetReleasePhase is hand-written. Every phase the API
	// can report must get a series, or a target in that phase reads as
	// "no phase" instead of 1.
	phases := []v1alpha1.ReleasePhase{
		v1alpha1.PhaseCurrent, v1alpha1.PhasePending, v1alpha1.PhaseHeld,
		v1alpha1.PhaseRolling, v1alpha1.PhaseSoaking, v1alpha1.PhaseRolledBack,
		v1alpha1.PhaseFailed, v1alpha1.PhaseDrifted, v1alpha1.PhaseUnknown,
	}
	for _, active := range phases {
		SetReleasePhase("rel", "target", string(active))
		got := series(t, reg, "hive_release_target_phase", "phase")
		if len(got) != len(phases) {
			t.Fatalf("active %s: %d series, want %d: %v", active, len(got), len(phases), got)
		}
		var ones []string
		for p, v := range got {
			if v == 1 {
				ones = append(ones, p)
			}
		}
		sort.Strings(ones)
		if strings.Join(ones, ",") != string(active) {
			t.Errorf("active %s: phases at 1 = %v, want only %s", active, ones, active)
		}
	}
}

func TestRegisterAddsCollectorsToControllerRuntimeRegistry(t *testing.T) {
	// MustRegister panics on a duplicate name or a mismatched label set, so
	// reaching the end already proves the collectors do not collide.
	Register()

	ReconcileMode.Reset()
	t.Cleanup(ReconcileMode.Reset)
	SetMode("c", "o", string(v1alpha1.ModeObserve))

	got := series(t, metrics.Registry, "hive_reconcile_mode", "mode")
	if got["Observe"] != 1 {
		t.Errorf("hive_reconcile_mode not served from the controller-runtime registry: %v", got)
	}
}
