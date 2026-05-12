package v1

import (
	"errors"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TeamSubjects is the subresource of Team for manage subjects.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type TeamSubjects struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// +patchStrategy=merge
	// +patchMergeKey=name
	// +listType=map
	// +listMapKey=name
	Items []TeamSubject `json:"items" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*TeamSubjects)(nil)

// TeamRole describes the role of  subject.
// +enum
type TeamRole string

const (
	// TeamRoleViewer is the role for team viewer.
	TeamRoleViewer TeamRole = "Viewer"
	// TeamRoleMember is the role for team member.
	TeamRoleMember TeamRole = "Member"
	// TeamRoleOwner is the role for team owner.
	TeamRoleOwner TeamRole = "Owner"
)

func (in TeamRole) String() string {
	return string(in)
}

func (in TeamRole) Validate() error {
	switch in {
	case TeamRoleViewer, TeamRoleMember, TeamRoleOwner:
		return nil
	default:
		return errors.New("invalid team role")
	}
}

// TeamSubject is the schema for the gpustack API.
type TeamSubject struct {
	// SubjectReference refers to the Subject.
	SubjectReference `json:",inline" protobuf:"bytes,2,opt,name=subjectReference"`

	// Role is the team role of the subject.
	//
	// +k8s:validation:enum=["Viewer","Member","Owner"]
	Role TeamRole `json:"role" protobuf:"bytes,1,name=role,casttype=TeamRole"`
}
