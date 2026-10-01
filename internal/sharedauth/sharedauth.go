// Package sharedauth is hive-shared-auth.sh, ported rule for rule: verify by
// write-through that the credential dirs are ONE store across every spoke,
// check the Claude token, and repair what is repairable (the .claude.json
// theme, a broken agy statusLine, group permissions on the shared dirs).
//
// The pass prints exactly what the bash prints, so the controller's
// status.report can be diffed against the CronJob's log line for line; the
// golden test in this package does that against a real job log.
//
// Bash: ConfigMap hive/hive-ops-scripts, hive-shared-auth.sh (live,
// 2026-10-01, 8.5k).
package sharedauth

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/tuna-os/hive-operator/internal/hiveclient"
)

// Execer is the slice of hiveclient.Client a pass needs.
type Execer interface {
	Pod(ctx context.Context, ns string) (string, error)
	Exec(ctx context.Context, ns, pod string, argv []string) (string, string, error)
}

// Config is one pass. The zero value of every Fix* is "check only".
type Config struct {
	Namespaces []string // HIVE_SHARED_AUTH_NAMESPACES, in order
	Primary    string   // HIVE_SHARED_AUTH_PRIMARY
	Home       string   // HIVE_AGENT_HOME — never $HOME (root's, under exec)
	Dirs       []string // HIVE_SHARED_AUTH_DIRS

	// What a `reconcile` would repair. All false = `check`.
	FixTheme      bool
	FixStatusLine bool
	FixPerms      bool

	// ExecTimeout bounds each exec (HIVE_EXEC_TIMEOUT, 120 s).
	ExecTimeout time.Duration
	// Stamp returns the per-pass probe stamp; nil = random.
	Stamp func() string
}

// Result is one pass.
type Result struct {
	// Lines is the pass in the bash output format.
	Lines []string
	// OK is the bash exit status 0.
	OK bool
	// PrimaryErr is set when the primary has no pod (bash exits 1 before
	// printing anything else).
	PrimaryErr error
	// Shared[ns][dir]: true shared, false NOT shared. Absent = not judged
	// (no pod, exec failed, or the primary itself).
	Shared map[string]map[string]bool
	// PrimaryUnwritable lists dirs the marker could not be written to.
	PrimaryUnwritable []string
	// Spokes holds the per-namespace credential/repair outcome; absent when
	// the namespace has no hive pod.
	Spokes map[string]*Spoke
}

// Spoke is one namespace's credential pass.
type Spoke struct {
	Token      string // ok | EMPTY | unreadable
	Theme      string // "" (set) | unset | repaired | repair-failed
	StatusLine string // "" (fine) | broken | repaired | repair-failed
	Perms      bool   // the group-perm repair ran
}

// ProbeMarkerPrefix is the per-pass marker name prefix. The name is unique
// per pass: the operator and the bash once both wrote a fixed
// `.shared-auth-probe`, and whichever wrote last made the other read a
// foreign stamp — the "NOT SHARED" alarms of 2026-09-24 were that race, not
// a private copy (same device 0:66 and inode in all three pods).
const ProbeMarkerPrefix = ".shared-auth-probe."

// legacyMarker is the fixed name earlier operator builds wrote; removed on
// cleanup in case one was left behind.
const legacyMarker = ".shared-auth-probe"

func randomStamp() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("probe-op-%d", binary.BigEndian.Uint32(b[:]))
}

