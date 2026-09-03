package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// KVCachePool is the schema for worker.gpustack.ai.
//
// A KVCacheBackend declares the physical cache; this declares which namespaces are granted a quota on
// it, how much of it, and under which reuse identity. It is the quota domain over exactly one
// backend, and the registry of the reuse domains its Bindings have claimed.
//
// It grants and accounts; it does not admit. Nothing here keeps a pod that knows a reuse domain's
// name from reaching that domain on the store — see KVCachePoolBinding for what the grant is and
// what it is not.
//
// It is cluster-scoped, for the reason the backend it references is: the backend is a privileged
// physical resource only an admin declares, pools must be shareable across namespaces, and a
// cross-namespace reference FROM a namespaced object is an anti-pattern. Data-plane isolation is a
// different axis, and the storage layer's tenant — not this object's scope — is what solves it.
// This is Kueue's ClusterQueue to KVCachePoolBinding's LocalQueue.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["kvcp"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Quota",type="string",jsonPath=".spec.quota.total"
// +k8s:crd-gen:printcolumn:name="Usage",type="string",jsonPath=".status.usage.total"
// +k8s:crd-gen:printcolumn:name="Phase",type="string",jsonPath=".status.phase"
// +k8s:crd-gen:printcolumn:name="Endpoint",type="string",jsonPath=".status.clientEndpoint"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type KVCachePool struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   KVCachePoolSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status KVCachePoolStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*KVCachePool)(nil)

// KVCachePoolSpec defines the desired spec of KVCachePool.
type KVCachePoolSpec struct {
	// Backends names the KVCacheBackend this pool draws from. It holds NAMES rather than a typed
	// reference so this package needs no compile-time dependency on that type.
	//
	// Exactly one entry is admitted, and the rule is the webhook's rather than the schema's so the
	// refusal can carry its reason: quota lands on a single master's per-tenant ledger, and one
	// master cannot account for bytes held in another backend. A schema bound would refuse the same
	// object with a message that explains nothing.
	//
	// It is immutable, webhook-enforced. Moving a pool to another backend would strand every tenant
	// quota on the old master's ledger with nothing left to delete them with.
	//
	// The reverse is NOT exclusive: one backend may be referenced by several pools, which is why the
	// ledger and the rendered policy converge per MASTER rather than per pool.
	//
	// +required
	// +listType=atomic
	Backends []string `json:"backends" protobuf:"bytes,1,rep,name=backends"`

	// Quota is the ceiling this pool declares over its backend.
	//
	// +required
	Quota KVCachePoolQuota `json:"quota" protobuf:"bytes,2,name=quota"`
}

// KVCachePoolQuota is the pool's declared ceiling.
//
// It is OUR number, not the master's. The master reports its own allocatable capacity and the two
// can disagree; the case where the disagreement is total — a ceiling declared over a backend that
// has mounted nothing — is a Condition rather than a silence.
type KVCachePoolQuota struct {
	// Total is required, which is why it is held by value: a pool with no declared ceiling has
	// nothing to write into any ledger.
	//
	// +required
	Total resource.Quantity `json:"total" protobuf:"bytes,1,name=total"`
}

// KVCachePoolStatus defines the observed state of KVCachePool.
type KVCachePoolStatus struct {
	// Phase summarizes the conditions: Provisioning, Ready, Degraded, Error, Deleting.
	Phase string `json:"phase,omitempty" protobuf:"bytes,1,opt,name=phase"`

	// PhaseMessage carries the reason for the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Conditions is the finer view, one condition per axis.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"` // nolint: lll

	// ClientEndpoint is the address an inference engine connects to, echoed from the backend's
	// Client endpoint.
	//
	// The backend's ADMIN address is deliberately republished NOWHERE. That one port serves the
	// Prometheus exposition and the HTTP admin API both, so it is the write face of the quota
	// ledger, while this object is cluster-scoped and readable by anyone holding a pool RBAC rule.
	// The operator dials it; nobody reads it here.
	//
	// It is absent, with a Condition saying why, whenever the backend has published no endpoints
	// yet. It is never filled from a Service name derived from the backend's own — a guessed
	// address that happens to resolve is how a pool would silently drive the wrong master.
	//
	// +k8s:validation:maxLength=259
	ClientEndpoint string `json:"clientEndpoint,omitempty" protobuf:"bytes,4,opt,name=clientEndpoint"`

	// Usage is what this pool's own tenants are holding. It is ABSENT until a scrape succeeds, and
	// absent is not the same as reporting zero.
	Usage *KVCachePoolUsage `json:"usage,omitempty" protobuf:"bytes,5,opt,name=usage"`

	// Domains is the registry of reuse identities claimed against this pool, one entry per Binding.
	//
	// It is AUTHORITATIVE rather than advisory: an entry exists because a Binding declares it, not
	// because a workload announced itself, so assembling it is one pass over the Binding index and
	// needs no watch on any workload kind.
	//
	// +listType=map
	// +listMapKey=name
	Domains []KVCachePoolDomain `json:"domains,omitempty" protobuf:"bytes,6,rep,name=domains"`

	// UsedBy names the Bindings that hold this pool. A non-empty UsedBy is what the finalizer
	// refuses deletion on, so the field is the enforcement input and not a display.
	//
	// Unlike the Binding's own list, THIS one is written by this API's reconciler, which already
	// lists the Bindings of a pool through its index. The two levels of usedBy therefore have
	// different writers, and only this one is self-contained.
	//
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	UsedBy []KVCacheObjectReference `json:"usedBy,omitempty" protobuf:"bytes,7,rep,name=usedBy"`
}

