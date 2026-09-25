package worker

import (
	"context"
	"errors"
	"fmt"
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

	// checks remembers, per artifact UID, the Secret resourceVersion the source was last asked
	// with and when it may be asked next. It paces the calls to the Hub: this controller also runs
	// on its own status writes and on every change of a referencing deployment, and none of those
	// is a reason to ask the Hub again. A changed Secret is. Losing it on restart costs one early
	// check per artifact, nothing else: whether access is revoked is decided from the conditions.
	checks sync.Map
}

var _ ctrlreconcile.Reconciler = (*ModelArtifactReconciler)(nil)

func (r *ModelArtifactReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		if err := r.Client.Status().Update(ctx, ma); err != nil {
			return ctrl.Result{}, err
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
		result, check = r.reconcileHuggingFace(ctx, ma)
	case ma.Spec.Source.PersistentVolumeClaim != nil:
		result = r.reconcileClaim(ctx, ma)
	default:
		// Admission refuses every other shape, ModelScope included; one stored before a rule
		// existed stays unresolved and says why.
		ModelArtifactConditionResolved.False(ma, "UnsupportedSource", "the source is not accepted in this version")
	}

	if !kubemeta.DeepEqual(before, &ma.Status) {
		if err := r.Client.Status().Update(ctx, ma); err != nil {
			// The answer is lost with the write, so the source must be asked again at once rather
			// than when the pacing entry says: a resolution whose status never landed would
			// otherwise wait out a whole revalidation interval.
			r.checks.Delete(ma.UID)
			return ctrl.Result{}, err
		}
	}
	// Recorded only once the answer is stored, for the same reason.
	if check != nil {
		r.checks.Store(ma.UID, *check)
	}

	return result, nil
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
	// A UID is never reused, so the pacing entry of a released artifact would otherwise stay for
	// the life of the process.
	r.checks.Delete(ma.UID)

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

// modelArtifactCheck is one artifact's pacing entry.
type modelArtifactCheck struct {
	secretVersion string
	next          time.Time
}

// reconcileHuggingFace resolves a Hugging Face source once, then revalidates its access. It returns
// the pacing entry to record once the status is stored, or nil when the source was not asked.
func (r *ModelArtifactReconciler) reconcileHuggingFace(
	ctx context.Context, ma *workercore.ModelArtifact,
) (ctrl.Result, *modelArtifactCheck) {
	source := ma.Spec.Source.HuggingFace

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

	hub, err := r.NewHuggingFace(ctx)
	if err != nil {
		ModelArtifactConditionDegraded.True(ma, modelartifact.ReasonSourceUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: modelArtifactUnavailableRetry}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, modelArtifactResolveTimeout)
	defer cancel()

	var after time.Duration
	if ma.Status.Resolved == nil {
		after = r.resolveHuggingFace(ctx, ma, hub, token)
	} else {
		after = r.revalidateHuggingFace(ctx, ma, hub, token, now, secretChanged)
	}

	return ctrl.Result{RequeueAfter: after}, &modelArtifactCheck{secretVersion: secretVersion, next: now.Add(after)}
}

// resolveHuggingFace resolves the source once and reports when the source is to be asked next.
func (r *ModelArtifactReconciler) resolveHuggingFace(
	ctx context.Context, ma *workercore.ModelArtifact, hub *modelartifact.HuggingFace, token string,
) time.Duration {
	source := ma.Spec.Source.HuggingFace
	resolution, err := hub.Resolve(ctx, source.Repository, source.Revision, token)
	if err != nil {
		reason := modelartifact.ReasonOf(err)
		ModelArtifactConditionResolved.False(ma, reason, err.Error())
		ModelArtifactConditionDegraded.True(ma, reason, err.Error())
		if reason == modelartifact.ReasonSourceUnavailable {
			return modelArtifactUnavailableRetry
		}
		return modelArtifactRefusedRetry
	}

	now := meta.NewTime(r.now())
	ma.Status.Resolved = &workercore.ModelArtifactResolved{
		Revision:          resolution.Commit,
		ManifestDigest:    resolution.Manifest.Digest,
		FileCount:         resolution.Manifest.FileCount,
		SizeBytes:         resolution.Manifest.SizeBytes,
		ResolvedTime:      now,
		LastValidatedTime: &now,
	}
	ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved,
		fmt.Sprintf("%s@%s resolved to commit %s", source.Repository, source.Revision, resolution.Commit))
	ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")
	r.warnOnRejectedToken(ctx, ma, hub, token)

	return r.revalidateInterval(ctx)
}

