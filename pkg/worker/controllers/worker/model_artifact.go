package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlcontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	// ModelArtifactConditionResolved says the source is bound to its immutable identity and the most
	// recent access check passed. Consumers create no new Pod while it is not True.
	ModelArtifactConditionResolved kubeapistatus.ConditionType = "Resolved"
	// ModelArtifactConditionDegraded says a resolution or an access check is failing, including one
	// that has not yet revoked access.
	ModelArtifactConditionDegraded kubeapistatus.ConditionType = "Degraded"

	// ModelArtifactProtectionFinalizer keeps a referenced artifact in Terminating until its last
	// reference is gone, the shape of PVC protection.
	ModelArtifactProtectionFinalizer = "worker.gpustack.ai/model-artifact-protection"

	modelArtifactReasonResolved      = "Resolved"
	modelArtifactReasonResolving     = "Resolving"
	modelArtifactReasonSecretMissing = "SecretNotFound"
	modelArtifactReasonClaimMissing  = "ClaimNotFound"
	modelArtifactReasonHealthy       = "Healthy"

	// modelArtifactTokenKey is the Secret key the token is read from, the convention TopologySource
	// already uses for a bearer token.
	modelArtifactTokenKey = "token"

	// modelArtifactConfirmDelay is how long a first failed access check waits before the second one
	// that revokes. One failure alone never revokes: a single refused request is not yet a revoked
	// grant.
	modelArtifactConfirmDelay = time.Minute
	// modelArtifactUnavailableRetry paces retries while the source cannot be reached at all.
	modelArtifactUnavailableRetry = time.Minute
	// modelArtifactRefusedRetry paces retries of a resolution the source refused. The fix is usually
	// a Secret change, which is watched; a grant made on the Hub's side is not, so it is retried too.
	modelArtifactRefusedRetry = 10 * time.Minute
	// modelArtifactMaxConcurrentReconciles bounds how many artifacts talk to the Hub at once.
	modelArtifactMaxConcurrentReconciles = 4
	// modelArtifactResolveTimeout bounds one whole resolution, every tree page included.
	modelArtifactResolveTimeout = 5 * time.Minute
)

// ModelArtifactReconciler resolves ModelArtifacts, revalidates their access and protects them from
// deletion while referenced.
type ModelArtifactReconciler struct {
	Client   ctrlcli.Client
	Recorder ctrlrecord.EventRecorder
	Now      func() time.Time
	// NewHuggingFace builds the Hub client from the current Settings. Tests replace it.
	NewHuggingFace func(ctx context.Context) (*modelartifact.HuggingFace, error)
	// NewModelScope builds the ModelScope client from the current Settings. Tests replace it.
	NewModelScope func(ctx context.Context) (modelArtifactHub, error)

	// checks remembers, per artifact UID, the Secret resourceVersion the source was last asked
	// with and when it may be asked next. It paces the calls to the Hub: this controller also runs
	// on its own status writes and on every change of a referencing deployment, and none of those
	// is a reason to ask the Hub again. A changed Secret is. Losing it on restart costs one early
	// check per artifact, nothing else: whether access is revoked is decided from the conditions.
	checks sync.Map
	// nodesWritten remembers, per artifact UID, when its status.nodes was last written, for the
	// window between two such writes. Losing it on restart costs one early write per artifact.
	nodesWritten sync.Map
}

var _ ctrlreconcile.Reconciler = (*ModelArtifactReconciler)(nil)

// modelArtifactHub is what a hub source resolves and revalidates through. The two hubs share one
// staircase; only the endpoints and the reason classifier differ, and those live in the clients.
type modelArtifactHub interface {
	Resolve(ctx context.Context, repository, revision, token string, filter modelartifact.Filter) (modelartifact.Resolution, error)
	ListManifest(ctx context.Context, repository, commit, token string, filter modelartifact.Filter) (modelartifact.Manifest, error)
	Revalidate(ctx context.Context, repository, commit, token string) error
	ValidToken(ctx context.Context, token string) (bool, error)
}

