package main

// --liveness: compare the operator's watchdog and nudge plans with the
// newest hive-watchdog* / hive-nudge job logs.
//
//   --live --liveness   plan every spoke in --sample from the LIVE cluster
//                       (read-only: GET /api/status, GET /api/pane, cat of
//                       hive-state.json and the governor block) and diff.
//   --watchdog-bash LOG --spoke spoke.json
//                       diff a deployed operator's .status.liveness
//                       .watchdogPlanText against a job log (offline).
//
// The two sides never see the same instant: the watchdog job ran up to 5
// minutes, the nudge job up to 30 minutes, before the operator's read, and
// both jobs ACT (a healed agent is ready by the time the operator looks; a
// kicked agent is no longer idle). Differences are therefore classified,
// and only the unexplained ones count:
//
//   skew/acted     bash healed (or kicked) the agent and the operator now
//                  sees the result
//   skew/time      the agent became overdue / unhealthy after the job ran
//   journal        a backoff line: bash's heal records live on its PVC, the
//                  operator's in .status.liveness.journal (empty until it runs)
//   logic          anything else — blocks promotion

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/liveness"
	"github.com/tuna-os/hive-operator/internal/rotation"
)

var reRestarted = regexp.MustCompile(`restarted \([^)]*\)$`)

// watchdogLines keeps the per-agent lines of a watchdog log (or plan text),
// keyed by agent, with the hive's restart answer normalised away.
func watchdogLines(text string) map[string][]string {
	out := map[string][]string{}
	for _, l := range lines(text) {
		l = strings.TrimRight(l, " \t\r")
		if l == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "usage: ") || strings.HasPrefix(l, "watchdog: ") ||
			strings.HasPrefix(l, "status snapshot is ") || strings.HasPrefix(l, "renewal reached for:") ||
			strings.HasPrefix(l, "!!!") || strings.HasPrefix(l, "WARN") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		out[f[0]] = append(out[f[0]], reRestarted.ReplaceAllString(l, "restarted (…)"))
	}
	return out
}

type residual struct {
	class, agent, bash, op string
}

func healing(ls []string) bool {
	for _, l := range ls {
		if strings.Contains(l, "-> healing") || strings.Contains(l, " MISMATCH ") || strings.Contains(l, " EFFORT ") {
			return true
		}
	}
	return false
}

func backingOff(ls []string) bool {
	for _, l := range ls {
		if strings.Contains(l, "backing off") {
			return true
		}
	}
	return false
}

func ok(ls []string) bool { return len(ls) == 1 && strings.HasSuffix(ls[0], " liveness ok") }

// compareWatchdog classifies per-agent differences (see the file comment).
func compareWatchdog(bash, op map[string][]string) (int, []residual) {
	agents := map[string]bool{}
	for a := range bash {
		agents[a] = true
	}
	for a := range op {
		agents[a] = true
	}
	names := make([]string, 0, len(agents))
	for a := range agents {
		names = append(names, a)
	}
	sort.Strings(names)
	same := 0
	var res []residual
	for _, a := range names {
		b, o := strings.Join(bash[a], "\n"), strings.Join(op[a], "\n")
		if b == o {
			same++
			continue
		}
		class := "logic"
		switch {
		case healing(bash[a]) && ok(op[a]):
			class = "skew/acted"
		case backingOff(bash[a]) || backingOff(op[a]):
			class = "journal"
		case ok(bash[a]) && len(op[a]) > 0:
			class = "skew/time"
		}
		res = append(res, residual{class, a, b, o})
	}
	return same, res
}

// nudgeEntry is one "ns agent idle Nm > Tm (longest cadence Lm xG)" line.
type nudgeEntry struct {
	ns, agent         string
	idle, thr, longst int
}

var reNudge = regexp.MustCompile(`^(\S+)\s+(\S+)\s+idle (\d+)m > (\d+)m \(longest cadence (\d+)m x\d+\)`)

func nudgeEntries(text string) map[string]nudgeEntry {
	out := map[string]nudgeEntry{}
	for _, l := range lines(text) {
		m := reNudge.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[3])
		t, _ := strconv.Atoi(m[4])
		lc, _ := strconv.Atoi(m[5])
		out[m[1]+"/"+m[2]] = nudgeEntry{m[1], m[2], i, t, lc}
	}
	return out
}

