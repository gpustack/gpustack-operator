package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
)

const (
	// nodeModelStorePinnedRequestName is the synthetic reconcile key that recomputes every node's
	// NodeModelStore.spec.pinned union even when zero ModelPrefetch objects exist.
	nodeModelStorePinnedRequestName = "__pinned__"

	// nodeModelStoreMaxPinned matches the NodeModelStore.spec.pinned schema ceiling.
	nodeModelStoreMaxPinned = 256
)

// recomputePinned rewrites every node's spec.pinned as the deterministic, sorted, deduplicated
// union of three priority tiers:
//  1. Admitted ModelTransfer source-generation digests reported on that node.
//  2. Non-deleting nodeStore ModelKernelArtifact digests on their seedNode (while not quarantined
//     and while Ready or awaiting initial resolution), on nodes hosting live Pods of
//     ModelDeployments referencing or mounting them, and on nodes protecting previousKernelArtifact
//     during an in-flight ModelDeploymentForger rollout.
//  3. Live ModelPrefetch disk-retention pins (spec.retention.pinned under allowPinned while the
//     retention TTL holds).
//
// ModelPrefetchReconciler is the sole writer of NodeModelStore.spec.pinned, so the full slice is
// replaced rather than merged. Each node is capped independently at 256 entries in priority order
// (1 > 2 > 3, lexicographic tie-break within each tier), and an update failure or cap saturation
// on one node never aborts reconciliation for remaining nodes.
func (r *ModelPrefetchReconciler) recomputePinned(ctx context.Context) error {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs); err != nil {
		return err
	}
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes); err != nil {
		return err
	}
	stores := new(workercore.NodeModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		return err
	}
	reader := r.reader()
	deps := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, deps); err != nil {
		return err
	}
	bindings := new(workercore.ModelStoreBindingList)
	if err := r.Client.List(ctx, bindings); err != nil {
		return err
	}
	artifacts := new(workercore.ModelArtifactList)
	if err := r.Client.List(ctx, artifacts); err != nil {
		return err
	}
	kernelArtifacts := new(workercore.ModelKernelArtifactList)
	if err := reader.List(ctx, kernelArtifacts); err != nil {
		return err
	}
	forgers := new(workercore.ModelDeploymentForgerList)
	if err := r.Client.List(ctx, forgers); err != nil {
		return err
	}
	pods := new(core.PodList)
	if err := r.Client.List(ctx, pods,
		ctrlcli.MatchingLabels{modelDeploymentLabelKeyName: modelDeploymentLabelValueName}); err != nil {
		return err
	}

	// depsByNS keeps derived-placement expansion inside each prefetch's own namespace.
	depsByNS := map[string][]workercore.ModelDeployment{}
	for i := range deps.Items {
		ns := deps.Items[i].Namespace
		depsByNS[ns] = append(depsByNS[ns], deps.Items[i])
	}
	bindingByKey := map[string]*workercore.ModelStoreBinding{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		bindingByKey[b.Namespace+"/"+b.Name] = b
	}
	artifactByKey := map[string]*workercore.ModelArtifact{}
	for i := range artifacts.Items {
		a := &artifacts.Items[i]
		artifactByKey[a.Namespace+"/"+a.Name] = a
	}

	tier1 := map[string]map[string]struct{}{}
	tier2 := map[string]map[string]struct{}{}
	tier3 := map[string]map[string]struct{}{}
	kernelByNodeDigest := map[string]map[string][]*workercore.ModelKernelArtifact{}

	addPin := func(tiers map[string]map[string]struct{}, node, digest string) {
		if node == "" || digest == "" {
			return
		}
		if tiers[node] == nil {
			tiers[node] = map[string]struct{}{}
		}
		tiers[node][digest] = struct{}{}
	}

	// Tier 1: every reported ModelTransfer source generation pins its digest on its own node.
	for i := range stores.Items {
		store := &stores.Items[i]
		if store.Status.ModelTransfer == nil {
			continue
		}
		for _, generation := range store.Status.ModelTransfer.Generations {
			addPin(tier1, store.Name, generation.Digest)
		}
	}

	// Tier 2: non-deleting nodeStore ModelKernelArtifacts on seedNode, live referencing
	// ModelDeployment Pod nodes, and live target deployment Pod nodes for previousKernelArtifact.
	for i := range kernelArtifacts.Items {
		mka := &kernelArtifacts.Items[i]
		if mka.DeletionTimestamp != nil || mka.Spec.Source.NodeStore == nil {
			continue
		}
		digest := mka.Spec.Source.NodeStore.ManifestDigest
		if digest == "" {
			continue
		}
		requiredNodes := kernelArtifactRequiredNodes(mka, stores.Items, deps.Items, forgers.Items, pods.Items)
		for _, node := range requiredNodes {
			addPin(tier2, node, digest)
			if kernelByNodeDigest[node] == nil {
				kernelByNodeDigest[node] = map[string][]*workercore.ModelKernelArtifact{}
			}
			kernelByNodeDigest[node][digest] = append(kernelByNodeDigest[node][digest], mka)
		}
	}

	// Tier 3: live ModelPrefetch disk-retention pins under allowPinned while the TTL holds.
	for i := range pfs.Items {
		pf := &pfs.Items[i]
		if pf.DeletionTimestamp != nil {
			continue
		}
		binding := bindingByKey[pf.Namespace+"/"+pf.Spec.BindingRef.Name]
		if !pf.Spec.Retention.Pinned || binding == nil || !bindingAllowsPinned(binding) {
			continue
		}
		artifact := artifactByKey[pf.Namespace+"/"+pf.Spec.ArtifactRef.Name]
		if artifact == nil || artifact.Status.Resolved == nil {
			continue
		}
		digest := artifact.Status.Resolved.ManifestDigest
		if digest == "" {
			continue
		}
		pools, err := ResolveAcceleratorTypeFlavorNames(ctx, r.Client, pf, depsByNS[pf.Namespace])
		if err != nil {
			return err
		}
		targets, err := prefetchTargetNodes(pf, nodes.Items, depsByNS[pf.Namespace], pools)
		if err != nil {
			continue
		}
		entries := modelstore.NodeEntries(stores.Items, digest)
		for _, node := range targets {
			entry, ok := entries[node]
			if !ok || entry.State != workercore.NodeModelStoreModelStateReady {
				continue
			}
			if r.ttlExpired(pf, entry) {
				continue
			}
			addPin(tier3, node, digest)
		}
	}

	logger := ctrllog.FromContext(ctx)
	var errs []error
	for i := range stores.Items {
		nms := &stores.Items[i]
		fresh, droppedKernel := selectNodePins(tier1[nms.Name], tier2[nms.Name], tier3[nms.Name], nodeModelStoreMaxPinned)
		if len(droppedKernel) > 0 {
			logger.Info("node model store reached pin cap; dropping kernel artifact digests",
				"node", nms.Name, "cap", nodeModelStoreMaxPinned, "dropped", len(droppedKernel))
			if r.Recorder != nil {
				for _, digest := range droppedKernel {
					for _, mka := range kernelByNodeDigest[nms.Name][digest] {
						r.Recorder.Eventf(mka, core.EventTypeWarning,
							workercore.ModelKernelArtifactReasonPinnedCapacityExceeded,
							"NodeModelStore %q reached the %d-entry pin cap before admitting digest %s",
							nms.Name, nodeModelStoreMaxPinned, digest)
					}
				}
			}
		}
		if len(fresh) == 0 && len(nms.Spec.Pinned) == 0 {
			continue
		}
		if len(fresh) == 0 {
			fresh = nil
		}
		if slices.Equal(nms.Spec.Pinned, fresh) {
			continue
		}
		updated := nms.DeepCopy()
		updated.Spec.Pinned = fresh
		if err := r.Client.Update(ctx, updated); err != nil {
			errs = append(errs, fmt.Errorf("update NodeModelStore %q pins: %w", nms.Name, err))
			continue
		}
	}

	return errors.Join(errs...)
}

