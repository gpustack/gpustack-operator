package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// InstanceType is the schema for worker.gpustack.ai.
//
// Underhood, an InstanceType is mapping to a Kueue ClusterQueue,
// and the InstanceType's name is the same as the ClusterQueue's name.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["instype"],subResources=["status"]
type InstanceType workercore.InstanceType

var _ runtime.Object = (*InstanceType)(nil)

// InstanceTypeList holds the list of InstanceType.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceTypeList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []InstanceType `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*InstanceTypeList)(nil)
