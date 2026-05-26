package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListAggregateInstanceTypes_Result(t *testing.T) {
	cases := []struct {
		name     string
		list     AggregatedInstanceTypeList
		sorted   bool
		expected []string
	}{
		{
			name: "empty list + sorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{},
			},
			sorted:   true,
			expected: []string{},
		},
		{
			name: "list with cpu-only + sorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{
					{
						Name: "gpustack-cpu-only",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: false,
						},
					},
				},
			},
			sorted:   true,
			expected: []string{"gpustack-cpu-only"},
		},
		{
			name: "list with acceleratable + unsorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{
					{
						Name: "gpustack-cpu-only",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: false,
						},
					},
					{
						Name: "gpustack-nvidia-tesla-t4",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
					{
						Name: "gpustack-nvidia-a10g",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
				},
			},
			sorted:   false,
			expected: []string{"gpustack-cpu-only", "gpustack-nvidia-tesla-t4", "gpustack-nvidia-a10g"},
		},
		{
			name: "list with acceleratable + sorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{
					{
						Name: "gpustack-cpu-only",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: false,
						},
					},
					{
						Name: "gpustack-nvidia-tesla-t4",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
					{
						Name: "gpustack-nvidia-a10g",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
				},
			},
			sorted:   true,
			expected: []string{"gpustack-nvidia-a10g", "gpustack-nvidia-tesla-t4", "gpustack-cpu-only"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			listOp := ListAggregateInstanceTypes{list: c.list}
			result := listOp.Result(c.sorted)
			actual := make([]string, len(result.Items))
			for i, item := range result.Items {
				actual[i] = item.Name
			}
			assert.Equal(t, c.expected, actual)
		})
	}
}