// Run performs one pass.
func Run(ctx context.Context, ex Execer, c Config) Result {
	res := Result{OK: true, Shared: map[string]map[string]bool{}, Spokes: map[string]*Spoke{}}
	out := func(format string, a ...any) { res.Lines = append(res.Lines, fmt.Sprintf(format, a...)) }
	if c.ExecTimeout == 0 {
		c.ExecTimeout = 120 * time.Second
	}
	// kx: stdout only, errors swallowed — `kubectl exec … 2>/dev/null`.
	kx := func(ns, pod string, argv ...string) string {
		cctx, cancel := context.WithTimeout(ctx, c.ExecTimeout)
		defer cancel()
		o, _, _ := ex.Exec(cctx, ns, pod, argv)
		return o
	}
	sh := func(ns, pod, script string) string { return kx(ns, pod, "sh", "-c", script) }
	podOf := func(ns string) string {
		p, err := ex.Pod(ctx, ns)
		if err != nil {
			return ""
		}
		return p
	}

	ppod := podOf(c.Primary)
	if ppod == "" {
		res.OK = false
		res.PrimaryErr = fmt.Errorf("no hive pod in primary namespace %s", c.Primary)
		return res
	}

	stamp := randomStamp()
	if c.Stamp != nil {
		stamp = c.Stamp()
	}
	marker := ProbeMarkerPrefix + stamp
	dirs := quoteAll(c.Dirs)
	home := hiveclient.ShellQuote(c.Home)

	// ── Write-through check ── one exec writes every marker.
	wfail := sh(c.Primary, ppod, fmt.Sprintf(
		`for d in %s; do printf '%%s' %s > %s/"$d"/%s || echo "$d"; done`,
		dirs, hiveclient.ShellQuote(stamp), home, hiveclient.ShellQuote(marker)))
	if f := strings.Fields(wfail); len(f) > 0 {
		res.PrimaryUnwritable = f
		out("%-10s %-9s PRIMARY NOT WRITABLE — cannot verify", strings.Join(f, " "), c.Primary)
		res.OK = false
	}
	for _, ns := range c.Namespaces {
		if ns == c.Primary {
			continue
		}
		pod := podOf(ns)
		if pod == "" {
			out("%-10s %-14s no hive pod — skipped", "*", ns)
			continue
		}
		// QUIRK (bash): a failed WRITE exec leaves no markers, and the reads
		// below then report NOT SHARED. Reproduced for parity.
		seen := sh(ns, pod, fmt.Sprintf(
			`for d in %s; do printf '%%s %%s\n' "$d" "$(cat %s/"$d"/%s 2>/dev/null)"; done`,
			dirs, home, hiveclient.ShellQuote(marker)))
		got := map[string]string{}
		for _, l := range strings.Split(seen, "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				got[f[0]] = f[1]
			}
		}
		res.Shared[ns] = map[string]bool{}
		for _, d := range c.Dirs {
			switch {
			case got[d] == stamp:
				out("%-10s %-14s SHARED ok", d, ns)
				res.Shared[ns][d] = true
			case seen == "":
				out("%-10s %-14s could not read (exec failed) — not judged", d, ns)
			default:
				out("%-10s %-14s NOT SHARED — this spoke has a private copy; a login here will not propagate", d, ns)
				res.Shared[ns][d] = false
				res.OK = false
			}
		}
	}
	sh(c.Primary, ppod, fmt.Sprintf(`for d in %s; do rm -f %s/"$d"/%s %s/"$d"/%s; done`,
		dirs, home, hiveclient.ShellQuote(marker), home, legacyMarker))

	// ── Credential health + repairs, ONE exec per spoke ──
	act := func(b bool) string {
		if b {
			return "reconcile"
		}
		return "check"
	}
	for _, ns := range c.Namespaces {
		pod := podOf(ns)
		if pod == "" {
			continue
		}
		o := kx(ns, pod, "sh", "-c", spokeScript, "sh", c.Home,
			act(c.FixTheme), act(c.FixStatusLine), act(c.FixPerms), strings.Join(c.Dirs, " "))
		sp := &Spoke{}
		res.Spokes[ns] = sp
		sp.Token = "unreadable"
		for _, l := range strings.Split(o, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "token" {
				sp.Token = f[1]
				break
			}
		}
		switch sp.Token {
		case "ok":
			out("%-10s %-14s claude token present", "creds", ns)
		case "EMPTY":
			out("%-10s %-14s CLAUDE TOKEN EMPTY — needs `claude auth login` (one login covers the fleet)", "creds", ns)
			res.OK = false
		default:
			sp.Token = "unreadable"
			out("%-10s %-14s claude credential unreadable", "creds", ns)
			res.OK = false
		}
		if strings.Contains(o, "theme repaired") {
			sp.Theme = "repaired"
			out("%-10s %-14s theme was unset -> set to dark", "theme", ns)
		}
		if strings.Contains(o, "theme repair-failed") {
			sp.Theme = "repair-failed"
			out("%-10s %-14s theme unset and REPAIR FAILED", "theme", ns)
			res.OK = false
		}
		if strings.Contains(o, "theme unset") {
			sp.Theme = "unset"
			out("%-10s %-14s THEME UNSET — CLI will stop at the picker; run reconcile", "theme", ns)
			res.OK = false
		}
		if strings.Contains(o, "statusline repaired") {
			sp.StatusLine = "repaired"
			out("%-10s %-14s broken agy statusLine (/status) removed", "agy", ns)
		}
		if strings.Contains(o, "statusline broken") {
			sp.StatusLine = "broken"
			out("%-10s %-14s agy statusLine is \"/status\" (broken) — run reconcile", "agy", ns)
		}
		if strings.Contains(o, "statusline repair-failed") {
			sp.StatusLine = "repair-failed"
			out("%-10s %-14s statusLine REPAIR FAILED", "agy", ns)
			res.OK = false
		}
		sp.Perms = strings.Contains(o, "perms repaired")
	}

	if res.OK {
		out("shared-auth: all spokes consistent")
	} else {
		out("shared-auth: PROBLEMS ABOVE")
	}
	return res
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = hiveclient.ShellQuote(s)
	}
	return strings.Join(q, " ")
}

