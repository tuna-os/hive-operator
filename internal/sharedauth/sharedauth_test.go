package sharedauth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var fleet = []string{"hive", "hive-reef", "hive-hanthor"}

func cfg(fix bool) Config {
	return Config{
		Namespaces: fleet, Primary: "hive", Home: "/data/home",
		Dirs:     []string{".claude", ".gemini", ".codex"},
		FixTheme: fix, FixStatusLine: fix, FixPerms: fix,
	}
}

// replay answers the three kinds of exec from canned spoke state, as the
// live cluster did at 2026-10-01T18:00Z (job hive-shared-auth-29847960).
type replay struct {
	stamp string
	token map[string]string
}

func (r *replay) Pod(_ context.Context, ns string) (string, error) { return "hive-" + ns, nil }
func (r *replay) Exec(_ context.Context, ns, _ string, argv []string) (string, string, error) {
	if len(argv) > 3 { // the per-spoke credential exec
		return "token " + r.token[ns] + "\n", "", nil
	}
	s := argv[2]
	if strings.Contains(s, "$(cat ") {
		var b strings.Builder
		for _, d := range []string{".claude", ".gemini", ".codex"} {
			fmt.Fprintf(&b, "%s %s\n", d, r.stamp)
		}
		return b.String(), "", nil
	}
	return "", "", nil
}

// TestGoldenLiveJob reproduces the live job log byte for byte.
func TestGoldenLiveJob(t *testing.T) {
	want, err := os.ReadFile("testdata/hive-shared-auth-20261001T1800.log")
	if err != nil {
		t.Fatal(err)
	}
	r := &replay{stamp: "probe-1-2", token: map[string]string{"hive": "EMPTY", "hive-reef": "EMPTY", "hive-hanthor": "EMPTY"}}
	c := cfg(true)
	c.Stamp = func() string { return r.stamp }
	res := Run(context.Background(), r, c)
	got := strings.Join(res.Lines, "\n") + "\n"
	if got != string(want) {
		t.Fatalf("report differs from the live job log\n--- got\n%s--- want\n%s", got, want)
	}
	if res.OK {
		t.Error("an empty token is exit 1 in bash")
	}
	if !res.Shared["hive-reef"][".claude"] || res.Spokes["hive"].Token != "EMPTY" {
		t.Errorf("structured result wrong: %+v %+v", res.Shared, res.Spokes["hive"])
	}
}

// local runs pod execs on the local filesystem: every argument's
// "/data/home" becomes <root>/<ns>/home, exactly as the bash harness's
// kubectl stub does, so both sides run the same commands on the same tree.
type local struct {
	root  string
	nopod map[string]bool
	path  string
}

func (l *local) Pod(_ context.Context, ns string) (string, error) {
	if l.nopod[ns] {
		return "", fmt.Errorf("no running hive pod in %s", ns)
	}
	return "hive-" + ns, nil
}

func (l *local) Exec(ctx context.Context, ns, _ string, argv []string) (string, string, error) {
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = strings.ReplaceAll(a, "/data/home", filepath.Join(l.root, ns, "home"))
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+l.path)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

const kubectlStub = `#!/usr/bin/env bash
# kubectl exec -n NS POD -- cmd... ; runs locally with /data/home → $FAKE_ROOT/NS/home
[ "$1" = exec ] || exit 1
ns=$3; shift 5
args=(); for a in "$@"; do args+=("${a//\/data\/home/$FAKE_ROOT/$ns/home}"); done
exec "${args[@]}"
`

const libStub = `hive_kube_env() { :; }
hive_pod() { case " $FAKE_NOPOD " in *" $1 "*) ;; *) echo "hive-$1";; esac; }
`

// scenario builds a fleet tree: a shared store, symlinked from every spoke
// unless the spoke has a private copy.
type scenario struct {
	name     string
	private  map[string][]string // ns → dirs that are NOT the shared store
	nopod    []string
	token    string            // "", "ok-token" or "missing"
	theme    map[string]string // ns → .claude.json content ("" = missing file)
	brokenSL bool
	roCodex  bool // the primary's .codex is not writable
}

