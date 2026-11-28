package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/modelforger"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

const (
	forgerWorkerCertCAFile     = "/var/run/gpustack/certs/ca.crt"
	forgerDefaultOperatorImage = "gpustack/gpustack-operator:latest"

	forgerDefaultProbeSteps             = 4
	forgerDefaultCooldownSeconds        = 600
	forgerMinCooldownSeconds            = 60
	forgerDefaultMaxKernels             = 4
	forgerDefaultSandboxTimeoutSeconds  = 1800
	forgerDefaultMaxRetainedGenerations = 3
	forgerDefaultFailureTimeoutSeconds  = 600
)

var defaultForgerProbeClient = runtimecontrol.NewClient()

// ModelDeploymentForgerProbeClient defines the sentinel HTTP surface used by
// ModelDeploymentForgerReconciler.
type ModelDeploymentForgerProbeClient interface {
	GetReady(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.ReadyResponse, error)
	GetRuntimeConfig(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.RuntimeConfigState, error)
	StartProbe(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.ProbeRequest) (*runtimecontrol.ProbeResponse, error)
	GetProbe(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.ProbeResponse, error)
	DeleteProbe(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.ProbeResponse, error)
}

// ModelDeploymentForgerIngester defines the seed-node model-manager ingestion client surface.
type ModelDeploymentForgerIngester interface {
	IngestTree(
		ctx context.Context, seedNodeHost string, port int, req modelstore.IngestTreeRequest,
	) (*modelstore.IngestTreeResponse, error)
}

type forgerProbeAttempt struct {
	PodName   string
	PodUID    string
	BootID    string
	NodeName  string
	StartedAt time.Time
}

type forgerPromotionAttempt struct {
	ArtifactName string
	StartedAt    time.Time
}

// ModelDeploymentForgerReconciler automates closed-loop short-window sentinel probing,
// Kueue-admitted single-GPU ForgeSandbox synthesis and independent verification,
// NodeModelStore publication, readiness-gated canary promotion, automatic rollback,
// persistent quarantine, and multi-generation retention pruning.
type ModelDeploymentForgerReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
	Recorder  ctrlrecord.EventRecorder

	Now          func() time.Time
	ProbeClient  ModelDeploymentForgerProbeClient
	DrainReader  *runtimecontrol.DrainReader
	IngestClient ModelDeploymentForgerIngester

	OperatorImage         string
	ResolveOperatorImage  func(ctx context.Context) (string, error)
	ResolveSentinelTarget func(
		ctx context.Context,
		md *workercore.ModelDeployment,
		role *workercore.ModelDeploymentRole,
		pod *core.Pod,
	) (runtimecontrol.Target, error)
	ResolveSeedEndpoint func(ctx context.Context, seedNode string) (host string, port int, err error)

	mu         sync.Mutex
	probes     map[string]forgerProbeAttempt
	promotions map[string]forgerPromotionAttempt
	defaultIC  ModelDeploymentForgerIngester
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentForgerReconciler)(nil)

// SetupController registers ModelDeploymentForgerReconciler and its watches with the manager.
func (r *ModelDeploymentForgerReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	ensureForgerMetricsRegistered()

	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()
	r.Recorder = opts.Manager.GetEventRecorderFor("modeldeploymentforger")
	if r.Now == nil {
		r.Now = time.Now
	}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeploymentforger").
		For(&workercore.ModelDeploymentForger{}, ctrlbuilder.WithPredicates(modelDeploymentForgerPredicate())).
		Watches(
			&workercore.ModelDeployment{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapTargetDeployment),
		).
		Watches(
			&core.Pod{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapPod),
			ctrlbuilder.WithPredicates(modelDeploymentForgerPodPredicate()),
		).
		Watches(
			&workercore.ModelKernelArtifact{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapKernelArtifact),
		).
		Watches(
			&kueue.Workload{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapWorkload),
			ctrlbuilder.WithPredicates(modelDeploymentForgerWorkloadPredicate()),
		).
		Complete(r)
}

