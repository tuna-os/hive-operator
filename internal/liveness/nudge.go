package liveness

import (
	"bufio"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Cadence is one agent's LONGEST cadence across every governor mode.
type Cadence struct {
	Agent   string
	Seconds int
}

var (
	reModes     = regexp.MustCompile(`^    modes:`)
	reSection   = regexp.MustCompile(`^    [a-z_]+:`)
	reAgentLine = regexp.MustCompile(`^            [a-zA-Z_-]+: `)
	reDur       = regexp.MustCompile(`^([0-9]+)([mhs])$`)
)

// LongestCadences ports hive-nudge.sh's awk over the `governor:` block of
// /data/hive.yaml.runtime: 4-space `modes:`, 8 per mode, 12 per agent
// cadence. `paused` (and anything unparseable) contributes nothing, so an
// agent paused in EVERY mode gets no entry and is never nudged — that is a
// deliberate config, not a stall. `threshold` is a mode knob, not an agent.
//
// QUIRK: awk's `for (a in max)` walks its hash in an unspecified order and
// the per-spoke cap (4) applies in that order. The port keeps first
// appearance in the file, which is deterministic; the two differ only when
// more than four agents are overdue on one spoke at once.
func LongestCadences(governor string) []Cadence {
	var order []string
	max := map[string]int{}
	in := false
	sc := bufio.NewScanner(strings.NewReader(governor))
	for sc.Scan() {
		l := sc.Text()
		if reModes.MatchString(l) {
			in = true
			continue
		}
		if in && reSection.MatchString(l) {
			in = false
		}
		if !in || !reAgentLine.MatchString(l) {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		name := strings.TrimSuffix(f[0], ":")
		if name == "threshold" {
			continue
		}
		m := reDur.FindStringSubmatch(f[1])
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		s := n
		switch m[2] {
		case "m":
			s = n * 60
		case "h":
			s = n * 3600
		}
		if _, seen := max[name]; !seen {
			order = append(order, name)
			max[name] = 0
		}
		if s > max[name] {
			max[name] = s
		}
	}
	var out []Cadence
	for _, a := range order {
		if max[a] > 0 {
			out = append(out, Cadence{a, max[a]})
		}
	}
	return out
}

// NudgeAgent is the slice of /api/status nudge reads.
type NudgeAgent struct {
	Name     string
	Paused   bool
	OnDemand bool
	// Enabled nil = absent (v6 does not send it), which jq's
	// `.enabled != false` treats as enabled.
	Enabled *bool
}

// NudgePolicy mirrors HIVE_NUDGE_*.
type NudgePolicy struct {
	Grace    int // HIVE_NUDGE_GRACE, 2 (× the longest cadence)
	FloorS   int // HIVE_NUDGE_FLOOR_S, 1800
	MaxPerNS int // HIVE_NUDGE_MAX_PER_NS, 4
}

// DefaultNudgePolicy mirrors the bash defaults.
func DefaultNudgePolicy() NudgePolicy { return NudgePolicy{Grace: 2, FloorS: 1800, MaxPerNS: 4} }

// NudgeInput is one spoke's nudge pass.
type NudgeInput struct {
	Namespace string
	Cadences  []Cadence
	// CadencesErr: the governor block could not be read.
	CadencesErr bool
	Agents      []NudgeAgent
	// Idle: seconds since last_kick per agent from hive-state.json; -1 (or a
	// last_kick that does not parse) is "never", which bash reads as 999999.
	Idle            map[string]int64
	BudgetExhausted bool
	BudgetPctUsed   float64
	BudgetWeekly    float64
	Policy          NudgePolicy
}

// Overdue is one agent nudge would kick, with its line WITHOUT the outcome
// suffix (" — kicked (queued)", " — would nudge", …).
type Overdue struct {
	Agent    string
	IdleS    int64
	ThreshS  int
	Longest  int
	Line     string
	Executed bool
}

// NudgePlan is one spoke's nudge pass. Candidates are in kick order; the
// executor stops after Policy.MaxPerNS SUCCESSFUL kicks (a failed or
// unanswered kick does not count toward the cap, as in bash).
type NudgePlan struct {
	// Skip is the one line printed instead of a pass (budget exhausted,
	// cadences unreadable), or "".
	Skip       string
	Candidates []Overdue
}

// Nudge ports hive-nudge.sh for one namespace.
//
// The threshold is each agent's LONGEST cadence across every governor mode
// × Grace, floored at FloorS: if an agent has not been kicked in longer
// than its own slowest schedule allows, no mode explains it. NEVER nudges a
// spoke whose token budget is exhausted — the governor suppresses kicks on
// purpose there, and nudging past it spends money the operator capped.
func Nudge(in NudgeInput) NudgePlan {
	pol := in.Policy
	if pol.Grace == 0 {
		pol = DefaultNudgePolicy()
	}
	if in.CadencesErr || len(in.Cadences) == 0 {
		return NudgePlan{Skip: fmt.Sprintf("%-14s could not read cadences — skipped", in.Namespace)}
	}
	if in.BudgetExhausted {
		return NudgePlan{Skip: fmt.Sprintf("%-14s budget exhausted (%d%% of %s) — NOT nudging; this is a cost control, not a stall",
			in.Namespace, int64(math.Floor(in.BudgetPctUsed)), strconv.FormatFloat(in.BudgetWeekly, 'f', -1, 64))}
	}
	byName := map[string]NudgeAgent{}
	for _, a := range in.Agents {
		if _, dup := byName[a.Name]; !dup {
			byName[a.Name] = a
		}
	}
	var p NudgePlan
	for _, c := range in.Cadences {
		a, ok := byName[c.Agent]
		if !ok || a.Paused || a.OnDemand || (a.Enabled != nil && !*a.Enabled) {
			continue
		}
		idle, ok := in.Idle[c.Agent]
		if !ok {
			continue
		}
		if idle < 0 {
			idle = 999999
		}
		thr := c.Seconds * pol.Grace
		if thr < pol.FloorS {
			thr = pol.FloorS
		}
		if idle <= int64(thr) {
			continue
		}
		p.Candidates = append(p.Candidates, Overdue{Agent: c.Agent, IdleS: idle, ThreshS: thr, Longest: c.Seconds,
			Line: fmt.Sprintf("%-14s %-14s idle %dm > %dm (longest cadence %dm x%d)",
				in.Namespace, c.Agent, idle/60, thr/60, c.Seconds/60, pol.Grace)})
	}
	return p
}

// Kick outcomes.
const (
	KickOK           = "ok"
	KickUnanswered   = "unanswered"
	KickFailed       = "failed"
	KickShadow       = "shadow"
	KickNotAttempted = ""
)

// KickResult is one executed (or simulated) kick.
type KickResult struct {
	Outcome string
	// Detail: the hive's status ("queued") or error text.
	Detail string
}

// NudgeLines renders the pass as hive-nudge.sh prints it for one
// namespace. results[i] belongs to Candidates[i]; a candidate with no
// result was not attempted (the cap was reached) and prints nothing, as
// bash `break`s out of the loop.
func NudgeLines(p NudgePlan, results []KickResult, kickTimeoutS int) []string {
	if p.Skip != "" {
		return []string{p.Skip}
	}
	var out []string
	for i, c := range p.Candidates {
		if i >= len(results) || results[i].Outcome == KickNotAttempted {
			break
		}
		r := results[i]
		switch r.Outcome {
		case KickShadow:
			out = append(out, c.Line+" — would nudge")
		case KickOK:
			out = append(out, fmt.Sprintf("%s — kicked (%s)", c.Line, r.Detail))
		case KickUnanswered:
			out = append(out, fmt.Sprintf("%s — kick not answered in %ds (agent likely wedged mid-delivery); skipped", c.Line, kickTimeoutS))
		default:
			d := r.Detail
			if len(d) > 100 {
				d = d[:100]
			}
			out = append(out, c.Line+" — KICK FAILED: "+d)
		}
	}
	return out
}

// NudgeFooter is the summary line (bash prints one for the whole fleet;
// the operator prints one per spoke).
func NudgeFooter(results []KickResult) string {
	total, failed, shadow := 0, 0, false
	for _, r := range results {
		switch r.Outcome {
		case KickOK:
			total++
		case KickShadow:
			total++
			shadow = true
		case KickUnanswered:
			failed++
		}
	}
	switch {
	case failed > 0:
		return fmt.Sprintf("nudge: %d kicked, %d not answered", total, failed)
	case total == 0:
		return "nudge: nothing overdue"
	case shadow:
		return fmt.Sprintf("nudge: %d agent(s) would be nudged", total)
	}
	return fmt.Sprintf("nudge: %d agent(s) nudged", total)
}

// ShadowKicks simulates the executor with every kick succeeding: the first
// MaxPerNS candidates.
func ShadowKicks(p NudgePlan, maxPerNS int) []KickResult {
	if maxPerNS <= 0 {
		maxPerNS = DefaultNudgePolicy().MaxPerNS
	}
	var out []KickResult
	for i := range p.Candidates {
		if i >= maxPerNS {
			break
		}
		out = append(out, KickResult{Outcome: KickShadow})
	}
	return out
}