func (s scenario) build(t *testing.T, root string) {
	mk := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wr := func(p, c string) {
		mk(filepath.Dir(p))
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(root, "shared")
	for _, d := range []string{".claude", ".gemini", ".codex"} {
		mk(filepath.Join(shared, d))
	}
	switch s.token {
	case "":
		wr(filepath.Join(shared, ".claude/.credentials.json"), `{"claudeAiOauth":{"accessToken":""}}`)
	case "ok-token":
		wr(filepath.Join(shared, ".claude/.credentials.json"), `{"claudeAiOauth":{"accessToken":"sk-x"}}`)
	}
	if s.brokenSL {
		wr(filepath.Join(shared, ".gemini/antigravity-cli/settings.json"), `{"statusLine":{"type":"command","command":"/status"},"x":1}`)
	}
	for _, ns := range fleet {
		home := filepath.Join(root, ns, "home")
		mk(home)
		priv := map[string]bool{}
		for _, d := range s.private[ns] {
			priv[d] = true
		}
		for _, d := range []string{".claude", ".gemini", ".codex"} {
			if priv[d] {
				mk(filepath.Join(home, d))
				continue
			}
			if err := os.Symlink(filepath.Join(shared, d), filepath.Join(home, d)); err != nil {
				t.Fatal(err)
			}
		}
		th, ok := s.theme[ns]
		if !ok {
			th = `{"theme":"dark","hasCompletedOnboarding":true}`
		}
		if th != "" {
			wr(filepath.Join(home, ".claude.json"), th)
		}
	}
	if s.roCodex {
		// a private, read-only .codex in the primary
		p := filepath.Join(root, "hive", "home", ".codex")
		_ = os.Remove(p)
		mk(p)
		if err := os.Chmod(p, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}
}

// TestDifferentialAgainstBash runs the live hive-shared-auth.sh (testdata
// copy of ConfigMap hive/hive-ops-scripts) and the port on identical trees,
// in check and reconcile mode, and requires identical output — and, after a
// reconcile, identical files.
func TestDifferentialAgainstBash(t *testing.T) {
	for _, bin := range []string{"bash", "jq", "timeout", "od"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only dir the unwritable scenario needs")
	}
	script, _ := filepath.Abs("testdata/hive-shared-auth.sh")

	scenarios := []scenario{
		{name: "healthy", token: "ok-token"},
		{name: "live-2026-10-01"},
		{name: "private-copy-and-no-pod", token: "ok-token",
			private: map[string][]string{"hive-reef": {".gemini"}}, nopod: []string{"hive-hanthor"}},
		{name: "theme-null-and-missing", token: "ok-token",
			theme: map[string]string{"hive-reef": `{"theme":null,"hasCompletedOnboarding":true}`, "hive-hanthor": ""}},
		{name: "broken-statusline", token: "ok-token", brokenSL: true},
		{name: "credential-missing", token: "missing"},
		{name: "primary-unwritable", token: "ok-token", roCodex: true},
	}
	for _, sc := range scenarios {
		for _, mode := range []string{"check", "reconcile"} {
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				base := t.TempDir()
				bin := filepath.Join(base, "bin")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				// chgrp: there is no group "node" here; succeed silently.
				for name, body := range map[string]string{"kubectl": kubectlStub, "chgrp": "#!/bin/sh\nexit 0\n"} {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				lib := filepath.Join(base, "lib.sh")
				if err := os.WriteFile(lib, []byte(libStub), 0o644); err != nil {
					t.Fatal(err)
				}
				path := bin + ":" + os.Getenv("PATH")

				bashRoot, goRoot := filepath.Join(base, "b"), filepath.Join(base, "g")
				sc.build(t, bashRoot)
				sc.build(t, goRoot)

				cmd := exec.Command("bash", script, mode)
				cmd.Env = append(os.Environ(), "PATH="+path, "HIVE_LIB="+lib, "FAKE_ROOT="+bashRoot,
					"FAKE_NOPOD="+strings.Join(sc.nopod, " "))
				bout, err := cmd.Output()
				bashOK := err == nil

				nop := map[string]bool{}
				for _, n := range sc.nopod {
					nop[n] = true
				}
				res := Run(context.Background(), &local{root: goRoot, nopod: nop, path: path}, cfg(mode == "reconcile"))
				got := strings.Join(res.Lines, "\n") + "\n"
				t.Logf("bash:\n%s", bout)
				if got != string(bout) {
					t.Fatalf("output differs\n--- port\n%s--- bash\n%s", got, bout)
				}
				if res.OK != bashOK {
					t.Fatalf("exit status: port ok=%v, bash ok=%v", res.OK, bashOK)
				}
				// Same files afterwards (repairs and marker cleanup).
				if b, g := snapshot(t, bashRoot), snapshot(t, goRoot); b != g {
					t.Fatalf("trees differ after the pass\n--- port\n%s--- bash\n%s", g, b)
				}
			})
		}
	}
}

func snapshot(t *testing.T, root string) string {
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if fi.Mode().IsRegular() {
			c, _ := os.ReadFile(p)
			fmt.Fprintf(&b, "%s %s\n", rel, c)
		}
		return nil
	})
	return b.String()
}
