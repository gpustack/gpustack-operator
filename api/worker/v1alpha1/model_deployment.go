package v1alpha1

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// ModelDeployment is the schema for worker.gpustack.ai.
//
// It is N replicas of one inference-engine role attached to a KV cache pool, so that the replicas
// hit each other's cached prefixes instead of each re-computing the same prefill.
//
// It RENDERS PODS DIRECTLY. The admission chain keys on Pods, so rendering Pods reuses every
// existing gate with no new integration point.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["md"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Engine",type="string",jsonPath=".spec.engine.name"
// +k8s:crd-gen:printcolumn:name="Roles",type="string",jsonPath=".status.roleSummary"
// +k8s:crd-gen:printcolumn:name="Phase",type="string",jsonPath=".status.phase"
// +k8s:crd-gen:printcolumn:name="Endpoint",type="string",jsonPath=".status.endpoint"
type ModelDeployment struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelDeploymentSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelDeploymentStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelDeployment)(nil)

// ModelDeploymentSpec defines the desired state of ModelDeployment.
type ModelDeploymentSpec struct {
	// Model names what the engine serves.
	//
	// +required
	Model ModelDeploymentModel `json:"model" protobuf:"bytes,1,name=model"`

	// Engine is the inference engine this deployment runs, which decides which argument keys the
	// operator owns and which carrier the transfer configuration arrives on. Ownership is per
	// (engine, key): a key one engine owns is an ordinary user argument on another.
	//
	// It does NOT decide the connector, which follows the role's hardware instead: the connector is
	// a property of the accelerator backend, so an Ascend pool and an NVIDIA pool running this
	// engine get different ones.
	//
	// +required
	Engine ModelDeploymentEngine `json:"engine" protobuf:"bytes,2,name=engine"`

	// KVCache optionally attaches the deployment to a shared KV cache pool. A managed vLLM
	// prefill/decode deployment without it still uses its router's point-to-point connector; it does
	// not render the shared-store connector or its client configuration.
	//
	// +optional
	KVCache *ModelDeploymentKVCache `json:"kvCache,omitempty" protobuf:"bytes,3,opt,name=kvCache"`

	// Roles are the engine roles this deployment runs. A single-role deployment names one; a
	// prefill/decode deployment names several, which is why this is a LIST FROM THE FIRST VERSION.
	//
	// The UPPER bound lives in the validating webhook and not in this schema: it tracks Kueue's cap
	// on Workload.spec.podSets, so the refusal can name whose limit it is, and following that number
	// is a webhook edit rather than a schema change every stored object must survive. The figure is
	// not restated here, because the one that binds is in the Kueue the cluster runs.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentRole `json:"roles" protobuf:"bytes,4,rep,name=roles"`

	// Router optionally puts a request router in front of the roles.
	//
	// IT IS EAST-WEST TRAFFIC MANAGEMENT, NOT A PREFILL/DECODE PAIRER, and the distinction decides
	// which shapes are legal behind it. Several plain servers is one of them: a router that scores on
	// a cache view picks between equals in a way a Service cannot, so "there is no pair here" is not a
	// reason to refuse one. Several prefillers with several decoders is another. A rule admitting only
	// one prefiller and one decoder would describe a pairer rather than this field.
	//
	// ON ASCEND HARDWARE THE PREFILL/DECODE TRANSFER LEG RENDERS BEHIND ONE ROUTER ALONE:
	// "llm-d-router", whose decode proxy relays the per-request handshake the Ascend connector waits
	// for. "vllm-router" drives a pair in a handshake vocabulary that connector rejects, so an Ascend
	// pair behind it renders as two complete engines with no leg between them; "sglang-gateway" is
	// refused in front of this engine before any of that applies. The no-leg shape is QUIET: the
	// deployment goes Ready, requests answer normally, and the two roles never exchange a block --
	// a correctly answering deployment is exactly what makes the missing leg hard to see.
	//
	// Absent means no router, and that stays a supported shape rather than a broken one: the roles are
	// individually addressable through their own Services either way, so a deployment written before
	// this field existed serves exactly as it did.
	//
	// +optional
	Router *ModelDeploymentRouter `json:"router,omitempty" protobuf:"bytes,5,opt,name=router"`

	// KVTransfer tunes the engine-to-engine KV transfer leg of a managed prefill/decode
	// pair.
	//
	// THIS FIELD AND KVCache ABOVE ARE TWO ORTHOGONAL AXES, NOT TWO BRANCHES OF ONE CHOICE, and
	// both may be set at once. The gate that turns this leg on — every admitted router-and-engine
	// pair (on Ascend hardware, "llm-d-router" alone; Router above names each combination and what
	// the quiet no-leg shape looks like) and a role kind of prefill or decode — reads none of
	// spec.kvCache, and when both are set the two are synthesized into ONE connector and one
	// --kv-transfer-config: a deployment may share a pool for its blocks AND hand them from prefill
	// to decode directly, at the same time.
	//
	// THE LEG THIS COVERS NEVER TRAVERSES THE STORE, and that is why the value does not come from
	// the KVCacheBackend: spec.transport there defines the data plane the store MEMBERS run, this
	// one is engine to engine, and the two planes declare separately. A deployment can render
	// this leg with no pool attached at all, which is another reason the field cannot live under
	// KVCache.
	//
	// +optional
	KVTransfer *ModelDeploymentKVTransfer `json:"kvTransfer,omitempty" protobuf:"bytes,6,opt,name=kvTransfer"`
}

// The engines a ModelDeployment can run, which are the values of ModelDeploymentEngine.Name's enum.
//
// They are declared beside the field whose schema closes the set, so that a reader of either finds
// the other. There is deliberately no "vllm-ascend": `vllm_ascend` is a Python package the runner
// installs when the accelerator backend is CANN, not an engine a user picks, and naming it here made
// the connector look like a property of the engine, which it is not.
const (
	ModelDeploymentEngineVLLM   = "vLLM"
	ModelDeploymentEngineSGLang = "SGLang"
)

// ModelDeploymentModel names the model the engine serves and, optionally, the weights it serves.
//
// It provisions nothing, and that has not changed: ArtifactRef REFERENCES weights that a
// ModelArtifact provisions, the way spec.kvCache.poolRef references a pool a Binding grants. The
// prohibition this type always carried stands: a source, a URI, a credential or a download policy
// never enters this object, because a weight-provisioning block here would be the first step
// towards the general-purpose serving CR this deliberately is not, and would make every deployment
// a credential holder. Without ArtifactRef, weights arrive through the role's additional volumes or
// through the engine's own hub client, as before.
type ModelDeploymentModel struct {
	// Name is the identifier the engine serves, e.g. "Qwen/Qwen2.5-72B-Instruct".
	//
	// It stays the SERVED NAME whether or not ArtifactRef is set: routers, the router's tokenizer
	// calls and the metrics' model label all match on it, so a managed role's own
	// --served-model-name must equal it, admission-enforced.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// ArtifactRef names a ModelArtifact IN THIS NAMESPACE holding the weights. The type is a
	// LocalObjectReference so that reaching another namespace is unrepresentable rather than
	// refused.
	//
	//   - It is FROZEN with the rest of this object: the weights a deployment serves are part of
	//     which deployment it is. Serving other weights means creating another deployment, which
	//     also keeps a prefill/decode pair from handing KV between two different weights.
	//   - An artifact that does not exist yet, or is not resolved, is ADMITTED: the deployment waits
	//     in status, creating no Pod, so a GitOps tool need not order the two objects.
	//   - With it, every managed role's engine gets the weights at a fixed local path (a claim) or
	//     the repository pinned to the resolved commit (a hub), and, with spec.kvCache, a weight
	//     identity in its store key prefix, so different weights never share KV blocks.
	//
	// +optional
	ArtifactRef *core.LocalObjectReference `json:"artifactRef,omitempty" protobuf:"bytes,2,opt,name=artifactRef"`
}

// ModelDeploymentEngine is the engine a deployment runs and the version of it.
//
// The two are one object because they were never meaningful apart: a role that names no image of
// its own has one assembled from the engine and the version together with the role's own
// InstanceType. The version carries no obligation of its own — it is owed exactly when some role
// needs that assembly, which admission rather than the schema decides, because which roles need it
// is a fact about the roles and not about this field.
type ModelDeploymentEngine struct {
	// Name selects the engine.
	//
	// It is the half of this object that is the deployment's identity and is frozen after creation,
	// while Version answers which build runs and stays editable — stated here because one object
	// reading otherwise would freeze the pair together.
	//
	// +required
	// +k8s:validation:enum=["vLLM","SGLang"]
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Version is the engine's own version, e.g. "0.29.0" for vllm or "0.5.18" for sglang.
	//
	//   - It is OPTIONAL, and the obligation sits with the roles instead: a role that names no image
	//     of its own has one synthesized from this version, so admission refuses an empty version
	//     beside such a role rather than letting the render assemble a malformed tag naming
	//     something never typed. A role that names an image never reads this field.
	//   - It is free-form and UNVALIDATED, by decision: the user guarantees that the version and the
	//     driver each role's hardware installed are aligned. A gate would need the runner's release
	//     matrix compiled into the operator, and the failure it would prevent is already legible as
	//     an ImagePullBackOff on a tag that does not exist.
	//   - It is per deployment rather than per role, which is what lets one version assemble a
	//     DIFFERENT image for each role: the backend half of the tag comes from the role's own
	//     InstanceType, so a prefill role on NVIDIA and a decode role on Ascend need no extra field.
	//     Published version sets do NOT overlap across every backend, so one version has to name a
	//     tag that exists for each backend the roles land on.
	//
	// +optional
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=64
	Version string `json:"version,omitempty" protobuf:"bytes,2,opt,name=version"`
}

