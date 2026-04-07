package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Setting is the schema for gpustack.ai.
//
// The namespace is the system namespace, which means the Setting is system-scoped.
//
// Underhood, a Setting is mapping to a key-value pair of a Kubernetes Secret,
// and the Setting's name is the same as the Secret's data key.
//
// +genclient
// +genclient:onlyVerbs=get,list,watch,apply,update,patch
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["set"]
type Setting struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   SettingSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status SettingStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*Setting)(nil)

// SettingSpec defines the desired state of Setting.
type SettingSpec struct {
	// Value contains the configuration data,
	// it is provided as a write-only input field.
	Value *string `json:"value,omitempty" protobuf:"bytes,1,opt,name=value"`
}

// SettingStatus defines the observed state of Setting.
type SettingStatus struct {
	// Description is the description of the settings.
	Description string `json:"description,omitempty" protobuf:"bytes,1,opt,name=description"`

	// Editable indicates whether the setting is editable on UI.
	Editable bool `json:"editable" protobuf:"varint,2,name=editable"`

	// Sensitive indicates whether the setting is sensitive.
	Sensitive bool `json:"sensitive" protobuf:"varint,3,name=sensitive"`

	// Value is the current value of the setting,
	// it is provided as a read-only output field.
	//
	// "(sensitive)" returns if the setting is sensitive.
	Value string `json:"value" protobuf:"bytes,4,name=value"`
	// Value_ is the shadow of the Value,
	// it is provided for system processing only.
	//
	// DO NOT EXPOSE.
	Value_ string `json:"-"`
}

// SettingList holds the list of Setting.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SettingList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Setting `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*SettingList)(nil)
