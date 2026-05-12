package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceLog is the subresource of Instance for extracting logs,
// which is the same as Kubernetes PodLog.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceLog struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
}

var _ runtime.Object = (*InstanceLog)(nil)

// TODO(thxCode): Support swagger doc generation for *Options types.

// InstanceLogOptions is the options of InstanceLog,
// which is the same as Kubernetes PodLogOptions.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceLogOptions struct {
	meta.TypeMeta `json:",inline"`

	// Follow the log stream of the Instance.
	Follow bool `json:"follow,omitempty" protobuf:"varint,1,opt,name=follow"`

	// A relative time in seconds before the current time from which to show logs.
	// If this value precedes the time an Instance was started,
	// only logs since the Instance start will be returned.
	// If this value is in the future, no logs will be returned.
	// Only one of sinceSeconds or sinceTime may be specified.
	//
	SinceSeconds *int64 `json:"sinceSeconds,omitempty" protobuf:"varint,2,opt,name=sinceSeconds"`

	// An RFC3339 timestamp from which to show logs. If this value
	// precedes the time an Instance was started, only logs since the Instance start will be returned.
	// If this value is in the future, no logs will be returned.
	// Only one of sinceSeconds or sinceTime may be specified.
	SinceTime *meta.Time `json:"sinceTime,omitempty" protobuf:"bytes,3,opt,name=sinceTime"`

	// If true, add an RFC3339 or RFC3339Nano timestamp at the beginning of every line
	// of log output.
	Timestamps bool `json:"timestamps,omitempty" protobuf:"varint,4,opt,name=timestamps"`

	// If set, the number of lines from the end of the logs to show. If not specified,
	// logs are shown from the creation of the container or sinceSeconds or sinceTime.
	TailLines *int64 `json:"tailLines,omitempty" protobuf:"varint,5,opt,name=tailLines"`

	// If set, the number of bytes to read from the server before terminating the
	// log output. This may not display a complete final line of logging, and may return
	// slightly more or slightly less than the specified limit.
	LimitBytes *int64 `json:"limitBytes,omitempty" protobuf:"varint,6,opt,name=limitBytes"`
}
