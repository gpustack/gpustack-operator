package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

const (
	// ModelDeploymentScalerBindingAnnotationKey names the annotation that carries the binding
	// between a scaler and the ModelDeployment it drives.
	//
	// The payload names the scaler and the term the scaler wrote under. A ModelDeployment that
	// carries this annotation belongs to that scaler alone, so a second scaler refuses to bind to
	// it and the worker reconciler refuses to write a replica count on behalf of anyone else.
	ModelDeploymentScalerBindingAnnotationKey = "modeldeployment.gpustack.ai/scaler-binding"

	// ModelDeploymentScalerDecisionAnnotationKey names the annotation that carries the last
	// committed decision of a scaler.
	//
	// The payload names the decision, the term it was written under and the replica vector it
	// committed. The planner reads it after a restart so that it rebuilds its state from what the
	// target actually carries instead of re-committing a vector it merely remembers.
	ModelDeploymentScalerDecisionAnnotationKey = "modeldeployment.gpustack.ai/scaler-decision"

	// ModelDeploymentScalerFinalizerKey names the finalizer a scaler holds on itself while bound
	// to a target.
	//
	// The finalizer is what lets a deleted scaler remove its binding annotation before the scaler
	// object disappears, so that the target keeps serving and becomes bindable again.
	ModelDeploymentScalerFinalizerKey = "modeldeployment.gpustack.ai/scaler-binding"

	// ModelDeploymentScalerModelIDAnnotationKey declares the exact catalog model identity on a
	// target ModelDeployment when the deployment spec does not establish it.
	ModelDeploymentScalerModelIDAnnotationKey = "worker.gpustack.ai/model-id"

	// ModelDeploymentScalerHardwareIDAnnotationKey declares the exact catalog hardware identity on
	// a target ModelDeployment when the deployment spec does not establish it.
	ModelDeploymentScalerHardwareIDAnnotationKey = "worker.gpustack.ai/hardware-id"

	// ModelDeploymentScalerPrecisionAnnotationKey declares the exact catalog precision or
	// quantization identity on a target ModelDeployment when the deployment spec does not establish
	// it.
	ModelDeploymentScalerPrecisionAnnotationKey = "worker.gpustack.ai/precision"

	// ModelDeploymentScalerPrefillHardwareIDAnnotationKey overrides the catalog hardware identity
	// declaration for Prefill roles on a target ModelDeployment.
	ModelDeploymentScalerPrefillHardwareIDAnnotationKey = "worker.gpustack.ai/prefill-hardware-id"

	// ModelDeploymentScalerDecodeHardwareIDAnnotationKey overrides the catalog hardware identity
	// declaration for Decode roles on a target ModelDeployment.
	ModelDeploymentScalerDecodeHardwareIDAnnotationKey = "worker.gpustack.ai/decode-hardware-id"
)

// The conditions a scaler reports.
//
// Bound says whether the scaler owns its target, Active says whether it is allowed to change it,
// MetricsReady says whether it has what it needs to plan, CapacityPriorsReady says whether
// configured catalog priors resolved, and Blocked says what stops it.
const (
	// ModelDeploymentScalerConditionTypeBound says whether the scaler holds the binding on its
	// target.
	ModelDeploymentScalerConditionTypeBound = "Bound"

	// ModelDeploymentScalerConditionTypeActive says whether the scaler acts on what it observes.
	ModelDeploymentScalerConditionTypeActive = "Active"

	// ModelDeploymentScalerConditionTypeMetricsReady says whether the scaler has the signals its
	// algorithm needs.
	ModelDeploymentScalerConditionTypeMetricsReady = "MetricsReady"

	// ModelDeploymentScalerConditionTypeCapacityPriorsReady says whether the scaler resolved the
	// capacity priors its algorithm needs.
	ModelDeploymentScalerConditionTypeCapacityPriorsReady = "CapacityPriorsReady"

	// ModelDeploymentScalerConditionTypeBlocked says whether something stops the scaler.
	ModelDeploymentScalerConditionTypeBlocked = "Blocked"
)

// The reasons a scaler condition carries.
//
// They name the situation rather than the mechanism, so a reader learns what is true of the
// scaler without reading the code that found it out.
const (
	// ModelDeploymentScalerReasonTargetNotFound says the target does not exist.
	ModelDeploymentScalerReasonTargetNotFound = "TargetNotFound"

	// ModelDeploymentScalerReasonAlreadyBound says another scaler owns the target.
	ModelDeploymentScalerReasonAlreadyBound = "AlreadyBound"

	// ModelDeploymentScalerReasonTargetReplaced says the target was deleted and recreated, so
	// the binding points at an object that no longer exists.
	ModelDeploymentScalerReasonTargetReplaced = "TargetReplaced"

	// ModelDeploymentScalerReasonInvalidBinding says the target's binding annotation cannot be read,
	// so no scaler may claim the target.
	ModelDeploymentScalerReasonInvalidBinding = "InvalidBinding"

	// ModelDeploymentScalerReasonBoundsConflict says the bounds of the roles contradict each
	// other.
	ModelDeploymentScalerReasonBoundsConflict = "BoundsConflict"

	// ModelDeploymentScalerReasonRatioConflict says the ratios contradict each other or the
	// bounds.
	ModelDeploymentScalerReasonRatioConflict = "RatioConflict"

	// ModelDeploymentScalerReasonPacingBounded says a step cap or a pending guard holds the
	// vector where it is.
	ModelDeploymentScalerReasonPacingBounded = "PacingBounded"

	// ModelDeploymentScalerReasonWaitingForMetrics says the scaler has not yet read the signals
	// its algorithm needs.
	ModelDeploymentScalerReasonWaitingForMetrics = "WaitingForMetrics"

	// ModelDeploymentScalerReasonPredictorWarmingUp says the predictor has not seen enough
	// points to forecast.
	ModelDeploymentScalerReasonPredictorWarmingUp = "PredictorWarmingUp"

	// ModelDeploymentScalerReasonSuspended says the scaler is suspended.
	ModelDeploymentScalerReasonSuspended = "Suspended"

	// ModelDeploymentScalerReasonShadowMode says the scaler only observes.
	ModelDeploymentScalerReasonShadowMode = "ShadowMode"

	// ModelDeploymentScalerReasonResourceExhausted says the cluster cannot give the role the
	// replicas it asked for.
	ModelDeploymentScalerReasonResourceExhausted = "ResourceExhausted"

	// ModelDeploymentScalerReasonScaleDownAborted says a retirement the scaler asked for failed
	// and the vector was restored.
	ModelDeploymentScalerReasonScaleDownAborted = "ScaleDownAborted"

	// ModelDeploymentScalerReasonRebalanceNotApplicable says the roles cannot give resources to
	// each other.
	ModelDeploymentScalerReasonRebalanceNotApplicable = "RebalanceNotApplicable"

	// ModelDeploymentScalerReasonRebalanceBlocked says a rebalance step cannot be taken.
	ModelDeploymentScalerReasonRebalanceBlocked = "RebalanceBlocked"

	// ModelDeploymentScalerReasonCapacityPriorUnresolved says a required capacity prior could not
	// be resolved from the catalog.
	ModelDeploymentScalerReasonCapacityPriorUnresolved = "CapacityPriorUnresolved"

	// ModelDeploymentScalerReasonInsufficientLoadCoverage says the matched profile does not carry
	// enough load points or boundary evidence to establish capacity.
	ModelDeploymentScalerReasonInsufficientLoadCoverage = "InsufficientLoadCoverage"

	// ModelDeploymentScalerReasonIncompatibleMetric says the matched profile does not measure the
	// throughput numerator, latency quantile, or cache convention the role requires.
	ModelDeploymentScalerReasonIncompatibleMetric = "IncompatibleMetric"
)

