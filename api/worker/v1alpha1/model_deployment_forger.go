package v1alpha1

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

const (
	// ModelDeploymentForgerBindingAnnotationKey names the annotation on a target ModelDeployment
	// carrying the single-writer lease for kernel artifact promotion.
	ModelDeploymentForgerBindingAnnotationKey = "modeldeployment.gpustack.ai/forger-binding"

	// ModelDeploymentForgerDecisionAnnotationKey names the annotation on a target ModelDeployment
	// recording the last kernel artifact decision committed by the bound forger.
	ModelDeploymentForgerDecisionAnnotationKey = "modeldeployment.gpustack.ai/forger-decision"

	// ModelDeploymentForgerFinalizerKey names the finalizer a ModelDeploymentForger holds on itself
	// while bound to a target or managing sandbox and artifact cleanup.
	ModelDeploymentForgerFinalizerKey = "worker.gpustack.ai/model-deployment-forger-protection"
)

// Condition types reported by ModelDeploymentForger.
const (
	// ModelDeploymentForgerConditionTypeBound reports whether the forger owns the forger-binding
	// on its target ModelDeployment.
	ModelDeploymentForgerConditionTypeBound = "Bound"

	// ModelDeploymentForgerConditionTypeActive reports whether the forger is permitted to promote
	// forged kernel artifacts onto its target.
	ModelDeploymentForgerConditionTypeActive = "Active"

	// ModelDeploymentForgerConditionTypeBlocked reports whether a transient runtime, quota, or
	// storage condition currently holds probing or promotion.
	ModelDeploymentForgerConditionTypeBlocked = "Blocked"

	// ModelDeploymentForgerConditionTypeReady reports the overall closed-loop lifecycle health of
	// the forger.
	ModelDeploymentForgerConditionTypeReady = "Ready"
)

// Condition and generation reasons carried by ModelDeploymentForger.
const (
	// ModelDeploymentForgerReasonBound says the forger holds the target's forger-binding lease, or
	// validated its target in non-mutating modes.
	ModelDeploymentForgerReasonBound = "Bound"

	// ModelDeploymentForgerReasonTargetNotFound says the target ModelDeployment does not exist.
	ModelDeploymentForgerReasonTargetNotFound = "TargetNotFound"

	// ModelDeploymentForgerReasonAlreadyBound says another ModelDeploymentForger owns the target.
	ModelDeploymentForgerReasonAlreadyBound = "AlreadyBound"

	// ModelDeploymentForgerReasonTargetReplaced says the target ModelDeployment was deleted and
	// recreated with a new UID.
	ModelDeploymentForgerReasonTargetReplaced = "TargetReplaced"

	// ModelDeploymentForgerReasonInvalidBinding says the target's forger-binding annotation is
	// unreadable or corrupt.
	ModelDeploymentForgerReasonInvalidBinding = "InvalidBinding"

	// ModelDeploymentForgerReasonActive says the forger runs in Active mode and is not suspended.
	ModelDeploymentForgerReasonActive = "Active"

	// ModelDeploymentForgerReasonShadowMode says the forger probes, builds, verifies, and publishes
	// kernel artifacts without mutating the target ModelDeployment.
	ModelDeploymentForgerReasonShadowMode = "ShadowMode"

	// ModelDeploymentForgerReasonObserveOnlyMode says the forger runs short-window probes and
	// records bottlenecks without launching ForgeSandbox Pods or mutating the target.
	ModelDeploymentForgerReasonObserveOnlyMode = "ObserveOnlyMode"

	// ModelDeploymentForgerReasonSuspended says spec.suspend is true.
	ModelDeploymentForgerReasonSuspended = "Suspended"

	// ModelDeploymentForgerReasonNoBlock says nothing currently holds the forger.
	ModelDeploymentForgerReasonNoBlock = "NoBlock"

	// ModelDeploymentForgerReasonRolePaused says a targeted role is paused or completing pause
	// drain.
	ModelDeploymentForgerReasonRolePaused = "RolePaused"

	// ModelDeploymentForgerReasonRuntimeControlUnsynced says a targeted role has not converged its
	// live runtimeControl state.
	ModelDeploymentForgerReasonRuntimeControlUnsynced = "RuntimeControlUnsynced"

	// ModelDeploymentForgerReasonRetirementInProgress says the target ModelDeployment is executing
	// a replica retirement operation.
	ModelDeploymentForgerReasonRetirementInProgress = "RetirementInProgress"

	// ModelDeploymentForgerReasonSandboxQuotaPending says Kueue has held ForgeSandbox admission
	// past the initial grace window.
	ModelDeploymentForgerReasonSandboxQuotaPending = "SandboxQuotaPending"

	// ModelDeploymentForgerReasonPeerSyncDisabled says Peer Sync is disabled on consumer nodes
	// required for multi-node kernel delivery.
	ModelDeploymentForgerReasonPeerSyncDisabled = "PeerSyncDisabled"

	// ModelDeploymentForgerReasonPinnedCapacityExceeded says a target NodeModelStore reached its
	// 256-entry pin ceiling.
	ModelDeploymentForgerReasonPinnedCapacityExceeded = "PinnedCapacityExceeded"

	// ModelDeploymentForgerReasonIdle says the forger is between passes with no active work.
	ModelDeploymentForgerReasonIdle = "Idle"

	// ModelDeploymentForgerReasonInProgress says a probe, sandbox forge, publish, or rollout pass
	// is currently running.
	ModelDeploymentForgerReasonInProgress = "InProgress"

	// ModelDeploymentForgerReasonRollbackCompleted says the most recent promotion failed canary
	// verification and was rolled back cleanly.
	ModelDeploymentForgerReasonRollbackCompleted = "RollbackCompleted"

	// ModelDeploymentForgerReasonDegraded says the most recent pass encountered an unrecovered
	// error.
	ModelDeploymentForgerReasonDegraded = "Degraded"

	// ModelDeploymentForgerReasonNoEligibleBottlenecks says probing found no unforged single-GPU
	// non-GEMM operator bottlenecks above threshold.
	ModelDeploymentForgerReasonNoEligibleBottlenecks = "NoEligibleBottlenecks"
)

