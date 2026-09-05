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
// It RENDERS PODS DIRECTLY. The admission chain keys on Pods — a plain Pod is a first-class citizen
// of it and an Instance is sugar that renders one — so rendering Pods reuses every existing gate
// with no new integration point. Instance could not serve as the substrate: it renders exactly one
// Pod, and its spec is immutable after creation, which turns a rolling update into
// recreate-everything.
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
	// It does NOT decide the connector, which follows the role's hardware instead. An Ascend pool
	// running this engine gets a different connector than an NVIDIA pool running it, because the
	// connector is a property of the accelerator backend.
	//
	// +required
	// +k8s:validation:enum=["vllm","sglang"]
	Engine string `json:"engine" protobuf:"bytes,2,name=engine"`

	// EngineVersion is the engine's own version, e.g. "0.25.1" for vllm or "0.5.18" for sglang.
	//
	// It is free-form and UNVALIDATED, by decision. Together with each role's observed hardware it
	// assembles that role's runner image; the operator checks neither that the combination was ever
	// published nor that the version supports the installed driver. The user guarantees version
	// alignment. A gate would need the runner's release matrix compiled into the operator, and the
	// failure it would prevent is already legible without one, as an ImagePullBackOff on a tag that
	// does not exist.
	//
	// It is per deployment rather than per role, which is what lets one engine and one version
	// assemble a DIFFERENT image for each role: the backend half of the tag comes from the role's
	// own InstanceType. A prefill role on NVIDIA and a decode role on Ascend therefore need no extra
	// field. That works because the published version sets overlap across backends, which is
	// measured rather than assumed - though not across ALL of them, so a per-role override is a
	// thing the P/D spec may need and this one does not.
	//
	// The lower bound is not decoration: `required` makes the key present, not the value non-empty,
	// and an empty version assembles a malformed tag whose ImagePullBackOff names a tag the user
	// never typed.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=64
	EngineVersion string `json:"engineVersion" protobuf:"bytes,5,name=engineVersion"`

	// KVCache attaches the deployment to a KV cache pool.
	//
	// +required
	KVCache ModelDeploymentKVCache `json:"kvCache" protobuf:"bytes,3,name=kvCache"`

	// Roles are the engine roles this deployment runs.
	//
	// It is a LIST FROM THE FIRST VERSION although only one entry is accepted today, because the
	// spec that introduces P/D disaggregation adds entries to it rather than replacing the field.
	// The length-1 bound lives in the validating webhook and not here, so that the refusal can name
	// the spec that lifts it, and so that lifting it is a webhook edit rather than a schema change
	// every stored object would have to survive.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentRole `json:"roles" protobuf:"bytes,4,rep,name=roles"`
}

// The engines a ModelDeployment can run, which are the values of ModelDeploymentSpec.Engine's enum.
//
// They are declared beside the field whose schema closes the set, so that a reader of either finds
// the other, and so that a value outside the set cannot reach the operator through this API.
// There is deliberately no "vllm-ascend" value. `vllm_ascend` is a Python package the runner
// installs when the accelerator backend is CANN, not an engine a user picks: the runner's own
// release matrix spells the service `vllm` for every Ascend image. Naming it here made the
// connector look like a property of the engine, which it is not - it varies with the accelerator
// backend, so the engine alone never settles what the operator injects.
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
	// from the pool's backend type and the engine. The enum has one value on purpose: it reserves
	// the discriminator, so naming a specific connector later is an enum widening rather than a new
	// field. There is no "none" — synthesizing nothing is reachable through a full command
	// replacement, which also marks the role unmanaged and moves CacheAttached to Unknown.
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
	// Name identifies the role. In this version there is exactly one role and its name is free-form;
	// the spec that introduces P/D disaggregation gives the name meaning.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=63
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Replicas is how many Pods this role runs. Each is an independent Kueue Workload: this version
	// creates no pod group, which is correct for one role whose replicas are independently useful
	// and is what the P/D spec replaces with cross-role atomic admission.
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
	// resources scaled by the requested card count, exactly as the Instance webhook derives them,
	// so they are not expressible here at all — which is a stronger guarantee than refusing them
	// would be, since a field that does not exist cannot be shadowed by a template either.
	//
	// InstanceType alone cannot supply this. An InstanceType's UnitResources size ONE card, and how
	// many cards a replica wants is a property of the model being served rather than of the pool it
	// is admitted against; two deployments on one InstanceType routinely want different counts.
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

	// Template overlays the rendered container. The operator renders first and merges this on top.
	// A non-empty Command is the TAKE-OVER tier: the user owns the whole argv, the operator
	// synthesizes no engine arguments and no client environment, the role is marked unmanaged and
	// CacheAttached goes to Unknown. Arguments fold into Command; there is deliberately no Args,
	// because a second append tier beside ExtraArgs would have no defined precedence and would make
	// the take-over tier ambiguous — args alone would be neither take-over nor append.
	//
	// The template is MUTABLE, unlike the one an Instance carries. Immutability there is a rule the
	// Instance webhook enforces on InstanceSpec rather than a property of any template type, and not
	// carrying it here is what makes a rollout possible at all.
	//
	// Its Resources are refused at admission. The accelerator request belongs in the role's own
	// Resources and the rest is derived from the InstanceType, so a template able to shadow either
	// would make the admission feasibility check read a ledger that does not match reality.
	Template *ModelDeploymentTemplate `json:"template,omitempty" protobuf:"bytes,7,opt,name=template"`
}