const (
	// ModelDeploymentScalerScaleDownOutcomeCommitted says the scale-down is in progress under retirement.
	ModelDeploymentScalerScaleDownOutcomeCommitted = "Committed"

	// ModelDeploymentScalerScaleDownOutcomeAborted says the retirement failed and the vector was restored.
	ModelDeploymentScalerScaleDownOutcomeAborted = "Aborted"
)

// ModelDeploymentScalerBinding is the payload of the binding annotation.
//
// It is the whole of the single-writer contract: the target names the scaler that owns it and the
// term that owner writes under, so a later scaler either takes over explicitly or stands down.
type ModelDeploymentScalerBinding struct {
	// ScalerName is the name of the scaler that owns the target.
	ScalerName string `json:"scalerName" protobuf:"bytes,1,opt,name=scalerName"`

	// ScalerUID is the UID of the scaler that owns the target.
	//
	// A scaler that was deleted and recreated with the same name carries a different UID, so the
	// target can tell the two apart.
	ScalerUID string `json:"scalerUID" protobuf:"bytes,2,opt,name=scalerUID"`

	// Term is the binding term the owner writes under.
	Term int64 `json:"term" protobuf:"varint,3,opt,name=term"`
}

// ModelDeploymentScalerDecision is the payload of the decision annotation.
//
// It records what the scaler last committed, so that a planner which lost its in-memory state
// rebuilds from the target instead of committing a vector twice.
type ModelDeploymentScalerDecision struct {
	// DecisionID identifies the decision that produced the vector.
	DecisionID string `json:"decisionID" protobuf:"bytes,1,opt,name=decisionID"`

	// Term is the binding term the decision was written under.
	Term int64 `json:"term" protobuf:"varint,2,opt,name=term"`

	// Vector is the replica count per role the decision committed.
	Vector map[string]int32 `json:"vector" protobuf:"bytes,3,rep,name=vector"`
}

// ModelDeploymentScaler is the schema for a metric-driven replica planner over one
// ModelDeployment.
//
// In Shadow mode it observes and records recommendations in status without modifying the target.
// In Active mode its reconciler commits the planned replica vector and decision annotation onto
// the target under the single-writer admission fence, and the ModelDeployment reconciler converges
// the serving replicas to that count.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mds"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Target",type="string",jsonPath=".spec.targetRef.name"
// +k8s:crd-gen:printcolumn:name="Mode",type="string",jsonPath=".spec.mode"
// +k8s:crd-gen:printcolumn:name="Algorithm",type="string",jsonPath=".spec.algorithm.type"
// +k8s:crd-gen:printcolumn:name="Bound",type="string",jsonPath=".status.conditions[?(@.type=='Bound')].status"
// +k8s:crd-gen:printcolumn:name="Active",type="string",jsonPath=".status.conditions[?(@.type=='Active')].status"
// +k8s:crd-gen:printcolumn:name="Blocked",type="string",jsonPath=".status.conditions[?(@.type=='Blocked')].status"
type ModelDeploymentScaler struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelDeploymentScalerSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelDeploymentScalerStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelDeploymentScaler)(nil)

// ModelDeploymentScalerList holds the list of ModelDeploymentScalers.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelDeploymentScalerList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelDeploymentScaler `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelDeploymentScalerList)(nil)

// ModelDeploymentScalerSpec defines the desired state of ModelDeploymentScaler.
type ModelDeploymentScalerSpec struct {
	// TargetRef names the ModelDeployment this scaler plans for.
	//
	// The target is in this namespace. A scaler without a target has nothing to plan for, so the
	// field is required and a name that matches nothing leaves the scaler unbound.
	//
	// +required
	TargetRef ModelDeploymentScalerTargetReference `json:"targetRef" protobuf:"bytes,1,name=targetRef"`

	// Mode decides whether the scaler only observes or also acts.
	//
	// Shadow observes and publishes what it would have done, and changes nothing. Active publishes
	// and the worker reconciler applies the committed vector. Shadow is the default, so a scaler
	// that was just created never moves a target before its owner asked for it.
	//
	// +optional
	// +k8s:validation:enum=["Shadow","Active"]
	// +k8s:validation:default="Shadow"
	Mode ModelDeploymentScalerMode `json:"mode,omitempty" protobuf:"bytes,2,opt,name=mode"`

	// Suspend stops the scaler without deleting it.
	//
	// A suspended scaler releases its owned binding and stops planning without being deleted,
	// reporting Suspended on its Bound and Active conditions until resumed.
	//
	// +optional
	Suspend bool `json:"suspend,omitempty" protobuf:"varint,3,opt,name=suspend"`

	// Sampling tunes how often the scaler looks at the target and how long it waits for a
	// committed scale-up to become admitted.
	//
	// +optional
	Sampling ModelDeploymentScalerSamplingSpec `json:"sampling,omitempty" protobuf:"bytes,4,opt,name=sampling"`

	// Roles are the roles of the target this scaler plans for, with the bounds each one stays
	// inside.
	//
	// Every role of the target the scaler plans for needs a bound, and a role the target does not
	// have is refused, so that the planner never reasons about a role that is not there.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentScalerRoleBounds `json:"roles" protobuf:"bytes,5,rep,name=roles"`

	// Ratios are the relationships between roles that a committed vector has to keep.
	//
	// A ratio bounds the replica count of a numerator role against the replica count of a
	// denominator role, so that a coupled prefill and decode pair does not drift apart. A ratio
	// names roles the scaler plans for, and a ratio that names an unknown role is refused.
	//
	// +optional
	// +listType=map
	// +listMapKey=numeratorRole
	// +listMapKey=denominatorRole
	Ratios []ModelDeploymentScalerRoleRatio `json:"ratios,omitempty" protobuf:"bytes,6,rep,name=ratios"`

	// Algorithm says how the scaler turns what it observes into a replica count.
	//
	// The algorithm named here is the one that runs, and the parameters of the other algorithms
	// are ignored, so a deployment states one algorithm and one set of parameters.
	//
	// +required
	Algorithm ModelDeploymentScalerAlgorithmSpec `json:"algorithm" protobuf:"bytes,7,name=algorithm"`

	// Behavior tunes how fast the scaler may move a role and how long it waits after a failure.
	//
	// +optional
	Behavior ModelDeploymentScalerBehaviorSpec `json:"behavior,omitempty" protobuf:"bytes,8,opt,name=behavior"`

	// Rebalance tunes what the scaler does when one role cannot grow because the cluster is
	// exhausted and another role could give resources back.
	//
	// +optional
	Rebalance ModelDeploymentScalerRebalanceSpec `json:"rebalance,omitempty" protobuf:"bytes,9,opt,name=rebalance"`
}

// ModelDeploymentScalerTargetReference names the target of a scaler.
type ModelDeploymentScalerTargetReference struct {
	// Name is the name of the target ModelDeployment in this namespace.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`
}

// ModelDeploymentScalerMode decides whether a scaler acts on what it observes.
type ModelDeploymentScalerMode string

const (
	// ModelDeploymentScalerModeShadow observes and publishes without changing the target.
	ModelDeploymentScalerModeShadow ModelDeploymentScalerMode = "Shadow"

	// ModelDeploymentScalerModeActive observes, publishes and has the worker reconciler apply
	// the committed vector.
	ModelDeploymentScalerModeActive ModelDeploymentScalerMode = "Active"
)

// ModelDeploymentScalerSamplingSpec tunes the observation interval and the pending timeout.
type ModelDeploymentScalerSamplingSpec struct {
	// IntervalSeconds is how often the scaler looks at the target.
	//
	// It is the floor on how fast the scaler reacts and the floor on how often it writes, so a
	// short interval costs scrape load and a long one costs reaction time.
	//
	// +optional
	// +k8s:validation:minimum=5
	// +k8s:validation:maximum=300
	IntervalSeconds int32 `json:"intervalSeconds,omitempty" protobuf:"varint,1,opt,name=intervalSeconds"`

	// PendingTimeoutSeconds is how long a committed scale-up may stay unadmitted before it is
	// retracted.
	//
	// The retraction restores the vector the scale-up started from, so a timeout undoes a change
	// rather than leaving a role waiting for resources that never arrive.
	//
	// +optional
	// +k8s:validation:minimum=60
	// +k8s:validation:maximum=1200
	PendingTimeoutSeconds int32 `json:"pendingTimeoutSeconds,omitempty" protobuf:"varint,2,opt,name=pendingTimeoutSeconds"`
}