// KVCacheObjectReference names an object that consumes another one in this group. It is the ONE
// shape every usedBy in this family carries, so a reader learns it once.
//
// It names NO API GROUP, and that is a constraint rather than an omission: a usedBy entry may only
// name a kind in this API group. Within one group, kind and name identify an object, so the group
// would be a constant on every entry. The constraint is what makes that safe, and it is the reason
// the list can be keyed at all — core.TypedLocalObjectReference carries an optional, defaultless
// apiGroup, which a structural schema refuses as a list map key, so keying on kind and name alone
// would silently merge two objects that differ only by group.
//
// All three fields are REQUIRED, and all three are list map keys — a structural schema accepts a key
// only where it is required or defaulted, and two consumers collapsing into one entry would let a
// finalizer release while a consumer still holds. Namespace is required but carries the EMPTY STRING
// when the referent is in the holder's own scope: a cluster-scoped object naming another, or a
// namespaced object naming something in its own namespace. Empty is a value here, not an absence.
type KVCacheObjectReference struct {
	// +required
	Kind string `json:"kind" protobuf:"bytes,1,name=kind"`

	// +required
	Namespace string `json:"namespace" protobuf:"bytes,2,name=namespace"`

	// +required
	Name string `json:"name" protobuf:"bytes,3,name=name"`
}

// KVCachePoolUsage is what the pool's tenants hold, as the master reports it.
type KVCachePoolUsage struct {
	// Total is the sum of the occupancy the master reports for the tenants THIS pool owns — never its
	// whole ledger, which a shared backend makes larger than this pool.
	//
	// WHAT occupancy means is the master's choice, not this API's, and it changed: a master exposing
	// used bytes apart from reservations is summed as committed bytes, while one exposing a single
	// charged figure — the shape 0.3.13 introduced — charges at the start of a write, so in-flight
	// reservations are already inside this total. Do not read it as committed usage without knowing
	// which the backend runs.
	//
	// A POINTER because omitempty does not omit a zero-valued struct, and an unobserved total must
	// not serialize as a cache that is empty.
	Total *resource.Quantity `json:"total,omitempty" protobuf:"bytes,1,opt,name=total"`
}

// KVCachePoolDomain is one reuse identity registered against this pool: what it is, who declared it,
// and what the master reports about it.
type KVCachePoolDomain struct {
	// Name is the reuse identity, and it is the storage layer's tenant_id.
	//
	// +required
	// +k8s:validation:maxLength=63
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Binding is the object that declared this domain. It is the only place a domain can be
	// declared, which is what keeps naming one a privileged act.
	//
	// +required
	Binding KVCachePoolBindingReference `json:"binding" protobuf:"bytes,2,name=binding"`

	// BlockSize and Dtype are echoed from the Binding that registered the domain, so a reader of
	// the registry does not have to fetch every Binding to learn what a domain's blocks are.
	//
	// Both are REQUIRED, unlike the observed figures below, and the difference is where they come
	// from: these are copied from a Binding that already requires them, so an entry missing one
	// could only be a writer bug. Leaving them optional would let the registry answer "this domain's
	// blocks are of unknown shape", which is not a state that exists.
	//
	// Dtype is spelled to match its JSON name exactly. DType would not, and the openapi generator
	// records every such mismatch as a checked-in API rule violation — a list worth keeping for the
	// names that genuinely cannot match.
	//
	// +required
	BlockSize int32 `json:"blockSize" protobuf:"varint,3,name=blockSize"`

	// +required
	Dtype string `json:"dtype" protobuf:"bytes,4,name=dtype"`

	// Blocks and HitRate are OBSERVED, never declared, and they are ABSENT when the scrape does not
	// carry this domain. A fabricated zero hit rate on a warm cache is worse than no number, and
	// zero blocks is a different fact from "not in the scrape", which is why Blocks is a pointer.
	Blocks *int64 `json:"blocks,omitempty" protobuf:"varint,5,opt,name=blocks"`

	// HitRate is a ratio held as a STRING with a pattern, never a float, matching the shape the
	// measured surface itself uses.
	//
	// The pattern is safe here in a way an enum on an echoed vendor value would not be: this ratio is
	// COMPUTED by this operator, so its spelling is ours to guarantee. It does oblige whoever writes
	// it to format to this shape, because a value that fails the pattern fails the WHOLE status
	// write — every other field frozen at its last value — and not this one field.
	//
	// +k8s:validation:pattern="^(0(\\.[0-9]{1,4})?|1(\\.0{1,4})?)$"
	HitRate string `json:"hitRate,omitempty" protobuf:"bytes,6,opt,name=hitRate"`
}

// KVCachePoolBindingReference names one KVCachePoolBinding.
//
// It carries no kind, unlike KVCacheObjectReference, because it is not a usedBy: it is the back
// pointer from a registered domain to the one object that could have declared it, and only a
// KVCachePoolBinding ever can. Both fields are required, because a Binding is namespaced and the
// namespace is half of its identity.
type KVCachePoolBindingReference struct {
	// +required
	Namespace string `json:"namespace" protobuf:"bytes,1,name=namespace"`

	// +required
	Name string `json:"name" protobuf:"bytes,2,name=name"`
}

// KVCachePoolList holds the list of KVCachePool.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KVCachePoolList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KVCachePool `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*KVCachePoolList)(nil)
