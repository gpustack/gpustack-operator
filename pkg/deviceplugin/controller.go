package deviceplugin

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

type (
	_DevicesNotifier struct {
		Manufacturer   string
		AllocationMode workercore.DeviceAllocationMode
		Channel        chan []string
	}

	// DevicesReconciler reconciles v1alpha1.Devices objects on a Kubernetes Node
	// and watches the events of Pods scheduled to the Node, to manage the status of Devices.
	DevicesReconciler struct {
		NodeName  string
		Client    ctrlcli.Client
		APIReader ctrlcli.Reader

		notifiersMutex sync.RWMutex
		notifiers      []_DevicesNotifier
		// lastLivePodUIDs caches the most recent broadcast live pod-UID set so a notifier
		// that subscribes after a reconcile is seeded immediately. Without it a late
		// subscriber (a per-vendor reclaim loop registering after the initial reconcile)
		// could stay unseeded until the next reconcile and never reclaim on its resync tick.
		lastLivePodUIDs []string

		// reservations records, keyed by pod UID, the accelerator devices a pod's
		// workload container was allocated, so the SSH sidecar's visibility Allocate
		// (same pod, same node, later in the same admission window) can co-allocate the
		// same physical devices without re-selecting them or racing the annotation's
		// cache propagation.
		reservationsMutex sync.RWMutex
		reservations      map[types.UID]workercore.DevicesStatus

		// allocateMutex serializes the whole workload Allocate identify→cross-mode-check→reserve
		// section for the node. All per-mode ResourceServers share this one reconciler, so a single
		// node-wide mutex makes concurrent Allocates (e.g. a Kueue-admitted batch of identical Pods)
		// run that section one at a time: the reservation each writes is visible before the next
		// identifies its pod, so getAllocatingPod (skipping already-reserved pods) maps each Allocate
		// to a distinct pod instead of all resolving to the oldest pending one, and no opposite-mode
		// Allocate for the same card can interleave between the cross-mode check and the reservation
		// write (TOCTOU). It is never held across the annotation patch (I/O).
		allocateMutex sync.Mutex
	}
)

