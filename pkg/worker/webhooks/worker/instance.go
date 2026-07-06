package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// InstanceWebhook hooks a v1alpha1.Instance object.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="instances",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="instances",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type InstanceWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *InstanceWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.Instance{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*InstanceWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*InstanceWebhook)(nil)
)

func (r *InstanceWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	inst := obj.(*workercore.Instance)

	if kuberess.IsReservedNamespace(inst.Namespace) {
		return nil, field.Invalid(
			field.NewPath("metadata.namespace"), inst.Namespace, "cannot create instance in reserved namespace")
	}

	if inst.Spec.Type == "" {
		return nil, field.Required(
			field.NewPath("spec.type"), "type must be specified")
	}

	// Only a wanna run instance requires an existing InstanceType and resource limits
	// validated against it; a stopped instance may reference an InstanceType that is
	// draining or already removed.
	var instType *worker.InstanceType
	if !ptr.Deref(inst.Spec.Stop, false) {
		instType = &worker.InstanceType{
			ObjectMeta: meta.ObjectMeta{
				Name: inst.Spec.Type,
			},
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return nil, field.InternalError(
					field.NewPath("spec.type"), fmt.Errorf("get instance type: %w", err))
			}
			err = r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType,
				ctrlclix.WithoutQuorum)
			if err != nil {
				return nil, field.NotFound(
					field.NewPath("spec.type"), inst.Spec.Type)
			}
		}
	}

	var errs field.ErrorList
	if instRess := inst.Spec.Resources; instType != nil && instRess != nil {
		errs = append(errs, validateResourceRequests(instType, instRess)...)
	}
	switch {
	case inst.Spec.Volume.Ephemeral != nil && inst.Spec.Volume.Persistent != nil:
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.volume"), "cannot specify both ephemeral and persistent of volume"))
	case inst.Spec.Volume.Ephemeral == nil && inst.Spec.Volume.Persistent == nil:
		errs = append(errs, field.Required(
			field.NewPath("spec.volume"), "either ephemeral or persistent of volume should be specified"))
	case inst.Spec.Volume.Ephemeral != nil:
		if inst.Spec.Volume.Ephemeral.Capacity.Sign() <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.volume.ephemeral.capacity"), inst.Spec.Volume.Ephemeral.Capacity.String(),
				"capacity of ephemeral volume must be positive"))
		}
	case inst.Spec.Volume.Persistent != nil:
		if inst.Spec.Volume.Persistent.Name == "" {
			errs = append(errs, field.Required(
				field.NewPath("spec.volume.persistent.name"), "name of the persistent volume must be specified"))
		}
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("Instance"), inst.Name, errs)
	}

	return nil, nil
}

