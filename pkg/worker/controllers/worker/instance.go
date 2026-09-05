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
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
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
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
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
	// InstanceTypePhaseInactive is the InstanceType status phase reported when its backing Kueue
	// ClusterQueue is held (Hold, an admin Inactive) or already fully drained (HoldAndDrain with no
	// reservations left), mirroring the "Inactive" summary from apistatus.GetSummaryOfClusterQueue.
	InstanceTypePhaseInactive = "Inactive"
	// InstanceTypePhaseDraining is the InstanceType status phase reported while its backing Kueue
	// ClusterQueue is actively evicting admitted workloads (HoldAndDrain with reservations still
	// outstanding), mirroring the "Draining" summary from apistatus.GetSummaryOfClusterQueue.
	InstanceTypePhaseDraining = "Draining"
)

const (
	// _PodReasonUnexpectedAdmissionError is the Pod status reason kubelet sets when it rejects
	// a Pod at admission — for an accelerator Pod, the device-plugin's Allocate returning an
	// error (e.g. FailedPrecondition on a cross-mode card conflict).
	_PodReasonUnexpectedAdmissionError = "UnexpectedAdmissionError"
	// _InstanceAdmissionFailureRetryWindow bounds how long after Instance creation a backing
	// Pod rejected at admission is still rebuilt. The gap between the Instance's and the
	// backing Pod's creation timestamps starts near zero and grows monotonically with every
	// rebuild, so it doubles as a stateless backoff and a hard retry deadline — no rebuild
	// bookkeeping has to be persisted on the Instance.
	_InstanceAdmissionFailureRetryWindow = 5 * time.Minute
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
	if inst.Spec.Stop {
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

	// Fetch the InstanceType that backs the Instance. It both sizes a new Pod and reports,
	// via its phase, whether the type is still usable.
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

	// Read the backing ClusterQueue's StopPolicy — the only signal that distinguishes an admin Hold
	// (block new admission, keep running Pods) from a HoldAndDrain drain or teardown (evict admitted
	// workloads). Both collapse to InstanceType phase Inactive once no reservation remains, and a fast
	// drain skips a durable Draining phase entirely, so the phase cannot be relied on to stop a drained
	// Instance. The ClusterQueue is named after the InstanceType.
	var draining bool
	if !instTypeGone {
		cq := new(kueue.ClusterQueue)
		cqErr := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: inst.Spec.Type}, cq)
		if cqErr != nil && !kerrors.IsNotFound(cqErr) {
			logger.Error(cqErr, "fetch backing cluster queue")
			return ctrl.Result{}, cqErr
		}
		draining = cqErr == nil && ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain
	}

	// Stop the Instance once its InstanceType is gone, being deleted, or its backing queue is
	// HoldAndDrain (a pool drain or a teardown that evicts admitted workloads), regardless of whether
	// a Pod exists. An admin Hold keeps running Pods, so a Hold-Inactive type does not stop the
	// Instance; a new Instance under it simply stays pending. Keying off the InstanceType's own
	// deletion timestamp (not only the queue) stops the Instance the moment teardown begins, without
	// waiting for the queue to drain the Pod. The InstanceType and ClusterQueue watches re-enqueue here
	// on a phase transition, StopPolicy change, or deletion, so the stop stays level-based even when no
	// Pod event fires.
	if instTypeGone || instType.DeletionTimestamp != nil || draining {
		// Persist the stop intent first so the instance reliably stays stopped
		// even if the status update below fails.
		inst.Spec.Stop = true
		err = r.Client.Update(ctx, inst)
		if err != nil {
			logger.Error(err, "stop instance with unschedulable instance type")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}

		logger.Info("stop instance as its instance type is gone, deleting, or draining", "type", inst.Spec.Type)
		return ctrl.Result{}, nil
	}

	// Rebuild a Pod kubelet rejected at admission — reason UnexpectedAdmissionError, which for
	// an accelerator Pod is the device-plugin's Allocate failing (e.g. the cross-mode
	// FailedPrecondition when kubelet's ListAndWatch snapshot raced a fresh reservation).
	// Admission rejection happens before any container starts and leaves no allocation
	// behind, so deleting and recreating the Pod is safe. The retry budget is stateless: the
	// gap between the Instance's and the Pod's creation timestamps starts near zero and
	// grows with every rebuild, so it doubles as the backoff (each attempt waits roughly the
	// running gap) and the deadline — a gap beyond the window means the rejection is
	// persistent, and the failed Pod stays as the visible error instead of hot-looping.
	if pod != nil && pod.DeletionTimestamp == nil &&
		pod.Status.Phase == core.PodFailed && pod.Status.Reason == _PodReasonUnexpectedAdmissionError {
		gap := max(pod.CreationTimestamp.Sub(inst.CreationTimestamp.Time), 0)
		if gap < _InstanceAdmissionFailureRetryWindow {
			err = r.Client.Delete(ctx, pod,
				ctrlclix.Terminated)
			if err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete admission-rejected pod")
				return ctrl.Result{}, err
			}
			backoff := max(gap, 2*time.Second)
			logger.Info("rebuild admission-rejected pod",
				"gap", gap, "backoff", backoff, "message", pod.Status.Message)
			return ctrl.Result{RequeueAfter: backoff}, nil
		}
		logger.Info("admission-rejected pod past the retry window; leaving it as the visible error",
			"gap", gap, "message", pod.Status.Message)
	}

	// Create the Pod if not exists.
	if pod == nil {
		// An accelerated type sizes its Pod (accelerator resource names, RuntimeClass, and the
		// sliced-vs-whole-card shape) from Status.Detail. Creating a Pod before the reconciler has
		// computed Detail would stamp a missing RuntimeClass and bogus resource names, so requeue
		// until it is ready — the periodic requeue re-checks as the InstanceType status populates.
		if instType.Spec.Acceleratable && !instType.Status.Detail.AcceleratorReady() {
			logger.V(2).Info("instance type accelerator detail not ready; requeue in 2s")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}

		// The queue-name label is READ from the type's published entrance rather than recomputed from
		// its name, so a type that has not published one yet has no Pod built against it. A Pod
		// carrying no queue-name label is not queued by Kueue at all -- the kubelet runs it directly,
		// charging no quota and passing none of the gates the chain applies -- and one carrying a
		// guessed name is routed at a LocalQueue that may not exist.
		//
		// WAITED FOR RATHER THAN FAILED, which is this path's own idiom and not the ModelDeployment
		// one. That render returns an error because it has several ways to be unable to build a Pod;
		// this one returns no error at all, and expresses "the type's status has not caught up" as the
		// guard directly above does. The periodic requeue is also what stands in for a watch here:
		// this controller watches ClusterQueue StopPolicy transitions, deliberately ignoring status
		// churn, so nothing would wake it when the entrance is published.
		if instType.Status.Entrance == "" {
			logger.V(2).Info("instance type publishes no queue entrance yet; requeue in 2s")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
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

	// Create the Service only when the Pod exposes ports: a portless NodePort Service is
	// rejected by the API, which would fail every reconcile before the status is written.
	if svc == nil {
		desired := r.convertServiceFromPod(ctx, pod)
		if len(desired.Spec.Ports) > 0 {
			err = r.Client.Create(ctx, desired)
			if err != nil {
				logger.Error(err, "create service")
				return ctrl.Result{}, err
			}
			svc = desired
		}
	} else if svc.DeletionTimestamp != nil {
		logger.V(3).Info("previous service deletion in progress; requeue in 2s")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	instStatus := inst.Status.DeepCopy()

	// Update the Phase in the Instance status.
	instStatus.Phase, instStatus.PhaseMessage = apistatus.GetSummaryOfPod(&pod.Status)

	if pod.Status.Phase == core.PodRunning {
		// Surface the node ports once ready; skipped for a portless Instance (svc nil).
		if svc != nil && len(instStatus.Ports) == 0 {
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
			instStatus.Allocations = deviceplugin.AllocatedAcceleratorGroupsOf(pod)
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

	additionalVols, additionalMounts := convertAdditionalVolumes(inst.Spec.AdditionalVolumes)

	// Construct containers.
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
		Resources: getResourceRequirements(inst.Spec.Resources, instType, true, overcommit, true, false),
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
		// Additional volumes are mounted into the workload container only; the sshd sidecar
		// nsenters into this container's mount namespace, so they are reachable over SSH anyway.
		VolumeMounts: append([]core.VolumeMount{{
			Name:      "workspace",
			MountPath: inst.Spec.VolumeMount,
		}}, additionalMounts...),
	}
	containers := []core.Container{mainC}

	if needSSHD {
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
			Resources: getResourceRequirements(inst.Spec.Resources, instType, false, false, false, true),
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

		containers = append(containers, sshdC)
	}

	// Construct pod.
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      inst.Name,
			Namespace: inst.Namespace,
			Labels: map[string]string{
				// The LocalQueue that fronts this type's pool, taken from what the type PUBLISHES
				// rather than derived from its name. The two agree only while the ClusterQueue is
				// created under the InstanceType's own name and both sides spell the result with
				// FormatLocalQueueName -- neither of which this package states or owns. The caller
				// guarantees it is non-empty; see the entrance guard in Reconcile.
				kueuectrlconst.QueueLabel:           instType.Status.Entrance, // Scheduling.
				deviceplugin.InstancePartOfLabelKey: string(inst.UID),         // Accessing.
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
			// Pin a node-pinned Instance through a nodeSelector, never through the Pod's own
			// nodeName: nodeName skips the scheduler entirely, so Kueue's admission gating and
			// the node's predicate checks would never run.
			NodeSelector: func() map[string]string {
				if inst.Spec.NodeName == "" {
					return nil
				}
				return map[string]string{
					core.LabelHostname: r.getNodeHostname(ctx, inst.Spec.NodeName),
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
					return append(vols, additionalVols...)
				}
				vols = append(vols, core.Volume{
					Name: "workspace",
					VolumeSource: core.VolumeSource{
						PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
							ClaimName: inst.Spec.Volume.Persistent.Name,
						},
					},
				})
				return append(vols, additionalVols...)
			}(),
			Containers: containers,
		},
	}

	// Ensure runtime class.
	if instType.Spec.Acceleratable {
		rn := nodefeature.GetAcceleratableRuntimeName(instType.Status.Detail.Manufacturer)
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

// convertAdditionalVolumes renders the Instance's additional volumes as Pod volumes paired with the
// mounts that place them in the workload container. Both are returned together so the volume name —
// derived from the entry's index, never from user input, so it can collide with neither "workspace"
// nor "sshd-authorized-keys" — is decided in one place.
//
// An entry with no source is skipped rather than rendered: admission rejects one, and a volume with
// an empty source would make the API server refuse the whole Pod on every reconcile.
func convertAdditionalVolumes(avs []workercore.InstanceAdditionalVolume) (vols []core.Volume, mounts []core.VolumeMount) {
	if len(avs) == 0 {
		return nil, nil
	}

	vols = make([]core.Volume, 0, len(avs))
	mounts = make([]core.VolumeMount, 0, len(avs))
	for i := range avs {
		av := &avs[i]

		var vs core.VolumeSource
		switch {
		case av.Persistent != nil:
			vs.PersistentVolumeClaim = &core.PersistentVolumeClaimVolumeSource{
				ClaimName: av.Persistent.Name,
			}
		case av.ConfigMap != nil:
			vs.ConfigMap = &core.ConfigMapVolumeSource{
				LocalObjectReference: *av.ConfigMap,
			}
		case av.Secret != nil:
			vs.Secret = &core.SecretVolumeSource{
				SecretName: av.Secret.Name,
			}
		case av.HostPath != nil:
			vs.HostPath = av.HostPath.DeepCopy()
		default:
			continue
		}

		name := additionalVolumeName(i)
		vols = append(vols, core.Volume{
			Name:         name,
			VolumeSource: vs,
		})
		mounts = append(mounts, core.VolumeMount{
			Name:      name,
			MountPath: av.MountPath,
			ReadOnly:  av.ReadOnly,
			SubPath:   av.SubPath,
		})
	}

	return vols, mounts
}

// additionalVolumeName is the Pod volume name of the additional volume at the given index.
func additionalVolumeName(i int) string {
	return "additional-" + strconvx.Itoa(i)
}

// getNodeHostname returns the node's own kubernetes.io/hostname label value, which some providers
// set to something other than the Node object's name, so the rendered selector must be read from the
// node rather than assumed. The Pod is rendered once, at creation, and never re-diffed, so a stale
// cache read here would pin the Pod to a wrong hostname for its whole life: fall back to a live read
// before giving up.
//
// It returns the given name when the node cannot be read at all or carries no hostname label. Both
// are visible states rather than silent ones — the Pod then selects a hostname nothing matches and
// stays Pending with the scheduler's own reason, which the Instance phase message surfaces.
func (r *InstanceReconciler) getNodeHostname(ctx context.Context, nodeName string) string {
	nd := new(core.Node)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, nd,
		ctrlclix.WithoutQuorum)
	if err != nil {
		err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, nd,
			ctrlclix.WithoutQuorum)
		if err != nil {
			// The Pod is rendered once, so this fallback is what the Instance is pinned to for
			// good. On a node whose hostname label differs from its name that selector matches
			// nothing, and the Pod's own Pending reason names the selector rather than the read
			// that produced it — so say here what the scheduler cannot.
			ctrllog.FromContext(ctx).Error(err, "fetch node for pin", "node", nodeName)
			return nodeName
		}
	}
	// Neither read waits on etcd quorum, so a label written moments ago may not be visible yet.
	// That is the accepted trade: kubelet sets the hostname label once, at registration, and does
	// not change it, so the value being read is one that has been settled since the node joined.
	if hostname := nd.Labels[core.LabelHostname]; hostname != "" {
		return hostname
	}
	return nodeName
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
		Watches(
			// Watch InstanceTypes and enqueue every Instance of that type the moment the type starts
			// being deleted, so a teardown stops its running Instances promptly — before the backing
			// queue finishes draining — instead of only being caught at Pod-creation time. The
			// draining (StopPolicy) signal is owned by the ClusterQueue watch below.
			&workercore.InstanceType{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstancesWhenInstanceTypeChanged,
			),
			ctrlbuilder.WithPredicates(
				// The stop now reads the backing ClusterQueue's StopPolicy, not the InstanceType
				// phase, so only the type entering deletion (teardown) still needs to re-enqueue
				// here; a plain phase transition no longer changes the stop decision. Ignore
				// creation and the frequent three-view status churn driven by Devices changes.
				ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return false
					},
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldInstType, newInstType := e.ObjectOld.(*workercore.InstanceType), e.ObjectNew.(*workercore.InstanceType)
						if newInstType.DeletionTimestamp == nil {
							return false
						}
						return oldInstType.DeletionTimestamp == nil ||
							!oldInstType.DeletionTimestamp.Equal(newInstType.DeletionTimestamp)
					},
				},
			),
		).
		Watches(
			// Watch the backing ClusterQueues (named after the InstanceType) and enqueue every
			// Instance of that type on a StopPolicy change, so a HoldAndDrain drain/teardown stops
			// its Instances promptly. Only a StopPolicy transition can change the stop decision;
			// ignore the frequent status churn.
			&kueue.ClusterQueue{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstancesWhenInstanceTypeChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						cq, ok := e.Object.(*kueue.ClusterQueue)
						return ok && ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldCQ, ok1 := e.ObjectOld.(*kueue.ClusterQueue)
						newCQ, ok2 := e.ObjectNew.(*kueue.ClusterQueue)
						if !ok1 || !ok2 {
							return false
						}
						return ptr.Deref(oldCQ.Spec.StopPolicy, kueue.None) != ptr.Deref(newCQ.Spec.StopPolicy, kueue.None)
					},
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
				},
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

