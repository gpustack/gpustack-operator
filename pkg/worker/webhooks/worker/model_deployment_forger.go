package worker

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	modelDeploymentForgerDefaultMaxKernelsPerGeneration = 4
	modelDeploymentForgerMinKernelsPerGeneration        = 1
	modelDeploymentForgerMaxKernelsPerGeneration        = 16

	modelDeploymentForgerDefaultMinSpeedupRatio = "1.05"

	modelDeploymentForgerDefaultProbeSteps      = 4
	modelDeploymentForgerMinProbeSteps          = 3
	modelDeploymentForgerMaxProbeSteps          = 5
	modelDeploymentForgerDefaultCooldownSeconds = 600
	modelDeploymentForgerMinCooldownSeconds     = 60

	modelDeploymentForgerDefaultTimeoutSeconds  = 1800
	modelDeploymentForgerMinTimeoutSeconds      = 60
	modelDeploymentForgerMaxTimeoutSeconds      = 14400
	modelDeploymentForgerMaxRunnerImageLen      = 512
	modelDeploymentForgerMaxNameLen             = 253
	modelDeploymentForgerMinOutputSizeLimitByte = 64 << 20
	modelDeploymentForgerMaxOutputSizeLimitByte = 16 << 30

	modelDeploymentForgerDefaultMaxRetainedGenerations = 3
	modelDeploymentForgerMinRetainedGenerations        = 1
	modelDeploymentForgerMaxRetainedGenerations        = 16
	modelDeploymentForgerDefaultFailureTimeoutSeconds  = 600
	modelDeploymentForgerMinFailureTimeoutSeconds      = 1

	forgerAllowedRunnerImagePrefixesSettingName = "forger-allowed-runner-image-prefixes"
)

var (
	modelDeploymentForgerDecimalPattern   = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
	modelDeploymentForgerMinSpeedupRat    = big.NewRat(101, 100)
	modelDeploymentForgerDefaultOutputQty = resource.MustParse("2Gi")
)

// forgerAllowedRunnerImagePrefixesValue reads the optional cluster-level prefix allowlist for
// External forge-runner images. It is a variable so tests can inject custom values or read errors.
var forgerAllowedRunnerImagePrefixesValue = func(ctx context.Context, reader ctrlcli.Reader) (string, error) {
	if s, ok := settings.Indexer()(forgerAllowedRunnerImagePrefixesSettingName); ok {
		val, err := s.Value(ctx)
		if err == nil || kerrors.IsNotFound(err) || errors.Is(err, setting.ErrSettingNotFound) {
			return val, nil
		}
		return "", err
	}
	if reader == nil {
		return "", nil
	}
	sec := new(core.Secret)
	err := reader.Get(ctx, ctrlcli.ObjectKey{
		Namespace: setting.DelegatedSecretNamespace,
		Name:      setting.DelegatedSecretName,
	}, sec)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if sec.Data == nil {
		return "", nil
	}
	return string(sec.Data[forgerAllowedRunnerImagePrefixesSettingName]), nil
}

// ModelDeploymentForgerWebhook validates ModelDeploymentForger specs and guards status updates.
//
// It reads the target ModelDeployment live when present so ineligible target roles (Encode,
// CPU-only, unmanaged command, Elastic EP, or SGLang-Ray) are refused at admission. When the
// target does not yet exist, target-dependent role checks are skipped so declarative applies do
// not depend on creation order.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeploymentforgers",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:validating:subResources=["status"]
type ModelDeploymentForgerWebhook struct {
	// APIReader reads the target ModelDeployment and optional settings live from the API server.
	APIReader ctrlcli.Reader

	// worker is the username the worker's own requests carry, the only writer of status.
	worker string
}

func (r *ModelDeploymentForgerWebhook) SetupWebhook(
	ctx context.Context, opts webhook.SetupOptions,
) (runtime.Object, error) {
	worker, err := selfUsername(ctx)
	if err != nil {
		return nil, fmt.Errorf("learn the worker's identity for the ModelDeploymentForger status rule: %w", err)
	}
	r.APIReader = opts.Manager.GetAPIReader()
	r.worker = worker

	return &workercore.ModelDeploymentForger{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelDeploymentForgerWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*ModelDeploymentForgerWebhook)(nil)
)

