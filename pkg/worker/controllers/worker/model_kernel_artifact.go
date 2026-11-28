package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlcontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	// ModelKernelArtifactConditionResolved reports whether the kernel source is bound to its
	// immutable content identity and remains available.
	ModelKernelArtifactConditionResolved kubeapistatus.ConditionType = workercore.ModelKernelArtifactConditionTypeResolved
	// ModelKernelArtifactConditionDegraded reports whether resolution, revalidation, or source
	// availability is experiencing errors.
	ModelKernelArtifactConditionDegraded kubeapistatus.ConditionType = workercore.ModelKernelArtifactConditionTypeDegraded
	// ModelKernelArtifactConditionPinned reports whether a nodeStore kernel artifact digest is
	// pinned on all required NodeModelStore caches.
	ModelKernelArtifactConditionPinned kubeapistatus.ConditionType = workercore.ModelKernelArtifactConditionTypePinned

	kernelDispatchManifestFile = "dispatch-manifest.json"
)

var (
	kernelManifestDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	kernelCommitPattern         = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

// ModelKernelArtifactReconciler resolves ModelKernelArtifact sources (nodeStore, commit-pinned
// huggingFace, and digest-pinned image), aggregates counts-only status.nodes, reports Pinned
// condition state against NodeModelStore.spec.pinned, and maintains deletion protection while
// referenced by a ModelDeployment or ModelDeploymentForger.
type ModelKernelArtifactReconciler struct {
	Client   ctrlcli.Client
	Recorder ctrlrecord.EventRecorder
	Now      func() time.Time
	// NewHuggingFace builds the Hub client from the current Settings. Tests replace it.
	NewHuggingFace func(ctx context.Context) (modelArtifactHub, error)
	// ClusterVersion returns the apiserver version for image-volume gating. Tests replace it.
	ClusterVersion func() kubediscovery.Version

	checks       sync.Map
	nodesWritten sync.Map
}

var _ ctrlreconcile.Reconciler = (*ModelKernelArtifactReconciler)(nil)

func (r *ModelKernelArtifactReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	mka := new(workercore.ModelKernelArtifact)
	if err := r.Client.Get(ctx, req.NamespacedName, mka); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	if mka.DeletionTimestamp != nil {
		return ctrl.Result{}, r.releaseModelKernelArtifact(ctx, mka)
	}
	if !ctrlcontrollerutil.ContainsFinalizer(mka, workercore.ModelKernelArtifactProtectionFinalizer) {
		ctrlcontrollerutil.AddFinalizer(mka, workercore.ModelKernelArtifactProtectionFinalizer)
		if err := r.Client.Update(ctx, mka); err != nil {
			return ctrl.Result{}, err
		}
	}

	// For valid huggingFace sources, write Resolving alone before asking the Hub so long tree
	// listings remain observable, matching ModelArtifactReconciler.
	if mka.Spec.Source.HuggingFace != nil &&
		mka.Spec.Source.NodeStore == nil &&
		mka.Spec.Source.Image == nil &&
		kernelCommitPattern.MatchString(mka.Spec.Source.HuggingFace.Revision) &&
		!ModelKernelArtifactConditionResolved.Exists(mka) {
		mka.Status.ObservedGeneration = mka.Generation
		ModelKernelArtifactConditionResolved.Unknown(mka, workercore.ModelKernelArtifactReasonResolving,
			"the source has not answered yet")
		ModelKernelArtifactConditionPinned.False(mka, workercore.ModelKernelArtifactReasonUnpinned,
			"only nodeStore kernel artifacts are pinned in NodeModelStore")
		if err := r.Client.Status().Update(ctx, mka); err != nil {
			return objectWriteResult(logger, err, "update model kernel artifact status to resolving", ctrl.Result{})
		}
		return ctrl.Result{Requeue: true}, nil
	}

	before := mka.Status.DeepCopy()
	prevResolvedStatus := ModelKernelArtifactConditionResolved.GetStatus(mka)
	prevResolvedReason := ModelKernelArtifactConditionResolved.GetReason(mka)
	prevPinnedReason := ModelKernelArtifactConditionPinned.GetReason(mka)
	mka.Status.ObservedGeneration = mka.Generation

	var (
		stores    []workercore.NodeModelStore
		storesErr error
	)
	if mka.Spec.Source.NodeStore != nil || mka.Spec.Source.HuggingFace != nil {
		list := new(workercore.NodeModelStoreList)
		storesErr = r.Client.List(ctx, list)
		if storesErr == nil {
			stores = list.Items
		}
	}

	var (
		result ctrl.Result
		check  *modelArtifactCheck
	)
	switch countKernelSources(&mka.Spec.Source) {
	case 1:
		switch {
		case mka.Spec.Source.NodeStore != nil:
			result = r.reconcileNodeStore(mka, stores, storesErr, prevResolvedStatus, prevResolvedReason)
		case mka.Spec.Source.HuggingFace != nil:
			result, check = r.reconcileHuggingFace(ctx, mka, mka.Spec.Source.HuggingFace)
		case mka.Spec.Source.Image != nil:
			r.reconcileImage(mka)
		}
	default:
		mka.Status.Resolved = nil
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonUnsupportedSource,
			"exactly one of nodeStore, huggingFace, or image source is required")
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonUnsupportedSource,
			"exactly one of nodeStore, huggingFace, or image source is required")
	}

	resolvedChanged := ModelKernelArtifactConditionResolved.GetStatus(mka) != prevResolvedStatus ||
		ModelKernelArtifactConditionResolved.GetReason(mka) != prevResolvedReason
	result = earliestResult(result, r.reconcileNodes(mka, before.Nodes, stores, storesErr, resolvedChanged))

	if err := r.reconcilePinnedCondition(ctx, mka, stores, storesErr, prevPinnedReason); err != nil {
		return ctrl.Result{}, err
	}

	if !kubemeta.DeepEqual(before, &mka.Status) {
		if err := r.Client.Status().Update(ctx, mka); err != nil {
			r.checks.Delete(mka.UID)
			return objectWriteResult(logger, err, "update model kernel artifact status", ctrl.Result{})
		}
	}
	if check != nil {
		r.checks.Store(mka.UID, *check)
	}
	if !kubemeta.DeepEqual(before.Nodes, mka.Status.Nodes) {
		r.nodesWritten.Store(mka.UID, r.now())
	}

	return result, nil
}