func (r *InstanceWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	instOld, inst := oldObj.(*workercore.Instance), newObj.(*workercore.Instance)

	stopped := ptr.Deref(instOld.Spec.Stop, false)
	starting := stopped && !ptr.Deref(inst.Spec.Stop, false)
	stopping := !stopped && ptr.Deref(inst.Spec.Stop, false)

	var errs field.ErrorList

	// Validate filed modification,
	// only can be modified when the instance is stopped,
	// except the "Volume".
	if !stopped {
		if inst.Spec.Type != instOld.Spec.Type {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.type"), "type is immutable"),
			)
		}
		if inst.Spec.Image != instOld.Spec.Image {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.image"), "image is immutable"),
			)
		}
		if inst.Spec.ImagePullPolicy != instOld.Spec.ImagePullPolicy {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.imagePullPolicy"), "imagePullPolicy is immutable"),
			)
		}
		if !kubemeta.DeepEqual(inst.Spec.Command, instOld.Spec.Command) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.command"), "command is immutable"),
			)
		}
		if inst.Spec.Privileged != instOld.Spec.Privileged {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.privileged"), "privileged is immutable"),
			)
		}
		if !kubemeta.DeepEqual(inst.Spec.Ports, instOld.Spec.Ports) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.ports"), "ports is immutable"),
			)
		}
		if !kubemeta.DeepEqual(inst.Spec.Env, instOld.Spec.Env) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.env"), "env is immutable"),
			)
		}
		if inst.Spec.VolumeMount != instOld.Spec.VolumeMount {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.volumeMount"), "volumeMount is immutable"),
			)
		}
		if !kubemeta.DeepEqual(inst.Spec.ImagePullSecret, instOld.Spec.ImagePullSecret) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.imagePullSecret"), "imagePullSecret is immutable"),
			)
		}
		if !kubemeta.DeepEqual(inst.Spec.Resources, instOld.Spec.Resources) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.resources"), "resources is immutable"),
			)
		}
		if !kubemeta.DeepEqual(inst.Spec.SSHPublicKey, instOld.Spec.SSHPublicKey) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.sshPublicKey"), "sshPublicKey is immutable"),
			)
		}
	}
	if !kubemeta.DeepEqual(inst.Spec.Volume, instOld.Spec.Volume) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.volume"), "volume is immutable"),
		)
	}

	// Validate state transition.
	switch {
	case starting:
		// Validate the instance is actually stopped before allowing it to be started.
		if instOld.Status.Phase != workerctrl.InstancePhaseStopped {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.stop"), "can only start stopped instance"))
		}
		// Validate the existence of the referenced InstanceType when starting a stopped instance.
		instType := &worker.InstanceType{
			ObjectMeta: meta.ObjectMeta{
				Name: inst.Spec.Type,
			},
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return nil, field.InternalError(
					field.NewPath("spec.type"), fmt.Errorf("get instance type: %w", err))
			}
			err = r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType,
				ctrlclix.WithoutQuorum)
			if err != nil {
				errs = append(errs, field.NotFound(
					field.NewPath("spec.type"), inst.Spec.Type))
			}
		}
		// Re-validate the resources that will take effect on start with the SAME checks
		// ValidateCreate applies (sign, accelerator/CPU caps, slice-percentage ranges, and
		// per-unit RAM / local storage), not just the upper caps — a stopped Instance may have
		// had its resources edited while stopped, when the immutability guard above is skipped.
		if inst.Spec.Resources != nil {
			errs = append(errs, validateResourceRequests(instType, inst.Spec.Resources)...)
		}
	case stopping:
		// Validate the instance is not starting when trying to stop it,
		// to avoid the race condition between starting and stopping an instance.
		if instOld.Status.Phase == workerctrl.InstancePhaseStarting {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.stop"), "cannot stop starting instance"))
		}
	}

	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(worker.Kind("Instance"), inst.Name, errs)
	}

	return nil, nil
}

