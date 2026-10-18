package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestModelDeploymentTableRendersTheColumns(t *testing.T) {
	table, err := modelDeploymentTable()
	require.NoError(t, err)

	md := &worker.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "sglang-epd-llmd"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: "SGLang"},
		},
		Status: workercore.ModelDeploymentStatus{
			RoleSummary: "1R1E1P1D",
			Phase:       "Ready",
			Endpoint:    "http://sglang-epd-llmd-router.default.svc:8000",
		},
	}
	got, err := table.ConvertToTable(context.Background(), md, nil)
	require.NoError(t, err)

	require.Len(t, got.Rows, 1)
	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, strings.TrimSpace(printCell(c)))
	}
	assert.Equal(t, []string{
		"sglang-epd-llmd",
		"SGLang",
		"1R1E1P1D",
		"Ready",
		"http://sglang-epd-llmd-router.default.svc:8000",
		"<unknown>",
	}, cells)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{"Name", "Engine", "Roles", "Phase", "Endpoint", "Age"}, headers)
}
