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
	// Type is the backend IMPLEMENTATION — who does placement, eviction, replication and metadata.
	// It is NOT the medium: where the bytes live is members[].medium.
	//
	// One value ships, spelled out rather than assumed, so a second implementation widens an enum
	// instead of reinterpreting an absent field.
	//
	// +k8s:validation:default="Mooncake"
	// +k8s:validation:enum=["Mooncake"]
	Type string `json:"type,omitempty" protobuf:"bytes,1,opt,name=type"`

	// Image is the container image every role of this backend runs.
	//
	//   - Left unset, the cluster-wide "kv-cache-backend-image" Setting applies, which is where a
	//     version this project has verified belongs. Set here, it overrides that Setting for this
	//     backend alone.
	//   - Unset in BOTH places is refused at admission, naming both.
	//   - It is never DERIVED from the operator's own image the way the Device Manager's is: the
	//     master and the engine client can be builds against different accelerator generations, so
	//     a derived image would silently pair a master with a runtime it cannot load.
	//
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,2,opt,name=image"`

	// ImagePullPolicy is the policy every role of this backend pulls its image with.
	//
	//   - Left unset, the operator RESOLVES it from the image tag by the rule the API server would
	//     otherwise have applied: Always for :latest or for an image naming no tag at all,
	//     IfNotPresent for anything else. It re-resolves whenever the image or this field moves,
	//     and resolves rather than leaving the field empty so the rendered workload stays
	//     comparable against it.
	//   - It does NOT inherit the cluster-wide "image-pull-policy" Setting, which is a value of the
	//     bundled-application chart install and reaches nothing a controller renders.
	//
	// +k8s:validation:enum=["Always","IfNotPresent","Never"]
	ImagePullPolicy core.PullPolicy `json:"imagePullPolicy,omitempty" protobuf:"bytes,5,opt,name=imagePullPolicy,casttype=k8s.io/api/core/v1.PullPolicy"`

	// ImagePullSecrets names the secrets that pull this backend's images, on every role. They live
	// in the namespace the workloads run in, which is this operator's own.
	//
	//   - Without it a private registry is unreachable: no role runs under a service account of
	//     ours carrying credentials, and the cluster-wide "image-pull-secrets" Setting reaches only
	//     the bundled-application chart.
	//   - The list is ATOMIC, replaced whole rather than merged. A structural schema keys a list
	//     only by a required, non-nullable field, and LocalObjectReference's name is neither.
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
	// The empty object is the default, and it has to be: structural-schema defaulting does not
	// descend into an ABSENT object, so a spec that never mentions a transport would store no
	// protocol at all and protocol's own default would silently not apply.
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
	// A group's POSITION in this list is its identity: the DaemonSet's name, its immutable selector
	// labels and its members' HTTP port all derive from it. Giving a group a name of its own is
	// possible and DELIBERATELY not done — a DaemonSet's selector is immutable, so introducing one
	// deletes and recreates every member and the whole cache goes with them.
	//
	//   - Appending a group, removing from the END of the list, and editing a group where it stands
	//     are all ALLOWED, including the widening of a nodeSelector that is how a group gains nodes.
	//   - Moving a group to another position is REFUSED at admission: it redefines every later
	//     position and rebuilds those members against a different group's spec, cache included.
	//   - Two shapes are knowingly not caught, because without a name neither can be told from an
	//     ordinary edit: a reorder combined with an edit to the same group, and removing a group
	//     that an identical later group replaces. Both leave two identical groups in the list.
	//   - To take a group out of service without removing it, narrow its nodeSelector until it
	//     matches no node. The group keeps its position and nothing is rebuilt.
	//
	// The cap of 32 is a SAFETY BOUND, not a statement about how many groups are useful: the port
	// derivation stays valid to 57455, but an unbounded list can render a port outside the valid
	// range with nothing reporting it.
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
// It carries a duration and NOT a policy enum. The only other policy — migrating a member's data
// before it leaves — needs the store's drain job API, which reaches the memory and NVMe-oF replicas
// alone and reports success over a disk tier it left untouched, so a policy field would ship with
// one value. It arrives when there are two; widening an enum is not a breaking change.
type KVCacheBackendScaleIn struct {
	// GracePeriodSeconds is the wait the operator asks a departing member for, after that member
	// deregisters its local disk tier. It renders into the preStop hook as the endpoint's
	// grace_period_seconds and nothing else reads it. A plain int32, because unset and zero mean the
	// same thing here: deregister the tier, then do not wait.
	//
	//   - The TIER is the only thing deregistered on the way out. A memory segment is dropped rather
	//     than drained, so shrinking a group loses the memory it held — a cost rather than a fault
	//     for a cache, whose content is recomputable.
	//   - No hook drains that segment because the member REFUSES to unmount it. Measured against
	//     Mooncake 0.3.13, on members this operator rendered: both of the member's unmount routes
	//     answer 500 for the segment its rendered startup asks for, at every grace period tried, using
	//     the id the leader itself publishes for that segment; the two refusals name two different
	//     record sets as the ones searched. On the same member in the same session, the same route
	//     answers 200 for a segment mounted through the member's own mount route.
	//     So this is NOT a question of identifying the member: one holding the exactly correct id for
	//     a segment it is certain is its own is refused just the same. No identity supplied here --
	//     a Pod address, a client id -- changes that answer.
	//   - It does NOT hold the tier open, so sizing it to let in-flight peer reads finish sizes it
	//     against something that does not happen. Measured against Mooncake 0.3.13: deregistration
	//     takes effect at once and the process then waits the full value regardless, so a peer
	//     reading a disk-resident key gets a clean miss for the whole window rather than at the end
	//     of it. Another backend image may behave otherwise; what this operator guarantees is the
	//     value it sends.
	//   - The Pod's terminationGracePeriodSeconds is DERIVED from this rather than set beside it, so
	//     the kubelet cannot kill the container in the middle of the wait this configures.
	//   - Setting it does NOT protect the same edit that shrinks the group: a Pod runs the template
	//     it was CREATED from, so a departing member leaves with whatever grace it started with. To
	//     make a grace apply to a shrink, change only this field and wait for the members to be
	//     recreated with it — their pod-spec-hash annotation moves — then narrow the selector.
	//   - The upper bound is the entrypoint's own, which refuses a larger value with HTTP 400.
	//
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=3600
	GracePeriodSeconds int32 `json:"gracePeriodSeconds,omitempty" protobuf:"varint,1,opt,name=gracePeriodSeconds"`
}