func (r *ModelArtifactReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	ma := new(workercore.ModelArtifact)
	if err := r.Client.Get(ctx, req.NamespacedName, ma); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	if ma.DeletionTimestamp != nil {
		return ctrl.Result{}, r.releaseModelArtifact(ctx, ma)
	}
	if !ctrlcontrollerutil.ContainsFinalizer(ma, ModelArtifactProtectionFinalizer) {
		ctrlcontrollerutil.AddFinalizer(ma, ModelArtifactProtectionFinalizer)
		if err := r.Client.Update(ctx, ma); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolving is written, alone, before the source is asked: a resolution may take minutes, and
	// an artifact that shows no condition in that time reads as one nobody is working on.
	if !ModelArtifactConditionResolved.Exists(ma) {
		ma.Status.ObservedGeneration = ma.Generation
		ModelArtifactConditionResolved.Unknown(ma, modelArtifactReasonResolving, "the source has not answered yet")
		// This write and the answer's below end a conflict with an empty result: the watch has no
		// predicate, so the change that caused the conflict is itself an event.
		if err := r.Client.Status().Update(ctx, ma); err != nil {
			return objectWriteResult(logger, err, "update model artifact status to resolving", ctrl.Result{})
		}
		return ctrl.Result{Requeue: true}, nil
	}

	before := ma.Status.DeepCopy()
	ma.Status.ObservedGeneration = ma.Generation

	var (
		result ctrl.Result
		check  *modelArtifactCheck
	)
	switch {
	case ma.Spec.Source.HuggingFace != nil:
		hub, err := r.NewHuggingFace(ctx)
		if err != nil {
			ModelArtifactConditionDegraded.True(ma, modelartifact.ReasonSourceUnavailable, err.Error())
			return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}, nil
		}
		result, check = r.reconcileHubSource(ctx, ma, hub, ma.Spec.Source.HuggingFace)
	case ma.Spec.Source.ModelScope != nil:
		hub, err := r.NewModelScope(ctx)
		if err != nil {
			ModelArtifactConditionDegraded.True(ma, modelartifact.ReasonSourceUnavailable, err.Error())
			return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}, nil
		}
		result, check = r.reconcileHubSource(ctx, ma, hub, ma.Spec.Source.ModelScope)
	case ma.Spec.Source.PersistentVolumeClaim != nil:
		result = r.reconcileClaim(ctx, ma)
	case ma.Spec.Source.Image != nil:
		r.reconcileImage(ma)
	default:
		// Admission refuses every other shape; one stored before a rule existed stays unresolved
		// and says why.
		ModelArtifactConditionResolved.False(ma, "UnsupportedSource", "the source is not accepted in this version")
	}
	result = earliestResult(result, r.reconcileNodes(ctx, ma, before.Nodes))

	if !kubemeta.DeepEqual(before, &ma.Status) {
		if err := r.Client.Status().Update(ctx, ma); err != nil {
			// The answer is lost with the write, so the source must be asked again at once rather
			// than when the pacing entry says: a resolution whose status never landed would
			// otherwise wait out a whole revalidation interval.
			r.checks.Delete(ma.UID)
			return objectWriteResult(logger, err, "update model artifact status", ctrl.Result{})
		}
	}
	// Recorded only once the answer is stored, for the same reason.
	if check != nil {
		r.checks.Store(ma.UID, *check)
	}
	if !kubemeta.DeepEqual(before.Nodes, ma.Status.Nodes) {
		r.nodesWritten.Store(ma.UID, r.now())
	}

	return result, nil
}

// earliestResult is the result that comes back soonest of two passes' asks. A bare requeue is the
// soonest ask there is, so it survives whenever either side asks for it.
func earliestResult(a, b ctrl.Result) ctrl.Result {
	switch {
	case a.Requeue || b.Requeue:
		return ctrl.Result{Requeue: true}
	case a.RequeueAfter == 0:
		return b
	case b.RequeueAfter == 0:
		return a
	case b.RequeueAfter < a.RequeueAfter:
		return b
	}
	return a
}

