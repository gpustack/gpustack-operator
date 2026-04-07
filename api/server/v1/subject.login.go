package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SubjectLogin is the subresource of Subject for login request.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"]
type SubjectLogin struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   SubjectLoginSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status SubjectLoginStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*SubjectLogin)(nil)

// SubjectLoginSpec defines the desired state of SubjectLogin.
type SubjectLoginSpec struct {
	// Credential is the credential of the subject,
	// it is provided as a write-only input field.
	//
	// +k8s:validation:format="password"
	Credential string `json:"credential,omitempty" protobuf:"bytes,1,opt,name=credential"`
}

// SubjectLoginStatus defines the observed state of SubjectLogin.
type SubjectLoginStatus struct {
	// Token is the token of the SubjectLogin.
	Token string `json:"token" protobuf:"bytes,1,opt,name=token"`
}