// ModelDeploymentForgerBinding is the JSON payload stored in the forger-binding annotation on a
// target ModelDeployment.
type ModelDeploymentForgerBinding struct {
	// ForgerName is the name of the ModelDeploymentForger that owns the target lease.
	ForgerName string `json:"forgerName" protobuf:"bytes,1,opt,name=forgerName"`

	// ForgerUID is the UID of the ModelDeploymentForger that owns the target lease.
	ForgerUID string `json:"forgerUID" protobuf:"bytes,2,opt,name=forgerUID"`

	// Term is the monotonic binding term under which the owner writes.
	Term int64 `json:"term" protobuf:"varint,3,opt,name=term"`
}

// ModelDeploymentForgerDecision is the JSON payload stored in the forger-decision annotation on a
// target ModelDeployment to allow controller restart recovery without duplicate promotions.
type ModelDeploymentForgerDecision struct {
	// DecisionID identifies the committed promotion or rollback decision.
	DecisionID string `json:"decisionID" protobuf:"bytes,1,opt,name=decisionID"`

	// Term is the binding term under which this decision was committed.
	Term int64 `json:"term" protobuf:"varint,2,opt,name=term"`

	// KernelArtifacts maps each managed role name to the ModelKernelArtifact name committed by
	// this decision, or empty string when rolled back to stock engine kernels.
	KernelArtifacts map[string]string `json:"kernelArtifacts,omitempty" protobuf:"bytes,3,rep,name=kernelArtifacts"`
}

// ModelDeploymentForger automates closed-loop short-window kernel probing, Kueue-scheduled
// single-GPU sandbox synthesis and independent verification, NodeModelStore publication, and
// readiness-gated canary rollout over one ModelDeployment.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mdf"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Target",type="string",jsonPath=".spec.targetRef.name"
// +k8s:crd-gen:printcolumn:name="Mode",type="string",jsonPath=".spec.mode"
// +k8s:crd-gen:printcolumn:name="Bound",type="string",jsonPath=".status.conditions[?(@.type=='Bound')].status"
// +k8s:crd-gen:printcolumn:name="Active",type="string",jsonPath=".status.conditions[?(@.type=='Active')].status"
// +k8s:crd-gen:printcolumn:name="Blocked",type="string",jsonPath=".status.conditions[?(@.type=='Blocked')].status"
// +k8s:crd-gen:printcolumn:name="Ready",type="string",jsonPath=".status.conditions[?(@.type=='Ready')].status"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type ModelDeploymentForger struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelDeploymentForgerSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelDeploymentForgerStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelDeploymentForger)(nil)

