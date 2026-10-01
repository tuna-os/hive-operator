// Command hive-usage is the per-hive-pod usage sidecar.
//
// It runs ccusage over the agents' session logs on a schedule, folds the
// cumulative session totals into a ledger (internal/usage), and serves:
//
//	GET /v1/usage[?since=RFC3339...]   JSON usage.Report (default windows 1h/5h/24h/7d)
//	GET /metrics                        Prometheus
//	GET /healthz                        liveness (the HTTP server is up)
//
// It is read-only against the hive: it mounts the data volumes readOnly and
// writes only to its own emptyDir (symlink views + ledger snapshot). It needs
// no credentials and makes no network calls (ccusage runs --offline).
//
// It deliberately has NO readiness probe dependency on the hive container and
// is placed after it in the pod spec: hive-upgrade patches container "hive"
// by name with a strategic merge, and judges health from that container's
// status, so a second container must never be the reason a rollout stalls.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tuna-os/hive-operator/internal/usage"
)

type server struct {
	col       *usage.Collector
	ledger    *usage.Ledger
	sources   []usage.Source
	statePath string
	pod, ns   string
	log       *slog.Logger

	mu     sync.Mutex
	status map[string]*usage.SourceStatus

	tokens, cost             *prometheus.CounterVec
	windowCost, windowTokens *prometheus.GaugeVec
	up, dur, lastOK, unprice *prometheus.GaugeVec
}

func main() {
	var (
		home, bin, scratch, statePath, listen, srcList, cfg string
		interval, slowInterval, retention, timeout          time.Duration
		once                                                bool
	)
	flag.StringVar(&home, "home", "/data/home", "Agents' home directory (NOT $HOME: in a hive pod that is /root).")
	flag.StringVar(&bin, "ccusage", "/usr/local/bin/ccusage", "ccusage binary.")
	flag.StringVar(&scratch, "scratch", "/var/lib/hive-usage/tmp", "Writable scratch dir for ccusage cache/tmp.")
	flag.StringVar(&statePath, "state", "/var/lib/hive-usage/ledger.json", "Ledger snapshot path (empty: none).")
	flag.StringVar(&listen, "listen", ":9464", "HTTP listen address.")
	flag.StringVar(&srcList, "sources", "claude,codex,antigravity,gemini,pi,goose", "ccusage sources to collect.")
	flag.StringVar(&cfg, "ccusage-config", "", "Optional ccusage.json (e.g. pricingOverrides for unpriced models).")
	flag.DurationVar(&interval, "interval", 5*time.Minute, "Collection interval.")
	flag.DurationVar(&slowInterval, "slow-interval", 15*time.Minute, "Interval for expensive sources (antigravity).")
	flag.DurationVar(&retention, "retention", 8*24*time.Hour, "Ledger history kept.")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "Per-ccusage-run timeout.")
	flag.BoolVar(&once, "once", false, "Collect every source once, print the report, exit.")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	want := map[string]bool{}
	for _, s := range strings.Split(srcList, ",") {
		want[strings.TrimSpace(s)] = true
	}
	var sources []usage.Source
	for _, s := range usage.DefaultSources() {
		if want[s.Name] {
			sources = append(sources, s)
		}
	}

	s := &server{
		col:    &usage.Collector{Binary: bin, Home: home, Scratch: scratch, Timeout: timeout, Config: cfg},
		ledger: usage.NewLedger(retention), sources: sources, statePath: statePath,
		pod: os.Getenv("POD_NAME"), ns: os.Getenv("POD_NAMESPACE"), log: log,
		status: map[string]*usage.SourceStatus{},
	}
	s.initMetrics()
	s.restore()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if once {
		for _, src := range sources {
			s.collect(ctx, src)
		}
		_ = json.NewEncoder(os.Stdout).Encode(s.report(nil))
		return
	}

	mux := http.NewServeMux()
	reg := prometheus.NewRegistry()
	reg.MustRegister(s.tokens, s.cost, s.windowCost, s.windowTokens, s.up, s.dur, s.lastOK, s.unprice)
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		since, err := usage.ParseSince(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.report(since))
	})
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()
	go s.loop(ctx, interval, slowInterval)
	log.Info("hive-usage listening", "addr", listen, "home", home, "sources", srcList)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}

