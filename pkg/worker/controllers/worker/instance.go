package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/apistatus"
	"gpustack.ai/gpustack/pkg/worker/kuberequest"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// InstanceReconciler reconciles v1alpha1.Instance objects to finish the following tasks:
//   - Create/Update/Delete a Kubernetes Pod as the backing resource of the Instance based on the Instance spec.
//   - Create a Kubernetes Service to expose the ports of the backing Pod if specified in the Instance spec.
//   - Update the Instance status based on the backing Pod status for better visibility and stability.
type InstanceReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

var _ ctrlreconcile.Reconciler = (*InstanceReconciler)(nil)

const (
	// InstancePhaseStarting is the Instance status phase reported when the
	// backing Pod is being created or updated to match the Instance spec.
	InstancePhaseStarting = "Starting"
	// InstancePhaseStopping is the Instance status phase reported when the
	// backing Pod is being deleted to stop the Instance.
	InstancePhaseStopping = "Stopping"
	// InstancePhaseStopped is the Instance status phase reported when the
	// backing Pod is deleted and the Instance is stopped.
	InstancePhaseStopped = "Stopped"
	// InstancePhaseReady is the Instance status phase reported when the
	// backing Pod is running and ready.
	InstancePhaseReady = "Ready"
	// InstancePhaseDeleting is the Instance status phase reported when the
	// Instance is marked as deleted and the backing Pod is being deleted.
	InstancePhaseDeleting = "Deleting"
)

const (
	// InstanceTypePhaseInactive is the InstanceType status phase reported when its
	// backing Kueue ClusterQueue is not active (e.g., draining via HoldAndDrain),
	// mirroring the "Inactive" summary from apistatus.GetSummaryOfClusterQueue.
	InstanceTypePhaseInactive = "Inactive"
)