// Reconcile drives one closed-loop reconciliation pass for a ModelDeploymentForger.
func (r *ModelDeploymentForgerReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	// Read the live Forger object directly so cross-resource Pod/Workload watches triggered
	// immediately after a status update cannot observe stale pre-transition phase/ActiveSandbox
	// state and mistakenly sweep a freshly created ForgeSandbox Pod.
	mdf := new(workercore.ModelDeploymentForger)
	if err := r.apiReaderOrClient().Get(ctx, req.NamespacedName, mdf); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	now := r.now()

	if !mdf.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, mdf)
	}
	if !slices.Contains(mdf.Finalizers, workercore.ModelDeploymentForgerFinalizerKey) {
		mdf.Finalizers = append(mdf.Finalizers, workercore.ModelDeploymentForgerFinalizerKey)
		if err := r.Client.Update(ctx, mdf); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	md := new(workercore.ModelDeployment)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: mdf.Namespace, Name: mdf.Spec.TargetRef.Name}, md); err != nil {
		if !kerrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		before := mdf.DeepCopy()
		r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionFalse,
			workercore.ModelDeploymentForgerReasonTargetNotFound, "the target ModelDeployment does not exist", now)
		r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
			workercore.ModelDeploymentForgerReasonTargetNotFound, "the target ModelDeployment does not exist", now)
		r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked, meta.ConditionFalse,
			workercore.ModelDeploymentForgerReasonNoBlock, "waiting for target ModelDeployment", now)
		r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeReady, meta.ConditionFalse,
			workercore.ModelDeploymentForgerReasonDegraded, "the target ModelDeployment does not exist", now)
		mdf.Status.ObservedGeneration = mdf.Generation
		if err := r.updateStatus(ctx, before, mdf); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	if mdf.Status.Binding != nil && mdf.Status.Binding.TargetUID != "" && mdf.Status.Binding.TargetUID != string(md.UID) {
		if mdf.Status.ObservedGeneration >= mdf.Generation {
			r.cleanupAllForgerSandboxes(ctx, mdf)
			before := mdf.DeepCopy()
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonTargetReplaced,
				"the target was replaced; edit the Forger spec to bind the new target", now)
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonTargetReplaced, "the target was replaced", now)
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonNoBlock, "the target was replaced", now)
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeReady, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonDegraded, "the target was replaced", now)
			mdf.Status.ObservedGeneration = mdf.Generation
			prevReason := modelDeploymentForgerConditionReason(before, workercore.ModelDeploymentForgerConditionTypeBound)
			if err := r.updateStatus(ctx, before, mdf); err != nil {
				return ctrl.Result{}, err
			}
			if r.Recorder != nil && prevReason != workercore.ModelDeploymentForgerReasonTargetReplaced {
				r.Recorder.Event(
					mdf, core.EventTypeWarning,
					workercore.ModelDeploymentForgerReasonTargetReplaced,
					"the target was replaced; edit the forger spec to bind the new target",
				)
			}
			return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
		}
		r.forgetForgerState(mdf.UID)
		mdf.Status.Roles = nil
	}

	prevBlockedReason := modelDeploymentForgerConditionReason(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked)
	mode := effectiveForgerMode(mdf)

	if mdf.Spec.Suspend || mode != workercore.ModelDeploymentForgerModeActive {
		before := mdf.DeepCopy()
		if err := r.rollbackUncommittedPromotions(ctx, mdf, md, now); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.cleanupRemovedRoles(ctx, mdf, md, now); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.releaseOwnedBinding(ctx, mdf, md); err != nil {
			return ctrl.Result{}, err
		}

		if mdf.Spec.Suspend {
			r.cleanupAllForgerSandboxes(ctx, mdf)
			r.forgetForgerState(mdf.UID)
			ensureForgerRoleStatuses(mdf)
			for i := range mdf.Status.Roles {
				mdf.Status.Roles[i].ActiveSandbox = nil
				mdf.Status.Roles[i].PreviousKernelArtifact = ""
				mdf.Status.Roles[i].Phase = workercore.ModelDeploymentForgerRolePhaseIdle
			}
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonSuspended, "the forger is suspended", now)
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonSuspended, "the forger is suspended", now)
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonNoBlock, "the forger is suspended", now)
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeReady, meta.ConditionTrue,
				workercore.ModelDeploymentForgerReasonIdle, "the forger is suspended", now)
			mdf.Status.ObservedGeneration = mdf.Generation
			return ctrl.Result{}, r.updateStatus(ctx, before, mdf)
		}

		if mode == workercore.ModelDeploymentForgerModeObserveOnly {
			r.cleanupAllForgerSandboxes(ctx, mdf)
			for i := range mdf.Status.Roles {
				rs := &mdf.Status.Roles[i]
				rs.ActiveSandbox = nil
				rs.PreviousKernelArtifact = ""
				if rs.Phase != workercore.ModelDeploymentForgerRolePhaseIdle &&
					rs.Phase != workercore.ModelDeploymentForgerRolePhaseProbing {
					rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
				}
			}
		} else {
			for i := range mdf.Status.Roles {
				rs := &mdf.Status.Roles[i]
				rs.PreviousKernelArtifact = ""
				if rs.Phase == workercore.ModelDeploymentForgerRolePhasePromoting {
					rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
				}
			}
		}

		if binding, ok, bindErr := readForgerBinding(md); bindErr != nil {
			return r.reportInvalidBinding(ctx, mdf, md.Name, bindErr, now)
		} else if ok && binding.ForgerUID != string(mdf.UID) {
			return r.reportAlreadyBound(ctx, mdf, now)
		}

		if mdf.Status.Binding == nil {
			mdf.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
				TargetUID:  string(md.UID),
				TargetName: md.Name,
			}
		} else {
			mdf.Status.Binding.TargetUID = string(md.UID)
			mdf.Status.Binding.TargetName = md.Name
		}
		r.recoverDecision(ctx, mdf, md)
		r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionTrue,
			workercore.ModelDeploymentForgerReasonBound, "the target is available", now)
		if mode == workercore.ModelDeploymentForgerModeObserveOnly {
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonObserveOnlyMode,
				"the forger observes short-window probes without launching sandboxes or mutating the target", now)
		} else {
			r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonShadowMode,
				"the forger builds and publishes verified kernel artifacts without mutating the target", now)
		}
		return r.reconcileTargetRoles(ctx, before, mdf, md, now, prevBlockedReason)
	}

	binding, hasBinding, err := readForgerBinding(md)
	if err != nil {
		return r.reportInvalidBinding(ctx, mdf, md.Name, err, now)
	}
	if hasBinding && binding.ForgerUID != string(mdf.UID) {
		return r.reportAlreadyBound(ctx, mdf, now)
	}

	term := modelDeploymentForgerInitialTerm
	if hasBinding {
		term = max(modelDeploymentForgerInitialTerm, binding.Term)
		if mdf.Status.Binding != nil {
			term = max(term, mdf.Status.Binding.Term)
		}
	} else if mdf.Status.Binding != nil {
		term = max(modelDeploymentForgerInitialTerm, mdf.Status.Binding.Term+1)
	}

	if !hasBinding || binding.ForgerUID != string(mdf.UID) || binding.Term != term {
		freshMD := new(workercore.ModelDeployment)
		if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(md), freshMD); err != nil {
			return ctrl.Result{}, err
		}
		if freshMD.Annotations == nil {
			freshMD.Annotations = make(map[string]string)
		}
		value, marshalErr := json.Marshal(workercore.ModelDeploymentForgerBinding{
			ForgerName: mdf.Name,
			ForgerUID:  string(mdf.UID),
			Term:       term,
		})
		if marshalErr != nil {
			return ctrl.Result{}, marshalErr
		}
		freshMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] = string(value)
		if err := r.Client.Update(ctx, freshMD); err != nil {
			if kerrors.IsInvalid(err) || kerrors.IsForbidden(err) {
				return r.reportBindingRejected(ctx, mdf, freshMD.Name, err, now)
			}
			return ctrl.Result{}, err
		}
		if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(freshMD), md); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := mdf.DeepCopy()
	mdf.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
		TargetUID:  string(md.UID),
		TargetName: md.Name,
		Term:       term,
	}
	if err := r.cleanupRemovedRoles(ctx, mdf, md, now); err != nil {
		return ctrl.Result{}, err
	}
	r.recoverDecision(ctx, mdf, md)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionTrue,
		workercore.ModelDeploymentForgerReasonBound, "the forger owns the target lease", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionTrue,
		workercore.ModelDeploymentForgerReasonActive, "the forger is actively managing kernel specialization", now)

	return r.reconcileTargetRoles(ctx, before, mdf, md, now, prevBlockedReason)
}

