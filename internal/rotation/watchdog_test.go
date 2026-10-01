package rotation

import (
	"testing"
	"time"
)

func TestBackendModelMismatch(t *testing.T) {
	cases := []struct {
		b, m string
		want bool
	}{
		{"claude", "claude-sonnet-5", false},
		{"claude", "gemini-3.8-flash-low", true}, // the half-placed pair that kept happening
		{"agy", "gemini-3.8-flash-low", false},
		{"agy", "claude-opus-5", true},
		{"codex", "gpt-5.6-luna", false},
		{"codex", "muse-spark-1.3-contributor", true},
		{"pi", "kiro-api-key/claude-sonnet-5:medium", false},
		{"pi", "gemini-3.8-flash-low", true},
		{"pi", "deepseek-v4-flash", true}, // pi is Kiro-only now
		{"muse", "anything", false},       // not judged
		{"goose", "x", false},
		{"", "x", false},
		{"claude", "", false},
	}
	for _, c := range cases {
		if got := BackendModelMismatch(c.b, c.m); got != c.want {
			t.Errorf("%s/%s: %v, want %v", c.b, c.m, got, c.want)
		}
	}
}

func TestMismatchRepairPicksOwnTierOnCurrentBackend(t *testing.T) {
	in := Input{Tiers: fleetTiers, Rungs: bashTiers()}
	r, ok := MismatchRepair(in, Agent{Name: "architect", CLI: "pi", Model: "gemini-3.8-flash-high"}, false)
	if !ok || r.Model != "kiro-api-key/claude-opus-5:high" {
		t.Fatalf("%+v %v", r, ok)
	}
	if _, ok := MismatchRepair(in, Agent{Name: "architect", CLI: "pi", Model: "gemini-3.8-flash-high"}, true); ok {
		t.Fatal("pinned agents are never repaired")
	}
	if _, ok := MismatchRepair(in, Agent{Name: "untiered", CLI: "pi", Model: "gemini"}, false); ok {
		t.Fatal("an agent with no tier has no rung to repair to")
	}
	if _, ok := MismatchRepair(in, Agent{Name: "guide", CLI: "copilot", Model: "x"}, false); ok {
		t.Fatal("copilot is not judged")
	}
}

func TestChooseRungHealthy(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	in := Input{Now: now, Tiers: fleetTiers, Rungs: bashTiers(), Policy: DefaultPolicy(),
		Agents:    []Agent{{Name: "guide", CLI: "claude", Model: "claude-sonnet-5", Cadence: "1h"}},
		Providers: map[string]Reading{"google": {40, ""}, "kiro": {10, ""}, "openai": {5, ""}, "anthropic": {50, ""}, "meta": {-1, "no-usage-api"}}}
	r, ok := ChooseRungHealthy(in, "guide")
	// google (rank 0) wins; within it the whole-line tie-break (no -s)
	// picks gemini-3.6 before gemini-3.8.
	if !ok || r.Model != "gemini-3.6-flash-low" {
		t.Fatalf("%+v", r)
	}
	// Never the current provider, never unmeasured, never exhausted.
	in.Providers["google"] = Reading{95, ""}
	r, _ = ChooseRungHealthy(in, "guide")
	if r.Provider != "kiro" || r.Model != "kiro-api-key/claude-sonnet-5:medium" {
		t.Fatalf("%+v", r)
	}
	// A high-volume agent: codex barred normally, but the watchdog always
	// passes the escape hatch (allow_subscription=1), so openai is fine;
	// kiro is still barred by its cadence guard.
	in.Agents[0].Cadence = "5m"
	r, _ = ChooseRungHealthy(in, "guide")
	if r.Provider != "openai" {
		t.Fatalf("%+v", r)
	}
	// Login-blocked provider is out.
	in.Agents = append(in.Agents, Agent{Name: "scanner", CLI: "codex", Model: "gpt-5.6-luna", Paused: true, PausedTrigger: "login-detector"})
	if r, ok := ChooseRungHealthy(in, "guide"); ok {
		t.Fatalf("%+v", r)
	}
	if _, ok := ChooseRungHealthy(in, "nobody"); ok {
		t.Fatal("unknown agent")
	}
}

func TestResetsAt(t *testing.T) {
	r, ok := ResetsAt("weekly=100% resets=2026-10-03T22:41:44Z")
	if !ok || !r.Equal(time.Date(2026, 10, 3, 22, 41, 44, 0, time.UTC)) {
		t.Fatalf("%v %v", r, ok)
	}
	if _, ok := ResetsAt("no-credential (ccleft auth_required)"); ok {
		t.Fatal("no instant")
	}
}