// ModelDeploymentKVCache attaches the deployment to a KV cache pool.
//
// THE REUSE DOMAIN IS NOT DECLARED HERE, AND THAT IS A SECURITY PROPERTY. The storage layer's tenant
// IS the reuse domain, so every distinct domain is a tenant with its own quota ledger: a workload
// free to name arbitrary domains could mint unlimited tenants in its namespace and escape the
// namespace quota ceiling entirely. Domain naming therefore lives on the KVCachePoolBinding, which
// an admin owns.
type ModelDeploymentKVCache struct {
	// PoolRef names a KVCachePoolBinding IN THIS NAMESPACE. The Binding is the authorization point:
	// an admin creating one in a namespace is what grants that namespace access to the pool. The
	// type is a LocalObjectReference rather than a namespaced one so that reaching another
	// namespace — or naming the cluster-scoped pool, or a bare endpoint URL — is unrepresentable
	// rather than merely rejected.
	//
	// +required
	PoolRef core.LocalObjectReference `json:"poolRef" protobuf:"bytes,1,name=poolRef"`

	// Connector names the connector implementation this deployment is configured for. The value is
	// an identity the deployment carries, not a setting the operator derives: "Mooncake" says which
	// connector this is, and nothing reads the field to produce the configuration. There is no
	// "none" — synthesizing nothing is reachable through a full command replacement, which also
	// marks the role unmanaged and moves CacheAttached to Unknown.
	//
	// THE KV TRANSFER CONVERGES ON MOONCAKE, and that is why the enum has one value. Mooncake is the
	// implementation that supports heterogeneous prefill and decode, which is the shape this API
	// exists to express. NIXL and ROCm NIXL stay reachable; nothing here has run them, and no claim
	// that they would work is made by this field's existence.
	//
	// THE RESERVATION IS IN THE SCHEMA AND IN NOTHING ELSE. This field is read by no code: binding
	// resolution passes a domain, an endpoint and a protocol; connector synthesis takes an engine, a
	// kind, a manufacturer and that connection; and the renderer dispatches on the ENGINE. So the
	// discriminator is reserved for an API that names a second one, and the seam it would dispatch
	// through does not exist yet.
	//
	// WIDENING THE ENUM IS FOUR THINGS, NOT ONE: one sub-package under pkg/worker/kvcache, one entry
	// here, one renderer, AND the wiring that threads this value to a dispatch point. That last item
	// is what the reservation does not already cover, and it is the reason a second implementation is
	// a piece of work rather than a constant.
	//
	// A WIDENED ENUM REACHES NEW DEPLOYMENTS ONLY. This field answers which deployment this is, so it
	// is frozen after creation: an existing deployment is recreated onto a second connector rather
	// than edited onto one. That is stated here because "widening the enum" otherwise reads as a
	// migration path for deployments that are already running.
	//
	// +k8s:validation:default="Mooncake"
	// +k8s:validation:enum=["Mooncake"]
	Connector string `json:"connector,omitempty" protobuf:"bytes,2,opt,name=connector"`
}

// ModelDeploymentKVTransfer carries the settings of the point-to-point KV transfer leg
// between a prefill role and a decode role. It composes with a shared store rather than excluding
// one: both legs may be configured on one deployment, and the synthesized connector carries the
// pair together.
type ModelDeploymentKVTransfer struct {
	// Protocol is the transport both ends of the direct leg are told to use. It uses the
	// KVCacheBackend transport values and their Mooncake mapping, except MUSA and MACA:
	// those are intra-node IPC transports, while prefill and decode may run on different nodes.
	//
	//   - IT IS DEPLOYMENT-WIDE ON PURPOSE. The protocol is a property of the link, not of either
	//     end, so a per-role field could only express a contradiction -- two ends naming different
	//     values for one connection, which fails at transfer time rather than at admission.
	//   - THE VALUE IS DECLARED, NOT DISCOVERED. The enum names supported transport families;
	//     the engine image must still carry the matching Mooncake build. CANN renders as
	//     "ascend" and ROCM as "hip", using the same mapping as KVCacheBackend members.
	//   - Auto selects TCP and renders "tcp"; it does not inspect the worker's fabric.
	//   - UNSET RENDERS "tcp", the transport every Mooncake build carries. The default lives in
	//     the renderer rather than in this schema, so the stored object holds exactly what was
	//     asked.
	//   - TCP IS ENFORCED, NOT ONLY REQUESTED, because the transfer engine selects its transport
	//     from the host's hardware and does not read the requested one. On vLLM the leg also gets
	//     MC_FORCE_TCP=1, and a role's own value wins. On SGLang the value maps onto the engine's
	//     transfer backend: TCP renders "mooncake_tcp", any other value renders "mooncake", and
	//     the value itself is not passed through. Neither pin renders while the deployment's store
	//     runs a transport other than tcp, because it is process-wide and would leave the store
	//     client without its fabric; the leg then keeps the engine's own selection.
	//   - IT IS READ ONLY ON THE POINT-TO-POINT LEG: the prefill/decode roles of every admitted
	//     router-and-engine pair. Where no leg renders -- no router, or an Ascend pair behind
	//     "vllm-router" -- the value is accepted and renders nothing. An Ascend pair behind
	//     "llm-d-router" renders the leg but not this value: that engine's transfer leg hardcodes
	//     its transport, so the declared protocol has no key to land in. Each silence is stated
	//     here because an accepted field that quietly does nothing is a promise broken quietly.
	//   - IT IS EDITABLE, and an edit RESTARTS EVERY ROLE: the value renders into both ends'
	//     argv, so a change rebuilds every Kueue pod group of the deployment. With roles split
	//     across InstanceTypes the groups rebuild independently, and a mixed-protocol window
	//     between a prefiller and a decoder exists until both converge -- the same window an
	//     engine version edit already opens.
	//
	// +optional
	// +k8s:validation:enum=["Auto","TCP","RDMA","EFA","CANN","ROCM"]
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,1,opt,name=protocol"`
}

