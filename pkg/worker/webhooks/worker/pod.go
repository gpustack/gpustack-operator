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
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// PodWebhook hooks core Pods routed to a GPUStack queue (selected by the
// "kueue.x-k8s.io/queue-name" label) and normalizes each container's accelerator
// request. Mutating: a logical slice's per-card compute budget
// (.sliced.cores-percentage) defaults to a whole card (100) and its per-card VRAM
// budget (.sliced.memory-percentage or .sliced.memory-mib) is folded into the
// credit-counting .sliced.units, while a partition request folds the profile's VRAM
// into .partitioned.units — any client-supplied units value is ignored and recomputed,
// since it is webhook-derived only. Validating: it enforces the normative accelerator
// request rules (see validatePodAcceleratorRequest) plus the per-family shape checks.
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

// containerGroup is the lifetime group a container belongs to. The request rules are
// scoped by lifetime, not by the field a container sits in, because two accelerator
// claims in different groups are not free of each other: the kubelet keeps a finished
// init container's devices in its Pod record for the Pod's whole life, while the
// scheduler charges the Pod only max(Σ init, Σ app) of each key — so two claims cost one
// unit of quota and consume two.
type containerGroup string

const (
	// containerGroupInit is spec.initContainers without restartPolicy: Always.
	containerGroupInit containerGroup = "initContainers"
	// containerGroupRunning is spec.containers.
	containerGroupRunning containerGroup = "containers"
	// containerGroupSidecar is a restartable init container (a native sidecar). It belongs
	// to neither group — it starts during the init phase and keeps running, so it overlaps
	// every later init container as well as every app container — and may not claim an
	// accelerator at all.
	containerGroupSidecar containerGroup = "sidecar"
)

// logicalResourceNames is the per-base family of logical (software) slicing resource
// names a container may request.
type logicalResourceNames struct {
	card     core.ResourceName // <base>.sliced
	units    core.ResourceName // <base>.sliced.units
	coresPct core.ResourceName // <base>.sliced.cores-percentage
	memPct   core.ResourceName // <base>.sliced.memory-percentage
	memMib   core.ResourceName // <base>.sliced.memory-mib
}

// logicalResourceNamesForBase builds the logical slicing resource family for an
// accelerator base (e.g. "nvidia.com/gpu").
func logicalResourceNamesForBase(base core.ResourceName) logicalResourceNames {
	return logicalResourceNames{
		card:     base + core.ResourceName(nodefeature.SlicedResourceNameSuffix),
		units:    base + core.ResourceName(nodefeature.SlicedUnitsResourceNameSuffix),
		coresPct: base + core.ResourceName(nodefeature.SlicedCoresPercentageResourceNameSuffix),
		memPct:   base + core.ResourceName(nodefeature.SlicedMemoryPercentageResourceNameSuffix),
		memMib:   base + core.ResourceName(nodefeature.SlicedMemoryMibResourceNameSuffix),
	}
}

// partitionResourceNames is the per-base family of physical (hardware) partitioning
// resource names a container may request. The per-profile key is not fixed per base —
// it carries the manufacturer's partition kind and the profile name — so it is resolved
// from the container's request instead of built here.
type partitionResourceNames struct {
	card  core.ResourceName // <base>.partitioned
	units core.ResourceName // <base>.partitioned.units
}

// partitionResourceNamesForBase builds the physical partitioning resource family for an
// accelerator base (e.g. "nvidia.com/gpu").
func partitionResourceNamesForBase(base core.ResourceName) partitionResourceNames {
	return partitionResourceNames{
		card:  base + core.ResourceName(nodefeature.PartitionedResourceNameSuffix),
		units: base + core.ResourceName(nodefeature.PartitionedUnitsResourceNameSuffix),
	}
}

