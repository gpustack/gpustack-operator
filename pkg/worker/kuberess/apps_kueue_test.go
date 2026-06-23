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

// renderKueueTransformations renders the Kueue chart values for the given
// manufacturers and returns the credits transformation rules keyed by input.
func renderKueueTransformations(t *testing.T, manufacturers []string) map[string]kueueTransformation {
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

	var cfg struct {
		Resources struct {
			Transformations []kueueTransformation `yaml:"transformations"`
		} `yaml:"resources"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(top.ManagerConfig.ControllerManagerConfigYaml), &cfg))

	byInput := make(map[string]kueueTransformation, len(cfg.Resources.Transformations))
	for _, tr := range cfg.Resources.Transformations {
		byInput[tr.Input] = tr
	}
	return byInput
}

// Test_kueueChartTransformations pins Task 9: the three per-manufacturer credits
// rules. The sliced rule folds in the card count via multiplyBy: <.sliced> with
// the single global factor 1/D = 1/12800, so credits = C×U/partitions.
func Test_kueueChartTransformations(t *testing.T) {
	const manu = nodefeature.ManufacturerNVIDIA
	var (
		exclusive  = string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeExclusive))
		shared     = string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeShared))
		slicedUnit = string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manu))
		slicedCard = string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeSliced))
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
			name:        "exclusive whole card is one credit",
			input:       exclusive,
			wantCredits: "1",
		},
		{
			name:        "shared ownership is a tenth of a credit",
			input:       shared,
			wantCredits: "0.1",
		},
		{
			// credits = .sliced.units × (1/12800) × .sliced = C×U/partitions, e.g.
			// 1/8 single card → 0.125, 2 cards ×1/8 → 0.25, 1/4 single → 0.25,
			// 1/512 single → 0.001953125. Kueue performs the multiplication; this
			// test pins the wiring (multiplyBy + factor) that produces those values.
			name:           "sliced unit folds the card count with factor 1/D",
			input:          slicedUnit,
			wantMultiplyBy: slicedCard,
			wantCredits:    "0.000078125",
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

	// The bare .sliced card key is only a multiplier, never a transformation input.
	_, ok := byInput[slicedCard]
	assert.False(t, ok, ".sliced must not be a transformation input")
}
