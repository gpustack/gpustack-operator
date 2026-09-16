package kubediscovery

import (
	utilversion "k8s.io/apimachinery/pkg/util/version"
)

// Feature is a Kubernetes capability whose availability is decided by the cluster version.
type Feature int

const (
	// FeatureNativeSidecar is the initContainers[].restartPolicy field a native sidecar is
	// rendered with. The SidecarContainers feature gate turns on by default in Kubernetes 1.29;
	// below that the API server DROPS the field without an error, degrading a native sidecar to a
	// plain init container.
	FeatureNativeSidecar Feature = iota + 1
)

// SupportsFeature returns whether the given Kubernetes version supports the feature.
//
// A nil or unparsable version answers false: a version nobody can read must not unlock a
// capability whose floor cannot be decided.
func SupportsFeature(v *Version, f Feature) bool {
	if v == nil {
		return false
	}
	parsed, err := utilversion.ParseGeneric(v.GitVersion)
	if err != nil {
		return false
	}

	if f == FeatureNativeSidecar {
		return parsed.Major() > 1 || (parsed.Major() == 1 && parsed.Minor() >= 29)
	}
	return false
}
