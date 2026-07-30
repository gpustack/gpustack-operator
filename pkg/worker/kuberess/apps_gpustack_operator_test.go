package kuberess

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// operatorChartValuesContext returns the context installGPUStackOperator builds, with the
// given overrides applied.
func operatorChartValuesContext(overrides map[string]any) map[string]any {
	manufacturers := nodefeature.GetKnownAcceleratableManufacturers()
	manufacturerIdentities := make(map[string]map[string]string, len(manufacturers))
	for _, m := range manufacturers {
		manufacturerIdentities[m] = manufacturerIdentity(m)
	}

	data := map[string]any{
		"ContainerRegistry":      "",
		"ContainerNamespace":     "",
		"ImagePullSecrets":       []string(nil),
		"ImagePullPolicy":        "",
		"ImageRepository":        "",
		"ImageTag":               "",
		"ManufacturerIdentities": manufacturerIdentities,
		"ComponentSwitches":      componentSwitches(sets.New[string]()),
		"Namespace":              SystemNamespaceName,
		"Release":                gpustackOperatorReleaseName,
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
		name                string
		data                map[string]any
		expectImageOverride bool
		// wantRegistry is the global.imageRegistry expected, or "" for the key being absent.
		wantRegistry string
		// wantNamespaceKey is whether global.imageNamespace is emitted at all, which is a
		// different question from its value: it is withheld exactly where it could rewrite a
		// pinned image, and emitted (empty) where nothing is pinned.
		wantNamespaceKey bool
		expectPullPolicy string
	}{
		{
			name: "compose image from settings",
			data: operatorChartValuesContext(map[string]any{
				"ImagePullSecrets": []string{"abc", "def"},
				"ImagePullPolicy":  "Always",
			}),
			expectImageOverride: false,
			wantNamespaceKey:    true,
			expectPullPolicy:    "Always",
		},
		{
			name: "mirror settings with a chart-composed image",
			data: operatorChartValuesContext(map[string]any{
				"ContainerRegistry":  "reg.local",
				"ContainerNamespace": "mirror",
			}),
			expectImageOverride: false,
			wantRegistry:        "reg.local",
			wantNamespaceKey:    true,
		},
		{
			name: "reuse running worker image",
			data: operatorChartValuesContext(map[string]any{
				"ImageRepository": "registry.example.com/myns/gpustack-operator",
				"ImageTag":        "v0.5.0",
			}),
			expectImageOverride: true,
			wantNamespaceKey:    false,
		},
		{
			// The regression this pair of expectations exists for: image mode has no
			// user-values channel, so a registry withheld here leaves every subchart pulling
			// from wherever its own reference points — the airgap gone, silently. The
			// namespace still cannot travel, because it would rewrite the operator image
			// this same overlay pinned to the running worker.
			name: "mirror settings while reusing the running worker image",
			data: operatorChartValuesContext(map[string]any{
				"ContainerRegistry":  "reg.local",
				"ContainerNamespace": "mirror",
				"ImageRepository":    "reg.local/team/gpustack-operator",
				"ImageTag":           "v0.7.0",
			}),
			expectImageOverride: true,
			wantRegistry:        "reg.local",
			wantNamespaceKey:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			values := renderOperatorChartValues(t, tc.data)

			// The worker performing the install is already running.
			assert.Equal(t, "gpustack-operator", values["fullnameOverride"])
			worker, _ := values["worker"].(map[string]any)
			assert.Equal(t, false, worker["enabled"], "worker disabled")

			// The image is either pinned to the running worker image or composed by the chart. It is
			// set on the chart's own image knob, which every component that runs this image reads
			// through the same merging helper — the device-managers and the migration hook Jobs
			// alike, the latter being what fails the install when it resolves a tag that does not
			// exist wherever this build came from.
			if tc.expectImageOverride {
				image, _ := values["image"].(map[string]any)
				assert.Equal(t, tc.data["ImageRepository"], image["repository"])
				assert.Equal(t, tc.data["ImageTag"], image["tag"])
			} else {
				assert.Nil(t, values["image"], "no chart-level image override")
			}
			deviceManager, _ := values["deviceManager"].(map[string]any)
			assert.Nil(t, deviceManager["image"], "device-managers inherit the chart image, never their own")

			global, _ := values["global"].(map[string]any)
			// Every manufacturer carries its identity, on the channel the subcharts read.
			manus, _ := global["manufacturers"].(map[string]any)
			assert.Len(t, manus, len(nodefeature.GetKnownAcceleratableManufacturers()),
				"all manufacturers rendered")
			require.NotNil(t, global, "global block rendered")
			if tc.wantRegistry != "" {
				assert.Equal(t, tc.wantRegistry, global["imageRegistry"],
					"the registry reaches the subcharts whatever the operator image is")
			} else {
				assert.NotContains(t, global, "imageRegistry", "no registry configured")
			}
			if tc.wantNamespaceKey {
				assert.Contains(t, global, "imageNamespace")
			} else {
				assert.NotContains(t, global, "imageNamespace",
					"the namespace would rewrite the pinned worker image")
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
	}, ApplicationNames())
}

func Test_isReleaseHeldByPeer(t *testing.T) {
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
			name: "the peer won the race to create the release record",
			err:  fmt.Errorf("helm install: release gpustack-operator-device-manager: %w", helmdriver.ErrReleaseExists),
			want: true,
		},
		{
			name: "the peer's own operation is still running",
			err:  errors.New("helm upgrade: release gpustack-operator-device-manager: another operation (install/upgrade/rollback) is in progress"),
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
			assert.Equal(t, tc.want, isReleaseHeldByPeer(tc.err))
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
