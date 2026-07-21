package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	klog "k8s.io/klog/v2"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

type ResourceServer struct {
	deviceplugin.UnimplementedDevicePluginServer

	Logger         klog.Logger
	Manufacturer   string
	AllocationMode workercore.DeviceAllocationMode
	Reconciler     *DevicesReconciler
	Responder      ContainerAllocateResponder

	server *grpc.Server
}

// GetResourceName returns the resource name to be registered to the Device Manager based on the kind and name.
func (s *ResourceServer) GetResourceName() core.ResourceName {
	// GetAcceleratableResourceName maps every mode, including the internal Visibility mode
	// (the device-only "device.gpustack.ai/<manufacturer>.visibility" resource). For sliced
	// this is the bare ".sliced" injection-token key; the ".sliced.units" counting key is
	// reported separately via Patch Node, not the device-plugin.
	return nodefeature.GetAcceleratableResourceName(s.Manufacturer, s.AllocationMode)
}

// GetDevicePluginOptions returns options to be communicated with the Device Manager.
func (s *ResourceServer) GetDevicePluginOptions(context.Context, *Empty) (*Options, error) {
	opts := &Options{
		GetPreferredAllocationAvailable: true,
	}
	return opts, nil
}

// ListAndWatch returns a stream of List of Devices.
// Whenever a Device state change or a Device disappears, ListAndWatch returns the new list.
func (s *ResourceServer) ListAndWatch(_ *Empty, srv grpc.ServerStreamingServer[ListAndWatchResponse]) error {
	// Get notifier at the beginning of ListAndWatch to avoid missing any update during the initial ListAndWatch.
	notifier := s.Reconciler.getReconcileNotifier(s.Manufacturer, s.AllocationMode)

	ctx := srv.Context()

	// Send the initial ListAndWatch response.
	s.Logger.Info("sending initial list and watch response")
	err := waitx.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) error {
		resp, err := s.getListAndWatchResponse(ctx)
		if err != nil {
			// Nothing to do, keep looping until success or context cancellation.
			s.Logger.Error(err, "get initial list and watch response, retry later")
		} else if err = srv.Send(resp); err != nil {
			// Return error to restart Device Plugin Server.
			return err
		}
		return nil
	})
	if err != nil {
		s.Logger.Error(err, "initial list and watch")
		return err
	}

	// Sliced containers leave per-pod working directories on the host; reclaim them
	// as their pods disappear from the reconciler's live pod-UUID set.
	var gc *podDirGC
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		gc = newPodDirGC(OperatorPodsDir)
	}

	// Watch for updates and send ListAndWatch response whenever there's a change.
	s.Logger.Info("watching for device updates")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case livePodUIDs := <-notifier:
			resp, err := s.getListAndWatchResponse(ctx)
			if err != nil {
				s.Logger.Error(err, "get list and watch response on update")
				return err
			}
			if err = srv.Send(resp); err != nil {
				s.Logger.Error(err, "send list and watch response")
				return err
			}
			s.Logger.Info("sent list and watch response")
			if gc != nil {
				gc.reconcile(livePodUIDs)
			}
		}
	}
}

func (s *ResourceServer) getListAndWatchResponse(ctx context.Context) (*ListAndWatchResponse, error) {
	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		return nil, err
	}

	resp := &deviceplugin.ListAndWatchResponse{}
	for i := range devs.Spec.Groups {
		devGroup := &devs.Spec.Groups[i]
		if devGroup.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range devGroup.Accelerators {
			devAccelerator := &devGroup.Accelerators[j]
			res := Resource{
				Group:  devGroup.ID,
				Device: devAccelerator.ID,
			}
			// ListAndWatch reflects hardware health only, never allocation state: a card held in
			// another mode stays advertised. Withholding its tokens here would make the advertised
			// allocatable count fluctuate with allocation (the external Detector drives these
			// updates), desynchronizing kubelet's device accounting. The cross-mode invariant is
			// enforced at the authoritative Allocate gate instead.
			health := deviceplugin.Healthy
			if devAccelerator.Status.Unhealthy {
				health = deviceplugin.Unhealthy
			}
			var topology *deviceplugin.TopologyInfo
			if numa := binding.StrRangeToList(devAccelerator.Topology.NumaAffinity); len(numa) > 0 {
				topology = &deviceplugin.TopologyInfo{
					Nodes: slicex.Transform(numa, func(n int) *deviceplugin.NUMANode {
						return &deviceplugin.NUMANode{
							ID: int64(n),
						}
					}),
				}
			}
			// Size the sliced token pool from the card's own per-card slicing capability
			// (its logical soft count, or the MIG physical ceiling). The count is ignored
			// for non-sliced modes.
			slicedCount := devAccelerator.Status.EffectiveSlicedCount()
			ids := res.GetDeviceIds(s.AllocationMode, slicedCount)
			for k := range ids {
				resp.Devices = append(resp.Devices,
					&deviceplugin.Device{
						ID:       ids[k],
						Health:   health,
						Topology: topology,
					},
				)
			}
		}
	}
	return resp, nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate from a list of available ones.