// KVCacheBackendExternal is a backend this operator does not run.
//
// TWO OF THESE MAY NAME THE SAME BACKEND, AND NOTHING HERE NOTICES. The same leader is reachable
// under more than one spelling, and this operator compares no addresses across objects, deliberately:
// every identity it could compare is either editable or needs the leader reachable at admission.
// Keeping two objects off one leader is YOURS, and three things go wrong when they are not, none of
// them a refusal on any object:
//
//   - The quota of a shared reuse domain flips and never settles. Uniqueness is enforced only
//     between Bindings whose pools name the SAME backend object, so two Bindings reaching one leader
//     through two objects are both admitted on one domain name, and each pool's reconciler writes
//     its own quota.ceiling back over the other's on every pass.
//   - An undersized quota shows up as a LOW HIT RATE and nothing else. Exceeding it does not refuse
//     the write: the store frees room by dropping that tenant's own older objects and retries,
//     irreversibly, without any counter moving.
//   - Two Bindings claiming one domain.name with a different blockSize or dtype CORRUPT each other's
//     blocks — the only one of the three that produces wrong answers rather than slow ones. The
//     reuse identity an engine is handed is the domain NAME alone, so two differently-shaped caches
//     land under one identity: the writes succeed, the reads succeed, and the tensors are wrong.
type KVCacheBackendExternal struct {
	// Endpoints are the addresses of a backend somebody else runs, one entry per named role. Both
	// roles are required: this operator reads the Admin address and publishes the Client one, so an
	// external backend that named only one leaves either the scrape or every engine with nothing to
	// point at. It is a list rather than a single address so that a multi-leader backend needs no
	// API change to describe.
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

	// Name says who the address is for. Client is what an inference engine connects to; Admin is the
	// port serving the Prometheus exposition and the HTTP admin API both, which is what THIS
	// OPERATOR reads. A consumer handed the wrong one fails at connect time with nothing to point
	// at, which is why the distinction is carried in the API rather than left to a convention.
	//
	// +required
	// +k8s:validation:enum=["Client","Admin"]
	Name string `json:"name" protobuf:"bytes,2,name=name"`
}

// KVCacheBackendLeader is the leader process: how many of it, how it places new writes, and the
// escape hatch for flags this API does not enumerate.
type KVCacheBackendLeader struct {
	// Replicas is how many leader processes run, of which exactly one serves at a time. The rest are
	// standbys: they hold no data, answer no request, and exist to take over.
	//
	//   - More than one REQUIRES HighAvailability. Electing a leader among several needs a leadership
	//     record, and the webhook refuses the pair without one rather than silently running two
	//     leaders against the same members.
	//   - Raising this past one TURNS THE ELECTION ON, and the flip is re-evaluated on every
	//     reconcile rather than decided at create. It restarts the leader and rolls every member —
	//     the member's master entry changes shape with it — so the store's cached contents do not
	//     survive the crossing. The same holds on the way back down to one.
	//   - Raising this adds no capacity, which members do. The ceiling is here to catch the reading
	//     that it does, and it is duplicated in the webhook on purpose: this one still holds when
	//     the webhook is not installed, which is when a second leader would be rendered rather than
	//     refused. Raise both together; widening a maximum is not a breaking change.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=5
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,1,opt,name=replicas"`

	// HighAvailability elects the leader through a Kubernetes Lease, and it is what allows Replicas
	// above 1. The election itself needs no settings: the Lease is named after this backend, so
	// there is no connection target to supply, and the API access it needs is rendered beside the
	// workload. What the block does carry is what a standby is allowed to start from.
	//
	//   - Unset, the leader runs as a single process exactly as before — no election flag, no extra
	//     object, the command line it ran before this field existed.
	//   - Set with Replicas at 1, the ELECTION is INERT: one process has nothing to elect between,
	//     so no election flag, Lease or API token is rendered until Replicas rises above 1. That
	//     makes an empty block safe to set up front on a store image built without the k8s-lease
	//     backend, whose master fails at startup the moment the election flags appear — those flags
	//     arrive only when there is something for them to elect. Snapshot is the exception and says
	//     so on itself.
	//   - With MultiTenancy on, a failover costs HIT RATE for up to one KVCachePool reconcile
	//     interval. Each replica seeds its tenant quota policy at its own start, so a standby that
	//     took over after a quota was raised applies the older, lower ceiling, and an over-quota
	//     write in this store is not refused — it evicts that tenant's own older objects,
	//     irreversibly and without moving any counter. The quota itself is not lost: the pool
	//     reconciler is the authority and writes the difference back on its next pass.
	HighAvailability *KVCacheBackendLeaderHighAvailability `json:"highAvailability,omitempty" protobuf:"bytes,2,opt,name=highAvailability"`

	// AllocationStrategy is how the leader picks which member takes a new write. Random spreads
	// them; FreeRatioFirst biases toward the emptier member.
	//
	// The enum is deliberately the two any pooled store would have, not every value the current
	// artifact's flag accepts: the rest are specific to one medium or one locality model, are
	// reachable through ExtraArgs, and would fix this API to one implementation's vocabulary.
	// Widening the enum later is not a breaking change.
	//
	// +k8s:validation:default="FreeRatioFirst"
	// +k8s:validation:enum=["Random","FreeRatioFirst"]
	AllocationStrategy string `json:"allocationStrategy,omitempty" protobuf:"bytes,3,opt,name=allocationStrategy"`

	// MultiTenancy turns on the leader's per-tenant quota ledger and the tenant-scoped shard index
	// behind it. Off, every request falls into one default tenant and the index degrades to a plain
	// key hash, so two callers using different tenant names read each other's cache.
	//
	// It is a FIELD rather than an extraArgs entry because another API validates against it: a
	// KVCachePool is refused when its backend has no ledger to write quota into, and a webhook
	// reading an unschema'd "true", "1" or "True" would be judging a value domain that belongs to
	// whoever typed it. The store's global -quota_bytes flag stays in extraArgs for the converse
	// reason: no other API needs to interpret it.
	//
	// Unset and false both mean no ledger, and unset renders NO flag rather than an explicit false.
	MultiTenancy bool `json:"multiTenancy,omitempty" protobuf:"varint,4,opt,name=multiTenancy"`

	// ExtraArgs passes flags this API does not enumerate straight through to the leader, after
	// the derived ones. Each entry is one flag token of its own, "-flag" or "-flag=value", and the
	// entries render verbatim in the order written. An entry whose key — what precedes the first
	// "=" once the leading dashes are off — collides with a flag rendered from a field above is
	// refused at admission, because two sources for one flag make the rendered command ambiguous.
	//
	// EVERY VALUE HERE IS WORLD-READABLE: stored verbatim on this cluster-scoped object, then
	// rendered into the leader container's argv, readable by anyone who can reach the Pod or the
	// Deployment, for the life of the object. A credential does not belong here, and since this
	// operator renders no flag that carries one, this field is the only way one arrives.
	//
	// +listType=atomic
	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// ExtraEnv passes environment variables this API does not enumerate straight through to the
	// leader container. The leader reads a handful of its settings from the environment rather
	// than from flags — the store's local snapshot path is one — and this is the hatch for
	// whichever of those grows a use this API has no field for.
	//
	// A name this operator already renders is REFUSED at admission, for the same reason a
	// colliding ExtraArgs key is: Kubernetes accepts a container carrying one name twice and
	// leaves the winner to the runtime, so the collision would not even be reported.
	//
	// EVERY VALUE HERE IS WORLD-READABLE: stored verbatim on this cluster-scoped object, then
	// rendered into the leader container's environment, readable by anyone who can reach the Pod
	// or the Deployment, for the life of the object. A credential does not belong here, and since
	// this operator renders no variable that carries one, this field is the only way one arrives.
	//
	// +listType=map
	// +listMapKey=name
	ExtraEnv []InstanceEnvVar `json:"extraEnv,omitempty" protobuf:"bytes,6,rep,name=extraEnv"`
}

