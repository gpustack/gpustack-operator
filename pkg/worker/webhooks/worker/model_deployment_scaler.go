package worker

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// The values a scaler starts from when the object leaves them out.
//
// EVERY DEFAULT HERE IS A VALUE THE ALGORITHM NEEDS TO HAVE, so an object that omits one is
// refused by the schema rather than answered with a zero. The zero value of a count is a
// meaningful count in some of these fields and a missing one in others, which is why each is
// named rather than derived.
const (
	// modelDeploymentScalerDefaultIntervalSeconds is how often a scaler looks at its target.
	modelDeploymentScalerDefaultIntervalSeconds = 15

	// modelDeploymentScalerDefaultPendingTimeoutSeconds is how long a committed scale-up may
	// stay unadmitted before it is retracted.
	modelDeploymentScalerDefaultPendingTimeoutSeconds = 300

	// modelDeploymentScalerDefaultStableWindowSeconds is the window the stable recommendation
	// is computed over.
	modelDeploymentScalerDefaultStableWindowSeconds = 180

	// modelDeploymentScalerDefaultPanicWindowSeconds is the window the panic recommendation is
	// computed over.
	modelDeploymentScalerDefaultPanicWindowSeconds = 60

	// modelDeploymentScalerDefaultMaxStepReplicas is the most replicas one pass may add.
	modelDeploymentScalerDefaultMaxStepReplicas = 4

	// modelDeploymentScalerDefaultMaxStepPercent is the most percent one pass may add.
	modelDeploymentScalerDefaultMaxStepPercent = 100

	// modelDeploymentScalerDefaultScaleDownStabilizationSeconds is the window the scale-down
	// recommendations are remembered over.
	modelDeploymentScalerDefaultScaleDownStabilizationSeconds = 300

	// modelDeploymentScalerDefaultCooldownAfterAbortSeconds is how long a role is held after an
	// aborted retirement.
	modelDeploymentScalerDefaultCooldownAfterAbortSeconds = 300

	// modelDeploymentScalerDefaultDecodePostScaleUpGraceSeconds is how long a Decode role is
	// held after it grew.
	modelDeploymentScalerDefaultDecodePostScaleUpGraceSeconds = 120

	// modelDeploymentScalerDefaultMinReplicas is the fewest replicas a role may have.
	modelDeploymentScalerDefaultMinReplicas = 1

	// modelDeploymentScalerDefaultCatalogKey is the ConfigMap data key a catalog reference reads
	// when the key is omitted.
	modelDeploymentScalerDefaultCatalogKey = priors.DefaultCatalogConfigMapKey
)

// modelDeploymentScalerMaxReplicas is the most replicas a role may be scaled to.
//
// The number bounds the search for a feasible replica vector, so it is a property of the search
// rather than of the algorithm: a vector is chosen from a box of at most this many integers per
// role.
const modelDeploymentScalerMaxReplicas = 1024

// modelDeploymentScalerFeasibilityBudget is how many replica vectors the search for a feasible
// one may try before it gives up.
//
// THE SEARCH IS EXACT, and the budget is what keeps it from spending an admission request on a
// shape nobody runs. A search that runs out of budget does not refuse the object: it says it
// could not decide, because refusing a configuration that is in fact feasible strands an object
// its owner cannot repair by editing it.
const modelDeploymentScalerFeasibilityBudget = 1 << 14

// ModelDeploymentScalerWebhook defaults and validates a ModelDeploymentScaler.
//
// IT READS THE TARGET when the target exists, because a scaler that names a role the target does
// not have, or a role it cannot scale, is a scaler that plans for nothing. The read is skipped
// when the target is absent, because a scaler may be written before its target.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeploymentscalers",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeploymentscalers",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelDeploymentScalerWebhook struct {
	webhook.DefaultValidator

	// APIReader reads the target ModelDeployment. There is deliberately no cached client beside
	// it: a cache that is behind would judge a scaler against a target that no longer exists.
	APIReader ctrlcli.Reader
}

func (r *ModelDeploymentScalerWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.ModelDeploymentScaler{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelDeploymentScalerWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*ModelDeploymentScalerWebhook)(nil)
)

// Default fills the values a scaler leaves out.
//
// THE MODE DEFAULTS TO SHADOW, so a scaler that was just created observes and changes nothing
// until its owner asks for it to act. That is the difference between a planner being installed
// and a planner being trusted with a target.
//
// THE ALGORITHM TYPE IS TAKEN FROM THE BLOCK THAT IS PRESENT when the type is left out, so that
// a scaler naming only a predictive block does not silently run the token throughput algorithm
// with the predictive block ignored. A scaler naming no block at all runs the token throughput
// algorithm, which is the one whose parameters have defaults for every value it needs.
func (r *ModelDeploymentScalerWebhook) Default(ctx context.Context, obj runtime.Object) error {
	s := obj.(*workercore.ModelDeploymentScaler)
	if !s.DeletionTimestamp.IsZero() {
		return nil
	}
	_, _, err := r.defaultModelDeploymentScaler(ctx, s)
	return err
}

func (r *ModelDeploymentScalerWebhook) defaultModelDeploymentScaler(
	ctx context.Context, s *workercore.ModelDeploymentScaler,
) (*workercore.ModelDeployment, bool, error) {
	if s.Spec.Mode == "" {
		s.Spec.Mode = workercore.ModelDeploymentScalerModeShadow
	}

	if s.Spec.Sampling.IntervalSeconds == 0 {
		s.Spec.Sampling.IntervalSeconds = modelDeploymentScalerDefaultIntervalSeconds
	}
	if s.Spec.Sampling.PendingTimeoutSeconds == 0 {
		s.Spec.Sampling.PendingTimeoutSeconds = modelDeploymentScalerDefaultPendingTimeoutSeconds
	}

	if s.Spec.Algorithm.Type == "" {
		s.Spec.Algorithm.Type = modelDeploymentScalerAlgorithmTypeOf(s.Spec.Algorithm)
	}
	defaultModelDeploymentScalerCapacityPrior(s.Spec.Algorithm.CapacityPrior)
	suppressDefaultSLOs, md, targetRead, err := r.shouldSuppressDefaultSLOs(ctx, s)
	if err != nil {
		return nil, false, err
	}
	switch s.Spec.Algorithm.Type {
	case workercore.ModelDeploymentScalerAlgorithmTypeProportional:
		if s.Spec.Algorithm.Proportional == nil {
			s.Spec.Algorithm.Proportional = &workercore.ModelDeploymentScalerProportionalSpec{}
		}
	case workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput:
		if s.Spec.Algorithm.TokenThroughput == nil {
			s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{}
		}
		defaultModelDeploymentScalerTokenThroughput(s.Spec.Algorithm.TokenThroughput, suppressDefaultSLOs)
	case workercore.ModelDeploymentScalerAlgorithmTypePredictive:
		if s.Spec.Algorithm.Predictive == nil {
			s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{}
		}
		defaultModelDeploymentScalerTokenThroughput(&s.Spec.Algorithm.Predictive.TokenThroughput, suppressDefaultSLOs)
	}

	for i := range s.Spec.Roles {
		if s.Spec.Roles[i].MinReplicas == 0 {
			s.Spec.Roles[i].MinReplicas = modelDeploymentScalerDefaultMinReplicas
		}
	}

	if s.Spec.Behavior.ScaleUp.MaxStepReplicas == 0 {
		s.Spec.Behavior.ScaleUp.MaxStepReplicas = modelDeploymentScalerDefaultMaxStepReplicas
	}
	if s.Spec.Behavior.ScaleUp.MaxStepPercent == 0 {
		s.Spec.Behavior.ScaleUp.MaxStepPercent = modelDeploymentScalerDefaultMaxStepPercent
	}
	if s.Spec.Behavior.ScaleDown.StabilizationWindowSeconds == 0 {
		s.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = modelDeploymentScalerDefaultScaleDownStabilizationSeconds
	}
	if s.Spec.Behavior.ScaleDown.CooldownAfterAbortSeconds == 0 {
		s.Spec.Behavior.ScaleDown.CooldownAfterAbortSeconds = modelDeploymentScalerDefaultCooldownAfterAbortSeconds
	}
	if s.Spec.Behavior.ScaleDown.DecodePostScaleUpGraceSeconds == 0 {
		s.Spec.Behavior.ScaleDown.DecodePostScaleUpGraceSeconds = modelDeploymentScalerDefaultDecodePostScaleUpGraceSeconds
	}

	if s.Spec.Rebalance.Policy == "" {
		s.Spec.Rebalance.Policy = workercore.ModelDeploymentScalerRebalancePolicyOpposing
	}

	return md, targetRead, nil
}

