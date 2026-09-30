package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// KVCachePool declares a backend quota shared through namespace bindings.
//
// Configuration changes proxy to the v1alpha1 resource. Controller-owned status is read-only
// through the main object; the public API serves no status subresource. Main-resource writes
// preserve the backing resource's status.
//
// +genclient
// +genclient:noStatus
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["kvcp"]
type KVCachePool workercore.KVCachePool

var _ runtime.Object = (*KVCachePool)(nil)

// KVCachePoolList holds the list of KVCachePools.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KVCachePoolList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KVCachePool `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*KVCachePoolList)(nil)
