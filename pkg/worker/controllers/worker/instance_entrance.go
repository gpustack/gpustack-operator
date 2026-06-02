package worker

import (
	"context"
	"strconv"
	"strings"

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

// InstanceEntranceReconciler reconciles all Kubernetes Pod objects to finish the following tasks:
//   - When n v1.Instance-related Pod is running,
//     create a corresponding Service to expose the Pod and annotate the Pod with the Service node port info.
type InstanceEntranceReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*InstanceEntranceReconciler)(nil)

func (r *InstanceEntranceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		logger.V(3).Info("skip deleted pod")
		return ctrl.Result{}, nil
	}
	// Skip if not running.
	if pod.Status.Phase != core.PodRunning {
		logger.V(2).Info("skip non-running pod", "phase", pod.Status.Phase)
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
		var externalHostIPs []string
		for i := range eNode.Status.Addresses {
			if eNode.Status.Addresses[i].Type == core.NodeExternalIP {
				externalHostIPs = append(externalHostIPs, eNode.Status.Addresses[i].Address)
			}
		}
		if len(externalHostIPs) > 0 {
			notes["externalHostIPs"] = strings.Join(externalHostIPs, ",")
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
			// Update notes.
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

func (r *InstanceEntranceReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("instance.entrance").
		For(
			&core.Pod{},
			ctrlbuilder.WithPredicates(
				// Interested in relevant Pod objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instances")
				}),
				// Trigger reconciliation when a Pod is:
				// - created(for listed Pod at controller startup);
				// - updated with phase changes to Running.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldPod, newPod := e.ObjectOld.(*core.Pod), e.ObjectNew.(*core.Pod)
						if newPod.DeletionTimestamp != nil {
							return false
						}
						return oldPod.Status.Phase != newPod.Status.Phase && newPod.Status.Phase == core.PodRunning
					},
					GenericFunc: func(e ctrlevent.GenericEvent) bool {
						return false
					},
				},
			),
		).
		Watches(
			// Watch Kubernetes Services and enqueue the corresponding Pods.
			&core.Service{},
			ctrlhandler.EnqueueRequestsFromMapFunc(
				r.enqueuePodWhenServiceChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in relevant Service objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instances")
				}),
			),
		).
		Complete(r)
}

func (r *InstanceEntranceReconciler) enqueuePodWhenServiceChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("service", ctrlcli.ObjectKeyFromObject(obj))

	if !systemmeta.MatchResource(obj, "instances") {
		logger.Error(nil, "mismatched resource type")
		return nil
	}

	svc := obj.(*core.Service)

	// Ensure the Service has been assigned with ports.
	if len(svc.Spec.Ports) == 0 {
		logger.V(3).Info("no ports")
		return nil
	}
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].NodePort == 0 {
			logger.V(3).Info("no node port assigned", "port", svc.Spec.Ports[i].Name)
			return nil
		}
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{
				Name:      svc.Name,
				Namespace: svc.Namespace,
			},
		},
	}
	logger.V(2).Info("enqueue pod for service", "requests", reqs)
	return reqs
}