// modelDeploymentScalerAlgorithmTypeOf picks the algorithm a scaler names by the block it filled.
func modelDeploymentScalerAlgorithmTypeOf(
	algorithm workercore.ModelDeploymentScalerAlgorithmSpec,
) workercore.ModelDeploymentScalerAlgorithmType {
	switch {
	case algorithm.Proportional != nil:
		return workercore.ModelDeploymentScalerAlgorithmTypeProportional
	case algorithm.Predictive != nil:
		return workercore.ModelDeploymentScalerAlgorithmTypePredictive
	default:
		return workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput
	}
}

// defaultModelDeploymentScalerCapacityPrior fills the defaults a capacity prior block leaves out.
func defaultModelDeploymentScalerCapacityPrior(cp *workercore.ModelDeploymentScalerCapacityPriorSpec) {
	if cp == nil {
		return
	}
	if cp.CatalogRef != nil && cp.CatalogRef.Key == "" {
		cp.CatalogRef.Key = modelDeploymentScalerDefaultCatalogKey
	}
	if cp.Headroom == nil {
		cp.Headroom = ptr.To(resource.MustParse("0.75"))
	}
}

// shouldSuppressDefaultSLOs reports whether latency target defaults must be suppressed so that
// catalog prior resolution requires explicit latency targets for roles lacking manual capacity.
func (r *ModelDeploymentScalerWebhook) shouldSuppressDefaultSLOs(
	ctx context.Context, s *workercore.ModelDeploymentScaler,
) (bool, *workercore.ModelDeployment, bool, error) {
	if !workerctrl.ModelDeploymentScalerNeedsCapacityPrior(s, nil) {
		return false, nil, false, nil
	}
	if r.APIReader == nil || strings.TrimSpace(s.Spec.TargetRef.Name) == "" {
		return true, nil, false, nil
	}
	md, err := r.readModelDeploymentScalerTarget(ctx, s)
	if err != nil {
		return false, nil, false, err
	}
	return workerctrl.ModelDeploymentScalerNeedsCapacityPrior(s, md), md, true, nil
}

// defaultModelDeploymentScalerTokenThroughput fills the values the token throughput algorithm
// needs to have. When capacity priors are resolved from a catalog, latency SLO defaults are left
// unset so that validation requires explicit latency targets.
func defaultModelDeploymentScalerTokenThroughput(
	tt *workercore.ModelDeploymentScalerTokenThroughputSpec, suppressDefaultSLOs bool,
) {
	if tt.StableWindowSeconds == 0 {
		tt.StableWindowSeconds = modelDeploymentScalerDefaultStableWindowSeconds
	}
	if tt.PanicWindowSeconds == 0 {
		tt.PanicWindowSeconds = modelDeploymentScalerDefaultPanicWindowSeconds
	}
	if tt.PanicThreshold == nil {
		tt.PanicThreshold = ptr.To(resource.MustParse("2.0"))
	}
	if suppressDefaultSLOs {
		return
	}
	if tt.TTFTTargetMilliseconds == nil {
		tt.TTFTTargetMilliseconds = ptr.To(resource.MustParse("500"))
	}
	if tt.ITLTargetMilliseconds == nil {
		tt.ITLTargetMilliseconds = ptr.To(resource.MustParse("50"))
	}
}

func (r *ModelDeploymentScalerWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	s := obj.(*workercore.ModelDeploymentScaler)

	warnings, errs := r.validateModelDeploymentScaler(ctx, s, false)
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(s.GroupVersionKind().GroupKind(), s.Name, errs)
	}

	return warnings, nil
}

func (r *ModelDeploymentScalerWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	s := newObj.(*workercore.ModelDeploymentScaler)
	if !s.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	old, _ := oldObj.(*workercore.ModelDeploymentScaler)

	warnings, errs := r.validateModelDeploymentScaler(ctx, s, true)
	errs = append(errs, validateModelDeploymentScalerTargetRefImmutable(old, s)...)
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(s.GroupVersionKind().GroupKind(), s.Name, errs)
	}

	return warnings, nil
}

// validateModelDeploymentScaler holds every rule a scaler is judged by.
//
// THE RULES THAT DO NOT NEED THE TARGET COME FIRST, and the ones that need it are skipped when
// it is absent. A scaler may be written before its target exists, and a rule that read a target
// that is not there would refuse an object that is merely early.
func (r *ModelDeploymentScalerWebhook) validateModelDeploymentScaler(
	ctx context.Context, s *workercore.ModelDeploymentScaler, isUpdate bool,
) (ctrladmission.Warnings, field.ErrorList) {
	var (
		warnings ctrladmission.Warnings
		errs     field.ErrorList
	)

	// THE OBJECT IS JUDGED AS IT WILL BE STORED, which is the object after defaulting. A field the
	// defaulting webhook fills is judged by the value it will carry, so a rule never refuses an
	// object for a value that is about to be filled in, and the two halves cannot disagree about
	// what a scaler that omitted a field means.
	effective := s.DeepCopy()
	md, targetRead, err := r.defaultModelDeploymentScaler(ctx, effective)
	if err != nil {
		return nil, append(errs, field.InternalError(field.NewPath("spec"), err))
	}

	errs = append(errs, validateModelDeploymentScalerMode(effective)...)
	errs = append(errs, validateModelDeploymentScalerTargetRef(effective)...)
	errs = append(errs, validateModelDeploymentScalerSampling(effective)...)
	errs = append(errs, validateModelDeploymentScalerRoles(effective)...)
	errs = append(errs, validateModelDeploymentScalerRatios(effective)...)
	errs = append(errs, validateModelDeploymentScalerBehavior(effective)...)
	errs = append(errs, validateModelDeploymentScalerRebalance(effective)...)
	errs = append(errs, validateModelDeploymentScalerAlgorithm(effective)...)

	if len(errs) > 0 {
		return nil, errs
	}

	errs = append(errs, validateModelDeploymentScalerRatioFeasibility(effective)...)

	if !targetRead {
		md, err = r.readModelDeploymentScalerTarget(ctx, s)
		if err != nil {
			return nil, append(errs, field.InternalError(field.NewPath("spec", "targetRef", "name"), err))
		}
	}
	if md == nil {
		if workerctrl.ModelDeploymentScalerNeedsCapacityPrior(effective, nil) {
			if sloErrs := validateModelDeploymentScalerUnresolvedTargetSLOs(effective); len(sloErrs) > 0 {
				return nil, append(errs, sloErrs...)
			}
		}
		if effective.Spec.Algorithm.CapacityPrior != nil {
			warnings = append(warnings, fmt.Sprintf(
				"target ModelDeployment %q does not exist yet; capacity prior compatibility will be verified by the controller once the target exists",
				effective.Spec.TargetRef.Name,
			))
		}
		return warnings, errs
	}

	targetWarnings, targetErrs := r.validateModelDeploymentScalerAgainstTarget(ctx, effective, md, isUpdate)
	warnings = append(warnings, targetWarnings...)
	errs = append(errs, targetErrs...)
	return warnings, errs
}

// readModelDeploymentScalerTarget reads the target of a scaler, or reports that it is absent.
func (r *ModelDeploymentScalerWebhook) readModelDeploymentScalerTarget(
	ctx context.Context, s *workercore.ModelDeploymentScaler,
) (*workercore.ModelDeployment, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("the scaler target cannot be read, because this handler has no reader")
	}

	md := new(workercore.ModelDeployment)
	err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: s.Namespace, Name: s.Spec.TargetRef.Name}, md)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the target ModelDeployment %q: %w", s.Spec.TargetRef.Name, err)
	}

	return md, nil
}

// validateModelDeploymentScalerMode validates the scaler execution mode.
func validateModelDeploymentScalerMode(s *workercore.ModelDeploymentScaler) field.ErrorList {
	switch s.Spec.Mode {
	case workercore.ModelDeploymentScalerModeShadow, workercore.ModelDeploymentScalerModeActive:
		return nil
	case "":
		return field.ErrorList{field.Required(field.NewPath("spec", "mode"),
			"a scaler either observes (Shadow) or acts (Active)")}
	default:
		return field.ErrorList{field.Invalid(field.NewPath("spec", "mode"), s.Spec.Mode,
			"a mode is either Shadow or Active")}
	}
}

// validateModelDeploymentScalerTargetRef refuses a scaler that names no target.
func validateModelDeploymentScalerTargetRef(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	if strings.TrimSpace(s.Spec.TargetRef.Name) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "targetRef", "name"),
			"this scaler plans for the ModelDeployment it names, and a scaler that names none "+
				"plans for nothing",
		))
	}

	return errs
}

// validateModelDeploymentScalerTargetRefImmutable refuses a scaler that is pointed at another
// target.
//
// THE TARGET IS WHAT THE SCALER OWNS, and a binding follows the target rather than the scaler:
// changing it would leave the old target carrying a binding nobody may write and the new target
// carrying none. A scaler that wants another target is a second scaler.
func validateModelDeploymentScalerTargetRefImmutable(
	old, s *workercore.ModelDeploymentScaler,
) field.ErrorList {
	errs := make(field.ErrorList, 0, 1)

	if old == nil || old.Spec.TargetRef.Name == s.Spec.TargetRef.Name {
		return errs
	}

	errs = append(errs, field.Invalid(
		field.NewPath("spec", "targetRef", "name"), s.Spec.TargetRef.Name,
		"the target of a scaler is fixed once it is set: a different target is a different "+
			"scaler, because the binding follows the target and the old one would be left "+
			"holding a binding nobody may write",
	))

	return errs
}

