package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func Test_renderGPUStackDeviceManagerApplyYamlTemplate(t *testing.T) {
	data := map[string]any{
		"Env": []core.EnvVar{
			{
				Name:  "NO_PROXY",
				Value: "127.0.0.1",
			},
			{
				Name: "SECRET_KEY",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: "secret-name"},
						Key:                  "secret-key",
					},
				},
			},
		},
		"ManagedLabel":       "gpustack.ai/managed",
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      nodefeature.GetKnownManufacturers(),
		"Namespace":          SystemNamespaceName,
		"Image":              "",
		"ImagePullPolicy":    "Always",
		"Version":            "dev",
		"SecurePort":         devicemanager.NewOptions().ServerOptions.BindPort,
	}
	funcMap := extendDeviceManagerApplyYamlTemplateFuncMap()

	yaml, err := renderDeviceManagerApplyYamlTemplate(data, funcMap)
	assert.NoError(t, err, "render device manager apply yaml template")
	t.Logf("Rendered device manager apply yaml:\n%s", yaml)
}
