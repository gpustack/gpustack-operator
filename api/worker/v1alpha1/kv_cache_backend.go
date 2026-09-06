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
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
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

	// Members are the groups of store members. Each entry selects nodes, names the medium those
	// nodes contribute, and may add a local disk tier on the same nodes.
	//
	// A group's POSITION in this list is its identity: the rendered DaemonSet's name and its
	// immutable selector labels are derived from it, and so is the port that group's members serve
	// their HTTP API on. Reordering entries, or removing one ahead of others, therefore redefines
	// every position after it — the members there are rebuilt against a different group's spec, and
	// their cache goes with them.
	//
	// GIVING A GROUP A NAME OF ITS OWN IS POSSIBLE AND IS DELIBERATELY NOT DONE. A name independent
	// of position would make reordering free, and the price of introducing one is paid once, in
	// full: a DaemonSet's spec.selector cannot be changed after creation, so every existing member
	// DaemonSet has to be deleted and recreated, and the entire cache goes with them. The criterion
	// is whether one full rebuild is worth it, and the trigger would be operators genuinely needing
	// to reorder or delete middle groups often enough to amortize that migration. There is no
	// evidence of such a need today. That is the state of the decision, not an argument for either
	// side, and a reader who has that evidence is the one who should reopen it.
	//
	// What the immutability refusal protects is THE CACHE, not against a misjudgement. "Comparing
	// groups by index misjudges them" is not the reason: it agrees with how rendering already
	// works, since after a reorder the group at position i really does hold different content and
	// those members would be rebuilt against another group's spec regardless.
	//
	// The cap of 32 is a SAFETY BOUND, not a statement about how many groups are useful. The port
	// derivation would stay valid to 57455; what makes 32 the right place to stop is that the shapes
	// this list is for — a hot and a cold tier, or one group per kind of hardware — are a handful,
	// while an unbounded list can render a port outside the valid range with nothing reporting it.
	//
	// +required
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=32
	// +listType=atomic
	Members []KVCacheBackendMember `json:"members" protobuf:"bytes,2,rep,name=members"`

	// ScaleIn is what a member does on its way out. Left unset, a member is stopped the way any
	// Pod is: it gets SIGTERM and the time its own shutdown needs, and nothing waits for the
	// readers of what it held.
	ScaleIn *KVCacheBackendScaleIn `json:"scaleIn,omitempty" protobuf:"bytes,3,opt,name=scaleIn"`
}

// KVCacheBackendScaleIn is what a member does on its way out.
//
// It carries a duration and NOT a policy enum. The other policy a draft of this API carried —
// migrating a member's data before it leaves — needs the store's drain job API, which is stateful
// orchestration this scope does not enter and which reaches only the memory and NVMe-oF replicas:
// it cannot name the segments of the disk-backed ones, and skips those keys without counting them
// as blocked, so it reports success over a disk tier it left untouched. So a policy field would
// ship with one value, which is a knob nobody can turn. It arrives when there are two; widening an
// enum is not a breaking change.
type KVCacheBackendScaleIn struct {
	// GracePeriodSeconds is how long a departing member holds its local disk tier open after
	// deregistering it with the leader, so offload reads already in flight finish there rather
	// than failing.
	//
	// It reaches ONLY the disk tier. The memory segment is still dropped rather than drained, and
	// not for want of a verb: the member's own API takes a graceful unmount with a grace period,
	// but it requires the segment ids, no route returns a client its own ids, and the name is not
	// derivable because the leader appends a fresh port on every start.
	//
	// So this is blocked on an upstream route, and one upstream route is the whole of what unblocks
	// it: a way for a client to read back its own segment ids. It is NOT blocked on the shutdown
	// hook talking to a fresh process that has forgotten them — a preStop runs against the same
	// process that mounted the segments, so anything reasoning from client identity is testing the
	// wrong claim. Until that route exists, shrinking a group drops the memory it held, and for a
	// cache that is a cost rather than a fault: the data is recomputable.
	//
	// The Pod's terminationGracePeriodSeconds is DERIVED from this rather than set beside it, so
	// the kubelet cannot kill the container in the middle of the wait this configures.
	//
	// A plain int32 and not a pointer: unset and zero mean the same thing here. Zero still
	// deregisters the tier, it just does not wait afterwards, which is what a member with no grace
	// configured should do.
	//
	// The upper bound is the entrypoint's own. It refuses a larger value with HTTP 400, so a
	// manifest above it would render a shutdown hook that fails every time it runs.
	//
	// SETTING THIS DOES NOT PROTECT THE SAME EDIT THAT SHRINKS THE GROUP. The value is rendered into
	// the member's Pod, and a Pod runs the template it was CREATED from — so a departing member
	// leaves with whatever grace it started with, and only its replacements carry the new one. An
	// apply that raises the grace and narrows nodeSelector at once therefore drains nothing.
	//
	// To make a grace apply to a shrink, do it in two steps: change only this field and wait for the
	// members to be recreated with it (their pod-spec-hash annotation moves), then narrow the
	// selector or remove the group.
	//
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=3600
	GracePeriodSeconds int32 `json:"gracePeriodSeconds,omitempty" protobuf:"varint,1,opt,name=gracePeriodSeconds"`
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

	// MultiTenancy turns on the leader's per-tenant quota ledger and the tenant-scoped shard index
	// behind it. Off, every request falls into one default tenant and the index degrades to a plain
	// key hash, so two callers using different tenant names read each other's cache.
	//
	// It is a FIELD rather than an extraArgs entry because another API validates against it: a
	// KVCachePool is refused when its backend has no ledger to write quota into. A webhook reading
	// an unschema'd string — "true", "1", "True" — would be judging a value domain that belongs to
	// whoever typed it.
	//
	// A plain bool, not a pointer, because unset and false mean the same thing here: no ledger.
	// Unset renders NO flag rather than an explicit false, so a backend that never asked for this
	// runs the command line it ran before the field existed.
	MultiTenancy bool `json:"multiTenancy,omitempty" protobuf:"varint,4,opt,name=multiTenancy"`

	// ExtraArgs passes flags this API does not enumerate straight through to the leader, after
	// the derived ones. A key that collides with a flag rendered from a field above is refused
	// at admission, because two sources for one flag make the rendered command ambiguous.
	//
	// EVERY VALUE HERE IS WORLD-READABLE. Each entry is rendered into the leader container's argv
	// as -key=value, so it is visible to anyone who can read the Pod or its controller, and it
	// stays visible for the life of the object. A credential does not belong here. This operator
	// renders no flag that carries one, so this field is the only way one arrives.
	ExtraArgs map[string]string `json:"extraArgs,omitempty" protobuf:"bytes,3,rep,name=extraArgs"`

	// Offload turns on writing evicted keys to the members' local disk tier. It is the leader's
	// half of a pair: the other half is members[].localDisk, which says where on each node those
	// bytes go, and admission refuses either half alone because the store degrades on both
	// mismatches without reporting either.
	Offload *KVCacheBackendLeaderOffload `json:"offload,omitempty" protobuf:"bytes,5,opt,name=offload"`
}

