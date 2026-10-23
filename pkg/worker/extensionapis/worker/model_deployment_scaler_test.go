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
)

// TestModelDeploymentScalerTableRendersTheColumns pins the columns a scaler is listed with.
//
// The three conditions are the ones a reader asks about before anything else: whether the
// scaler owns the deployment, whether it is allowed to act on it, and whether something stops
// it.
func TestModelDeploymentScalerTableRendersTheColumns(t *testing.T) {
	table, err := modelDeploymentScalerTable()
	require.NoError(t, err)

	scaler := &worker.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-scaler"},
		Spec: workercore.ModelDeploymentScalerSpec{
			TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "qwen-72b"},
			Mode:      workercore.ModelDeploymentScalerModeActive,
			Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
			},
		},
		Status: workercore.ModelDeploymentScalerStatus{
			Conditions: []gpustack.Condition{
				{Type: workercore.ModelDeploymentScalerConditionTypeBound, Status: meta.ConditionTrue},
				{Type: workercore.ModelDeploymentScalerConditionTypeActive, Status: meta.ConditionTrue},
				{Type: workercore.ModelDeploymentScalerConditionTypeBlocked, Status: meta.ConditionFalse},
			},
		},
	}
	got, err := table.ConvertToTable(context.Background(), scaler, nil)
	require.NoError(t, err)

	require.Len(t, got.Rows, 1)
	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, strings.TrimSpace(printCell(c)))
	}
	assert.Equal(t, []string{
		"qwen-scaler",
		"qwen-72b",
		"Active",
		"TokenThroughput",
		"True",
		"True",
		"False",
		"<unknown>",
	}, cells)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{
		"Name", "Target", "Mode", "Algorithm", "Bound", "Active", "Blocked", "Age",
	}, headers)
}

// TestModelDeploymentScalerHandlerServesThePublicKind pins that the handler answers with the
// public kind and not with the alpha one it stores.
//
// The two differ in their type identity, so a handler that returned the alpha kind would serve
// an object the public client cannot decode, and the object it listed would not be the object
// the public client asked for.
func TestModelDeploymentScalerHandlerServesThePublicKind(t *testing.T) {
	h := new(ModelDeploymentScalerHandler)

	assert.IsType(t, &worker.ModelDeploymentScaler{}, h.New())
	assert.IsType(t, &worker.ModelDeploymentScalerList{}, h.NewList())
	assert.IsType(t, &workercore.ModelDeploymentScalerList{}, h.NewListForProxy())
}

// TestModelDeploymentScalerHandlerCastsBetweenTheVersions pins that the cast between the two
// versions is a reinterpretation of the same object.
//
// A cast that copied would lose the identity the proxy keeps, and a cast in the wrong direction
// would store the public object in the alpha storage.
func TestModelDeploymentScalerHandlerCastsBetweenTheVersions(t *testing.T) {
	h := new(ModelDeploymentScalerHandler)

	public := &worker.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-scaler"},
	}
	core := h.CastObjectTo(public)
	require.NotNil(t, core)
	assert.Equal(t, "qwen-scaler", core.Name)

	back := h.CastObjectFrom(context.Background(), core)
	require.NotNil(t, back)
	assert.Equal(t, "qwen-scaler", back.Name)
}
