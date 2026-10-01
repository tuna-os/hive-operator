package hiveclient

import (
	"encoding/json"
	"testing"
)

// TestShellQuote guards the quoting used to pass the dashboard token and request bodies
// into `sh -c`. A break here leaks or corrupts a credential rather than failing
// loudly, so it is worth a test even though it is three lines.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"has space", "'has space'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	} {
		if got := ShellQuote(tc.in); got != tc.want {
			t.Errorf("ShellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPlacementBody pins the wire shape of the atomic placement call. A nil
// effort must be absent (leave unchanged); an empty one must be sent as ""
// (reset to the backend default) — the handler tells them apart by presence.
func TestPlacementBody(t *testing.T) {
	empty, high := "", "high"
	for _, tc := range []struct {
		p    Placement
		want string
	}{
		{Placement{Backend: "pi", Model: "kiro-api-key/claude-sonnet-5:medium"}, `{"backend":"pi","model":"kiro-api-key/claude-sonnet-5:medium"}`},
		{Placement{Backend: "agy", Model: "gemini-3.8-flash", ReasoningEffort: &high}, `{"backend":"agy","model":"gemini-3.8-flash","reasoning_effort":"high"}`},
		{Placement{ReasoningEffort: &empty}, `{"reasoning_effort":""}`},
	} {
		b, err := json.Marshal(tc.p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("Marshal(%+v) = %s, want %s", tc.p, b, tc.want)
		}
	}
}
