package registry

import (
	"context"
	"os"
	"regexp"
	"testing"
)

// TestLiveGHCR checks the real registry. Opt-in: HIVE_REGISTRY_LIVE=1.
func TestLiveGHCR(t *testing.T) {
	if os.Getenv("HIVE_REGISTRY_LIVE") == "" {
		t.Skip("set HIVE_REGISTRY_LIVE=1 to query ghcr.io")
	}
	c := New()
	ctx := context.Background()
	for _, repo := range []string{"ghcr.io/hivecommons/hive", "ghcr.io/hivecommons/hive-hub"} {
		img, err := c.Resolve(ctx, repo, "v6-latest")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:v6-latest -> %s rev=%s platforms=%v", repo, img.Digest, img.Revision, img.Platforms)
	}
	tags, err := c.Tags(ctx, "ghcr.io/hivecommons/hive")
	if err != nil {
		t.Fatal(err)
	}
	n := NewestMatching(tags, regexp.MustCompile(`^v5\.`))
	if len(n) > 3 {
		n = n[:3]
	}
	t.Logf("%d tags; newest v5: %v", len(tags), n)
}
