package worker

import (
	"context"
	"fmt"
	"slices"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/setting"
)

const (
	transferDrainFor        = "model-transfer.gpustack.ai/drain-for"
	transferDrainedFor      = "model-transfer.gpustack.ai/drained-for"
	transferDrainedInstance = "model-transfer.gpustack.ai/drained-instance"
	transferWorkerInstance  = "model-transfer.gpustack.ai/worker-instance"
)

// ModelTransferImageReconciler rolls OnDelete chart-prepared source fleets only after every worker
// acknowledges release of its own native instance. Unknown release blocks replacement.
type ModelTransferImageReconciler struct {
	Client ctrlcli.Client
	Reader ctrlcli.Reader
}

func (r *ModelTransferImageReconciler) reader() ctrlcli.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

func (r *ModelTransferImageReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	reader := r.reader()
	sets := new(apps.DaemonSetList)
	if err := reader.List(ctx, sets, ctrlcli.InNamespace(setting.DelegatedSecretNamespace), ctrlcli.MatchingLabels{
		"app.kubernetes.io/part-of": "gpustack-operator", "app.kubernetes.io/component": "model-transfer",
	}); err != nil {
		return ctrl.Result{}, err
	}
	byVendor := map[string][]*apps.DaemonSet{}
	for i := range sets.Items {
		ds := &sets.Items[i]
		if ds.Labels["app.kubernetes.io/managed-by"] != "Helm" || ds.Labels["app.kubernetes.io/instance"] == "" || ds.UID == "" {
			continue
		}
		v := ds.Labels["gpustack.ai/manufacturer"]
		if v == "amd" || v == "ascend" || v == "nvidia" {
			byVendor[v] = append(byVendor[v], ds)
		}
	}
	pending := false
	for vendor, matches := range byVendor {
		if len(matches) != 1 {
			return ctrl.Result{}, fmt.Errorf("ambiguous model-transfer DaemonSets for %s", vendor)
		}
		done, err := r.reconcileFleet(ctx, matches[0])
		if err != nil {
			return ctrl.Result{}, err
		}
		pending = pending || !done
	}
	if pending {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ModelTransferImageReconciler) reconcileFleet(ctx context.Context, ds *apps.DaemonSet) (bool, error) {
	if ds.Spec.UpdateStrategy.Type != apps.OnDeleteDaemonSetStrategyType {
		return false, fmt.Errorf("DaemonSet %s must use OnDelete", ds.Name)
	}
	idx := slices.IndexFunc(ds.Spec.Template.Spec.Containers, func(c core.Container) bool { return c.Name == "worker" })
	if idx < 0 {
		return false, fmt.Errorf("DaemonSet %s has no worker container", ds.Name)
	}
	image := ds.Spec.Template.Spec.Containers[idx].Image
	if image == "" {
		return false, fmt.Errorf("DaemonSet %s has an empty worker image", ds.Name)
	}

	selector, err := meta.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return false, err
	}
	pods := new(core.PodList)
	if err := r.reader().List(ctx, pods, ctrlcli.InNamespace(ds.Namespace),
		ctrlcli.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, err
	}
	owned := make([]core.Pod, 0)
	nodes := map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		owner := meta.GetControllerOf(p)
		if owner == nil || owner.UID != ds.UID || owner.Kind != "DaemonSet" {
			continue
		}
		if p.UID == "" || p.Spec.NodeName == "" || nodes[p.Spec.NodeName] {
			return false, nil
		}
		nodes[p.Spec.NodeName] = true
		owned = append(owned, *p)
	}
	changed := false
	for i := range owned {
		j := slices.IndexFunc(owned[i].Spec.Containers, func(c core.Container) bool { return c.Name == "worker" })
		if j < 0 || owned[i].Spec.Containers[j].Image != image || owned[i].Annotations[transferDrainFor] != "" {
			changed = true
		}
	}
	if !changed {
		return true, nil
	}

	// The DS status must reflect this exact template and all scheduled Pods. Zero is not release proof.
	if ds.Status.ObservedGeneration != ds.Generation || ds.Status.DesiredNumberScheduled == 0 ||
		int32(len(owned)) != ds.Status.DesiredNumberScheduled || ds.Status.CurrentNumberScheduled != ds.Status.DesiredNumberScheduled {
		return false, nil
	}
	// A drained native boot cannot resume admission. Replace even a matching-image
	// Pod after its exact acknowledgement; a newly created Pod starts unfrozen.
	candidates := make([]core.Pod, 0, len(owned))
	for i := range owned {
		pod := &owned[i]
		j := slices.IndexFunc(pod.Spec.Containers, func(c core.Container) bool { return c.Name == "worker" })
		if j < 0 || pod.DeletionTimestamp != nil {
			return false, nil
		}
		if pod.Spec.Containers[j].Image == image {
			if prior := pod.Annotations[transferDrainFor]; prior != "" {
				instance := pod.Annotations[transferWorkerInstance]
				if instance == "" || pod.Annotations[transferDrainedFor] != prior || pod.Annotations[transferDrainedInstance] != instance {
					return false, nil
				}
				if err := r.Client.Delete(ctx, pod,
					ctrlcli.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}); ctrlcli.IgnoreNotFound(err) != nil {
					return false, err
				}
				return false, nil
			}
			continue
		}
		candidates = append(candidates, *pod)
		if pod.Annotations[transferDrainFor] != image {
			fresh := pod.DeepCopy()
			if fresh.Annotations == nil {
				fresh.Annotations = map[string]string{}
			}
			fresh.Annotations[transferDrainFor] = image
			delete(fresh.Annotations, transferDrainedFor)
			delete(fresh.Annotations, transferDrainedInstance)
			if err := r.Client.Update(ctx, fresh); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	for i := range candidates {
		p := &candidates[i]
		instance := p.Annotations[transferWorkerInstance]
		if instance == "" || p.Annotations[transferDrainedFor] != image || p.Annotations[transferDrainedInstance] != instance {
			return false, nil
		}
	}
	// Delete with UID and resourceVersion preconditions; a recreated Pod never inherits proof.
	for i := range candidates {
		p := &candidates[i]
		if err := r.Client.Delete(ctx, p, ctrlcli.Preconditions{UID: &p.UID, ResourceVersion: &p.ResourceVersion}); ctrlcli.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	return false, nil
}

func (r *ModelTransferImageReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.Reader = opts.Manager.GetAPIReader()
	return ctrl.NewControllerManagedBy(opts.Manager).Named("modeltransferimage").
		For(&apps.DaemonSet{}, ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
			return obj.GetNamespace() == setting.DelegatedSecretNamespace && obj.GetLabels()["app.kubernetes.io/component"] == "model-transfer"
		}))).
		Watches(&core.Pod{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueue),
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
				return obj.GetNamespace() == setting.DelegatedSecretNamespace && obj.GetLabels()["app.kubernetes.io/component"] == "model-transfer"
			}))).
		Complete(r)
}

func (r *ModelTransferImageReconciler) enqueue(_ context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	if obj.GetNamespace() != setting.DelegatedSecretNamespace {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName}}}
}
