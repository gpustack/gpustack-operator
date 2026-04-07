package kubemeta

import "k8s.io/apimachinery/pkg/api/equality"

// DeepEqual is a wrapper around equality.Semantic.DeepEqual that can be used to compare any two objects for deep equality,
// taking into account Kubernetes-specific semantics for certain types.
func DeepEqual(a, b any) bool {
	return equality.Semantic.DeepEqual(a, b)
}
