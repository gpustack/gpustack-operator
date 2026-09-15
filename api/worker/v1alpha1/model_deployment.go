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
// +k8s:crd-gen:printcolumn:name="Engine",type="string",jsonPath=".spec.engine"
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

	// Engine selects the inference engine, which decides which argument keys the operator owns and
	// which carrier the transfer configuration arrives on. Ownership is per (engine, key): a key one
	// engine owns is an ordinary user argument on another.
	//
	// It does NOT decide the connector, which follows the role's hardware instead: the connector is a
	// property of the accelerator backend, so an Ascend pool and an NVIDIA pool running this engine
	// get different ones.
	//
	// +required
	// +k8s:validation:enum=["vllm","sglang"]
	Engine string `json:"engine" protobuf:"bytes,2,name=engine"`

	// EngineVersion is the engine's own version, e.g. "0.25.1" for vllm or "0.5.18" for sglang.
	//
	//   - It is free-form and UNVALIDATED, by decision: the user guarantees that the version and the
	//     driver each role's hardware installed are aligned. A gate would need the runner's release
	//     matrix compiled into the operator, and the failure it would prevent is already legible as
	//     an ImagePullBackOff on a tag that does not exist.
	//   - It is per deployment rather than per role, which is what lets one version assemble a
	//     DIFFERENT image for each role: the backend half of the tag comes from the role's own
	//     InstanceType, so a prefill role on NVIDIA and a decode role on Ascend need no extra field.
	//     Published version sets do NOT overlap across every backend, so one version has to name a
	//     tag that exists for each backend the roles land on.
	//   - The lower bound is not decoration: `required` makes the key present, not the value
	//     non-empty, and an empty version assembles a malformed tag naming something never typed.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=64
	EngineVersion string `json:"engineVersion" protobuf:"bytes,5,name=engineVersion"`

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
	// NOTHING RENDERS A ROUTER YET. The field is accepted and the rules stated on it are applied, but
	// no Deployment, ConfigMap or Service is created from it and status.endpoint does not move. Every
	// sentence below describes the contract this field commits to, not behavior already in place, and
	// each says which of the two it is where that is not obvious.
	//
	// THE PARAGRAPH ABOVE EXPIRES WHOLE, on the first change that renders anything from this field.
	// Delete it then, rather than editing it down: whoever writes that renderer is the one reader
	// guaranteed to be looking here, and a paragraph trimmed clause by clause becomes a list of what
	// is still missing, which is the thing nobody keeps current.
	//
	// IT IS EAST-WEST TRAFFIC MANAGEMENT, NOT A PREFILL/DECODE PAIRER, and the distinction decides
	// which shapes are legal behind it. Several plain servers is one of them: a router that scores on
	// a cache view picks between equals in a way a Service cannot, so "there is no pair here" is not a
	// reason to refuse one. Several prefillers with several decoders is another. A rule admitting only
	// one prefiller and one decoder would describe a pairer rather than this field.
	//
	// Absent means no router, and that stays a supported shape rather than a broken one: the roles are
	// individually addressable through their own Services either way, so a deployment written before
	// this field existed serves exactly as it did.
	//
	// +optional
	Router *ModelDeploymentRouter `json:"router,omitempty" protobuf:"bytes,6,opt,name=router"`
}

// The engines a ModelDeployment can run, which are the values of ModelDeploymentSpec.Engine's enum.
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
// It provisions nothing. Weights arrive through the role template's volumes or through the engine's
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

	// Connector selects how the engine's transfer configuration is produced. "auto" synthesizes it
	// from the pool's backend type and the engine. There is no "none" — synthesizing nothing is
	// reachable through a full command replacement, which also marks the role unmanaged and moves
	// CacheAttached to Unknown.
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
	// +k8s:validation:default="auto"
	// +k8s:validation:enum=["auto"]
	Connector string `json:"connector,omitempty" protobuf:"bytes,2,opt,name=connector"`
}

