package main

// --live: run the operator's planner against the LIVE cluster, read-only,
// and diff it with the newest bash job logs — no operator deployment needed.
//
// Every call is a read: GET pods/jobs/configmaps/secrets/deployments, pod
// logs, and two `exec`s that only read (curl GET /api/status with the
// spoke's X-Hive-Internal token; curl GET ccleft /readings). Nothing is
// placed, paused, scaled or written.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/rotation"
	"github.com/tuna-os/hive-operator/internal/usage"
)

// bash's AGENT_TIERS.
var agentTiers = map[string]string{
	"supervisor": "T2", "scanner": "T2", "ci-maintainer": "T2",
	"quality": "T2", "guide": "T2", "outreach": "T2",
	"operations": "T2", "telemetry": "T2", "architect": "T1",
	"sec-check": "T1", "strategist": "T1",
}

type liveEnv struct {
	ctx    context.Context
	cs     kubernetes.Interface
	hive   *hiveclient.Client
	spokes []hivev1.HiveSpoke
	ladder []rotation.Rung
	cc     *usage.CcleftOutput
	now    time.Time
}

func loadSample(path string) ([]hivev1.HiveSpoke, hivev1.ModelLadder, error) {
	var spokes []hivev1.HiveSpoke
	var ladder hivev1.ModelLadder
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, ladder, err
	}
	for _, doc := range strings.Split(string(b), "\n---\n") {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			return nil, ladder, err
		}
		switch meta.Kind {
		case "HiveSpoke":
			var s hivev1.HiveSpoke
			if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
				return nil, ladder, err
			}
			spokes = append(spokes, s)
		case "ModelLadder":
			if err := yaml.Unmarshal([]byte(doc), &ladder); err != nil {
				return nil, ladder, err
			}
		}
	}
	return spokes, ladder, nil
}

// latestJobLog: the newest SUCCEEDED job of a CronJob and its log.
func (e *liveEnv) latestJobLog(cronjob string) (string, string, error) {
	n, _, l, err := e.latestJob(cronjob)
	return n, l, err
}

func (e *liveEnv) latestJob(cronjob string) (string, time.Time, string, error) {
	jobs, err := e.cs.BatchV1().Jobs("hive").List(e.ctx, metav1.ListOptions{})
	if err != nil {
		return "", time.Time{}, "", err
	}
	var best *batchv1.Job
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if len(j.OwnerReferences) == 0 || j.OwnerReferences[0].Name != cronjob || j.Status.Succeeded == 0 {
			continue
		}
		if best == nil || j.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = j
		}
	}
	if best == nil {
		return "", time.Time{}, "", fmt.Errorf("no succeeded job for cronjob %s", cronjob)
	}
	at := best.CreationTimestamp.Time
	pods, err := e.cs.CoreV1().Pods("hive").List(e.ctx, metav1.ListOptions{LabelSelector: "job-name=" + best.Name})
	if err != nil || len(pods.Items) == 0 {
		return best.Name, at, "", fmt.Errorf("no pod for job %s: %v", best.Name, err)
	}
	rc, err := e.cs.CoreV1().Pods("hive").GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Stream(e.ctx)
	if err != nil {
		return best.Name, at, "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return best.Name, at, string(b), err
}

// paceMoves: "ns/agent" → {from, to} model for every demote/restore/promote
// line of a hive-pace apply log.
func paceMoves(log string) map[string][2]string {
	out := map[string][2]string{}
	for _, l := range lines(log) {
		f := strings.Fields(l)
		if len(f) >= 5 && (f[0] == "demote" || f[0] == "restore" || f[0] == "kiro-demote" || f[0] == "kiro-promote") && f[3] == "->" {
			out[f[1]] = [2]string{f[2], f[4]}
		}
	}
	return out
}

// revert puts agents the pace job moved back where they were when it ran,
// so a status snapshot taken AFTER the job reproduces the job's input.
func revert(ns string, agents []rotation.Agent, moves map[string][2]string) []string {
	var undone []string
	for i := range agents {
		if mv, ok := moves[ns+"/"+agents[i].Name]; ok && agents[i].Model == mv[1] {
			agents[i].Model = mv[0]
			undone = append(undone, agents[i].Name)
		}
	}
	return undone
}

