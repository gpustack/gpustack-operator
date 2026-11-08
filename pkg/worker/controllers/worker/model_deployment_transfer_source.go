package worker

import (
	"context"
	"fmt"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const (
	modelPrefetchArtifactIndex   = "spec.artifactRef.name"
	modelDeploymentArtifactIndex = "modelTransfer.artifact"
	modelDeploymentAnyArtifact   = "*"
)

func modelPrefetchArtifactIndexValues(obj ctrlcli.Object) []string {
	pf, ok := obj.(*workercore.ModelPrefetch)
	if !ok || pf.Spec.ArtifactRef.Name == "" {
		return nil
	}
	return []string{pf.Spec.ArtifactRef.Name}
}

func modelDeploymentArtifactIndexValues(obj ctrlcli.Object) []string {
	md, ok := obj.(*workercore.ModelDeployment)
	if !ok || ModelDeploymentArtifactName(md) == "" {
		return nil
	}
	return []string{ModelDeploymentArtifactName(md), modelDeploymentAnyArtifact}
}

func modelPrefetchPublishedServing(pf *workercore.ModelPrefetch, now time.Time) *workercore.ModelPrefetchTransferServing {
	if pf.UID == "" || pf.Generation < 1 || pf.DeletionTimestamp != nil || pf.Spec.Source == nil ||
		pf.Status.Transfer == nil || pf.Status.Transfer.Serving == nil {
		return nil
	}
	proof := pf.Status.Transfer.Serving
	if proof.ArtifactUID == "" || proof.ArtifactGeneration < 1 || proof.Digest == "" || !proof.ReadyLeaseExpiresAt.After(now) {
		return nil
	}
	for _, condition := range pf.Status.Conditions {
		if condition.Type == ModelPrefetchConditionSourceReady && condition.Status == meta.ConditionTrue &&
			condition.ObservedGeneration == pf.Generation {
			return proof
		}
	}
	return nil
}

// modelDeploymentModelTransferSourceObserved consumes only the owning prefetch's
// current identity and lease proof. Broker admission still validates authority live.
func modelDeploymentModelTransferSourceObserved(
	ctx context.Context, cli ctrlcli.Reader, artifact *workercore.ModelArtifact,
) (bool, time.Time, error) {
	if artifact == nil || artifact.UID == "" || artifact.Generation < 1 || artifact.Status.Resolved == nil ||
		artifact.Status.ObservedGeneration != artifact.Generation || artifact.Status.Resolved.ManifestDigest == "" {
		return false, time.Time{}, nil
	}
	prefetches := new(workercore.ModelPrefetchList)
	if err := cli.List(ctx, prefetches, ctrlcli.InNamespace(artifact.Namespace),
		ctrlcli.MatchingFields{modelPrefetchArtifactIndex: artifact.Name}); err != nil {
		return false, time.Time{}, fmt.Errorf("read source prefetch observations: %w", err)
	}
	now := time.Now()
	var until time.Time
	for i := range prefetches.Items {
		pf := &prefetches.Items[i]
		if pf.Namespace != artifact.Namespace || pf.Spec.ArtifactRef.Name != artifact.Name {
			continue
		}
		proof := modelPrefetchPublishedServing(pf, now)
		if proof == nil || proof.ArtifactUID != string(artifact.UID) || proof.ArtifactGeneration != artifact.Generation ||
			proof.Digest != artifact.Status.Resolved.ManifestDigest {
			continue
		}
		if until.IsZero() || proof.ReadyLeaseExpiresAt.Before(&meta.Time{Time: until}) {
			until = proof.ReadyLeaseExpiresAt.Time
		}
	}
	return !until.IsZero(), until, nil
}

func modelTransferSourceLeaseRequeue(result ctrl.Result, until, now time.Time) ctrl.Result {
	if until.IsZero() || result.Requeue {
		return result
	}
	delay := max(until.Sub(now), time.Millisecond)
	if result.RequeueAfter <= 0 || delay < result.RequeueAfter {
		result.RequeueAfter = delay
	}
	return result
}

type modelPrefetchSourceReadinessKey struct {
	artifact, uid, artifactUID, digest string
	generation, artifactGeneration     int64
	enabled, deleting, ready           bool
}

func modelPrefetchSourceKey(pf *workercore.ModelPrefetch, now time.Time) modelPrefetchSourceReadinessKey {
	if pf.Spec.Source == nil && pf.Status.Transfer == nil {
		return modelPrefetchSourceReadinessKey{}
	}
	key := modelPrefetchSourceReadinessKey{
		artifact: pf.Spec.ArtifactRef.Name, uid: string(pf.UID), generation: pf.Generation,
		enabled: pf.Spec.Source != nil, deleting: pf.DeletionTimestamp != nil,
		ready: modelPrefetchPublishedServing(pf, now) != nil,
	}
	if pf.Status.Transfer != nil && pf.Status.Transfer.Serving != nil {
		proof := pf.Status.Transfer.Serving
		key.artifactUID, key.artifactGeneration, key.digest = proof.ArtifactUID, proof.ArtifactGeneration, proof.Digest
	}
	return key
}

func modelPrefetchSourceReadinessChanged() ctrlpredicate.Predicate {
	selected := func(obj ctrlcli.Object) bool {
		pf, ok := obj.(*workercore.ModelPrefetch)
		return ok && (pf.Spec.Source != nil || pf.Status.Transfer != nil)
	}
	return ctrlpredicate.Funcs{
		CreateFunc:  func(e ctrlevent.CreateEvent) bool { return selected(e.Object) },
		DeleteFunc:  func(e ctrlevent.DeleteEvent) bool { return selected(e.Object) },
		GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*workercore.ModelPrefetch)
			next, nextOK := e.ObjectNew.(*workercore.ModelPrefetch)
			if !oldOK || !nextOK {
				return false
			}
			// Object events update the serving proof; modelTransferSourceLeaseRequeue
			// supplies the expiry signal when no object changes.
			now := time.Now()
			return modelPrefetchSourceKey(old, now) != modelPrefetchSourceKey(next, now)
		},
	}
}