// releaseModelArtifact removes the protection finalizer once nothing references the artifact.
// The ModelDeployment and Instance watches re-enqueue it when a reference goes away.
func (r *ModelArtifactReconciler) releaseModelArtifact(ctx context.Context, ma *workercore.ModelArtifact) error {
	if !ctrlcontrollerutil.ContainsFinalizer(ma, ModelArtifactProtectionFinalizer) {
		return nil
	}
	referrers, err := r.modelArtifactReferrers(ctx, ma)
	if err != nil {
		return err
	}
	if len(referrers) > 0 {
		ctrllog.FromContext(ctx).V(4).Info("model artifact is still referenced", "referrers", referrers)
		return nil
	}
	ctrlcontrollerutil.RemoveFinalizer(ma, ModelArtifactProtectionFinalizer)
	if err := r.Client.Update(ctx, ma); err != nil {
		return err
	}
	// A UID is never reused, so the pacing entries of a released artifact would otherwise stay for
	// the life of the process.
	r.checks.Delete(ma.UID)
	r.nodesWritten.Delete(ma.UID)

	return nil
}

// modelArtifactReferrers lists what references the artifact in its namespace, as "kind/name".
func (r *ModelArtifactReconciler) modelArtifactReferrers(ctx context.Context, ma *workercore.ModelArtifact) ([]string, error) {
	var referrers []string

	mds := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mds, ctrlcli.InNamespace(ma.Namespace)); err != nil {
		return nil, err
	}
	for i := range mds.Items {
		if ModelDeploymentArtifactName(&mds.Items[i]) == ma.Name {
			referrers = append(referrers, "ModelDeployment/"+mds.Items[i].Name)
		}
	}

	insts := new(workercore.InstanceList)
	if err := r.Client.List(ctx, insts, ctrlcli.InNamespace(ma.Namespace)); err != nil {
		return nil, err
	}
	for i := range insts.Items {
		for _, name := range InstanceArtifactNames(&insts.Items[i]) {
			if name == ma.Name {
				referrers = append(referrers, "Instance/"+insts.Items[i].Name)
				break
			}
		}
	}

	return referrers, nil
}

// ModelDeploymentArtifactName is the ModelArtifact a deployment references, or "".
func ModelDeploymentArtifactName(md *workercore.ModelDeployment) string {
	if md.Spec.Model.ArtifactRef == nil {
		return ""
	}

	return md.Spec.Model.ArtifactRef.Name
}

// InstanceArtifactNames are the ModelArtifacts an Instance's volumes reference.
func InstanceArtifactNames(inst *workercore.Instance) []string {
	var names []string
	for _, av := range inst.Spec.AdditionalVolumes {
		if av.Model != nil {
			names = append(names, av.Model.ArtifactRef.Name)
		}
	}

	return names
}

// reconcileClaim resolves a claim source: the claim existing is all a claim can say, because the
// operator never reads what the claim holds.
func (r *ModelArtifactReconciler) reconcileClaim(ctx context.Context, ma *workercore.ModelArtifact) ctrl.Result {
	source := ma.Spec.Source.PersistentVolumeClaim
	pvc := new(core.PersistentVolumeClaim)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: ma.Namespace, Name: source.ClaimName}, pvc)
	switch {
	case kerrors.IsNotFound(err):
		ModelArtifactConditionResolved.False(ma, modelArtifactReasonClaimMissing,
			fmt.Sprintf("PersistentVolumeClaim %q does not exist in this namespace", source.ClaimName))
		ModelArtifactConditionDegraded.True(ma, modelArtifactReasonClaimMissing,
			fmt.Sprintf("PersistentVolumeClaim %q does not exist in this namespace", source.ClaimName))
		return ctrl.Result{}
	case err != nil:
		ModelArtifactConditionDegraded.True(ma, modelartifact.ReasonSourceUnavailable,
			fmt.Sprintf("read PersistentVolumeClaim %q: %v", source.ClaimName, err))
		return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}
	}

	if ma.Status.Resolved == nil {
		ma.Status.Resolved = &workercore.ModelArtifactResolved{ResolvedTime: meta.NewTime(r.now())}
	}
	ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved,
		fmt.Sprintf("PersistentVolumeClaim %q exists; its content is the user's", source.ClaimName))
	ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")

	return ctrl.Result{}
}