// validateModelDeploymentScalerSampling holds the two intervals a scaler works in.
func validateModelDeploymentScalerSampling(s *workercore.ModelDeploymentScaler) field.ErrorList {
	errs := make(field.ErrorList, 0, 2)

	path := field.NewPath("spec", "sampling")
	if s.Spec.Sampling.IntervalSeconds < 5 || s.Spec.Sampling.IntervalSeconds > 300 {
		errs = append(errs, field.Invalid(
			path.Child("intervalSeconds"), s.Spec.Sampling.IntervalSeconds,
			"the interval is how often the scaler looks at its target and how often it writes, "+
				"so it is between 5 and 300 seconds",
		))
	}
	if s.Spec.Sampling.PendingTimeoutSeconds < 60 || s.Spec.Sampling.PendingTimeoutSeconds > 1200 {
		errs = append(errs, field.Invalid(
			path.Child("pendingTimeoutSeconds"), s.Spec.Sampling.PendingTimeoutSeconds,
			"the pending timeout is how long a committed scale-up may stay unadmitted before it "+
				"is retracted, so it is between 60 and 1200 seconds",
		))
	}

	return errs
}

// validateModelDeploymentScalerRoles holds the bounds of the roles a scaler plans for.
func validateModelDeploymentScalerRoles(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	path := field.NewPath("spec", "roles")
	seen := map[string]int{}
	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		rolePath := path.Index(i)

		if first, taken := seen[bounds.Name]; taken {
			errs = append(errs, field.Duplicate(rolePath.Child("name"), bounds.Name))
			errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
				fmt.Sprintf("role %q is already declared at index %d", bounds.Name, first)))
			continue
		}
		seen[bounds.Name] = i

		if bounds.MinReplicas < 1 {
			errs = append(errs, field.Invalid(rolePath.Child("minReplicas"), bounds.MinReplicas,
				"a role runs at least one replica"))
		}
		if bounds.MaxReplicas < 1 {
			errs = append(errs, field.Invalid(rolePath.Child("maxReplicas"), bounds.MaxReplicas,
				"a role runs at least one replica"))
		}
		if bounds.MaxReplicas > modelDeploymentScalerMaxReplicas {
			errs = append(errs, field.Invalid(rolePath.Child("maxReplicas"), bounds.MaxReplicas,
				fmt.Sprintf("a role runs at most %d replicas", modelDeploymentScalerMaxReplicas)))
		}
		if bounds.MinReplicas > bounds.MaxReplicas {
			errs = append(errs, field.Invalid(rolePath.Child("minReplicas"), bounds.MinReplicas,
				fmt.Sprintf("the floor of role %q is above its ceiling of %d",
					bounds.Name, bounds.MaxReplicas)))
		}
	}

	return errs
}

// validateModelDeploymentScalerRatios holds the relationships between roles.
//
// A RATIO NAMES ROLES THE SCALER PLANS FOR, because a ratio between a role that is planned for
// and one that is not cannot be kept by any vector.
func validateModelDeploymentScalerRatios(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	known := map[string]bool{}
	for i := range s.Spec.Roles {
		known[s.Spec.Roles[i].Name] = true
	}

	path := field.NewPath("spec", "ratios")
	seen := map[string]int{}
	for i := range s.Spec.Ratios {
		ratio := &s.Spec.Ratios[i]
		ratioPath := path.Index(i)

		if ratio.NumeratorRole == ratio.DenominatorRole {
			errs = append(errs, field.Invalid(ratioPath.Child("numeratorRole"), ratio.NumeratorRole,
				"a ratio compares two roles, and this one compares a role with itself"))
			continue
		}
		if !known[ratio.NumeratorRole] {
			errs = append(errs, field.Invalid(ratioPath.Child("numeratorRole"), ratio.NumeratorRole,
				"this role is not one of the roles this scaler plans for"))
		}
		if !known[ratio.DenominatorRole] {
			errs = append(errs, field.Invalid(ratioPath.Child("denominatorRole"), ratio.DenominatorRole,
				"this role is not one of the roles this scaler plans for"))
		}

		key := ratio.NumeratorRole + "/" + ratio.DenominatorRole
		if first, taken := seen[key]; taken {
			errs = append(errs, field.Duplicate(ratioPath, key))
			errs = append(errs, field.Invalid(ratioPath, key,
				fmt.Sprintf("this ratio is already declared at index %d", first)))
			continue
		}
		seen[key] = i

		errs = append(errs, validateModelDeploymentScalerRatioQuantities(ratio, ratioPath)...)
	}

	return errs
}

// validateModelDeploymentScalerRatioQuantities holds the numbers of one ratio.
//
// THE TARGET IS INSIDE THE BAND, because a target the band excludes is a target no vector can
// reach, and a band that excludes it is a band nothing satisfies.
func validateModelDeploymentScalerRatioQuantities(
	ratio *workercore.ModelDeploymentScalerRoleRatio, ratioPath *field.Path,
) field.ErrorList {
	errs := make(field.ErrorList, 0, 5)

	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		ratioPath.Child("target"), ratio.Target, "the target of a ratio is how many replicas the "+
			"numerator role holds per replica of the denominator role",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		ratioPath.Child("min"), ratio.Min, "the lower bound of a ratio is how few replicas the "+
			"numerator role may hold per replica of the denominator role",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		ratioPath.Child("max"), ratio.Max, "the upper bound of a ratio is how many replicas the "+
			"numerator role may hold per replica of the denominator role",
	)...)

	minimum, minimumSet := modelDeploymentScalerRatioValue(ratio.Min)
	maximum, maximumSet := modelDeploymentScalerRatioValue(ratio.Max)
	if minimumSet && maximumSet && minimum.Cmp(maximum) > 0 {
		errs = append(errs, field.Invalid(ratioPath.Child("max"), ratio.Max.String(),
			"the upper bound of a ratio is below its lower bound, so no ratio satisfies it"))
	}

	target, targetSet := modelDeploymentScalerRatioValue(ratio.Target)
	if targetSet && minimumSet && target.Cmp(minimum) < 0 {
		errs = append(errs, field.Invalid(ratioPath.Child("target"), ratio.Target.String(),
			"the target of a ratio is below its lower bound, so no vector reaches it"))
	}
	if targetSet && maximumSet && target.Cmp(maximum) > 0 {
		errs = append(errs, field.Invalid(ratioPath.Child("target"), ratio.Target.String(),
			"the target of a ratio is above its upper bound, so no vector reaches it"))
	}

	return errs
}

// validateModelDeploymentScalerPositiveQuantity refuses a quantity that is not a positive number.
func validateModelDeploymentScalerPositiveQuantity(
	path *field.Path, quantity *resource.Quantity, what string,
) field.ErrorList {
	var errs field.ErrorList

	if quantity == nil {
		return errs
	}
	if quantity.Sign() <= 0 {
		errs = append(errs, field.Invalid(path, quantity.String(),
			what+", so it is a positive number"))
	}

	return errs
}

// modelDeploymentScalerRatioValue reads a quantity as an exact rational, and says whether it was
// set at all.
//
// THE RATIONAL IS EXACT, because a ratio bound is compared against a quotient of two integers and
// a rounded one would decide a boundary wrongly.
func modelDeploymentScalerRatioValue(quantity *resource.Quantity) (*big.Rat, bool) {
	if quantity == nil {
		return nil, false
	}

	dec := quantity.AsDec()
	num := new(big.Int).Set(dec.UnscaledBig())
	den := big.NewInt(1)
	scale := int64(dec.Scale())
	if scale >= 0 {
		den.Exp(big.NewInt(10), big.NewInt(scale), nil)
	} else {
		num.Mul(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(-scale), nil))
	}

	return new(big.Rat).SetFrac(num, den), true
}

// validateModelDeploymentScalerBehavior holds the pacing rules of a scaler.
func validateModelDeploymentScalerBehavior(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	path := field.NewPath("spec", "behavior", "scaleUp")
	if s.Spec.Behavior.ScaleUp.MaxStepReplicas < 0 {
		errs = append(errs, field.Invalid(path.Child("maxStepReplicas"),
			s.Spec.Behavior.ScaleUp.MaxStepReplicas, "a step cap is a count of replicas"))
	}
	if s.Spec.Behavior.ScaleUp.MaxStepPercent < 0 || s.Spec.Behavior.ScaleUp.MaxStepPercent > 1000 {
		errs = append(errs, field.Invalid(path.Child("maxStepPercent"),
			s.Spec.Behavior.ScaleUp.MaxStepPercent, "a step cap in percent is between 0 and 1000"))
	}
	errs = append(errs, validateModelDeploymentScalerStepPolicy(
		path.Child("stepPolicy"), s.Spec.Behavior.ScaleUp.StepPolicy,
	)...)

	downPath := field.NewPath("spec", "behavior", "scaleDown")
	errs = append(errs, validateModelDeploymentScalerStepPolicy(
		downPath.Child("stepPolicy"), s.Spec.Behavior.ScaleDown.StepPolicy,
	)...)

	return errs
}

