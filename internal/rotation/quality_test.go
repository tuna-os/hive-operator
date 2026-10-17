package rotation

import (
	"testing"
	"time"
)

func TestScoreMeetsRequiresFiniteEvidence(t *testing.T) {
	for _, tc := range []struct {
		score, minimum string
		want           bool
	}{
		{"", "", true}, {"garbage", "", true}, {"40.2", "40.2", true},
		{"39.7", "40.2", false}, {"", "40.2", false}, {"NaN", "40.2", false},
		{"+Inf", "40.2", false}, {"43.6", "NaN", false}, {"43.6", "bad", false},
	} {
		if got := ScoreMeets(tc.score, tc.minimum); got != tc.want {
			t.Errorf("ScoreMeets(%q, %q) = %v, want %v", tc.score, tc.minimum, got, tc.want)
		}
	}
}

func qualityInput() Input {
	pol := DefaultPolicy()
	pol.Canaries = false
	return Input{Now: time.Now(), MinimumScore: "40.2", Policy: pol,
		Agents:      []Agent{{Name: "guide", CLI: "claude", Model: "claude-haiku-4-5", Effort: "max", Cadence: "1h"}},
		Tiers:       map[string]string{"guide": "T2"},
		Providers:   map[string]Reading{"anthropic": {Percent: 10}, "google": {Percent: 10}},
		Rungs:       []Rung{{Tier: "T2", Provider: "anthropic", Backend: "claude", Model: "claude-sonnet-5", Effort: "max", Score: "43.6"}},
		PaceDemoted: map[string]Placement{"guide": {Provider: "anthropic", Backend: "claude", Model: "claude-sonnet-5"}},
	}
}

func TestQualityRotationCorrectsJournalDemotion(t *testing.T) {
	in := qualityInput()
	in.Agents[0] = Agent{Name: "guide", CLI: "codex", Model: "gpt-5.6-luna", Effort: "low", Cadence: "1h"}
	in.Providers["openai"] = Reading{Percent: 10}
	in.Rungs[0] = Rung{Tier: "T2", Provider: "openai", Backend: "codex", Model: "gpt-5.6-sol", Effort: "high", Score: "44.2"}
	in.PaceDemoted["guide"] = Placement{Provider: "openai", Backend: "codex", Model: "gpt-5.6-sol"}
	p := Compute(in)
	if len(p.Decisions) != 1 || p.Decisions[0].Action != ActionMove || p.Decisions[0].To.Model != "gpt-5.6-sol" || p.Decisions[0].ToEffort != "high" {
		t.Fatalf("below-floor journal demotion must move to a scored exact effort: %+v", p)
	}
	in.MinimumScore = ""
	if p := Compute(in); len(p.Decisions) != 1 || p.Decisions[0].Action != ActionKeep {
		t.Fatalf("absent policy must retain legacy demotion stickiness: %+v", p)
	}
}

func TestQualityRotationDoesNotBypassCapacityOrActivateHolds(t *testing.T) {
	for _, mode := range []string{"unscored", "hot", "hold", "pin", "paused", "on-demand"} {
		t.Run(mode, func(t *testing.T) {
			in := qualityInput()
			switch mode {
			case "unscored":
				in.Rungs[0].Score = ""
			case "hot":
				in.PaceBlocked = map[string]bool{"anthropic": true}
			case "hold":
				in.Holds = map[string]bool{"guide": true}
			case "pin":
				in.Pins = map[string]PinTarget{"guide": {}}
			case "paused":
				in.Agents[0].Paused = true
			case "on-demand":
				in.Agents[0].OnDemand = true
			}
			p := Compute(in)
			for _, d := range p.Decisions {
				if d.Action == ActionMove || d.Action == ActionResume || d.Action == ActionCanary {
					t.Fatalf("must not start agent without eligible capacity or override intentional inactivity: %+v", p)
				}
			}
			if (mode == "unscored" || mode == "hot") && (len(p.Decisions) != 1 || p.Decisions[0].Action != ActionStrand) {
				t.Fatalf("active off-policy agent without eligible capacity must be paused: %+v", p)
			}
		})
	}
}

func TestQualityEffortIsExactAndBackendSpecific(t *testing.T) {
	in := Input{MinimumScore: "40.2", Rungs: []Rung{
		{Backend: "codex", Model: "gpt-5.6-luna", Effort: "max", Score: "42.1"},
		{Backend: "agy", Model: "gemini-3.8-flash-high", Score: "40.2"},
		{Backend: "pi", Model: "kiro-api-key/claude-opus-5:high", Score: "52.3"},
	}}
	if in.PlacementAllowed("codex", "gpt-5.6-luna", "xhigh") || in.PlacementAllowed("codex", "gpt-5.6-luna", "") {
		t.Fatal("an explicit max score must not approve lower or unknown effort")
	}
	if in.PlacementAllowed("agy", "gemini-3.8-flash-high", "low") {
		t.Fatal("agy suffix alone must not approve a mismatched actual --effort")
	}
	if !in.PlacementAllowed("pi", "kiro-api-key/claude-opus-5:high", "low") {
		t.Fatal("pi launch suffix, not unrelated explicit effort, selects thinking")
	}
	if !in.PlacementAllowed("codex", "gpt-5.6-luna", "max") {
		t.Fatal("exact scored effort should be eligible")
	}
}

