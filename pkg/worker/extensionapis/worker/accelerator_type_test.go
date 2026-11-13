package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestAcceleratorTypeTableRendersTheColumns(t *testing.T) {
	tc, err := acceleratorTypeTable()
	require.NoError(t, err)

	at := &worker.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "gpustack--nvidia-b300-linux-amd64"},
		Spec: workercore.AcceleratorTypeSpec{
			AcceleratorGroup: "nvidia-b300",
			OS:               "linux",
			Arch:             "amd64",
		},
		Status: workercore.AcceleratorTypeStatus{
			Entrance: "gpustack-fnv64-1234",
			Phase:    "Active",
			Accelerator: workercore.InstanceTypeResource{
				OnceMaxRequest: resource.MustParse("8"),
				Remaining:      resource.MustParse("8"),
			},
			AcceleratorShared: workercore.InstanceTypeResource{
				OnceMaxRequest: resource.MustParse("8"),
				Remaining:      resource.MustParse("128"),
			},
			AcceleratorSliced: workercore.InstanceTypeResource{
				OnceMaxRequest: resource.MustParse("800"),
				Remaining:      resource.MustParse("800"),
			},
			AcceleratorPartitioned: workercore.InstanceTypePartitionedResource{
				InstanceTypeResource: workercore.InstanceTypeResource{
					OnceMaxRequest: resource.MustParse("0"),
					Remaining:      resource.MustParse("0"),
				},
			},
		},
	}

	got, err := tc.ConvertToTable(context.Background(), at, nil)
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)

	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, strings.TrimSpace(printCell(c)))
	}
	assert.Equal(t, []string{
		"gpustack--nvidia-b300-linux-amd64",
		"gpustack-fnv64-1234",
		"8/8 8/128 800/800 0/0",
		"-",
		"Active",
		"<unknown>",
	}, cells)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{
		"Name",
		"Entrance",
		"Accelerator(EX/SH/SL/PT)",
		"Partitions",
		"Phase",
		"Age",
	}, headers)
}