// The resulting preferred allocation is not guaranteed to be the allocation ultimately performed by the Device Manager.
// It is only designed to help the Device Manager make a more informed allocation decision when possible.
func (s *ResourceServer) GetPreferredAllocation(ctx context.Context, req *PreferredAllocationRequest) (*PreferredAllocationResponse, error) {
	// The visibility token pool is flat and interchangeable — every token resolves to the
	// same reserved device at Allocate time — so there is no preference to express. Return an
	// empty response (kubelet picks freely) rather than run the sliced per-card bin-fit, which
	// assumes tokens map to real devices.
	if s.AllocationMode == workercore.DeviceAllocationModeVisibility {
		return &PreferredAllocationResponse{
			ContainerResponses: []*ContainerPreferredAllocationResponse{{}},
		}, nil
	}

	ctrReq := req.GetContainerRequests()[0]

	resName := s.GetResourceName()
	resQuantity := *resource.NewQuantity(int64(ctrReq.GetAllocationSize()), resource.DecimalSI)
	// Advisory path: do not skip reserved pods (kubelet may ignore this hint, and the authoritative
	// pod identification with skip-reserved happens in Allocate).
	pod, ctr, err := s.Reconciler.getAllocatingPodWithRetry(ctx, resName, resQuantity, false)
	if err != nil {
		s.Logger.Error(err, "get allocating pod for preferred allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for preferred allocation: %v", err)
	}

	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		s.Logger.Error(err, "get devices for preferred allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get devices for preferred allocation: %v", err)
	}

	ctrResp, err := s.getContainerPreferredAllocationResponse(ctrReq, pod, ctr, devs)
	if err != nil {
		s.Logger.Error(err, "get container preferred allocation response")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get container preferred allocation response: %v", err)
	}

	resp := &PreferredAllocationResponse{
		ContainerResponses: []*ContainerPreferredAllocationResponse{ctrResp},
	}
	s.Logger.Info("get preferred allocation response", "pod", kubemeta.GetNamespacedNameKey(pod), "response", resp)
	return resp, nil
}