// reconcileImage resolves an image source: admission pinned the reference, so the resolution is
// the spec being stored. No registry is read, no revalidation is scheduled, and no node ever
// aggregates — the image's bytes are the user's, delivered by kubelet wherever a pod lands.
func (r *ModelArtifactReconciler) reconcileImage(ma *workercore.ModelArtifact) {
	if ma.Status.Resolved == nil {
		ma.Status.Resolved = &workercore.ModelArtifactResolved{ResolvedTime: meta.NewTime(r.now())}
	}
	ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved,
		"the image is the user's; the operator does not read it")
	ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")
}

// modelArtifactCheck is one artifact's pacing entry.
type modelArtifactCheck struct {
	secretVersion string
	next          time.Time
}

// reconcileHubSource resolves a hub source once, then revalidates its access. It returns the
// pacing entry to record once the status is stored, or nil when the source was not asked.
func (r *ModelArtifactReconciler) reconcileHubSource(
	ctx context.Context, ma *workercore.ModelArtifact, hub modelArtifactHub, source *workercore.ModelArtifactHubSource,
) (ctrl.Result, *modelArtifactCheck) {
	token, secretVersion, err := r.modelArtifactToken(ctx, ma.Namespace, source.SecretRef)
	switch {
	case errors.Is(err, errModelArtifactSecretMissing):
		// A Secret that went away is a credential that went away, so a resolved artifact stops
		// being consumable at once; the engine that would read it could not download either.
		ModelArtifactConditionResolved.False(ma, modelArtifactReasonSecretMissing, err.Error())
		ModelArtifactConditionDegraded.True(ma, modelArtifactReasonSecretMissing, err.Error())
		return ctrl.Result{RequeueAfter: modelArtifactRefusedRetry}, nil
	case err != nil:
		// A Secret that could not be read says nothing about the grant, like a source that could
		// not be reached, so it never revokes.
		ModelArtifactConditionDegraded.True(ma, modelartifact.ReasonSourceUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}, nil
	}

	now := r.now()
	secretChanged := true
	if last, ok := r.checks.Load(ma.UID); ok {
		check := last.(modelArtifactCheck)
		secretChanged = check.secretVersion != secretVersion
		if !secretChanged && now.Before(check.next) {
			return ctrl.Result{RequeueAfter: check.next.Sub(now)}, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, modelArtifactResolveTimeout)
	defer cancel()

	var after time.Duration
	if ma.Status.Resolved == nil {
		after = r.resolveHubSource(ctx, ma, hub, source, token, now)
	} else {
		after = r.revalidateHubSource(ctx, ma, hub, source, token, now, secretChanged)
	}

	return ctrl.Result{RequeueAfter: after}, &modelArtifactCheck{secretVersion: secretVersion, next: now.Add(after)}
}

// resolveHubSource resolves the source once and reports when the source is to be asked next.
func (r *ModelArtifactReconciler) resolveHubSource(
	ctx context.Context, ma *workercore.ModelArtifact, hub modelArtifactHub, source *workercore.ModelArtifactHubSource,
	token string, now time.Time,
) time.Duration {
	resolution, err := hub.Resolve(ctx, source.Repository, source.Revision, token, modelartifact.Filter{
		Allow: ma.Spec.AllowPatterns, Ignore: ma.Spec.IgnorePatterns,
	})
	if err != nil {
		reason := modelartifact.ReasonOf(err)
		// An anchor turns a confirmed unavailability into an identity: one blip is not a verdict,
		// the same discipline a revocation follows, but a hub that could not be reached twice in a
		// row never said no, so the anchor the user asserted becomes the resolution. A hub that
		// answered — with content or with a refusal — is a hub whose answer decides.
		if reason == modelartifact.ReasonSourceUnavailable && ma.Spec.ExpectedDigest != "" &&
			ModelArtifactConditionDegraded.IsTrue(ma) && ModelArtifactConditionDegraded.GetReason(ma) == reason {
			if wait := modelArtifactConditionSince(ma, ModelArtifactConditionDegraded).Add(modelArtifactConfirmDelay).Sub(now); wait > 0 {
				return wait
			}
			ma.Status.Resolved = &workercore.ModelArtifactResolved{
				ManifestDigest: ma.Spec.ExpectedDigest,
				DigestSource:   workercore.ModelArtifactDigestSourceExpected,
				ResolvedTime:   meta.NewTime(r.now()),
			}
			ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved,
				fmt.Sprintf("resolved to the expected digest %s without the hub; the hub is not contacted again", ma.Spec.ExpectedDigest))
			ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")

			return r.revalidateInterval(ctx)
		}
		ModelArtifactConditionResolved.False(ma, reason, err.Error())
		ModelArtifactConditionDegraded.True(ma, reason, err.Error())
		if reason == modelartifact.ReasonSourceUnavailable {
			// Stamped with this pass's time, as the revalidation staircase does: the anchor's
			// confirm cadence runs from the outage this pass saw, not from a condition another
			// failure left behind.
			ModelArtifactConditionDegraded.LastTransitionTime(ma, now.UTC().Format(time.RFC3339))
			return modelArtifactUnavailableRetry
		}
		return modelArtifactRefusedRetry
	}

	// The anchor is an assertion about this very answer: a hub that serves different content than
	// the user pinned is refused, both digests named for the ticket.
	if anchor := ma.Spec.ExpectedDigest; anchor != "" && resolution.Manifest.Digest != anchor {
		err = &modelartifact.SourceError{
			Reason: modelartifact.ReasonDigestMismatch,
			Message: fmt.Sprintf("the artifact expects %s, the hub resolved %s@%s to %s",
				anchor, source.Repository, source.Revision, resolution.Manifest.Digest),
		}
		ModelArtifactConditionResolved.False(ma, modelartifact.ReasonDigestMismatch, err.Error())
		ModelArtifactConditionDegraded.True(ma, modelartifact.ReasonDigestMismatch, err.Error())

		return modelArtifactRefusedRetry
	}

	resolvedAt := meta.NewTime(r.now())
	ma.Status.Resolved = &workercore.ModelArtifactResolved{
		Revision:          resolution.Commit,
		ManifestDigest:    resolution.Manifest.Digest,
		DigestSource:      workercore.ModelArtifactDigestSourceHub,
		FileCount:         resolution.Manifest.FileCount,
		SizeBytes:         resolution.Manifest.SizeBytes,
		ResolvedTime:      resolvedAt,
		LastValidatedTime: &resolvedAt,
	}
	ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved,
		fmt.Sprintf("%s@%s resolved to commit %s", source.Repository, source.Revision, resolution.Commit))
	ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")
	r.warnOnRejectedToken(ctx, ma, source, hub, token)

	return r.revalidateInterval(ctx)
}

