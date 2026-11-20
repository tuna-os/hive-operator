package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tuna-os/hive-operator/internal/usage"
)

func newTestServer(statePath string, sources ...string) *server {
	s := &server{
		ledger:    usage.NewLedger(8 * 24 * time.Hour),
		statePath: statePath,
		pod:       "hive-0",
		ns:        "hive",
		log:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		status:    map[string]*usage.SourceStatus{},
	}
	for _, name := range sources {
		s.sources = append(s.sources, usage.Source{Name: name})
	}
	s.initMetrics()
	return s
}

// session builds one cumulative session report whose last activity is at.
func session(id, agent, model string, at time.Time, tok usage.Tokens) usage.SessionReport {
	return usage.SessionReport{Sessions: []usage.Session{{
		ID: id, Project: agent, Last: at, Models: map[string]usage.Tokens{model: tok},
	}}}
}

func byProject(s usage.Session) string { return s.Project }

func inputOf(w usage.WindowUsage) (rows int, input int64) {
	for _, r := range w.Rows {
		input += r.Tokens.Input
	}
	return len(w.Rows), input
}

// A restarted sidecar must resume measuring deltas from its snapshot. If the
// snapshot were lost, the whole cumulative session total would be counted
// again as fresh spend.
func TestPersistRestoreResumesDeltas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ledger.json")
	now := time.Now().UTC()

	a := newTestServer(path)
	a.ledger.Observe("claude", session("s1", "agent-a", "claude-sonnet", now,
		usage.Tokens{Input: 100, Total: 100, CostUSD: 0.25}), byProject, now)
	a.persist()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot mode = %o, want 600", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp snapshot left behind (stat err = %v)", err)
	}

	b := newTestServer(path)
	b.restore()
	if !b.ledger.Primed("claude") {
		t.Fatal("restored ledger is not primed for claude")
	}
	since := now.Add(-time.Hour)
	if got, want := b.ledger.Since(since), a.ledger.Since(since); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored totals = %v, want %v", got, want)
	}

	later := now.Add(5 * time.Minute)
	added := b.ledger.Observe("claude", session("s1", "agent-a", "claude-sonnet", later,
		usage.Tokens{Input: 110, Total: 110, CostUSD: 0.25}), byProject, later)
	if len(added) != 1 || added[0].Tokens.Input != 10 {
		t.Fatalf("after restore added = %+v, want one entry with 10 input tokens", added)
	}
}

func TestPersistWithoutStatePathWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := newTestServer("")
	now := time.Now().UTC()
	s.ledger.Observe("claude", session("s1", "agent-a", "m", now, usage.Tokens{Input: 1, Total: 1}), byProject, now)

	s.persist()
	s.restore()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("persist with an empty state path wrote %d file(s)", len(entries))
	}
	if !s.ledger.Primed("claude") {
		t.Error("restore with an empty state path must leave the ledger alone")
	}
}

func TestRestoreIgnoresMissingAndUnreadableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	s := newTestServer(path)
	s.restore() // no file yet: first start of a fresh pod
	if s.ledger.Primed("claude") {
		t.Fatal("missing snapshot primed the ledger")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.restore() // corrupt file: warn and start empty rather than crash
	if s.ledger.Primed("claude") || len(s.ledger.Since(time.Time{})) != 0 {
		t.Fatal("unreadable snapshot changed the ledger")
	}
}

