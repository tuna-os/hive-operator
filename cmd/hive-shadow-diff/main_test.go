package main

import (
	"strings"
	"testing"
)

// Verbatim apply-mode log shape from hive-rotate (school), including lines
// that only one side can print.
const bashLog = `usage: reusing hive/hive-provider-usage published 312s ago
reviewer       operator  held paused by HIVE_ROTATE_HOLD (declared in git)
supervisor     google    pinned by HIVE_ROTATE_PIN — placement left alone
scanner        google    gemini-3.6-flash-low  ->  anthropic claude-sonnet-5
    paused: paused
scanner        resumed on anthropic (was stranded on google)
sec-check      anthropic claude-sonnet-5 ok (pace-demoted from claude-fable-5-1)

contributors:
agy-contributor          google    ok (replicas=1)

fleet already on the best available rung
`

func TestPlanLines(t *testing.T) {
	got := planLines(strings.NewReader(bashLog))
	if len(got) != 4 {
		t.Fatalf("agents %v", got)
	}
	if len(got["scanner"]) != 1 || !strings.Contains(got["scanner"][0], "->") {
		t.Fatalf("scanner %q", got["scanner"])
	}
	if _, ok := got["agy-contributor"]; ok {
		t.Fatal("contributors section must be ignored")
	}
}
