// Package v1alpha1 contains the API types for the hive operator.
//
// +kubebuilder:object:generate=true
// +groupName=hive.tunaos.org
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version for these types.
	GroupVersion = schema.GroupVersion{Group: "hive.tunaos.org", Version: "v1alpha1"}

	// SchemeBuilder registers these types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds these types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
