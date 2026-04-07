package v1

import (
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Team is the schema for server.gpustack.ai.
//
// The namespace is the system namespace, which means the Team is system-scoped.
//
// Underhood, a Team is mapping to a Kubernetes Namespace,
// and the Team's name is the same as the Namespace's name.
//
// +genclient
// +genclient:onlyVerbs=create,get,list,watch,apply,update,patch,delete,deleteCollection
// +genclient:method=GetSubjects,verb=get,subresource=subjects,result=TeamSubjects
// +genclient:method=UpdateSubjects,verb=update,subresource=subjects,input=TeamSubjects,result=TeamSubjects
// +genclient:method=PatchSubjects,verb=update,subresource=subjects,input=TeamSubjects,result=TeamSubjects
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["team"]
type Team struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   TeamSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status TeamStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*Team)(nil)

// TeamSpec defines the desired state of Team.
type TeamSpec struct {
	// DisplayName is the display name of the team.
	//
	// +k8s:validation:maxLength=64
	DisplayName string `json:"displayName,omitempty" protobuf:"bytes,1,opt,name=displayName"`

	// Description is the description of the team.
	//
	// +k8s:validation:maxLength=1024
	Description string `json:"description,omitempty" protobuf:"bytes,2,opt,name=description"`
}

// TeamStatus defines the observed state of Team.
type TeamStatus struct {
	// Phase is the current phase of the team.
	Phase core.NamespacePhase `json:"phase" protobuf:"bytes,1,opt,name=phase,casttype=k8s.io/api/core/v1.NamespacePhase"`
}

// TeamList holds the list of Team.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type TeamList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Team `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*TeamList)(nil)