func (r *DevicesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nodeName := req.Name

	logger := ctrllog.FromContext(ctx).WithValues("node", nodeName)

	devs := new(workercore.Devices)
	err := r.Client.Get(ctx, req.NamespacedName, devs)
	if err != nil {
		logger.Error(err, "fetch devices")
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	podList := new(core.PodList)
	err = r.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: nodeName},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list pods with node name")
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	// Clear/Initialize devices status.
	eDevsStatus := workercore.DevicesStatus{
		Groups: make([]workercore.DevicesAllocationGroup, 0, len(devs.Spec.Groups)),
	}
	for i := range devs.Spec.Groups {
		devsGroup := &devs.Spec.Groups[i]
		devsStatusGroup := workercore.DevicesAllocationGroup{
			ID:           devsGroup.ID,
			Manufacturer: devsGroup.Manufacturer,
			Accelerators: make([]workercore.AcceleratorAllocation, 0, len(devsGroup.Accelerators)),
		}
		for j := range devsGroup.Accelerators {
			devAccelerator := &devsGroup.Accelerators[j]
			devsStatusGroup.Accelerators = append(devsStatusGroup.Accelerators, workercore.AcceleratorAllocation{
				ID:        devAccelerator.ID,
				Index:     devAccelerator.Index,
				Mode:      workercore.DeviceAllocationModeNone,
				Remaining: nodefeature.ResourceMaxUnits,
			})
		}
		eDevsStatus.Groups = append(eDevsStatus.Groups, devsStatusGroup)
	}

	// Merge allocated accelerators, and in the same pass collect the live pod-UUID
	// set this node hands to the sliced per-pod working-dir GC (empty/nil ⇒ no pods;
	// non-sliced consumers ignore the payload).
	livePodUIDs := make([]string, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Keep terminating pods (DeletionTimestamp != nil) in the live set: during
		// the grace period their containers can still be running with the working
		// dir mounted, so the per-pod GC must not reclaim it until the pod object is
		// actually gone. They are still skipped for the allocation-status merge.
		livePodUIDs = append(livePodUIDs, string(pod.UID))
		if pod.DeletionTimestamp != nil {
			continue
		}

		podDevsStatus, err := extractAllocatedStatusFromPod(pod)
		if err != nil {
			logger.Error(err, "extract allocated accelerators from pod", "pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}

		eDevsStatus, err = applyAllocatedStatus(podDevsStatus, eDevsStatus)
		if err != nil {
			logger.Error(err, "merge allocated accelerators into devices status", "pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}
	}

	if !kubemeta.DeepEqual(devs.Status, eDevsStatus) {
		devs.Status = eDevsStatus
		err = r.Client.Status().Update(ctx, devs)
		if err != nil {
			logger.Error(err, "update devices status")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}
	}

	// Drop reservations whose pod is gone (same live set the sliced working-dir GC uses).
	r.pruneReservations(livePodUIDs)

	r.notifiersMutex.Lock()
	r.lastLivePodUIDs = livePodUIDs
	for i := range r.notifiers {
		notifier := &r.notifiers[i]
		select {
		case notifier.Channel <- livePodUIDs:
		default:
			logger.Error(nil,
				"notifier channel is full, skipping notify",
				"manufacturer", notifier.Manufacturer,
				"mode", notifier.AllocationMode.String(),
			)
		}
	}
	r.notifiersMutex.Unlock()

	return ctrl.Result{}, nil
}

const IndexingPodsByNodeName = "pods.nodeName"

func (r *DevicesReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Pod{}, IndexingPodsByNodeName,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			pod := obj.(*core.Pod)
			return []string{pod.Spec.NodeName}
		})
	if err != nil {
		return fmt.Errorf("index pod '%s': %w", IndexingPodsByNodeName, err)
	}

	r.NodeName = osx.Getenv("KUBERNETES_NODE_NAME")
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("deviceplugin.manage.devices").
		For(
			&workercore.Devices{},
			ctrlbuilder.WithPredicates(
				// Interested in the Devices object corresponding to the current node.
				ctrlpredicate.NewPredicateFuncs(func(object ctrlcli.Object) bool {
					return object.GetName() == r.NodeName
				}),
			),
		).
		Watches(
			// Watch Kubernetes Pods and enqueue the corresponding v1.Devices.
			&core.Pod{},
			ctrlhandler.EnqueueRequestsFromMapFunc(
				r.enqueueDevicesWhenPodChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Pod is scheduled to the current node
				// and has the AllocatedAcceleratorAnnoKey annotation.
				ctrlpredicate.NewPredicateFuncs(func(object ctrlcli.Object) bool {
					pod := object.(*core.Pod)
					return pod.Spec.NodeName == r.NodeName
				}),
				ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return kubemeta.HasAnnotation(e.Object, AllocatedAcceleratorAnnoKey)
					},
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return kubemeta.HasAnnotation(e.Object, AllocatedAcceleratorAnnoKey)
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldPod, newPod := e.ObjectOld, e.ObjectNew
						if newPod.GetDeletionTimestamp() == nil {
							return !mapx.EqualWithKey(oldPod.GetAnnotations(), newPod.GetAnnotations(), AllocatedAcceleratorAnnoKey)
						}
						if kubemeta.HasAnnotation(oldPod, AllocatedAcceleratorAnnoKey) {
							if oldPod.GetDeletionTimestamp() == nil {
								return true
							}
							return !oldPod.GetDeletionTimestamp().Equal(newPod.GetDeletionTimestamp())
						}
						return false
					},
				},
			),
		).
		Complete(r)
}

