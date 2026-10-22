package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelDeploymentScaler is the v1 view of a metric-driven replica planner.
//
// Configuration changes proxy to the v1alpha1 resource. The status subresource is served, so
// the planner's decisions are readable without going through the alpha version.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],subResources=["status"]
type ModelDeploymentScaler workercore.ModelDeploymentScaler

var _ runtime.Object = (*ModelDeploymentScaler)(nil)

// ModelDeploymentScalerList holds the list of ModelDeploymentScalers.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentScalerList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []ModelDeploymentScaler `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentScalerList)(nil)
