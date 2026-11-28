package worker

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode"

	conregname "github.com/google/go-containerregistry/pkg/name"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	modelDeploymentForgerDefaultMaxKernelsPerGeneration = 4
	modelDeploymentForgerMinKernelsPerGeneration        = 1
	modelDeploymentForgerMaxKernelsPerGeneration        = 16

	modelDeploymentForgerDefaultMinSpeedupRatio = "1.05"

	modelDeploymentForgerDefaultProbeSteps              = 4
	modelDeploymentForgerMinProbeSteps                  = 3
	modelDeploymentForgerMaxProbeSteps                  = 5
	modelDeploymentForgerDefaultCooldownSeconds         = 600
	modelDeploymentForgerMinCooldownSeconds             = 60
	modelDeploymentForgerDefaultMinActiveRequests int32 = 1

	modelDeploymentForgerDefaultActiveDeadlineSeconds int64 = 1800
	modelDeploymentForgerMinActiveDeadlineSeconds     int64 = 60
	modelDeploymentForgerMaxActiveDeadlineSeconds     int64 = 14400
	modelDeploymentForgerMaxRunnerImageLen                  = 512
	modelDeploymentForgerMaxRunnerArgEntries                = 64
	modelDeploymentForgerMaxRunnerArgBytes                  = 4096
	modelDeploymentForgerMaxRunnerEnvEntries                = 64
	modelDeploymentForgerMaxRunnerEnvNameLen                = 256
	modelDeploymentForgerMaxRunnerEnvFromEntries            = 16
	modelDeploymentForgerMaxImagePullSecrets                = 32
	modelDeploymentForgerMaxPartitionProfileLen             = 64
	modelDeploymentForgerMaxNameLen                         = 253
	modelDeploymentForgerMinOutputSizeLimitByte             = 64 << 20
	modelDeploymentForgerMaxOutputSizeLimitByte             = 16 << 30
	modelDeploymentForgerReservedEnvPrefix                  = "GPUSTACK_FORGE_"

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
		if err == nil {
			if val != "" || reader == nil {
				return val, nil
			}
		} else if !kerrors.IsNotFound(err) && !errors.Is(err, setting.ErrSettingNotFound) {
			return "", err
		}
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