func countKernelSources(src *workercore.ModelKernelArtifactSource) int {
	n := 0
	if src.NodeStore != nil {
		n++
	}
	if src.HuggingFace != nil {
		n++
	}
	if src.Image != nil {
		n++
	}
	return n
}

func (r *ModelKernelArtifactReconciler) releaseModelKernelArtifact(
	ctx context.Context, mka *workercore.ModelKernelArtifact,
) error {
	if !ctrlcontrollerutil.ContainsFinalizer(mka, workercore.ModelKernelArtifactProtectionFinalizer) {
		return nil
	}
	referrers, err := r.modelKernelArtifactReferrers(ctx, mka)
	if err != nil {
		return err
	}
	if len(referrers) > 0 {
		ctrllog.FromContext(ctx).V(4).Info("model kernel artifact is still referenced", "referrers", referrers)
		return nil
	}
	ctrlcontrollerutil.RemoveFinalizer(mka, workercore.ModelKernelArtifactProtectionFinalizer)
	if err := r.Client.Update(ctx, mka); err != nil {
		return err
	}
	r.checks.Delete(mka.UID)
	r.nodesWritten.Delete(mka.UID)
	return nil
}

func (r *ModelKernelArtifactReconciler) modelKernelArtifactReferrers(
	ctx context.Context, mka *workercore.ModelKernelArtifact,
) ([]string, error) {
	var referrers []string

	mds := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mds, ctrlcli.InNamespace(mka.Namespace)); err != nil {
		return nil, err
	}
	for i := range mds.Items {
		md := &mds.Items[i]
		for _, role := range md.Spec.Roles {
			if role.KernelArtifactRef != nil && role.KernelArtifactRef.Name == mka.Name {
				referrers = append(referrers, "ModelDeployment/"+md.Name)
				break
			}
		}
	}

	mdfs := new(workercore.ModelDeploymentForgerList)
	if err := r.Client.List(ctx, mdfs, ctrlcli.InNamespace(mka.Namespace)); err != nil {
		return nil, err
	}
	for i := range mdfs.Items {
		mdf := &mdfs.Items[i]
		for _, role := range mdf.Status.Roles {
			if role.PreviousKernelArtifact == mka.Name || role.ActiveKernelArtifact == mka.Name {
				referrers = append(referrers, "ModelDeploymentForger/"+mdf.Name)
				break
			}
		}
	}

	return referrers, nil
}

func validateNodeStoreSource(src *workercore.ModelKernelArtifactNodeStoreSource) (reason, message string) {
	if !kernelManifestDigestPattern.MatchString(src.ManifestDigest) {
		return workercore.ModelKernelArtifactReasonDigestMismatch,
			fmt.Sprintf("nodeStore.manifestDigest %q must be sha256 and 64 lowercase hex characters", src.ManifestDigest)
	}
	if !kernelManifestDigestPattern.MatchString(src.DispatchManifestDigest) {
		return workercore.ModelKernelArtifactReasonDigestMismatch,
			fmt.Sprintf("nodeStore.dispatchManifestDigest %q must be sha256 and 64 lowercase hex characters", src.DispatchManifestDigest)
	}
	if src.SizeBytes <= 0 {
		return workercore.ModelKernelArtifactReasonUnsupportedSource, "nodeStore.sizeBytes must be greater than zero"
	}
	if src.FileCount < 2 {
		return workercore.ModelKernelArtifactReasonUnsupportedSource, "nodeStore.fileCount must be at least 2"
	}
	if strings.TrimSpace(src.SeedNode) == "" {
		return workercore.ModelKernelArtifactReasonUnsupportedSource, "nodeStore.seedNode is required"
	}
	if strings.TrimSpace(src.ForgerRef.Name) == "" || strings.TrimSpace(src.ForgerRef.UID) == "" {
		return workercore.ModelKernelArtifactReasonUnsupportedSource, "nodeStore.forgerRef name and uid are required"
	}
	return "", ""
}