// KVCacheBackendLeaderHighAvailability turns leader election on, and carries what a standby is
// allowed to start from.
//
// DECLARING THE BLOCK IS THE SWITCH, and there is no key inside it to turn the feature back off.
// An `enabled: false` beside `replicas: 3` would be a third state that admission would have to
// adjudicate and every reader would have to remember, while presence has no such state. Lease
// tuning — duration, renew deadline — can also be added here later without a breaking change.
type KVCacheBackendLeaderHighAvailability struct {
	// Snapshot writes the leader's own metadata to storage both leader Pods can reach, so a standby
	// that takes over starts from that baseline rather than from nothing.
	//
	// Without it a standby REPLICATES NOTHING. The store's operation log is the only other way to
	// feed one, and it runs on a leadership backend this operator's image cannot carry, so the
	// snapshot is the whole of what a failover can recover. What it recovers is bounded by
	// IntervalSeconds: the cache comes back partially cold rather than entirely cold.
	//
	//   - The flags this renders arrive AS SOON AS THIS FIELD IS SET, unlike the election's, which
	//     wait for Replicas to rise above one. A single leader restores its own last snapshot when
	//     it restarts, which is worth having on its own — and it means a store image too old to
	//     carry the snapshot subsystem refuses to start here instead of ignoring the field.
	//   - A snapshot older than the running process is read back without a version check of any
	//     kind. Moving the image BACKWARDS across a snapshot format change is outside what this API
	//     makes any promise about.
	Snapshot *KVCacheBackendLeaderSnapshot `json:"snapshot,omitempty" protobuf:"bytes,1,opt,name=snapshot"`

	// MemberAddressing selects how a member is told to find the master once an election runs. Both
	// forms reach the leader that is serving, by different routes, and they are rendered into the
	// same one variable — so changing this rolls every member group.
	//
	//   - Lease: the member is handed the Lease's coordinates and reads the current holder itself.
	//     This needs the member to talk to the API server, which is why the member image has to
	//     carry the leadership backend at all.
	//   - Service: the member is handed the leader Service's address, exactly as it is without high
	//     availability. The Service publishes only READY endpoints and a standby deliberately is not
	//     ready, so the address resolves to the serving leader — the open part is whether the
	//     client's reconnect follows that endpoint across an election, and how long it takes.
	//
	// NEITHER FORM HAS BEEN MEASURED against the other. The default is Lease because that is what
	// this operator has always rendered, not because it won a comparison, and the number that would
	// settle it is how long a member cannot reach a master after the leader pod is deleted. Until
	// that is measured on a cluster, treat Service as the one to try rather than the one to trust.
	//
	// +k8s:validation:default="Lease"
	// +k8s:validation:enum=["Lease","Service"]
	MemberAddressing string `json:"memberAddressing,omitempty" protobuf:"bytes,2,opt,name=memberAddressing"`
}

// KVCacheBackendLeaderSnapshot is where the leader's metadata baseline is kept, and how much of it.
//
// ONE STORAGE SHAPE, and it is the narrower one. The store also speaks S3, which would need an
// endpoint, a bucket and a credential reference — a group of fields plus Secret handling, for a
// capability a ReadWriteMany claim already covers inside the cluster. Widening to it later adds a
// field beside this one and breaks nothing.
//
// There is no catalog setting either, because the catalog rides in the same storage: the store's
// embedded catalog is constructed over the object store itself, so one claim carries both the
// payloads and the index of which ones exist. The alternative it offers is a Redis, which would add
// an external dependency to a feature whose whole premise is not having one.
type KVCacheBackendLeaderSnapshot struct {
	// PersistentVolumeClaimName names the claim the snapshot is written to and read from, resolved
	// in the namespace this operator runs its workloads in — a KVCacheBackend is cluster-scoped and
	// has no namespace of its own to resolve it against.
	//
	// REQUIRED: the claim must be ReadWriteMany. The serving leader writes the snapshot and a
	// standby reads it, they are different Pods, and a claim only one of them can mount leaves the
	// standby reading an empty directory with nothing logging it. Admission does not check this —
	// the claim may not exist yet when the backend is created — so the reconciler checks it once it
	// can see the claim and publishes the answer as a condition.
	//
	// EVERY KEY THE CACHE HOLDS IS NAMEABLE FROM THIS VOLUME. A snapshot is the master's metadata
	// written as plain bytes with no encryption, so whoever can mount this claim can enumerate those
	// keys, including their tenant names under MultiTenancy. The claim also outlives the backend:
	// nothing here deletes it.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName" protobuf:"bytes,1,name=persistentVolumeClaimName"`

	// IntervalSeconds is how long the store waits between snapshots, which is the same thing as HOW
	// MUCH A FAILOVER LOSES: objects written since the last one are not in the baseline the standby
	// starts from. Unset renders no flag and leaves the store's own default in place, so a default
	// that moves upstream shows up as a behavior change to investigate rather than as a value this
	// API silently re-asserted.
	//
	// +k8s:validation:minimum=1
	IntervalSeconds *int32 `json:"intervalSeconds,omitempty" protobuf:"varint,2,opt,name=intervalSeconds"`

	// RetentionCount is how many recent snapshots are kept, older ones being deleted as newer ones
	// land. Its floor is one because the store refuses to start when snapshots are on and this is
	// zero. Unset renders no flag, for the reason above.
	//
	// Keeping more than one is not spare capacity: a restore tries the stored snapshots in turn and
	// falls back to an older one when a payload cannot be read, so the count is how many times that
	// fallback can happen.
	//
	// +k8s:validation:minimum=1
	RetentionCount *int32 `json:"retentionCount,omitempty" protobuf:"varint,3,opt,name=retentionCount"`
}

