package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// ModelStore is the schema for worker.gpustack.ai.
//
// It is ONE NODE POOL'S CACHE POLICY, the admin's per-pool layer above the cluster defaults: the
// watermarks and download limits of every node its selector matches, overriding only the fields it
// sets. Nodes no store matches keep the Settings' cluster defaults, so this object never has to
// restate them.
//
// The cache's root path is deliberately absent: it is the plugin DaemonSet's hostPath, decided at
// deployment, and a per-pool path would need a per-pool DaemonSet rather than a field here.
//
// TWO STORES MUST NOT MATCH ONE NODE. The worker refuses to guess: it picks the alphabetically
// first name deterministically, records that choice in the node's NodeModelStore.spec.store, and
// sets SelectorOverlap on both stores so the misconfiguration is visible where it was made.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["ms"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Nodes",type="integer",jsonPath=".status.nodes"
// +k8s:crd-gen:printcolumn:name="High",type="integer",jsonPath=".spec.watermarks.highPercent"
// +k8s:crd-gen:printcolumn:name="Low",type="integer",jsonPath=".spec.watermarks.lowPercent"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type ModelStore struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelStoreSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelStoreStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelStore)(nil)

// ModelStoreSpec is the pool's cache policy. Every field but the selector is optional: a nil field
// leaves the cluster default in place on the matched nodes.
type ModelStoreSpec struct {
	// NodeSelector selects the pool's nodes. Two stores whose selectors match one node are a
	// misconfiguration this API reports rather than resolves silently; see SelectorOverlap.
	//
	// +required
	NodeSelector meta.LabelSelector `json:"nodeSelector" protobuf:"bytes,1,name=nodeSelector"`

	// Watermarks bounds the matched nodes' cache filesystem usage. Nil keeps the cluster defaults.
	//
	// +optional
	Watermarks *NodeModelStoreWatermarks `json:"watermarks,omitempty" protobuf:"bytes,2,opt,name=watermarks"`

	// Download limits the matched nodes' downloads. Nil keeps the cluster defaults.
	//
	// +optional
	Download *ModelStoreDownload `json:"download,omitempty" protobuf:"bytes,3,opt,name=download"`
}

// ModelStoreDownload is the pool's download policy, stated field by field: a field left out
// carries no opinion and keeps the cluster default, so a pool may raise the concurrency without
// also restating the bandwidth. Explicitly setting a field is a real override — bytesPerSecond 0
// is "unlimited on this pool", not "unspecified".
type ModelStoreDownload struct {
	// Concurrency is how many HTTP requests the matched nodes run at once; left out, the cluster
	// default applies.
	//
	// +optional
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=64
	Concurrency *int32 `json:"concurrency,omitempty" protobuf:"varint,1,opt,name=concurrency"`

	// BytesPerSecond is the matched nodes' download rate limit; 0 is unlimited. Left out, the
	// cluster default applies.
	//
	// +optional
	// +k8s:validation:minimum=0
	BytesPerSecond *int64 `json:"bytesPerSecond,omitempty" protobuf:"varint,2,opt,name=bytesPerSecond"`
}

// ModelStoreStatus is what the worker observes about the pool: how many nodes it matches, what
// those nodes' caches hold in total, and whether the selector overlaps another store's.
//
// Conditions: Ready says the store's policy is applied on every matched node; SelectorOverlap says
// another store's selector also matches at least one of them.
type ModelStoreStatus struct {
	// Nodes is the number of nodes the selector currently matches.
	//
	// +optional
	Nodes int32 `json:"nodes,omitempty" protobuf:"varint,1,opt,name=nodes"`

	// Capacity sums the matched nodes' NodeModelStore capacity readings. It is absent while no
	// matched node has reported one.
	//
	// +optional
	Capacity *ModelStoreCapacity `json:"capacity,omitempty" protobuf:"bytes,2,opt,name=capacity"`

	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"` // nolint: lll
}

// ModelStoreCapacity is the pool's cache footprint, summed over the matched nodes.
type ModelStoreCapacity struct {
	// TotalBytes is the summed size of the matched nodes' cache filesystems.
	//
	// +required
	TotalBytes int64 `json:"totalBytes" protobuf:"varint,1,name=totalBytes"`

	// StoredBytes is what the matched nodes' published trees and partial downloads occupy.
	//
	// +required
	StoredBytes int64 `json:"storedBytes" protobuf:"varint,2,name=storedBytes"`
}

// ModelStoreList holds the list of ModelStore.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelStoreList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelStore `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelStoreList)(nil)
