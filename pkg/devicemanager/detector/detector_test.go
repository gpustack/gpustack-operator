package detector

import (
	"strings"
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
// It also pins what the detector STRIPS from the flavor's NodeLabels: gpustack.ai/managed and the
// general(CPU) key (both worker-owned — see worker.TestNodeDevicesControlLabels for the mirror) plus
// the .count sizing pin, leaving only the accelerator selector keys + os/arch.
func TestAcceleratableDevicesSelectorLabels(t *testing.T) {
	const featKey = nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"

	// A node NFD has not yet labeled with the accelerator feature (only the stable os/arch). It is
	// managed, but that mark lives on the flavor's NodeLabels (ExtractNodeFlavors stamps
	// gpustack.ai/managed=true) and must be stripped here — the worker's NodeDevicesReconciler owns it.
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{
		core.LabelOSStable:         "linux",
		core.LabelArchStable:       "amd64",
		systemname.ManagedLabelKey: "true",
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
			// Even when the pass resolves a REAL CPU key (vendor + family/id present), the paired
			// general(CPU) key must be filtered out — the worker owns the CPU key on the Devices.
			name: "a resolved real CPU key is still filtered out",
			published: map[string]string{
				featKey:            "true",
				featKey + ".count": "2",
				"feature.node.kubernetes.io/cpu-model.vendor_id": "AuthenticAMD",
				"feature.node.kubernetes.io/cpu-model.family":    "25",
				"feature.node.kubernetes.io/cpu-model.id":        "1",
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
			want:      nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := acceleratableDevicesSelectorLabels(node, c.published)
			assert.Equal(t, c.want, got)
			assert.NotContains(t, got, systemname.ManagedLabelKey,
				"gpustack.ai/managed (stamped on the flavor's NodeLabels) is stripped — the worker owns it")
			for k := range got {
				assert.False(t, strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix),
					"no general(CPU) key survives — the worker owns it on the Devices")
			}
		})
	}
}
