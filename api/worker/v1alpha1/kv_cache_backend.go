package v1alpha1

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// KVCacheBackend is the schema for worker.gpustack.ai.
//
// It declares which machines contribute what medium to one KV cache backend, and reports the
// backend's OBSERVED state. It is cluster-scoped because it is a privileged physical resource: it
// names nodes, claims host memory and host paths, and on the RDMA path needs hostNetwork plus
// /dev/infiniband. Tenant isolation is a different axis, owned one layer up.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["kvcb"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Type",type="string",jsonPath=".spec.type"
// +k8s:crd-gen:printcolumn:name="Phase",type="string",jsonPath=".status.phase"
// +k8s:crd-gen:printcolumn:name="Endpoint",type="string",jsonPath=".status.endpoints[?(@.name=='Client')].address"
// +k8s:crd-gen:printcolumn:name="Capacity",type="string",jsonPath=".status.capacity.total"
type KVCacheBackend struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   KVCacheBackendSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status KVCacheBackendStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*KVCacheBackend)(nil)

// KVCacheBackendSpec defines the desired spec of KVCacheBackend.
type KVCacheBackendSpec struct {
	// Type is the backend IMPLEMENTATION — who does placement, eviction, replication and
	// metadata. It is NOT the medium: where the bytes live is members[].medium, and collapsing
	// the two is the category error this field exists to make impossible.
	//
	// One value ships. It is spelled out rather than assumed so the object says what it is, and
	// so a second implementation widens an enum instead of reinterpreting an absent field.
	//
	// +k8s:validation:default="Mooncake"
	// +k8s:validation:enum=["Mooncake"]
	Type string `json:"type,omitempty" protobuf:"bytes,1,opt,name=type"`

	// Image is the container image every role of this backend runs.
	//
	// It is OPTIONAL. Left unset, the reconciler uses the cluster-wide default pinned in the
	// "kv-cache-backend-image" Setting, which is where a version this project has verified
	// belongs — an admin pins it once instead of restating it on every object. Set here, it
	// overrides that default for this backend alone.
	//
	// It is never DERIVED from the operator's own image the way the Device Manager's is: the
	// master and the engine client can be builds against different accelerator generations —
	// the base wheel's master links CUDA 12 while a current vLLM image carries CUDA 13 — so a
	// derived image would silently pair a master with a runtime it cannot load. Unset here AND
	// unset in the Setting is refused at admission, naming both places.
	//
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,2,opt,name=image"`

	// ImagePullPolicy is the policy every role of this backend pulls its image with.
	//
	// It is declared here rather than inherited from the cluster-wide "image-pull-policy" Setting.
	// That setting is a value of the bundled-application chart install and reaches nothing a
	// controller renders, so inheriting it would make this the one API in the group whose
	// workloads move when a chart value moves.
	//
	// Left unset, the operator RESOLVES the policy from the image tag by the rule the API server
	// would otherwise have applied — Always for :latest or for an image naming no tag at all,
	// IfNotPresent for anything else — and re-resolves it whenever the image or this field moves.
	// It is resolved rather than left empty because a field the server fills in cannot be
	// converged: an operator comparing against that default either rewrites the workload on every
	// pass or has to skip the comparison, and skipping it strands the value a spec has moved off.
	//
	// +k8s:validation:enum=["Always","IfNotPresent","Never"]
	ImagePullPolicy core.PullPolicy `json:"imagePullPolicy,omitempty" protobuf:"bytes,5,opt,name=imagePullPolicy,casttype=k8s.io/api/core/v1.PullPolicy"`

	// ImagePullSecrets names the secrets that pull this backend's images, on every role.
	//
	// Without it a private registry is unreachable: neither the leader Deployment nor a member
	// DaemonSet runs under a service account of ours carrying credentials, and the cluster-wide
	// "image-pull-secrets" Setting reaches only the bundled-application chart. The secrets live in
	// the namespace the workloads run in, which is this operator's own.
	//
	// The list is ATOMIC — it is replaced whole rather than merged. A structural schema may key a
	// list by a field only when that field is required and non-nullable, and LocalObjectReference's
	// name is neither.
	//
	// +listType=atomic
	// +k8s:validation:maxItems=32
	ImagePullSecrets []core.LocalObjectReference `json:"imagePullSecrets,omitempty" protobuf:"bytes,6,rep,name=imagePullSecrets"`

	// Connection describes how this backend is reached: managed by this operator, or external.
	// Exactly one is set, enforced by the webhook.
	//
	// +required
	Connection KVCacheBackendConnection `json:"connection" protobuf:"bytes,3,name=connection"`

	// Transport describes the data plane the members use.
	//
	// The empty object is the default, and it has to be. Structural-schema defaulting does not
	// descend into an object that is ABSENT, so without this the common spec — one that never
	// mentions a transport — would store no protocol at all and the field's own default would
	// silently not apply. Measured against an API server: omitted leaves `transport` empty, while
	// `transport: {}` comes back as `{"protocol":"Auto"}`.
	//
	// +k8s:validation:default={}
	Transport KVCacheBackendTransport `json:"transport,omitempty" protobuf:"bytes,4,opt,name=transport"`
}

