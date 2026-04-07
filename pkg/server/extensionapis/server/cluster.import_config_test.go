package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/worker"
)

// Check that the cluster import config template can be rendered with the expected data.
func Test_renderClusterImportConfigTemplate(t *testing.T) {
	data := map[string]any{
		"ContainerRegistry":  "",
		"ContainerNamespace": "",
		"Namespace":          "gpustack-system",
		"Image":              "",
		"ImagePullPolicy":    "IfNotPresent",
		"Version":            "dev",
		"ClusterType":        "Loopback",
		"ServerURL":          "https://127.0.0.1",
		"Token":              "DUMMY_TOKEN",
		"Team":               "default",
		"Cluster":            "local",
		"SecurePort":         worker.NewOptions().BindPort,
	}

	cfg, err := renderClusterImportConfigTemplate(data)
	assert.NoError(t, err, "render cluster import config template")
	t.Logf("Rendered cluster import config:\n%s", cfg)
}
