package v1

import (
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstancePersistentVolumeEvents is the events of an InstancePersistentVolume subresource,
// which provides the events related to the underlying Kubernetes PersistentVolumeClaim.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstancePersistentVolumeEvents struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []core.Event `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*InstancePersistentVolumeEvents)(nil)