// ModelDeploymentScalerRoleBounds are the bounds one role stays inside.
type ModelDeploymentScalerRoleBounds struct {
	// Name is the name of the role on the target.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// MinReplicas is the fewest replicas this role may have.
	//
	// +optional
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=1024
	MinReplicas int32 `json:"minReplicas,omitempty" protobuf:"varint,2,opt,name=minReplicas"`

	// MaxReplicas is the most replicas this role may have.
	//
	// +required
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=1024
	MaxReplicas int32 `json:"maxReplicas" protobuf:"varint,3,opt,name=maxReplicas"`
}

// ModelDeploymentScalerRoleRatio is a relationship two roles keep.
type ModelDeploymentScalerRoleRatio struct {
	// NumeratorRole is the role whose replica count is the numerator.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	NumeratorRole string `json:"numeratorRole" protobuf:"bytes,1,name=numeratorRole"`

	// DenominatorRole is the role whose replica count is the denominator.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	DenominatorRole string `json:"denominatorRole" protobuf:"bytes,2,name=denominatorRole"`

	// Target is the ratio the projection aims at.
	//
	// It is the ratio the planner looks for inside the bounds of every other rule, and it is
	// what a coupled pair settles on when nothing else decides.
	//
	// +optional
	Target *resource.Quantity `json:"target,omitempty" protobuf:"bytes,3,opt,name=target"`

	// Min is the smallest ratio the numerator may hold against the denominator.
	//
	// +optional
	Min *resource.Quantity `json:"min,omitempty" protobuf:"bytes,4,opt,name=min"`

	// Max is the largest ratio the numerator may hold against the denominator.
	//
	// +optional
	Max *resource.Quantity `json:"max,omitempty" protobuf:"bytes,5,opt,name=max"`
}

// ModelDeploymentScalerAlgorithmSpec names the algorithm and holds its parameters.
type ModelDeploymentScalerAlgorithmSpec struct {
	// Type is the algorithm that runs.
	//
	// +required
	// +k8s:validation:enum=["Proportional","TokenThroughput","Predictive"]
	Type ModelDeploymentScalerAlgorithmType `json:"type" protobuf:"bytes,1,name=type"`

	// Proportional holds the parameters of the Proportional algorithm.
	//
	// It is required when Type is Proportional and ignored otherwise.
	//
	// +optional
	Proportional *ModelDeploymentScalerProportionalSpec `json:"proportional,omitempty" protobuf:"bytes,2,opt,name=proportional"`

	// TokenThroughput holds the parameters of the TokenThroughput algorithm.
	//
	// It is required when Type is TokenThroughput and ignored otherwise.
	//
	// +optional
	TokenThroughput *ModelDeploymentScalerTokenThroughputSpec `json:"tokenThroughput,omitempty" protobuf:"bytes,3,opt,name=tokenThroughput"`

	// Predictive holds the parameters of the Predictive algorithm.
	//
	// It is required when Type is Predictive and ignored otherwise.
	//
	// +optional
	Predictive *ModelDeploymentScalerPredictiveSpec `json:"predictive,omitempty" protobuf:"bytes,4,opt,name=predictive"`

	// CapacityPrior permits resolving missing token capacities from a measured catalog for
	// TokenThroughput and Predictive.
	//
	// An absent object means every capacity is stated by hand, and the scaler uses only those
	// values. A present object permits resolving the capacities the scaler does not state, and
	// only from the offline catalog it names. A capacity the scaler states is never replaced by a
	// catalog value. When every serving capacity is stated, no catalog is read at all.
	//
	// +optional
	CapacityPrior *ModelDeploymentScalerCapacityPriorSpec `json:"capacityPrior,omitempty" protobuf:"bytes,5,opt,name=capacityPrior"` // nolint: lll
}

// ModelDeploymentScalerAlgorithmType is the algorithm a scaler runs.
type ModelDeploymentScalerAlgorithmType string

const (
	// ModelDeploymentScalerAlgorithmTypeProportional scales on how much work a replica already
	// carries.
	ModelDeploymentScalerAlgorithmTypeProportional ModelDeploymentScalerAlgorithmType = "Proportional"

	// ModelDeploymentScalerAlgorithmTypeTokenThroughput scales on how many tokens a second the
	// role has to serve.
	ModelDeploymentScalerAlgorithmTypeTokenThroughput ModelDeploymentScalerAlgorithmType = "TokenThroughput"

	// ModelDeploymentScalerAlgorithmTypePredictive forecasts the demand and scales on the
	// forecast.
	ModelDeploymentScalerAlgorithmTypePredictive ModelDeploymentScalerAlgorithmType = "Predictive"
)

// ModelDeploymentScalerProportionalSpec holds the parameters of the Proportional algorithm.
type ModelDeploymentScalerProportionalSpec struct {
	// Metric is the signal the algorithm scales on.
	//
	// +required
	// +k8s:validation:enum=["QueueDepth","RunningRequests","WaitingRequests","PromptTokensPerSecond","GenerationTokensPerSecond","TotalTokensPerSecond"]
	Metric ModelDeploymentScalerProportionalMetric `json:"metric" protobuf:"bytes,1,name=metric"`

	// RoleTargets is the signal one replica of each role is expected to carry.
	//
	// Every role the scaler plans for needs a target, because the algorithm divides what it
	// observed by it to reach a replica count.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	RoleTargets []ModelDeploymentScalerRoleTarget `json:"roleTargets,omitempty" protobuf:"bytes,2,rep,name=roleTargets"`

	// Tolerance is the fraction around the target inside which nothing happens.
	//
	// A role whose observed load already sits inside the tolerance holds its replica count, so a
	// steady deployment does not move for noise.
	//
	// +optional
	Tolerance *resource.Quantity `json:"tolerance,omitempty" protobuf:"bytes,3,opt,name=tolerance"`
}

// ModelDeploymentScalerProportionalMetric is the signal the Proportional algorithm scales on.
type ModelDeploymentScalerProportionalMetric string

const (
	// ModelDeploymentScalerProportionalMetricQueueDepth counts the requests a role is running or
	// waiting for.
	ModelDeploymentScalerProportionalMetricQueueDepth ModelDeploymentScalerProportionalMetric = "QueueDepth"

	// ModelDeploymentScalerProportionalMetricRunningRequests counts the requests a role is
	// running.
	ModelDeploymentScalerProportionalMetricRunningRequests ModelDeploymentScalerProportionalMetric = "RunningRequests"

	// ModelDeploymentScalerProportionalMetricWaitingRequests counts the requests a role is
	// waiting for.
	ModelDeploymentScalerProportionalMetricWaitingRequests ModelDeploymentScalerProportionalMetric = "WaitingRequests"

	// ModelDeploymentScalerProportionalMetricPromptTokensPerSecond counts the prompt tokens a
	// role processes per second.
	ModelDeploymentScalerProportionalMetricPromptTokensPerSecond ModelDeploymentScalerProportionalMetric = "PromptTokensPerSecond"

	// ModelDeploymentScalerProportionalMetricGenerationTokensPerSecond counts the tokens a role
	// generates per second.
	ModelDeploymentScalerProportionalMetricGenerationTokensPerSecond ModelDeploymentScalerProportionalMetric = "GenerationTokensPerSecond"

	// ModelDeploymentScalerProportionalMetricTotalTokensPerSecond counts every token a role
	// processes per second.
	ModelDeploymentScalerProportionalMetricTotalTokensPerSecond ModelDeploymentScalerProportionalMetric = "TotalTokensPerSecond"
)