// selectNodePins admits at most limit unique digests for one node in deterministic priority order
// (tier1 > tier2 > tier3, lexicographic tie-break within each tier), returns the admitted slice in
// canonical sorted order, and reports any tier2 (ModelKernelArtifact) digests dropped by the cap.
func selectNodePins(
	tier1, tier2, tier3 map[string]struct{}, limit int,
) (admitted, droppedTier2 []string) {
	if limit <= 0 {
		return nil, sortedSetKeys(tier2)
	}
	chosen := make(map[string]struct{}, min(len(tier1)+len(tier2)+len(tier3), limit))
	admitted = make([]string, 0, min(len(tier1)+len(tier2)+len(tier3), limit))

	for _, d := range sortedSetKeys(tier1) {
		if len(admitted) >= limit {
			break
		}
		chosen[d] = struct{}{}
		admitted = append(admitted, d)
	}
	for _, d := range sortedSetKeys(tier2) {
		if _, ok := chosen[d]; ok {
			continue
		}
		if len(admitted) < limit {
			chosen[d] = struct{}{}
			admitted = append(admitted, d)
			continue
		}
		droppedTier2 = append(droppedTier2, d)
	}
	for _, d := range sortedSetKeys(tier3) {
		if len(admitted) >= limit {
			break
		}
		if _, ok := chosen[d]; ok {
			continue
		}
		chosen[d] = struct{}{}
		admitted = append(admitted, d)
	}

	sort.Strings(admitted)
	return admitted, droppedTier2
}

func sortedSetKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// kernelArtifactRequiredNodes returns the sorted node names that must pin a non-deleting nodeStore
// ModelKernelArtifact: its seedNode (when not quarantined and while seedNode reports the digest
// Ready or before initial resolution), every node hosting a live Pod of a referencing
// ModelDeployment role or still mounting the artifact's CSI volume, and every node hosting a live
// Pod of a ModelDeploymentForger target role that holds this artifact as previousKernelArtifact.
func kernelArtifactRequiredNodes(
	mka *workercore.ModelKernelArtifact,
	stores []workercore.NodeModelStore,
	deps []workercore.ModelDeployment,
	forgers []workercore.ModelDeploymentForger,
	pods []core.Pod,
) []string {
	if mka == nil || mka.DeletionTimestamp != nil || mka.Spec.Source.NodeStore == nil {
		return nil
	}
	digest := mka.Spec.Source.NodeStore.ManifestDigest
	if digest == "" {
		return nil
	}

	nodeSet := map[string]struct{}{}
	if !mka.Status.Quarantined {
		if seed := mka.Spec.Source.NodeStore.SeedNode; seed != "" {
			if mka.Status.Resolved == nil {
				nodeSet[seed] = struct{}{}
			} else if entry, ok := modelstore.NodeEntries(stores, digest)[seed]; ok &&
				entry.State == workercore.NodeModelStoreModelStateReady {
				nodeSet[seed] = struct{}{}
			}
		}
	}

	addDeploymentRoleNodes := func(ns, mdName, roleName string, allowMissingComponent bool) {
		for i := range pods {
			p := &pods[i]
			if podMatchesDeploymentRole(p, ns, mdName, roleName, allowMissingComponent) {
				nodeSet[p.Spec.NodeName] = struct{}{}
			}
		}
	}

	for i := range pods {
		p := &pods[i]
		if !isLiveModelDeploymentPod(p) || p.Namespace != mka.Namespace {
			continue
		}
		if src := modelDeploymentPodKernelVolumeSource(p); src != nil && src.CSI != nil &&
			src.CSI.Driver == modelstore.DriverName &&
			(src.CSI.VolumeAttributes[modelstore.VolumeAttrArtifact] == mka.Name ||
				src.CSI.VolumeAttributes[modelstore.VolumeAttrManifestDigest] == digest) {
			nodeSet[p.Spec.NodeName] = struct{}{}
		}
	}

	for i := range deps {
		md := &deps[i]
		if md.Namespace != mka.Namespace {
			continue
		}
		singleRole := len(md.Spec.Roles) == 1
		for _, role := range md.Spec.Roles {
			if role.KernelArtifactRef != nil && role.KernelArtifactRef.Name == mka.Name {
				addDeploymentRoleNodes(md.Namespace, md.Name, role.Name, singleRole)
			}
		}
	}

	for i := range forgers {
		mdf := &forgers[i]
		if mdf.Namespace != mka.Namespace || mdf.Spec.TargetRef.Name == "" {
			continue
		}
		for _, rs := range mdf.Status.Roles {
			if rs.PreviousKernelArtifact == mka.Name {
				addDeploymentRoleNodes(mdf.Namespace, mdf.Spec.TargetRef.Name, rs.Name, true)
			}
		}
	}

	return sortedSetKeys(nodeSet)
}

// isLiveModelDeploymentPod reports whether pod is scheduled onto a node and has not reached a
// terminal phase.
func isLiveModelDeploymentPod(pod *core.Pod) bool {
	if pod == nil || pod.Spec.NodeName == "" {
		return false
	}
	if pod.Labels[modelDeploymentLabelKeyName] != modelDeploymentLabelValueName {
		return false
	}
	return pod.Status.Phase != core.PodSucceeded && pod.Status.Phase != core.PodFailed
}

func podMatchesDeploymentRole(pod *core.Pod, ns, mdName, roleName string, allowMissingComponent bool) bool {
	if !isLiveModelDeploymentPod(pod) {
		return false
	}
	if pod.Namespace != ns || pod.Labels[modelDeploymentLabelKeyInstance] != mdName {
		return false
	}
	comp := pod.Labels[modelDeploymentLabelKeyComponent]
	if roleName != "" && comp != roleName && (!allowMissingComponent || comp != "") {
		return false
	}
	return true
}

