package worker

import (
	"context"
	"fmt"
	"time"

	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// ModelDeploymentReconciler reconciles v1alpha1.ModelDeployment objects to finish the following
// tasks:
//   - Render one Kubernetes Pod per replica of each role, owned by the ModelDeployment, carrying the
//     entrance label that routes it into the role's pool.
//   - Converge that set continuously: recreate a replica that was deleted, remove one that scaled
//     away, and rebuild one whose spec no longer matches what it was built from.
//
// It creates NO Instance. An Instance renders exactly one Pod and its spec is immutable after
// creation, so routing replicas through it would make "one replica, several Pods" inexpressible and
// degenerate every rollout into recreate-everything. The admission chain keys on Pods, and a plain
// Pod is a first-class citizen of it.
type ModelDeploymentReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
	Recorder  ctrlrecord.EventRecorder
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentReconciler)(nil)

func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	md := new(workercore.ModelDeployment)
	err := r.Client.Get(ctx, req.NamespacedName, md)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch model deployment")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Clean up if the ModelDeployment is marked as deleted.
	if md.DeletionTimestamp != nil {
		return r.teardownModelDeployment(ctx, md)
	}

	// Lock.
	if !systemmeta.Lock(md) {
		err = r.Client.Update(ctx, md)
		if err != nil {
			logger.Error(err, "lock model deployment")
			return ctrl.Result{}, err
		}
	}

	return r.convergeModelDeployment(ctx, md)
}

