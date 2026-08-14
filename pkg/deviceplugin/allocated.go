package deviceplugin

import (
	"cmp"
	"slices"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// AllocatedAccelerator pairs one accelerator the kubelet allocated with the group carrying its
// model-wide facts — memory, driver and runtime version — which a vendor's injection reads together
// with the accelerator itself. Both fields point into the Devices the pairs were collected from, so
// they stay valid only as long as it does, and neither is to be written through.
type AllocatedAccelerator struct {
	// Group is the accelerator's device group.
	Group *workercore.DevicesGroup

	// Accel is the accelerator itself.
	Accel *workercore.Accelerator
}

// AllocatedAccelerators returns the accelerators the kubelet allocated to one container, ordered by
// the enumeration index their detector recorded.
//
// That order is the contract: a vendor runtime that numbers the accelerators of a container itself
// numbers them the way its management library enumerates the host — by PCI bus id — and the recorded
// index is that same enumeration. So an injection whose entries are addressed by position
// (a per-accelerator quota, a compute mask) lines up with the numbering the container will use only
// when it is emitted in this order. A vendor whose numbering instead follows the order the injection
// itself lists is free to use it too; ordering costs it nothing, since it is self-consistent either
// way.
//
// The ledger cannot be walked for it directly. It keeps one group per accelerator model, so a walk
// over groups is in enumeration order only within a group, and a container holding accelerators of
// two models interleaves them; the group order is not part of the API either, both lists being
// declared as maps keyed by identity.
//
// A ledger recording no index for its accelerators — every one left at zero — yields the walk order
// of the ledger, which is what a caller would have had without this. Ordering is a stable sort, so
// it neither invents an order nor discards the one already there.
func AllocatedAccelerators(devs *workercore.Devices, allocated map[Resource]int32) []AllocatedAccelerator {
	accels := make([]AllocatedAccelerator, 0, len(allocated))
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			accel := &grp.Accelerators[j]
			res := Resource{
				Group:  grp.ID,
				Device: accel.ID,
			}
			if _, existed := allocated[res]; !existed {
				continue
			}
			accels = append(accels, AllocatedAccelerator{Group: grp, Accel: accel})
		}
	}
	slices.SortStableFunc(accels, func(a, b AllocatedAccelerator) int {
		return cmp.Compare(a.Accel.Index, b.Accel.Index)
	})

	return accels
}

// AllocatedAcceleratorIDs returns the IDs of the given accelerators, in the order they are held —
// the value every vendor's device-visibility variable carries.
func AllocatedAcceleratorIDs(accels []AllocatedAccelerator) []string {
	ids := make([]string, 0, len(accels))
	for i := range accels {
		ids = append(ids, accels[i].Accel.ID)
	}

	return ids
}