// spokeScript is the bash's per-spoke exec, verbatim except that `reconcile`
// is decided per repair ($2 theme, $3 statusLine, $4 perms) so each can be
// switched off in the spec, and the perm repair announces itself.
// Args: $1 home, $2-$4 check|reconcile, $5 space-separated dirs.
const spokeScript = `
H="$1"; TA="$2"; SA="$3"; PA="$4"; DIRS="$5"
t=$(jq -r "if ((.claudeAiOauth.accessToken // \"\") == \"\") then \"EMPTY\" else \"ok\" end" "$H/.claude/.credentials.json" 2>/dev/null)
echo "token ${t:-unreadable}"
th=$(jq -r ".theme // \"null\"" "$H/.claude.json" 2>/dev/null)
if [ "${th:-null}" = null ]; then
  if [ "$TA" = reconcile ]; then
    f="$H/.claude.json"
    if [ ! -f "$f" ]; then
      printf "%s" "{\"theme\":\"dark\",\"hasCompletedOnboarding\":true}" > "$f" && chgrp node "$f" && chmod 664 "$f" \
        && echo "theme repaired" || echo "theme repair-failed"
    else
      tmp=$(mktemp) && jq ".theme = \"dark\" | .hasCompletedOnboarding = true" "$f" > "$tmp" \
        && cat "$tmp" > "$f" && rm -f "$tmp" && echo "theme repaired" || echo "theme repair-failed"
    fi
  else echo "theme unset"; fi
fi
sf="$H/.gemini/antigravity-cli/settings.json"
if [ -f "$sf" ] && [ "$(jq -r ".statusLine.command // empty" "$sf" 2>/dev/null)" = "/status" ]; then
  if [ "$SA" = reconcile ]; then
    tmp=$(mktemp) && jq "del(.statusLine)" "$sf" > "$tmp" && cat "$tmp" > "$sf" && rm -f "$tmp" \
      && echo "statusline repaired" || echo "statusline repair-failed"
  else echo "statusline broken"; fi
fi
if [ "$PA" = reconcile ]; then
  for d in $DIRS; do p="$H/$d"; [ -e "$p" ] || continue
    chgrp -R node "$p" 2>/dev/null; chmod -R g+rwX "$p" 2>/dev/null; done
  echo "perms repaired"
fi`