// ReceiveDeletionUpdate keeps the worker-only status guard active while a forger is Terminating.
func (*ModelDeploymentForgerWebhook) ReceiveDeletionUpdate() {}

func (r *ModelDeploymentForgerWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	mdf := obj.(*workercore.ModelDeploymentForger)
	if errs := r.validateModelDeploymentForger(ctx, nil, mdf); len(errs) > 0 {
		return nil, kerrors.NewInvalid(
			workercore.SchemeGroupVersionKind("ModelDeploymentForger").GroupKind(), mdf.Name, errs,
		)
	}

	return nil, nil
}

func (r *ModelDeploymentForgerWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	mdf := newObj.(*workercore.ModelDeploymentForger)
	if isStatusRequest(ctx) {
		return nil, r.validateModelDeploymentForgerStatus(ctx, mdf)
	}
	if !mdf.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	old, _ := oldObj.(*workercore.ModelDeploymentForger)

	if errs := r.validateModelDeploymentForger(ctx, old, mdf); len(errs) > 0 {
		return nil, kerrors.NewInvalid(
			workercore.SchemeGroupVersionKind("ModelDeploymentForger").GroupKind(), mdf.Name, errs,
		)
	}

	return nil, nil
}

func (*ModelDeploymentForgerWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	return nil, nil
}

func defaultModelDeploymentForger(mdf *workercore.ModelDeploymentForger) {
	if mdf.Spec.Mode == "" {
		mdf.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
	}
	for i := range mdf.Spec.Roles {
		if mdf.Spec.Roles[i].MaxKernelsPerGeneration == 0 {
			mdf.Spec.Roles[i].MaxKernelsPerGeneration = modelDeploymentForgerDefaultMaxKernelsPerGeneration
		}
		if mdf.Spec.Roles[i].MinSpeedupRatio == "" {
			mdf.Spec.Roles[i].MinSpeedupRatio = modelDeploymentForgerDefaultMinSpeedupRatio
		}
	}
	if mdf.Spec.Probe.Steps == 0 {
		mdf.Spec.Probe.Steps = modelDeploymentForgerDefaultProbeSteps
	}
	if mdf.Spec.Probe.CooldownSeconds == 0 {
		mdf.Spec.Probe.CooldownSeconds = modelDeploymentForgerDefaultCooldownSeconds
	}
	if mdf.Spec.Sandbox.RunnerMode == "" {
		mdf.Spec.Sandbox.RunnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
	}
	if mdf.Spec.Sandbox.TimeoutSeconds == 0 {
		mdf.Spec.Sandbox.TimeoutSeconds = modelDeploymentForgerDefaultTimeoutSeconds
	}
	if mdf.Spec.Sandbox.OutputSizeLimit == nil {
		mdf.Spec.Sandbox.OutputSizeLimit = ptr.To(modelDeploymentForgerDefaultOutputQty.DeepCopy())
	}
	if mdf.Spec.Rollout.MaxRetainedGenerations == 0 {
		mdf.Spec.Rollout.MaxRetainedGenerations = modelDeploymentForgerDefaultMaxRetainedGenerations
	}
	if mdf.Spec.Rollout.FailureTimeoutSeconds == 0 {
		mdf.Spec.Rollout.FailureTimeoutSeconds = modelDeploymentForgerDefaultFailureTimeoutSeconds
	}
}

func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForger(
	ctx context.Context, old, mdf *workercore.ModelDeploymentForger,
) field.ErrorList {
	effective := mdf.DeepCopy()
	defaultModelDeploymentForger(effective)

	var errs field.ErrorList
	errs = append(errs, validateModelDeploymentForgerTargetRef(effective)...)
	errs = append(errs, validateModelDeploymentForgerTargetRefImmutable(old, mdf)...)
	errs = append(errs, validateModelDeploymentForgerMode(effective)...)
	errs = append(errs, validateModelDeploymentForgerRoles(effective)...)
	errs = append(errs, validateModelDeploymentForgerProbe(&effective.Spec.Probe)...)
	errs = append(errs, r.validateModelDeploymentForgerSandbox(ctx, &effective.Spec.Sandbox)...)
	errs = append(errs, validateModelDeploymentForgerRollout(&effective.Spec.Rollout)...)

	if len(errs) > 0 {
		return errs
	}

	md, err := r.readModelDeploymentForgerTarget(ctx, effective)
	if err != nil {
		return append(errs, field.InternalError(field.NewPath("spec", "targetRef", "name"), err))
	}
	if md == nil {
		return nil
	}

	return validateModelDeploymentForgerAgainstTarget(effective, md)
}

