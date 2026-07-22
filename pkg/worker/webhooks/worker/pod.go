package worker

import (
	"context"
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/webhook"
)

// PodWebhook hooks core Pods routed to a GPUStack queue (selected by the
// "kueue.x-k8s.io/queue-name" label) and normalizes each container's sliced
// accelerator request. Mutating: the per-card compute budget
// (.sliced.cores-percentage) defaults to a whole card (100), and the per-card
// VRAM budget (.sliced.memory-percentage or .sliced.memory-mib) is always folded
// into the credit-counting .sliced.units so Kueue and the device-plugin agree —
// any client-supplied .sliced.units is ignored and recomputed, since the value is
// webhook-derived only. Validating: a .sliced request that omits the card count,
// names no memory, sets both memory keys, carries a non-positive budget, or sets a
// percentage budget above 100 is rejected; a Pod mixing accelerator allocation
// modes (exclusive/shared/sliced) is also rejected.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="",version="v1",resource="pods",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:validating:objectSelector={"matchExpressions":[{"key":"kueue.x-k8s.io/queue-name","operator":"Exists"}]}
// +k8s:webhook-gen:validating:namePrefix="gpustack-worker"
// +k8s:webhook-gen:mutating:group="",version="v1",resource="pods",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:objectSelector={"matchExpressions":[{"key":"kueue.x-k8s.io/queue-name","operator":"Exists"}]}
// +k8s:webhook-gen:mutating:namePrefix="gpustack-worker"
type PodWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *PodWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &core.Pod{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*PodWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*PodWebhook)(nil)
)

// slicedResourceNames is the per-manufacturer family of sliced resource names a
// container may request.
type slicedResourceNames struct {
	card     core.ResourceName // <base>.sliced
	units    core.ResourceName // <base>.sliced.units
	coresPct core.ResourceName // <base>.sliced.cores-percentage
	memPct   core.ResourceName // <base>.sliced.memory-percentage
	memMib   core.ResourceName // <base>.sliced.memory-mib
}

// slicedBases scans a container's Requests and Limits once and returns the
// sorted, unique accelerator bases it requests a slice of — the "<base>" of any
// "<base>.sliced" family resource ("<base>.sliced" itself and every sub-key such
// as ".sliced.units" or ".sliced.memory-percentage"). Matching the whole family
// (not just the bare card key) ensures a request naming only a sub-key is still
// validated and folded, never slipping through unchecked.
func slicedBases(ctr *core.Container) []core.ResourceName {
	bases := sets.New[core.ResourceName]()
	for _, rl := range []core.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
		for name := range rl {
			if base, _, ok := strings.Cut(string(name), nodefeature.SlicedResourceNameSuffix); ok &&
				nodefeature.IsKnownAcceleratableResourceName(core.ResourceName(base)) {
				bases.Insert(core.ResourceName(base))
			}
		}
	}
	return sets.List(bases)
}

// slicedResourceNamesForBase builds the sliced resource family for an accelerator
// base (e.g. "nvidia.com/gpu").
func slicedResourceNamesForBase(base core.ResourceName) slicedResourceNames {
	return slicedResourceNames{
		card:     base + core.ResourceName(nodefeature.SlicedResourceNameSuffix),
		units:    base + core.ResourceName(nodefeature.SlicedUnitsResourceNameSuffix),
		coresPct: base + core.ResourceName(nodefeature.SlicedCoresPercentageResourceNameSuffix),
		memPct:   base + core.ResourceName(nodefeature.SlicedMemoryPercentageResourceNameSuffix),
		memMib:   base + core.ResourceName(nodefeature.SlicedMemoryMibResourceNameSuffix),
	}
}

// physicalSlicedProfileOf returns the MIG profile name a resource name encodes — the "<profile>"
// of a "<base>.sliced.mig-<profile>" physical-slice key of a known accelerator base —
// or "" when the name is not such a key.
func physicalSlicedProfileOf(name core.ResourceName) string {
	s := string(name)
	i := strings.Index(s, nodefeature.SlicedMigResourceNameInfix)
	if i < 0 {
		return ""
	}
	if !nodefeature.IsKnownAcceleratableResourceName(core.ResourceName(s[:i])) {
		return ""
	}
	return s[i+len(nodefeature.SlicedMigResourceNameInfix):]
}

