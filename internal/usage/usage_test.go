package usage

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixtures are real `ccusage <source> session --json` output (v20.0.24)
// captured from the hive pod's logs on 2026-09-24, cut to a few sessions.

func load(t *testing.T, name string) SessionReport {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseSessionReport(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseClaudeShape(t *testing.T) {
	r := load(t, "claude.session.json")
	if len(r.Sessions) != 4 {
		t.Fatalf("sessions = %d", len(r.Sessions))
	}
	s := r.Sessions[0]
	if AgentFromProject(s.Project) != "telemetry" {
		t.Fatalf("project %q → %q", s.Project, AgentFromProject(s.Project))
	}
	m, ok := s.Models["claude-sonnet-5"]
	if !ok || !near(m.CostUSD, 0.0697848) || m.Total != m.Input+m.Output+m.CacheRead+m.CacheCreate {
		t.Fatalf("model breakdown: %+v", s.Models)
	}
	if s.First.IsZero() || s.Last.Before(s.First) {
		t.Fatalf("activity span: %v – %v", s.First, s.Last)
	}
	if len(r.UnpricedModels) != 1 || r.UnpricedModels[0] != "claude-opus-5-5" {
		t.Fatalf("unpriced: %v", r.UnpricedModels)
	}
	if AgentFromProject(r.Sessions[3].Project) != Unattributed {
		t.Fatalf("bare '-' project must be unattributed, got %q", AgentFromProject(r.Sessions[3].Project))
	}
}

// Codex rows use a models{} map and costUSD instead of modelBreakdowns and
// totalCost, have no projectPath and no firstActivity.
func TestParseCodexShape(t *testing.T) {
	r := load(t, "codex.session.json")
	s := r.Sessions[2]
	m := s.Models["gpt-5.6-luna"]
	if !near(m.CostUSD, 0.00406768) || m.Total != 49722 || m.Reasoning != 60 {
		t.Fatalf("codex model: %+v", m)
	}
	if !s.First.IsZero() || s.Last.IsZero() || s.Project != "" {
		t.Fatalf("codex session shape changed: %+v", s)
	}
}

func TestParsePiAndAntigravity(t *testing.T) {
	pi := load(t, "pi.session.json")
	for _, s := range pi.Sessions {
		a := AgentFromProject(s.Project)
		if a != "sec-check" && a != "strategist" {
			t.Fatalf("pi project %q → %q", s.Project, a)
		}
		for model := range s.Models {
			if ProviderOf("pi", model) != "deepseek" {
				t.Fatalf("pi model %q → %q", model, ProviderOf("pi", model))
			}
		}
	}
	agy := load(t, "antigravity.session.json")
	for _, s := range agy.Sessions {
		if s.Project != "Antigravity" {
			t.Fatalf("antigravity project is a constant today, got %q", s.Project)
		}
		for model := range s.Models {
			if p := ProviderOf("antigravity", model); p != "google" && !strings.HasPrefix(model, "model_placeholder") {
				t.Fatalf("antigravity model %q → %q", model, p)
			}
		}
	}
}

// The mapping is diffed against bash's hive_provider_of; these are the rows
// its unit tests pin (tests/test_hive_ops_lib.py) plus the ccusage names.
func TestProviderOf(t *testing.T) {
	for _, tc := range []struct{ cli, model, want string }{
		{"copilot", "gpt-5", "github"},
		{"muse", "muse-spark-1.3-contributor", "meta"},
		{"pi", "[pi] deepseek-v4-flash", "deepseek"},
		{"agy", "claude-sonnet-4-6", "anthropic"}, // model wins over CLI
		{"codex", "gpt-5.6-luna", "openai"},
		{"claude", "claude-fable-5-1", "anthropic"},
		{"agy", "gemini-3.8-flash-low", "google"},
		{"antigravity", "model_placeholder_m322", "google"},
		{"goose", "", "deepseek"},
		{"bob", "", "ibm"},
		{"mystery", "", "unknown"},
	} {
		if got := ProviderOf(tc.cli, tc.model); got != tc.want {
			t.Errorf("ProviderOf(%q,%q) = %q, want %q", tc.cli, tc.model, got, tc.want)
		}
	}
}

func sess(id, project string, last time.Time, model string, cost float64, total int64) Session {
	return Session{ID: id, Project: project, First: last.Add(-30 * time.Minute), Last: last,
		Models: map[string]Tokens{model: {Total: total, Output: total / 10, CostUSD: cost}}}
}

// The ledger's whole purpose: deltas between collections land in the right
// interval, per agent, and a provider window can be summed from any instant.
func TestLedgerDeltas(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	l := NewLedger(8 * 24 * time.Hour)
	attr := func(s Session) string { return AgentFromProject(s.Project) }

	// First collection: back-fill across each session's own span.
	r1 := SessionReport{Sessions: []Session{sess("a", "-data-agents-scanner", t0.Add(-2*time.Hour), "claude-sonnet-5", 1.0, 1000)}}
	if n := len(l.Observe("claude", r1, attr, t0)); n != 1 {
		t.Fatalf("backfill entries = %d", n)
	}
	if l.Primed("claude") != true {
		t.Fatal("source must be primed after one collection")
	}

	// Second: session a grew by $0.5, session b is new.
	t1 := t0.Add(5 * time.Minute)
	a2 := sess("a", "-data-agents-scanner", t1, "claude-sonnet-5", 1.5, 1500)
	b := sess("b", "-data-agents-architect", t1, "claude-opus-5", 2.0, 800)
	b.First = t0.Add(time.Minute)
	l.Observe("claude", SessionReport{Sessions: []Session{a2, b}}, attr, t1)

	win := l.Since(t0)
	k := func(agent, model string) Key {
		return Key{Source: "claude", Provider: "anthropic", Agent: agent, Model: model}
	}
	if got := win[k("scanner", "claude-sonnet-5")].CostUSD; !near(got, 0.5) {
		t.Fatalf("scanner since t0 = %v, want only the 0.5 delta", got)
	}
	if got := win[k("architect", "claude-opus-5")].CostUSD; !near(got, 2.0) {
		t.Fatalf("architect since t0 = %v", got)
	}
	if got := l.Since(time.Time{})[k("scanner", "claude-sonnet-5")].CostUSD; !near(got, 1.5) {
		t.Fatalf("scanner all-time = %v", got)
	}

	// A session that vanishes (aged out of the logs) is not a refund, and a
	// session whose totals shrink produces no negative entry.
	t2 := t1.Add(5 * time.Minute)
	shrunk := sess("b", "-data-agents-architect", t2, "claude-opus-5", 1.0, 400)
	if n := len(l.Observe("claude", SessionReport{Sessions: []Session{shrunk}}, attr, t2)); n != 0 {
		t.Fatalf("shrinking session produced %d entries", n)
	}
	if got := l.Since(time.Time{})[k("architect", "claude-opus-5")].CostUSD; !near(got, 2.0) {
		t.Fatalf("architect after shrink = %v", got)
	}
}

func TestLedgerBackfillSpreadsAcrossSpan(t *testing.T) {
	now := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	l := NewLedger(0)
	s := Session{ID: "x", First: now.Add(-10 * time.Hour), Last: now,
		Models: map[string]Tokens{"claude-sonnet-5": {Total: 1000, CostUSD: 10}}}
	l.Observe("claude", SessionReport{Sessions: []Session{s}}, nil, now)
	// Half the span is inside the last 5 hours.
	got := 0.0
	for _, t := range l.Since(now.Add(-5 * time.Hour)) {
		got += t.CostUSD
	}
	if !near(got, 5) {
		t.Fatalf("5h slice of a 10h back-filled session = %v, want 5", got)
	}
	total := int64(0)
	for _, t := range l.Since(time.Time{}) {
		total += t.Total
	}
	if total != 1000 {
		t.Fatalf("spread lost tokens: %d", total)
	}
}

func TestLedgerSnapshotRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	l := NewLedger(time.Hour * 24)
	l.Observe("pi", SessionReport{Sessions: []Session{sess("s", "--data-agents-guide--", now, "[pi] deepseek-flash", 1, 10)}}, nil, now)
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	l2 := NewLedger(time.Hour * 24)
	if err := json.Unmarshal(b, l2); err != nil {
		t.Fatal(err)
	}
	if !l2.Primed("pi") {
		t.Fatal("restored ledger lost its priming")
	}
	// Re-observing the same totals after a restart adds nothing.
	if n := len(l2.Observe("pi", SessionReport{Sessions: []Session{sess("s", "--data-agents-guide--", now, "[pi] deepseek-flash", 1, 10)}}, nil, now.Add(time.Minute))); n != 0 {
		t.Fatalf("restart re-counted %d entries", n)
	}
}

// Numbers from Phase 0 (DESIGN.md): at 17:12Z Anthropic reported 34% of the
// 5h window (resets 17:40) and 26% weekly; the shared Claude store showed
// $22.64 and $36.85 API-equivalent in those windows.
func TestEvaluateCalibratesFromReading(t *testing.T) {
	now := time.Date(2026, 9, 24, 17, 12, 0, 0, time.UTC)
	resets := time.Date(2026, 9, 24, 17, 40, 0, 0, time.UTC)
	res := Evaluate(WindowInput{Now: now, Duration: 5 * time.Hour, Consumed: 22.64,
		RecentConsumed: 6.85, RecentSpan: time.Hour, Reading: Reading{Percent: 34, ResetsAt: resets}})
	if !res.Start.Equal(resets.Add(-5 * time.Hour)) {
		t.Fatalf("window start %v, want anchored on the provider reset", res.Start)
	}
	if res.LimitSource != "learned" || math.Abs(res.Limit-66.588) > 0.01 {
		t.Fatalf("learned limit %v (%s)", res.Limit, res.LimitSource)
	}
	if math.Abs(res.Ratio-0.34) > 1e-6 {
		t.Fatalf("ratio %v", res.Ratio)
	}
	// 43.95 remaining at 6.85/h exhausts in ~6.4h — after the 17:40 reset, so
	// no ETA inside this window.
	if !res.ExhaustionETA.IsZero() {
		t.Fatalf("ETA %v should be beyond the reset", res.ExhaustionETA)
	}
	if UsedPercent(Reading{Percent: 34}, res) != 34 {
		t.Fatal("a fresh provider reading must win")
	}
}

func TestEvaluateUnknownIsNotExhausted(t *testing.T) {
	res := Evaluate(WindowInput{Now: time.Now(), Duration: time.Hour, Consumed: 100, Reading: Reading{Percent: -1}})
	if res.Ratio != -1 || res.Remaining != -1 || res.LimitSource != "none" {
		t.Fatalf("no limit must read unknown, got %+v", res)
	}
	if UsedPercent(Reading{Percent: -1}, res) != -1 {
		t.Fatal("unmeasured must stay -1, never 100")
	}
}

func TestEvaluateConfiguredLimitAndETA(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	res := Evaluate(WindowInput{Now: now, Duration: 7 * 24 * time.Hour, Consumed: 80, ConfiguredLimit: 100,
		RecentConsumed: 5, RecentSpan: time.Hour, PreviousLearned: 90, Reading: Reading{Percent: -1}})
	if res.LimitSource != "configured" || res.Remaining != 20 || res.Learned != 90 {
		t.Fatalf("%+v", res)
	}
	if want := now.Add(4 * time.Hour); !res.ExhaustionETA.Equal(want) {
		t.Fatalf("ETA %v, want %v", res.ExhaustionETA, want)
	}
	if UsedPercent(Reading{Percent: -1}, res) != 80 {
		t.Fatal("without a reading, used% is consumed/limit")
	}
}

func TestEvaluateSmoothsLearnedLimit(t *testing.T) {
	now := time.Now()
	res := Evaluate(WindowInput{Now: now, Duration: time.Hour, Consumed: 50, PreviousLearned: 100,
		Reading: Reading{Percent: 25}, Smoothing: 0.5})
	if !near(res.Learned, 150) { // 0.5×200 + 0.5×100
		t.Fatalf("smoothed %v", res.Learned)
	}
	// Below the calibration floor the previous estimate is kept.
	res = Evaluate(WindowInput{Now: now, Duration: time.Hour, Consumed: 1, PreviousLearned: 100, Reading: Reading{Percent: 2}})
	if res.Learned != 100 {
		t.Fatalf("coarse reading moved the estimate: %v", res.Learned)
	}
}

func TestParseProbeConfigMap(t *testing.T) {
	// Verbatim from hive/hive-provider-usage, 2026-09-24T17:12:32Z.
	data := map[string]string{
		"anthropic":        "34% used resets=2026-09-24T17:40:00.326149+00:00",
		"anthropic_limits": `[{"slot":"slot0","percent":34,"resets_at":"2026-09-24T17:40:00.326149+00:00"},{"slot":"slot1","percent":26,"resets_at":"2026-09-30T23:00:00.326175+00:00"}]`,
		"deepseek":         "100% used balance=-1.23",
		"google":           "52% used resets=2026-09-24T19:17:07Z",
		"measured_by":      "hive-reef",
		"meta":             "unknown no-usage-api",
		"openai":           "100% used weekly=100% resets=2026-09-26T08:15:18Z",
		"updated_at":       "2026-09-24T17:12:32Z",
	}
	all, at := ParseProbeConfigMap(data)
	if at.Format(time.RFC3339) != "2026-09-24T17:12:32Z" {
		t.Fatalf("updated_at %v", at)
	}
	if all["meta"].Percent != -1 || all["meta"].Note != "no-usage-api" {
		t.Fatalf("meta %+v", all["meta"])
	}
	if all["openai"].Percent != 100 || all["openai"].ResetsAt.IsZero() {
		t.Fatalf("openai %+v", all["openai"])
	}
	w := all["anthropic"].Reading("slot1", at)
	if w.Percent != 26 || w.ResetsAt.Day() != 30 {
		t.Fatalf("weekly slot %+v", w)
	}
	if _, ok := all["measured_by"]; ok {
		t.Fatal("metadata keys are not providers")
	}
	if ParseProbeValue("garbage").Note != "unpublished" {
		t.Fatal("unparseable value must read unpublished")
	}
}

// Collect wires source layout, per-CODEX_HOME attribution and the
// Antigravity DB scan together, with ccusage stubbed.
func TestCollectAttribution(t *testing.T) {
	home := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(home, ".codex-scanner/sessions/2026/09/20"), 0o755))
	must(os.MkdirAll(filepath.Join(home, ".codex-guide/sessions/2026/09/20"), 0o755))
	must(os.MkdirAll(filepath.Join(home, ".codex/"), 0o755)) // shared login store: no sessions
	agy := filepath.Join(home, ".gemini/antigravity-cli/conversations")
	must(os.MkdirAll(agy, 0o755))
	must(os.WriteFile(filepath.Join(agy, "ffb164b9-1311-4c38-a7bf-e858dbf62f26.db"),
		[]byte("SQLite format 3\x00...workspace file:///data/agents/quality/repo..."), 0o644))

	codexJSON, _ := os.ReadFile("testdata/codex.session.json")
	agyJSON, _ := os.ReadFile("testdata/antigravity.session.json")
	var calls []string
	c := &Collector{Home: home, Scratch: t.TempDir(), Exec: func(_ context.Context, env []string, args ...string) ([]byte, error) {
		ch, homeOK := "", false
		for _, e := range env {
			if strings.HasPrefix(e, "CODEX_HOME=") {
				ch = filepath.Base(strings.TrimPrefix(e, "CODEX_HOME="))
			}
			homeOK = homeOK || e == "HOME="+home
		}
		if homeOK {
			calls = append(calls, args[0]+":"+ch)
		}
		if args[0] == "codex" {
			return codexJSON, nil
		}
		return agyJSON, nil
	}}
	run := c.Collect(context.Background(), Source{Name: "codex", Detect: []string{".codex-*/sessions"}, PerRoot: true})
	if run.Err != nil {
		t.Fatal(run.Err)
	}
	if len(calls) != 2 || calls[0] != "codex:.codex-guide" || calls[1] != "codex:.codex-scanner" {
		t.Fatalf("one ccusage run per CODEX_HOME with HOME=agents' home, got %v", calls)
	}
	seen := map[string]int{}
	for _, s := range run.Report.Sessions {
		seen[run.Attribute(s)]++
	}
	if seen["guide"] != 3 || seen["scanner"] != 3 {
		t.Fatalf("codex attribution %v", seen)
	}

	run = c.Collect(context.Background(), Source{Name: "antigravity",
		Detect: []string{".gemini/antigravity-cli/conversations"}, AttributeFromFile: true})
	if run.Err != nil {
		t.Fatal(run.Err)
	}
	got := map[string]string{}
	for _, s := range run.Report.Sessions {
		got[s.ID] = run.Attribute(s)
	}
	if got["ffb164b9-1311-4c38-a7bf-e858dbf62f26"] != "quality" {
		t.Fatalf("antigravity attribution %v", got)
	}
	for id, a := range got {
		if id != "ffb164b9-1311-4c38-a7bf-e858dbf62f26" && a != Unattributed {
			t.Fatalf("session without a DB must be unattributed, %s → %s", id, a)
		}
	}
	if run.Fingerprint == "" {
		t.Fatal("present source must be fingerprinted")
	}

	// Absent source: no run, no error.
	calls = nil
	run = c.Collect(context.Background(), Source{Name: "goose", Detect: []string{".local/share/goose/sessions"}})
	if run.Err != nil || len(calls) != 0 || run.Fingerprint != "" {
		t.Fatalf("absent source ran: %+v %v", run, calls)
	}
}

