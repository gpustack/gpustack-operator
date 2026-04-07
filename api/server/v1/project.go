package v1

import (
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Project is the schema for server.gpustack.ai.
//
// The namespace is the Team name, which means the Project is team-scoped.
//
// Underhood, a Project is mapping to a Kubernetes Namespace,
// and the Project's name is the same as the Namespace's name.
//
// +genclient
// +genclient:onlyVerbs=create,get,list,watch,apply,update,patch,delete,deleteCollection
// +genclient:method=GetClusters,verb=get,subresource=clusters,result=ProjectClusters
// +genclient:method=UpdateClusters,verb=update,subresource=clusters,input=ProjectClusters,result=ProjectClusters
// +genclient:method=PatchClusters,verb=update,subresource=clusters,input=ProjectClusters,result=ProjectClusters
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["proj"]
type Project struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ProjectSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status ProjectStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*Project)(nil)

// ProjectSpec defines the desired state of Project.
type ProjectSpec struct {
	// DisplayName is the display name of the project.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,1,opt,name=displayName"`

	// Description is the description of the project.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,2,opt,name=description"`
}

// ProjectStatus defines the observed state of Project.
type ProjectStatus struct {
	// Team is the team that the project belongs to.
	Team string `json:"team" protobuf:"bytes,1,name=team"`

	// Phase is the current phase of the project.
	Phase core.NamespacePhase `json:"phase" protobuf:"bytes,2,name=phase,casttype=k8s.io/api/core/v1.NamespacePhase"`
}

// ProjectList holds the list of Project.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ProjectList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Project `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ProjectList)(nil)
