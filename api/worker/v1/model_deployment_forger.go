package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelDeploymentForger is the v1 view of a closed-loop kernel probing and forging policy over one
// ModelDeployment.
//
// Configuration changes proxy to the v1alpha1 resource. No writable status subresource is served
// on v1 because status carries the worker-verified activeSandbox proof used to authorize node
// cache ingestion; status is read through the main object.
//
// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mdf"]
type ModelDeploymentForger workercore.ModelDeploymentForger

var _ runtime.Object = (*ModelDeploymentForger)(nil)

// ModelDeploymentForgerList holds the list of ModelDeploymentForgers.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentForgerList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelDeploymentForger `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentForgerList)(nil)