func (r *ModelKernelArtifactReconciler) reconcileNodeStore(
	mka *workercore.ModelKernelArtifact,
	stores []workercore.NodeModelStore,
	storesErr error,
	prevResolvedStatus, prevResolvedReason string,
) ctrl.Result {
	src := mka.Spec.Source.NodeStore
	if reason, msg := validateNodeStoreSource(src); reason != "" {
		mka.Status.Resolved = nil
		ModelKernelArtifactConditionResolved.False(mka, reason, msg)
		ModelKernelArtifactConditionDegraded.True(mka, reason, msg)
		return ctrl.Result{}
	}
	if storesErr != nil {
		ModelKernelArtifactConditionDegraded.True(mka, modelartifact.ReasonSourceUnavailable, storesErr.Error())
		return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}
	}

	agg := modelstore.AggregateEntries(modelstore.NodeEntries(stores, src.ManifestDigest))
	now := meta.NewTime(r.now())

	if agg.Ready >= 1 {
		if mka.Status.Resolved == nil {
			mka.Status.Resolved = &workercore.ModelKernelArtifactResolved{
				ManifestDigest:         src.ManifestDigest,
				DispatchManifestDigest: src.DispatchManifestDigest,
				FileCount:              src.FileCount,
				SizeBytes:              src.SizeBytes,
				ResolvedTime:           now,
				LastValidatedTime:      &now,
				DigestSource:           workercore.ModelKernelArtifactDigestSourceForge,
			}
		} else {
			mka.Status.Resolved.ManifestDigest = src.ManifestDigest
			mka.Status.Resolved.DispatchManifestDigest = src.DispatchManifestDigest
			mka.Status.Resolved.FileCount = src.FileCount
			mka.Status.Resolved.SizeBytes = src.SizeBytes
			mka.Status.Resolved.DigestSource = workercore.ModelKernelArtifactDigestSourceForge
			if prevResolvedStatus != string(meta.ConditionTrue) {
				mka.Status.Resolved.LastValidatedTime = &now
			}
		}
		ModelKernelArtifactConditionResolved.True(mka, workercore.ModelKernelArtifactReasonResolved,
			"kernel bundle is ready in NodeModelStore")
		ModelKernelArtifactConditionDegraded.False(mka, workercore.ModelKernelArtifactReasonHealthy, "")
		return ctrl.Result{}
	}

	// Previously resolved nodeStore bundle lost every Ready copy across the cluster.
	if mka.Status.Resolved != nil {
		msg := "no node holds a ready copy of the node-stored kernel bundle"
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonSourceLost, msg)
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonSourceLost, msg)
		if prevResolvedReason != workercore.ModelKernelArtifactReasonSourceLost && r.Recorder != nil {
			r.Recorder.Eventf(mka, core.EventTypeWarning, workercore.ModelKernelArtifactReasonSourceLost,
				"kernel bundle %s dropped to zero ready copies across NodeModelStores", src.ManifestDigest)
		}
		return ctrl.Result{}
	}

	// Newly created artifact waiting for seedNode's NodeModelStore status report to land.
	ModelKernelArtifactConditionResolved.Unknown(mka, workercore.ModelKernelArtifactReasonResolving,
		"waiting for a NodeModelStore to report a ready copy of the kernel bundle")
	if agg.Failed > 0 {
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonSourceLost,
			"one or more NodeModelStores reported a failed copy before initial resolution")
	} else {
		ModelKernelArtifactConditionDegraded.False(mka, workercore.ModelKernelArtifactReasonHealthy, "")
	}
	return ctrl.Result{}
}