// acceleratorBaseOf returns the accelerator base a resource name is a key of, together
// with the family it belongs to. It reports false for anything outside the four
// accelerator families, including the visibility resource, which is deliberately outside
// them so the one-family rules ignore it.
func acceleratorBaseOf(name core.ResourceName) (core.ResourceName, nodefeature.ResourceFamily, bool) {
	family := nodefeature.ResourceFamilyOf(name)
	s := string(name)
	switch family {
	case nodefeature.ResourceFamilyExclusive:
		return name, family, true
	case nodefeature.ResourceFamilyShared:
		base, _ := strings.CutSuffix(s, nodefeature.SharedResourceNameSuffix)
		return core.ResourceName(base), family, true
	case nodefeature.ResourceFamilySliced:
		base, _, _ := strings.Cut(s, nodefeature.SlicedResourceNameSuffix)
		return core.ResourceName(base), family, true
	case nodefeature.ResourceFamilyPartitioned:
		base, _, _ := strings.Cut(s, nodefeature.PartitionedResourceNameSuffix)
		return core.ResourceName(base), family, true
	}
	return "", nodefeature.ResourceFamilyNone, false
}

// containerClaims scans a container's Requests and Limits once and returns the sorted,
// unique accelerator bases it claims, grouped by family. Every key of a family counts —
// the card key itself and every sub-key such as ".sliced.units" or the per-profile
// partition key — so a request naming only a sub-key is still validated and folded,
// never slipping through unchecked.
func containerClaims(ctr *core.Container) map[nodefeature.ResourceFamily][]core.ResourceName {
	bases := make(map[nodefeature.ResourceFamily]sets.Set[core.ResourceName])
	for _, rl := range []core.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
		for name := range rl {
			base, family, ok := acceleratorBaseOf(name)
			if !ok {
				continue
			}
			if bases[family] == nil {
				bases[family] = sets.New[core.ResourceName]()
			}
			bases[family].Insert(base)
		}
	}
	claims := make(map[nodefeature.ResourceFamily][]core.ResourceName, len(bases))
	for family, set := range bases {
		claims[family] = sets.List(set)
	}
	return claims
}

// partitionRequest is one per-profile partition key a container sets, with the profile it
// names and the quantity it asks for.
type partitionRequest struct {
	name     core.ResourceName
	profile  string
	quantity resource.Quantity
}

// containerPartitionRequests returns the per-profile partition keys a container sets for an
// accelerator base, sorted by profile name. The key embeds the manufacturer's partition kind,
// so it is read back off the container rather than rebuilt from a base the webhook cannot map
// to a manufacturer. Requests win over Limits, as everywhere else in this webhook.
func containerPartitionRequests(ctr *core.Container, base core.ResourceName) []partitionRequest {
	byProfile := make(map[string]partitionRequest)
	for _, rl := range []core.ResourceList{ctr.Resources.Limits, ctr.Resources.Requests} {
		for name, q := range rl {
			profile, ok := nodefeature.PartitionedProfileOf(name)
			if !ok {
				continue
			}
			if b, _, _ := acceleratorBaseOf(name); b == base {
				byProfile[profile] = partitionRequest{name: name, profile: profile, quantity: q}
			}
		}
	}
	out := make([]partitionRequest, 0, len(byProfile))
	for _, profile := range sets.List(sets.KeySet(byProfile)) {
		out = append(out, byProfile[profile])
	}
	return out
}

// containerPartitionProfiles returns the sorted, unique partition profile names a container
// requests for an accelerator base.
func containerPartitionProfiles(ctr *core.Container, base core.ResourceName) []string {
	reqs := containerPartitionRequests(ctr, base)
	profiles := make([]string, 0, len(reqs))
	for i := range reqs {
		profiles = append(profiles, reqs[i].profile)
	}
	return profiles
}

// podContainer pairs a container with its lifetime group and its field path.
type podContainer struct {
	ctr   *core.Container
	group containerGroup
	path  *field.Path
}

