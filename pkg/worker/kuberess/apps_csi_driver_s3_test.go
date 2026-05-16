package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	yaml "gopkg.in/yaml.v3"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

func Test_getCSIDriverS3ChartTemplateValues(t *testing.T) {
	data := map[string]any{
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"ImagePullSecrets":   []string{"abc", "def"},
		"Manufacturers":      devicefeature.GetKnownManufacturers(),
		"Namespace":          "gpustack-system",
		"Image":              "",
		"ImagePullPolicy":    "IfNotPresent",
	}

	values := getCSIDriverS3ChartTemplateValues("csi-driver-s3", data)
	v, err := values.GetValues(t.Context())
	assert.NoError(t, err, "render csi-driver-s3 chart template values")
	t.Logf("Rendered csi-driver-s3 chart values:\n%s", string(funcx.MustNoError(yaml.Marshal(v))))
}
