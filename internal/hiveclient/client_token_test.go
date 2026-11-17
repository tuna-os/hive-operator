package hiveclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// The commands run inside the hive container, where every user, agents
// included, can read /proc/<pid>/cmdline. X-Hive-Internal is
// owner-equivalent there, so the token must reach curl on stdin and never
// appear in the command.
func TestTokenNeverInArgv(t *testing.T) {
	const tok = "tok-SENTINEL"
	var argvSeen [][]string
	var stdinSeen []string
	c := &Client{execFn: func(_ context.Context, _, _ string, argv []string, stdin io.Reader) (string, string, error) {
		argvSeen = append(argvSeen, argv)
		var in string
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			in = string(b)
		}
		stdinSeen = append(stdinSeen, in)
		return `{}`, "", nil
	}}
	ctx := context.Background()
	var v map[string]any
	_ = c.GetJSON(ctx, "ns", "pod", tok, "/api/status", &v)
	_, _ = c.Post(ctx, "ns", "pod", tok, "/api/kick/guide")
	_, _ = c.Place(ctx, "ns", "pod", tok, "guide", Placement{Model: "m"})
	if len(argvSeen) != 3 {
		t.Fatalf("expected 3 execs, got %d", len(argvSeen))
	}
	for i, argv := range argvSeen {
		if strings.Contains(strings.Join(argv, " "), tok) {
			t.Errorf("token is in the command: %q", argv)
		}
		if !strings.Contains(stdinSeen[i], tok) {
			t.Errorf("exec %d: token was not sent on stdin", i)
		}
	}
}

// curl really sends the header it reads from `-K -`, quoting included.
func TestCurlTokenConfigSendsHeader(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	for _, tok := range []string{"plain-token", `has"quote\and-backslash`} {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("X-Hive-Internal")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		c := &Client{execFn: func(ctx context.Context, _, _ string, argv []string, stdin io.Reader) (string, string, error) {
			args := make([]string, len(argv))
			for i, a := range argv {
				args[i] = strings.ReplaceAll(a, APIAddr, srv.URL)
			}
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			cmd.Stdin = stdin
			var out, errOut strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &errOut
			err := cmd.Run()
			return out.String(), errOut.String(), err
		}}
		var v map[string]any
		if err := c.GetJSON(context.Background(), "ns", "pod", tok, "/api/status", &v); err != nil {
			t.Fatalf("GetJSON: %v", err)
		}
		if got != tok {
			t.Errorf("server saw X-Hive-Internal %q, want %q", got, tok)
		}
		srv.Close()
	}
}
