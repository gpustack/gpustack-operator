package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	yaml "gopkg.in/yaml.v3"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

func Test_getKueueChartTemplateValues(t *testing.T) {
	data := map[string]any{
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      devicefeature.GetKnownManufacturers(),
		"Namespace":          "gpustack-toolkit-system",
	}
	funcMap := extendKueueChartValuesTemplateFuncMap()

	values := getKueueChartTemplateValues("kueue", data, funcMap)
	v, err := values.GetValues(t.Context())
	assert.NoError(t, err, "get kueue chart template values")
	t.Logf("Rendered kueue chart values:\n%s", string(funcx.MustNoError(yaml.Marshal(v))))
}