// ModelDeploymentRole is one engine role and its replicas.
//
// Replicas, InstanceType and Resources are STRUCTURED FIELDS AND MUST STAY SO. They are inputs to
// admission and scheduling — Kueue PodSet counts, flavor selection and the request the queue
// accounts — so a container field able to shadow any of them would make the admission feasibility
// check read a ledger that does not match reality. That is why the container fields below carry no
// resource request at all: the accelerator half belongs in Resources and the rest is derived from
// the InstanceType, and neither can be overridden here.
//
// EDITING A CONTAINER FIELD ROLLS THIS ROLE'S REPLICAS, and only this role's -- with one
// exception. The declared parallel degrees of a prefill or decode role -- the degree flags in
// ExtraArgs, and vLLM's VLLM_DP_SIZE environment entry -- are the one container field that can
// render into a document BOTH roles carry: the vLLM-Ascend transfer leg writes the same
// parallel blocks into both roles' Pods, so on that leg editing one role's degrees rewrites
// the other role's Pods too, and the edit rolls the pair. Where no shared document renders
// them, a degree edit stays this role's own like every other container-field edit: every
// replica is a Kueue pod group of its own, so they are replaced one at a time -- one per role
// per pass -- and every sibling role keeps serving throughout. A `replicas` change rolls
// nothing at all: it adds or removes instances, and every instance that stays keeps running,
// keeps the accelerators it was admitted with and keeps whatever cache it holds.
//
// THE SET OF ROLES IS FIXED AFTER CREATION. Admission refuses adding, removing or renaming a role.
// A replica's group is named from the deployment, its role and its ordinal, so edits to one role's
// running configuration do not rename a sibling role's groups.
//
// A DEPARTURE THIS OPERATOR DID NOT INITIATE IS NOT A ROLLOUT. The replica that left is replaced on
// its own, under a new name, while its siblings keep serving — see
// docs/modules/model-deployment/deployment.md under "One group per replica" and "Rollout is a rolling
// replacement".
type ModelDeploymentRole struct {
	// Name identifies the role, and it is also the name of the Kueue PodSet the role becomes.
	//
	//   - The pattern is Kueue's own PodSetReference shape, enforced here because the name is written
	//     verbatim into each Pod's role-hash annotation: Kueue groups a pod group's Pods into PodSets
	//     by that annotation, so a name it cannot take as a PodSet reference is a name whose role does
	//     not survive the grouping.
	//   - UNIQUENESS IS THE SCHEMA'S. Roles is a list-map keyed on this field, so the API server
	//     refuses a duplicate before any webhook runs; the webhook carries the same rule only as a
	//     backstop for that marker being dropped. Two roles sharing a name would collapse into one
	//     PodSet whose count is their sum, which is a silent merge rather than an error.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=63
	// +k8s:validation:pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Kind is what the engine is told this role is. It is CLOSED and it is NOT the role's name: Name
	// is free-form and identifies the PodSet, while this selects behavior, and a semantic reachable
	// by typing a string is one typo away from silently changing. Two roles may share a kind and
	// differ in name ONLY where that kind is Server, because a pair of servers is a set of equals and
	// two prefillers are not: nothing that consumes these roles expresses a second prefiller, so a
	// deployment declaring one would render a role no reader of the rendered configuration could
	// reach. It defaults to Server, the shape a deployment written before disaggregation existed has,
	// so such a deployment renders exactly as it did.
	//
	// +k8s:validation:default="Server"
	// +k8s:validation:enum=["Server","Prefill","Decode"]
	Kind ModelDeploymentRoleKind `json:"kind,omitempty" protobuf:"bytes,8,opt,name=kind,casttype=ModelDeploymentRoleKind"`

	// Replicas is how many independent serving instances this role runs. The instances are
	// independent: each one starts, serves and is replaced on its own, and none of them depends on
	// another being present.
	//
	// CHANGING THIS NUMBER ADDS OR REMOVES INSTANCES. Growing it creates new instances beside the
	// ones already running; shrinking it removes some of them. The instances that survive are not
	// restarted: they keep serving without interruption and keep whatever cache they hold.
	//
	// THE UPPER BOUND IS A LIMIT ON THIS OPERATOR, NOT ON KUBERNETES. A pass renders every instance
	// this role declares before it writes any of them, so the number is a multiplier on the work one
	// reconcile does; left open at the type's range, a single accepted field value is enough to
	// exhaust the worker before the API server ever throttles the creates. The bound is set where no
	// deployment anybody serves can reach it.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=1024
	Replicas int32 `json:"replicas,omitempty" protobuf:"varint,2,opt,name=replicas"`

	// ReplicaSize is how many Pods form ONE serving instance. Those Pods are fate-sharing: they
	// start together, they are replaced together, and none of them serves alone — the instance,
	// not the Pod, is the unit that appears and disappears.
	//
	// THIS NUMBER IS FIXED AT CREATION AND CANNOT BE CHANGED. An instance's size is the shape of the
	// instance, not a dial on it: the Pods a running instance is made of are not the Pods a different
	// size asks for. Scaling is what replicas is for, and it leaves every running instance alone. To
	// serve at a different size, create a deployment that declares it.
	//
	// ABOVE ONE, THE PODS OF AN INSTANCE NEED EACH OTHER'S ADDRESSES, so an instance of several Pods
	// is rendered with stable names and publishes the first Pod's address, this instance's size and
	// each Pod's own rank to every container. What an engine does with those facts -- which
	// parallelism it turns on, and over how many ranks -- stays the author's to say.
	//
	// THE GO IDENTIFIER IS NOT Size BECAUSE gogo protobuf generates a Size() method on this type and
	// Go forbids a field and a method sharing a name; the API field is size.
	//
	// THE UPPER BOUND IS THE SAME LIMIT REPLICAS CARRIES, AND IT MULTIPLIES WITH IT: this number is
	// how many Pods one instance is rendered as, so a pass renders replicas times this many before it
	// writes any of them. It is set far above the sizes an accelerator topology makes sense at, and
	// far below the range that turns one accepted field value into an out-of-memory worker.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=64
	ReplicaSize int32 `json:"size,omitempty" protobuf:"varint,15,opt,name=size"`

	// InstanceType is the name of the InstanceType whose pool this role's Pods are admitted against.
	// It is what the queue-name entrance label is derived from.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	InstanceType string `json:"instanceType" protobuf:"bytes,3,name=instanceType"`

	// Resources is what one Pod of this role asks of an accelerator, and it is a STRUCTURED
	// FIELD FOR THE SAME REASON Replicas and InstanceType are: admission and scheduling read it.
	//
	// It carries only the ACCELERATOR half of a request, because that is the only half a workload
	// decides. CPU, memory and ephemeral storage are DERIVED from the InstanceType's per-unit
	// resources scaled by the requested card count, so they are not expressible here at all — a
	// stronger guarantee than refusing them, since a field that does not exist cannot be shadowed by
	// the container fields below either.
	//
	// InstanceType alone cannot supply this half: its UnitResources size ONE card, and how many cards
	// a Pod wants is a property of the model being served, so two deployments on one InstanceType
	// routinely want different counts.
	Resources *ModelDeploymentRoleResources `json:"resources,omitempty" protobuf:"bytes,4,opt,name=resources"`

	// Image is the container image to run. Leaving it empty is the ordinary case: the operator then
	// synthesizes one from the pool's accelerator backend, the observed runtime version and the
	// requested engine.
	//
	// +optional
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,7,opt,name=image"`

	// ImagePullPolicy is the pull policy for Image.
	//
	// +optional
	ImagePullPolicy core.PullPolicy `json:"imagePullPolicy,omitempty" protobuf:"bytes,9,opt,name=imagePullPolicy"`

	// ImagePullSecrets are the secrets used to pull Image.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=32
	ImagePullSecrets []core.LocalObjectReference `json:"imagePullSecrets,omitempty" protobuf:"bytes,10,rep,name=imagePullSecrets"`

	// Privileged runs the container privileged.
	//
	// +optional
	Privileged bool `json:"privileged,omitempty" protobuf:"varint,11,opt,name=privileged"`

	// Ports are the container ports to expose in addition to the engine's own. They do not
	// reserve or select the transfer engine's runtime port window.
	//
	// +optional
	// +patchMergeKey=port
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=port
	// +listMapKey=protocol
	Ports []ModelDeploymentPort `json:"ports,omitempty" patchStrategy:"merge" patchMergeKey:"port" protobuf:"bytes,12,rep,name=ports"`

	// AdditionalVolumes are volumes mounted into the container alongside the operator's own.
	//
	// +optional
	// +listType=atomic
	AdditionalVolumes []ModelDeploymentAdditionalVolume `json:"additionalVolumes,omitempty" protobuf:"bytes,13,rep,name=additionalVolumes"` // nolint: lll

	// ShmSize limits the memory-backed /dev/shm volume in each role Pod. It must be positive.
	// Omission renders 16Gi. An explicit /dev/shm mount in AdditionalVolumes takes precedence.
	// Used shared memory counts toward the container's memory limit; this is not a reservation.
	// Existing elastic members keep their mounts; new and replacement Pods use the current value.
	//
	// +optional
	ShmSize *resource.Quantity `json:"shmSize,omitempty" protobuf:"bytes,19,opt,name=shmSize"`

	// Command replaces the whole argv, which is the TAKE-OVER tier: the user owns the whole
	// command line, the operator synthesizes no engine argument and no client environment, the
	// role is marked unmanaged and CacheAttached goes to Unknown. Arguments fold into Command;
	// there is deliberately no Args, because a second append tier beside ExtraArgs would have no
	// defined precedence.
	//
	// IT IS FROZEN AFTER CREATION, because it decides whether the operator configures this role at
	// all: a role that supplies one is taken over by its author, which changes cache injection and
	// what status can claim. The rest of the container fields are how the build is fetched, shaped
	// and tuned, and stay editable.
	//
	// +optional
	// +listType=atomic
	Command []string `json:"command,omitempty" protobuf:"bytes,14,rep,name=command"`

	// ExtraArgs is appended AFTER the operator-synthesized arguments. An entry naming a key the
	// operator owns is REJECTED rather than merged: a silent merge produces two values for one
	// connector argument and no way to tell which one won.
	//
	// The name stays ExtraArgs rather than Args because args would read as the whole argv, which is
	// what Command means; the two tiers differ in whether the operator contributes anything at all.
	//
	// THIS LIST IS NOT READ WHEN COMMAND IS SET: appending to an argv the role's author replaced
	// would put words into a command line they own, so the take-over tier takes the whole line and
	// this field does nothing beside it.
	//
	// +listType=atomic
	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// Env is appended the same way and refused on the same terms. Keys the operator merely defaults
	// are not owned: a user's value wins there and no rejection follows.
	//
	// There is ONE list here rather than an overlay beside it: the former second tier was appended
	// and refused for owned names on exactly the same terms, so the nesting expressed a precedence
	// that never existed.
	//
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	Env []ModelDeploymentEnvVar `json:"env,omitempty" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,6,rep,name=env"`

	// Topology optionally requires Kueue to place each replica's Pod group in one domain at this level.
	// It applies to this role's independent replica group; it does not require other roles or replicas
	// to share that domain.
	//
	// +optional
	Topology *ModelDeploymentRoleTopology `json:"topology,omitempty" protobuf:"bytes,16,name=topology"`

	// TerminationGracePeriodSeconds is the whole time a departing replica of this role gets, from
	// its Pod's delete to the kill, and it is written to each Pod's field of the same name. Unset
	// renders 30, the Kubernetes default.
	//
	//   - The drain hook is budgeted against it: the hook waits for the engine to go idle until
	//     5 s before this, and those 5 s are left for the engine to exit on SIGTERM. So raising it
	//     lengthens the wait for running requests and the time the engine has to exit, together.
	//   - A LONGER GRACE COSTS ACCELERATORS AND QUOTA. A departing replica holds both until it
	//     exits, on every delete, and a rollout waits each departing replica out one at a time per
	//     role, so the grace multiplies into every rollout. An idle replica still exits as soon as
	//     its engine does; the grace is a ceiling, not a wait.
	//   - An SGLang prefill or decode role needs about 45: measured idle, such an engine takes
	//     26-28 s after its delete to exit, which leaves 2 s before a kill at 30.
	//   - Changing it replaces every replica of the role, as any edit to a rendered Pod field does,
	//     and the replicas that leave in that rollout leave with the grace they were created with.
	//   - A take-over role, one that sets Command, gets it written to its Pods as given, and gets
	//     no drain hook either way; unset, its Pods keep the Kubernetes default.
	//   - The lower bound keeps a wait between the two fixed ends: the engine serves the first 5 s
	//     untouched and the last 5 s are the exit's, and at 15 the 5 s left between them hold the two
	//     idle reads the hook returns on. The upper bound is an hour, far past any request a replica
	//     serves; a request that needs longer needs a router that can move it, which no supported
	//     one does.
	//
	// +optional
	// +k8s:validation:minimum=15
	// +k8s:validation:maximum=3600
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty" protobuf:"varint,17,opt,name=terminationGracePeriodSeconds"` // nolint: lll

	// ElasticEP opts this role into the managed elastic-EP profile: the operator then renders
	// one dedicated logical Ray cluster for the deployment, with a CPU-only control-plane head
	// and one whole-GPU TP group per DP engine rank. PRESENCE IS THE PROFILE DISCRIMINANT: a role
	// without it renders exactly as it did before this field existed, and a profile that is
	// turned on and off again describes a different deployment. The field's presence is frozen
	// at creation. Width is editable for scale-up; scale-down is
	// refused by admission in this release.
	//
	// The profile admits exactly one such role per deployment, running the vLLM engine as a
	// single Server role of one instance of one Pod. Every member -- the reserved API/DP-master
	// and every Ray-only worker -- takes a complete TP group declared through ExtraArgs.
	// All GPUs are whole, unsliced and unpartitioned. Admission binds the resource count to TP.
	// The auxiliary head requests no container resources and never counts toward Width.
	//
	// +optional
	ElasticEP *ModelDeploymentRoleElasticEP `json:"elasticEp,omitempty" protobuf:"bytes,18,opt,name=elasticEp"`
}

// ModelDeploymentRoleElasticEP is the elastic-EP profile of one role.
//
// Width is the only mutable field: it answers how large the collective currently should be,
// which is a running-state question. This release accepts increases and unchanged values;
// admission refuses a decrease until Elastic EP scale-down support is complete.
// The role cannot enable or disable this profile after creation.
// Its tensor parallel size comes from ExtraArgs and cannot change after creation.
type ModelDeploymentRoleElasticEP struct {
	// Width is the total number of GPU engines in the elastic collective, INCLUDING the
	// reserved API/DP-master member; width-1 of them are Ray-only workers. It is the total
	// engine world the engine is told to run, not a Pod count and not a rank mapping: which
	// member holds which rank is the engine's own runtime fact and is never implied by this
	// number.
	//
	// +k8s:validation:minimum=2
	// +k8s:validation:maximum=64
	Width int32 `json:"width" protobuf:"varint,1,name=width"`
}