func (r *ModelDeploymentForgerReconciler) reportBindingRejected(
	ctx context.Context, mdf *workercore.ModelDeploymentForger, targetName string, err error, now time.Time,
) (ctrl.Result, error) {
	msg := err.Error()
	ctrl.LoggerFrom(ctx).Error(err, "the target rejected forger binding acquisition", "target", targetName)
	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeWarning, workercore.ModelDeploymentForgerReasonInvalidBinding,
			"failed acquiring forger binding on target %s: %v", targetName, err)
	}
	before := mdf.DeepCopy()
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonInvalidBinding, msg, now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonInvalidBinding, msg, now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonNoBlock, msg, now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeReady, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonDegraded, msg, now)
	mdf.Status.ObservedGeneration = mdf.Generation
	if updateErr := r.updateStatus(ctx, before, mdf); updateErr != nil {
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
}

func (r *ModelDeploymentForgerReconciler) reportInvalidBinding(
	ctx context.Context, mdf *workercore.ModelDeploymentForger, targetName string, err error, now time.Time,
) (ctrl.Result, error) {
	ctrl.LoggerFrom(ctx).Error(err, "the target's forger binding annotation cannot be read", "target", targetName)
	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeWarning, workercore.ModelDeploymentForgerReasonInvalidBinding,
			"the target's forger binding annotation cannot be read: %v", err)
	}
	before := mdf.DeepCopy()
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonInvalidBinding, "the target's forger binding annotation cannot be read", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonInvalidBinding, "the target's forger binding annotation cannot be read", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonNoBlock, "the target's forger binding annotation cannot be read", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeReady, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonDegraded, "the target's forger binding annotation cannot be read", now)
	mdf.Status.ObservedGeneration = mdf.Generation
	if updateErr := r.updateStatus(ctx, before, mdf); updateErr != nil {
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
}

