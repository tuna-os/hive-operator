package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fakeGHCR mimics GHCR: 401 + Bearer challenge without a token, anonymous
// tokens from /token, an index for v6-latest, per-platform manifests, a config
// blob with the revision label, and paginated tags/list with Link headers.
type fakeGHCR struct {
	srv       *httptest.Server
	index     []byte
	tags      []string
	tokenHits int
}

func newFakeGHCR(t *testing.T) *fakeGHCR {
	f := &fakeGHCR{}
	cfg := []byte(`{"architecture":"amd64","os":"linux","config":{"Labels":{"org.opencontainers.image.revision":"5d26aa998c80311040acd5a0279090b2a451b5e1"}}}`)
	cfgD := digestOf(cfg)
	amd := []byte(fmt.Sprintf(`{"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q}}`, mtOCIManifest, cfgD))
	amdD := digestOf(amd)
	arm := []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:00"}}`)
	armD := digestOf(arm)
	f.index = []byte(fmt.Sprintf(`{"mediaType":%q,"manifests":[
	  {"digest":%q,"platform":{"os":"linux","architecture":"arm64"}},
	  {"digest":%q,"platform":{"os":"linux","architecture":"amd64"}},
	  {"digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}}]}`, mtDockerList, armD, amdD))
	for i := 0; i < 5; i++ {
		f.tags = append(f.tags, fmt.Sprintf("v5.35.%d", i+8))
	}
	f.tags = append(f.tags, "v6-latest", "edge", "v5.36.0-rc.1", "v4.9.0", "abc1234", "v5.100.2")

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenHits++
		if r.URL.Query().Get("scope") != "repository:hivecommons/hive:pull" {
			http.Error(w, "bad scope", 400)
			return
		}
		_, _ = w.Write([]byte(`{"token":"anon"}`))
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="repository:hivecommons/hive:pull"`, f.srv.URL))
			w.WriteHeader(401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v2/hivecommons/hive/")
		switch {
		case p == "manifests/v6-latest" || p == "manifests/"+digestOf(f.index):
			w.Header().Set("Docker-Content-Digest", digestOf(f.index))
			_, _ = w.Write(f.index)
		case p == "manifests/"+amdD:
			_, _ = w.Write(amd)
		case p == "manifests/"+armD:
			_, _ = w.Write(arm)
		case p == "blobs/"+cfgD:
			_, _ = w.Write(cfg)
		case p == "tags/list":
			n := 2
			last := r.URL.Query().Get("last")
			start := 0
			if last != "" {
				for i, t := range f.tags {
					if t == last {
						start = i + 1
					}
				}
			}
			end := start + n
			if end >= len(f.tags) {
				end = len(f.tags)
			} else {
				w.Header().Set("Link", fmt.Sprintf(`</v2/hivecommons/hive/tags/list?last=%s&n=%d>; rel="next"`, f.tags[end-1], n))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "hivecommons/hive", "tags": f.tags[start:end]})
		default:
			w.WriteHeader(404)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGHCR) repo() string {
	return strings.TrimPrefix(f.srv.URL, "http://") + "/hivecommons/hive"
}

func TestResolveTagReadsDigestPlatformsAndRevision(t *testing.T) {
	f := newFakeGHCR(t)
	c := &HTTP{Client: f.srv.Client(), Scheme: "http"}
	img, err := c.Resolve(context.Background(), f.repo(), "v6-latest")
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != digestOf(f.index) {
		t.Fatalf("digest %s, want %s", img.Digest, digestOf(f.index))
	}
	if strings.Join(img.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("platforms %v (attestation entries must be ignored)", img.Platforms)
	}
	if img.Revision != "5d26aa998c80311040acd5a0279090b2a451b5e1" {
		t.Fatalf("revision %q", img.Revision)
	}
	if img.Tag != "v6-latest" {
		t.Fatalf("tag %q", img.Tag)
	}
	// Second call reuses the cached token.
	if _, err := c.Resolve(context.Background(), f.repo(), digestOf(f.index)); err != nil {
		t.Fatal(err)
	}
	if f.tokenHits != 1 {
		t.Fatalf("token fetched %d times, want 1", f.tokenHits)
	}
}

func TestResolveMissingTagIsNotFound(t *testing.T) {
	f := newFakeGHCR(t)
	c := &HTTP{Client: f.srv.Client(), Scheme: "http"}
	_, err := c.Resolve(context.Background(), f.repo(), "v9-latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTagsFollowsPagination(t *testing.T) {
	f := newFakeGHCR(t)
	c := &HTTP{Client: f.srv.Client(), Scheme: "http", PageSize: 2}
	tags, err := c.Tags(context.Background(), f.repo())
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != len(f.tags) {
		t.Fatalf("got %d tags over pages, want %d: %v", len(tags), len(f.tags), tags)
	}
	got := NewestMatching(tags, regexp.MustCompile(`^v5\.`))
	if len(got) == 0 || got[0] != "v5.100.2" {
		t.Fatalf("newest v5 = %v; semver order must be numeric and skip prereleases", got)
	}
	for _, g := range got {
		if g == "v5.36.0-rc.1" {
			t.Fatal("prerelease selected")
		}
	}
	if got[1] != "v5.35.12" || got[len(got)-1] != "v5.35.8" {
		t.Fatalf("order %v", got)
	}
}

func TestSemverOrder(t *testing.T) {
	a, _ := ParseSemver("v5.35.9")
	b, _ := ParseSemver("v5.35.10")
	if !a.Less(b) || b.Less(a) {
		t.Fatal("v5.35.9 must sort below v5.35.10")
	}
	if _, ok := ParseSemver("v6-latest"); ok {
		t.Fatal("v6-latest is not semver")
	}
}