// ModelDeploymentScalerRoleTarget is the signal one replica of a role is expected to carry.
type ModelDeploymentScalerRoleTarget struct {
	// Name is the role this target belongs to.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Target is the signal one replica of this role is expected to carry.
	//
	// It is a quantity, so a rate is written as a quantity per second and a count as a plain
	// number.
	//
	// +required
	Target resource.Quantity `json:"target" protobuf:"bytes,2,name=target"`
}

// ModelDeploymentScalerTokenThroughputSpec holds the parameters of the TokenThroughput
// algorithm.
type ModelDeploymentScalerTokenThroughputSpec struct {
	// PrefillTargetTokensPerSecond is the prompt-token rate one Prefill replica sustains.
	//
	// It is the capacity the demand is divided by, so the number of replicas follows from how
	// many prompt tokens per second one replica is expected to serve.
	//
	// +optional
	PrefillTargetTokensPerSecond *resource.Quantity `json:"prefillTargetTokensPerSecond,omitempty" protobuf:"bytes,1,opt,name=prefillTargetTokensPerSecond"` // nolint: lll

	// DecodeTargetTokensPerSecond is the generation-token rate one Decode replica sustains.
	//
	// +optional
	DecodeTargetTokensPerSecond *resource.Quantity `json:"decodeTargetTokensPerSecond,omitempty" protobuf:"bytes,2,opt,name=decodeTargetTokensPerSecond"`

	// ServerTargetTokensPerSecond is the token rate one Server replica sustains, over prompt
	// tokens and generation tokens together.
	//
	// +optional
	ServerTargetTokensPerSecond *resource.Quantity `json:"serverTargetTokensPerSecond,omitempty" protobuf:"bytes,3,opt,name=serverTargetTokensPerSecond"`

	// StableWindowSeconds is the window the stable recommendation is computed over.
	//
	// The stable window is long enough to smooth a burst, so a short spike does not move the
	// replica count.
	//
	// +optional
	// +k8s:validation:minimum=30
	// +k8s:validation:maximum=600
	StableWindowSeconds int32 `json:"stableWindowSeconds,omitempty" protobuf:"varint,4,opt,name=stableWindowSeconds"`

	// PanicWindowSeconds is the window the panic recommendation is computed over.
	//
	// The panic window is short, so it reacts to a burst that the stable window smooths away.
	//
	// +optional
	// +k8s:validation:minimum=10
	// +k8s:validation:maximum=300
	PanicWindowSeconds int32 `json:"panicWindowSeconds,omitempty" protobuf:"varint,5,opt,name=panicWindowSeconds"`

	// PanicThreshold is the growth rate over the ready replicas that starts a panic.
	//
	// A panic holds the replica count up and stops scale-downs until the load settles.
	//
	// +optional
	PanicThreshold *resource.Quantity `json:"panicThreshold,omitempty" protobuf:"bytes,6,opt,name=panicThreshold"`

	// TTFTTargetMilliseconds is the time to first token the role is expected to keep under.
	//
	// A role that is observed above this target is a role that needs more replicas, whatever the
	// token counters say.
	//
	// +optional
	TTFTTargetMilliseconds *resource.Quantity `json:"ttftTargetMilliseconds,omitempty" protobuf:"bytes,7,opt,name=ttftTargetMilliseconds"`

	// ITLTargetMilliseconds is the inter-token latency the role is expected to keep under.
	//
	// +optional
	ITLTargetMilliseconds *resource.Quantity `json:"itlTargetMilliseconds,omitempty" protobuf:"bytes,8,opt,name=itlTargetMilliseconds"`

	// LatencyQuantile is the quantile of the latency distribution the role is held to.
	//
	// IT IS THE QUANTILE THE ONLINE HISTOGRAMS ARE SAMPLED AT and the quantile the capacity
	// prior is resolved against. An absent value means the ninety fifth percentile.
	//
	// IT IS A NUMBER BETWEEN ZERO EXCLUSIVE AND ONE INCLUSIVE, and any percentile is allowed.
	// THE VALUE IS READ AT THE PRECISION THE QUANTITY RESOLVES IT, so a quantile with more
	// digits than that resolution keeps the rounded value and not the written one.
	//
	// +optional
	LatencyQuantile *resource.Quantity `json:"latencyQuantile,omitempty" protobuf:"bytes,9,opt,name=latencyQuantile"`
}

// ModelDeploymentScalerPredictiveSpec holds the parameters of the Predictive algorithm.
//
// It carries the TokenThroughput parameters as well, because the predictor falls back to the
// token throughput result while it has not seen enough points yet.
type ModelDeploymentScalerPredictiveSpec struct {
	// TokenThroughput holds the parameters the predictor falls back to.
	//
	// +optional
	TokenThroughput ModelDeploymentScalerTokenThroughputSpec `json:"tokenThroughput,omitempty" protobuf:"bytes,1,opt,name=tokenThroughput"`

	// HorizonSeconds is how far ahead the demand is forecast.
	//
	// +optional
	// +k8s:validation:minimum=30
	// +k8s:validation:maximum=600
	HorizonSeconds int32 `json:"horizonSeconds,omitempty" protobuf:"varint,2,opt,name=horizonSeconds"`

	// PrefillTokenCapacityPerReplica is the prompt-token rate one Prefill replica sustains.
	//
	// +optional
	PrefillTokenCapacityPerReplica *resource.Quantity `json:"prefillTokenCapacityPerReplica,omitempty" protobuf:"bytes,3,opt,name=prefillTokenCapacityPerReplica"` // nolint: lll

	// DecodeTokenCapacityPerReplica is the generation-token rate one Decode replica sustains.
	//
	// +optional
	DecodeTokenCapacityPerReplica *resource.Quantity `json:"decodeTokenCapacityPerReplica,omitempty" protobuf:"bytes,4,opt,name=decodeTokenCapacityPerReplica"` // nolint: lll

	// ServerTokenCapacityPerReplica is the token rate one Server replica sustains.
	//
	// +optional
	ServerTokenCapacityPerReplica *resource.Quantity `json:"serverTokenCapacityPerReplica,omitempty" protobuf:"bytes,5,opt,name=serverTokenCapacityPerReplica"` // nolint: lll

	// QLevel is how much the level of the forecast follows each new observation.
	//
	// +optional
	QLevel *resource.Quantity `json:"qLevel,omitempty" protobuf:"bytes,6,opt,name=qLevel"`

	// QTrend is how much the trend of the forecast follows each new observation.
	//
	// +optional
	QTrend *resource.Quantity `json:"qTrend,omitempty" protobuf:"bytes,7,opt,name=qTrend"`

	// MeasurementNoise is how much the forecast trusts each new observation.
	//
	// +optional
	MeasurementNoise *resource.Quantity `json:"measurementNoise,omitempty" protobuf:"bytes,8,opt,name=measurementNoise"`

	// MinPoints is how many observations the predictor needs before it forecasts.
	//
	// Until it has them, the algorithm reports that it is warming up and returns the token
	// throughput result.
	//
	// +optional
	// +k8s:validation:minimum=1
	// +k8s:validation:maximum=64
	MinPoints int32 `json:"minPoints,omitempty" protobuf:"varint,9,opt,name=minPoints"`
}

// ModelDeploymentScalerCapacityPriorSpec permits resolving missing token capacities from an
// offline catalog for TokenThroughput and Predictive.
//
// The presence of the spec is the request, because it holds no switch that turns the resolution
// off. The resolution fills only the capacities the scaler does not state, and only from the
// offline catalog, so there is no source outside it.
type ModelDeploymentScalerCapacityPriorSpec struct {
	// CatalogRef names an optional ConfigMap and key in this namespace as the sole catalog source.
	//
	// When omitted, the embedded catalog is used.
	//
	// +optional
	CatalogRef *ModelDeploymentScalerCatalogReference `json:"catalogRef,omitempty" protobuf:"bytes,2,opt,name=catalogRef"`

	// Headroom is the margin in (0,1] applied to catalog-derived seed capacities.
	//
	// It defaults to 0.75 when capacityPrior is present and does not apply to explicit capacity
	// fields.
	//
	// +optional
	Headroom *resource.Quantity `json:"headroom,omitempty" protobuf:"bytes,4,opt,name=headroom"`
}

