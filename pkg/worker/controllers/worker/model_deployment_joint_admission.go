package worker

import (
	"context"
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	kueueadmissioncheck "sigs.k8s.io/kueue/pkg/util/admissioncheck"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
)

const (
	// _JointAdmissionCheckName is the AdmissionCheck object every operator-owned ClusterQueue
	// references. The name and the controller name below are the contract shared with
	// kuberess.InstallModelDeploymentJointAdmissionCheck.
	_JointAdmissionCheckName = "gpustack-model-deployment-joint"

	// _JointAdmissionControllerName claims the check for the reconciler in this file. Kueue routes a
	// Workload's check to whoever declares this name, so no other controller answers it.
	_JointAdmissionControllerName = "worker.gpustack.ai/model-deployment-joint"

	// _JointAdmissionFieldOwner owns this controller's entries in a Workload's admissionChecks.
	_JointAdmissionFieldOwner = "worker.gpustack.ai/model-deployment-joint"
)

// ModelDeploymentJointAdmissionCheckReconciler marks the joint-admission AdmissionCheck Active.
//
// Kueue turns a ClusterQueue referencing an inactive check inactive, so a check nobody activates
// stops the queue admitting anything at all. Activation is therefore the statement that this
// controller is running, and it is made here rather than at install time for exactly that reason.
type ModelDeploymentJointAdmissionCheckReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentJointAdmissionCheckReconciler)(nil)

func (r *ModelDeploymentJointAdmissionCheckReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	ac := new(kueue.AdmissionCheck)
	if err := r.Client.Get(ctx, req.NamespacedName, ac); err != nil {
		logger.Error(err, "fetch admission check")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	if ac.Spec.ControllerName != _JointAdmissionControllerName {
		logger.V(3).Info("skip admission check not owned by this controller")
		return ctrl.Result{}, nil
	}

	if kubemeta.IsConditionTrue(ac.Status.Conditions, kueue.AdmissionCheckActive) {
		return ctrl.Result{}, nil
	}

	kubemeta.SetCondition(&ac.Status.Conditions, meta.Condition{
		Type:    kueue.AdmissionCheckActive,
		Status:  meta.ConditionTrue,
		Reason:  "Ready",
		Message: "the model deployment joint admission check controller is running",
	})
	if err := r.Client.Status().Update(ctx, ac); err != nil {
		logger.Error(err, "mark admission check active")
		return ctrl.Result{}, err
	}

	logger.V(2).Info("activated model deployment joint admission check", "name", ac.Name)

	return ctrl.Result{}, nil
}

func (r *ModelDeploymentJointAdmissionCheckReconciler) SetupController(
	_ context.Context, opts controller.SetupOptions,
) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeploymentjointadmissioncheck").
		For(
			&kueue.AdmissionCheck{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
				ac := obj.(*kueue.AdmissionCheck)

				return ac.Spec.ControllerName == _JointAdmissionControllerName
			})),
		).
		Complete(r)
}

// ModelDeploymentJointAdmissionReconciler gates every group of one ModelDeployment on the whole set
// having reserved quota.
//
// WITH ONE WORKLOAD PER GROUP, KUEUE'S OWN ATOMICITY NO LONGER COVERS THE DEPLOYMENT. A pod group is
// admitted as a unit, which is what makes a single-group deployment all-or-nothing; a deployment
// spread over two instanceTypes is two groups and two Workloads, and nothing relates them. Without
// this check a prefiller can be admitted and serving while its decoder waits for capacity that never
// arrives, which is a deployment that looks half-started and is in fact never going to finish.
//
// IT ANSWERS Ready OR Pending AND NEVER Retry. A Retry does not leave a Workload sitting on its
// reservation: Kueue EVICTS it, resetting the checks and dropping the reservation in two separate
// writes, and its scheduler then refuses to reserve again while a check reads Retry. Two siblings
// waiting on each other through Retry would trade the same quota indefinitely, each one's wait
// destroying the reservation the other was waiting to observe. Pending holds, which is the whole
// reason it is the answer here.
//
// IT NEVER REJECTS AND NEVER PREEMPTS. A deployment whose set is permanently infeasible is bounded
// elsewhere; within this controller a set that cannot assemble waits.
type ModelDeploymentJointAdmissionReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentJointAdmissionReconciler)(nil)

func (r *ModelDeploymentJointAdmissionReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	wl := new(kueue.Workload)
	if err := r.Client.Get(ctx, req.NamespacedName, wl); err != nil {
		logger.Error(err, "fetch workload")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// The same gates the node-devices check states: before reservation there is nothing to confirm,
	// and after eviction or finish the verdict is moot. An evicted Workload still reports a
	// reservation for the window between Kueue's two writes, and overwriting that reset wedges it.
	if !kueueworkload.HasQuotaReservation(wl) || kueueworkload.IsEvicted(wl) ||
		kueueworkload.IsFinished(wl) || !kueueworkload.IsActive(wl) {
		logger.V(3).Info("skip workload not holding quota reservation, evicted, finished or deactivated")

		return ctrl.Result{}, nil
	}
	if kueueworkload.IsAdmitted(wl) {
		logger.V(3).Info("skip already-admitted workload; the barrier has already opened")

		return ctrl.Result{}, nil
	}

	checks, err := kueueadmissioncheck.FilterForController(
		ctx, r.Client, wl.Status.AdmissionChecks, _JointAdmissionControllerName)
	if err != nil {
		logger.Error(err, "filter admission checks for controller")
		return ctrl.Result{}, err
	}
	if len(checks) == 0 {
		return ctrl.Result{}, nil
	}

	md, err := r.workloadModelDeployment(ctx, wl)
	if err != nil {
		logger.Error(err, "resolve the workload's model deployment")
		return ctrl.Result{}, err
	}

	// EVERYTHING THAT IS NOT A MULTI-GROUP ModelDeployment IS READY AT ONCE, AND THAT CASE HAS TO BE
	// PRESENT OR THE CHECK PARKS THE CLUSTER. The check is referenced from every operator-owned
	// queue, so every Workload in them carries it -- including ones this operator did not create. A
	// controller that answered only about its own objects would leave all the others Pending forever
	// with nothing naming the cause.
	if md == nil || len(modelDeploymentPodGroups(md)) < 2 {
		return ctrl.Result{}, r.applyVerdict(ctx, wl, checks, kueue.CheckStateReady,
			"nothing to wait for: this workload is not one group of a multi-group model deployment")
	}

	state, message, err := r.jointVerdict(ctx, md)
	if err != nil {
		logger.Error(err, "judge the deployment's groups")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.applyVerdict(ctx, wl, checks, state, message)
}

// jointVerdict answers Ready when every group of the deployment holds a quota reservation.
//
// THE RESERVATION IS THE OBSERVABLE, not the admission: a sibling still held by this very check has
// reserved and not been admitted, so waiting for admission would be a deadlock in which every group
// waits for a state only the others opening can produce.
//
// A GROUP WITH NO WORKLOAD YET IS NOT READY AND IS NOT AN ERROR. Kueue composes a group's Workload
// only once it has seen that group's whole declared total, so a group still being created simply has
// not got there, and the message says which one.
func (r *ModelDeploymentJointAdmissionReconciler) jointVerdict(
	ctx context.Context, md *workercore.ModelDeployment,
) (kueue.CheckState, string, error) {
	byGroup, err := modelDeploymentReplicaGroups(ctx, r.Client, md)
	if err != nil {
		return "", "", err
	}

	wlList := new(kueue.WorkloadList)
	if err = r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum); err != nil {
		return "", "", fmt.Errorf("list workloads: %w", err)
	}

	var waiting []string
	for _, group := range modelDeploymentPodGroups(md) {
		members := byGroup[group.Name]
		if members != nil && anyWorkloadHoldsQuotaFor(wlList.Items, members) {
			continue
		}
		waiting = append(waiting, group.InstanceType)
	}
	if len(waiting) == 0 {
		return kueue.CheckStateReady, "every group of this deployment has reserved quota", nil
	}

	return kueue.CheckStatePending, fmt.Sprintf(
		"holding this group until the whole deployment can run: %d of %d groups have reserved quota, "+
			"and the ones still waiting are on instance types %s. Every group keeps the quota it has "+
			"reserved while it waits",
		len(modelDeploymentPodGroups(md))-len(waiting), len(modelDeploymentPodGroups(md)),
		strings.Join(waiting, ", ")), nil
}

// modelDeploymentReplicaGroups indexes a deployment's own replicas by the pod group each one joined.
//
// THE LIST IS SCOPED BY THE DEPLOYMENT'S OWN LABEL, not by the namespace. Its callers run on a
// controller that watches every Workload in the cluster, so an unscoped list here is an unbounded
// read in a hot path -- it would cost the whole namespace on every gated Workload's event, and the
// namespaces this runs in are the ones with the most Pods in them.
//
// THE LABEL NARROWS THE READ; OWNERSHIP STILL DECIDES MEMBERSHIP. A label is a value anyone can copy
// onto a Pod, and an owner reference is not.
func modelDeploymentReplicaGroups(
	ctx context.Context, cli ctrlcli.Client, md *workercore.ModelDeployment,
) (map[string]sets.Set[types.UID], error) {
	podList := new(core.PodList)
	err := cli.List(ctx, podList,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{modelDeploymentLabelKeyInstance: md.Name},
		ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("list replicas: %w", err)
	}

	byGroup := make(map[string]sets.Set[types.UID])
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !modelDeploymentOwns(pod, md) {
			continue
		}
		group := pod.Labels[kueuepodconst.GroupNameLabel]
		if byGroup[group] == nil {
			byGroup[group] = sets.New[types.UID]()
		}
		byGroup[group].Insert(pod.UID)
	}

	return byGroup, nil
}

