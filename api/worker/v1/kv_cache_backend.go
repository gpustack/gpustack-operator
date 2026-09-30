package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// KVCacheBackend declares a managed or external KV cache backend and reports its endpoints and capacity.
//
// Configuration changes proxy to the v1alpha1 resource. Controller-owned status is read-only
// through the main object; the public API serves no status subresource. Main-resource writes
// preserve the backing resource's status.
//
// +genclient
// +genclient:noStatus
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["kvcb"]
type KVCacheBackend workercore.KVCacheBackend

var _ runtime.Object = (*KVCacheBackend)(nil)

// KVCacheBackendList holds the list of KVCacheBackends.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KVCacheBackendList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KVCacheBackend `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*KVCacheBackendList)(nil)
