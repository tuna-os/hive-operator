package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source describes one ccusage source as laid out in a hive pod.
//
// WHERE THE LOGS ARE (hive v5.35, measured 2026-09-24)
// ----------------------------------------------------
// Agents run with HOME=/data/home/agents/<name>, but .claude, .codex, .gemini,
// .copilot and .config there are symlinks back to /data/home, and /data/home/
// .claude, .gemini and .codex are RWX PVCs shared by every spoke. So:
//
//	claude       /data/home/.claude/projects/-data-agents-<a>/*.jsonl    shared, per-agent by project
//	codex        /data/home/.codex-<a>/sessions/**/rollout-*.jsonl      per-spoke, per-agent by CODEX_HOME
//	antigravity  /data/home/.gemini/antigravity-cli/conversations/*.db  shared, agent only inside the DB
//	pi           /data/home/.pi/agent/sessions/--data-agents-<a>--/     per-spoke, per-agent by project
//	goose        /data/home/.local/share/goose/sessions/sessions.db     per-spoke, no agent
//	copilot      /data/home/.copilot (session-store.db — NOT read by ccusage 20.0.24)
//	muse (meta)  /data/home/.local/share/muse/sessions                  NOT supported by ccusage
//
// ccusage is run with HOME=<agents' home> so its default paths resolve, and
// it is given no lookback view: it does not follow symlinked FILES (its walker
// uses DirEntry::file_type), so a filtered symlink farm reads as empty.
//
// A shared store is read by every spoke's sidecar. The operator de-duplicates
// on Fingerprint, never on mount identity: the same hostPath directory shows a
// different st_dev and st_ino in different pods (measured: dev 65 vs 66), so
// only the store's content identifies it.
type Source struct {
	// Name is the ccusage source (subcommand).
	Name string
	// Detect are paths relative to Home; the source is collected only when at
	// least one exists (an absent CLI is not an error). They are also what
	// the store fingerprint is computed over.
	Detect []string
	// PerRoot: each Detect match is a separate CODEX_HOME and a separate run,
	// because a CODEX_HOME list loses which directory a session came from.
	PerRoot bool
	// AttributeFromFile: attribute sessions by scanning the session's DB for
	// /data/agents/<name> (Antigravity keeps the workspace path inside its
	// SQLite trajectory; ccusage reports projectPath "Antigravity").
	AttributeFromFile bool
}

// DefaultSources is the fleet's layout.
func DefaultSources() []Source {
	return []Source{
		{Name: "claude", Detect: []string{".claude/projects"}},
		{Name: "codex", Detect: []string{".codex-*/sessions"}, PerRoot: true},
		{Name: "antigravity", Detect: []string{".gemini/antigravity-cli/conversations", ".gemini/antigravity/conversations"}, AttributeFromFile: true},
		{Name: "gemini", Detect: []string{".gemini/tmp"}},
		{Name: "pi", Detect: []string{".pi/agent/sessions"}},
		{Name: "goose", Detect: []string{".local/share/goose/sessions"}},
	}
}

// codexAgent maps a CODEX_HOME to an agent: hive gives each codex agent its
// own /data/home/.codex-<agent> (Codex refuses a CODEX_HOME the agent UID does
// not own), while plain .codex is the shared login store and holds no sessions.
func codexAgent(codexHome string) string {
	b := filepath.Base(codexHome)
	if a, ok := strings.CutPrefix(b, ".codex-"); ok && a != "" && a != "unknown" {
		return a
	}
	return Unattributed
}

// Run is the outcome of collecting one source once.
type Run struct {
	Source         string
	Fingerprint    string
	Report         SessionReport
	Attribute      Attributor
	Roots          int
	Duration       time.Duration
	UnpricedModels []string
	Err            error
}

// Collector runs ccusage over a home directory.
type Collector struct {
	Binary  string        // path to the ccusage binary
	Home    string        // the agents' home, e.g. /data/home — NOT $HOME, which is /root in exec
	Scratch string        // writable dir for ccusage's cache/tmp (the data mounts are read-only)
	Timeout time.Duration // per ccusage invocation
	Config  string        // optional ccusage.json (pricingOverrides for unpriced models)
	// Exec is swappable for tests.
	Exec func(ctx context.Context, env []string, args ...string) ([]byte, error)

	mu      sync.Mutex
	agyScan map[string]agyScan // db path → cached workspace scan
}

type agyScan struct {
	size  int64
	mtime time.Time
	agent string
}