// ModelDeploymentScalerCatalogReference selects a catalog key in a ConfigMap in the scaler's
// namespace.
type ModelDeploymentScalerCatalogReference struct {
	// Name is the name of the ConfigMap in this namespace.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Key is the ConfigMap data key that holds the catalog document.
	//
	// The document is YAML or JSON, and the reader accepts the catalog under
	// the priors.yaml key and under the priors.json key.
	//
	// It defaults to priors.yaml when omitted.
	//
	// +optional
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	// +k8s:validation:default="priors.yaml"
	Key string `json:"key,omitempty" protobuf:"bytes,2,opt,name=key"`
}

// ModelDeploymentScalerBehaviorSpec tunes how fast a role may move and how long the scaler
// waits after a failure.
type ModelDeploymentScalerBehaviorSpec struct {
	// ScaleUp tunes how fast a role may grow.
	//
	// +optional
	ScaleUp ModelDeploymentScalerScaleUpRules `json:"scaleUp,omitempty" protobuf:"bytes,1,opt,name=scaleUp"`

	// ScaleDown tunes how fast a role may shrink and what happens after an aborted retirement.
	//
	// +optional
	ScaleDown ModelDeploymentScalerScaleDownRules `json:"scaleDown,omitempty" protobuf:"bytes,2,opt,name=scaleDown"`
}

// ModelDeploymentScalerScaleUpRules tune how fast a role may grow.
type ModelDeploymentScalerScaleUpRules struct {
	// StabilizationWindowSeconds is how long a recommendation is held before it takes effect.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=600
	StabilizationWindowSeconds int32 `json:"stabilizationWindowSeconds,omitempty" protobuf:"varint,1,opt,name=stabilizationWindowSeconds"`

	// MaxStepReplicas is the most replicas one pass may add to a role.
	//
	// Zero leaves the bound unset, so only the percentage bound applies.
	//
	// +optional
	// +k8s:validation:minimum=0
	MaxStepReplicas int32 `json:"maxStepReplicas,omitempty" protobuf:"varint,2,opt,name=maxStepReplicas"`

	// MaxStepPercent is the most percent of the current replica count one pass may add to a role.
	//
	// Zero leaves the bound unset, so only the replica bound applies.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=1000
	MaxStepPercent int32 `json:"maxStepPercent,omitempty" protobuf:"varint,3,opt,name=maxStepPercent"`

	// StepPolicy is a step bound stated in one unit.
	//
	// It replaces the two bounds above, so a deployment states its cap once instead of twice.
	//
	// +optional
	StepPolicy *ModelDeploymentScalerStepPolicy `json:"stepPolicy,omitempty" protobuf:"bytes,4,opt,name=stepPolicy"`
}

// ModelDeploymentScalerScaleDownRules tune how fast a role may shrink.
type ModelDeploymentScalerScaleDownRules struct {
	// StabilizationWindowSeconds is the window the recommendations are remembered over.
	//
	// The largest recommendation in that window is the floor a scale-down may go to, so a role
	// that was asked to grow does not shrink again right away.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=1800
	StabilizationWindowSeconds int32 `json:"stabilizationWindowSeconds,omitempty" protobuf:"varint,1,opt,name=stabilizationWindowSeconds"`

	// CooldownAfterAbortSeconds is how long a role is held after an aborted retirement.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=3600
	CooldownAfterAbortSeconds int32 `json:"cooldownAfterAbortSeconds,omitempty" protobuf:"varint,2,opt,name=cooldownAfterAbortSeconds"`

	// DecodePostScaleUpGraceSeconds is how long a Decode role is held after it grew.
	//
	// A Decode role that just grew does not shrink again inside this window, because a pair that
	// oscillates costs more than it saves.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=3600
	DecodePostScaleUpGraceSeconds int32 `json:"decodePostScaleUpGraceSeconds,omitempty" protobuf:"varint,3,opt,name=decodePostScaleUpGraceSeconds"`

	// StepPolicy is a step bound stated in one unit.
	//
	// It replaces the default of removing at most one replica per role per pass.
	//
	// +optional
	StepPolicy *ModelDeploymentScalerStepPolicy `json:"stepPolicy,omitempty" protobuf:"bytes,4,opt,name=stepPolicy"`
}

// ModelDeploymentScalerStepPolicyType is the unit a step bound is expressed in.
type ModelDeploymentScalerStepPolicyType string

const (
	// ModelDeploymentScalerStepPolicyTypePods counts replicas.
	ModelDeploymentScalerStepPolicyTypePods ModelDeploymentScalerStepPolicyType = "Pods"

	// ModelDeploymentScalerStepPolicyTypePercent counts percent of the current replica count.
	ModelDeploymentScalerStepPolicyTypePercent ModelDeploymentScalerStepPolicyType = "Percent"
)

// ModelDeploymentScalerStepPolicy is a bound on how far one role may move in one pass.
type ModelDeploymentScalerStepPolicy struct {
	// Type is the unit of Value.
	//
	// +required
	// +k8s:validation:enum=["Pods","Percent"]
	Type ModelDeploymentScalerStepPolicyType `json:"type" protobuf:"bytes,1,name=type"`

	// Value is the bound expressed in the unit Type names.
	//
	// +required
	// +k8s:validation:minimum=1
	Value int32 `json:"value" protobuf:"varint,2,opt,name=value"`
}

// ModelDeploymentScalerRebalanceSpec tunes what the scaler does when one role cannot grow.
type ModelDeploymentScalerRebalanceSpec struct {
	// Policy decides whether another role may give resources back to an exhausted one.
	//
	// +optional
	// +k8s:validation:enum=["Opposing","Priority","Disabled"]
	Policy ModelDeploymentScalerRebalancePolicy `json:"policy,omitempty" protobuf:"bytes,1,opt,name=policy"`

	// Priority orders the roles from the one that may give resources away to the one that may
	// not.
	//
	// It only means something under the Priority policy, where a role late in the order is the
	// first one asked to shrink. A role that is not named keeps the place of its own
	// recommendation.
	//
	// +optional
	// +listType=atomic
	Priority []string `json:"priority,omitempty" protobuf:"bytes,2,rep,name=priority"`
}

// ModelDeploymentScalerRebalancePolicy decides whether another role may give resources back.
type ModelDeploymentScalerRebalancePolicy string

const (
	// ModelDeploymentScalerRebalancePolicyOpposing lets a role whose own recommendation is to
	// shrink give a replica to an exhausted role.
	ModelDeploymentScalerRebalancePolicyOpposing ModelDeploymentScalerRebalancePolicy = "Opposing"

	// ModelDeploymentScalerRebalancePolicyPriority lets a role give a replica to an exhausted
	// role even when its own recommendation is to hold, following the order in priority.
	ModelDeploymentScalerRebalancePolicyPriority ModelDeploymentScalerRebalancePolicy = "Priority"

	// ModelDeploymentScalerRebalancePolicyDisabled turns rebalancing off.
	ModelDeploymentScalerRebalancePolicyDisabled ModelDeploymentScalerRebalancePolicy = "Disabled"
)

