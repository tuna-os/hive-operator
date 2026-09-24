package controller

import "testing"

func boolPtr(b bool) *bool { return &b }

// TestObserveAgentsOverlaysRuntimeOverride guards the ordering that matters
// most: /api/status can lag a switch/model write, so the persisted override
// journal must win whenever it names a backend or model.
func TestObserveAgentsOverlaysRuntimeOverride(t *testing.T) {
	agents := []rawAgent{{Name: "scanner", CLI: "muse", Model: "spark"}}
	runtime := runtimeStateFile{Agents: map[string]runtimeAgentOverride{
		"scanner": {Backend: "codex", Model: "gpt-luna"},
	}}
	obs := observeAgents(agents, runtime, nil, nil)
	if len(obs.Agents) != 1 || obs.Agents[0].Backend != "codex" || obs.Agents[0].Model != "gpt-luna" {
		t.Fatalf("override journal was not applied: %#v", obs.Agents)
	}
}

// TestObserveAgentsPartialOverrideKeepsUnsetField checks that an override
// entry naming only a backend (or only a model) does not blank out the field
// it left empty.
func TestObserveAgentsPartialOverrideKeepsUnsetField(t *testing.T) {
	agents := []rawAgent{{Name: "scanner", CLI: "muse", Model: "spark"}}
	runtime := runtimeStateFile{Agents: map[string]runtimeAgentOverride{
		"scanner": {Backend: "codex"},
	}}
	obs := observeAgents(agents, runtime, nil, nil)
	if obs.Agents[0].Backend != "codex" || obs.Agents[0].Model != "spark" {
		t.Fatalf("partial override corrupted the unset field: %#v", obs.Agents[0])
	}
}

// TestObserveAgentsClassifiesExclusively pins the on-demand > paused >
// running precedence the count gauges depend on.
func TestObserveAgentsClassifiesExclusively(t *testing.T) {
	agents := []rawAgent{
		{Name: "a", OnDemand: boolPtr(true), Paused: true},
		{Name: "b", Paused: true},
		{Name: "c"},
	}
	obs := observeAgents(agents, runtimeStateFile{}, nil, nil)
	if obs.OnDemand != 1 || obs.Paused != 1 || obs.Running != 1 {
		t.Fatalf("unexpected classification: onDemand=%d paused=%d running=%d", obs.OnDemand, obs.Paused, obs.Running)
	}
}

// TestObserveAgentsAppliesPinsAndAges confirms pins and previously computed
// idle ages are attached per-agent by name, independent of ordering.
func TestObserveAgentsAppliesPinsAndAges(t *testing.T) {
	agents := []rawAgent{{Name: "supervisor"}, {Name: "scanner"}}
	ages := map[string]int64{"supervisor": -1, "scanner": 42}
	pinned := map[string]bool{"supervisor": true}
	obs := observeAgents(agents, runtimeStateFile{}, ages, pinned)
	byName := map[string]struct {
		idle   int64
		pinned bool
	}{}
	for _, a := range obs.Agents {
		byName[a.Name] = struct {
			idle   int64
			pinned bool
		}{a.IdleSeconds, a.Pinned}
	}
	if !byName["supervisor"].pinned || byName["supervisor"].idle != -1 {
		t.Fatalf("supervisor pin/age not applied: %#v", byName["supervisor"])
	}
	if byName["scanner"].pinned || byName["scanner"].idle != 42 {
		t.Fatalf("scanner pin/age not applied: %#v", byName["scanner"])
	}
}
