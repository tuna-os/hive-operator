package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/hiveclient"
	"github.com/tuna-os/hive-operator/internal/liveness"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/rotation"
)

// LivenessAPI is what the watchdog and nudge need from a spoke, bound to its
// pod and X-Hive-Internal token. Pane and Governor are reads and run in
// Shadow too (bash's watchdog reads the live pane in every mode); the rest
// mutate and run only in Enforce.
type LivenessAPI interface {
	rotation.HiveAPI
	// Pane is GET /api/pane/{agent}?lines=60, lines joined with "\n" ("" when
	// the hive returned none).
	Pane(ctx context.Context, ns, agent string) (string, error)
	// Governor is the `governor:` block of /data/hive.yaml.runtime.
	Governor(ctx context.Context, ns string) (string, error)
	// Restart is POST /api/restart/{agent}; fixPerms first reopens the shared
	// agy/muse first-run state, in the same exec.
	Restart(ctx context.Context, ns, agent string, fixPerms bool) (string, error)
	// Hygiene is the shared-home permission repair (gemini_hygiene).
	Hygiene(ctx context.Context, ns string) error
}

// livenessPermsFix reopens the shared first-run state every agy/muse launch
// rewrites mode 0600 under its own uid, so the NEXT agent to launch hits
// EACCES and sits on a wizard. Only a root exec can chmod a file another
// agent uid owns (v5's entrypoint perm guard runs as dev).
const livenessPermsFix = `for f in /data/home/.gemini/antigravity-cli/cache/*.json /data/home/.gemini/antigravity-cli/*.json \
         /data/home/.config/muse/*.json; do
  [ -f "$f" ] || continue
  chown dev:node "$f" 2>/dev/null; chmod 660 "$f" 2>/dev/null
done`

// livenessHygiene is gemini_hygiene: agy's top-level settings/onboarding and
// cache (a recursive sweep of antigravity-cli blew a 90 s exec), muse's
// shared trust store, and the per-agent codex sqlite homes. Directories
// 2770 (setgid keeps the node group), files 660.
const livenessHygiene = `g=/data/home/.gemini/antigravity-cli
if [ -d "$g" ]; then
  chown dev:node "$g" "$g"/*.json "$g/cache" "$g"/cache/* 2>/dev/null
  chmod 660 "$g"/*.json "$g"/cache/* 2>/dev/null
  chmod 2770 "$g" "$g/cache" 2>/dev/null
fi
for d in /data/home/.config/muse /data/home/.codex-*; do
  [ -d "$d" ] || continue
  chown -R dev:node "$d" 2>/dev/null
  find "$d" -type f ! -perm -660 -exec chmod 660 {} + 2>/dev/null
  find "$d" -type d ! -perm -2770 -exec chmod 2770 {} + 2>/dev/null
done
true`

func (b boundAPI) Pane(ctx context.Context, ns, agent string) (string, error) {
	var r struct {
		Lines []string `json:"lines"`
	}
	if err := b.c.GetJSON(ctx, ns, b.pod, b.token, "/api/pane/"+hiveclient.EscapePath(agent)+"?lines=60", &r); err != nil {
		return "", err
	}
	return strings.Join(r.Lines, "\n"), nil
}

func (b boundAPI) Governor(ctx context.Context, ns string) (string, error) {
	return b.c.Sh(ctx, ns, b.pod, `sed -n "/^governor:/,/^[a-z_]*:/p" /data/hive.yaml.runtime`)
}

func (b boundAPI) Restart(ctx context.Context, ns, agent string, fixPerms bool) (string, error) {
	if !fixPerms {
		return b.c.Post(ctx, ns, b.pod, b.token, "/api/restart/"+hiveclient.EscapePath(agent))
	}
	script := livenessPermsFix + "\n" + fmt.Sprintf(`curl -sS -X POST --max-time 150 -H %s %s 2>&1`,
		hiveclient.ShellQuote("X-Hive-Internal: "+b.token), hiveclient.ShellQuote(hiveclient.APIAddr+"/api/restart/"+hiveclient.EscapePath(agent)))
	return b.c.Sh(ctx, ns, b.pod, script)
}