// KVCacheBackendConnection is how the backend is reached. Exactly one branch is set; the webhook
// refuses both and neither, because a spec with no branch describes nothing and a spec with two
// describes two different backends.
type KVCacheBackendConnection struct {
	// Managed asks this operator to run the leader and the store members.
	Managed *KVCacheBackendManaged `json:"managed,omitempty" protobuf:"bytes,1,opt,name=managed"`

	// External names a backend somebody else runs. Nothing is rendered for it; the reconciler
	// only observes.
	External *KVCacheBackendExternal `json:"external,omitempty" protobuf:"bytes,2,opt,name=external"`
}

// KVCacheBackendManaged is the operator-run shape of a backend: the leader, and the member groups
// that contribute media to it.
type KVCacheBackendManaged struct {
	// Leader is the metadata service every member and every client talks to. The name is this
	// API's, not the artifact's: the rendered flags and environment variables keep the vendor's
	// own "master" spelling, and the mapping lives in the renderer.
	//
	// +required
	Leader KVCacheBackendLeader `json:"leader" protobuf:"bytes,1,name=leader"`

	// Members are the groups of store members. Each entry selects nodes and names the medium
	// those nodes contribute, so a hot DRAM tier and a cold filesystem tier are expressible in
	// the shape. This scope reconciles exactly one group and the webhook refuses a second,
	// naming the tiering follow-on: a two-group manifest is schema-valid and admission-refused
	// rather than half-reconciled.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=atomic
	Members []KVCacheBackendMember `json:"members" protobuf:"bytes,2,rep,name=members"`
}

// KVCacheBackendExternal is a backend this operator does not run.
type KVCacheBackendExternal struct {
	// Endpoints are the addresses of a backend somebody else runs, one entry per named role.
	// Both roles are required here, and for the same reason they are two entries and not one
	// string: this operator reads the Admin address and publishes the Client address, so an
	// external backend that named only one leaves either the scrape or every engine with
	// nothing to point at.
	//
	// It is a list rather than a single address so that a multi-leader backend needs no API
	// change to describe.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=map
	// +listMapKey=name
	Endpoints []KVCacheBackendEndpoint `json:"endpoints" protobuf:"bytes,1,rep,name=endpoints"`
}

// The names a KVCacheBackendEndpoint can carry. They are constants because three places have to
// agree on them — the object an admin writes for an external backend, the reconciler that publishes
// them for a managed one, and every consumer that picks one — and a typo in any of the three is a
// connection to an address nobody serves.
const (
	// KVCacheBackendEndpointNameClient is the address an inference engine connects to.
	KVCacheBackendEndpointNameClient = "Client"
	// KVCacheBackendEndpointNameAdmin is the address this operator reads: one port serving the
	// Prometheus exposition and the HTTP admin API both.
	KVCacheBackendEndpointNameAdmin = "Admin"
)

