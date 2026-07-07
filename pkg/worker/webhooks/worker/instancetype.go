package worker

import (
	"context"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/webhook"
)

// QueueEntranceLabelKey, on an InstanceType, records the name of the namespaced LocalQueue that
// fronts its backing ClusterQueue (a workload's "kueue.x-k8s.io/queue-name" value). The Pod
// webhook reverse-looks-up the InstanceType by this label to read the authoritative per-card VRAM
// (spec.memory), never trusting the user-writable LocalQueue. The Default webhook stamps it with
// nodefeature.FormatLocalQueueName(<InstanceType name>).
const QueueEntranceLabelKey = "schedule." + systemname.LabelPrefix + "queue-entrance"

// InstanceTypeWebhook defaults and validates a v1alpha1.InstanceType.
//
// The defaulting webhook enriches the descriptor spec from a matching ResourceFlavor at
// admission, so a stored InstanceType is spec-clear from day one. The validating webhook
// requires the admin-writable inputs on create and freezes the unit spec on update.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="instancetypes",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="instancetypes",scope="Cluster"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type InstanceTypeWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *InstanceTypeWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.InstanceType{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*InstanceTypeWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*InstanceTypeWebhook)(nil)
)

func (r *InstanceTypeWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	it := obj.(*workercore.InstanceType)

	if errs := validateInstanceTypeSpec(it); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("InstanceType"), it.Name, errs)
	}
	return nil, nil
}

func (r *InstanceTypeWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	itOld, it := oldObj.(*workercore.InstanceType), newObj.(*workercore.InstanceType)

	if errs := validateInstanceTypeSpec(it); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("InstanceType"), it.Name, errs)
	}
	if errs := validateInstanceTypeUnitSpecImmutable(itOld, it); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("InstanceType"), itOld.Name, errs)
	}
	return nil, nil
}

func (r *InstanceTypeWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// Default stamps the InstanceType's metadata labels and enriches its descriptor spec from a
// matching ResourceFlavor, so a stored InstanceType is spec-clear and selectable from day one.
// From the spec identity it derives the (acceleratable, group) feature key and, with
// kubernetes.io/os|arch, stamps the schedule discriminators the pool's Devices and ResourceFlavors
// carry (pruning a stale feature key left by a group/acceleratable change), plus the entrance
// label (nodefeature.FormatLocalQueueName(name)) the Pod webhook reverse-looks-up for the per-card
// VRAM. It then lists the ResourceFlavors by those discriminators, takes the first, and fills
// manufacturer/product/family (and, when accelerated, memory/cores/sliceable) from its notes.
// Labels are stamped whenever group is set; descriptor enrichment is a snapshot at admission
// (skipped once populated) and the reconciler does not refresh it as hardware changes.
func (r *InstanceTypeWebhook) Default(ctx context.Context, obj runtime.Object) error {
	it := obj.(*workercore.InstanceType)

	// Skip defaulting when group is empty, leaving the admin's spec intact.
	// The validating webhook will reject the create/update when group is empty.
	if it.Spec.Group == "" {
		return nil
	}

	// The pool's feature key from the spec identity.
	featureKey := nodefeature.GeneralFeatureLabelPrefix + it.Spec.Group
	if it.Spec.Acceleratable {
		featureKey = nodefeature.AcceleratableFeatureLabelPrefix + it.Spec.Group
	}

	// Stamp the pool's schedule labels on the InstanceType so it is selectable by the same
	// (feature-key, os, arch) discriminators its Devices and ResourceFlavors carry. Drop a stale
	// feature key left by a group/acceleratable change — its key varies, while os/arch keys are
	// fixed and simply overwritten.
	if it.Labels == nil {
		it.Labels = make(map[string]string)
	}
	for k := range it.Labels {
		if k != featureKey &&
			(strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
				strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)) {
			delete(it.Labels, k)
		}
	}
	it.Labels[featureKey] = "true"
	if it.Spec.OS != "" {
		it.Labels[core.LabelOSStable] = it.Spec.OS
	}
	if it.Spec.Arch != "" {
		it.Labels[core.LabelArchStable] = it.Spec.Arch
	}
	// Advertise the fronting LocalQueue name so the Pod webhook reverse-looks-up this
	// InstanceType for the authoritative per-card VRAM (spec.memory).
	it.Labels[QueueEntranceLabelKey] = nodefeature.FormatLocalQueueName(it.Name)

	// Default the descriptors only while manufacturer and product are still empty; for an
	// accelerated type the per-card memory counts too (it feeds the ClusterQueue annotation
	// the Pod webhook reads), so a type already carrying its VRAM is treated as populated.
	if it.Spec.Manufacturer == "" && it.Spec.Product == "" && (!it.Spec.Acceleratable || it.Spec.Memory == "") {
		lbs := systemmeta.GetResourcesLabelSetOfType[ctrlcli.MatchingLabels]("nodes")
		lbs[featureKey] = "true"
		if it.Spec.OS != "" {
			lbs[core.LabelOSStable] = it.Spec.OS
		}
		if it.Spec.Arch != "" {
			lbs[core.LabelArchStable] = it.Spec.Arch
		}

		rfList := new(kueue.ResourceFlavorList)
		err := r.Client.List(ctx, rfList, lbs,
			ctrlclix.WithoutQuorum,
			ctrlcli.UnsafeDisableDeepCopy,
			ctrlcli.Limit(1))
		if err != nil {
			return err
		}

		if len(rfList.Items) != 0 {
			_, notes := systemmeta.DescribeResource(&rfList.Items[0])
			it.Spec.Manufacturer = notes["manufacturer"]
			it.Spec.Product = notes["product"]
			it.Spec.Family = notes["family"]
			// Clear the accelerator descriptors before (re)deriving them, so a non-accelerated
			// type never keeps stale Memory/Cores/Sliceable from a prior accelerated state.
			it.Spec.InstanceTypeAccelerator = workercore.InstanceTypeAccelerator{}
			if it.Spec.Acceleratable {
				it.Spec.Memory = notes["memory"]
				it.Spec.Cores = notes["cores"]
				it.Spec.Sliceable = notes["sliceable"] == "true"
			}
		}
	}

	return nil
}

