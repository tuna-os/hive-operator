package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/metrics"
)

const ladderController = "modelladder"

// ModelLadderReconciler builds the effective ladder while remaining read-only.
// Rotation is owned by HiveSpokeReconciler and is independently mode-gated.
type ModelLadderReconciler struct {
	client.Client
	Interval time.Duration
	HTTP     *http.Client
}

// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=modelladders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hive.tunaos.org,resources=modelladders/status,verbs=get;update;patch

func (r *ModelLadderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ladder hivev1.ModelLadder
	if err := r.Get(ctx, req.NamespacedName, &ladder); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	mode := ladder.Spec.Mode
	if mode == "" {
		mode = hivev1.ModeShadow
	}
	metrics.SetMode(ladderController, ladder.Name, string(mode))
	iv := r.Interval
	if iv == 0 {
		iv = 15 * time.Minute
	}

	rungs := append([]hivev1.Rung(nil), ladder.Spec.Builtin...)
	dropped := []string{}
	if ladder.Spec.BenchmarkURL != "" {
		bench, err := r.benchmark(ctx, &ladder)
		if err != nil {
			dropped = append(dropped, "benchmark: "+err.Error())
		} else {
			rungs = append(bench, rungs...)
		}
	}

	inv, invErr := r.inventory(ctx, &ladder)
	effective := make([]hivev1.Rung, 0, len(rungs))
	seen := map[string]bool{}
	for _, rung := range rungs {
		if rung.Tier == "" {
			rung.Tier = tierForScore(rung.Score, ladder.Spec.Bands)
		}
		if rung.Tier == "" || rung.Provider == "" || rung.Backend == "" || rung.Model == "" {
			dropped = append(dropped, fmt.Sprintf("%s/%s: incomplete rung", rung.Provider, rung.Model))
			continue
		}
		key := strings.ToLower(rung.Provider + "\x00" + rung.Model + "\x00" + rung.Effort)
		if seen[key] {
			continue
		}
		seen[key] = true
		available := true
		if invErr == nil && len(inv[rung.Provider]) > 0 && !inv[rung.Provider][rung.Model] {
			available = false
			dropped = append(dropped, fmt.Sprintf("%s/%s: absent from live inventory", rung.Provider, rung.Model))
		}
		rung.Available = available
		if available {
			effective = append(effective, rung)
		}
	}

	now := metav1.Now()
	ladder.Status.Effective = effective
	ladder.Status.Dropped = dropped
	ladder.Status.ObservedAt = &now
	ready := metav1.ConditionTrue
	reason, message := "Ready", fmt.Sprintf("%d effective rungs", len(effective))
	if len(effective) == 0 {
		ready, reason, message = metav1.ConditionFalse, "Empty", "no placeable rungs"
	}
	apiMeta.SetStatusCondition(&ladder.Status.Conditions, metav1.Condition{Type: "Ready", Status: ready, Reason: reason, Message: message, ObservedGeneration: ladder.Generation})
	if err := r.Status().Update(ctx, &ladder); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: iv}, nil
}

func (r *ModelLadderReconciler) inventory(ctx context.Context, ladder *hivev1.ModelLadder) (map[string]map[string]bool, error) {
	ref := ladder.Spec.InventoryConfigMapRef
	if ref == nil {
		ref = &hivev1.ConfigMapKeyRef{Name: "hive-model-inventory", Namespace: "hive", Key: "inventory.tsv"}
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &cm); err != nil {
		return nil, err
	}
	result := map[string]map[string]bool{}
	for _, line := range strings.Split(cm.Data[ref.Key], "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		if result[f[0]] == nil {
			result[f[0]] = map[string]bool{}
		}
		result[f[0]][f[2]] = true
	}
	return result, nil
}

func (r *ModelLadderReconciler) benchmark(ctx context.Context, ladder *hivev1.ModelLadder) ([]hivev1.Rung, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ladder.Spec.BenchmarkURL, nil)
	if err != nil {
		return nil, err
	}
	if ref := ladder.Spec.BenchmarkSecretRef; ref != nil {
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &sec); err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+string(sec.Data[ref.Key]))
	}
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var direct []hivev1.Rung
	if json.Unmarshal(b, &direct) == nil {
		return direct, nil
	}
	var wrapped struct {
		Rungs []hivev1.Rung `json:"rungs"`
	}
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return nil, fmt.Errorf("decode benchmark: %w", err)
	}
	sort.SliceStable(wrapped.Rungs, func(i, j int) bool {
		a, _ := strconv.ParseFloat(wrapped.Rungs[i].Score, 64)
		b, _ := strconv.ParseFloat(wrapped.Rungs[j].Score, 64)
		return a > b
	})
	return wrapped.Rungs, nil
}

func tierForScore(score string, bands []hivev1.BandSpec) string {
	v, err := strconv.ParseFloat(score, 64)
	if err != nil {
		return ""
	}
	for _, band := range bands {
		min, err := strconv.ParseFloat(band.MinScore, 64)
		if err == nil && v >= min {
			return band.Tier
		}
	}
	return ""
}

func (r *ModelLadderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&hivev1.ModelLadder{}).Complete(r)
}
