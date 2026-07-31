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
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
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
	if !inst.Spec.Stop {
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
		// A slice request whose accelerator Detail is not yet computed cannot be validated;
		// reject it with a transient (retryable) error, never a permanent Invalid, so the same
		// request succeeds once the reconciler populates Status.Detail.
		if slicingRequestNotReady(instType, instRess) {
			return nil, kerrors.NewInternalError(fmt.Errorf("instance type %s is not ready yet; retry", instType.Name))
		}
		errs = append(errs, validateResourceRequests(instType, instRess)...)
	}
	nodeErr, err := r.validateNodePin(ctx, inst.Spec.NodeName)
	if err != nil {
		return nil, err
	}
	if nodeErr != nil {
		errs = append(errs, nodeErr)
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

	stopped := instOld.Spec.Stop
	starting := stopped && !inst.Spec.Stop
	stopping := !stopped && inst.Spec.Stop

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
		if inst.Spec.NodeName != instOld.Spec.NodeName {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec.nodeName"), "nodeName is immutable"),
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
		// The node pin is deliberately NOT re-validated here: it is checked once, on create, and a
		// node that has since gone away makes the Pod stay Pending with the scheduler's own reason
		// rather than block the start.
		//
		// Re-validate the resources that will take effect on start with the SAME checks
		// ValidateCreate applies (sign, accelerator/CPU caps, slice-percentage ranges, and
		// per-unit RAM / local storage), not just the upper caps — a stopped Instance may have
		// had its resources edited while stopped, when the immutability guard above is skipped.
		if inst.Spec.Resources != nil {
			// As on create, a slice request whose accelerator Detail is not yet computed is
			// rejected with a transient (retryable) error, not a permanent Invalid.
			if slicingRequestNotReady(instType, inst.Spec.Resources) {
				return nil, kerrors.NewInternalError(fmt.Errorf("instance type %s is not ready yet; retry", instType.Name))
			}
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

// validateNodePin checks that the pinned Node exists, catching a typo at creation rather than
// leaving the Instance Pending forever. An unpinned Instance passes.
//
// Existence is the only thing checked, and only on create. Whether the node can actually serve the
// Instance's InstanceType is left to the scheduler: a node outside the pool's flavors simply does
// not schedule, whereas rejecting the pin here would forbid legitimate placements — pinning a
// card-less Instance (a model download, say) to a specific accelerated node among them.
//
// A rejection is returned as the *field.Error; a read failure that is not a plain "not found" is
// returned as the error instead, so a transient API problem never becomes a permanent rejection.
func (r *InstanceWebhook) validateNodePin(ctx context.Context, nodeName string) (*field.Error, error) {
	if nodeName == "" {
		return nil, nil
	}

	path := field.NewPath("spec.nodeName")
	nd := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: nodeName,
		},
	}
	err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(nd), nd)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, field.InternalError(path, fmt.Errorf("get node: %w", err))
		}
		// A cache miss can be an unsynced informer, so confirm against the live reader — where
		// only a real "not found" is a permanent rejection. A timeout or an authorization
		// failure must stay retryable rather than read as a node that does not exist.
		err = r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(nd), nd,
			ctrlclix.WithoutQuorum)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return nil, field.InternalError(path, fmt.Errorf("get node: %w", err))
			}
			return field.NotFound(path, nodeName), nil
		}
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
	if inst.Spec.Stop {
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
	// A slice request cannot be sized until the reconciler has computed the accelerator Detail:
	// its sliceability and per-card sizing come from Status.Detail. Reject with a transient
	// (retryable) error rather than fall through to whole-card sizing (which would silently
	// mis-size the Pod); the same request succeeds once Detail is populated.
	if slicingRequestNotReady(instType, instRess) {
		return kerrors.NewInternalError(fmt.Errorf("instance type %s is not ready yet; retry", instType.Name))
	}
	if instType.Spec.Acceleratable {
		if instRess.Accelerator == nil {
			// Default a request here,
			// and let later scheduling to block if it exceeds the instance type's CPU capacity.
			instRess.Accelerator = resource.NewQuantity(1, resource.DecimalSI)
		}
		memPct := instRess.AcceleratorSlicedMemoryPercentage
		coresPct := instRess.AcceleratorSlicedCoresPercentage
		partitionPct, sizeable := partitionProfileMemoryPercent(instType, instRess.AcceleratorPartitionedProfile)
		if !sizeable {
			// The pool offers the profile but cannot size it yet. Reject as retryable, the same
			// way slicingRequestNotReady does for an uncomputed Detail — whole-card sizing here
			// would stick, because Default does not run again once the resources are set.
			return kerrors.NewInternalError(fmt.Errorf(
				"instance type %s is not ready yet (partition profile %q cannot be sized from the "+
					"observed accelerator detail); retry",
				instType.Name, instRess.AcceleratorPartitionedProfile))
		}
		switch {
		case partitionPct > 0:
			// A hardware partition is one instance on ONE card, so default an absent or
			// explicitly zero accelerator count to 1 (nil was already defaulted above);
			// otherwise validation would reject it for not being exactly 1.
			if instRess.Accelerator.Value() == 0 {
				instRess.Accelerator = resource.NewQuantity(1, resource.DecimalSI)
			}
			// UnitResources sizes a whole card, so size the host CPU/RAM by the share of the
			// card's VRAM the profile occupies — the same VRAM-anchored fraction the logical
			// slice path uses, so a partition and a slice of equal size cost equal host
			// resources.
			if err = sizeAcceleratorUnitByPercent(instType, instRess, partitionPct, withGeneralOvercommit); err != nil {
				return err
			}
		case instType.Status.Detail.IsLogicallySliceable() && (memPct > 0 || coresPct > 0):
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
			if err = sizeAcceleratorUnitByPercent(instType, instRess,
				int64(instRess.AcceleratorSlicedMemoryPercentage), withGeneralOvercommit); err != nil {
				return err
			}
		default:
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

// sizeAcceleratorUnitByPercent sizes the host CPU/RAM of a request that holds a fraction of ONE
// card — a logical slice or a hardware partition — as that percentage of the InstanceType's
// whole-card unit resources. An explicit request is preserved unless overcommit is on, matching
// the whole-card path.
func sizeAcceleratorUnitByPercent(
	instType *worker.InstanceType, instRess *workercore.InstanceResources,
	unitPct int64, withGeneralOvercommit bool,
) error {
	var err error
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
	return nil
}

// partitionProfileMemoryPercent reports the share of one card's VRAM the requested hardware
// partition profile occupies, as a percentage in [1,100].
//
// It reports sizeable=false when the pool offers the profile but its observed Detail cannot size
// it yet — the profile's per-instance memory has not been populated, or the per-card VRAM has not
// — which is a transient state during detection or a device-manager rollout skew. The caller must
// reject such a request as retryable, exactly as the Pod webhook does for the same state: falling
// back to whole-card sizing would persist an Instance sized for a whole card, and Default does not
// run again to correct it.
//
// It reports (0, true) when the request is not a partition request at all, or when the named
// profile is not offered. That second case is permanent, not transient, and validation rejects it
// on its own terms with a message naming the offered profiles.
func partitionProfileMemoryPercent(instType *worker.InstanceType, profile string) (pct int64, sizeable bool) {
	if profile == "" {
		return 0, true
	}
	prof, _, found := partitionProfile((*workercore.InstanceType)(instType), profile)
	if !found {
		return 0, true
	}
	if prof.MemoryMib <= 0 {
		return 0, false
	}
	cardVRAMMib, err := instanceTypeCardVRAMMib((*workercore.InstanceType)(instType))
	if err != nil || cardVRAMMib <= 0 {
		return 0, false
	}
	return min(max(prof.MemoryMib*100/cardVRAMMib, 1), 100), true
}

// slicingRequestNotReady reports whether the request asks for a share of a card — a logical slice
// (a non-zero memory or compute percentage) or a hardware partition (a non-empty profile) — of an
// accelerated InstanceType whose observed accelerator Detail has not been computed yet.
// Sliceability, the partition profile inventory and the per-card sizing are all read from
// Status.Detail, so until it is ready such a request can neither be sized nor validated and must be
// rejected as retryable — never treated as a whole-card request, which would silently admit a
// mis-sized Pod, and never as an unknown profile, which would permanently reject a valid request.
func slicingRequestNotReady(instType *worker.InstanceType, instRess *workercore.InstanceResources) bool {
	if instRess == nil || !instType.Spec.Acceleratable {
		return false
	}
	// A non-zero percentage (including a negative, which range validation later rejects only
	// once Detail is ready) is a slice request: gate it as not-ready so an empty Detail can
	// never fall through to whole-card sizing.
	slice := instRess.AcceleratorSlicedMemoryPercentage != 0 || instRess.AcceleratorSlicedCoresPercentage != 0
	partition := instRess.AcceleratorPartitionedProfile != ""
	return (slice || partition) && !instType.Status.Detail.AcceleratorReady()
}

// validateResourceRequests checks an Instance's resource requests against its InstanceType's
// entitlements: sign, the accelerator/CPU caps, the partition profile inventory, the
// sliced-percentage ranges, and (via
// capResourcesToInstanceType) the per-unit RAM / local-storage limits. Shared by ValidateCreate and
// the start (resume) path of ValidateUpdate, so a stopped Instance whose resources were edited
// cannot be started with a request that create would have rejected — the start path previously
// re-checked only the upper caps (capResourcesToInstanceType), letting negative / over-max /
// out-of-range slice requests through.
func validateResourceRequests(instType *worker.InstanceType, instRess *workercore.InstanceResources) field.ErrorList {
	var errs field.ErrorList
	// Validate accelerator request first since it may determine the validation of other resource requests.
	if instType.Spec.Acceleratable {
		memPct := int64(instRess.AcceleratorSlicedMemoryPercentage)
		coresPct := int64(instRess.AcceleratorSlicedCoresPercentage)
		switch {
		case instRess.AcceleratorPartitionedProfile != "":
			errs = append(errs, validatePartitionedAcceleratorRequest(instType, instRess)...)
		case memPct != 0 || coresPct != 0:
			// A logical slice is requested as memory/compute percentages in [0,100]; the two
			// budgets are independent. A pool with no logically sliceable card cannot serve
			// the request at all — on an all-partitioned pool it would otherwise be admitted
			// and then stay Pending forever — so it is rejected here rather than reshaped
			// into a whole-card request.
			if !instType.Status.Detail.IsLogicallySliceable() {
				errs = append(errs, field.Forbidden(
					field.NewPath("spec.resources.acceleratorSlicedMemoryPercentage"),
					fmt.Sprintf("instance type %s does not offer logical slicing", instType.Name)))
				break
			}
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
			// A sliced request (a non-zero percentage) is a fraction of ONE card: the
			// slice is expressed through the percentages, not the card count, so the
			// accelerator count must be exactly 1.
			errs = append(errs, validateSingleCardRequest(instRess, "sliced")...)
		default:
			// A zero-percentage request — on a sliceable type or not — is a whole-card
			// exclusive request, which may span multiple cards.
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
			cpuRequestRejection(instType)))
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

// cpuRequestRejection explains why a CPU request does not fit a non-accelerated InstanceType. A
// zero maximum does not mean "the limit is small", and the two ways to reach it read very
// differently to an administrator: a pool whose nodes are all drained or unmanaged keeps its
// ClusterQueue admitting — so it reports phase Active with a capacity of zero — and a bare
// "exceeds the maximum" then sends the reader looking for a limit that was never the problem.
// Name the actual state instead, and carry the maximum in the ordinary case as the RAM and local
// storage messages already do.
func cpuRequestRejection(instType *worker.InstanceType) string {
	cpu := instType.Status.CPU
	switch {
	case cpu.Capacity.IsZero():
		return fmt.Sprintf("instance type %s has no CPU capacity: no managed node currently backs it",
			instType.Name)
	case cpu.OnceMaxRequest.IsZero():
		return fmt.Sprintf("instance type %s has no CPU available: its capacity %s is fully requested",
			instType.Name, cpu.Capacity.String())
	default:
		return fmt.Sprintf("exceeds the maximum CPU request %s of instance type %s",
			cpu.OnceMaxRequest.String(), instType.Name)
	}
}

// validateSingleCardRequest checks that a request holding a fraction of ONE card — a logical
// slice or a hardware partition — asks for exactly one card. Compare with Cmp (not Value(),
// which rounds a fractional quantity like "1m" up to 1) so only a true 1 passes. The kind names
// the request in the message.
func validateSingleCardRequest(instRess *workercore.InstanceResources, kind string) field.ErrorList {
	one := resource.NewQuantity(1, resource.DecimalSI)
	if instRess.Accelerator != nil && instRess.Accelerator.Cmp(*one) == 0 {
		return nil
	}
	got := "0"
	if instRess.Accelerator != nil {
		got = instRess.Accelerator.String()
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("spec.resources.accelerator"), got,
		fmt.Sprintf("accelerator request must be exactly 1 for a %s request", kind))}
}

// validatePartitionedAcceleratorRequest checks a hardware partition request: it is mutually
// exclusive with the two logical slice percentages (hardware partitioning and software slicing
// cannot both apply to one card), the fronting pool offers the capability at all, it names a
// profile that pool actually offers — reported with the offered set, since a profile the pool
// cannot build would otherwise sit Pending forever — and it is exactly one card, one instance. A
// manufacturer with no hardware partitioning yields no partition resource key at all, so its
// request is rejected here rather than shaped into an empty key by the controller.
func validatePartitionedAcceleratorRequest(
	instType *worker.InstanceType, instRess *workercore.InstanceResources,
) field.ErrorList {
	var errs field.ErrorList
	profile := instRess.AcceleratorPartitionedProfile
	profilePath := field.NewPath("spec.resources.acceleratorPartitionedProfile")

	if instRess.AcceleratorSlicedMemoryPercentage != 0 || instRess.AcceleratorSlicedCoresPercentage != 0 {
		errs = append(errs, field.Forbidden(profilePath,
			"a hardware partition and a logical slice percentage are mutually exclusive; "+
				"clear acceleratorSlicedMemoryPercentage and acceleratorSlicedCoresPercentage"))
	}

	manufacturer := instType.Status.Detail.Manufacturer
	if nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModePartitioned) == "" {
		errs = append(errs, field.Invalid(profilePath, profile,
			fmt.Sprintf("manufacturer %s does not support hardware partitioning", manufacturer)))
		return errs
	}

	// The manufacturer can partition, but this pool's cards may not be in a partitioning mode.
	// Reject on the missing capability before the profile lookup, which would otherwise report an
	// empty offered set and read as a mistyped profile.
	if !instType.Status.Detail.IsPhysicallySliceable() {
		errs = append(errs, field.Forbidden(profilePath,
			fmt.Sprintf("instance type %s does not offer hardware partitioning", instType.Name)))
		return errs
	}

	offered := make([]string, 0, len(instType.Status.Detail.SlicedDetail.Physical.Profiles))
	var found bool
	for _, p := range instType.Status.Detail.SlicedDetail.Physical.Profiles {
		offered = append(offered, p.Name)
		if p.Name == profile {
			found = true
		}
	}
	switch {
	case !found:
		errs = append(errs, field.Invalid(profilePath, profile,
			fmt.Sprintf("instance type %s does not offer this partition profile; offered: %v",
				instType.Name, offered)))
	case nodefeature.GetAcceleratablePartitionedProfileResourceName(manufacturer, profile) == "":
		// An offered profile whose name is not a valid resource-name segment cannot be
		// requested: the key it would produce is not addressable.
		errs = append(errs, field.Invalid(profilePath, profile,
			"partition profile name does not yield a valid resource name"))
	}

	// A partition is one instance on one card (rule 3), like a logical slice.
	errs = append(errs, validateSingleCardRequest(instRess, "partitioned")...)

	return errs
}

// validateExclusiveAcceleratorRequest checks a whole-card (exclusive) accelerator request: it may
// not be negative, must be a whole number of cards, and may not exceed the InstanceType's
// whole-card OnceMaxRequest — which is zero for a pool with no free unpartitioned card, so an
// all-partitioned pool rejects a positive claim here. A nil request is left to defaulting. It is
// shared by a non-sliceable type and by a zero-percentage (whole-card) request on a sliceable type.
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
