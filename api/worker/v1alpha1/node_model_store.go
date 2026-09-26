package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// NodeModelStore is the schema for worker.gpustack.ai.
//
// It is ONE NODE'S MODEL CACHE, named after the Node and owned by it, and it carries both halves of
// the node's contract the way a Node does: the worker writes spec, the effective configuration it
// computes from the Settings, and the model-manager plugin on that node writes status, what the
// node holds. status.models reports content the way Node.status.images reports images.
//
// NOTHING IN IT NAMES A TENANT. It is cluster-scoped, so a namespace, an artifact, a repository or a
// Pod written here would leak across tenants; an entry is a digest and its sizes only.
//
// The worker creates it once the node's CSINode lists the plugin's driver, which is kubelet's own
// record that the plugin registered there, and never deletes it when the driver briefly leaves
// CSINode, as a restart or a rolling upgrade makes it do. Only the plugin Pod running on the named
// node may write status, webhook-enforced.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["nms"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Ready",type="string",jsonPath=".status.conditions[?(@.type=='Ready')].status"
// +k8s:crd-gen:printcolumn:name="Used",type="integer",jsonPath=".status.capacity.usedPercent"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type NodeModelStore struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   NodeModelStoreSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status NodeModelStoreStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*NodeModelStore)(nil)

// NodeModelStoreSpec is the node's effective configuration, the worker's merge of the Settings.
//
// No tenant-writable object feeds it: a tenant who could choose where a privileged node process
// connects could make it request any address. The plugin checks it again before using it, since a
// hand edit bypasses the worker's checks.
type NodeModelStoreSpec struct {
	// Watermarks bound the cache filesystem's usage.
	//
	// +required
	Watermarks NodeModelStoreWatermarks `json:"watermarks" protobuf:"bytes,1,name=watermarks"`

	// Download limits the node's downloads.
	//
	// +required
	Download NodeModelStoreDownload `json:"download" protobuf:"bytes,2,name=download"`

	// Hub is how the node reaches the model hub.
	//
	// +required
	Hub NodeModelStoreHub `json:"hub" protobuf:"bytes,3,name=hub"`

	// Kubelet is the node's effective kubelet thresholds the cache's cap derives from, as the worker
	// read them from kubelet's configz endpoint, which merges kubelet's flags, configuration file and
	// drop-ins. It is absent while they could not be read, and the plugin then assumes kubelet's
	// defaults and says so.
	//
	// The worker writes it; nothing else does. It is an observed value held in the spec, not the
	// status, because the plugin reads its whole configuration from its spec and owns the status,
	// and reading configz needs the nodes/proxy permission, which the plugin never holds: it
	// reaches every kubelet endpoint. Nothing reconciles toward it as a desired state: the worker
	// refreshes the reading and writes what it read, so a hand edit holds only until the next
	// reading.
	//
	// +optional
	Kubelet *NodeModelStoreKubelet `json:"kubelet,omitempty" protobuf:"bytes,4,opt,name=kubelet"`
}

// NodeModelStoreKubelet is the part of a node's effective kubelet configuration that bounds a cache
// sharing kubelet's filesystem: kubelet evicts Pods below the available thresholds and collects
// images above the image threshold.
type NodeModelStoreKubelet struct {
	// NodefsAvailable is evictionHard["nodefs.available"] as kubelet reports it, a percentage such
	// as "10%" or a quantity such as "20Gi"; empty when kubelet sets none.
	//
	// +optional
	NodefsAvailable string `json:"nodefsAvailable,omitempty" protobuf:"bytes,1,opt,name=nodefsAvailable"`

	// ImagefsAvailable is evictionHard["imagefs.available"], in the same forms; empty when kubelet
	// sets none.
	//
	// +optional
	ImagefsAvailable string `json:"imagefsAvailable,omitempty" protobuf:"bytes,2,opt,name=imagefsAvailable"`

	// ImageGCHighThresholdPercent is the disk usage at which kubelet starts collecting images.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=100
	ImageGCHighThresholdPercent *int32 `json:"imageGCHighThresholdPercent,omitempty" protobuf:"varint,3,opt,name=imageGCHighThresholdPercent"` // nolint: lll
}