// ModelDeploymentForgerList holds a list of ModelDeploymentForger resources.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentForgerList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelDeploymentForger `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentForgerList)(nil)

// ModelDeploymentForgerSpec defines the desired closed-loop kernel forging policy for one
// ModelDeployment.
type ModelDeploymentForgerSpec struct {
	// TargetRef names the target ModelDeployment in this namespace. Immutable after creation.
	//
	// +required
	TargetRef core.LocalObjectReference `json:"targetRef" protobuf:"bytes,1,name=targetRef"`

	// Mode selects how far the closed loop proceeds: ObserveOnly distills bottlenecks from probes
	// only, Shadow also runs ForgeSandbox and publishes verified ModelKernelArtifacts without
	// mutating the target, and Active promotes verified artifacts onto target roles.
	//
	// +optional
	// +k8s:validation:enum=["ObserveOnly","Shadow","Active"]
	// +k8s:validation:default="Shadow"
	Mode ModelDeploymentForgerMode `json:"mode,omitempty" protobuf:"bytes,2,opt,name=mode,casttype=ModelDeploymentForgerMode"`

	// Suspend pauses probing, sandbox creation, and promotion and releases any held target binding
	// without deleting the forger.
	//
	// +optional
	Suspend bool `json:"suspend,omitempty" protobuf:"varint,3,opt,name=suspend"`

	// Roles lists the serving roles on the target ModelDeployment to probe and specialize.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentForgerRoleSpec `json:"roles" protobuf:"bytes,4,rep,name=roles"`

	// Probe configures short-window live sentinel profiling passes.
	//
	// +optional
	// +k8s:validation:default={}
	Probe ModelDeploymentForgerProbeSpec `json:"probe,omitempty" protobuf:"bytes,5,opt,name=probe"`

	// Sandbox configures the Kueue-admitted single-GPU ForgeSandbox Pod and verifier limits.
	//
	// +optional
	// +k8s:validation:default={}
	Sandbox ModelDeploymentForgerSandboxSpec `json:"sandbox,omitempty" protobuf:"bytes,6,opt,name=sandbox"`

	// Rollout configures generation retention and canary failure rollback deadlines.
	//
	// +optional
	// +k8s:validation:default={}
	Rollout ModelDeploymentForgerRolloutSpec `json:"rollout,omitempty" protobuf:"bytes,7,opt,name=rollout"`
}

// ModelDeploymentForgerMode controls whether a ModelDeploymentForger observes, stages, or promotes
// forged kernel artifacts.
// +enum
type ModelDeploymentForgerMode string

const (
	// ModelDeploymentForgerModeObserveOnly runs short-window sentinel probes and records observed
	// bottlenecks without launching ForgeSandbox Pods or mutating the target deployment.
	ModelDeploymentForgerModeObserveOnly ModelDeploymentForgerMode = "ObserveOnly"

	// ModelDeploymentForgerModeShadow probes, forges, independently verifies, and publishes
	// ModelKernelArtifact bundles without mutating the target deployment.
	ModelDeploymentForgerModeShadow ModelDeploymentForgerMode = "Shadow"

	// ModelDeploymentForgerModeActive runs the full loop and promotes verified ModelKernelArtifact
	// generations onto the target deployment under readiness-gated rollout.
	ModelDeploymentForgerModeActive ModelDeploymentForgerMode = "Active"
)

// ModelDeploymentForgerRoleSpec configures kernel forging thresholds for one target role.
type ModelDeploymentForgerRoleSpec struct {
	// Name is the target role name in ModelDeployment.spec.roles (Server, Prefill, or Decode).
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// MaxKernelsPerGeneration caps how many top non-GEMM single-GPU operator bottlenecks are
	// targeted in one ForgeSandbox pass.
	//
	// +optional
	// +k8s:validation:default=4
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=16
	MaxKernelsPerGeneration int32 `json:"maxKernelsPerGeneration,omitempty" protobuf:"varint,2,opt,name=maxKernelsPerGeneration"`

	// MinSpeedupRatio is a decimal ratio string at or above "1.01" (default "1.05") that
	// forge-verifier requires over the reference implementation before admitting a kernel.
	//
	// +optional
	// +k8s:validation:default="1.05"
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=32
	MinSpeedupRatio string `json:"minSpeedupRatio,omitempty" protobuf:"bytes,3,opt,name=minSpeedupRatio"`
}