// ModelDeploymentScalerStatus is the observed state of ModelDeploymentScaler.
type ModelDeploymentScalerStatus struct {
	// ObservedGeneration is the generation this status was computed from.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,1,opt,name=observedGeneration"`

	// Binding is the target this scaler is bound to.
	//
	// +optional
	Binding *ModelDeploymentScalerBindingStatus `json:"binding,omitempty" protobuf:"bytes,2,opt,name=binding"`

	// Roles is what the scaler saw and what it recommends for each role.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	Roles []ModelDeploymentScalerRoleStatus `json:"roles,omitempty" protobuf:"bytes,3,rep,name=roles"`

	// LastCommit is the decision the scaler last committed.
	//
	// +optional
	LastCommit *ModelDeploymentScalerCommitStatus `json:"lastCommit,omitempty" protobuf:"bytes,4,opt,name=lastCommit"`

	// PendingCohort is the scale-up the scaler committed and is waiting on.
	//
	// +optional
	PendingCohort *ModelDeploymentScalerPendingCohortStatus `json:"pendingCohort,omitempty" protobuf:"bytes,5,opt,name=pendingCohort"`

	// Rebalance is the transaction in which one role gives a replica to another.
	//
	// +optional
	Rebalance *ModelDeploymentScalerRebalanceStatus `json:"rebalance,omitempty" protobuf:"bytes,6,opt,name=rebalance"`

	// LastScaleDown is the outcome of the last scale-down the scaler committed.
	//
	// +optional
	LastScaleDown *ModelDeploymentScalerScaleDownStatus `json:"lastScaleDown,omitempty" protobuf:"bytes,7,opt,name=lastScaleDown"`

	// Predictors is the state of the predictor of each role.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	Predictors []ModelDeploymentScalerPredictorStatus `json:"predictors,omitempty" protobuf:"bytes,8,rep,name=predictors"`

	// Conditions holds the conditions of the scaler.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,9,rep,name=conditions"`

	// ResolvedCapacityPriors records the resolved capacity prior basis and per-field effective
	// capacities for each role.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	ResolvedCapacityPriors []ModelDeploymentScalerResolvedCapacityPriorStatus `json:"resolvedCapacityPriors,omitempty" protobuf:"bytes,10,rep,name=resolvedCapacityPriors"` // nolint: lll

	// LatencyQuantile is the quantile of the latency distribution the scaler is held to.
	//
	// IT IS THE QUANTILE THE SCALER ASKED FOR, and the observed latencies of every role were
	// sampled at it. It is absent only when the scaler names none, which means the ninety
	// fifth percentile.
	//
	// IT IS A STABLE VALUE, because it changes when the configuration changes and not when the
	// sampler reads a new sample.
	//
	// +optional
	// +k8s:validation:maxLength=64
	LatencyQuantile string `json:"latencyQuantile,omitempty" protobuf:"bytes,11,opt,name=latencyQuantile"`
}

// ModelDeploymentScalerBindingStatus is the binding a scaler holds.
type ModelDeploymentScalerBindingStatus struct {
	// TargetUID is the UID of the bound target.
	//
	// A target that was deleted and recreated carries a different UID, so the binding tells the
	// two apart.
	//
	// +optional
	TargetUID string `json:"targetUID,omitempty" protobuf:"bytes,1,opt,name=targetUID"`

	// TargetName is the name of the bound target.
	//
	// +optional
	TargetName string `json:"targetName,omitempty" protobuf:"bytes,2,opt,name=targetName"`

	// Term is the binding term the scaler writes under.
	//
	// +optional
	Term int64 `json:"term" protobuf:"varint,3,opt,name=term"`
}

// ModelDeploymentScalerRoleStatus is what the scaler saw and recommends for one role.
type ModelDeploymentScalerRoleStatus struct {
	// Name is the role this status belongs to.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// CurrentReplicas is the replica count the target reports for this role.
	//
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty" protobuf:"varint,2,opt,name=currentReplicas"`

	// ReadyReplicas is the replica count of this role that is ready to serve.
	//
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty" protobuf:"varint,3,opt,name=readyReplicas"`

	// SignalState is how much the scaler trusts what it observed for this role.
	//
	// It is Fresh when every required signal was read in the last window, Idle when the role was
	// read and had nothing to do, Missing when a required signal was absent and Stale when the
	// window it belongs to has passed.
	//
	// +optional
	SignalState string `json:"signalState,omitempty" protobuf:"bytes,4,opt,name=signalState"`

	// PromptTokensPerSecond is the prompt-token rate the scaler observed.
	//
	// +optional
	PromptTokensPerSecond *resource.Quantity `json:"promptTokensPerSecond,omitempty" protobuf:"bytes,5,opt,name=promptTokensPerSecond"`

	// GenerationTokensPerSecond is the generation-token rate the scaler observed.
	//
	// +optional
	GenerationTokensPerSecond *resource.Quantity `json:"generationTokensPerSecond,omitempty" protobuf:"bytes,6,opt,name=generationTokensPerSecond"`

	// RunningRequests is the number of requests the role was running.
	//
	// +optional
	RunningRequests int32 `json:"runningRequests,omitempty" protobuf:"varint,7,opt,name=runningRequests"`

	// WaitingRequests is the number of requests the role was waiting for.
	//
	// +optional
	WaitingRequests int32 `json:"waitingRequests,omitempty" protobuf:"varint,8,opt,name=waitingRequests"`

	// KVCacheUsage is how much of the key value cache the role was using.
	//
	// +optional
	KVCacheUsage *resource.Quantity `json:"kvCacheUsage,omitempty" protobuf:"bytes,9,opt,name=kvCacheUsage"`

	// TTFTMilliseconds is the time to first token the scaler observed.
	//
	// +optional
	TTFTMilliseconds *resource.Quantity `json:"ttftMilliseconds,omitempty" protobuf:"bytes,10,opt,name=ttftMilliseconds"`

	// ITLMilliseconds is the inter-token latency the scaler observed.
	//
	// +optional
	ITLMilliseconds *resource.Quantity `json:"itlMilliseconds,omitempty" protobuf:"bytes,11,opt,name=itlMilliseconds"`

	// RawRecommendedReplicas is the replica count the algorithm asked for, before the bounds and
	// the ratios were applied.
	//
	// +optional
	RawRecommendedReplicas int32 `json:"rawRecommendedReplicas,omitempty" protobuf:"varint,12,opt,name=rawRecommendedReplicas"`

	// ConstrainedReplicas is the replica count the planner settled on after the bounds and the
	// ratios were applied.
	//
	// +optional
	ConstrainedReplicas int32 `json:"constrainedReplicas,omitempty" protobuf:"varint,13,opt,name=constrainedReplicas"`

	// Reason says why the recommendation is what it is.
	//
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,14,opt,name=reason"`

	// LastScaleUpTime is when this role last grew.
	//
	// +optional
	LastScaleUpTime *meta.Time `json:"lastScaleUpTime,omitempty" protobuf:"bytes,15,opt,name=lastScaleUpTime"`

	// StabilizationHistory is the recommendation this role received over the last window.
	//
	// It is bounded, and it is what lets the scale-down floor survive a restart.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=256
	StabilizationHistory []ModelDeploymentScalerStabilizationSample `json:"stabilizationHistory,omitempty" protobuf:"bytes,16,rep,name=stabilizationHistory"` // nolint: lll
}

// ModelDeploymentScalerStabilizationSample is one recommendation a role received.
type ModelDeploymentScalerStabilizationSample struct {
	// Timestamp is when the recommendation was made.
	//
	// +required
	Timestamp meta.Time `json:"timestamp" protobuf:"bytes,1,name=timestamp"`

	// Replicas is the replica count the recommendation asked for.
	//
	// +required
	Replicas int32 `json:"replicas" protobuf:"varint,2,opt,name=replicas"`
}

