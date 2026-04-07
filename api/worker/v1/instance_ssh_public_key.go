package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceSSHPublicKey is the schema for worker.gpustack.ai.
//
// Underhood, an InstanceSSHPublicKey is mapping to a Kubernetes Secret,
// and the InstanceSSHPublicKey's name is the same as the Kubernetes Secret's name.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"]
type InstanceSSHPublicKey struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Data is the SSH public key data.
	Data string `json:"data" protobuf:"bytes,2,name=data"`
}

var _ runtime.Object = (*InstanceSSHPublicKey)(nil)

// InstanceSSHPublicKeyList holds the list of InstanceSSHPublicKey.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceSSHPublicKeyList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstanceSSHPublicKey `json:"items" protobuf:"bytes,2,name=items"`
}

var _ runtime.Object = (*InstanceSSHPublicKeyList)(nil)