// teardownModelDeployment deletes the replicas and releases the finalizer once they are gone.
//
// The finalizer is held until the last Pod has actually left rather than dropped as soon as the
// deletes are issued, because a replica that outlives its owner keeps holding accelerators that the
// admission ledger has already stopped accounting for.
func (r *ModelDeploymentReconciler) teardownModelDeployment(
	ctx context.Context, md *workercore.ModelDeployment,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	if systemmeta.IsLocked(md) {
		pods, err := r.listModelDeploymentPods(ctx, md)
		if err != nil {
			logger.Error(err, "list replicas")
			return ctrl.Result{}, err
		}

		if len(pods) > 0 {
			// Report the phase before issuing the deletes, so an operator watching the object sees
			// Deleting rather than the last Ready it happened to reach.
			if err = r.syncModelDeploymentStatus(ctx, md, pods); err != nil {
				logger.Error(err, "update model deployment status to deleting")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}

			for i := range pods {
				if pods[i].DeletionTimestamp != nil {
					continue
				}
				if err = r.Client.Delete(ctx, &pods[i]); err != nil && !kerrors.IsNotFound(err) {
					logger.Error(err, "delete replica", "pod", pods[i].Name)
					return ctrl.Result{}, err
				}
			}

			logger.V(3).Info("replica deletion in progress; requeue in 2s", "replicas", len(pods))

			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// Unlock.
	if systemmeta.Unlock(md) {
		logger.V(3).Info("skip deleted model deployment")
		return ctrl.Result{}, nil
	}

	err := r.Client.Update(ctx, md)
	if err != nil {
		logger.Error(err, "unlock model deployment")
	}

	return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
}

// convergeModelDeployment brings the rendered replicas in line with the spec.
//
// It is level-based: it renders what the spec says should exist, compares that with what does, and
// issues only the difference. A pass over a spec that has not changed writes nothing, which is what
// keeps a controller that runs on every Pod event from rewriting the world.
func (r *ModelDeploymentReconciler) convergeModelDeployment(
	ctx context.Context, md *workercore.ModelDeployment,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	desired, err := r.renderModelDeploymentPods(ctx, md)
	if err != nil {
		// A render failure is the InstanceType not being ready, or a role the renderer cannot build
		// a container from. Both are visible to the reader through the controller's own retry, and
		// the second cannot be reached at all without bypassing admission.
		logger.Error(err, "render replicas")
		return ctrl.Result{}, err
	}

	actual, err := r.listModelDeploymentPods(ctx, md)
	if err != nil {
		logger.Error(err, "list replicas")
		return ctrl.Result{}, err
	}

	for i := range actual {
		pod := &actual[i]
		if pod.DeletionTimestamp != nil {
			// Already on its way out; a create issued now would race the delete and be rejected.
			continue
		}

		want, wanted := desired[pod.Name]
		if !wanted {
			// Scaled away, or renamed by a role rename.
			logger.Info("removing replica no longer in the spec", "pod", pod.Name)
			if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete replica", "pod", pod.Name)
				return ctrl.Result{}, err
			}

			continue
		}

		delete(desired, pod.Name)

		if pod.Annotations[modelDeploymentPodSpecHashAnnotation] == want.Annotations[modelDeploymentPodSpecHashAnnotation] {
			continue
		}

		// The rollout is recreate rather than surge: a replica built before a spec change is deleted
		// here and rebuilt by the pass that observes its absence. The cost is this replica's cached
		// blocks, which its siblings lose when it goes.
		logger.Info("recreating replica built from an earlier spec", "pod", pod.Name)
		if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
			logger.Error(err, "delete outdated replica", "pod", pod.Name)
			return ctrl.Result{}, err
		}
	}

	// A name still held by a terminating replica is not a failure, so it does not end the pass: the
	// observations below run either way. Returning here instead would mean that during exactly the
	// window a departure event is for — a replica on its way out — no event and no status were
	// written at all.
	var requeue bool
	for name := range desired {
		if err = r.Client.Create(ctx, desired[name]); err != nil {
			if kerrors.IsAlreadyExists(err) {
				// A Pod under this name is still terminating. It is waited for, not adopted:
				// adopting one would keep a replica built from an earlier spec alive under a name
				// the current spec claims.
				logger.V(3).Info("replica name still taken; requeue in 2s", "pod", name)
				requeue = true

				continue
			}
			logger.Error(err, "create replica", "pod", name)

			return ctrl.Result{}, err
		}
		logger.Info("created replica", "pod", name)
	}

	if err = r.syncModelDeploymentService(ctx, md); err != nil {
		logger.Error(err, "sync service")
		return ctrl.Result{}, err
	}

	// Read the replicas back rather than reusing the list this pass started from: status must
	// describe what exists now, not the snapshot the convergence decided against.
	actual, err = r.listModelDeploymentPods(ctx, md)
	if err != nil {
		logger.Error(err, "list replicas")
		return ctrl.Result{}, err
	}

	r.recordModelDeploymentDepartures(md, actual)

	if err = r.syncModelDeploymentStatus(ctx, md, actual); err != nil {
		logger.Error(err, "sync status")
		return ctrl.Result{}, err
	}

	if requeue {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// recordModelDeploymentDepartures records one event per replica that has left, so an operator
// correlating a burst of failed requests with a preemption has the correlation written down rather
// than having to infer it.
func (r *ModelDeploymentReconciler) recordModelDeploymentDepartures(
	md *workercore.ModelDeployment, pods []core.Pod,
) {
	if r.Recorder == nil {
		return
	}

	for _, d := range modelDeploymentReplicaDepartures(pods) {
		r.Recorder.Event(md, core.EventTypeWarning, d.reason, d.message)
	}
}

// syncModelDeploymentService converges the one Service the deployment is reached through.
//
// It aligns an existing Service rather than replacing it, because the allocated ClusterIP is state
// the operator did not write and every client that resolved the name is still using.
func (r *ModelDeploymentReconciler) syncModelDeploymentService(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	expected := renderModelDeploymentService(md)

	actual := new(core.Service)
	err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(expected), actual, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return err
		}

		return ctrlcli.IgnoreAlreadyExists(r.Client.Create(ctx, expected))
	}

	if !modelDeploymentOwns(actual, md) {
		// A Service of this name that belongs to something else is left alone: taking it over would
		// redirect whatever already points at it.
		return fmt.Errorf("service %s/%s is not owned by this deployment", actual.Namespace, actual.Name)
	}

	if !alignModelDeploymentService(actual, expected) {
		return nil
	}

	return r.Client.Update(ctx, actual)
}

