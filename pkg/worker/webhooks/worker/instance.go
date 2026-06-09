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
// +k8s:webhook-gen:mutating:operations=["CREATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
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

	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name: inst.Spec.Type,
		},
	}
	err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, field.InternalError(
				field.NewPath("spec.type"), fmt.Errorf("get instance type: %w", err))
		}
		return nil, field.NotFound(
			field.NewPath("spec.type"), inst.Spec.Type)
	}

	var errs field.ErrorList
	if instRess := inst.Spec.Resources; instRess != nil {
		// Validate accelerator request first since it may determine the validation of other resource requests.
		if instType.Spec.Acceleratable {
			if instRess.Accelerator != nil {
				if instRess.Accelerator.Sign() < 0 {
					errs = append(errs, field.Invalid(
						field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
						"accelerator request cannot be negative"))
				} else if instRess.Accelerator.Cmp(instType.Status.Accelerator.OnceMaxRequest) > 0 {
					errs = append(errs, field.Invalid(
						field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
						fmt.Sprintf("exceeds the maximum accelerator request of instance type %s", instType.Name)))
				}
			}
		} else if instRess.Accelerator != nil && !instRess.Accelerator.IsZero() {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.accelerator"), instRess.Accelerator.String(),
				"accelerator request must not specified for non-acceleratable instance type"))
		}
		if instRess.CPU.Sign() < 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.cpu"), instRess.CPU.String(),
				"CPU request cannot be negative"))
		} else if instRess.CPU.Cmp(instType.Status.CPU.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.cpu"), instRess.CPU.String(),
				fmt.Sprintf("exceeds the maximum CPU request of instance type %s", instType.Name)))
		}
		if instRess.RAM.Sign() < 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.ram"), instRess.RAM.String(),
				"RAM request cannot be negative"))
		} else if instRess.RAM.Cmp(instType.Status.RAM.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.ram"), instRess.RAM.String(),
				fmt.Sprintf("exceeds the maximum RAM request of instance type %s", instType.Name)))
		}
		if instRess.LocalStorage.Sign() < 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.localStorage"), instRess.LocalStorage.String(),
				"local storage request cannot be negative"))
		} else if instRess.LocalStorage.Cmp(instType.Status.LocalStorage.OnceMaxRequest) > 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.resources.localStorage"), instRess.LocalStorage.String(),
				fmt.Sprintf("exceeds the maximum local storage request of instance type %s", instType.Name)))
		}
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

	var errs field.ErrorList

	// Immutable fields validation.
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
	if !kubemeta.DeepEqual(inst.Spec.Volume, instOld.Spec.Volume) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.volume"), "volume is immutable"),
		)
	}
	if !kubemeta.DeepEqual(inst.Spec.SSHPublicKey, instOld.Spec.SSHPublicKey) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.sshPublicKey"), "sshPublicKey is immutable"),
		)
	}

	// Phase transition validation.
	if !ptr.Deref(instOld.Spec.Stop, false) && ptr.Deref(inst.Spec.Stop, false) {
		if instOld.Status.Phase == workerctrl.PhaseStarting {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.stop"), "cannot stop starting instance"))
		}
	}
	if ptr.Deref(instOld.Spec.Stop, false) && !ptr.Deref(inst.Spec.Stop, false) {
		if instOld.Status.Phase != workerctrl.PhaseStopped {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.stop"), "can only start stopped instance"))
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
		return field.NotFound(
			field.NewPath("spec.type"), inst.Spec.Type)
	}

	if inst.Spec.ImagePullPolicy == "" {
		inst.Spec.ImagePullPolicy = core.PullIfNotPresent
	}

	if inst.Spec.VolumeMount == "" {
		inst.Spec.VolumeMount = "/workspace"
	}

	if inst.Spec.Resources == nil {
		inst.Spec.Resources = &workercore.InstanceResources{}
	}

	withGeneralOvercommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)

	instRess := inst.Spec.Resources
	if instType.Spec.Acceleratable {
		if instRess.Accelerator == nil {
			// Default a request here,
			// and let later scheduling to block if it exceeds the instance type's CPU capacity.
			instRess.Accelerator = resource.NewQuantity(1, resource.DecimalSI)
		}
		// Allow zero accelerator request for acceleratable instance type,
		// but treat it as 1 when calculating other resource requests by unit.
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
		stgQ := *resource.NewQuantity(15<<30, resource.BinarySI) // 15Gi
		if stgQ.Cmp(instType.Status.LocalStorage.OnceMaxRequest) > 0 {
			stgQ = instType.Status.LocalStorage.OnceMaxRequest.DeepCopy()
		}
		instRess.LocalStorage = stgQ
	}

	return nil
}
