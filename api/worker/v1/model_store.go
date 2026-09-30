package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelStore is the schema for worker.gpustack.ai.
//
// ModelStore proxies the v1alpha1.ModelStore.
//
// +genclient
// +genclient:noStatus
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["ms"]
type ModelStore workercore.ModelStore

var _ runtime.Object = (*ModelStore)(nil)

// ModelStoreList holds the list of ModelStores.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelStoreList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelStore `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelStoreList)(nil)
