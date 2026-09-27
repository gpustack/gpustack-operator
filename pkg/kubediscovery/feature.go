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

	// FeatureImageVolume is the volumes[].image field an image volume is rendered with. The
	// ImageVolume feature gate turns on by default in Kubernetes 1.35 (beta) and is GA in 1.36;
	// below that the gate has to be opened on the apiserver, and an apiserver that does not know
	// the field DROPS it without an error, so a Pod would silently lose its weights.
	FeatureImageVolume
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

	switch f {
	case FeatureNativeSidecar:
		return parsed.Major() > 1 || (parsed.Major() == 1 && parsed.Minor() >= 29)
	case FeatureImageVolume:
		return parsed.Major() > 1 || (parsed.Major() == 1 && parsed.Minor() >= 35)
	}
	return false
}
