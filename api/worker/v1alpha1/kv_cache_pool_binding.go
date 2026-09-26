package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// KVCachePoolBinding is the schema for worker.gpustack.ai.
//
// It is the PROVISIONING POINT: creating one in a namespace is what gives that namespace a quota on
// a pool and registers the ONE reuse domain it will write under, so both are objects an admin can
// RBAC and audit rather than strings a tenant types into a workload.
//
// The storage layer's tenant IS the domain, so registering one creates a quota ledger, which is what
// makes naming a domain a privileged act rather than something a workload could mint at will.
// Workloads pointing at the SAME Binding share KV; a namespace needing two reuse boundaries creates
// two Bindings, exactly as one with two scheduling boundaries has two Kueue LocalQueues.
//
// It is NOT an enforcement boundary, and must not be described as one. The store accepts whatever
// tenant id a caller sends, over a Service any pod in the cluster can dial, and nothing derives a
// credential from this object — so a workload that knows another namespace's domain name can read
// and write that domain's cache today. What a Binding governs is who is GRANTED capacity and under
// which name. Enforcement needs an authenticated proxy or network isolation, and neither exists yet.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["kvcpb"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Pool",type="string",jsonPath=".spec.poolRef.name"
// +k8s:crd-gen:printcolumn:name="Domain",type="string",jsonPath=".spec.domain.name"
// +k8s:crd-gen:printcolumn:name="BlockSize",type="integer",jsonPath=".spec.domain.blockSize"
// +k8s:crd-gen:printcolumn:name="Dtype",type="string",jsonPath=".spec.domain.dtype"
// +k8s:crd-gen:printcolumn:name="Effective",type="string",jsonPath=".status.effectiveQuota"
// +k8s:crd-gen:printcolumn:name="Usage",type="string",jsonPath=".status.usage"
// +k8s:crd-gen:printcolumn:name="Phase",type="string",jsonPath=".status.phase"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type KVCachePoolBinding struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   KVCachePoolBindingSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status KVCachePoolBindingStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*KVCachePoolBinding)(nil)

// KVCachePoolBindingSpec defines the desired spec of KVCachePoolBinding.
type KVCachePoolBindingSpec struct {
	// PoolRef names the pool this namespace is being granted. It is IMMUTABLE, webhook-enforced:
	// re-pointing a Binding would move a namespace's grant silently and leave its bytes on
	// the old master.
	//
	// +required
	PoolRef KVCachePoolBindingPoolReference `json:"poolRef" protobuf:"bytes,1,name=poolRef"`

	// Domain is the reuse identity this Binding registers, and it is REQUIRED. It maps to the
	// storage layer's tenant_id (isolation) and cache_salt (prefix identity), so registering a
	// domain creates a tenant with a quota ledger of its own.
	//
	// EXACTLY ONE DOMAIN AND NOT A LIST, deliberately, so the cardinality is enforced by the schema
	// and not by a webhook rule: one Binding is one tenant, every figure in Status is a single series
	// rather than a sum, and no rule for dividing one ceiling among several domains has to be
	// invented.
	//
	// EVERY FIELD IS IMMUTABLE, webhook-enforced. Name re-points this namespace at a different
	// ledger and strands the old one. BlockSize or Dtype changed under a warm cache is silent
	// corruption: the writes succeed, the reads succeed, and the tensors are wrong.
	//
	// +required
	Domain KVCachePoolBindingDomain `json:"domain" protobuf:"bytes,2,name=domain"`

	// Quota is what this namespace asks of its pool, shaped like the pool's own declared ceiling so
	// one concept is spelled one way on both sides of the grant.
	//
	// +required
	Quota KVCachePoolBindingQuota `json:"quota" protobuf:"bytes,3,name=quota"`
}

