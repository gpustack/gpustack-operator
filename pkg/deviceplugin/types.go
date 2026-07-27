package deviceplugin

import (
	"context"

	core "k8s.io/api/core/v1"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

type (
	Options                              = deviceplugin.DevicePluginOptions
	Device                               = deviceplugin.Device
	Empty                                = deviceplugin.Empty
	ListAndWatchResponse                 = deviceplugin.ListAndWatchResponse
	PreferredAllocationRequest           = deviceplugin.PreferredAllocationRequest
	PreferredAllocationResponse          = deviceplugin.PreferredAllocationResponse
	ContainerPreferredAllocationRequest  = deviceplugin.ContainerPreferredAllocationRequest
	ContainerPreferredAllocationResponse = deviceplugin.ContainerPreferredAllocationResponse
	AllocateRequest                      = deviceplugin.AllocateRequest
	AllocateResponse                     = deviceplugin.AllocateResponse
	ContainerAllocateRequest             = deviceplugin.ContainerAllocateRequest
	ContainerAllocateResponse            = deviceplugin.ContainerAllocateResponse
	Mount                                = deviceplugin.Mount
	DeviceSpec                           = deviceplugin.DeviceSpec
	CDIDevice                            = deviceplugin.CDIDevice
	PreStartContainerRequest             = deviceplugin.PreStartContainerRequest
	PreStartContainerResponse            = deviceplugin.PreStartContainerResponse

	Server interface {
		// Start starts the gRPC server and registers it to kubelet.
		// This method should be blocking until context is canceled or an error occurs.
		// If context is canceled, the server should be stopped and unregistered from kubelet.
		Start(ctx context.Context, kubeSocket string) error
		// Stop stops the gRPC server and unregisters it from kubelet.
		// This method should be non-blocking and return immediately.
		Stop()
	}

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

	// PhysicalSlicedResponder is an optional capability of a ContainerAllocateResponder that
	// owns a hardware GPU partition (e.g. an NVIDIA MIG instance): materializing it for a
	// container carrying a "<base>.partitioned.<kind>-<profile>" request. The server invokes it
	// — under the vendor's own per-card lock — on the card IT chose, after reserving that card
	// and before patching the allocation annotation, so the placement actually taken is recorded
	// upward (AllocatedPhysicalProfile / AllocatedPhysicalPlacements) for the reconciler's
	// placement-aware ledger. A responder that does not implement it cannot serve partition
	// requests.
	PhysicalSlicedResponder interface {
		ActuatePhysicalSliced(
			context.Context,
			*core.Pod,
			*core.Container,
			*workercore.Devices,
			map[Resource]int32,
			string,
		) (*PhysicalSlicedAllocation, error)
	}
)

// PhysicalSlicedAllocation is the outcome of materializing a physical GPU partition for a
// container: the per-card placements to record upward into the Pod's allocation annotation
// and the container response carrying the partition's visible-devices env.
type PhysicalSlicedAllocation struct {
	// Profile is the physical-slice profile that was materialized (e.g. "1g.10gb").
	Profile string
	// Placements maps each allocated card to the memory-slice interval(s) its partition
	// occupies. The server folds these into the allocation annotation as the reconciler
	// ledger's occupied source.
	Placements map[Resource][]workercore.AcceleratorPhysicalPlacement
	// Response carries the vendor visible-devices env for the partitions (no logical-slice
	// artifacts). The server returns it in place of GetContainerAllocateResponse.
	Response *ContainerAllocateResponse
	// Rollback tears down whatever this allocation created; a no-op for a fully reused
	// partition. The server calls it when the post-actuation annotation patch fails, so no
	// half-owned partition persists.
	Rollback func()
}
