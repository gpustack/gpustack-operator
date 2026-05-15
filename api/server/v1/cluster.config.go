package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
)

// ClusterConfig is the subresource of non-loopback Cluster for extracting configuration.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterConfig struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ClusterConfigSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status ClusterConfigStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ClusterConfig)(nil)

// ClusterConfigSpec defines the desired state of ClusterConfig.
type ClusterConfigSpec struct {
	// Config is the config of the non-loopback Cluster.
	//
	// Immutable after creation.
	Config string `json:"config,omitempty" protobuf:"bytes,1,opt,name=config"`
}

// ClusterConfigStatus defines the observed state of ClusterConfig.
type ClusterConfigStatus struct {
	// Type is the type of Cluster.
	Type servercore.ClusterType `json:"type" protobuf:"bytes,1,name=type,casttype=ClusterType"`

	// Config is the config of the non-loopback Cluster.
	Config string `json:"config,omitempty" protobuf:"bytes,2,opt,name=config"`
}