func (e *liveEnv) ccleft() error {
	pods, err := e.cs.CoreV1().Pods("hive").List(e.ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=ccleft"})
	if err != nil || len(pods.Items) == 0 {
		pods, err = e.cs.CoreV1().Pods("hive").List(e.ctx, metav1.ListOptions{LabelSelector: "app=ccleft"})
	}
	pod := ""
	if err == nil {
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
				pod = p.Name
				break
			}
		}
	}
	if pod == "" {
		return fmt.Errorf("no running ccleft pod: %v", err)
	}
	out, _, err := e.hive.Exec(e.ctx, "hive", pod, []string{"curl", "-sS", "-m", "15", "http://127.0.0.1:9464/readings"})
	if err != nil {
		return err
	}
	var cc usage.CcleftOutput
	if err := json.Unmarshal([]byte(out), &cc); err != nil {
		return err
	}
	e.cc = &cc
	return nil
}

// agents reads /api/status (govModel/cadence) for one spoke, overlaid with
// hive-state.json overrides exactly as the HiveSpoke controller does.
func (e *liveEnv) agents(ns string) ([]rotation.Agent, string, error) {
	pod, err := e.hive.Pod(e.ctx, ns)
	if err != nil {
		return nil, "", err
	}
	tok, err := e.hive.Secret(e.ctx, ns, "hive-secrets", "HIVE_DASHBOARD_TOKEN")
	if err != nil {
		return nil, "", err
	}
	var st struct {
		Timestamp string `json:"timestamp"`
		Agents    []struct {
			Name, CLI, GovModel, Model, Cadence, PausedTrigger string
			ReasoningEffort                                    string `json:"reasoningEffort"`
			Paused                                             bool
			OnDemand                                           *bool `json:"onDemand"`
		} `json:"agents"`
	}
	if err := e.hive.GetJSON(e.ctx, ns, pod, tok, "/api/status", &st); err != nil {
		return nil, "", err
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
	var out []rotation.Agent
	for _, a := range st.Agents {
		m := a.Model
		if a.GovModel != "" {
			m = a.GovModel
		}
		cli := a.CLI
		if o, ok := rt.Agents[a.Name]; ok {
			if o.Backend != "" {
				cli = o.Backend
			}
			if o.Model != "" {
				m = o.Model
			}
		}
		out = append(out, rotation.Agent{Name: a.Name, CLI: cli, Model: m, Effort: a.ReasoningEffort, Cadence: a.Cadence,
			Paused: a.Paused, PausedTrigger: a.PausedTrigger, OnDemand: a.OnDemand != nil && *a.OnDemand})
	}
	return out, st.Timestamp, nil
}

func (e *liveEnv) readings() (map[string]rotation.Reading, string) {
	out := map[string]rotation.Reading{}
	if e.cc != nil {
		for _, p := range rotation.Providers {
			l := usage.CcleftProbe(e.cc, p, e.now, 0, 0)
			out[p] = rotation.Reading{Percent: float64(l.Percent), Note: l.Note}
		}
		return out, "ccleft"
	}
	cm, err := e.cs.CoreV1().ConfigMaps("hive").Get(e.ctx, "hive-provider-usage", metav1.GetOptions{})
	for _, p := range rotation.Providers {
		v := "unknown unpublished"
		if err == nil {
			if x, ok := cm.Data[p]; ok {
				v = x
			}
		}
		l := usage.PublishedToProbe(v)
		out[p] = rotation.Reading{Percent: float64(l.Percent), Note: l.Note}
	}
	return out, "configmap"
}

func rungsFrom(l hivev1.ModelLadder) []rotation.Rung {
	var out []rotation.Rung
	for _, r := range l.Spec.Builtin {
		if r.Backend == "muse" && !strings.HasSuffix(r.Model, "-contributor") {
			continue
		}
		out = append(out, rotation.Rung{Tier: r.Tier, Provider: r.Provider, Backend: r.Backend, Model: r.Model})
	}
	return out
}

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

// paceActions: the action lines and footer of a hive-pace apply log.
func paceActions(log string) []string {
	var out []string
	for _, l := range lines(log) {
		if strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "pace: ") {
			out = append(out, l)
		}
	}
	return out
}

// contributorLines: the contributors section of a hive-rotate log.
func contributorLines(log string) []string {
	var out []string
	on := false
	for _, l := range lines(log) {
		switch {
		case l == "contributors:":
			on = true
		case l == "" || strings.HasPrefix(l, "fleet already") || strings.Contains(l, " change(s) — run "):
			if on && l != "" {
				on = false
			}
		case on:
			out = append(out, l)
		}
	}
	return out
}

