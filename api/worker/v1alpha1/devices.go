package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Devices is the schema for worker.gpustack.ai.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",subResources=["status"]
type Devices struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   DevicesSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status DevicesStatus `json:"status,omitempty" protobuf:"bytes,3,name=status"`
}

var _ runtime.Object = (*Devices)(nil)

// DevicesSpec defines the desired state of Devices.
type DevicesSpec struct {
	// Groups is the list of device groups on the worker.
	//
	// +listType=map
	// +listMapKey=id
	// +listMapKey=manufacturer
	Groups []DevicesGroup `json:"groups" yaml:"groups" protobuf:"bytes,1,name=groups"`
}

// DevicesStatus defines the observed state of Devices.
type DevicesStatus struct {
	// Groups is the list of device groups on the worker.
	//
	// +listType=map
	// +listMapKey=id
	// +listMapKey=manufacturer
	Groups []DevicesAllocationGroup `json:"groups,omitempty" yaml:"groups,omitempty" protobuf:"bytes,1,opt,name=groups"`
}

type (
	// DevicesGroup is a group of devices with the same metadata.
	DevicesGroup struct {
		// ID is the universally unique identifier for this device group.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer" protobuf:"bytes,2,name=manufacturer"`

		// Name is the name of the device product.
		Name string `json:"name" yaml:"name" protobuf:"bytes,3,name=name"`

		// Memory is the memory size of the device in MiB.
		Memory uint64 `json:"memory" yaml:"memory" protobuf:"bytes,4,name=memory"`

		// Cores is the number of cores of the device.
		Cores uint32 `json:"cores,omitempty" yaml:"cores,omitempty" protobuf:"bytes,5,opt,name=cores"`

		// DriverVersion is the version of the driver used by the device.
		DriverVersion string `json:"driverVersion,omitempty" yaml:"driverVersion,omitempty" protobuf:"bytes,6,opt,name=driverVersion"`

		// RuntimeVersion is the version of the runtime used by the device.
		RuntimeVersion string `json:"runtimeVersion,omitempty" yaml:"runtimeVersion,omitempty" protobuf:"bytes,7,opt,name=runtimeVersion"`

		// ComputeCapability is the compute capability of the device.
		ComputeCapability string `json:"computeCapability,omitempty" yaml:"computeCapability,omitempty" protobuf:"bytes,8,opt,name=computeCapability"`

		// Family is the family of the device.
		Family string `json:"family,omitempty" yaml:"family,omitempty" protobuf:"bytes,9,opt,name=family"`

		// Accelerators is the list of the accelerator devices in this group.
		//
		// +listType=map
		// +listMapKey=id
		Accelerators []Accelerator `json:"accelerators,omitempty" yaml:"accelerators,omitempty" protobuf:"bytes,10,opt,name=accelerators"`

		// AcceleratorsFeature is the slicing capability shared by every accelerator in this group.
		AcceleratorsFeature AcceleratorsFeature `json:"acceleratorsFeature,omitempty" yaml:"acceleratorsFeature,omitempty" protobuf:"bytes,11,opt,name=acceleratorsFeature"` // nolint: lll
	}

	// DeviceTopology describes the topology information of the device.
	DeviceTopology struct {
		// PciBusID is the PCI bus ID of the device.
		PciBusID string `json:"pciBusId" yaml:"pciBusId" protobuf:"bytes,1,name=pciBusId"`

		// PciRootID is the PCI root ID of the device.
		PciRootID string `json:"pciRootId" yaml:"pciRootId" protobuf:"bytes,2,name=pciRootId"`

		// PciClass is the PCI class of the device.
		PciClass string `json:"pciClass" yaml:"pciClass" protobuf:"bytes,3,name=pciClass"`

		// NumaAffinity is the NUMA node that the device is attached to.
		NumaAffinity string `json:"numaAffinity" yaml:"numaAffinity" protobuf:"bytes,4,name=numaAffinity"`

		// CpuAffinity is the CPU cores that are close to the device.
		CpuAffinity string `json:"cpuAffinity" yaml:"cpuAffinity" protobuf:"bytes,5,name=cpuAffinity"`

		// RoCE is the RoCE (RDMA over Converged Ethernet) network information of the device.
		RoCE *DeviceEthernet `json:"roce,omitempty" yaml:"roce,omitempty" protobuf:"bytes,6,opt,name=roce"`
	}

	// DeviceEthernet describes the Ethernet information of the device.
	DeviceEthernet struct {
		// IP is the IP address of the Ethernet interface of the device.
		IP string `json:"ip" yaml:"ip" protobuf:"bytes,1,name=ip"`

		// SubnetMask is the subnet mask of the Ethernet interface of the device.
		SubnetMask string `json:"subnetMask" yaml:"subnetMask" protobuf:"bytes,2,name=subnetMask"`

		// Gateway is the gateway of the Ethernet interface of the device.
		Gateway string `json:"gateway" yaml:"gateway" protobuf:"bytes,3,name=gateway"`
	}

	// DevicesAllocationGroup describes the allocated device group.
	DevicesAllocationGroup struct {
		// ID is the universally unique identifier for this device group.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer" protobuf:"bytes,2,name=manufacturer"`

		// Accelerators is the list of the allocated accelerator devices in this group.
		//
		// +listType=map
		// +listMapKey=id
		Accelerators []AcceleratorAllocation `json:"accelerators,omitempty" yaml:"accelerators,omitempty" protobuf:"bytes,3,opt,name=accelerators"`
	}
)

