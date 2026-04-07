package v1

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
// +genclient:onlyVerbs=get,list,watch
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["instype"]
type InstanceType struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   InstanceTypeSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status InstanceTypeStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*InstanceType)(nil)

// InstanceTypeSpec defines the desired spec of InstanceType.
type InstanceTypeSpec struct {
	// Group indicates the group of the InstanceType.
	Group string `json:"group" protobuf:"bytes,1,name=group"`

	// Acceleratable indicates whether the InstanceType is acceleratable.
	Acceleratable bool `json:"acceleratable" protobuf:"bytes,2,name=acceleratable"`

	// Manufacturer is the name of the InstanceType manufacturer, e.g. "amd", "nvidia", "intel".
	Manufacturer string `json:"manufacturer,omitempty" protobuf:"bytes,3,opt,name=manufacturer"`

	// Product is the name of the InstanceType product, e.g. "A100", "V100", "T4".
	Product string `json:"product,omitempty" protobuf:"bytes,4,opt,name=product"`

	// Memory is the VRAM size of the InstanceType, e.g. "65535Mi".
	Memory string `json:"memory,omitempty" protobuf:"bytes,5,opt,name=memory"`

	// Family is the family of the InstanceType, e.g. "ampere", "volta", "turing".
	Family string `json:"family,omitempty" protobuf:"bytes,6,opt,name=family"`

	// ComputeCapability is the compute capability of the InstanceType, e.g. "8.0", "7.0".
	ComputeCapability string `json:"computeCapability,omitempty" protobuf:"bytes,7,opt,name=computeCapability"`

	// Sliced indicates whether the InstanceType is sliced.
	// When Sliced is blank, that means the InstanceType is not sliced.
	Sliced int64 `json:"sliced,omitempty" protobuf:"varint,8,opt,name=sliced"`
}

// InstanceTypeStatus describes the observed state of the InstanceType.
type InstanceTypeStatus struct {
	// Phase is the summary of conditions.
	Phase string `json:"phase" protobuf:"bytes,1,name=phase"`

	// PhaseMessage is the message of the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Accelerator is the accelerator resource of the InstanceType, e.g. "1", "4".
	Accelerator InstanceTypeResource `json:"accelerator" protobuf:"bytes,3,name=accelerator"`

	// CPU is the CPU resource of the InstanceType, e.g. "4", "8".
	CPU InstanceTypeResource `json:"cpu" protobuf:"bytes,4,name=cpu"`

	// RAM is the RAM resource of the InstanceType, e.g. "40G", "16G".
	RAM InstanceTypeResource `json:"ram" protobuf:"bytes,5,name=ram"`

	// LocalStorage is the local storage resource of the InstanceType, e.g. "100G", "500G".
	LocalStorage InstanceTypeResource `json:"localStorage" protobuf:"bytes,6,name=localStorage"`
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

// InstanceTypeList holds the list of InstanceType.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceTypeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstanceType `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*InstanceTypeList)(nil)
