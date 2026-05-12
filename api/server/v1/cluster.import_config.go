package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
)

// ClusterImportConfig is the subresource of non-loopback Cluster for fetching importing configuration.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterImportConfig struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Status ClusterImportConfigStatus `json:"status,omitempty" protobuf:"bytes,2,opt,name=status"`
}

var _ runtime.Object = (*ClusterImportConfig)(nil)

// ClusterImportConfigStatus defines the observed state of ClusterImportConfig.
type ClusterImportConfigStatus struct {
	// Type is the type of Cluster,
	// it is a read-only field to expose the type of the non-loopback Cluster.
	Type servercore.ClusterType `json:"type" protobuf:"bytes,1,name=type,casttype=ClusterType"`

	// Config is the config of the non-loopback Cluster,
	// it is a read-only field to expose the config of the non-loopback Cluster.
	Config string `json:"config,omitempty" protobuf:"bytes,2,opt,name=config"`
}