// KVCacheBackendEndpoint is one named address of a backend. The same type serves the external
// branch's input and the status's output, so a reader learns one shape.
type KVCacheBackendEndpoint struct {
	// Address is host:port.
	//
	// 259 and not 253: the bound is on host:port, and the host alone may be a DNS subdomain of the
	// full 253 characters, which leaves room for a colon and a five-digit port. At 253 the schema
	// refused an address the webhook's own host:port rule accepts.
	//
	// +required
	// +k8s:validation:maxLength=259
	Address string `json:"address" protobuf:"bytes,1,name=address"`

	// Name says who the address is for, and the two readers want different things. Client is
	// what an inference engine connects to. Admin is the port serving the Prometheus exposition
	// and the HTTP admin API both, which is what THIS OPERATOR reads. A consumer handed the
	// wrong one fails at connect time with nothing to point at, which is why the distinction is
	// carried in the API rather than left to a convention.
	//
	// +required
	// +k8s:validation:enum=["Client","Admin"]
	Name string `json:"name" protobuf:"bytes,2,name=name"`
}

// KVCacheBackendLeader is the leader process: how many of it, how it places new writes, and the
// escape hatch for flags this API does not enumerate.
type KVCacheBackendLeader struct {
	// Replicas is how many leader processes run. One, and only one, in this scope: electing a
	// leader among several needs a backend store this scope does not enter, and the webhook
	// refuses anything else while naming that follow-on rather than silently running one anyway.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,1,opt,name=replicas"`

	// AllocationStrategy is how the leader picks which member takes a new write. Random spreads
	// them; FreeRatioFirst biases toward the emptier member.
	//
	// The enum is deliberately the two that any pooled store would have, rather than every value
	// the current artifact's flag accepts: the others it accepts are specific to one medium or
	// one locality model, are reachable through ExtraArgs for anyone who needs them, and would
	// otherwise fix this API to one implementation's vocabulary. Widening the enum later is not
	// a breaking change.
	//
	// +k8s:validation:default="FreeRatioFirst"
	// +k8s:validation:enum=["Random","FreeRatioFirst"]
	AllocationStrategy string `json:"allocationStrategy,omitempty" protobuf:"bytes,2,opt,name=allocationStrategy"`

	// ExtraArgs passes flags this API does not enumerate straight through to the leader, after
	// the derived ones. A key that collides with a flag rendered from a field above is refused
	// at admission, because two sources for one flag make the rendered command ambiguous.
	ExtraArgs map[string]string `json:"extraArgs,omitempty" protobuf:"bytes,3,rep,name=extraArgs"`
}

// KVCacheBackendTransport is the data plane the members use.
type KVCacheBackendTransport struct {
	// Protocol is the transport the members use. Auto resolves to TCP, and
	// status.members[].protocol reports what the leader says each member actually came up on, so
	// the outcome is observed rather than assumed from this field.
	//
	// Auto is deliberately NOT a per-node probe that promotes itself to a faster fabric, for two
	// reasons. A member group renders one DaemonSet, so a single Pod template covers every node the
	// group selects and cannot carry a different transport per node. And promoting to RDMA means
	// granting hostNetwork and two capabilities: a privilege is requested, never inferred on an
	// operator's behalf.
	//
	// It stays in the enum rather than being dropped because it is the honest answer for an
	// operator with no opinion, and because it is where node-level fabric discovery would attach
	// later without an API change.
	//
	// TCP is the universal fallback. RDMA, HIP and Ascend are peers of one another — each is a
	// fabric- or vendor-specific fast path, not a spelling of TCP: the ROCm build compiles a HIP
	// transport in, and the NPU build ships a separate Ascend transport library linking the CANN
	// runtime.
	//
	// The bar for membership here is "measured as compiled into a published artifact", which is
	// what excludes the other ten strings that artifact's config parser accepts. It is NOT
	// "measured to move bytes": only TCP has been exercised end to end, and RDMA, HIP and Ascend
	// each await a run on that hardware. A member also needs the runtime its transport links —
	// Ascend needs CANN in the member image — and the webhook cannot see inside an image, so
	// that pairing is the operator's to get right.
	//
	// +k8s:validation:default="Auto"
	// +k8s:validation:enum=["Auto","TCP","RDMA","HIP","Ascend"]
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,1,opt,name=protocol"`
}