func (r *ModelDeploymentForgerReconciler) reportAlreadyBound(
	ctx context.Context, mdf *workercore.ModelDeploymentForger, now time.Time,
) (ctrl.Result, error) {
	before := mdf.DeepCopy()
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBound, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonAlreadyBound, "another ModelDeploymentForger owns the target", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeActive, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonAlreadyBound, "another ModelDeploymentForger owns the target", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeBlocked, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonNoBlock, "another ModelDeploymentForger owns the target", now)
	r.setCondition(mdf, workercore.ModelDeploymentForgerConditionTypeReady, meta.ConditionFalse,
		workercore.ModelDeploymentForgerReasonDegraded, "another ModelDeploymentForger owns the target", now)
	mdf.Status.ObservedGeneration = mdf.Generation
	if err := r.updateStatus(ctx, before, mdf); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
}

func (r *ModelDeploymentForgerReconciler) reconcileDeletion(
	ctx context.Context, mdf *workercore.ModelDeploymentForger,
) (ctrl.Result, error) {
	r.forgetForgerState(mdf.UID)
	r.cleanupAllForgerSandboxes(ctx, mdf)

	if slices.Contains(mdf.Finalizers, workercore.ModelDeploymentForgerFinalizerKey) {
		md := new(workercore.ModelDeployment)
		err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: mdf.Namespace, Name: mdf.Spec.TargetRef.Name}, md)
		if err == nil {
			if rbErr := r.rollbackUncommittedPromotions(ctx, mdf, md, r.now()); rbErr != nil {
				return ctrl.Result{}, rbErr
			}
			if relErr := r.releaseOwnedBinding(ctx, mdf, md); relErr != nil {
				return ctrl.Result{}, relErr
			}
		} else if !kerrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		mdf.Finalizers = slices.DeleteFunc(mdf.Finalizers, func(value string) bool {
			return value == workercore.ModelDeploymentForgerFinalizerKey
		})
		if err := r.Client.Update(ctx, mdf); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *ModelDeploymentForgerReconciler) cleanupRoleSandboxObjects(
	ctx context.Context, namespace string, sb *workercore.ModelDeploymentForgerSandboxStatus,
) {
	if sb == nil {
		return
	}
	if sb.PodName != "" {
		_ = r.Client.Delete(ctx, &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: sb.PodName}})
	}
	if sb.ConfigMapName != "" {
		_ = r.Client.Delete(ctx, &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: sb.ConfigMapName}})
	}
}