// ModelDeploymentTemplate overlays the container the operator renders for one replica.
//
// IT EXISTS BECAUSE InstanceTemplate'S Image IS REQUIRED AND THIS ONE'S CANNOT BE. A role that
// names no image has one synthesized from the accelerator backend its InstanceType observed, so
// requiring the field would force every user of the overlay to give up synthesis — two capabilities
// this API offers, excluding each other for no reason other than a shared struct. Relaxing the
// marker on InstanceTemplate was rejected: it would move a guarantee the Instance's schema holds
// today down into a webhook, which is later, more expensive and easier to bypass, and it would do
// that to a published API for the convenience of an unpublished one.
//
// The fields are InstanceTemplate's, minus VolumeMount, which nothing here reads — an unused field
// in a schema is a promise, and strict decoding turns leaving it out into a clear refusal rather
// than a value silently ignored. Numbering restarts at 1 and runs contiguously because this type is
// new in an unreleased API; there is nothing on the wire to reserve around.
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

	// Ports are the container ports to expose in addition to the engine's own.
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
	// Accelerator is how many accelerator cards ONE REPLICA asks for. Absent or zero on an
	// acceleratable InstanceType is a CPU-only replica, which is legitimate for a small model.
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

// ModelDeploymentStatus defines the observed state of ModelDeployment.
//
// It is REBUILT FROM OBSERVED STATE ON EVERY RECONCILE, so a stale field cannot survive a
// disagreement with the Pods.
type ModelDeploymentStatus struct {
	// Phase summarizes the conditions: Starting, Ready, Degraded, Deleting. Ready means every role's
	// ready count equals its desired count; Degraded means at least one replica is ready and at
	// least one is not.
	Phase string `json:"phase,omitempty" protobuf:"bytes,1,opt,name=phase"`

	// PhaseMessage carries the reason for the phase.
	PhaseMessage string `json:"phaseMessage,omitempty" protobuf:"bytes,2,opt,name=phaseMessage"`

	// Conditions is the finer view, one condition per axis: DomainRegistered, QuotaReserved,
	// CacheAttached. The three are independent — "quota reserved but cache not attached" is a real
	// and actionable state — which is what a single phase string cannot carry.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"` // nolint: lll

	// Endpoint is the one address every replica serves behind, in the form
	// http://<name>.<namespace>.svc:<port>. It is absent until the Service has an address.
	//
	// +k8s:validation:maxLength=512
	Endpoint string `json:"endpoint,omitempty" protobuf:"bytes,4,opt,name=endpoint"`

	// Roles is one entry per declared role.
	//
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentRoleStatus `json:"roles,omitempty" protobuf:"bytes,5,rep,name=roles"`

	// KVCache is the reuse domain this deployment actually attached to, read from the Binding. It
	// exists so an operator can tell a cache-sharing misconfiguration from a cache that is merely
	// cold by reading this object alone.
	//
	// A POINTER because omitempty does not omit a zero-valued struct: held by value it would
	// serialize as an empty object on every pass where the Binding could not be resolved, which a
	// reader cannot tell from a domain whose every field happens to be empty.
	KVCache *ModelDeploymentKVCacheStatus `json:"kvCache,omitempty" protobuf:"bytes,6,opt,name=kvCache"`
}

// ModelDeploymentRoleStatus is one role's observed readiness.
type ModelDeploymentRoleStatus struct {
	// Name is the role this entry describes.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Desired is how many Pods the spec asks for, and Ready is how many of them are Ready. Neither
	// carries omitempty: they are counted from a Pod list that succeeded, so a zero here is an
	// observed zero and must serialize as one. A failed list writes no status at all.
	Desired int32 `json:"desired" protobuf:"varint,2,name=desired"`

	Ready int32 `json:"ready" protobuf:"varint,3,name=ready"`

	// Unmanaged is true when the role replaced the whole command line, so the operator synthesized
	// no engine argument and no client environment for it. It carries no omitempty for the same
	// reason the counts do not: false is the ordinary case and must be visible as an answer rather
	// than as a missing field.
	Unmanaged bool `json:"unmanaged" protobuf:"varint,4,name=unmanaged"`
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

// ModelDeploymentList holds the list of ModelDeployment.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelDeployment `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentList)(nil)