func (r *ModelDeploymentForgerWebhook) readModelDeploymentForgerTarget(
	ctx context.Context, mdf *workercore.ModelDeploymentForger,
) (*workercore.ModelDeployment, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("the forger target cannot be read, because this handler has no reader")
	}

	md := new(workercore.ModelDeployment)
	err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: mdf.Namespace, Name: mdf.Spec.TargetRef.Name}, md)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read target ModelDeployment %q: %w", mdf.Spec.TargetRef.Name, err)
	}

	return md, nil
}

func validateModelDeploymentForgerTargetRef(mdf *workercore.ModelDeploymentForger) field.ErrorList {
	name := mdf.Spec.TargetRef.Name
	path := field.NewPath("spec", "targetRef", "name")
	switch {
	case strings.TrimSpace(name) == "":
		return field.ErrorList{field.Required(path, "must name the target ModelDeployment in this namespace")}
	case len(name) > modelDeploymentForgerMaxNameLen || strings.TrimSpace(name) != name:
		return field.ErrorList{field.Invalid(path, name,
			fmt.Sprintf("must be at most %d characters without surrounding whitespace", modelDeploymentForgerMaxNameLen))}
	default:
		return nil
	}
}

func validateModelDeploymentForgerTargetRefImmutable(
	old, mdf *workercore.ModelDeploymentForger,
) field.ErrorList {
	if old == nil || old.Spec.TargetRef.Name == mdf.Spec.TargetRef.Name {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "targetRef", "name"), mdf.Spec.TargetRef.Name,
		"spec.targetRef is immutable after creation: create a new ModelDeploymentForger for a different target",
	)}
}

func validateModelDeploymentForgerMode(mdf *workercore.ModelDeploymentForger) field.ErrorList {
	switch mdf.Spec.Mode {
	case workercore.ModelDeploymentForgerModeObserveOnly,
		workercore.ModelDeploymentForgerModeShadow,
		workercore.ModelDeploymentForgerModeActive:
		return nil
	default:
		return field.ErrorList{field.Invalid(
			field.NewPath("spec", "mode"), mdf.Spec.Mode,
			"must be one of ObserveOnly, Shadow, or Active",
		)}
	}
}

func validateModelDeploymentForgerRoles(mdf *workercore.ModelDeploymentForger) field.ErrorList {
	rolesPath := field.NewPath("spec", "roles")
	if len(mdf.Spec.Roles) == 0 {
		return field.ErrorList{field.Required(rolesPath, "at least one target role is required")}
	}

	var errs field.ErrorList
	seen := sets.New[string]()
	for i := range mdf.Spec.Roles {
		role := &mdf.Spec.Roles[i]
		rolePath := rolesPath.Index(i)

		trimmed := strings.TrimSpace(role.Name)
		switch {
		case trimmed == "":
			errs = append(errs, field.Required(rolePath.Child("name"),
				"must name a role declared on the target ModelDeployment"))
		case len(role.Name) > modelDeploymentForgerMaxNameLen || trimmed != role.Name:
			errs = append(errs, field.Invalid(rolePath.Child("name"), role.Name,
				fmt.Sprintf("must be at most %d characters without surrounding whitespace",
					modelDeploymentForgerMaxNameLen)))
		case seen.Has(role.Name):
			errs = append(errs, field.Duplicate(rolePath.Child("name"), role.Name))
		default:
			seen.Insert(role.Name)
		}

		if role.MaxKernelsPerGeneration < modelDeploymentForgerMinKernelsPerGeneration ||
			role.MaxKernelsPerGeneration > modelDeploymentForgerMaxKernelsPerGeneration {
			errs = append(errs, field.Invalid(
				rolePath.Child("maxKernelsPerGeneration"), role.MaxKernelsPerGeneration,
				fmt.Sprintf("must be between %d and %d",
					modelDeploymentForgerMinKernelsPerGeneration, modelDeploymentForgerMaxKernelsPerGeneration),
			))
		}

		if err := validateModelDeploymentForgerMinSpeedupRatio(role.MinSpeedupRatio); err != nil {
			errs = append(errs, field.Invalid(
				rolePath.Child("minSpeedupRatio"), role.MinSpeedupRatio, err.Error(),
			))
		}
	}

	return errs
}