func (s *ResourceServer) getContainerPreferredAllocationResponse(
	ctrReq *ContainerPreferredAllocationRequest,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
) (*ContainerPreferredAllocationResponse, error) {
	availableDeviceIDs := ctrReq.GetAvailableDeviceIDs()
	sort.Strings(availableDeviceIDs)
	availableResUnitsMap := make(map[Resource][]ResourceUnit)
	for i := range availableDeviceIDs {
		resUnit, err := ConvertResourceUnitFromDeviceIds(availableDeviceIDs[i])
		if err != nil {
			return nil, fmt.Errorf("convert available device id %q: %w", availableDeviceIDs[i], err)
		}
		availableResUnitsMap[resUnit.Resource] = append(availableResUnitsMap[resUnit.Resource], resUnit)
	}

	mustIncludedDeviceIDs := ctrReq.GetMustIncludeDeviceIDs()
	sort.Strings(mustIncludedDeviceIDs)
	mustIncludedResUnitsMap := make(map[Resource][]ResourceUnit)
	for i := range mustIncludedDeviceIDs {
		resUnit, err := ConvertResourceUnitFromDeviceIds(mustIncludedDeviceIDs[i])
		if err != nil {
			return nil, fmt.Errorf("convert must include device id %q: %w", mustIncludedDeviceIDs[i], err)
		}
		mustIncludedResUnitsMap[resUnit.Resource] = append(mustIncludedResUnitsMap[resUnit.Resource], resUnit)
	}

	allocationSize := ctrReq.GetAllocationSize()
	preferredDeviceIDsSet := extractPreferredAcceleratorIDsFromPod(pod, devs)
	remainingSize := allocationSize

	// For sliced, each selected card must still have this container's per-card ".sliced.units"
	// (the memory budget the Pod webhook folded in) free, so slices spread across cards instead of
	// stacking on one and over-committing its VRAM. The ledger records sliced allocations in real
	// units, so a card carrying a slice reports Remaining below a fresh card. Zero → no per-card
	// bin-fit (a Pod the webhook did not shape); the loop then behaves as before.
	slicedUnits := int32(0)
	if s.AllocationMode == workercore.DeviceAllocationModeSliced && ctr != nil {
		if q, ok := ctr.Resources.Limits[nodefeature.GetAcceleratableSlicedUnitsResourceName(s.Manufacturer)]; ok {
			slicedUnits = int32(min(q.Value(), int64(nodefeature.ResourceMaxUnits)))
		}
	}

	selectedResUnits := make([]ResourceUnit, 0, allocationSize)
	var unselectedResUnits []ResourceUnit // Only used if provided preferred device IDs.
	for i := range devs.Spec.Groups {
		devsGroup := &devs.Spec.Groups[i]
		if devsGroup.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range devsGroup.Accelerators {
			devsAccelerator := &devsGroup.Accelerators[j]
			res := Resource{
				Group:  devsGroup.ID,
				Device: devsAccelerator.ID,
			}

			// Skip the resource is not in the available list.
			resUnits, existed := availableResUnitsMap[res]
			if !existed {
				continue
			}
			// Skip the resource is occupied by other modes.
			mode := workercore.DeviceAllocationModeNone
			if len(devs.Status.Groups) > i && len(devs.Status.Groups[i].Accelerators) > j {
				mode = devs.Status.Groups[i].Accelerators[j].Mode
			}
			if mode != workercore.DeviceAllocationModeNone && mode != s.AllocationMode {
				continue
			}

			// Sliced spreads across cards: defer a card that cannot fit this slice's per-card
			// units without over-committing it (its ledger Remaining is below the request) to
			// unselectedResUnits, so it is used only as a last resort when no card fits. Stacking
			// slices on one card is what drove runtime per-card VRAM overcommit.
			if slicedUnits > 0 {
				remaining := int32(nodefeature.ResourceMaxUnits)
				if len(devs.Status.Groups) > i && len(devs.Status.Groups[i].Accelerators) > j {
					remaining = devs.Status.Groups[i].Accelerators[j].Remaining
				}
				if remaining < slicedUnits {
					unselectedResUnits = append(unselectedResUnits, resUnits[0])
					continue
				}
			}

			// Exclusive, shared and sliced all select one device unit (token) per
			// card; the per-card concurrency/units accounting lives elsewhere (Kueue
			// credits and the ".sliced.units" capacity), not in the device plugin.
			if miResUnits, existed := mustIncludedResUnitsMap[res]; existed {
				// Only the first must-include unit per card is consumed (one token).
				preferredDeviceIDsSet.Delete(res.Device)
				selectedResUnits = append(selectedResUnits, miResUnits[0])
			} else {
				if preferredDeviceIDsSet.Len() != 0 && !preferredDeviceIDsSet.Has(res.Device) {
					unselectedResUnits = append(unselectedResUnits, resUnits[0])
					continue
				}
				preferredDeviceIDsSet.Delete(res.Device)
				selectedResUnits = append(selectedResUnits, resUnits[0])
			}
			remainingSize -= 1
			if preferredDeviceIDsSet.Len() == 0 && remainingSize <= 0 {
				goto outside
			}
		}
	}
outside:

	if preferredDeviceIDsSet.Len() > 0 {
		s.Logger.Error(nil, "not enough preferred devices: %v", preferredDeviceIDsSet.UnsortedList())
		if len(unselectedResUnits) == 0 {
			return &ContainerPreferredAllocationResponse{}, nil
		}
		if remainingSize <= int32(len(unselectedResUnits)) {
			s.Logger.Info("try to allocate from unselected devices since preferred devices are not enough")
			selectedResUnits = append(selectedResUnits, unselectedResUnits[:remainingSize]...)
			remainingSize = 0
		}
	}

	if remainingSize > 0 {
		s.Logger.Error(nil, "not enough devices: required %d, but only %d available", allocationSize, allocationSize-remainingSize)
		return &ContainerPreferredAllocationResponse{}, nil
	}

	deviceIDs := make([]string, 0, len(selectedResUnits))
	for i := range selectedResUnits {
		deviceIDs = append(deviceIDs, selectedResUnits[i].String())
	}

	resp := &ContainerPreferredAllocationResponse{
		DeviceIDs: deviceIDs,
	}
	return resp, nil
}

