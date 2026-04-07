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
		GetContainerAllocateResponse(context.Context, *core.Pod, *workercore.Devices, map[Resource]int32) (*ContainerAllocateResponse, error)
	}
)
