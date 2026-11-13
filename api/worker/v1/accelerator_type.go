package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// AcceleratorType is the schema for worker.gpustack.ai.
//
// An AcceleratorType represents an accelerated hardware pool backed by a canonical Kueue
// ClusterQueue, without workspace CPU, memory, or local-storage presets.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["acctype"],subResources=["status"]
type AcceleratorType workercore.AcceleratorType

var _ runtime.Object = (*AcceleratorType)(nil)

// AcceleratorTypeList holds the list of AcceleratorType.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AcceleratorTypeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []AcceleratorType `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*AcceleratorTypeList)(nil)