// KVCachePoolBindingQuota is the quota this namespace asks of its pool.
type KVCachePoolBindingQuota struct {
	// Ceiling is what this namespace may consume in its reuse domain, written verbatim into that
	// one tenant's requested quota rather than kept as a total this operator maintains.
	//
	//   - IT IS A REQUEST, NOT A GRANT. The pool reduces every tenant's effective quota in proportion
	//     when the sum of requests exceeds allocatable capacity, and Status.EffectiveQuota is what
	//     was actually granted.
	//   - EXCEEDING IT EVICTS RATHER THAN REFUSES, which is the opposite of what the word suggests.
	//     A write past the ceiling is not rejected: the store frees room by dropping this namespace's
	//     own older objects and retries. A ceiling set too low therefore costs cache inside this
	//     namespace rather than failed writes, and costs it without any counter moving. Writes are
	//     refused only when eviction cannot free enough, which needs those older objects held by
	//     unexpired read leases.
	//   - It is REQUIRED, because the state it would otherwise allow does not work: the storage layer
	//     has no default policy and refuses a tenant it holds no policy for, so a Binding without
	//     this field would pass admission, report Ready and refuse every byte its workloads wrote.
	//   - A value that is not positive is refused at admission.
	//
	// +required
	Ceiling resource.Quantity `json:"ceiling" protobuf:"bytes,1,name=ceiling"`
}

