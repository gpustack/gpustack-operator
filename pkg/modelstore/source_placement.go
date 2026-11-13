package modelstore

import (
	"context"
	"errors"
	"sort"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/mapx"
)

// ResourceFlavorResourceType is the systemmeta resource type carried by the ResourceFlavors the
// worker's NodeFlavor reconciler owns; the flavor lookup below reads only those, so an
// admin-created flavor outside the reconciler never answers a pool lookup.
const ResourceFlavorResourceType = "nodes"

// The placement refusals shared by the prefetch controller, its admission webhook, and the
// upcoming source IntentSource. Both fields set at once is ambiguous — the schema cannot see it
// because neither is required — and a source placement that names neither is a refusal, not an
// expansion: unlike the disk-only default, a source never derives its set from consumer
// deployments.
var (
	ErrPrefetchPlacementAmbiguous = errors.New("placement sets both acceleratorTypes and nodeSelector")
	ErrPrefetchPlacementMissing   = errors.New("placement requires acceleratorTypes or nodeSelector")
)

// AcceleratorTypeFlavorNames carries, per AcceleratorType name, the ResourceFlavor names the type's
// pool selects. A type resolving to no flavors — unknown, or no synced flavor yet — maps to an
// empty entry, which leaves its target set empty rather than erroring: the placement contract
// is "every node carrying the type's flavors", and a type with none carries none.
type AcceleratorTypeFlavorNames map[string][]string

// PrefetchPlacementTypeNames returns the AcceleratorType names a prefetch's placement selects, or
// the ones its namespace's deployments serve the artifact on. An empty result means the
// placement's nodeSelector owns the target set, or that there is nothing to derive it from.
// The deployments are the prefetch's own namespace's — both callers (the controller and the
// admission webhook) scope them so before passing them, and the derivation follows the
// artifact ref within that slice.
func PrefetchPlacementTypeNames(
	pf *workercore.ModelPrefetch, deps []workercore.ModelDeployment,
) []string {
	placement := pf.Spec.Placement
	switch {
	case placement != nil && len(placement.AcceleratorTypes) > 0 && placement.NodeSelector != nil:
		return nil
	case placement != nil && len(placement.AcceleratorTypes) > 0:
		return placement.AcceleratorTypes
	case placement != nil && placement.NodeSelector != nil:
		return nil
	default:
		// The derived set: every AcceleratorType the namespace's own deployments serve these weights
		// on, so warming follows serving without a second place to say where.
		return DeploymentAcceleratorTypes(deps, pf.Spec.ArtifactRef.Name)
	}
}

