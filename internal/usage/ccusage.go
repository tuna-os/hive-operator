// Package usage turns agent-CLI session logs into per-provider, per-agent
// consumption, using ccusage (https://github.com/ryoppippi/ccusage) as the
// parser, instead of scraping each CLI's TUI for a "% used" number.
//
// WHAT CCUSAGE GIVES US, AND WHAT IT DOES NOT
// -------------------------------------------
// ccusage reads the transcripts every CLI already writes and reports tokens
// and API-equivalent cost per session, day, week or month. It is local, fast
// (except Antigravity, see below) and has no credentials. It does NOT know how
// much quota is left: no provider publishes that in the logs. Remaining quota
// is therefore limit − consumption, with the limit either configured on the
// UsagePool or learned by calibrating consumption against a provider reading
// (see pool.go). Keep that separation in mind: this package measures spend;
// it never claims to measure headroom.
//
// The per-source JSON shapes are NOT uniform (measured on v20.0.24):
//
//	claude/pi/antigravity session rows: modelBreakdowns[] + totalCost + projectPath
//	codex session rows:                 models{} map + costUSD, no projectPath,
//	                                    no firstActivity, directory = date path
//
// normalise() below is the one place that knows this.
package usage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Tokens is one token breakdown. Cost is API-equivalent USD as ccusage prices
// it; a model ccusage cannot price contributes 0 and is listed in
// SessionReport.UnpricedModels — never read a 0 cost as "free".
type Tokens struct {
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cacheRead"`
	CacheCreate int64   `json:"cacheCreate"`
	Reasoning   int64   `json:"reasoning,omitempty"`
	Total       int64   `json:"total"`
	CostUSD     float64 `json:"costUSD"`
}

// Add accumulates o into t.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheCreate += o.CacheCreate
	t.Reasoning += o.Reasoning
	t.Total += o.Total
	t.CostUSD += o.CostUSD
}

// Sub returns t − o, clamped at zero per field. A session whose counters went
// backwards (file rewritten, compaction) must not produce negative spend.
func (t Tokens) Sub(o Tokens) Tokens {
	c := func(a, b int64) int64 {
		if a < b {
			return 0
		}
		return a - b
	}
	cost := t.CostUSD - o.CostUSD
	if cost < 0 {
		cost = 0
	}
	return Tokens{
		Input: c(t.Input, o.Input), Output: c(t.Output, o.Output),
		CacheRead: c(t.CacheRead, o.CacheRead), CacheCreate: c(t.CacheCreate, o.CacheCreate),
		Reasoning: c(t.Reasoning, o.Reasoning), Total: c(t.Total, o.Total), CostUSD: cost,
	}
}

// IsZero is true when nothing was consumed.
func (t Tokens) IsZero() bool { return t.Total == 0 && t.CostUSD == 0 && t.Output == 0 }

// Session is one normalised ccusage session row.
type Session struct {
	ID string `json:"id"`
	// Project is ccusage's projectPath (claude, pi, antigravity) — the only
	// per-agent signal most sources carry.
	Project string    `json:"project,omitempty"`
	First   time.Time `json:"first,omitempty"`
	Last    time.Time `json:"last"`
	// Models maps model id → consumption within this session.
	Models map[string]Tokens `json:"models"`
}

// Total sums a session over models.
func (s Session) Total() Tokens {
	var t Tokens
	for _, m := range s.Models {
		t.Add(m)
	}
	return t
}

// SessionReport is `ccusage <source> session --json`, normalised.
type SessionReport struct {
	Sessions       []Session `json:"sessions"`
	UnpricedModels []string  `json:"unpricedModels,omitempty"`
}

type rawBreakdown struct {
	ModelName           string  `json:"modelName"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

type rawCodexModel struct {
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	CacheCreationTokens   int64 `json:"cacheCreationTokens"`
	CacheReadTokens       int64 `json:"cacheReadTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type rawSession struct {
	SessionID      string                   `json:"sessionId"`
	ProjectPath    string                   `json:"projectPath"`
	FirstActivity  string                   `json:"firstActivity"`
	LastActivity   string                   `json:"lastActivity"`
	TotalCost      *float64                 `json:"totalCost"`
	CostUSD        *float64                 `json:"costUSD"`
	TotalTokens    int64                    `json:"totalTokens"`
	ModelBreakdown []rawBreakdown           `json:"modelBreakdowns"`
	Models         map[string]rawCodexModel `json:"models"`
}

