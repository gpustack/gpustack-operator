package worker

import (
	"context"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

// Conditions of a ModelStoreBinding's status.
const (
	// ModelStoreBindingConditionReady says the accounting could read everything it counts.
	ModelStoreBindingConditionReady = "Ready"
	// ModelStoreBindingConditionOverQuota says the namespace's warm footprint passed the grant.
	ModelStoreBindingConditionOverQuota = "OverQuota"
)

// ModelStoreBindingReconciler keeps a grant's status equal to what its namespace actually warms:
// usedBytes counts each distinct digest any of the namespace's prefetches targets, at its full
// size, for every TARGET node holding it — a digest a workload mounted on a node the prefetch
// never asked for belongs to the node's watermarks, not to this budget. A shared tree charges
// every namespace whose prefetches target it, and a digest nobody holds charges nothing.
//
// An unmeasurable contribution — an artifact that has not resolved, mostly — leaves the whole
// figure absent rather than half a number: a usedBytes that silently excluded a prefetch would
// read as headroom the namespace does not have.
type ModelStoreBindingReconciler struct {
	Client ctrlcli.Client
	Now    func() time.Time
}

var _ ctrlreconcile.Reconciler = (*ModelStoreBindingReconciler)(nil)

func (r *ModelStoreBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	binding := new(workercore.ModelStoreBinding)
	switch err := r.Client.Get(ctx, req.NamespacedName, binding); {
	case kerrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	if binding.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs, ctrlcli.InNamespace(binding.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	stores := new(workercore.NodeModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		return ctrl.Result{}, err
	}
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	deps := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, deps, ctrlcli.InNamespace(binding.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	// holding[digest] is the set of nodes listing the digest, in any state: a downloading partial
	// is on the node's disk as surely as a published tree.
	holding := map[string]map[string]bool{}
	for i := range stores.Items {
		for _, m := range stores.Items[i].Status.Models {
			if holding[m.Digest] == nil {
				holding[m.Digest] = map[string]bool{}
			}
			holding[m.Digest][stores.Items[i].Name] = true
		}
	}

	// The artifacts are read once for the namespace: a round-trip per prefetch is a hot-path
	// pattern this controller has no business paying on every node report.
	artifacts := new(workercore.ModelArtifactList)
	if err := r.Client.List(ctx, artifacts, ctrlcli.InNamespace(binding.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	artifactByName := map[string]*workercore.ModelArtifact{}
	for i := range artifacts.Items {
		artifactByName[artifacts.Items[i].Name] = &artifacts.Items[i]
	}

	// charged[digest] is the subset of its holders the grant is charged for: the target nodes of a
	// prefetch that names the digest. Nodes holding it for other reasons — a workload's ordinary
	// mount elsewhere — are the watermarks' business.
	charged := map[string]map[string]bool{}
	sizes := map[string]int64{}
	var used int64
	measured := true
	for i := range pfs.Items {
		pf := &pfs.Items[i]
		artifact, ok := artifactByName[pf.Spec.ArtifactRef.Name]
		if !ok {
			// The prefetch's own status carries the missing artifact; the accounting keeps the
			// rest of the figure out of the way rather than guess at zero.
			measured = false
			continue
		}
		resolved := artifact.Status.Resolved
		if resolved == nil || resolved.ManifestDigest == "" || resolved.SizeBytes == 0 {
			measured = false
			continue
		}
		sizes[resolved.ManifestDigest] = resolved.SizeBytes

		targets, err := PrefetchTargetNodes(pf, nodes.Items, deps.Items)
		if err != nil {
			// The prefetch's own status carries the broken placement, but its bytes cannot be
			// counted either, so the figure is not a measurement.
			measured = false
			continue
		}
		if charged[resolved.ManifestDigest] == nil {
			charged[resolved.ManifestDigest] = map[string]bool{}
		}
		for _, node := range targets {
			if holding[resolved.ManifestDigest][node] {
				charged[resolved.ManifestDigest][node] = true
			}
		}
	}
	for digest, nodesCharged := range charged {
		used += sizes[digest] * int64(len(nodesCharged))
	}

	before := binding.Status.DeepCopy()
	if measured {
		u := resource.NewQuantity(used, resource.BinarySI)
		binding.Status.UsedBytes = u
	} else {
		binding.Status.UsedBytes = nil
	}

	quota := binding.Spec.Quota.Bytes.Value()
	over := gpustack.Condition{
		Type:               ModelStoreBindingConditionOverQuota,
		ObservedGeneration: binding.Generation,
	}
	ready := gpustack.Condition{
		Type:               ModelStoreBindingConditionReady,
		ObservedGeneration: binding.Generation,
	}
	storeMissing := false
	for _, ref := range binding.Spec.StoreRefs {
		if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: ref.Name}, new(workercore.ModelStore)); err != nil {
			if kerrors.IsNotFound(err) {
				storeMissing = true
				break
			}

			return ctrl.Result{}, err
		}
	}
	switch {
	case !measured:
		ready.Status, ready.Reason = meta.ConditionFalse, "Unmeasured"
		ready.Message = "an artifact of this namespace's prefetches has not resolved, so the footprint cannot be counted"
		over.Status, over.Reason = meta.ConditionUnknown, "Unmeasured"
		over.Message = "the footprint is not fully counted, so over-quota cannot be judged yet"
	default:
		ready.Status, ready.Reason = meta.ConditionTrue, "Accounted"
		ready.Message = "the namespace's warm footprint is counted"
		if used > quota {
			over.Status, over.Reason = meta.ConditionTrue, "PastBudget"
			over.Message = "the namespace's warm footprint passed the grant; existing prefetches keep running, new ones are refused"
		} else {
			over.Status, over.Reason = meta.ConditionFalse, "WithinBudget"
			over.Message = "the namespace's warm footprint is within the grant"
		}
	}
	if storeMissing {
		ready.Status, ready.Reason = meta.ConditionFalse, "StoreMissing"
		ready.Message = "a store this grant names no longer exists; new prefetches are refused until the grant is fixed"
	}
	binding.Status.Phase = bindingStatusPhase(ready.Status, over.Status)
	binding.Status.PhaseMessage = ready.Message
	binding.Status.Conditions = carryConditionTransitions(binding.Status.Conditions,
		[]gpustack.Condition{ready, over}, r.nowOrNow())
	if kubemeta.DeepEqual(before, &binding.Status) {
		return ctrl.Result{}, nil
	}

	logger.V(2).Info("accounted the namespace's warm footprint",
		"binding", binding.Name, "usedBytes", used, "measured", measured, "storeMissing", storeMissing)

	return ctrl.Result{}, r.Client.Status().Update(ctx, binding)
}

// bindingStatusPhase summarizes the two conditions for `.status.phase` and its print column, most
// urgent first: an over-budget grant reads OverQuota even when something else also keeps the
// grant from being fully healthy.
func bindingStatusPhase(ready, over meta.ConditionStatus) string {
	if over == meta.ConditionTrue {
		return "OverQuota"
	}
	if ready == meta.ConditionTrue {
		return "Ready"
	}

	return "Error"
}

func (r *ModelStoreBindingReconciler) nowOrNow() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func (r *ModelStoreBindingReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modelstorebinding").
		For(&workercore.ModelStoreBinding{}, ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{})).
		Watches(
			&workercore.ModelPrefetch{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllBindings),
			ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{}),
		).
		Watches(
			&workercore.ModelArtifact{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllBindings),
			ctrlbuilder.WithPredicates(artifactResolutionChanged),
		).
		Watches(
			&workercore.NodeModelStore{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllBindings),
			ctrlbuilder.WithPredicates(nodeReportChanged),
		).
		Complete(r)
}

// enqueueAllBindings re-enqueues every grant: the accounting reads across namespaces by design, and
// grants are few.
func (r *ModelStoreBindingReconciler) enqueueAllBindings(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	bindings := new(workercore.ModelStoreBindingList)
	if err := r.Client.List(ctx, bindings); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model store bindings")

		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(bindings.Items))
	for i := range bindings.Items {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{
			Namespace: bindings.Items[i].Namespace, Name: bindings.Items[i].Name,
		}})
	}

	return reqs
}