func (b boundAPI) Hygiene(ctx context.Context, ns string) error {
	_, err := b.c.Sh(ctx, ns, b.pod, livenessHygiene)
	return err
}

// livenessInputs is what Reconcile observed for the liveness passes.
type livenessInputs struct {
	agents   []liveness.Agent
	nudge    []liveness.NudgeAgent
	ages     map[string]int64
	snapshot time.Time
	budget   map[string]any
	readings map[string]rotation.Reading
	sources  map[string]string
}

func livenessPolicy(sp *hivev1.HiveSpoke) (liveness.Policy, liveness.NudgePolicy, time.Duration, time.Duration) {
	wp, np := liveness.DefaultPolicy(), liveness.DefaultNudgePolicy()
	wiv, niv := 5*time.Minute, 30*time.Minute
	s := sp.Spec.Liveness
	if s == nil {
		return wp, np, wiv, niv
	}
	set := func(dst *int, v int32) {
		if v > 0 {
			*dst = int(v)
		}
	}
	set(&wp.MaxMutations, s.MaxMutations)
	set(&wp.BackoffBaseMin, s.BackoffBaseMinutes)
	set(&wp.BackoffMaxMin, s.BackoffMaxMinutes)
	set(&wp.StallMin, s.StallMinutes)
	if s.MaxSnapshotAgeSeconds > 0 {
		wp.MaxSnapshotAge = time.Duration(s.MaxSnapshotAgeSeconds) * time.Second
	}
	set(&np.Grace, s.NudgeGrace)
	set(&np.FloorS, s.NudgeFloorSeconds)
	set(&np.MaxPerNS, s.NudgeMaxPerSpoke)
	if s.WatchdogIntervalMinutes > 0 {
		wiv = time.Duration(s.WatchdogIntervalMinutes) * time.Minute
	}
	if s.NudgeIntervalMinutes > 0 {
		niv = time.Duration(s.NudgeIntervalMinutes) * time.Minute
	}
	return wp, np, wiv, niv
}

// livenessSlack lets a pass run a little early so a 2-minute reconcile
// cadence does not stretch a 5-minute interval to 6.
const livenessSlack = 30 * time.Second

func dueAt(last *metav1.Time, iv time.Duration) time.Time {
	if last == nil {
		return time.Time{}
	}
	return last.Add(iv - livenessSlack)
}

// livenessNextDue is when the next liveness pass is due, so Reconcile can
// requeue for it instead of waiting out its whole interval.
func livenessNextDue(sp *hivev1.HiveSpoke) time.Time {
	mode := sp.Spec.LivenessMode
	if mode == hivev1.ModeObserve || sp.Status.Liveness == nil {
		return time.Time{}
	}
	_, _, wiv, niv := livenessPolicy(sp)
	var next time.Time
	pick := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if sp.Spec.Liveness == nil || !sp.Spec.Liveness.DisableWatchdog {
		pick(dueAt(sp.Status.Liveness.WatchdogAt, wiv))
	}
	if sp.Spec.Liveness == nil || !sp.Spec.Liveness.DisableNudge {
		pick(dueAt(sp.Status.Liveness.NudgeAt, niv))
	}
	return next
}

func journalFromStatus(j *hivev1.LivenessJournal) liveness.Journal {
	out := liveness.Journal{Heals: map[string]liveness.Heal{}, Panes: map[string]liveness.PaneSeen{}, Resets: map[string]time.Time{}}
	if j == nil {
		return out
	}
	for _, h := range j.Heals {
		out.Heals[h.Agent] = liveness.Heal{Count: int(h.Count), Last: h.Last.Time}
	}
	for _, p := range j.Panes {
		out.Panes[p.Agent] = liveness.PaneSeen{Hash: p.Hash, Since: p.Since.Time}
	}
	for _, r := range j.Resets {
		out.Resets[r.Provider] = r.At.Time
	}
	if j.HygieneAt != nil {
		out.HygieneAt = j.HygieneAt.Time
	}
	return out
}