// KVCacheBackendTransport is the data plane the members use.
type KVCacheBackendTransport struct {
	// Protocol is the transport the members are ASKED to use. Auto resolves to TCP.
	//
	// THESE VALUES ARE NOT THE STORE'S OWN SPELLINGS. What is written here is translated before it
	// reaches a member, and two of the eight change word entirely: CANN renders as ascend and ROCM
	// as hip. So a member's environment, its logs, and status.members[].protocol below all report
	// the store's lowercase spelling rather than the one written here, and comparing the two as
	// strings finds a difference that is not one.
	//
	//   - TCP is the universal fallback. RDMA, EFA, CANN, ROCM, MUSA and MACA are peers of one
	//     another, each a fabric- or vendor-specific fast path rather than a spelling of TCP: EFA in
	//     particular is reached through libfabric's SRD provider and has no RC queue pairs, so the
	//     RDMA transport cannot drive it. MUSA and MACA are intra-node IPC transports, not host
	//     fabrics: they take no hostNetwork, no capabilities and no device resource.
	//   - Whether a member came up on what it asked for is NOT visible through this API.
	//     status.members[].protocol echoes this request back rather than reporting a result, so a
	//     member that fell back to the store's tcp still reads as the fabric there, while serving.
	//     Only the member's own log says which transport the data plane installed.
	//   - Auto is deliberately NOT a per-node probe that promotes itself to a faster fabric: a member
	//     group renders one DaemonSet, whose single Pod template cannot carry a different transport
	//     per node, and promoting to RDMA grants hostNetwork and two capabilities — a privilege is
	//     requested, never inferred on an operator's behalf.
	//   - Membership in this enum means MEASURED AS COMPILED into an artifact a member can run, which
	//     is what excludes the other eight strings that artifact's config parser accepts. It does not
	//     mean measured to move bytes: only TCP has been exercised end to end. It also does not mean
	//     this project publishes an image carrying it — MUSA and MACA are deliberately in the enum
	//     with no variant in pack/mirrored-mooncake, so a group on either names its own image. A
	//     value here with neither a project variant nor a working self-built image is what the rule
	//     excludes; an absent variant on its own is not.
	//   - A host fabric needs two things this API cannot check: the member image must carry the
	//     runtime its transport links — the CANN toolkit for CANN, libfabric for EFA — and the NODE must run a
	//     device plugin, since a hostPath alone leaves the device cgroup refusing to open the device.
	//     Which resource the member asks for is deviceResourceName below.
	//   - RESPELLING THIS ENUM CARRIES A RESIDUAL RISK, knowingly accepted, on the same terms as
	//     Medium's. The values were once Auto, TCP, RDMA, EFA, HIP and Ascend; HIP and Ascend are
	//     gone, replaced by the toolchain names ROCM and CANN, and the rest changed case. An object
	//     storing one of the old values becomes undeletable, because schema validation runs on the
	//     write path only: it reads back fine while every update is refused, the controller's
	//     finalizer removal included. NO RELEASE IS EXPOSED — this type is absent from every tag
	//     through v0.8.6, checked per tag — but a cluster tracking the default branch is, since that
	//     branch carried the old spellings. Clearing it is the first shipping release's job: confirm
	//     no leftover object exists, or write a recovery procedure. A conversion webhook is NOT the
	//     answer here for the reason an alias map is not: the schema enum is the gate a stored object
	//     meets first, so widening what admission accepts reaches nothing that the API server has
	//     already refused.
	//
	// +k8s:validation:default="Auto"
	// +k8s:validation:enum=["Auto","TCP","RDMA","EFA","CANN","ROCM","MUSA","MACA"]
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,1,opt,name=protocol"`

	// DeviceResourceName is the extended resource a host-fabric member asks one of, so the device
	// cgroup lets it open the fabric device. It is CONSULTED ONLY on the RDMA and EFA protocols;
	// beside any other it renders nothing.
	//
	//   - It is DECLARED rather than derived: the name belongs to whichever plugin the cluster's
	//     administrator installed, so no name hard-coded here would be right on two clusters, and no
	//     admission rule can check a node for a plugin whose resource it cannot know.
	//   - EFA is the exception. Its plugin advertises exactly one name, so an EFA member asks for
	//     vpc.amazonaws.com/efa when this is unset. That is a default rather than a property of the
	//     protocol, and setting the field overrides it.
	//   - UNSET IS NOT A SAFE DEFAULT, IT IS THE OLD BEHAVIOR. A fabric member naming no resource
	//     mounts the device tree and requests nothing, so the cgroup refuses the open, the store
	//     installs the store's tcp, and the object still reads as the fabric it asked for. Naming one instead
	//     keeps the member off a node that advertises none, which is the safer failure but not
	//     always the wanted one, so both stay reachable.
	//
	// The bounds below are the API server's own for a resource name: 63 characters after the slash
	// and for each domain label, refused here rather than on the DaemonSet rendered from it, where
	// they strand reconciliation with no obvious cause. The domain's 253-character limit is NOT among
	// them — no regular expression can bound a repeated group whose labels vary in length, so
	// 63.63.63.62 makes a domain of 254 that the 317 below still admits — and admission carries that
	// one instead, so it is absent when the webhook is not installed.
	//
	// +k8s:validation:pattern="^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*/[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$"
	// +k8s:validation:maxLength=317
	DeviceResourceName string `json:"deviceResourceName,omitempty" protobuf:"bytes,2,opt,name=deviceResourceName"`
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

	// Medium is what the SEGMENT this member group mounts is made of: host memory (DRAM) or
	// device memory (VRAM).
	//
	// It is a choice rather than an identity: the renderer splits on it. A DRAM member charges
	// CapacityPerMember against the Pod's host memory; a VRAM member charges it against nothing,
	// because its segment is device memory and claiming it is allocating it. What lets a VRAM member
	// reach its device is declared and never inferred — SecurityContext, HostPaths and
	// RuntimeClassName below, each on its own. The field stays immutable — a segment already mounted
	// cannot change kind underneath the data in it — so the choice is made when the group is
	// declared.
	//
	//   - A local disk, NVMe-oF, a DAX device and a distributed filesystem are NOT member groups, and
	//     each is reached elsewhere: the first through localDisks below, NVMe-oF as a target
	//     coordinate with no Pod, and the last two on the leader's own process.
	//   - Narrowing the enum carries a RESIDUAL RISK, knowingly accepted. An object created with one
	//     of those values, while this CRD was installed but the webhook was not, becomes undeletable:
	//     schema validation runs on the write path only, so it reads back fine while every update is
	//     refused, the controller's finalizer removal included. The exposure is development clusters
	//     only, this type being absent from every tag through v0.8.6, so clearing it is the first
	//     shipping release's job — confirm no leftover object exists, or write a recovery procedure.
	//
	// +required
	// +k8s:validation:enum=["DRAM","VRAM"]
	Medium string `json:"medium" protobuf:"bytes,2,name=medium"`

	// CapacityPerMember sizes ONE member, not one node. It becomes the member's global segment size
	// and is counted into the member Pod's own resource request, so a member that does not fit stays
	// Pending instead of overcommitting the node.
	//
	//   - A group declaring a tier in LocalDisks needs at least one BUCKET here, which is the unit
	//     that tier is written in. A bucket's bytes are held in this segment until the bucket is
	//     complete, so a smaller segment never holds a bucket's worth at once and the tier stays
	//     empty under every workload, which nothing else reports. A group with no tier has no such
	//     floor.
	//   - The name is "per member" for a shape that is DECIDED AND NOT DONE: several members per
	//     node, split by NUMA domain. Today one selected node runs one member.
	//   - What would reopen that is a two-socket node reporting RDMA interfaces on more than one NUMA
	//     node AND that node's member observed transferring across the socket boundary, there being
	//     nothing else on this path that consumes the NUMA affinity this operator already discovers.
	//     One group per NUMA domain is not the shape it would take — a group selects nodes through
	//     nodeSelector, while NUMA is a property inside a node rather than a label on one.
	//
	// +required
	CapacityPerMember resource.Quantity `json:"capacityPerMember" protobuf:"bytes,3,name=capacityPerMember"`

	// LocalBufferSize is the member client's local staging buffer, counted into the Pod's
	// memory request beside CapacityPerMember.
	LocalBufferSize resource.Quantity `json:"localBufferSize,omitempty" protobuf:"bytes,4,opt,name=localBufferSize"`

	// ExtraArgs passes config keys this API does not enumerate straight through to the member. An
	// entry is written as its own flag token, "-key=value", and renders as the entrypoint's own
	// "-D key=value" override with the dashes gone — one namespace per side, each the one its own
	// binary documents. A key that collides with one derived from a field above is refused at
	// admission.
	//
	// EVERY VALUE HERE IS WORLD-READABLE: stored verbatim on this cluster-scoped object, then
	// rendered into the member container's argv, readable by anyone who can reach the Pod or the
	// DaemonSet, for the life of the object. A credential does not belong here, and since this
	// operator renders no flag that carries one, this field is the only way one arrives.
	//
	// +listType=atomic
	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// ExtraEnv passes environment variables this API does not enumerate straight through to the
	// member container.
	//
	//   - It is NOT a second spelling of ExtraArgs: the two reach different places. ExtraArgs renders
	//     as the entrypoint's "-D key=value" config override, while a whole family of this store's
	//     settings — the local disk tier's flush thresholds, its promotion behavior, the rest of its
	//     eviction knobs — has no config key at all and is read from the ENVIRONMENT only.
	//   - A name this operator already renders is REFUSED at admission, for the same reason a
	//     colliding ExtraArgs key is: Kubernetes accepts a container carrying one name twice and
	//     leaves the winner to the runtime, so the collision would not even be reported. That
	//     includes the tier's bucket thresholds, which this operator sizes itself; a tuner who needs
	//     to move them needs a field, and this hatch is deliberately not it.
	//
	// The list is keyed by name, and the schema refuses two entries sharing one.
	//
	// EVERY VALUE HERE IS WORLD-READABLE: stored verbatim on this cluster-scoped object, then
	// rendered into the member container's environment, readable by anyone who can reach the Pod or
	// the DaemonSet, for the life of the object. A credential does not belong here, and since this
	// operator renders no variable that carries one, this field is the only way one arrives.
	//
	// +listType=map
	// +listMapKey=name
	ExtraEnv []InstanceEnvVar `json:"extraEnv,omitempty" protobuf:"bytes,6,rep,name=extraEnv"`

	// Image overrides the backend's Image for this member group only. Left unset, the group runs
	// the backend's Image.
	//
	// A group's NodeSelector is what makes this necessary: two groups can select nodes of different
	// accelerator vendors or generations, and the store's client ships as one wheel per vendor, each
	// carrying the transports it was compiled with and the runtime it links. The vendor runtime is
	// also the medium's: a VRAM group needs a build with VRAM segments compiled in, which the stock
	// CPU default is not.
	//
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,7,opt,name=image"`

	// LocalDisks declares the local disk tiers this group's nodes contribute, one entry per host
	// directory, and an empty list leaves the group memory only. Declaring an entry is what turns
	// the tier on: the store's leader takes the switch from this list's presence, not from any
	// field of its own.
	//
	//   - What the tier is written in is a BUCKET, and that is why this operator sizes one. The store
	//     writes nothing until a bucket is full, by bytes or by object count, so under the store's
	//     own thresholds — sized for a saturated production store — the tier stays empty while every
	//     other signal looks healthy. The pair this operator renders instead is not in this API, and
	//     members[].extraEnv refuses those names.
	//   - It is a LAYER on this group rather than a group of its own, which is the store's shape: the
	//     leader routes an offload task to the client that owns the key's memory replica, so a member
	//     holding no memory segment is never chosen and would report a cold tier that never fills.
	//   - To check what the tier actually holds rather than what it declared, read the leader's own
	//     master_allocated_file_size_bytes; status.capacity reports the declared CAPACITY only.
	//   - At most ONE entry, and the bound is the status contract rather than any one renderer's
	//     reach: status.capacity is a single pair of figures for the whole backend and cannot
	//     attribute a tier's bytes to one disk, so two entries would describe neither. The same
	//     sentence, at the group level, is why admission allows only one group to carry a list at
	//     all — lift these together with that status shape or not at all.
	//
	// The list is keyed by path, and the schema refuses two entries naming one directory.
	//
	// +k8s:validation:maxItems=1
	// +listType=map
	// +listMapKey=path
	LocalDisks []KVCacheBackendMemberLocalDisk `json:"localDisks,omitempty" protobuf:"bytes,8,rep,name=localDisks"`

	// Transport declares the data plane this group uses, overriding the backend's
	// spec.transport.protocol for this group only. Left unset, the group inherits the backend's.
	//
	// The override exists for the one thing two media do not agree on: a VRAM group reaching its
	// peers over a fabric while the DRAM group beside it stays on TCP. Everything else about the
	// fabric — the device a host-fabric member asks for — stays backend-wide, since it describes
	// the nodes' fabric rather than one group.
	Transport *KVCacheBackendMemberTransport `json:"transport,omitempty" protobuf:"bytes,9,opt,name=transport"`

	// There is NO per-group device-resource field. A member's segment is one cudaMalloc on one
	// device — measured upstream, the splits exist to stay under a transport's registration limit
	// and nothing on that path selects a device — so asking the scheduler for one of an extended
	// resource would take a whole accelerator away from inference to account for a fraction of one
	// device's memory, on a node where the member cannot use the rest of what it took. A group sits
	// alongside the engine on a device instead, sized against that engine's own memory fraction.

	// SecurityContext is the member container's security context, merged ONTO the one the renderer
	// derives from the group's effective protocol rather than replacing it.
	//
	// The merge is per field: a field set here wins, a field left unset keeps whatever the renderer
	// put there, and capabilities.add is the UNION of both sides. The union is the part worth
	// stating, because the alternative is silent: a host-fabric group needs IPC_LOCK to pin the
	// memory it registers and SYS_RESOURCE to raise the limit that pinning hits, and replacing this
	// value whole would drop both while leaving a container that starts, runs, and fails only at
	// registration. Dropping one of the two is therefore not something this field can express; a
	// group that must not hold them declares a protocol that does not ask for them.
	//
	// THIS IS ROOT ON THE NODE, and deliberately so: Privileged, or a RunAsUser of zero paired with
	// a HostPaths entry, gives the member container what a process on the node has. The grant is
	// not an escalation of who can make it — this object is cluster-scoped precisely because it is
	// a privileged physical resource, so whoever can write one already holds the cluster. It is
	// written here rather than inferred so that reading the object tells you what was granted.
	SecurityContext *core.SecurityContext `json:"securityContext,omitempty" protobuf:"bytes,10,opt,name=securityContext"`

	// HostPaths mounts directories or files from the selected nodes into the member container.
	//
	// It exists because a vendor's USER-SPACE DRIVER is not in the image and is not under /dev, so
	// no device grant reaches it: an Ascend member needs the driver tree and the DCMI library from
	// the node, and a container runtime that injects them is the other way to get there. Privileged
	// alone does NOT cover this — it opens the node's device tree, which is where the device nodes
	// are and is not where the libraries are.
	//
	// Entries are mounted in the order written. The volume backing each one is named from its
	// POSITION rather than from anything declared here, so an entry can collide with neither
	// another entry nor a volume the renderer owns.
	//
	// LocalDisks above is not this field spelled differently: that list declares tier capacity the
	// leader routes offload tasks to, with a deregistration hook and a grace period derived from
	// its presence. A directory mounted here is a mount and nothing more.
	//
	// +k8s:validation:maxItems=32
	HostPaths []KVCacheBackendMemberHostPath `json:"hostPaths,omitempty" protobuf:"bytes,11,rep,name=hostPaths"`

	// RuntimeClassName selects the container runtime the member's Pods run under, which is how a
	// vendor runtime injects its driver libraries and device nodes without any of them being named
	// here.
	//
	// It is DECLARED rather than looked up from the group's hardware, unlike the equivalent on a
	// model deployment, and the reason is that a member group has no InstanceType to ask: it selects
	// nodes by label, and a label does not carry a manufacturer this operator can map. A cluster
	// whose vendor runtime is the default runtime needs nothing here.
	//
	// A name no RuntimeClass on the cluster carries makes the API server REJECT the Pod outright,
	// so the member group stops at admission of its own Pods rather than starting without the
	// runtime. That is the loud failure, and it is the one wanted here.
	//
	// +k8s:validation:pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	// +k8s:validation:maxLength=253
	RuntimeClassName string `json:"runtimeClassName,omitempty" protobuf:"bytes,12,opt,name=runtimeClassName"`
}