// anyWorkloadHoldsQuotaFor reports whether one of these Workloads owns any of the group's replicas
// and holds a reservation.
func anyWorkloadHoldsQuotaFor(wls []kueue.Workload, members sets.Set[types.UID]) bool {
	for i := range wls {
		wl := &wls[i]
		if modelDeploymentWorkloadOwnsAny(wl, members) && kueueworkload.HasQuotaReservation(wl) {
			return true
		}
	}

	return false
}

// workloadModelDeployment resolves the deployment a Workload's replicas belong to, or nil when the
// Workload is not one of this operator's.
//
// KUEUE OWNS A GROUP'S WORKLOAD FROM ITS MEMBERS, with a plain owner reference per Pod and no
// controller reference, so the path runs Workload to Pod to ModelDeployment. Any owner is enough:
// every member of a group belongs to the same deployment by construction.
func (r *ModelDeploymentJointAdmissionReconciler) workloadModelDeployment(
	ctx context.Context, wl *kueue.Workload,
) (*workercore.ModelDeployment, error) {
	for _, ref := range wl.OwnerReferences {
		if ref.Kind != "Pod" || ref.APIVersion != "v1" {
			continue
		}

		pod := new(core.Pod)
		err := r.Client.Get(ctx,
			ctrlcli.ObjectKey{Namespace: wl.Namespace, Name: ref.Name}, pod, ctrlclix.WithoutQuorum)
		if err != nil {
			if ctrlcli.IgnoreNotFound(err) != nil {
				return nil, err
			}

			continue
		}

		owner := meta.GetControllerOf(pod)
		if owner == nil || owner.Kind != "ModelDeployment" {
			continue
		}

		md := new(workercore.ModelDeployment)
		err = r.Client.Get(ctx,
			ctrlcli.ObjectKey{Namespace: wl.Namespace, Name: owner.Name}, md, ctrlclix.WithoutQuorum)
		if err != nil {
			if ctrlcli.IgnoreNotFound(err) != nil {
				return nil, err
			}

			continue
		}

		return md, nil
	}

	return nil, nil
}

// applyVerdict writes the state for every check this controller owns.
//
// The whole owned set is applied rather than only what changed, for the reason the node-devices
// check states: admissionChecks is a list keyed by name under a server-side apply, so an entry this
// field owner stops claiming is pruned.
func (r *ModelDeploymentJointAdmissionReconciler) applyVerdict(
	ctx context.Context,
	wl *kueue.Workload,
	checks []kueue.AdmissionCheckReference,
	state kueue.CheckState,
	message string,
) error {
	desired, changed := desiredCheckStates(wl, checks, state, message)
	if !changed {
		return nil
	}

	return kueueworkload.PatchStatus(ctx, r.Client, wl, ctrlcli.FieldOwner(_JointAdmissionFieldOwner),
		func(w *kueue.Workload) (bool, error) {
			for i := range desired {
				kueueworkload.SetAdmissionCheckState(&w.Status.AdmissionChecks, desired[i], clock.RealClock{})
			}

			return true, nil
		})
}

// jointSiblings maps a Workload that changed to the other groups of the same deployment.
//
// WITHOUT IT THE BARRIER ONLY EVER CLOSES. A held group's verdict is a statement about a DIFFERENT
// object: it stays Pending until the last sibling reserves quota, and that reservation is written to
// the sibling's Workload. Watching only this controller's own object delivers that event to the
// sibling alone, so the held group is never judged again and a barrier that correctly refused a
// half-feasible set never opens when the set becomes feasible.
//
// IT HIDES IN A CLUSTER WHERE THE GROUPS RESERVE AT NEARLY THE SAME MOMENT, which is the ordinary
// case and not a guarantee: each group's own event then arrives after the others have already
// reserved, and every group opens on its own watch.
func (r *ModelDeploymentJointAdmissionReconciler) jointSiblings(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx)

	wl, ok := obj.(*kueue.Workload)
	if !ok {
		return nil
	}

	md, err := r.workloadModelDeployment(ctx, wl)
	if err != nil {
		logger.Error(err, "resolve the workload's model deployment for sibling mapping")
		return nil
	}
	if md == nil || len(modelDeploymentPodGroups(md)) < 2 {
		return nil
	}

	byGroup, err := modelDeploymentReplicaGroups(ctx, r.Client, md)
	if err != nil {
		logger.Error(err, "index the deployment's replicas for sibling mapping")
		return nil
	}
	members := sets.New[types.UID]()
	for _, group := range byGroup {
		members = members.Union(group)
	}

	wlList := new(kueue.WorkloadList)
	if err = r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum); err != nil {
		logger.Error(err, "list workloads for sibling mapping")
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range wlList.Items {
		sibling := &wlList.Items[i]
		// The changed Workload is left out because the watch below already enqueues it.
		if sibling.Name == wl.Name || !modelDeploymentWorkloadOwnsAny(sibling, members) {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sibling.Namespace, Name: sibling.Name},
		})
	}

	return reqs
}

func (r *ModelDeploymentJointAdmissionReconciler) SetupController(
	_ context.Context, opts controller.SetupOptions,
) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeploymentjointadmission").
		For(&kueue.Workload{}).
		Watches(&kueue.Workload{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.jointSiblings)).
		Complete(r)
}