// NodeModelStoreWatermarks are percentages of the cache filesystem's usage by everything on it.
type NodeModelStoreWatermarks struct {
	// HighPercent is the usage above which the plugin removes unreferenced content. On a filesystem
	// the cache shares with kubelet, the plugin may apply a lower one so kubelet neither evicts Pods
	// nor collects images because of the cache.
	//
	// +required
	// +k8s:validation:minimum=2
	// +k8s:validation:maximum=95
	HighPercent int32 `json:"highPercent" protobuf:"varint,1,name=highPercent"`

	// LowPercent is the usage a collection removes down to, below HighPercent.
	//
	// +required
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=94
	LowPercent int32 `json:"lowPercent" protobuf:"varint,2,name=lowPercent"`
}

// NodeModelStoreDownload limits the node's downloads, across all of them.
type NodeModelStoreDownload struct {
	// Concurrency is how many HTTP requests the node runs at once. A large file is fetched as byte
	// ranges that share this budget, because one connection per file was measured to set a whole
	// download's wall time.
	//
	// +required
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=64
	Concurrency int32 `json:"concurrency" protobuf:"varint,1,name=concurrency"`

	// BytesPerSecond is the node's download rate limit; 0 is unlimited.
	//
	// +optional
	// +k8s:validation:minimum=0
	BytesPerSecond int64 `json:"bytesPerSecond,omitempty" protobuf:"varint,2,opt,name=bytesPerSecond"`
}

// NodeModelStoreHub is how the node reaches the model hub, the same way the ModelArtifact
// controller and an engine download reach it.
type NodeModelStoreHub struct {
	// HuggingFaceEndpoint is the Hugging Face Hub's base URL.
	//
	// +required
	// +k8s:validation:minLength=1
	HuggingFaceEndpoint string `json:"huggingFaceEndpoint" protobuf:"bytes,1,name=huggingFaceEndpoint"`

	// HTTPSProxy is the proxy downloads go through, an http or https URL without credentials.
	//
	// +optional
	HTTPSProxy string `json:"httpsProxy,omitempty" protobuf:"bytes,2,opt,name=httpsProxy"`

	// NoProxy is the comma-separated host list that bypasses HTTPSProxy.
	//
	// +optional
	NoProxy string `json:"noProxy,omitempty" protobuf:"bytes,3,opt,name=noProxy"`

	// CABundleConfigMap names a ConfigMap in the operator's namespace whose "ca.crt" holds PEM
	// certificates trusted beside the system pool. The name travels rather than the certificates,
	// because a bundle can be larger than this object may be.
	//
	// +optional
	CABundleConfigMap string `json:"caBundleConfigMap,omitempty" protobuf:"bytes,4,opt,name=caBundleConfigMap"`
}

// NodeModelStoreStatus is what the plugin reports about its node, rebuilt from the node's disk and
// mounts whenever the plugin starts. It is written only when something in it changes, so a node
// whose content is not changing writes nothing.
type NodeModelStoreStatus struct {
	// ObservedGeneration is the spec generation the plugin applies.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,1,opt,name=observedGeneration"`

	// Capacity is the cache filesystem's size and usage.
	//
	// +optional
	Capacity *NodeModelStoreCapacity `json:"capacity,omitempty" protobuf:"bytes,2,opt,name=capacity"`

	// Models is the content on the node, keyed by digest. When more is on disk than fits, the
	// referenced entries are kept first and then the most recently used, and the Ready condition's
	// message counts the omission.
	//
	// +optional
	// +listType=map
	// +listMapKey=digest
	// +k8s:validation:maxItems=256
	Models []NodeModelStoreModel `json:"models,omitempty" protobuf:"bytes,3,rep,name=models"`

	// Conditions: Ready says the plugin serves and applies spec's generation; CapacityLow says usage
	// is above the high watermark with nothing the plugin may remove.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,4,rep,name=conditions"` // nolint: lll
}

// NodeModelStoreCapacity is the cache filesystem's size and usage, coarse enough that it changes
// only when content does.
type NodeModelStoreCapacity struct {
	// TotalBytes is the cache filesystem's size.
	//
	// +required
	TotalBytes int64 `json:"totalBytes" protobuf:"varint,1,name=totalBytes"`

	// StoredBytes is what published trees and partial downloads occupy.
	//
	// +required
	StoredBytes int64 `json:"storedBytes" protobuf:"varint,2,name=storedBytes"`

	// UsedPercent is the filesystem's usage by everything on it, rounded down to a multiple of 5.
	//
	// +required
	UsedPercent int32 `json:"usedPercent" protobuf:"varint,3,name=usedPercent"`
}