func (r *ModelDeploymentForgerReconciler) cleanupAllForgerSandboxes(
	ctx context.Context, mdf *workercore.ModelDeploymentForger,
) {
	for i := range mdf.Status.Roles {
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, mdf.Status.Roles[i].ActiveSandbox)
	}

	match := ctrlcli.MatchingLabels{
		modelforger.SandboxLabelName:     "model-deployment-forger",
		modelforger.SandboxLabelInstance: modelforger.SafeLabelValue(mdf.Name),
	}
	pods := new(core.PodList)
	if err := r.Client.List(ctx, pods, ctrlcli.InNamespace(mdf.Namespace), match); err == nil {
		for i := range pods.Items {
			_ = r.Client.Delete(ctx, &pods.Items[i])
		}
	}
	cms := new(core.ConfigMapList)
	if err := r.Client.List(ctx, cms, ctrlcli.InNamespace(mdf.Namespace), match); err == nil {
		for i := range cms.Items {
			_ = r.Client.Delete(ctx, &cms.Items[i])
		}
	}
}

func (r *ModelDeploymentForgerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ModelDeploymentForgerReconciler) apiReaderOrClient() ctrlcli.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ModelDeploymentForgerReconciler) probeClient() ModelDeploymentForgerProbeClient {
	if r.ProbeClient != nil {
		return r.ProbeClient
	}
	return defaultForgerProbeClient
}

func (r *ModelDeploymentForgerReconciler) drainReader() *runtimecontrol.DrainReader {
	if r.DrainReader != nil {
		return r.DrainReader
	}
	return runtimecontrol.NewDrainReader(runtimecontrol.WithDrainClock(r.now))
}

func (r *ModelDeploymentForgerReconciler) ingestClient() (ModelDeploymentForgerIngester, error) {
	if r.IngestClient != nil {
		return r.IngestClient, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.defaultIC != nil {
		return r.defaultIC, nil
	}

	var opts []modelforger.IngestClientOption
	if _, statErr := os.Stat(forgerWorkerCertCAFile); statErr == nil {
		opts = append(opts, modelforger.WithIngestCAFile(forgerWorkerCertCAFile))
	} else {
		opts = append(opts, modelforger.WithIngestInsecureSkipVerify(true))
	}
	ic, err := modelforger.NewIngestClient(opts...)
	if err != nil {
		return nil, err
	}
	r.defaultIC = ic
	return ic, nil
}

func (r *ModelDeploymentForgerReconciler) resolveSentinelTarget(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
) (runtimecontrol.Target, error) {
	if r.ResolveSentinelTarget != nil {
		return r.ResolveSentinelTarget(ctx, md, role, pod)
	}
	port := ModelDeploymentRoleServingPort(md, role)
	if rawPort := strings.TrimSpace(pod.Annotations["prometheus.io/port"]); rawPort != "" {
		if parsed, err := strconv.ParseInt(rawPort, 10, 32); err == nil && parsed > 0 {
			port = int32(parsed)
		}
	}
	scheme := strings.ToLower(string(ModelDeploymentRoleServingScheme(md.Spec.Engine.Name, role)))
	if rawScheme := strings.ToLower(strings.TrimSpace(pod.Annotations["prometheus.io/scheme"])); rawScheme == "http" || rawScheme == "https" {
		scheme = rawScheme
	}
	if scheme == "" {
		scheme = "http"
	}
	return runtimecontrol.Target{
		Scheme:             scheme,
		Host:               pod.Status.PodIP,
		Port:               int(port),
		InsecureSkipVerify: scheme == "https",
	}, nil
}

func (r *ModelDeploymentForgerReconciler) resolveSeedEndpoint(
	ctx context.Context, seedNode string,
) (host string, port int, err error) {
	if r.ResolveSeedEndpoint != nil {
		return r.ResolveSeedEndpoint(ctx, seedNode)
	}
	list := new(core.PodList)
	if err := r.apiReaderOrClient().List(
		ctx,
		list,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels{deviceplugin.ComponentLabelKey: modelstore.PluginComponent},
		ctrlclix.WithoutQuorum,
	); err != nil {
		return "", 0, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil || p.Spec.NodeName != seedNode || strings.TrimSpace(p.Status.PodIP) == "" {
			continue
		}
		if !deviceplugin.IsPodReady(p) {
			continue
		}
		secPort := modelstore.DefaultPluginSecurePort
		for cIdx := range p.Spec.Containers {
			for pIdx := range p.Spec.Containers[cIdx].Ports {
				if p.Spec.Containers[cIdx].Ports[pIdx].Name == modelstore.PluginSecurePortName {
					secPort = int(p.Spec.Containers[cIdx].Ports[pIdx].ContainerPort)
				}
			}
		}
		return p.Status.PodIP, secPort, nil
	}
	return "", 0, fmt.Errorf("no ready model-manager Pod found on seedNode %q", seedNode)
}

func (r *ModelDeploymentForgerReconciler) resolveOperatorImage(ctx context.Context) (string, error) {
	if strings.TrimSpace(r.OperatorImage) != "" {
		return strings.TrimSpace(r.OperatorImage), nil
	}
	if r.ResolveOperatorImage != nil {
		return r.ResolveOperatorImage(ctx)
	}

	podNS := osx.Getenv("KUBERNETES_POD_NAMESPACE", kuberess.SystemNamespaceName)
	if podName := osx.Getenv("KUBERNETES_POD_NAME"); podName != "" {
		selfPod := new(core.Pod)
		if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Namespace: podNS, Name: podName}, selfPod); err == nil {
			for i := range selfPod.Spec.Containers {
				if selfPod.Spec.Containers[i].Name == "main" && selfPod.Spec.Containers[i].Image != "" {
					return selfPod.Spec.Containers[i].Image, nil
				}
			}
			if len(selfPod.Spec.Containers) > 0 && selfPod.Spec.Containers[0].Image != "" {
				return selfPod.Spec.Containers[0].Image, nil
			}
		}
	}

	pods := new(core.PodList)
	if err := r.apiReaderOrClient().List(
		ctx, pods, ctrlcli.InNamespace(kuberess.SystemNamespaceName), ctrlclix.WithoutQuorum,
	); err == nil {
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.DeletionTimestamp != nil || p.Labels["app.kubernetes.io/component"] != "worker" {
				continue
			}
			for cIdx := range p.Spec.Containers {
				if p.Spec.Containers[cIdx].Name == "main" && p.Spec.Containers[cIdx].Image != "" {
					return p.Spec.Containers[cIdx].Image, nil
				}
			}
		}
	}

	return forgerDefaultOperatorImage, nil
}