func (r *ModelPrefetchReconciler) enqueuePinned(_ context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: nodeModelStorePinnedRequestName}}}
}

func (r *ModelPrefetchReconciler) enqueuePinnedForModelDeploymentPod(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	list := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(obj.GetNamespace())); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model kernel artifacts for pod pin recompute")
		return nil
	}
	for i := range list.Items {
		if list.Items[i].Spec.Source.NodeStore != nil {
			return r.enqueuePinned(ctx, obj)
		}
	}
	return nil
}

func modelDeploymentKernelArtifactRefs(md *workercore.ModelDeployment) map[string]string {
	refs := map[string]string{}
	if md == nil {
		return refs
	}
	for _, role := range md.Spec.Roles {
		if role.KernelArtifactRef != nil && role.KernelArtifactRef.Name != "" {
			refs[role.Name] = role.KernelArtifactRef.Name
		}
	}
	return refs
}

func modelDeploymentForgerPreviousKernelArtifacts(mdf *workercore.ModelDeploymentForger) map[string]string {
	refs := map[string]string{}
	if mdf == nil {
		return refs
	}
	for _, role := range mdf.Status.Roles {
		if role.PreviousKernelArtifact != "" {
			refs[role.Name] = role.PreviousKernelArtifact
		}
	}
	return refs
}

var modelKernelArtifactPinChanged = ctrlpredicate.Funcs{
	CreateFunc: func(e ctrlevent.CreateEvent) bool {
		mka, ok := e.Object.(*workercore.ModelKernelArtifact)
		return ok && mka.Spec.Source.NodeStore != nil
	},
	DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
		mka, ok := e.Object.(*workercore.ModelKernelArtifact)
		return ok && mka.Spec.Source.NodeStore != nil
	},
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.ModelKernelArtifact)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.ModelKernelArtifact)
		if !ok {
			return false
		}
		if o.Spec.Source.NodeStore == nil && n.Spec.Source.NodeStore == nil {
			return false
		}
		return o.Generation != n.Generation ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp) ||
			o.Status.Quarantined != n.Status.Quarantined ||
			!kubemeta.DeepEqual(o.Status.Resolved, n.Status.Resolved)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

var modelDeploymentKernelRefsChanged = ctrlpredicate.Funcs{
	CreateFunc: func(e ctrlevent.CreateEvent) bool {
		md, ok := e.Object.(*workercore.ModelDeployment)
		return ok && len(modelDeploymentKernelArtifactRefs(md)) > 0
	},
	DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
		md, ok := e.Object.(*workercore.ModelDeployment)
		return ok && len(modelDeploymentKernelArtifactRefs(md)) > 0
	},
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.ModelDeployment)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.ModelDeployment)
		if !ok {
			return false
		}
		return !maps.Equal(modelDeploymentKernelArtifactRefs(o), modelDeploymentKernelArtifactRefs(n)) ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

var modelDeploymentForgerPinChanged = ctrlpredicate.Funcs{
	CreateFunc: func(e ctrlevent.CreateEvent) bool {
		mdf, ok := e.Object.(*workercore.ModelDeploymentForger)
		return ok && len(modelDeploymentForgerPreviousKernelArtifacts(mdf)) > 0
	},
	DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
		mdf, ok := e.Object.(*workercore.ModelDeploymentForger)
		return ok && len(modelDeploymentForgerPreviousKernelArtifacts(mdf)) > 0
	},
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.ModelDeploymentForger)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.ModelDeploymentForger)
		if !ok {
			return false
		}
		return !maps.Equal(modelDeploymentForgerPreviousKernelArtifacts(o), modelDeploymentForgerPreviousKernelArtifacts(n)) ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

var modelDeploymentPodPinChanged = ctrlpredicate.Funcs{
	CreateFunc: func(e ctrlevent.CreateEvent) bool {
		pod, ok := e.Object.(*core.Pod)
		return ok && isLiveModelDeploymentPod(pod)
	},
	DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
		pod, ok := e.Object.(*core.Pod)
		return ok && pod.Spec.NodeName != "" && pod.Labels[modelDeploymentLabelKeyName] == modelDeploymentLabelValueName
	},
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*core.Pod)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*core.Pod)
		if !ok {
			return false
		}
		if o.Labels[modelDeploymentLabelKeyName] != modelDeploymentLabelValueName &&
			n.Labels[modelDeploymentLabelKeyName] != modelDeploymentLabelValueName {
			return false
		}
		return o.Spec.NodeName != n.Spec.NodeName ||
			isLiveModelDeploymentPod(o) != isLiveModelDeploymentPod(n)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}
