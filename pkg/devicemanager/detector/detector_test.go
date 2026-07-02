package detector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
)

// TestAcceleratableDevicesSelectorLabels pins that the Devices selector labels are derived from the
// feature labels being published this pass, NOT read back off the node. The node here carries only
// the stable os/arch (NFD has not merged the accelerator feature labels yet), yet the feature key
// must still appear in the result — this guards the real-cluster regression where a freshly
// onboarded node's Devices stayed unstamped, so the three-view and AdmissionCheck could not find it.
func TestAcceleratableDevicesSelectorLabels(t *testing.T) {
	const featKey = nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"

	// A node NFD has not yet labeled with the accelerator feature (only the stable os/arch).
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{
		core.LabelOSStable:   "linux",
		core.LabelArchStable: "amd64",
	}}}

	cases := []struct {
		name      string
		published map[string]string
		want      map[string]string
	}{
		{
			name: "feature published this pass yields the selector labels",
			published: map[string]string{
				featKey:              "true",
				featKey + ".count":   "4",
				featKey + ".product": "Tesla-T4",
				featKey + ".memory":  "16Gi",
			},
			want: map[string]string{
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
				featKey:              "true",
			},
		},
		{
			name:      "nothing published yet yields no selector labels",
			published: map[string]string{},
			want:      map[string]string{},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := acceleratableDevicesSelectorLabels(node, c.published)
			assert.Equal(t, c.want, got)
			assert.NotContains(t, got, systemname.ManagedLabelKey, "managed mark is synced separately, not stamped here")
		})
	}
}
