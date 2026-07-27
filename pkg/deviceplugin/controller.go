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
	"gpustack.ai/gpustack/pkg/device"
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

	// _ReservationKey identifies one container's in-process allocation claim. A pod can hold
	// several at once — the device-plugin API serves one container per Allocate call — so the
	// container name is part of the identity, not a detail of the payload.
	_ReservationKey struct {
		PodUID    types.UID
		Container string
	}

	// _Reservation is the in-process mirror of one container's durable allocation record.
	// DeviceIDs are the device IDs kubelet offered for that container's Allocate; a family
	// whose Allocate picks the card itself hands back devices those IDs do not name, so the
	// IDs cannot be re-derived from the allocation and are kept alongside it.
	_Reservation struct {
		Allocated workercore.DevicesStatus
		DeviceIDs []string
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

		// reservations records, keyed by pod UID AND container name, the accelerator
		// devices a container was allocated, so the SSH sidecar's visibility Allocate
		// (same pod, same node, later in the same admission window) can co-allocate the
		// same physical devices without re-selecting them or racing the annotation's
		// cache propagation. Keying by pod alone would let the first served container of a
		// pod mask every later one: the already-reserved skip would refuse to resolve them,
		// so two containers each holding a live claim could never both be served.
		reservationsMutex sync.RWMutex
		reservations      map[_ReservationKey]_Reservation

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
	//
	// physicalOccupied/physicalAllocated reconstruct each physical-slice card's occupied
	// placement intervals and per-profile instance counts from the same Pod annotations,
	// keyed by card, to feed the placement-aware ledger fold below (pure arithmetic, no
	// device access).
	physicalOccupied := make(map[Resource][]workercore.AcceleratorPhysicalPlacement)
	physicalAllocated := make(map[Resource]map[string]int32)
	livePodUIDs := make([]string, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Keep terminating pods (DeletionTimestamp != nil) in the live set AND in the
		// allocation merge: during the grace period their containers can still be running
		// with the working dir mounted and their hardware still carved, and the reclaimer
		// destroys an instance on pod deletion rather than on container exit. Dropping them
		// here would report a card free while it is still physically occupied.
		livePodUIDs = append(livePodUIDs, string(pod.UID))

		podDevsStatus, err := extractAllocatedStatusFromPod(pod)
		if err != nil {
			// An unreadable record is the one failure this rebuild cannot absorb: the pod's
			// cards drop out of the ledger and read FREE while its containers still hold them,
			// which is how an opposite-mode pod lands on an occupied card. Nothing here can
			// recover the occupancy, so say loudly what is at stake. The reachable cause is an
			// annotation an older device-manager wrote in a shape this one no longer reads —
			// pre-release formats are a clean break, so drain a node before upgrading it.
			logger.Error(err, "cannot read the allocation this pod holds; its cards will be "+
				"reported free while it still occupies them — drain the node before upgrading "+
				"the device manager",
				"pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}

		eDevsStatus, err = applyAllocatedStatus(podDevsStatus, eDevsStatus)
		if err != nil {
			logger.Error(err, "merge allocated accelerators into devices status", "pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}

		accumulatePhysicalOccupied(podDevsStatus, physicalOccupied, physicalAllocated)
	}

	// Fold the per-card physical-slice profile ledger into the same wholesale Status build
	// (never a second, stompable write), computed by pure arithmetic from the
	// annotation-derived occupied set and the detect-time-cached empty-card Placements.
	// The upward transport → aggregated output bridge lives in foldPhysicalLedger.
	foldPhysicalLedger(devs, &eDevsStatus, physicalOccupied, physicalAllocated)

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
	r.notifiersMutex.Unlock()
	r.notifyListeners()

	return ctrl.Result{}, nil
}

// notifyListeners broadcasts the last live-pod-UID sweep to every ListAndWatch subscriber,
// triggering an immediate response rebuild. It fires on every Reconcile and synchronously from
// reserveDevices/releaseReservation, so a card reserved (or freed) in-process is reported
// Unhealthy (or Healthy again) to kubelet in the same instant instead of waiting for the next
// annotation-driven reconcile — closing the window where kubelet could hand a just-held card to
// an opposite-mode pod. Sends are non-blocking and skipped until the first sweep seeds the set,
// so a pre-Reconcile reservation cannot flush a nil live set into the sliced working-dir GC.
func (r *DevicesReconciler) notifyListeners() {
	r.notifiersMutex.Lock()
	defer r.notifiersMutex.Unlock()
	if r.lastLivePodUIDs == nil {
		return
	}
	for i := range r.notifiers {
		notifier := &r.notifiers[i]
		select {
		case notifier.Channel <- r.lastLivePodUIDs:
		default:
		}
	}
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

// reserveDevices records the accelerator devices a container was allocated, keyed by pod UID
// and container name, together with the device IDs kubelet offered for it. It mirrors the
// AllocatedAcceleratorAnnoKey annotation the workload Allocate persists (which stays the
// durable read fallback), giving the sidecar's visibility Allocate a race-free in-process
// source for the same-pod, same-window co-allocation. A no-op for an empty UID, an empty
// container name or an empty allocation.
func (r *DevicesReconciler) reserveDevices(
	podUID types.UID, container string, allocated workercore.DevicesStatus, deviceIDs []string,
) {
	if podUID == "" || container == "" || len(allocated.Groups) == 0 {
		return
	}
	r.reservationsMutex.Lock()
	if r.reservations == nil {
		r.reservations = make(map[_ReservationKey]_Reservation)
	}
	r.reservations[_ReservationKey{PodUID: podUID, Container: container}] = _Reservation{
		Allocated: allocated,
		DeviceIDs: deviceIDs,
	}
	r.reservationsMutex.Unlock()
	// The reservation immediately withholds the card from the opposite mode in ListAndWatch;
	// notify so kubelet sees the health flip now, not after the annotation-driven reconcile.
	r.notifyListeners()
}

// reservedDevices returns the devices recorded for one container by reserveDevices and whether
// a reservation exists.
func (r *DevicesReconciler) reservedDevices(podUID types.UID, container string) (workercore.DevicesStatus, bool) {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	got, ok := r.reservations[_ReservationKey{PodUID: podUID, Container: container}]
	return got.Allocated, ok
}

// pickAcceleratorOwner returns the container holding a pod's accelerator claim among candidates,
// excluding self: the lexicographically smallest remaining name. Accelerator claims are confined
// to a single container group, so a pod holds at most one such record and the answer is
// unambiguous; the names are still compared so a pod that somehow holds several resolves to the
// same container on every call, and every record source agrees on which one it is.
func pickAcceleratorOwner(candidates []string, self string) (string, bool) {
	var owner string
	for _, name := range candidates {
		if name == self {
			continue
		}
		if owner == "" || name < owner {
			owner = name
		}
	}
	return owner, owner != ""
}

// reservedAcceleratorDevices returns the accelerator devices reserved for a pod by a container
// other than self — what a visibility request co-allocates — along with the name of the container
// they were read from, so a caller that must also resolve a per-container record keyed by owner
// uses the same pick that produced the devices.
func (r *DevicesReconciler) reservedAcceleratorDevices(
	podUID types.UID, self string,
) (workercore.DevicesStatus, string, bool) {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	var candidates []string
	for k := range r.reservations {
		if k.PodUID != podUID {
			continue
		}
		candidates = append(candidates, k.Container)
	}
	owner, ok := pickAcceleratorOwner(candidates, self)
	if !ok {
		return workercore.DevicesStatus{}, "", false
	}
	return r.reservations[_ReservationKey{PodUID: podUID, Container: owner}].Allocated, owner, true
}

// reservationsFor returns every container reservation currently held for a pod, keyed by
// container name.
func (r *DevicesReconciler) reservationsFor(podUID types.UID) map[string]_Reservation {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	var held map[string]_Reservation
	for k, v := range r.reservations {
		if k.PodUID != podUID {
			continue
		}
		if held == nil {
			held = make(map[string]_Reservation, 1)
		}
		held[k.Container] = v
	}
	return held
}

// releaseReservation drops the reservation recorded for one container. It rolls back a
// reservation written before a durable-annotation patch that then failed: without the
// annotation the Pod-delete watch (gated on it) would never enqueue a prune, so the card would
// stay held for the opposite mode until the next full resync. Undoing it here frees the card
// immediately.
func (r *DevicesReconciler) releaseReservation(podUID types.UID, container string) {
	r.reservationsMutex.Lock()
	delete(r.reservations, _ReservationKey{PodUID: podUID, Container: container})
	r.reservationsMutex.Unlock()
	// Mirror reserveDevices: a rolled-back reservation restores the card's health for the
	// opposite mode in ListAndWatch, so notify immediately.
	r.notifyListeners()
}

// reservedPhysicalOccupied lists, per accelerator resource, the physical placements the
// in-process reservations currently claim. A placement-authoritative Allocate publishes its
// choice here before releasing the node allocate mutex, so the next serialized caller decides
// against a card that is already spoken for rather than one that merely has not been carved
// yet. It is the synchronous counterpart to LivePhysicalOccupied, whose annotation source
// lags by a cache round-trip; the two are unioned at the decision, and an interval recorded
// in both is harmless because overlap, not multiplicity, is what a placement decision reads.
func (r *DevicesReconciler) reservedPhysicalOccupied() map[Resource][]workercore.AcceleratorPhysicalPlacement {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	occupied := make(map[Resource][]workercore.AcceleratorPhysicalPlacement)
	allocated := make(map[Resource]map[string]int32)
	for _, v := range r.reservations {
		accumulatePhysicalOccupied(v.Allocated, occupied, allocated)
	}
	return occupied
}

// reservedModeForResource reports the allocation mode a physical card (group:device) is
// currently held in by any pod's reservation, and the owning pod UID. The reservation map is
// written synchronously by every workload Allocate, so it is the race-safe cross-pod source of
// a card's held mode when the Devices ledger Status has not yet reconciled. Returns
// DeviceAllocationModeNone and an empty UID when no reservation holds the card.
func (r *DevicesReconciler) reservedModeForResource(group, device string) (workercore.DeviceAllocationMode, types.UID) {
	r.reservationsMutex.RLock()
	defer r.reservationsMutex.RUnlock()
	for key, reservation := range r.reservations {
		status := reservation.Allocated
		for i := range status.Groups {
			grp := &status.Groups[i]
			if grp.ID != group {
				continue
			}
			for j := range grp.Accelerators {
				acc := &grp.Accelerators[j]
				if acc.ID == device && acc.Mode != workercore.DeviceAllocationModeNone {
					return acc.Mode, key.PodUID
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
	for key := range r.reservations {
		if !live.Has(key.PodUID) {
			delete(r.reservations, key)
		}
	}
}

const (
	AllocatedAcceleratorAnnoKey       = "device.gpustack.ai/accelerator.allocated"
	_PreferredAcceleratorIDAnnoKey    = "device.gpustack.ai/accelerator.preferred-id"
	_PreferredAcceleratorIndexAnnoKey = "device.gpustack.ai/accelerator.preferred-index"
)

// _AllocationMatch describes which pending container an Allocate (or the advisory
// GetPreferredAllocation) call can be serving. The device-plugin API omits the pod identity, so
// the resolution is a search over the node's pending containers rather than a lookup.
type _AllocationMatch struct {
	// ResourceName and Quantity are the only two dimensions the RPC itself carries: the
	// resource being served, and how many device IDs kubelet offered for it.
	ResourceName core.ResourceName
	Quantity     resource.Quantity
	// SkipReserved drops a (pod, container) that a previous Allocate already reserved, so
	// concurrent calls serialized by allocateMutex resolve to distinct containers instead of
	// all matching the same oldest one. The visibility path leaves it false: it must re-find
	// the pod whose workload container already holds a reservation.
	SkipReserved bool
	// Feasible, when set, drops a candidate this call cannot actually serve. It is the only
	// disambiguator available for the request dimensions the RPC does not carry — a
	// partition's profile, a slice's per-card units — which is how two pods differing only in
	// one of them can otherwise absorb each other's call.
	//
	// It is deliberately NOT an admission gate: when it rejects every candidate the search
	// falls back to the unfiltered set, because the ledger it reads lags reality and must
	// never turn a resolvable Allocate into a hard failure. Admission is enforced upstream by
	// the Pod webhook and the node-devices admission check.
	Feasible func(pod *core.Pod, ctr *core.Container) bool
}

func (r *DevicesReconciler) getAllocatingPodWithRetry(
	ctx context.Context, match _AllocationMatch,
) (pod *core.Pod, ctr *core.Container, err error) {
	for i := 0; i < 5; i++ {
		pod, ctr, err = r.getAllocatingPod(ctx, match)
		if err == nil {
			return pod, ctr, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, nil, fmt.Errorf("get allocating pod with retry: %w", err)
}

// getAllocatingPod maps a kubelet Allocate/GetPreferredAllocation call to the container being
// admitted, preferring the oldest Pending pod. Candidates are ranked rather than filtered: an
// unreserved feasible container wins; failing that an unreserved but infeasible one (the
// feasibility test disambiguates, it does not gate); failing that an already-reserved one, whose
// allocation is then replayed instead of erroring — the containment for a kubelet that received
// an AllocateResponse but died before checkpointing it.
func (r *DevicesReconciler) getAllocatingPod(
	ctx context.Context, match _AllocationMatch,
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

	type candidate struct {
		pod *core.Pod
		ctr *core.Container
	}
	var feasible, infeasible, reserved []candidate

	classify := func(pod *core.Pod, ctr *core.Container) {
		if !containerRequests(ctr, match.ResourceName, match.Quantity) {
			return
		}
		// A container whose own Allocate already reserved its cards is claimed: the
		// reservation is the synchronous, cache-lag-free marker, so under allocateMutex the
		// next call in a concurrent batch does not re-pick what the previous one just took.
		if match.SkipReserved {
			if _, ok := r.reservedDevices(pod.UID, ctr.Name); ok {
				reserved = append(reserved, candidate{pod, ctr})
				return
			}
		}
		if match.Feasible != nil && !match.Feasible(pod, ctr) {
			infeasible = append(infeasible, candidate{pod, ctr})
			return
		}
		feasible = append(feasible, candidate{pod, ctr})
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if p := pod.Status.Phase; p != "" && p != core.PodPending {
			continue
		}
		for j := range pod.Spec.InitContainers {
			classify(pod, &pod.Spec.InitContainers[j])
		}
		for j := range pod.Spec.Containers {
			classify(pod, &pod.Spec.Containers[j])
		}
	}

	logger := ctrllog.FromContext(ctx)
	switch {
	case len(feasible) > 0:
		return feasible[0].pod, feasible[0].ctr, nil
	case len(infeasible) > 0:
		logger.Info("no candidate container can be served by this allocation; "+
			"resolving to the oldest one anyway rather than failing a resolvable request",
			"resource", match.ResourceName, "candidates", len(infeasible),
			"pod", ctrlcli.ObjectKeyFromObject(infeasible[0].pod), "container", infeasible[0].ctr.Name)
		return infeasible[0].pod, infeasible[0].ctr, nil
	case len(reserved) > 0:
		logger.Info("every candidate container is already reserved; replaying the oldest one's "+
			"allocation, which a kubelet that lost its checkpoint is asking for again",
			"resource", match.ResourceName, "candidates", len(reserved),
			"pod", ctrlcli.ObjectKeyFromObject(reserved[0].pod), "container", reserved[0].ctr.Name)
		return reserved[0].pod, reserved[0].ctr, nil
	}

	return nil, nil, fmt.Errorf("cannot find pending pod with resource request %s=%s",
		match.ResourceName, match.Quantity.String())
}

// containerRequests reports whether the container asks for exactly resQuantity of resName.
func containerRequests(ctr *core.Container, resName core.ResourceName, resQuantity resource.Quantity) bool {
	q, ok := ctr.Resources.Limits[resName]
	return ok && q.Equal(resQuantity)
}

type (
	// ContainerAllocation is what the device plugin allocated for one container of a pod.
	//
	// DeviceIDs are the device IDs kubelet offered for that container's Allocate. They are
	// recorded rather than re-derived because a family whose Allocate picks the card itself
	// hands back devices those IDs do not name, and because the IDs must keep being
	// advertised Healthy for as long as the allocation lives: kubelet refuses to re-admit a
	// container whose checkpointed IDs have left the healthy set.
	ContainerAllocation struct {
		Devices   workercore.DevicesStatus `json:"devices"`
		DeviceIDs []string                 `json:"deviceIDs,omitempty"`
	}

	// PodAllocations is the value of the AllocatedAcceleratorAnnoKey annotation, keyed by
	// container name. The container dimension is what stops a second Allocate from erasing
	// the first container's claim, and what makes a repeated Allocate for one container
	// overwrite its own entry instead of double-counting the card.
	PodAllocations map[string]ContainerAllocation
)

// Aggregate folds every container's record into the pod-wide allocation the ledger consumes.
// Two containers holding the same card keep separate entries, so the card is charged for both.
// Containers are visited in name order, so the result does not depend on map iteration.
func (in PodAllocations) Aggregate() workercore.DevicesStatus {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)

	var (
		aggregated workercore.DevicesStatus
		groupIndex = make(map[string]int)
	)
	for _, name := range names {
		groups := in[name].Devices.Groups
		for i := range groups {
			src := &groups[i]
			idx, ok := groupIndex[src.ID]
			if !ok {
				idx = len(aggregated.Groups)
				groupIndex[src.ID] = idx
				aggregated.Groups = append(aggregated.Groups, workercore.DevicesAllocationGroup{
					ID:           src.ID,
					Manufacturer: src.Manufacturer,
				})
			}
			dst := &aggregated.Groups[idx]
			dst.Accelerators = append(dst.Accelerators, src.Accelerators...)
		}
	}
	return aggregated
}

// AllocatedAcceleratorsOf reads a pod's per-container allocation records. An unannotated pod
// yields an empty map and no error.
func AllocatedAcceleratorsOf(pod *core.Pod) (PodAllocations, error) {
	str := pod.Annotations[AllocatedAcceleratorAnnoKey]
	if str == "" {
		return nil, nil
	}
	var allocations PodAllocations
	if err := json.Unmarshal(stringx.ToBytes(&str), &allocations); err != nil {
		return nil, err
	}
	return allocations, nil
}

// AllocatedAcceleratorGroupsOf returns the pod-wide accelerator allocation recorded on a pod,
// for a caller that only reports it (the Instance status) and has no use for the per-container
// breakdown. An unannotated or malformed annotation yields nil.
func AllocatedAcceleratorGroupsOf(pod *core.Pod) []workercore.DevicesAllocationGroup {
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		return nil
	}
	return allocations.Aggregate().Groups
}

// patchAllocatingPod records one container's allocation on its pod, merging it into whatever the
// pod's other containers already hold. Re-recording the same container overwrites its own entry,
// so a repeated Allocate is idempotent rather than additive.
//
// The merge starts from the pod as the informer has it, then overlays this process's own
// reservations for the pod's other containers. That overlay is not redundant: a strategic-merge
// patch replaces the annotation's whole value, and the informer copy can predate a sibling
// container's patch, so writing only what the cached copy carried would silently erase the
// sibling's claim from the ledger.
func (r *DevicesReconciler) patchAllocatingPod(
	ctx context.Context, pod *core.Pod, container string,
	allocatedStatus workercore.DevicesStatus, deviceIDs []string,
) error {
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		return fmt.Errorf("read allocated accelerators: %w", err)
	}
	if allocations == nil {
		allocations = make(PodAllocations, 1)
	}
	for name, reserved := range r.reservationsFor(pod.UID) {
		if name == container {
			continue
		}
		allocations[name] = ContainerAllocation{
			Devices:   reserved.Allocated,
			DeviceIDs: reserved.DeviceIDs,
		}
	}
	allocations[container] = ContainerAllocation{
		Devices:   allocatedStatus,
		DeviceIDs: deviceIDs,
	}

	allocationsBytes, err := json.Marshal(allocations)
	if err != nil {
		return fmt.Errorf("marshal allocated accelerators: %w", err)
	}

	obj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				AllocatedAcceleratorAnnoKey: string(allocationsBytes),
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
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		return workercore.DevicesStatus{}, err
	}
	return allocations.Aggregate(), nil
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

// accumulatePhysicalOccupied folds a Pod's annotation-recorded physical-slice placements
// into the per-card occupied-interval set and per-profile instance counts, keyed by
// (group, device). A Pod records at most one instance per card (AllocatedPhysicalProfile +
// AllocatedPhysicalPlacements — the upward transport); unioning across the node's Pods
// yields each card's live occupancy with no device access.
func accumulatePhysicalOccupied(
	podStatus workercore.DevicesStatus,
	occupied map[Resource][]workercore.AcceleratorPhysicalPlacement,
	allocated map[Resource]map[string]int32,
) {
	for i := range podStatus.Groups {
		grp := &podStatus.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if acc.AllocatedPhysicalProfile == "" || len(acc.AllocatedPhysicalPlacements) == 0 {
				continue
			}
			res := Resource{Group: grp.ID, Device: acc.ID}
			occupied[res] = append(occupied[res], acc.AllocatedPhysicalPlacements...)
			if allocated[res] == nil {
				allocated[res] = make(map[string]int32)
			}
			// A Pod holds exactly one instance of its profile per card, so count the
			// instance (one per accelerator entry), not its placement intervals.
			allocated[res][acc.AllocatedPhysicalProfile]++
		}
	}
}

// foldPhysicalLedger sets the aggregated OUTPUT AllocatedProfiles/RemainingProfiles on each
// physical-slice-enabled card in the wholesale Status, from the annotation-reconstructed
// occupied set (occupied/allocated — the upward transport accumulatePhysicalOccupied built)
// and the card's detect-time-cached empty-card Placements. A card is physical-slice-enabled
// when its capability carries physical-slice profiles (e.g. NVIDIA MIG); RemainingProfiles
// is the count of each profile's cached legal placements that overlap no occupied interval.
// The status accelerators are built 1:1 from devs.Spec by the caller, so they are indexed
// positionally. A card whose capability has no cached Placements (a not-yet-upgraded
// DaemonSet) yields empty RemainingProfiles — the "ledger not ready" state the
// AdmissionCheck distinguishes from "profile full".
func foldPhysicalLedger(
	devs *workercore.Devices,
	status *workercore.DevicesStatus,
	occupied map[Resource][]workercore.AcceleratorPhysicalPlacement,
	allocated map[Resource]map[string]int32,
) {
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			profiles := acc.Status.PhysicalSliced.Profiles
			if len(profiles) == 0 {
				continue
			}
			possible := make(map[string][]workercore.AcceleratorPhysicalPlacement, len(profiles))
			for k := range profiles {
				p := &profiles[k]
				if len(p.Placements) > 0 {
					possible[p.Name] = p.Placements
				}
			}
			res := Resource{Group: grp.ID, Device: acc.ID}
			dst := &status.Groups[i].Accelerators[j]
			dst.AllocatedProfiles = device.ProfileCountSlice(allocated[res])
			dst.RemainingProfiles = device.ProfileCountSlice(device.ComputeRemainingProfiles(occupied[res], possible))
		}
	}
}

// LivePhysicalOccupied lists, per accelerator resource, the physical-slice placements that Pods
// on this node currently claim by annotation — the same annotation-derived occupied set the
// ledger fold uses. A per-vendor reclaim loop consults it as an attribution self-check, so a
// mis-attributed ownership marker never destroys an instance a running Pod still holds. A
// terminating Pod still counts, matching the live set the reclaim loop drives from: its
// instance is destroyed when the Pod object is gone, not when its containers exit. It reads the
// informer cache (no device I/O).
func (r *DevicesReconciler) LivePhysicalOccupied(ctx context.Context) (map[Resource][]workercore.AcceleratorPhysicalPlacement, error) {
	podList := new(core.PodList)
	if err := r.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: r.NodeName},
		ctrlcli.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	occupied := make(map[Resource][]workercore.AcceleratorPhysicalPlacement)
	allocated := make(map[Resource]map[string]int32)
	for i := range podList.Items {
		podDevsStatus, err := extractAllocatedStatusFromPod(&podList.Items[i])
		if err != nil {
			continue
		}
		accumulatePhysicalOccupied(podDevsStatus, occupied, allocated)
	}
	return occupied, nil
}

// liveDeviceIDs returns every device ID a live allocation on this node holds: the IDs the
// in-process reservations carry, plus the IDs the durable Pod annotations carry — the only
// record that survives a device-manager restart. A terminating Pod still counts, matching
// LivePhysicalOccupied: its allocation is released when the Pod object is gone, not when its
// containers exit.
//
// The kubelet checkpoints the exact IDs it offered a container and refuses any later
// allocation for it unless every one of them is still advertised healthy, so this set is what
// a family whose health is a node-level count must keep Healthy. It reads the informer cache
// (no device I/O). An unreadable annotation is logged and skipped: its IDs then risk being
// reported Unhealthy, which the operator has to know about.
func (r *DevicesReconciler) liveDeviceIDs(ctx context.Context) (sets.Set[string], error) {
	podList := new(core.PodList)
	if err := r.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: r.NodeName},
		ctrlcli.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	held := sets.New[string]()
	for i := range podList.Items {
		pod := &podList.Items[i]
		allocations, err := AllocatedAcceleratorsOf(pod)
		if err != nil {
			ctrllog.FromContext(ctx).Error(err, "read the allocation annotation of a pod; "+
				"its device IDs may be reported unhealthy and strand the kubelet's checkpoint",
				"pod", kubemeta.GetNamespacedNameKey(pod))
			continue
		}
		for _, allocation := range allocations {
			held.Insert(allocation.DeviceIDs...)
		}
	}
	r.reservationsMutex.RLock()
	for _, reservation := range r.reservations {
		held.Insert(reservation.DeviceIDs...)
	}
	r.reservationsMutex.RUnlock()
	return held, nil
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