func (r *ModelKernelArtifactReconciler) reconcileImage(mka *workercore.ModelKernelArtifact) {
	mka.Status.Resolved = nil
	mka.Status.Nodes = nil

	ver := r.clusterVersion()
	if !kubediscovery.SupportsFeature(&ver, kubediscovery.FeatureImageVolume) {
		msg := fmt.Sprintf(
			"cluster Kubernetes version %q does not serve image volumes; Kubernetes 1.35 or later is required",
			ver.GitVersion,
		)
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonUnsupportedSource, msg)
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonUnsupportedSource, msg)
		return
	}

	reference := mka.Spec.Source.Image.Reference
	ref, digest, pinned := strings.Cut(reference, "@")
	if !pinned || ref == "" || strings.ContainsAny(ref, "@ \t\n\r") || !kernelManifestDigestPattern.MatchString(digest) {
		msg := "image reference must pin a sha256 digest (registry/repository@sha256:<64 hex>)"
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonUnsupportedSource, msg)
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonUnsupportedSource, msg)
		return
	}

	ModelKernelArtifactConditionResolved.True(mka, workercore.ModelKernelArtifactReasonResolved,
		"the image is the user's; the operator does not read it")
	ModelKernelArtifactConditionDegraded.False(mka, workercore.ModelKernelArtifactReasonHealthy, "")
}

func (r *ModelKernelArtifactReconciler) reconcileHuggingFace(
	ctx context.Context, mka *workercore.ModelKernelArtifact, source *workercore.ModelArtifactHubSource,
) (ctrl.Result, *modelArtifactCheck) {
	if !kernelCommitPattern.MatchString(source.Revision) {
		msg := fmt.Sprintf("huggingFace revision %q must be a full 40-character lowercase hexadecimal commit SHA", source.Revision)
		mka.Status.Resolved = nil
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonUnsupportedSource, msg)
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonUnsupportedSource, msg)
		return ctrl.Result{}, nil
	}

	token, secretVersion, err := r.kernelArtifactToken(ctx, mka.Namespace, source.SecretRef)
	switch {
	case errors.Is(err, errModelArtifactSecretMissing):
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonSecretNotFound, err.Error())
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonSecretNotFound, err.Error())
		return ctrl.Result{RequeueAfter: modelArtifactRefusedRetry}, nil
	case err != nil:
		ModelKernelArtifactConditionDegraded.True(mka, modelartifact.ReasonSourceUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}, nil
	}

	now := r.now()
	secretChanged := true
	if last, ok := r.checks.Load(mka.UID); ok {
		check := last.(modelArtifactCheck)
		secretChanged = check.secretVersion != secretVersion
		if !secretChanged && now.Before(check.next) {
			return ctrl.Result{RequeueAfter: check.next.Sub(now)}, nil
		}
	}

	hub, err := r.newHuggingFace(ctx)
	if err != nil {
		ModelKernelArtifactConditionDegraded.True(mka, modelartifact.ReasonSourceUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, modelArtifactResolveTimeout)
	defer cancel()

	var after time.Duration
	if mka.Status.Resolved == nil {
		after = r.resolveHuggingFace(ctx, mka, hub, source, token, now)
	} else {
		after = r.revalidateHuggingFace(ctx, mka, hub, source, token, now, secretChanged)
	}

	return ctrl.Result{RequeueAfter: after}, &modelArtifactCheck{secretVersion: secretVersion, next: now.Add(after)}
}

func (r *ModelKernelArtifactReconciler) resolveHuggingFace(
	ctx context.Context, mka *workercore.ModelKernelArtifact, hub modelArtifactHub,
	source *workercore.ModelArtifactHubSource, token string, now time.Time,
) time.Duration {
	resolution, err := hub.Resolve(ctx, source.Repository, source.Revision, token, modelartifact.Filter{})
	if err != nil {
		reason := modelartifact.ReasonOf(err)
		ModelKernelArtifactConditionResolved.False(mka, reason, err.Error())
		ModelKernelArtifactConditionDegraded.True(mka, reason, err.Error())
		if reason == modelartifact.ReasonSourceUnavailable {
			ModelKernelArtifactConditionDegraded.LastTransitionTime(mka, now.UTC().Format(time.RFC3339))
			return modelArtifactUnavailableRetry
		}
		return modelArtifactRefusedRetry
	}

	if resolution.Commit != source.Revision {
		msg := fmt.Sprintf("the artifact pins commit %s, but the hub resolved %s@%s to %s",
			source.Revision, source.Repository, source.Revision, resolution.Commit)
		ModelKernelArtifactConditionResolved.False(mka, workercore.ModelKernelArtifactReasonDigestMismatch, msg)
		ModelKernelArtifactConditionDegraded.True(mka, workercore.ModelKernelArtifactReasonDigestMismatch, msg)
		return modelArtifactRefusedRetry
	}

	dispatchDigest, ok := findDispatchManifestDigest(resolution.Manifest.Entries)
	if !ok {
		msg := fmt.Sprintf("commit %s of %q does not contain %s at repository root",
			resolution.Commit, source.Repository, kernelDispatchManifestFile)
		ModelKernelArtifactConditionResolved.False(mka, modelartifact.ReasonInvalidManifest, msg)
		ModelKernelArtifactConditionDegraded.True(mka, modelartifact.ReasonInvalidManifest, msg)
		return modelArtifactRefusedRetry
	}

	resolvedAt := meta.NewTime(now)
	mka.Status.Resolved = &workercore.ModelKernelArtifactResolved{
		ManifestDigest:         resolution.Manifest.Digest,
		DispatchManifestDigest: dispatchDigest,
		Revision:               resolution.Commit,
		FileCount:              resolution.Manifest.FileCount,
		SizeBytes:              resolution.Manifest.SizeBytes,
		ResolvedTime:           resolvedAt,
		LastValidatedTime:      &resolvedAt,
		DigestSource:           workercore.ModelKernelArtifactDigestSourceHub,
	}
	ModelKernelArtifactConditionResolved.True(mka, workercore.ModelKernelArtifactReasonResolved,
		fmt.Sprintf("%s@%s resolved to commit %s", source.Repository, source.Revision, resolution.Commit))
	ModelKernelArtifactConditionDegraded.False(mka, workercore.ModelKernelArtifactReasonHealthy, "")
	r.warnOnRejectedToken(ctx, mka, source, hub, token)

	return r.revalidateInterval(ctx)
}

