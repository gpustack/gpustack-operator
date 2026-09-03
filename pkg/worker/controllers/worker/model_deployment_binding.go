package worker

import (
	"context"
	"fmt"
	"slices"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
)

// ModelDeploymentKind is how a ModelDeployment names itself in a KVCachePoolBinding's usedBy.
//
// The Binding's API declares this kind as the ONLY writer of that list, and its finalizer refuses to
// release a Binding the list is not empty on. So the constant is not a label: it is the token the
// refusal keys on, and misspelling it would leave the refusal enforcing over an empty list forever.
const ModelDeploymentKind = "ModelDeployment"

// ModelDeploymentConditionDomainRegistered reports whether the referenced KVCachePoolBinding
// resolved and its reuse domain was read.
const ModelDeploymentConditionDomainRegistered kubeapistatus.ConditionType = "DomainRegistered"

// The reasons DomainRegistered carries. Three rather than two, because the three send a reader to
// three different places: create the Binding, wait for it (or look at the pool), find who deleted it.
const (
	modelDeploymentReasonRegistered      = "Registered"
	modelDeploymentReasonBindingNotFound = "BindingNotFound"
	modelDeploymentReasonBindingNotReady = "BindingNotReady"
	modelDeploymentReasonBindingDeleting = "BindingDeleting"
)

// modelDeploymentDomain is what one pass observed about the referenced Binding.
//
// KVCache is nil when the Binding could not be read at all. That is NOT the same as an empty
// projection: a deployment that resolved once and has since lost its Binding keeps the domain it
// attached to, and the condition is what says the reading is stale. Emptying it would delete the one
// record of which cache the replicas are still writing into.
type modelDeploymentDomain struct {
	KVCache *workercore.ModelDeploymentKVCacheStatus
	Ready   bool
	Reason  string
	Message string
}

// resolveModelDeploymentDomain reads the KVCachePoolBinding this deployment references and projects
// its immutable domain block.
//
// The read is scoped to the deployment's OWN namespace and there is no other lookup: poolRef is a
// LocalObjectReference, so the name it carries can only ever mean an object here. The pool is named
// from the Binding rather than read, because the projection wants its name and nothing else.
func (r *ModelDeploymentReconciler) resolveModelDeploymentDomain(
	ctx context.Context, md *workercore.ModelDeployment,
) (*modelDeploymentDomain, error) {
	kvcpb, err := r.getModelDeploymentBinding(ctx, md)
	if err != nil {
		return nil, err
	}

	name := md.Spec.KVCache.PoolRef.Name
	if kvcpb == nil {
		return &modelDeploymentDomain{
			Reason: modelDeploymentReasonBindingNotFound,
			Message: fmt.Sprintf(
				"kv cache pool binding %q does not exist in namespace %q; an admin creates it, and "+
					"creating one is what grants this namespace access to the pool",
				name, md.Namespace),
		}, nil
	}

	observed := &modelDeploymentDomain{
		KVCache: &workercore.ModelDeploymentKVCacheStatus{
			Binding: kvcpb.Name,
			Pool:    kvcpb.Spec.PoolRef.Name,
			// Echoed from the Binding's spec, which requires and freezes all three, rather than from
			// its status: the domain is what an admin declared, and a projection that waited for the
			// Binding's own controller would report no domain during exactly the window the deployment
			// is asking which cache it is on.
			Domain: workercore.ModelDeploymentKVCacheDomain{
				Name:      kvcpb.Spec.Domain.Name,
				BlockSize: kvcpb.Spec.Domain.BlockSize,
				Dtype:     kvcpb.Spec.Domain.Dtype,
			},
		},
	}
	observed.Ready, observed.Reason, observed.Message = modelDeploymentBindingUsable(kvcpb)

	return observed, nil
}

// modelDeploymentBindingUsable reports whether a Binding can actually take a byte.
//
// It reads TWO independent facts, and the redundancy is the point — each covers the other's failure
// mode:
//
//   - The Binding's own phase, which its controller derives from every axis it has. Reading the
//     summary rather than a copy of the axis list means an axis added later is honored here without
//     this file changing; a copied list would go stale silently and report usable.
//   - The QuotaGranted axis directly. That axis exists because readiness once reported True both
//     when the master granted an effective quota of zero and when it carried no ledger entry at all,
//     so a Binding could read Ready while no write could succeed. This spec is what made that
//     reachable, so the one regression it was burned by is checked rather than delegated.
//
// A Binding on its way out is its own answer. It is not released here and must not be: the claim in
// usedBy is what holds the deletion, and dropping it would let the authorization vanish from under
// replicas that are still writing.
func modelDeploymentBindingUsable(kvcpb *workercore.KVCachePoolBinding) (bool, string, string) {
	if kvcpb.DeletionTimestamp != nil {
		return false, modelDeploymentReasonBindingDeleting, fmt.Sprintf(
			"kv cache pool binding %q is being deleted; the replicas keep serving and this deployment "+
				"still holds it, so the deletion waits until the deployment goes away",
			kvcpb.Name)
	}

	if kvcpb.Status.Phase != KVCachePoolPhaseReady {
		return false, modelDeploymentReasonBindingNotReady, fmt.Sprintf(
			"kv cache pool binding %q is %s: %s",
			kvcpb.Name, modelDeploymentBindingPhase(kvcpb), kvcpb.Status.PhaseMessage)
	}

	if !KVCachePoolBindingConditionQuotaGranted.IsTrue(kvcpb) {
		return false, modelDeploymentReasonBindingNotReady, fmt.Sprintf(
			"kv cache pool binding %q reports itself ready but has no quota to write into: %s",
			kvcpb.Name, KVCachePoolBindingConditionQuotaGranted.GetMessage(kvcpb))
	}

	return true, modelDeploymentReasonRegistered, fmt.Sprintf(
		"reuse domain %q is registered through kv cache pool binding %q on pool %q",
		kvcpb.Spec.Domain.Name, kvcpb.Name, kvcpb.Spec.PoolRef.Name)
}

