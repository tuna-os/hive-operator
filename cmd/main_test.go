package main

import (
	"sort"
	"strings"
	"testing"
)

// The namespace list decides how wide the operator's list and watch
// permissions must be, so both ends matter: a list must reach the cache
// exactly, and an empty flag must leave the cache options untouched.
func TestCacheOptions(t *testing.T) {
	cases := []struct {
		name string
		flag string
		want []string
	}{
		{"empty keeps the cluster-wide cache", "", nil},
		{"only separators keeps the cluster-wide cache", " , ,", nil},
		{"two namespaces", "hive,hive-reef", []string{"hive", "hive-reef"}},
		{"spaces and empty items are dropped", " hive , ,hive-hub,", []string{"hive", "hive-hub"}},
		{"a repeated namespace counts once", "hive,hive", []string{"hive"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := cacheOptions(tc.flag)
			if tc.want == nil {
				// A non-nil empty map would still read as "no restriction"
				// today; nil is what the manager got before the flag existed.
				if opts.DefaultNamespaces != nil {
					t.Fatalf("DefaultNamespaces = %v, want nil", opts.DefaultNamespaces)
				}
				return
			}
			var got []string
			for ns := range opts.DefaultNamespaces {
				got = append(got, ns)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("DefaultNamespaces = %v, want %v", got, tc.want)
			}
		})
	}
}
