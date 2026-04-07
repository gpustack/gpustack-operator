package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceImagePullSecret is the schema for worker.gpustack.ai.
//
// Underhood, an InstanceImagePullSecret is mapping to a Kubernetes Secret,
// and the InstanceImagePullSecret's name is the same as the Kubernetes Secret's name.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"]
type InstanceImagePullSecret struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Data is the image pull secret data.
	//
	// This field is a write-only input,
	// and it is expected to be a JSON string that contains the image pull secret data.
	Data string `json:"data,omitempty" protobuf:"bytes,2,opt,name=data"`
}

var _ runtime.Object = (*InstanceSSHPublicKey)(nil)

// InstanceImagePullSecretList holds the list of InstanceImagePullSecret.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceImagePullSecretList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstanceImagePullSecret `json:"items" protobuf:"bytes,2,name=items"`
}

var _ runtime.Object = (*InstanceImagePullSecretList)(nil)
