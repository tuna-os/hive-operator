package sharedauth

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSpokeRepairDoesNotFollowSymlink: the per-spoke exec runs as root. A
// symlink an agent plants at .claude.json or at the agy settings file must
// not make root create, truncate or rewrite the link's target.
func TestSpokeRepairDoesNotFollowSymlink(t *testing.T) {
	for _, bin := range []string{"jq", "stat", "mktemp"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	base := t.TempDir()
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "chgrp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Each victim is valid JSON that every repair would otherwise act on.
	victim := filepath.Join(base, "victim.json")
	const victimBody = `{"theme":null,"statusLine":{"command":"/status"},"secret":1}`
	if err := os.WriteFile(victim, []byte(victimBody), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "created-by-root")

	root := filepath.Join(base, "g")
	scenario{name: "symlink", token: "ok-token", brokenSL: true,
		theme: map[string]string{"hive-reef": `{"theme":null}`}}.build(t, root)
	link := func(target, p string) {
		_ = os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	// hive: .claude.json -> existing file (rewrite path).
	link(victim, filepath.Join(root, "hive", "home", ".claude.json"))
	// hive-hanthor: dangling .claude.json (create path).
	link(missing, filepath.Join(root, "hive-hanthor", "home", ".claude.json"))
	// shared agy settings -> existing file.
	link(victim, filepath.Join(root, "shared", ".gemini", "antigravity-cli", "settings.json"))
	reef := filepath.Join(root, "hive-reef", "home", ".claude.json")
	if err := os.Chmod(reef, 0o640); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), &local{root: root, nopod: map[string]bool{}, path: bin + ":" + os.Getenv("PATH")}, cfg(true))
	t.Logf("%v", res.Lines)

	if b, _ := os.ReadFile(victim); string(b) != victimBody {
		t.Errorf("root repair wrote through a symlink: victim is now %q", b)
	}
	if _, err := os.Lstat(missing); err == nil {
		t.Error("root repair created the target of a dangling symlink")
	}
	if res.Spokes["hive"].Theme != "repair-failed" || res.Spokes["hive-hanthor"].Theme != "repair-failed" {
		t.Errorf("a symlinked .claude.json must be reported, got hive=%q hive-hanthor=%q",
			res.Spokes["hive"].Theme, res.Spokes["hive-hanthor"].Theme)
	}
	// The real file next to them is still repaired, keeping its mode.
	b, err := os.ReadFile(reef)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{\n  \"theme\": \"dark\",\n  \"hasCompletedOnboarding\": true\n}\n" {
		t.Errorf("regular .claude.json not repaired: %q", b)
	}
	if fi, err := os.Lstat(reef); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o640 {
		t.Errorf("repaired .claude.json lost its mode: %v %v", fi.Mode(), err)
	}
}