// statusModeOf returns the allocation mode the Devices ledger currently records for a physical
// card, or DeviceAllocationModeNone when the card is free or absent from the Status.
func statusModeOf(devs *workercore.Devices, res Resource) workercore.DeviceAllocationMode {
	for i := range devs.Status.Groups {
		grp := &devs.Status.Groups[i]
		if grp.ID != res.Group {
			continue
		}
		for j := range grp.Accelerators {
			if grp.Accelerators[j].ID == res.Device {
				return grp.Accelerators[j].Mode
			}
		}
	}
	return workercore.DeviceAllocationModeNone
}

// cardHeldInOtherMode reports whether the physical card is currently held in a mode different
// from this server's, per the ledger Status OR the in-process reservations (the reservation is
// race-safe: written synchronously by every workload Allocate). Free (None) or same-mode is not
// a conflict. It returns the conflicting mode for logging.
func (s *ResourceServer) cardHeldInOtherMode(devs *workercore.Devices, res Resource) (bool, workercore.DeviceAllocationMode) {
	if m := statusModeOf(devs, res); m != workercore.DeviceAllocationModeNone && m != s.AllocationMode {
		return true, m
	}
	if m, _ := s.Reconciler.reservedModeForResource(res.Group, res.Device); m != workercore.DeviceAllocationModeNone && m != s.AllocationMode {
		return true, m
	}
	return false, workercore.DeviceAllocationModeNone
}

