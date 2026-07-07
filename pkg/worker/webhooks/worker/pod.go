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
// names no memory, sets both memory keys, carries a non-positive or over-100
// budget, or whose compute budget is smaller than its percentage memory budget is
// rejected.
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
// request across both Requests and Limits.
func podAcceleratorModes(pod *core.Pod) sets.Set[string] {
	modes := sets.New[string]()
	for ci := range pod.Spec.Containers {
		ctr := &pod.Spec.Containers[ci]
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

	for ci := range pod.Spec.Containers {
		ctr := &pod.Spec.Containers[ci]
		for _, base := range slicedBases(ctr) {
			names := slicedResourceNamesForBase(base)

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

// cardVRAMMib reads the per-card VRAM (MiB) from the operator-owned InstanceType that fronts the
// Pod's LocalQueue. The InstanceType is reverse-looked-up by the queue-entrance label the Default
// webhook stamps with the LocalQueue name, so the authoritative VRAM is never taken from the
// user-writable, namespaced LocalQueue. It falls back to a non-cached read when the controller
// cache is not yet warm, and errors when no (or more than one) InstanceType matches, or its
// spec.memory is missing or unparseable, so an unfoldable memory-mib request is rejected.
func (r *PodWebhook) cardVRAMMib(ctx context.Context, pod *core.Pod) (int64, error) {
	lqName := pod.Labels[kueuectrlconst.QueueLabel]
	if lqName == "" {
		return 0, fmt.Errorf("pod has no %q label", kueuectrlconst.QueueLabel)
	}

	itList := new(workercore.InstanceTypeList)
	sel := ctrlcli.MatchingLabels{QueueEntranceLabelKey: lqName}
	err := r.Client.List(ctx, itList, sel)
	if err != nil || len(itList.Items) == 0 {
		// The controller cache may not be warm yet; fall back to a direct read.
		if err = r.APIReader.List(ctx, itList, sel, ctrlclix.WithoutQuorum); err != nil {
			return 0, fmt.Errorf("list instance types fronting local queue %s: %w", lqName, err)
		}
	}
	switch len(itList.Items) {
	case 0:
		return 0, fmt.Errorf("no instance type fronts local queue %s", lqName)
	case 1:
	default:
		return 0, fmt.Errorf("more than one instance type fronts local queue %s", lqName)
	}
	it := &itList.Items[0]

	memStr := it.Spec.Memory
	if memStr == "" {
		return 0, fmt.Errorf("instance type %s has no memory", it.Name)
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

func (r *PodWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	pod := obj.(*core.Pod)

	var errs field.ErrorList

	// A Pod may request only one accelerator allocation mode; exclusive, shared and
	// sliced are mutually exclusive within a Pod.
	if modes := podAcceleratorModes(pod); modes.Len() > 1 {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "containers"),
			fmt.Sprintf("a Pod may request only one accelerator allocation mode, found %v", sets.List(modes))))
	}

	for ci := range pod.Spec.Containers {
		ctr := &pod.Spec.Containers[ci]
		for _, base := range slicedBases(ctr) {
			names := slicedResourceNamesForBase(base)
			errs = append(errs, validateSlicedContainer(ctr, names, ci)...)
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

// validateSlicedContainer checks one container's sliced request for the given
// manufacturer family: it must name exactly one positive memory budget, carry no
// non-positive sliced budget, and keep the compute budget at least as large as
// the percentage memory budget.
func validateSlicedContainer(ctr *core.Container, names slicedResourceNames, idx int) field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec", "containers").Index(idx).Child("resources", "requests")

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

	if hasMemPct && memPctQ.Value() > 0 && hasCoresPct && coresPctQ.Value() < memPctQ.Value() {
		errs = append(errs, field.Invalid(path.Key(string(names.coresPct)), coresPctQ.String(),
			fmt.Sprintf("must not be less than %s", names.memPct)))
	}

	return errs
}