func (r *ModelDeploymentForgerReconciler) listDeploymentPods(
	ctx context.Context, md *workercore.ModelDeployment,
) ([]core.Pod, error) {
	list := new(core.PodList)
	if err := r.Client.List(
		ctx,
		list,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *ModelDeploymentForgerReconciler) findPodByName(
	ctx context.Context, namespace, name string,
) (*core.Pod, error) {
	if name == "" {
		return nil, nil
	}
	pod := new(core.Pod)
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pod, nil
}

func forgerRoleStateKey(forgerUID types.UID, roleName string) string {
	return string(forgerUID) + "/" + roleName
}

func (r *ModelDeploymentForgerReconciler) rememberProbe(forgerUID types.UID, roleName string, attempt forgerProbeAttempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.probes == nil {
		r.probes = make(map[string]forgerProbeAttempt)
	}
	r.probes[forgerRoleStateKey(forgerUID, roleName)] = attempt
}

func (r *ModelDeploymentForgerReconciler) loadProbe(forgerUID types.UID, roleName string) (forgerProbeAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.probes[forgerRoleStateKey(forgerUID, roleName)]
	return v, ok
}

func (r *ModelDeploymentForgerReconciler) forgetProbe(forgerUID types.UID, roleName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.probes, forgerRoleStateKey(forgerUID, roleName))
}

func (r *ModelDeploymentForgerReconciler) rememberPromotion(forgerUID types.UID, roleName string, attempt forgerPromotionAttempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promotions == nil {
		r.promotions = make(map[string]forgerPromotionAttempt)
	}
	r.promotions[forgerRoleStateKey(forgerUID, roleName)] = attempt
}

func (r *ModelDeploymentForgerReconciler) rememberPromotionIfAbsent(
	forgerUID types.UID, roleName string, attempt forgerPromotionAttempt,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promotions == nil {
		r.promotions = make(map[string]forgerPromotionAttempt)
	}
	key := forgerRoleStateKey(forgerUID, roleName)
	if _, exists := r.promotions[key]; !exists {
		r.promotions[key] = attempt
	}
}

