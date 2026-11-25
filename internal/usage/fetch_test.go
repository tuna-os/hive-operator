package usage

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// usagePod is a hive pod; ip is its status.podIP and containers its container names.
func usagePod(name string, phase corev1.PodPhase, ip string, labels map[string]string, containers ...string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hive", Labels: labels},
		Status:     corev1.PodStatus{Phase: phase, PodIP: ip},
	}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

// sidecar serves body at /v1/usage with the given status and returns its
// loopback IP and port, so the fetcher's http://<podIP>:<port> reaches it.
func sidecar(t *testing.T, status int, body any, gotQuery *string) (string, int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotQuery != nil {
			*gotQuery = r.URL.Path + "?" + r.URL.RawQuery
		}
		w.WriteHeader(status)
		if s, ok := body.(string); ok {
			_, _ = w.Write([]byte(s))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, int32(port)
}

var hiveLabels = map[string]string{"app.kubernetes.io/name": "hive"}

func TestHTTPFetcherReadsReportFromRunningPodWithSidecar(t *testing.T) {
	var q string
	ip, port := sidecar(t, http.StatusOK, Report{Schema: SchemaVersion, Pod: "hive-1", Namespace: "hive"}, &q)
	cs := fake.NewClientset(
		// Skipped: not running, no IP, and no hive-usage container.
		usagePod("pending", corev1.PodPending, ip, hiveLabels, "hive-usage"),
		usagePod("no-ip", corev1.PodRunning, "", hiveLabels, "hive-usage"),
		usagePod("no-sidecar", corev1.PodRunning, ip, hiveLabels, "hive"),
		// Skipped: wrong label.
		usagePod("other-app", corev1.PodRunning, ip, map[string]string{"app.kubernetes.io/name": "other"}, "hive-usage"),
		usagePod("hive-1", corev1.PodRunning, ip, hiveLabels, "hive", "hive-usage"),
	)
	since := time.Date(2026, 11, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	r, err := (&HTTPFetcher{CS: cs}).Fetch(context.Background(), "hive", port, []time.Time{since})
	if err != nil {
		t.Fatal(err)
	}
	if r.Pod != "hive-1" || r.Schema != SchemaVersion {
		t.Fatalf("report = %+v", r)
	}
	if want := Query(since); q != want {
		t.Fatalf("request = %q, want %q", q, want)
	}
	if !strings.Contains(q, "since=2026-11-01T11%3A00%3A00Z") {
		t.Fatalf("since not sent as UTC RFC3339: %q", q)
	}
}

func TestHTTPFetcherHonoursCustomLabel(t *testing.T) {
	ip, port := sidecar(t, http.StatusOK, Report{Schema: SchemaVersion, Pod: "spoke"}, nil)
	cs := fake.NewClientset(
		usagePod("default-labelled", corev1.PodRunning, ip, hiveLabels, "hive-usage"),
		usagePod("spoke", corev1.PodRunning, ip, map[string]string{"role": "spoke"}, "hive-usage"),
	)
	r, err := (&HTTPFetcher{CS: cs, Label: "role=spoke"}).Fetch(context.Background(), "hive", port, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pod != "spoke" {
		t.Fatalf("pod = %q, want the pod matching Label", r.Pod)
	}
}

func TestHTTPFetcherErrors(t *testing.T) {
	good := usagePod("hive-1", corev1.PodRunning, "127.0.0.1", hiveLabels, "hive-usage")
	for _, tc := range []struct {
		name   string
		status int
		body   any
		pods   []*corev1.Pod
		want   string
	}{
		{name: "no pods", pods: nil, want: "no running hive pod with a hive-usage container in hive"},
		{name: "http error", status: http.StatusInternalServerError, body: "boom", pods: []*corev1.Pod{good}, want: "HTTP 500"},
		{name: "bad json", status: http.StatusOK, body: "{not json", pods: []*corev1.Pod{good}, want: "decode hive-usage report"},
		{name: "schema mismatch", status: http.StatusOK, body: Report{Schema: SchemaVersion + 1}, pods: []*corev1.Pod{good}, want: "schema 2, want 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := int32(1)
			if tc.pods != nil {
				_, port = sidecar(t, tc.status, tc.body, nil)
			}
			cs := fake.NewClientset()
			for _, p := range tc.pods {
				if _, err := cs.CoreV1().Pods("hive").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			r, err := (&HTTPFetcher{CS: cs}).Fetch(context.Background(), "hive", port, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (report %+v), want containing %q", err, r, tc.want)
			}
			if r != nil {
				t.Fatalf("report must be nil on error, got %+v", r)
			}
		})
	}
}

func TestHasContainer(t *testing.T) {
	p := usagePod("p", corev1.PodRunning, "10.0.0.1", nil, "hive", "hive-usage")
	if !hasContainer(*p, "hive-usage") || hasContainer(*p, "usage") || hasContainer(corev1.Pod{}, "hive-usage") {
		t.Fatal("hasContainer must match the exact container name only")
	}
}
