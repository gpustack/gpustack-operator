package worker

import (
	"context"
	"strconv"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// PodReconciler reconciles the Kubernetes Pod object,
// which is related to the v1.Instance.
type PodReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*PodReconciler)(nil)

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	pod := new(core.Pod)
	err := r.Client.Get(ctx, req.NamespacedName, pod)
	if err != nil {
		logger.Error(err, "fetch pod")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Skip if deleted.
	if pod.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	// Skip if not running.
	if pod.Status.Phase != core.PodRunning {
		return ctrl.Result{}, nil
	}

	notes := make(map[string]string)

	// Construct the Service and set the owner reference to the Pod.
	eSvc := &core.Service{
		ObjectMeta: kubemeta.SanitizeObjectMeta(pod.ObjectMeta),
		Spec: core.ServiceSpec{
			Selector: pod.Labels,
			Type:     core.ServiceTypeNodePort,
			Ports: func() (ret []core.ServicePort) {
				for i := range pod.Spec.Containers {
					for j := range pod.Spec.Containers[i].Ports {
						ret = append(ret, core.ServicePort{
							Name:       pod.Spec.Containers[i].Ports[j].Name,
							Port:       pod.Spec.Containers[i].Ports[j].ContainerPort,
							Protocol:   pod.Spec.Containers[i].Ports[j].Protocol,
							TargetPort: intstr.FromInt32(pod.Spec.Containers[i].Ports[j].ContainerPort),
						})
					}
				}
				return ret
			}(),
		},
	}
	kubemeta.ControlOnWithoutBlock(eSvc, pod, core.SchemeGroupVersion.WithKind("Pod"))
	aSvc, err := kubeclientset.CreateWithCtrlClient(ctx, r.Client, eSvc)
	if err != nil {
		logger.Error(err, "create service")
		return ctrl.Result{}, err
	}

	// Retrieve the external IP address of the node where the Pod is running for better visibility.
	eNode := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: pod.Spec.NodeName,
		},
	}
	err = r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(eNode), eNode)
	if err == nil {
		for i := range eNode.Status.Addresses {
			if eNode.Status.Addresses[i].Type == core.NodeExternalIP {
				notes["externalHostIP"] = eNode.Status.Addresses[i].Address
				break
			}
		}
	}

	// Annotate the Pod with the Service node port info.
	for i := range aSvc.Spec.Ports {
		if aSvc.Spec.Ports[i].NodePort == 0 {
			continue
		}
		notes[aSvc.Spec.Ports[i].Name] = strconv.Itoa(int(aSvc.Spec.Ports[i].NodePort))
	}
	if len(notes) > 0 && !systemmeta.NoteResource(pod, "instances", notes) {
		podAlignFn := func(aPod *core.Pod) (_ *core.Pod, skip bool, err error) {
			skip = true
			// Align notes.
			if !systemmeta.EqualResourceTypeAndNotes(pod, aPod) {
				systemmeta.NoteResource(aPod, "instances", notes)
				skip = false
			}
			return aPod, skip, nil
		}
		_, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, pod,
			kubeclientset.WithUpdateAlign(podAlignFn))
		if err != nil {
			logger.Error(err, "annotate pod with service node port info")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *PodReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.pods").
		For(
			// Focus on the Kubernetes Pod.
			&core.Pod{},
			ctrlbuilder.WithPredicates(
				// Only reconcile when the Pod has the "instances" resource note,
				// which means the Pod is related to the worker Instance.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instances")
				}),
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldPod, newPod := e.ObjectOld.(*core.Pod), e.ObjectNew.(*core.Pod)
						if newPod.GetDeletionTimestamp() != nil {
							return false
						}
						return oldPod.Status.Phase != newPod.Status.Phase && newPod.Status.Phase == core.PodRunning
					},
				},
			),
		).
		Watches(
			// Enqueue the Kubernetes Pod when its corresponding Service is ready.
			&core.Service{},
			ctrlhandler.EnqueueRequestsFromMapFunc(
				r.findObjectWhenServiceUpdating,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						return !systemmeta.MatchResource(e.ObjectNew, "instances")
					},
				}),
			),
		).
		Complete(r)
}

func (r *PodReconciler) findObjectWhenServiceUpdating(_ context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	if obj == nil {
		return nil
	}

	svc := obj.(*core.Service)
	if len(svc.Spec.Ports) == 0 {
		return nil
	}

	// Ensure the Service has been assigned with node ports,
	// which means the Service is ready.
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].NodePort == 0 {
			return nil
		}
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{
				Namespace: svc.Namespace,
				Name:      svc.Name,
			},
		},
	}
	return reqs
}