// ModelDeploymentForgerProbeSpec configures bounded short-window sentinel profiling.
type ModelDeploymentForgerProbeSpec struct {
	// Steps is the number of execution steps captured per probe window (3 to 5, default 4).
	//
	// +optional
	// +k8s:validation:default=4
	// +k8s:validation:minimum=3
	// +k8s:validation:maximum=5
	Steps int32 `json:"steps,omitempty" protobuf:"varint,1,opt,name=steps"`

	// CooldownSeconds is the minimum wait time in seconds between probe passes (at least 60,
	// default 600).
	//
	// +optional
	// +k8s:validation:default=600
	// +k8s:validation:minimum=60
	CooldownSeconds int32 `json:"cooldownSeconds,omitempty" protobuf:"varint,2,opt,name=cooldownSeconds"`

	// MinActiveRequests is the minimum running request count observed on a leader replica before
	// arming a short-window probe.
	//
	// +optional
	// +k8s:validation:default=1
	// +k8s:validation:minimum=0
	MinActiveRequests int32 `json:"minActiveRequests,omitempty" protobuf:"varint,3,opt,name=minActiveRequests"`
}

// ModelDeploymentForgerSandboxSpec configures Kueue-admitted single-GPU ForgeSandbox execution.
type ModelDeploymentForgerSandboxSpec struct {
	// RunnerMode selects External (black-box synthesis container image) or Mock (deterministic
	// built-in operator subcommands for pipeline qualification).
	//
	// +optional
	// +k8s:validation:enum=["External","Mock"]
	// +k8s:validation:default="External"
	RunnerMode ModelDeploymentForgerRunnerMode `json:"runnerMode,omitempty" protobuf:"bytes,1,opt,name=runnerMode,casttype=ModelDeploymentForgerRunnerMode"` // nolint: lll

	// RunnerImage is the container image executed by forge-runner when RunnerMode is External.
	//
	// +optional
	// +k8s:validation:maxLength=512
	RunnerImage string `json:"runnerImage,omitempty" protobuf:"bytes,2,opt,name=runnerImage"`

	// PriorityClassName optionally sets the Kubernetes PriorityClass on the ForgeSandbox Pod.
	//
	// +optional
	// +k8s:validation:maxLength=253
	PriorityClassName string `json:"priorityClassName,omitempty" protobuf:"bytes,3,opt,name=priorityClassName"`

	// TimeoutSeconds bounds total wall-clock execution of one ForgeSandbox Pod (60 to 14400,
	// default 1800).
	//
	// +optional
	// +k8s:validation:default=1800
	// +k8s:validation:minimum=60
	// +k8s:validation:maximum=14400
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" protobuf:"varint,4,opt,name=timeoutSeconds"`

	// OutputSizeLimit bounds the forge-output emptyDir staging volume (64Mi to 16Gi, default 2Gi).
	//
	// +optional
	// +k8s:validation:default="2Gi"
	OutputSizeLimit *resource.Quantity `json:"outputSizeLimit,omitempty" protobuf:"bytes,5,opt,name=outputSizeLimit"`

	// Resources optionally specifies host CPU and memory requests and limits for ForgeSandbox
	// containers alongside the single whole accelerator card requested from Kueue.
	//
	// +optional
	Resources *ModelDeploymentForgerSandboxResources `json:"resources,omitempty" protobuf:"bytes,6,opt,name=resources"`
}

// ModelDeploymentForgerRunnerMode selects the synthesis runner implementation inside ForgeSandbox.
// +enum
type ModelDeploymentForgerRunnerMode string

const (
	// ModelDeploymentForgerRunnerModeExternal runs the user-supplied RunnerImage container.
	ModelDeploymentForgerRunnerModeExternal ModelDeploymentForgerRunnerMode = "External"

	// ModelDeploymentForgerRunnerModeMock runs the deterministic built-in forge-mock subcommand.
	ModelDeploymentForgerRunnerModeMock ModelDeploymentForgerRunnerMode = "Mock"
)

