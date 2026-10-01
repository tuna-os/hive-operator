package usage

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Key identifies one consumption series.
type Key struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Agent    string `json:"agent"`
	Model    string `json:"model"`
}

// Entry is consumption attributed to one instant.
type Entry struct {
	At time.Time `json:"at"`
	Key
	Tokens Tokens `json:"tokens"`
}

// Ledger turns repeated cumulative session reports into a time series.
//
// WHY A LEDGER
// ------------
// Provider quotas are windows — Anthropic's 5-hour session and weekly caps,
// Codex's primary/secondary windows, Antigravity's per-group resets — whose
// start is set by the provider, not by the calendar. ccusage only groups by
// day, week, month or session (blocks are Claude-only, and anchor on the hour
// of the first message, not on the provider's reset). None of those can answer
// "how much since 12:40".
//
// Session totals, however, are cumulative and keyed. Differencing each
// session's per-model totals between collections gives the spend in that
// interval, attributable to the agent that owns the session. Summing entries
// since any instant then answers any window exactly, to the collection
// interval. On the first collection (or after a restart without state) there
// is no previous total, so each session is back-filled across its own
// [first, last] activity span — approximate, and marked as such by Primed.
type Ledger struct {
	mu        sync.Mutex
	Retention time.Duration
	seen      map[string]map[string]Tokens // source\x00session → model → cumulative
	seenAt    map[string]time.Time         // source\x00session → last collection that listed it
	lastSeen  map[string]time.Time         // source → last collection
	entries   []Entry
}

// NewLedger returns a ledger that keeps retention of history.
func NewLedger(retention time.Duration) *Ledger {
	return &Ledger{Retention: retention, seen: map[string]map[string]Tokens{}, seenAt: map[string]time.Time{}, lastSeen: map[string]time.Time{}}
}

// Attributor names the agent a session belongs to.
type Attributor func(Session) string

// Primed reports whether source has at least one previous collection, i.e.
// whether new entries are measured deltas rather than back-fill.
func (l *Ledger) Primed(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.lastSeen[source]
	return ok
}

// Observe folds one collection of source into the ledger and returns the
// entries it added (the deltas — what Prometheus counters should grow by).
// Sessions absent from rep are left alone: a transcript that ages out of the
// lookback view has not been refunded.
func (l *Ledger) Observe(source string, rep SessionReport, attribute Attributor, now time.Time) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	now = now.UTC()
	prevAt, primed := l.lastSeen[source]
	start := len(l.entries)
	for _, s := range rep.Sessions {
		id := source + "\x00" + s.ID
		prev := l.seen[id]
		agent := Unattributed
		if attribute != nil {
			agent = attribute(s)
		}
		from, to := s.First, s.Last
		if from.IsZero() {
			from = to
		}
		if primed && prev != nil {
			// Known session: its growth happened since the last collection.
			if from.Before(prevAt) {
				from = prevAt
			}
		}
		if to.IsZero() || to.After(now) {
			to = now
		}
		if from.After(to) {
			from = to
		}
		models := make([]string, 0, len(s.Models))
		for m := range s.Models {
			models = append(models, m)
		}
		sort.Strings(models)
		cur := make(map[string]Tokens, len(s.Models))
		for _, m := range models {
			t := s.Models[m]
			cur[m] = t
			delta := t.Sub(prev[m])
			if delta.IsZero() {
				continue
			}
			k := Key{Source: source, Provider: ProviderOf(source, m), Agent: agent, Model: m}
			l.spread(k, delta, from, to)
		}
		l.seen[id] = cur
		l.seenAt[id] = now
	}
	l.lastSeen[source] = now
	added := append([]Entry(nil), l.entries[start:]...)
	l.pruneLocked(now)
	return added
}

// spread distributes t uniformly over [from, to] in slices of at most an hour.
// A span under an hour is one entry at its end (the common, measured case:
// growth since the previous collection).
func (l *Ledger) spread(k Key, t Tokens, from, to time.Time) {
	span := to.Sub(from)
	n := int(span / time.Hour)
	if span%time.Hour != 0 {
		n++
	}
	if n <= 1 {
		l.entries = append(l.entries, Entry{At: to, Key: k, Tokens: t})
		return
	}
	var acc Tokens
	for i := 1; i <= n; i++ {
		part := Tokens{
			Input: t.Input * int64(i) / int64(n), Output: t.Output * int64(i) / int64(n),
			CacheRead: t.CacheRead * int64(i) / int64(n), CacheCreate: t.CacheCreate * int64(i) / int64(n),
			Reasoning: t.Reasoning * int64(i) / int64(n), Total: t.Total * int64(i) / int64(n),
			CostUSD: t.CostUSD * float64(i) / float64(n),
		}
		slice := part.Sub(acc)
		acc = part
		// Stamp each slice at its midpoint so a window boundary on a slice
		// edge splits the session proportionally.
		at := from.Add(span * time.Duration(2*i-1) / time.Duration(2*n))
		l.entries = append(l.entries, Entry{At: at, Key: k, Tokens: slice})
	}
}

func (l *Ledger) pruneLocked(now time.Time) {
	sort.SliceStable(l.entries, func(i, j int) bool { return l.entries[i].At.Before(l.entries[j].At) })
	if l.Retention <= 0 {
		return
	}
	cut := now.Add(-l.Retention)
	// Forget sessions no collection has listed for a whole retention period;
	// if one ever reappears it is back-filled across its own span, which
	// lands before the cut and is pruned again.
	for id, at := range l.seenAt {
		if at.Before(cut) {
			delete(l.seenAt, id)
			delete(l.seen, id)
		}
	}
	i := sort.Search(len(l.entries), func(i int) bool { return !l.entries[i].At.Before(cut) })
	l.entries = append([]Entry(nil), l.entries[i:]...)
}

// Since sums every entry at or after since, per key.
func (l *Ledger) Since(since time.Time) map[Key]Tokens {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[Key]Tokens{}
	i := sort.Search(len(l.entries), func(i int) bool { return !l.entries[i].At.Before(since) })
	for _, e := range l.entries[i:] {
		t := out[e.Key]
		t.Add(e.Tokens)
		out[e.Key] = t
	}
	return out
}

type ledgerState struct {
	Seen     map[string]map[string]Tokens `json:"seen"`
	SeenAt   map[string]time.Time         `json:"seenAt"`
	LastSeen map[string]time.Time         `json:"lastSeen"`
	Entries  []Entry                      `json:"entries"`
}

// MarshalJSON snapshots the ledger so a restarted sidecar resumes measuring
// deltas instead of back-filling again.
func (l *Ledger) MarshalJSON() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return json.Marshal(ledgerState{Seen: l.seen, SeenAt: l.seenAt, LastSeen: l.lastSeen, Entries: l.entries})
}

// UnmarshalJSON restores a snapshot.
func (l *Ledger) UnmarshalJSON(b []byte) error {
	var st ledgerState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if st.Seen == nil {
		st.Seen = map[string]map[string]Tokens{}
	}
	if st.SeenAt == nil {
		st.SeenAt = map[string]time.Time{}
	}
	if st.LastSeen == nil {
		st.LastSeen = map[string]time.Time{}
	}
	l.seen, l.seenAt, l.lastSeen, l.entries = st.Seen, st.SeenAt, st.LastSeen, st.Entries
	return nil
}
