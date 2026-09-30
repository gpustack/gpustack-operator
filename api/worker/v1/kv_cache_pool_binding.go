package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// KVCachePoolBinding is the schema for worker.gpustack.ai.
//
// KVCachePoolBinding proxies the v1alpha1.KVCachePoolBinding.
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
