package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelPrefetch is the v1 view of a namespace's model residency: what is being warmed where, and
// how far it has got.
//
// It proxies the v1alpha1 resource for every verb. It is status-only by design: per-node facts
// already live in the node's report, so the view serves the aggregates and adds no subresource of
// its own.
//
// +genclient
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
