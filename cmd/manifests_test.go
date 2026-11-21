package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	operatorSA        = "hive-operator"
	operatorNamespace = "hive-system"
	generatedRole     = "hive-operator"
	crGroup           = "hive.tunaos.org"
)

type manifest struct {
	file string
	kind string
	raw  []byte
}

// manifests returns every YAML document of the files a glob matches.
func manifests(t *testing.T, glob string) []manifest {
	t.Helper()
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests match %s (%v)", glob, err)
	}
	var out []manifest
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range regexp.MustCompile(`(?m)^---[ \t]*$`).Split(string(b), -1) {
			var meta struct{ Kind string }
			if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if meta.Kind != "" {
				out = append(out, manifest{file: f, kind: meta.Kind, raw: []byte(doc)})
			}
		}
	}
	return out
}

func decode[T any](t *testing.T, m manifest) T {
	t.Helper()
	var v T
	if err := yaml.UnmarshalStrict(m.raw, &v); err != nil {
		t.Fatalf("%s: %s: %v", m.file, m.kind, err)
	}
	return v
}

// grants flattens rules to "group/resource verb" lines.
func grants(rules []rbacv1.PolicyRule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					out[g+"/"+res+" "+v] = true
				}
			}
		}
	}
	return out
}

// clusterScoped says whether a grant line is about a kind with no namespace.
// Those are the only rules a ClusterRoleBinding may carry: the operator's own
// kinds (all cluster-scoped, checked below) and Nodes.
func clusterScoped(grant string) bool {
	return strings.HasPrefix(grant, crGroup+"/") || strings.HasPrefix(grant, "/nodes ")
}

func isOperator(subjects []rbacv1.Subject) bool {
	return len(subjects) == 1 && subjects[0].Kind == "ServiceAccount" &&
		subjects[0].Name == operatorSA && subjects[0].Namespace == operatorNamespace
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// namespacesNamed collects every namespace a custom resource names in its
// spec: any field whose name ends in "namespace" or "namespaces".
func namespacesNamed(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if l := strings.ToLower(k); strings.HasSuffix(l, "namespace") || strings.HasSuffix(l, "namespaces") {
				switch n := val.(type) {
				case string:
					into[n] = true
				case []any:
					for _, e := range n {
						if s, ok := e.(string); ok {
							into[s] = true
						}
					}
				}
			}
			namespacesNamed(val, into)
		}
	case []any:
		for _, e := range x {
			namespacesNamed(e, into)
		}
	}
}