// validateModelDeploymentScalerStepPolicy holds one step bound.
func validateModelDeploymentScalerStepPolicy(
	path *field.Path, policy *workercore.ModelDeploymentScalerStepPolicy,
) field.ErrorList {
	var errs field.ErrorList

	if policy == nil {
		return errs
	}

	switch policy.Type {
	case workercore.ModelDeploymentScalerStepPolicyTypePods,
		workercore.ModelDeploymentScalerStepPolicyTypePercent:
	case "":
		errs = append(errs, field.Required(path.Child("type"),
			"a step policy says whether its value counts replicas or percent"))
	default:
		errs = append(errs, field.Invalid(path.Child("type"), policy.Type,
			"a step policy counts either replicas or percent"))
	}
	if policy.Value <= 0 {
		errs = append(errs, field.Invalid(path.Child("value"), policy.Value,
			"a step cap is a positive count"))
	}

	return errs
}

// validateModelDeploymentScalerRebalance holds the rebalance order of a scaler.
func validateModelDeploymentScalerRebalance(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	switch s.Spec.Rebalance.Policy {
	case workercore.ModelDeploymentScalerRebalancePolicyOpposing,
		workercore.ModelDeploymentScalerRebalancePolicyPriority,
		workercore.ModelDeploymentScalerRebalancePolicyDisabled:
	case "":
		errs = append(errs, field.Required(field.NewPath("spec", "rebalance", "policy"),
			"a rebalance policy says whether another role may give resources back"))
	default:
		errs = append(errs, field.Invalid(field.NewPath("spec", "rebalance", "policy"),
			s.Spec.Rebalance.Policy, "a rebalance policy is Opposing, Priority or Disabled"))
	}

	known := map[string]bool{}
	for i := range s.Spec.Roles {
		known[s.Spec.Roles[i].Name] = true
	}

	path := field.NewPath("spec", "rebalance", "priority")
	seen := map[string]int{}
	for i, role := range s.Spec.Rebalance.Priority {
		if !known[role] {
			errs = append(errs, field.Invalid(path.Index(i), role,
				"this role is not one of the roles this scaler plans for"))
			continue
		}
		if first, taken := seen[role]; taken {
			errs = append(errs, field.Duplicate(path.Index(i), role))
			errs = append(errs, field.Invalid(path.Index(i), role,
				fmt.Sprintf("role %q is already listed at index %d", role, first)))
			continue
		}
		seen[role] = i
	}

	return errs
}

// validateModelDeploymentScalerAlgorithm holds the algorithm a scaler runs and its parameters.
func validateModelDeploymentScalerAlgorithm(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	path := field.NewPath("spec", "algorithm")
	switch s.Spec.Algorithm.Type {
	case workercore.ModelDeploymentScalerAlgorithmTypeProportional:
		if s.Spec.Algorithm.TokenThroughput != nil {
			errs = append(errs, field.Invalid(path.Child("tokenThroughput"), s.Spec.Algorithm.TokenThroughput,
				"this block is for the TokenThroughput algorithm, and this scaler runs Proportional"))
		}
		if s.Spec.Algorithm.Predictive != nil {
			errs = append(errs, field.Invalid(path.Child("predictive"), s.Spec.Algorithm.Predictive,
				"this block is for the Predictive algorithm, and this scaler runs Proportional"))
		}
		if s.Spec.Algorithm.CapacityPrior != nil {
			errs = append(errs, field.Invalid(path.Child("capacityPrior"), s.Spec.Algorithm.CapacityPrior,
				"this block is for the TokenThroughput and Predictive algorithms, and this scaler runs Proportional"))
		}
	case workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput:
		if s.Spec.Algorithm.Proportional != nil {
			errs = append(errs, field.Invalid(path.Child("proportional"), s.Spec.Algorithm.Proportional,
				"this block is for the Proportional algorithm, and this scaler runs TokenThroughput"))
		}
		if s.Spec.Algorithm.Predictive != nil {
			errs = append(errs, field.Invalid(path.Child("predictive"), s.Spec.Algorithm.Predictive,
				"this block is for the Predictive algorithm, and this scaler runs TokenThroughput"))
		}
	case workercore.ModelDeploymentScalerAlgorithmTypePredictive:
		if s.Spec.Algorithm.Proportional != nil {
			errs = append(errs, field.Invalid(path.Child("proportional"), s.Spec.Algorithm.Proportional,
				"this block is for the Proportional algorithm, and this scaler runs Predictive"))
		}
		if s.Spec.Algorithm.TokenThroughput != nil {
			errs = append(errs, field.Invalid(path.Child("tokenThroughput"), s.Spec.Algorithm.TokenThroughput,
				"this block is for the TokenThroughput algorithm, and this scaler runs Predictive"))
		}
	case "":
		errs = append(errs, field.Required(path.Child("type"),
			"this scaler names no algorithm"))
	default:
		errs = append(errs, field.Invalid(path.Child("type"), s.Spec.Algorithm.Type,
			"an algorithm is Proportional, TokenThroughput or Predictive"))
	}

	errs = append(errs, validateModelDeploymentScalerProportional(s)...)
	errs = append(errs, validateModelDeploymentScalerTokenThroughput(s)...)
	errs = append(errs, validateModelDeploymentScalerPredictive(s)...)
	errs = append(errs, validateModelDeploymentScalerCapacityPrior(s)...)

	return errs
}

// validateModelDeploymentScalerCapacityPrior validates the structural fields of the capacity prior block.
func validateModelDeploymentScalerCapacityPrior(s *workercore.ModelDeploymentScaler) field.ErrorList {
	cp := s.Spec.Algorithm.CapacityPrior
	if cp == nil {
		return nil
	}

	var errs field.ErrorList
	path := field.NewPath("spec", "algorithm", "capacityPrior")

	if cp.CatalogRef != nil {
		refPath := path.Child("catalogRef")
		if strings.TrimSpace(cp.CatalogRef.Name) == "" {
			errs = append(errs, field.Required(refPath.Child("name"),
				"the catalog reference must name a ConfigMap"))
		} else if why := validation.IsDNS1123Subdomain(cp.CatalogRef.Name); len(why) > 0 {
			errs = append(errs, field.Invalid(refPath.Child("name"), cp.CatalogRef.Name,
				fmt.Sprintf("a ConfigMap name must be a valid DNS subdomain: %s", strings.Join(why, "; "))))
		}

		if strings.TrimSpace(cp.CatalogRef.Key) == "" {
			errs = append(errs, field.Required(refPath.Child("key"),
				"the catalog reference must name a ConfigMap data key"))
		} else if why := validation.IsConfigMapKey(cp.CatalogRef.Key); len(why) > 0 {
			errs = append(errs, field.Invalid(refPath.Child("key"), cp.CatalogRef.Key,
				fmt.Sprintf("a ConfigMap key must be a valid data key: %s", strings.Join(why, "; "))))
		}
	}

	if cp.Headroom != nil {
		headroom, ok := modelDeploymentScalerRatioValue(cp.Headroom)
		if !ok || headroom.Sign() <= 0 {
			errs = append(errs, field.Invalid(path.Child("headroom"), cp.Headroom.String(),
				"headroom is a positive fraction of the seed capacity"))
		} else if headroom.Cmp(big.NewRat(1, 1)) > 0 {
			errs = append(errs, field.Invalid(path.Child("headroom"), cp.Headroom.String(),
				"headroom is a fraction of the seed capacity, so it is at most one"))
		}
	}

	return errs
}