// ModelDeploymentRoleTopology is the topology request for one independent replica group.
type ModelDeploymentRoleTopology struct {
	// RequiredLevel is one configured Kueue topology level, such as topology.kubernetes.io/zone.
	// Empty omits an explicit level and lets a compatible TAS flavor choose its hierarchy.
	// +optional
	RequiredLevel string `json:"requiredLevel,omitempty" protobuf:"bytes,1,name=requiredLevel"`
}

// ModelDeploymentRoleKind is what a role is told it is, in a prefill/decode disaggregated
// deployment.
//
// The values are the roles an inference engine understands, not the roles this operator invents:
// each selects the discriminator term the engine's own KV-transfer configuration takes. A kind the
// engine's rendering has no term for is refused at admission rather than rendered into a
// configuration the engine would reject at start-up.
// +enum
type ModelDeploymentRoleKind string

const (
	// ModelDeploymentRoleKindServer is a role that serves whole requests by itself: prefill and
	// decode in one process. It is the default and the only kind a single-role deployment has, and
	// it is refused alongside any other kind, because "one plain server plus a prefiller" is not a
	// shape anything consumes.
	ModelDeploymentRoleKindServer ModelDeploymentRoleKind = "Server"
	// ModelDeploymentRoleKindPrefill is a role that computes the prompt's KV blocks and hands them
	// on rather than decoding them itself.
	ModelDeploymentRoleKindPrefill ModelDeploymentRoleKind = "Prefill"
	// ModelDeploymentRoleKindDecode is a role that consumes KV blocks a prefiller produced and
	// generates tokens from them.
	ModelDeploymentRoleKindDecode ModelDeploymentRoleKind = "Decode"
)

// ModelDeploymentPort defines one port a role's replica exposes beside the engine's own.
//
// IT CARRIES NO NAME, and that is a decision rather than an omission. The rendered container port is
// named from the protocol and the number, so a name written here would be accepted by the schema and
// then discarded — the pattern this API refuses everywhere else, a promise broken quietly. Two ports
// of one role are told apart by their numbers, which the webhook already requires to be unique.
type ModelDeploymentPort struct {
	// Port is the port number to expose on the replica.
	//
	// The bounds are the container port's own. Without them a zero or out-of-range number is
	// admitted here and refused later by the API server, on the rendered Pod, as a per-pass create
	// failure naming a container port instead of an admission error naming this field.
	//
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=65535
	Port int32 `json:"port" protobuf:"varint,1,name=port"`

	// Protocol is the protocol to use for the port.
	//
	// +k8s:validation:enum=["TCP","UDP","SCTP"]
	// +k8s:validation:default="TCP"
	Protocol core.Protocol `json:"protocol,omitempty" protobuf:"bytes,2,opt,name=protocol"`
}

// ModelDeploymentEnvVar defines one environment variable appended to a role's replica.
type ModelDeploymentEnvVar struct {
	// Name is the name of the environment variable; each name in one role must be unique.
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Value is the value of the environment variable.
	Value string `json:"value" protobuf:"bytes,2,name=value"`
}

// ModelDeploymentAdditionalVolume defines one volume to mount in a role's replica besides the
// operator's own. One of the sources below must be set; an entry naming none is skipped by the
// render rather than refused.
type ModelDeploymentAdditionalVolume struct {
	// MountPath is the absolute in-container path to mount the volume at. It must not duplicate
	// another entry's path, nor a path the operator's own volumes already mount.
	//
	// +required
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	MountPath string `json:"mountPath" protobuf:"bytes,1,name=mountPath"`

	// ReadOnly mounts the volume read-only.
	ReadOnly bool `json:"readOnly,omitempty" protobuf:"varint,2,opt,name=readOnly"`

	// SubPath mounts a relative path inside the volume rather than its root.
	// It must not be absolute nor contain a ".." element.
	//
	// The pattern below enforces only the first half. A ".." element cannot be excluded by this
	// engine's regular expressions, which have no negative lookahead, so admission carries that
	// half — see the webhook. Both halves are refused there rather than left to the API server's
	// rejection of the rendered Pod, which arrives as a per-pass create failure naming a
	// volumeMount instead of an admission error naming this field.
	//
	// +k8s:validation:pattern="^[^/].*$"
	// +k8s:validation:maxLength=1024
	SubPath string `json:"subPath,omitempty" protobuf:"bytes,3,opt,name=subPath"`

	// ConfigMap is the reference to the ConfigMap to mount, in the same namespace.
	ConfigMap *core.LocalObjectReference `json:"configMap,omitempty" protobuf:"bytes,4,opt,name=configMap"`

	// Secret is the reference to the Secret to mount, in the same namespace.
	Secret *core.LocalObjectReference `json:"secret,omitempty" protobuf:"bytes,5,opt,name=secret"`

	// HostPath is the path on the Kubernetes Node to mount. It crosses the node boundary: the
	// mount reaches the node's own filesystem rather than a namespaced object, so what it exposes
	// is decided by what the node carries rather than by anything this API can see.
	HostPath *core.HostPathVolumeSource `json:"hostPath,omitempty" protobuf:"bytes,6,opt,name=hostPath"`
}

// ModelDeploymentRoleResources is what one Pod of a role asks of accelerators and fabric interfaces.
//
// It deliberately mirrors the accelerator fields of InstanceResources — the same names, the same
// meanings — rather than inventing a second vocabulary for one request, and it deliberately omits
// that type's CPU, RAM and LocalStorage, which are derived here rather than declared.
type ModelDeploymentRoleResources struct {
	// Accelerator is how many accelerator cards ONE POD asks for.
	//
	//   - Left unset on an acceleratable InstanceType it DEFAULTS TO ONE at admission, on create and
	//     on update alike, the same way an Instance's does. The value is written into the stored
	//     object rather than applied at render time, so what was admitted is what can be read back.
	//   - AN EXPLICIT ZERO IS KEPT, because it is a value the user wrote, and on an acceleratable
	//     InstanceType it asks for nothing that pool's queue accounts in. It is accepted while it is
	//     the only role using that type, and refused when another role shares the type: replicas
	//     asking for nothing the queue accounts in are admitted and run while that queue charges
	//     them nothing, spending from the pool their siblings on that type are charged for.
	//   - A Pod meant to run without an accelerator belongs on an InstanceType that is not
	//     acceleratable, where CPU is what the queue accounts in.
	Accelerator *resource.Quantity `json:"accelerator,omitempty" protobuf:"bytes,1,opt,name=accelerator"`

	// AcceleratorSlicedMemoryPercentage is the per-accelerator VRAM budget requested on a sliced
	// InstanceType, as a percentage in [0,100]. 0 disables slicing, making the request an exclusive
	// whole-accelerator one. It is ignored by an InstanceType offering no slicing.
	//
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=100
	AcceleratorSlicedMemoryPercentage int32 `json:"acceleratorSlicedMemoryPercentage,omitempty" protobuf:"varint,2,opt,name=acceleratorSlicedMemoryPercentage"` // nolint: lll

	// AcceleratorSlicedCoresPercentage is the per-accelerator compute budget requested on a sliced
	// InstanceType, as a percentage in [0,100], independent of the memory percentage.
	//
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=100
	AcceleratorSlicedCoresPercentage int32 `json:"acceleratorSlicedCoresPercentage,omitempty" protobuf:"varint,3,opt,name=acceleratorSlicedCoresPercentage"` // nolint: lll

	// AcceleratorPartitionedProfile is the hardware partition profile requested on a
	// partition-offering InstanceType, e.g. "3g.40gb". A non-empty value is mutually exclusive with
	// the two slice percentages: hardware partitioning and software slicing cannot both apply to one
	// accelerator. It is ignored by an InstanceType offering no partition.
	//
	// +k8s:validation:maxLength=64
	AcceleratorPartitionedProfile string `json:"acceleratorPartitionedProfile,omitempty" protobuf:"bytes,4,opt,name=acceleratorPartitionedProfile"` // nolint: lll

	// Interface is the number of fabric interfaces one role Pod requests, as a whole number.
	// Unset or zero requests none. A positive count selects the device-plugin resource of the
	// effective cache or direct-transfer protocol. A single RDMA interface uses the shared
	// resource; multiple RDMA interfaces use the exclusive resource. EFA uses its own plugin
	// resource. Conflicting effective protocols are refused rather than assigned one of the
	// available device families. Admission enforces the whole number and the protocol rules,
	// the same as for accelerator; the schema carries no bound of its own.
	//
	// +optional
	Interface *resource.Quantity `json:"interface,omitempty" protobuf:"bytes,5,opt,name=interface"`
}

