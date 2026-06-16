package kuberess

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

func Test_getGPUStackDeviceManagerChartTemplateValues(t *testing.T) {
	manufacturers := nodefeature.GetKnownAcceleratableManufacturers()
	manufacturerVendorIDs := make(map[string]string, len(manufacturers))
	for _, m := range manufacturers {
		manufacturerVendorIDs[m] = nodefeature.GetPciVendorID(m)
	}

	testCases := []struct {
		name                 string
		data                 map[string]any
		expectImageOverride  bool
		expectGlobalRegistry bool
	}{
		{
			name: "compose image from settings",
			data: map[string]any{
				"ContainerRegistry":     "",
				"ContainerNamespace":    "",
				"ImagePullSecrets":      []string{"abc", "def"},
				"ImagePullPolicy":       "Always",
				"ImageRepository":       "",
				"ImageTag":              "",
				"ManufacturerVendorIDs": manufacturerVendorIDs,
				"Namespace":             SystemNamespaceName,
				"Release":               "gpustack-operator-device-manager",
			},
			expectImageOverride:  false,
			expectGlobalRegistry: true,
		},
		{
			name: "reuse running worker image",
			data: map[string]any{
				"ImagePullPolicy":       "IfNotPresent",
				"ImageRepository":       "registry.example.com/myns/gpustack-operator",
				"ImageTag":              "v0.5.0",
				"ManufacturerVendorIDs": manufacturerVendorIDs,
				"Namespace":             SystemNamespaceName,
				"Release":               "gpustack-operator-device-manager",
			},
			expectImageOverride:  true,
			expectGlobalRegistry: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			values, err := getGPUStackDeviceManagerChartTemplateValues("gpustack-operator", tc.data).
				GetValues(context.Background())
			assert.NoError(t, err, "render device manager chart values")

			// The worker is already running; only the device-managers must be rendered.
			assert.Equal(t, "gpustack-operator", values["fullnameOverride"])
			worker, _ := values["worker"].(map[string]any)
			assert.Equal(t, false, worker["enabled"], "worker disabled")
			deviceManager, _ := values["deviceManager"].(map[string]any)
			assert.Equal(t, true, deviceManager["enabled"], "device-manager enabled")

			// Every manufacturer is mapped to its PCI vendor ID.
			manus, _ := values["manufacturers"].(map[string]any)
			assert.Len(t, manus, len(manufacturers), "all manufacturers rendered")

			// The image is either pinned to the running worker image or composed by the chart.
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
				assert.Nil(t, global, "no global block when reusing the worker image")
			}
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