// NodeModelStoreModel is one digest on the node.
type NodeModelStoreModel struct {
	// Digest is the manifest digest, the content's address.
	//
	// +required
	Digest string `json:"digest" protobuf:"bytes,1,name=digest"`

	// State is Downloading, Ready or Failed.
	//
	// +required
	// +k8s:validation:enum=["Downloading","Ready","Failed"]
	State NodeModelStoreModelState `json:"state" protobuf:"bytes,2,name=state,casttype=NodeModelStoreModelState"`

	// SizeBytes is the content's size.
	//
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty" protobuf:"varint,3,opt,name=sizeBytes"`

	// DownloadedBytes is how much of a Downloading entry's content is on the node's disk, the bytes
	// an earlier attempt left for a resume included, so it never goes back. It is written on
	// thresholds, not at every byte: once the entry moved by 5% of SizeBytes and 30 seconds passed
	// since the node's status was last written. Absent outside Downloading.
	//
	// +optional
	// +k8s:validation:minimum=0
	DownloadedBytes int64 `json:"downloadedBytes,omitempty" protobuf:"varint,9,opt,name=downloadedBytes"`

	// Source is where the content's bytes come from: Hub, the model hub. It names no repository or
	// endpoint. Content published before the field existed has none.
	//
	// +optional
	// +k8s:validation:enum=["Hub"]
	Source NodeModelStoreModelSource `json:"source,omitempty" protobuf:"bytes,10,opt,name=source,casttype=NodeModelStoreModelSource"` // nolint: lll

	// Referenced says some Pod mounts the content. Which Pod is never recorded.
	//
	// +optional
	Referenced bool `json:"referenced,omitempty" protobuf:"varint,4,opt,name=referenced"`

	// LastUsedTime is when the content was last mounted or unmounted, truncated to the hour so that
	// use does not rewrite the object.
	//
	// +optional
	LastUsedTime *meta.Time `json:"lastUsedTime,omitempty" protobuf:"bytes,5,opt,name=lastUsedTime"`

	// Reason classifies a Failed entry: InvalidRequest, AccessDenied, SourceUnavailable,
	// IntegrityMismatch, InsufficientCapacity or Canceled.
	//
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,6,opt,name=reason"`

	// Message says why for people, in words that name no tenant: what the reason means and a detail
	// such as the hub's HTTP status. The full error, with the file and the URL, is in the plugin's
	// log. Nothing parses it, and it never carries a credential.
	//
	// +optional
	Message string `json:"message,omitempty" protobuf:"bytes,7,opt,name=message"`

	// RetryTime is the earliest next attempt of a Failed entry. Until then a mount of the digest
	// returns at once without downloading, because kubelet retries every failed mount and each
	// retry would otherwise download everything again.
	//
	// +optional
	RetryTime *meta.Time `json:"retryTime,omitempty" protobuf:"bytes,8,opt,name=retryTime"`
}

// NodeModelStoreModelSource is where a node's content comes from.
// +enum
type NodeModelStoreModelSource string

// NodeModelStoreModelSourceHub is a download from the model hub.
const NodeModelStoreModelSourceHub NodeModelStoreModelSource = "Hub"

// NodeModelStoreModelState is where a digest is on a node.
// +enum
type NodeModelStoreModelState string

const (
	// NodeModelStoreModelStateDownloading is being materialized.
	NodeModelStoreModelStateDownloading NodeModelStoreModelState = "Downloading"
	// NodeModelStoreModelStateReady is published and mountable.
	NodeModelStoreModelStateReady NodeModelStoreModelState = "Ready"
	// NodeModelStoreModelStateFailed failed its last attempt and waits for RetryTime.
	NodeModelStoreModelStateFailed NodeModelStoreModelState = "Failed"
)

// NodeModelStoreList holds the list of NodeModelStore.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NodeModelStoreList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []NodeModelStore `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*NodeModelStoreList)(nil)