// ModelDeploymentRouter is the router that fronts a deployment's roles, and how much of it this
// operator runs.
//
// THERE IS NO FIELD SELECTING WHO RUNS THE ROUTER, and that is a decision rather than an omission.
// This operator renders and owns it; a deployment fronted by a router the cluster already runs is not
// expressible. A field offering that choice while only one of its values rendered anything would
// carry no information -- every object would hold the same value -- and adding one later is an
// optional field with a default, which is backward compatible. Shipping the choice first and then
// changing what its values mean would not be.
type ModelDeploymentRouter struct {
	// Name selects which router implementation fronts this deployment.
	//
	// THE VALUE FOLLOWS THE PROJECT'S OWN SPELLING, NOT THIS API'S HOUSE STYLE, and the difference is
	// visible in the same word twice: the transport protocol on the cache backend types spells it
	// "Auto" while the connector here spells it "auto". The casing convention is per API type, and the
	// reason is the one ModelDeploymentRoleKind states about itself -- these values are terms the
	// outside tool understands, not terms this operator invents. "llm-d-router" is how the llm-d
	// project spells this router in its module path, so it is spelled that way here.
	//
	// THE RENAME FROM "llm-d" CARRIES NO CONVERSION, because that spelling never shipped in a
	// release: it lived on main between this field's introduction and this rename, with no release
	// cut in between, so no released CRD ever accepted it and every object a release could have
	// written spells the value this enum requires. A cluster running an unreleased build of the
	// interval is outside that guarantee and edits such an object by hand.
	//
	// WIDENING IT IS FOUR THINGS, NOT ONE: one entry here, one configuration renderer, the object set
	// that router needs, AND the wiring that threads this value to a dispatch point. The schema
	// reservation covers the first of those and nothing else, which is why a second router is a piece
	// of work rather than a constant.
	//
	// A VALUE IS ALSO ENGINE-MATCHED, and the match is a separate rule rather than something this
	// enum can express: "vllm-router" and "sglang-gateway" are each one project's own router for
	// its own engine, so each is refused in front of the other's. "llm-d-router" takes either
	// engine, because upstream carries a handshake connector and a metrics configuration for each
	// of them.
	//
	// +required
	// +k8s:validation:enum=["llm-d-router","vllm-router","sglang-gateway"]
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Replicas is how many router Pods to run. Absent means one.
	//
	// It is an optional field, and that is forced rather than chosen. A schema
	// default is applied before any webhook sees the object, so a plain int32 defaulted to one arrives
	// indistinguishable from one the user typed. Keeping the distinction readable at admission is what
	// lets a later rule answer "did anyone ask for this" at all, and a default that erases the
	// difference cannot be un-erased afterwards.
	//
	// MORE THAN ONE REPLICA TRADES CACHE CONSISTENCY FOR AVAILABILITY. A router that scores on a
	// prefix cache holds that state per replica: upstream reports radix trees that do not synchronize
	// across replicas and a hit rate falling by ten to twenty percent as a result, and reports that
	// where replicas do exchange events the exchange improves load estimation without making two
	// replicas route alike. More than one is permitted; the cost is stated here rather than left to be
	// found on a dashboard.
	//
	// +optional
	// +k8s:validation:minimum=1
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,2,opt,name=replicas"`

	// Image overrides the router's container image. Empty means the operator assembles one from Name,
	// the same way a role's image is assembled when the role names none.
	//
	// +optional
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,3,opt,name=image"`

	// ExtraArgs are additional flags for the router process.
	//
	// A flag the operator derives itself is refused rather than merged, so that one setting has one
	// source. The owned catalog is keyed by router because the engine-keyed catalog guarding a role's
	// extraArgs answers a different question and cannot stand in for it.
	//
	// +optional
	// +listType=atomic
	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,4,rep,name=extraArgs"`

	// ImagePullPolicy is the pull policy for Image.
	//
	// +optional
	ImagePullPolicy core.PullPolicy `json:"imagePullPolicy,omitempty" protobuf:"bytes,5,opt,name=imagePullPolicy"`

	// ImagePullSecrets are the secrets used to pull Image.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=32
	ImagePullSecrets []core.LocalObjectReference `json:"imagePullSecrets,omitempty" protobuf:"bytes,6,rep,name=imagePullSecrets"`

	// RequestTimeoutSeconds is how long the router waits for a reply before giving up on it.
	//
	// UNSET DOES NOT MEAN ONE THING ACROSS THE ROUTERS, and saying so here is the point of this
	// paragraph. Leaving it out renders nothing, so each router keeps its own upstream default: one
	// day under "llm-d-router", whose proxy carries the timeout, against half an hour under the two
	// configured by their command line. That is a factor of forty-eight, and it is why setting this
	// field is the only way a declaration survives a change of router — its absence leaves three
	// upstream opinions in place rather than choosing between them.
	//
	// ZERO IS NOT ACCEPTED. It would mean "wait forever" under the proxy and nothing in particular
	// under the other two, and a field meaning the same thing across three implementations cannot
	// carry one implementation's special value. A day is already long enough that the difference is
	// theoretical; the floor can be lowered later without breaking an object that exists.
	//
	// +optional
	// +k8s:validation:minimum=1
	RequestTimeoutSeconds *int32 `json:"requestTimeoutSeconds,omitempty" protobuf:"varint,7,opt,name=requestTimeoutSeconds"`

	// DisaggregationThresholdTokens is how many prompt tokens NOT already in a prefix cache make a
	// request worth splitting between a prefiller and a decoder. Below it the decode replica serves
	// the whole request itself. Unset renders the router's own current value.
	//
	// ZERO DISABLES DISAGGREGATION ENTIRELY rather than meaning "always split": the decider returns
	// "do not disaggregate" on a zero threshold before reading anything else. It is accepted rather
	// than refused because it is a value upstream defines, and the field is optional, so writing
	// zero and leaving the field out remain two different statements.
	//
	// IT IS MEANINGFUL UNDER "llm-d-router" ALONE and is REFUSED under the other two rather than
	// ignored, because a field that is legal to write and renders nothing is a shape this API has
	// rejected before. The refusal is stable because Name is frozen after creation, so an object
	// cannot become invalid through a later edit to some other field.
	//
	// +optional
	// +k8s:validation:minimum=0
	DisaggregationThresholdTokens *int32 `json:"disaggregationThresholdTokens,omitempty" protobuf:"varint,8,opt,name=disaggregationThresholdTokens"`
}

// The routers a ModelDeployment can name, which are the values of ModelDeploymentRouter.Name's enum.
//
// Declared beside the field whose schema closes the set, so that a reader of either finds the other.
const (
	ModelDeploymentRouterLLMD   = "llm-d-router"
	ModelDeploymentRouterVLLM   = "vllm-router"
	ModelDeploymentRouterSGLang = "sglang-gateway"
)

// ModelDeploymentStatus defines the observed state of ModelDeployment.
//
// It is REBUILT FROM OBSERVED STATE ON EVERY RECONCILE, so a stale field cannot survive a
// disagreement with the Pods.
type ModelDeploymentStatus struct {
	// Phase summarizes the conditions: Starting, Ready, Degraded, Deleting. Ready means every role's
	// ready count equals its desired count and, when declared, the router has a ready replica.
	// Degraded means a serving component is ready while another required one is not.
	Phase string `json:"phase,omitempty" protobuf:"bytes,1,opt,name=phase"`

	// PhaseMessage carries the reason for the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Conditions is the finer view, one condition per axis: DomainRegistered, QuotaReserved,
	// CacheAttached, ReplicasUpToDate, RoleKindsReady, KVEventsPublishing, RouterReady,
	// WeightsReady. They are independent —
	// "quota reserved but cache not attached" is a real and actionable state — which is what a single
	// phase string cannot carry.
	//
	// KVEventsPublishing reports rendered configuration rather than observing the stream. A publisher
	// that was configured and then crashed therefore remains True until a live consumer observes it.
	//
	// WeightsReady reports whether every engine role's weights are available: a claim artifact
	// mounted, or an engine's own download of a hub artifact finished. While it is False for a
	// reason other than a Pod still starting, no replica is created and none that runs is touched.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"` // nolint: lll

	// Endpoint is the address clients use. Without a router it is the deployment-wide Service, in the
	// form <scheme>://<name>.<namespace>.svc:<port>. With a router it is the router's Service and is
	// absent until the router has a ready replica.
	//
	// A role endpoint uses https where that role's own arguments put its listener on TLS. The managed
	// router endpoint uses the router's own transport instead, which is http. A client reads the
	// selected value from here rather than assuming either one.
	//
	// +k8s:validation:maxLength=512
	Endpoint string `json:"endpoint,omitempty" protobuf:"bytes,4,opt,name=endpoint"`

	// Roles is one entry per declared role.
	//
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentRoleStatus `json:"roles,omitempty" protobuf:"bytes,5,rep,name=roles"`

	// KVCache is the reuse domain this deployment actually attached to, read from the Binding, so
	// that telling a cache-sharing misconfiguration from a cache that is merely cold takes one object
	// rather than two.
	//
	// It is ABSENT until the Binding has been resolved once, rather than present and empty: an empty
	// object here would be indistinguishable from a domain whose every field happens to be empty.
	// Once resolved, it keeps the last domain read when the Binding can no longer be resolved,
	// because the replicas are still writing into that domain; DomainRegistered is what says the
	// reading is stale.
	KVCache *ModelDeploymentKVCacheStatus `json:"kvCache,omitempty" protobuf:"bytes,6,opt,name=kvCache"`

	// Router is the observed contract of the managed router requested by spec.router.
	//
	// It is ABSENT when spec.router is, rather than present and empty, for the same reason KVCache is:
	// an empty object here cannot be told apart from a contract whose every string happens to be
	// empty.
	Router *ModelDeploymentRouterStatus `json:"router,omitempty" protobuf:"bytes,7,opt,name=router"`

	// RoleSummary is the current Ready count by role kind, for kubectl's Roles column. R counts
	// managed router Pods; S, P, and D count server, prefill, and decode instances. A serving
	// instance may contain several Pods, so the engine figures are not Pod counts.
	RoleSummary string `json:"roleSummary,omitempty" protobuf:"bytes,8,opt,name=roleSummary"`

	// Model echoes the weights spec.model.artifactRef resolved to, and how they reach the engine.
	//
	// It is ABSENT without spec.model.artifactRef, and while the artifact has not resolved, for the
	// reason KVCache is: an empty object here cannot be told apart from an identity whose every
	// field happens to be empty.
	Model *ModelDeploymentModelStatus `json:"model,omitempty" protobuf:"bytes,9,opt,name=model"`

	// Retirement is the persisted reservation of the retirement operation this deployment is
	// running, at most one at a time. It is ABSENT when no operation exists, rather than present
	// and empty: an operation carries its state in the object, so an empty one would be neither.
	// Its identity binds the admitting metadata.generation, the role, the replica ordinal and
	// the target member and Workload UIDs, so a controller restart resumes it and a conflicting
	// operation cannot silently adopt it.
	//
	// +optional
	Retirement *ModelDeploymentRetirementStatus `json:"retirement,omitempty" protobuf:"bytes,10,opt,name=retirement"`
}