func (r *DevicesReconciler) enqueueDevicesWhenPodChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("pod", ctrlcli.ObjectKeyFromObject(obj))

	pod := obj.(*core.Pod)

	// Ensure the Pod is scheduled to the current node, as a safety check.
	if pod.Spec.NodeName != r.NodeName {
		logger.Error(nil, "pod is not scheduled to the current node")
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{
				Name: pod.Spec.NodeName,
			},
		},
	}
	logger.V(2).Info("enqueued devices for pod", "requests", reqs)
	return reqs
}

func (r *DevicesReconciler) getReconcileNotifier(manufacturer string, allocationMode workercore.DeviceAllocationMode) <-chan []string {
	r.notifiersMutex.Lock()
	defer r.notifiersMutex.Unlock()

	channel := make(chan []string, 4)
	r.notifiers = append(r.notifiers, _DevicesNotifier{
		Manufacturer:   manufacturer,
		AllocationMode: allocationMode,
		Channel:        channel,
	})
	// Seed a late subscriber with the last broadcast set so its reclaim loop never stays
	// unseeded. The channel is freshly buffered, so this send never blocks.
	if r.lastLivePodUIDs != nil {
		channel <- r.lastLivePodUIDs
	}
	return channel
}

func (r *DevicesReconciler) getDevices(ctx context.Context) (*workercore.Devices, error) {
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{
			Name: r.NodeName,
		},
	}
	err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(devs), devs,
		&ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
	return devs, err
}

// reserveDevices records the accelerator devices a pod's workload container was
// allocated, keyed by pod UID. It mirrors the AllocatedAcceleratorAnnoKey annotation the
// workload Allocate persists (which stays the durable read fallback), giving the sidecar's
// visibility Allocate a race-free in-process source for the same-pod, same-window
// co-allocation. A no-op for an empty UID or an empty allocation.
func (r *DevicesReconciler) reserveDevices(podUID types.UID, allocated workercore.DevicesStatus) {
	if podUID == "" || len(allocated.Groups) == 0 {
		return
	}
	r.reservationsMutex.Lock()
	defer r.reservationsMutex.Unlock()
	if r.reservations == nil {
		r.reservations = make(map[types.UID]workercore.DevicesStatus)
	}
	r.reservations[podUID] = allocated
}

// reservedDevices returns the devices recorded for a pod by reserveDevices and whether a
// reservation exists.
func (r *DevicesReconciler) reservedDevices(podUID types.UID) (workercore.DevicesStatus, bool) {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	got, ok := r.reservations[podUID]
	return got, ok
}

// releaseReservation drops the reservation recorded for a pod. It rolls back a reservation
// written before a durable-annotation patch that then failed: without the annotation the
// Pod-delete watch (gated on it) would never enqueue a prune, so the card would stay held for
// the opposite mode until the next full resync. Undoing it here frees the card immediately.
func (r *DevicesReconciler) releaseReservation(podUID types.UID) {
	r.reservationsMutex.Lock()
	defer r.reservationsMutex.Unlock()
	delete(r.reservations, podUID)
}

// reservedModeForResource reports the allocation mode a physical card (group:device) is
// currently held in by any pod's reservation, and the owning pod UID. The reservation map is
// written synchronously by every workload Allocate, so it is the race-safe cross-pod source of
// a card's held mode when the Devices ledger Status has not yet reconciled. Returns
// DeviceAllocationModeNone and an empty UID when no reservation holds the card.
func (r *DevicesReconciler) reservedModeForResource(group, device string) (workercore.DeviceAllocationMode, types.UID) {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	for uid, status := range r.reservations {
		for i := range status.Groups {
			grp := &status.Groups[i]
			if grp.ID != group {
				continue
			}
			for j := range grp.Accelerators {
				acc := &grp.Accelerators[j]
				if acc.ID == device && acc.Mode != workercore.DeviceAllocationModeNone {
					return acc.Mode, uid
				}
			}
		}
	}
	return workercore.DeviceAllocationModeNone, ""
}