func (r *ModelDeploymentForgerReconciler) loadPromotion(forgerUID types.UID, roleName string) (forgerPromotionAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.promotions[forgerRoleStateKey(forgerUID, roleName)]
	return v, ok
}

func (r *ModelDeploymentForgerReconciler) forgetPromotion(forgerUID types.UID, roleName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.promotions, forgerRoleStateKey(forgerUID, roleName))
}

func (r *ModelDeploymentForgerReconciler) forgetForgerState(forgerUID types.UID) {
	prefix := string(forgerUID) + "/"
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.probes {
		if strings.HasPrefix(k, prefix) {
			delete(r.probes, k)
		}
	}
	for k := range r.promotions {
		if strings.HasPrefix(k, prefix) {
			delete(r.promotions, k)
		}
	}
}

func effectiveForgerMode(mdf *workercore.ModelDeploymentForger) workercore.ModelDeploymentForgerMode {
	if mdf == nil || mdf.Spec.Mode == "" {
		return workercore.ModelDeploymentForgerModeShadow
	}
	return mdf.Spec.Mode
}

func effectiveRunnerMode(mdf *workercore.ModelDeploymentForger) workercore.ModelDeploymentForgerRunnerMode {
	if mdf == nil || mdf.Spec.Sandbox.RunnerMode == "" {
		return workercore.ModelDeploymentForgerRunnerModeExternal
	}
	return mdf.Spec.Sandbox.RunnerMode
}

func effectiveProbeSteps(mdf *workercore.ModelDeploymentForger) int {
	if mdf == nil || mdf.Spec.Probe.Steps <= 0 {
		return forgerDefaultProbeSteps
	}
	return int(mdf.Spec.Probe.Steps)
}

func effectiveProbeCooldown(mdf *workercore.ModelDeploymentForger) time.Duration {
	secs := int32(forgerDefaultCooldownSeconds)
	if mdf != nil && mdf.Spec.Probe.CooldownSeconds > 0 {
		secs = max(int32(forgerMinCooldownSeconds), mdf.Spec.Probe.CooldownSeconds)
	}
	return time.Duration(secs) * time.Second
}

func effectiveMinActiveRequests(mdf *workercore.ModelDeploymentForger) int32 {
	if mdf == nil || mdf.Spec.Probe.MinActiveRequests < 0 {
		return 0
	}
	return mdf.Spec.Probe.MinActiveRequests
}

func effectiveMaxKernelsPerGeneration(roleSpec *workercore.ModelDeploymentForgerRoleSpec) int {
	if roleSpec == nil || roleSpec.MaxKernelsPerGeneration <= 0 {
		return forgerDefaultMaxKernels
	}
	return int(roleSpec.MaxKernelsPerGeneration)
}

func effectiveMinSpeedupRatio(roleSpec *workercore.ModelDeploymentForgerRoleSpec) float64 {
	if roleSpec == nil || strings.TrimSpace(roleSpec.MinSpeedupRatio) == "" {
		return modelforger.DefaultMinSpeedupRatio
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(roleSpec.MinSpeedupRatio), 64)
	if err != nil || parsed < modelforger.MinAllowedSpeedupRatio {
		return modelforger.DefaultMinSpeedupRatio
	}
	return parsed
}

func effectiveSandboxTimeoutSeconds(mdf *workercore.ModelDeploymentForger) int64 {
	if mdf == nil || mdf.Spec.Sandbox.ActiveDeadlineSeconds <= 0 {
		return forgerDefaultSandboxTimeoutSeconds
	}
	return mdf.Spec.Sandbox.ActiveDeadlineSeconds
}

func effectiveMaxRetainedGenerations(mdf *workercore.ModelDeploymentForger) int {
	if mdf == nil || mdf.Spec.Rollout.MaxRetainedGenerations <= 0 {
		return forgerDefaultMaxRetainedGenerations
	}
	return int(mdf.Spec.Rollout.MaxRetainedGenerations)
}

func effectiveFailureTimeoutSeconds(mdf *workercore.ModelDeploymentForger) int32 {
	if mdf == nil || mdf.Spec.Rollout.FailureTimeoutSeconds <= 0 {
		return forgerDefaultFailureTimeoutSeconds
	}
	return mdf.Spec.Rollout.FailureTimeoutSeconds
}

