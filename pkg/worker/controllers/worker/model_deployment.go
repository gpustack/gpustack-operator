package worker

import (
	"context"
	"fmt"
	"slices"
	"time"

	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
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

	// CacheScraper reads each replica's own account of its cache client. It is an interface rather
	// than a dial this reconciler makes, because every case the condition it feeds has to get right
	// is a failure, and a real dial cannot be made to fail on demand.
	//
	// A nil scraper is every replica unreadable, which the condition reports as Unknown. It is nil
	// today: the concrete per-engine reader is not written, and inventing a metric name would be the
	// exact assumption the condition exists to refuse.
	CacheScraper ModelDeploymentCacheScraper
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
			// Deleting rather than the last Ready it happened to reach. The Binding is deliberately
			// not re-read: a teardown pass has no question to ask it, and the domain a replica is
			// still writing into is the one that was last observed.
			if err = r.syncModelDeploymentStatus(ctx, md, pods, nil); err != nil {
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

		// The last replica has left, so the claim on the Binding can go. Released any earlier, the
		// authorization could be deleted from under a process that is still writing through it.
		if err := r.releaseModelDeploymentBinding(ctx, md); err != nil {
			logger.Error(err, "release kv cache pool binding")
			return ctrl.Result{}, err
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

	// Resolved first, and its verdict deliberately does NOT gate the convergence below. A Binding
	// that is missing or briefly not usable — a store leader restart makes every Binding not-Ready
	// for tens of seconds — leaves the replicas serving; the condition is the signal. Refusing to
	// converge here would turn a routine upgrade of the store into an outage of every deployment on
	// it.
	domain, err := r.resolveModelDeploymentDomain(ctx, md)
	if err != nil {
		logger.Error(err, "resolve kv cache pool binding")
		return ctrl.Result{}, err
	}

	// Claimed whatever the verdict was, so long as the Binding could be read at all: the claim is
	// what holds an admin's delete off a deployment that is still writing, and a claim that came and
	// went with readiness would open exactly that window.
	if err = r.claimModelDeploymentBinding(ctx, md); err != nil {
		logger.Error(err, "claim kv cache pool binding")
		return ctrl.Result{}, err
	}

	// Resolved once per pass, not per role: the endpoint and the transport belong to the pool and its
	// backend. A nil result means there is nothing to connect to yet, and the replicas are rendered
	// without a connector rather than with a partial one.
	connection, err := r.resolveModelDeploymentConnection(ctx, domain)
	if err != nil {
		logger.Error(err, "resolve kv cache connection")
		return ctrl.Result{}, err
	}

	desired, err := r.renderModelDeploymentPods(ctx, md, connection)
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

		if connection == nil && md.Status.KVCache != nil {
			// LOSING THE CONNECTOR IS NOT A REASON TO REBUILD A REPLICA, and without this the hash
			// makes it one. A pass that cannot resolve the connection renders replicas without a
			// connector, so every running replica's hash differs from the desired one and the
			// recreate below would fire on all of them at once.
			//
			// The guard is BOTH halves. A deployment that never resolved a domain has no connector
			// to lose, so a hash difference there comes from the spec and must still roll out --
			// suppressing it on `connection == nil` alone breaks the ordinary rollout of every
			// deployment that does not use a cache at all, which is what the spec-change test says
			// when this branch is written without the second half.
			//
			// What reaches that state is ordinary rather than exotic: a store leader restart makes
			// every Binding on the pool briefly not-Ready -- measured at 3.5 to 32 seconds in this
			// project -- and a deployment whose Binding is not usable resolves no connection. So a
			// few seconds of store unavailability would delete every replica of every deployment on
			// that pool, and each one then reloads its weights. The blip becomes the outage.
			//
			// Leaving them alone is also what this design already decided for the neighboring case:
			// an admin deleting the Binding leaves running Pods running, because tearing down a
			// serving deployment because an admin object vanished is worse than serving without a
			// cache. The same reasoning covers a Binding that is merely unwell.
			//
			// Nothing is lost when it comes back. The connector resolves to the same values, the
			// desired hash returns to what these replicas already carry, and this branch stops
			// firing. If it comes back with a DIFFERENT endpoint, the hash differs from the one they
			// carry and they are recreated on that pass -- which is the rollout that should happen.
			//
			// New replicas are still created below, without a connector: a replica that does not
			// exist yet cannot be given an address that does not exist yet either.
			logger.V(3).Info("no connection this pass; leaving the replica as built",
				"pod", pod.Name)

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

	if err = r.syncModelDeploymentStatus(ctx, md, actual, domain); err != nil {
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

// recordModelDeploymentRuntimeVersionSkew records that a role's pool disagrees on a runtime
// version, and does so only for a role whose image the operator SYNTHESIZES.
//
// A role that states its own image is unaffected by the pool's version spread: the operator did not
// choose that tag and the spread tells its owner nothing actionable. Emitting it anyway would train
// readers to ignore the reason they need when the image IS synthesized.
//
// It fires on every pass while the disagreement lasts, which is deliberate. A driver rollout is a
// standing condition rather than an edge, and the API server folds repeats of one
// (object, reason, message) into a single event with a count -- so a standing warning stays visible
// for as long as it is true instead of scrolling away, which is precisely what an operator
// diagnosing an ImagePullBackOff hours later needs.
func (r *ModelDeploymentReconciler) recordModelDeploymentRuntimeVersionSkew(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, instType *worker.InstanceType,
) {
	if r.Recorder == nil {
		return
	}
	if role.Template != nil && role.Template.Image != "" {
		return
	}

	message, ok := modelDeploymentRuntimeVersionSkew(role.Name, instType.Status.Detail)
	if !ok {
		return
	}

	r.Recorder.Event(md, core.EventTypeWarning, modelDeploymentEventRuntimeVersionSkew, message)
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
	connection *ModelDeploymentConnectorInput,
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

		// Recorded here rather than in a pass of its own, because this is the one place that
		// already holds both the role and its InstanceType: a second loop would read the same
		// object again for a message.
		r.recordModelDeploymentRuntimeVersionSkew(md, role, instType)

		in := ModelDeploymentRenderInput{
			Deployment:                 md,
			Role:                       role,
			InstanceType:               instType,
			RuntimeClassName:           r.getModelDeploymentRuntimeClassName(ctx, instType),
			GeneralResourcesOvercommit: overcommit,
		}

		// The connector is synthesized PER ROLE even though its connection is per deployment,
		// because the accelerator is the role's: it selects the store connector the engine
		// registers, and only the role's InstanceType knows it.
		//
		// A nil connection renders replicas with NO connector, which is what a deployment whose
		// Binding has not resolved gets. A take-over role also gets none, but that decision is the
		// renderer's rather than this loop's -- it holds the template. Synthesizing here for a role
		// that will discard it costs a pure function call and keeps one rule in one place.
		if connection != nil {
			roleConnection := *connection
			roleConnection.Engine = md.Spec.Engine
			roleConnection.Manufacturer = instType.Status.Detail.Manufacturer

			connector, err := SynthesizeModelDeploymentConnector(roleConnection)
			if err != nil {
				return nil, fmt.Errorf("role %q cannot be given a cache client: %w", role.Name, err)
			}
			in.Connector = connector
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
		Owns(
			// The Service is reconciled by this controller, so it has to be watched by it. Without
			// this, an externally deleted or edited Service is corrected only when something else
			// happens to wake the deployment, or at the next resync hours away: the endpoint stays
			// broken while reconcile code that would realign it never runs.
			//
			// A watch is what makes the convergence level-based rather than a one-shot at creation.
			// The gap does not show up in manual testing, because nobody deletes the Service by
			// hand -- it shows up when something else in the cluster does.
			&core.Service{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, ModelDeploymentResourceType)
				}),
			),
		).
		Watches(
			// A Binding's readiness is observed rather than declared, so the deployment has to be
			// woken when it moves. Without this the transition would only be noticed at the next
			// resync, which is hours away: a store leader restart would leave every deployment on it
			// reporting a domain it had already regained.
			&workercore.KVCachePoolBinding{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentBinding),
		).
		Watches(
			// The InstanceType's OBSERVED detail is now an input to the rendered Pod: a role that
			// names no image has one synthesized from the pool's accelerator runtime version, so a
			// driver rollout changes the image every replica should be running. Without this watch
			// that change would be picked up only when something else woke the deployment, and the
			// spec-hash comparison would go on matching a Pod built from a version the pool no
			// longer reports.
			//
			// No GenerationChangedPredicate here, and that is the point: what moves is the status,
			// which does not bump the generation. The predicate on the primary object would filter
			// out exactly the updates this watch exists for.
			&worker.InstanceType{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentInstanceType),
		).
		Watches(
			// The pool's published client endpoint is the address every replica dials, and it is
			// rendered into the Pod. It MOVES: a store leader restart or a recreated Service gives
			// the pool a new address, and the deployment's own Binding can stay Ready across that,
			// so the Binding watch above does not cover it. Without this watch the spec hash goes on
			// matching replicas built from an address nobody answers, which is the one failure this
			// design refuses to render on purpose -- from outside the Pod it is indistinguishable
			// from a cache miss.
			&workercore.KVCachePool{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentPool),
		).
		Watches(
			// The backend's transport protocol is the connector's other rendered input, and
			// `spec.transport` is absent from the backend webhook's immutability rule, so an admin
			// can edit it on a running pool. Same staleness, same silence.
			&workercore.KVCacheBackend{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentBackend),
		).
		Complete(r)
}

// mapModelDeploymentPool enqueues every deployment attached to a pool, through the Bindings that
// grant access to it.
//
// It goes pool -> Bindings -> deployments rather than listing every deployment and resolving each
// one's Binding: the Binding is what names the pool, and a cluster has far fewer Bindings than the
// N+1 reads that walk would cost. Each deployment matches exactly one Binding, so no request is
// emitted twice.
func (r *ModelDeploymentReconciler) mapModelDeploymentPool(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	kvcpbList := new(workercore.KVCachePoolBindingList)
	if err := r.Client.List(ctx, kvcpbList, ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list kv cache pool bindings for kv cache pool",
			"kv cache pool", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range kvcpbList.Items {
		kvcpb := &kvcpbList.Items[i]
		if kvcpb.Spec.PoolRef.Name != obj.GetName() {
			continue
		}
		reqs = append(reqs, r.mapModelDeploymentBinding(ctx, kvcpb)...)
	}

	return reqs
}

// mapModelDeploymentBackend enqueues every deployment on a pool that names this backend.
//
// One more hop than the pool's, for the same reason: the deployment references a Binding, the
// Binding a pool, and the pool a backend. A pool admits exactly one backend, but the field is a
// list, so membership is tested rather than equality.
func (r *ModelDeploymentReconciler) mapModelDeploymentBackend(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	kvcpList := new(workercore.KVCachePoolList)
	if err := r.Client.List(ctx, kvcpList, ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list kv cache pools for kv cache backend",
			"kv cache backend", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range kvcpList.Items {
		kvcp := &kvcpList.Items[i]
		if !slices.Contains(kvcp.Spec.Backends, obj.GetName()) {
			continue
		}
		reqs = append(reqs, r.mapModelDeploymentPool(ctx, kvcp)...)
	}

	return reqs
}

// mapModelDeploymentInstanceType enqueues every deployment with a role admitted against the type.
//
// The scan crosses namespaces, unlike the Binding's, and it has to: an InstanceType is
// cluster-scoped, so the deployments referencing one are not confined to any namespace. The set
// walked is every ModelDeployment on the cluster, which is bounded by how many a cluster has rather
// than by how many nodes or Pods it runs.
func (r *ModelDeploymentReconciler) mapModelDeploymentInstanceType(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	mdList := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mdList, ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for instance type",
			"instance type", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range mdList.Items {
		md := &mdList.Items[i]
		for j := range md.Spec.Roles {
			if md.Spec.Roles[j].InstanceType != obj.GetName() {
				continue
			}
			reqs = append(reqs, ctrlreconcile.Request{
				NamespacedName: ctrlcli.ObjectKeyFromObject(md),
			})

			break // one request per deployment, however many of its roles match
		}
	}

	return reqs
}

// mapModelDeploymentBinding enqueues the deployments in a Binding's namespace that reference it.
//
// The scan is a namespaced List rather than an index because the reference is a plain field on a
// namespaced object: the query never crosses a namespace, so the set it walks is one team's
// deployments.
func (r *ModelDeploymentReconciler) mapModelDeploymentBinding(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	mdList := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mdList,
		ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for kv cache pool binding",
			"kv cache pool binding", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range mdList.Items {
		md := &mdList.Items[i]
		if md.Spec.KVCache.PoolRef.Name != obj.GetName() {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKeyFromObject(md),
		})
	}

	return reqs
}
