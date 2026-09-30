package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TopologySource is the schema for worker.gpustack.ai.
//
// TopologySource proxies the v1alpha1.TopologySource.
//
// +genclient
// +genclient:noStatus
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["toposrc"]
type TopologySource workercore.TopologySource

var _ runtime.Object = (*TopologySource)(nil)

// TopologySourceList holds the list of TopologySources.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type TopologySourceList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []TopologySource `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*TopologySourceList)(nil)
