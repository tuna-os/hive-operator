package controller

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

// config/samples/fleet.yaml must stay the LIVE bash configuration: the
// TIERS table row for row, and each spoke's pins/holds from its CronJob env.
// A drift here shows up as every shadow line differing.
func TestFleetSampleMatchesLiveBash(t *testing.T) {
	b, err := os.ReadFile("../../config/samples/fleet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spokes := map[string]hivev1.HiveSpoke{}
	var ladder hivev1.ModelLadder
	for _, doc := range strings.Split(string(b), "\n---\n") {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "HiveSpoke":
			var s hivev1.HiveSpoke
			if err := yaml.UnmarshalStrict([]byte(doc), &s); err != nil {
				t.Fatal(err)
			}
			spokes[s.Name] = s
		case "ModelLadder":
			if err := yaml.UnmarshalStrict([]byte(doc), &ladder); err != nil {
				t.Fatal(err)
			}
		}
	}
	var got []string
	for _, r := range ladder.Spec.Builtin {
		got = append(got, strings.Join([]string{r.Tier, r.Provider, r.Backend, r.Model}, "|"))
	}
	// hive-rotate.sh TIERS (live, 2026-10-01).
	want := `T1|google|agy|gemini-3.8-flash-high
T1|kiro|pi|kiro-api-key/claude-opus-5:high
T1|kiro|pi|kiro-api-key/gpt-5-6-sol:high
T1|openai|codex|gpt-5.6-sol
T1|anthropic|claude|claude-opus-5
T2|google|agy|gemini-3.8-flash-low
T2|kiro|pi|kiro-api-key/claude-sonnet-5:medium
T2|kiro|pi|kiro-api-key/gpt-5-6-luna:medium
T2|openai|codex|gpt-5.6-luna
T2|anthropic|claude|claude-sonnet-5
T2|google|agy|gemini-3.6-flash-low
T2|meta|muse|muse-spark-1.3-contributor
T3|google|agy|gemini-3.8-flash-low
T3|kiro|pi|kiro-api-key/claude-haiku-4-5:low
T3|openai|codex|gpt-5.6-luna
T3|anthropic|claude|claude-haiku-4-5-20251001
T3|google|agy|gemini-3.6-flash-low
T3|meta|muse|muse-spark-1.3-contributor`
	if strings.Join(got, "\n") != want {
		t.Fatalf("ladder drifted from TIERS:\n%s", strings.Join(got, "\n"))
	}
	env := map[string]struct{ pin, hold string }{
		"school": {"supervisor", "reviewer"}, "reef": {"", ""}, "hanthor": {"", "reviewer"},
	}
	for name, e := range env {
		s, ok := spokes[name]
		if !ok {
			t.Fatalf("no spoke %s", name)
		}
		var pins []string
		for _, p := range s.Spec.Pins {
			pins = append(pins, p.Agent)
		}
		if strings.Join(pins, ",") != e.pin || strings.Join(s.Spec.Holds, ",") != e.hold {
			t.Errorf("%s: pins %v holds %v, want HIVE_ROTATE_PIN=%q HIVE_ROTATE_HOLD=%q", name, pins, s.Spec.Holds, e.pin, e.hold)
		}
		if s.Spec.RotationMode != hivev1.ModeShadow {
			t.Errorf("%s: rotationMode %s — promotion is a separate, reviewed change", name, s.Spec.RotationMode)
		}
		if s.Spec.LivenessMode != hivev1.ModeShadow {
			t.Errorf("%s: livenessMode %s — promotion is a separate, reviewed change (docs/liveness-promotion.md)", name, s.Spec.LivenessMode)
		}
	}
	if spokes["school"].Spec.Pace.FleetOrder != 0 || spokes["reef"].Spec.Pace.FleetOrder != 1 || spokes["hanthor"].Spec.Pace.FleetOrder != 2 {
		t.Error("pace fleetOrder must follow HIVE_PACE_NAMESPACES: hive hive-reef hive-hanthor")
	}
}