func runLive(samplePath string) int {
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
	readings, src := e.readings()
	fmt.Printf("live shadow diff at %s, readings from %s:\n", e.now.Format(time.RFC3339), src)
	for _, p := range rotation.Providers {
		fmt.Printf("  %-9s %s\n", p, usage.ProbeLine{Percent: int(readings[p].Percent), Note: readings[p].Note}.Published())
	}
	fmt.Println()

	cronjobOf := map[string]string{"hive": "hive-rotate", "hive-reef": "hive-rotate-reef", "hive-hanthor": "hive-rotate-hanthor"}
	sort.SliceStable(spokes, func(i, j int) bool {
		return spokes[i].Spec.Pace != nil && spokes[j].Spec.Pace != nil && spokes[i].Spec.Pace.FleetOrder < spokes[j].Spec.Pace.FleetOrder
	})
	totalDiff := 0
	paceJob, paceAt, paceLog, paceErr := e.latestJob("hive-pace")
	moves := paceMoves(paceLog)
	var fleet []rotation.FleetAgent
	pins := map[string]bool{}
	for _, sp := range spokes {
		ns := sp.Spec.Namespace
		agents, ts, err := e.agents(ns)
		if err != nil {
			fmt.Printf("== %s: %v\n", sp.Name, err)
			totalDiff++
			continue
		}
		// The fleet as the pace job saw it.
		paceAgents := append([]rotation.Agent(nil), agents...)
		if u := revert(ns, paceAgents, moves); len(u) > 0 {
			fmt.Printf("   (pace view of %s: undid %s's moves of %v)\n", ns, paceJob, u)
		}
		for _, a := range paceAgents {
			fa := rotation.FleetAgent{Namespace: ns, Agent: a}
			for _, r := range e.ladder {
				if r.Tier == agentTiers[a.Name] {
					fa.Members = append(fa.Members, r)
				}
			}
			fleet = append(fleet, fa)
		}
		job, rotAt, log, jobErr := e.latestJob(cronjobOf[ns])
		// A pace job newer than the rotate job moved agents after bash
		// planned: plan from the state bash saw.
		if paceErr == nil && jobErr == nil && paceAt.After(rotAt) {
			if u := revert(ns, agents, moves); len(u) > 0 {
				fmt.Printf("   (rotate view of %s: undid %s's later moves of %v)\n", ns, paceJob, u)
			}
		}
		in := rotation.Input{Now: e.now, Namespace: ns, Primary: sp.Spec.PrimarySpoke == ns, PrimaryNamespace: "hive",
			Agents: agents, Providers: readings, UsageSource: src, Tiers: agentTiers, Rungs: e.ladder,
			Pins: map[string]bool{}, Holds: map[string]bool{}, Policy: rotation.DefaultPolicy()}
		for _, p := range sp.Spec.Pins {
			in.Pins[p.Agent] = true
			pins[ns+"/"+p.Agent] = true
		}
		for _, h := range sp.Spec.Holds {
			in.Holds[h] = true
		}
		plan := rotation.Compute(in)
		fmt.Printf("== %s (%s): operator vs %s; /api/status snapshot %s\n", sp.Name, ns, job, ts)
		if jobErr != nil {
			fmt.Printf("   %v\n", jobErr)
			totalDiff++
			continue
		}
		bash := planLines(strings.NewReader(log))
		op := planLines(strings.NewReader(strings.Join(plan.Lines(false), "\n")))
		same, diff := compare(bash, op)
		totalDiff += diff
		if in.Primary {
			var deps []rotation.ContribDeploy
			if list, err := cs.AppsV1().Deployments("hive-contributors").List(ctx, metav1.ListOptions{}); err == nil {
				for _, d := range list.Items {
					cd := rotation.ContribDeploy{Name: d.Name}
					if d.Spec.Replicas != nil {
						cd.Replicas = *d.Spec.Replicas
					}
					for _, ev := range d.Spec.Template.Spec.Containers[0].Env {
						switch ev.Name {
						case "AGENT_BACKEND":
							cd.Backend = ev.Value
						case "AGENT_MODEL":
							cd.Model = ev.Value
						}
					}
					deps = append(deps, cd)
				}
			}
			var got []string
			for _, d := range rotation.Contributors(in, deps, "hive-contributors") {
				got = append(got, d.Line)
			}
			want := contributorLines(log)
			if strings.Join(got, "\n") == strings.Join(want, "\n") {
				same++
			} else {
				diff++
				totalDiff++
				fmt.Printf("≠ contributors\n    bash:     %s\n    operator: %s\n", strings.Join(want, "\n              "), strings.Join(got, "\n              "))
			}
		}
		fmt.Printf("   %d identical, %d different\n\n", same, diff)
	}

	// Pace: the actions the operator plans from bash's own published
	// verdicts (hive/hive-pace) and the live fleet, against the newest
	// hive-pace job. (The operator's OWN verdicts need its sample history,
	// which only a running operator accumulates — compare those from
	// UsagePool .status.pace once deployed.)
	cm, cmErr := cs.CoreV1().ConfigMaps("hive").Get(ctx, "hive-pace", metav1.GetOptions{})
	fmt.Printf("== pace: operator vs %s\n", paceJob)
	if paceErr != nil || cmErr != nil {
		fmt.Printf("   %v %v\n", paceErr, cmErr)
		return 1
	}
	log := paceLog
	verdicts, kb := verdictsFromCM(cm.Data)
	pin := map[string]bool{}
	for k := range pins {
		pin[k] = true
	}
	pp := rotation.PlanPace(rotation.PaceInput{Now: e.now, Fleet: fleet, Verdicts: verdicts, Kiro: kb, Readings: readings,
		Pins: pin, Config: rotation.DefaultPaceConfig()})
	want, got := paceActions(log), pp.Text()
	tab := strings.Join(rotation.VerdictTable(verdicts), "\n")
	if !strings.Contains(log, tab) {
		fmt.Printf("≠ verdict table (rendered from hive/hive-pace)\n%s\n", tab)
		totalDiff++
	}
	if strings.Join(want, "\n") == strings.Join(got, "\n") {
		fmt.Printf("   actions identical (%d line(s)), fleet %d agents\n", len(want), len(fleet))
	} else {
		totalDiff++
		fmt.Printf("≠ actions\n    bash:     %s\n    operator: %s\n", strings.Join(want, "\n              "), strings.Join(got, "\n              "))
	}
	fmt.Printf("\n%d difference(s)\n", totalDiff)
	if totalDiff > 0 {
		return 1
	}
	return 0
}

