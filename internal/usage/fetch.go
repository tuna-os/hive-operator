package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Fetcher reads a spoke's hive-usage sidecar.
type Fetcher interface {
	Fetch(ctx context.Context, namespace string, port int32, since []time.Time) (*Report, error)
}

// HTTPFetcher reads the sidecar over plain HTTP on the hive pod's IP.
//
// Not through pod exec like hiveclient: the sidecar serves only aggregate,
// credential-free usage numbers, so there is nothing to protect behind the
// exec path, and exec would put every read through the API server.
type HTTPFetcher struct {
	CS    kubernetes.Interface
	HTTP  *http.Client
	Label string // default app.kubernetes.io/name=hive
}

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, ns string, port int32, since []time.Time) (*Report, error) {
	label := f.Label
	if label == "" {
		label = "app.kubernetes.io/name=hive"
	}
	pods, err := f.CS.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: label})
	if err != nil {
		return nil, err
	}
	ip := ""
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" && hasContainer(p, "hive-usage") {
			ip = p.Status.PodIP
			break
		}
	}
	if ip == "" {
		return nil, fmt.Errorf("no running hive pod with a hive-usage container in %s", ns)
	}
	if port == 0 {
		port = 9464
	}
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d%s", ip, port, Query(since...)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("hive-usage %s: HTTP %d", ns, resp.StatusCode)
	}
	var r Report
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode hive-usage report: %w", err)
	}
	if r.Schema != SchemaVersion {
		return nil, fmt.Errorf("hive-usage %s: schema %d, want %d", ns, r.Schema, SchemaVersion)
	}
	return &r, nil
}

func hasContainer(p corev1.Pod, name string) bool {
	for _, c := range p.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}