// ModelDeploymentRetirementStatus is the persisted reservation of one retirement operation.
//
// The reservation is the protocol's progress: every step, budget boundary and retry already
// consumed is carried here, so a controller restart re-enters at the observed state instead of
// re-deciding one. Phase budgets read deadline and phaseStartedAt together, so a restart never
// resets a phase that was already running.
type ModelDeploymentRetirementStatus struct {
	// RoleName is the role whose replica is retiring.
	//
	// +required
	RoleName string `json:"roleName" protobuf:"bytes,1,name=roleName"`

	// ReplicaOrdinal is the ordinal of the retiring replica within its role. Zero is a real
	// position, the first replica, and is always encoded rather than omitted.
	//
	// +required
	ReplicaOrdinal int32 `json:"replicaOrdinal" protobuf:"varint,2,name=replicaOrdinal"`

	// ObservedGeneration is the metadata.generation whose intent this reservation was admitted
	// against.
	//
	// +required
	ObservedGeneration int64 `json:"observedGeneration" protobuf:"varint,3,name=observedGeneration"`

	// TargetMemberUIDs are the UIDs of the member Pods the reservation holds. A deletion of one
	// of them carries its UID as a precondition, so a same-name replacement is never deleted in
	// the target's place.
	//
	// +optional
	// +listType=atomic
	TargetMemberUIDs []string `json:"targetMemberUIDs,omitempty" protobuf:"bytes,4,rep,name=targetMemberUIDs"`

	// TargetWorkloadUID is the UID of the Workload the reservation holds.
	//
	// +required
	TargetWorkloadUID string `json:"targetWorkloadUID" protobuf:"bytes,5,name=targetWorkloadUID"`

	// State is the protocol step the reservation sits at. Aborted names a budget exhausted
	// before deletion, where every member and the Workload were retained; it is retained state,
	// not a rollback.
	//
	// +required
	// +k8s:validation:enum=["Admitted","Disqualified","Withdrawing","Draining","Deleting","Settling","Aborted","Completed"]
	State ModelDeploymentRetirementState `json:"state" protobuf:"bytes,6,name=state,casttype=ModelDeploymentRetirementState"`

	// Reason names what put the reservation at its state, e.g. which step a budget exhausted at.
	//
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,7,opt,name=reason"`

	// StartedAt is when the operation was admitted.
	//
	// +required
	StartedAt meta.Time `json:"startedAt" protobuf:"bytes,8,name=startedAt"`

	// Deadline is the overall budget. A phase ends at min(deadline, its own start plus its
	// budget), so no phase outlives the operation.
	//
	// +required
	Deadline meta.Time `json:"deadline" protobuf:"bytes,9,name=deadline"`

	// PhaseStartedAt is when the current phase began. It is persisted so a controller restart
	// never resets a phase budget that was already running.
	//
	// +required
	PhaseStartedAt meta.Time `json:"phaseStartedAt" protobuf:"bytes,10,name=phaseStartedAt"`

	// LastConsumedRetryToken is the retry directive token this reservation last consumed. It is
	// persisted before the annotation that carried the token is cleared, so a crash between the
	// two leaves the token consumed and any replay a no-op. Empty means none was consumed.
	//
	// +optional
	LastConsumedRetryToken string `json:"lastConsumedRetryToken,omitempty" protobuf:"bytes,11,opt,name=lastConsumedRetryToken"`
}

// ModelDeploymentRetirementState is the protocol step a retirement reservation sits at.
// +enum
type ModelDeploymentRetirementState string

const (
	// ModelDeploymentRetirementStateAdmitted is the reservation as written, before any step ran.
	ModelDeploymentRetirementStateAdmitted ModelDeploymentRetirementState = "Admitted"
	// ModelDeploymentRetirementStateDisqualified is after eligibility removal, while Services converge.
	ModelDeploymentRetirementStateDisqualified ModelDeploymentRetirementState = "Disqualified"
	// ModelDeploymentRetirementStateWithdrawing waits for actual serving confirmation of the target.
	ModelDeploymentRetirementStateWithdrawing ModelDeploymentRetirementState = "Withdrawing"
	// ModelDeploymentRetirementStateDraining reads engine in-flight queues in place, every member retained.
	ModelDeploymentRetirementStateDraining ModelDeploymentRetirementState = "Draining"
	// ModelDeploymentRetirementStateDeleting is the committed deletion step.
	ModelDeploymentRetirementStateDeleting ModelDeploymentRetirementState = "Deleting"
	// ModelDeploymentRetirementStateSettling observes accelerator release and quota convergence after deletion.
	ModelDeploymentRetirementStateSettling ModelDeploymentRetirementState = "Settling"
	// ModelDeploymentRetirementStateAborted is a budget exhausted before deletion: members, Workload
	// and capacity were retained, and a healthy instance requalifies without a rollout.
	ModelDeploymentRetirementStateAborted ModelDeploymentRetirementState = "Aborted"
	// ModelDeploymentRetirementStateCompleted is the terminal transition of a completed
	// retirement: it is set on the same pass that clears the reservation, so a reader of the
	// status observes completion as the field's absence and never sees this value stored.
	ModelDeploymentRetirementStateCompleted ModelDeploymentRetirementState = "Completed"
)

// ModelDeploymentModelStatus is the resolved weight identity a deployment serves.
//
// Every field is READ FROM THE MODELARTIFACT, never declared here, so reading which commit and which
// content a deployment serves takes one object rather than two.
type ModelDeploymentModelStatus struct {
	// Artifact is the ModelArtifact this deployment references, in this namespace.
	//
	// +required
	Artifact string `json:"artifact" protobuf:"bytes,1,name=artifact"`

	// Revision is the commit a hub artifact resolved to. Absent for a claim.
	//
	// +optional
	Revision string `json:"revision,omitempty" protobuf:"bytes,2,opt,name=revision"`

	// ManifestDigest is a hub artifact's content address. Absent for a claim.
	//
	// +optional
	ManifestDigest string `json:"manifestDigest,omitempty" protobuf:"bytes,3,opt,name=manifestDigest"`

	// Delivery is how the weights reach the engine: "PVC", the claim mounted read-only at a fixed
	// path; "Engine", the engine downloading the pinned commit itself; "Node", the node's
	// model-manager plugin materializing the verified files and mounting them read-only at the same
	// fixed path; or "Image", kubelet pulling the pinned OCI image into a read-only image volume.
	//
	// +required
	// +k8s:validation:enum=["PVC","Engine","Node","Image"]
	Delivery ModelDeploymentModelDelivery `json:"delivery" protobuf:"bytes,4,name=delivery,casttype=ModelDeploymentModelDelivery"`
}

// ModelDeploymentModelDelivery is how a deployment's weights reach its engine.
// +enum
type ModelDeploymentModelDelivery string

const (
	// ModelDeploymentModelDeliveryPVC mounts a claim artifact read-only at a fixed path.
	ModelDeploymentModelDeliveryPVC ModelDeploymentModelDelivery = "PVC"
	// ModelDeploymentModelDeliveryEngine has the engine download a hub artifact's resolved commit
	// into a size-limited cache volume, with the artifact's token from its Secret.
	ModelDeploymentModelDeliveryEngine ModelDeploymentModelDelivery = "Engine"
	// ModelDeploymentModelDeliveryNode has the node's model-manager plugin materialize a hub
	// artifact's verified files into the node's cache and mount them read-only.
	ModelDeploymentModelDeliveryNode ModelDeploymentModelDelivery = "Node"
	// ModelDeploymentModelDeliveryImage mounts an image artifact's OCI image read-only through a
	// Kubernetes image volume: kubelet pulls the pinned image on the node that needs it, and the
	// node's plugin cache plays no part.
	ModelDeploymentModelDeliveryImage ModelDeploymentModelDelivery = "Image"
)