// compareNudge: bash's kicks at jobAt vs the operator's candidates now.
// idleNow lets a candidate that only crossed its threshold after the job be
// told apart from a rule difference.
func compareNudge(bash, op map[string]nudgeEntry, idleNow map[string]int64, sinceJob time.Duration) (int, []residual) {
	keys := map[string]bool{}
	for k := range bash {
		keys[k] = true
	}
	for k := range op {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	same := 0
	var res []residual
	for _, k := range names {
		b, inB := bash[k]
		o, inO := op[k]
		switch {
		case inB && inO && b.thr == o.thr && b.longst == o.longst:
			same++
		case inB && inO:
			res = append(res, residual{"logic", k, fmt.Sprintf("thr %dm longest %dm", b.thr, b.longst), fmt.Sprintf("thr %dm longest %dm", o.thr, o.longst)})
		case inB:
			// bash kicked it: idle restarted from 0 (the kick), so the
			// operator rightly no longer sees it overdue.
			cls := "logic"
			if idle, ok := idleNow[k]; ok && idle >= 0 && time.Duration(idle)*time.Second <= sinceJob+time.Minute {
				cls = "skew/acted"
			}
			res = append(res, residual{cls, k, fmt.Sprintf("kicked (idle %dm > %dm)", b.idle, b.thr), "not overdue"})
		default:
			cls := "logic"
			if time.Duration(o.idle)*time.Minute-sinceJob <= time.Duration(o.thr)*time.Minute {
				cls = "skew/time"
			}
			res = append(res, residual{cls, k, "not overdue", fmt.Sprintf("would kick (idle %dm > %dm)", o.idle, o.thr)})
		}
	}
	return same, res
}

func printResiduals(res []residual) (logic int) {
	for _, r := range res {
		fmt.Printf("≠ %-22s [%s]\n    bash:     %s\n    operator: %s\n", r.agent, r.class,
			strings.ReplaceAll(r.bash, "\n", "\n              "), strings.ReplaceAll(r.op, "\n", "\n              "))
		if r.class == "logic" {
			logic++
		}
	}
	return logic
}

// liveAgents reads one spoke for the liveness planners (all reads).
type liveSpoke struct {
	agents   []liveness.Agent
	rot      []rotation.Agent
	nudge    []liveness.NudgeAgent
	snapshot time.Time
	budget   struct {
		exhausted   bool
		pct, weekly float64
	}
	panes    map[string]string
	ages     map[string]int64
	governor string
}

func (e *liveEnv) livenessSpoke(ns string) (*liveSpoke, error) {
	pod, err := e.hive.Pod(e.ctx, ns)
	if err != nil {
		return nil, err
	}
	tok, err := e.hive.Secret(e.ctx, ns, "hive-secrets", "HIVE_DASHBOARD_TOKEN")
	if err != nil {
		return nil, err
	}
	var st struct {
		Timestamp string `json:"timestamp"`
		Budget    struct {
			Exhausted bool    `json:"BUDGET_EXHAUSTED"`
			Pct       float64 `json:"BUDGET_PCT_USED"`
			Weekly    float64 `json:"BUDGET_WEEKLY"`
		} `json:"budget"`
		Agents []struct {
			Name, CLI, GovModel, Model, Cadence, PausedTrigger, Busy string
			ReasoningEffort                                          string `json:"reasoningEffort"`
			LiveSummary                                              string `json:"liveSummary"`
			Paused, NeedsLogin, AuthKnown, AuthAvailable             bool
			OnDemand, Enabled                                        *bool
		} `json:"agents"`
	}
	if err := e.hive.GetJSON(e.ctx, ns, pod, tok, "/api/status", &st); err != nil {
		return nil, err
	}
	var rt struct {
		Agents map[string]struct {
			Backend string `json:"backend_override"`
			Model   string `json:"model_override"`
		} `json:"agents"`
	}
	if out, err := e.hive.Sh(e.ctx, ns, pod, "cat /data/hive-state.json 2>/dev/null"); err == nil {
		_ = json.Unmarshal([]byte(out), &rt)
	}
	ls := &liveSpoke{panes: map[string]string{}, ages: map[string]int64{}}
	ls.snapshot, _ = time.Parse(time.RFC3339, st.Timestamp)
	ls.budget.exhausted, ls.budget.pct, ls.budget.weekly = st.Budget.Exhausted, st.Budget.Pct, st.Budget.Weekly
	for _, a := range st.Agents {
		m, cli := a.Model, a.CLI
		if a.GovModel != "" {
			m = a.GovModel
		}
		if o, ok := rt.Agents[a.Name]; ok {
			if o.Backend != "" {
				cli = o.Backend
			}
			if o.Model != "" {
				m = o.Model
			}
		}
		la := liveness.Agent{Name: a.Name, CLI: cli, Model: m, Effort: a.ReasoningEffort, Paused: a.Paused, Busy: a.Busy,
			NeedsLogin: a.NeedsLogin, AuthKnown: a.AuthKnown, AuthAvailable: a.AuthAvailable, LiveSummary: a.LiveSummary}
		ls.agents = append(ls.agents, la)
		ls.rot = append(ls.rot, rotation.Agent{Name: a.Name, CLI: cli, Model: m, Effort: a.ReasoningEffort, Cadence: a.Cadence,
			Paused: a.Paused, PausedTrigger: a.PausedTrigger, OnDemand: a.OnDemand != nil && *a.OnDemand})
		ls.nudge = append(ls.nudge, liveness.NudgeAgent{Name: a.Name, Paused: a.Paused, OnDemand: a.OnDemand != nil && *a.OnDemand, Enabled: a.Enabled})
		if liveness.NeedsLivePane(la) {
			var p struct {
				Lines []string `json:"lines"`
			}
			if err := e.hive.GetJSON(e.ctx, ns, pod, tok, "/api/pane/"+a.Name+"?lines=60", &p); err == nil {
				ls.panes[a.Name] = strings.Join(p.Lines, "\n")
			}
		}
	}
	if out, err := e.hive.Sh(e.ctx, ns, pod, `now=$(date -u +%s); jq -r '.agents | to_entries[] | "\(.key) \(.value.last_kick // "never")"' /data/hive-state.json 2>/dev/null | while read -r a t; do if [ "$t" = "never" ]; then echo "$a -1"; else e=$(date -u -d "$t" +%s 2>/dev/null) && echo "$a $((now-e))" || echo "$a -1"; fi; done`); err == nil {
		for _, l := range lines(out) {
			if f := strings.Fields(l); len(f) == 2 {
				if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					ls.ages[f[0]] = v
				}
			}
		}
	}
	ls.governor, _ = e.hive.Sh(e.ctx, ns, pod, `sed -n "/^governor:/,/^[a-z_]*:/p" /data/hive.yaml.runtime`)
	return ls, nil
}

