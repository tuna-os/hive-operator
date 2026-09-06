// Command manager runs the hive fleet operator.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/controller"
	"github.com/tuna-os/hive-operator/internal/dashboard"
	"github.com/tuna-os/hive-operator/internal/metrics"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(hivev1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr, dashAddr string
	var spokeInterval, authInterval time.Duration
	var leaderElect bool
	var leaderNS string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Prometheus /metrics address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe address.")
	flag.StringVar(&dashAddr, "dashboard-bind-address", ":8082", "Fleet dashboard address. Empty disables it.")
	flag.DurationVar(&spokeInterval, "spoke-interval", 2*time.Minute, "How often to re-observe each spoke.")
	flag.DurationVar(&authInterval, "sharedauth-interval", 30*time.Minute, "How often to verify shared credentials.")
	// Out-of-cluster there is no serviceaccount namespace to infer, so `make
	// run` fails at startup unless leader election is disabled or given one.
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election. Disable for local runs.")
	flag.StringVar(&leaderNS, "leader-election-namespace", "", "Namespace holding the leader lease. Required when running outside the cluster.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	metrics.Register()

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "hive-operator.tunaos.org",
		LeaderElectionNamespace: leaderNS,
	})
	if err != nil {
		setupLog.Error(err, "unable to build manager")
		os.Exit(1)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to build clientset")
		os.Exit(1)
	}

	if err := (&controller.HiveSpokeReconciler{Client: mgr.GetClient(), Interval: spokeInterval}).
		SetupWithManager(mgr, cs, cfg); err != nil {
		setupLog.Error(err, "unable to set up HiveSpoke controller")
		os.Exit(1)
	}
	if err := (&controller.SharedAuthReconciler{Client: mgr.GetClient(), Interval: authInterval}).
		SetupWithManager(mgr, cs, cfg); err != nil {
		setupLog.Error(err, "unable to set up SharedAuth controller")
		os.Exit(1)
	}

	if dashAddr != "" {
		srv := &dashboard.Server{Client: mgr.GetClient()}
		// Runnable so it starts only after the cache is warm — otherwise the
		// first page load races an empty cache and renders an empty fleet.
		if err := mgr.Add(&httpRunnable{addr: dashAddr, h: srv.Handler(), log: setupLog}); err != nil {
			setupLog.Error(err, "unable to add dashboard")
			os.Exit(1)
		}
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("starting", "metrics", metricsAddr, "dashboard", dashAddr)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

// httpRunnable serves the dashboard as a manager Runnable so it starts only
// once the cache is warm. Started directly, the first page load races an empty
// cache and renders an empty fleet, which is indistinguishable from an outage.
type httpRunnable struct {
	addr string
	h    http.Handler
	log  logr.Logger
}

func (r *httpRunnable) Start(ctx context.Context) error {
	srv := &http.Server{Addr: r.addr, Handler: r.h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()
	r.log.Info("dashboard listening", "addr", r.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// NeedLeaderElection is false: the dashboard is read-only, so every replica may
// serve it.
func (r *httpRunnable) NeedLeaderElection() bool { return false }
