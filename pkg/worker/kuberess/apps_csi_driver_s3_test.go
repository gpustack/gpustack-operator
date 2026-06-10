package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	yaml "gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

func Test_getCSIDriverS3ChartTemplateValues(t *testing.T) {
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
		"Manufacturers":      nodefeature.GetKnownAcceleratableManufacturers(),
		"Namespace":          SystemNamespaceName,
		"ImagePullPolicy":    "Always",
		"DriverName":         CSIProvisionerS3,
	}

	values := getCSIDriverS3ChartTemplateValues("csi-driver-s3", data)
	v, err := values.GetValues(t.Context())
	assert.NoError(t, err, "render csi-driver-s3 chart template values")
	t.Logf("Rendered csi-driver-s3 chart values:\n%s", string(funcx.MustNoError(yaml.Marshal(v))))
}