func journalToStatus(j liveness.Journal) *hivev1.LivenessJournal {
	out := &hivev1.LivenessJournal{}
	for _, a := range sortedKeys(j.Heals) {
		h := j.Heals[a]
		out.Heals = append(out.Heals, hivev1.HealRecord{Agent: a, Count: int32(h.Count), Last: metav1.NewTime(h.Last)})
	}
	for _, a := range sortedKeys(j.Panes) {
		p := j.Panes[a]
		out.Panes = append(out.Panes, hivev1.PaneRecord{Agent: a, Hash: p.Hash, Since: metav1.NewTime(p.Since)})
	}
	for _, p := range sortedKeys(j.Resets) {
		out.Resets = append(out.Resets, hivev1.ProviderReset{Provider: p, At: metav1.NewTime(j.Resets[p])})
	}
	if !j.HygieneAt.IsZero() {
		t := metav1.NewTime(j.HygieneAt)
		out.HygieneAt = &t
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// liveness runs the watchdog (every 5 min) and nudge (every 30 min) passes
// for one spoke under spec.livenessMode.
func (r *HiveSpokeReconciler) liveness(ctx context.Context, sp *hivev1.HiveSpoke, li livenessInputs, api LivenessAPI, now time.Time) {
	mode := sp.Spec.LivenessMode
	if mode == "" {
		mode = hivev1.ModeShadow
	}
	metrics.SetMode(spokeController+"-liveness", sp.Name, string(mode))
	if mode == hivev1.ModeObserve {
		if sp.Status.Liveness != nil {
			sp.Status.Liveness.WatchdogPlan, sp.Status.Liveness.WatchdogPlanText = nil, nil
			sp.Status.Liveness.NudgePlan, sp.Status.Liveness.NudgePlanText = nil, nil
		}
		return
	}
	if sp.Status.Liveness == nil {
		sp.Status.Liveness = &hivev1.LivenessStatus{}
	}
	st := sp.Status.Liveness
	wpol, npol, wiv, niv := livenessPolicy(sp)
	enforce := mode == hivev1.ModeEnforce && api != nil
	if mode == hivev1.ModeEnforce && api == nil {
		apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "LivenessEnforced", Status: metav1.ConditionFalse,
			Reason: "NoActuator", ObservedGeneration: sp.Generation,
			Message: "livenessMode is Enforce but the hive API is unreachable this reconcile; planned as Shadow"})
	}

	// A rotation apply in this very reconcile changed placements that
	// /api/status does not show yet: judging panes now would heal agents
	// mid-relaunch. Defer the watchdog to the next reconcile (bash never
	// chains a watchdog onto a rotate either).
	rotatedNow := sp.Status.RotationAppliedAt != nil && sp.Status.RotationAppliedAt.Time.Equal(now)
	disabled := sp.Spec.Liveness != nil && sp.Spec.Liveness.DisableWatchdog
	if !disabled && !rotatedNow && !now.Before(dueAt(st.WatchdogAt, wiv)) {
		r.watchdogPass(ctx, sp, mode, enforce, li, api, wpol, now)
	}
	if disabled {
		st.WatchdogPlan, st.WatchdogPlanText = nil, nil
	}
	if sp.Spec.Liveness != nil && sp.Spec.Liveness.DisableNudge {
		st.NudgePlan, st.NudgePlanText = nil, nil
	} else if !now.Before(dueAt(st.NudgeAt, niv)) {
		r.nudgePass(ctx, sp, enforce, li, api, npol, now)
	}
}