// ModelDeploymentForgerSandboxResources specifies host CPU and memory requests for ForgeSandbox.
type ModelDeploymentForgerSandboxResources struct {
	// CPU is the host CPU request and limit for the ForgeSandbox Pod, such as "8".
	//
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty" protobuf:"bytes,1,opt,name=cpu"`

	// RAM is the host memory request and limit for the ForgeSandbox Pod, such as "32Gi".
	//
	// +optional
	RAM *resource.Quantity `json:"ram,omitempty" protobuf:"bytes,2,opt,name=ram"`
}

// ModelDeploymentForgerRolloutSpec configures artifact retention and canary rollback timing.
type ModelDeploymentForgerRolloutSpec struct {
	// MaxRetainedGenerations is the number of recent ModelKernelArtifact generations retained per
	// role before unreferenced older generations are pruned (1 to 16, default 3).
	//
	// +optional
	// +k8s:validation:default=3
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=16
	MaxRetainedGenerations int32 `json:"maxRetainedGenerations,omitempty" protobuf:"varint,1,opt,name=maxRetainedGenerations"`

	// FailureTimeoutSeconds is how long a promoted kernel generation may wait for canary readiness
	// and loaded-kernel confirmation before automatic rollback and quarantine (default 600).
	//
	// +optional
	// +k8s:validation:default=600
	// +k8s:validation:minimum=1
	FailureTimeoutSeconds int32 `json:"failureTimeoutSeconds,omitempty" protobuf:"varint,2,opt,name=failureTimeoutSeconds"`
}

// ModelDeploymentForgerStatus defines the observed closed-loop state of ModelDeploymentForger.
type ModelDeploymentForgerStatus struct {
	// ObservedGeneration is the metadata generation last evaluated by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,1,opt,name=observedGeneration"`

	// Binding records the active forger-binding lease held on the target ModelDeployment.
	//
	// +optional
	Binding *ModelDeploymentForgerBindingStatus `json:"binding,omitempty" protobuf:"bytes,2,opt,name=binding"`

	// Roles reports per-role forging phase, active sandbox identity, distilled bottlenecks, and
	// recent generation outcomes.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentForgerRoleStatus `json:"roles,omitempty" protobuf:"bytes,3,rep,name=roles"`

	// Conditions reports Bound, Active, Blocked, and Ready status for the forger.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,4,rep,name=conditions"` // nolint: lll
}

// ModelDeploymentForgerBindingStatus reports the lease held on a target ModelDeployment.
type ModelDeploymentForgerBindingStatus struct {
	// TargetUID is the UID of the bound ModelDeployment.
	//
	// +optional
	TargetUID string `json:"targetUID,omitempty" protobuf:"bytes,1,opt,name=targetUID"`

	// TargetName is the name of the bound ModelDeployment.
	//
	// +optional
	TargetName string `json:"targetName,omitempty" protobuf:"bytes,2,opt,name=targetName"`

	// Term is the monotonic binding term under which the forger writes.
	//
	// +optional
	Term int64 `json:"term,omitempty" protobuf:"varint,3,opt,name=term"`
}

