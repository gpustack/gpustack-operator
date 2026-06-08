package kuberequest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// qty parses a quantity for terse fixture construction.
func qty(s string) resource.Quantity { return resource.MustParse(s) }

// qtyEqual compares quantities by value (Cmp) so the assertion does not depend
// on the SI format (BinarySI vs DecimalSI) of the operands.
func qtyEqual(t *testing.T, want, got resource.Quantity, msg string) {
	t.Helper()
	assert.Zerof(t, want.Cmp(got), "%s: want %s, got %s", msg, want.String(), got.String())
}

func TestScaleToOvercommit(t *testing.T) {
	cases := []struct {
		name          string
		resName       core.ResourceName
		val           resource.Quantity
		acceleratable bool
		want          resource.Quantity
	}{
		// CPU, non-acceleratable: req = 800m × val.Value().
		{"CPU non-acc, zero", core.ResourceCPU, qty("0"), false, qty("0")},
		{"CPU non-acc, 1 core", core.ResourceCPU, qty("1"), false, qty("800m")},
		{"CPU non-acc, 2 cores", core.ResourceCPU, qty("2"), false, qty("1600m")},
		{"CPU non-acc, 4 cores", core.ResourceCPU, qty("4"), false, qty("3200m")},
		{"CPU non-acc, 8 cores", core.ResourceCPU, qty("8"), false, qty("6400m")},
		// Value() rounds up: 500m -> 1, 1500m -> 2.
		{"CPU non-acc, 500m rounds up to 1 core", core.ResourceCPU, qty("500m"), false, qty("800m")},
		{"CPU non-acc, 1500m rounds up to 2 cores", core.ResourceCPU, qty("1500m"), false, qty("1600m")},

		// CPU, acceleratable: req = 100m × val.Value().
		{"CPU acc, zero", core.ResourceCPU, qty("0"), true, qty("0")},
		{"CPU acc, 1 core", core.ResourceCPU, qty("1"), true, qty("100m")},
		{"CPU acc, 2 cores", core.ResourceCPU, qty("2"), true, qty("200m")},
		{"CPU acc, 16 cores", core.ResourceCPU, qty("16"), true, qty("1600m")},
		{"CPU acc, 500m rounds up to 1 core", core.ResourceCPU, qty("500m"), true, qty("100m")},

		// Memory: req = 128Mi × (val.Value() / Gi).
		{"RAM, zero", core.ResourceMemory, qty("0"), false, qty("0")},
		{"RAM, 1Gi", core.ResourceMemory, qty("1Gi"), false, qty("128Mi")},
		{"RAM, 4Gi", core.ResourceMemory, qty("4Gi"), false, qty("512Mi")},
		{"RAM, 16Gi", core.ResourceMemory, qty("16Gi"), false, qty("2Gi")},
		{"RAM, 64Gi", core.ResourceMemory, qty("64Gi"), false, qty("8Gi")},
		// Acceleratable flag must not affect memory scaling.
		{"RAM, 16Gi unaffected by acceleratable flag", core.ResourceMemory, qty("16Gi"), true, qty("2Gi")},
		// Sub-Gi collapses to 0.
		{"RAM, 512Mi below Gi truncates to 0", core.ResourceMemory, qty("512Mi"), false, qty("0")},
		{"RAM, 1023Mi below Gi truncates to 0", core.ResourceMemory, qty("1023Mi"), false, qty("0")},
		// Non-Gi multiple truncates downward.
		{"RAM, 1.5Gi truncates to 1Gi multiplier", core.ResourceMemory, qty("1536Mi"), false, qty("128Mi")},
		{"RAM, 3.9Gi truncates to 3Gi multiplier", core.ResourceMemory, qty("3994Mi"), false, qty("384Mi")},

		// Storage: same formula and base as memory.
		{"Storage, zero", core.ResourceEphemeralStorage, qty("0"), false, qty("0")},
		{"Storage, 1Gi", core.ResourceEphemeralStorage, qty("1Gi"), false, qty("128Mi")},
		{"Storage, 32Gi", core.ResourceEphemeralStorage, qty("32Gi"), false, qty("4Gi")},
		{"Storage, 512Mi below Gi truncates to 0", core.ResourceEphemeralStorage, qty("512Mi"), false, qty("0")},

		// Unknown resource names pass through unchanged.
		{"unknown nvidia.com/gpu passes through", "nvidia.com/gpu", qty("7"), false, qty("7")},
		{"unknown amd.com/gpu passes through with acc=true", "amd.com/gpu", qty("3"), true, qty("3")},
		{"unknown pods passes through", core.ResourcePods, qty("10"), false, qty("10")},
		{"unknown hugepages-2Mi passes through", "hugepages-2Mi", qty("4Mi"), false, qty("4Mi")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScaleToOvercommit(c.resName, c.val, c.acceleratable)
			qtyEqual(t, c.want, got, "ScaleToOvercommit")
		})
	}
}

