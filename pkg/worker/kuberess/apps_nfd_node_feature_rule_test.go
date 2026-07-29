package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	nfdv1alpha1 "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
	kyaml "sigs.k8s.io/yaml"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// TestCPUInfoNodeFeatureRule pins the two lists the rule matches accelerators on: the PCI
// vendor IDs of the manufacturers this worker manages, and the PCI device classes
// pkg/nodefeature calls acceleratable. A node matching neither is never classified, so the
// scheduling chain starts or stops here.
//
// The render is decoded into NFD's own types, strictly, so a misspelled field fails the test
// rather than the cluster.
func TestCPUInfoNodeFeatureRule(t *testing.T) {
	testCases := []struct {
		name          string
		manufacturers []string
		wantVendorIDs []string
	}{
		{
			name:          "every known manufacturer",
			manufacturers: nodefeature.GetKnownAcceleratableManufacturers(),
			wantVendorIDs: nodefeature.GetAcceleratablePciVendorIDs(),
		},
		{
			name:          "a single manufacturer",
			manufacturers: []string{nodefeature.ManufacturerNVIDIA},
			wantVendorIDs: []string{"10de"},
		},
		{
			name: "repeats and unknown names collapse",
			manufacturers: []string{
				nodefeature.ManufacturerNVIDIA, nodefeature.ManufacturerNVIDIA, "not-a-manufacturer",
			},
			wantVendorIDs: []string{"10de"},
		},
	}

	wantClasses := make([]string, 0, len(nodefeature.GetAcceleratablePciClassPrefixes()))
	for _, prefix := range nodefeature.GetAcceleratablePciClassPrefixes() {
		wantClasses = append(wantClasses, "^"+prefix)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			content, err := cpuInfoNodeFeatureRule(tc.manufacturers)
			require.NoError(t, err, "render the rule")

			var rule nfdv1alpha1.NodeFeatureRule
			require.NoError(t, kyaml.UnmarshalStrict([]byte(content), &rule),
				"the render is a NodeFeatureRule NFD accepts")
			assert.Equal(t, cpuInfoNodeFeatureRuleName, rule.Name)

			var vendors, classes []string
			for _, r := range rule.Spec.Rules {
				for _, term := range r.MatchFeatures {
					if term.Feature != "pci.device" {
						continue
					}
					require.NotNil(t, term.MatchExpressions, "the pci.device term matches on expressions")
					vendor, ok := (*term.MatchExpressions)["vendor"]
					require.True(t, ok, "the pci.device term matches a vendor")
					assert.Equal(t, nfdv1alpha1.MatchIn, vendor.Op)
					vendors = vendor.Value

					class, ok := (*term.MatchExpressions)["class"]
					require.True(t, ok, "the pci.device term matches a class")
					assert.Equal(t, nfdv1alpha1.MatchInRegexp, class.Op)
					classes = class.Value
				}
			}

			assert.Equal(t, tc.wantVendorIDs, vendors, "the managed manufacturers' vendor IDs")
			assert.Equal(t, wantClasses, classes, "the acceleratable device classes")
		})
	}
}
