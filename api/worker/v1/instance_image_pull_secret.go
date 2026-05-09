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

	Spec InstanceImagePullSecretSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
}

// InstanceImagePullSecretSpec defines the desired state of InstanceImagePullSecret.
type InstanceImagePullSecretSpec struct {
	// DisplayName is the display name of the InstanceImagePullSecret.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,1,opt,name=displayName"`

	// Description is the description of the InstanceImagePullSecret.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,2,opt,name=description"`

	// Registry is the registry of the image pull secret.
	//
	// This field is a write-only input,
	// and it is required in writing.
	Registry string `json:"registry,omitempty" protobuf:"bytes,3,opt,name=registry"`

	// Username is the username of the image pull secret.
	//
	// This field is a write-only input,
	// and it is required in writing.
	Username string `json:"username,omitempty" protobuf:"bytes,4,opt,name=username"`

	// Password is the password of the image pull secret.
	//
	// This field is a write-only input,
	// and it is required in writing.
	Password string `json:"password,omitempty" protobuf:"bytes,5,opt,name=password"`

	// Email is the email of the image pull secret.
	//
	// This field is a write-only input,
	// and it is optional in writing.
	Email string `json:"email,omitempty" protobuf:"bytes,6,opt,name=email"`
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
