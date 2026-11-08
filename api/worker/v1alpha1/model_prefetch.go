package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
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

	// Source opts this prefetch into source preparation for remote weight loading, beside the
	// disk residency it always asked for. With it, the target nodes may fill a registered-memory
	// source from this artifact, and the prefetch reports that readiness in status.transfer,
	// separately from the disk figures above.
	//
	// IT REQUIRES AN EXPLICIT PLACEMENT. A source is memory a node holds against a known target
	// set, so the derived-from-deployments default an ordinary prefetch falls back to — which
	// follows this namespace's consumers around — would prepare whatever machines happen to
	// serve the model this week. A source intent on a prefetch whose placement names no
	// instanceTypes and no nodeSelector is refused at admission, and the two out together stay
	// refused for the ambiguity they already were.
	//
	// The optional Binding sourceMemory grant bounds memory independently of disk quota.
	// Without it, source preparation uses the node's configured source capacity.
	//
	// Absent means a prefetch that warms the node cache only, exactly as this object has always
	// meant, with the same placement defaults, warm-up behavior and status meaning as before.
	//
	// +optional
	Source *ModelPrefetchSource `json:"source,omitempty" protobuf:"bytes,6,opt,name=source"`
}

// ModelPrefetchSource is the source-preparation intent, and its presence is the opt-in. It
// carries no settings of its own: the target set is the placement's, the memory it may hold is
// bounded by node capacity and the optional Binding grant. Readiness is reported in status.transfer.
// An object an older client wrote has no source and keeps its meaning unchanged.
type ModelPrefetchSource struct{}

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

	// Transfer reports the source preparation spec.source asks for, aggregated from the target
	// nodes' reports. It is ABSENT without spec.source — the intent is what turns it on — and
	// absent while nothing has been observed, rather than present and empty, for the same reason
	// the disk figures above are zero rather than missing only when measured.
	//
	// It is deliberately separate from the disk figures above, and neither implies the other:
	// readyNodes counts complete published trees, which survive a daemon restart, while the
	// transfer entries are scoped to a generation held in memory, and a generation that stops
	// being served does not un-publish the tree beneath it.
	//
	// +optional
	Transfer *ModelPrefetchTransferStatus `json:"transfer,omitempty" protobuf:"bytes,5,opt,name=transfer"`
}

// ModelPrefetchTransferStatus is the transfer half of a source-preparing prefetch, aggregated
// from the target nodes' reports for this artifact's digest. The bytes are observed figures the
// nodes reported as exact integer bytes; quantity comparisons use resource.Quantity.Value().
// The limits echo the admitted grant policy.
type ModelPrefetchTransferStatus struct {
	// ReservedBytes is the memory this prefetch's admitted generations hold reserved against the
	// namespace's grant. It is charged to this namespace even when the physical bytes are shared
	// with another namespace's reservation, so it can exceed the residency any one node reports.
	//
	// +optional
	ReservedBytes int64 `json:"reservedBytes,omitempty" protobuf:"varint,1,opt,name=reservedBytes"`

	// Limits echoes the policy the source applies to this namespace's read sessions: the grant's
	// limits as they were admitted, which a later edit of the Binding cannot reach back and
	// change. Absent until a generation is admitted.
	//
	// +optional
	Limits *ModelPrefetchTransferLimits `json:"limits,omitempty" protobuf:"bytes,2,opt,name=limits"`

	// Generations summarizes at most 16 source generations, with serving entries first
	// and generation identity order within each group. The SourceSummaryTruncated condition
	// reports omitted entries. Accounting and source readiness use the complete observation.
	//
	// +optional
	// +listType=map
	// +listMapKey=generation
	// +k8s:validation:maxItems=16
	Generations []ModelPrefetchTransferGeneration `json:"generations,omitempty" protobuf:"bytes,3,rep,name=generations"` // nolint: lll

	// Serving binds SourceReady to the current artifact revision and the earliest
	// lease expiry among its serving generations. It is an observation, not an
	// authorization; absent or expired proof grants no source readiness.
	//
	// +optional
	Serving *ModelPrefetchTransferServing `json:"serving,omitempty" protobuf:"bytes,4,opt,name=serving"`
}

