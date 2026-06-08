package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/devicemanager"
)

func Test_renderGPUStackDeviceManagerApplyYamlTemplate(t *testing.T) {
	data := map[string]any{
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      devicefeature.GetKnownManufacturers(),
		"Namespace":          SystemNamespaceName,
		"Image":              "",
		"ImagePullPolicy":    "Always",
		"Env": []core.EnvVar{
			{
				Name:  "ENV1",
				Value: "VALUE1",
			},
			{
				Name: "ENV2",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: "secret-name"},
						Key:                  "secret-key",
					},
				},
			},
		},
		"Version":    "dev",
		"SecurePort": devicemanager.NewOptions().ServerOptions.BindPort,
	}
	funcMap := extendDeviceManagerApplyYamlTemplateFuncMap()

	yaml, err := renderDeviceManagerApplyYamlTemplate(data, funcMap)
	assert.NoError(t, err, "render device manager apply yaml template")
	t.Logf("Rendered device manager apply yaml:\n%s", yaml)
}