func (r *InstanceWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

func (r *InstanceWebhook) Default(ctx context.Context, obj runtime.Object) error {
	inst := obj.(*workercore.Instance)

	if inst.Spec.ImagePullPolicy == "" {
		inst.Spec.ImagePullPolicy = core.PullIfNotPresent
	}

	if inst.Spec.VolumeMount == "" {
		inst.Spec.VolumeMount = "/workspace"
	}

	if inst.Spec.Resources == nil {
		inst.Spec.Resources = &workercore.InstanceResources{}
	}

	// If type is not specified,
	// skip defaulting resources and let later validation to block it.
	if inst.Spec.Type == "" {
		return nil
	}

	// If the instance is stopped,
	// skip defaulting resources.
	if ptr.Deref(inst.Spec.Stop, false) {
		return nil
	}

	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name: inst.Spec.Type,
		},
	}
	err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return field.InternalError(
				field.NewPath("spec.type"), fmt.Errorf("get instance type: %w", err))
		}
		err = r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType,
			ctrlclix.WithoutQuorum)
		if err != nil {
			return field.NotFound(
				field.NewPath("spec.type"), inst.Spec.Type)
		}
	}

	withGeneralOvercommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)

	instRess := inst.Spec.Resources
	if instType.Spec.Acceleratable {
		if instRess.Accelerator == nil {
			// Default a request here,
			// and let later scheduling to block if it exceeds the instance type's CPU capacity.
			instRess.Accelerator = resource.NewQuantity(1, resource.DecimalSI)
		}
		memPct := instRess.AcceleratorSlicedMemoryPercentage
		coresPct := instRess.AcceleratorSlicedCoresPercentage
		if instType.Spec.Sliceable && (memPct > 0 || coresPct > 0) {
			// A slice is a fraction of ONE card, so default an absent or explicitly zero
			// accelerator count to 1 (nil was already defaulted above); otherwise
			// validation would reject it for not being exactly 1.
			if instRess.Accelerator.Value() == 0 {
				instRess.Accelerator = resource.NewQuantity(1, resource.DecimalSI)
			}
			// On a sliceable InstanceType, when only one of the memory/compute slice
			// percentages is set, copy it to the other so a bare memory request yields
			// an equal compute share (and vice versa).
			switch {
			case memPct > 0 && coresPct == 0:
				instRess.AcceleratorSlicedCoresPercentage = memPct
			case coresPct > 0 && memPct == 0:
				instRess.AcceleratorSlicedMemoryPercentage = coresPct
			}
			// A sliced request holds a fraction of ONE card (its accelerator count is
			// pinned to 1 by validation). UnitResources sizes a whole card, so size the
			// host CPU/RAM by the memory percentage — the fraction of the card actually
			// reserved. The compute percentage only throttles GPU cores, not host
			// resources. The memory percentage is non-zero here (copy-filled above when
			// only the compute percentage was set).
			unitPct := int64(instRess.AcceleratorSlicedMemoryPercentage)
			if withGeneralOvercommit || instRess.CPU.IsZero() {
				instRess.CPU, err = quantityx.StringPercentMultiply(instType.Spec.UnitResources.CPU, unitPct)
				if err != nil {
					return field.InternalError(
						field.NewPath("spec.resources.cpu"),
						fmt.Errorf("invalid CPU unit of instance type %s: %w", instType.Name, err))
				}
			}
			if withGeneralOvercommit || instRess.RAM.IsZero() {
				instRess.RAM, err = quantityx.StringPercentMultiply(instType.Spec.UnitResources.RAM, unitPct)
				if err != nil {
					return field.InternalError(
						field.NewPath("spec.resources.ram"),
						fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err))
				}
			}
		} else {
			// A whole-card request — a non-sliceable type, or a zero-percentage request on a
			// sliceable type — scales the unit CPU/RAM by the accelerator count. Allow a
			// zero/absent count, but treat it as 1 when calculating other resource requests.
			accC := instRess.Accelerator.Value()
			if accC == 0 {
				accC = 1
			}
			if withGeneralOvercommit || instRess.CPU.IsZero() {
				instRess.CPU, err = quantityx.StringMultiply(instType.Spec.UnitResources.CPU, accC)
				if err != nil {
					return field.InternalError(
						field.NewPath("spec.resources.cpu"),
						fmt.Errorf("invalid CPU unit of instance type %s: %w", instType.Name, err))
				}
			}
			if withGeneralOvercommit || instRess.RAM.IsZero() {
				instRess.RAM, err = quantityx.StringMultiply(instType.Spec.UnitResources.RAM, accC)
				if err != nil {
					return field.InternalError(
						field.NewPath("spec.resources.ram"),
						fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err))
				}
			}
		}
	} else {
		cpuC := int64(1)
		if instRess.CPU.IsZero() {
			// Default a request here,
			// and let later scheduling to block if it exceeds the instance type's CPU capacity.
			instRess.CPU = *resource.NewQuantity(1, resource.DecimalSI)
		} else {
			cpuC = instRess.CPU.Value()
		}
		if withGeneralOvercommit || instRess.RAM.IsZero() {
			instRess.RAM, err = quantityx.StringMultiply(instType.Spec.UnitResources.RAM, cpuC)
			if err != nil {
				return field.InternalError(
					field.NewPath("spec.resources.ram"),
					fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err))
			}
		}
	}
	if instRess.LocalStorage.IsZero() {
		// Default a local-storage request to 15Gi, but never above the InstanceType's
		// LocalStorage (the validating webhook caps an explicit request there too).
		def := resource.NewQuantity(15<<30, resource.BinarySI) // 15Gi
		if instType.Spec.LocalStorage != "" {
			if maxStg, perr := resource.ParseQuantity(instType.Spec.LocalStorage); perr == nil && def.Cmp(maxStg) > 0 {
				def = &maxStg
			}
		}
		instRess.LocalStorage = *def
	}

	return nil
}

