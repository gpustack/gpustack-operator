package service

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestAggregatedInstanceTypeMirrorsEveryStatusView guards the hand-written mirror the gateway keeps
// of the cluster InstanceType's resource views.
//
// The aggregated types re-declare those views field by field rather than embedding them, and no
// generator maintains them, so adding a view upstream is not a compile error here: the new dimension
// is simply never ingested, summed or served, and the fleet reads as having no capacity on it.
// Comparing the field sets by reflection is what turns that silent omission into a failure.
func TestAggregatedInstanceTypeMirrorsEveryStatusView(t *testing.T) {
	views := fieldNamesOfType(
		reflect.TypeFor[workercore.InstanceTypeStatus](),
		reflect.TypeFor[workercore.InstanceTypeResource](),
	)

	cases := []struct {
		name   string
		mirror reflect.Type
		field  reflect.Type
	}{
		{
			name:   "overview bundle carries one quantity per view",
			mirror: reflect.TypeFor[AggregatedInstanceTypeOverviewResource](),
			field:  reflect.TypeFor[resource.Quantity](),
		},
		{
			name:   "candidate carries one resource per view",
			mirror: reflect.TypeFor[AggregatedInstanceTypeOnceMaxRequestCandidate](),
			field:  reflect.TypeFor[AggregatedInstanceTypeResource](),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.ElementsMatch(t, views, fieldNamesOfType(c.mirror, c.field),
				"%s must mirror exactly the InstanceTypeStatus views", c.mirror.Name())
		})
	}
}

// fieldNamesOfType returns the names of in's direct fields declared with the given type.
func fieldNamesOfType(in, field reflect.Type) []string {
	names := make([]string, 0, in.NumField())
	for i := range in.NumField() {
		if in.Field(i).Type == field {
			names = append(names, in.Field(i).Name)
		}
	}
	return names
}
