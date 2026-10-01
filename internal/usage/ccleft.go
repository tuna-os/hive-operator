package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tuna-os/ccleft"
)

// CcleftOutput is the document `ccleft serve` answers on GET /readings. The
// readings are ccleft's own library type, so the operator and the prober
// cannot drift apart on the schema.
type CcleftOutput struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Readings    []ccleft.Reading `json:"readings"`
}

// ReadingsFetcher reads a ccleft serve instance.
type ReadingsFetcher interface {
	Readings(ctx context.Context, url string) (*CcleftOutput, error)
}

// HTTPReadingsFetcher reads GET <url>/readings over plain HTTP. ccleft answers
// from its cache: this never causes an upstream quota call, so the operator
// can read it as often as it reconciles.
type HTTPReadingsFetcher struct {
	HTTP *http.Client
}

// Readings implements ReadingsFetcher.
func (f *HTTPReadingsFetcher) Readings(ctx context.Context, url string) (*CcleftOutput, error) {
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/readings", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ccleft %s: HTTP %d", url, resp.StatusCode)
	}
	var out CcleftOutput
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode ccleft readings: %w", err)
	}
	return &out, nil
}

// CcleftProvider maps a pool provider name onto ccleft's provider.
func CcleftProvider(pool string) ccleft.Provider {
	switch pool {
	case "anthropic":
		return ccleft.Claude
	case "openai":
		return ccleft.Codex
	case "google":
		return ccleft.Agy
	case "github":
		return ccleft.Copilot
	case "meta":
		return ccleft.Muse
	}
	return ccleft.Provider(pool) // kiro, deepseek, or already a ccleft name
}

// SelectCcleftReading picks the reading for provider (and account, when
// pinned). Without a pin exactly one account must exist: two accounts behind
// one pool is a configuration question, never something to guess.
func SelectCcleftReading(out *CcleftOutput, p ccleft.Provider, account string) (ccleft.Reading, error) {
	var found []ccleft.Reading
	for _, r := range out.Readings {
		if r.Provider != p {
			continue
		}
		if account != "" && r.Account != account {
			continue
		}
		found = append(found, r)
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) == 0 && account != "":
		return ccleft.Reading{}, fmt.Errorf("ccleft has no %s reading for account %s", p, account)
	case len(found) == 0:
		return ccleft.Reading{}, fmt.Errorf("ccleft has no %s reading", p)
	}
	ids := make([]string, 0, len(found))
	for _, r := range found {
		ids = append(ids, r.Account)
	}
	return ccleft.Reading{}, fmt.Errorf("ccleft serves %d %s accounts (%s); pin spec.ccleft.account", len(found), p, strings.Join(ids, ","))
}

// CcleftUsable reports whether a reading may feed a pool at now: it must
// carry a measured verdict (a stale last-good counts — it IS a measurement)
// no older than maxAge.
func CcleftUsable(r ccleft.Reading, now time.Time, maxAge time.Duration) error {
	if !r.State.HasQuota() {
		msg := strings.TrimSpace(string(r.State) + " " + r.Cause)
		return fmt.Errorf("ccleft %s reading has no quota verdict (%s)", r.Provider, msg)
	}
	if r.FetchedAt.IsZero() || now.Sub(r.FetchedAt) > maxAge {
		return fmt.Errorf("ccleft %s reading measured %s, older than %s", r.Provider, r.FetchedAt.Format(time.RFC3339), maxAge)
	}
	return nil
}

// kindFor bands a pool window's duration onto a ccleft window kind.
func kindFor(d time.Duration) ccleft.Kind {
	switch {
	case d <= 6*time.Hour:
		return ccleft.KindFiveHour
	case d <= 48*time.Hour:
		return ccleft.KindDaily
	case d <= 14*24*time.Hour:
		return ccleft.KindWeekly
	}
	return ccleft.KindMonthly
}

// PickCcleftWindow finds the ccleft window for one pool window: by id when
// given, else the binding window of the matching kind (and scope), the one
// with the least remaining when several match.
func PickCcleftWindow(r ccleft.Reading, id, scope string, d time.Duration) (ccleft.Window, bool) {
	if id != "" {
		for _, w := range r.Windows {
			if w.ID == id {
				return w, true
			}
		}
		return ccleft.Window{}, false
	}
	kind := kindFor(d)
	var best ccleft.Window
	found := false
	for _, w := range r.Windows {
		if !w.Binding || w.Kind != kind || w.Scope != scope {
			continue
		}
		if !found || remainingPct(w) < remainingPct(best) {
			best, found = w, true
		}
	}
	return best, found
}

func remainingPct(w ccleft.Window) float64 {
	if w.RemainingPct != nil {
		return *w.RemainingPct
	}
	if w.Depleted() {
		return 0
	}
	return 100
}

// CcleftReading converts a ccleft window into the pool's Reading: percent
// used, the provider's reset time, and the measurement time. A window with
// no percentage (a prepaid balance) reads 100 when depleted and 0 otherwise —
// the same two values the bash probe publishes for such pools.
func CcleftReading(w ccleft.Window, fetchedAt time.Time) Reading {
	rd := Reading{Percent: -1, At: fetchedAt}
	switch {
	case w.UsedPct != nil:
		rd.Percent = *w.UsedPct
	case w.RemainingPct != nil:
		rd.Percent = 100 - *w.RemainingPct
	case w.Remaining != nil:
		if w.Depleted() {
			rd.Percent = 100
		} else {
			rd.Percent = 0
		}
	}
	if w.ResetsAt != nil {
		rd.ResetsAt = w.ResetsAt.UTC()
	}
	return rd
}
