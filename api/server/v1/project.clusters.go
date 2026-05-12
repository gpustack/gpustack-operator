package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
)

// ProjectClusters is the subresource of Project for manage cluster bindings.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ProjectClusters struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// +patchStrategy=merge
	// +patchMergeKey=name
	// +listType=map
	// +listMapKey=name
	Items []ProjectCluster `json:"items" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,2,rep,name=items"`
}

// HasCluster checks if the given Cluster exists in the project clusters.
func (in *ProjectClusters) HasCluster(cls *Cluster) bool {
	for i := range in.Items {
		if in.Items[i].Namespace == cls.Namespace && in.Items[i].Name == cls.Name {
			return true
		}
	}

	return false
}

var _ runtime.Object = (*ProjectClusters)(nil)

// ProjectCluster is the schema for the gpustack API.
type ProjectCluster struct {
	// ClusterReference refers to the Cluster.
	servercore.ClusterReference `json:",inline" protobuf:"bytes,1,opt,name=clusterReference"`
}
