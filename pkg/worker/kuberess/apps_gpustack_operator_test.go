package kuberess

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// operatorChartValuesContext returns the context installGPUStackOperator builds, with the
// given overrides applied.
func operatorChartValuesContext(overrides map[string]any) map[string]any {
	manufacturers := nodefeature.GetKnownAcceleratableManufacturers()
	manufacturerVendorIDs := make(map[string]string, len(manufacturers))
	for _, m := range manufacturers {
		manufacturerVendorIDs[m] = nodefeature.GetPciVendorID(m)
	}

	data := map[string]any{
		"ContainerRegistry":     "",
		"ContainerNamespace":    "",
		"ImagePullSecrets":      []string(nil),
		"ImagePullPolicy":       "",
		"ImageRepository":       "",
		"ImageTag":              "",
		"ManufacturerVendorIDs": manufacturerVendorIDs,
		"ComponentSwitches":     componentSwitches(sets.New[string]()),
		"Namespace":             SystemNamespaceName,
		"Release":               gpustackOperatorReleaseName,
	}
	for k, v := range overrides {
		data[k] = v
	}

	return data
}

func renderOperatorChartValues(t *testing.T, data map[string]any) map[string]any {
	t.Helper()

	values, err := getGPUStackOperatorChartTemplateValues(gpustackOperatorChartName, data).
		GetValues(context.Background())
	assert.NoError(t, err, "render operator chart values")

	return values
}

func Test_getGPUStackOperatorChartTemplateValues(t *testing.T) {
	testCases := []struct {
		name                 string
		data                 map[string]any
		expectImageOverride  bool
		expectGlobalRegistry bool
		expectPullPolicy     string
	}{
		{
			name: "compose image from settings",
			data: operatorChartValuesContext(map[string]any{
				"ImagePullSecrets": []string{"abc", "def"},
				"ImagePullPolicy":  "Always",
			}),
			expectImageOverride:  false,
			expectGlobalRegistry: true,
			expectPullPolicy:     "Always",
		},
		{
			name: "reuse running worker image",
			data: operatorChartValuesContext(map[string]any{
				"ImageRepository": "registry.example.com/myns/gpustack-operator",
				"ImageTag":        "v0.5.0",
			}),
			expectImageOverride:  true,
			expectGlobalRegistry: false,
			expectPullPolicy:     "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			values := renderOperatorChartValues(t, tc.data)

			// The worker performing the install is already running.
			assert.Equal(t, "gpustack-operator", values["fullnameOverride"])
			worker, _ := values["worker"].(map[string]any)
			assert.Equal(t, false, worker["enabled"], "worker disabled")

			// Every manufacturer is mapped to its PCI vendor ID.
			manus, _ := values["manufacturers"].(map[string]any)
			assert.Len(t, manus, len(nodefeature.GetKnownAcceleratableManufacturers()),
				"all manufacturers rendered")

			// The image is either pinned to the running worker image or composed by the chart.
			deviceManager, _ := values["deviceManager"].(map[string]any)
			if tc.expectImageOverride {
				image, _ := deviceManager["image"].(map[string]any)
				assert.Equal(t, "registry.example.com/myns/gpustack-operator", image["repository"])
				assert.Equal(t, "v0.5.0", image["tag"])
			} else {
				assert.Nil(t, deviceManager["image"], "no device-manager image override")
			}

			global, _ := values["global"].(map[string]any)
			if tc.expectGlobalRegistry {
				assert.NotNil(t, global, "global block rendered")
				assert.Contains(t, global, "imageRegistry")
				assert.Contains(t, global, "imageNamespace")
			} else {
				assert.NotContains(t, global, "imageRegistry",
					"no registry override when reusing the worker image")
			}

			// The pull policy must reach the subcharts, which only global.imagePullPolicy does.
			if tc.expectPullPolicy != "" {
				assert.Equal(t, tc.expectPullPolicy, global["imagePullPolicy"])
			} else {
				assert.NotContains(t, global, "imagePullPolicy",
					"each component keeps its own pull policy when none is configured")
			}
		})
	}
}