// revalidateHubSource checks access at the resolved commit and reports when the source is to be
// asked next. The commit and the digest are never re-resolved.
//
// A REFUSAL REVOKES ONLY WHEN CONFIRMED. The first one sets Degraded, stamped with the time it
// happened, and the check is repeated after modelArtifactConfirmDelay; the same refusal then sets
// Resolved=False. A failure to reach the source never revokes: it says nothing about the grant.
func (r *ModelArtifactReconciler) revalidateHubSource(
	ctx context.Context, ma *workercore.ModelArtifact, hub modelArtifactHub, source *workercore.ModelArtifactHubSource,
	token string, now time.Time, secretChanged bool,
) time.Duration {
	interval := r.revalidateInterval(ctx)
	if ma.Status.Resolved.DigestSource == workercore.ModelArtifactDigestSourceExpected {
		// An anchored identity has no commit to probe and no hub assumed to exist; the pacing
		// entry alone keeps the reconciler from spinning.
		return interval
	}
	if last := ma.Status.Resolved.LastValidatedTime; last != nil && ModelArtifactConditionResolved.IsTrue(ma) &&
		!ModelArtifactConditionDegraded.IsTrue(ma) && now.Before(last.Add(interval)) {
		// Nothing asks for a check yet: the pacing entry was lost, or the Secret changed back.
		if _, ok := r.checks.Load(ma.UID); !ok {
			return last.Add(interval).Sub(now)
		}
	}

	// The anchor makes the periodic check an assertion: the tree is re-listed at the resolved
	// commit and re-canonicalized, so a hub or a mirror that stops serving the pinned content
	// fails the same way a resolution would. An unanchored artifact keeps the two-request probe.
	var err error
	if anchor := ma.Spec.ExpectedDigest; anchor != "" {
		manifest, listErr := hub.ListManifest(ctx, source.Repository, ma.Status.Resolved.Revision, token,
			modelartifact.Filter{Allow: ma.Spec.AllowPatterns, Ignore: ma.Spec.IgnorePatterns})
		switch {
		case listErr != nil:
			err = listErr
		case manifest.Digest != anchor:
			err = &modelartifact.SourceError{
				Reason: modelartifact.ReasonDigestMismatch,
				Message: fmt.Sprintf("the artifact expects %s, the hub resolves %s@%s to %s",
					anchor, source.Repository, ma.Status.Resolved.Revision, manifest.Digest),
			}
		}
	} else {
		err = hub.Revalidate(ctx, source.Repository, ma.Status.Resolved.Revision, token)
	}
	if err == nil {
		validated := meta.NewTime(now)
		ma.Status.Resolved.LastValidatedTime = &validated
		ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved, "access confirmed at the resolved commit")
		ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")
		// The token is judged when it is new, not on every interval: an unchanged rejected token
		// was already reported, and one more event per interval would only repeat it.
		if secretChanged {
			r.warnOnRejectedToken(ctx, ma, source, hub, token)
		}
		return interval
	}

	reason := modelartifact.ReasonOf(err)
	if !modelArtifactRevokingReason(reason) {
		ModelArtifactConditionDegraded.True(ma, reason, err.Error())
		return modelArtifactUnavailableRetry
	}

	first := !ModelArtifactConditionDegraded.IsTrue(ma) || ModelArtifactConditionDegraded.GetReason(ma) != reason
	if first {
		ModelArtifactConditionDegraded.True(ma, reason,
			fmt.Sprintf("%v; access is checked again in %s and revoked if refused again", err, modelArtifactConfirmDelay))
		// Stamped explicitly: Degraded may already be True for another reason, and the confirmation
		// delay runs from this refusal, not from that one.
		ModelArtifactConditionDegraded.LastTransitionTime(ma, now.UTC().Format(time.RFC3339))
		return modelArtifactConfirmDelay
	}
	if wait := modelArtifactConditionSince(ma, ModelArtifactConditionDegraded).Add(modelArtifactConfirmDelay).Sub(now); wait > 0 {
		return wait
	}
	ModelArtifactConditionResolved.False(ma, reason, err.Error())
	ModelArtifactConditionDegraded.True(ma, reason, err.Error())

	return modelArtifactRefusedRetry
}

