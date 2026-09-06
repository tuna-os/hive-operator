package hiveclient

import "testing"

// TestShellQuote guards the quoting used to pass tokens and session cookies
// into `sh -c`. A break here leaks or corrupts a credential rather than failing
// loudly, so it is worth a test even though it is three lines.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"has space", "'has space'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
