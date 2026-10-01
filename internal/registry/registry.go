// Package registry resolves image tags against an OCI registry anonymously.
//
// It does exactly what the hive-upgrade script did with curl, and nothing more:
// take an anonymous pull token, fetch the image index for a tag, record the
// digest of the exact bytes served (a moving tag is never deployed, only the
// digest it pointed at), list the platforms in the index (for the architecture
// gate), and read the linux/amd64 config's org.opencontainers.image.revision
// label so a moving tag such as v6-latest still gets a human version label.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means the registry answered 404 for the reference.
var ErrNotFound = errors.New("not found in registry")

// RevisionLabel is the OCI label carrying the source commit.
const RevisionLabel = "org.opencontainers.image.revision"

// Image is a resolved reference.
type Image struct {
	Repo      string   // host/path, e.g. ghcr.io/hivecommons/hive
	Tag       string   // empty when resolved by digest
	Digest    string   // sha256:… of the index (or single manifest) bytes
	Platforms []string // os/arch, sorted
	Revision  string   // config label, may be empty
}

// Client is what the release controller needs from a registry.
type Client interface {
	// Resolve fetches ref (a tag or sha256 digest) in repo.
	Resolve(ctx context.Context, repo, ref string) (Image, error)
	// Tags lists every tag in repo, following pagination.
	Tags(ctx context.Context, repo string) ([]string, error)
}

// HTTP is the anonymous registry client.
type HTTP struct {
	Client *http.Client
	// Scheme is "https" unless overridden (tests use plain http).
	Scheme string
	// PageSize for tags/list. Default 1000.
	PageSize int

	mu     sync.Mutex
	tokens map[string]string // repo -> bearer token
}

// New returns an anonymous client with sane timeouts.
func New() *HTTP {
	return &HTTP{Client: &http.Client{Timeout: 30 * time.Second}, Scheme: "https"}
}

const (
	mtOCIIndex     = "application/vnd.oci.image.index.v1+json"
	mtDockerList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mtDockerManif  = "application/vnd.docker.distribution.manifest.v2+json"
	acceptIndex    = mtOCIIndex + "," + mtDockerList + "," + mtOCIManifest + "," + mtDockerManif
	acceptManifest = mtOCIManifest + "," + mtDockerManif
)

// SplitRepo splits ghcr.io/org/name into host and path.
func SplitRepo(repo string) (host, path string, err error) {
	i := strings.Index(repo, "/")
	if i <= 0 || i == len(repo)-1 {
		return "", "", fmt.Errorf("repo %q must be host/path", repo)
	}
	return repo[:i], repo[i+1:], nil
}

func (c *HTTP) scheme() string {
	if c.Scheme == "" {
		return "https"
	}
	return c.Scheme
}

func (c *HTTP) httpClient() *http.Client {
	if c.Client == nil {
		return http.DefaultClient
	}
	return c.Client
}

// get performs an authenticated GET, fetching an anonymous bearer token on a
// 401 challenge and retrying once.
func (c *HTTP) get(ctx context.Context, repo, u, accept string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		c.mu.Lock()
		tok := c.tokens[repo]
		c.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return resp, nil
		}
		ch := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		tok, err = c.token(ctx, repo, ch)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.tokens == nil {
			c.tokens = map[string]string{}
		}
		c.tokens[repo] = tok
		c.mu.Unlock()
	}
	return nil, errors.New("unreachable")
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// token fetches an anonymous pull token from the challenge's realm. When the
// challenge is absent it falls back to GHCR's /token endpoint.
func (c *HTTP) token(ctx context.Context, repo, challenge string) (string, error) {
	host, path, err := SplitRepo(repo)
	if err != nil {
		return "", err
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		realm = c.scheme() + "://" + host + "/token"
	}
	service := params["service"]
	if service == "" {
		service = host
	}
	q := url.Values{}
	q.Set("scope", "repository:"+path+":pull")
	q.Set("service", service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token for %s: HTTP %d", repo, resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token for %s: %w", repo, err)
	}
	if body.Token == "" {
		body.Token = body.AccessToken
	}
	if body.Token == "" {
		return "", fmt.Errorf("token for %s: empty", repo)
	}
	return body.Token, nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type manifestDoc struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    *descriptor  `json:"config"`
}

func (c *HTTP) manifest(ctx context.Context, repo, ref, accept string) ([]byte, string, error) {
	host, path, err := SplitRepo(repo)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.get(ctx, repo, fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme(), host, path, ref), accept)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, "", fmt.Errorf("%s:%s: %w", repo, ref, ErrNotFound)
	default:
		return nil, "", fmt.Errorf("%s manifests/%s: HTTP %d", repo, ref, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	d := "sha256:" + hex.EncodeToString(sum[:])
	if h := resp.Header.Get("Docker-Content-Digest"); h != "" && h != d {
		return nil, "", fmt.Errorf("%s:%s: served bytes hash to %s but registry says %s", repo, ref, d, h)
	}
	return b, d, nil
}

