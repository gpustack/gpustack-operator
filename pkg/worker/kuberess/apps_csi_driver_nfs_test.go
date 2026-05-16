package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	yaml "gopkg.in/yaml.v3"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

func Test_getCSIDriverNFSChartTemplateValues(t *testing.T) {
	data := map[string]any{
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      devicefeature.GetKnownManufacturers(),
		"Namespace":          "gpustack-system",
		"Image":              "",
		"ImagePullPolicy":    "IfNotPresent",
	}

	values := getCSIDriverNFSChartTemplateValues("csi-driver-nfs", data)
	v, err := values.GetValues(t.Context())
	assert.NoError(t, err, "render csi-driver-nfs chart template values")
	t.Logf("Rendered csi-driver-nfs chart values:\n%s", string(funcx.MustNoError(yaml.Marshal(v))))
}
