package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

func Test_getKueueChartTemplateValues(t *testing.T) {
	data := map[string]any{
		"Env": []core.EnvVar{
			{
				Name:  "NO_PROXY",
				Value: "127.0.0.1",
			},
		},
		"ManagedLabel":       "gpustack.ai/managed",
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      nodefeature.GetKnownAcceleratableManufacturers(),
		"Namespace":          SystemNamespaceName,
		"ImagePullPolicy":    "Always",
	}
	funcMap := extendKueueChartValuesTemplateFuncMap()

	values := getKueueChartTemplateValues("kueue", data, funcMap)
	v, err := values.GetValues(t.Context())
	assert.NoError(t, err, "get kueue chart template values")
	t.Logf("Rendered kueue chart values:\n%s", string(funcx.MustNoError(yaml.Marshal(v))))
}

// kueueTransformation mirrors a Kueue config.ResourceTransformation entry as
// rendered into managerConfig.controllerManagerConfigYaml.
type kueueTransformation struct {
	Input      string            `yaml:"input"`
	Strategy   string            `yaml:"strategy"`
	MultiplyBy string            `yaml:"multiplyBy"`
	Outputs    map[string]string `yaml:"outputs"`
}

// renderKueueConfigYAML renders the Kueue chart values for the given manufacturers
// and returns the embedded controller-manager Configuration YAML.
func renderKueueConfigYAML(t *testing.T, manufacturers []string) string {
	t.Helper()

	data := map[string]any{
		"Manufacturers":   manufacturers,
		"Namespace":       SystemNamespaceName,
		"ImagePullPolicy": "Always",
	}
	values := getKueueChartTemplateValues("kueue", data, extendKueueChartValuesTemplateFuncMap())
	v, err := values.GetValues(t.Context())
	require.NoError(t, err, "get kueue chart template values")

	// managerConfig.controllerManagerConfigYaml is an embedded YAML string.
	var top struct {
		ManagerConfig struct {
			ControllerManagerConfigYaml string `yaml:"controllerManagerConfigYaml"`
		} `yaml:"managerConfig"`
	}
	require.NoError(t, yaml.Unmarshal(funcx.MustNoError(yaml.Marshal(v)), &top))
	return top.ManagerConfig.ControllerManagerConfigYaml
}

// renderKueueTransformations renders the Kueue chart values for the given
// manufacturers and returns the credits transformation rules keyed by input.
func renderKueueTransformations(t *testing.T, manufacturers []string) map[string]kueueTransformation {
	t.Helper()

	var cfg struct {
		Resources struct {
			Transformations []kueueTransformation `yaml:"transformations"`
		} `yaml:"resources"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(renderKueueConfigYAML(t, manufacturers)), &cfg))

	byInput := make(map[string]kueueTransformation, len(cfg.Resources.Transformations))
	for _, tr := range cfg.Resources.Transformations {
		byInput[tr.Input] = tr
	}
	return byInput
}

// Test_kueueChartTransformations pins the three per-manufacturer credits rules on
// the integer credit base B = D = 1600000: exclusive→B, shared→B/10, and the sliced
// rule folds in the card count via multiplyBy: <.sliced> with factor B/D = 1, so
// credits = B×C×U/partitions stays integer-valued and Kueue's ResourceValue int64
// ceil never rounds a fractional credit up to 1.
func Test_kueueChartTransformations(t *testing.T) {
	const manu = nodefeature.ManufacturerNVIDIA
	var (
		exclusive  = string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeExclusive))
		shared     = string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeShared))
		sliced     = string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeSliced))
		slicedUnit = string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manu))
		credits    = string(nodefeature.GetAcceleratableCreditsResourceName(manu))
	)

	byInput := renderKueueTransformations(t, []string{manu})

	cases := []struct {
		name           string
		input          string
		wantMultiplyBy string
		wantCredits    string
	}{
		{
			name:        "exclusive whole card is B credits",
			input:       exclusive,
			wantCredits: "1600000",
		},
		{
			name:        "shared ownership is B/10 credits",
			input:       shared,
			wantCredits: "160000",
		},
		{
			// credits = .sliced.units × (B/D) × .sliced = B×C×U/partitions, e.g.
			// 1/8 single card → 200000, 2 cards ×1/8 → 400000, 1/4 single → 400000,
			// 1/512 single → 3125. With B=D the factor is exactly 1, so the
			// .sliced.units value is itself the credit value. Kueue performs the
			// multiplication; this test pins the wiring (multiplyBy + factor).
			name:           "sliced unit folds the card count with factor B/D",
			input:          slicedUnit,
			wantMultiplyBy: sliced,
			wantCredits:    "1",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			tr, ok := byInput[c.input]
			require.True(t, ok, "transformation for input %q must exist", c.input)
			assert.Equal(t, "Replace", tr.Strategy, "strategy")
			assert.Equal(t, c.wantMultiplyBy, tr.MultiplyBy, "multiplyBy")
			assert.Equal(t, c.wantCredits, tr.Outputs[credits], "credits output")
		})
	}

	// .sliced is the multiplyBy above; Kueue does not consume a multiplyBy resource
	// on Replace, so it leaks into the Workload. It no longer needs a drop rule (nor
	// do the node-level .sliced.* counters) — the queue's quotaCheckStrategy:
	// IgnoreUndeclared ignores any resource the CQ does not cover, including this leak.
	_, dropped := byInput[sliced]
	assert.False(t, dropped, ".sliced must not carry a drop transformation (IgnoreUndeclared handles the leak)")
	assert.Len(t, byInput, len(cases), "only the three credits rules remain")
}

// Test_kueueChartQuotaCheckStrategy pins the admission escape hatch that keeps
// Instances admissible. A single-dimension ClusterQueue covers only cpu (general)
// or credits (accelerated), yet every Instance's Workload also carries
// cpu/memory/ephemeral-storage the queue does not cover; without IgnoreUndeclared
// Kueue refuses to assign a flavor for those and the Workload never admits.
func Test_kueueChartQuotaCheckStrategy(t *testing.T) {
	// Assert on the no-manufacturer rendering: a CPU-only pool exists even with no
	// accelerators, and it is where the memory/storage leak bites — so the strategy
	// must be emitted unconditionally, not only under the manufacturers guard.
	var cfg struct {
		FeatureGates struct {
			QuotaCheckStrategy bool `yaml:"QuotaCheckStrategy"`
		} `yaml:"featureGates"`
		Resources struct {
			QuotaCheckStrategy string `yaml:"quotaCheckStrategy"`
		} `yaml:"resources"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(renderKueueConfigYAML(t, nil)), &cfg))

	assert.True(t, cfg.FeatureGates.QuotaCheckStrategy, "QuotaCheckStrategy feature gate must be enabled")
	assert.Equal(t, "IgnoreUndeclared", cfg.Resources.QuotaCheckStrategy, "quotaCheckStrategy must be IgnoreUndeclared")
}
