package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// AcceleratorType is the schema for worker.gpustack.ai.
//
// An AcceleratorType represents an accelerated hardware pool backed by a canonical Kueue
// ClusterQueue, without workspace CPU, memory, or local-storage presets.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["acctype"],subResources=["status"]
type AcceleratorType struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   AcceleratorTypeSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status AcceleratorTypeStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*AcceleratorType)(nil)

// AcceleratorTypeSpec defines the desired spec of AcceleratorType. Its field order and protobuf
// numbering group the admin-editable fields (DisplayName, Description, Inactive) first, followed
// by the immutable pool identity inputs.
type AcceleratorTypeSpec struct {
	// DisplayName is a human-friendly label for the AcceleratorType. It is admin-editable and, for
	// a derived AcceleratorType, stamped at derivation time.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,1,opt,name=displayName"`

	// Description is a free-form admin annotation for the AcceleratorType.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,2,opt,name=description"`

	// Inactive takes the AcceleratorType out of service. When true, admission refuses new
	// workloads targeting this AcceleratorType, and the backing ClusterQueue is held when every
	// fronting type on the pool is inactive.
	Inactive bool `json:"inactive,omitempty" protobuf:"varint,3,opt,name=inactive"`

	// AcceleratorGroup is the accelerator group (the acceleratable node key) of the
	// AcceleratorType, e.g. "nvidia-a10g". It selects the accelerator pool the type schedules
	// onto and is a required admin-writable input.
	//
	// +required
	AcceleratorGroup string `json:"acceleratorGroup" protobuf:"bytes,4,opt,name=acceleratorGroup"`

	// GeneralGroup is the general(CPU) group (the general node key) of the AcceleratorType, e.g.
	// "amd-epyc-7763", or the literal "generic" for a CPU-manufacturer-agnostic pool. The
	// mutating webhook defaults an empty value to "generic"; it participates as a scheduling
	// discriminator only when instance-type-aware-cpu-manufacturer is enabled.
	GeneralGroup string `json:"generalGroup,omitempty" protobuf:"bytes,5,opt,name=generalGroup"`

	// OS is the operating system of the AcceleratorType, e.g. "linux", "windows".
	//
	// It is a required admin-writable input, enforced by the validating webhook.
	//
	// +required
	OS string `json:"os" protobuf:"bytes,6,opt,name=os"`

	// Arch is the architecture of the AcceleratorType, e.g. "amd64", "arm64".
	//
	// It is a required admin-writable input, enforced by the validating webhook.
	//
	// +required
	Arch string `json:"arch" protobuf:"bytes,7,opt,name=arch"`
}

// AcceleratorTypeStatus describes the observed state of the AcceleratorType.
type AcceleratorTypeStatus struct {
	// Detail is the observed hardware descriptor of the AcceleratorType, computed by the
	// reconciler from the matched ResourceFlavor's notes and the pool's Devices ledger.
	Detail InstanceTypeDetail `json:"detail,omitempty" protobuf:"bytes,1,opt,name=detail"`

	// Entrance is the name of the namespaced LocalQueue that fronts this
	// AcceleratorType's backing ClusterQueue — the value a workload sets as its
	// "kueue.x-k8s.io/queue-name" label to be admitted.
	Entrance string `json:"entrance,omitempty" protobuf:"bytes,2,opt,name=entrance"`

	// Phase is the summary of conditions.
	Phase string `json:"phase" protobuf:"bytes,3,name=phase"`

	// PhaseMessage is the message of the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,4,opt,name=phaseMessage"`

	// Accelerator is the allocatable-as-exclusive view: whole accelerators that are
	// entirely free, e.g. "1", "4".
	Accelerator InstanceTypeResource `json:"accelerator" protobuf:"bytes,5,name=accelerator"`

	// AcceleratorShared is the shareable view: per-accelerator ownership shares (up to
	// SharedResourceMaxSize owners per accelerator) summed over free and already-shared
	// accelerators.
	AcceleratorShared InstanceTypeResource `json:"acceleratorShared" protobuf:"bytes,6,name=acceleratorShared"`

	// AcceleratorSliced is the sliceable view: per-accelerator VRAM-percent units (one
	// hundred per accelerator) summed over free and already-sliced accelerators.
	AcceleratorSliced InstanceTypeResource `json:"acceleratorSliced" protobuf:"bytes,7,name=acceleratorSliced"`

	// AcceleratorPartitioned is the hardware-partitionable view: the partition instances the
	// pool's partitioned accelerators can still host, summed over those accelerators. It is
	// disjoint from the three views above — an accelerator in a partitioning mode can serve no
	// other kind of claim — so a pool with no partitioned accelerator reports zero here.
	AcceleratorPartitioned InstanceTypePartitionedResource `json:"acceleratorPartitioned" protobuf:"bytes,8,name=acceleratorPartitioned"`

	// Conditions is the finer view of the AcceleratorType's state.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,9,rep,name=conditions"` // nolint: lll
}

// AcceleratorTypeList holds the list of AcceleratorType.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AcceleratorTypeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []AcceleratorType `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*AcceleratorTypeList)(nil)
