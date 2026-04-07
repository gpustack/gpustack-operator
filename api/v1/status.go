package v1

import meta "k8s.io/apimachinery/pkg/apis/meta/v1"

// Condition contains details for one aspect of the current state of this API Resource.
type Condition struct {
	// Type of condition in CamelCase or in foo.example.com/CamelCase.
	//
	// +required
	// +k8s:validation:pattern="^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$"
	// +k8s:validation:maxLength=316
	Type string `json:"type" protobuf:"bytes,1,opt,name=type"`

	// Status of the condition, one of True, False, Unknown.
	//
	// +required
	// +k8s:validation:enum=["True","False","Unknown"]
	Status meta.ConditionStatus `json:"status" protobuf:"bytes,2,opt,name=status"`

	// LastTransitionTime is the last time the condition transitioned from one status to another.
	// This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
	//
	// +required
	// +k8s:validation:format="datetime"
	LastTransitionTime meta.Time `json:"lastTransitionTime" protobuf:"bytes,3,name=lastTransitionTime"`

	// ObservedGeneration represents the .metadata.generation that the condition was set based upon.
	// For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9,
	// the condition is out of date with respect to the current state of the instance.
	//
	// +k8s:validation:minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,4,opt,name=observedGeneration"`

	// Reason contains a programmatic identifier indicating the reason for the condition's last transition.
	// Producers of specific condition types may define expected values and meanings for this field,
	// and whether the values are considered a guaranteed API.
	// The value should be a CamelCase string.
	//
	// +k8s:validation:maxLength=1024
	// +k8s:validation:pattern="^$|^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$"
	Reason string `json:"reason,omitempty" protobuf:"bytes,5,opt,name=reason"`

	// Message is a human readable message indicating details about the transition.
	// This may be an empty string.
	//
	// +k8s:validation:maxLength=32768
	Message string `json:"message,omitempty" protobuf:"bytes,6,opt,name=message"`
}

// Status includes phase, phase message and conditions.
type Status struct {
	// Phase is the summary of conditions.
	Phase string `json:"phase,omitempty" protobuf:"bytes,1,opt,name=phase"`

	// PhaseMessage is the message of the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Conditions holds the conditions for the object.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"`
}
