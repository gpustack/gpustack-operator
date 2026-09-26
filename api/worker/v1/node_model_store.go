package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// NodeModelStore is the v1 view of one node's model cache: what the node holds, how far its
// downloads are, and its effective configuration. Its writers, the worker and the node's plugin,
// write the v1alpha1 resource; this view serves get, list, watch and delete, the last for the
// garbage collector, which watches the group's preferred version.
//
// +genclient
// +genclient:nonNamespaced
// +genclient:onlyVerbs=get,list,watch,delete
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"]
type NodeModelStore workercore.NodeModelStore

var _ runtime.Object = (*NodeModelStore)(nil)

// NodeModelStoreList holds the list of NodeModelStores.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NodeModelStoreList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []NodeModelStore `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*NodeModelStoreList)(nil)
