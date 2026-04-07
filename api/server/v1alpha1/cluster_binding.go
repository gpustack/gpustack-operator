package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterBinding is the schema for server.gpustack.ai.
//
// The namespace is the Project name, which means the ClusterBinding is project-scoped.
// And the name of ClusterBinding must be same as the Cluster name.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced"
type ClusterBinding struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// ClusterRef can refer to a Cluster in the global namespace.
	ClusterRef ClusterReference `json:"clusterRef" protobuf:"bytes,2,name=clusterRef"`
}

var _ runtime.Object = (*ClusterBinding)(nil)

// ClusterBindingList contains a list of ClusterBinding.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterBindingList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ClusterBinding `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ClusterBindingList)(nil)