func (r *InstanceReconciler) enqueueInstancesWhenInstanceTypeChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("instancetype", obj.GetName())

	instances := new(workercore.InstanceList)
	if err := r.Client.List(ctx, instances); err != nil {
		logger.Error(err, "list instances of instance type")
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range instances.Items {
		if instances.Items[i].Spec.Type != obj.GetName() {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKeyFromObject(&instances.Items[i]),
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue instances from instance type", "requests", reqs)
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
//	withVisibility       — write the sidecar visibility entry
//	                        (device.gpustack.ai/<manufacturer>.visibility) with
//	                        main's card count, so the device-plugin co-allocates
//	                        the SSH sidecar the same physical device(s) as main;
//	                        same accelerator gate. Mutually exclusive with
//	                        withAccelerator.
//
// The flags map to the container shapes the controller emits in
// convertPodFromInstance:
//
//	main + sshd: main has general + accelerator; sshd has the device-only
//	             visibility resource (main's card count), resolved on the node
//	main alone : main has general + accelerator
//
// For general resources the limit is always the user-facing quantity, and the
// request is either the same quantity (overcommit off) or its overcommit-scaled
// form (overcommit on). The acceleratable flag passed to ScaleToOvercommit
// comes from the InstanceType, so on accelerator nodes CPU uses the smaller
// 100m base to keep CPU from gating placement.
//
// For the accelerator entry the resource name and value depend on the
// allocation mode: a partition request (a non-empty AcceleratorPartitionedProfile
// on a manufacturer that has hardware partitioning) emits .partitioned and
// .partitioned.<kind>-<profile>, both exactly 1, whose .partitioned.units the Pod
// webhook folds from the profile's VRAM; a logical slice request (a logically
// sliceable type with a non-zero memory percentage) emits the bare .sliced card
// count plus the per-card memory/compute percentages, which the Pod webhook folds
// into .sliced.units; everything else (a non-sliced type, or a 0% request) uses the
// raw quantity and the exclusive resource name.
// getResourceRequirements renders the resource keys one container asks for.
//
// It takes the RESOURCES rather than the object holding them, because two CRDs now render Pods
// against one InstanceType and the accelerator-key algebra below — which mode's key to emit, which
// manufacturer's spelling, which credits the Pod webhook will fold — must exist exactly once. A
// second copy would be a manifest of the same facts, free to drift from this one.
func getResourceRequirements(
	instRess *workercore.InstanceResources,
	instType *worker.InstanceType,
	withGeneral, withGeneralOvercommit bool,
	withAccelerator bool,
	withVisibility bool,
) core.ResourceRequirements {
	rr := core.ResourceRequirements{
		Limits:   core.ResourceList{},
		Requests: core.ResourceList{},
	}

	if withGeneral {
		for n, q := range map[core.ResourceName]resource.Quantity{
			core.ResourceCPU:              instRess.CPU,
			core.ResourceMemory:           instRess.RAM,
			core.ResourceEphemeralStorage: instRess.LocalStorage,
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
		instRess.Accelerator != nil &&
		instRess.Accelerator.Sign() > 0
	if requestAccelerator {
		cardQ := *instRess.Accelerator
		manufacturer := instType.Status.Detail.Manufacturer
		partProfile := instRess.AcceleratorPartitionedProfile
		partCardResName := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModePartitioned)
		partProfileResName := nodefeature.GetAcceleratablePartitionedProfileResourceName(manufacturer, partProfile)
		switch {
		case withAccelerator:
			switch {
			case partProfile != "" && partCardResName != "" && partProfileResName != "":
				// A hardware partition request is always one card and one instance of one
				// profile shape, so both keys are exactly 1 regardless of the requested card
				// count (the webhook pins it to 1). The credit-counting .partitioned.units is
				// folded by the Pod webhook from the profile's VRAM, so it is not written here.
				one := *resource.NewQuantity(1, resource.DecimalSI)
				rr.Limits[partCardResName] = one
				rr.Requests[partCardResName] = one
				rr.Limits[partProfileResName] = one
				rr.Requests[partProfileResName] = one
			case instType.Status.Detail.IsLogicallySliceable() && instRess.AcceleratorSlicedMemoryPercentage > 0:
				// A sliced request emits the bare card count C (.sliced, which Kueue
				// folds into credits via multiplyBy) plus the per-card memory/compute
				// percentages. The Pod webhook folds .sliced.memory-percentage into the
				// credit-counting .sliced.units before the Pod is persisted.
				slicedResName := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModeSliced)
				memResName := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)
				coresResName := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)
				memQ := *resource.NewQuantity(int64(instRess.AcceleratorSlicedMemoryPercentage), resource.DecimalSI)
				coresQ := *resource.NewQuantity(int64(instRess.AcceleratorSlicedCoresPercentage), resource.DecimalSI)
				rr.Limits[slicedResName] = cardQ
				rr.Requests[slicedResName] = cardQ
				rr.Limits[memResName] = memQ
				rr.Requests[memResName] = memQ
				rr.Limits[coresResName] = coresQ
				rr.Requests[coresResName] = coresQ
			default:
				// A non-sliced type, a 0% request on a sliced type, or a partition request a
				// manufacturer without hardware partitioning cannot express (no key exists to
				// emit, and the webhook rejects such a request) is an exclusive whole-card
				// request.
				resName := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModeExclusive)
				rr.Limits[resName] = cardQ
				rr.Requests[resName] = cardQ
			}
		case withVisibility:
			// The SSH sidecar requests the internal visibility resource with main's
			// card count (never the slice percentages — it grants device access, not a
			// slice). The device-plugin resolves it to main's already-allocated
			// device(s) on the node, giving the sidecar a narrow device-cgroup grant.
			visResName := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModeVisibility)
			rr.Limits[visResName] = cardQ
			rr.Requests[visResName] = cardQ
		}
	}

	return rr
}

