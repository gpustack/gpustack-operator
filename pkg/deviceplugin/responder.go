package deviceplugin

import (
	"context"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

type (
	// ContainerAllocateResponder renders the kubelet Allocate response for a container. Every
	// vendor allocator implements it, and it answers the exclusive and shared families
	// outright. The two optional interfaces below take over per family — PhysicalSlicedResponder
	// for a hardware partition, LogicalSlicedResponder for a logical slice — so a responder
	// implementing neither still serves those families from here, unchanged.
	ContainerAllocateResponder interface {
		// GetContainerAllocateResponse returns the ContainerAllocateResponse
		// for the given pod, devices and allocated resources.
		GetContainerAllocateResponse(
			context.Context,
			*core.Pod,
			*core.Container,
			*workercore.Devices,
			map[Resource]int32,
		) (*ContainerAllocateResponse, error)
	}

	// PhysicalSlicedResponder is an optional capability of a ContainerAllocateResponder that owns
	// a hardware GPU partition (e.g. an NVIDIA MIG instance) end to end: materializing it for the
	// container that requests one, and naming it again for a container co-allocating the same
	// partition. A responder that does not implement it cannot serve partition requests at all.
	// The two live on one interface so that "a responder able to carve a partition can name it"
	// is a compile-time invariant rather than a runtime hole.
	PhysicalSlicedResponder interface {
		// ActuatePhysicalSliced materializes the partition for a container carrying a
		// "<base>.partitioned.<kind>-<profile>" request. The server invokes it — under the
		// vendor's own per-accelerator lock — on the accelerator IT chose, after reserving that
		// accelerator and before patching the allocation annotation, so the placement actually
		// taken is recorded upward (AllocatedPhysicalProfile / AllocatedPhysicalPlacements) for
		// the reconciler's placement-aware ledger.
		ActuatePhysicalSliced(
			context.Context,
			*core.Pod,
			*core.Container,
			*workercore.Devices,
			map[Resource]int32,
			string,
		) (*PhysicalSlicedAllocation, error)

		// GetPhysicalSlicedVisibilityResponse returns the container response naming the
		// partitions the owner container already holds on the given accelerators. The server
		// invokes it for a visibility Allocate whose accelerator-holding container requests a
		// partition profile, passing the container being served, the accelerators the owner
		// holds, and the owner's name. It must resolve from a durable, node-local record — not
		// in-process state — and must return an error, never an empty or parent-accelerator
		// response, when the identity cannot be resolved or cannot be shown to still be live.
		//
		// It returns a whole container response, not an identity: vendors differ in how they
		// make a device visible (an env for NVIDIA, injected device nodes elsewhere), so
		// substituting a partition for an accelerator stays inside the vendor's own response
		// shape.
		GetPhysicalSlicedVisibilityResponse(
			context.Context,
			*core.Pod,
			*core.Container,
			*workercore.Devices,
			map[Resource]int32,
			string,
		) (*ContainerAllocateResponse, error)
	}

	// LogicalSlicedResponder is an optional capability of a ContainerAllocateResponder whose
	// logical slice occupies a POSITION on the accelerator rather than only a share of it — an
	// AMD CU mask is a range of compute units, and two containers handed the same range share
	// those units instead of the accelerator. A responder that does not implement it keeps
	// today's behavior exactly: the server never reads logical occupancy and never records a
	// window.
	//
	// The two methods are split by where they may run, not by taste:
	//
	//   - PlaceLogicalSliced is called under the node allocate mutex, so the window it picks is
	//     published into the reservation before the next serialized Allocate reads it. Choosing
	//     it in GetContainerAllocateResponse instead cannot be made race-free: that call happens
	//     after the mutex is released AND after the durable annotation is written, so two
	//     concurrent allocations would pick the same free window and neither choice would
	//     survive a restart. It must therefore be pure over the snapshot it is given: no I/O.
	//   - GetLogicalSlicedResponse is called after the mutex is released, and CONSUMES the
	//     placement the server published rather than recomputing it. The responder's own I/O
	//     (creating a per-container working directory, say) belongs here for the same reason the
	//     annotation patch does — off the serialized path.
	//
	// Unlike PhysicalSlicedResponder there is no rollback: a mask is a string in an environment,
	// and nothing was materialized on the accelerator that a failure could strand.
	LogicalSlicedResponder interface {
		// PlaceLogicalSliced picks the geometry this container will occupy on each allocated
		// accelerator, given what the node's live allocations already occupy. Returning an empty
		// map is legal and means "this responder records no geometry for this container".
		PlaceLogicalSliced(
			context.Context,
			*core.Pod,
			*core.Container,
			*workercore.Devices,
			map[Resource]int32,
			Placements,
		) (Placements, error)

		// GetLogicalSlicedResponse renders the container response for a placement the server
		// has already published. It replaces GetContainerAllocateResponse for the sliced mode.
		GetLogicalSlicedResponse(
			context.Context,
			*core.Pod,
			*core.Container,
			*workercore.Devices,
			map[Resource]int32,
			Placements,
		) (*ContainerAllocateResponse, error)
	}
)

// Placements maps an accelerator resource — its group ID plus its own ID — to the run(s) an
// allocation occupies on it. It carries both ledgers, and the unit is the one the ledger counts in:
// memory-slice indexes for a hardware partition (NVIDIA: MIG memory slices), the manufacturer's own
// compute units for a logical slice (AMD: CU-mask bit indexes, exactly as they appear in
// HSA_CU_MASK).
//
// Keyed by Resource, not by UUID alone, because every producer and consumer of a placement ledger
// walks Devices.Spec or an allocation status group by group and has the group in hand: the partition
// candidates a decision ranks, the per-accelerator AllocatedProfiles/RemainingProfiles the ledger
// fold writes into the matching status accelerator, and the reservation the server publishes are all
// addressed that way.
type Placements map[Resource][]workercore.AcceleratorPlacement

// PhysicalSlicedAllocation is the outcome of materializing a physical GPU partition for a
// container: the per-accelerator placements to record upward into the Pod's allocation annotation
// and the container response carrying the partition's visible-devices env.
type PhysicalSlicedAllocation struct {
	// Profile is the physical-slice profile that was materialized (e.g. "1g.10gb").
	Profile string
	// Placements maps each allocated accelerator to the memory-slice interval(s) its partition
	// occupies. The server folds these into the allocation annotation as the reconciler
	// ledger's occupied source.
	Placements Placements
	// IDs maps each allocated accelerator to its partition's own driver identifier (a MIG UUID).
	// The allocator reads it when it creates or reuses the partition, and the server folds it into
	// the allocation annotation so a later reader can address the partition directly instead of
	// re-deriving it from the profile and the placement.
	IDs map[Resource]string
	// Response carries the vendor visible-devices env for the partitions (no logical-slice
	// artifacts). The server returns it in place of GetContainerAllocateResponse.
	Response *ContainerAllocateResponse
	// Rollback tears down whatever this allocation created; a no-op for a fully reused
	// partition. The server calls it when the post-actuation annotation patch fails, so no
	// half-owned partition persists.
	Rollback func()
}