func validateModelDeploymentForgerMinSpeedupRatio(raw string) error {
	if !modelDeploymentForgerDecimalPattern.MatchString(raw) {
		return fmt.Errorf("must be a decimal string at or above 1.01")
	}
	rat, ok := new(big.Rat).SetString(raw)
	if !ok {
		return fmt.Errorf("must be a valid decimal string at or above 1.01")
	}
	if rat.Cmp(modelDeploymentForgerMinSpeedupRat) < 0 {
		return fmt.Errorf("must be at least 1.01")
	}
	return nil
}

func validateModelDeploymentForgerProbe(probe *workercore.ModelDeploymentForgerProbeSpec) field.ErrorList {
	probePath := field.NewPath("spec", "probe")
	var errs field.ErrorList

	if probe.Steps < modelDeploymentForgerMinProbeSteps || probe.Steps > modelDeploymentForgerMaxProbeSteps {
		errs = append(errs, field.Invalid(probePath.Child("steps"), probe.Steps,
			fmt.Sprintf("must be between %d and %d",
				modelDeploymentForgerMinProbeSteps, modelDeploymentForgerMaxProbeSteps)))
	}
	if probe.CooldownSeconds < modelDeploymentForgerMinCooldownSeconds {
		errs = append(errs, field.Invalid(probePath.Child("cooldownSeconds"), probe.CooldownSeconds,
			fmt.Sprintf("must be at least %d seconds", modelDeploymentForgerMinCooldownSeconds)))
	}
	if probe.MinActiveRequests < 0 {
		errs = append(errs, field.Invalid(probePath.Child("minActiveRequests"), probe.MinActiveRequests,
			"must be greater than or equal to 0"))
	}

	return errs
}

