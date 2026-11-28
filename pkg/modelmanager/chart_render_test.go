package modelmanager

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelmChartRendersModelManagerIngestAndRBAC(t *testing.T) {
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not in PATH")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartDir := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "gpustack-operator", "chart"))

	render := func(t *testing.T, extraArgs ...string) string {
		t.Helper()
		args := append([]string{"template", "rel", chartDir, "--kube-version", "1.31.0"}, extraArgs...)
		out, rerr := exec.Command(helmBin, args...).CombinedOutput()
		require.NoError(t, rerr, string(out))
		return string(out)
	}

	t.Run("worker deployment mounts projected gpustack-model-ingest token", func(t *testing.T) {
		doc := render(t, "--show-only", "templates/worker/deployment.yaml")
		assert.Contains(t, doc, "mountPath: /var/run/secrets/gpustack-model-ingest")
		assert.Contains(t, doc, "audience: gpustack-model-ingest")
		assert.Contains(t, doc, "expirationSeconds: 3600")
		assert.Contains(t, doc, "path: token")
	})

	t.Run("model-manager daemonset templates --ingest-service-account from release fullname without extra hostPath", func(t *testing.T) {
		doc := render(t, "--set", "fullnameOverride=custom-op", "--show-only", "templates/model-manager/daemonset.yaml")
		assert.Contains(t, doc, "--ingest-service-account=custom-op-worker")
		assert.NotContains(t, doc, "--ingest-service-account=gpustack-operator-worker")
	})

	t.Run("model-manager ClusterRole grants pods tokenreviews modelkernelartifacts and modeldeploymentforgers unconditionally", func(t *testing.T) {
		doc := render(t,
			"--set", "modelTransfer.enabled=false",
			"--set", "modelManager.port=0",
			"--show-only", "templates/model-manager/serviceaccount.yaml",
		)
		assert.Contains(t, doc, `resources: ["pods"]`)
		assert.Contains(t, doc, `resources: ["tokenreviews"]`)
		assert.Contains(t, doc, `"modelkernelartifacts"`)
		assert.Contains(t, doc, `resources: ["modeldeploymentforgers"]`)
		assert.NotContains(t, doc, "secrets")
	})
}
