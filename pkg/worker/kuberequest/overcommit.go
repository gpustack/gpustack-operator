package kuberequest

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"gpustack.ai/gpustack/pkg/utils/quantityx"
)

// Overcommit base quantities — the per-unit "request" charge that the
// scheduler sees for one whole unit of limit. The limit stays the user-facing
// ask; the request is intentionally smaller so multiple pods can share a node.
//
//	1 CPU core   → 800m request  (or 100m for acceleratable nodes)
//	1 Gi RAM     → 128Mi request
//	1 Gi storage → 128Mi request
//
// The acceleratable variant is much smaller because on accelerator nodes the
// scarce resource is the accelerator itself; CPU should not gate placement.
var (
	_CPUOvercommitBaseQuantity          = resource.MustParse("800m")
	_RAMOvercommitBaseQuantity          = resource.MustParse("128Mi")
	_LocalStorageOvercommitBaseQuantity = resource.MustParse("128Mi")

	_CPUOvercommitBaseQuantityForAcceleratable = resource.MustParse("100m")
)

// ScaleToOvercommit returns the overcommit-shaped request value for a given
// limit-shaped quantity. The caller is expected to keep the original quantity
// as the container's resource limit and use the return of this function as the
// resource request — see getResourceRequirements in
// pkg/worker/controllers/worker/instance.go for the canonical call site.
//
// Formulas (val.Value() rounds up to the nearest integer; division is integer
// division so sub-unit remainders are lost):
//
//	CPU,  acceleratable=false: req = 800m  × val.Value()
//	CPU,  acceleratable=true:  req = 100m  × val.Value()
//	RAM:                       req = 128Mi × (val.Value() / Gi)
//	Storage:                   req = 128Mi × (val.Value() / Gi)
//	other resource names:      val (pass-through, unchanged)
//
// val is always the limit-shaped quantity of resName itself — callers must not
// substitute another resource's value (e.g., feeding Accelerator.Value() in
// place of CPU.Value()), otherwise ScaleBackOvercommit cannot recover the
// original limit.
//
// Caveats worth knowing at the call site:
//   - RAM/Storage under 1 Gi truncate to a 0 request. From the scheduler's
//     perspective such a pod consumes no memory/storage on the node.
//   - RAM/Storage are quantized to whole Gi — 1.5 Gi behaves like 1 Gi.
//   - CPU is quantized to whole cores via the ceiling in Value() — 500m
//     behaves like 1 core, 1500m like 2 cores.
func ScaleToOvercommit(
	resName core.ResourceName,
	val resource.Quantity,
	acceleratable bool,
) resource.Quantity {
	switch resName {
	case core.ResourceCPU:
		if acceleratable {
			return quantityx.Multiply(_CPUOvercommitBaseQuantityForAcceleratable, val.Value())
		}
		return quantityx.Multiply(_CPUOvercommitBaseQuantity, val.Value())
	case core.ResourceMemory:
		return quantityx.Multiply(_RAMOvercommitBaseQuantity, val.Value()/quantityx.Gi)
	case core.ResourceEphemeralStorage:
		return quantityx.Multiply(_LocalStorageOvercommitBaseQuantity, val.Value()/quantityx.Gi)
	}
	return val
}

// ScaleBackOvercommit returns the limits-equivalent quantity for an
// overcommit-shaped request value produced by ScaleToOvercommit. The canonical
// caller is convertInstanceTypeFromClusterQueue in
// pkg/worker/extensionapis/worker/instance_type.go, which scales the reserved
// total reported by Kueue (which is in overcommit-request units) back to
// limits units before subtracting it from capacity so that capacity,
// remaining, and once-max-request all share the same units.
//
// Inverse formulas — all use integer division, which is exact for sums of
// whole-unit pod reservations because each pod contributes a clean multiple
// of the base:
//
//	CPU, acceleratable=false: limit = (val.MilliValue() / 800) × 1 core
//	CPU, acceleratable=true:  limit = (val.MilliValue() / 100) × 1 core
//	RAM:                      limit = (val.Value() / 128Mi)    × 1 Gi
//	Storage:                  limit = (val.Value() / 128Mi)    × 1 Gi
//	other resource names:     val (pass-through, unchanged)
//
// The inversion is exact as long as ScaleToOvercommit was always called with
// val being the limit-shaped quantity of the same resName (which is the
// contract upheld by getResourceRequirements). Any sub-unit information that
// the forward direction quantized away — sub-Gi RAM/Storage truncating to 0,
// fractional CPU rounding up via Value() — cannot be recovered here.
func ScaleBackOvercommit(
	resName core.ResourceName,
	val resource.Quantity,
	acceleratable bool,
) resource.Quantity {
	switch resName {
	case core.ResourceCPU:
		baseMilli := _CPUOvercommitBaseQuantity.MilliValue()
		if acceleratable {
			baseMilli = _CPUOvercommitBaseQuantityForAcceleratable.MilliValue()
		}
		return *resource.NewMilliQuantity((val.MilliValue()/baseMilli)*1000, resource.DecimalSI)
	case core.ResourceMemory:
		base := _RAMOvercommitBaseQuantity.Value()
		return *resource.NewQuantity((val.Value()/base)*quantityx.Gi, resource.BinarySI)
	case core.ResourceEphemeralStorage:
		base := _LocalStorageOvercommitBaseQuantity.Value()
		return *resource.NewQuantity((val.Value()/base)*quantityx.Gi, resource.BinarySI)
	}
	return val
}