// loop collects sources sequentially — ccusage is CPU-bound and the sidecar
// shares a node with the agents, so never run two at once.
func (s *server) loop(ctx context.Context, interval, slow time.Duration) {
	next := map[string]time.Time{}
	for {
		now := time.Now()
		for _, src := range s.sources {
			if now.Before(next[src.Name]) {
				continue
			}
			s.collect(ctx, src)
			iv := interval
			if src.Name == "antigravity" {
				iv = slow
			}
			next[src.Name] = time.Now().Add(iv)
		}
		s.refreshWindowGauges()
		s.persist()
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func (s *server) collect(ctx context.Context, src usage.Source) {
	now := time.Now().UTC()
	_ = os.MkdirAll(s.col.Scratch, 0o755)
	run := s.col.Collect(ctx, src)
	st := &usage.SourceStatus{Source: src.Name, Fingerprint: run.Fingerprint, LastRun: now,
		DurationSeconds: run.Duration.Seconds(), Roots: run.Roots, Sessions: len(run.Report.Sessions),
		UnpricedModels: run.UnpricedModels}
	s.mu.Lock()
	if prev := s.status[src.Name]; prev != nil {
		st.LastSuccess = prev.LastSuccess
	}
	s.mu.Unlock()
	s.dur.WithLabelValues(src.Name).Set(run.Duration.Seconds())
	if run.Err != nil {
		st.Error = run.Err.Error()
		s.up.WithLabelValues(src.Name).Set(0)
		s.log.Warn("collect failed", "source", src.Name, "err", run.Err)
	} else {
		added := s.ledger.Observe(src.Name, run.Report, run.Attribute, now)
		for _, e := range added {
			l := []string{e.Source, e.Provider, e.Agent, e.Model}
			s.tokens.WithLabelValues(append(l, "input")...).Add(float64(e.Tokens.Input))
			s.tokens.WithLabelValues(append(l, "output")...).Add(float64(e.Tokens.Output))
			s.tokens.WithLabelValues(append(l, "cache_read")...).Add(float64(e.Tokens.CacheRead))
			s.tokens.WithLabelValues(append(l, "cache_create")...).Add(float64(e.Tokens.CacheCreate))
			s.cost.WithLabelValues(l...).Add(e.Tokens.CostUSD)
		}
		st.OK, st.LastSuccess = true, now
		s.up.WithLabelValues(src.Name).Set(1)
		s.lastOK.WithLabelValues(src.Name).Set(float64(now.Unix()))
		for _, m := range run.UnpricedModels {
			s.unprice.WithLabelValues(src.Name, m).Set(1)
		}
		s.log.Info("collected", "source", src.Name, "sessions", st.Sessions, "roots", st.Roots,
			"entries", len(added), "seconds", run.Duration.Seconds())
	}
	st.Primed = s.ledger.Primed(src.Name)
	s.mu.Lock()
	s.status[src.Name] = st
	s.mu.Unlock()
}

func (s *server) report(since []time.Time) usage.Report {
	now := time.Now().UTC()
	r := usage.Report{Schema: usage.SchemaVersion, Pod: s.pod, Namespace: s.ns, GeneratedAt: now}
	s.mu.Lock()
	for _, src := range s.sources {
		if st := s.status[src.Name]; st != nil {
			r.Sources = append(r.Sources, *st)
		}
	}
	s.mu.Unlock()
	if len(since) == 0 {
		for _, w := range usage.DefaultWindows {
			t := now.Add(-w.D).Truncate(time.Second)
			r.Windows = append(r.Windows, usage.WindowUsage{Label: w.Label, Since: t, Rows: usage.Rows(s.ledger.Since(t))})
		}
		return r
	}
	for _, t := range since {
		r.Windows = append(r.Windows, usage.WindowUsage{Since: t, Rows: usage.Rows(s.ledger.Since(t))})
	}
	return r
}

func (s *server) refreshWindowGauges() {
	now := time.Now()
	s.windowCost.Reset()
	s.windowTokens.Reset()
	for _, w := range usage.DefaultWindows {
		for k, t := range s.ledger.Since(now.Add(-w.D)) {
			s.windowCost.WithLabelValues(k.Source, k.Provider, k.Agent, w.Label).Add(t.CostUSD)
			s.windowTokens.WithLabelValues(k.Source, k.Provider, k.Agent, w.Label).Add(float64(t.Total))
		}
	}
}

func (s *server) persist() {
	if s.statePath == "" {
		return
	}
	b, err := json.Marshal(s.ledger)
	if err != nil {
		return
	}
	tmp := s.statePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, s.statePath)
	}
}

func (s *server) restore() {
	if s.statePath == "" {
		return
	}
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, s.ledger); err != nil {
		s.log.Warn("ignoring unreadable ledger snapshot", "err", err)
	}
}

func (s *server) initMetrics() {
	labels := []string{"source", "provider", "agent", "model"}
	s.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_usage_tokens_total",
		Help: "Tokens consumed, from agent session logs via ccusage. Back-filled on first collection.",
	}, append(labels, "kind"))
	s.cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_usage_cost_usd_total",
		Help: "API-equivalent USD consumed (ccusage pricing). Unpriced models count 0 — see hive_usage_unpriced_model.",
	}, labels)
	wl := []string{"source", "provider", "agent", "window"}
	s.windowCost = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_window_cost_usd", Help: "API-equivalent USD consumed in the trailing window.",
	}, wl)
	s.windowTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_window_tokens", Help: "Tokens consumed in the trailing window.",
	}, wl)
	s.up = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_collect_success", Help: "1 if the last ccusage run for this source succeeded.",
	}, []string{"source"})
	s.dur = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_collect_duration_seconds", Help: "Wall time of the last ccusage run.",
	}, []string{"source"})
	s.lastOK = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_last_success_timestamp_seconds", Help: "Unix time of the last successful collection.",
	}, []string{"source"})
	s.unprice = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_usage_unpriced_model", Help: "1 for a model ccusage could not price (its cost reads as 0).",
	}, []string{"source", "model"})
}
