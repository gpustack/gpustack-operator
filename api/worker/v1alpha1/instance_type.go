package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceType is the schema for worker.gpustack.ai.
//
// Underhood, an InstanceType is mapping to a Kueue ClusterQueue,
// and the InstanceType's name is the same as the ClusterQueue's name.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["instype"],subResources=["status"]
type InstanceType struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   InstanceTypeSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status InstanceTypeStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*InstanceType)(nil)

// InstanceTypeSpec defines the desired spec of InstanceType.
type InstanceTypeSpec struct {
	// AcceleratorGroup is the accelerator group (the acceleratable node key) of the
	// InstanceType, e.g. "nvidia-a10g". It selects the accelerator pool the type schedules
	// onto and is required by the validating webhook when Acceleratable is true.
	AcceleratorGroup string `json:"acceleratorGroup,omitempty" protobuf:"bytes,1,opt,name=acceleratorGroup"`

	// Acceleratable indicates whether the InstanceType is acceleratable.
	Acceleratable bool `json:"acceleratable" protobuf:"bytes,2,name=acceleratable"`

	// Manufacturer is the name of the InstanceType manufacturer.
	Manufacturer string `json:"manufacturer,omitempty" protobuf:"bytes,3,opt,name=manufacturer"`

	// Product is the name of the InstanceType product.
	Product string `json:"product,omitempty" protobuf:"bytes,4,opt,name=product"`

	// Family is the family of the InstanceType.
	Family string `json:"family,omitempty" protobuf:"bytes,5,opt,name=family"`

	// OS is the operating system of the InstanceType, e.g. "linux", "windows".
	//
	// It is a required admin-writable input, enforced by the validating webhook.
	OS string `json:"os" protobuf:"bytes,6,opt,name=os"`

	// Arch is the architecture of the InstanceType, e.g. "amd64", "arm64".
	//
	// It is a required admin-writable input, enforced by the validating webhook.
	Arch string `json:"arch" protobuf:"bytes,7,opt,name=arch"`

	// CPU describes the CPU information of the InstanceType.
	InstanceTypeCPU `json:",inline" protobuf:"bytes,8,opt,name=cpu"`

	// Accelerator describes the accelerator information of the InstanceType.
	InstanceTypeAccelerator `json:",inline" protobuf:"bytes,9,opt,name=accelerator"`

	// UnitResources describes the unit resources of the InstanceType.
	//
	// It is a required admin-writable input, enforced by the validating webhook, and is
	// immutable after creation; a derived InstanceType is stamped with the fixed default.
	UnitResources InstanceTypeUnitResources `json:"unitResources" protobuf:"bytes,10,opt,name=unitResources"`

	// LocalStorage is the ephemeral local storage of the InstanceType, e.g. "100Gi".
	//
	// It is a required admin-writable input carrying a case-sensitive "Gi" suffix, enforced
	// by the validating webhook, and is immutable after creation; a derived InstanceType is
	// stamped with the fixed default.
	LocalStorage string `json:"localStorage" protobuf:"bytes,11,opt,name=localStorage"`

	// GeneralGroup is the general(CPU) group (the general node key) of the InstanceType, e.g.
	// "amd-epyc-7763", or the literal "generic" for a CPU-manufacturer-agnostic pool. The
	// mutating webhook defaults an empty value to "generic"; it participates as a scheduling
	// discriminator only when instance-type-aware-cpu-manufacturer is enabled.
	GeneralGroup string `json:"generalGroup,omitempty" protobuf:"bytes,12,opt,name=generalGroup"`

	// DisplayName is a human-friendly label for the InstanceType. The mutating webhook
	// defaults an empty value to Product.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,13,opt,name=displayName"`

	// Inactive takes the InstanceType out of service. When true, the InstanceTypeReconciler
	// holds the backing ClusterQueue (blocks new admission without evicting running
	// workloads); clearing it reactivates the queue. A queue stopped by any means is
	// reflected back into Inactive=true.
	Inactive bool `json:"inactive,omitempty" protobuf:"varint,14,opt,name=inactive"`

	// Description is a free-form admin annotation for the InstanceType.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,15,opt,name=description"`
}

