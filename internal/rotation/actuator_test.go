package rotation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tuna-os/hive-operator/internal/hiveclient"
)

type fakeAPI struct {
	calls     []string
	placeResp string
	resp      map[string]string // path prefix → body
}

func (f *fakeAPI) Place(_ context.Context, ns, agent string, p hiveclient.Placement) (string, error) {
	b, _ := json.Marshal(p)
	f.calls = append(f.calls, ns+" PUT "+agent+" "+string(b))
	if f.placeResp == "ERR" {
		return "", errors.New("transport")
	}
	if f.placeResp != "" {
		return f.placeResp, nil
	}
	return `{"ok":true,"applied":true,"status":"updated"}`, nil
}

func (f *fakeAPI) Post(_ context.Context, ns, path string) (string, error) {
	f.calls = append(f.calls, ns+" POST "+path)
	for p, body := range f.resp {
		if strings.HasPrefix(path, p) {
			return body, nil
		}
	}
	return `{"status":"?"}`, nil
}

func okPosts() map[string]string {
	return map[string]string{"/api/pause/": `{"status":"paused"}`, "/api/resume/": `{"status":"resumed"}`}
}

// place_agent is ONE atomic PUT: backend + model, plus the effort an agy
// model requires — never a switch/model pair, never a kick.
func TestActuatorAtomicPlacement(t *testing.T) {
	api := &fakeAPI{resp: okPosts()}
	d := Decision{Agent: "scanner", Action: ActionMove,
		From: Placement{"meta", "muse", "muse-spark-1.3-contributor"}, To: Placement{"kiro", "pi", "kiro-api-key/claude-sonnet-5:medium"}}
	if err := (&HiveActuator{API: api}).Apply(context.Background(), "hive", d); err != nil {
		t.Fatal(err)
	}
	want := `hive PUT scanner {"backend":"pi","model":"kiro-api-key/claude-sonnet-5:medium"}`
	if strings.Join(api.calls, "|") != want {
		t.Fatalf("calls %v", api.calls)
	}

	api = &fakeAPI{resp: okPosts()}
	d = Decision{Agent: "architect", Action: ActionMove,
		From: Placement{"kiro", "pi", "kiro-api-key/claude-opus-5:high"}, To: Placement{"google", "agy", "gemini-3.8-flash-high"}}
	_ = (&HiveActuator{API: api}).Apply(context.Background(), "hive", d)
	if len(api.calls) != 1 || !strings.HasSuffix(api.calls[0], `{"backend":"agy","model":"gemini-3.8-flash-high","reasoning_effort":"high"}`) {
		t.Fatalf("agy placement must carry its effort in the same request: %v", api.calls)
	}
}

func TestActuatorRejectsUnconfirmedPlacement(t *testing.T) {
	for _, body := range []string{
		`{"ok":true,"applied":false,"status":"updated"}`,
		`{"ok":false,"error":"owner access required"}`,
		`{"ok":true,"status":"queued"}`,
		`not json`,
	} {
		api := &fakeAPI{placeResp: body}
		d := Decision{Agent: "a", Action: ActionMove, To: Placement{"kiro", "pi", "kiro-api-key/claude-haiku-4-5:low"}}
		if err := (&HiveActuator{API: api}).Apply(context.Background(), "hive", d); err == nil {
			t.Fatalf("%s must fail", body)
		}
	}
	if !PlacementOK(`{"ok":true,"status":"updated (restarted)"}`) {
		t.Fatal("an ok updated response with no `applied` key is confirmation")
	}
	api := &fakeAPI{}
	d := Decision{Agent: "a", Action: ActionMove, To: Placement{"kiro", "pi", ""}}
	if err := (&HiveActuator{API: api}).Apply(context.Background(), "hive", d); err == nil || len(api.calls) != 0 {
		t.Fatal("a blank model must never be written")
	}
}

func TestActuatorResumeAfterAndStrands(t *testing.T) {
	api := &fakeAPI{resp: okPosts()}
	d := Decision{Agent: "guide", Action: ActionMove, ResumeAfter: true,
		To: Placement{"google", "agy", "gemini-3.8-flash-low"}}
	if err := (&HiveActuator{API: api}).Apply(context.Background(), "hive-reef", d); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 2 || api.calls[1] != "hive-reef POST /api/resume/guide" {
		t.Fatalf("calls %v", api.calls)
	}
	api = &fakeAPI{resp: okPosts()}
	_ = (&HiveActuator{API: api}).Apply(context.Background(), "hive", Decision{Agent: "x", Action: ActionStrand})
	_ = (&HiveActuator{API: api}).Apply(context.Background(), "hive", Decision{Agent: "y", Action: ActionStrand, SkipActuation: true})
	_ = (&HiveActuator{API: api}).Apply(context.Background(), "hive", Decision{Agent: "z", Action: ActionResume})
	_ = (&HiveActuator{API: api}).Apply(context.Background(), "hive", Decision{Agent: "k", Action: ActionKeep})
	if strings.Join(api.calls, "|") != "hive POST /api/pause/x|hive POST /api/resume/z" {
		t.Fatalf("calls %v", api.calls)
	}
	api = &fakeAPI{resp: map[string]string{"/api/pause/": `{"error":"nope"}`}}
	if err := (&HiveActuator{API: api}).Apply(context.Background(), "hive", Decision{Agent: "x", Action: ActionStrand}); err == nil {
		t.Fatal("a refused pause is an error")
	}
}