// validateModelDeploymentScalerProportional holds the parameters of the proportional algorithm.
func validateModelDeploymentScalerProportional(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	proportional := s.Spec.Algorithm.Proportional
	if proportional == nil {
		return errs
	}

	path := field.NewPath("spec", "algorithm", "proportional")
	switch proportional.Metric {
	case workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
		workercore.ModelDeploymentScalerProportionalMetricRunningRequests,
		workercore.ModelDeploymentScalerProportionalMetricWaitingRequests,
		workercore.ModelDeploymentScalerProportionalMetricPromptTokensPerSecond,
		workercore.ModelDeploymentScalerProportionalMetricGenerationTokensPerSecond,
		workercore.ModelDeploymentScalerProportionalMetricTotalTokensPerSecond:
	case "":
		errs = append(errs, field.Required(path.Child("metric"),
			"the proportional algorithm scales on a signal, and this one names none"))
	default:
		errs = append(errs, field.Invalid(path.Child("metric"), proportional.Metric,
			"the proportional algorithm scales on a signal this operator does not publish"))
	}

	if proportional.Tolerance != nil {
		tolerance, _ := modelDeploymentScalerRatioValue(proportional.Tolerance)
		if tolerance.Cmp(big.NewRat(1, 1)) > 0 {
			errs = append(errs, field.Invalid(path.Child("tolerance"), proportional.Tolerance.String(),
				"the tolerance is a fraction of the target, so it is at most one"))
		} else if tolerance.Sign() <= 0 {
			errs = append(errs, field.Invalid(path.Child("tolerance"), proportional.Tolerance.String(),
				"the tolerance is a positive fraction of the target"))
		}
	}

	known := map[string]bool{}
	for i := range s.Spec.Roles {
		known[s.Spec.Roles[i].Name] = true
	}

	targetsPath := path.Child("roleTargets")
	seen := map[string]int{}
	for i := range proportional.RoleTargets {
		target := &proportional.RoleTargets[i]
		targetPath := targetsPath.Index(i)

		if !known[target.Name] {
			errs = append(errs, field.Invalid(targetPath.Child("name"), target.Name,
				"this role is not one of the roles this scaler plans for"))
		}
		if first, taken := seen[target.Name]; taken {
			errs = append(errs, field.Duplicate(targetPath.Child("name"), target.Name))
			errs = append(errs, field.Invalid(targetPath.Child("name"), target.Name,
				fmt.Sprintf("role %q already has a target at index %d", target.Name, first)))
			continue
		}
		seen[target.Name] = i

		errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
			targetPath.Child("target"), &target.Target,
			"the target of a role is the signal one replica of it is expected to carry",
		)...)
	}

	return errs
}

// validateModelDeploymentScalerTokenThroughput holds the parameters of the token throughput
// algorithm.
func validateModelDeploymentScalerTokenThroughput(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	path := field.NewPath("spec", "algorithm", "tokenThroughput")
	if tt := s.Spec.Algorithm.TokenThroughput; tt != nil {
		errs = append(errs, validateModelDeploymentScalerTokenThroughputSpec(path, tt)...)
	}
	if predictive := s.Spec.Algorithm.Predictive; predictive != nil {
		errs = append(errs, validateModelDeploymentScalerTokenThroughputSpec(
			field.NewPath("spec", "algorithm", "predictive", "tokenThroughput"),
			&predictive.TokenThroughput,
		)...)
	}

	return errs
}

// validateModelDeploymentScalerTokenThroughputSpec holds the parameters one token throughput
// block carries.
func validateModelDeploymentScalerTokenThroughputSpec(
	path *field.Path, tt *workercore.ModelDeploymentScalerTokenThroughputSpec,
) field.ErrorList {
	errs := make(field.ErrorList, 0, 6)

	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("prefillTargetTokensPerSecond"), tt.PrefillTargetTokensPerSecond,
		"the prefill target is the prompt-token rate one prefill replica sustains",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("decodeTargetTokensPerSecond"), tt.DecodeTargetTokensPerSecond,
		"the decode target is the generation-token rate one decode replica sustains",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("serverTargetTokensPerSecond"), tt.ServerTargetTokensPerSecond,
		"the server target is the token rate one server replica sustains",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("panicThreshold"), tt.PanicThreshold,
		"the panic threshold is the growth rate over the ready replicas that starts a panic",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("ttftTargetMilliseconds"), tt.TTFTTargetMilliseconds,
		"the time to first token target is the latency a role is expected to keep under",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("itlTargetMilliseconds"), tt.ITLTargetMilliseconds,
		"the inter-token latency target is the latency a role is expected to keep under",
	)...)
	// THE QUANTILE IS READ AS THE EXACT VALUE THE QUANTITY HOLDS, because the
	// value the quantity resolved is the one every comparison uses.
	if tt.LatencyQuantile != nil {
		quantilePath := path.Child("latencyQuantile")
		quantile, ok := modelDeploymentScalerRatioValue(tt.LatencyQuantile)
		switch {
		case !ok:
			errs = append(errs, field.Invalid(quantilePath, tt.LatencyQuantile.String(),
				"the latency quantile must be a number"))
		case quantile.Sign() <= 0:
			errs = append(errs, field.Invalid(quantilePath, tt.LatencyQuantile.String(),
				"the latency quantile is a quantile, so it is greater than zero"))
		case quantile.Cmp(big.NewRat(1, 1)) > 0:
			errs = append(errs, field.Invalid(quantilePath, tt.LatencyQuantile.String(),
				"the latency quantile is a fraction of the distribution, so it is at most one"))
		}
	}
	if tt.StableWindowSeconds != 0 && (tt.StableWindowSeconds < 30 || tt.StableWindowSeconds > 600) {
		errs = append(errs, field.Invalid(
			path.Child("stableWindowSeconds"), tt.StableWindowSeconds,
			"the stable window is between 30 and 600 seconds",
		))
	}
	if tt.PanicWindowSeconds != 0 && (tt.PanicWindowSeconds < 10 || tt.PanicWindowSeconds > 300) {
		errs = append(errs, field.Invalid(
			path.Child("panicWindowSeconds"), tt.PanicWindowSeconds,
			"the panic window is between 10 and 300 seconds",
		))
	}

	return errs
}

// validateModelDeploymentScalerPredictive holds the parameters of the predictive algorithm.
func validateModelDeploymentScalerPredictive(s *workercore.ModelDeploymentScaler) field.ErrorList {
	predictive := s.Spec.Algorithm.Predictive
	if predictive == nil {
		return nil
	}

	errs := make(field.ErrorList, 0, 6)

	path := field.NewPath("spec", "algorithm", "predictive")
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("prefillTokenCapacityPerReplica"), predictive.PrefillTokenCapacityPerReplica,
		"the prefill capacity is the prompt tokens one Prefill replica holds",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("decodeTokenCapacityPerReplica"), predictive.DecodeTokenCapacityPerReplica,
		"the decode capacity is the generation tokens one Decode replica holds",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("serverTokenCapacityPerReplica"), predictive.ServerTokenCapacityPerReplica,
		"the server capacity is the tokens one Server replica holds",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("qLevel"), predictive.QLevel,
		"qLevel is the level process noise",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("qTrend"), predictive.QTrend,
		"qTrend is the trend process noise",
	)...)
	errs = append(errs, validateModelDeploymentScalerPositiveQuantity(
		path.Child("measurementNoise"), predictive.MeasurementNoise,
		"measurementNoise is the observation noise",
	)...)

	return errs
}

// validateModelDeploymentScalerRatioFeasibility refuses a scaler whose bounds and ratios admit
// no replica vector at all.
//
// A SET OF BOUNDS AND A SET OF RATIOS THAT CANNOT HOLD AT ONCE is a scaler that can never
// commit anything: every vector it might pick breaks one of the rules it was given, so it
// would either hold the target forever or break a rule to move it.
//
// THE SEARCH IS OVER INTEGERS, because a replica count is an integer and a vector that only
// satisfies the ratios over the reals is not one the operator can commit. The search is exact:
// it enumerates the vectors of the roles that a ratio couples, and it tightens the remaining
// roles from every ratio as it goes.
func validateModelDeploymentScalerRatioFeasibility(s *workercore.ModelDeploymentScaler) field.ErrorList {
	var errs field.ErrorList

	components := modelDeploymentScalerComponents(s)
	for _, component := range components {
		budget := modelDeploymentScalerFeasibilityBudget
		if component.feasible(&budget) {
			continue
		}
		if budget <= 0 {
			// The search could not decide. Refusing here would refuse a configuration that may
			// well be feasible, and an owner cannot repair that by editing the object.
			continue
		}
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "roles"), component.roleNames(),
			"no replica vector inside these bounds satisfies every ratio at the same time, so "+
				"this scaler could never commit anything",
		))
	}

	return errs
}

// modelDeploymentScalerRatioBand is one declared ratio as a band of exact rationals.
type modelDeploymentScalerRatioBand struct {
	numerator   string
	denominator string
	minimum     *big.Rat
	maximum     *big.Rat
}

// modelDeploymentScalerComponent is a set of roles whose replica counts are bound to each other.
//
// The roles of a scaler that no ratio mentions are alone in their component and are always
// feasible, because any count inside their own bounds satisfies every rule.
type modelDeploymentScalerComponent struct {
	roles  []string
	low    []int64
	high   []int64
	ratios []modelDeploymentScalerRatioBand
	index  map[string]int
}

// roleNames names the roles of a component, for the message a refusal carries.
func (c *modelDeploymentScalerComponent) roleNames() string {
	return strings.Join(c.roles, ", ")
}

