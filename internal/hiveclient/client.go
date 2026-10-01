// Package hiveclient talks to a hive spoke.
//
// AUTH, WHICH IS NOT OBVIOUS
// --------------------------
// Everything goes through pod exec to 127.0.0.1:3002 with the spoke's shared
// dashboard token in X-Hive-Internal. The public hostname is login-gated and
// the node proxy on :3001 strips the internal header.
//
// On hive v6 that header is owner-equivalent: with no hive_session cookie
// alongside it, authenticate() grants a verified owner identity, so pause,
// resume, kick and the atomic PUT /api/config/agent/{name}/models all work
// headlessly (hivecommons/hive#4134). Do NOT send a session cookie with it —
// a cookie scopes the request down to that user's live allowlist role.
//
// This replaces the owner session cookie the bash ops used. Those sessions are
// minted only by a GitHub device-flow login and expire, and every mutation
// silently stopped the day the newest one did. On v5 the same header was
// read-only ({"error":"owner access required"}), which is why the cookie path
// existed at all.
package hiveclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

// GetJSON performs an authenticated read via the internal header.
func (c *Client) GetJSON(ctx context.Context, ns, pod, token, path string, v any) error {
	script := fmt.Sprintf(
		`curl -sS -m 25 -H %s %s 2>/dev/null`,
		ShellQuote("X-Hive-Internal: "+token), ShellQuote(APIAddr+path))
	out, err := c.Sh(ctx, ns, pod, script)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("empty response from %s", path)
	}
	return json.Unmarshal([]byte(out), v)
}

// Do performs an authenticated request and returns the response body.
//
// Pass curl straight to exec rather than wrapping it in another `sh -c` with an
// interpolated secret: that has produced responses that parse but describe a
// different request.
func (c *Client) Do(ctx context.Context, ns, pod, token, method, path string, body []byte) (string, error) {
	script := fmt.Sprintf(`curl -sS -m 75 -X %s -H %s`, ShellQuote(method), ShellQuote("X-Hive-Internal: "+token))
	if body != nil {
		script += fmt.Sprintf(` -H 'Content-Type: application/json' --data-raw %s`, ShellQuote(string(body)))
	}
	script += " " + ShellQuote(APIAddr+path) + " 2>&1"
	return c.Sh(ctx, ns, pod, script)
}

// Post performs a body-less owner mutation such as pause, resume or kick.
func (c *Client) Post(ctx context.Context, ns, pod, token, path string) (string, error) {
	return c.Do(ctx, ns, pod, token, "POST", path, nil)
}

// Placement is the body of PUT /api/config/agent/{name}/models.
//
// One call sets backend, model and effort and is followed by a single restart.
// The older /api/switch then /api/model sequence could leave an agent on a
// backend/model pair that cannot launch when the second call failed.
type Placement struct {
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	// ReasoningEffort nil leaves the effort unchanged; a pointer to "" resets
	// it to the backend default.
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// Place applies a placement atomically and marks the fields operator-owned, so
// the ACMM pack apply on restart cannot revert them.
func (c *Client) Place(ctx context.Context, ns, pod, token, agent string, p Placement) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return c.Do(ctx, ns, pod, token, "PUT", "/api/config/agent/"+EscapePath(agent)+"/models", b)
}

// ShellQuote single-quotes a string for /bin/sh.
func ShellQuote(s string) string {
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
