package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ModelArtifactProgress is where a ModelArtifact's content is across the cluster's nodes, computed
// when asked and never stored: the nodes' NodeModelStores, with each downloading node's bytes read
// live from its plugin where it answers.
//
// It is an aggregate and NAMES NO NODE: it is namespaced and a tenant reads it, and node names would
// hand every tenant the cluster's topology. The per-node view is the NodeModelStore.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelArtifactProgress struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Timestamp is when it was computed.
	Timestamp meta.Time `json:"timestamp" protobuf:"bytes,2,opt,name=timestamp"`

	// ManifestDigest and SizeBytes are the content's, from the artifact's resolution.
	ManifestDigest string `json:"manifestDigest,omitempty" protobuf:"bytes,3,opt,name=manifestDigest"`
	SizeBytes      int64  `json:"sizeBytes,omitempty" protobuf:"varint,4,opt,name=sizeBytes"`

	// Ready, Downloading and Failed count the nodes holding the content published, downloading it,
	// and waiting to retry a failed attempt.
	Ready       int32 `json:"ready" protobuf:"varint,5,opt,name=ready"`
	Downloading int32 `json:"downloading" protobuf:"varint,6,opt,name=downloading"`
	Failed      int32 `json:"failed" protobuf:"varint,7,opt,name=failed"`

	// DownloadingPercent is the mean progress of the downloading nodes, each a whole copy, in whole
	// percent; absent while no node downloads.
	//
	// +optional
	DownloadingPercent *int32 `json:"downloadingPercent,omitempty" protobuf:"varint,8,opt,name=downloadingPercent"`

	// DownloadingBytes is what the downloading nodes hold of it, summed.
	DownloadingBytes int64 `json:"downloadingBytes" protobuf:"varint,9,opt,name=downloadingBytes"`

	// Live counts the downloading nodes whose bytes were read from their plugin for this answer; the
	// others contribute the value their node last wrote, on its 5% threshold.
	Live int32 `json:"live" protobuf:"varint,10,opt,name=live"`

	// FailureReasons counts the failed nodes by reason, without a message or a node.
	//
	// +listType=atomic
	// +optional
	FailureReasons []ModelArtifactProgressReason `json:"failureReasons,omitempty" protobuf:"bytes,11,rep,name=failureReasons"`

	// Reason says why there is nothing to count: the artifact is not resolved, or its claim source is
	// mounted from its volume and never downloaded.
	//
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,12,opt,name=reason"`
}

var _ runtime.Object = (*ModelArtifactProgress)(nil)

// ModelArtifactProgressReason is how many failed nodes share a reason.
type ModelArtifactProgressReason struct {
	Reason string `json:"reason" protobuf:"bytes,1,opt,name=reason"`
	Count  int32  `json:"count" protobuf:"varint,2,opt,name=count"`
}