// Test_gpustackOperatorChartTemplateSwitchesEveryApplication guards the overlay against the
// name table drifting away from it: every application name --disable-applications accepts
// must render a switch that follows the disable set.
func Test_gpustackOperatorChartTemplateSwitchesEveryApplication(t *testing.T) {
	for name, key := range applicationValuesKeys {
		t.Run(name, func(t *testing.T) {
			enabled := renderOperatorChartValues(t, operatorChartValuesContext(nil))
			disabled := renderOperatorChartValues(t, operatorChartValuesContext(map[string]any{
				"ComponentSwitches": componentSwitches(sets.New(name)),
			}))

			for _, tc := range []struct {
				values map[string]any
				want   bool
			}{{enabled, true}, {disabled, false}} {
				block, ok := tc.values[key].(map[string]any)
				assert.True(t, ok, "the overlay renders a %q block", key)
				assert.Equal(t, tc.want, block["enabled"], "%q switch", key)
			}
		})
	}
}

func Test_componentSwitches(t *testing.T) {
	allEnabled := map[string]bool{
		"kueue":                  true,
		"node-feature-discovery": true,
		"nodeFeatureRule":        true,
		"csi-driver-nfs":         true,
		"csi-driver-s3":          true,
		"deviceManager":          true,
	}

	testCases := []struct {
		name    string
		disable sets.Set[string]
		want    map[string]bool
	}{
		{
			name:    "nothing disabled",
			disable: sets.New[string](),
			want:    allEnabled,
		},
		{
			name:    "both CSI drivers disabled",
			disable: sets.New("csi-driver-nfs", "csi-driver-s3"),
			want: map[string]bool{
				"kueue":                  true,
				"node-feature-discovery": true,
				"nodeFeatureRule":        true,
				"csi-driver-nfs":         false,
				"csi-driver-s3":          false,
				"deviceManager":          true,
			},
		},
		{
			// kubeapp.ExecuteInstall short-circuits on the wildcard, so the installer
			// never runs and never renders these switches.
			name:    "the wildcard is not a component and switches nothing off",
			disable: sets.New(applicationWildcard),
			want:    allEnabled,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, componentSwitches(tc.disable))
		})
	}
}

func Test_ApplicationNames(t *testing.T) {
	assert.Equal(t, []string{
		"*",
		"csi-driver-nfs",
		"csi-driver-s3",
		"device-manager",
		"kueue",
		"node-feature-discovery",
		"node-feature-rule",
	}, ApplicationNames())
}

func Test_isReleaseNameTaken(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "no error",
			err:  nil,
			want: false,
		},
		{
			name: "helm refuses the name a concurrent replica already created",
			err:  errors.New("helm install: release gpustack-operator-device-manager: cannot re-use a name that is still in use"),
			want: true,
		},
		{
			name: "any other failure is the caller's to report",
			err:  errors.New("helm install: release gpustack-operator-device-manager: timed out waiting for the condition"),
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isReleaseNameTaken(tc.err))
		})
	}
}

func Test_splitImageReference(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	testCases := []struct {
		name           string
		ref            string
		wantRepository string
		wantTag        string
	}{
		{
			name:           "tagged reference",
			ref:            "registry.example.com/myns/gpustack-operator:v0.5.0",
			wantRepository: "registry.example.com/myns/gpustack-operator",
			wantTag:        "v0.5.0",
		},
		{
			name:           "tagged reference with registry port",
			ref:            "registry.example.com:5000/myns/gpustack-operator:v0.5.0",
			wantRepository: "registry.example.com:5000/myns/gpustack-operator",
			wantTag:        "v0.5.0",
		},
		{
			name:           "untagged reference keeps repository and defers tag to the chart",
			ref:            "registry.example.com/myns/gpustack-operator",
			wantRepository: "registry.example.com/myns/gpustack-operator",
			wantTag:        "",
		},
		{
			name:           "digest reference falls back to the chart default image",
			ref:            "registry.example.com/myns/gpustack-operator@" + digest,
			wantRepository: "",
			wantTag:        "",
		},
		{
			name:           "empty reference falls back to the chart default image",
			ref:            "",
			wantRepository: "",
			wantTag:        "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repository, tag := splitImageReference(tc.ref)
			assert.Equal(t, tc.wantRepository, repository, "repository")
			assert.Equal(t, tc.wantTag, tag, "tag")
		})
	}
}
