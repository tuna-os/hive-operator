package usage

import (
	"fmt"
	"net/url"
	"sort"
	"time"
)

// SchemaVersion is bumped on any incompatible change to Report.
const SchemaVersion = 1

// Report is what the hive-usage sidecar serves at GET /v1/usage.
type Report struct {
	Schema      int            `json:"schema"`
	Pod         string         `json:"pod,omitempty"`
	Namespace   string         `json:"namespace,omitempty"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Sources     []SourceStatus `json:"sources"`
	// Windows answers each requested ?since= (or the default 1h/5h/24h/7d).
	Windows []WindowUsage `json:"windows"`
}

// SourceStatus is the health of one source's collection.
type SourceStatus struct {
	Source string `json:"source"`
	// Fingerprint identifies the underlying store; equal fingerprints from
	// different pods are the same shared store and must be counted once.
	Fingerprint     string    `json:"fingerprint,omitempty"`
	OK              bool      `json:"ok"`
	Error           string    `json:"error,omitempty"`
	LastRun         time.Time `json:"lastRun,omitempty"`
	LastSuccess     time.Time `json:"lastSuccess,omitempty"`
	DurationSeconds float64   `json:"durationSeconds"`
	Roots           int       `json:"roots"`
	Sessions        int       `json:"sessions"`
	// Primed is false until the source has been collected twice: until then
	// every number is back-filled from session spans, not measured.
	Primed         bool     `json:"primed"`
	UnpricedModels []string `json:"unpricedModels,omitempty"`
}

// WindowUsage is consumption since an instant.
type WindowUsage struct {
	Label string    `json:"label,omitempty"`
	Since time.Time `json:"since"`
	Rows  []Row     `json:"rows"`
}

// Row is one (source, provider, agent, model) total.
type Row struct {
	Key
	Tokens Tokens `json:"tokens"`
}

// DefaultWindows are served when a caller asks for none.
var DefaultWindows = []struct {
	Label string
	D     time.Duration
}{{"1h", time.Hour}, {"5h", 5 * time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}}

// Rows flattens a ledger sum, sorted for stable output.
func Rows(m map[Key]Tokens) []Row {
	out := make([]Row, 0, len(m))
	for k, t := range m {
		out = append(out, Row{Key: k, Tokens: t})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.Model < b.Model
	})
	return out
}

// Query builds the /v1/usage query for the given window starts.
func Query(since ...time.Time) string {
	v := url.Values{}
	for _, s := range since {
		v.Add("since", s.UTC().Format(time.RFC3339))
	}
	return "/v1/usage?" + v.Encode()
}

// ParseSince reads ?since= values.
func ParseSince(q url.Values) ([]time.Time, error) {
	var out []time.Time
	for _, s := range q["since"] {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("since=%q: %w", s, err)
		}
		out = append(out, t.UTC())
	}
	return out, nil
}

// Window returns the usage block for since, or nil.
func (r *Report) Window(since time.Time) *WindowUsage {
	for i := range r.Windows {
		if r.Windows[i].Since.Equal(since) {
			return &r.Windows[i]
		}
	}
	return nil
}