// ModelDeploymentScalerCommitStatus is the decision the scaler last committed.
type ModelDeploymentScalerCommitStatus struct {
	// DecisionID identifies the decision.
	//
	// +optional
	DecisionID string `json:"decisionID,omitempty" protobuf:"bytes,1,opt,name=decisionID"`

	// Term is the binding term the decision was written under.
	//
	// +optional
	Term int64 `json:"term" protobuf:"varint,2,opt,name=term"`

	// Vector is the replica count per role the decision committed.
	//
	// +optional
	Vector map[string]int32 `json:"vector,omitempty" protobuf:"bytes,3,rep,name=vector"`

	// BaselineVector is the replica count per role the decision started from.
	//
	// It is the vector the scaler restores when the decision has to be retracted.
	//
	// +optional
	BaselineVector map[string]int32 `json:"baselineVector,omitempty" protobuf:"bytes,4,rep,name=baselineVector"`

	// Timestamp is when the decision was committed.
	//
	// +optional
	Timestamp *meta.Time `json:"timestamp,omitempty" protobuf:"bytes,5,opt,name=timestamp"`

	// TargetGeneration is the generation of the target the decision was computed from.
	//
	// +optional
	TargetGeneration int64 `json:"targetGeneration,omitempty" protobuf:"varint,6,opt,name=targetGeneration"`
}

// ModelDeploymentScalerPendingCohortStatus is the scale-up the scaler is waiting on.
type ModelDeploymentScalerPendingCohortStatus struct {
	// DecisionID identifies the decision that added the cohort.
	//
	// +optional
	DecisionID string `json:"decisionID,omitempty" protobuf:"bytes,1,opt,name=decisionID"`

	// Term is the binding term the decision was written under.
	//
	// +optional
	Term int64 `json:"term" protobuf:"varint,2,opt,name=term"`

	// BaselineVector is the replica count per role before the cohort was added.
	//
	// It is the vector the scaler restores when the cohort is retracted.
	//
	// +optional
	BaselineVector map[string]int32 `json:"baselineVector,omitempty" protobuf:"bytes,3,rep,name=baselineVector"`

	// TargetVector is the replica count per role the cohort asked for.
	//
	// +optional
	TargetVector map[string]int32 `json:"targetVector,omitempty" protobuf:"bytes,4,rep,name=targetVector"`

	// StartedAt is when the cohort was committed.
	//
	// +optional
	StartedAt *meta.Time `json:"startedAt,omitempty" protobuf:"bytes,5,opt,name=startedAt"`
}

// ModelDeploymentScalerRebalanceStatus is the transaction in which one role gives a replica to
// another.
type ModelDeploymentScalerRebalanceStatus struct {
	// ReceiverRole is the role that receives the replica.
	//
	// +optional
	ReceiverRole string `json:"receiverRole,omitempty" protobuf:"bytes,1,opt,name=receiverRole"`

	// DonorRole is the role that gives the replica.
	//
	// +optional
	DonorRole string `json:"donorRole,omitempty" protobuf:"bytes,2,opt,name=donorRole"`

	// DonorTargetReplicas is the replica count the donor is heading for.
	//
	// +optional
	DonorTargetReplicas int32 `json:"donorTargetReplicas,omitempty" protobuf:"varint,3,opt,name=donorTargetReplicas"`

	// ReceiverTargetReplicas is the replica count the receiver is heading for.
	//
	// +optional
	ReceiverTargetReplicas int32 `json:"receiverTargetReplicas,omitempty" protobuf:"varint,4,opt,name=receiverTargetReplicas"`

	// StartedAt is when the transaction began.
	//
	// +optional
	StartedAt *meta.Time `json:"startedAt,omitempty" protobuf:"bytes,5,opt,name=startedAt"`

	// ExhaustionEpisodeGeneration counts the exhaustion episodes the transaction belongs to.
	//
	// It keeps one episode from draining more donors than it should.
	//
	// +optional
	ExhaustionEpisodeGeneration int64 `json:"exhaustionEpisodeGeneration,omitempty" protobuf:"varint,6,opt,name=exhaustionEpisodeGeneration"`

	// DonationAttempted says whether the donor was already asked to give the replica.
	//
	// +optional
	DonationAttempted bool `json:"donationAttempted,omitempty" protobuf:"varint,7,opt,name=donationAttempted"`

	// Reason says why the transaction is where it is.
	//
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,8,opt,name=reason"`
}

// ModelDeploymentScalerScaleDownStatus is the outcome of the last scale-down.
type ModelDeploymentScalerScaleDownStatus struct {
	// Role is the role the scale-down removed a replica from.
	//
	// +optional
	Role string `json:"role,omitempty" protobuf:"bytes,1,opt,name=role"`

	// Ordinal is the replica the scale-down removed.
	//
	// +optional
	Ordinal int32 `json:"ordinal,omitempty" protobuf:"varint,2,opt,name=ordinal"`

	// PreScaleDownVector is the replica count per role before the scale-down.
	//
	// It is the vector the scaler restores when the retirement aborts.
	//
	// +optional
	PreScaleDownVector map[string]int32 `json:"preScaleDownVector,omitempty" protobuf:"bytes,3,rep,name=preScaleDownVector"`

	// Outcome is how the scale-down ended.
	//
	// It is Committed while the retirement runs and Aborted when the retirement failed and the
	// scaler restored the vector.
	//
	// +optional
	Outcome string `json:"outcome,omitempty" protobuf:"bytes,4,opt,name=outcome"`

	// AbortedAt is when the retirement aborted.
	//
	// +optional
	AbortedAt *meta.Time `json:"abortedAt,omitempty" protobuf:"bytes,5,opt,name=abortedAt"`

	// CooldownUntil is when the role may shrink again.
	//
	// +optional
	CooldownUntil *meta.Time `json:"cooldownUntil,omitempty" protobuf:"bytes,6,opt,name=cooldownUntil"`
}

// ModelDeploymentScalerPredictorStatus is the state of the predictor of one role.
type ModelDeploymentScalerPredictorStatus struct {
	// Name is the role this predictor belongs to.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Level is the demand the predictor currently believes in.
	//
	// +optional
	Level *resource.Quantity `json:"level,omitempty" protobuf:"bytes,2,opt,name=level"`

	// Trend is how fast the predictor believes the demand is moving.
	//
	// +optional
	Trend *resource.Quantity `json:"trend,omitempty" protobuf:"bytes,3,opt,name=trend"`

	// Covariance is how much the predictor trusts its own estimate.
	//
	// +optional
	Covariance *ModelDeploymentScalerKalmanCovariance `json:"covariance,omitempty" protobuf:"bytes,4,opt,name=covariance"`

	// SampleCount is how many observations the predictor has seen.
	//
	// +optional
	SampleCount int32 `json:"sampleCount,omitempty" protobuf:"varint,5,opt,name=sampleCount"`

	// History is the demand the predictor has seen.
	//
	// It is bounded, because the predictor only needs the recent past.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=256
	History []ModelDeploymentScalerDemandSample `json:"history,omitempty" protobuf:"bytes,6,rep,name=history"`
}

// ModelDeploymentScalerKalmanCovariance is the covariance matrix of a predictor.
//
// It is a two by two matrix in the order level, trend, so the four entries are the variance of
// the level, the covariance of the level with the trend, the covariance of the trend with the
// level, and the variance of the trend.
type ModelDeploymentScalerKalmanCovariance struct {
	// P00 is the variance of the level.
	//
	// +optional
	P00 *resource.Quantity `json:"p00,omitempty" protobuf:"bytes,1,opt,name=p00"`

	// P01 is the covariance of the level with the trend.
	//
	// +optional
	P01 *resource.Quantity `json:"p01,omitempty" protobuf:"bytes,2,opt,name=p01"`

	// P10 is the covariance of the trend with the level.
	//
	// +optional
	P10 *resource.Quantity `json:"p10,omitempty" protobuf:"bytes,3,opt,name=p10"`

	// P11 is the variance of the trend.
	//
	// +optional
	P11 *resource.Quantity `json:"p11,omitempty" protobuf:"bytes,4,opt,name=p11"`
}