// operatorJournal reads a deployed operator's liveness journal for a spoke
// (empty when the operator does not run liveness yet).
func operatorJournal(ctx context.Context, cfg *rest.Config, name string) liveness.Journal {
	j := liveness.Journal{Heals: map[string]liveness.Heal{}, Panes: map[string]liveness.PaneSeen{}, Resets: map[string]time.Time{}}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return j
	}
	u, err := dc.Resource(schema.GroupVersionResource{Group: "hive.tunaos.org", Version: "v1alpha1", Resource: "hivespokes"}).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return j
	}
	raw, _, _ := unstructuredField(u.Object, "status", "liveness", "journal")
	b, _ := json.Marshal(raw)
	var lj hivev1.LivenessJournal
	if json.Unmarshal(b, &lj) != nil {
		return j
	}
	for _, h := range lj.Heals {
		j.Heals[h.Agent] = liveness.Heal{Count: int(h.Count), Last: h.Last.Time}
	}
	for _, p := range lj.Panes {
		j.Panes[p.Agent] = liveness.PaneSeen{Hash: p.Hash, Since: p.Since.Time}
	}
	for _, r := range lj.Resets {
		j.Resets[r.Provider] = r.At.Time
	}
	if lj.HygieneAt != nil {
		j.HygieneAt = lj.HygieneAt.Time
	}
	return j
}

func unstructuredField(obj map[string]any, path ...string) (any, bool, error) {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[p]
		if !ok {
			return nil, false, nil
		}
	}
	return cur, true, nil
}