func (c *Collector) exec(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if c.Exec != nil {
		return c.Exec(ctx, env, args...)
	}
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", c.Binary, strings.Join(args, " "), err, truncate(stderr.String(), 300))
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Collect runs one source and returns its normalised sessions.
func (c *Collector) Collect(ctx context.Context, src Source) (run Run) {
	start := time.Now()
	run.Source = src.Name
	defer func() { run.Duration = time.Since(start) }()

	var roots []string
	for _, g := range src.Detect {
		m, _ := filepath.Glob(filepath.Join(c.Home, g))
		roots = append(roots, m...)
	}
	sort.Strings(roots)
	run.Roots = len(roots)
	run.Fingerprint = Fingerprint(roots)
	if len(roots) == 0 {
		return run // absent source: not an error, just nothing to read
	}

	type job struct{ codexHome, agent string }
	jobs := []job{{}}
	if src.PerRoot {
		jobs = jobs[:0]
		for _, r := range roots {
			home := filepath.Dir(r) // .codex-<a>/sessions → .codex-<a>
			jobs = append(jobs, job{codexHome: home, agent: codexAgent(home)})
		}
	}

	byAgent := map[string]string{} // session id → agent, for PerRoot sources
	for _, j := range jobs {
		env := c.env()
		if j.codexHome != "" {
			env = append(env, "CODEX_HOME="+j.codexHome)
		}
		args := []string{src.Name, "session", "--json", "--offline"}
		if c.Config != "" {
			// Optional by design: the ConfigMap is mounted optional, and a
			// missing pricing file must not stop measurement.
			if _, err := os.Stat(c.Config); err == nil {
				args = append(args, "--config", c.Config)
			}
		}
		cctx, cancel := context.WithTimeout(ctx, c.timeout())
		out, err := c.exec(cctx, env, args...)
		cancel()
		if err != nil {
			run.Err = err
			return run
		}
		rep, err := ParseSessionReport(out)
		if err != nil {
			run.Err = err
			return run
		}
		for i := range rep.Sessions {
			if src.PerRoot {
				// Codex session ids are date paths and repeat across homes.
				rep.Sessions[i].ID = j.agent + "/" + rep.Sessions[i].ID
				byAgent[rep.Sessions[i].ID] = j.agent
			}
		}
		run.Report.Sessions = append(run.Report.Sessions, rep.Sessions...)
		run.UnpricedModels = append(run.UnpricedModels, rep.UnpricedModels...)
	}
	run.UnpricedModels = uniq(run.UnpricedModels)
	run.Report.UnpricedModels = run.UnpricedModels

	switch {
	case src.PerRoot:
		run.Attribute = func(s Session) string {
			if a, ok := byAgent[s.ID]; ok {
				return a
			}
			return Unattributed
		}
	case src.AttributeFromFile:
		dirs := roots
		run.Attribute = func(s Session) string { return c.agentFromDB(dirs, s.ID) }
	default:
		run.Attribute = func(s Session) string { return AgentFromProject(s.Project) }
	}
	return run
}

func (c *Collector) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Minute
}

// env builds ccusage's environment: a fixed TZ so dates never depend on the
// node, HOME at the agents' home so every default path resolves, and cache and
// temp in the sidecar's own scratch (the data volumes are mounted read-only).
func (c *Collector) env() []string {
	return []string{
		"TZ=UTC", "HOME=" + c.Home, "NO_COLOR=1", "PATH=/usr/bin:/bin",
		"XDG_CACHE_HOME=" + filepath.Join(c.Scratch, "cache"),
		"TMPDIR=" + c.Scratch, "SQLITE_TMPDIR=" + c.Scratch,
	}
}

func uniq(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// Fingerprint identifies a store by content: a hash of the relative paths of
// its lexicographically-first 64 files under each root, plus the root
// basenames. Two pods mounting the same shared PVC agree on it; two spokes'
// private stores do not. Mount identity (st_dev/st_ino) is useless here — see
// Source.
func Fingerprint(roots []string) string {
	if len(roots) == 0 {
		return ""
	}
	h := sha256.New()
	for _, r := range roots {
		fmt.Fprintf(h, "root %s\n", filepath.Base(r))
		var names []string
		_ = filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if rel, err := filepath.Rel(r, p); err == nil {
				names = append(names, rel)
			}
			return nil
		})
		sort.Strings(names)
		if len(names) > 64 {
			names = names[:64]
		}
		for _, n := range names {
			fmt.Fprintln(h, n)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var agentPathRE = regexp.MustCompile(`/data/agents/([a-z0-9][a-z0-9-]*)`)

// agentFromDB finds the hive agent an Antigravity conversation belongs to by
// scanning its database (and WAL) for the first /data/agents/<name> — the
// workspace path agy records in the trajectory. Cached per (size, mtime), so a
// quiet conversation is read once.
func (c *Collector) agentFromDB(dirs []string, sessionID string) string {
	var p string
	var info os.FileInfo
	for _, d := range dirs {
		cand := filepath.Join(d, sessionID+".db")
		if st, err := os.Stat(cand); err == nil {
			p, info = cand, st
			break
		}
	}
	if p == "" {
		return Unattributed
	}
	c.mu.Lock()
	if c.agyScan == nil {
		c.agyScan = map[string]agyScan{}
	}
	if s, ok := c.agyScan[p]; ok && s.size == info.Size() && s.mtime.Equal(info.ModTime()) {
		c.mu.Unlock()
		return s.agent
	}
	c.mu.Unlock()
	agent := scanAgent(p)
	if agent == Unattributed {
		agent = scanAgent(p + "-wal")
	}
	c.mu.Lock()
	c.agyScan[p] = agyScan{size: info.Size(), mtime: info.ModTime(), agent: agent}
	c.mu.Unlock()
	return agent
}

func scanAgent(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return Unattributed
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return Unattributed
	}
	return mostFrequentAgent(b)
}

// mostFrequentAgent picks the agent path that occurs most often. The path
// sits inside protobuf blobs, where a length or tag byte that happens to be a
// letter extends or truncates a match ("guid", "guidej" next to 131×"guide"
// in one real conversation) — a single first match is wrong often enough.
func mostFrequentAgent(b []byte) string {
	counts := map[string]int{}
	for _, m := range agentPathRE.FindAllSubmatch(b, 4096) {
		counts[string(m[1])]++
	}
	best, bestN := Unattributed, 0
	for a, n := range counts {
		if n > bestN || (n == bestN && a < best) {
			best, bestN = a, n
		}
	}
	return best
}
