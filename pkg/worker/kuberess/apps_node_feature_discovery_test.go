package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	yaml "gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

func Test_getNfdChartTemplateValues(t *testing.T) {
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
		"Manufacturers":      nodefeature.GetKnownManufacturers(),
		"Namespace":          SystemNamespaceName,
		"ImagePullPolicy":    "Always",
	}
	funcMap := extendNfdChartValuesTemplateFuncMap()

	values := getNfdChartTemplateValues("node-feature-discovery", data, funcMap)
	v, err := values.GetValues(t.Context())
	assert.NoError(t, err, "get nfd chart template values")
	t.Logf("Rendered nfd chart values:\n%s", string(funcx.MustNoError(yaml.Marshal(v))))
}