type rawSessionReport struct {
	Sessions []rawSession `json:"sessions"`
	Totals   struct {
		UnpricedModels []string `json:"unpricedModels"`
	} `json:"totals"`
}

// ParseSessionReport parses `ccusage <source> session --json` output from any
// source ccusage supports.
func ParseSessionReport(b []byte) (SessionReport, error) {
	var raw rawSessionReport
	if err := json.Unmarshal(b, &raw); err != nil {
		return SessionReport{}, fmt.Errorf("decode ccusage session report: %w", err)
	}
	out := SessionReport{UnpricedModels: raw.Totals.UnpricedModels}
	for _, rs := range raw.Sessions {
		s, err := normalise(rs)
		if err != nil {
			return SessionReport{}, err
		}
		out.Sessions = append(out.Sessions, s)
	}
	return out, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func normalise(rs rawSession) (Session, error) {
	s := Session{ID: rs.SessionID, Project: rs.ProjectPath, Models: map[string]Tokens{}}
	var err error
	if s.First, err = parseTime(rs.FirstActivity); err != nil {
		return s, err
	}
	if s.Last, err = parseTime(rs.LastActivity); err != nil {
		return s, err
	}
	switch {
	case len(rs.ModelBreakdown) > 0:
		for _, b := range rs.ModelBreakdown {
			t := s.Models[b.ModelName]
			t.Add(Tokens{
				Input: b.InputTokens, Output: b.OutputTokens,
				CacheRead: b.CacheReadTokens, CacheCreate: b.CacheCreationTokens,
				Total:   b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheCreationTokens,
				CostUSD: b.Cost,
			})
			s.Models[b.ModelName] = t
		}
	case len(rs.Models) > 0:
		// Codex prices the session, not each model: split the session cost
		// across models by total tokens. Exact when one model ran, which is
		// the overwhelming case for an agent session.
		var sessTokens int64
		names := make([]string, 0, len(rs.Models))
		for n, m := range rs.Models {
			sessTokens += m.TotalTokens
			names = append(names, n)
		}
		sort.Strings(names)
		cost := 0.0
		if rs.CostUSD != nil {
			cost = *rs.CostUSD
		} else if rs.TotalCost != nil {
			cost = *rs.TotalCost
		}
		for _, n := range names {
			m := rs.Models[n]
			share := 0.0
			if sessTokens > 0 {
				share = cost * float64(m.TotalTokens) / float64(sessTokens)
			}
			s.Models[n] = Tokens{
				Input: m.InputTokens, Output: m.OutputTokens,
				CacheRead: m.CacheReadTokens, CacheCreate: m.CacheCreationTokens,
				Reasoning: m.ReasoningOutputTokens, Total: m.TotalTokens, CostUSD: share,
			}
		}
	}
	return s, nil
}

// AgentFromProject maps a ccusage projectPath to a hive agent name.
//
// Hive runs every agent with cwd /data/agents/<name>, and each CLI encodes the
// cwd into its project key differently:
//
//	claude       -data-agents-<name>
//	pi           --data-agents-<name>--
//	contributor  -home-dev--local-state-hive-agent-cwd   (hive-contributors pods)
//
// Anything else is "unattributed": still counted against the provider pool,
// never silently dropped — dropping it would under-report a shared account.
func AgentFromProject(project string) string {
	p := strings.Trim(project, "-")
	switch {
	case strings.HasPrefix(p, "data-agents-"):
		return strings.TrimPrefix(p, "data-agents-")
	case strings.Contains(p, "hive-agent-cwd"):
		return ContributorAgent
	case p == "":
		return Unattributed
	}
	return Unattributed
}

const (
	// Unattributed is consumption no agent can be named for.
	Unattributed = "_unattributed"
	// ContributorAgent is consumption from the hive-contributors pool pods,
	// which share the credential store (and therefore the quota) with spokes.
	ContributorAgent = "_contributor"
)
