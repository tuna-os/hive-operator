package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

func TestRungIdentityIncludesTierAndEffort(t *testing.T) {
	base := hivev1.Rung{Tier: "T2", Provider: "openai", Model: "gpt-5.6-luna", Effort: "medium"}

	if rungIdentity(base) == rungIdentity(hivev1.Rung{Tier: "T3", Provider: base.Provider, Model: base.Model, Effort: base.Effort}) {
		t.Fatal("same model at a different tier was de-duplicated")
	}
	if rungIdentity(base) == rungIdentity(hivev1.Rung{Tier: base.Tier, Provider: base.Provider, Model: base.Model, Effort: "low"}) {
		t.Fatal("same model at a different effort was de-duplicated")
	}
	if rungIdentity(base) != rungIdentity(hivev1.Rung{Tier: "t2", Provider: "OpenAI", Model: "GPT-5.6-LUNA", Effort: "MEDIUM"}) {
		t.Fatal("identity must be case-insensitive")
	}
}

// The benchmark key must come from the uncached Secrets reader. Through the
// cached Client the same read starts an informer over every Secret in the
// cluster, which is what the role's list and watch on secrets were for.
func TestBenchmarkKeyIsReadThroughSecretsReader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	ladder := &hivev1.ModelLadder{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}, Spec: hivev1.ModelLadderSpec{
		BenchmarkURL:       srv.URL,
		BenchmarkSecretRef: &hivev1.SecretKeyRef{Namespace: "hive", Name: "bench", Key: "key"},
	}}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "hive", Name: "bench"}, Data: map[string][]byte{"key": []byte("k-1")}}
	// The cached client deliberately does not hold the Secret: a read that
	// goes through it fails this test.
	cached := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ladder).Build()
	uncached := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()

	r := &ModelLadderReconciler{Client: cached, Secrets: uncached}
	if _, err := r.benchmark(context.Background(), ladder); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k-1" {
		t.Fatalf("Authorization = %q, want the key from the Secrets reader", gotAuth)
	}
}

// config/rbac/role.yaml is generated from the kubebuilder markers. Secrets are
// only ever fetched by name, so the role must not let the operator enumerate
// them.
func TestRoleGrantsOnlyGetOnSecrets(t *testing.T) {
	b, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(b, &role); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res != "secrets" && res != "*" {
				continue
			}
			found = true
			if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
				t.Errorf("rule for %v grants %v on secrets, want only get", rule.Resources, rule.Verbs)
			}
		}
	}
	if !found {
		t.Fatal("no rule for secrets: the HiveSpoke controller reads hive-secrets by name")
	}
}
