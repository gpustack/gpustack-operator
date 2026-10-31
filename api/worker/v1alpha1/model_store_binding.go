package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// ModelStoreBinding is the schema for worker.gpustack.ai.
//
// It is the PROVISIONING POINT for model prefetch: creating one in a namespace is what grants that
// namespace a byte budget on the named stores and the right to pin, so both are grants an admin can
// RBAC and audit rather than powers a tenant assumes. A namespace without a Binding cannot warm
// anything, and a Binding is the single place its consumption is reported back to it.
//
// THE GRANT IS EXPLICIT. There is no default pool a Binding falls back to: the chart's default
// pool is deployment configuration, not an authorization basis, and a fallback would create a
// second source of truth for who may warm where.
//
// It is NOT an enforcement boundary and must not be described as one. The budget gates prefetch
// admission and accounting; bytes a workload mounts through the ordinary delivery path are charged
// to the node's watermarks exactly as before, never to this object.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["msb"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Quota",type="string",jsonPath=".spec.quota.bytes"
// +k8s:crd-gen:printcolumn:name="AllowPinned",type="boolean",jsonPath=".spec.allowPinned"
// +k8s:crd-gen:printcolumn:name="SourceMemory",type="string",jsonPath=".spec.sourceMemory.bytes"
// +k8s:crd-gen:printcolumn:name="Used",type="string",jsonPath=".status.usedBytes"
// +k8s:crd-gen:printcolumn:name="Phase",type="string",jsonPath=".status.phase"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type ModelStoreBinding struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelStoreBindingSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelStoreBindingStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelStoreBinding)(nil)

// ModelStoreBindingSpec is the grant. StoreRefs is immutable, AllowPinned may be enabled,
// and Quota.Bytes may grow. SourceMemory follows its field's online update rules;
// existing reservations remain accounted when the grant changes or is removed.
type ModelStoreBindingSpec struct {
	// StoreRefs names the ModelStores this namespace may prefetch into, at least one of them.
	// Prefetch admission refuses a prefetch whose target set falls outside every granted store.
	//
	// +required
	// +listType=atomic
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=8
	StoreRefs []ModelStoreBindingStoreReference `json:"storeRefs" protobuf:"bytes,1,rep,name=storeRefs"`

	// Quota is the namespace's byte budget.
	//
	// +required
	Quota ModelStoreBindingQuota `json:"quota" protobuf:"bytes,2,name=quota"`

	// AllowPinned is whether prefetches in this namespace may pin. It defaults to false, and it is
	// the only path to pinning: pinning is an admin-granted capability, never a tenant default.
	//
	// +optional
	AllowPinned *bool `json:"allowPinned,omitempty" protobuf:"varint,3,opt,name=allowPinned"`

	// SourceMemory grants the registered memory this namespace's source preparation may hold: the
	// ceiling its admitted reservations draw on, the registration count, and the admitted read
	// sessions it may run. It is the whole grant and it is SEPARATE from the disk one —
	// quota.bytes counts filesystem usage and allowPinned grants pinning, and neither authorizes
	// a byte of memory.
	//
	// Absent means source preparation follows the node's configured source capacity.
	// While present, bytes and registrations may grow but cannot shrink. Sessions may
	// change online; existing sessions finish before a lower limit admits new ones.
	// Removing the section returns to node capacity without releasing existing charges.
	//
	// +optional
	SourceMemory *ModelStoreBindingSourceMemory `json:"sourceMemory,omitempty" protobuf:"bytes,4,opt,name=sourceMemory"`
}

// ModelStoreBindingSourceMemory is the registered-memory grant source preparation draws on. All
// three limits are administrator-set, because a limit nobody set would be a default pretending to
// be a capacity answer: the memory a checkpoint needs is the artifact's, not the chart's.
type ModelStoreBindingSourceMemory struct {
	// Bytes is the registered-memory ceiling this namespace's admitted reservations draw on. It
	// is a memory budget, not a second disk quota: filesystem usage never consumes it, and
	// registered bytes never consume quota.bytes.
	//
	// +required
	Bytes resource.Quantity `json:"bytes" protobuf:"bytes,1,name=bytes"`

	// Registrations is how many live registrations the namespace's source generations may hold at
	// once.
	//
	// +required
	// +k8s:validation:minimum=1
	Registrations int32 `json:"registrations" protobuf:"varint,2,opt,name=registrations"`

	// Sessions is how many read sessions the namespace may hold admitted at once. Read-session
	// admission is deliberately its own limit and never inferred from prefetch retention: a
	// namespace whose prefetches pin content has not thereby bought its engines unlimited reads.
	//
	// +required
	// +k8s:validation:minimum=1
	Sessions int32 `json:"sessions" protobuf:"varint,3,opt,name=sessions"`
}

// ModelStoreBindingStoreReference names a cluster-scoped ModelStore this namespace is granted.
//
// It carries no namespace, and that is the point: a store is cluster-scoped, so there is none to
// name, and no namespaced object here ever reads across a namespace boundary.
type ModelStoreBindingStoreReference struct {
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`
}

// ModelStoreBindingQuota is the namespace's byte budget, measured in filesystem usage.
type ModelStoreBindingQuota struct {
	// Bytes is what the namespace's prefetches may occupy in total. The measure is the content's
	// own size, per digest and per node holding it, so a tree two namespaces share counts fully
	// against each of them — conservative, and impossible to game by warming what another tenant
	// already warmed. An increase passes admission; a decrease is refused.
	//
	// +required
	Bytes resource.Quantity `json:"bytes" protobuf:"bytes,1,name=bytes"`
}

// ModelStoreBindingStatus is the namespace's own view of its grant: what it is using and whether
// it is over.
//
// Every observed figure below is ABSENT rather than zero when it was not measured, and the two say
// different things: a figure reading zero was measured as zero, and a missing figure was not
// measured at all.
type ModelStoreBindingStatus struct {
	// Phase summarizes the conditions: Ready, OverQuota, Error.
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

	// UsedBytes is what the namespace's prefetches occupy, the full size of every distinct digest
	// any of them targets on any node holding it. It is absent while nothing has been measured,
	// and zero is a measurement rather than the lack of one.
	//
	// +optional
	UsedBytes *resource.Quantity `json:"usedBytes,omitempty" protobuf:"bytes,4,opt,name=usedBytes"`

	// ReservedMemory is this binding's retained source-memory charge in the pinned authority,
	// including filling and draining generations and their runtime overhead. Physical bytes
	// shared with another namespace still charge each reservation separately. It is absent
	// while no retained charge is measured or the authority cannot be read.
	//
	// +optional
	ReservedMemory *resource.Quantity `json:"reservedMemory,omitempty" protobuf:"bytes,5,opt,name=reservedMemory"`
}

// ModelStoreBindingList holds the list of ModelStoreBinding.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelStoreBindingList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelStoreBinding `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelStoreBindingList)(nil)