func (r *ModelKernelArtifactReconciler) revalidateHuggingFace(
	ctx context.Context, mka *workercore.ModelKernelArtifact, hub modelArtifactHub,
	source *workercore.ModelArtifactHubSource, token string, now time.Time, secretChanged bool,
) time.Duration {
	interval := r.revalidateInterval(ctx)
	if last := mka.Status.Resolved.LastValidatedTime; last != nil &&
		ModelKernelArtifactConditionResolved.IsTrue(mka) &&
		!ModelKernelArtifactConditionDegraded.IsTrue(mka) &&
		now.Before(last.Add(interval)) {
		if _, ok := r.checks.Load(mka.UID); !ok {
			return last.Add(interval).Sub(now)
		}
	}

	manifest, err := hub.ListManifest(ctx, source.Repository, mka.Status.Resolved.Revision, token, modelartifact.Filter{})
	if err == nil {
		if _, hasDispatch := findDispatchManifestDigest(manifest.Entries); !hasDispatch {
			err = &modelartifact.SourceError{
				Reason: modelartifact.ReasonInvalidManifest,
				Message: fmt.Sprintf("commit %s of %q no longer contains %s at repository root",
					mka.Status.Resolved.Revision, source.Repository, kernelDispatchManifestFile),
			}
		} else if manifest.Digest != mka.Status.Resolved.ManifestDigest {
			err = &modelartifact.SourceError{
				Reason: workercore.ModelKernelArtifactReasonDigestMismatch,
				Message: fmt.Sprintf("the artifact resolved %s, the hub now lists %s@%s as %s",
					mka.Status.Resolved.ManifestDigest, source.Repository, mka.Status.Resolved.Revision, manifest.Digest),
			}
		}
	}

	if err == nil {
		validated := meta.NewTime(now)
		mka.Status.Resolved.LastValidatedTime = &validated
		ModelKernelArtifactConditionResolved.True(mka, workercore.ModelKernelArtifactReasonResolved,
			"access confirmed at the resolved commit")
		ModelKernelArtifactConditionDegraded.False(mka, workercore.ModelKernelArtifactReasonHealthy, "")
		if secretChanged {
			r.warnOnRejectedToken(ctx, mka, source, hub, token)
		}
		return interval
	}

	reason := modelartifact.ReasonOf(err)
	if !modelArtifactRevokingReason(reason) && reason != modelartifact.ReasonInvalidManifest {
		ModelKernelArtifactConditionDegraded.True(mka, reason, err.Error())
		return modelArtifactUnavailableRetry
	}

	first := !ModelKernelArtifactConditionDegraded.IsTrue(mka) ||
		ModelKernelArtifactConditionDegraded.GetReason(mka) != reason
	if first {
		ModelKernelArtifactConditionDegraded.True(mka, reason,
			fmt.Sprintf("%v; access is checked again in %s and revoked if refused again", err, modelArtifactConfirmDelay))
		ModelKernelArtifactConditionDegraded.LastTransitionTime(mka, now.UTC().Format(time.RFC3339))
		return modelArtifactConfirmDelay
	}
	if wait := modelKernelArtifactConditionSince(mka, ModelKernelArtifactConditionDegraded).Add(modelArtifactConfirmDelay).Sub(now); wait > 0 {
		return wait
	}
	ModelKernelArtifactConditionResolved.False(mka, reason, err.Error())
	ModelKernelArtifactConditionDegraded.True(mka, reason, err.Error())

	return modelArtifactRefusedRetry
}