// KVCacheBackendMemberHostPath is one directory or file taken from a selected node into the member
// container.
//
// Only a host path, and none of the other volume sources: what a member group needs from outside
// its image is the node's own driver tree and device nodes. A ConfigMap, a Secret or a claim has no
// use here that is known, and a source added without one is a validation surface nobody exercises.
type KVCacheBackendMemberHostPath struct {
	// Path is the absolute path on the node.
	//
	// +required
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	Path string `json:"path" protobuf:"bytes,1,name=path"`

	// MountPath is the absolute path inside the member container. It must duplicate neither another
	// entry's mount path nor one the renderer owns.
	//
	// +required
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	MountPath string `json:"mountPath" protobuf:"bytes,2,name=mountPath"`

	// Type is the kubelet's host-path type check, applied before the mount.
	//
	// Left unset it is the EMPTY type, for which the kubelet's mounter returns immediately and looks
	// at the path not at all — so a missing path becomes an empty directory in the container and the
	// member starts anyway. Naming a type is what turns that into a FailedMount the Pod stops at.
	//
	// +k8s:validation:enum=["","DirectoryOrCreate","Directory","FileOrCreate","File","Socket","CharDevice","BlockDevice"]
	Type *core.HostPathType `json:"type,omitempty" protobuf:"bytes,3,opt,name=type"`

	// ReadOnly mounts it read-only. A driver tree is read by the member and written by nobody, so
	// this is the right setting for one, and it is not the default because a device node under /dev
	// is the other thing mounted here and that one is written.
	ReadOnly bool `json:"readOnly,omitempty" protobuf:"varint,4,opt,name=readOnly"`
}

