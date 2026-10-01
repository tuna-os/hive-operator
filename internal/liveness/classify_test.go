package liveness

import (
	"strconv"
	"strings"
	"testing"
)

// Every classification pane_classify_text knows, with panes shaped like the
// ones that earned each rule (and the near misses that must stay ready).
func TestClassify(t *testing.T) {
	cases := []struct {
		name, pane, want string
	}{
		{"empty", "", StateEmpty},
		{"blank lines only", "\n   \n\t\n", StateEmpty},
		{"muse ready", "  muse-spark-1.3-contributor · max · /data/agents/supervisor · Auto-review", StateReady},
		{"pi ready", "─────\n                                           (kiro-api-key) claude-haiku-4-5 • low", StateReady},
		{"agy theme wizard", "Choose your color scheme\n> Dark\n  Light\n[Next]", StateWizard},
		{"agy ToS", "Terms of Service & Data Use\nPress enter to continue", StateWizard},
		{"antigravity welcome", "Welcome to Antigravity\n", StateWizard},
		{"muse trust", "Do you trust this workspace?\n  1. Yes\n  2. No\nPermission denied (os error 13)", StateWizard},
		{"claude trust folder", "Do you trust the contents of this directory?\n❯ 1. Yes, I trust this folder", StateWizard},
		{"claude login picker", "Select login method:\n ❯ 1. Claude account with subscription", StateAuth},
		{"login expired", "API Error: 401 · Login expired · Please run /login", StateAuth},
		{"copilot device flow", "Sign in to use Copilot\nPaste code here if prompted >", StateAuth},
		{"browser didn't open", "Browser didn't open? Use the url below to sign in", StateAuth},
		{"muse approval", "Would you like to run the following command?\n  env | grep GH_\n› 1. Yes, proceed (y)\n  2. No (esc)", StateApproval},
		{"approval question without menu chrome is not approval", "Would you like to run the following command?\n  ls", StateReady},
		{"menu without question is not approval", "› 1. Yes, proceed (y)", StateReady},
		{"bare shell", "some output\nhive-architect@hive-6f77b74574-qwtj9:/data/agents/architect$", StateShell},
		{"bare shell, trailing spaces", "hive-architect@hive-6f7:/data/agents/architect$   \n\n", StateShell},
		{"root prompt", "/ #", StateShell},
		{"dead launch line", "hive-guide@hive-6f7:/data/agents/guide$ pi --model kiro-api-key/claude-sonnet-5:medium", StateShell},
		{"dollar mid-line stays ready", "cost so far: $3 used", StateReady},
		// Loose English an agent may be READING must not trip a chrome rule.
		{"issue body about logging in", "Fix: users cannot log in after token refresh (#412)", StateReady},
		// Wizard outranks auth (both can be on screen: agy's first run).
		{"wizard beats auth", "Choose your color scheme\nNot logged in", StateWizard},
	}
	for _, c := range cases {
		if got := Classify(c.pane); got != c.want {
			t.Errorf("%s: Classify = %s, want %s\n%s", c.name, got, c.want, c.pane)
		}
	}
}

// The heal backoff ladder: 0 before the first heal, then 5, 10, 20, 40,
// 80, capped at 120 minutes.
func TestBackoffLadder(t *testing.T) {
	want := []int{0, 5, 10, 20, 40, 80, 120, 120, 120}
	for n, w := range want {
		if got := BackoffSeconds(n, 5, 120); got != w*60 {
			t.Errorf("n=%d: %ds, want %dm", n, got, w)
		}
	}
	// HIVE_WATCHDOG_BASE_MIN honoured; a cap below 2×base clamps.
	if got := BackoffSeconds(2, 7, 10); got != 600 {
		t.Errorf("base 7 cap 10, n=2: %d", got)
	}
}

func TestEffortChange(t *testing.T) {
	cases := []struct{ b, m, rec, want string }{
		{"agy", "gemini-3.8-flash-high", "", "high"}, // never set = low
		{"agy", "gemini-3.8-flash-high", "low", "high"},
		{"agy", "gemini-3.8-flash-high", "high", ""},
		{"agy", "gemini-3.8-flash-low", "", ""},
		{"agy", "gemini-3.8-flash-medium", "medium", ""},
		{"agy", "gemini-3.8-flash", "", ""}, // unsuffixed: nothing required
		{"pi", "kiro-api-key/claude-opus-5:high", "low", ""},
	}
	for _, c := range cases {
		if got := EffortChange(c.b, c.m, c.rec); got != c.want {
			t.Errorf("EffortChange(%s,%s,%q) = %q, want %q", c.b, c.m, c.rec, got, c.want)
		}
	}
}

func TestLongestCadences(t *testing.T) {
	gov := `governor:
    modes:
        busy:
            supervisor: 5m
            architect: 30m
            telemetry: paused
            threshold: 5
        idle:
            supervisor: 1h
            architect: 2h
            telemetry: paused
            weird: 3d
            threshold: 0
    eval_interval: 5m
            ghost: 9h
`
	got := LongestCadences(gov)
	var s []string
	for _, c := range got {
		s = append(s, c.Agent+"="+strconv.Itoa(c.Seconds))
	}
	// telemetry is paused in every mode → no entry (never nudged); `weird`
	// does not parse; `threshold` is a knob; `ghost` is past `modes:`.
	if strings.Join(s, " ") != "supervisor=3600 architect=7200" {
		t.Fatalf("%v", s)
	}
}