// PartitionProfileMemoryPercent reports the share of one card's VRAM the requested hardware
// partition profile occupies, as a percentage in [1,100].
//
// It reports sizeable=false when the pool offers the profile but its observed Detail cannot size
// it yet — the profile's per-instance memory has not been populated, or the per-card VRAM has not
// — which is a transient state during detection or a device-manager rollout skew. The caller must
// reject such a request as retryable rather than fall back to whole-card sizing.
//
// It reports (0, true) when the request is not a partition request at all, or when the named
// profile is not offered. That second case is permanent, not transient, and each caller's own
// validation rejects it with a message naming the offered profiles.
//
// It sits beside getResourceRequirements rather than in the webhook package it was written for,
// because sizing a request against an InstanceType is now done by two callers — the Instance
// webhook, which defaults the values onto the object, and the ModelDeployment renderer, which has
// no mutating webhook and derives them at render time. A second copy of a VRAM-anchored percentage
// would be free to drift from this one, and the symptom of that drift is a Pod whose host CPU and
// memory do not match the fraction of the card it holds.
func PartitionProfileMemoryPercent(
	instType *workercore.InstanceType, profile string,
) (pct int64, sizeable bool) {
	if profile == "" {
		return 0, true
	}
	prof, _, found := PartitionProfile(instType, profile)
	if !found {
		return 0, true
	}
	if prof.MemoryMib <= 0 {
		return 0, false
	}
	cardVRAMMib, err := InstanceTypeCardVRAMMib(instType)
	if err != nil || cardVRAMMib <= 0 {
		return 0, false
	}
	return min(max(prof.MemoryMib*100/cardVRAMMib, 1), 100), true
}

