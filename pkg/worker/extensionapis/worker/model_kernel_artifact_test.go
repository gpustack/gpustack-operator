package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

func TestModelKernelArtifactHandlerAndTable(t *testing.T) {
	_, hasStatus := any(&worker.ModelKernelArtifact{}).(extensionapi.ObjectWithStatusSubResource)
	assert.False(t, hasStatus, "v1 ModelKernelArtifact must serve no writable status subresource")

	h := new(ModelKernelArtifactHandler)
	assert.IsType(t, &worker.ModelKernelArtifact{}, h.New())
	assert.IsType(t, &worker.ModelKernelArtifactList{}, h.NewList())
	assert.IsType(t, &workercore.ModelKernelArtifactList{}, h.NewListForProxy())

	public := &worker.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-kernels"},
	}
	coreObj := h.CastObjectTo(public)
	require.NotNil(t, coreObj)
	assert.Equal(t, "qwen-kernels", coreObj.Name)
	back := h.CastObjectFrom(context.Background(), coreObj)
	require.NotNil(t, back)
	assert.Equal(t, "qwen-kernels", back.Name)

	table, err := modelKernelArtifactTable()
	require.NoError(t, err)

	digest := "sha256:" + strings.Repeat("a", 64)
	artifact := &worker.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-kernels"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest: digest,
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Architecture: "sm_90a",
			},
		},
		Status: workercore.ModelKernelArtifactStatus{
			Resolved: &workercore.ModelKernelArtifactResolved{SizeBytes: 4096},
			Nodes:    &workercore.ModelArtifactNodes{Ready: 2},
			Conditions: []gpustack.Condition{
				{Type: workercore.ModelKernelArtifactConditionTypeResolved, Status: meta.ConditionTrue},
			},
		},
	}

	got, err := table.ConvertToTable(context.Background(), artifact, nil)
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{
		"Name", "Source", "Engine", "Arch", "Size", "Ready", "Resolved", "Quarantined", "Age",
	}, headers)
}