// pruneReservations drops reservations for pods no longer in the live set, so a
// reservation cannot outlive its pod. It piggybacks the Reconcile live-pod-UID sweep.
func (r *DevicesReconciler) pruneReservations(livePodUIDs []string) {
	r.reservationsMutex.Lock()
	defer r.reservationsMutex.Unlock()
	if len(r.reservations) == 0 {
		return
	}
	live := sets.New[types.UID]()
	for i := range livePodUIDs {
		live.Insert(types.UID(livePodUIDs[i]))
	}
	for uid := range r.reservations {
		if !live.Has(uid) {
			delete(r.reservations, uid)
		}
	}
}

const (
	AllocatedAcceleratorAnnoKey       = "device.gpustack.ai/accelerator.allocated"
	_PreferredAcceleratorIDAnnoKey    = "device.gpustack.ai/accelerator.preferred-id"
	_PreferredAcceleratorIndexAnnoKey = "device.gpustack.ai/accelerator.preferred-index"
)

func (r *DevicesReconciler) getAllocatingPodWithRetry(
	ctx context.Context, resName core.ResourceName, resQuantity resource.Quantity, skipReserved bool,
) (pod *core.Pod, ctr *core.Container, err error) {
	for i := 0; i < 5; i++ {
		pod, ctr, err = r.getAllocatingPod(ctx, resName, resQuantity, skipReserved)
		if err == nil {
			return pod, ctr, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, nil, fmt.Errorf("get allocating pod with retry: %w", err)
}

// getAllocatingPod maps a kubelet Allocate/GetPreferredAllocation call to the pod being admitted.
// The device-plugin API omits the pod identity, so it picks the oldest Pending pod on the node whose
// container requests the matching resource+quantity. When skipReserved is set it also skips pods that
// already hold an in-process reservation, so concurrent workload Allocates serialized by allocateMutex
// each resolve to a distinct pod instead of all matching the same oldest one; the visibility path
// passes false, since it must re-find its own reserved pod.
func (r *DevicesReconciler) getAllocatingPod(
	ctx context.Context, resName core.ResourceName, resQuantity resource.Quantity, skipReserved bool,
) (*core.Pod, *core.Container, error) {
	podList := new(core.PodList)
	err := r.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: r.NodeName},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		return nil, nil, fmt.Errorf("list pods with node name: %w", err)
	}
	if len(podList.Items) == 0 {
		return nil, nil, fmt.Errorf("no pods found with node name %s", r.NodeName)
	}

	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].CreationTimestamp.Before(&podList.Items[j].CreationTimestamp)
	})

	for i := range podList.Items {
		pod := &podList.Items[i]
		if p := pod.Status.Phase; p != "" && p != core.PodPending {
			continue
		}
		// Skip a pod whose workload Allocate already reserved its cards: the reservation is the
		// synchronous, cache-lag-free claim marker, so under allocateMutex the next Allocate in a
		// concurrent batch does not re-pick the pod the previous one just claimed.
		if skipReserved {
			if _, ok := r.reservedDevices(pod.UID); ok {
				continue
			}
		}
		for j := range pod.Spec.InitContainers {
			ctr := &pod.Spec.InitContainers[j]
			for actualResName, actualResQuantity := range ctr.Resources.Limits {
				if actualResName == resName && actualResQuantity.Equal(resQuantity) {
					return pod, ctr, nil
				}
			}
		}
		for j := range pod.Spec.Containers {
			ctr := &pod.Spec.Containers[j]
			for actualResName, actualResQuantity := range ctr.Resources.Limits {
				if actualResName == resName && actualResQuantity.Equal(resQuantity) {
					return pod, ctr, nil
				}
			}
		}
	}

	return nil, nil, fmt.Errorf("cannot find pending pod with resource request %s=%s", resName, resQuantity.String())
}