// modelDeploymentScalerComponents groups the roles of a scaler into the sets their ratios
// couple.
func modelDeploymentScalerComponents(s *workercore.ModelDeploymentScaler) []*modelDeploymentScalerComponent {
	parent := make([]int, len(s.Spec.Roles))
	for i := range parent {
		parent[i] = i
	}

	position := map[string]int{}
	for i := range s.Spec.Roles {
		position[s.Spec.Roles[i].Name] = i
	}

	var bands []modelDeploymentScalerRatioBand
	for i := range s.Spec.Ratios {
		ratio := &s.Spec.Ratios[i]
		numerator, ok := position[ratio.NumeratorRole]
		if !ok {
			continue
		}
		denominator, ok := position[ratio.DenominatorRole]
		if !ok {
			continue
		}
		modelDeploymentScalerUnion(parent, numerator, denominator)

		band := modelDeploymentScalerRatioBand{
			numerator:   ratio.NumeratorRole,
			denominator: ratio.DenominatorRole,
		}
		if minimum, set := modelDeploymentScalerRatioValue(ratio.Min); set {
			band.minimum = minimum
		}
		if maximum, set := modelDeploymentScalerRatioValue(ratio.Max); set {
			band.maximum = maximum
		}
		bands = append(bands, band)
	}

	groups := map[int]*modelDeploymentScalerComponent{}
	for i := range s.Spec.Roles {
		root := modelDeploymentScalerFind(parent, i)
		component, ok := groups[root]
		if !ok {
			component = &modelDeploymentScalerComponent{index: map[string]int{}}
			groups[root] = component
		}
		component.index[s.Spec.Roles[i].Name] = len(component.roles)
		component.roles = append(component.roles, s.Spec.Roles[i].Name)
		component.low = append(component.low, int64(s.Spec.Roles[i].MinReplicas))
		component.high = append(component.high, int64(s.Spec.Roles[i].MaxReplicas))
	}

	for _, band := range bands {
		numerator := groups[modelDeploymentScalerFind(parent, position[band.numerator])]
		denominator := groups[modelDeploymentScalerFind(parent, position[band.denominator])]
		if numerator != denominator {
			continue
		}
		numerator.ratios = append(numerator.ratios, band)
	}

	components := make([]*modelDeploymentScalerComponent, 0, len(groups))
	for _, component := range groups {
		components = append(components, component)
	}

	return components
}

// modelDeploymentScalerFind returns the root of a role's set.
func modelDeploymentScalerFind(parent []int, i int) int {
	for parent[i] != i {
		parent[i] = parent[parent[i]]
		i = parent[i]
	}
	return i
}

// modelDeploymentScalerUnion joins two sets of roles.
func modelDeploymentScalerUnion(parent []int, a, b int) {
	rootA, rootB := modelDeploymentScalerFind(parent, a), modelDeploymentScalerFind(parent, b)
	if rootA == rootB {
		return
	}
	parent[rootB] = rootA
}

// feasible reports whether some replica vector inside the bounds satisfies every ratio.
func (c *modelDeploymentScalerComponent) feasible(budget *int) bool {
	if len(c.ratios) == 0 {
		return true
	}

	// THE TWO OBVIOUS VECTORS ARE TRIED FIRST, because a set of roles that is already
	// consistent settles on one of them and the search never runs.
	for _, candidate := range [][]int64{c.uniform(c.low), c.uniform(c.high)} {
		if c.holds(candidate) {
			return true
		}
	}

	return c.search(0, c.low, c.high, budget)
}

// uniform copies a bound vector so the search can test it without mutating the component's bounds.
func (c *modelDeploymentScalerComponent) uniform(values []int64) []int64 {
	vector := make([]int64, len(c.roles))
	copy(vector, values)
	return vector
}

// holds reports whether a vector satisfies every ratio of the component.
func (c *modelDeploymentScalerComponent) holds(vector []int64) bool {
	for _, band := range c.ratios {
		numerator := vector[c.index[band.numerator]]
		denominator := vector[c.index[band.denominator]]
		if denominator == 0 {
			return false
		}
		quotient := new(big.Rat).SetFrac(big.NewInt(numerator), big.NewInt(denominator))
		if band.minimum != nil && quotient.Cmp(band.minimum) < 0 {
			return false
		}
		if band.maximum != nil && quotient.Cmp(band.maximum) > 0 {
			return false
		}
	}
	return true
}

// search looks for a vector, assigning one role at a time and tightening the rest from the
// ratios as it goes.
func (c *modelDeploymentScalerComponent) search(pos int, low, high []int64, budget *int) bool {
	if pos >= len(c.roles) {
		return c.holds(low)
	}
	if *budget <= 0 {
		return false
	}

	index := c.index[c.roles[pos]]
	for value := low[index]; value <= high[index]; value++ {
		*budget--
		if *budget <= 0 {
			return false
		}

		nextLow := append([]int64(nil), low...)
		nextHigh := append([]int64(nil), high...)
		nextLow[index], nextHigh[index] = value, value
		if !c.propagate(index, nextLow, nextHigh) {
			continue
		}
		if c.search(pos+1, nextLow, nextHigh, budget) {
			return true
		}
	}

	return false
}

// propagate tightens the bounds of the roles a known count constrains.
//
// IT NEVER CHANGES A COUNT THAT WAS ALREADY CHOSEN. Tightening a chosen role can only make its
// interval empty, and an empty interval is a branch that is pruned rather than a count that is
// rewritten.
func (c *modelDeploymentScalerComponent) propagate(index int, low, high []int64) bool {
	for _, band := range c.ratios {
		numerator := c.index[band.numerator]
		denominator := c.index[band.denominator]
		switch {
		case numerator == index:
			if !c.tightenDenominator(low, high, denominator, numerator, band) {
				return false
			}
		case denominator == index:
			if !c.tightenNumerator(low, high, numerator, denominator, band) {
				return false
			}
		}
	}
	return true
}

// tightenNumerator bounds the numerator of a ratio whose denominator is known.
//
// The band says the quotient lies between the two bounds, so the numerator lies between the
// bounds multiplied by the denominator.
func (c *modelDeploymentScalerComponent) tightenNumerator(
	low, high []int64, numerator, denominator int, band modelDeploymentScalerRatioBand,
) bool {
	value := big.NewRat(low[denominator], 1)
	if band.minimum != nil {
		bound := modelDeploymentScalerCeil(new(big.Rat).Mul(band.minimum, value))
		if bound > low[numerator] {
			low[numerator] = bound
		}
	}
	if band.maximum != nil {
		bound := modelDeploymentScalerFloor(new(big.Rat).Mul(band.maximum, value))
		if bound < high[numerator] {
			high[numerator] = bound
		}
	}
	return low[numerator] <= high[numerator]
}

// tightenDenominator bounds the denominator of a ratio whose numerator is known.
//
// The band says the quotient lies between the two bounds, so the denominator lies between the
// numerator divided by the upper bound and the numerator divided by the lower one.
func (c *modelDeploymentScalerComponent) tightenDenominator(
	low, high []int64, denominator, numerator int, band modelDeploymentScalerRatioBand,
) bool {
	value := big.NewRat(low[numerator], 1)
	if band.maximum != nil {
		bound := modelDeploymentScalerCeil(new(big.Rat).Quo(value, band.maximum))
		if bound > low[denominator] {
			low[denominator] = bound
		}
	}
	if band.minimum != nil {
		bound := modelDeploymentScalerFloor(new(big.Rat).Quo(value, band.minimum))
		if bound < high[denominator] {
			high[denominator] = bound
		}
	}
	return low[denominator] <= high[denominator]
}

// modelDeploymentScalerCeil rounds a rational up.
func modelDeploymentScalerCeil(ratio *big.Rat) int64 {
	quotient, remainder := new(big.Int).QuoRem(ratio.Num(), ratio.Denom(), new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient.Int64()
}

// modelDeploymentScalerFloor rounds a rational down.
func modelDeploymentScalerFloor(ratio *big.Rat) int64 {
	quotient, _ := new(big.Int).QuoRem(ratio.Num(), ratio.Denom(), new(big.Int))
	return quotient.Int64()
}

// validateModelDeploymentScalerAgainstTarget holds the rules that need the target to answer.
//
// THE ROLES A SCALER PLANS FOR ARE THE ROLES OF THE TARGET, so a scaler naming a role the
// target does not have is a scaler that plans for nothing, and a scaler naming a role the
// target cannot scale is a scaler whose recommendation is meaningless.
func (r *ModelDeploymentScalerWebhook) validateModelDeploymentScalerAgainstTarget(
	ctx context.Context, s *workercore.ModelDeploymentScaler, md *workercore.ModelDeployment, isUpdate bool,
) (ctrladmission.Warnings, field.ErrorList) {
	var (
		warnings ctrladmission.Warnings
		errs     field.ErrorList
	)

	declared := map[string]*workercore.ModelDeploymentRole{}
	for i := range md.Spec.Roles {
		declared[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	rolesPath := field.NewPath("spec", "roles")
	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		rolePath := rolesPath.Index(i)

		role, ok := declared[bounds.Name]
		if !ok {
			errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
				"the target does not have a role with this name, so there is nothing to scale"))
			continue
		}
		if len(role.Command) > 0 {
			errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
				"this role runs a command of its own, and a scaler cannot tell what that command "+
					"serves, so it cannot tell when the role is busy"))
		}
		if role.ElasticEP != nil {
			errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
				"this role runs the elastic-EP profile, which spreads its width across the "+
					"members of one replica, so another replica would add no capacity"))
		}
		errs = append(errs, validateModelDeploymentScalerRoleNames(md, role, bounds, rolePath)...)
	}

	if s.Spec.Algorithm.CapacityPrior != nil {
		catalogWarnings, catalogErrs := r.validateModelDeploymentScalerCatalogRoles(ctx, s, md, declared, isUpdate)
		warnings = append(warnings, catalogWarnings...)
		errs = append(errs, catalogErrs...)
		return warnings, errs
	}

	errs = append(errs, validateModelDeploymentScalerAlgorithmRoles(s, declared)...)

	return warnings, errs
}