// DeviceAllocationMode describes the allocation mode of the accelerator device.
// +enum
type DeviceAllocationMode uint32

const (
	// DeviceAllocationModeNone indicates that the allocation mode of the device is unknown.
	DeviceAllocationModeNone DeviceAllocationMode = iota
	// DeviceAllocationModeExclusive indicates that the device is allocated exclusively to a single consumer.
	DeviceAllocationModeExclusive
	// DeviceAllocationModeShared indicates that the device is allocated to multiple consumers,
	// and the resources are shared among them.
	DeviceAllocationModeShared
	// DeviceAllocationModeSliced indicates that the device is allocated to multiple consumers,
	// and the resources are partitioned among them.
	DeviceAllocationModeSliced
	// DeviceAllocationModeVisibility is an internal-only mode: it grants a container
	// device-cgroup visibility to the physical device(s) another container in the same Pod
	// was allocated (used by the SSH sidecar to reach main's accelerator), without a real
	// device selection or any resource accounting. It is never advertised on an InstanceType
	// and never written to Devices status.
	DeviceAllocationModeVisibility
)

func (in DeviceAllocationMode) String() string {
	switch in {
	case DeviceAllocationModeExclusive:
		return "Exclusive"
	case DeviceAllocationModeShared:
		return "Shared"
	case DeviceAllocationModeSliced:
		return "Sliced"
	case DeviceAllocationModeVisibility:
		return "Visibility"
	default:
		return "None"
	}
}