// physicalSlicedRequest reports whether the container requests a physical-slice (MIG) profile for
// the given accelerator base, returning the profile name and its requested quantity
// (preferring Requests over Limits). A container carries at most one distinct profile
// (a Pod carries at most one, enforced in ValidateCreate).
func physicalSlicedRequest(ctr *core.Container, base core.ResourceName) (string, resource.Quantity, bool) {
	prefix := string(base) + nodefeature.SlicedMigResourceNameInfix
	for _, rl := range []core.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
		for name, q := range rl {
			if profile, ok := strings.CutPrefix(string(name), prefix); ok && profile != "" {
				return profile, q, true
			}
		}
	}
	return "", resource.Quantity{}, false
}

// podSlicedContainer pairs a container with its field path and whether it is an init
// container.
type podSlicedContainer struct {
	ctr  *core.Container
	init bool
	path *field.Path
}

// podSlicedContainers returns every init and app container of the Pod, each with its
// field path. MIG requests are validated and folded in both (getAllocatingPod
// attributes across init and app containers); the existing soft-slice folding stays
// scoped to app Containers, so its behavior is unchanged.
func podSlicedContainers(pod *core.Pod) []podSlicedContainer {
	out := make([]podSlicedContainer, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for i := range pod.Spec.InitContainers {
		out = append(out, podSlicedContainer{
			ctr: &pod.Spec.InitContainers[i], init: true,
			path: field.NewPath("spec", "initContainers").Index(i),
		})
	}
	for i := range pod.Spec.Containers {
		out = append(out, podSlicedContainer{
			ctr: &pod.Spec.Containers[i], init: false,
			path: field.NewPath("spec", "containers").Index(i),
		})
	}
	return out
}

// podPhysicalSlicedProfiles returns the set of distinct MIG profile names the Pod requests across
// all its init and app containers.
func podPhysicalSlicedProfiles(pod *core.Pod) sets.Set[string] {
	profiles := sets.New[string]()
	for _, sc := range podSlicedContainers(pod) {
		for _, rl := range []core.ResourceList{sc.ctr.Resources.Requests, sc.ctr.Resources.Limits} {
			for name := range rl {
				if p := physicalSlicedProfileOf(name); p != "" {
					profiles.Insert(p)
				}
			}
		}
	}
	return profiles
}

// acceleratorMode classifies a resource name into the allocation mode it requests
// — "exclusive" (<base>), "shared" (<base>.shared) or "sliced" (<base>.sliced and
// its sub-keys) — or "" when it is not a known accelerator resource.
func acceleratorMode(name core.ResourceName) string {
	s := string(name)
	if base, _, ok := strings.Cut(s, nodefeature.SlicedResourceNameSuffix); ok &&
		nodefeature.IsKnownAcceleratableResourceName(core.ResourceName(base)) {
		return "sliced"
	}
	if base, ok := strings.CutSuffix(s, nodefeature.SharedResourceNameSuffix); ok &&
		nodefeature.IsKnownAcceleratableResourceName(core.ResourceName(base)) {
		return "shared"
	}
	if nodefeature.IsKnownAcceleratableResourceName(name) {
		return "exclusive"
	}
	return ""
}

// podAcceleratorModes returns the set of allocation modes the Pod's containers
// request across both Requests and Limits. Init containers are scanned as well as app
// containers (matching the MIG-profile scan), so a cross-mode conflict that involves an
// init-container request — e.g. MIG in an init container and exclusive/shared in an app
// container — is still caught by the one-mode-per-Pod check.
func podAcceleratorModes(pod *core.Pod) sets.Set[string] {
	modes := sets.New[string]()
	containers := make([]*core.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for ci := range pod.Spec.InitContainers {
		containers = append(containers, &pod.Spec.InitContainers[ci])
	}
	for ci := range pod.Spec.Containers {
		containers = append(containers, &pod.Spec.Containers[ci])
	}
	for _, ctr := range containers {
		for _, rl := range []core.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
			for name := range rl {
				if m := acceleratorMode(name); m != "" {
					modes.Insert(m)
				}
			}
		}
	}
	return modes
}

// containerResource returns the requested quantity for name, preferring Requests
// and falling back to Limits, plus whether it was present at all.
func containerResource(ctr *core.Container, name core.ResourceName) (resource.Quantity, bool) {
	if q, ok := ctr.Resources.Requests[name]; ok {
		return q, true
	}
	if q, ok := ctr.Resources.Limits[name]; ok {
		return q, true
	}
	return resource.Quantity{}, false
}

// setContainerResource sets both Requests and Limits, which an extended resource
// requires to be equal.
func setContainerResource(ctr *core.Container, name core.ResourceName, q resource.Quantity) {
	if ctr.Resources.Requests == nil {
		ctr.Resources.Requests = core.ResourceList{}
	}
	if ctr.Resources.Limits == nil {
		ctr.Resources.Limits = core.ResourceList{}
	}
	ctr.Resources.Requests[name] = q
	ctr.Resources.Limits[name] = q
}

func (r *PodWebhook) Default(ctx context.Context, obj runtime.Object) error {
	pod := obj.(*core.Pod)

	// The per-card VRAM is looked up lazily, only when a container folds an
	// absolute .sliced.memory-mib request; -1 means "not looked up yet".
	cardVRAMMib := int64(-1)

	for _, sc := range podSlicedContainers(pod) {
		ctr := sc.ctr
		for _, base := range slicedBases(ctr) {
			names := slicedResourceNamesForBase(base)

			// A physical (MIG) request folds its per-card credit from the profile's VRAM
			// (the same fold as .sliced.memory-mib) and takes none of the logical budget
			// keys. Handled for init and app containers alike.
			if profile, _, ok := physicalSlicedRequest(ctr, base); ok {
				units, err := r.physicalSlicedContainerUnits(ctx, pod, ctr, names, profile)
				if err != nil {
					return err
				}
				setContainerResource(ctr, names.units, *resource.NewQuantity(units, resource.DecimalSI))
				continue
			}

			// Logical (soft) slicing is folded on app containers only (unchanged behavior;
			// the init-container soft-slice gap is a pre-existing one this spec leaves as is).
			if sc.init {
				continue
			}

			// Default the per-card compute budget to a whole card.
			if _, ok := containerResource(ctr, names.coresPct); !ok {
				setContainerResource(ctr, names.coresPct, *resource.NewQuantity(100, resource.DecimalSI))
			}

			// .sliced.units is webhook-derived only: always recompute it from the
			// memory budget, overwriting any client-supplied value (no trusted path
			// sets it directly). The inputs are bounded here so the fold cannot
			// overflow int64.
			memPctQ, hasMemPct := containerResource(ctr, names.memPct)
			memMibQ, hasMemMib := containerResource(ctr, names.memMib)
			var units int64
			switch {
			case hasMemPct && memPctQ.Value() > 0:
				// memory-percentage wins over memory-mib; D/100 per percent.
				if memPctQ.Value() > 100 {
					return fmt.Errorf("container %q: %s must be within (0, 100]", ctr.Name, names.memPct)
				}
				units = memPctQ.Value() * nodefeature.ResourceMaxUnits / 100
			case hasMemMib && memMibQ.Value() > 0:
				if cardVRAMMib < 0 {
					var err error
					cardVRAMMib, err = r.cardVRAMMib(ctx, pod)
					if err != nil {
						return err
					}
				}
				if memMibQ.Value() > cardVRAMMib {
					return fmt.Errorf("container %q: %s must not exceed the per-card VRAM (%d MiB)",
						ctr.Name, names.memMib, cardVRAMMib)
				}
				units = nodefeature.MemoryMibToUnits(memMibQ.Value(), cardVRAMMib)
			default:
				continue // no usable memory request; validation rejects it
			}
			if units <= 0 {
				return fmt.Errorf("container %q: cannot derive a positive %s from the requested memory",
					ctr.Name, names.units)
			}
			setContainerResource(ctr, names.units, *resource.NewQuantity(units, resource.DecimalSI))
		}
	}

	return nil
}

// frontingInstanceType reverse-looks-up the operator-owned InstanceType that fronts the Pod's
// LocalQueue by the queue-entrance label the Default webhook stamps with the LocalQueue name, so
// the authoritative per-card VRAM and MIG profile inventory are never taken from the user-writable,
// namespaced LocalQueue. It falls back to a non-cached read when the controller cache is not yet
// warm, and errors when no (or more than one) InstanceType matches.
func (r *PodWebhook) frontingInstanceType(ctx context.Context, pod *core.Pod) (*workercore.InstanceType, error) {
	lqName := pod.Labels[kueuectrlconst.QueueLabel]
	if lqName == "" {
		return nil, fmt.Errorf("pod has no %q label", kueuectrlconst.QueueLabel)
	}

	itList := new(workercore.InstanceTypeList)
	sel := ctrlcli.MatchingLabels{QueueEntranceLabelKey: lqName}
	err := r.Client.List(ctx, itList, sel)
	if err != nil || len(itList.Items) == 0 {
		// The controller cache may not be warm yet; fall back to a direct read.
		if err = r.APIReader.List(ctx, itList, sel, ctrlclix.WithoutQuorum); err != nil {
			return nil, fmt.Errorf("list instance types fronting local queue %s: %w", lqName, err)
		}
	}
	switch len(itList.Items) {
	case 0:
		return nil, fmt.Errorf("no instance type fronts local queue %s", lqName)
	case 1:
	default:
		return nil, fmt.Errorf("more than one instance type fronts local queue %s", lqName)
	}
	return &itList.Items[0], nil
}

// cardVRAMMib reads the per-card VRAM (MiB) from the observed Status.Detail of the InstanceType
// fronting the Pod's LocalQueue. It errors when the Detail's Memory is missing (detail not yet
// computed) or unparseable, so an unfoldable memory-mib request is rejected rather than silently
// mis-sized.
func (r *PodWebhook) cardVRAMMib(ctx context.Context, pod *core.Pod) (int64, error) {
	it, err := r.frontingInstanceType(ctx, pod)
	if err != nil {
		return 0, err
	}
	return instanceTypeCardVRAMMib(it)
}

// instanceTypeCardVRAMMib parses the per-card VRAM (MiB) from an InstanceType's observed
// Status.Detail.Memory. An empty value is the not-yet-ready state (reject as retryable rather than
// mis-size); a non-positive or unparseable value is a hard error.
func instanceTypeCardVRAMMib(it *workercore.InstanceType) (int64, error) {
	memStr := it.Status.Detail.Memory
	if memStr == "" {
		return 0, fmt.Errorf("instance type %s has no per-card memory yet (detail not ready)", it.Name)
	}
	q, err := resource.ParseQuantity(memStr)
	if err != nil {
		return 0, fmt.Errorf("parse memory %q of instance type %s: %w", memStr, it.Name, err)
	}
	mib := q.Value() / quantityx.Mi
	if mib <= 0 {
		return 0, fmt.Errorf("instance type %s has non-positive memory %q", it.Name, memStr)
	}
	return mib, nil
}

// physicalProfile finds a MIG profile in the fronting InstanceType's observed physical-slice
// inventory (Status.Detail), returning the aggregate (its per-instance MemoryMib and pool-wide
// instance ceiling Count), whether the accelerator Detail has been computed at all, and whether the
// profile was found.
func physicalProfile(
	it *workercore.InstanceType, profile string,
) (prof workercore.AcceleratorSlicedPhysicalDetailProfile, ready, found bool) {
	ready = it.Status.Detail.AcceleratorReady()
	for _, p := range it.Status.Detail.SlicedDetail.Physical.Profiles {
		if p.Name == profile {
			return p, ready, true
		}
	}
	return workercore.AcceleratorSlicedPhysicalDetailProfile{}, ready, false
}

// physicalSlicedContainerUnits validates a container's MIG profile request against the fronting InstanceType's
// observed inventory and returns the .sliced.units it folds to. A not-yet-computed Detail is a
// retryable rejection; a profile the pool does not offer, or a card count above the pool's
// per-profile instance ceiling, is a permanent rejection. The fold is
// MemoryMibToUnits(profile.MemoryMib, cardVRAM) — the same VRAM-anchored fold the soft
// .sliced.memory-mib path uses, so a MIG instance and a soft slice of the same VRAM charge
// identical credits.
func (r *PodWebhook) physicalSlicedContainerUnits(
	ctx context.Context, pod *core.Pod, ctr *core.Container, names slicedResourceNames, profile string,
) (int64, error) {
	it, err := r.frontingInstanceType(ctx, pod)
	if err != nil {
		return 0, err
	}
	prof, ready, found := physicalProfile(it, profile)
	if !found {
		if !ready {
			return 0, fmt.Errorf("instance type %s is not ready yet (accelerator detail not computed); retry", it.Name)
		}
		return 0, fmt.Errorf("container %q: MIG profile %q is not offered by the target pool (instance type %s)",
			ctr.Name, profile, it.Name)
	}
	// A profile can appear in the inventory before its per-instance MemoryMib is populated (partial
	// detail during detection / rollout skew). Treat a missing memory as not-ready/retryable rather
	// than folding to zero units and rejecting permanently.
	if prof.MemoryMib <= 0 {
		return 0, fmt.Errorf("instance type %s is not ready yet (MIG profile %q has no memory detail); retry",
			it.Name, profile)
	}
	if cardQ, ok := containerResource(ctr, names.card); ok && prof.Count > 0 && cardQ.Value() > int64(prof.Count) {
		return 0, fmt.Errorf("container %q: %d cards of MIG profile %q exceed the pool ceiling of %d instance(s)",
			ctr.Name, cardQ.Value(), profile, prof.Count)
	}
	cardVRAMMib, err := instanceTypeCardVRAMMib(it)
	if err != nil {
		return 0, err
	}
	units := nodefeature.MemoryMibToUnits(prof.MemoryMib, cardVRAMMib)
	if units <= 0 {
		return 0, fmt.Errorf("container %q: cannot derive a positive %s from MIG profile %q",
			ctr.Name, names.units, profile)
	}
	return units, nil
}

func (r *PodWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	pod := obj.(*core.Pod)

	var errs field.ErrorList

	// A Pod may request only one accelerator allocation mode; exclusive, shared and
	// sliced are mutually exclusive within a Pod.
	if modes := podAcceleratorModes(pod); modes.Len() > 1 {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "containers"),
			fmt.Sprintf("a Pod may request only one accelerator allocation mode, found %v", sets.List(modes))))
	}

	// A physical (MIG) request anchors on the profile name; the device-plugin's Allocate
	// attribution models one profile per Pod, so a Pod naming more than one distinct
	// profile is unattributable and rejected at ingress.
	if profiles := podPhysicalSlicedProfiles(pod); profiles.Len() > 1 {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "containers"),
			fmt.Sprintf("a Pod may request only one MIG profile, found %v", sets.List(profiles))))
	}

	for _, sc := range podSlicedContainers(pod) {
		ctr := sc.ctr
		// Anchor field errors under resources.limits: extended resources (GPU / MIG keys) are
		// conventionally specified there, so this matches the user's manifest even when only
		// limits is set (requests is defaulted equal).
		limPath := sc.path.Child("resources", "limits")
		for _, base := range slicedBases(ctr) {
			names := slicedResourceNamesForBase(base)
			if profile, q, ok := physicalSlicedRequest(ctr, base); ok {
				errs = append(errs, validatePhysicalSlicedContainer(ctr, names, base, profile, q, limPath)...)
			} else {
				errs = append(errs, validateLogicalSlicedContainer(ctr, names, limPath)...)
			}
		}
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(core.SchemeGroupVersion.WithKind("Pod").GroupKind(), pod.Name, errs)
	}

	return nil, nil
}