func findDispatchManifestDigest(entries []modelartifact.ManifestEntry) (digest string, ok bool) {
	for _, e := range entries {
		if e.Path == kernelDispatchManifestFile {
			if strings.HasPrefix(e.Digest, modelartifact.DigestSHA256+":") {
				return e.Digest, true
			}
			return "", true
		}
	}
	return "", false
}

func modelKernelArtifactConditionSince(mka *workercore.ModelKernelArtifact, c kubeapistatus.ConditionType) time.Time {
	for _, cond := range mka.Status.Conditions {
		if cond.Type == string(c) {
			return cond.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

func (r *ModelKernelArtifactReconciler) reconcileNodes(
	mka *workercore.ModelKernelArtifact,
	stored *workercore.ModelArtifactNodes,
	stores []workercore.NodeModelStore,
	storesErr error,
	forceWrite bool,
) ctrl.Result {
	digest := kernelArtifactNodeDigest(mka)
	if digest == "" {
		mka.Status.Nodes = nil
		return ctrl.Result{}
	}
	if storesErr != nil {
		mka.Status.Nodes = stored
		return ctrl.Result{RequeueAfter: modelArtifactNodesWindow}
	}

	agg := modelstore.AggregateEntries(modelstore.NodeEntries(stores, digest))
	next := &workercore.ModelArtifactNodes{
		Ready:              agg.Ready,
		Downloading:        agg.Downloading,
		Failed:             agg.Failed,
		DownloadingPercent: agg.SteppedPercent(),
	}
	if kubemeta.DeepEqual(next, stored) {
		mka.Status.Nodes = stored
		return ctrl.Result{}
	}
	if !forceWrite {
		if last, ok := r.nodesWritten.Load(mka.UID); ok {
			if wait := modelArtifactNodesWindow - r.now().Sub(last.(time.Time)); wait > 0 {
				mka.Status.Nodes = stored
				return ctrl.Result{RequeueAfter: wait}
			}
		}
	}
	mka.Status.Nodes = next
	return ctrl.Result{}
}

func kernelArtifactNodeDigest(mka *workercore.ModelKernelArtifact) string {
	switch {
	case mka.Spec.Source.NodeStore != nil:
		if reason, _ := validateNodeStoreSource(mka.Spec.Source.NodeStore); reason != "" {
			return ""
		}
		return mka.Spec.Source.NodeStore.ManifestDigest
	case mka.Spec.Source.HuggingFace != nil && mka.Status.Resolved != nil:
		return mka.Status.Resolved.ManifestDigest
	default:
		return ""
	}
}

func (r *ModelKernelArtifactReconciler) reconcilePinnedCondition(
	ctx context.Context,
	mka *workercore.ModelKernelArtifact,
	stores []workercore.NodeModelStore,
	storesErr error,
	prevPinnedReason string,
) error {
	if mka.Spec.Source.NodeStore == nil || countKernelSources(&mka.Spec.Source) != 1 {
		ModelKernelArtifactConditionPinned.False(mka, workercore.ModelKernelArtifactReasonUnpinned,
			"only nodeStore kernel artifacts are pinned in NodeModelStore")
		return nil
	}
	if reason, _ := validateNodeStoreSource(mka.Spec.Source.NodeStore); reason != "" {
		ModelKernelArtifactConditionPinned.False(mka, workercore.ModelKernelArtifactReasonUnpinned,
			"kernel artifact source is invalid and cannot be pinned")
		return nil
	}
	if storesErr != nil {
		return nil //nolint:nilerr // store list failure is already reported on Degraded and requeued
	}

	deps := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, deps, ctrlcli.InNamespace(mka.Namespace)); err != nil {
		return err
	}
	forgers := new(workercore.ModelDeploymentForgerList)
	if err := r.Client.List(ctx, forgers, ctrlcli.InNamespace(mka.Namespace)); err != nil {
		return err
	}
	pods := new(core.PodList)
	if err := r.Client.List(ctx, pods,
		ctrlcli.InNamespace(mka.Namespace),
		ctrlcli.MatchingLabels{modelDeploymentLabelKeyName: modelDeploymentLabelValueName}); err != nil {
		return err
	}

	requiredNodes := kernelArtifactRequiredNodes(mka, stores, deps.Items, forgers.Items, pods.Items)
	storeByName := make(map[string]*workercore.NodeModelStore, len(stores))
	for i := range stores {
		storeByName[stores[i].Name] = &stores[i]
	}

	digest := mka.Spec.Source.NodeStore.ManifestDigest
	var pinnedCount, capExceededCount int
	for _, node := range requiredNodes {
		nms := storeByName[node]
		if nms == nil {
			continue
		}
		if slices.Contains(nms.Spec.Pinned, digest) {
			pinnedCount++
			continue
		}
		if len(nms.Spec.Pinned) >= nodeModelStoreMaxPinned {
			capExceededCount++
		}
	}

	switch {
	case capExceededCount > 0:
		msg := fmt.Sprintf("%d target NodeModelStore(s) reached the %d-entry pin cap before admitting this kernel artifact",
			capExceededCount, nodeModelStoreMaxPinned)
		ModelKernelArtifactConditionPinned.False(mka, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded, msg)
		if prevPinnedReason != workercore.ModelKernelArtifactReasonPinnedCapacityExceeded && r.Recorder != nil {
			r.Recorder.Eventf(mka, core.EventTypeWarning,
				workercore.ModelKernelArtifactReasonPinnedCapacityExceeded, "%s", msg)
		}
	case len(requiredNodes) > 0 && pinnedCount == len(requiredNodes):
		ModelKernelArtifactConditionPinned.True(mka, workercore.ModelKernelArtifactReasonPinned,
			"kernel digest is pinned on all target NodeModelStores")
	default:
		ModelKernelArtifactConditionPinned.False(mka, workercore.ModelKernelArtifactReasonUnpinned,
			"kernel digest is not yet pinned on all target NodeModelStores")
	}

	return nil
}

func (r *ModelKernelArtifactReconciler) warnOnRejectedToken(
	ctx context.Context, mka *workercore.ModelKernelArtifact,
	source *workercore.ModelArtifactHubSource, hub modelArtifactHub, token string,
) {
	if token == "" || r.Recorder == nil || source.SecretRef == nil {
		return
	}
	valid, err := hub.ValidToken(ctx, token)
	if err != nil || valid {
		return
	}
	r.Recorder.Eventf(mka, core.EventTypeWarning, "InvalidToken",
		"the hub rejects the token in Secret %q; a public repository still resolves, as if no token had been sent",
		source.SecretRef.Name)
}

func (r *ModelKernelArtifactReconciler) kernelArtifactToken(
	ctx context.Context, namespace string, ref *core.LocalObjectReference,
) (token, version string, err error) {
	if ref == nil {
		return "", "", nil
	}
	secret := new(core.Secret)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		if kerrors.IsNotFound(err) {
			return "", "", fmt.Errorf("%w: Secret %q does not exist in this namespace", errModelArtifactSecretMissing, ref.Name)
		}
		return "", "", fmt.Errorf("read Secret %q: %w", ref.Name, err)
	}
	token = strings.TrimSpace(string(secret.Data[modelArtifactTokenKey]))
	if token == "" {
		return "", "", fmt.Errorf("%w: Secret %q has no %q key", errModelArtifactSecretMissing, ref.Name, modelArtifactTokenKey)
	}
	return token, secret.ResourceVersion, nil
}

func (r *ModelKernelArtifactReconciler) revalidateInterval(ctx context.Context) time.Duration {
	d, err := time.ParseDuration(settings.ModelArtifactRevalidateInterval.ShouldValue(ctx))
	if err != nil || d < time.Minute {
		return 24 * time.Hour
	}
	return d
}

func (r *ModelKernelArtifactReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ModelKernelArtifactReconciler) clusterVersion() kubediscovery.Version {
	if r.ClusterVersion != nil {
		return r.ClusterVersion()
	}
	return system.LoopbackKubeVersion.Get()
}

func (r *ModelKernelArtifactReconciler) newHuggingFace(ctx context.Context) (modelArtifactHub, error) {
	if r.NewHuggingFace != nil {
		return r.NewHuggingFace(ctx)
	}
	return r.newHuggingFaceFromSettings(ctx)
}

func (r *ModelKernelArtifactReconciler) newHuggingFaceFromSettings(ctx context.Context) (modelArtifactHub, error) {
	client, err := r.newHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	return &modelartifact.HuggingFace{
		Endpoint: settings.ModelArtifactHuggingFaceEndpoint.ShouldValue(ctx),
		Client:   client,
	}, nil
}

func (r *ModelKernelArtifactReconciler) newHubHTTPClient(ctx context.Context) (*http.Client, error) {
	opts := modelartifact.HTTPClientOptions{
		HTTPSProxy: settings.ModelArtifactHTTPSProxy.ShouldValue(ctx),
		NoProxy:    settings.ModelArtifactNoProxy.ShouldValue(ctx),
	}
	if name := settings.ModelArtifactCABundle.ShouldValue(ctx); name != "" {
		cm := new(core.ConfigMap)
		if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: name}, cm); err != nil {
			return nil, fmt.Errorf("read the CA bundle ConfigMap %q: %w", name, err)
		}
		opts.CABundle = []byte(cm.Data["ca.crt"])
	}
	return modelartifact.NewHTTPClient(opts)
}