// podContainers returns every init and app container of the Pod with its lifetime group
// and field path, in manifest order.
func podContainers(pod *core.Pod) []podContainer {
	out := make([]podContainer, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for i := range pod.Spec.InitContainers {
		ctr := &pod.Spec.InitContainers[i]
		group := containerGroupInit
		if ctr.RestartPolicy != nil && *ctr.RestartPolicy == core.ContainerRestartPolicyAlways {
			group = containerGroupSidecar
		}
		out = append(out, podContainer{
			ctr: ctr, group: group,
			path: field.NewPath("spec", "initContainers").Index(i),
		})
	}
	for i := range pod.Spec.Containers {
		out = append(out, podContainer{
			ctr: &pod.Spec.Containers[i], group: containerGroupRunning,
			path: field.NewPath("spec", "containers").Index(i),
		})
	}
	return out
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

	// The fold covers whichever container group holds the claims: the request rules
	// confine them to one group, so folding every claiming container folds exactly it.
	for _, pc := range podContainers(pod) {
		ctr := pc.ctr
		claims := containerClaims(ctr)

		// A partition request folds its per-card credit from the profile's VRAM (the same
		// VRAM-anchored fold the absolute logical memory request uses) and takes none of
		// the logical budget keys.
		for _, base := range claims[nodefeature.ResourceFamilyPartitioned] {
			profiles := containerPartitionProfiles(ctr, base)
			if len(profiles) != 1 {
				// Validation rejects a partition claim that names no profile, or more than
				// one; there is nothing to fold either way.
				continue
			}
			units, err := r.partitionContainerUnits(ctx, pod, ctr, base, profiles[0])
			if err != nil {
				return err
			}
			names := partitionResourceNamesForBase(base)
			setContainerResource(ctr, names.units, *resource.NewQuantity(units, resource.DecimalSI))
		}

		for _, base := range claims[nodefeature.ResourceFamilySliced] {
			names := logicalResourceNamesForBase(base)

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
// the authoritative per-card VRAM and partition profile inventory are never taken from the
// user-writable, namespaced LocalQueue. It falls back to a non-cached read when the controller
// cache is not yet warm, and errors when no (or more than one) InstanceType matches.
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
	return workerctrl.InstanceTypeCardVRAMMib(it)
}

// partitionContainerUnits validates a container's partition profile request against the fronting
// InstanceType's observed inventory and returns the .partitioned.units it folds to. A
// not-yet-computed Detail is a retryable rejection; a profile the pool does not offer, or a card
// count above the pool's per-profile instance ceiling, is a permanent rejection.
//
// The fold is VRAM-anchored: a profile charges the share of one whole card its own per-instance
// memory occupies — the profile's memory over the card's, as a percentage, times the units one
// percent of a card is worth. On a 96 GiB card a profile of 48 GiB therefore folds to
// 50 x 16000 = 800000 units, and one of 96 GiB to 100 x 16000 = 1600000, the full per-card
// basis. It is the same fold the logical .sliced.memory-mib path uses, so a partition instance
// and a logical slice of the same VRAM charge identical credits.
func (r *PodWebhook) partitionContainerUnits(
	ctx context.Context, pod *core.Pod, ctr *core.Container, base core.ResourceName, profile string,
) (int64, error) {
	it, err := r.frontingInstanceType(ctx, pod)
	if err != nil {
		return 0, err
	}
	prof, ready, found := workerctrl.PartitionProfile(it, profile)
	if !found {
		if !ready {
			return 0, fmt.Errorf("instance type %s is not ready yet (accelerator detail not computed); retry", it.Name)
		}
		return 0, fmt.Errorf("container %q: partition profile %q is not offered by the target pool (instance type %s)",
			ctr.Name, profile, it.Name)
	}
	// A profile can appear in the inventory before its per-instance MemoryMib is populated (partial
	// detail during detection / rollout skew). Treat a missing memory as not-ready/retryable rather
	// than folding to zero units and rejecting permanently.
	if prof.MemoryMib <= 0 {
		return 0, fmt.Errorf("instance type %s is not ready yet (partition profile %q has no memory detail); retry",
			it.Name, profile)
	}
	cardVRAMMib, err := workerctrl.InstanceTypeCardVRAMMib(it)
	if err != nil {
		return 0, err
	}
	units := nodefeature.MemoryMibToUnits(prof.MemoryMib, cardVRAMMib)
	if units <= 0 {
		return 0, fmt.Errorf("container %q: cannot derive a positive %s from partition profile %q",
			ctr.Name, partitionResourceNamesForBase(base).units, profile)
	}
	return units, nil
}

func (r *PodWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	pod := obj.(*core.Pod)

	if errs := validatePodAcceleratorRequest(pod); len(errs) > 0 {
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

// validatePodAcceleratorRequest enforces the normative accelerator request rules on a Pod:
//
//  1. all containers request the same family, and the claims sit in exactly one container group;
//  2. <base>.sliced is exactly 1 (multi-card logical slicing is deferred);
//  3. <base>.partitioned is exactly 1;
//  4. a partition request names exactly one profile shape;
//  5. each per-profile key's value is exactly 1;
//  6. at most one container requests a slicing family, logical or physical;
//  7. a restartable init container requests no accelerator family at all.
//
// Rules 4 and 6 are scoped to the group holding the claims, which rule 1 makes unique; they are
// evaluated over every non-sidecar container so that a Pod violating rule 1 still reports them.
func validatePodAcceleratorRequest(pod *core.Pod) field.ErrorList {
	var errs field.ErrorList

	containers := podContainers(pod)
	claims := make([]map[nodefeature.ResourceFamily][]core.ResourceName, len(containers))
	for i := range containers {
		claims[i] = containerClaims(containers[i].ctr)
	}

	var (
		families = sets.New[nodefeature.ResourceFamily]()
		groups   = sets.New[containerGroup]()
		profiles = sets.New[string]()
		slicing  int
	)
	for i := range containers {
		pc, claim := &containers[i], claims[i]
		if len(claim) == 0 {
			continue
		}

		// Rule 7. A native sidecar overlaps both groups, so it can belong to neither and may
		// not claim an accelerator. The SSH sidecar is unaffected: the visibility resource is
		// deliberately outside the accelerator families.
		if pc.group == containerGroupSidecar {
			errs = append(errs, field.Forbidden(pc.path.Child("resources", "limits"),
				"a restartable init container (a native sidecar) may not request an accelerator; "+
					"move the request to an app container"))
			continue
		}

		for family, bases := range claim {
			families.Insert(family)
			if family == nodefeature.ResourceFamilyPartitioned {
				for _, base := range bases {
					profiles.Insert(containerPartitionProfiles(pc.ctr, base)...)
				}
			}
		}
		groups.Insert(pc.group)
		if len(claim[nodefeature.ResourceFamilySliced]) > 0 ||
			len(claim[nodefeature.ResourceFamilyPartitioned]) > 0 {
			slicing++
		}
	}

	// Rule 1, family half.
	if families.Len() > 1 {
		names := make([]string, 0, families.Len())
		for _, f := range sets.List(families) {
			names = append(names, string(f))
		}
		errs = append(errs, field.Forbidden(field.NewPath("spec"),
			fmt.Sprintf("a Pod may request only one accelerator family, found %v", names)))
	}

	// Rule 1, group half. The init group is the one asked to give up its claim: an init
	// container's devices stay in the kubelet's Pod record for the Pod's whole life, so the
	// two claims coexist while the scheduler charges the Pod for only one of them.
	if groups.Len() > 1 {
		errs = append(errs, field.Forbidden(field.NewPath("spec", string(containerGroupInit)),
			fmt.Sprintf("a Pod's accelerator requests must all sit in one container group; "+
				"spec.%s must give up its request, because its devices are held for the Pod's whole life "+
				"while the scheduler charges the Pod only once", containerGroupInit)))
	}

	// Rule 6.
	if slicing > 1 {
		errs = append(errs, field.Forbidden(field.NewPath("spec"),
			fmt.Sprintf("at most one container may request a slicing family, found %d", slicing)))
	}

	// Rule 4.
	if profiles.Len() > 1 {
		errs = append(errs, field.Forbidden(field.NewPath("spec"),
			fmt.Sprintf("a Pod may request only one partition profile, found %v", sets.List(profiles))))
	}

	// Rules 2, 3 and 5, plus the per-family shape checks. A sidecar is skipped: rule 7 already
	// told it to drop the request entirely, so grading the request's shape adds only noise.
	for i := range containers {
		pc, claim := &containers[i], claims[i]
		if pc.group == containerGroupSidecar {
			continue
		}
		// Anchor field errors under resources.limits: extended resources (accelerator keys) are
		// conventionally specified there, so this matches the user's manifest even when only
		// limits is set (requests is defaulted equal).
		limPath := pc.path.Child("resources", "limits")
		for _, base := range claim[nodefeature.ResourceFamilySliced] {
			errs = append(errs, validateLogicalSliceRequest(pc.ctr, base, limPath)...)
		}
		for _, base := range claim[nodefeature.ResourceFamilyPartitioned] {
			errs = append(errs, validatePartitionRequest(pc.ctr, base, limPath)...)
		}
	}

	return errs
}

// isExactlyOne reports whether q is exactly one. It compares with Cmp rather than Value(),
// which rounds a fractional quantity like "1m" up to 1 and would let it pass.
func isExactlyOne(q resource.Quantity) bool {
	return q.Cmp(*resource.NewQuantity(1, resource.DecimalSI)) == 0
}

// validatePartitionRequest checks one container's physical partitioning request for an
// accelerator base: it must set the card key at exactly 1 (rule 3), name exactly one profile
// (rule 4 at container scope — a bare card key has no hardware shape to actuate), and each
// per-profile key it sets must be exactly 1 (rule 5). Its mutual exclusion with the logical
// keys is the Pod-wide one-family rule, not a per-container branch.
func validatePartitionRequest(ctr *core.Container, base core.ResourceName, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	names := partitionResourceNamesForBase(base)

	cardQ, hasCard := containerResource(ctr, names.card)
	switch {
	case !hasCard:
		errs = append(errs, field.Required(path.Key(string(names.card)),
			fmt.Sprintf("a partition request must set %s", names.card)))
	case !isExactlyOne(cardQ):
		errs = append(errs, field.Invalid(path.Key(string(names.card)), cardQ.String(),
			"a partition request is always a single card; request one Pod per instance"))
	}

	reqs := containerPartitionRequests(ctr, base)
	switch len(reqs) {
	case 1:
	case 0:
		errs = append(errs, field.Required(path.Key(string(names.card)),
			fmt.Sprintf("a %s request must name a profile, e.g. %s.<kind>-<profile>", names.card, names.card)))
	default:
		errs = append(errs, field.Forbidden(path.Key(string(names.card)),
			fmt.Sprintf("a container may request only one partition profile, found %v",
				containerPartitionProfiles(ctr, base))))
	}

	for i := range reqs {
		if !isExactlyOne(reqs[i].quantity) {
			errs = append(errs, field.Invalid(path.Key(string(reqs[i].name)), reqs[i].quantity.String(),
				"a partition profile request must be exactly 1 instance"))
		}
	}

	return errs
}

// validateLogicalSliceRequest checks one container's logical (software) slicing request for an
// accelerator base: it must set the card key at exactly 1 (rule 2), name exactly one memory
// budget (percentage or mib), and every budget it sets must be positive, with the percentage
// budgets capped at 100. The compute and memory budgets are independent dimensions.
func validateLogicalSliceRequest(ctr *core.Container, base core.ResourceName, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	names := logicalResourceNamesForBase(base)

	memPctQ, hasMemPct := containerResource(ctr, names.memPct)
	memMibQ, hasMemMib := containerResource(ctr, names.memMib)
	coresPctQ, hasCoresPct := containerResource(ctr, names.coresPct)

	// A sliced sub-resource is meaningless without the card count it slices; a bare
	// sub-key alone would otherwise fold to credits with no card request behind it.
	cardQ, hasCard := containerResource(ctr, names.card)
	switch {
	case !hasCard:
		errs = append(errs, field.Required(path.Key(string(names.card)),
			fmt.Sprintf("a sliced request must set %s", names.card)))
	case !isExactlyOne(cardQ):
		errs = append(errs, field.Invalid(path.Key(string(names.card)), cardQ.String(),
			"a logical slice request is always a single card; multi-card logical slicing is not supported yet"))
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