// PartitionProfile finds a partition profile in an InstanceType's observed physical-slice
// inventory (Status.Detail), returning the aggregate (its per-instance MemoryMib and pool-wide
// instance ceiling Count), whether the accelerator Detail has been computed at all, and whether
// the profile was found.
func PartitionProfile(
	instType *workercore.InstanceType, profile string,
) (prof workercore.AcceleratorSlicedPhysicalDetailProfile, ready, found bool) {
	ready = instType.Status.Detail.AcceleratorReady()
	for _, p := range instType.Status.Detail.SlicedDetail.Physical.Profiles {
		if p.Name == profile {
			return p, ready, true
		}
	}
	return workercore.AcceleratorSlicedPhysicalDetailProfile{}, ready, false
}

// InstanceTypeCardVRAMMib parses the per-card VRAM (MiB) from an InstanceType's observed
// Status.Detail.Memory. An empty value is the not-yet-ready state (reject as retryable rather than
// mis-size); a non-positive or unparseable value is a hard error.
func InstanceTypeCardVRAMMib(instType *workercore.InstanceType) (int64, error) {
	memStr := instType.Status.Detail.Memory
	if memStr == "" {
		return 0, fmt.Errorf("instance type %s has no per-card memory yet (detail not ready)", instType.Name)
	}
	q, err := resource.ParseQuantity(memStr)
	if err != nil {
		return 0, fmt.Errorf("parse memory %q of instance type %s: %w", memStr, instType.Name, err)
	}
	mib := q.Value() / quantityx.Mi
	if mib <= 0 {
		return 0, fmt.Errorf("instance type %s has non-positive memory %q", instType.Name, memStr)
	}
	return mib, nil
}