func (r *InstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	inst := new(workercore.Instance)
	err := r.Client.Get(ctx, req.NamespacedName, inst)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch instance")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Clean up if the Instance is marked as deleted.
	if inst.DeletionTimestamp != nil {
		if systemmeta.Unlock(inst) {
			logger.V(3).Info("skip deleted instance")
			return ctrl.Result{}, nil
		}

		// Ensure the backing Pod is deleted before unlocking and let other controllers or users to clean up the Instance.
		pod := new(core.Pod)
		err = r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(inst), pod,
			ctrlclix.WithoutQuorum)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "fetch pod")
				return ctrl.Result{}, err
			}
			pod = nil
		}

		if pod == nil {
			err = r.Client.Update(ctx, inst)
			if err != nil {
				logger.Error(err, "unlock instance")
			}
			return ctrl.Result{}, err
		}

		if inst.Status.Phase != InstancePhaseDeleting {
			logger.Info("instance deleting")

			inst.Status.Phase = InstancePhaseDeleting
			err = r.Client.Status().Update(ctx, inst)
			if err != nil {
				logger.Error(err, "update instance status to deleting")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}

		if pod.DeletionTimestamp == nil {
			err = r.Client.Delete(ctx, pod,
				ctrlclix.Terminated)
			if err != nil {
				if !kerrors.IsNotFound(err) {
					logger.Error(err, "delete pod")
					return ctrl.Result{}, err
				}
			}
		}

		logger.V(3).Info("pod deletion in progress; requeue in 2s")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Lock.
	if !systemmeta.Lock(inst) {
		err = r.Client.Update(ctx, inst)
		if err != nil {
			logger.Error(err, "lock instance")
			return ctrl.Result{}, err
		}
	}

	// Fetch the backing Pod.
	pod := new(core.Pod)
	err = r.Client.Get(ctx, req.NamespacedName, pod,
		ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch pod")
			return ctrl.Result{}, err
		}
		pod = nil
	}

	// If the Instance is marked as stopped,
	// ensure the backing Pod is deleted and update the Instance status to "Stopped".
	if ptr.Deref(inst.Spec.Stop, false) {
		// Pod already deleted, mark the Instance as stopped.
		if pod == nil {
			if inst.Status.Phase == InstancePhaseStopped {
				logger.V(3).Info("instance already stopped")
				return ctrl.Result{}, nil
			}

			inst.Status = workercore.InstanceStatus{
				Phase: InstancePhaseStopped,
			}
			err = r.Client.Status().Update(ctx, inst)
			if err != nil {
				logger.Error(err, "update instance status to stopped")
				return ctrl.Result{}, err
			}
			logger.Info("instance stopped")
			return ctrl.Result{}, nil
		}

		// If instance is not marked as stopping,
		// mark it as stopping first to avoid racing with other controllers or users to update the instance status.
		if inst.Status.Phase != InstancePhaseStopping {
			inst.Status.Phase = InstancePhaseStopping
			err = r.Client.Status().Update(ctx, inst)
			if err != nil {
				logger.Error(err, "update instance status to stopping")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}

		// Pod still exists, delete it.
		if pod.DeletionTimestamp == nil {
			err = r.Client.Delete(ctx, pod,
				ctrlclix.Terminated)
			if err != nil {
				if !kerrors.IsNotFound(err) {
					logger.Error(err, "delete pod")
					return ctrl.Result{}, err
				}
			}
		}

		// Pod deletion in progress, requeue and wait for it to be deleted.
		logger.V(2).Info("pod deletion in progress; requeue in 2s")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Create the Pod if not exists.
	if pod == nil {
		// Fetch the InstanceType that backs the Pod.
		instType := new(worker.InstanceType)
		err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: inst.Spec.Type}, instType,
			ctrlclix.WithoutQuorum)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "fetch instance type")
				return ctrl.Result{}, err
			}
			// Maybe the InstanceType is not cached yet,
			// try to fetch directly from API server to avoid the cache staleness.
			err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: inst.Spec.Type}, instType,
				ctrlclix.WithoutQuorum)
			if err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "fetch instance type")
				return ctrl.Result{}, err
			}
		}
		instTypeGone := kerrors.IsNotFound(err)

		if instTypeGone || instType.Status.Phase == InstanceTypePhaseInactive {
			// Persist the stop intent first so the instance reliably stays stopped
			// even if the status update below fails.
			inst.Spec.Stop = ptr.To(true)
			err = r.Client.Update(ctx, inst)
			if err != nil {
				logger.Error(err, "stop instance with inactive instance type")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}

			logger.Info("stop instance as inactive instance type", "type", inst.Spec.Type)
			return ctrl.Result{}, nil
		}

		// If the instance is marked as stopping/stopped/ready, mark it as starting first
		// to avoid racing with other controllers or users to update the instance status.
		if inst.Status.Phase == InstancePhaseStopping ||
			inst.Status.Phase == InstancePhaseStopped ||
			inst.Status.Phase == InstancePhaseReady {
			inst.Status = workercore.InstanceStatus{
				Phase: InstancePhaseStarting,
			}
			err = r.Client.Status().Update(ctx, inst)
			if err != nil {
				logger.Error(err, "update instance status to starting")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}

			logger.Info("instance starting")
		}

		pod = r.convertPodFromInstance(ctx, inst, instType)
		err = r.Client.Create(ctx, pod)
		if err != nil {
			logger.Error(err, "create pod")
			return ctrl.Result{}, err
		}
	} else if pod.DeletionTimestamp != nil {
		logger.V(3).Info("previous pod deletion in progress; requeue in 2s")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Fetch the backing Service.
	svc := new(core.Service)
	err = r.Client.Get(ctx, req.NamespacedName, svc,
		ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch service")
			return ctrl.Result{}, err
		}
		svc = nil
	}

	// Create the Service if not exists.
	if svc == nil {
		svc = r.convertServiceFromPod(ctx, pod)
		err = r.Client.Create(ctx, svc)
		if err != nil {
			logger.Error(err, "create service")
			return ctrl.Result{}, err
		}
	} else if svc.DeletionTimestamp != nil {
		logger.V(3).Info("previous service deletion in progress; requeue in 2s")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	instStatus := inst.Status.DeepCopy()

	// Update the Phase in the Instance status.
	instStatus.Phase, instStatus.PhaseMessage = apistatus.GetSummaryOfPod(&pod.Status)

	if pod.Status.Phase == core.PodRunning {
		// Update the Ports in the Instance status if all ready.
		if len(instStatus.Ports) == 0 {
			allReady := true
			portsMap := make(map[string]int32)
			for i := range svc.Spec.Ports {
				if svc.Spec.Ports[i].NodePort == 0 {
					allReady = false
					break
				}
				portsMap[svc.Spec.Ports[i].Name] = svc.Spec.Ports[i].NodePort
			}
			if allReady {
				instStatus.Ports = make([]workercore.InstanceServicePort, 0, len(inst.Spec.Ports))
				for i := range inst.Spec.Ports {
					portKey := getPortName(inst.Spec.Ports[i])
					instStatus.Ports = append(instStatus.Ports, workercore.InstanceServicePort{
						InstancePort: inst.Spec.Ports[i],
						NodePort:     portsMap[portKey],
					})
				}
			} else {
				logger.V(2).Info("instance ports not ready; requeue in 2s")
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
		}

		// Update the NodeName and HostIPs in the Instance status.
		if instStatus.NodeName != pod.Spec.NodeName {
			// Fetch the backing Node.
			nd := new(core.Node)
			err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: pod.Spec.NodeName}, nd,
				ctrlclix.WithoutQuorum)
			if err != nil {
				logger.Error(err, "fetch node")
				return ctrl.Result{}, err
			}
			instStatus.NodeName = nd.Name
			for i := range nd.Status.Addresses {
				if nd.Status.Addresses[i].Type == core.NodeExternalIP {
					instStatus.HostIPs = append(instStatus.HostIPs, core.HostIP{
						IP: nd.Status.Addresses[i].Address,
					})
				}
			}
			for i := range nd.Status.Addresses {
				if nd.Status.Addresses[i].Type == core.NodeInternalIP {
					instStatus.HostIPs = append(instStatus.HostIPs, core.HostIP{
						IP: nd.Status.Addresses[i].Address,
					})
				}
			}
		}

		// Update the PodIPs in the Instance status if not exists.
		if len(instStatus.PodIPs) == 0 {
			instStatus.PodIPs = pod.Status.PodIPs
			if len(instStatus.PodIPs) == 0 && pod.Status.PodIP != "" {
				instStatus.PodIPs = append(instStatus.PodIPs, core.PodIP{
					IP: pod.Status.PodIP,
				})
			}
		}

		// Update the Allocations in the Instance status if not exists.
		if len(instStatus.Allocations) == 0 {
			if v := pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey]; v != "" {
				var ds workercore.DevicesStatus
				json.ShouldUnmarshal(stringx.ToBytes(&v), &ds)
				instStatus.Allocations = ds.Groups
			}
		}
	}

	if kubemeta.DeepEqual(&inst.Status, instStatus) {
		logger.V(3).Info("instance status up to date")
		return ctrl.Result{}, nil
	}

	currentPhase, lastPhase := inst.Status.Phase, instStatus.Phase

	inst.Status = *instStatus
	err = r.Client.Status().Update(ctx, inst)
	if err != nil {
		logger.Error(err, "update instance status to ready")
		return ctrl.Result{}, err
	}

	if currentPhase != lastPhase && lastPhase == InstancePhaseReady {
		logger.Info("instance started")
	} else {
		logger.V(3).Info("updated instance status")
	}
	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) convertPodFromInstance(
	ctx context.Context,
	inst *workercore.Instance,
	instType *worker.InstanceType,
) *core.Pod {
	needSSHD := inst.Spec.SSHPublicKey != nil && inst.Spec.SSHPublicKey.Name != ""

	overcommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)

	// Construct containers.
	var containers []core.Container
	if needSSHD {
		// Main container.
		mainC := core.Container{
			Name:            "main",
			Image:           inst.Spec.Image,
			ImagePullPolicy: inst.Spec.ImagePullPolicy,
			Command:         inst.Spec.Command,
			SecurityContext: func() *core.SecurityContext {
				sc := &core.SecurityContext{
					RunAsUser: ptr.To[int64](0),
				}
				if inst.Spec.Privileged {
					sc.Privileged = ptr.To(true)
				}
				return sc
			}(),
			Resources: getResourceRequirements(inst, instType, true, overcommit, false),
			Ports: slicex.Transform(inst.Spec.Ports, func(p workercore.InstancePort) core.ContainerPort {
				return core.ContainerPort{
					Name:          getPortName(p),
					Protocol:      p.Protocol,
					ContainerPort: p.Port,
				}
			}),
			Env: slicex.Transform(inst.Spec.Env, func(e workercore.InstanceEnvVar) core.EnvVar {
				return core.EnvVar{
					Name:  e.Name,
					Value: e.Value,
				}
			}),
			VolumeMounts: []core.VolumeMount{{
				Name:      "workspace",
				MountPath: inst.Spec.VolumeMount,
			}},
		}

		// SSHD container.
		sshdC := core.Container{
			Name: "sshd",
			Image: func() string {
				img := settings.InstanceSSHServerImage.ShouldValue(ctx)
				if cn := settings.ContainerNamespace.ShouldValue(ctx); cn != "" {
					_, suffix, found := strings.Cut(img, "/")
					if found {
						img = cn + "/" + suffix
					}
				}
				if rn := settings.ContainerRegistry.ShouldValue(ctx); rn != "" {
					img = rn + "/" + img
				}
				return img
			}(),
			ImagePullPolicy: inst.Spec.ImagePullPolicy,
			Stdin:           true,
			TTY:             true,
			SecurityContext: &core.SecurityContext{
				Capabilities: &core.Capabilities{
					Add: []core.Capability{
						"SYS_ADMIN",
						"SYS_PTRACE",
					},
				},
			},
			Resources: getResourceRequirements(inst, instType, false, false, true),
			Env: []core.EnvVar{
				{
					Name:  "VOLUME_MOUNT_PATH",
					Value: inst.Spec.VolumeMount,
				},
			},
			VolumeMounts: func() []core.VolumeMount {
				if inst.Spec.SSHPublicKey == nil {
					return nil
				}
				return []core.VolumeMount{{
					Name:      "sshd-authorized-keys",
					MountPath: "/var/run/sshd-authorized-keys",
					ReadOnly:  true,
				}}
			}(),
		}

		containers = []core.Container{mainC, sshdC}
	} else {
		// Main container.
		mainC := core.Container{
			Name:            "main",
			Image:           inst.Spec.Image,
			ImagePullPolicy: inst.Spec.ImagePullPolicy,
			Command:         inst.Spec.Command,
			SecurityContext: func() *core.SecurityContext {
				sc := &core.SecurityContext{
					RunAsUser: ptr.To[int64](0),
				}
				if inst.Spec.Privileged {
					sc.Privileged = ptr.To(true)
				}
				return sc
			}(),
			Resources: getResourceRequirements(inst, instType, true, overcommit, true),
			Ports: slicex.Transform(inst.Spec.Ports, func(p workercore.InstancePort) core.ContainerPort {
				return core.ContainerPort{
					Name:          getPortName(p),
					Protocol:      p.Protocol,
					ContainerPort: p.Port,
				}
			}),
			Env: slicex.Transform(inst.Spec.Env, func(e workercore.InstanceEnvVar) core.EnvVar {
				return core.EnvVar{
					Name:  e.Name,
					Value: e.Value,
				}
			}),
			VolumeMounts: []core.VolumeMount{{
				Name:      "workspace",
				MountPath: inst.Spec.VolumeMount,
			}},
		}

		containers = []core.Container{mainC}
	}

	// Construct pod.
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      inst.Name,
			Namespace: inst.Namespace,
			Labels: map[string]string{
				// The queue-name label references the LocalQueue, which is
				// named by the hash of the ClusterQueue(InstanceType) name.
				kueuectrlconst.QueueLabel:   nodefeature.FormatLocalQueueName(inst.Spec.Type), // Scheduling.
				"app.kubernetes.io/part-of": string(inst.UID),                                 // Accessing.
			},
		},
		Spec: core.PodSpec{
			HostIPC:                      true,
			ShareProcessNamespace:        ptr.To(true),
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			ImagePullSecrets: func() []core.LocalObjectReference {
				if inst.Spec.ImagePullSecret == nil {
					return nil
				}
				return []core.LocalObjectReference{
					*inst.Spec.ImagePullSecret,
				}
			}(),
			Volumes: func() (vols []core.Volume) {
				if inst.Spec.SSHPublicKey != nil {
					vols = append(vols, core.Volume{
						Name: "sshd-authorized-keys",
						VolumeSource: core.VolumeSource{
							Secret: &core.SecretVolumeSource{
								SecretName:  inst.Spec.SSHPublicKey.Name,
								DefaultMode: ptr.To[int32](0o600),
							},
						},
					})
				}
				if inst.Spec.Volume.Ephemeral != nil {
					vols = append(vols, core.Volume{
						Name: "workspace",
						VolumeSource: core.VolumeSource{
							EmptyDir: &core.EmptyDirVolumeSource{
								SizeLimit: &inst.Spec.Volume.Ephemeral.Capacity,
							},
						},
					})
					return vols
				}
				vols = append(vols, core.Volume{
					Name: "workspace",
					VolumeSource: core.VolumeSource{
						PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
							ClaimName: inst.Spec.Volume.Persistent.Name,
						},
					},
				})
				return vols
			}(),
			Containers: containers,
		},
	}

	// Ensure runtime class.
	if instType.Spec.Acceleratable {
		rn := nodefeature.GetAcceleratableRuntimeName(instType.Spec.Manufacturer)
		if rn != "" {
			rc := new(node.RuntimeClass)
			err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: rn}, rc,
				ctrlclix.WithoutQuorum)
			if err == nil {
				pod.Spec.RuntimeClassName = ptr.To(rn)
			}
		}
	}

	systemmeta.NoteResource(pod, "instances", nil)
	kubemeta.ControlOnWithoutBlock(pod, inst, workercore.SchemeGroupVersionKind("Instance"))

	return pod
}