// ModelDeploymentForgerRoleStatus reports closed-loop state for one targeted role.
type ModelDeploymentForgerRoleStatus struct {
	// Name is the targeted role name in ModelDeployment.spec.roles.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Phase is the current lifecycle step for this role.
	//
	// +optional
	// +k8s:validation:enum=["Idle","Probing","Forging","Verifying","Publishing","Promoting"]
	Phase ModelDeploymentForgerRolePhase `json:"phase,omitempty" protobuf:"bytes,2,opt,name=phase,casttype=ModelDeploymentForgerRolePhase"`

	// ActiveGeneration is the monotonically increasing forge generation number currently running or
	// most recently promoted for this role.
	//
	// +optional
	ActiveGeneration int64 `json:"activeGeneration,omitempty" protobuf:"varint,3,opt,name=activeGeneration"`

	// ActiveKernelArtifact names the ModelKernelArtifact currently promoted or staged for this
	// role.
	//
	// +optional
	ActiveKernelArtifact string `json:"activeKernelArtifact,omitempty" protobuf:"bytes,4,opt,name=activeKernelArtifact"`

	// PreviousKernelArtifact names the prior ModelKernelArtifact retained and pinned for automatic
	// rollback while a promotion is in flight; empty when stock engine kernels were running.
	//
	// +optional
	PreviousKernelArtifact string `json:"previousKernelArtifact,omitempty" protobuf:"bytes,5,opt,name=previousKernelArtifact"`

	// ActiveSandbox records the worker-created ForgeSandbox Pod currently running for this role.
	// Model-manager verifies PodUID against this worker-guarded field before ingesting outputs.
	//
	// +optional
	ActiveSandbox *ModelDeploymentForgerSandboxStatus `json:"activeSandbox,omitempty" protobuf:"bytes,6,opt,name=activeSandbox"`

	// ObservedBottlenecks lists the hottest eligible single-GPU non-GEMM kernels distilled from the
	// most recent probe pass.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=32
	ObservedBottlenecks []ModelDeploymentForgerBottleneck `json:"observedBottlenecks,omitempty" protobuf:"bytes,7,rep,name=observedBottlenecks"` // nolint: lll

	// Generations is a bounded history (up to 16 entries) of forge generations for this role.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=16
	Generations []ModelDeploymentForgerGenerationStatus `json:"generations,omitempty" protobuf:"bytes,8,rep,name=generations"`
}

// ModelDeploymentForgerRolePhase names the closed-loop phase of one targeted role.
// +enum
type ModelDeploymentForgerRolePhase string

const (
	// ModelDeploymentForgerRolePhaseIdle indicates the role is waiting for its next probe window.
	ModelDeploymentForgerRolePhaseIdle ModelDeploymentForgerRolePhase = "Idle"

	// ModelDeploymentForgerRolePhaseProbing indicates a short-window sentinel probe is active.
	ModelDeploymentForgerRolePhaseProbing ModelDeploymentForgerRolePhase = "Probing"

	// ModelDeploymentForgerRolePhaseForging indicates the Kueue ForgeSandbox runner is executing.
	ModelDeploymentForgerRolePhaseForging ModelDeploymentForgerRolePhase = "Forging"

	// ModelDeploymentForgerRolePhaseVerifying indicates forge-verifier is validating candidate
	// solutions.
	ModelDeploymentForgerRolePhaseVerifying ModelDeploymentForgerRolePhase = "Verifying"

	// ModelDeploymentForgerRolePhasePublishing indicates verified outputs are being ingested into
	// NodeModelStore and published as a ModelKernelArtifact.
	ModelDeploymentForgerRolePhasePublishing ModelDeploymentForgerRolePhase = "Publishing"

	// ModelDeploymentForgerRolePhasePromoting indicates the new ModelKernelArtifact is rolling out
	// across the target role's replicas under readiness gating.
	ModelDeploymentForgerRolePhasePromoting ModelDeploymentForgerRolePhase = "Promoting"
)

// ModelDeploymentForgerSandboxStatus records the identity of an active worker-owned ForgeSandbox
// Pod.
type ModelDeploymentForgerSandboxStatus struct {
	// PodName is the name of the ForgeSandbox Pod in this namespace.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	PodName string `json:"podName" protobuf:"bytes,1,name=podName"`

	// PodUID is the Kubernetes UID of the ForgeSandbox Pod, verified by model-manager before
	// ingesting emptyDir outputs.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=128
	PodUID string `json:"podUID" protobuf:"bytes,2,name=podUID"`

	// NodeName is the Kubernetes Node where the ForgeSandbox Pod was scheduled; empty while
	// awaiting scheduling.
	//
	// +optional
	NodeName string `json:"nodeName,omitempty" protobuf:"bytes,3,opt,name=nodeName"`

	// ConfigMapName is the name of the owner-referenced input ConfigMap mounted into the sandbox.
	//
	// +optional
	ConfigMapName string `json:"configMapName,omitempty" protobuf:"bytes,4,opt,name=configMapName"`

	// StartedAt is when the ForgeSandbox Pod was created.
	//
	// +optional
	StartedAt *meta.Time `json:"startedAt,omitempty" protobuf:"bytes,5,opt,name=startedAt"`
}