// KVCachePoolBindingPoolReference names the cluster-scoped pool this Binding grants.
//
// It carries no namespace, and that is the point: a pool is cluster-scoped, so there is none to
// name, and no namespaced object here ever reads across a namespace boundary.
type KVCachePoolBindingPoolReference struct {
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`
}

// KVCachePoolBindingDomain is the reuse identity: what the cache isolates on, and what its blocks
// are shaped like.
type KVCachePoolBindingDomain struct {
	// Name is the domain, and it becomes the storage layer's tenant_id verbatim.
	//
	//   - It must be claimed by NO OTHER BINDING on a master that serves this Binding's pool, which
	//     the webhook enforces: two Bindings on one domain over one master would share cache —
	//     possibly intended — but collide on one quota ledger, which never is. Uniqueness is per
	//     master rather than per pool because one master can serve several pools and the tenant space
	//     is master-global; it is not cluster-wide because two masters hold two ledgers.
	//   - The accepted shape is a DNS-1123 label, checked by the webhook. That is strictly inside
	//     what the master accepts as a tenant_id and is what a Kubernetes object name already looks
	//     like, so nobody learns a second naming rule. This is the ONLY place the shape is judged;
	//     every consumer downstream copies the name rather than re-judging it.
	//
	// +required
	// +k8s:validation:maxLength=63
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// BlockSize is the number of tokens one cache block holds.
	//
	// +required
	BlockSize int32 `json:"blockSize" protobuf:"varint,2,name=blockSize"`

	// Dtype is the element type the cached tensors carry, in the engine's own lowercase spelling.
	//
	// The exhaustive set belongs to whatever spec owns workloads, so this API does not enumerate it —
	// that would make a new engine dtype an API change — and the webhook judges the syntactic form
	// only. It is spelled to match its JSON name exactly; DType would not, and the openapi generator
	// records every such mismatch as a checked-in API rule violation.
	//
	// +required
	// +k8s:validation:maxLength=32
	Dtype string `json:"dtype" protobuf:"bytes,3,name=dtype"`
}

// KVCachePoolBindingStatus is the namespace's own view of the pool: what it asked for, what the pool
// actually granted, what it is using, and whether it is over.
//
// Every figure below is read from ONE tenant's series, because a Binding registers exactly one reuse
// domain and the storage layer's tenant IS that domain: nothing here is summed, and no figure can
// hide a second domain behind it.
//
// Every observed figure below is ABSENT rather than zero when it was not observed, and the two say
// different things: a figure reading zero was measured as zero, and a missing figure was not
// measured at all. Reading an absent one as zero is the mistake this distinction exists to prevent.
type KVCachePoolBindingStatus struct {
	// Phase summarizes the conditions: Provisioning, Ready, Error, Deleting.
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

	// RequestedQuota is what the operator asked the pool for on this namespace's behalf: the
	// requested quota it wrote for this Binding's one tenant. It is absent, with a Condition saying
	// why, whenever the operator refused to write — a master that answered that multi-tenancy is
	// off, or a policy source it cannot rewrite.
	RequestedQuota *resource.Quantity `json:"requestedQuota,omitempty" protobuf:"bytes,4,opt,name=requestedQuota"`

	// EffectiveQuota is what the pool actually granted. It is LOWER than RequestedQuota whenever the
	// sum of every tenant's request exceeds the pool's allocatable capacity: the pool then recomputes
	// each tenant's effective quota in proportion to what that tenant requested. A pool with no
	// mounted members grants ZERO to everyone, and that case carries its own Condition rather than
	// appearing as an ordinary shortfall. A granted zero is reported as zero; a quota nobody could
	// read is absent instead, because the two are different answers.
	EffectiveQuota *resource.Quantity `json:"effectiveQuota,omitempty" protobuf:"bytes,5,opt,name=effectiveQuota"`

	// Usage is what the master reports this namespace's reuse domain as holding, and WHICH figure
	// that is depends on the master's version rather than on this API.
	//
	// A master exposing used bytes apart from reservations is read as committed bytes, in-flight
	// writes deliberately left out — a burst of concurrent writes would otherwise read as consumption
	// that never happened. A master exposing one charged figure instead, the shape 0.3.13 introduced,
	// charges it when a write STARTS, so in-flight reservations are inside this number and cannot be
	// subtracted. The Binding's own QuotaObserved message says which of the two answered.
	Usage *resource.Quantity `json:"usage,omitempty" protobuf:"bytes,6,opt,name=usage"`

	// OverQuota is true when Usage exceeds EffectiveQuota, and it does NOT mean the domain tried to
	// write more than it was granted. The store never charges a domain past its grant, so writing past
	// it leaves Usage AT the grant and this false; what happens on that path instead is that the store
	// evicts the domain's OWN objects to make room and admits the write, and a write fails only while
	// every object holding the grant is pinned and nothing can be evicted.
	//
	// So this reports ONE situation: the grant was RECUT below what the domain already holds, which is
	// what a proportional cut does when the pool's members shrink or another Binding joins. Waiting on
	// it as the signal that writes are being refused is waiting for something that never arrives.
	//
	// An observed false — the ordinary, healthy case — is reported AS false. This field is absent only
	// when the tenant could not be scraped at all. Those two readings are the easiest pair here to
	// confuse and they mean opposite things.
	OverQuota *bool `json:"overQuota,omitempty" protobuf:"varint,7,opt,name=overQuota"`

	// Blocks and HitRate are OBSERVED from the master and the engine, never declared. They are absent
	// when the scrape does not carry this tenant, because a fabricated zero hit rate on a warm cache
	// is worse than no number at all. Zero blocks and "not in the scrape" are different facts here as
	// well: zero is a measurement, absence is the lack of one.
	//
	// This operator writes neither today, so both are always absent: the current store generation
	// exports no per-tenant object count, and its hit rate is per master rather than per tenant.
	Blocks *int64 `json:"blocks,omitempty" protobuf:"varint,8,opt,name=blocks"`

	// HitRate is a ratio held as a STRING with a pattern, never a float. See the pool's own HitRate
	// for why a pattern is safe on a computed ratio and what it obliges of whoever writes it.
	//
	// +k8s:validation:pattern="^(0(\\.[0-9]{1,4})?|1(\\.0{1,4})?)$"
	HitRate string `json:"hitRate,omitempty" protobuf:"bytes,9,opt,name=hitRate"`

	// UsedBy names the workloads in THIS namespace that hold the pool through this Binding — always a
	// single-scope query, and a non-empty UsedBy is what the finalizer refuses deletion on.
	//
	// IT IS WRITTEN BY THE CONSUMER, NOT BY THIS API'S OWN RECONCILER, which only reads it and
	// enforces on it. The kind that writes it is ModelDeployment, whose spec.kvCache.poolRef names a
	// Binding in its own namespace, so an empty list is not evidence that nobody holds the pool.
	// Entries leave Namespace empty, everything that can appear being in this Binding's own namespace.
	//
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	UsedBy []KVCacheObjectReference `json:"usedBy,omitempty" protobuf:"bytes,10,rep,name=usedBy"`
}

// KVCachePoolBindingList holds the list of KVCachePoolBinding.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KVCachePoolBindingList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KVCachePoolBinding `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*KVCachePoolBindingList)(nil)