// KVCacheBackendLeaderOffload turns the local disk tier on, leader side.
//
// Both settings are the leader's, and Enabled gates the feature outright: the store ANDs its
// eviction-time and promotion behavior with it, and every offload entry point returns early
// without it. A tier configured on the member alone is inert, which is why admission requires the
// two halves together rather than letting one render on its own.
type KVCacheBackendLeaderOffload struct {
	// Enabled turns on offloading to the members' local disks.
	//
	// A plain bool, not a pointer, because unset and false mean the same thing: no offloading.
	// Unset renders NO flag rather than an explicit false, so a backend that never asked for this
	// runs the command line it ran before the field existed.
	Enabled bool `json:"enabled,omitempty" protobuf:"varint,1,opt,name=enabled"`

	// OnEvict defers the write to disk from the moment a key is stored to the moment it is
	// evicted, so a key that is never evicted is never written to disk.
	//
	// It REQUIRES Enabled. The store ANDs the two, so setting this alone is accepted, echoed back
	// in the leader's own startup log, and then does nothing — which is why admission refuses it
	// rather than rendering a flag that reads as taken.
	OnEvict bool `json:"onEvict,omitempty" protobuf:"varint,2,opt,name=onEvict"`
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

	// Medium is what the SEGMENT this member group mounts is made of. One value: host memory.
	//
	// It is an identity rather than a choice, which is why the field survives with a single value
	// exactly as spec.type does: the object states what the group contributes, so a second medium
	// widens this enum instead of being inferred from a field that is not there.
	//
	// An earlier shape offered five values, and four of them named things that are not member
	// groups at all. A local disk is not a group of its own — the leader routes an offload task to
	// the client holding the key's memory replica, so a group with no memory segment never
	// receives one — and it is declared in the localDisk field below, on the group that does hold
	// the memory. NVMe-oF is a target coordinate registered once, with no node affinity and no
	// Pod. A DAX device and a distributed filesystem are configured on the leader's own process,
	// not on any member. Each is reachable, and none of them through this field.
	//
	// NARROWING THIS ENUM CARRIES A RESIDUAL RISK, KNOWINGLY ACCEPTED. An object created with one
	// of the four removed values, while this CRD was installed but the webhook was not, becomes
	// undeletable: CRD schema validation runs on the WRITE path only (rest.BeforeCreate /
	// rest.BeforeUpdate), so the object still reads back fine, but every update is refused —
	// including the controller removing its finalizer. Reads are not the failure; deletion is.
	//
	// The exposure is development clusters only. This type is absent from every tag from v0.7.3
	// through v0.8.6, so no cluster running a release can hold such an object. The
	// accepted risk is therefore bounded by the first release that ships this type, and clearing
	// it is that release's job: before it, either confirm no leftover objects exist, or write down
	// a recovery procedure. Widening the enum later is not a breaking change, so a fifth medium
	// that turns out to be a member group after all costs nothing to add.
	//
	// +required
	// +k8s:validation:enum=["DRAM"]
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
	//
	// EVERY VALUE HERE IS WORLD-READABLE. Each entry is rendered into the member container's argv
	// as -D key=value, so it is visible to anyone who can read the Pod or its controller, and it
	// stays visible for the life of the object. A credential does not belong here. This operator
	// renders no flag that carries one, so this field is the only way one arrives.
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

	// LocalDisk declares a directory on the nodes this group already selects and points the store
	// client's offload keys at it. Left unset, the group is memory only.
	//
	// WHAT IS AND IS NOT ESTABLISHED. Setting this is observed to make the leader accept a local
	// disk segment from the member, publish the declared capacity, and run eviction — and THIS
	// PROJECT HAS NOT OBSERVED DATA ACTUALLY REACHING THE TIER in any environment. Filling a
	// member's memory segment under a low watermark, the leader reported objects "deferred for disk
	// offload" while the tier stayed empty, in both configurations this API can render. The cause is
	// not established and this is not a claim about the store in general.
	//
	// So before relying on the tier, check the one figure that answers the question: the leader's
	// own master_allocated_file_size_bytes is bytes actually written, and reads 0 for a tier that
	// holds nothing while every other signal looks healthy. status.capacity reports the declared
	// CAPACITY and will not show this.
	//
	// It is a LAYER on this group rather than a group of its own, and that is the store's shape
	// rather than a simplification here: the leader routes an offload task to the client that owns
	// the key's memory replica, so a member holding no memory segment is never chosen. Such a
	// member would still report its disk capacity to the leader, so the backend would show a cold
	// tier of several terabytes that never takes a byte.
	LocalDisk *KVCacheBackendMemberLocalDisk `json:"localDisk,omitempty" protobuf:"bytes,7,opt,name=localDisk"`
}