// renderModelDeploymentPods renders every replica of every role, keyed by Pod name.
func (r *ModelDeploymentReconciler) renderModelDeploymentPods(
	ctx context.Context, md *workercore.ModelDeployment,
) (map[string]*core.Pod, error) {
	// The overcommit setting is the Instance path's, deliberately: it decides how a declared
	// resource becomes a request, and this renderer derives the same values the Instance webhook
	// does. A second knob for one translation would let the two disagree on one cluster.
	overcommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)

	desired := make(map[string]*core.Pod)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]

		instType, err := r.getModelDeploymentInstanceType(ctx, role.InstanceType)
		if err != nil {
			return nil, err
		}

		in := ModelDeploymentRenderInput{
			Deployment:                 md,
			Role:                       role,
			InstanceType:               instType,
			RuntimeClassName:           r.getModelDeploymentRuntimeClassName(ctx, instType),
			GeneralResourcesOvercommit: overcommit,
			// The connector and its ConfigMap arrive with the rule that resolves the referenced
			// Binding. Until then a replica is rendered with no connector at all, which is also what
			// a deployment whose Binding cannot be read gets.
		}

		for ordinal := range role.Replicas {
			in.Ordinal = ordinal
			pod, err := renderModelDeploymentPod(in)
			if err != nil {
				return nil, err
			}
			desired[pod.Name] = pod
		}
	}

	return desired, nil
}

// getModelDeploymentInstanceType reads the InstanceType a role is admitted against.
//
// A missing type is an error rather than a render without one: the type supplies both how to spell
// the accelerator keys and the per-card resources the host request is derived from, so a Pod
// rendered without it would ask for something other than what the role declared.
func (r *ModelDeploymentReconciler) getModelDeploymentInstanceType(
	ctx context.Context, name string,
) (*worker.InstanceType, error) {
	instType := new(worker.InstanceType)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, instType,
		ctrlclix.WithoutQuorum)
	if err == nil {
		return instType, nil
	}
	if !kerrors.IsNotFound(err) {
		return nil, fmt.Errorf("get instance type %s: %w", name, err)
	}

	// Maybe the InstanceType is not cached yet; read through to the API server rather than treat a
	// cold cache as a missing type.
	err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: name}, instType,
		ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("get instance type %s: %w", name, err)
	}

	return instType, nil
}

// getModelDeploymentRuntimeClassName reports the runtime class an accelerated replica needs, and ""
// when it needs none or the cluster does not have it.
//
// A missing class is not an error: it is how a cluster that runs the vendor's runtime under a
// different name, or not at all, reports itself. It is looked up rather than assumed because a
// Pod naming a RuntimeClass that does not exist is rejected outright.
func (r *ModelDeploymentReconciler) getModelDeploymentRuntimeClassName(
	ctx context.Context, instType *worker.InstanceType,
) string {
	if !instType.Spec.Acceleratable {
		return ""
	}

	name := nodefeature.GetAcceleratableRuntimeName(instType.Status.Detail.Manufacturer)
	if name == "" {
		return ""
	}

	rc := new(node.RuntimeClass)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, rc, ctrlclix.WithoutQuorum); err != nil {
		return ""
	}

	return name
}

// listModelDeploymentPods returns the replicas this deployment owns.
//
// It selects on the identity labels and then confirms the controller reference, so a Pod that
// carries the labels but belongs to a deployment of the same name that has since been recreated is
// not adopted by the new one.
func (r *ModelDeploymentReconciler) listModelDeploymentPods(
	ctx context.Context, md *workercore.ModelDeployment,
) ([]core.Pod, error) {
	podList := new(core.PodList)
	err := r.Client.List(ctx, podList,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		},
		ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, err
	}

	owned := make([]core.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		if !modelDeploymentOwns(&podList.Items[i], md) {
			continue
		}
		owned = append(owned, podList.Items[i])
	}

	return owned, nil
}

// modelDeploymentOwns reports whether the object is controlled by this deployment.
func modelDeploymentOwns(obj ctrlcli.Object, md *workercore.ModelDeployment) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == md.UID {
			return true
		}
	}

	return false
}

func (r *ModelDeploymentReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()
	r.Recorder = opts.Manager.GetEventRecorderFor("modeldeployment")

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeployment").
		For(
			&workercore.ModelDeployment{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.GenerationChangedPredicate{},
			),
		).
		Owns(
			// Watch the replicas this deployment renders. Ownership rather than a label match is
			// what enqueues here, so a Pod that has lost its owner cannot keep waking a deployment
			// that no longer claims it.
			&core.Pod{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, ModelDeploymentResourceType)
				}),
			),
		).
		Complete(r)
}