func (r *HiveSpokeReconciler) watchdogPass(ctx context.Context, sp *hivev1.HiveSpoke, mode hivev1.ReconcileMode, enforce bool,
	li livenessInputs, api LivenessAPI, pol liveness.Policy, now time.Time) {
	st := sp.Status.Liveness
	ladderName := sp.Spec.LadderRef
	if ladderName == "" {
		ladderName = "fleet"
	}
	var rungs []rotation.Rung
	var ladder hivev1.ModelLadder
	if err := r.Get(ctx, client.ObjectKey{Name: ladderName}, &ladder); err == nil {
		rungs = ladderRungs(ladder.Status.Effective)
	}
	rotMode := sp.Spec.RotationMode
	if rotMode == "" {
		rotMode = hivev1.ModeShadow
	}
	rin := buildRotationInput(sp, li.readings, li.sources, rungs, now, rotMode)

	in := liveness.Input{Now: now, Snapshot: li.snapshot, Agents: li.agents, LivePane: map[string]string{},
		Pins: map[string]bool{}, Rotation: rin, Journal: journalFromStatus(st.Journal), Policy: pol}
	for _, p := range sp.Spec.Pins {
		in.Pins[p.Agent] = true
	}
	// Re-read the live pane (GET, every mode) for the agents that look
	// unhealthy or are mid-turn: /api/status lags it by minutes.
	if api != nil {
		for _, a := range li.agents {
			if liveness.NeedsLivePane(a) {
				if pane, err := api.Pane(ctx, sp.Spec.Namespace, a.Name); err == nil {
					in.LivePane[a.Name] = pane
				}
			}
		}
	}
	plan := liveness.Watchdog(in)
	journal := plan.Journal

	outcomes := liveness.ShadowOutcomes(plan.Actions)
	statuses := make([]hivev1.LivenessAction, len(plan.Actions))
	start := time.Now()
	rotationEnforced := rotMode == hivev1.ModeEnforce
	for i, a := range plan.Actions {
		la := hivev1.LivenessAction{Agent: a.Agent, Kind: a.Kind, State: a.State, ToBackend: a.To.Backend, ToModel: a.To.Model,
			Effort: a.Effort, HealCount: int32(a.HealCount), Reason: a.Reason}
		if a.Kind == liveness.KindRotateOff && !rotationEnforced {
			// Placement is rotation's: while this spoke's rotation is not
			// Enforce, the rotate-off is reported and the agent is restarted
			// in place (bash's path when the placement does not happen).
			la.Error = "reported only: rotationMode is " + string(rotMode)
		}
		if a.Kind == liveness.KindWake && !rotationEnforced {
			la.Error = "reported only: rotationMode is " + string(rotMode) + " (the bash rotate re-decides on its own tick)"
		}
		if enforce {
			out, applied, err := r.applyLiveness(ctx, sp, a, api, rotationEnforced, start, pol)
			outcomes[i] = out
			la.Applied = applied
			if err != nil {
				la.Error = err.Error()
			}
			// A heal deferred by the time budget did not happen: bash
			// records nothing for it.
			if errorsIsDeferred(err) && (a.Kind == liveness.KindRestart || a.Kind == liveness.KindRotateOff) {
				if prev, ok := in.Journal.Heals[a.Agent]; ok {
					journal.Heals[a.Agent] = prev
				} else {
					delete(journal.Heals, a.Agent)
				}
			}
			// A rotate-off that fell back to a restart clears the stall clock.
			if a.Kind == liveness.KindRotateOff && !applied {
				delete(journal.Panes, a.Agent)
			}
		} else if a.Kind == liveness.KindRotateOff && !rotationEnforced {
			outcomes[i] = []string{liveness.RestartLine(a.Agent, a.State, "would restart")}
			delete(journal.Panes, a.Agent)
		}
		statuses[i] = la
		metrics.Action(spokeController+"-liveness", sp.Name, a.Kind, la.Applied)
	}
	t := metav1.NewTime(now)
	st.WatchdogAt = &t
	st.WatchdogPlan = nil
	for _, s := range statuses {
		if s.Kind == liveness.KindHygiene && !s.Applied && s.Error == "" && s.Agent == "" {
			continue // hourly hygiene is housekeeping, not a plan line
		}
		st.WatchdogPlan = append(st.WatchdogPlan, s)
	}
	st.WatchdogPlanText = liveness.Render(plan.Lines, plan.Actions, outcomes)
	st.Journal = journalToStatus(journal)

	reason, status := "Planned", metav1.ConditionTrue
	msg := fmt.Sprintf("%d agent(s) healed (%s)", plan.Healed, mode)
	if len(plan.Human) > 0 {
		msg += "; needs a human login: " + strings.Join(plan.Human, " ")
	}
	if plan.Woke {
		msg = "renewal wake-up: " + plan.Lines[0]
	}
	apiMeta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "WatchdogPassed", Status: status, Reason: reason,
		Message: msg, ObservedGeneration: sp.Generation})
}