// modelArtifactRevokingReason reports whether a failed access check can revoke: a refusal can, a
// failure to reach the source cannot. A digest mismatch can, because the hub answered — its
// answer is just not the content the artifact asserts.
func modelArtifactRevokingReason(reason string) bool {
	return reason == modelartifact.ReasonAccessDenied || reason == modelartifact.ReasonRevisionNotFound ||
		reason == modelartifact.ReasonDigestMismatch
}

func modelArtifactConditionSince(ma *workercore.ModelArtifact, c kubeapistatus.ConditionType) time.Time {
	for _, cond := range ma.Status.Conditions {
		if cond.Type == string(c) {
			return cond.LastTransitionTime.Time
		}
	}

	return time.Time{}
}

// warnOnRejectedToken emits a Warning when the hub rejects the token outright. A hub ignores a
// rejected token on a public repository, answering as if none had been sent, so without this a
// mistyped token would never surface. It does not change the conditions.
func (r *ModelArtifactReconciler) warnOnRejectedToken(
	ctx context.Context, ma *workercore.ModelArtifact, source *workercore.ModelArtifactHubSource,
	hub modelArtifactHub, token string,
) {
	if token == "" || r.Recorder == nil || source.SecretRef == nil {
		return
	}
	valid, err := hub.ValidToken(ctx, token)
	if err != nil || valid {
		return
	}
	r.Recorder.Eventf(ma, core.EventTypeWarning, "InvalidToken",
		"the hub rejects the token in Secret %q; a public repository still resolves, as if no token had been sent",
		source.SecretRef.Name)
}