// modelDeploymentBindingPhase spells the phase for a message, including the one case where there is
// none: a Binding whose own controller has not reached it yet has an empty phase, and "is : " reads
// like a bug in this operator rather than as a Binding nobody has looked at.
func modelDeploymentBindingPhase(kvcpb *workercore.KVCachePoolBinding) string {
	if kvcpb.Status.Phase == "" {
		return "not observed yet"
	}

	return kvcpb.Status.Phase
}

// observeModelDeploymentDomain folds one pass's reading of the Binding into the status.
//
// A nil reading means this pass did not look — a teardown pass does not — and then the condition and
// the projection are both left exactly as they were. Overwriting them with "not observed" would turn
// every teardown into a report that the domain had gone away.
func observeModelDeploymentDomain(holder *workercore.ModelDeployment, observed *modelDeploymentDomain) {
	if observed == nil {
		return
	}

	if observed.KVCache != nil {
		holder.Status.KVCache = observed.KVCache
	}

	if observed.Ready {
		ModelDeploymentConditionDomainRegistered.True(holder, observed.Reason, observed.Message)

		return
	}

	ModelDeploymentConditionDomainRegistered.False(holder, observed.Reason, observed.Message)
}

// claimModelDeploymentBinding records this deployment in the Binding's usedBy.
//
// The claim is what makes the Binding's deletion refusal real: that list is declared to have exactly
// one writer, this kind, and until something writes it the refusal enforces over a list that is
// always empty. It is written on every pass that can read the Binding, INCLUDING one where the
// Binding is not usable — a store leader restart makes every Binding briefly not-Ready, and a claim
// that came and went with readiness would open a window in which an admin's delete went through
// under replicas that are still writing.
func (r *ModelDeploymentReconciler) claimModelDeploymentBinding(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	return r.syncModelDeploymentBindingClaim(ctx, md, true)
}

// releaseModelDeploymentBinding drops the claim, so a Binding deleted after its deployments can
// finish. It is called only once the replicas are gone: released while one is still up, the
// authorization could vanish under a process that is still writing through it.
func (r *ModelDeploymentReconciler) releaseModelDeploymentBinding(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	return r.syncModelDeploymentBindingClaim(ctx, md, false)
}

func (r *ModelDeploymentReconciler) syncModelDeploymentBindingClaim(
	ctx context.Context, md *workercore.ModelDeployment, claim bool,
) error {
	kvcpb, err := r.getModelDeploymentBinding(ctx, md)
	if err != nil {
		return err
	}
	if kvcpb == nil {
		// Nothing to claim, and nothing to release either: a Binding that is gone took its list with
		// it. Reported through DomainRegistered rather than as an error, because a deployment whose
		// Binding was deleted keeps serving.
		return nil
	}

	ref := workercore.KVCacheObjectReference{
		Kind: ModelDeploymentKind,
		// Left empty by the field's own rule: everything that can appear here is in the Binding's
		// namespace, so naming it would restate the Binding's metadata on every entry.
		Namespace: "",
		Name:      md.Name,
	}

	at := slices.IndexFunc(kvcpb.Status.UsedBy,
		func(e workercore.KVCacheObjectReference) bool { return e == ref })
	if claim == (at >= 0) {
		// Already says what it should. This guard is what keeps a settled deployment from rewriting
		// another controller's object on every pass, which with the Binding's own reconciler writing
		// the same status would make every pass a conflict.
		return nil
	}

	if claim {
		kvcpb.Status.UsedBy = append(kvcpb.Status.UsedBy, ref)
		// One order for one state, so a Binding held by two deployments is not rewritten because they
		// claimed it in a different order than last time.
		slices.SortFunc(kvcpb.Status.UsedBy, compareKVCacheObjectReferences)
	} else {
		kvcpb.Status.UsedBy = slices.Delete(kvcpb.Status.UsedBy, at, at+1)
	}

	// A conflict goes back on the queue rather than being retried in place: the Binding's own
	// reconciler writes this status too, so the losing side's whole view of the object is stale by
	// then and retrying just the list would write a claim derived from something that has moved.
	return r.Client.Status().Update(ctx, kvcpb)
}

// getModelDeploymentBinding reads the referenced Binding, returning nil for one that does not exist.
//
// Absence is a value rather than an error because it is an ordinary, reportable state: admission
// refuses a poolRef naming no Binding, so the way to reach it is to delete the Binding under a
// running deployment, and that case keeps the replicas serving.
func (r *ModelDeploymentReconciler) getModelDeploymentBinding(
	ctx context.Context, md *workercore.ModelDeployment,
) (*workercore.KVCachePoolBinding, error) {
	kvcpb := new(workercore.KVCachePoolBinding)
	err := r.Client.Get(ctx,
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Spec.KVCache.PoolRef.Name},
		kvcpb, ctrlclix.WithoutQuorum)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get kv cache pool binding %s/%s: %w",
			md.Namespace, md.Spec.KVCache.PoolRef.Name, err)
	}

	return kvcpb, nil
}
