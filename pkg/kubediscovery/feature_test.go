package kubediscovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSupportsFeature(t *testing.T) {
	for _, tc := range []struct {
		name       string
		feature    Feature
		gitVersion string
		want       bool
	}{
		// FeatureNativeSidecar: on by default from 1.29.
		{"sidecar at the floor", FeatureNativeSidecar, "v1.29.0", true},
		{"sidecar above the floor", FeatureNativeSidecar, "v1.30.2", true},
		{"sidecar below the floor", FeatureNativeSidecar, "v1.28.15", false},
		{"sidecar at the chart's own floor", FeatureNativeSidecar, "v1.23.17", false},
		{"sidecar vendor suffix", FeatureNativeSidecar, "v1.29.3-gke.1000", true},
		{"sidecar pre-release at the floor", FeatureNativeSidecar, "v1.29.0-rc.1", true},
		// FeatureImageVolume: on by default from 1.35 (1.36 GA with LockToDefault).
		{"image volume at the floor", FeatureImageVolume, "v1.35.0", true},
		{"image volume above the floor", FeatureImageVolume, "v1.35.7", true},
		{"image volume at GA", FeatureImageVolume, "v1.36.1", true},
		{"image volume below the floor", FeatureImageVolume, "v1.34.9", false},
		{"image volume at the beta-off line", FeatureImageVolume, "v1.33.1", false},
		{"image volume at the alpha line", FeatureImageVolume, "v1.31.0", false},
		{"image volume vendor suffix", FeatureImageVolume, "v1.35.5-kind.1", true},
		// Versions nobody can read never unlock either capability.
		{"image volume unparseable", FeatureImageVolume, "not-a-version", false},
		{"image volume empty", FeatureImageVolume, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				SupportsFeature(&Version{GitVersion: tc.gitVersion}, tc.feature))
		})
	}

	assert.False(t, SupportsFeature(nil, FeatureNativeSidecar),
		"no version never unlocks a capability whose floor cannot be decided")
	assert.False(t, SupportsFeature(nil, FeatureImageVolume),
		"no version never unlocks a capability whose floor cannot be decided")
}