func (r *PodWebhook) ValidateUpdate(_ context.Context, _, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

func (r *PodWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// validatePhysicalSlicedContainer checks one container's physical-slice (MIG) request: it must set the
// card count it slices, its value must be exactly 1 (one instance per card — request more cards
// for more instances), and it is mutually exclusive with the three logical (soft) slice keys on
// that container. The profile-existence and count-vs-ceiling checks (which need the InstanceType
// Detail) are enforced in the Default webhook alongside the units fold.
func validatePhysicalSlicedContainer(
	ctr *core.Container, names slicedResourceNames, base core.ResourceName,
	profile string, q resource.Quantity, path *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	physicalKey := string(base) + nodefeature.SlicedMigResourceNameInfix + profile

	if _, ok := containerResource(ctr, names.card); !ok {
		errs = append(errs, field.Required(path.Key(string(names.card)),
			fmt.Sprintf("a %s request must set %s", physicalKey, names.card)))
	}
	// Compare with Cmp (not Value(), which rounds a fractional quantity like "1m" up to 1)
	// so only a true 1 passes.
	one := resource.NewQuantity(1, resource.DecimalSI)
	if q.Cmp(*one) != 0 {
		errs = append(errs, field.Invalid(path.Key(physicalKey), q.String(),
			"a MIG profile request must be exactly 1"))
	}
	// Physical (MIG) and logical (soft) slicing are mutually exclusive on one container.
	for _, ln := range []core.ResourceName{names.coresPct, names.memPct, names.memMib} {
		if _, ok := containerResource(ctr, ln); ok {
			errs = append(errs, field.Forbidden(path.Key(string(ln)),
				fmt.Sprintf("cannot combine %s with the logical slice key %s", physicalKey, ln)))
		}
	}
	return errs
}

// validateLogicalSlicedContainer checks one container's sliced request for the given
// manufacturer family: it must name exactly one memory budget (percentage or mib),
// every budget it sets must be positive, and the percentage budgets must not exceed
// 100. The compute and memory budgets are independent.
func validateLogicalSlicedContainer(ctr *core.Container, names slicedResourceNames, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	memPctQ, hasMemPct := containerResource(ctr, names.memPct)
	memMibQ, hasMemMib := containerResource(ctr, names.memMib)
	coresPctQ, hasCoresPct := containerResource(ctr, names.coresPct)

	// A sliced sub-resource is meaningless without the card count it slices; a bare
	// sub-key alone would otherwise fold to credits with no card request behind it.
	if _, ok := containerResource(ctr, names.card); !ok {
		errs = append(errs, field.Required(path.Key(string(names.card)),
			fmt.Sprintf("a sliced request must set %s", names.card)))
	}

	switch {
	case !hasMemPct && !hasMemMib:
		errs = append(errs, field.Required(path.Key(string(names.memPct)),
			fmt.Sprintf("a %s request must set %s or %s", names.card, names.memPct, names.memMib)))
	case hasMemPct && hasMemMib:
		errs = append(errs, field.Forbidden(path.Key(string(names.memMib)),
			fmt.Sprintf("cannot set both %s and %s", names.memPct, names.memMib)))
	}

	if hasMemPct && (memPctQ.Value() <= 0 || memPctQ.Value() > 100) {
		errs = append(errs, field.Invalid(path.Key(string(names.memPct)), memPctQ.String(),
			"must be within (0, 100]"))
	}
	if hasMemMib && memMibQ.Value() <= 0 {
		errs = append(errs, field.Invalid(path.Key(string(names.memMib)), memMibQ.String(),
			"must be greater than zero"))
	}
	if hasCoresPct && (coresPctQ.Value() <= 0 || coresPctQ.Value() > 100) {
		errs = append(errs, field.Invalid(path.Key(string(names.coresPct)), coresPctQ.String(),
			"must be within (0, 100]"))
	}

	return errs
}