// validateInstanceTypeSpec collects the required-input errors: group/os/arch non-empty plus
// the unit-spec well-formedness checks.
func validateInstanceTypeSpec(it *workercore.InstanceType) field.ErrorList {
	var errs field.ErrorList
	if it.Spec.Group == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "group"), "must be specified"))
	}
	if it.Spec.OS == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "os"), "must be specified"))
	}
	if it.Spec.Arch == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "arch"), "must be specified"))
	}
	return append(errs, validateInstanceTypeUnitSpec(it)...)
}

// validateInstanceTypeUnitSpec enforces the unit spec: all three fields must be set and
// well-formed — unitCPU a unitless positive integer, unitRAM and localStorage a positive
// integer with a case-sensitive "Gi" suffix.
func validateInstanceTypeUnitSpec(it *workercore.InstanceType) field.ErrorList {
	cpu, ram, localStg := it.Spec.UnitResources.CPU, it.Spec.UnitResources.RAM, it.Spec.LocalStorage

	var errs field.ErrorList
	if extractPositiveNumberFromString(cpu) == "" {
		errs = append(errs, field.Invalid(field.NewPath("spec", "unitResources", "cpu"),
			cpu, "must be a positive integer with no unit suffix"))
	}
	if extractPositiveNumberFromQuantity(ram, "Gi") == "" {
		errs = append(errs, field.Invalid(field.NewPath("spec", "unitResources", "ram"),
			ram, "must be a positive integer with a case-sensitive \"Gi\" suffix"))
	}
	if extractPositiveNumberFromQuantity(localStg, "Gi") == "" {
		errs = append(errs, field.Invalid(field.NewPath("spec", "localStorage"),
			localStg, "must be a positive integer with a case-sensitive \"Gi\" suffix"))
	}
	return errs
}

// extractPositiveNumberFromString returns v unchanged when it is a positive integer
// carrying no unit suffix, and "" otherwise (including empty input).
func extractPositiveNumberFromString(v string) string {
	n, err := strconvx.ParseInt[int32](v, 10, 32)
	if err == nil && n > 0 {
		return v
	}
	return ""
}

// extractPositiveNumberFromQuantity strips the given suffix and returns the bare
// positive integer, or "" when the suffix is absent or the remainder is not a positive
// integer (including empty input).
func extractPositiveNumberFromQuantity(v, suffix string) string {
	b, ok := strings.CutSuffix(v, suffix)
	if ok {
		return extractPositiveNumberFromString(b)
	}
	return ""
}

// validateInstanceTypeUnitSpecImmutable rejects any change to the unit resources or the local
// storage after creation.
func validateInstanceTypeUnitSpecImmutable(itOld, it *workercore.InstanceType) field.ErrorList {
	var errs field.ErrorList
	if itOld.Spec.UnitResources != it.Spec.UnitResources {
		errs = append(errs, field.Invalid(field.NewPath("spec", "unitResources"),
			it.Spec.UnitResources, "unitResources is immutable"))
	}
	if itOld.Spec.LocalStorage != it.Spec.LocalStorage {
		errs = append(errs, field.Invalid(field.NewPath("spec", "localStorage"),
			it.Spec.LocalStorage, "localStorage is immutable"))
	}
	return errs
}
