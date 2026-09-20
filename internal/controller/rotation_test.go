package controller

import (
	"testing"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

func TestPlanRotationExhaustedProvider(t *testing.T) {
	agents := []hivev1.AgentState{{Name: "scanner", Backend: "agy", Model: "gemini-low"}}
	providers := []hivev1.ProviderState{{Provider: "google", UsedPercent: 100}, {Provider: "openai", UsedPercent: 22}}
	rungs := []hivev1.Rung{
		{Tier: "T2", Provider: "google", Backend: "agy", Model: "gemini-low", Available: true},
		{Tier: "T2", Provider: "openai", Backend: "codex", Model: "gpt-luna", Effort: "medium", Available: true},
	}
	got := planRotation(agents, providers, rungs)
	if len(got) != 1 || got[0].ToBackend != "codex" || got[0].ToModel != "gpt-luna" || got[0].ToEffort != "medium" {
		t.Fatalf("unexpected plan: %#v", got)
	}
}

func TestPlanRotationKeepsHealthyRungAndPins(t *testing.T) {
	agents := []hivev1.AgentState{
		{Name: "scanner", Backend: "codex", Model: "gpt-luna"},
		{Name: "supervisor", Backend: "muse", Model: "spark", Pinned: true},
	}
	providers := []hivev1.ProviderState{{Provider: "openai", UsedPercent: 90}, {Provider: "meta", UsedPercent: 100}}
	rungs := []hivev1.Rung{{Tier: "T2", Provider: "openai", Backend: "codex", Model: "gpt-luna", Available: true}}
	if got := planRotation(agents, providers, rungs); len(got) != 0 {
		t.Fatalf("expected no plan, got %#v", got)
	}
}

func TestPlanRotationReplacesUnknownRung(t *testing.T) {
	agents := []hivev1.AgentState{{Name: "operations", Backend: "muse", Model: "spark"}}
	providers := []hivev1.ProviderState{{Provider: "meta", UsedPercent: -1}, {Provider: "openai", UsedPercent: 22}}
	rungs := []hivev1.Rung{{Tier: "T2", Provider: "openai", Backend: "codex", Model: "gpt-luna", Available: true}}
	got := planRotation(agents, providers, rungs)
	if len(got) != 1 || got[0].Reason != "current rung is not in the effective ladder" {
		t.Fatalf("unexpected plan: %#v", got)
	}
}

func TestPlanRotationChangesEffortWithoutChangingProvider(t *testing.T) {
	agents := []hivev1.AgentState{{Name: "scanner", Backend: "codex", Model: "gpt-luna", Effort: "low"}}
	providers := []hivev1.ProviderState{{Provider: "openai", UsedPercent: 25}}
	rungs := []hivev1.Rung{{Tier: "T2", Provider: "openai", Backend: "codex", Model: "gpt-luna", Effort: "medium", Available: true}}
	got := planRotation(agents, providers, rungs)
	if len(got) != 1 || got[0].FromBackend != got[0].ToBackend || got[0].FromModel != got[0].ToModel || got[0].ToEffort != "medium" {
		t.Fatalf("unexpected effort-only plan: %#v", got)
	}
}
