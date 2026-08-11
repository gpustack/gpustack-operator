package deviceplugin

import (
	"context"

	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// The first block below aliases k8s.io/kubelet's device-plugin v1beta1 API types, re-exported
// so a vendor allocator can name the gRPC payloads it builds without importing the kubelet
// package itself.
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

	// Server is the lifecycle contract every device-plugin gRPC server (ResourceServer among
	// them) exposes to its caller, independent of the kubelet API surface it also implements.
	Server interface {
		// Start starts the gRPC server and registers it to kubelet.
		// This method should be blocking until context is canceled or an error occurs.
		// If context is canceled, the server should be stopped and unregistered from kubelet.
		Start(ctx context.Context, kubeSocket string) error
		// Stop stops the gRPC server and unregisters it from kubelet.
		// This method should be non-blocking and return immediately.
		Stop()
	}
)
