package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ModelDeploymentMetrics is a best-effort snapshot of the deployment's serving metrics.
// Missing measurements are listed explicitly; a measured zero remains a numeric zero.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentMetrics struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Timestamp       meta.Time `json:"timestamp" protobuf:"bytes,2,opt,name=timestamp"`
	// +listType=atomic
	Processing []ModelDeploymentMetricGauge `json:"processing,omitempty" protobuf:"bytes,3,rep,name=processing"`
	// +listType=atomic
	Queueing []ModelDeploymentMetricGauge `json:"queueing,omitempty" protobuf:"bytes,4,rep,name=queueing"`
	// +listType=atomic
	CacheHits []ModelDeploymentCacheHit `json:"cacheHits,omitempty" protobuf:"bytes,5,rep,name=cacheHits"`
	// +listType=atomic
	Missing []ModelDeploymentMetricMissing `json:"missing,omitempty" protobuf:"bytes,6,rep,name=missing"`
	Partial bool                           `json:"partial" protobuf:"varint,7,opt,name=partial"`
	// +listType=atomic
	Latency []ModelDeploymentMetricWindow `json:"latency,omitempty" protobuf:"bytes,8,rep,name=latency"`
	// +listType=atomic
	Traffic []ModelDeploymentMetricWindow `json:"traffic,omitempty" protobuf:"bytes,9,rep,name=traffic"`
	// +listType=atomic
	Transfer []ModelDeploymentMetricWindow `json:"transfer,omitempty" protobuf:"bytes,10,rep,name=transfer"`
}

var _ runtime.Object = (*ModelDeploymentMetrics)(nil)

// ModelDeploymentMetricGauge is one compatible sum of current Pod gauges.
type ModelDeploymentMetricGauge struct {
	Name       string    `json:"name" protobuf:"bytes,1,opt,name=name"`
	Source     string    `json:"source" protobuf:"bytes,2,opt,name=source"`
	Scope      string    `json:"scope" protobuf:"bytes,3,opt,name=scope"`
	Unit       string    `json:"unit" protobuf:"bytes,4,opt,name=unit"`
	Value      float64   `json:"value" protobuf:"fixed64,5,opt,name=value"`
	PodCount   int32     `json:"podCount" protobuf:"varint,6,opt,name=podCount"`
	ObservedAt meta.Time `json:"observedAt" protobuf:"bytes,7,opt,name=observedAt"`
}

// ModelDeploymentMetricWindow is one Pod's rate or histogram mean over two scrapes.
// Samples is the number of new events or histogram observations in that window.
type ModelDeploymentMetricWindow struct {
	Name          string    `json:"name" protobuf:"bytes,1,opt,name=name"`
	Pod           string    `json:"pod" protobuf:"bytes,2,opt,name=pod"`
	Source        string    `json:"source" protobuf:"bytes,3,opt,name=source"`
	Scope         string    `json:"scope" protobuf:"bytes,4,opt,name=scope"`
	Unit          string    `json:"unit" protobuf:"bytes,5,opt,name=unit"`
	Value         float64   `json:"value" protobuf:"fixed64,6,opt,name=value"`
	Samples       float64   `json:"samples" protobuf:"fixed64,7,opt,name=samples"`
	WindowSeconds float64   `json:"windowSeconds" protobuf:"fixed64,8,opt,name=windowSeconds"`
	ObservedAt    meta.Time `json:"observedAt" protobuf:"bytes,9,opt,name=observedAt"`
}

// ModelDeploymentCacheHit is a token hit ratio over two reads of the same Pod counters.
// Its source and scope prevent local prefix hits from being mixed with external-store hits.
type ModelDeploymentCacheHit struct {
	Pod           string    `json:"pod" protobuf:"bytes,1,opt,name=pod"`
	Source        string    `json:"source" protobuf:"bytes,2,opt,name=source"`
	Scope         string    `json:"scope" protobuf:"bytes,3,opt,name=scope"`
	Unit          string    `json:"unit" protobuf:"bytes,4,opt,name=unit"`
	Hits          float64   `json:"hits" protobuf:"fixed64,5,opt,name=hits"`
	Queries       float64   `json:"queries" protobuf:"fixed64,6,opt,name=queries"`
	Rate          float64   `json:"rate" protobuf:"fixed64,7,opt,name=rate"`
	WindowSeconds float64   `json:"windowSeconds" protobuf:"fixed64,8,opt,name=windowSeconds"`
	PodCount      int32     `json:"podCount" protobuf:"varint,9,opt,name=podCount"`
	ObservedAt    meta.Time `json:"observedAt" protobuf:"bytes,10,opt,name=observedAt"`
}

// ModelDeploymentMetricMissing names one source that could not contribute to the snapshot.
type ModelDeploymentMetricMissing struct {
	Pod    string `json:"pod" protobuf:"bytes,1,opt,name=pod"`
	Source string `json:"source" protobuf:"bytes,2,opt,name=source"`
	Reason string `json:"reason" protobuf:"bytes,3,opt,name=reason"`
}