// ModelDeploymentForgerBottleneck summarizes one eligible single-GPU non-GEMM operator bottleneck
// distilled from a short-window sentinel probe.
type ModelDeploymentForgerBottleneck struct {
	// LocalKernelKey is the canonical single-GPU operator key over static per-rank dimensions.
	//
	// +required
	// +k8s:validation:minLength=1
	LocalKernelKey string `json:"localKernelKey" protobuf:"bytes,1,name=localKernelKey"`

	// OpFamily is the canonical single-GPU operator family name, such as "rmsnorm".
	//
	// +required
	// +k8s:validation:minLength=1
	OpFamily string `json:"opFamily" protobuf:"bytes,2,name=opFamily"`

	// GPUTimeSharePercent is the decimal percentage of profiled single-GPU kernel self-time spent
	// in this operator, such as "12.5".
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=32
	GPUTimeSharePercent string `json:"gpuTimeSharePercent" protobuf:"bytes,3,name=gpuTimeSharePercent"`

	// ObservedAt is when the probe window that distilled this bottleneck completed.
	ObservedAt meta.Time `json:"observedAt" protobuf:"bytes,4,name=observedAt"`
}

// ModelDeploymentForgerGenerationStatus records the outcome of one forge generation for a role.
type ModelDeploymentForgerGenerationStatus struct {
	// Generation is the sequential 1-based forge generation number for this role.
	//
	// +required
	// +k8s:validation:minimum=1
	Generation int64 `json:"generation" protobuf:"varint,1,name=generation"`

	// ArtifactName is the ModelKernelArtifact created for this generation, if published.
	//
	// +optional
	ArtifactName string `json:"artifactName,omitempty" protobuf:"bytes,2,opt,name=artifactName"`

	// ManifestDigest is the content address of the published kernel bundle, if published.
	//
	// +optional
	ManifestDigest string `json:"manifestDigest,omitempty" protobuf:"bytes,3,opt,name=manifestDigest"`

	// Outcome summarizes how this generation ended.
	//
	// +optional
	// +k8s:validation:enum=["Promoted","RolledBack","Failed","Skipped"]
	Outcome ModelDeploymentForgerGenerationOutcome `json:"outcome,omitempty" protobuf:"bytes,4,opt,name=outcome,casttype=ModelDeploymentForgerGenerationOutcome"` // nolint: lll

	// Reason is a machine-readable reason code for Outcome.
	//
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,5,opt,name=reason"`

	// Message provides human-readable context for Outcome.
	//
	// +optional
	Message string `json:"message,omitempty" protobuf:"bytes,6,opt,name=message"`

	// StartedAt is when this generation began.
	//
	// +optional
	StartedAt *meta.Time `json:"startedAt,omitempty" protobuf:"bytes,7,opt,name=startedAt"`

	// FinishedAt is when this generation reached its terminal outcome.
	//
	// +optional
	FinishedAt *meta.Time `json:"finishedAt,omitempty" protobuf:"bytes,8,opt,name=finishedAt"`
}

// ModelDeploymentForgerGenerationOutcome classifies the terminal result of one forge generation.
// +enum
type ModelDeploymentForgerGenerationOutcome string

const (
	// ModelDeploymentForgerGenerationOutcomePromoted indicates the kernel artifact converged across
	// all target replicas in Active mode (or published cleanly in Shadow mode).
	ModelDeploymentForgerGenerationOutcomePromoted ModelDeploymentForgerGenerationOutcome = "Promoted"

	// ModelDeploymentForgerGenerationOutcomeRolledBack indicates promotion failed canary
	// verification, rolled back to the prior kernel artifact, and quarantined the failed artifact.
	ModelDeploymentForgerGenerationOutcomeRolledBack ModelDeploymentForgerGenerationOutcome = "RolledBack"

	// ModelDeploymentForgerGenerationOutcomeFailed indicates probing, sandbox synthesis,
	// verification, or ingestion failed before promotion.
	ModelDeploymentForgerGenerationOutcomeFailed ModelDeploymentForgerGenerationOutcome = "Failed"

	// ModelDeploymentForgerGenerationOutcomeSkipped indicates probing found no eligible unforged
	// bottlenecks.
	ModelDeploymentForgerGenerationOutcomeSkipped ModelDeploymentForgerGenerationOutcome = "Skipped"
)