var errModelArtifactSecretMissing = errors.New("secret missing")

// modelArtifactToken reads the token a hub source names, with the Secret's resourceVersion. An
// unset reference is no token and no error.
func (r *ModelArtifactReconciler) modelArtifactToken(
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

func (r *ModelArtifactReconciler) revalidateInterval(ctx context.Context) time.Duration {
	d, err := time.ParseDuration(settings.ModelArtifactRevalidateInterval.ShouldValue(ctx))
	if err != nil || d < time.Minute {
		return 24 * time.Hour
	}

	return d
}

func (r *ModelArtifactReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

// newHuggingFaceFromSettings builds the Hub client from the administrator's Settings.
func (r *ModelArtifactReconciler) newHuggingFaceFromSettings(ctx context.Context) (*modelartifact.HuggingFace, error) {
	client, err := r.newHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}

	return &modelartifact.HuggingFace{
		Endpoint: settings.ModelArtifactHuggingFaceEndpoint.ShouldValue(ctx),
		Client:   client,
	}, nil
}

// newModelScopeFromSettings builds the ModelScope client from the administrator's Settings, the
// same proxy and CA bundle the Hugging Face client takes.
func (r *ModelArtifactReconciler) newModelScopeFromSettings(ctx context.Context) (modelArtifactHub, error) {
	client, err := r.newHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}

	return &modelartifact.ModelScope{
		Endpoint: settings.ModelArtifactModelScopeEndpoint.ShouldValue(ctx),
		Client:   client,
	}, nil
}

