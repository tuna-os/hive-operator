package usage

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseSinceNormalisesToUTC(t *testing.T) {
	q := url.Values{"since": {"2026-11-19T10:30:00+02:00", "2026-11-19T00:00:00Z"}}
	got, err := ParseSince(q)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{
		time.Date(2026, 11, 19, 8, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d instants, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) || got[i].Location() != time.UTC {
			t.Errorf("since[%d] = %v (%v), want %v UTC", i, got[i], got[i].Location(), want[i])
		}
	}
}

func TestParseSinceAbsentIsEmpty(t *testing.T) {
	got, err := ParseSince(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want none so the sidecar serves its default windows", got)
	}
}

func TestParseSinceRejectsMalformed(t *testing.T) {
	// One bad value fails the whole request: the sidecar answers 400 rather
	// than serving a partial set of windows.
	q := url.Values{"since": {"2026-11-19T00:00:00Z", "yesterday"}}
	got, err := ParseSince(q)
	if err == nil {
		t.Fatalf("got %v, want an error", got)
	}
	if got != nil {
		t.Errorf("got %v alongside the error, want nil", got)
	}
	if !strings.Contains(err.Error(), `since="yesterday"`) {
		t.Errorf("error %q does not name the offending value", err)
	}
}

func TestQueryRoundTripsThroughParseSince(t *testing.T) {
	a := time.Date(2026, 11, 19, 10, 30, 0, 0, time.FixedZone("x", 2*3600))
	b := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

	raw := Query(a, b)
	if !strings.HasPrefix(raw, "/v1/usage?") {
		t.Fatalf("Query = %q, want /v1/usage? prefix", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSince(u.Query())
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{a.UTC(), b}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %v, want %v", got, want)
	}
}

func TestQueryWithoutWindows(t *testing.T) {
	if got := Query(); got != "/v1/usage?" {
		t.Fatalf("Query() = %q", got)
	}
}

func TestReportWindowMatchesByInstant(t *testing.T) {
	since := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	r := &Report{Windows: []WindowUsage{
		{Label: "1h", Since: since.Add(4 * time.Hour)},
		{Label: "5h", Since: since},
	}}

	// Same instant in another zone still matches: Equal, not ==.
	w := r.Window(since.In(time.FixedZone("x", 3*3600)))
	if w == nil || w.Label != "5h" {
		t.Fatalf("Window = %+v, want the 5h block", w)
	}
	if r.Window(since.Add(time.Second)) != nil {
		t.Fatal("Window matched an instant that was never requested")
	}
	if (&Report{}).Window(since) != nil {
		t.Fatal("empty report returned a window")
	}
}

func TestRowsSortedByProviderSourceAgentModel(t *testing.T) {
	tok := Tokens{Input: 1}
	m := map[Key]Tokens{
		{Source: "s2", Provider: "p1", Agent: "a", Model: "m"}: tok,
		{Source: "s1", Provider: "p2", Agent: "a", Model: "m"}: tok,
		{Source: "s1", Provider: "p1", Agent: "b", Model: "m"}: tok,
		{Source: "s1", Provider: "p1", Agent: "a", Model: "n"}: tok,
		{Source: "s1", Provider: "p1", Agent: "a", Model: "m"}: tok,
	}
	want := []Key{
		{Source: "s1", Provider: "p1", Agent: "a", Model: "m"},
		{Source: "s1", Provider: "p1", Agent: "a", Model: "n"},
		{Source: "s1", Provider: "p1", Agent: "b", Model: "m"},
		{Source: "s2", Provider: "p1", Agent: "a", Model: "m"},
		{Source: "s1", Provider: "p2", Agent: "a", Model: "m"},
	}
	rows := Rows(m)
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, k := range want {
		if rows[i].Key != k || rows[i].Tokens != tok {
			t.Errorf("rows[%d] = %+v, want key %+v", i, rows[i], k)
		}
	}
}
