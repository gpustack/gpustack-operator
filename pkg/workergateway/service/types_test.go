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
		fields []reflect.Type
	}{
		{
			name:   "overview bundle carries one dimension per view",
			mirror: reflect.TypeFor[AggregatedInstanceTypeOverviewResource](),
			fields: []reflect.Type{
				reflect.TypeFor[resource.Quantity](),
				reflect.TypeFor[[]workercore.AcceleratorProfileCount](),
			},
		},
		{
			name:   "candidate carries one resource per view",
			mirror: reflect.TypeFor[AggregatedInstanceTypeOnceMaxRequestCandidate](),
			fields: []reflect.Type{reflect.TypeFor[AggregatedInstanceTypeResource]()},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.ElementsMatch(t, views, fieldNamesOfType(c.mirror, c.fields...),
				"%s must mirror exactly the InstanceTypeStatus views", c.mirror.Name())
		})
	}
}

// fieldNamesOfType returns the names of in's direct fields declared with any of the given types —
// either as that type, or as a type embedding it, which is how a view that carries extra data
// alongside the shared resource shape is declared. Recognizing the embedding form is what keeps such
// a view inside the comparison: matching the type exactly would drop it from both sides at once, and
// the two sides would then agree while the extra data goes unmirrored.
//
// Accepting several types is what lets a dimension be mirrored in the shape it needs — a scalar
// quantity for most, a per-profile ledger for the hardware-partitioned one — without loosening the
// guard: a view mirrored in none of the accepted shapes still drops out and fails the comparison.
func fieldNamesOfType(in reflect.Type, fields ...reflect.Type) []string {
	names := make([]string, 0, in.NumField())
	for i := range in.NumField() {
		for _, field := range fields {
			if declaresType(in.Field(i).Type, field) {
				names = append(names, in.Field(i).Name)
				break
			}
		}
	}
	return names
}

// declaresType reports whether t is field, or a struct embedding it.
func declaresType(t, field reflect.Type) bool {
	if t == field {
		return true
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		if f := t.Field(i); f.Anonymous && f.Type == field {
			return true
		}
	}
	return false
}