// DeploymentAcceleratorTypes collects the sorted unique non-empty AcceleratorType names across
// same-namespace ModelDeployments referencing artifactName (dep.DeletionTimestamp == nil).
// CPU-only roles (role.AcceleratorType == "") are skipped cleanly.
func DeploymentAcceleratorTypes(deps []workercore.ModelDeployment, artifactName string) []string {
	var out []string
	seen := make(map[string]bool)
	for i := range deps {
		if deps[i].DeletionTimestamp != nil {
			continue
		}
		ref := deps[i].Spec.Model.ArtifactRef
		if ref == nil || ref.Name != artifactName {
			continue
		}
		for _, role := range deps[i].Spec.Roles {
			name := role.AcceleratorType
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)

	return out
}

// PrefetchTargetNodes resolves the placement into sorted node names: the named AcceleratorTypes'
// nodes, or the selector's matches, or — with no placement at all — the AcceleratorTypes of the
// namespace's deployments that reference the artifact. This is the disk-only contract: a
// source's target set must come from PrefetchSourceTargetNodes, which never derives.
func PrefetchTargetNodes(
	pf *workercore.ModelPrefetch, nodes []core.Node, deps []workercore.ModelDeployment,
	pools AcceleratorTypeFlavorNames,
) ([]string, error) {
	placement := pf.Spec.Placement
	if placement != nil && len(placement.AcceleratorTypes) > 0 && placement.NodeSelector != nil {
		return nil, ErrPrefetchPlacementAmbiguous
	}
	if placement != nil && placement.NodeSelector != nil {
		return matchSelectorNodes(nodes, placement.NodeSelector)
	}

	// The named AcceleratorTypes' target set is every node carrying one of the types' flavors: the
	// pool flavors each named type selects, resolved by the schedule labels its spec derives.
	// An AcceleratorType's own name is never a flavor name — flavor names are topology-qualified
	// and count-suffixed — so comparing it against one matches nothing.
	var wanted map[string]bool
	for _, name := range PrefetchPlacementTypeNames(pf, deps) {
		wanted = wantFlavors(wanted, pools[name])
	}

	return matchFlavorNodes(nodes, wanted), nil
}

// PrefetchSourceTargetNodes resolves a source's placement into sorted node names. The source
// contract is explicit-only: the placement must name acceleratorTypes or a nodeSelector, the set
// is never derived from consumer deployments, and a type resolving to no flavors yields an
// empty target rather than an error. Ambiguity and a missing placement are refusals.
func PrefetchSourceTargetNodes(
	pf *workercore.ModelPrefetch, nodes []core.Node, pools AcceleratorTypeFlavorNames,
) ([]string, error) {
	placement := pf.Spec.Placement
	if placement == nil || (len(placement.AcceleratorTypes) == 0 && placement.NodeSelector == nil) {
		return nil, ErrPrefetchPlacementMissing
	}
	if placement != nil && len(placement.AcceleratorTypes) > 0 && placement.NodeSelector != nil {
		return nil, ErrPrefetchPlacementAmbiguous
	}
	if placement.NodeSelector != nil {
		return matchSelectorNodes(nodes, placement.NodeSelector)
	}

	var wanted map[string]bool
	for _, name := range placement.AcceleratorTypes {
		wanted = wantFlavors(wanted, pools[name])
	}

	return matchFlavorNodes(nodes, wanted), nil
}

// AcceleratorTypeFlavorSelector computes the schedule discriminator labels for an AcceleratorType's pool flavors.
func AcceleratorTypeFlavorSelector(at *workercore.AcceleratorType, cpuAware bool) map[string]string {
	return nodefeature.PoolScheduleLabels(
		true, cpuAware,
		at.Spec.GeneralGroup, at.Spec.AcceleratorGroup,
		at.Spec.OS, at.Spec.Arch,
	)
}

// ResolveAcceleratorTypeFlavorNames resolves the AcceleratorType names a prefetch's placement — or, in
// its default form, the type names its namespace's deployments serve the artifact on — into the
// ResourceFlavor names each type's pool selects: the flavors carrying the schedule labels the
// type's spec derives, the same reverse-lookup the type's backing ClusterQueue and its status
// Detail run. Flavor names are topology-qualified and count-suffixed, so an AcceleratorType's
// object name is never itself a flavor name and cannot be compared against one. The
// CPU-manufacturer awareness comes in resolved by the caller — the worker reads its setting
// here, and a source resolver passes its own decision — so this package stays free of
// worker settings.
func ResolveAcceleratorTypeFlavorNames(
	ctx context.Context, reader ctrlcli.Reader, pf *workercore.ModelPrefetch,
	deps []workercore.ModelDeployment, cpuAware bool,
) (AcceleratorTypeFlavorNames, error) {
	return ResolvePlacementAcceleratorTypeFlavorNames(ctx, reader,
		PrefetchPlacementTypeNames(pf, deps), cpuAware)
}

// ResolvePlacementAcceleratorTypeFlavorNames is the pool resolution behind
// ResolveAcceleratorTypeFlavorNames over explicit type names, so a caller that names its types
// itself — a source intent with no derived default — resolves the same pools by the same
// reverse-lookup.
func ResolvePlacementAcceleratorTypeFlavorNames(
	ctx context.Context, reader ctrlcli.Reader, typeNames []string, cpuAware bool,
) (AcceleratorTypeFlavorNames, error) {
	if len(typeNames) == 0 {
		return nil, nil
	}
	// One list serves every named type: a cluster carries a handful of operator-owned flavors,
	// and a type's pool is a label containment over the shared set.
	flavors := new(kueue.ResourceFlavorList)
	if err := reader.List(ctx, flavors,
		systemmeta.GetResourcesLabelSetOfType[ctrlcli.MatchingLabels](ResourceFlavorResourceType),
		ctrlcli.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	pools := AcceleratorTypeFlavorNames{}
	for _, name := range typeNames {
		if _, resolved := pools[name]; resolved {
			continue
		}
		at := new(workercore.AcceleratorType)
		err := reader.Get(ctx, ctrlcli.ObjectKey{Name: name}, at)
		if kerrors.IsNotFound(err) {
			pools[name] = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		sched := AcceleratorTypeFlavorSelector(at, cpuAware)
		var flavorNames []string
		for i := range flavors.Items {
			if mapx.Contain(flavors.Items[i].Labels, sched) {
				flavorNames = append(flavorNames, flavors.Items[i].Name)
			}
		}
		sort.Strings(flavorNames)
		pools[name] = flavorNames
	}

	return pools, nil
}

// matchSelectorNodes returns the sorted names of the nodes the selector matches; an
// unparsable selector is an error, not an empty set.
func matchSelectorNodes(nodes []core.Node, selector *meta.LabelSelector) ([]string, error) {
	sel, err := meta.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range nodes {
		if sel.Matches(labels.Set(nodes[i].Labels)) {
			out = append(out, nodes[i].Name)
		}
	}
	sort.Strings(out)

	return out, nil
}

// matchFlavorNodes returns the sorted names of the nodes carrying any wanted flavor, matched by
// the node's own topology profile — the same match the flavor reconciler and the device layer run.
func matchFlavorNodes(nodes []core.Node, wanted map[string]bool) []string {
	var out []string
	for i := range nodes {
		for flavorName := range wanted {
			if nodefeature.MatchNodeFlavor(&nodes[i], flavorName) != nil {
				out = append(out, nodes[i].Name)
				break
			}
		}
	}
	sort.Strings(out)

	return out
}

// wantFlavors adds a type's flavors to the wanted set, allocating it on first use.
func wantFlavors(wanted map[string]bool, flavorNames []string) map[string]bool {
	if wanted == nil {
		wanted = map[string]bool{}
	}
	for _, flavorName := range flavorNames {
		wanted[flavorName] = true
	}
	return wanted
}
