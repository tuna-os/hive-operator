package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/metrics"
	"github.com/tuna-os/hive-operator/internal/usage"
)

const poolController = "usagepool"

// UsagePoolReconciler measures provider pools from the hive-usage sidecars.
//
// Observe-only by construction: it reads sidecars and a ConfigMap and writes
// only its own status and metrics. Rotation consumes the result (HiveSpoke
// spec.rotationUsageSource=UsagePool); a pool itself never acts.
type UsagePoolReconciler struct {
	client.Client
	Fetch    usage.Fetcher
	Interval time.Duration
	Now      func() time.Time
}

// +kubebuilder:rbac:groups=hive.tunaos.org,resources=usagepools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=usagepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// defaultSources maps a provider to the ccusage sources that can spend it.
func defaultSources(provider string) []string {
	switch provider {
	case "anthropic":
		return []string{"claude"}
	case "openai":
		return []string{"codex"}
	case "google":
		return []string{"antigravity", "gemini"}
	case "deepseek":
		return []string{"pi", "goose"}
	}
	return nil
}

func fmtNum(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func parseNum(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func metaTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	m := metav1.NewTime(t)
	return &m
}

// Reconcile implements the controller.
func (r *UsagePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool hivev1.UsagePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	iv := r.Interval
	if iv == 0 {
		iv = 2 * time.Minute
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	mode := pool.Spec.Mode
	if mode == "" {
		mode = hivev1.ModeObserve
	}
	metrics.SetMode(poolController, pool.Name, string(mode))

	if err := r.evaluate(ctx, &pool, now); err != nil {
		metrics.ReconcileErrors.WithLabelValues(poolController, pool.Name).Inc()
		apiMeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse,
			Reason: "Error", Message: err.Error(), ObservedGeneration: pool.Generation})
	}
	pool.Status.ObservedAt = metaTime(now)
	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: iv}, nil
}

func (r *UsagePoolReconciler) namespaces(ctx context.Context, pool *hivev1.UsagePool) ([]string, error) {
	if len(pool.Spec.Namespaces) > 0 {
		return append([]string(nil), pool.Spec.Namespaces...), nil
	}
	var spokes hivev1.HiveSpokeList
	if err := r.List(ctx, &spokes); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range spokes.Items {
		out = append(out, s.Spec.Namespace)
	}
	sort.Strings(out)
	return out, nil
}

// reading returns the provider reading for one window, or an unknown one when
// there is no reading or it is stale.
func (r *UsagePoolReconciler) readings(ctx context.Context, pool *hivev1.UsagePool, now time.Time) (usage.ProbeReading, time.Time, string) {
	ref := pool.Spec.Reading
	if ref == nil {
		return usage.ProbeReading{Percent: -1}, time.Time{}, "no reading configured"
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &cm); err != nil {
		return usage.ProbeReading{Percent: -1}, time.Time{}, "reading: " + err.Error()
	}
	all, at := usage.ParseProbeConfigMap(cm.Data)
	key := ref.Key
	if key == "" {
		key = pool.Spec.Provider
	}
	maxAge := 30 * time.Minute
	if ref.MaxAge != nil {
		maxAge = ref.MaxAge.Duration
	}
	if at.IsZero() || now.Sub(at) > maxAge {
		return usage.ProbeReading{Percent: -1}, at, fmt.Sprintf("reading stale (updated_at %s)", at.Format(time.RFC3339))
	}
	pr, ok := all[key]
	if !ok {
		return usage.ProbeReading{Percent: -1}, at, "reading has no key " + key
	}
	return pr, at, ""
}

type windowPlan struct {
	spec    hivev1.UsageWindowSpec
	reading usage.Reading
	start   time.Time
	resets  time.Time
}