// KVCacheBackendMember is one group of store members: the nodes it selects, the medium each
// contributes, and how much.
type KVCacheBackendMember struct {
	// NodeSelector selects the nodes that contribute this medium. One member runs per selected
	// node; widening the selector adds members and the leader admits their segments into
	// subsequent allocation immediately, with no leader or member restart.
	//
	// +required
	NodeSelector map[string]string `json:"nodeSelector" protobuf:"bytes,1,rep,name=nodeSelector"`

	// Medium is what this member group physically contributes. DFS covers NFS and 3FS, which
	// are media rather than backend implementations.
	//
	// The enum carries all five because all five are media the store itself supports, and the
	// shape a tiered backend will need must not change later. Only "DRAM" is RECONCILED here:
	// the other four additionally need the leader's file or DAX flags and a mount on the member,
	// and nothing renders those yet, so admission refuses them rather than starting a member that
	// would quietly hold its segment in memory under a name that says otherwise.
	//
	// +required
	// +k8s:validation:enum=["DRAM","LocalDisk","NoF","CXL","DFS"]
	Medium string `json:"medium" protobuf:"bytes,2,name=medium"`

	// CapacityPerMember sizes ONE member, not one node: a node can eventually run several
	// members, one per NUMA domain. It becomes the member's global segment size and is counted
	// into the member Pod's own resource request, so a member that does not fit stays Pending
	// instead of overcommitting the node.
	//
	// +required
	CapacityPerMember resource.Quantity `json:"capacityPerMember" protobuf:"bytes,3,name=capacityPerMember"`

	// LocalBufferSize is the member client's local staging buffer, counted into the Pod's
	// memory request beside CapacityPerMember.
	LocalBufferSize resource.Quantity `json:"localBufferSize,omitempty" protobuf:"bytes,4,opt,name=localBufferSize"`

	// ExtraArgs passes config keys this API does not enumerate straight through to the member. It
	// is keyed by CONFIG KEY rather than by environment-variable name — one namespace per side,
	// each the one its own binary documents. A key that collides with one derived from a field
	// above is refused at admission.
	ExtraArgs map[string]string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// Image overrides the backend's Image for this member group only. Left unset, the group runs
	// the backend's Image.
	//
	// A group's NodeSelector is what makes this necessary: two groups can select nodes of different
	// accelerator vendors or generations, and the store's client ships as one wheel per vendor —
	// CUDA 12, CUDA 13, ROCm, NPU — each carrying the transports it was compiled with and the
	// runtime it links. The transport itself is backend-wide, so this is not a per-group transport;
	// it is the per-group runtime that one transport needs on differing hardware.
	//
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,6,opt,name=image"`
}