// The generated hive-operator role allows pod exec, Secret reads and CronJob
// and Deployment writes. Bound with a ClusterRoleBinding it gives those in
// every namespace, kube-system included. These checks hold the manifests to
// the narrow shape: that role per hive namespace, the cache limited to the
// same namespaces, and cluster-wide only the kinds that have no namespace.
func TestOperatorRoleIsBoundPerNamespace(t *testing.T) {
	rbac := append(manifests(t, "../config/rbac/*.yaml"), manifests(t, "../config/rbac-fleet/*.yaml")...)

	clusterRoles := map[string][]rbacv1.PolicyRule{}
	for _, m := range rbac {
		if m.kind == "ClusterRole" {
			r := decode[rbacv1.ClusterRole](t, m)
			clusterRoles[r.Name] = r.Rules
		}
	}
	generated, ok := clusterRoles[generatedRole]
	if !ok {
		t.Fatalf("config/rbac has no ClusterRole %s", generatedRole)
	}

	// Cluster-wide: exactly the cluster-scoped rules of the generated role.
	wantCluster := map[string]bool{}
	for g := range grants(generated) {
		if clusterScoped(g) {
			wantCluster[g] = true
		}
	}
	gotCluster := map[string]bool{}
	for _, m := range rbac {
		if m.kind != "ClusterRoleBinding" {
			continue
		}
		b := decode[rbacv1.ClusterRoleBinding](t, m)
		rules, known := clusterRoles[b.RoleRef.Name]
		if !known {
			t.Fatalf("%s: ClusterRoleBinding %s binds %s, which config/rbac does not define", m.file, b.Name, b.RoleRef.Name)
		}
		for g := range grants(rules) {
			if !clusterScoped(g) {
				t.Errorf("%s: ClusterRoleBinding %s grants %q in every namespace; bind it per namespace in config/rbac-fleet", m.file, b.Name, g)
			}
			if isOperator(b.Subjects) {
				gotCluster[g] = true
			}
		}
	}
	if got, want := strings.Join(sorted(gotCluster), "\n"), strings.Join(sorted(wantCluster), "\n"); got != want {
		t.Errorf("cluster-wide grants of the operator:\n%s\nwant the cluster-scoped rules of config/rbac/role.yaml:\n%s", got, want)
	}
	for _, m := range manifests(t, "../config/crd/*.yaml") {
		var crd struct {
			Spec struct{ Scope string }
		}
		if err := yaml.Unmarshal(m.raw, &crd); err != nil {
			t.Fatal(err)
		}
		if crd.Spec.Scope != "Cluster" {
			t.Errorf("%s: scope %q: clusterScoped() takes every %s kind for cluster-scoped", m.file, crd.Spec.Scope, crGroup)
		}
	}

	// Per namespace: the generated role, and the manager's cache, on one list.
	bound := map[string]bool{}
	eventsInDefault := map[string]bool{}
	roles := map[string][]rbacv1.PolicyRule{}
	for _, m := range rbac {
		if m.kind == "Role" {
			r := decode[rbacv1.Role](t, m)
			roles[r.Namespace+"/"+r.Name] = r.Rules
		}
	}
	for _, m := range rbac {
		if m.kind != "RoleBinding" {
			continue
		}
		b := decode[rbacv1.RoleBinding](t, m)
		if !isOperator(b.Subjects) {
			t.Errorf("%s: RoleBinding %s/%s: subjects %v, want only the operator's service account", m.file, b.Namespace, b.Name, b.Subjects)
			continue
		}
		switch {
		case b.RoleRef.Kind == "ClusterRole" && b.RoleRef.Name == generatedRole:
			bound[b.Namespace] = true
		case b.RoleRef.Kind == "Role" && b.Namespace == "default":
			for g := range grants(roles["default/"+b.RoleRef.Name]) {
				eventsInDefault[g] = true
			}
		}
	}
	var flag string
	for _, m := range manifests(t, "../config/manager/*.yaml") {
		if m.kind != "Deployment" {
			continue
		}
		for _, c := range decode[appsv1.Deployment](t, m).Spec.Template.Spec.Containers {
			for _, a := range c.Args {
				if v, ok := strings.CutPrefix(a, "--watch-namespaces="); ok {
					flag = v
				}
			}
		}
	}
	watched := map[string]bool{}
	for ns := range cacheOptions(flag).DefaultNamespaces {
		watched[ns] = true
	}
	if len(watched) == 0 {
		t.Fatal("config/manager passes no --watch-namespaces: the cache lists every namespace, which only a cluster-wide binding allows")
	}
	if got, want := strings.Join(sorted(bound), ","), strings.Join(sorted(watched), ","); got != want {
		t.Errorf("RoleBindings of %s are in [%s], --watch-namespaces is [%s]: a watched namespace without a binding stops the manager, a binding that is not watched is unused authority", generatedRole, got, want)
	}

	// Every namespace a sample names, and the two the code falls back to when
	// a field is empty, must be on that list.
	named := map[string]bool{"hive": true, "hive-contributors": true}
	for _, m := range append(manifests(t, "../config/samples/*.yaml"), manifests(t, "../config/usage/*.yaml")...) {
		var doc map[string]any
		if err := yaml.Unmarshal(m.raw, &doc); err != nil {
			t.Fatal(err)
		}
		if api, _ := doc["apiVersion"].(string); strings.HasPrefix(api, crGroup+"/") {
			namespacesNamed(doc["spec"], named)
		}
	}
	for _, ns := range sorted(named) {
		if !watched[ns] {
			t.Errorf("namespace %s is named by a sample or a default but is not in --watch-namespaces", ns)
		}
	}

	// An event about a cluster-scoped object is written to default.
	for g := range grants(generated) {
		if strings.HasSuffix(strings.Fields(g)[0], "/events") && !eventsInDefault[g] {
			t.Errorf("the operator has no %q in namespace default: events about a HiveRelease are refused", g)
		}
	}
	for g := range eventsInDefault {
		if !strings.HasSuffix(strings.Fields(g)[0], "/events") {
			t.Errorf("the operator is granted %q in namespace default; only events belong there", g)
		}
	}
}
