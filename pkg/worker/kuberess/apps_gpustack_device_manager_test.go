package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/devicemanager"
)

func Test_renderGPUStackDeviceManagerApplyYamlTemplate(t *testing.T) {
	data := map[string]any{
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      devicefeature.GetKnownManufacturers(),
		"Namespace":          "gpustack-system",
		"Image":              "",
		"ImagePullPolicy":    "IfNotPresent",
		"Version":            "dev",
		"SecurePort":         devicemanager.NewOptions().ServerOptions.BindPort,
	}
	funcMap := extendDeviceManagerApplyYamlTemplateFuncMap()

	yaml, err := renderDeviceManagerApplyYamlTemplate(data, funcMap)
	assert.NoError(t, err, "render device manager apply yaml template")
	t.Logf("Rendered device manager apply yaml:\n%s", yaml)
}