func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForgerSandbox(
	ctx context.Context, sandbox *workercore.ModelDeploymentForgerSandboxSpec,
) field.ErrorList {
	sandboxPath := field.NewPath("spec", "sandbox")
	var errs field.ErrorList

	switch sandbox.RunnerMode {
	case workercore.ModelDeploymentForgerRunnerModeExternal:
		img := sandbox.RunnerImage
		imgPath := sandboxPath.Child("runnerImage")
		switch {
		case strings.TrimSpace(img) == "":
			errs = append(errs, field.Required(imgPath,
				"runnerImage is required when runnerMode is External"))
		case len(img) > modelDeploymentForgerMaxRunnerImageLen:
			errs = append(errs, field.Invalid(imgPath, img,
				fmt.Sprintf("must be at most %d characters", modelDeploymentForgerMaxRunnerImageLen)))
		case strings.ContainsFunc(img, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
			errs = append(errs, field.Invalid(imgPath, img,
				"must not contain whitespace or control characters"))
		default:
			if prefixErr := r.validateModelDeploymentForgerRunnerImagePrefix(ctx, img, imgPath); prefixErr != nil {
				errs = append(errs, prefixErr)
			}
		}
	case workercore.ModelDeploymentForgerRunnerModeMock:
		if len(sandbox.RunnerImage) > modelDeploymentForgerMaxRunnerImageLen {
			errs = append(errs, field.Invalid(sandboxPath.Child("runnerImage"), sandbox.RunnerImage,
				fmt.Sprintf("must be at most %d characters", modelDeploymentForgerMaxRunnerImageLen)))
		}
	default:
		errs = append(errs, field.Invalid(sandboxPath.Child("runnerMode"), sandbox.RunnerMode,
			"must be External or Mock"))
	}

	if sandbox.TimeoutSeconds < modelDeploymentForgerMinTimeoutSeconds ||
		sandbox.TimeoutSeconds > modelDeploymentForgerMaxTimeoutSeconds {
		errs = append(errs, field.Invalid(sandboxPath.Child("timeoutSeconds"), sandbox.TimeoutSeconds,
			fmt.Sprintf("must be between %d and %d seconds",
				modelDeploymentForgerMinTimeoutSeconds, modelDeploymentForgerMaxTimeoutSeconds)))
	}

	if sandbox.OutputSizeLimit != nil {
		limitBytes := sandbox.OutputSizeLimit.Value()
		if limitBytes < modelDeploymentForgerMinOutputSizeLimitByte ||
			limitBytes > modelDeploymentForgerMaxOutputSizeLimitByte {
			errs = append(errs, field.Invalid(sandboxPath.Child("outputSizeLimit"),
				sandbox.OutputSizeLimit.String(),
				"must be between 64Mi and 16Gi"))
		}
	}

	if sandbox.Resources != nil {
		resPath := sandboxPath.Child("resources")
		if sandbox.Resources.CPU != nil && sandbox.Resources.CPU.Sign() <= 0 {
			errs = append(errs, field.Invalid(resPath.Child("cpu"), sandbox.Resources.CPU.String(),
				"must be greater than zero"))
		}
		if sandbox.Resources.RAM != nil && sandbox.Resources.RAM.Sign() <= 0 {
			errs = append(errs, field.Invalid(resPath.Child("ram"), sandbox.Resources.RAM.String(),
				"must be greater than zero"))
		}
	}

	return errs
}

func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForgerRunnerImagePrefix(
	ctx context.Context, image string, imgPath *field.Path,
) *field.Error {
	raw, err := forgerAllowedRunnerImagePrefixesValue(ctx, r.APIReader)
	if err != nil {
		return field.InternalError(imgPath, fmt.Errorf("read setting %q: %w",
			forgerAllowedRunnerImagePrefixesSettingName, err))
	}

	var prefixes []string
	for _, entry := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(entry); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		return nil
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(image, prefix) {
			return nil
		}
	}

	return field.Forbidden(imgPath, fmt.Sprintf(
		"runnerImage %q does not match any allowed prefix configured in setting %q",
		image, forgerAllowedRunnerImagePrefixesSettingName,
	))
}

func validateModelDeploymentForgerRollout(rollout *workercore.ModelDeploymentForgerRolloutSpec) field.ErrorList {
	rolloutPath := field.NewPath("spec", "rollout")
	var errs field.ErrorList

	if rollout.MaxRetainedGenerations < modelDeploymentForgerMinRetainedGenerations ||
		rollout.MaxRetainedGenerations > modelDeploymentForgerMaxRetainedGenerations {
		errs = append(errs, field.Invalid(
			rolloutPath.Child("maxRetainedGenerations"), rollout.MaxRetainedGenerations,
			fmt.Sprintf("must be between %d and %d",
				modelDeploymentForgerMinRetainedGenerations, modelDeploymentForgerMaxRetainedGenerations),
		))
	}
	if rollout.FailureTimeoutSeconds < modelDeploymentForgerMinFailureTimeoutSeconds {
		errs = append(errs, field.Invalid(
			rolloutPath.Child("failureTimeoutSeconds"), rollout.FailureTimeoutSeconds,
			fmt.Sprintf("must be at least %d second", modelDeploymentForgerMinFailureTimeoutSeconds),
		))
	}

	return errs
}

func validateModelDeploymentForgerAgainstTarget(
	mdf *workercore.ModelDeploymentForger, md *workercore.ModelDeployment,
) field.ErrorList {
	declared := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		declared[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	var errs field.ErrorList
	rolesPath := field.NewPath("spec", "roles")
	for i := range mdf.Spec.Roles {
		rSpec := &mdf.Spec.Roles[i]
		namePath := rolesPath.Index(i).Child("name")

		role, ok := declared[rSpec.Name]
		if !ok {
			errs = append(errs, field.Invalid(namePath, rSpec.Name,
				"the target ModelDeployment does not declare a role with this name"))
			continue
		}

		errs = append(errs, validateModelDeploymentRoleKernelScope(md, role, namePath)...)
	}

	return errs
}