// ModelDeploymentRoleStatus is one role's observed readiness.
type ModelDeploymentRoleStatus struct {
	// Name is the role this entry describes.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Desired is how many INSTANCES the spec asks for, and Ready is how many of them are Ready. Both
	// count instances rather than Pods, which is the same number only while an instance is one Pod: a
	// role of two instances of four Pods reports two, and an instance is Ready only when every Pod it
	// declares is. Both are ALWAYS present -- they are counted from a Pod list that succeeded, so a
	// zero here is an observed zero. A failed list writes no status at all.
	Desired int32 `json:"desired" protobuf:"varint,2,name=desired"`

	Ready int32 `json:"ready" protobuf:"varint,3,name=ready"`

	// QuotaReserved is how many of the role's replicas hold a quota reservation. Each replica is
	// its own Kueue workload, so a role sits at any count between zero and Desired while capacity
	// arrives — where a role that shared one workload passed all-or-nothing and this figure could
	// not exist.
	//
	// ALWAYS PRESENT, AND ITS ZERO IS AN OBSERVED ONE: the figure is counted from Pod and Workload
	// lists that succeeded, and a failed list writes no status at all rather than a zero, because
	// "this pass could not see" and "no replica holds quota" call for opposite actions — one waits,
	// the other investigates — and a zero written for both makes them the same reading.
	QuotaReserved int32 `json:"quotaReserved" protobuf:"varint,7,name=quotaReserved"`

	// Unmanaged is true when the role replaced the whole command line, so the operator synthesized
	// no engine argument and no client environment for it. It is ALWAYS present, for the same reason
	// the counts are: false is the ordinary case and has to be visible as an answer rather than as a
	// missing field.
	Unmanaged bool `json:"unmanaged" protobuf:"varint,4,name=unmanaged"`

	// Kind echoes the role's kind, so reading the status alone answers which half of a
	// disaggregated deployment an entry describes. It is ALWAYS present: every role has a kind,
	// defaulted if the user named none, so an absent value would mean the status was written by
	// something that did not know about kinds rather than that the role has none.
	//
	// The enum is the same one the spec field carries and has to stay that way: this field is written
	// from the spec field with the unset case resolved, so a value the writer can produce and this
	// list does not name would make every later status write on the object fail, taking every other
	// figure down with the kind. The marker sits on the field because the type's own enum marker is a
	// Go-level one and does not become schema validation.
	//
	// +k8s:validation:enum=["Server","Prefill","Decode"]
	Kind ModelDeploymentRoleKind `json:"kind" protobuf:"bytes,5,name=kind,casttype=ModelDeploymentRoleKind"`

	// AssignedFlavors is the set of ResourceFlavors Kueue assigned to this role's replicas for
	// their ACCELERATOR credits, deduplicated and sorted. It is a set because each replica is its
	// own workload and Kueue assigns a flavor per workload, so two replicas of one role can carry
	// different assignments — a state one PodSet per role could not produce.
	//
	//   - ABSENT MEANS NO ASSIGNED REPLICA NAMED A FLAVOR, rather than an empty list reading as an
	//     assignment to nothing: "not assigned yet" and "assigned, but on a pool carrying no
	//     accelerator names" are both that same fact here, and absent keeps them from reading as a
	//     third thing.
	//   - ONE ENTRY MEANS EVERY ASSIGNED REPLICA OF THE ROLE NAMES IT. SEVERAL ENTRIES MEAN THE
	//     REPLICAS WERE ASSIGNED DIFFERENT FLAVORS, which is the signal to investigate rather than a
	//     degraded form of one answer: WHICH replica carries which flavor is deliberately not here,
	//     because the ordinal a per-replica answer would key on is the converger's internal slotting
	//     rather than a promise this API makes, and a reader needing it reads the replicas' own Pods.
	//   - AN ADMITTED ROLE MAY STILL REPORT NOTHING HERE, and that is the field's contract rather
	//     than a gap in it. The answer is read through the same function the per-accelerator
	//     admission gate uses, which speaks only of accelerator credits, so a role admitted on a pool
	//     carrying no accelerator names a flavor for `cpu` and nothing here. The two answers are kept
	//     identical on purpose: a flavor reported here that the gate would not fit against would be
	//     worse than none.
	//
	// +optional
	// +listType=atomic
	AssignedFlavors []string `json:"assignedFlavors,omitempty" protobuf:"bytes,6,rep,name=assignedFlavors"`

	// Parallelism is the parallelism the role's own arguments declare, with the provenance of
	// the reading. Nothing is defaulted into it: a degree the role never declares is nil, an
	// explicit 1 and an explicit local 0 are preserved as values, and a reading that has not
	// happened yet reports Unknown with a reason rather than a silent 1. The numeric results
	// the prefill/decode transfer document derives from the same arguments are unaffected by
	// this view.
	//
	// It is optional so a role stored before this view existed stays writable: absence names
	// that fact, never a defaulted reading, and the object the operator writes fills it again.
	//
	// +optional
	Parallelism ModelDeploymentRoleParallelismStatus `json:"parallelism" protobuf:"bytes,8,name=parallelism"`

	// Endpoints is the role's endpoint eligibility and its actual serving confirmation. The two
	// are separate answers that never collapse: eligibility is the set the operator qualified,
	// and serving is what Routers were observed to still select. A count that was never
	// observed is nil, and zero is an observed fact only.
	//
	// It is optional like parallelism, and for the same reason: a role stored before this view
	// existed carries neither object, and absence must not reject its next write.
	//
	// +optional
	Endpoints ModelDeploymentRoleEndpointsStatus `json:"endpoints" protobuf:"bytes,9,name=endpoints"`
}

// ModelDeploymentRoleParallelismStatus is one role's declared parallelism, read from the
// command line rendered for it.
//
// Until the arguments have been read, every declared degree is nil, the mode set is empty, the
// balance is Unknown and the source is Unknown with complete=false and a reason — the same
// honesty the counts above owe an unobserved figure.
type ModelDeploymentRoleParallelismStatus struct {
	// Declared carries the degrees the role's arguments declare.
	//
	// +optional
	Declared ModelDeploymentParallelismDeclaredStatus `json:"declared" protobuf:"bytes,1,name=declared"`

	// Modes are the parallelism modes present in the role's arguments, keyed by canonical
	// engine mode. An observed mode carries its explicit boolean; the absence of a key is not
	// false. Empty until the arguments have been read.
	//
	// +optional
	// +mapType=atomic
	Modes map[string]bool `json:"modes,omitempty" protobuf:"bytes,2,rep,name=modes"`

	// LoadBalance is the balance shape the declared degrees derive to. Unknown means not
	// derivable from what the role declares; a support judgment is carried in reason fields,
	// never as a mode value here.
	//
	// +k8s:validation:enum=["Internal","External","Hybrid","MultiPort","Unknown"]
	LoadBalance ModelDeploymentLoadBalance `json:"loadBalance" protobuf:"bytes,3,name=loadBalance,casttype=ModelDeploymentLoadBalance"`

	// Source is the provenance of this reading.
	//
	// +optional
	Source ModelDeploymentParallelismSourceStatus `json:"source" protobuf:"bytes,4,name=source"`
}

// ModelDeploymentParallelismDeclaredStatus carries the degrees a role's arguments declare.
//
// A nil field means the role does not declare that degree; it is never read as a zero or a
// one. An explicit value is preserved exactly, including an explicit local degree of 0, which
// says something an omitted field cannot.
type ModelDeploymentParallelismDeclaredStatus struct {
	// +optional
	TensorParallel *int32 `json:"tensorParallel,omitempty" protobuf:"varint,1,opt,name=tensorParallel"`

	// +optional
	PipelineParallel *int32 `json:"pipelineParallel,omitempty" protobuf:"varint,2,opt,name=pipelineParallel"`

	// +optional
	DataParallel *int32 `json:"dataParallel,omitempty" protobuf:"varint,3,opt,name=dataParallel"`

	// +optional
	DataParallelLocal *int32 `json:"dataParallelLocal,omitempty" protobuf:"varint,4,opt,name=dataParallelLocal"`

	// +optional
	PrefillContextParallel *int32 `json:"prefillContextParallel,omitempty" protobuf:"varint,5,opt,name=prefillContextParallel"`

	// +optional
	DecodeContextParallel *int32 `json:"decodeContextParallel,omitempty" protobuf:"varint,6,opt,name=decodeContextParallel"`

	// +optional
	ExpertParallel *int32 `json:"expertParallel,omitempty" protobuf:"varint,7,opt,name=expertParallel"`

	// +optional
	AttentionContextParallel *int32 `json:"attentionContextParallel,omitempty" protobuf:"varint,8,opt,name=attentionContextParallel"`

	// MoEDPSize is the role's declared expert-dispatch data parallel size.
	//
	// +optional
	MoEDPSize *int32 `json:"moeDpSize,omitempty" protobuf:"varint,9,opt,name=moeDpSize"`

	// DWDPSize is the role's declared data-within-data parallel size.
	//
	// +optional
	DWDPSize *int32 `json:"dwdpSize,omitempty" protobuf:"varint,10,opt,name=dwdpSize"`
}

// ModelDeploymentParallelismSourceStatus is the provenance of a role's parallelism reading.
type ModelDeploymentParallelismSourceStatus struct {
	// Kind is where the reading came from. An unreadable source is Unknown with complete=false
	// and a reason, never a silently defaulted degree.
	//
	// +k8s:validation:enum=["ExtraArgs","Command","UnmanagedCommand","Unknown"]
	Kind ModelDeploymentParallelismSourceKind `json:"kind" protobuf:"bytes,1,name=kind,casttype=ModelDeploymentParallelismSourceKind"`

	// Complete is whether the reading covered everything the contract reads. Only a complete
	// reading is one the declared degrees above can be trusted from; an incomplete one keeps
	// them nil rather than partial.
	Complete bool `json:"complete" protobuf:"varint,2,name=complete"`

	// UnreadableReason names what could not be established, e.g. a reading that has not happened
	// yet, or a balance shape the declared flags disagree on. A complete reading can still carry
	// one when the source was read in full but a derived fact is undecidable.
	//
	// +optional
	UnreadableReason string `json:"unreadableReason,omitempty" protobuf:"bytes,3,opt,name=unreadableReason"`
}

// ModelDeploymentParallelismSourceKind names where a role's parallelism reading came from.
// +enum
type ModelDeploymentParallelismSourceKind string

const (
	// ModelDeploymentParallelismSourceKindExtraArgs reads the owned extra arguments of the role.
	ModelDeploymentParallelismSourceKindExtraArgs ModelDeploymentParallelismSourceKind = "ExtraArgs"
	// ModelDeploymentParallelismSourceKindCommand reads a role-supplied full command line.
	ModelDeploymentParallelismSourceKindCommand ModelDeploymentParallelismSourceKind = "Command"
	// ModelDeploymentParallelismSourceKindUnmanagedCommand reads a replaced command line the
	// operator synthesized no argument for.
	ModelDeploymentParallelismSourceKindUnmanagedCommand ModelDeploymentParallelismSourceKind = "UnmanagedCommand"
	// ModelDeploymentParallelismSourceKindUnknown means the source produced no trustworthy reading.
	ModelDeploymentParallelismSourceKindUnknown ModelDeploymentParallelismSourceKind = "Unknown"
)

// ModelDeploymentLoadBalance is the balance shape declared parallel degrees derive to.
// +enum
type ModelDeploymentLoadBalance string

const (
	// ModelDeploymentLoadBalanceInternal balances inside one engine process.
	ModelDeploymentLoadBalanceInternal ModelDeploymentLoadBalance = "Internal"
	// ModelDeploymentLoadBalanceExternal balances outside the engine, e.g. through an
	// external load balancer the rank layout derives.
	ModelDeploymentLoadBalanceExternal ModelDeploymentLoadBalance = "External"
	// ModelDeploymentLoadBalanceHybrid combines an internal and an external leg.
	ModelDeploymentLoadBalanceHybrid ModelDeploymentLoadBalance = "Hybrid"
	// ModelDeploymentLoadBalanceMultiPort balances across several engine listeners.
	ModelDeploymentLoadBalanceMultiPort ModelDeploymentLoadBalance = "MultiPort"
	// ModelDeploymentLoadBalanceUnknown means the declaration does not derive to a shape.
	ModelDeploymentLoadBalanceUnknown ModelDeploymentLoadBalance = "Unknown"
)

