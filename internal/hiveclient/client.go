// Package hiveclient talks to a hive spoke.
//
// AUTH, WHICH IS NOT OBVIOUS
// --------------------------
// Two credentials exist and they are not interchangeable:
//
//	X-Hive-Internal: <token>   authenticates READS. Every mutation returns
//	                           {"error":"owner access required"}. Forged
//	                           X-Hive-User / X-Hive-Role headers are rejected.
//	Cookie: hive_session=<id>  full owner access. This is the one for writes.
//
// The cookie name is `hive_session` with an underscore. `hive-session-v1`
// appears in the binary and looks right; it is not the cookie name.
//
// Sessions live in /data/dashboard-sessions.json inside the pod and are minted
// only by a GitHub device-flow login. There is no headless way to create one.
// Pick the newest by expiry and do NOT filter locally on expiry: the store
// writes the pod's UTC offset while a caller writes its own, and a
// lexicographic ISO-8601 compare across differing offsets silently discards
// live sessions near the boundary. The server is the only authority.
//
// Everything goes through `kubectl exec`-equivalent pod exec to 127.0.0.1:3002.
// The public hostname is login-gated and the node proxy on :3001 strips the
// internal header.
package hiveclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// APIAddr is the in-pod address of the Go dashboard API.
const APIAddr = "http://127.0.0.1:3002"

// Client execs into a spoke's hive pod.
type Client struct {
	cs    kubernetes.Interface
	cfg   *rest.Config
	Label string
}

// New builds a Client.
func New(cs kubernetes.Interface, cfg *rest.Config) *Client {
	return &Client{cs: cs, cfg: cfg, Label: "app.kubernetes.io/name=hive"}
}

// Pod returns the hive pod name in a namespace.
func (c *Client) Pod(ctx context.Context, ns string) (string, error) {
	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: c.Label})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no running hive pod in %s", ns)
}

// Exec runs argv in the hive pod and returns stdout.
func (c *Client) Exec(ctx context.Context, ns, pod string, argv []string) (string, string, error) {
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: argv,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.cfg, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

// Sh runs a /bin/sh -c script in the pod.
func (c *Client) Sh(ctx context.Context, ns, pod, script string) (string, error) {
	out, errOut, err := c.Exec(ctx, ns, pod, []string{"/bin/sh", "-c", script})
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut))
	}
	return out, nil
}

type sessionRec struct {
	Role      string `json:"Role"`
	ExpiresAt string `json:"ExpiresAt"`
}

// OwnerSession returns the newest owner session id, or "" when none exists.
//
// Newest-by-expiry with no local expiry filter — see the package comment.
func (c *Client) OwnerSession(ctx context.Context, ns, pod string) (string, error) {
	out, err := c.Sh(ctx, ns, pod, "cat /data/dashboard-sessions.json 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		return "", err
	}
	var m map[string]sessionRec
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return "", fmt.Errorf("parse session store: %w", err)
	}
	type kv struct {
		id  string
		exp string
	}
	var owners []kv
	for id, r := range m {
		if r.Role == "owner" {
			owners = append(owners, kv{id, r.ExpiresAt})
		}
	}
	if len(owners) == 0 {
		return "", nil
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].exp > owners[j].exp })
	return owners[0].id, nil
}

// GetJSON performs an authenticated read via the internal header.
func (c *Client) GetJSON(ctx context.Context, ns, pod, token, path string, v any) error {
	script := fmt.Sprintf(
		`curl -sS -m 25 -H %s %s 2>/dev/null`,
		shellQuote("X-Hive-Internal: "+token), shellQuote(APIAddr+path))
	out, err := c.Sh(ctx, ns, pod, script)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("empty response from %s", path)
	}
	return json.Unmarshal([]byte(out), v)
}

// Post performs a mutation with the owner session cookie.
//
// Pass curl straight to exec rather than wrapping it in another `sh -c` with an
// interpolated secret: that has produced responses that parse but describe a
// different request.
func (c *Client) Post(ctx context.Context, ns, pod, session, path string) (string, error) {
	script := fmt.Sprintf(
		`curl -sS -m 75 -X POST -H %s %s 2>&1`,
		shellQuote("Cookie: hive_session="+session), shellQuote(APIAddr+path))
	return c.Sh(ctx, ns, pod, script)
}

// shellQuote single-quotes a string for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// EscapePath escapes a path segment for use in an API URL.
func EscapePath(s string) string { return url.PathEscape(s) }

// Secret reads one key from a Secret and returns it as a string.
func (c *Client) Secret(ctx context.Context, ns, name, key string) (string, error) {
	s, err := c.cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %s", ns, name, key)
	}
	return string(v), nil
}
