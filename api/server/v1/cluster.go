package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
)

// Cluster is the schema for server.gpustack.ai.
//
// The namespace is the Team name, which means the Cluster is team-scoped.
//
// Cluster proxies the v1alpha1.Cluster.
//
// +genclient
// +genclient:method=GetConfig,verb=get,subresource=config,result=ClusterConfig
// +genclient:method=UpdateConfig,verb=update,subresource=config,input=ClusterConfig,result=ClusterConfig
// +genclient:method=GetImportConfig,verb=get,subresource=importconfig,result=ClusterImportConfig
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["cls"],subResources=["status"]
type Cluster servercore.Cluster

var _ runtime.Object = (*Cluster)(nil)

// ClusterList holds the list of Cluster.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Cluster `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ClusterList)(nil)