// validateResourceRequests checks an Instance's resource requests against its InstanceType's
// entitlements: sign, the accelerator/CPU caps, the sliced-percentage ranges, and (via
// capResourcesToInstanceType) the per-unit RAM / local-storage limits. Shared by ValidateCreate and
// the start (resume) path of ValidateUpdate, so a stopped Instance whose resources were edited
// cannot be started with a request that create would have rejected — the start path previously
// re-checked only the upper caps (capResourcesToInstanceType), letting negative / over-max /
// out-of-range slice requests through.
func validateResourceRequests(instType *worker.InstanceType, instRess *workercore.InstanceResources) field.ErrorList {
	var errs field.ErrorList
	// Validate accelerator request first since it may determine the validation of other resource requests.
	if instType.Spec.Acceleratable {
		switch {
		case instType.Spec.Sliceable:
			// The slice is requested as memory/compute percentages in [0,100] (0 disables
			// slicing). The compute budget must not be smaller than the memory budget.
			memPct := int64(instRess.AcceleratorSlicedMemoryPercentage)
			coresPct := int64(instRess.AcceleratorSlicedCoresPercentage)
			memPath := field.NewPath("spec.resources.acceleratorSlicedMemoryPercentage")
			coresPath := field.NewPath("spec.resources.acceleratorSlicedCoresPercentage")
			if memPct < 0 || memPct > 100 {
				errs = append(errs, field.Invalid(memPath, instRess.AcceleratorSlicedMemoryPercentage,
					"must be between 0 and 100"))
			}
			if coresPct < 0 || coresPct > 100 {
				errs = append(errs, field.Invalid(coresPath, instRess.AcceleratorSlicedCoresPercentage,
					"must be between 0 and 100"))
			}
			if coresPct > 0 && coresPct < memPct {
				errs = append(errs, field.Invalid(coresPath, instRess.AcceleratorSlicedCoresPercentage,
					"must not be less than the memory percentage"))
			}
			if memPct > 0 || coresPct > 0 {
				// A sliced request (a non-zero percentage) is a fraction of ONE card: the
				// slice is expressed through the percentages, not the card count, so the
				// accelerator count must be exactly 1. Compare with Cmp (not Value(), which
				// rounds a fractional quantity like "1m" up to 1) so only a true 1 passes.
				one := resource.NewQuantity(1, resource.DecimalSI)
				if instRess.Accelerator == nil || instRess.Accelerator.Cmp(*one) != 0 {
					got := "0"
					if instRess.Accelerator != nil {
						got = instRess.Accelerator.String()
					}
					errs = append(errs, field.Invalid(
						field.NewPath("spec.resources.accelerator"), got,
						"accelerator request must be exactly 1 for a sliced request"))
				}
			} else {
				// A zero-percentage request on a sliceable type is a whole-card exclusive
				// request, which may span multiple cards and is bounded like a non-sliceable
				// type.
				errs = append(errs, validateExclusiveAcceleratorRequest(instType, instRess)...)
			}
		default:
			errs = append(errs, validateExclusiveAcceleratorRequest(instType, instRess)...)
		}
	} else if instRess.Accelerator != nil && !instRess.Accelerator.IsZero() {
		errs = append(errs, field.Invalid(
			field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
			"accelerator request must not be specified for non-acceleratable instance type"))
	}
	if instRess.CPU.Sign() < 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec.resources.cpu"), instRess.CPU.String(),
			"CPU request cannot be negative"))
	} else if !instType.Spec.Acceleratable &&
		instRess.CPU.Cmp(instType.Status.CPU.OnceMaxRequest) > 0 {
		// Only a non-accelerated type has a CPU capacity view; an accelerated type's
		// Status.CPU is zero (its CPU derives from unitCPU × count, bounded elsewhere).
		errs = append(errs, field.Invalid(
			field.NewPath("spec.resources.cpu"), instRess.CPU.String(),
			fmt.Sprintf("exceeds the maximum CPU request of instance type %s", instType.Name)))
	}
	// A negative RAM or local-storage request is rejected outright; a positive one
	// must stay within the InstanceType's per-unit RAM entitlement and its local
	// storage (validated against UnitResources / LocalStorage below).
	if instRess.RAM.Sign() < 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec.resources.ram"), instRess.RAM.String(),
			"RAM request cannot be negative"))
	}
	if instRess.LocalStorage.Sign() < 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec.resources.localStorage"), instRess.LocalStorage.String(),
			"local storage request cannot be negative"))
	}
	errs = append(errs, capResourcesToInstanceType(instType, instRess)...)
	return errs
}

