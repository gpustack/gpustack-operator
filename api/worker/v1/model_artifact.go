package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelArtifact is the v1 view of a model's weights: where they come from and where they are.
//
// It proxies the v1alpha1 resource for every verb and serves the progress subresource. IT HAS NO
// WRITABLE STATUS SUBRESOURCE: the proxy writes with the worker's identity, and the status guard
// admits exactly that identity to a ModelArtifact's status, which mounts are authorized by. The status
// is read through the object.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"]
type ModelArtifact workercore.ModelArtifact

var _ runtime.Object = (*ModelArtifact)(nil)

// ModelArtifactList holds the list of ModelArtifacts.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelArtifactList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []ModelArtifact `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelArtifactList)(nil)