// modelDeploymentForgerPredicate ignores self-authored status writes on ModelDeploymentForger.
func modelDeploymentForgerPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.Funcs{
		CreateFunc:  func(ctrlevent.CreateEvent) bool { return true },
		DeleteFunc:  func(ctrlevent.DeleteEvent) bool { return true },
		GenericFunc: func(ctrlevent.GenericEvent) bool { return true },
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			if e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero() {
				return true
			}
			return !slices.Equal(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
		},
	}
}

func modelDeploymentForgerPodPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
		lbls := obj.GetLabels()
		return lbls[modelforger.SandboxLabelName] == "model-deployment-forger" ||
			lbls[modelDeploymentLabelKeyName] == modelDeploymentLabelValueName
	})
}

func modelDeploymentForgerWorkloadPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
		lbls := obj.GetLabels()
		return lbls[modelforger.SandboxLabelName] == "model-deployment-forger" ||
			lbls[modelDeploymentLabelKeyInstance] != ""
	})
}

func (r *ModelDeploymentForgerReconciler) mapTargetDeployment(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	list := new(workercore.ModelDeploymentForgerList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed listing ModelDeploymentForgers for target event", "target", obj.GetName())
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.TargetRef.Name == obj.GetName() {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

func (r *ModelDeploymentForgerReconciler) mapForgerInstanceLabel(
	ctx context.Context, obj ctrlcli.Object, instanceLabel string,
) []ctrlreconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "ModelDeploymentForger" && ref.Name != "" && (ref.Controller == nil || *ref.Controller) {
			return []ctrlreconcile.Request{{
				NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      ref.Name,
				},
			}}
		}
	}
	safeLabel := modelforger.SafeLabelValue(instanceLabel)
	if r.Client != nil {
		list := new(workercore.ModelDeploymentForgerList)
		if err := r.Client.List(ctx, list, ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err == nil {
			var reqs []ctrlreconcile.Request
			for i := range list.Items {
				if modelforger.SafeLabelValue(list.Items[i].Name) == safeLabel || list.Items[i].Name == instanceLabel {
					reqs = append(reqs, ctrlreconcile.Request{
						NamespacedName: ctrlcli.ObjectKeyFromObject(&list.Items[i]),
					})
				}
			}
			if len(reqs) > 0 {
				return reqs
			}
		}
	}
	return []ctrlreconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      safeLabel,
		},
	}}
}

func (r *ModelDeploymentForgerReconciler) mapPod(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	lbls := obj.GetLabels()
	if lbls[modelforger.SandboxLabelName] == "model-deployment-forger" && lbls[modelforger.SandboxLabelInstance] != "" {
		return r.mapForgerInstanceLabel(ctx, obj, lbls[modelforger.SandboxLabelInstance])
	}
	if mdName := lbls[modelDeploymentLabelKeyInstance]; mdName != "" {
		return r.mapTargetDeployment(ctx, &meta.PartialObjectMetadata{
			ObjectMeta: meta.ObjectMeta{Namespace: obj.GetNamespace(), Name: mdName},
		})
	}
	return nil
}

func (r *ModelDeploymentForgerReconciler) mapKernelArtifact(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	mka, ok := obj.(*workercore.ModelKernelArtifact)
	if !ok {
		return nil
	}
	if mka.Spec.Source.NodeStore != nil && mka.Spec.Source.NodeStore.ForgerRef.Name != "" {
		return []ctrlreconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: mka.Namespace,
				Name:      mka.Spec.Source.NodeStore.ForgerRef.Name,
			},
		}}
	}
	list := new(workercore.ModelDeploymentForgerList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(mka.Namespace), ctrlclix.WithoutQuorum); err != nil {
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range list.Items {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

func (r *ModelDeploymentForgerReconciler) mapWorkload(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "Pod" && ref.Name != "" {
			pod, err := r.findPodByName(ctx, obj.GetNamespace(), ref.Name)
			if err == nil && pod != nil {
				return r.mapPod(ctx, pod)
			}
		}
	}
	lbls := obj.GetLabels()
	if lbls[modelforger.SandboxLabelInstance] != "" {
		return r.mapForgerInstanceLabel(ctx, obj, lbls[modelforger.SandboxLabelInstance])
	}
	return nil
}
