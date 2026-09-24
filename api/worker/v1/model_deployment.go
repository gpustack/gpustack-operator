package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelDeployment is the v1 view of a managed serving deployment.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],subResources=["status"]
type ModelDeployment workercore.ModelDeployment

var _ runtime.Object = (*ModelDeployment)(nil)

// ModelDeploymentList holds the list of ModelDeployments.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []ModelDeployment `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentList)(nil)