// InstanceTypeCPU describes the information of the CPU.
type InstanceTypeCPU struct {
	// PhysicalCores is the number of physical cores of the CPU, e.g. "4", "8".
	PhysicalCores string `json:"physicalCores,omitempty" protobuf:"bytes,1,opt,name=physicalCores"`

	// ThreadsPerPhysicalCore is the number of threads per physical core of the CPU, e.g. "2", "4".
	ThreadsPerPhysicalCore string `json:"threadsPerPhysicalCore,omitempty" protobuf:"bytes,2,opt,name=threadsPerPhysicalCore"`

	// LogicalCores is the number of logical cores of the CPU, e.g. "8", "16".
	LogicalCores string `json:"logicalCores,omitempty" protobuf:"bytes,3,opt,name=logicalCores"`

	// Stepping is the stepping of the CPU, e.g. "0", "1".
	Stepping string `json:"stepping,omitempty" protobuf:"bytes,4,opt,name=stepping"`

	// ClockSpeed is the speed in Hz of the CPU, e.g. "2000"
	ClockSpeed string `json:"clockSpeed,omitempty" protobuf:"bytes,5,opt,name=clockSpeed"`

	// MaxClockSpeed is the maximum speed in Hz of the CPU, e.g. "3000"
	MaxClockSpeed string `json:"maxClockSpeed,omitempty" protobuf:"bytes,6,opt,name=maxClockSpeed"`

	// CacheLine is the cache line size in bytes of the CPU, e.g. "64", "128".
	CacheLine string `json:"cacheLine,omitempty" protobuf:"bytes,7,opt,name=cacheLine"`

	// Cache describes the cache information of the CPU.
	Cache InstanceTypeCPUCache `json:"cache,omitempty" protobuf:"bytes,8,opt,name=cache"`
}

// InstanceTypeCPUCache describes the cache information of the CPU.
type InstanceTypeCPUCache struct {
	// L1I is the L1 instruction cache size in bytes of the CPU.
	L1I string `json:"l1i,omitempty" protobuf:"bytes,1,opt,name=l1i"`

	// L1D is the L1 data cache size in bytes of the CPU.
	L1D string `json:"l1d,omitempty" protobuf:"bytes,2,opt,name=l1d"`

	// L2 is the L2 cache size in bytes of the CPU.
	L2 string `json:"l2,omitempty" protobuf:"bytes,3,opt,name=l2"`

	// L3 is the L3 cache size in bytes of the CPU.
	L3 string `json:"l3,omitempty" protobuf:"bytes,4,opt,name=l3"`
}

// InstanceTypeAccelerator describes the information of the accelerator.
type InstanceTypeAccelerator struct {
	// Memory is the VRAM size of the accelerator, e.g. "65535Mi".
	Memory string `json:"memory,omitempty" protobuf:"bytes,1,opt,name=memory"`

	// Cores is the number of cores of the accelerator, e.g. "128", "256".
	Cores string `json:"cores,omitempty" protobuf:"bytes,2,opt,name=cores"`

	// ComputeCapability is the compute capability of the accelerator, e.g. "8.0", "7.0".
	ComputeCapability string `json:"computeCapability,omitempty" protobuf:"bytes,3,opt,name=computeCapability"`

	// Feature is the accelerator's slicing capability: the physical / logical slicing mode, the
	// per-device maximum slice count, whether compute may be overcommitted, and the memory step.
	// A zero MaxSlices means the accelerator cannot be sliced. It mirrors the device group's
	// AcceleratorsFeature, folded in from the node-derived ResourceFlavor.
	//
	// Field number 4 is reserved for the removed Sliceable bool.
	Feature AcceleratorsFeature `json:"feature,omitempty" protobuf:"bytes,6,opt,name=feature"`

	// CPU describes the CPU information of the accelerator.
	CPU InstanceTypeAcceleratorCPU `json:"cpu,omitempty" protobuf:"bytes,5,opt,name=cpu"`
}

// IsSliceable reports whether the accelerator can be sliced: its slicing descriptor
// carries a non-zero maximum slice count.
func (in InstanceTypeAccelerator) IsSliceable() bool {
	return in.Feature.MaxSlices() > 0
}

// InstanceTypeAcceleratorCPU describes the CPU information of the accelerator.
type InstanceTypeAcceleratorCPU struct {
	// Manufacturer is the name of the CPU manufacturer, e.g. "amd", "intel".
	Manufacturer string `json:"manufacturer,omitempty" protobuf:"bytes,1,opt,name=manufacturer"`

	// Product is the name of the CPU product.
	Product string `json:"product,omitempty" protobuf:"bytes,2,opt,name=product"`

	// Family is the family of the CPU.
	Family string `json:"family,omitempty" protobuf:"bytes,3,opt,name=family"`

	// Detail inlines the CPU details of the CPU.
	InstanceTypeCPU `json:",inline" protobuf:"bytes,4,opt,name=cpu"`
}

// InstanceTypeDetail is the observed hardware descriptor of an InstanceType, computed by the
// reconciler from the matched ResourceFlavor's notes and the pool's Devices ledger. It lives on
// the status side: because it embeds the accelerator's AcceleratorSlicedDetail (which holds a
// slice), it is not comparable and must never appear on the comparable, map-key InstanceTypeSpec.
type InstanceTypeDetail struct {
	// Manufacturer is the name of the InstanceType manufacturer.
	Manufacturer string `json:"manufacturer,omitempty" protobuf:"bytes,1,opt,name=manufacturer"`

	// Product is the name of the InstanceType product.
	Product string `json:"product,omitempty" protobuf:"bytes,2,opt,name=product"`

	// Family is the family of the InstanceType.
	Family string `json:"family,omitempty" protobuf:"bytes,3,opt,name=family"`

	// CPU describes the CPU information of the InstanceType.
	InstanceTypeCPU `json:",inline" protobuf:"bytes,4,opt,name=cpu"`

	// Accelerator describes the accelerator information of the InstanceType.
	InstanceTypeAcceleratorDetail `json:",inline" protobuf:"bytes,5,opt,name=accelerator"`
}

