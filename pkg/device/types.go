package device

import (
	"context"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// Structures for device metadata.
type (
	// Topology represents the topology information of a device,
	// including its NUMA node and PCI information.
	Topology = workercore.DeviceTopology
	// Ethernet represents the Ethernet information of a device,
	// including its name and PCI information.
	Ethernet = workercore.DeviceEthernet
	// AcceleratorSliced represents one slicing capability of a device model,
	// including its maximum slice count and compute/memory sharing semantics.
	AcceleratorSliced = workercore.AcceleratorSliced
	// AcceleratorPhysicalSlicedProfile represents one hardware partition profile of a
	// device model, such as an NVIDIA MIG profile.
	AcceleratorPhysicalSlicedProfile = workercore.AcceleratorPhysicalSlicedProfile
	// AcceleratorsFeature represents the slicing features shared by every
	// accelerator in a group.
	AcceleratorsFeature = workercore.AcceleratorsFeature
	// AcceleratorStatus represents the status of the accelerator device,
	// including its health status and other status information.
	AcceleratorStatus = workercore.AcceleratorStatus
	// Accelerator represents the metadata of the accelerator device,
	// including its name, topology, features, and status.
	Accelerator = workercore.Accelerator
	// DevicesGroup represents a group of devices that have the same name and memory size.
	DevicesGroup = workercore.DevicesGroup
	// DevicesGroupList represents a list of device groups.
	DevicesGroupList = []DevicesGroup
)

// Structures for device metrics.
type (
	// AcceleratorMetrics represents the metrics of the accelerator device,
	// including its memory usage, core usage, temperature, and power consumption.
	AcceleratorMetrics struct {
		// ID is the universally unique identifier for this device.
		ID string `json:"id" yaml:"id"`
		// Memory is the memory size of the device in MiB.
		Memory uint64 `json:"memory" yaml:"memory"`
		// MemoryUsage is the memory used by the device in MiB.
		MemoryUsage uint64 `json:"memoryUsage" yaml:"memoryUsage"`
		// MemoryUtilization is the percentage of memory being used by the device, in the range of [0, 100].
		MemoryUtilization uint32 `json:"memoryUtilization" yaml:"memoryUtilization"`
		// CoresUtilization is the percentage of cores being used by the device, in the range of [0, 100].
		CoresUtilization uint32 `json:"coresUtilization" yaml:"coresUtilization"`
		// Temperature is the temperature of the device in Celsius.
		Temperature uint32 `json:"temperature" yaml:"temperature"`
		// PowerUsage is the power used by the device in Watts.
		PowerUsage uint32 `json:"powerUsage" yaml:"powerUsage"`
		// Unhealthy indicates whether the device is healthy or not.
		Unhealthy bool `json:"unhealthy" yaml:"unhealthy"`
	}
	// MetricsGroup represents a group of device metrics.
	MetricsGroup struct {
		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer"`
		// Timestamp is the time when the metrics were collected.
		Timestamp time.Time `json:"timestamp" yaml:"timestamp"`
		// Accelerators is the list of the accelerator device metrics in this group.
		Accelerators []AcceleratorMetrics `json:"accelerators" yaml:"accelerators"`
	}
	// MetricsGroupList represents a list of device metrics groups.
	MetricsGroupList = []MetricsGroup
)

type (
	// DetectorOptions represents the options for configuring the detector.
	DetectorOptions struct {
		Logger klog.Logger
	}

	// Detector is an interface for detecting devices on the system.
	Detector interface {
		// Name returns the name of the device type that this interface detects.
		Name() string

		// DetectAccelerator detects the accelerators on the system and returns a list of device groups.
		//
		// If noPciCheck is true, the detector will skip PCI checks when detecting accelerators.
		// This is useful for platforms where PCI information is not available or not reliable.
		DetectAccelerator(noPciCheck bool) (DevicesGroupList, error)

		// MonitorAccelerator monitors the accelerators on the system and returns a list of device metrics groups.
		//
		// If noPciCheck is true, the detector will skip PCI checks when monitoring accelerators.
		// This is useful for platforms where PCI information is not available or not reliable.
		MonitorAccelerator(noPciCheck bool) (MetricsGroupList, error)
	}
)

type (
	// AllocatorSlicingPolicy represents the policy for how devices are sliced when allocating to containers.
	AllocatorSlicingPolicy = string

	// AllocatorOptions represents the options for configuring the allocator.
	AllocatorOptions struct {
		Logger        klog.Logger
		KubeSocket    string
		NoShared      bool
		NoSliced      bool
		SlicingPolicy AllocatorSlicingPolicy
	}

	// Allocator is an interface for allocating devices to containers.
	Allocator interface {
		// Name returns the name of the device type that this interface allocates.
		Name() string

		// Start starts the allocator and prepares it for allocation.
		//
		// The allocator should be ready to allocate devices after this method is called.
		Start(ctx context.Context) error

		// Stop stops the allocator and releases any resources it holds.
		// After this method is called, the allocator should not be used for allocation anymore.
		Stop()
	}
)

const (
	// AllocatorSlicingPolicyBestEffort means the allocator will
	// attempt to slice devices with the most suitable partitioning mode in the order:
	// `soft`, `virtual` and `physical`.
	AllocatorSlicingPolicyBestEffort AllocatorSlicingPolicy = "best-effort"

	// AllocatorSlicingPolicyQoS means the allocator will
	// attempt to slice devices with the most suitable partitioning mode in the order:
	// `physical`, `virtual` and `soft`.
	AllocatorSlicingPolicyQoS AllocatorSlicingPolicy = "qos"
)

// GetAllSlicingPolicies returns a list of all supported allocator slicing policies.
func GetAllSlicingPolicies() []AllocatorSlicingPolicy {
	return []AllocatorSlicingPolicy{
		AllocatorSlicingPolicyBestEffort,
		AllocatorSlicingPolicyQoS,
	}
}