// Allocate is called during container creation so that
// the Device Plugin can run device specific operations and instruct Kubelet of the steps
// to make the Device available in the container.
func (s *ResourceServer) Allocate(ctx context.Context, req *AllocateRequest) (*AllocateResponse, error) {
	if s.Responder == nil {
		return nil, grpcstatus.Errorf(grpccodes.Internal, "unconfigured responder")
	}

	if s.AllocationMode == workercore.DeviceAllocationModeVisibility {
		return s.allocateVisibility(ctx, req)
	}

	ctrReq := req.GetContainerRequests()[0]

	allocatedDeviceIDs := ctrReq.GetDevicesIds()
	sort.Strings(allocatedDeviceIDs)
	allocatedResUnitsMap := make(map[Resource][]ResourceUnit)
	for i := range allocatedDeviceIDs {
		resUnit, err := ConvertResourceUnitFromDeviceIds(allocatedDeviceIDs[i])
		if err != nil {
			s.Logger.Error(err, "convert device id", "device id", allocatedDeviceIDs[i])
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "invalid device id %q: %v", allocatedDeviceIDs[i], err)
		}
		allocatedResUnitsMap[resUnit.Resource] = append(allocatedResUnitsMap[resUnit.Resource], resUnit)
	}

	resName := s.GetResourceName()
	resQuantity := *resource.NewQuantity(int64(len(ctrReq.GetDevicesIds())), resource.DecimalSI)

	// Identify the pod, enforce the cross-mode invariant, and reserve the cards under the node
	// allocate mutex (see DevicesReconciler.allocateMutex). Holding it across the whole section
	// makes a concurrent Allocate batch (e.g. Kueue admitting identical Pods together) resolve to
	// DISTINCT pods — getAllocatingPod skips pods a prior Allocate already reserved, and that
	// reservation is written here before the next Allocate reads it — and stops an opposite-mode
	// Allocate for the same card from interleaving between the check and the reservation (TOCTOU).
	// Its reads (getAllocatingPod, getDevices) are served from the informer cache and it makes no
	// durable writes; crucially the mutex is NOT held across the annotation patch, which runs after
	// the mutex is released.
	var (
		pod                 *core.Pod
		ctr                 *core.Container
		devs                *workercore.Devices
		allocatedStatus     workercore.DevicesStatus
		allocatedAllocation = make(map[Resource]int32)
	)
	if err := func() error {
		s.Reconciler.allocateMutex.Lock()
		defer s.Reconciler.allocateMutex.Unlock()

		var err error
		pod, ctr, err = s.Reconciler.getAllocatingPod(ctx, resName, resQuantity, true)
		if err != nil {
			s.Logger.Error(err, "get allocating pod for allocation")
			return grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for allocation: %v", err)
		}

		devs, err = s.Reconciler.getDevices(ctx)
		if err != nil {
			s.Logger.Error(err, "get devices for allocation")
			return grpcstatus.Errorf(grpccodes.Internal, "get devices for allocation: %v", err)
		}

		// For a sliced allocation each token placed on a card commits the container's per-card
		// ".sliced.units" — the normalized units the Pod webhook folds the memory budget into.
		// Recording that real cost (not the loose injection-token count) keeps the per-card ledger
		// honest, so the node-devices admission check refuses a card whose committed units would
		// exceed capacity and the InstanceType sliced view reports the true remaining. Fall back to
		// the token count when the units request is absent (a sliced Pod the webhook did not shape).
		slicedUnitsPerToken := int64(1)
		if s.AllocationMode == workercore.DeviceAllocationModeSliced {
			unitsResName := nodefeature.GetAcceleratableSlicedUnitsResourceName(s.Manufacturer)
			if q, ok := ctr.Resources.Limits[unitsResName]; ok && q.Value() > 0 {
				slicedUnitsPerToken = q.Value()
			}
		}

		// Enforce the per-card exclusive/shared/sliced cross-mode invariant at the authoritative
		// on-node gate: refuse a card kubelet assigned that another mode already holds (per the
		// ledger Status OR the in-process reservation), so an exclusive tenant truly owns its card
		// on every path, Kueue or raw. Free (None) and same-mode (e.g. sliced-on-sliced) cards pass.
		for res := range allocatedResUnitsMap {
			if held, mode := s.cardHeldInOtherMode(devs, res); held {
				s.Logger.Error(nil, "cross-mode allocation rejected",
					"pod", kubemeta.GetNamespacedNameKey(pod), "card", res.String(),
					"heldMode", mode.String(), "requestedMode", s.AllocationMode.String())
				return grpcstatus.Errorf(grpccodes.FailedPrecondition,
					"card %s is held in %s mode, cannot allocate it in %s mode", res, mode, s.AllocationMode)
			}
		}

		for i := range devs.Spec.Groups {
			devsGroup := &devs.Spec.Groups[i]
			if devsGroup.Manufacturer != s.Manufacturer {
				continue
			}
			for j := range devsGroup.Accelerators {
				devsAccelerator := &devsGroup.Accelerators[j]
				res := Resource{
					Group:  devsGroup.ID,
					Device: devsAccelerator.ID,
				}
				resUnits, existed := allocatedResUnitsMap[res]
				if !existed {
					continue
				}
				var allocated int32
				switch s.AllocationMode {
				default:
					allocated = nodefeature.ResourceMaxUnits // a whole card
				case workercore.DeviceAllocationModeShared:
					allocated = nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize // units per owner
				case workercore.DeviceAllocationModeSliced:
					// Real per-card units this container's slices commit on the card, so the
					// ledger reflects capacity (not the loose injection-token count); the
					// node-devices admission check then refuses a card whose committed units
					// would exceed capacity, and the InstanceType sliced view reports true remaining.
					allocated = int32(min(slicedUnitsPerToken*int64(len(resUnits)), int64(nodefeature.ResourceMaxUnits)))
				}
				if allocated > nodefeature.ResourceMaxUnits {
					allocated = nodefeature.ResourceMaxUnits
				}
				if len(allocatedStatus.Groups) == 0 || allocatedStatus.Groups[len(allocatedStatus.Groups)-1].ID != devsGroup.ID {
					allocatedStatus.Groups = append(allocatedStatus.Groups, workercore.DevicesAllocationGroup{
						ID:           devsGroup.ID,
						Manufacturer: devsGroup.Manufacturer,
					})
				}
				devsStatusGroup := &allocatedStatus.Groups[len(allocatedStatus.Groups)-1]
				devsStatusGroup.Accelerators = append(devsStatusGroup.Accelerators, workercore.AcceleratorAllocation{
					ID:        devsAccelerator.ID,
					Index:     devsAccelerator.Index,
					Mode:      s.AllocationMode,
					Allocated: allocated,
				})
				allocatedAllocation[res] = allocated
			}
		}

		// Reserve the cards in-process before releasing the mutex: the card is taken the instant
		// the check passes, so the next serialized Allocate observes it (cross-mode check and
		// getAllocatingPod's skip-reserved), and the SSH sidecar's visibility Allocate can
		// co-allocate the same physical device without racing the annotation's cache propagation.
		s.Reconciler.reserveDevices(pod.UID, allocatedStatus)
		return nil
	}(); err != nil {
		return nil, err
	}

	// Persist the durable allocation annotation outside the mutex (I/O). On failure roll back the
	// reservation written above: with the annotation absent, the Pod-delete watch (gated on it)
	// would never enqueue a prune, so the card would stay stranded for the opposite mode. Kubelet
	// does not start the container on this error, so freeing the reservation now is safe and keeps
	// the release counting honest.
	if err := s.Reconciler.patchAllocatingPod(ctx, pod, allocatedStatus); err != nil {
		s.Reconciler.releaseReservation(pod.UID)
		s.Logger.Error(err, "patch allocating pod for allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "patch allocating pod for allocation: %v", err)
	}

	ctrResp, err := s.Responder.GetContainerAllocateResponse(ctx, pod, ctr, devs, allocatedAllocation)
	if err != nil {
		s.Logger.Error(err, "get container allocate response")
		return nil, err
	}

	resp := &AllocateResponse{
		ContainerResponses: []*ContainerAllocateResponse{ctrResp},
	}
	s.Logger.Info("allocate response",
		"pod", kubemeta.GetNamespacedNameKey(pod),
		"response", resp)
	return resp, nil
}