func (r *ModelDeploymentReconciler) mapModelDeploymentSourcePrefetch(ctx context.Context, obj ctrlcli.Object) ([]ctrlreconcile.Request, error) {
	pf, ok := obj.(*workercore.ModelPrefetch)
	if !ok || pf.Spec.ArtifactRef.Name == "" {
		return nil, nil
	}
	return r.modelDeploymentArtifactConsumers(ctx, pf.Namespace, pf.Spec.ArtifactRef.Name)
}

func (r *ModelDeploymentReconciler) mapModelDeploymentTransferSources(ctx context.Context, _ ctrlcli.Object) ([]ctrlreconcile.Request, error) {
	return r.modelDeploymentArtifactConsumers(ctx, "", modelDeploymentAnyArtifact)
}

func (r *ModelDeploymentReconciler) modelDeploymentArtifactConsumers(
	ctx context.Context, namespace, artifact string,
) ([]ctrlreconcile.Request, error) {
	deployments := new(workercore.ModelDeploymentList)
	options := []ctrlcli.ListOption{ctrlcli.MatchingFields{modelDeploymentArtifactIndex: artifact}}
	if namespace != "" {
		options = append(options, ctrlcli.InNamespace(namespace))
	}
	if err := r.Client.List(ctx, deployments, options...); err != nil {
		return nil, fmt.Errorf("list artifact consumers for a source change: %w", err)
	}
	requests := make([]ctrlreconcile.Request, 0, len(deployments.Items))
	for i := range deployments.Items {
		md := &deployments.Items[i]
		if md.DeletionTimestamp == nil && ModelDeploymentArtifactName(md) != "" {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		}
	}
	return requests, nil
}
