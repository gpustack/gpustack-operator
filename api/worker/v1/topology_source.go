package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TopologySource selects the topology inventory used to form Kueue placement hierarchies.
//
// Configuration changes proxy to the v1alpha1 resource. Controller-owned status is read-only
// through the main object; the public API serves no status subresource. Main-resource writes
// preserve the backing resource's status.
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
