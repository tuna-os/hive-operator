package rotation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeAPI struct {
	calls []string
	resp  map[string]string // path prefix → body
}

func (f *fakeAPI) Post(_ context.Context, ns, path string) (string, error) {
	f.calls = append(f.calls, ns+" "+path)
	for p, body := range f.resp {
		if strings.HasPrefix(path, p) {
			if body == "ERR" {
				return "", errors.New("transport")
			}
			return body, nil
		}
	}
	return `{"status":"?"}`, nil
}

func ok() map[string]string {
	return map[string]string{
		"/api/switch/": `{"status":"switched"}`, "/api/model/": `{"status":"model_set"}`,
		"/api/effort/": `{"status":"effort_set"}`, "/api/pause/": `{"status":"paused"}`,
		"/api/resume/": `{"status":"resumed"}`,
	}
}

// place_agent's sequence: switch → model → effort, and nothing else (no kick).
func TestActuatorPlacementSequence(t *testing.T) {
	api := &fakeAPI{resp: ok()}
	d := Decision{Agent: "scanner", Action: ActionMove,
		From: Placement{"google", "agy", "gemini-3.6-flash-low"}, To: Placement{"anthropic", "claude", "claude-sonnet-5"}}
	if err := (&HiveActuator{API: api}).Apply(context.Background(), "hive", d); err != nil {
		t.Fatal(err)
	}
	want := []string{"hive /api/switch/scanner/claude", "hive /api/model/scanner/claude-sonnet-5"}
	if strings.Join(api.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", api.calls)
	}

	api = &fakeAPI{resp: ok()}
	d = Decision{Agent: "architect", Action: ActionMove, ToEffort: "high",
		From: Placement{"anthropic", "claude", "claude-opus-5"}, To: Placement{"google", "agy", "gemini-3.8-flash-high"}}
	_ = (&HiveActuator{API: api}).Apply(context.Background(), "hive", d)
	if len(api.calls) != 3 || api.calls[2] != "hive /api/effort/architect/high" {
		t.Fatalf("agy placement must sync effort: %v", api.calls)
	}
}

// Backend and model are two non-transactional calls: a failed model call must
// put the backend back, or the agent is left on e.g. codex/claude-sonnet-5,
// which cannot launch.
func TestActuatorRollsBackBackendOnModelFailure(t *testing.T) {
	resp := ok()
	resp["/api/model/"] = `{"error":"unknown model"}`
	api := &fakeAPI{resp: resp}
	d := Decision{Agent: "scanner", Action: ActionMove,
		From: Placement{"google", "agy", "gemini-3.6-flash-low"}, To: Placement{"openai", "codex", "gpt-5.6-luna"}}
	err := (&HiveActuator{API: api}).Apply(context.Background(), "hive", d)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if last := api.calls[len(api.calls)-1]; last != "hive /api/switch/scanner/agy" {
		t.Fatalf("no rollback: %v", api.calls)
	}
}

func TestActuatorStrandAndResume(t *testing.T) {
	api := &fakeAPI{resp: ok()}
	a := &HiveActuator{API: api}
	if err := a.Apply(context.Background(), "hive-reef", Decision{Agent: "ops", Action: ActionStrand}); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), "hive-reef", Decision{Agent: "ops", Action: ActionResume}); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), "hive-reef", Decision{Agent: "ops", Action: ActionKeep}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(api.calls, "|") != "hive-reef /api/pause/ops|hive-reef /api/resume/ops" {
		t.Fatalf("calls %v", api.calls)
	}
}