func (r *ModelKernelArtifactReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.Recorder = opts.Manager.GetEventRecorderFor("modelkernelartifact")
	if r.NewHuggingFace == nil {
		r.NewHuggingFace = r.newHuggingFaceFromSettings
	}
	if r.ClusterVersion == nil {
		r.ClusterVersion = system.LoopbackKubeVersion.Get
	}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modelkernelartifact").
		For(&workercore.ModelKernelArtifact{}).
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: modelArtifactMaxConcurrentReconciles}).
		Watches(&core.Secret{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueKernelArtifactsForSecret)).
		Watches(&workercore.NodeModelStore{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.mapModelKernelArtifactNodeModelStore),
			ctrlbuilder.WithPredicates(nodeKernelReportChanged)).
		Watches(&workercore.ModelDeployment{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueKernelArtifactsForDeployment),
			ctrlbuilder.WithPredicates(modelDeploymentKernelRefsChanged)).
		Watches(&workercore.ModelDeploymentForger{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueKernelArtifactsForForger),
			ctrlbuilder.WithPredicates(modelDeploymentForgerKernelRefsChanged)).
		Watches(&core.Pod{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueKernelArtifactsForDeploymentPod),
			ctrlbuilder.WithPredicates(modelDeploymentPodPinChanged)).
		Complete(r)
}