func TestReportDefaultWindows(t *testing.T) {
	s := newTestServer("", "claude", "codex", "gemini")
	// Status is recorded out of source order and gemini has none: the report
	// follows the configured source order and skips sources never collected.
	s.status["codex"] = &usage.SourceStatus{Source: "codex", OK: true}
	s.status["claude"] = &usage.SourceStatus{Source: "claude", OK: false, Error: "boom"}

	now := time.Now().UTC()
	s.ledger.Observe("claude", session("recent", "agent-a", "m-recent", now.Add(-30*time.Minute),
		usage.Tokens{Input: 100, Total: 100}), byProject, now)
	s.ledger.Observe("claude", session("older", "agent-a", "m-older", now.Add(-3*time.Hour),
		usage.Tokens{Input: 50, Total: 50}), byProject, now)

	for _, since := range [][]time.Time{nil, {}} {
		r := s.report(since)

		if r.Schema != usage.SchemaVersion || r.Pod != "hive-0" || r.Namespace != "hive" {
			t.Errorf("report header = schema %d pod %q ns %q", r.Schema, r.Pod, r.Namespace)
		}
		var names []string
		for _, st := range r.Sources {
			names = append(names, st.Source)
		}
		if want := []string{"claude", "codex"}; !reflect.DeepEqual(names, want) {
			t.Errorf("sources = %v, want %v", names, want)
		}
		if len(r.Sources) == 2 && r.Sources[0].Error != "boom" {
			t.Errorf("claude status = %+v, want its recorded error", r.Sources[0])
		}

		if len(r.Windows) != len(usage.DefaultWindows) {
			t.Fatalf("got %d windows, want %d", len(r.Windows), len(usage.DefaultWindows))
		}
		wantRows := map[string]int{"1h": 1, "5h": 2, "24h": 2, "7d": 2}
		wantInput := map[string]int64{"1h": 100, "5h": 150, "24h": 150, "7d": 150}
		for i, dw := range usage.DefaultWindows {
			w := r.Windows[i]
			if w.Label != dw.Label {
				t.Errorf("window %d label = %q, want %q", i, w.Label, dw.Label)
			}
			if want := r.GeneratedAt.Add(-dw.D).Truncate(time.Second); !w.Since.Equal(want) {
				t.Errorf("window %s since = %v, want %v", w.Label, w.Since, want)
			}
			if rows, input := inputOf(w); rows != wantRows[w.Label] || input != wantInput[w.Label] {
				t.Errorf("window %s: %d rows / %d input, want %d / %d",
					w.Label, rows, input, wantRows[w.Label], wantInput[w.Label])
			}
		}
	}
}

func TestReportExplicitSince(t *testing.T) {
	s := newTestServer("", "claude")
	now := time.Now().UTC()
	s.ledger.Observe("claude", session("s1", "agent-a", "m", now.Add(-time.Hour),
		usage.Tokens{Input: 100, Total: 100}), byProject, now)

	late, early := now.Add(-10*time.Minute), now.Add(-2*time.Hour)
	r := s.report([]time.Time{late, early})

	if len(r.Windows) != 2 {
		t.Fatalf("got %d windows, want one per requested instant", len(r.Windows))
	}
	for i, want := range []time.Time{late, early} {
		if r.Windows[i].Label != "" || !r.Windows[i].Since.Equal(want) {
			t.Errorf("window %d = label %q since %v, want unlabelled %v", i, r.Windows[i].Label, r.Windows[i].Since, want)
		}
	}
	if rows, _ := inputOf(r.Windows[0]); rows != 0 {
		t.Errorf("window after the activity has %d rows, want 0", rows)
	}
	if rows, input := inputOf(r.Windows[1]); rows != 1 || input != 100 {
		t.Errorf("window before the activity = %d rows / %d input, want 1 / 100", rows, input)
	}
}

// windowGauge returns one window gauge family as window label → value for agent.
func windowGauge(t *testing.T, reg *prometheus.Registry, family, agent string) map[string]float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range fams {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["agent"] == agent {
				out[labels["window"]] = m.GetGauge().GetValue()
			}
		}
	}
	return out
}

func TestRefreshWindowGauges(t *testing.T) {
	s := newTestServer("", "claude")
	reg := prometheus.NewRegistry()
	reg.MustRegister(s.windowCost, s.windowTokens)

	now := time.Now().UTC()
	s.ledger.Observe("claude", session("recent", "agent-a", "m-recent", now.Add(-30*time.Minute),
		usage.Tokens{Input: 100, Total: 100, CostUSD: 0.25}), byProject, now)
	s.ledger.Observe("claude", session("older", "agent-a", "m-older", now.Add(-3*time.Hour),
		usage.Tokens{Input: 50, Total: 50, CostUSD: 0.5}), byProject, now)

	wantTokens := map[string]float64{"1h": 100, "5h": 150, "24h": 150, "7d": 150}
	wantCost := map[string]float64{"1h": 0.25, "5h": 0.75, "24h": 0.75, "7d": 0.75}

	// Gauges are rebuilt from the ledger each pass; a second pass over the
	// same ledger must not add to the first.
	for pass := 1; pass <= 2; pass++ {
		s.refreshWindowGauges()
		if got := windowGauge(t, reg, "hive_usage_window_tokens", "agent-a"); !reflect.DeepEqual(got, wantTokens) {
			t.Errorf("pass %d tokens = %v, want %v", pass, got, wantTokens)
		}
		if got := windowGauge(t, reg, "hive_usage_window_cost_usd", "agent-a"); !reflect.DeepEqual(got, wantCost) {
			t.Errorf("pass %d cost = %v, want %v", pass, got, wantCost)
		}
	}
}