// KVCacheBackendMemberTransport is a member group's override of the backend's data plane. It
// carries a protocol only: the device a host-fabric member asks for describes the nodes' fabric
// rather than one group, so it stays on the backend.
type KVCacheBackendMemberTransport struct {
	// Protocol is the transport this group's members are ASKED to use, with the same values and
	// the same Auto-resolves-to-TCP rule as the backend's spec.transport.protocol, which this
	// field replaces for this group when set — including the residual risk that field records for
	// the respelling, which applies to a stored value here the same way.
	//
	// +k8s:validation:default="Auto"
	// +k8s:validation:enum=["Auto","TCP","RDMA","EFA","CANN","ROCM","MUSA","MACA"]
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,1,opt,name=protocol"`
}

// KVCacheBackendMemberLocalDisk is one local disk tier a member group's nodes contribute.
//
// An entry in a group's list is what turns the tier on, on both sides at once: the leader renders
// the flags that send evicted keys to disk from the list's presence alone, and it renders them in
// the deferred mode — a key's write lands at eviction time and never at storage time — because
// that is the only mode the store protects. The other mode has no field and none is wanted: it
// evicts an unflushed bucket's sole replica, so there is nothing to pair this declaration with and
// nothing to forget.
type KVCacheBackendMemberLocalDisk struct {
	// Path is the directory on each selected node that holds this tier, mounted into the member
	// container from the host at the same location. It is REQUIRED and has no default: choosing a
	// host directory on somebody else's nodes is not a default this operator may pick, because the
	// wrong one fills a filesystem that nothing in Kubernetes accounts for.
	//
	//   - Declaring a tier REQUIRES a shell in the group's image. An init container surveys this
	//     directory before the member starts, so that reusing a path is something an administrator is
	//     told rather than discovers through a key that reads back as somebody else's. It runs
	//     `sh -c`, and an image without a shell keeps the member from starting at all.
	//   - Creating this directory and giving it the right owner is YOURS, not this operator's, and a
	//     member whose container cannot write it fails at start. The omission is deliberate: an init
	//     container that chowns has to name a uid, while members[].image can put a different vendor's
	//     build on each group, and a chmod 0777 instead opens the directory to every process on the
	//     node. An operator whose uid holds for a whole backend has what would settle it.
	//
	// +required
	// +k8s:validation:maxLength=4096
	Path string `json:"path" protobuf:"bytes,1,name=path"`

	// Capacity caps what this tier stores, in bytes. Left unset, the store's own ceiling applies and
	// nothing is rendered, so a ceiling that moves upstream is a change to investigate rather than
	// one this API silently restated.
	//
	//   - It is also the figure EVICTION measures against, which is why Eviction below is not usable
	//     without it: the store's watermark quota defaults to zero, which that path reads as "no
	//     quota" and returns from having evicted nothing. One value is rendered into both ceilings.
	//   - A set capacity must hold one BUCKET, the unit this tier is written in: the store stops
	//     taking offload work as soon as one more bucket would not fit, so a smaller tier never
	//     receives a key. The bucket size is this operator's to choose, so the floor moves with it.
	//   - It is NOT counted into the Pod's resource requests, unlike CapacityPerMember. The tier is a
	//     host directory, outside the kubelet's ephemeral-storage accounting entirely, so a request
	//     against it would reserve a figure nothing polices and would then keep the member off the
	//     very node that has the disk. Watching that filesystem is the operator's.
	Capacity resource.Quantity `json:"capacity,omitempty" protobuf:"bytes,2,opt,name=capacity"`

	// KeyLimit caps how many keys this tier holds. It is Capacity's other HALF rather than an
	// alternative to it: the store bounds the tier by bytes AND by key count, stops taking offload
	// work when either would be exceeded, and applies its own ceiling to whichever this object
	// leaves out. Left unset or zero, nothing is rendered, on the same rule as Capacity. It carries
	// the same bucket floor and for the same reason — the store checks against one whole bucket's
	// worth of keys, so a limit below that is a tier that can never receive one.
	//
	// +k8s:validation:minimum=0
	KeyLimit int64 `json:"keyLimit,omitempty" protobuf:"varint,3,opt,name=keyLimit"`

	// Eviction is what this tier does once it is full. Left unset, nothing is rendered and the
	// store's own behavior applies, so a default that moves upstream is a change to investigate
	// rather than one this API silently restated.
	Eviction *KVCacheBackendMemberLocalDiskEviction `json:"eviction,omitempty" protobuf:"bytes,4,opt,name=eviction"`

	// CleanAfterDelete asks this operator to empty Path when the backend is deleted, on every node
	// this group's NodeSelector picks AT THAT MOMENT. It DEFAULTS TO FALSE, and left alone the
	// directory keeps whatever it holds.
	//
	// It is a switch rather than a default because what is on that disk is the administrator's, and
	// deleting it is not a decision this operator may take on their behalf. That is also why it is
	// reachable where preparing the directory is not: removing content needs no uid, creating does.
	//
	//   - "At that moment" is the whole of the promise. Nothing stores the selector's history and the
	//     members are gone by the time cleanup runs, so narrowing NodeSelector or removing the
	//     LocalDisks entry before deleting the backend leaves the dropped nodes holding their content
	//     with nothing reported about them. Delete the backend first and edit afterwards.
	//   - WHAT IS REMOVED IS THE CONTENT, NOT THE DIRECTORY, which was made by whoever prepared the
	//     node, may be a mount point, and carries an owner this operator did not choose.
	//   - The cleanup runs the image THIS GROUP runs, on the nodes it selects, with the backend's
	//     imagePullSecrets, so it does not wait on a pull the members already did.
	//   - A node this operator cannot reach in time keeps its content, and deletion is not held open
	//     for it: a finalizer waiting on a node that is gone leaves an object nobody can delete. The
	//     node gets a warning Event naming what was left.
	//   - A node where another KVCacheBackend declares an overlapping path is SKIPPED, with the same
	//     kind of Event. Nothing refuses two backends naming one directory, and emptying it for this
	//     one would take the other one's live data with it.
	CleanAfterDelete bool `json:"cleanAfterDelete,omitempty" protobuf:"varint,5,opt,name=cleanAfterDelete"`
}