type deferredErr struct{}

func (deferredErr) Error() string { return "deferred to the next pass (time budget)" }

func errorsIsDeferred(err error) bool {
	_, ok := err.(deferredErr)
	return ok
}

// livenessTimeBudget: start no restart-causing call after this much of a
// pass (HIVE_WATCHDOG_BUDGET_S): each restarts an agent inside the request.
const livenessTimeBudget = 170 * time.Second

// applyLiveness performs one watchdog action (Enforce) and returns bash's
// outcome lines for it.
func (r *HiveSpokeReconciler) applyLiveness(ctx context.Context, sp *hivev1.HiveSpoke, a liveness.Action, api LivenessAPI,
	rotationEnforced bool, start time.Time, pol liveness.Policy) ([]string, bool, error) {
	ns := sp.Spec.Namespace
	if a.Mutating() && time.Since(start) >= livenessTimeBudget {
		return []string{fmt.Sprintf("%-14s %-8s heal deferred to the next pass (budget)", a.Agent, a.State)}, false, deferredErr{}
	}
	act := &rotation.HiveActuator{API: api}
	restart := func() ([]string, bool, error) {
		body, err := api.Restart(ctx, ns, a.Agent, a.CLI == "agy" || a.CLI == "muse")
		rs := respStatus(body)
		if err != nil && rs == "" {
			rs = err.Error()
		}
		ok := err == nil && rs == "restarted"
		var e error
		if !ok {
			e = fmt.Errorf("restart: %s", rs)
		}
		return []string{liveness.RestartLine(a.Agent, a.State, rs)}, ok, e
	}
	switch a.Kind {
	case liveness.KindHygiene:
		if err := api.Hygiene(ctx, ns); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	case liveness.KindWake:
		if !rotationEnforced {
			return nil, false, nil
		}
		// Re-derive placement from scratch: make rotation due now. The next
		// reconcile (requeued shortly) applies it with fresh readings.
		sp.Status.RotationAppliedAt = nil
		return nil, true, nil
	case liveness.KindRepair:
		if err := act.Place(ctx, ns, a.Agent, a.To.Backend, a.To.Model); err != nil {
			return []string{"    ! placement failed: " + trunc(err.Error(), 200), "    ! repair failed"}, false, err
		}
		return nil, true, nil
	case liveness.KindEffort:
		body, err := api.Post(ctx, ns, "/api/effort/"+hiveclient.EscapePath(a.Agent)+"/"+a.Effort)
		rs := respStatus(body)
		if err == nil && rs == "effort_set" {
			return nil, true, nil
		}
		if rs == "" && err != nil {
			rs = err.Error()
		}
		return []string{"    ! effort set failed: " + rs}, false, fmt.Errorf("effort set: %s", rs)
	case liveness.KindRotateOff:
		if rotationEnforced {
			if err := act.Place(ctx, ns, a.Agent, a.To.Backend, a.To.Model); err == nil {
				return nil, true, nil
			} else {
				out, _, rerr := restart()
				return append([]string{"    ! placement failed: " + trunc(err.Error(), 200)}, out...), false,
					fmt.Errorf("rotate-off failed (%v); restarted in place: %v", err, rerr)
			}
		}
		out, _, err := restart()
		return out, false, err
	case liveness.KindRestart:
		return restart()
	}
	return nil, false, nil
}

