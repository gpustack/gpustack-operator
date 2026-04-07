package v1

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstancePersistentVolume is the schema for worker.gpustack.ai.
//
// Underhood, an InstancePersistentVolume is mapping to a Kubernetes PersistentVolumeClaim,
// and the InstancePersistentVolume's name is the same as the Kubernetes PersistentVolumeClaim's name.
//
// +genclient:onlyVerbs=create,get,list,watch,delete,deleteCollection
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"]
type InstancePersistentVolume struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   InstancePersistentVolumeSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status InstancePersistentVolumeStatus `json:"status,omitempty" protobuf:"bytes,3,name=status"`
}

var _ runtime.Object = (*InstancePersistentVolume)(nil)

// InstancePersistentVolumeSpec defines the desired state of InstancePersistentVolume.
type InstancePersistentVolumeSpec struct {
	// Type is the name of Kubernetes StorageClass that provisions corresponding Kubernetes PersistentVolume.
	Type *string `json:"type,omitempty" protobuf:"bytes,1,opt,name=type"`

	// Capacity is the capacity of the InstanceVolume.
	//
	// +required
	Capacity resource.Quantity `json:"capacity" protobuf:"bytes,2,name=capacity"`

	// AccessMode is the access mode of the InstanceVolume.
	//
	// +k8s:validation:enum=["ReadWriteMany","ReadWriteOnce","ReadOnlyMany","ReadWriteOncePod"]
	AccessMode *core.PersistentVolumeAccessMode `json:"accessMode,omitempty" protobuf:"bytes,3,opt,name=accessMode"`
}

// InstancePersistentVolumeStatus defines the observed state of InstancePersistentVolume.
type InstancePersistentVolumeStatus struct {
	// Phase is the phase of the InstanceVolume.
	Phase core.PersistentVolumeClaimPhase `json:"phase,omitempty" protobuf:"bytes,1,opt,name=phase"`

	// Volume is the reference to the Kubernetes PersistentVolume that is bound to the InstanceVolume.
	Volume *core.ObjectReference `json:"volume,omitempty" protobuf:"bytes,2,opt,name=volume"`
}

// InstancePersistentVolumeList holds the list of InstancePersistentVolume.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstancePersistentVolumeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstancePersistentVolume `json:"items" protobuf:"bytes,2,name=items"`
}

var _ runtime.Object = (*InstancePersistentVolumeList)(nil)