// KVCacheBackendMemberLocalDiskEviction is what the tier does once it is full: drop what it already
// holds to make room, or stop taking new work.
//
// The two are different OUTCOMES rather than two settings of one knob: evicting, the tier goes on
// accepting writes indefinitely and its oldest content leaves; not evicting, the tier fills to its
// Capacity and the store stops sending it work, so what is already there stays readable.
type KVCacheBackendMemberLocalDiskEviction struct {
	// Enabled is whether this tier evicts at all. It DEFAULTS TO TRUE, so declaring this block
	// without it asks for eviction rather than against it.
	//
	// OMITTING THIS KEY AND WRITING `enabled: false` ARE DIFFERENT, and the default is what makes
	// them different: only the explicit false turns eviction off. Without that, declaring this block
	// merely to set a Watermark would stop the tier evicting for everyone who did so.
	//
	// Turning it off renders TWO settings, not one: an eviction policy of "none", the store's own
	// name for that value, and an explicit false on its watermark-eviction switch. They belong to
	// different layers, and eviction should be off at whichever layer ends up asking.
	//
	// +k8s:validation:default=true
	Enabled *bool `json:"enabled,omitempty" protobuf:"varint,1,opt,name=enabled"`

	// Policy is the order in which entries leave. FIFO drops the oldest written first, LRU the least
	// recently read. It is REFUSED together with Enabled set to false, because there is no order in
	// which nothing leaves.
	//
	//   - The enum is the two any cache would offer, deliberately, rather than every string the
	//     store's parser happens to read. It carries NO value meaning "do not evict": that is
	//     Enabled's job, and a third value saying the same thing would be a second spelling
	//     admission would then have to adjudicate against the first.
	//   - Left unset NOTHING IS RENDERED and the store's own default applies, which is first-in
	//     first-out. That earns more here than usual: the store maps a policy string it does not
	//     recognize onto no eviction at all — no error, no warning, no failure to start — so a policy
	//     is only ever sent when this API is the one that chose it, from a fixed set of spellings.
	//
	// +k8s:validation:enum=["FIFO","LRU"]
	Policy string `json:"policy,omitempty" protobuf:"bytes,2,opt,name=policy"`

	// Watermark is WHEN eviction runs: it starts once the tier passes High and stops once it is back
	// under Low, both as a percentage of Capacity. Left unset, nothing is rendered and the store's
	// own marks apply.
	//
	// It REQUIRES Capacity, which is what the percentages are of, and it is refused together with
	// Enabled set to false.
	Watermark *KVCacheBackendMemberLocalDiskEvictionWatermark `json:"watermark,omitempty" protobuf:"bytes,3,opt,name=watermark"`
}

