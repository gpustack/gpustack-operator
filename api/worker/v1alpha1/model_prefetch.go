package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// ModelPrefetch is the schema for worker.gpustack.ai.
//
// It is a tenant's RESIDENCY INTENT for one model artifact: keep the artifact's weights on a set of
// nodes ahead of any Pod that mounts them. The worker delivers it the same way it delivers a cold
// mount — one warm-up Pod per target node, the artifact's own CSI volume, no accelerator request —
// so the bytes land through the node cache's ordinary path, once per node, verified, and Pod exits
// once the tree is readable. The Pod IS the delivery operation; there is no separate job state.
//
// Placement narrows the target set: the InstanceTypes of a node pool, or a nodeSelector for named
// nodes, or — when both are left out — the InstanceTypes of the namespace's own deployments that
// reference the artifact. Retention decides what happens after: whether the content is pinned
// against collection (an admin-granted capability) and how long a node keeps it after its last use.
//
// Deleting the object revokes the intent: the warm-up Pods go, the nodes' pins drop, and the bytes
// are reclaimed by the node cache's own collection once they are unreferenced and past its grace.
// Nothing is torn out from under a Pod still reading the tree.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mpf"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Artifact",type="string",jsonPath=".spec.artifactRef.name"
// +k8s:crd-gen:printcolumn:name="Binding",type="string",jsonPath=".spec.bindingRef.name"
// +k8s:crd-gen:printcolumn:name="Ready",type="integer",jsonPath=".status.readyNodes"
// +k8s:crd-gen:printcolumn:name="Desired",type="integer",jsonPath=".status.desiredNodes"
// +k8s:crd-gen:printcolumn:name="Pinned",type="boolean",jsonPath=".spec.retention.pinned"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type ModelPrefetch struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelPrefetchSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelPrefetchStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelPrefetch)(nil)

// ModelPrefetchSpec is the residency intent.
type ModelPrefetchSpec struct {
	// ArtifactRef names the ModelArtifact in this namespace whose weights warm the target set. It
	// must be resolved before any Pod is rendered: admission and delivery both read its digest.
	//
	// +required
	ArtifactRef ModelPrefetchArtifactReference `json:"artifactRef" protobuf:"bytes,1,name=artifactRef"`

	// BindingRef names the ModelStoreBinding in this namespace that pays for the bytes. It must
	// grant a store covering the target set, and its budget bounds the prefetch at admission.
	//
	// +required
	BindingRef ModelPrefetchBindingReference `json:"bindingRef" protobuf:"bytes,2,name=bindingRef"`

	// Placement narrows the target set. InstanceTypes and NodeSelector are mutually exclusive;
	// both set is refused, and both left out derives the target set from the namespace's own
	// deployments that reference the artifact. The relational rules are webhook-enforced.
	//
	// +optional
	Placement *ModelPrefetchPlacement `json:"placement,omitempty" protobuf:"bytes,3,opt,name=placement"`

	// MinReady is how many target nodes holding the content make the prefetch Available; 0 means
	// all of them.
	//
	// +optional
	// +k8s:validation:default=0
	// +k8s:validation:minimum=0
	MinReady int32 `json:"minReady,omitempty" protobuf:"varint,4,opt,name=minReady"`

	// Retention decides what happens to the content after it has landed.
	//
	// +optional
	Retention ModelPrefetchRetention `json:"retention,omitempty" protobuf:"bytes,5,opt,name=retention"`
}

// ModelPrefetchArtifactReference names an artifact in the prefetch's own namespace.
type ModelPrefetchArtifactReference struct {
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`
}

// ModelPrefetchBindingReference names a binding in the prefetch's own namespace.
type ModelPrefetchBindingReference struct {
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`
}

// ModelPrefetchPlacement narrows the target set of nodes the artifact warms.
type ModelPrefetchPlacement struct {
	// InstanceTypes names the node pools to warm, by the InstanceType objects the scheduling chain
	// already publishes. The target set is every node carrying one of these types' flavors.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=8
	InstanceTypes []string `json:"instanceTypes,omitempty" protobuf:"bytes,1,rep,name=instanceTypes"`

	// NodeSelector selects the target nodes directly, the "download to these nodes" form.
	//
	// +optional
	NodeSelector *meta.LabelSelector `json:"nodeSelector,omitempty" protobuf:"bytes,2,opt,name=nodeSelector"`
}

// ModelPrefetchRetention decides what happens to the content after it has landed.
type ModelPrefetchRetention struct {
	// Pinned keeps the content on every target node against the node cache's collection: a pinned
	// digest is never an eviction candidate, though it still counts toward usage. It requires the
	// Binding to allow pinning, and admission refuses it otherwise.
	//
	// +optional
	Pinned bool `json:"pinned,omitempty" protobuf:"varint,1,opt,name=pinned"`

	// TTLAfterLastUse unpins a node's copy once nothing mounted it for this long. It is enforced
	// at the hour granularity the node's report already carries: retention does not need finer
	// precision, and finer recording would multiply the node's writes for a decision that cannot
	// tell the difference.
	//
	// +optional
	TTLAfterLastUse *meta.Duration `json:"ttlAfterLastUse,omitempty" protobuf:"bytes,2,opt,name=ttlAfterLastUse"`
}

// ModelPrefetchStatus is the prefetch's own view of its delivery, aggregated from the target
// nodes' reports: which node holds the digest, which is fetching it, and whether enough do.
type ModelPrefetchStatus struct {
	// DesiredNodes is the size of the target set the current placement resolves to.
	//
	// +optional
	DesiredNodes int32 `json:"desiredNodes,omitempty" protobuf:"varint,1,opt,name=desiredNodes"`

	// ReadyNodes is how many target nodes report the digest published and mountable.
	//
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty" protobuf:"varint,2,opt,name=readyNodes"`

	// DownloadingNodes is how many target nodes are still fetching it.
	//
	// +optional
	DownloadingNodes int32 `json:"downloadingNodes,omitempty" protobuf:"varint,3,opt,name=downloadingNodes"`

	// Conditions: Progressing says delivery is under way; Available says ReadyNodes reached the
	// MinReady bar; Degraded says a target node gave up, and the message says why.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,4,rep,name=conditions"` // nolint: lll
}

// ModelPrefetchList holds the list of ModelPrefetch.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelPrefetchList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelPrefetch `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelPrefetchList)(nil)