// ModelDeploymentRole is one engine role and its replicas.
//
// Replicas and InstanceType are STRUCTURED FIELDS AND MUST STAY SO. They are inputs to admission and
// scheduling — Kueue PodSet counts and flavor selection — so a template that could shadow them would
// make the admission feasibility check read a ledger that does not match reality. The template may
// override container content and nothing else.
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

	// Replicas is how many Pods this role runs. They are NOT independent Workloads: every replica of
	// every role joins one Kueue pod group, so the deployment is admitted as a unit or not at all.
	//
	// CHANGING THIS NUMBER REBUILDS THE GROUP. It moves the total the group declares, which every Pod
	// carries and which Kueue requires them all to agree on, so the operator deletes the group's Pods
	// and recreates them under the new total rather than adding or trimming a few. A replica that
	// leaves loses its cached blocks to its siblings.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	Replicas int32 `json:"replicas,omitempty" protobuf:"varint,2,opt,name=replicas"`

	// InstanceType is the name of the InstanceType whose pool this role's Pods are admitted against.
	// It is what the queue-name entrance label is derived from.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	InstanceType string `json:"instanceType" protobuf:"bytes,3,name=instanceType"`

	// Resources is what one replica of this role asks of an accelerator, and it is a STRUCTURED
	// FIELD FOR THE SAME REASON Replicas and InstanceType are: admission and scheduling read it.
	//
	// It carries only the ACCELERATOR half of a request, because that is the only half a workload
	// decides. CPU, memory and ephemeral storage are DERIVED from the InstanceType's per-unit
	// resources scaled by the requested card count, so they are not expressible here at all — a
	// stronger guarantee than refusing them, since a field that does not exist cannot be shadowed by
	// a template either.
	//
	// InstanceType alone cannot supply this half: its UnitResources size ONE card, and how many cards
	// a replica wants is a property of the model being served, so two deployments on one InstanceType
	// routinely want different counts.
	Resources *ModelDeploymentRoleResources `json:"resources,omitempty" protobuf:"bytes,4,opt,name=resources"`

	// ExtraArgs is appended AFTER the operator-synthesized arguments. An entry naming a key the
	// operator owns is REJECTED rather than merged: a silent merge produces two values for one
	// connector argument and no way to tell which one won.
	//
	// +listType=atomic
	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// Env is appended the same way and refused on the same terms. Keys the operator merely defaults
	// are not owned: a user's value wins there and no rejection follows.
	//
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	Env []InstanceEnvVar `json:"env,omitempty" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,6,rep,name=env"`

	// Template overlays the rendered container: the operator renders first and merges this on top.
	//
	//   - A non-empty Command is the TAKE-OVER tier — the user owns the whole argv, the operator
	//     synthesizes no engine arguments and no client environment, the role is marked unmanaged and
	//     CacheAttached goes to Unknown. Arguments fold into Command; there is deliberately no Args,
	//     because a second append tier beside ExtraArgs would have no defined precedence.
	//   - It is MUTABLE, unlike the one an Instance carries, which is what makes a rollout possible
	//     at all.
	//   - EDITING IT RESTARTS EVERY ROLE, not just the replicas this template belongs to: every
	//     replica of the deployment is one member of a single Kueue pod group whose members cannot
	//     leave one at a time, so the group is rebuilt whole. The same is true of a `replicas` change,
	//     of adding or removing a role, and of a departure this operator did not initiate — see
	//     docs/reference/model-deployment.md under "Rollout is recreate".
	//   - Its Resources are refused at admission. The accelerator request belongs in the role's own
	//     Resources and the rest is derived from the InstanceType, so a template able to shadow either
	//     would make the admission feasibility check read a ledger that does not match reality.
	Template *ModelDeploymentTemplate `json:"template,omitempty" protobuf:"bytes,7,opt,name=template"`

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

// ModelDeploymentTemplate overlays the container the operator renders for one replica.
//
// IT EXISTS BECAUSE InstanceTemplate'S Image IS REQUIRED AND THIS ONE'S CANNOT BE: a role that names
// no image has one synthesized from the accelerator backend its InstanceType observed, so requiring
// the field would force every user of the overlay to give up synthesis.
//
// The fields are InstanceTemplate's, minus VolumeMount, which nothing here reads: an unused field in
// a schema is a promise, and strict decoding turns leaving it out into a clear refusal rather than a
// value silently ignored.
type ModelDeploymentTemplate struct {
	// Image is the container image to run. Leaving it empty is the ordinary case: the operator then
	// synthesizes one from the pool's accelerator backend, the observed runtime version and the
	// requested engine.
	//
	// +optional
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,1,opt,name=image"`

	// ImagePullPolicy is the pull policy for Image.
	//
	// +optional
	ImagePullPolicy core.PullPolicy `json:"imagePullPolicy,omitempty" protobuf:"bytes,2,opt,name=imagePullPolicy"`

	// Command replaces the whole argv, which is the TAKE-OVER tier described on the role's Template
	// field. The operator contributes no engine argument and no client environment.
	//
	// +optional
	Command []string `json:"command,omitempty" protobuf:"bytes,3,rep,name=command"`

	// Privileged runs the container privileged.
	//
	// +optional
	Privileged bool `json:"privileged,omitempty" protobuf:"varint,4,opt,name=privileged"`

	// Ports are the container ports to expose in addition to the engine's own. They do not
	// reserve or select the transfer engine's runtime port window.
	//
	// +optional
	// +patchMergeKey=port
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=port
	// +listMapKey=protocol
	Ports []InstancePort `json:"ports,omitempty" patchStrategy:"merge" patchMergeKey:"port" protobuf:"bytes,5,rep,name=ports"` // nolint: lll

	// Env are environment entries merged on top of the role's own. A name the operator owns is
	// refused here just as it is in the role's Env: the renderer drops owned names from both tiers,
	// so admission has to refuse both, or one path becomes a silent drop.
	//
	// +optional
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	Env []InstanceEnvVar `json:"env,omitempty" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,6,rep,name=env"`

	// Resources is present ONLY so that supplying it can be refused with a message that says where
	// the request belongs. Dropping the field would let strict decoding refuse it earlier and more
	// cheaply, but an unknown-field error says "not here" while the webhook's says "it goes in the
	// role's own Resources" — and mistaking the template for the place resources live is the whole
	// reason anyone writes this field.
	//
	// +optional
	Resources *InstanceResources `json:"resources,omitempty" protobuf:"bytes,7,opt,name=resources"`

	// ImagePullSecret is the secret used to pull Image.
	//
	// +optional
	ImagePullSecret *core.LocalObjectReference `json:"imagePullSecret,omitempty" protobuf:"bytes,8,opt,name=imagePullSecret"`

	// AdditionalVolumes are volumes mounted into the container alongside the operator's own.
	//
	// +optional
	// +listType=atomic
	AdditionalVolumes []InstanceAdditionalVolume `json:"additionalVolumes,omitempty" protobuf:"bytes,9,rep,name=additionalVolumes"` // nolint: lll
}