func (*ModelDeploymentForgerWebhook) Default(_ context.Context, obj runtime.Object) error {
	mdf := obj.(*workercore.ModelDeploymentForger)
	if !mdf.DeletionTimestamp.IsZero() {
		return nil
	}
	if mdf.Spec.Sandbox.ActiveDeadlineSeconds <= 0 {
		mdf.Spec.Sandbox.ActiveDeadlineSeconds = modelDeploymentForgerDefaultActiveDeadlineSeconds
	}
	defaultModelDeploymentForger(mdf)
	return nil
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
	if mdf.Spec.Probe.MinActiveRequests == nil {
		mdf.Spec.Probe.MinActiveRequests = ptr.To(modelDeploymentForgerDefaultMinActiveRequests)
	}
	if mdf.Spec.Sandbox.RunnerMode == "" {
		mdf.Spec.Sandbox.RunnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
	}
	if mdf.Spec.Sandbox.ActiveDeadlineSeconds == 0 {
		mdf.Spec.Sandbox.ActiveDeadlineSeconds = modelDeploymentForgerDefaultActiveDeadlineSeconds
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
	errs = append(errs, validateModelDeploymentForgerName(effective)...)
	errs = append(errs, validateModelDeploymentForgerTargetRef(effective)...)
	errs = append(errs, validateModelDeploymentForgerTargetRefImmutable(old, mdf)...)
	errs = append(errs, validateModelDeploymentForgerMode(effective)...)
	errs = append(errs, validateModelDeploymentForgerRoles(effective)...)
	errs = append(errs, validateModelDeploymentForgerProbe(&effective.Spec.Probe)...)
	errs = append(errs, r.validateModelDeploymentForgerSandbox(ctx, effective.Spec.Mode, &effective.Spec.Sandbox)...)
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

	return r.validateModelDeploymentForgerAgainstTarget(ctx, effective, md)
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

func validateModelDeploymentForgerName(mdf *workercore.ModelDeploymentForger) field.ErrorList {
	if len(mdf.Name) > kvalidation.DNS1123LabelMaxLength {
		return field.ErrorList{field.Invalid(
			field.NewPath("metadata", "name"), mdf.Name,
			fmt.Sprintf("must be at most %d characters so it fits Kubernetes label values",
				kvalidation.DNS1123LabelMaxLength),
		)}
	}
	if msgs := kvalidation.IsValidLabelValue(mdf.Name); len(msgs) > 0 {
		return field.ErrorList{field.Invalid(
			field.NewPath("metadata", "name"), mdf.Name,
			fmt.Sprintf("must be a valid Kubernetes label value: %s", strings.Join(msgs, "; ")),
		)}
	}
	return nil
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
		case len(role.Name) > kvalidation.DNS1123LabelMaxLength || trimmed != role.Name:
			errs = append(errs, field.Invalid(rolePath.Child("name"), role.Name,
				fmt.Sprintf("must be at most %d characters without surrounding whitespace",
					kvalidation.DNS1123LabelMaxLength)))
		default:
			if msgs := kvalidation.IsDNS1123Label(role.Name); len(msgs) > 0 {
				errs = append(errs, field.Invalid(rolePath.Child("name"), role.Name,
					fmt.Sprintf("must be a valid DNS-1123 label: %s", strings.Join(msgs, "; "))))
			} else if seen.Has(role.Name) {
				errs = append(errs, field.Duplicate(rolePath.Child("name"), role.Name))
			} else {
				seen.Insert(role.Name)
			}
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
	if probe.MinActiveRequests != nil && *probe.MinActiveRequests < 0 {
		errs = append(errs, field.Invalid(probePath.Child("minActiveRequests"), *probe.MinActiveRequests,
			"must be greater than or equal to 0"))
	}

	return errs
}

func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForgerSandbox(
	ctx context.Context,
	mode workercore.ModelDeploymentForgerMode,
	sandbox *workercore.ModelDeploymentForgerSandboxSpec,
) field.ErrorList {
	sandboxPath := field.NewPath("spec", "sandbox")
	var errs field.ErrorList

	switch sandbox.RunnerMode {
	case workercore.ModelDeploymentForgerRunnerModeExternal:
		if sandbox.Runner == nil {
			if mode != workercore.ModelDeploymentForgerModeObserveOnly {
				errs = append(errs, field.Required(sandboxPath.Child("runner"),
					"runner is required when runnerMode is External"))
			}
		} else {
			errs = append(errs, r.validateModelDeploymentForgerSandboxRunner(ctx, sandbox.Runner, sandboxPath.Child("runner"))...)
		}
	case workercore.ModelDeploymentForgerRunnerModeMock:
		if sandbox.Runner != nil {
			errs = append(errs, field.Invalid(sandboxPath.Child("runner"), sandbox.Runner,
				"runner container overrides are only allowed when runnerMode is External"))
		}
	default:
		errs = append(errs, field.Invalid(sandboxPath.Child("runnerMode"), sandbox.RunnerMode,
			"must be External or Mock"))
	}

	if sandbox.ActiveDeadlineSeconds < modelDeploymentForgerMinActiveDeadlineSeconds ||
		sandbox.ActiveDeadlineSeconds > modelDeploymentForgerMaxActiveDeadlineSeconds {
		errs = append(errs, field.Invalid(sandboxPath.Child("activeDeadlineSeconds"), sandbox.ActiveDeadlineSeconds,
			fmt.Sprintf("must be between %d and %d seconds",
				modelDeploymentForgerMinActiveDeadlineSeconds, modelDeploymentForgerMaxActiveDeadlineSeconds)))
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

	errs = append(errs, validateModelDeploymentForgerSandboxImagePullSecrets(
		sandbox.ImagePullSecrets, sandboxPath.Child("imagePullSecrets"),
	)...)

	if sandbox.Resources != nil {
		errs = append(errs, validateModelDeploymentForgerSandboxResources(
			sandbox.Resources, sandboxPath.Child("resources"),
		)...)
	}

	return errs
}

func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForgerSandboxRunner(
	ctx context.Context, runner *workercore.ModelDeploymentForgerSandboxRunner, runnerPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	img := runner.Image
	imgPath := runnerPath.Child("image")
	switch {
	case strings.TrimSpace(img) == "":
		errs = append(errs, field.Required(imgPath,
			"image is required when runnerMode is External"))
	case len(img) > modelDeploymentForgerMaxRunnerImageLen:
		errs = append(errs, field.Invalid(imgPath, img,
			fmt.Sprintf("must be at most %d characters", modelDeploymentForgerMaxRunnerImageLen)))
	case strings.ContainsFunc(img, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		errs = append(errs, field.Invalid(imgPath, img,
			"must not contain whitespace or control characters"))
	default:
		if err := validateRunnerImageReference(img); err != nil {
			errs = append(errs, field.Invalid(imgPath, img, err.Error()))
		} else if prefixErr := r.validateModelDeploymentForgerRunnerImagePrefix(ctx, img, imgPath); prefixErr != nil {
			errs = append(errs, prefixErr)
		}
	}

	if runner.ImagePullPolicy != "" {
		switch runner.ImagePullPolicy {
		case core.PullAlways, core.PullNever, core.PullIfNotPresent:
		default:
			errs = append(errs, field.NotSupported(runnerPath.Child("imagePullPolicy"), runner.ImagePullPolicy,
				[]string{string(core.PullAlways), string(core.PullNever), string(core.PullIfNotPresent)}))
		}
	}

	errs = append(errs, validateModelDeploymentForgerStringArgs(runner.Command, runnerPath.Child("command"))...)
	errs = append(errs, validateModelDeploymentForgerStringArgs(runner.Args, runnerPath.Child("args"))...)
	errs = append(errs, validateModelDeploymentForgerRunnerEnv(runner.Env, runnerPath.Child("env"))...)
	errs = append(errs, validateModelDeploymentForgerRunnerEnvFrom(runner.EnvFrom, runnerPath.Child("envFrom"))...)

	return errs
}

func validateModelDeploymentForgerStringArgs(values []string, fldPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(values) > modelDeploymentForgerMaxRunnerArgEntries {
		errs = append(errs, field.TooMany(fldPath, len(values), modelDeploymentForgerMaxRunnerArgEntries))
	}
	for i, val := range values {
		itemPath := fldPath.Index(i)
		if len(val) > modelDeploymentForgerMaxRunnerArgBytes {
			errs = append(errs, field.Invalid(itemPath, fmt.Sprintf("<%d bytes>", len(val)),
				fmt.Sprintf("must be at most %d bytes", modelDeploymentForgerMaxRunnerArgBytes)))
		}
		if strings.IndexByte(val, 0) != -1 {
			errs = append(errs, field.Invalid(itemPath, val, "must not contain NUL bytes"))
		}
	}
	return errs
}

func validateModelDeploymentForgerRunnerEnv(
	env []workercore.ModelDeploymentForgerSandboxEnvVar, envPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	if len(env) > modelDeploymentForgerMaxRunnerEnvEntries {
		errs = append(errs, field.TooMany(envPath, len(env), modelDeploymentForgerMaxRunnerEnvEntries))
	}

	seen := sets.New[string]()
	for i := range env {
		ev := &env[i]
		evPath := envPath.Index(i)
		namePath := evPath.Child("name")

		switch {
		case ev.Name == "":
			errs = append(errs, field.Required(namePath, "must not be empty"))
		case len(ev.Name) > modelDeploymentForgerMaxRunnerEnvNameLen:
			errs = append(errs, field.Invalid(namePath, ev.Name,
				fmt.Sprintf("must be at most %d characters", modelDeploymentForgerMaxRunnerEnvNameLen)))
		default:
			if msgs := kvalidation.IsEnvVarName(ev.Name); len(msgs) > 0 {
				errs = append(errs, field.Invalid(namePath, ev.Name,
					fmt.Sprintf("must be a valid environment variable name: %s", strings.Join(msgs, "; "))))
			} else if strings.HasPrefix(strings.ToUpper(ev.Name), modelDeploymentForgerReservedEnvPrefix) {
				errs = append(errs, field.Forbidden(namePath,
					"environment variable names starting with GPUSTACK_FORGE_ are reserved by the operator"))
			}
			if seen.Has(ev.Name) {
				errs = append(errs, field.Duplicate(namePath, ev.Name))
			} else {
				seen.Insert(ev.Name)
			}
		}

		if ev.Value != "" && ev.ValueFrom != nil {
			errs = append(errs, field.Invalid(evPath.Child("valueFrom"), ev.ValueFrom,
				"may not be specified when value is non-empty"))
		}
		if ev.ValueFrom != nil {
			vfPath := evPath.Child("valueFrom")
			cmRef := ev.ValueFrom.ConfigMapKeyRef
			secRef := ev.ValueFrom.SecretKeyRef
			switch {
			case cmRef == nil && secRef == nil:
				errs = append(errs, field.Required(vfPath,
					"must specify exactly one of configMapKeyRef or secretKeyRef"))
			case cmRef != nil && secRef != nil:
				errs = append(errs, field.Invalid(vfPath, ev.ValueFrom,
					"may not specify more than one of configMapKeyRef or secretKeyRef"))
			case cmRef != nil:
				errs = append(errs, validateModelDeploymentForgerKeySelector(
					cmRef.Name, cmRef.Key, vfPath.Child("configMapKeyRef"),
				)...)
			case secRef != nil:
				errs = append(errs, validateModelDeploymentForgerKeySelector(
					secRef.Name, secRef.Key, vfPath.Child("secretKeyRef"),
				)...)
			}
		}
	}
	return errs
}

func validateModelDeploymentForgerKeySelector(name, key string, refPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	namePath := refPath.Child("name")
	if name == "" {
		errs = append(errs, field.Required(namePath, "must not be empty"))
	} else if msgs := kvalidation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		errs = append(errs, field.Invalid(namePath, name, strings.Join(msgs, "; ")))
	}

	keyPath := refPath.Child("key")
	if key == "" {
		errs = append(errs, field.Required(keyPath, "must not be empty"))
	} else if msgs := kvalidation.IsConfigMapKey(key); len(msgs) > 0 {
		errs = append(errs, field.Invalid(keyPath, key, strings.Join(msgs, "; ")))
	}
	return errs
}

func validateModelDeploymentForgerRunnerEnvFrom(
	envFrom []core.EnvFromSource, envFromPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	if len(envFrom) > modelDeploymentForgerMaxRunnerEnvFromEntries {
		errs = append(errs, field.TooMany(envFromPath, len(envFrom), modelDeploymentForgerMaxRunnerEnvFromEntries))
	}

	for i := range envFrom {
		ef := &envFrom[i]
		efPath := envFromPath.Index(i)

		switch {
		case ef.ConfigMapRef == nil && ef.SecretRef == nil:
			errs = append(errs, field.Required(efPath,
				"must specify exactly one of configMapRef or secretRef"))
		case ef.ConfigMapRef != nil && ef.SecretRef != nil:
			errs = append(errs, field.Invalid(efPath, ef,
				"may not specify more than one of configMapRef or secretRef"))
		case ef.ConfigMapRef != nil:
			namePath := efPath.Child("configMapRef", "name")
			if ef.ConfigMapRef.Name == "" {
				errs = append(errs, field.Required(namePath, "must not be empty"))
			} else if msgs := kvalidation.IsDNS1123Subdomain(ef.ConfigMapRef.Name); len(msgs) > 0 {
				errs = append(errs, field.Invalid(namePath, ef.ConfigMapRef.Name, strings.Join(msgs, "; ")))
			}
		case ef.SecretRef != nil:
			namePath := efPath.Child("secretRef", "name")
			if ef.SecretRef.Name == "" {
				errs = append(errs, field.Required(namePath, "must not be empty"))
			} else if msgs := kvalidation.IsDNS1123Subdomain(ef.SecretRef.Name); len(msgs) > 0 {
				errs = append(errs, field.Invalid(namePath, ef.SecretRef.Name, strings.Join(msgs, "; ")))
			}
		}

		if ef.Prefix != "" {
			prefixPath := efPath.Child("prefix")
			if len(ef.Prefix) > modelDeploymentForgerMaxRunnerEnvNameLen {
				errs = append(errs, field.Invalid(prefixPath, ef.Prefix,
					fmt.Sprintf("must be at most %d characters", modelDeploymentForgerMaxRunnerEnvNameLen)))
			} else if msgs := kvalidation.IsEnvVarName(ef.Prefix); len(msgs) > 0 {
				errs = append(errs, field.Invalid(prefixPath, ef.Prefix,
					fmt.Sprintf("must be a valid environment variable prefix: %s", strings.Join(msgs, "; "))))
			} else if strings.HasPrefix(strings.ToUpper(ef.Prefix), modelDeploymentForgerReservedEnvPrefix) {
				errs = append(errs, field.Forbidden(prefixPath,
					"environment variable prefixes starting with GPUSTACK_FORGE_ are reserved by the operator"))
			}
		}
	}
	return errs
}

func validateModelDeploymentForgerSandboxImagePullSecrets(
	refs []core.LocalObjectReference, secretsPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	if len(refs) > modelDeploymentForgerMaxImagePullSecrets {
		errs = append(errs, field.TooMany(secretsPath, len(refs), modelDeploymentForgerMaxImagePullSecrets))
	}

	seen := sets.New[string]()
	for i := range refs {
		name := refs[i].Name
		namePath := secretsPath.Index(i).Child("name")
		if name == "" {
			errs = append(errs, field.Required(namePath, "name of the referenced secret must be specified"))
			continue
		}
		if msgs := kvalidation.IsDNS1123Subdomain(name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(namePath, name, strings.Join(msgs, "; ")))
			continue
		}
		if seen.Has(name) {
			errs = append(errs, field.Duplicate(namePath, name))
			continue
		}
		seen.Insert(name)
	}
	return errs
}

func validateModelDeploymentForgerSandboxResources(
	res *workercore.ModelDeploymentForgerSandboxResources, resPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	if res.CPU != nil && res.CPU.Sign() <= 0 {
		errs = append(errs, field.Invalid(resPath.Child("cpu"), res.CPU.String(),
			"must be greater than zero"))
	}
	if res.RAM != nil && res.RAM.Sign() <= 0 {
		errs = append(errs, field.Invalid(resPath.Child("ram"), res.RAM.String(),
			"must be greater than zero"))
	}
	if res.LocalStorage != nil && res.LocalStorage.Sign() <= 0 {
		errs = append(errs, field.Invalid(resPath.Child("localStorage"), res.LocalStorage.String(),
			"must be greater than zero"))
	}

	if res.AcceleratorSlicedMemoryPercentage < 0 || res.AcceleratorSlicedMemoryPercentage > 100 {
		errs = append(errs, field.Invalid(resPath.Child("acceleratorSlicedMemoryPercentage"),
			res.AcceleratorSlicedMemoryPercentage, "must be between 0 and 100"))
	}
	if res.AcceleratorSlicedCoresPercentage < 0 || res.AcceleratorSlicedCoresPercentage > 100 {
		errs = append(errs, field.Invalid(resPath.Child("acceleratorSlicedCoresPercentage"),
			res.AcceleratorSlicedCoresPercentage, "must be between 0 and 100"))
	}
	if res.AcceleratorSlicedCoresPercentage > 0 && res.AcceleratorSlicedMemoryPercentage == 0 {
		errs = append(errs, field.Invalid(resPath.Child("acceleratorSlicedCoresPercentage"),
			res.AcceleratorSlicedCoresPercentage,
			"acceleratorSlicedMemoryPercentage must be greater than 0 when acceleratorSlicedCoresPercentage is set"))
	}

	if profile := res.AcceleratorPartitionedProfile; profile != "" {
		profilePath := resPath.Child("acceleratorPartitionedProfile")
		if len(profile) > modelDeploymentForgerMaxPartitionProfileLen {
			errs = append(errs, field.Invalid(profilePath, profile,
				fmt.Sprintf("must be at most %d characters", modelDeploymentForgerMaxPartitionProfileLen)))
		} else if strings.ContainsFunc(profile, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			errs = append(errs, field.Invalid(profilePath, profile,
				"must not contain whitespace or control characters"))
		}
		if res.AcceleratorSlicedMemoryPercentage > 0 || res.AcceleratorSlicedCoresPercentage > 0 {
			errs = append(errs, field.Invalid(profilePath, profile,
				fmt.Sprintf(
					"a partition profile cannot be combined with %s or %s: hardware partitioning and "+
						"software slicing cannot both apply to one accelerator",
					resPath.Child("acceleratorSlicedMemoryPercentage"),
					resPath.Child("acceleratorSlicedCoresPercentage"),
				)))
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
		if runnerImageMatchesPrefix(image, prefix) {
			return nil
		}
	}

	return field.Forbidden(imgPath, fmt.Sprintf(
		"runnerImage %q does not match any allowed prefix configured in setting %q",
		image, forgerAllowedRunnerImagePrefixesSettingName,
	))
}

func validateRunnerImageReference(image string) error {
	if hasInvalidImagePathSegments(image) {
		return errors.New("must not contain empty or dot path segments")
	}
	parsed, err := conregname.ParseReference(image)
	if err != nil {
		return fmt.Errorf("is not a valid container image reference: %w", err)
	}
	for seg := range strings.SplitSeq(parsed.Context().RepositoryStr(), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("must not contain empty or dot repository segments")
		}
	}
	return nil
}

func hasInvalidImagePathSegments(image string) bool {
	if image == "" {
		return true
	}
	namePart := image
	if at := strings.IndexByte(namePart, '@'); at != -1 {
		digestPart := namePart[at+1:]
		if digestPart == "" || strings.IndexByte(digestPart, '/') != -1 {
			return true
		}
		namePart = namePart[:at]
	}
	lastSlash := strings.LastIndexByte(namePart, '/')
	if colon := strings.IndexByte(namePart[lastSlash+1:], ':'); colon != -1 {
		tagPart := namePart[lastSlash+1+colon+1:]
		if tagPart == "" {
			return true
		}
		namePart = namePart[:lastSlash+1+colon]
	}
	if namePart == "" {
		return true
	}
	for seg := range strings.SplitSeq(namePart, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func isRegistryAuthorityPrefix(prefix string) bool {
	host := prefix
	if colon := strings.IndexByte(prefix, ':'); colon != -1 {
		host = prefix[:colon]
	}
	return strings.IndexByte(host, '.') != -1 || host == "localhost"
}

func runnerImageMatchesPrefix(image, prefix string) bool {
	if prefix == "" || hasInvalidImagePathSegments(image) || !strings.HasPrefix(image, prefix) {
		return false
	}
	ref, err := conregname.ParseReference(image)
	if err != nil {
		return false
	}
	for seg := range strings.SplitSeq(ref.Context().RepositoryStr(), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}

	rest := image[len(prefix):]
	if strings.IndexByte(prefix, '/') == -1 {
		switch {
		case prefix[len(prefix)-1] == ':' && !isRegistryAuthorityPrefix(prefix):
			return rest != "" && strings.IndexByte(rest, '/') == -1 && strings.IndexByte(rest, '@') == -1
		case prefix[len(prefix)-1] == '@':
			return rest != "" && strings.IndexByte(rest, '/') == -1
		case strings.IndexByte(prefix, '@') != -1:
			return rest == ""
		case isRegistryAuthorityPrefix(prefix):
			if len(rest) <= 1 || rest[0] != '/' {
				return false
			}
			reg := ref.Context().RegistryStr()
			return reg == prefix || (prefix == "docker.io" && reg == conregname.DefaultRegistry)
		case strings.IndexByte(prefix, ':') != -1:
			return rest == ""
		default:
			if rest == "" {
				return true
			}
			switch rest[0] {
			case '/':
				return len(rest) > 1
			case '@', ':':
				return len(rest) > 1 && strings.IndexByte(rest[1:], '/') == -1
			default:
				return false
			}
		}
	}

	switch prefix[len(prefix)-1] {
	case ':':
		return rest != "" && strings.IndexByte(rest, '/') == -1 && strings.IndexByte(rest, '@') == -1
	case '@':
		return rest != "" && strings.IndexByte(rest, '/') == -1
	case '/':
		return rest != ""
	}

	lastSeg := prefix[strings.LastIndexByte(prefix, '/')+1:]
	if strings.IndexByte(lastSeg, ':') != -1 || strings.IndexByte(lastSeg, '@') != -1 {
		return rest == ""
	}

	if rest == "" {
		return true
	}
	switch rest[0] {
	case '/':
		return len(rest) > 1
	case '@', ':':
		return len(rest) > 1 && strings.IndexByte(rest[1:], '/') == -1
	default:
		return false
	}
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

func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForgerAgainstTarget(
	ctx context.Context, mdf *workercore.ModelDeploymentForger, md *workercore.ModelDeployment,
) field.ErrorList {
	declared := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		declared[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	res := mdf.Spec.Sandbox.Resources
	needsPoolCheck := r.APIReader != nil && res != nil &&
		(res.AcceleratorSlicedMemoryPercentage > 0 ||
			res.AcceleratorSlicedCoresPercentage > 0 ||
			res.AcceleratorPartitionedProfile != "")
	checkedPools := sets.New[string]()

	var errs field.ErrorList
	rolesPath := field.NewPath("spec", "roles")
	resPath := field.NewPath("spec", "sandbox", "resources")
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
		if mdf.Spec.Mode == workercore.ModelDeploymentForgerModeActive {
			for _, av := range role.AdditionalVolumes {
				if kernelPathsOverlap(av.MountPath, modelDeploymentKernelMountPath) {
					errs = append(errs, field.Invalid(namePath, rSpec.Name, fmt.Sprintf(
						"target role mounts an additionalVolume at %q overlapping %q, "+
							"which conflicts with Active kernel promotion",
						av.MountPath, modelDeploymentKernelMountPath,
					)))
					break
				}
			}
		}

		if needsPoolCheck && role.AcceleratorType != "" && !checkedPools.Has(role.AcceleratorType) {
			checkedPools.Insert(role.AcceleratorType)
			detail, found, err := r.readForgerTargetPoolDetail(ctx, role.AcceleratorType)
			if err != nil {
				errs = append(errs, field.InternalError(resPath, err))
				continue
			}
			if found {
				errs = append(errs, validateForgerSandboxResourcesAgainstPoolDetail(
					role.AcceleratorType, detail, res, resPath,
				)...)
			}
		}
	}

	return errs
}

func (r *ModelDeploymentForgerWebhook) readForgerTargetPoolDetail(
	ctx context.Context, name string,
) (*workercore.InstanceTypeDetail, bool, error) {
	key := ctrlcli.ObjectKey{Name: name}

	v1AT := new(worker.AcceleratorType)
	if err := r.APIReader.Get(ctx, key, v1AT); err == nil {
		return &v1AT.Status.Detail, true, nil
	} else if !kerrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("read AcceleratorType %q: %w", name, err)
	}

	at := new(workercore.AcceleratorType)
	if err := r.APIReader.Get(ctx, key, at); err == nil {
		return &at.Status.Detail, true, nil
	} else if !kerrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("read AcceleratorType %q: %w", name, err)
	}

	v1IT := new(worker.InstanceType)
	if err := r.APIReader.Get(ctx, key, v1IT); err == nil {
		return &v1IT.Status.Detail, true, nil
	} else if !kerrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("read InstanceType %q: %w", name, err)
	}

	it := new(workercore.InstanceType)
	if err := r.APIReader.Get(ctx, key, it); err == nil {
		return &it.Status.Detail, true, nil
	} else if !kerrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("read InstanceType %q: %w", name, err)
	}

	return nil, false, nil
}

func validateForgerSandboxResourcesAgainstPoolDetail(
	typeName string,
	detail *workercore.InstanceTypeDetail,
	res *workercore.ModelDeploymentForgerSandboxResources,
	resPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	switch {
	case res.AcceleratorPartitionedProfile != "":
		profile := res.AcceleratorPartitionedProfile
		profilePath := resPath.Child("acceleratorPartitionedProfile")
		if nodefeature.GetAcceleratablePartitionedProfileResourceName(detail.Manufacturer, profile) == "" {
			errs = append(errs, field.Invalid(profilePath, profile,
				fmt.Sprintf("accelerator type %s (manufacturer %q) does not support partition profile %q",
					typeName, detail.Manufacturer, profile)))
			return errs
		}
		if !detail.IsPhysicallySliceable() && len(detail.SlicedDetail.Physical.Profiles) == 0 {
			errs = append(errs, field.Forbidden(profilePath,
				fmt.Sprintf("accelerator type %s does not offer hardware partitioning", typeName)))
			return errs
		}
		profiles := detail.SlicedDetail.Physical.Profiles
		offered := make([]string, 0, len(profiles))
		for _, p := range profiles {
			if p.Name == profile {
				return nil
			}
			offered = append(offered, p.Name)
		}
		errs = append(errs, field.Invalid(profilePath, profile,
			fmt.Sprintf("accelerator type %s does not offer this partition profile; offered: %v",
				typeName, offered)))

	case res.AcceleratorSlicedMemoryPercentage > 0 || res.AcceleratorSlicedCoresPercentage > 0:
		if !detail.IsLogicallySliceable() {
			errs = append(errs, field.Forbidden(resPath.Child("acceleratorSlicedMemoryPercentage"),
				fmt.Sprintf("accelerator type %s does not offer logical slicing", typeName)))
		}
	}

	return errs
}
