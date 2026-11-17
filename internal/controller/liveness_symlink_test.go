package controller

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// runRepair runs a root repair script on a tree under root instead of
// /data/home, owned by the current user instead of dev:node.
func runRepair(t *testing.T, script, root string) {
	t.Helper()
	for _, bin := range []string{"sh", "perl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skip(err)
	}
	s := strings.ReplaceAll(script, "/data/home", root)
	s = strings.ReplaceAll(s, " dev node ", " "+u.Username+" "+g.Name+" ")
	s = strings.ReplaceAll(s, "dev:node", u.Username+":"+g.Name)
	if out, err := exec.Command("sh", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// symlinkTree is the shared home an agent has planted links in: a link
// matching each glob the repairs use, a link inside each recursive sweep,
// and a codex home that is a link to a directory outside the tree.
func symlinkTree(t *testing.T) (root, victim, victimDir string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "home")
	victim = filepath.Join(base, "victim")
	victimDir = filepath.Join(base, "victimdir")
	wr := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ln := func(target, p string) {
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	wr(victim)
	wr(filepath.Join(victimDir, "inner.json"))
	g := filepath.Join(root, ".gemini", "antigravity-cli")
	wr(filepath.Join(g, "settings.json"))
	wr(filepath.Join(g, "cache", "a.json"))
	ln(victim, filepath.Join(g, "evil.json"))
	ln(victim, filepath.Join(g, "cache", "evil.json"))
	muse := filepath.Join(root, ".config", "muse")
	wr(filepath.Join(muse, "trust.json"))
	ln(victim, filepath.Join(muse, "evil.json"))
	wr(filepath.Join(root, ".codex-guide", "sub", "state.sqlite"))
	ln(victim, filepath.Join(root, ".codex-guide", "sub", "evil"))
	ln(victimDir, filepath.Join(root, ".codex-evil"))
	return root, victim, victimDir
}

// TestRootRepairsDoNotFollowSymlinks: the hygiene and agy/muse perms-fix
// execs run as root. A link an agent plants in the shared home must not get
// its target chowned dev:node or made mode 660; the real files still must.
func TestRootRepairsDoNotFollowSymlinks(t *testing.T) {
	for name, script := range map[string]string{"hygiene": livenessHygiene, "permsfix": livenessPermsFix} {
		t.Run(name, func(t *testing.T) {
			root, victim, victimDir := symlinkTree(t)
			runRepair(t, script, root)
			for _, p := range []string{victim, filepath.Join(victimDir, "inner.json")} {
				if m := mode(t, p); m != 0o600 {
					t.Errorf("root repair followed a symlink: %s is now %o", p, m)
				}
			}
			g := filepath.Join(root, ".gemini", "antigravity-cli")
			want := []string{
				filepath.Join(g, "settings.json"),
				filepath.Join(g, "cache", "a.json"),
				filepath.Join(root, ".config", "muse", "trust.json"),
			}
			if name == "hygiene" {
				want = append(want, filepath.Join(root, ".codex-guide", "sub", "state.sqlite"))
				for _, d := range []string{g, filepath.Join(g, "cache"), filepath.Join(root, ".codex-guide", "sub")} {
					fi, err := os.Lstat(d)
					if err != nil {
						t.Fatal(err)
					}
					if fi.Mode().Perm() != 0o770 || fi.Mode()&os.ModeSetgid == 0 {
						t.Errorf("%s not repaired: mode %v, want 2770", d, fi.Mode())
					}
				}
			}
			for _, p := range want {
				if m := mode(t, p); m != 0o660 {
					t.Errorf("%s not repaired: mode %o, want 660", p, m)
				}
			}
		})
	}
}

// TestRootRepairRefusesSymlinkedDirectory: an agent can rename the cache
// directory and put a link to any directory in its place. The repair must
// not walk through it.
func TestRootRepairRefusesSymlinkedDirectory(t *testing.T) {
	for name, script := range map[string]string{"hygiene": livenessHygiene, "permsfix": livenessPermsFix} {
		t.Run(name, func(t *testing.T) {
			root, _, victimDir := symlinkTree(t)
			cache := filepath.Join(root, ".gemini", "antigravity-cli", "cache")
			if err := os.RemoveAll(cache); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victimDir, cache); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(victimDir, 0o700); err != nil {
				t.Fatal(err)
			}
			runRepair(t, script, root)
			if m := mode(t, filepath.Join(victimDir, "inner.json")); m != 0o600 {
				t.Errorf("repair walked through a symlinked directory: inner.json is now %o", m)
			}
			if m := mode(t, victimDir); m != 0o700 {
				t.Errorf("repair chmodded a symlinked directory's target: now %o", m)
			}
		})
	}
}