func runLiveLiveness(e *liveEnv, cfg *rest.Config) int {
	readings, src := e.readings()
	fmt.Printf("live liveness diff at %s (readings from %s)\n\n", e.now.Format(time.RFC3339), src)
	watchdogJob := map[string]string{"hive": "hive-watchdog", "hive-reef": "hive-watchdog-reef", "hive-hanthor": "hive-watchdog-hanthor"}
	logic := 0
	nudgeJob, nudgeAt, nudgeLog, nudgeErr := e.latestJob("hive-nudge")
	bashNudge := nudgeEntries(nudgeLog)
	opNudge := map[string]nudgeEntry{}
	idleNow := map[string]int64{}
	for _, sp := range e.spokes {
		ns := sp.Spec.Namespace
		ls, err := e.livenessSpoke(ns)
		if err != nil {
			fmt.Printf("== %s: %v\n", sp.Name, err)
			logic++
			continue
		}
		job, jobAt, log, jobErr := e.latestJob(watchdogJob[ns])
		fmt.Printf("== %s (%s) watchdog: operator vs %s (%s ago); /api/status snapshot %s\n", sp.Name, ns, job,
			e.now.Sub(jobAt).Round(time.Second), ls.snapshot.Format(time.RFC3339))
		rin := rotation.Input{Now: e.now, Namespace: ns, Agents: ls.rot, Providers: readings, UsageSource: src, Tiers: agentTiers,
			Rungs: e.ladder, Pins: map[string]bool{}, Holds: map[string]bool{}, Policy: rotation.DefaultPolicy()}
		in := liveness.Input{Now: e.now, Snapshot: ls.snapshot, Agents: ls.agents, LivePane: ls.panes, Pins: map[string]bool{},
			Rotation: rin, Journal: operatorJournal(e.ctx, cfg, sp.Name), Policy: liveness.DefaultPolicy()}
		for _, p := range sp.Spec.Pins {
			in.Pins[p.Agent] = true
		}
		plan := liveness.Watchdog(in)
		opText := liveness.Render(plan.Lines, plan.Actions, liveness.ShadowOutcomes(plan.Actions))
		if jobErr != nil {
			fmt.Printf("   %v\n", jobErr)
			logic++
		} else {
			same, res := compareWatchdog(watchdogLines(log), watchdogLines(strings.Join(opText, "\n")))
			l := printResiduals(res)
			logic += l
			fmt.Printf("   %d identical, %d explained, %d logic\n", same, len(res)-l, l)
		}

		np := liveness.Nudge(liveness.NudgeInput{Namespace: ns, Cadences: liveness.LongestCadences(ls.governor),
			CadencesErr: ls.governor == "", Agents: ls.nudge, Idle: ls.ages, BudgetExhausted: ls.budget.exhausted,
			BudgetPctUsed: ls.budget.pct, BudgetWeekly: ls.budget.weekly, Policy: liveness.DefaultNudgePolicy()})
		for k, v := range nudgeEntries(strings.Join(liveness.NudgeLines(np, liveness.ShadowKicks(np, 4), 30), "\n")) {
			opNudge[k] = v
		}
		for a, v := range ls.ages {
			idleNow[ns+"/"+a] = v
		}
		fmt.Println()
	}
	fmt.Printf("== nudge: operator vs %s (%s ago)\n", nudgeJob, e.now.Sub(nudgeAt).Round(time.Second))
	if nudgeErr != nil {
		fmt.Printf("   %v\n", nudgeErr)
		logic++
	} else {
		same, res := compareNudge(bashNudge, opNudge, idleNow, e.now.Sub(nudgeAt))
		l := printResiduals(res)
		logic += l
		fmt.Printf("   %d identical, %d explained, %d logic\n", same, len(res)-l, l)
	}
	fmt.Printf("\n%d unexplained difference(s)\n", logic)
	if logic > 0 {
		return 1
	}
	return 0
}

func runLivenessLive(samplePath string) int {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	spokes, ladder, err := loadSample(samplePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e := &liveEnv{ctx: ctx, cs: cs, hive: hiveclient.New(cs, cfg), spokes: spokes, ladder: rungsFrom(ladder), now: time.Now().UTC()}
	if err := e.ccleft(); err != nil {
		fmt.Printf("ccleft unreadable (%v): readings from the ConfigMap\n", err)
	}
	return runLiveLiveness(e, cfg)
}

// offlineWatchdog diffs a job log against a deployed operator's
// .status.liveness.watchdogPlanText.
func offlineWatchdog(bashLog string, sp hivev1.HiveSpoke) int {
	if sp.Status.Liveness == nil || len(sp.Status.Liveness.WatchdogPlanText) == 0 {
		fmt.Println("spoke has no .status.liveness.watchdogPlanText — is livenessMode Shadow and the operator new enough?")
		return 2
	}
	at := "?"
	if sp.Status.Liveness.WatchdogAt != nil {
		at = sp.Status.Liveness.WatchdogAt.UTC().Format(time.RFC3339)
	}
	fmt.Printf("spoke %s  watchdog pass %s\n\n", sp.Name, at)
	same, res := compareWatchdog(watchdogLines(bashLog), watchdogLines(strings.Join(sp.Status.Liveness.WatchdogPlanText, "\n")))
	l := printResiduals(res)
	fmt.Printf("\n%d identical, %d explained, %d logic\n", same, len(res)-l, l)
	if l > 0 {
		return 1
	}
	return 0
}