// newHubHTTPClient builds the client both hub controllers send their requests with.
func (r *ModelArtifactReconciler) newHubHTTPClient(ctx context.Context) (*http.Client, error) {
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

func (r *ModelArtifactReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// The proxy Setting's default comes from the environment, which no admission reads, and the
	// value is rendered into tenant Pods: refuse to start rather than render a credential there.
	if v := settings.ModelArtifactHTTPSProxy.DefaultValue(); v != "" {
		if err := modelartifact.ValidateProxy(v); err != nil {
			return fmt.Errorf("setting %s: %w", settings.ModelArtifactHTTPSProxy.Name(), err)
		}
	}

	if err := opts.Manager.GetFieldIndexer().IndexField(
		ctx, &workercore.ModelArtifact{}, IndexingModelArtifactByManifestDigest, indexModelArtifactByManifestDigest,
	); err != nil {
		return fmt.Errorf("index model artifact '%s': %w", IndexingModelArtifactByManifestDigest, err)
	}
	if err := opts.Manager.GetFieldIndexer().IndexField(
		ctx, &workercore.NodeModelStore{}, IndexingNodeModelStoreByModelDigest, indexNodeModelStoreByModelDigest,
	); err != nil {
		return fmt.Errorf("index node model store '%s': %w", IndexingNodeModelStoreByModelDigest, err)
	}

	r.Client = opts.Manager.GetClient()
	r.Recorder = opts.Manager.GetEventRecorderFor("modelartifact")
	if r.NewHuggingFace == nil {
		r.NewHuggingFace = r.newHuggingFaceFromSettings
	}
	if r.NewModelScope == nil {
		r.NewModelScope = r.newModelScopeFromSettings
	}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modelartifact").
		For(&workercore.ModelArtifact{}).
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: modelArtifactMaxConcurrentReconciles}).
		Watches(&core.Secret{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueModelArtifactsForSecret)).
		Watches(&core.PersistentVolumeClaim{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueModelArtifactsForClaim)).
		Watches(&workercore.ModelDeployment{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueTerminatingModelArtifactOfDeployment)).
		Watches(&workercore.Instance{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueTerminatingModelArtifactsOfInstance)).
		Watches(&workercore.NodeModelStore{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.mapModelArtifactNodeModelStore)).
		Complete(r)
}

func (r *ModelArtifactReconciler) enqueueModelArtifactsForSecret(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	return r.enqueueModelArtifacts(ctx, obj.GetNamespace(), func(ma *workercore.ModelArtifact) bool {
		for _, hub := range []*workercore.ModelArtifactHubSource{ma.Spec.Source.HuggingFace, ma.Spec.Source.ModelScope} {
			if hub != nil && hub.SecretRef != nil && hub.SecretRef.Name == obj.GetName() {
				return true
			}
		}
		return false
	})
}

func (r *ModelArtifactReconciler) enqueueModelArtifactsForClaim(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	return r.enqueueModelArtifacts(ctx, obj.GetNamespace(), func(ma *workercore.ModelArtifact) bool {
		claim := ma.Spec.Source.PersistentVolumeClaim
		return claim != nil && claim.ClaimName == obj.GetName()
	})
}

func (r *ModelArtifactReconciler) enqueueModelArtifacts(
	ctx context.Context, namespace string, match func(*workercore.ModelArtifact) bool,
) []ctrlreconcile.Request {
	list := new(workercore.ModelArtifactList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(namespace)); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model artifacts")
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

// enqueueTerminatingModelArtifactOfDeployment and enqueueTerminatingModelArtifactsOfInstance wake an
// artifact waiting on its references to go away. Only a Terminating artifact is enqueued: a
// deployment's status changes on every pass of its own controller, and none of those concerns an
// artifact that is not being deleted.
func (r *ModelArtifactReconciler) enqueueTerminatingModelArtifactOfDeployment(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	md, ok := obj.(*workercore.ModelDeployment)
	if !ok {
		return nil
	}

	return r.enqueueTerminatingModelArtifacts(ctx, md.Namespace, ModelDeploymentArtifactName(md))
}

func (r *ModelArtifactReconciler) enqueueTerminatingModelArtifactsOfInstance(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	inst, ok := obj.(*workercore.Instance)
	if !ok {
		return nil
	}

	return r.enqueueTerminatingModelArtifacts(ctx, inst.Namespace, InstanceArtifactNames(inst)...)
}

func (r *ModelArtifactReconciler) enqueueTerminatingModelArtifacts(
	ctx context.Context, namespace string, names ...string,
) []ctrlreconcile.Request {
	var reqs []ctrlreconcile.Request
	for _, name := range names {
		if name == "" {
			continue
		}
		key := ctrlcli.ObjectKey{Namespace: namespace, Name: name}
		ma := new(workercore.ModelArtifact)
		if err := r.Client.Get(ctx, key, ma); err != nil || ma.DeletionTimestamp == nil {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: key})
	}

	return reqs
}