// revalidateHuggingFace checks access at the resolved commit and reports when the source is to be
// asked next. The commit and the digest are never re-resolved.
//
// A REFUSAL REVOKES ONLY WHEN CONFIRMED. The first one sets Degraded, stamped with the time it
// happened, and the check is repeated after modelArtifactConfirmDelay; the same refusal then sets
// Resolved=False. A failure to reach the source never revokes: it says nothing about the grant.
func (r *ModelArtifactReconciler) revalidateHuggingFace(
	ctx context.Context, ma *workercore.ModelArtifact, hub *modelartifact.HuggingFace, token string, now time.Time,
	secretChanged bool,
) time.Duration {
	interval := r.revalidateInterval(ctx)
	if last := ma.Status.Resolved.LastValidatedTime; last != nil && ModelArtifactConditionResolved.IsTrue(ma) &&
		!ModelArtifactConditionDegraded.IsTrue(ma) && now.Before(last.Add(interval)) {
		// Nothing asks for a check yet: the pacing entry was lost, or the Secret changed back.
		if _, ok := r.checks.Load(ma.UID); !ok {
			return last.Add(interval).Sub(now)
		}
	}

	source := ma.Spec.Source.HuggingFace
	err := hub.Revalidate(ctx, source.Repository, ma.Status.Resolved.Revision, token)
	if err == nil {
		validated := meta.NewTime(now)
		ma.Status.Resolved.LastValidatedTime = &validated
		ModelArtifactConditionResolved.True(ma, modelArtifactReasonResolved, "access confirmed at the resolved commit")
		ModelArtifactConditionDegraded.False(ma, modelArtifactReasonHealthy, "")
		// The token is judged when it is new, not on every interval: an unchanged rejected token
		// was already reported, and one more event per interval would only repeat it.
		if secretChanged {
			r.warnOnRejectedToken(ctx, ma, hub, token)
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
// failure to reach the source cannot.
func modelArtifactRevokingReason(reason string) bool {
	return reason == modelartifact.ReasonAccessDenied || reason == modelartifact.ReasonRevisionNotFound
}

func modelArtifactConditionSince(ma *workercore.ModelArtifact, c kubeapistatus.ConditionType) time.Time {
	for _, cond := range ma.Status.Conditions {
		if cond.Type == string(c) {
			return cond.LastTransitionTime.Time
		}
	}

	return time.Time{}
}

// warnOnRejectedToken emits a Warning when the Hub rejects the token outright. The Hub ignores a
// rejected token on a public repository, answering as if none had been sent, so without this a
// mistyped token would never surface. It does not change the conditions.
func (r *ModelArtifactReconciler) warnOnRejectedToken(
	ctx context.Context, ma *workercore.ModelArtifact, hub *modelartifact.HuggingFace, token string,
) {
	if token == "" || r.Recorder == nil {
		return
	}
	valid, err := hub.ValidToken(ctx, token)
	if err != nil || valid {
		return
	}
	r.Recorder.Eventf(ma, core.EventTypeWarning, "InvalidToken",
		"the Hub rejects the token in Secret %q; a public repository still resolves, as if no token had been sent",
		ma.Spec.Source.HuggingFace.SecretRef.Name)
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
	client, err := modelartifact.NewHTTPClient(opts)
	if err != nil {
		return nil, err
	}

	return &modelartifact.HuggingFace{
		Endpoint: settings.ModelArtifactHuggingFaceEndpoint.ShouldValue(ctx),
		Client:   client,
	}, nil
}

func (r *ModelArtifactReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	// The proxy Setting's default comes from the environment, which no admission reads, and the
	// value is rendered into tenant Pods: refuse to start rather than render a credential there.
	if v := settings.ModelArtifactHTTPSProxy.DefaultValue(); v != "" {
		if err := modelartifact.ValidateProxy(v); err != nil {
			return fmt.Errorf("setting %s: %w", settings.ModelArtifactHTTPSProxy.Name(), err)
		}
	}

	r.Client = opts.Manager.GetClient()
	r.Recorder = opts.Manager.GetEventRecorderFor("modelartifact")
	if r.NewHuggingFace == nil {
		r.NewHuggingFace = r.newHuggingFaceFromSettings
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
		Complete(r)
}

func (r *ModelArtifactReconciler) enqueueModelArtifactsForSecret(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	return r.enqueueModelArtifacts(ctx, obj.GetNamespace(), func(ma *workercore.ModelArtifact) bool {
		hub := ma.Spec.Source.HuggingFace
		return hub != nil && hub.SecretRef != nil && hub.SecretRef.Name == obj.GetName()
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
