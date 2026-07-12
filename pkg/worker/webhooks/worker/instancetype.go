package worker

import (
	"context"
	"strings"

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
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/settings"
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

	// Default the CPU group to the generic sentinel so a pool collapses without the admin
	// setting it; a real CPU group only matters when awareness is on.
	if it.Spec.GeneralGroup == "" {
		it.Spec.GeneralGroup = nodefeature.GeneralGroupGeneric
	}
	// An accelerated type needs an accelerator group to have a pool to schedule onto; leave it
	// for the validating webhook to reject rather than stamping degenerate labels.
	if it.Spec.Acceleratable && it.Spec.AcceleratorGroup == "" {
		return nil
	}

	// The pool's schedule labels from the spec identity and the awareness setting. Read the
	// setting here (the worker process carries the local settings indexer); validation stays
	// setting-independent.
	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)
	sched := nodefeature.PoolScheduleLabels(
		it.Spec.Acceleratable, cpuAware,
		it.Spec.GeneralGroup, it.Spec.AcceleratorGroup,
		it.Spec.OS, it.Spec.Arch)

	// Stamp the schedule labels on the InstanceType so it is selectable by the same
	// discriminators its Devices and ResourceFlavors carry. Drop a stale feature key left by a
	// group/acceleratable change — its key varies, while the os/arch and the acceleratable
	// boolean have fixed keys and are simply overwritten.
	if it.Labels == nil {
		it.Labels = make(map[string]string, len(sched)+1)
	}
	for k := range it.Labels {
		if _, want := sched[k]; want {
			continue
		}
		if strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
			strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix) {
			delete(it.Labels, k)
		}
	}
	for k, v := range sched {
		it.Labels[k] = v
	}
	// Advertise the fronting LocalQueue name so the Pod webhook reverse-looks-up this
	// InstanceType for the authoritative per-card VRAM (spec.memory).
	it.Labels[QueueEntranceLabelKey] = nodefeature.FormatLocalQueueName(it.Name)

	// A CPU-manufacturer-agnostic pool (awareness off and not acceleratable) collapses many CPU
	// kinds into one type, so no single ResourceFlavor's manufacturer/product/family is a valid
	// representative. Clear the CPU descriptors and skip enrichment (and its flavor List) so no
	// arbitrary flavor's identity is stamped onto the collapsed pool. An admin-provided DisplayName
	// is preserved, but the empty Product leaves a defaulted DisplayName unset on this path.
	if !cpuAware && !it.Spec.Acceleratable {
		it.Spec.Manufacturer = ""
		it.Spec.Product = ""
		it.Spec.Family = ""
		return nil
	}

	// Default the descriptors only while manufacturer and product are still empty; for an
	// accelerated type the per-card memory counts too (it feeds the ClusterQueue annotation
	// the Pod webhook reads), so a type already carrying its VRAM is treated as populated.
	if it.Spec.Manufacturer == "" && it.Spec.Product == "" && (!it.Spec.Acceleratable || it.Spec.Memory == "") {
		lbs := systemmeta.GetResourcesLabelSetOfType[ctrlcli.MatchingLabels]("nodes")
		for k, v := range sched {
			lbs[k] = v
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
			// Fold the raw CPU detail back into the spec only when CPU-manufacturer awareness is
			// on (the flavor carries the cpuDetail note always for a CPU flavor, but only when
			// aware for an accelerated one). Off leaves the CPU spec untouched.
			if cpuAware {
				foldCPUDetail(it, notes["cpuDetail"])
			}
		}
	}

	// Default the human-friendly DisplayName to the (possibly just-enriched) Product; an
	// admin-provided DisplayName is preserved. Cap the defaulted value at the field's maxLength (64)
	// so a long Product cannot produce a DisplayName the CRD schema would reject; an explicit
	// over-length DisplayName is left to fail validation as user error.
	if it.Spec.DisplayName == "" {
		it.Spec.DisplayName = it.Spec.Product
		if runes := []rune(it.Spec.DisplayName); len(runes) > 64 {
			it.Spec.DisplayName = string(runes[:64])
		}
	}

	return nil
}

// foldCPUDetail unmarshals a ResourceFlavor's cpuDetail note back into the InstanceType spec,
// mirroring the shape the NodeFlavorReconciler stored (the single typed source). InstanceTypeSpec
// inlines both an InstanceTypeCPU and an InstanceTypeAccelerator, so: a non-accelerated type's note
// is a plain InstanceTypeCPU folded into the embedded InstanceTypeCPU (promoted as spec.physicalCores
// etc.), while an accelerated type's note is an InstanceTypeAcceleratorCPU folded into spec.CPU (the
// embedded InstanceTypeAccelerator's CPU field, which also records the CPU's own
// manufacturer/product/family, distinct from the device's). The note is a nice-to-have, so a
// malformed note leaves the spec unchanged (the unmarshal target is only assigned on success); an
// empty note is a no-op.
func foldCPUDetail(it *workercore.InstanceType, raw string) {
	if raw == "" {
		return
	}
	if it.Spec.Acceleratable {
		var detail workercore.InstanceTypeAcceleratorCPU
		if err := json.Unmarshal([]byte(raw), &detail); err == nil {
			it.Spec.CPU = detail
		}
	} else {
		var detail workercore.InstanceTypeCPU
		if err := json.Unmarshal([]byte(raw), &detail); err == nil {
			it.Spec.InstanceTypeCPU = detail
		}
	}
}

// validateInstanceTypeSpec collects the required-input errors: an accelerator group when
// accelerated, os/arch non-empty, plus the unit-spec well-formedness checks. It reads no
// editable setting — validation is independent of instance-type-aware-cpu-manufacturer; the
// mutating webhook defaults an empty GeneralGroup to "generic", so GeneralGroup is never
// required here.
func validateInstanceTypeSpec(it *workercore.InstanceType) field.ErrorList {
	var errs field.ErrorList
	if it.Spec.Acceleratable && it.Spec.AcceleratorGroup == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "acceleratorGroup"),
			"must be specified when acceleratable is true"))
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
