package controller

import (
	"testing"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

func TestRungIdentityIncludesTierAndEffort(t *testing.T) {
	base := hivev1.Rung{Tier: "T2", Provider: "openai", Model: "gpt-5.6-luna", Effort: "medium"}

	if rungIdentity(base) == rungIdentity(hivev1.Rung{Tier: "T3", Provider: base.Provider, Model: base.Model, Effort: base.Effort}) {
		t.Fatal("same model at a different tier was de-duplicated")
	}
	if rungIdentity(base) == rungIdentity(hivev1.Rung{Tier: base.Tier, Provider: base.Provider, Model: base.Model, Effort: "low"}) {
		t.Fatal("same model at a different effort was de-duplicated")
	}
	if rungIdentity(base) != rungIdentity(hivev1.Rung{Tier: "t2", Provider: "OpenAI", Model: "GPT-5.6-LUNA", Effort: "MEDIUM"}) {
		t.Fatal("identity must be case-insensitive")
	}
}