// ModelPrefetchTransferServing identifies the source readiness a prefetch observed.
type ModelPrefetchTransferServing struct {
	// ArtifactUID identifies the artifact whose source was observed.
	//
	// +required
	// +k8s:validation:minLength=1
	ArtifactUID string `json:"artifactUID" protobuf:"bytes,1,name=artifactUID"`

	// ArtifactGeneration is the artifact revision the source serves.
	//
	// +required
	// +k8s:validation:minimum=1
	ArtifactGeneration int64 `json:"artifactGeneration" protobuf:"varint,2,name=artifactGeneration"`

	// Digest is the resolved content identity the source serves.
	//
	// +required
	// +k8s:validation:minLength=1
	Digest string `json:"digest" protobuf:"bytes,3,name=digest"`

	// ReadyLeaseExpiresAt bounds how long this readiness observation is usable.
	//
	// +required
	ReadyLeaseExpiresAt meta.Time `json:"readyLeaseExpiresAt" protobuf:"bytes,4,name=readyLeaseExpiresAt"`
}

// ModelPrefetchTransferLimits is the applied source policy, echoed where it was admitted so a
// reader can see what is enforced without reading the grant and guessing what admission made of
// it.
type ModelPrefetchTransferLimits struct {
	// Bytes is the registered-memory ceiling the namespace's admitted reservations draw on.
	//
	// +required
	Bytes resource.Quantity `json:"bytes" protobuf:"bytes,1,name=bytes"`

	// Registrations is the live registration count the source allows the namespace.
	//
	// +required
	// +k8s:validation:minimum=1
	Registrations int32 `json:"registrations" protobuf:"varint,2,opt,name=registrations"`

	// Sessions is the concurrent admitted read sessions the source allows the namespace.
	//
	// +required
	// +k8s:validation:minimum=1
	Sessions int32 `json:"sessions" protobuf:"varint,3,opt,name=sessions"`
}

// ModelPrefetchTransferGeneration is one source generation's standing across the target nodes.
type ModelPrefetchTransferGeneration struct {
	// Generation is the source's identity for this registered content: an opaque token assigned
	// when the fill has verified, and the only thing the figures beside it are true of. A drain,
	// a refill or a daemon restart supersedes it; a new generation never inherits the old one's
	// figures. It names no tenant and carries no key.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=128
	Generation string `json:"generation" protobuf:"bytes,1,opt,name=generation"`

	// ReadyNodes is how many target nodes hold this generation registered and admit readers
	// against it. It is NOT readyNodes, and neither implies the other: a node whose daemon
	// restarted stops counting here while the published tree beneath it stays disk-ready.
	//
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty" protobuf:"varint,2,opt,name=readyNodes"`

	// DrainingNodes is how many target nodes hold this generation past admission — they have
	// stopped admitting new sessions and are fencing their in-flight work, or their completion
	// could not be proven and they hold the registration until a qualified fence applies. Their
	// memory is still held.
	//
	// +optional
	DrainingNodes int32 `json:"drainingNodes,omitempty" protobuf:"varint,3,opt,name=drainingNodes"`

	// ResidentBytes sums what the target nodes hold registered for this generation, serving and
	// draining alike, because a draining generation's memory is still held. The bytes are
	// physical, so a copy two namespaces share counts once here and once in each namespace's
	// reservation.
	//
	// +optional
	ResidentBytes int64 `json:"residentBytes,omitempty" protobuf:"varint,4,opt,name=residentBytes"`

	// Registrations is the live registration count the nodes hold for this generation.
	//
	// +optional
	Registrations int32 `json:"registrations,omitempty" protobuf:"varint,5,opt,name=registrations"`

	// ActiveSessions is how many read sessions the nodes have admitted against this generation.
	//
	// +optional
	ActiveSessions int32 `json:"activeSessions,omitempty" protobuf:"varint,6,opt,name=activeSessions"`

	// RejectedAdmissions counts the admission refusals the nodes have reported since this
	// generation started — over the memory grant, out of registrations, past the session limit —
	// so a capacity shortfall is visible where it was made rather than only as a replica that
	// never starts.
	//
	// +optional
	RejectedAdmissions int64 `json:"rejectedAdmissions,omitempty" protobuf:"varint,7,opt,name=rejectedAdmissions"` // nolint: lll
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