// validateExclusiveAcceleratorRequest checks a whole-card (exclusive) accelerator request:
// it may not be negative, must be a whole number of cards, and may not exceed the
// InstanceType's whole-card OnceMaxRequest. A nil request is left to defaulting. It is shared
// by a non-sliceable type and by a zero-percentage (whole-card) request on a sliceable type.
func validateExclusiveAcceleratorRequest(
	instType *worker.InstanceType, instRess *workercore.InstanceResources,
) field.ErrorList {
	if instRess.Accelerator == nil {
		return nil
	}
	if instRess.Accelerator.Sign() < 0 {
		return field.ErrorList{field.Invalid(
			field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
			"accelerator request cannot be negative")}
	}
	// A whole-card request is emitted verbatim as a Pod extended-resource quantity, which
	// Kubernetes requires to be an integer, so reject a fractional count (e.g. "1m") here
	// rather than letting it fail later at Pod admission.
	if _, ok := instRess.Accelerator.AsInt64(); !ok {
		return field.ErrorList{field.Invalid(
			field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
			"accelerator request must be a whole number of cards")}
	}
	if instRess.Accelerator.Cmp(instType.Status.Accelerator.OnceMaxRequest) > 0 {
		return field.ErrorList{field.Invalid(
			field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
			fmt.Sprintf("exceeds the maximum accelerator request of instance type %s", instType.Name))}
	}
	return nil
}

// capResourcesToInstanceType rejects an Instance whose CPU or RAM exceeds the InstanceType's
// per-unit entitlement (unitCPU/unitRAM x unit count) or whose local storage exceeds the
// InstanceType's LocalStorage. The unit count mirrors the Default derivation: the accelerator
// count for an acceleratable type, otherwise the CPU count. The CPU cap applies only to an
// acceleratable type — a non-accelerated type's CPU is bounded by its ClusterQueue capacity
// (Status.CPU) in ValidateCreate, and deriving its unit count from the CPU request itself would
// make a unitCPU x count cap circular.
func capResourcesToInstanceType(
	instType *worker.InstanceType, instRess *workercore.InstanceResources,
) field.ErrorList {
	var errs field.ErrorList

	unitCount := int64(1)
	if instType.Spec.Acceleratable {
		if instRess.Accelerator != nil && instRess.Accelerator.Value() > 1 {
			unitCount = instRess.Accelerator.Value()
		}
	} else if instRess.CPU.Value() > 1 {
		unitCount = instRess.CPU.Value()
	}

	if instType.Spec.Acceleratable {
		if maxCPU, err := quantityx.StringMultiply(instType.Spec.UnitResources.CPU, unitCount); err == nil &&
			instRess.CPU.Cmp(maxCPU) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.cpu"), instRess.CPU.String(),
				fmt.Sprintf("exceeds the maximum CPU %s of instance type %s", maxCPU.String(), instType.Name)))
		}
	}
	if maxRAM, err := quantityx.StringMultiply(instType.Spec.UnitResources.RAM, unitCount); err == nil &&
		instRess.RAM.Cmp(maxRAM) > 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec.resources.ram"), instRess.RAM.String(),
			fmt.Sprintf("exceeds the maximum RAM %s of instance type %s", maxRAM.String(), instType.Name)))
	}
	if instType.Spec.LocalStorage != "" {
		if maxStg, err := resource.ParseQuantity(instType.Spec.LocalStorage); err == nil &&
			instRess.LocalStorage.Cmp(maxStg) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.localStorage"), instRess.LocalStorage.String(),
				fmt.Sprintf("exceeds the local storage %s of instance type %s", maxStg.String(), instType.Name)))
		}
	}
	return errs
}