// InstanceTypeAcceleratorDetail describes the observed accelerator information of an InstanceType.
// It mirrors InstanceTypeAccelerator but carries the pool-aggregated SlicedDetail (the observed
// slicing capability) in place of the desired-state Feature, so the status-side detail can hold
// the slice-bearing AcceleratorSlicedDetail the comparable Spec must not.
type InstanceTypeAcceleratorDetail struct {
	// Memory is the VRAM size of the accelerator, e.g. "65535Mi".
	Memory string `json:"memory,omitempty" protobuf:"bytes,1,opt,name=memory"`

	// Cores is the number of cores of the accelerator, e.g. "128", "256".
	Cores string `json:"cores,omitempty" protobuf:"bytes,2,opt,name=cores"`

	// ComputeCapability is the compute capability of the accelerator, e.g. "8.0", "7.0".
	ComputeCapability string `json:"computeCapability,omitempty" protobuf:"bytes,3,opt,name=computeCapability"`

	// SlicedDetail is the pool's aggregated slicing capability for this accelerator group.
	SlicedDetail AcceleratorSlicedDetail `json:"slicedDetail,omitempty" protobuf:"bytes,4,opt,name=slicedDetail"`

	// CPU describes the CPU information of the accelerator.
	CPU InstanceTypeAcceleratorCPU `json:"cpu,omitempty" protobuf:"bytes,5,opt,name=cpu"`
}

// InstanceTypeStatus describes the observed state of the InstanceType.
type InstanceTypeStatus struct {
	// Phase is the summary of conditions.
	Phase string `json:"phase" protobuf:"bytes,1,name=phase"`

	// PhaseMessage is the message of the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Accelerator is the allocatable-as-exclusive view: whole cards that are
	// entirely free, e.g. "1", "4".
	Accelerator InstanceTypeResource `json:"accelerator" protobuf:"bytes,3,name=accelerator"`

	// AcceleratorShared is the shareable view: per-card ownership shares (up to
	// SharedResourceMaxSize owners per card) summed over free and already-shared
	// cards.
	AcceleratorShared InstanceTypeResource `json:"acceleratorShared" protobuf:"bytes,4,name=acceleratorShared"`

	// AcceleratorSliced is the sliceable view: per-card VRAM-percent units (one
	// hundred per card) summed over free and already-sliced cards.
	AcceleratorSliced InstanceTypeResource `json:"acceleratorSliced" protobuf:"bytes,5,name=acceleratorSliced"`

	// CPU is the CPU resource of the InstanceType, e.g. "4", "8".
	CPU InstanceTypeResource `json:"cpu" protobuf:"bytes,6,name=cpu"`

	// Entrance is the name of the namespaced LocalQueue that fronts this
	// InstanceType's backing ClusterQueue — the value a workload sets as its
	// "kueue.x-k8s.io/queue-name" label to be admitted. It is derived from the
	// InstanceType name (see nodefeature.FormatLocalQueueName).
	Entrance string `json:"entrance,omitempty" protobuf:"bytes,7,opt,name=entrance"`

	// Detail is the observed hardware descriptor of the InstanceType, computed by the
	// reconciler from the matched ResourceFlavor's notes and the pool's Devices ledger.
	Detail InstanceTypeDetail `json:"detail,omitempty" protobuf:"bytes,8,opt,name=detail"`
}

// InstanceTypeResource describes the resource of the InstanceType.
type InstanceTypeResource struct {
	// OnceMaxRequest is the maximum value of the resource that can be requested once.
	//
	// This is a soft limitation. Requesting this value may result in scheduling failure.
	OnceMaxRequest resource.Quantity `json:"onceMaxRequest,omitempty" protobuf:"bytes,1,opt,name=onceMaxRequest"`

	// Remaining is the remaining requestable value of the resource.
	Remaining resource.Quantity `json:"remaining,omitempty" protobuf:"bytes,2,opt,name=remaining"`

	// Capacity is the total value of the resource.
	Capacity resource.Quantity `json:"capacity,omitempty" protobuf:"bytes,3,opt,name=capacity"`
}

// InstanceTypeUnitResources describes the unit resources of the InstanceType.
type InstanceTypeUnitResources struct {
	// CPU is the unit CPU resource(cores) of the InstanceType.
	CPU string `json:"cpu,omitempty" protobuf:"bytes,1,opt,name=cpu"`

	// RAM is the unit RAM resource(Mi) of the InstanceType.
	RAM string `json:"ram,omitempty" protobuf:"bytes,3,opt,name=ram"`
}

// InstanceTypeList holds the list of InstanceType.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceTypeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstanceType `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*InstanceTypeList)(nil)
