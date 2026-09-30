package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelStoreBinding grants a namespace a model-cache budget and permission to pin artifacts.
//
// Configuration changes proxy to the v1alpha1 resource. Controller-owned status is read-only
// through the main object; the public API serves no status subresource. Main-resource writes
// preserve the backing resource's status.
//
// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["msb"]
type ModelStoreBinding workercore.ModelStoreBinding

var _ runtime.Object = (*ModelStoreBinding)(nil)

// ModelStoreBindingList holds the list of ModelStoreBindings.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelStoreBindingList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelStoreBinding `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelStoreBindingList)(nil)
