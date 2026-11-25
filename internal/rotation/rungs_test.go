package rotation

import (
	"reflect"
	"testing"
)

func TestRungChainWalksEveryNotchDown(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"kiro-api-key/claude-opus-5:high", []string{
			"kiro-api-key/claude-opus-5:high",
			"kiro-api-key/claude-sonnet-5:high",
			"kiro-api-key/claude-haiku-4-5:low",
		}},
		{"gpt-6-astra", []string{"gpt-6-astra", "gpt-5.6-luna"}},
		{"claude-haiku-4-5", []string{"claude-haiku-4-5"}},
		{"", nil},
	} {
		if got := RungChain(tc.model); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("RungChain(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestRungUpTowardRefusesModelsNotBelowOriginal(t *testing.T) {
	const opus = "kiro-api-key/claude-opus-5:high"
	for name, current := range map[string]string{
		"original itself": opus, // nothing sits above the head of the chain
		"unrelated model": "claude-sonnet-5",
		"empty":           "",
	} {
		if got := RungUpToward(opus, current); got != "" {
			t.Errorf("%s: RungUpToward(%q, %q) = %q, want \"\"", name, opus, current, got)
		}
	}
}

func TestDemotedFrom(t *testing.T) {
	const opus = "kiro-api-key/claude-opus-5:high"
	for _, tc := range []struct {
		current string
		want    bool
	}{
		{"kiro-api-key/claude-sonnet-5:high", true},
		{"kiro-api-key/claude-haiku-4-5:low", true},
		{opus, false}, // the original is not "below" itself
		{"claude-sonnet-5", false},
		{"", false},
	} {
		if got := demotedFrom(opus, tc.current); got != tc.want {
			t.Errorf("demotedFrom(%q, %q) = %v, want %v", opus, tc.current, got, tc.want)
		}
	}
	if demotedFrom("", "claude-sonnet-5") {
		t.Error("an empty original has no chain to be demoted from")
	}
}

func TestAgyEffortOnlyForAgyBackend(t *testing.T) {
	for _, tc := range []struct{ backend, model, want string }{
		{"agy", "gemini-3.8-flash-high", "high"},
		{"agy", "gemini-3.8-flash-medium", "medium"},
		{"agy", "gemini-3.7-flash-low", "low"},
		{"agy", "gemini-3.8-flash", ""},         // no suffix: no effort key
		{"agy", "gemini-high-preview", ""},      // suffix must be the last segment
		{"claude", "gemini-3.8-flash-high", ""}, // other backends never get one
		{"", "gemini-3.8-flash-high", ""},
	} {
		if got := AgyEffort(tc.backend, tc.model); got != tc.want {
			t.Errorf("AgyEffort(%q, %q) = %q, want %q", tc.backend, tc.model, got, tc.want)
		}
	}
}
