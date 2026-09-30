package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelPrefetch is the v1 view of a namespace's model residency: what is being warmed where, and
// how far it has got.
//
// It proxies configuration changes to the v1alpha1 resource. Status reports aggregated node
// readiness through the main object; the public API serves no status or progress subresource.
//
// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"]
type ModelPrefetch workercore.ModelPrefetch

var _ runtime.Object = (*ModelPrefetch)(nil)

// ModelPrefetchList holds the list of ModelPrefetches.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelPrefetchList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []ModelPrefetch `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelPrefetchList)(nil)
