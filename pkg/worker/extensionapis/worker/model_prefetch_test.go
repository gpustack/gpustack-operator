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

func TestModelPrefetchTableRendersTheColumns(t *testing.T) {
	table, err := modelPrefetchTable()
	require.NoError(t, err)

	pf := &worker.ModelPrefetch{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "warm"},
		Spec: workercore.ModelPrefetchSpec{
			ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  workercore.ModelPrefetchBindingReference{Name: "cache"},
		},
		Status: workercore.ModelPrefetchStatus{
			DesiredNodes: 3,
			ReadyNodes:   2,
			Conditions: []gpustack.Condition{{
				Type:   "Available",
				Status: meta.ConditionTrue,
			}},
		},
	}
	got, err := table.ConvertToTable(context.Background(), pf, nil)
	require.NoError(t, err)

	require.Len(t, got.Rows, 1)
	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, strings.TrimSpace(printCell(c)))
	}
	// The convertor books the object's Name first and an Age last; the columns this view adds sit
	// between them.
	assert.Equal(t, []string{"warm", "model", "cache", "2", "3", "True", "<unknown>"}, cells,
		"the table answers who warms what, and how far, at a kubectl glance")

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{"Name", "Artifact", "Binding", "Ready", "Desired", "Available", "Age"},
		headers, "the column names are part of the contract kubectl prints")
}

func printCell(c any) string {
	type stringer interface{ String() string }
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	if s, ok := c.(string); ok {
		return s
	}

	return ""
}
