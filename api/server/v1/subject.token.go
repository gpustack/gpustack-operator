package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SubjectToken is the subresource of Subject for token request.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SubjectToken struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   SubjectTokenSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status SubjectTokenStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*SubjectToken)(nil)

// SubjectTokenSpec defines the desired state of SubjectToken.
type SubjectTokenSpec struct {
	// ExpirationSeconds is the requested duration of validity of the request. The
	// token issuer may return a token with a different validity duration so a
	// client needs to check the 'expiration' field in a response.
	//
	// The value must be non-negative.
	// The maximum value is controlled by the loopback Kubernetes Cluster ApiServer.
	//
	// +k8s:validation:minimum=0
	// +k8s:validation:exclusiveMinimum
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty" protobuf:"varint,1,opt,name=expirationSeconds"`
}

// SubjectTokenStatus defines the observed state of SubjectToken.
type SubjectTokenStatus struct {
	// Token is the token of the SubjectToken.
	Token string `json:"token" protobuf:"bytes,1,name=token"`

	// ExpirationTimestamp is the time of expiration of the returned token.
	ExpirationTimestamp meta.Time `json:"expirationTimestamp" protobuf:"bytes,2,name=expirationTimestamp"`
}