func TestScaleBackOvercommit(t *testing.T) {
	cases := []struct {
		name          string
		resName       core.ResourceName
		val           resource.Quantity
		acceleratable bool
		want          resource.Quantity
	}{
		// CPU, non-acceleratable: limit = (val.MilliValue() / 800) × 1 core.
		{"CPU non-acc, zero", core.ResourceCPU, qty("0"), false, qty("0")},
		{"CPU non-acc, 800m -> 1 core", core.ResourceCPU, qty("800m"), false, qty("1")},
		{"CPU non-acc, 1600m -> 2 cores", core.ResourceCPU, qty("1600m"), false, qty("2")},
		{"CPU non-acc, 6400m -> 8 cores", core.ResourceCPU, qty("6400m"), false, qty("8")},
		// Non-base-multiple input is ill-formed (should not occur in practice
		// because all reservations are sums of base × N) — integer division truncates.
		{"CPU non-acc, 850m truncates to 1 core", core.ResourceCPU, qty("850m"), false, qty("1")},

		// CPU, acceleratable: limit = (val.MilliValue() / 100) × 1 core.
		{"CPU acc, zero", core.ResourceCPU, qty("0"), true, qty("0")},
		{"CPU acc, 100m -> 1 core", core.ResourceCPU, qty("100m"), true, qty("1")},
		{"CPU acc, 200m -> 2 cores", core.ResourceCPU, qty("200m"), true, qty("2")},
		{"CPU acc, 1600m -> 16 cores", core.ResourceCPU, qty("1600m"), true, qty("16")},

		// Memory: limit = (val.Value() / 128Mi) × 1 Gi.
		{"RAM, zero", core.ResourceMemory, qty("0"), false, qty("0")},
		{"RAM, 128Mi -> 1Gi", core.ResourceMemory, qty("128Mi"), false, qty("1Gi")},
		{"RAM, 512Mi -> 4Gi", core.ResourceMemory, qty("512Mi"), false, qty("4Gi")},
		{"RAM, 2Gi -> 16Gi", core.ResourceMemory, qty("2Gi"), false, qty("16Gi")},
		{"RAM, 8Gi -> 64Gi", core.ResourceMemory, qty("8Gi"), false, qty("64Gi")},
		{"RAM, 2Gi unaffected by acceleratable flag", core.ResourceMemory, qty("2Gi"), true, qty("16Gi")},

		// Storage: same formula and base as memory.
		{"Storage, zero", core.ResourceEphemeralStorage, qty("0"), false, qty("0")},
		{"Storage, 128Mi -> 1Gi", core.ResourceEphemeralStorage, qty("128Mi"), false, qty("1Gi")},
		{"Storage, 4Gi -> 32Gi", core.ResourceEphemeralStorage, qty("4Gi"), false, qty("32Gi")},

		// Unknown resource names pass through unchanged.
		{"unknown nvidia.com/gpu passes through", "nvidia.com/gpu", qty("3"), false, qty("3")},
		{"unknown amd.com/gpu passes through", "amd.com/gpu", qty("5"), false, qty("5")},
		{"unknown pods passes through", core.ResourcePods, qty("10"), false, qty("10")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScaleBackOvercommit(c.resName, c.val, c.acceleratable)
			qtyEqual(t, c.want, got, "ScaleBackOvercommit")
		})
	}
}