// KVCacheBackendMemberLocalDiskEvictionWatermark is the band eviction works between.
//
// BOTH MARKS ARE GIVEN TOGETHER, rather than one at a time on the block above, because either mark
// alone describes nothing this operator would want to render: a high mark on its own leaves the
// store pairing it with a low mark this object never states, and the member refuses that pair at
// startup whenever the unstated default is not below it, for a reason that appears only in a
// container log.
type KVCacheBackendMemberLocalDiskEvictionWatermark struct {
	// High is the percentage of Capacity at which eviction starts. A PERCENTAGE and not a quantity:
	// the store takes a fraction of its own quota rather than a size, and a size here would restate
	// a figure Capacity already carries — one that would quietly stop matching the moment Capacity
	// moved.
	//
	// +required
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=100
	High int32 `json:"high" protobuf:"varint,1,name=high"`

	// Low is the percentage of Capacity eviction stops at, and it MUST be below High. Equal marks
	// would make every write past the mark evict, which is the thrashing a band exists to prevent.
	// The store refuses the pair when its member starts; admission refuses it here instead, where
	// the message reaches whoever wrote it.
	//
	// +required
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=100
	Low int32 `json:"low" protobuf:"varint,2,name=low"`
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
	// CapacityObserved, Deletable, RolloutComplete, and — each only where it has something to be a
	// verdict about — SnapshotStorageShared and ElectionObserved. Every one is derived from an
	// observed document.
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
	// succeeds, and absent again is not the same as reporting zero: an empty object here would be
	// indistinguishable from a scrape that returned nothing.
	Capacity *KVCacheBackendCapacity `json:"capacity,omitempty" protobuf:"bytes,5,opt,name=capacity"`

	// Members is one entry per observed store member.
	//
	// +listType=map
	// +listMapKey=segmentID
	Members []KVCacheBackendMemberStatus `json:"members,omitempty" protobuf:"bytes,6,rep,name=members"`

	// UsedBy names the objects that consume this backend. A non-empty UsedBy is what the
	// finalizer refuses deletion on, so the field is the enforcement input and not a display.
	//
	// It is written by the CONSUMERS, not by this backend's own reconciler, which only reads it and
	// holds its teardown on it. Today exactly one consumer writes here: a KVCachePool claims the
	// backend it draws from, under kind KVCachePool, and drops the claim only after removing what it
	// registered on that backend's master. Entries leave Namespace empty, a backend being
	// cluster-scoped and so is everything that claims one. KVCacheObjectReference's own doc says why
	// the shape is neither of the two core reference types.
	//
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	UsedBy []KVCacheObjectReference `json:"usedBy,omitempty" protobuf:"bytes,7,rep,name=usedBy"`
}

// KVCacheBackendCapacity is the backend's capacity AS THE LEADER REPORTS IT. Both figures are
// ABSENT when the scrape failed: a zero here would read as an empty cache, and a retained previous
// value would read as a current one.
type KVCacheBackendCapacity struct {
	Total *resource.Quantity `json:"total,omitempty" protobuf:"bytes,1,opt,name=total"`
	Used  *resource.Quantity `json:"used,omitempty" protobuf:"bytes,2,opt,name=used"`
}

// KVCacheBackendMemberStatus is one observed store member.
type KVCacheBackendMemberStatus struct {
	// SegmentID is the segment's unique identifier as the leader reports it.
	//
	// +required
	SegmentID string `json:"segmentID" protobuf:"bytes,6,name=segmentID"`

	// ClientID is the identifier the member process minted when it started. Unlike the advertised
	// address, it remains distinct when several host-network members run on one node.
	//
	// +required
	ClientID string `json:"clientID" protobuf:"bytes,7,name=clientID"`

	// SegmentName is the member's advertised address as the leader reports it. It is not unique:
	// host-network members placed on one node advertise the same address.
	//
	// +required
	SegmentName string `json:"segmentName" protobuf:"bytes,1,name=segmentName"`

	// NodeName is the node contributing this member's medium.
	NodeName string `json:"nodeName,omitempty" protobuf:"bytes,2,opt,name=nodeName"`

	// Medium is what this member contributes, echoed from the group that selected its node.
	Medium string `json:"medium,omitempty" protobuf:"bytes,3,opt,name=medium"`

	// Protocol is the transport this member REGISTERED with when it mounted its segment.
	//
	// It is NOT an observation. The value travels from the member's own mount request through the
	// leader's listing unchanged, so it cannot disagree with spec.transport.protocol: a member that
	// asks for a host fabric and comes up on TCP because the device is missing still reports the
	// fabric here, while serving. Agreement with the spec is therefore not confirmation that the
	// request took effect; the transport the data plane installed is only in the member's own log.
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,4,opt,name=protocol"`

	// State is the member's state AS THE LEADER REPORTS IT, read from the leader's own segment
	// listing rather than inferred from the member Pod. The states the store defines, in this API's
	// casing: OK, Draining, Drained, GracefullyUnmounting, Unmounting, Undefined.
	//
	//   - Draining and the two unmounting states are what a shrink passes through, so the field can
	//     distinguish a member on its way out from one that is simply gone. That is the whole reason
	//     it carries the store's vocabulary instead of a summary of it.
	//   - It carries no "unreached" sentinel, because there is no pass that would write one: a
	//     listing that cannot be read leaves the PREVIOUS entries in place and says so through
	//     MembersMounted, rather than rewriting them as blank. Whether what is here was just
	//     refreshed is that condition's question, and this field never answers it.
	//   - It carries no enum marker, deliberately, unlike every enum on the spec side. The value's
	//     domain belongs to the store: a store version that adds a state would make the whole status
	//     write fail validation — not this one field, the entire object — leaving every other status
	//     field frozen at its last value. Phase, further up, is open for the same reason.
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
