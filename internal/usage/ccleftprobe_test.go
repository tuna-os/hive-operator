package usage

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tuna-os/ccleft"
)

// testdata/ccleft.readings-20261001.json is GET /readings from the fleet's
// ccleft at 2026-10-01T14:28:49Z (account fingerprints redacted). The bash
// rotate job published hive/hive-provider-usage from the same ccleft four
// minutes earlier (14:25:00Z); every value below is copied from that
// ConfigMap, except kiro's credit count, which moved 458.85 → 459.36.
func readings20261001(t *testing.T) *CcleftOutput {
	t.Helper()
	b, err := os.ReadFile("testdata/ccleft.readings-20261001.json")
	if err != nil {
		t.Fatal(err)
	}
	var out CcleftOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestCcleftProbeMatchesPublishedConfigMap(t *testing.T) {
	out := readings20261001(t)
	now := time.Date(2026, 10, 1, 14, 29, 0, 0, time.UTC)
	want := map[string]string{
		"anthropic": "100% used no-credential (ccleft auth_required cause=no_credentials: needs an interactive login)",
		"google":    "95% used resets=2026-10-07T03:17:08Z",
		"kiro":      "4% used credits=459.36/10000 resets=2026-11-01T00:00:00Z",
		"meta":      "unknown no-usage-api (ccleft unsupported cause=api_key_login)",
		"openai":    "100% used weekly=100% resets=2026-10-03T22:41:44Z",
	}
	for p, w := range want {
		got := CcleftProbe(out, p, now, 0, 0).Published()
		if got != w {
			t.Errorf("%s:\n got  %q\n want %q", p, got, w)
		}
		if back := PublishedToProbe(w); back.Published() != w {
			t.Errorf("published_to_probe round trip %q → %q", w, back.Published())
		}
	}
}

func TestCcleftProbeStaleness(t *testing.T) {
	out := readings20261001(t)
	// An hour and a bit later every fresh reading is past HIVE_CCLEFT_MAX_AGE_S.
	late := time.Date(2026, 10, 1, 15, 30, 0, 0, time.UTC)
	if got := CcleftProbe(out, "google", late, 0, 0); got.Percent != -1 || got.Note != "ccleft-stale age=61m" {
		t.Fatalf("expired fresh reading: %+v", got)
	}
	// A stale (last-good) reading counts only for HIVE_CCLEFT_MAX_STALE_S.
	for i := range out.Readings {
		if out.Readings[i].Provider == ccleft.Agy {
			out.Readings[i].Stale = true
		}
	}
	at := time.Date(2026, 10, 1, 14, 40, 0, 0, time.UTC)
	if got := CcleftProbe(out, "google", at, 0, 0).String(); got != "95 resets=2026-10-07T03:17:08Z (ccleft stale 11m)" {
		t.Fatalf("young stale reading: %q", got)
	}
	at = time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	if got := CcleftProbe(out, "google", at, 0, 0); got.Percent != -1 {
		t.Fatalf("stale reading past 30m must be unmeasured: %+v", got)
	}
	if got := CcleftProbe(out, "github", at, 0, 0).String(); got != "-1 ccleft-no-reading" {
		t.Fatalf("absent provider: %q", got)
	}
}

func TestCcleftProbeLimitedWithoutWindowIs100(t *testing.T) {
	out := &CcleftOutput{Readings: []ccleft.Reading{{Provider: ccleft.Codex, State: "limited",
		Message: "provider reports limit_reached", FetchedAt: time.Unix(1000, 0)}}}
	if got := CcleftProbe(out, "openai", time.Unix(1060, 0), 0, 0).String(); got != "100 ccleft limited: provider reports limit_reached" {
		t.Fatalf("%q", got)
	}
	out.Readings[0].State = "rate_limited"
	out.Readings[0].Cause = "429"
	if got := CcleftProbe(out, "openai", time.Unix(1060, 0), 0, 0).String(); got != "-1 ccleft-rate_limited cause=429" {
		t.Fatalf("%q", got)
	}
}

func TestCcleftKiroSampleAndLimits(t *testing.T) {
	out := readings20261001(t)
	now := time.Date(2026, 10, 1, 14, 29, 0, 0, time.UTC)
	s, ok := CcleftKiroSample(out, now, 0, 0)
	if !ok || s.Used != 459.36 || s.Limit != 10000 || s.Pct != 4.594 || s.Reset != 1793491200 || s.TS != 1790864929 {
		t.Fatalf("kiro sample %+v", s)
	}
	if l := CcleftAnthropicLimits(out, now, 0, 0); l != nil {
		t.Fatalf("auth_required claude has no limits: %+v", l)
	}
	l, ok := SampleFromLine("kiro", PublishedToProbe("4% used credits=458.85/10000 resets=2026-11-01T00:00:00Z"), 5)
	// awk printf "%.3f" of 4.5885 (binary 4.58849…) prints 4.588.
	if !ok || l.Pct != 4.588 || l.Reset != 1793491200 {
		t.Fatalf("kiro line sample %+v", l)
	}
}