func (r *DevicesReconciler) patchAllocatingPod(ctx context.Context, pod *core.Pod, allocatedStatus workercore.DevicesStatus) error {
	allocatedStatusBytes, err := json.Marshal(allocatedStatus)
	if err != nil {
		return fmt.Errorf("marshal allocated groups: %w", err)
	}

	obj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				AllocatedAcceleratorAnnoKey: string(allocatedStatusBytes),
			},
		},
	}
	objBytes, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal object meta: %w", err)
	}

	_, err = kubeclientset.PatchWithCtrlClient(ctx, r.Client, pod, types.StrategicMergePatchType, objBytes)
	return err
}

func extractAllocatedStatusFromPod(pod *core.Pod) (allocatedStatus workercore.DevicesStatus, err error) {
	if pod.Annotations != nil && pod.Annotations[AllocatedAcceleratorAnnoKey] != "" {
		str := pod.Annotations[AllocatedAcceleratorAnnoKey]
		err = json.Unmarshal(stringx.ToBytes(&str), &allocatedStatus)
	}
	return allocatedStatus, err
}

func applyAllocatedStatus(allocatedStatus, remainingStatus workercore.DevicesStatus) (workercore.DevicesStatus, error) {
	if len(allocatedStatus.Groups) == 0 {
		return remainingStatus, nil
	}

	dstAcceleratorIndex := make(map[Resource]*workercore.AcceleratorAllocation)
	for i := range remainingStatus.Groups {
		dstGrp := &remainingStatus.Groups[i]
		for j := range dstGrp.Accelerators {
			dstAccelerator := &dstGrp.Accelerators[j]
			res := Resource{
				Group:  dstGrp.ID,
				Device: dstAccelerator.ID,
			}
			dstAcceleratorIndex[res] = dstAccelerator
		}
	}

	for i := range allocatedStatus.Groups {
		srcGrp := &allocatedStatus.Groups[i]
		for j := range srcGrp.Accelerators {
			srcAccelerator := &srcGrp.Accelerators[j]
			res := Resource{
				Group:  srcGrp.ID,
				Device: srcAccelerator.ID,
			}
			dstAccelerator, exists := dstAcceleratorIndex[res]
			if !exists {
				continue
			}
			if dstAccelerator.Mode != workercore.DeviceAllocationModeNone &&
				dstAccelerator.Mode != srcAccelerator.Mode {
				return remainingStatus, fmt.Errorf("conflicting allocation mode for resource %v: %v vs. %v",
					res, dstAccelerator.Mode, srcAccelerator.Mode)
			}
			dstAccelerator.Mode = srcAccelerator.Mode
			dstAccelerator.Remaining = max(dstAccelerator.Remaining-srcAccelerator.Allocated, 0)
		}
	}

	return remainingStatus, nil
}

func extractPreferredAcceleratorIDsFromPod(pod *core.Pod, devices *workercore.Devices) sets.Set[string] {
	if pod != nil && pod.Annotations != nil {
		str, ok := pod.Annotations[_PreferredAcceleratorIDAnnoKey]
		if ok {
			requiredIDs := strings.Split(str, ",")
			for i := range requiredIDs {
				requiredIDs[i] = strings.TrimSpace(requiredIDs[i])
			}
			return sets.New[string](requiredIDs...)
		}

		str, ok = pod.Annotations[_PreferredAcceleratorIndexAnnoKey]
		if ok {
			requiredIndexes := strings.Split(str, ",")
			for i := range requiredIndexes {
				requiredIndexes[i] = strings.TrimSpace(requiredIndexes[i])
			}
			requiredIndexesSet := sets.New[string](requiredIndexes...)

			requiredIDsSet := sets.New[string]()
			for i := range devices.Spec.Groups {
				devGroup := &devices.Spec.Groups[i]
				for j := range devGroup.Accelerators {
					devAccelerator := &devGroup.Accelerators[j]
					if requiredIndexesSet.Has(strconv.Itoa(int(devAccelerator.Index))) {
						requiredIDsSet.Insert(devAccelerator.ID)
					}
				}
			}
			return requiredIDsSet
		}
	}

	return sets.New[string]()
}
