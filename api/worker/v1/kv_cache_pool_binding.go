package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// KVCachePoolBinding grants a namespace a cache quota and reuse domain within a pool.
//
// Configuration changes proxy to the v1alpha1 resource. Controller-owned status is read-only
// through the main object; the public API serves no status subresource. Main-resource writes
// preserve the backing resource's status.
//
// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["kvcpb"]
type KVCachePoolBinding workercore.KVCachePoolBinding

var _ runtime.Object = (*KVCachePoolBinding)(nil)

// KVCachePoolBindingList holds the list of KVCachePoolBindings.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KVCachePoolBindingList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KVCachePoolBinding `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*KVCachePoolBindingList)(nil)