// allocateVisibility serves the SSH sidecar's visibility request: it does not select a
// device but reuses the physical device(s) the workload container (main) was already
// allocated, recorded in the in-process reservation. It returns only the vendor
// visible-devices env (via the Responder), consuming no ledger units and writing no
// allocation status. It fails closed when no reservation exists, rather than emitting an
// empty visible-devices env a runtime could interpret as "all devices".
func (s *ResourceServer) allocateVisibility(ctx context.Context, req *AllocateRequest) (*AllocateResponse, error) {
	ctrReq := req.GetContainerRequests()[0]

	resName := s.GetResourceName()
	resQuantity := *resource.NewQuantity(int64(len(ctrReq.GetDevicesIds())), resource.DecimalSI)
	// Do not skip reserved pods: visibility re-finds its own pod (whose workload Allocate already
	// recorded the reservation) to co-allocate the same physical device to the sidecar.
	pod, ctr, err := s.Reconciler.getAllocatingPod(ctx, resName, resQuantity, false)
	if err != nil {
		s.Logger.Error(err, "get allocating pod for visibility allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for visibility allocation: %v", err)
	}

	reserved, ok := s.Reconciler.reservedDevices(pod.UID)
	if !ok || len(reserved.Groups) == 0 {
		// Fail closed: the workload container's allocation has not been recorded yet (or was
		// pruned). Returning an error rejects this admission rather than emitting an empty
		// visible-devices env a runtime could read as "all devices". Because main is always
		// allocated before sshd in the same pod, this path should not occur in practice; if it
		// ever does, recovery is the controller recreating the Pod.
		err = fmt.Errorf("no reserved devices for pod %s; refusing to grant visibility", kubemeta.GetNamespacedNameKey(pod))
		s.Logger.Error(err, "visibility allocation without reservation")
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "%v", err)
	}

	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		s.Logger.Error(err, "get devices for visibility allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get devices for visibility allocation: %v", err)
	}

	// Devices currently present on the node, so a stale reservation (a reserved device no
	// longer in the inventory) fails closed below instead of yielding an empty
	// visible-devices env from the Responder.
	present := make(map[Resource]struct{})
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			present[Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}] = struct{}{}
		}
	}

	// Reuse main's reserved devices as the "allocated" set so the Responder emits the vendor
	// visible-devices env for exactly those devices; no HAMi mounts/limit, since this server
	// is not the sliced mode. No patchAllocatingPod/reserveDevices: visibility consumes no
	// ledger units and holds no reservation of its own.
	reservedCount := 0
	allocated := make(map[Resource]int32)
	for i := range reserved.Groups {
		grp := &reserved.Groups[i]
		for j := range grp.Accelerators {
			reservedCount++
			res := Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}
			if _, ok := present[res]; !ok {
				continue
			}
			allocated[res] = grp.Accelerators[j].Allocated
		}
	}
	// Fail closed unless every reserved device is still present AND the reservation matches the
	// visibility request exactly. A partial/stale reservation (a reserved device gone from the
	// inventory) or a request that does not match the reserved device count would otherwise grant
	// the sidecar a different device set than the workload holds; refuse rather than emit a
	// degraded (or empty) visible-devices env a runtime could misread.
	requestCount := len(ctrReq.GetDevicesIds())
	if len(allocated) != reservedCount || reservedCount != requestCount {
		err = fmt.Errorf("visibility reservation for pod %s does not match the request "+
			"(reserved=%d, present=%d, requested=%d); refusing to grant visibility",
			kubemeta.GetNamespacedNameKey(pod), reservedCount, len(allocated), requestCount)
		s.Logger.Error(err, "visibility allocation with mismatched or stale reservation")
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "%v", err)
	}

	ctrResp, err := s.Responder.GetContainerAllocateResponse(ctx, pod, ctr, devs, allocated)
	if err != nil {
		s.Logger.Error(err, "get container visibility allocate response")
		return nil, err
	}

	resp := &AllocateResponse{
		ContainerResponses: []*ContainerAllocateResponse{ctrResp},
	}
	s.Logger.Info("visibility allocate response",
		"pod", kubemeta.GetNamespacedNameKey(pod),
		"response", resp)
	return resp, nil
}