func compare(bash, op map[string][]string) (int, int) {
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
	same, diff := 0, 0
	for _, a := range names {
		b, o := strings.Join(bash[a], "\n"), strings.Join(op[a], "\n")
		if b == o {
			same++
			continue
		}
		diff++
		fmt.Printf("≠ %s\n", a)
		for _, l := range bash[a] {
			fmt.Printf("    bash:     %s\n", l)
		}
		if len(bash[a]) == 0 {
			fmt.Printf("    bash:     (no line)\n")
		}
		for _, l := range op[a] {
			fmt.Printf("    operator: %s\n", l)
		}
		if len(op[a]) == 0 {
			fmt.Printf("    operator: (no line)\n")
		}
	}
	return same, diff
}

// verdictsFromCM parses hive/hive-pace's pace.json and kiro-budget.json.
func verdictsFromCM(data map[string]string) (map[string]rotation.PaceVerdict, rotation.KiroBudget) {
	var raw map[string]struct {
		Verdict     string
		Pressure    *float64
		BindingSlot string `json:"binding_slot"`
		Slots       map[string]struct {
			Pct          float64
			Reset        *int64
			Samples      int
			SpanS        int64    `json:"span_s"`
			HoursLeft    *float64 `json:"hours_left"`
			AllowedRate  *float64 `json:"allowed_rate"`
			ObservedRate *float64 `json:"observed_rate"`
			Ratio        *float64
		}
	}
	_ = json.Unmarshal([]byte(data["pace.json"]), &raw)
	v := map[string]rotation.PaceVerdict{}
	for p, r := range raw {
		pv := rotation.PaceVerdict{Verdict: r.Verdict, Pressure: r.Pressure, BindingSlot: r.BindingSlot, Slots: map[string]rotation.SlotFit{}}
		for s, f := range r.Slots {
			sf := rotation.SlotFit{Pct: f.Pct, Samples: f.Samples, SpanS: f.SpanS, HoursLeft: f.HoursLeft,
				AllowedRate: f.AllowedRate, ObservedRate: f.ObservedRate, Ratio: f.Ratio}
			if f.Reset != nil {
				sf.Reset = *f.Reset
			}
			pv.Slots[s] = sf
		}
		v[p] = pv
	}
	var k struct {
		Verdict                        string
		Used, Limit, Remaining, Safety float64
		Reset                          int64
		ReadingAgeS                    int64 `json:"reading_age_s"`
		Allowed, Burn, Ratio           *float64
		HoursLeft                      *float64 `json:"hours_left"`
		Burn6h                         *float64 `json:"burn_6h"`
		Samples                        int
		SpanS                          int64 `json:"span_s"`
		WindowStart                    int64 `json:"window_start"`
	}
	_ = json.Unmarshal([]byte(data["kiro-budget.json"]), &k)
	return v, rotation.KiroBudget{Verdict: k.Verdict, Used: k.Used, Limit: k.Limit, Remaining: k.Remaining, Reset: k.Reset,
		ReadingAgeS: k.ReadingAgeS, Safety: k.Safety, HoursLeft: k.HoursLeft, Allowed: k.Allowed, Burn: k.Burn,
		Burn6h: k.Burn6h, Ratio: k.Ratio, Samples: k.Samples, SpanS: k.SpanS, WindowStart: k.WindowStart}
}