// ModelDeploymentScalerDemandSample is one observation the predictor has seen.
type ModelDeploymentScalerDemandSample struct {
	// Timestamp is when the observation was made.
	//
	// +required
	Timestamp meta.Time `json:"timestamp" protobuf:"bytes,1,name=timestamp"`

	// TokensPerSecond is the demand that was observed.
	//
	// +required
	TokensPerSecond resource.Quantity `json:"tokensPerSecond" protobuf:"bytes,2,name=tokensPerSecond"`
}

// ModelDeploymentScalerCapacityFieldSource names where an effective capacity field value came
// from.
type ModelDeploymentScalerCapacityFieldSource string

const (
	// ModelDeploymentScalerCapacityFieldSourceExplicit says the capacity field was set explicitly
	// in the scaler spec.
	ModelDeploymentScalerCapacityFieldSourceExplicit ModelDeploymentScalerCapacityFieldSource = "Explicit"

	// ModelDeploymentScalerCapacityFieldSourceCatalog says the capacity field was derived from a
	// catalog profile.
	ModelDeploymentScalerCapacityFieldSourceCatalog ModelDeploymentScalerCapacityFieldSource = "Catalog"
)

// ModelDeploymentScalerResolvedCapacityPriorStatus records the resolved capacity prior basis and
// effective field values for one role.
type ModelDeploymentScalerResolvedCapacityPriorStatus struct {
	// Name is the role this status belongs to.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// SourceID is the catalog source identifier the profile came from.
	//
	// +optional
	// +k8s:validation:maxLength=253
	SourceID string `json:"sourceID,omitempty" protobuf:"bytes,3,opt,name=sourceID"`

	// ProfileDigest is the content digest of the catalog profile.
	//
	// +optional
	// +k8s:validation:maxLength=253
	ProfileDigest string `json:"profileDigest,omitempty" protobuf:"bytes,4,opt,name=profileDigest"`

	// CompatibilityFingerprint covers the target, role, model, engine, hardware, quantization,
	// topology, workload, catalog digest, and latency targets this resolution depends on.
	//
	// +optional
	// +k8s:validation:maxLength=253
	CompatibilityFingerprint string `json:"compatibilityFingerprint,omitempty" protobuf:"bytes,5,opt,name=compatibilityFingerprint"` // nolint: lll

	// Metric is the throughput metric numerator the capacity was resolved from.
	//
	// +optional
	// +k8s:validation:maxLength=253
	Metric string `json:"metric,omitempty" protobuf:"bytes,6,opt,name=metric"`

	// Quantile is the latency quantile statistic used to verify the role's latency constraints.
	//
	// +optional
	// +k8s:validation:maxLength=253
	Quantile string `json:"quantile,omitempty" protobuf:"bytes,7,opt,name=quantile"`

	// WorkloadID is the workload envelope identifier the profile was measured under.
	//
	// +optional
	// +k8s:validation:maxLength=253
	WorkloadID string `json:"workloadID,omitempty" protobuf:"bytes,8,opt,name=workloadID"`

	// Denominator is the measurement denominator reconstructed for one scaling replica.
	//
	// +optional
	// +k8s:validation:maxLength=253
	Denominator string `json:"denominator,omitempty" protobuf:"bytes,9,opt,name=denominator"`

	// Headroom is the margin applied to the catalog seed capacity.
	//
	// +optional
	Headroom *resource.Quantity `json:"headroom,omitempty" protobuf:"bytes,10,opt,name=headroom"`

	// SeedTokensPerSecond is the per-replica catalog seed capacity before headroom is applied.
	//
	// +optional
	SeedTokensPerSecond *resource.Quantity `json:"seedTokensPerSecond,omitempty" protobuf:"bytes,11,opt,name=seedTokensPerSecond"` // nolint: lll

	// Fields records the effective value and source of each required capacity field for this role.
	//
	// +optional
	// +listType=map
	// +listMapKey=field
	// +k8s:validation:maxItems=8
	Fields []ModelDeploymentScalerResolvedCapacityFieldStatus `json:"fields,omitempty" protobuf:"bytes,12,rep,name=fields"` // nolint: lll

	// CatalogDigest is the content digest of the catalog the profile was resolved from.
	//
	// +optional
	// +k8s:validation:maxLength=253
	CatalogDigest string `json:"catalogDigest,omitempty" protobuf:"bytes,13,opt,name=catalogDigest"`

	// SourceRevision pins the upstream source revision the catalog profile was imported from.
	//
	// +optional
	// +k8s:validation:maxLength=253
	SourceRevision string `json:"sourceRevision,omitempty" protobuf:"bytes,14,opt,name=sourceRevision"`

	// WorkloadRevision pins the workload definition the profile was measured under.
	//
	// +optional
	// +k8s:validation:maxLength=253
	WorkloadRevision string `json:"workloadRevision,omitempty" protobuf:"bytes,15,opt,name=workloadRevision"`

	// GPUsPerReplica is the number of accelerators in one scaling replica used to reconstruct a
	// PerGPU measurement denominator.
	//
	// +optional
	// +k8s:validation:minimum=0
	GPUsPerReplica int32 `json:"gpusPerReplica,omitempty" protobuf:"varint,16,opt,name=gpusPerReplica"`

	// MeasuredReplicas is the number of scaling replicas in the measured system used to
	// reconstruct a WholeSystem measurement denominator.
	//
	// +optional
	// +k8s:validation:minimum=0
	MeasuredReplicas int32 `json:"measuredReplicas,omitempty" protobuf:"varint,17,opt,name=measuredReplicas"`

	// LatencyBounds records the quantile and conservative-bound basis for each required latency
	// metric of the role.
	//
	// +optional
	// +listType=map
	// +listMapKey=metric
	// +k8s:validation:maxItems=8
	LatencyBounds []ModelDeploymentScalerResolvedLatencyBoundStatus `json:"latencyBounds,omitempty" protobuf:"bytes,18,rep,name=latencyBounds"` // nolint: lll

	// RequestedQuantile is the latency quantile the resolution was held to.
	//
	// IT IS THE QUANTILE THE SCALER ASKED FOR and not the quantile the catalog measured, and
	// the two differ when the catalog measured a higher quantile than the request needed.
	//
	// +optional
	// +k8s:validation:maxLength=253
	RequestedQuantile string `json:"requestedQuantile,omitempty" protobuf:"bytes,19,opt,name=requestedQuantile"`
}

// ModelDeploymentScalerResolvedLatencyBoundStatus records the measured quantile basis for one
// required latency metric.
type ModelDeploymentScalerResolvedLatencyBoundStatus struct {
	// Metric is the required latency metric name, such as TTFT or ITL.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=64
	Metric string `json:"metric" protobuf:"bytes,1,name=metric"`

	// Quantile is the measured latency quantile statistic used to bound the metric.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=64
	Quantile string `json:"quantile" protobuf:"bytes,2,name=quantile"`

	// Conservative says whether a quantile higher than the required quantile was used as a
	// conservative upper bound.
	//
	// +optional
	Conservative bool `json:"conservative,omitempty" protobuf:"varint,3,opt,name=conservative"`
}

// ModelDeploymentScalerResolvedCapacityFieldStatus records the effective value and source of one
// algorithm capacity field.
type ModelDeploymentScalerResolvedCapacityFieldStatus struct {
	// Field is the name of the capacity field.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Field string `json:"field" protobuf:"bytes,1,name=field"`

	// Source says whether the effective value came from an explicit spec field or from the
	// catalog.
	//
	// +required
	// +k8s:validation:enum=["Explicit","Catalog"]
	Source ModelDeploymentScalerCapacityFieldSource `json:"source" protobuf:"bytes,2,name=source"`

	// EffectiveTokensPerSecond is the effective per-replica token capacity used for this field.
	//
	// +required
	EffectiveTokensPerSecond resource.Quantity `json:"effectiveTokensPerSecond" protobuf:"bytes,3,name=effectiveTokensPerSecond"` // nolint: lll
}