func (s *ResourceServer) Start(ctx context.Context, kubeSocket string) error {
	if s.server != nil {
		s.Logger.Error(nil, "server already started")
		<-ctx.Done()
		return ctx.Err()
	}

	socket := filepath.Join(filepath.Dir(kubeSocket),
		stringx.Join(".", s.Manufacturer, strings.ToLower(s.AllocationMode.String()), "sock"))
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		s.Logger.Error(err, "clean up socket", "socket", socket)
		return err
	}
	defer func() {
		_ = os.Remove(socket)
	}()

	var lis net.Listener
	{
		var err error
		lis, err = net.Listen("unix", socket)
		if err != nil {
			s.Logger.Error(err, "listen on socket", "socket", socket)
			return err
		}
	}
	defer osx.Close(lis)

	s.server = grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
	)
	deviceplugin.RegisterDevicePluginServer(s.server, s)

	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		s.Logger.Info("serving on socket", "socket", socket)
		return s.server.Serve(lis)
	})
	gp.Go(func(ctx context.Context) error {
		err := waitx.PollUntilContextTimeout(ctx, time.Second, 10*time.Second, true, func(ctx context.Context) error {
			if len(s.server.GetServiceInfo()) == 0 {
				return errors.New("gRPC server is not ready")
			}
			return nil
		})
		if err != nil {
			s.Logger.Error(err, "wait for server to be ready")
			return err
		}
		s.Logger.Info("registering to kubelet")
		return s.register(ctx, kubeSocket, socket)
	})
	return gp.Wait()
}

func (s *ResourceServer) register(ctx context.Context, kubeSocket, socket string) error {
	regOpts, err := s.GetDevicePluginOptions(ctx, &Empty{})
	if err != nil {
		s.Logger.Error(err, "get device plugin options")
		return err
	}

	cli, err := grpc.NewClient("unix://"+kubeSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		s.Logger.Error(err, "dial kubelet socket", "socket", kubeSocket)
		return err
	}
	defer osx.Close(cli)

	regCli := deviceplugin.NewRegistrationClient(cli)
	regReq := &deviceplugin.RegisterRequest{
		Version:      deviceplugin.Version,
		Endpoint:     filepath.Base(socket),
		ResourceName: string(s.GetResourceName()),
		Options:      regOpts,
	}
	if _, err = regCli.Register(ctx, regReq); err != nil {
		s.Logger.Error(err, "register to kubelet", "socket", kubeSocket)
		return err
	}

	return nil
}

func (s *ResourceServer) Stop() {
	if s.server == nil {
		s.Logger.Errorf(nil, "server not started")
		return
	}

	s.Logger.Info("stopping")
	s.server.Stop()
	s.server = nil
}
