package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelKernelArtifact is the v1 view of a verified single-GPU kernel bundle and its residency.
//
// Main-resource operations proxy to the v1alpha1 resource. No writable status subresource is
// served on v1 because status authorizes kernel CSI mounts and is guarded to the worker identity
// on v1alpha1; status is read through the main object.
//
// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mkart"]
type ModelKernelArtifact workercore.ModelKernelArtifact

var _ runtime.Object = (*ModelKernelArtifact)(nil)

// ModelKernelArtifactList holds the list of ModelKernelArtifacts.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelKernelArtifactList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelKernelArtifact `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelKernelArtifactList)(nil)
