package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceTypeFlavor is an aggregated, read-only projection over the operator-owned Kueue
// ResourceFlavors — the catalog of hardware groups an InstanceType can be built on. Each
// distinct pool (accelerated or generic CPU-only) surfaces as one InstanceTypeFlavor, parsed
// from the flavor's note.gpustack.ai/* annotations, deduplicated, and sorted. It is served
// list-only by the aggregated apiserver: there is no backing CRD and no controller, so an
// administrator can discover the group + descriptor fields needed to author an InstanceType.
//
// +genclient
// +genclient:nonNamespaced
// +genclient:onlyVerbs=list
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["instypeflavor"]
type InstanceTypeFlavor struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec InstanceTypeFlavorSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
}

var _ runtime.Object = (*InstanceTypeFlavor)(nil)

// InstanceTypeFlavorSpec describes one pool aggregated from the ResourceFlavor notes. Its
// fields mirror the InstanceTypeSpec descriptor ordering — group + acceleratable +
// manufacturer/product/family + memory/cores — so the catalog and an InstanceType read
// consistently.
type InstanceTypeFlavorSpec struct {
	// Group is the pool key (the InstanceType group), e.g. "nvidia-a10g", "generic".
	Group string `json:"group,omitempty" protobuf:"bytes,1,opt,name=group"`

	// Acceleratable reports whether the pool represents accelerated hardware; a generic
	// (CPU-only) pool is false. It delimits generic from accelerated flavors.
	Acceleratable bool `json:"acceleratable" protobuf:"varint,2,name=acceleratable"`

	// Manufacturer is the device (or CPU) manufacturer, e.g. "nvidia", "generic".
	Manufacturer string `json:"manufacturer,omitempty" protobuf:"bytes,3,opt,name=manufacturer"`

	// Product is the product name, e.g. "NVIDIA A10G"; empty for a generic pool.
	Product string `json:"product,omitempty" protobuf:"bytes,4,opt,name=product"`

	// Family is the product family, e.g. "ampere"; empty for a generic pool.
	Family string `json:"family,omitempty" protobuf:"bytes,5,opt,name=family"`

	// Memory is the per-card VRAM, e.g. "24576Mi"; empty for a generic pool.
	Memory string `json:"memory,omitempty" protobuf:"bytes,6,opt,name=memory"`

	// Cores is the per-card accelerator core count, e.g. "9216"; empty for a generic pool.
	Cores string `json:"cores,omitempty" protobuf:"bytes,7,opt,name=cores"`

	// Sliceable reports whether the accelerator can be sliced; false for a generic pool.
	Sliceable bool `json:"sliceable,omitempty" protobuf:"varint,8,opt,name=sliceable"`
}

// InstanceTypeFlavorList holds the list of InstanceTypeFlavor.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceTypeFlavorList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstanceTypeFlavor `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*InstanceTypeFlavorList)(nil)