// Resolve implements Client.
func (c *HTTP) Resolve(ctx context.Context, repo, ref string) (Image, error) {
	img := Image{Repo: repo}
	if strings.HasPrefix(ref, "sha256:") {
		img.Digest = ref
	} else {
		img.Tag = ref
	}
	b, d, err := c.manifest(ctx, repo, ref, acceptIndex)
	if err != nil {
		return img, err
	}
	if img.Digest != "" && img.Digest != d {
		return img, fmt.Errorf("%s@%s: served bytes hash to %s", repo, ref, d)
	}
	img.Digest = d
	var doc manifestDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return img, fmt.Errorf("%s:%s: %w", repo, ref, err)
	}
	var cfg *descriptor
	if len(doc.Manifests) > 0 {
		var pick string
		for _, m := range doc.Manifests {
			if m.Platform == nil || m.Platform.OS == "unknown" {
				continue // attestation manifests
			}
			img.Platforms = append(img.Platforms, m.Platform.OS+"/"+m.Platform.Architecture)
			if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				pick = m.Digest
			} else if pick == "" {
				pick = m.Digest
			}
		}
		sort.Strings(img.Platforms)
		if pick != "" {
			mb, _, err := c.manifest(ctx, repo, pick, acceptManifest)
			if err != nil {
				return img, err
			}
			var md manifestDoc
			if err := json.Unmarshal(mb, &md); err != nil {
				return img, err
			}
			cfg = md.Config
		}
	} else {
		cfg = doc.Config
	}
	if cfg != nil && cfg.Digest != "" {
		labels, os, arch, err := c.config(ctx, repo, cfg.Digest)
		if err != nil {
			return img, err
		}
		img.Revision = labels[RevisionLabel]
		if len(doc.Manifests) == 0 && os != "" {
			img.Platforms = []string{os + "/" + arch}
		}
	}
	return img, nil
}

func (c *HTTP) config(ctx context.Context, repo, digest string) (map[string]string, string, string, error) {
	host, path, err := SplitRepo(repo)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := c.get(ctx, repo, fmt.Sprintf("%s://%s/v2/%s/blobs/%s", c.scheme(), host, path, digest), "")
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("%s blobs/%s: HTTP %d", repo, digest, resp.StatusCode)
	}
	var body struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, "", "", err
	}
	return body.Config.Labels, body.OS, body.Architecture, nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// Tags implements Client. It follows the Link rel="next" header, which is how
// GHCR paginates (/v2/<repo>/tags/list?last=<tag>&n=1000).
func (c *HTTP) Tags(ctx context.Context, repo string) ([]string, error) {
	host, path, err := SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	n := c.PageSize
	if n <= 0 {
		n = 1000
	}
	base := c.scheme() + "://" + host
	next := fmt.Sprintf("/v2/%s/tags/list?n=%d", path, n)
	var all []string
	for page := 0; next != "" && page < 200; page++ {
		resp, err := c.get(ctx, repo, base+next, "application/json")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%s tags/list: HTTP %d", repo, resp.StatusCode)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, body.Tags...)
		next = ""
		if m := linkNext.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			next = m[1]
			if u, err := url.Parse(next); err == nil && u.IsAbs() {
				next = u.RequestURI()
			}
		} else if len(body.Tags) == n {
			// No Link header but a full page: ask for the page after the last tag.
			next = fmt.Sprintf("/v2/%s/tags/list?n=%d&last=%s", path, n, url.QueryEscape(body.Tags[len(body.Tags)-1]))
		}
	}
	return all, nil
}

// Semver is a parsed vMAJOR.MINOR.PATCH[-pre] tag.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseSemver parses a tag; ok is false when it is not semver.
func ParseSemver(tag string) (Semver, bool) {
	m := semverRe.FindStringSubmatch(tag)
	if m == nil {
		return Semver{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return Semver{a, b, c, m[4]}, true
}

// Less orders by numeric components (v5.35.9 < v5.35.10), prerelease first.
func (s Semver) Less(o Semver) bool {
	if s.Major != o.Major {
		return s.Major < o.Major
	}
	if s.Minor != o.Minor {
		return s.Minor < o.Minor
	}
	if s.Patch != o.Patch {
		return s.Patch < o.Patch
	}
	if s.Pre == o.Pre {
		return false
	}
	if s.Pre == "" {
		return false
	}
	if o.Pre == "" {
		return true
	}
	return s.Pre < o.Pre
}

// NewestMatching returns matching non-prerelease semver tags, newest first.
func NewestMatching(tags []string, line *regexp.Regexp) []string {
	type tv struct {
		tag string
		v   Semver
	}
	var c []tv
	seen := map[string]bool{}
	for _, t := range tags {
		if seen[t] || !line.MatchString(t) {
			continue
		}
		seen[t] = true
		v, ok := ParseSemver(t)
		if !ok || v.Pre != "" {
			continue
		}
		c = append(c, tv{t, v})
	}
	sort.SliceStable(c, func(i, j int) bool { return c[j].v.Less(c[i].v) })
	out := make([]string, len(c))
	for i := range c {
		out[i] = c[i].tag
	}
	return out
}
