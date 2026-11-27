package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

func TestModelDeploymentForgerHandlerAndTable(t *testing.T) {
	_, hasStatus := any(&worker.ModelDeploymentForger{}).(extensionapi.ObjectWithStatusSubResource)
	assert.False(t, hasStatus, "v1 ModelDeploymentForger must serve no writable status subresource")

	h := new(ModelDeploymentForgerHandler)
	assert.IsType(t, &worker.ModelDeploymentForger{}, h.New())
	assert.IsType(t, &worker.ModelDeploymentForgerList{}, h.NewList())
	assert.IsType(t, &workercore.ModelDeploymentForgerList{}, h.NewListForProxy())

	public := &worker.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-forger"},
	}
	coreObj := h.CastObjectTo(public)
	require.NotNil(t, coreObj)
	assert.Equal(t, "qwen-forger", coreObj.Name)
	back := h.CastObjectFrom(context.Background(), coreObj)
	require.NotNil(t, back)
	assert.Equal(t, "qwen-forger", back.Name)

	table, err := modelDeploymentForgerTable()
	require.NoError(t, err)

	forger := &worker.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-forger"},
		Spec: workercore.ModelDeploymentForgerSpec{
			TargetRef: core.LocalObjectReference{Name: "qwen-pd"},
			Mode:      workercore.ModelDeploymentForgerModeActive,
		},
		Status: workercore.ModelDeploymentForgerStatus{
			Conditions: []gpustack.Condition{
				{Type: workercore.ModelDeploymentForgerConditionTypeBound, Status: meta.ConditionTrue},
				{Type: workercore.ModelDeploymentForgerConditionTypeActive, Status: meta.ConditionTrue},
				{Type: workercore.ModelDeploymentForgerConditionTypeBlocked, Status: meta.ConditionFalse},
				{Type: workercore.ModelDeploymentForgerConditionTypeReady, Status: meta.ConditionTrue},
			},
		},
	}

	got, err := table.ConvertToTable(context.Background(), forger, nil)
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)

	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, strings.TrimSpace(printCell(c)))
	}
	assert.Equal(t, []string{
		"qwen-forger",
		"qwen-pd",
		"Active",
		"True",
		"True",
		"False",
		"True",
		"<unknown>",
	}, cells)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{
		"Name", "Target", "Mode", "Bound", "Active", "Blocked", "Ready", "Age",
	}, headers)
}
