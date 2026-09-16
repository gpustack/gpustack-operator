package kubediscovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSupportsFeature(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gitVersion string
		want       bool
	}{
		{"at the floor", "v1.29.0", true},
		{"above the floor", "v1.30.2", true},
		{"below the floor", "v1.28.15", false},
		{"the chart's own floor", "v1.23.17", false},
		{"vendor suffix", "v1.29.3-gke.1000", true},
		{"pre-release at the floor", "v1.29.0-rc.1", true},
		{"unparseable", "not-a-version", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				SupportsFeature(&Version{GitVersion: tc.gitVersion}, FeatureNativeSidecar))
		})
	}

	assert.False(t, SupportsFeature(nil, FeatureNativeSidecar),
		"no version never unlocks a capability whose floor cannot be decided")
}