func TestQualityPaceBlocksHardcodedDowngradeAndStaleRestoration(t *testing.T) {
	a := FleetAgent{Namespace: "hive", MinimumScore: "40.2",
		Agent:    Agent{Name: "guide", CLI: "codex", Model: "gpt-5.6-sol", Effort: "high", Cadence: "1h"},
		Eligible: []Rung{{Backend: "codex", Model: "gpt-5.6-sol", Effort: "high", Score: "44.2"}}}
	in := PaceInput{Fleet: []FleetAgent{a}, Config: DefaultPaceConfig(), Verdicts: map[string]PaceVerdict{"openai": {Verdict: "hot"}}}
	if p := PlanPace(in); p.Changes != 0 {
		t.Fatalf("unscored Luna low downgrade must be blocked: %+v", p)
	}
	in.Fleet[0].MinimumScore = ""
	if p := PlanPace(in); p.Changes != 1 || p.Decisions[0].Action != ActionDemote || p.Decisions[0].To.Model != "gpt-5.6-luna" {
		t.Fatalf("unset policy retains the real Sol to Luna pacing notch: %+v", p)
	}
	a = FleetAgent{Namespace: "hive", MinimumScore: "40.2",
		Agent:    Agent{Name: "guide", CLI: "claude", Model: "claude-sonnet-5", Effort: "max", Cadence: "1h"},
		Eligible: []Rung{{Backend: "claude", Model: "claude-sonnet-5", Effort: "max", Score: "43.6"}}}
	in = PaceInput{Fleet: []FleetAgent{a}, Config: DefaultPaceConfig(), Verdicts: map[string]PaceVerdict{"anthropic": {Verdict: "cold"}}}
	in.Demoted = map[string]Placement{"hive/guide": {Provider: "anthropic", Backend: "claude", Model: "claude-opus-5"}}
	if p := PlanPace(in); p.Changes != 0 {
		t.Fatalf("unlisted stale original must never restore: %+v", p)
	}
	in.Fleet[0].MinimumScore = ""
	if p := PlanPace(in); p.Changes != 1 || p.Decisions[0].Action != ActionRestore {
		t.Fatalf("unset policy retains legacy restore: %+v", p)
	}
}

func TestQualityKiroPaceCannotUseUnscoredNotch(t *testing.T) {
	a := FleetAgent{Namespace: "hive", MinimumScore: "40.2",
		Agent:    Agent{Name: "guide", CLI: "pi", Model: "kiro-api-key/claude-opus-5:high", Cadence: "1h"},
		Eligible: []Rung{{Backend: "pi", Model: "kiro-api-key/claude-opus-5:high", Score: "52.3"}}}
	in := PaceInput{Fleet: []FleetAgent{a}, Config: DefaultPaceConfig(),
		Kiro: KiroBudget{Verdict: "over", Burn: fptr(200), Allowed: fptr(100)}}
	for _, d := range PlanPace(in).Decisions {
		if d.Action == ActionDemote {
			t.Fatalf("Kiro hardcoded Sonnet high notch is unscored: %+v", d)
		}
	}
}

func TestQualityHealthyHealingFiltersStaleEffectiveRungs(t *testing.T) {
	in := qualityInput()
	in.Rungs = []Rung{{Tier: "T2", Provider: "google", Backend: "agy", Model: "gemini-3.8-flash-low", Score: "32.6"}}
	if r, ok := ChooseRungHealthy(in, "guide"); ok {
		t.Fatalf("watchdog must not choose stale below-floor effective rung: %+v", r)
	}
	in.Rungs[0] = Rung{Tier: "T2", Provider: "google", Backend: "agy", Model: "gemini-3.8-flash-high", Score: "40.2"}
	if _, ok := ChooseRungHealthy(in, "guide"); !ok {
		t.Fatal("watchdog should retain measured healthy qualifying destination")
	}
}

func TestKiroCreditMultProviderQualifiedSpellingEquivalence(t *testing.T) {
	for _, family := range []string{"sol", "terra", "luna"} {
		legacy := KiroCreditMult("kiro-api-key/gpt-5-6-" + family + ":high")
		if got := KiroCreditMult("kiro/gpt-5.6-" + family); got != legacy {
			t.Errorf("%s credit multiplier = %v, want legacy %v", family, got, legacy)
		}
	}
}

func TestQualityRotationRejectsImplicitEffort(t *testing.T) {
	in := qualityInput()
	in.Providers["openai"] = Reading{Percent: 10}
	in.Rungs[0] = Rung{Tier: "T2", Provider: "openai", Backend: "omp", Model: "openai-codex/gpt-5.6-luna", Score: "42.1"}
	if p := Compute(in); len(p.Decisions) != 1 || p.Decisions[0].Action != ActionStrand {
		t.Fatalf("implicit effort must not inherit a previous model's unmeasured effort: %+v", p)
	}
	in.Rungs[0].Effort = "max"
	if p := Compute(in); len(p.Decisions) != 1 || p.Decisions[0].Action != ActionMove || p.Decisions[0].ToEffort != "max" {
		t.Fatalf("explicit measured effort must remain placeable: %+v", p)
	}
}

func TestQualityRotationRepairsActualAgyEffortDrift(t *testing.T) {
	in := qualityInput()
	in.Agents[0] = Agent{Name: "guide", CLI: "agy", Model: "gemini-3.8-flash-high", Effort: "low", Cadence: "1h"}
	in.Rungs[0] = Rung{Tier: "T2", Provider: "google", Backend: "agy", Model: "gemini-3.8-flash-high", Score: "40.2"}
	if p := Compute(in); len(p.Decisions) != 1 || p.Decisions[0].Action != ActionMove || p.Decisions[0].ToEffort != "high" {
		t.Fatalf("model suffix must not hide actual effort drift: %+v", p)
	}
}