var nodeKernelReportChanged = ctrlpredicate.Funcs{
	CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
	DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.NodeModelStore)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.NodeModelStore)
		if !ok {
			return false
		}
		return !kubemeta.DeepEqual(o.Status.Models, n.Status.Models) ||
			!slices.Equal(o.Spec.Pinned, n.Spec.Pinned)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

func modelDeploymentForgerKernelRefs(mdf *workercore.ModelDeploymentForger) map[string]string {
	refs := map[string]string{}
	if mdf == nil {
		return refs
	}
	for _, role := range mdf.Status.Roles {
		if role.ActiveKernelArtifact != "" || role.PreviousKernelArtifact != "" {
			refs[role.Name] = role.ActiveKernelArtifact + "|" + role.PreviousKernelArtifact
		}
	}
	return refs
}

var modelDeploymentForgerKernelRefsChanged = ctrlpredicate.Funcs{
	CreateFunc: func(e ctrlevent.CreateEvent) bool {
		mdf, ok := e.Object.(*workercore.ModelDeploymentForger)
		return ok && len(modelDeploymentForgerKernelRefs(mdf)) > 0
	},
	DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
		mdf, ok := e.Object.(*workercore.ModelDeploymentForger)
		return ok && len(modelDeploymentForgerKernelRefs(mdf)) > 0
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
		return !maps.Equal(modelDeploymentForgerKernelRefs(o), modelDeploymentForgerKernelRefs(n)) ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

func (r *ModelKernelArtifactReconciler) enqueueKernelArtifactsForSecret(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	return r.enqueueKernelArtifacts(ctx, obj.GetNamespace(), func(mka *workercore.ModelKernelArtifact) bool {
		hub := mka.Spec.Source.HuggingFace
		return hub != nil && hub.SecretRef != nil && hub.SecretRef.Name == obj.GetName()
	})
}

func (r *ModelKernelArtifactReconciler) mapModelKernelArtifactNodeModelStore(
	ctx context.Context, _ ctrlcli.Object,
) []ctrlreconcile.Request {
	list := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, list); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model kernel artifacts for node model store")
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range list.Items {
		mka := &list.Items[i]
		resolvedHub := mka.Spec.Source.HuggingFace != nil &&
			mka.Status.Resolved != nil &&
			mka.Status.Resolved.ManifestDigest != ""
		if mka.Spec.Source.NodeStore != nil || resolvedHub {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mka)})
		}
	}
	return reqs
}

func (r *ModelKernelArtifactReconciler) enqueueKernelArtifactsForDeployment(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	return r.enqueueKernelArtifacts(ctx, obj.GetNamespace(), func(mka *workercore.ModelKernelArtifact) bool {
		return mka.DeletionTimestamp != nil || mka.Spec.Source.NodeStore != nil
	})
}

func (r *ModelKernelArtifactReconciler) enqueueKernelArtifactsForForger(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	return r.enqueueKernelArtifacts(ctx, obj.GetNamespace(), func(mka *workercore.ModelKernelArtifact) bool {
		return mka.DeletionTimestamp != nil || mka.Spec.Source.NodeStore != nil
	})
}

func (r *ModelKernelArtifactReconciler) enqueueKernelArtifactsForDeploymentPod(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	return r.enqueueKernelArtifacts(ctx, obj.GetNamespace(), func(mka *workercore.ModelKernelArtifact) bool {
		return mka.Spec.Source.NodeStore != nil
	})
}

func (r *ModelKernelArtifactReconciler) enqueueKernelArtifacts(
	ctx context.Context, namespace string, match func(*workercore.ModelKernelArtifact) bool,
) []ctrlreconcile.Request {
	list := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(namespace)); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model kernel artifacts")
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range list.Items {
		if match(&list.Items[i]) {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}