// validateModelDeploymentScalerCatalogRoles validates role coupling, explicit latency targets,
// and catalog prior resolution for a scaler that asked for a capacity prior.
func (r *ModelDeploymentScalerWebhook) validateModelDeploymentScalerCatalogRoles(
	ctx context.Context, s *workercore.ModelDeploymentScaler, md *workercore.ModelDeployment,
	declared map[string]*workercore.ModelDeploymentRole, isUpdate bool,
) (ctrladmission.Warnings, field.ErrorList) {
	var (
		warnings ctrladmission.Warnings
		errs     field.ErrorList
	)

	algorithmName := "the token throughput algorithm"
	tt := s.Spec.Algorithm.TokenThroughput
	ttPath := field.NewPath("spec", "algorithm", "tokenThroughput")
	if s.Spec.Algorithm.Type == workercore.ModelDeploymentScalerAlgorithmTypePredictive {
		algorithmName = "the predictive algorithm"
		if s.Spec.Algorithm.Predictive != nil {
			tt = &s.Spec.Algorithm.Predictive.TokenThroughput
		}
		ttPath = field.NewPath("spec", "algorithm", "predictive", "tokenThroughput")
	}

	coupled := modelDeploymentScalerCoupledToKinds(s, declared,
		workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode)

	rolesPath := field.NewPath("spec", "roles")
	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		role, ok := declared[bounds.Name]
		if !ok {
			continue
		}
		if workerctrl.ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindEncode && !coupled[bounds.Name] {
			errs = append(errs, field.Invalid(rolesPath.Index(i).Child("name"), bounds.Name,
				"an encode role is scaled by the role it is coupled to, and this one is "+
					"coupled to no prefill or decode role, so "+algorithmName+" has nothing to "+
					"scale it from"))
		}
	}

	if !workerctrl.ModelDeploymentScalerNeedsCapacityPrior(s, md) {
		return warnings, errs
	}

	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		role, ok := declared[bounds.Name]
		if !ok {
			continue
		}
		kind := workerctrl.ModelDeploymentEffectiveRoleKind(role)
		if kind == workercore.ModelDeploymentRoleKindEncode || !modelDeploymentScalerRoleNeedsCapacityPrior(s, kind) {
			continue
		}
		if (kind == workercore.ModelDeploymentRoleKindPrefill || kind == workercore.ModelDeploymentRoleKindServer) &&
			(tt == nil || tt.TTFTTargetMilliseconds == nil) {
			errs = append(errs, field.Required(
				ttPath.Child("ttftTargetMilliseconds"),
				fmt.Sprintf("role %q (%s) resolves capacity from a catalog prior and requires an explicit ttftTargetMilliseconds", bounds.Name, kind),
			))
		}
		if (kind == workercore.ModelDeploymentRoleKindDecode || kind == workercore.ModelDeploymentRoleKindServer) &&
			(tt == nil || tt.ITLTargetMilliseconds == nil) {
			errs = append(errs, field.Required(
				ttPath.Child("itlTargetMilliseconds"),
				fmt.Sprintf("role %q (%s) resolves capacity from a catalog prior and requires an explicit itlTargetMilliseconds", bounds.Name, kind),
			))
		}
	}

	if len(errs) > 0 {
		return warnings, errs
	}

	if _, err := workerctrl.ResolveModelDeploymentScalerCapacityPriors(ctx, r.APIReader, s, md); err != nil {
		var refusal *priors.RefusalError
		if errors.As(err, &refusal) {
			if isUpdate {
				warnings = append(warnings, fmt.Sprintf(
					"capacity prior cannot be resolved (%s); admitting update so the scaler can be repaired or placed in Shadow mode",
					refusal.Error(),
				))
				return warnings, errs
			}
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "algorithm", "capacityPrior"),
				s.Spec.Algorithm.CapacityPrior,
				refusal.Error(),
			))
			return warnings, errs
		}
		errs = append(errs, field.InternalError(field.NewPath("spec", "algorithm", "capacityPrior"), err))
	}

	return warnings, errs
}

// modelDeploymentScalerRoleNeedsCapacityPrior reports whether a role kind is missing any required
// capacity field on the scaler algorithm.
func modelDeploymentScalerRoleNeedsCapacityPrior(
	s *workercore.ModelDeploymentScaler, kind workercore.ModelDeploymentRoleKind,
) bool {
	probe := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{{Kind: kind}},
		},
	}
	return workerctrl.ModelDeploymentScalerNeedsCapacityPrior(s, probe)
}

// validateModelDeploymentScalerUnresolvedTargetSLOs refuses a scaler created before its target
// exists when capacity-prior resolution suppressed the latency defaults and the scaler omits the
// latency targets that any unstated capacity kind requires.
func validateModelDeploymentScalerUnresolvedTargetSLOs(
	s *workercore.ModelDeploymentScaler,
) field.ErrorList {
	var (
		errs   field.ErrorList
		tt     *workercore.ModelDeploymentScalerTokenThroughputSpec
		ttPath *field.Path
	)
	switch s.Spec.Algorithm.Type {
	case workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput:
		tt = s.Spec.Algorithm.TokenThroughput
		ttPath = field.NewPath("spec", "algorithm", "tokenThroughput")
	case workercore.ModelDeploymentScalerAlgorithmTypePredictive:
		if s.Spec.Algorithm.Predictive != nil {
			tt = &s.Spec.Algorithm.Predictive.TokenThroughput
		}
		ttPath = field.NewPath("spec", "algorithm", "predictive", "tokenThroughput")
	default:
		return nil
	}

	needsServer := modelDeploymentScalerRoleNeedsCapacityPrior(s, workercore.ModelDeploymentRoleKindServer)
	needsPrefill := modelDeploymentScalerRoleNeedsCapacityPrior(s, workercore.ModelDeploymentRoleKindPrefill)
	needsDecode := modelDeploymentScalerRoleNeedsCapacityPrior(s, workercore.ModelDeploymentRoleKindDecode)

	noSLOs := tt == nil || (tt.TTFTTargetMilliseconds == nil && tt.ITLTargetMilliseconds == nil)
	if (noSLOs && (needsServer || needsPrefill)) || (!needsDecode && (needsServer || needsPrefill) && (tt == nil || tt.TTFTTargetMilliseconds == nil)) {
		errs = append(errs, field.Required(
			ttPath.Child("ttftTargetMilliseconds"),
			"a scaler that resolves capacity from a catalog prior before its target exists requires an explicit ttftTargetMilliseconds",
		))
	}
	if (noSLOs && (needsServer || needsDecode)) || (!needsPrefill && (needsServer || needsDecode) && (tt == nil || tt.ITLTargetMilliseconds == nil)) {
		errs = append(errs, field.Required(
			ttPath.Child("itlTargetMilliseconds"),
			"a scaler that resolves capacity from a catalog prior before its target exists requires an explicit itlTargetMilliseconds",
		))
	}
	return errs
}

// validateModelDeploymentScalerRoleNames refuses a bound that names an object the cluster
// cannot hold.
//
// THE NAMES A REPLICA HAS DEPEND ON ITS ORDINAL, so the bound that allows the most replicas is
// the one that has to fit. The count is measured rather than reserved for a worst case, because
// a worst case would take characters away from every deployment to cover counts nobody runs.
func validateModelDeploymentScalerRoleNames(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	bounds *workercore.ModelDeploymentScalerRoleBounds, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	ordinal := int(bounds.MaxReplicas) - 1
	if ordinal < 0 {
		ordinal = 0
	}

	group := fmt.Sprintf("%s-%s-r%d", md.Name, role.Name, ordinal)
	if why := validation.IsDNS1123Label(group); len(why) > 0 {
		errs = append(errs, field.Invalid(rolePath.Child("maxReplicas"), bounds.MaxReplicas,
			fmt.Sprintf("at %d replicas a replica of this role is named %q (%d characters), which "+
				"cannot be the name of the Pod group or of the Workload that reserves its quota: "+
				"%s. Shorten the role or the deployment, or declare fewer replicas",
				bounds.MaxReplicas, group, len(group), strings.Join(why, "; "))))
	}

	if int(role.ReplicaSize) > 1 {
		member := fmt.Sprintf("%s-%s-r%d-m%d", md.Name, role.Name, ordinal, int(role.ReplicaSize)-1)
		if why := validation.IsDNS1123Label(member); len(why) > 0 {
			errs = append(errs, field.Invalid(rolePath.Child("maxReplicas"), bounds.MaxReplicas,
				fmt.Sprintf("at %d replicas a member of this role is named %q (%d characters), "+
					"which cannot be a Pod name: %s. Shorten the role or the deployment, or "+
					"declare fewer replicas",
					bounds.MaxReplicas, member, len(member), strings.Join(why, "; "))))
		}
	}

	return errs
}