func (r *InstanceReconciler) convertServiceFromPod(
	_ context.Context,
	pod *core.Pod,
) *core.Service {
	svc := &core.Service{
		ObjectMeta: kubemeta.SanitizeObjectMeta(pod.ObjectMeta),
		Spec: core.ServiceSpec{
			Selector: pod.Labels,
			Type:     core.ServiceTypeNodePort,
		},
	}

	// Fill the service ports based on the pod container ports.
	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].Ports {
			svc.Spec.Ports = append(svc.Spec.Ports, core.ServicePort{
				Name:       pod.Spec.Containers[i].Ports[j].Name,
				Port:       pod.Spec.Containers[i].Ports[j].ContainerPort,
				Protocol:   pod.Spec.Containers[i].Ports[j].Protocol,
				TargetPort: intstr.FromInt32(pod.Spec.Containers[i].Ports[j].ContainerPort),
			})
		}
	}

	systemmeta.NoteResource(svc, "instances", nil)
	kubemeta.ControlOnWithoutBlock(svc, pod, core.SchemeGroupVersion.WithKind("Pod"))

	return svc
}

func (r *InstanceReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("instance").
		For(
			&workercore.Instance{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.GenerationChangedPredicate{},
			),
		).
		Watches(
			// Watch relevant Kubernetes Pods and enqueue the corresponding Instances.
			&core.Pod{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceWhenPodChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in relevant Pod objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instances")
				}),
			),
		).
		Watches(
			// Watch Kubernetes Services and enqueue the corresponding Pods.
			&core.Service{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
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

func (r *InstanceReconciler) enqueueInstanceWhenPodChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("pod", ctrlcli.ObjectKeyFromObject(obj))

	if !systemmeta.MatchResource(obj, "instances") {
		logger.Error(nil, "mismatched resource type")
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKeyFromObject(obj),
		},
	}
	logger.V(2).Info("enqueue instance from pod", "requests", reqs)
	return reqs
}

