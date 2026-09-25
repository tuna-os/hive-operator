package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/ccleft"
)

// testdata/ccleft.readings.json is a real GET /readings from the tuna-os
// Hive's ccleft (2026-09-25 11:47Z) with account fingerprints replaced:
// claude 429 without a last-good yet, codex weekly exhausted, agy with
// gemini/3p pools, kiro credits, deepseek negative balance, gemini/muse
// unsupported.
func liveReadings(t *testing.T) *CcleftOutput {
	t.Helper()
	b, err := os.ReadFile("testdata/ccleft.readings.json")
	if err != nil {
		t.Fatal(err)
	}
	var out CcleftOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

var liveNow = time.Date(2026, 9, 25, 11, 50, 0, 0, time.UTC)

func TestHTTPReadingsFetcher(t *testing.T) {
	b, _ := os.ReadFile("testdata/ccleft.readings.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readings" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	out, err := (&HTTPReadingsFetcher{}).Readings(context.Background(), srv.URL+"/")
	if err != nil || len(out.Readings) != 7 || out.GeneratedAt.IsZero() {
		t.Fatalf("got %v %+v", err, out)
	}
	if _, err := (&HTTPReadingsFetcher{}).Readings(context.Background(), srv.URL+"/nope"); err == nil {
		t.Fatal("HTTP 404 must be an error")
	}
}

func TestSelectCcleftReading(t *testing.T) {
	out := liveReadings(t)
	r, err := SelectCcleftReading(out, CcleftProvider("openai"), "")
	if err != nil || r.Provider != ccleft.Codex || len(r.Homes) != 12 {
		t.Fatalf("codex: %v %+v", err, r)
	}
	if _, err := SelectCcleftReading(out, ccleft.Codex, "000000000000ffff"); err == nil {
		t.Fatal("unknown pinned account must be an error")
	}
	if _, err := SelectCcleftReading(out, ccleft.Copilot, ""); err == nil {
		t.Fatal("missing provider must be an error")
	}
	two := &CcleftOutput{Readings: append(append([]ccleft.Reading(nil), out.Readings...), ccleft.Reading{Provider: ccleft.Codex, Account: "000000000000beef"})}
	if _, err := SelectCcleftReading(two, ccleft.Codex, ""); err == nil || !strings.Contains(err.Error(), "pin spec.ccleft.account") {
		t.Fatalf("two accounts must require a pin: %v", err)
	}
	if r, err := SelectCcleftReading(two, ccleft.Codex, "000000000000beef"); err != nil || r.Account != "000000000000beef" {
		t.Fatalf("pin: %v %+v", err, r)
	}
}

func TestCcleftUsable(t *testing.T) {
	out := liveReadings(t)
	claude, _ := SelectCcleftReading(out, ccleft.Claude, "")
	if err := CcleftUsable(claude, liveNow, 30*time.Minute); err == nil || !strings.Contains(err.Error(), "rate_limited http_429") {
		t.Fatalf("a 429 with no last-good has no verdict: %v", err)
	}
	codex, _ := SelectCcleftReading(out, ccleft.Codex, "")
	if err := CcleftUsable(codex, liveNow, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := CcleftUsable(codex, liveNow.Add(time.Hour), 30*time.Minute); err == nil {
		t.Fatal("an old measurement must be refused")
	}
	// A stale last-good (served during a 429) is still a measurement.
	codex.Stale, codex.Cause = true, "http_429"
	if err := CcleftUsable(codex, liveNow, 30*time.Minute); err != nil {
		t.Fatalf("stale last-good within maxAge must be usable: %v", err)
	}
}

func TestPickCcleftWindowAndReading(t *testing.T) {
	out := liveReadings(t)
	agy, _ := SelectCcleftReading(out, CcleftProvider("google"), "")
	// Scope-restricted automatic match: the Gemini pool, not agy's 3p pool.
	w, ok := PickCcleftWindow(agy, "", "gemini", 5*time.Hour)
	if !ok || w.ID != "gemini-5h" {
		t.Fatalf("5h/gemini → %+v", w)
	}
	w, ok = PickCcleftWindow(agy, "", "gemini", 168*time.Hour)
	if !ok || w.ID != "gemini-weekly" {
		t.Fatalf("weekly/gemini → %+v", w)
	}
	rd := CcleftReading(w, agy.FetchedAt)
	// 78.3% used at 11:47Z; the bash probe read "74% used
	// resets=2026-09-30T03:17:08Z" at 11:27Z (the pool is burning ~2%/h).
	if rd.Percent < 78 || rd.Percent >= 79 || rd.ResetsAt.Format(time.RFC3339) != "2026-09-30T03:17:08Z" || !rd.At.Equal(agy.FetchedAt) {
		t.Fatalf("gemini-weekly reading %+v", rd)
	}
	if _, ok := PickCcleftWindow(agy, "", "gemini", 24*time.Hour); ok {
		t.Fatal("agy has no daily window")
	}
	if _, ok := PickCcleftWindow(agy, "", "", 5*time.Hour); ok {
		t.Fatal("unscoped match must not grab a scoped pool")
	}

	codex, _ := SelectCcleftReading(out, ccleft.Codex, "")
	w, ok = PickCcleftWindow(codex, "", "", 168*time.Hour)
	if rd := CcleftReading(w, codex.FetchedAt); !ok || rd.Percent != 100 || rd.ResetsAt.Format(time.RFC3339) != "2026-09-26T08:15:18Z" {
		t.Fatalf("codex weekly %+v %+v", w, rd)
	}
	if _, ok := PickCcleftWindow(codex, "", "", 5*time.Hour); ok {
		t.Fatal("codex has no 5h window today; must not match")
	}

	kiro, _ := SelectCcleftReading(out, ccleft.Kiro, "")
	w, ok = PickCcleftWindow(kiro, "plan", "", 0)
	if rd := CcleftReading(w, kiro.FetchedAt); !ok || *w.Limit != 10000 || rd.Percent < 37 || rd.Percent > 39 {
		t.Fatalf("kiro plan %+v %+v", w, rd)
	}

	// A prepaid balance has no percentage: depleted reads 100, like bash.
	ds, _ := SelectCcleftReading(out, ccleft.DeepSeek, "")
	w, ok = PickCcleftWindow(ds, "balance:usd", "", 0)
	if rd := CcleftReading(w, ds.FetchedAt); !ok || rd.Percent != 100 {
		t.Fatalf("deepseek %+v %+v", w, rd)
	}
	pos := 5.0
	if rd := CcleftReading(ccleft.Window{ID: "balance:usd", Kind: ccleft.KindBalance, Remaining: &pos}, liveNow); rd.Percent != 0 {
		t.Fatalf("funded balance %+v", rd)
	}
	if rd := CcleftReading(ccleft.Window{ID: "x"}, liveNow); rd.Known() {
		t.Fatalf("an empty window must stay unknown: %+v", rd)
	}
}
