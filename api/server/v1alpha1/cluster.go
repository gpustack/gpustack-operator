package v1alpha1

import (
	"errors"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	api "gpustack.ai/gpustack/api/v1"
)

// Cluster is the schema for server.gpustack.ai.
//
// The namespace is the Team name, which means the Cluster is team-scoped.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",subResources=["status"]
type Cluster struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ClusterSpec   `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	Status ClusterStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*Cluster)(nil)

// ClusterReference is the reference of the cluster.
type ClusterReference struct {
	// Namespace is the namespace of the cluster.
	Namespace string `json:"namespace" protobuf:"bytes,1,name=namespace"`

	// Name is the name of the cluster.
	Name string `json:"name" protobuf:"bytes,2,name=name"`
}

// ToNamespacedName converts to types.NamespacedName.
func (in *ClusterReference) ToNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: in.Namespace,
		Name:      in.Name,
	}
}

// ClusterType describes the type of Cluster.
// +enum
type ClusterType string

const (
	// ClusterTypeLoopback is a local cluster,
	// which reuse the loopback config to connect to the cluster.
	ClusterTypeLoopback ClusterType = "Loopback"
	// ClusterTypeProxy is a proxy cluster,
	// which must provide a config to connect to the cluster.
	ClusterTypeProxy ClusterType = "Proxy"
	// ClusterTypeReverseProxy is a reverse proxy cluster,
	// which use the in-memory config and dual connection to connect to the cluster.
	ClusterTypeReverseProxy ClusterType = "ReverseProxy"
)

func (in ClusterType) String() string {
	return string(in)
}

func (in ClusterType) Validate() error {
	switch in {
	case ClusterTypeLoopback, ClusterTypeProxy, ClusterTypeReverseProxy:
		return nil
	default:
		return errors.New("invalid cluster type")
	}
}

// ClusterSpec defines the desired spec of Cluster.
type ClusterSpec struct {
	// Type is the type of the Cluster.
	//
	// +k8s:validation:enum=["Loopback","Proxy","ReverseProxy"]
	// +k8s:validation:cel[0]:rule="oldSelf == self"
	// +k8s:validation:cel[0]:message="type is immutable field"
	Type ClusterType `json:"type" protobuf:"bytes,1,name=type,casttype=ClusterType"`

	// DisplayName is the display name of the cluster.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,2,opt,name=displayName"`

	// Description is the description of the cluster.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,3,opt,name=description"`
}

// ClusterStatus defines the observed state of Cluster.
type ClusterStatus struct {
	// ConfigSecretName is the name of the secret storing the config of the cluster,
	ConfigSecretName string `json:"configSecretName,omitempty" protobuf:"bytes,1,opt,name=configSecretName"`

	// Endpoint is the endpoint of the cluster.
	Endpoint string `json:"endpoint,omitempty" protobuf:"bytes,2,opt,name=endpoint"`

	// Version is the version of the cluster.
	Version string `json:"version,omitempty" protobuf:"bytes,3,opt,name=version"`

	// CA is the certificate authority of the cluster.
	CA string `json:"ca,omitempty" protobuf:"bytes,4,opt,name=ca"`

	// Status includes phase, phase message and conditions.
	api.Status `json:",inline" protobuf:"bytes,5,opt,name=status"`
}

// ClusterList holds the list of Cluster.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Cluster `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ClusterList)(nil)