// validateModelDeploymentScalerAlgorithmRoles holds the rules that couple an algorithm to the
// kinds of the roles it plans for.
//
// THE ALGORITHM READS A SIGNAL PER ROLE KIND, so a role of a kind the algorithm has no signal
// for is a role the algorithm cannot plan for. An Encode role is the exception: it is scaled by
// the role it is coupled to rather than by its own signal, so it needs a ratio to a Prefill or
// a Decode role.
func validateModelDeploymentScalerAlgorithmRoles(
	s *workercore.ModelDeploymentScaler, declared map[string]*workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList

	switch s.Spec.Algorithm.Type {
	case workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput:
		tt := s.Spec.Algorithm.TokenThroughput
		if tt == nil {
			return errs
		}
		errs = append(errs, validateModelDeploymentScalerTokenThroughputRoles(s, declared,
			func(kind workercore.ModelDeploymentRoleKind) *resource.Quantity {
				switch kind {
				case workercore.ModelDeploymentRoleKindPrefill:
					return tt.PrefillTargetTokensPerSecond
				case workercore.ModelDeploymentRoleKindDecode:
					return tt.DecodeTargetTokensPerSecond
				default:
					return tt.ServerTargetTokensPerSecond
				}
			}, "the token throughput algorithm")...)
	case workercore.ModelDeploymentScalerAlgorithmTypePredictive:
		predictive := s.Spec.Algorithm.Predictive
		if predictive == nil {
			return errs
		}
		errs = append(errs, validateModelDeploymentScalerTokenThroughputRoles(s, declared,
			func(kind workercore.ModelDeploymentRoleKind) *resource.Quantity {
				switch kind {
				case workercore.ModelDeploymentRoleKindPrefill:
					return predictive.TokenThroughput.PrefillTargetTokensPerSecond
				case workercore.ModelDeploymentRoleKindDecode:
					return predictive.TokenThroughput.DecodeTargetTokensPerSecond
				default:
					return predictive.TokenThroughput.ServerTargetTokensPerSecond
				}
			}, "the predictive algorithm")...)
		errs = append(errs, validateModelDeploymentScalerPredictiveCapacities(s, declared)...)
	case workercore.ModelDeploymentScalerAlgorithmTypeProportional:
		errs = append(errs, validateModelDeploymentScalerProportionalRoles(s, declared)...)
	}

	return errs
}

// validateModelDeploymentScalerTokenThroughputRoles holds the rule that a role of a kind the
// algorithm has no target for is refused.
func validateModelDeploymentScalerTokenThroughputRoles(
	s *workercore.ModelDeploymentScaler, declared map[string]*workercore.ModelDeploymentRole,
	target func(workercore.ModelDeploymentRoleKind) *resource.Quantity, algorithm string,
) field.ErrorList {
	var errs field.ErrorList

	coupled := modelDeploymentScalerCoupledToKinds(s, declared,
		workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode)

	rolesPath := field.NewPath("spec", "roles")
	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		rolePath := rolesPath.Index(i)

		role, ok := declared[bounds.Name]
		if !ok {
			continue
		}
		kind := role.Kind
		if kind == "" {
			kind = workercore.ModelDeploymentRoleKindServer
		}

		if kind == workercore.ModelDeploymentRoleKindEncode {
			if !coupled[bounds.Name] {
				errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
					"an encode role is scaled by the role it is coupled to, and this one is "+
						"coupled to no prefill or decode role, so "+algorithm+" has nothing to "+
						"scale it from"))
			}
			continue
		}

		if target(kind) == nil {
			errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
				fmt.Sprintf("this role is a %s role, and %s needs the target of that kind to "+
					"know how much one replica of it serves", kind, algorithm)))
		}
	}

	return errs
}

// validateModelDeploymentScalerPredictiveCapacities holds the rule that the predictive
// algorithm needs the capacity of every role kind it plans for.
func validateModelDeploymentScalerPredictiveCapacities(
	s *workercore.ModelDeploymentScaler, declared map[string]*workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList

	predictive := s.Spec.Algorithm.Predictive
	if predictive == nil {
		return errs
	}

	rolesPath := field.NewPath("spec", "roles")
	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		rolePath := rolesPath.Index(i)

		role, ok := declared[bounds.Name]
		if !ok {
			continue
		}
		kind := role.Kind
		if kind == "" {
			kind = workercore.ModelDeploymentRoleKindServer
		}

		var capacity *resource.Quantity
		switch kind {
		case workercore.ModelDeploymentRoleKindPrefill:
			capacity = predictive.PrefillTokenCapacityPerReplica
		case workercore.ModelDeploymentRoleKindDecode:
			capacity = predictive.DecodeTokenCapacityPerReplica
		case workercore.ModelDeploymentRoleKindEncode:
			continue
		default:
			capacity = predictive.ServerTokenCapacityPerReplica
		}

		if capacity == nil {
			errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
				fmt.Sprintf("this role is a %s role, and the predictive algorithm needs the "+
					"capacity of that kind to know how many replicas the demand needs", kind)))
		}
	}

	return errs
}

// validateModelDeploymentScalerProportionalRoles holds the rule that every scaled role either
// has a target of its own or is coupled to a role that has one.
func validateModelDeploymentScalerProportionalRoles(
	s *workercore.ModelDeploymentScaler, declared map[string]*workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList

	proportional := s.Spec.Algorithm.Proportional
	if proportional == nil {
		return errs
	}

	targeted := map[string]bool{}
	for i := range proportional.RoleTargets {
		targeted[proportional.RoleTargets[i].Name] = true
	}
	coupled := modelDeploymentScalerCoupledToTargeted(s, targeted)

	rolesPath := field.NewPath("spec", "roles")
	for i := range s.Spec.Roles {
		bounds := &s.Spec.Roles[i]
		rolePath := rolesPath.Index(i)
		if _, ok := declared[bounds.Name]; !ok {
			continue
		}
		if targeted[bounds.Name] || coupled[bounds.Name] {
			continue
		}
		errs = append(errs, field.Invalid(rolePath.Child("name"), bounds.Name,
			"the proportional algorithm divides what it observed by the target of a role, and "+
				"this role has no target of its own and is coupled to no role that has one"))
	}

	return errs
}

// modelDeploymentScalerCoupledToKinds names the roles that a ratio couples to a role of one of
// the given kinds.
//
// A ROLE OF ANOTHER KIND IS WHAT COUNTS, because a role coupled to another role of the same
// kind is not coupled to anything the algorithm could read differently.
func modelDeploymentScalerCoupledToKinds(
	s *workercore.ModelDeploymentScaler, declared map[string]*workercore.ModelDeploymentRole,
	kinds ...workercore.ModelDeploymentRoleKind,
) map[string]bool {
	wanted := map[string]bool{}
	for _, kind := range kinds {
		wanted[string(kind)] = true
	}

	kindOf := func(name string) string {
		role, ok := declared[name]
		if !ok {
			return ""
		}
		if role.Kind == "" {
			return string(workercore.ModelDeploymentRoleKindServer)
		}
		return string(role.Kind)
	}

	coupled := map[string]bool{}
	for i := range s.Spec.Ratios {
		ratio := &s.Spec.Ratios[i]
		if wanted[kindOf(ratio.NumeratorRole)] {
			coupled[ratio.DenominatorRole] = true
		}
		if wanted[kindOf(ratio.DenominatorRole)] {
			coupled[ratio.NumeratorRole] = true
		}
	}

	return coupled
}

// modelDeploymentScalerCoupledToTargeted names the roles that a ratio couples to a role that
// carries a target of its own.
func modelDeploymentScalerCoupledToTargeted(
	s *workercore.ModelDeploymentScaler, targeted map[string]bool,
) map[string]bool {
	coupled := map[string]bool{}
	for i := range s.Spec.Ratios {
		ratio := &s.Spec.Ratios[i]
		if targeted[ratio.NumeratorRole] {
			coupled[ratio.DenominatorRole] = true
		}
		if targeted[ratio.DenominatorRole] {
			coupled[ratio.NumeratorRole] = true
		}
	}

	return coupled
}
