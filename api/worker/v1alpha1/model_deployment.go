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
	ModelDeploymentEngineVLLM   = "vllm"
	ModelDeploymentEngineSGLang = "sglang"
)

// ModelDeploymentModel names the model the engine serves.
//
// It provisions nothing. Weights arrive through the role's additional volumes or through the engine's
// own hub client; a weight-provisioning block here would be the first step towards the
// general-purpose serving CR this deliberately is not.
type ModelDeploymentModel struct {
	// Name is the identifier the engine serves, e.g. "Qwen/Qwen2.5-72B-Instruct".
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`
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
	// +k8s:validation:enum=["vllm","sglang"]
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Version is the engine's own version, e.g. "0.25.1" for vllm or "0.5.18" for sglang.
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
	// an identity the deployment carries, not a setting the operator derives: "mooncake" says which
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
	// +k8s:validation:default="mooncake"
	// +k8s:validation:enum=["mooncake"]
	Connector string `json:"connector,omitempty" protobuf:"bytes,2,opt,name=connector"`
}

// ModelDeploymentKVTransfer carries the settings of the point-to-point KV transfer leg
// between a prefill role and a decode role. It composes with a shared store rather than excluding
// one: both legs may be configured on one deployment, and the synthesized connector carries the
// pair together.
type ModelDeploymentKVTransfer struct {
	// Protocol is the transport both ends of the leg are told to use, in the mooncake
	// configuration's own spelling, e.g. "tcp" or "rdma".
	//
	//   - IT IS DEPLOYMENT-WIDE ON PURPOSE. The protocol is a property of the link, not of either
	//     end, so a per-role field could only express a contradiction -- two ends naming different
	//     values for one connection, which fails at transfer time rather than at admission.
	//   - THE VALUE IS DECLARED, NOT DISCOVERED, AND IT IS NOT GATED. The accepted set is a
	//     property of the mooncake build inside the engine's own image, which this operator
	//     neither ships nor can inspect: a HIP-compiled build makes "hip" a working point-to-point
	//     transport, and an enum here would hard-code one image's compile set onto another image's
	//     connector. The value is passed through verbatim, and a value the engine build rejects
	//     raises at engine startup, in the container that owns the fact.
	//   - UNSET RENDERS "tcp", the transport every mooncake build carries. The default lives in
	//     the renderer rather than in this schema, so the stored object holds exactly what was
	//     asked.
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
	// +k8s:validation:maxLength=64
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
// docs/reference/model-deployment.md under "One group per replica" and "Rollout is a rolling
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
	// +k8s:validation:default="server"
	// +k8s:validation:enum=["server","prefill","decode"]
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
	ModelDeploymentRoleKindServer ModelDeploymentRoleKind = "server"
	// ModelDeploymentRoleKindPrefill is a role that computes the prompt's KV blocks and hands them
	// on rather than decoding them itself.
	ModelDeploymentRoleKindPrefill ModelDeploymentRoleKind = "prefill"
	// ModelDeploymentRoleKindDecode is a role that consumes KV blocks a prefiller produced and
	// generates tokens from them.
	ModelDeploymentRoleKindDecode ModelDeploymentRoleKind = "decode"
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
	// CacheAttached, ReplicasUpToDate, RoleKindsReady, KVEventsPublishing, RouterReady. They are
	// independent —
	// "quota reserved but cache not attached" is a real and actionable state — which is what a single
	// phase string cannot carry.
	//
	// KVEventsPublishing reports rendered configuration rather than observing the stream. A publisher
	// that was configured and then crashed therefore remains True until a live consumer observes it.
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
	// It is ABSENT while the Binding cannot be resolved, rather than present and empty: an empty
	// object here would be indistinguishable from a domain whose every field happens to be empty.
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
}

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
	// +k8s:validation:enum=["server","prefill","decode"]
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
}

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