func (r *UsagePoolReconciler) evaluate(ctx context.Context, pool *hivev1.UsagePool, now time.Time) error {
	unit := usage.Unit(pool.Spec.Unit)
	if unit == "" {
		unit = usage.UnitCostUSD
	}
	sources := pool.Spec.Sources
	if len(sources) == 0 {
		sources = defaultSources(pool.Spec.Provider)
	}
	want := map[string]bool{}
	for _, s := range sources {
		want[s] = true
	}
	nss, err := r.namespaces(ctx, pool)
	if err != nil {
		return err
	}
	probe, readingAt, readingNote := r.readings(ctx, pool, now)

	// Place every window, then ask each sidecar for all starts in one call.
	plans := make([]windowPlan, 0, len(pool.Spec.Windows))
	recentStart := now.Add(-time.Hour).Truncate(time.Second)
	since := []time.Time{recentStart}
	for _, w := range pool.Spec.Windows {
		rd := probe.Reading(w.ReadingSlot, readingAt)
		start, resets := usage.WindowStart(now, w.Duration.Duration, rd)
		start = start.Truncate(time.Second)
		plans = append(plans, windowPlan{spec: w, reading: rd, start: start, resets: resets})
		since = append(since, start)
	}

	// Fetch, de-duplicating shared stores by fingerprint: the fleet's
	// .claude/.gemini/.codex are one PVC mounted by every spoke, and counting
	// each spoke's view of it would triple the pool.
	type counted struct {
		rep *usage.Report
		ok  map[string]bool // source → counted from this namespace
	}
	seenStore := map[string]string{} // source\x00fingerprint → namespace
	var got []counted
	pool.Status.Sources = nil
	var fetchErrs []string
	for _, ns := range nss {
		rep, err := r.Fetch.Fetch(ctx, ns, pool.Spec.SidecarPort, since)
		if err != nil {
			fetchErrs = append(fetchErrs, ns+": "+err.Error())
			for s := range want {
				pool.Status.Sources = append(pool.Status.Sources, hivev1.UsageSourceStatus{Namespace: ns, Source: s, Error: err.Error()})
				metrics.UsageSourceUp.WithLabelValues(pool.Name, ns, s, "false").Set(0)
			}
			continue
		}
		c := counted{rep: rep, ok: map[string]bool{}}
		for _, st := range rep.Sources {
			if !want[st.Source] {
				continue
			}
			ss := hivev1.UsageSourceStatus{Namespace: ns, Source: st.Source, Fingerprint: st.Fingerprint,
				OK: st.OK, Primed: st.Primed, Error: st.Error, LastSuccess: metaTime(st.LastSuccess),
				UnpricedModels: st.UnpricedModels}
			key := st.Source + "\x00" + st.Fingerprint
			if st.OK {
				if prev, dup := seenStore[key]; dup && st.Fingerprint != "" {
					ss.Counted = false
					ss.Error = "same store as " + prev
				} else {
					seenStore[key] = ns
					ss.Counted = true
					c.ok[st.Source] = true
				}
			}
			pool.Status.Sources = append(pool.Status.Sources, ss)
			metrics.UsageSourceUp.WithLabelValues(pool.Name, ns, st.Source, strconv.FormatBool(ss.Counted)).Set(b2f(st.OK))
		}
		got = append(got, c)
	}

	sum := func(start time.Time) (float64, map[string]float64, map[string]map[string]float64) {
		total := 0.0
		byAgent := map[string]float64{}
		byAgentModel := map[string]map[string]float64{}
		for _, c := range got {
			w := c.rep.Window(start)
			if w == nil {
				continue
			}
			for _, row := range w.Rows {
				if !c.ok[row.Source] || row.Provider != pool.Spec.Provider {
					continue
				}
				v := unit.Value(row.Tokens)
				total += v
				byAgent[row.Agent] += v
				if byAgentModel[row.Agent] == nil {
					byAgentModel[row.Agent] = map[string]float64{}
				}
				byAgentModel[row.Agent][row.Model] += v
			}
		}
		return total, byAgent, byAgentModel
	}

	recent, _, _ := sum(recentStart)
	prev := map[string]hivev1.UsageWindowStatus{}
	for _, w := range pool.Status.Windows {
		prev[w.Name] = w
	}
	pool.Status.Windows = nil
	pool.Status.Agents = nil
	metrics.AgentUsage.DeletePartialMatch(map[string]string{"pool": pool.Name})
	for _, p := range plans {
		consumed, byAgent, byAgentModel := sum(p.start)
		limit := 0.0
		if p.spec.Limit != "" {
			limit = parseNum(p.spec.Limit)
		}
		res := usage.Evaluate(usage.WindowInput{
			Now: now, Duration: p.spec.Duration.Duration, Consumed: consumed,
			RecentConsumed: recent, RecentSpan: time.Hour,
			ConfiguredLimit: limit, PreviousLearned: parseNum(prev[p.spec.Name].Learned),
			Reading: p.reading,
		})
		used := usage.UsedPercent(p.reading, res)
		ws := hivev1.UsageWindowStatus{
			Name: p.spec.Name, Start: metaTime(res.Start), ResetsAt: metaTime(res.ResetsAt),
			Consumed: fmtNum(consumed), LimitSource: res.LimitSource,
			UsedPercent: fmtNum(used), ReadingPercent: fmtNum(p.reading.Percent),
			BurnPerHour: fmtNum(res.BurnPerHour), ExhaustionETA: metaTime(res.ExhaustionETA),
		}
		if !p.reading.Known() {
			ws.ReadingPercent = "-1"
		}
		if res.Limit > 0 {
			ws.Limit, ws.Remaining = fmtNum(res.Limit), fmtNum(res.Remaining)
		}
		if res.Learned > 0 {
			ws.Learned = fmtNum(res.Learned)
		}
		pool.Status.Windows = append(pool.Status.Windows, ws)

		labels := []string{pool.Name, pool.Spec.Provider, p.spec.Name}
		metrics.UsageRatio.WithLabelValues(labels...).Set(res.Ratio)
		src := "ccusage"
		if p.reading.Known() {
			src = "reading"
		}
		metrics.UsedPercentPool.DeletePartialMatch(map[string]string{"pool": pool.Name, "window": p.spec.Name})
		metrics.UsedPercentPool.WithLabelValues(pool.Name, pool.Spec.Provider, p.spec.Name, src).Set(used)
		metrics.ReadingPercent.WithLabelValues(labels...).Set(p.reading.Percent)
		metrics.Consumed.WithLabelValues(pool.Name, pool.Spec.Provider, p.spec.Name, string(unit)).Set(consumed)
		metrics.Remaining.WithLabelValues(pool.Name, pool.Spec.Provider, p.spec.Name, string(unit)).Set(res.Remaining)
		metrics.BurnPerHour.WithLabelValues(pool.Name, pool.Spec.Provider, p.spec.Name, string(unit)).Set(res.BurnPerHour)
		metrics.Limit.DeletePartialMatch(map[string]string{"pool": pool.Name, "window": p.spec.Name})
		if res.Limit > 0 {
			metrics.Limit.WithLabelValues(pool.Name, pool.Spec.Provider, p.spec.Name, string(unit), res.LimitSource).Set(res.Limit)
		}
		eta := -1.0
		if !res.ExhaustionETA.IsZero() {
			eta = res.ExhaustionETA.Sub(now).Seconds()
		}
		metrics.ExhaustionETA.WithLabelValues(labels...).Set(eta)

		agents := make([]string, 0, len(byAgent))
		for a := range byAgent {
			agents = append(agents, a)
		}
		sort.Slice(agents, func(i, j int) bool { return byAgent[agents[i]] > byAgent[agents[j]] })
		for _, a := range agents {
			share := 0.0
			if consumed > 0 {
				share = byAgent[a] / consumed
			}
			models := make([]string, 0, len(byAgentModel[a]))
			for m := range byAgentModel[a] {
				models = append(models, m)
			}
			bm := byAgentModel[a]
			sort.Slice(models, func(i, j int) bool { return bm[models[i]] > bm[models[j]] })
			pool.Status.Agents = append(pool.Status.Agents, hivev1.AgentUsage{Agent: a, Window: p.spec.Name,
				Consumed: fmtNum(byAgent[a]), Share: fmtNum(share), Models: models})
			metrics.AgentUsage.WithLabelValues(pool.Name, pool.Spec.Provider, a, p.spec.Name, string(unit)).Set(byAgent[a])
		}
	}

	status, reason, msg := metav1.ConditionTrue, "Measured", fmt.Sprintf("%d window(s) from %d namespace(s)", len(plans), len(got))
	switch {
	case len(got) == 0:
		status, reason, msg = metav1.ConditionFalse, "NoSidecar", fmt.Sprintf("no hive-usage sidecar answered: %v", fetchErrs)
	case len(fetchErrs) > 0:
		reason, msg = "Partial", fmt.Sprintf("%s; unreachable: %v", msg, fetchErrs)
	}
	apiMeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason,
		Message: msg, ObservedGeneration: pool.Generation})
	rs, rr, rm := metav1.ConditionTrue, "Fresh", "provider reading is fresh"
	if readingNote != "" {
		rs, rr, rm = metav1.ConditionFalse, "Unavailable", readingNote
	}
	apiMeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: "Reading", Status: rs, Reason: rr,
		Message: rm, ObservedGeneration: pool.Generation})
	var unpriced []string
	for _, s := range pool.Status.Sources {
		if s.Counted {
			unpriced = append(unpriced, s.UnpricedModels...)
		}
	}
	if len(unpriced) > 0 && unit == usage.UnitCostUSD {
		apiMeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: "Priced", Status: metav1.ConditionFalse,
			Reason: "UnpricedModels", ObservedGeneration: pool.Generation,
			Message: fmt.Sprintf("ccusage cannot price %v; their cost counts as 0 — add pricingOverrides to the sidecar's ccusage.json", unpriced)})
	} else {
		apiMeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: "Priced", Status: metav1.ConditionTrue,
			Reason: "AllPriced", Message: "every model consumed is priced", ObservedGeneration: pool.Generation})
	}
	return nil
}

// SetupWithManager wires the controller.
func (r *UsagePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&hivev1.UsagePool{}).Complete(r)
}