// Two mounts of one shared store agree on the fingerprint; a different store
// does not. Mount identity (st_dev/st_ino) differed between pods for the same
// PVC, so content is the only usable identity.
func TestFingerprint(t *testing.T) {
	mk := func(files ...string) string {
		d := t.TempDir()
		root := filepath.Join(d, "projects")
		for _, f := range files {
			p := filepath.Join(root, f)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte("x"), 0o644)
		}
		return root
	}
	a := Fingerprint([]string{mk("-data-agents-scanner/1.jsonl", "-data-agents-guide/2.jsonl")})
	b := Fingerprint([]string{mk("-data-agents-guide/2.jsonl", "-data-agents-scanner/1.jsonl")})
	c := Fingerprint([]string{mk("-data-agents-scanner/3.jsonl")})
	if a != b || a == c || a == "" {
		t.Fatalf("fingerprints a=%s b=%s c=%s", a, b, c)
	}
}

func TestMostFrequentAgent(t *testing.T) {
	b := []byte("x/data/agents/guid\x01 /data/agents/guide\x00/data/agents/guidej /data/agents/guide /data/agents/guide")
	if got := mostFrequentAgent(b); got != "guide" {
		t.Fatalf("got %q", got)
	}
	if got := mostFrequentAgent([]byte("nothing here")); got != Unattributed {
		t.Fatalf("got %q", got)
	}
}