// KVCacheBackendMemberLocalDisk is the local SSD tier this member group's nodes contribute.
//
// It is the member's half of a pair; the leader's half is leader.offload, and admission refuses
// either half alone. Set on its own, the leader never enqueues an offload task and the disk stays
// empty while the member reports its capacity, which is a tier that reads as present and is not.
type KVCacheBackendMemberLocalDisk struct {
	// Path is the directory on each selected node that holds this tier. It is mounted into the
	// member container from the host at the same location.
	//
	// It is REQUIRED and has no default. The store defaults it to a path of its own, and choosing
	// a host directory on somebody else's nodes is not a default this operator may pick: the wrong
	// one fills a filesystem that nothing in Kubernetes accounts for.
	//
	// CREATING THIS DIRECTORY AND GIVING IT THE RIGHT OWNER IS YOURS, NOT THIS OPERATOR'S. Nothing
	// here creates or chowns the path; a member whose container cannot write it fails at start.
	// That is a deliberate omission rather than a missing feature, and the reason is recorded here
	// so it can be judged rather than inherited: both ways of writing it need an apology attached.
	// An init container that chowns has to name a uid, while members[].image can put a different
	// vendor's build — and a different uid — on each group, so the uid that is right for one group
	// is a guess for the next. A chmod 0777 instead opens the directory to every process on the
	// node. A design where either choice needs a caveat is one that is not settled, so the switch
	// that would render it does not exist. An operator who has a uid that holds for their whole
	// backend has information this API does not, which is the case that would settle it.
	//
	// +required
	// +k8s:validation:maxLength=4096
	Path string `json:"path" protobuf:"bytes,1,name=path"`

	// Capacity caps what this tier stores. Left unset, the store's own ceiling applies and nothing
	// is rendered, so a ceiling that moves upstream is a change to investigate rather than one
	// this API silently restated.
	//
	// It is NOT counted into the Pod's resource requests, unlike CapacityPerMember. The tier is a
	// host directory, which is outside the kubelet's ephemeral-storage accounting entirely — a
	// request against it would reserve a figure nothing polices and would then keep the member off
	// the very node that has the disk. Watching that filesystem is the operator's, and the
	// documentation says so.
	Capacity resource.Quantity `json:"capacity,omitempty" protobuf:"bytes,2,opt,name=capacity"`
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
	// It is written by the CONSUMERS, not by this backend's own reconciler, which only reads it and
	// holds its teardown on it. Today exactly one consumer writes here: a KVCachePool claims the
	// backend it draws from, under kind KVCachePool, when its reconciler resolves it — and drops the
	// claim only after removing what it registered on that backend's master.
	//
	// It is neither a core ObjectReference nor a TypedLocalObjectReference. The first has seven
	// fields, five of which mean nothing here, all optional — so an entirely empty entry would
	// validate against a field a finalizer enforces on — and upstream tells new APIs not to embed
	// it. The second is closer but cannot be KEYED: its apiGroup is optional with no default, and a
	// structural schema takes a list map key only where the field is required or defaulted, so a
	// list keyed on kind and name would silently merge two objects differing only by group.
	//
	// KVCacheObjectReference drops the group and states the constraint that replaces it — an entry
	// may only name a kind in this API group — so all three of its fields are required and all three
	// are keys. Entries here leave Namespace empty: a backend is cluster-scoped and so is everything
	// that claims one.
	//
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	UsedBy []KVCacheObjectReference `json:"usedBy,omitempty" protobuf:"bytes,7,rep,name=usedBy"`
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
