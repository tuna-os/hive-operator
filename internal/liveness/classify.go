// Package liveness is the watchdog and nudge planner that replaces
// `hive-rotate.sh watchdog` (the hive-watchdog, -reef, -hanthor CronJobs,
// every 5 minutes) and `hive-nudge.sh nudge` (hive-nudge, :13 and :43).
//
// PORTED, NOT REDESIGNED
// ----------------------
// Every rule below is in the LIVE scripts (ConfigMap hive/hive-ops-scripts,
// 2026-10-01: hive-rotate.sh 1922 lines, hive-nudge.sh 176, hive-lib.sh 668)
// and was earned by an incident; comments name the bash function. Output
// lines use the bash printf formats byte for byte so a Shadow plan can be
// diffed against the job log (cmd/hive-shadow-diff --liveness).
//
// The planner is pure: the controller fetches /api/status, the live panes it
// asks for (NeedsLivePane), the governor block and the readings, and later
// applies the Actions. Placement decisions (rotate-off, mismatch repair) come
// from internal/rotation, not from here.
package liveness

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strings"
)

// Pane states (pane_classify_text) plus the watchdog's own `stalled`.
const (
	StateReady    = "ready"
	StateWizard   = "wizard"
	StateAuth     = "auth"
	StateApproval = "approval"
	StateShell    = "shell"
	StateEmpty    = "empty"
	StateStalled  = "stalled"
)

var (
	// First-run wizards (agy theme/ToS, workspace trust prompts incl.
	// muse's) are DIALOGS caused by shared-home permission drift — repair
	// and relaunch in place, never rotate off a healthy provider over one.
	reWizard = regexp.MustCompile(`(?i)\[next\]|\[previous\]|terms of service & data use|accent: highlighted|enter toggl|choose your color scheme|do you trust the contents|do you trust this workspace|i trust this folder|welcome to (the )?antigravity`)
	reAuth   = regexp.MustCompile(`(?i)login expired|run /login|not logged in|please run /login|please use /login|select login method|sign in to use copilot|paste code here if prompted|browser didn.t open\? use the url below`)
	// muse parked on a tool-approval prompt: v5.35+ launches hosted muse
	// with --approval-mode on-request and the LLM judge escalates
	// env-reading commands to a human who never comes (reef/outreach and
	// hanthor/guide sat 38-53 min on it, 2026-09-24). Anchored to muse's
	// own menu chrome; BOTH lines must be present.
	reApprovalQ   = regexp.MustCompile(`Would you like to run the following command\?`)
	reApprovalYes = regexp.MustCompile(`Yes, proceed \(y\)`)
	// The agents' prompts are `hive-<agent>@<pod>:<path>$`; a trailing
	// command after it is a launch line the CLI never drew over.
	reLaunchLine = regexp.MustCompile(`^hive-.*@.*:.*\$ `)
)

func blank(l string) bool { return strings.TrimSpace(l) == "" }

// Classify ports pane_classify_text: ready|wizard|auth|approval|shell|empty.
// Patterns are anchored to CLI CHROME, never loose English an agent might
// be reading in an issue body. grep matches per line, which Go's `.` (no
// newline) reproduces.
func Classify(text string) string {
	lines := strings.Split(text, "\n")
	var last string
	for _, l := range lines {
		if !blank(l) {
			last = l
		}
	}
	if last == "" {
		return StateEmpty
	}
	if reWizard.MatchString(text) {
		return StateWizard
	}
	if reAuth.MatchString(text) {
		return StateAuth
	}
	if reApprovalQ.MatchString(text) && reApprovalYes.MatchString(text) {
		return StateApproval
	}
	// A shell prompt as the LAST line = the CLI exited to bash.
	last = strings.TrimRight(last, " \t\r\v\f")
	if strings.HasSuffix(last, "$") || strings.HasSuffix(last, "#") || reLaunchLine.MatchString(last) {
		return StateShell
	}
	return StateReady
}

// PaneHash is the stall detector's fingerprint (`md5sum | cut -c1-16`).
func PaneHash(text string) string {
	h := md5.Sum([]byte(text))
	return hex.EncodeToString(h[:])[:16]
}

// BackoffSeconds ports watchdog_backoff_s: 0 before the first heal, then
// base, 2×base, 4×base … capped (the CrashLoopBackOff analog). A fixed
// 5-minute interval restarted a permanently broken agent 288×/day, and v5
// counts each restart into its own crash-loop breaker.
func BackoffSeconds(n, baseMin, capMin int) int {
	if n < 1 {
		return 0
	}
	m := baseMin
	for n > 1 && m < capMin {
		m *= 2
		n--
	}
	if m > capMin {
		m = capMin
	}
	return m * 60
}

// AgyEffort ports agy_effort_of (only agy; "" for any other backend or an
// unsuffixed model).
func agyEffort(backend, model string) string {
	if backend != "agy" {
		return ""
	}
	for _, e := range []string{"high", "medium", "low"} {
		if strings.HasSuffix(model, "-"+e) {
			return e
		}
	}
	return ""
}

// EffortChange ports effort_change: the effort to set, or "". recorded is
// the effort the agent runs with ("" = never set, which v5/v6 launch as
// agy's default, low).
//
// Bash keeps `recorded` in a per-agent file on the ops PVC because v5 did
// not expose the effort; v6's /api/status does (reasoningEffort), so the
// operator compares against the hive's own value. Where the two disagree
// (a hand-set effort bash never recorded) the operator is the one that is
// right; that is a known shadow-diff class, not a logic difference.
func EffortChange(backend, model, recorded string) string {
	if backend != "agy" {
		return ""
	}
	want := agyEffort(backend, model)
	if want == "" {
		return ""
	}
	if recorded == "" {
		recorded = "low"
	}
	if want == recorded {
		return ""
	}
	return want
}