// ModelDeploymentRoleEndpointsStatus is one role's endpoint eligibility and actual serving
// confirmation.
//
// nil IS NOT ZERO here, and neither is the reverse: a nil eligible count says the qualified
// set was never observed, an eligible 0 says it was observed and is empty, and only a
// Confirmed serving state may carry a value. The condition EndpointEligibility says at the
// deployment level when the per-role counts below are unobserved rather than empty.
type ModelDeploymentRoleEndpointsStatus struct {
	// Eligible is how many endpoints the role's qualification list holds, set only when that
	// list is complete. It is named for eligibility and never presents itself as serving.
	//
	// +optional
	Eligible *int32 `json:"eligible,omitempty" protobuf:"varint,1,opt,name=eligible"`

	// Serving is the actual serving confirmation taken over the role's endpoints.
	//
	// +optional
	Serving ModelDeploymentServingStatus `json:"serving" protobuf:"bytes,2,name=serving"`
}

// ModelDeploymentServingStatus is the actual serving confirmation of a role's endpoints: a
// deduplicated union over complete, identity-bound per-Router views. It is an observation,
// never a restatement of the eligible count.
type ModelDeploymentServingStatus struct {
	// State is the confirmation answer. Only Confirmed means the union was observed; the other
	// states differ in WHY no number is offered, and none of them encodes one.
	//
	// +k8s:validation:enum=["Confirmed","NotConverged","Unknown","NotConfigured"]
	State ModelDeploymentServingState `json:"state" protobuf:"bytes,1,name=state,casttype=ModelDeploymentServingState"`

	// Value is the confirmed serving count, present only when State is Confirmed, with an
	// explicit zero as real as any other number.
	//
	// +optional
	Value *int32 `json:"value,omitempty" protobuf:"varint,2,opt,name=value"`
}

// ModelDeploymentServingState is the confirmation answer of the per-Router serving union.
// +enum
type ModelDeploymentServingState string

const (
	// ModelDeploymentServingStateConfirmed means complete, fresh, identity-matched views agree on Value.
	ModelDeploymentServingStateConfirmed ModelDeploymentServingState = "Confirmed"
	// ModelDeploymentServingStateNotConverged means complete views disagree about the target.
	ModelDeploymentServingStateNotConverged ModelDeploymentServingState = "NotConverged"
	// ModelDeploymentServingStateUnknown means a required view is missing, stale or indeterminate.
	ModelDeploymentServingStateUnknown ModelDeploymentServingState = "Unknown"
	// ModelDeploymentServingStateNotConfigured means the deployment declares no router at all,
	// so there is no Router process whose answer could be missing.
	ModelDeploymentServingStateNotConfigured ModelDeploymentServingState = "NotConfigured"
)

// ModelDeploymentKVCacheStatus is the reuse domain this deployment attached to.
//
// Every field is READ FROM THE BINDING, never declared here. It is echoed onto this object so that
// diagnosing a cache that is not shared takes one object rather than two — a wrong block size or
// dtype is silent cache pollution, where writes succeed, reads succeed and the tensors are wrong.
type ModelDeploymentKVCacheStatus struct {
	// Binding is the KVCachePoolBinding this deployment resolved, in this namespace.
	//
	// +required
	Binding string `json:"binding" protobuf:"bytes,1,name=binding"`

	// Pool is the KVCachePool that Binding points at.
	//
	// +required
	Pool string `json:"pool" protobuf:"bytes,2,name=pool"`

	// Domain is the reuse identity, echoed from the Binding's immutable domain block.
	//
	// +required
	Domain ModelDeploymentKVCacheDomain `json:"domain" protobuf:"bytes,3,name=domain"`
}

// ModelDeploymentKVCacheDomain is the reuse identity echoed from the Binding.
type ModelDeploymentKVCacheDomain struct {
	// Name is the reuse identity, and it is the storage layer's tenant. Two deployments echoing the
	// same name share KV; two echoing different names do not.
	//
	// +required
	// +k8s:validation:maxLength=63
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// BlockSize and Dtype are what this domain's blocks are made of. They are echoed rather than
	// validated here: the Binding requires and freezes both, so an entry missing one could only be
	// a writer bug.
	//
	// +required
	BlockSize int32 `json:"blockSize" protobuf:"varint,2,name=blockSize"`

	// +required
	Dtype string `json:"dtype" protobuf:"bytes,3,name=dtype"`
}

// ModelDeploymentRouterStatus is the contract a router is configured from.
//
// EVERY STRING HERE IS ONE THE OPERATOR ACTUALLY RENDERED, never a default written down in
// documentation. A consumer reading this object and the router the operator configured therefore
// cannot disagree about a selector, a topic or a metric name, which is what makes this object worth
// publishing rather than a restatement of what the documentation already says.
type ModelDeploymentRouterStatus struct {
	// Name echoes the spec, so that a reader holding only this object knows which implementation the
	// contract below was shaped for.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Endpoint is the router's own address, present once the router's Service has one.
	//
	// +k8s:validation:maxLength=512
	Endpoint string `json:"endpoint,omitempty" protobuf:"bytes,2,opt,name=endpoint"`

	// PoolEndpoint is the address of the KV cache pool this deployment attached to, in the form its
	// client takes. A router that consults the pool needs it, and resolving it from the Binding is
	// work this operator has already done.
	//
	// +k8s:validation:maxLength=512
	PoolEndpoint string `json:"poolEndpoint,omitempty" protobuf:"bytes,3,opt,name=poolEndpoint"`

	// Roles is one entry per declared role, and its key set EQUALS the role set. A router discovers
	// live replicas for itself; what it cannot discover is which selector names which half of a pair.
	//
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentRouterRoleStatus `json:"roles,omitempty" protobuf:"bytes,4,rep,name=roles"`

	// Metrics are the serving metrics the roles expose and the port they are served on. They are
	// deployment-wide because they are a property of the engine, which is a deployment-wide field.
	Metrics *ModelDeploymentRouterMetrics `json:"metrics,omitempty" protobuf:"bytes,5,opt,name=metrics"`
}

// ModelDeploymentRouterRoleStatus is one role as a router sees it.
type ModelDeploymentRouterRoleStatus struct {
	// Name is the role's name, matching spec.roles[].name.
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Kind is the role's effective kind, which is what tells a prefiller from a decoder. It is the
	// resolved value rather than the field, because a role naming no kind is a server and a consumer
	// matching on the empty string would find nothing.
	Kind ModelDeploymentRoleKind `json:"kind" protobuf:"bytes,2,name=kind,casttype=ModelDeploymentRoleKind"`

	// Selector is the label selector matching exactly this role's Pods that answer the API -- one
	// member per replica, its leader, which at size one is the replica's only Pod -- published
	// VERBATIM so that a router is configured from observed strings rather than from a documented
	// convention.
	//
	// The unit it names is the Pod, not the replica: a replica may span several Pods, and a member
	// other than the leader serves no API even though the role runs it, so it stays outside what
	// this selector matches.
	//
	// A router given a selector survives scaling; a router given a list of addresses does not, and
	// would have to be reconfigured and restarted every time a role grew or shrank.
	Selector map[string]string `json:"selector,omitempty" protobuf:"bytes,3,rep,name=selector"`

	// Endpoint is this role's own address, which stays reachable whether or not a router fronts the
	// deployment, so that one half of a pair can be addressed directly while debugging.
	//
	// +k8s:validation:maxLength=512
	Endpoint string `json:"endpoint,omitempty" protobuf:"bytes,4,opt,name=endpoint"`

	// KVEvents is where this role publishes its cache events, absent on a role configured not to
	// publish.
	KVEvents *ModelDeploymentRouterKVEvents `json:"kvEvents,omitempty" protobuf:"bytes,5,opt,name=kvEvents"`
}

// ModelDeploymentRouterKVEvents is one role's cache-event stream, as something a consumer can reach.
//
// A BIND ADDRESS IS NOT A DIALABLE ADDRESS. The engine is configured with what its publisher binds,
// which names no host; these are the addresses a consumer connects to. Publishing the bind string
// here would be publishing a value that works nowhere but inside the publishing Pod.
type ModelDeploymentRouterKVEvents struct {
	// Endpoint is the stream a consumer subscribes to.
	//
	// +k8s:validation:maxLength=512
	Endpoint string `json:"endpoint" protobuf:"bytes,1,name=endpoint"`

	// ReplayEndpoint is where a consumer that joined late asks for the events it missed. A consumer
	// without it starts with an empty view of a cache that is not empty.
	//
	// +k8s:validation:maxLength=512
	ReplayEndpoint string `json:"replayEndpoint,omitempty" protobuf:"bytes,2,opt,name=replayEndpoint"`

	// Topic is the topic the publisher was configured with. A consumer subscribing to a different one
	// receives nothing and reports no error.
	Topic string `json:"topic,omitempty" protobuf:"bytes,3,opt,name=topic"`
}

// ModelDeploymentRouterMetrics are the serving metrics a router scores on.
//
// THE NAMES ARE PUBLISHED RATHER THAN ASSUMED because they are the engine's, and this operator knows
// which engine it configured. A router holding a name that engine does not expose scores every
// replica identically and reports nothing wrong.
//
// Their presence here says the operator configured an engine that exposes them. It does NOT say they
// are reachable from where a router runs, and it cannot: a role may name any image.
type ModelDeploymentRouterMetrics struct {
	// Port is the port the metrics are served on. The lower bound is not decoration: unlike the names
	// beside it, this is a number a consumer dials, and a zero would fail only at connect time.
	//
	// +required
	// +k8s:validation:minimum=1
	Port int32 `json:"port" protobuf:"varint,1,name=port"`

	// QueuedRequests names the metric holding requests waiting to be admitted by the engine.
	QueuedRequests string `json:"queuedRequests" protobuf:"bytes,2,name=queuedRequests"`

	// RunningRequests names the metric holding requests the engine is currently serving.
	RunningRequests string `json:"runningRequests" protobuf:"bytes,3,name=runningRequests"`

	// KVCacheUtilization names the metric holding how full the engine's KV cache is.
	KVCacheUtilization string `json:"kvCacheUtilization" protobuf:"bytes,4,name=kvCacheUtilization"`
}

// ModelDeploymentList holds the list of ModelDeployment.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelDeployment `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentList)(nil)