// KVCacheBackendStatus defines the observed state of KVCacheBackend.
type KVCacheBackendStatus struct {
	// Phase summarizes the conditions: Provisioning, Ready, Degraded, Error, Deleting. It is
	// derived from the leader's own health document rather than from its Pod phase — a Running
	// Pod whose leader reports its service not ready is Provisioning, not Ready.
	Phase string `json:"phase,omitempty" protobuf:"bytes,1,opt,name=phase"`

	// PhaseMessage carries the reason for the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Conditions is the finer view, one condition per axis: LeaderAvailable, MembersMounted,
	// CapacityObserved, Deletable. Every one is derived from an observed document.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"` // nolint: lll

	// Endpoints are this backend's addresses, one entry per named role — the same shape the
	// external branch takes as input. A managed backend fills both from its own Service; an
	// external one echoes what was declared.
	//
	// +listType=map
	// +listMapKey=name
	Endpoints []KVCacheBackendEndpoint `json:"endpoints,omitempty" protobuf:"bytes,4,rep,name=endpoints"`

	// Capacity is what the leader reports it has and has allocated. It is ABSENT until a scrape
	// succeeds, and absent again is not the same as reporting zero.
	//
	// A POINTER because omitempty does not omit a zero-valued struct. Held by value it serialized
	// as "capacity": {} on every failed or gated observation — an empty object where the contract
	// says there should be no field at all, and a shape a client cannot tell from a scrape that
	// returned nothing.
	Capacity *KVCacheBackendCapacity `json:"capacity,omitempty" protobuf:"bytes,5,opt,name=capacity"`

	// Members is one entry per observed store member.
	//
	// +listType=map
	// +listMapKey=segmentName
	Members []KVCacheBackendMemberStatus `json:"members,omitempty" protobuf:"bytes,6,rep,name=members"`

	// UsedBy names the objects that consume this backend. A non-empty UsedBy is what the
	// finalizer refuses deletion on, so the field is the enforcement input and not a display.
	//
	// It is a TypedLocalObjectReference and not a core ObjectReference: five of that type's seven
	// fields mean nothing here, all of them are optional — so an entirely empty entry would
	// validate against a field a finalizer enforces on — and upstream tells new APIs not to embed
	// it. "Local" here means only that there is no namespace field, which is right: a backend is
	// cluster-scoped and so is everything that claims one.
	//
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=name
	UsedBy []core.TypedLocalObjectReference `json:"usedBy,omitempty" protobuf:"bytes,7,rep,name=usedBy"`
}

// KVCacheBackendCapacity is the backend's capacity AS THE LEADER REPORTS IT. Both figures are
// pointers and stay absent when the scrape failed: a zero here would read as an empty cache, and a
// retained previous value would read as a current one.
type KVCacheBackendCapacity struct {
	Total *resource.Quantity `json:"total,omitempty" protobuf:"bytes,1,opt,name=total"`
	Used  *resource.Quantity `json:"used,omitempty" protobuf:"bytes,2,opt,name=used"`
}

// KVCacheBackendMemberStatus is one observed store member.
type KVCacheBackendMemberStatus struct {
	// SegmentName is the member's segment as the leader knows it, derived from the node.
	//
	// +required
	SegmentName string `json:"segmentName" protobuf:"bytes,1,name=segmentName"`

	// NodeName is the node contributing this member's medium.
	NodeName string `json:"nodeName,omitempty" protobuf:"bytes,2,opt,name=nodeName"`

	// Medium is what this member contributes, echoed from the group that selected its node.
	Medium string `json:"medium,omitempty" protobuf:"bytes,3,opt,name=medium"`

	// Protocol is the transport the LEADER reports this member came up on.
	//
	// It is an OBSERVATION throughout, never an echo of spec.transport.protocol, and the two can
	// disagree: a member handed an RDMA request on a node whose device is missing comes up on TCP.
	// Read it as what the data plane is doing, and the spec field as what was asked for.
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,4,opt,name=protocol"`

	// State is the member's state AS THE LEADER REPORTS IT, read from the leader's own segment
	// listing rather than inferred from the member Pod. The states the store defines, in this API's
	// casing: OK, Draining, Drained, GracefullyUnmounting, Unmounting, Undefined.
	//
	// It carries no "unreached" sentinel, because there is no pass that would write one: a listing
	// that cannot be read leaves the PREVIOUS entries in place and says so through MembersMounted,
	// rather than rewriting them as blank. Whether what is here was just refreshed is the
	// condition's question, and this field never answers it.
	//
	// Draining and the two unmounting states are what a shrink passes through, so the field can
	// distinguish a member on its way out from one that is simply gone. That is the whole reason it
	// carries the store's vocabulary instead of a summary of it.
	//
	// It carries no enum marker deliberately, unlike every enum on the spec side. The value's domain
	// belongs to the store and not to this API: a store version that adds a state would make the
	// whole status write fail validation — not this one field, the entire object — leaving every
	// other status field frozen at its last value. Phase, one field up, is open for the same reason.
	State string `json:"state,omitempty" protobuf:"bytes,5,opt,name=state"`
}

// KVCacheBackendList holds the list of KVCacheBackend.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KVCacheBackendList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KVCacheBackend `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*KVCacheBackendList)(nil)