// ModelDeploymentRoleResources is what one replica of a role asks of an accelerator.
//
// It deliberately mirrors the accelerator fields of InstanceResources — the same names, the same
// meanings — rather than inventing a second vocabulary for one request, and it deliberately omits
// that type's CPU, RAM and LocalStorage, which are derived here rather than declared.
type ModelDeploymentRoleResources struct {
	// Accelerator is how many accelerator cards ONE REPLICA asks for.
	//
	//   - Left unset on an acceleratable InstanceType it DEFAULTS TO ONE at admission, on create and
	//     on update alike, the same way an Instance's does. The value is written into the stored
	//     object rather than applied at render time, so what was admitted is what can be read back.
	//   - AN EXPLICIT ZERO IS KEPT, because it is a value the user wrote, and on an acceleratable
	//     InstanceType it asks for nothing that pool's queue accounts in. It is accepted while it is
	//     the only role using that type, and refused when another role shares the type because the
	//     resulting multi-PodSet Workload cannot be admitted by that queue.
	//   - A replica meant to run without an accelerator belongs on an InstanceType that is not
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
	// outside tool understands, not terms this operator invents. "llm-d" is how that project spells
	// itself in its module path, its API group and its label domain, so it is spelled that way here.
	//
	// ONE VALUE TODAY IS A CHOICE TAKEN FOR NOW, NOT THE ABSENCE OF ONE. This field exists ahead of a
	// second implementation precisely so that adding one is a widening of this enum rather than a new
	// field appearing on an API that already shipped without it.
	//
	// WIDENING IT IS FOUR THINGS, NOT ONE: one entry here, one configuration renderer, the object set
	// that router needs, AND the wiring that threads this value to a dispatch point. The schema
	// reservation covers the first of those and nothing else, which is why a second router is a piece
	// of work rather than a constant.
	//
	// +required
	// +k8s:validation:enum=["llm-d"]
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
	// the same way a role's image is assembled when its template names none.
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
}

// The routers a ModelDeployment can name, which are the values of ModelDeploymentRouter.Name's enum.
//
// Declared beside the field whose schema closes the set, so that a reader of either finds the other.
const (
	ModelDeploymentRouterLLMD = "llm-d"
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
}

// ModelDeploymentRoleStatus is one role's observed readiness.
type ModelDeploymentRoleStatus struct {
	// Name is the role this entry describes.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Desired is how many Pods the spec asks for, and Ready is how many of them are Ready. Both are
	// ALWAYS present: they are counted from a Pod list that succeeded, so a zero here is an observed
	// zero. A failed list writes no status at all.
	Desired int32 `json:"desired" protobuf:"varint,2,name=desired"`

	Ready int32 `json:"ready" protobuf:"varint,3,name=ready"`

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

	// AssignedFlavor is the ResourceFlavor Kueue assigned to this role's PodSet for its ACCELERATOR
	// credits.
	//
	//   - NOT ASSIGNED YET AND ASSIGNED ARE DIFFERENT FACTS, so a role waiting for quota reports no
	//     flavor at all rather than an empty name, which would read as an assignment to a flavor
	//     called "".
	//   - Per role rather than per deployment, because Kueue assigns a flavor per PodSet and two
	//     roles of one deployment can be assigned different ones.
	//   - AN ADMITTED ROLE MAY STILL REPORT NOTHING HERE, and that is the field's contract rather
	//     than a gap in it. The answer is read through the same function the per-accelerator
	//     admission gate uses, which speaks only of accelerator credits, so a role admitted on a pool
	//     carrying no accelerator names a flavor for `cpu` and nothing here. The two answers are kept
	//     identical on purpose: a flavor reported here that the gate would not fit against would be
	//     worse than none.
	AssignedFlavor *string `json:"assignedFlavor,omitempty" protobuf:"bytes,6,opt,name=assignedFlavor"`
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

	// Selector is the label selector matching exactly this role's replicas, published VERBATIM so
	// that a router is configured from observed strings rather than from a documented convention.
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