func respStatus(body string) string {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return strings.TrimSpace(trunc(body, 200))
	}
	if s, ok := m["status"].(string); ok && s != "" {
		return s
	}
	if s, ok := m["error"].(string); ok {
		return s
	}
	return ""
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (r *HiveSpokeReconciler) nudgePass(ctx context.Context, sp *hivev1.HiveSpoke, enforce bool, li livenessInputs,
	api LivenessAPI, pol liveness.NudgePolicy, now time.Time) {
	st := sp.Status.Liveness
	in := liveness.NudgeInput{Namespace: sp.Spec.Namespace, Agents: li.nudge, Idle: li.ages, Policy: pol,
		BudgetExhausted: sp.Status.BudgetExhausted}
	in.BudgetPctUsed, _ = numFrom(li.budget, "BUDGET_PCT_USED")
	in.BudgetWeekly, _ = numFrom(li.budget, "BUDGET_WEEKLY")
	if api == nil {
		in.CadencesErr = true
	} else if gov, err := api.Governor(ctx, sp.Spec.Namespace); err != nil {
		in.CadencesErr = true
	} else {
		in.Cadences = liveness.LongestCadences(gov)
	}
	plan := liveness.Nudge(in)
	var results []liveness.KickResult
	if enforce {
		done := 0
		for range plan.Candidates {
			if done >= pol.MaxPerNS {
				break
			}
			i := len(results)
			kctx, cancel := context.WithTimeout(ctx, 40*time.Second)
			body, err := api.Post(kctx, sp.Spec.Namespace, "/api/kick/"+hiveclient.EscapePath(plan.Candidates[i].Agent))
			cancel()
			var res liveness.KickResult
			switch {
			case err != nil:
				res = liveness.KickResult{Outcome: liveness.KickUnanswered, Detail: err.Error()}
			case strings.Contains(body, `"ok":true`):
				s := respStatus(body)
				if s == "" {
					s = "ok"
				}
				res = liveness.KickResult{Outcome: liveness.KickOK, Detail: s}
				done++
			default:
				res = liveness.KickResult{Outcome: liveness.KickFailed, Detail: respStatus(body)}
			}
			results = append(results, res)
		}
	} else {
		results = liveness.ShadowKicks(plan, pol.MaxPerNS)
	}
	st.NudgePlan = nil
	for i, res := range results {
		c := plan.Candidates[i]
		la := hivev1.LivenessAction{Agent: c.Agent, Kind: liveness.KindKick,
			Reason:  fmt.Sprintf("idle %dm > %dm (longest cadence %dm x%d)", c.IdleS/60, c.ThreshS/60, c.Longest/60, pol.Grace),
			Applied: res.Outcome == liveness.KickOK}
		if res.Outcome == liveness.KickUnanswered || res.Outcome == liveness.KickFailed {
			la.Error = res.Outcome + ": " + res.Detail
		}
		st.NudgePlan = append(st.NudgePlan, la)
		metrics.Action(spokeController+"-nudge", sp.Name, liveness.KindKick, la.Applied)
	}
	st.NudgePlanText = liveness.NudgeLines(plan, results, 30)
	if plan.Skip == "" {
		st.NudgePlanText = append(st.NudgePlanText, liveness.NudgeFooter(results))
	}
	t := metav1.NewTime(now)
	st.NudgeAt = &t
}