func TestRoundTrip(t *testing.T) {
	// Each case takes a limit, runs it through ScaleToOvercommit, then
	// through ScaleBackOvercommit, and asserts the recovered value.
	// wantBack is usually the original limit; lossy cases (sub-Gi memory,
	// fractional CPU) document where information is dropped on purpose.
	cases := []struct {
		name          string
		resName       core.ResourceName
		limit         resource.Quantity
		acceleratable bool
		wantBack      resource.Quantity
	}{
		// CPU, non-acceleratable: exact for whole-core inputs.
		{"CPU non-acc, 0 cores", core.ResourceCPU, qty("0"), false, qty("0")},
		{"CPU non-acc, 1 core", core.ResourceCPU, qty("1"), false, qty("1")},
		{"CPU non-acc, 2 cores", core.ResourceCPU, qty("2"), false, qty("2")},
		{"CPU non-acc, 8 cores", core.ResourceCPU, qty("8"), false, qty("8")},
		{"CPU non-acc, 64 cores", core.ResourceCPU, qty("64"), false, qty("64")},

		// CPU, acceleratable: exact for whole-core inputs because the
		// canonical caller now always passes the CPU limit (not Accelerator).
		{"CPU acc, 0 cores", core.ResourceCPU, qty("0"), true, qty("0")},
		{"CPU acc, 1 core", core.ResourceCPU, qty("1"), true, qty("1")},
		{"CPU acc, 16 cores", core.ResourceCPU, qty("16"), true, qty("16")},
		{"CPU acc, 64 cores", core.ResourceCPU, qty("64"), true, qty("64")},

		// Memory: exact for whole-Gi inputs.
		{"RAM, 0", core.ResourceMemory, qty("0"), false, qty("0")},
		{"RAM, 1Gi", core.ResourceMemory, qty("1Gi"), false, qty("1Gi")},
		{"RAM, 16Gi", core.ResourceMemory, qty("16Gi"), false, qty("16Gi")},
		{"RAM, 256Gi", core.ResourceMemory, qty("256Gi"), false, qty("256Gi")},

		// Memory: sub-Gi inputs collapse to 0 and stay 0 — the original
		// limit is dropped from the scheduler's view.
		{"RAM, 100Mi collapses to 0", core.ResourceMemory, qty("100Mi"), false, qty("0")},
		{"RAM, 512Mi collapses to 0", core.ResourceMemory, qty("512Mi"), false, qty("0")},
		{"RAM, 1023Mi collapses to 0", core.ResourceMemory, qty("1023Mi"), false, qty("0")},

		// Storage: exact for whole-Gi inputs.
		{"Storage, 0", core.ResourceEphemeralStorage, qty("0"), false, qty("0")},
		{"Storage, 1Gi", core.ResourceEphemeralStorage, qty("1Gi"), false, qty("1Gi")},
		{"Storage, 128Gi", core.ResourceEphemeralStorage, qty("128Gi"), false, qty("128Gi")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := ScaleToOvercommit(c.resName, c.limit, c.acceleratable)
			back := ScaleBackOvercommit(c.resName, req, c.acceleratable)
			qtyEqual(t, c.wantBack, back, "round-trip")
		})
	}
}

// TestAggregatedReservations models the canonical Kueue path: each pod
// contributes a clean base-multiple to the reserved total, so the inverse
// must recover the aggregated limit exactly.
func TestAggregatedReservations(t *testing.T) {
	cases := []struct {
		name          string
		resName       core.ResourceName
		limits        []resource.Quantity
		acceleratable bool
		wantAggregate resource.Quantity
	}{
		{
			name:          "CPU non-acc, three pods (4 + 2 + 8 cores)",
			resName:       core.ResourceCPU,
			limits:        []resource.Quantity{qty("4"), qty("2"), qty("8")},
			wantAggregate: qty("14"),
		},
		{
			name:          "CPU acc, three pods (16 + 8 + 32 cores)",
			resName:       core.ResourceCPU,
			limits:        []resource.Quantity{qty("16"), qty("8"), qty("32")},
			acceleratable: true,
			wantAggregate: qty("56"),
		},
		{
			name:          "RAM, three pods (16Gi + 8Gi + 32Gi)",
			resName:       core.ResourceMemory,
			limits:        []resource.Quantity{qty("16Gi"), qty("8Gi"), qty("32Gi")},
			wantAggregate: qty("56Gi"),
		},
		{
			name:          "Storage, three pods (32Gi + 64Gi + 128Gi)",
			resName:       core.ResourceEphemeralStorage,
			limits:        []resource.Quantity{qty("32Gi"), qty("64Gi"), qty("128Gi")},
			wantAggregate: qty("224Gi"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var total resource.Quantity
			for _, l := range c.limits {
				total.Add(ScaleToOvercommit(c.resName, l, c.acceleratable))
			}
			back := ScaleBackOvercommit(c.resName, total, c.acceleratable)
			qtyEqual(t, c.wantAggregate, back, "aggregate inversion")
		})
	}
}