type (
	// Accelerator describes the information of an accelerator device.
	Accelerator struct {
		// ID is the universally unique identifier for this device.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Index is the logic number of the device, starting from 0.
		Index uint32 `json:"index" yaml:"index" protobuf:"varint,2,name=index"`

		// PhysicalIndexes is the device char list for the device, constructed by the manufacturer.
		PhysicalIndexes []uint32 `json:"physicalIndexes" yaml:"physicalIndexes" protobuf:"varint,3,rep,name=physicalIndexes"`

		// Topology is the topology information of the device.
		Topology DeviceTopology `json:"topology" yaml:"topology" protobuf:"bytes,4,name=topology"`

		// Status is the current status of the device.
		// Field number 5 is reserved for the removed per-accelerator Features.
		Status AcceleratorStatus `json:"status" yaml:"status" protobuf:"bytes,6,name=status"`
	}

	// AcceleratorSliced describes one slicing capability of a device model.
	AcceleratorSliced struct {
		// MaxSize is the maximum number of slices a single device can be split into.
		MaxSize int32 `json:"maxSize" yaml:"maxSize" protobuf:"varint,1,opt,name=maxSize"`

		// CoresPercentageOvercommit reports whether each slice may claim up to 100% of the
		// device compute (time-sharing / weighted sharing), so the sum across slices may
		// exceed one whole device; false means compute is partitioned (the sum stays within
		// one device).
		CoresPercentageOvercommit bool `json:"coresPercentageOvercommit,omitempty" yaml:"coresPercentageOvercommit,omitempty" protobuf:"varint,2,opt,name=coresPercentageOvercommit"` // nolint: lll

		// MemoryPercentageStep is the granularity, in percentage points, at which the device
		// memory can be sliced.
		MemoryPercentageStep int32 `json:"memoryPercentageStep,omitempty" yaml:"memoryPercentageStep,omitempty" protobuf:"varint,3,opt,name=memoryPercentageStep"` // nolint: lll
	}

	// AcceleratorPhysicalSlicedProfile describes one hardware partition profile of a device
	// model, such as an NVIDIA MIG profile (e.g. "1g.5gb"). The compute/memory slice counts
	// express the request granularity on each axis, one dimension richer than a scalar step.
	AcceleratorPhysicalSlicedProfile struct {
		// Name is the profile identifier, e.g. "1g.5gb". It is the display name and the
		// future resource-key suffix for a physical-slice request.
		Name string `json:"name" yaml:"name" protobuf:"bytes,1,opt,name=name"`

		// MemoryMib is the memory of one instance of this profile, in MiB.
		MemoryMib int64 `json:"memoryMib" yaml:"memoryMib" protobuf:"varint,2,opt,name=memoryMib"`

		// ComputeSlices is the number of compute slices one instance occupies — the request
		// granularity on the compute axis (1..7 on current hardware).
		ComputeSlices int32 `json:"computeSlices" yaml:"computeSlices" protobuf:"varint,3,opt,name=computeSlices"` // nolint: lll

		// MemorySlices is the number of memory slices one instance occupies — the request
		// granularity on the memory axis (1..8 on current hardware).
		MemorySlices int32 `json:"memorySlices" yaml:"memorySlices" protobuf:"varint,4,opt,name=memorySlices"` // nolint: lll

		// Count is the maximum number of instances of this profile on a single card.
		Count int32 `json:"count" yaml:"count" protobuf:"varint,5,opt,name=count"`
	}

	// AcceleratorsFeature describes the slicing features shared by every accelerator in a group.
	AcceleratorsFeature struct {
		// PhysicalSliced enables physical (hardware) slicing such as NVIDIA MIG — a real
		// spatial partition of cores and memory — when its MaxSize is non-zero.
		PhysicalSliced AcceleratorSliced `json:"physicalSliced,omitempty" yaml:"physicalSliced,omitempty" protobuf:"bytes,1,opt,name=physicalSliced"`

		// LogicalSliced enables logical (software) slicing via a vendor vGPU scheme or an
		// ld.preload interception library when its MaxSize is non-zero.
		LogicalSliced AcceleratorSliced `json:"logicalSliced,omitempty" yaml:"logicalSliced,omitempty" protobuf:"bytes,2,opt,name=logicalSliced"`
	}

	// AcceleratorStatus describes the observed state of the accelerator device.
	AcceleratorStatus struct {
		// Unhealthy indicates whether the device is healthy or not.
		Unhealthy bool `json:"unhealthy" yaml:"unhealthy" protobuf:"bytes,1,name=unhealthy"`
	}

	// AcceleratorAllocation describes the allocated accelerator device.
	AcceleratorAllocation struct {
		// ID is the universally unique identifier for this device.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Index is the logic number of the device, starting from 0.
		Index uint32 `json:"index" yaml:"index" protobuf:"varint,2,name=index"`

		// Mode is the allocation mode of the device.
		Mode DeviceAllocationMode `json:"mode" yaml:"mode" protobuf:"varint,3,name=mode,casttype=DeviceAllocationMode"`

		// Allocated is the allocated units of the device.
		Allocated int32 `json:"allocated,omitempty" yaml:"allocated,omitempty" protobuf:"varint,4,opt,name=allocated"`

		// Remaining is the remaining allocatable units of the device.
		Remaining int32 `json:"remaining,omitempty" yaml:"remaining,omitempty" protobuf:"varint,5,opt,name=remaining"`
	}
)

// MaxSlices returns the maximum number of slices a single device in the group can be
// split into: the larger of the physical (hardware) and logical (software) slice sizes.
// A zero Size means the mode is disabled, so a group with neither capability yields 0
// (not sliceable), and the allocator advertises no ".sliced" resource for it.
func (in AcceleratorsFeature) MaxSlices() int32 {
	n := in.PhysicalSliced.MaxSize
	if in.LogicalSliced.MaxSize > n {
		n = in.LogicalSliced.MaxSize
	}
	return n
}

// SlicedCoresOvercommit reports whether the slicing mode that determines MaxSlices()
// allows each slice to claim up to 100% of the device compute. It reads the flag off
// the mode with the larger MaxSize (the same mode MaxSlices() picks), so the per-card
// compute budget matches the winning mode: true multiplies the compute budget by the
// slice count, false caps it at a single whole-card 100%.
func (in AcceleratorsFeature) SlicedCoresOvercommit() bool {
	if in.LogicalSliced.MaxSize > in.PhysicalSliced.MaxSize {
		return in.LogicalSliced.CoresPercentageOvercommit
	}
	return in.PhysicalSliced.CoresPercentageOvercommit
}

// DevicesList holds the list of Devices.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type DevicesList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,name=metadata"`

	Items []Devices `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*DevicesList)(nil)