func (r *InstanceReconciler) enqueuePodWhenServiceChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("service", ctrlcli.ObjectKeyFromObject(obj))

	if !systemmeta.MatchResource(obj, "instances") {
		logger.Error(nil, "mismatched resource type")
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKeyFromObject(obj),
		},
	}
	logger.V(2).Info("enqueue instance from service", "requests", reqs)
	return reqs
}

// getPortName generates a unique name for a given InstancePort,
// which is required for naming the container ports and service ports.
// The name is generated by combining the protocol and port number,
// and converting it to lower case to ensure it meets Kubernetes naming requirements.
func getPortName(port workercore.InstancePort) string {
	return strings.ToLower(fmt.Sprintf("%s-%d", port.Protocol, port.Port))
}

// getResourceRequirements builds the Kubernetes ResourceRequirements for a
// container derived from an Instance + its InstanceType. The output is split
// across two axes:
//
//	withGeneral          — write CPU / RAM / EphemeralStorage entries
//	withGeneralOvercommit — if withGeneral, scale the request down via
//	                        kuberequest.ScaleToOvercommit so multiple pods
//	                        can share a node; limits stay at the user ask
//	withAccelerator      — write the accelerator entry (Limits == Requests),
//	                        gated by instType.Spec.Acceleratable AND the
//	                        pod actually asking for Accelerator > 0
//
// The three flags map to the three container shapes the controller emits in
// convertPodFromInstance:
//
//	main + sshd: main has general only, sshd has accelerator only
//	main alone : main has general + accelerator
//
// For general resources the limit is always the user-facing quantity, and the
// request is either the same quantity (overcommit off) or its overcommit-scaled
// form (overcommit on). The acceleratable flag passed to ScaleToOvercommit
// comes from the InstanceType, so on accelerator nodes CPU uses the smaller
// 100m base to keep CPU from gating placement.
//
// For the accelerator entry the resource name and value depend on the
// allocation mode: sliced types are quantized via QuantityToAlignedValue and
// use the sliced resource name; exclusive types use the raw quantity and the
// exclusive resource name.
func getResourceRequirements(
	inst *workercore.Instance,
	instType *worker.InstanceType,
	withGeneral, withGeneralOvercommit bool,
	withAccelerator bool,
) core.ResourceRequirements {
	rr := core.ResourceRequirements{
		Limits:   core.ResourceList{},
		Requests: core.ResourceList{},
	}

	if withGeneral {
		for n, q := range map[core.ResourceName]resource.Quantity{
			core.ResourceCPU:              inst.Spec.Resources.CPU,
			core.ResourceMemory:           inst.Spec.Resources.RAM,
			core.ResourceEphemeralStorage: inst.Spec.Resources.LocalStorage,
		} {
			rr.Limits[n] = q
			if withGeneralOvercommit {
				rr.Requests[n] = kuberequest.ScaleToOvercommit(n, q, instType.Spec.Acceleratable)
				continue
			}
			rr.Requests[n] = q
		}
	}

	requestAccelerator := instType.Spec.Acceleratable &&
		inst.Spec.Resources.Accelerator != nil &&
		inst.Spec.Resources.Accelerator.Sign() > 0
	if requestAccelerator {
		if withAccelerator {
			if instType.Spec.Sliced > 0 {
				// Sliced types request a pair of keys: the per-card unit count
				// (.sliced.units = U×D/partitions, independent of the card count C)
				// and the bare card count C (.sliced), which Kueue folds into
				// credits via multiplyBy. The per-card units never multiply by C.
				u := inst.Spec.Resources.AcceleratorUnits
				if u <= 0 {
					u = 1
				}
				unitsQ := nodefeature.QuantityToAlignedValue(*resource.NewQuantity(int64(u), resource.DecimalSI), instType.Spec.Sliced)
				unitsResName := nodefeature.GetAcceleratableSlicedUnitsResourceName(instType.Spec.Manufacturer)
				cardQ := *inst.Spec.Resources.Accelerator
				cardResName := nodefeature.GetAcceleratableResourceName(instType.Spec.Manufacturer, workercore.DeviceAllocationModeSliced)
				rr.Limits[unitsResName] = unitsQ
				rr.Requests[unitsResName] = unitsQ
				rr.Limits[cardResName] = cardQ
				rr.Requests[cardResName] = cardQ
			} else {
				resQuantity := *inst.Spec.Resources.Accelerator
				resName := nodefeature.GetAcceleratableResourceName(instType.Spec.Manufacturer, workercore.DeviceAllocationModeExclusive)
				rr.Limits[resName] = resQuantity
				rr.Requests[resName] = resQuantity
			}
		}
	}

	return rr
}
