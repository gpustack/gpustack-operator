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
	"gpustack.ai/gpustack/pkg/device"
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
	// The partition pool's health is a node-level count over a stable set of IDs rather than a
	// per-card verdict, so it is published on its own.
	if s.AllocationMode == workercore.DeviceAllocationModePartitioned {
		return s.getPartitionListAndWatchResponse(ctx, devs)
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
			// This server's family draws tokens only from the card population that can
			// physically serve it, and sizes its pool from that card's own capability:
			// logical slicing and hardware partitioning are exclusive card states, and a
			// partitioned card is no longer available as a whole card. Scope, not health, is
			// the mechanism here — the populations are physically exclusive, so a card
			// skipped this way could never become servable while it stays in that state.
			// Visibility is exempt and advertised everywhere: the SSH sidecar must
			// co-allocate the very card its workload holds, whatever state that card is in.
			var poolSize int32
			switch s.AllocationMode {
			case workercore.DeviceAllocationModeExclusive, workercore.DeviceAllocationModeShared:
				if !device.IsWholeCardCapable(devAccelerator.Status) {
					continue
				}
			case workercore.DeviceAllocationModeSliced:
				if !device.IsLogicallySliceable(devAccelerator.Status) {
					continue
				}
				poolSize = devAccelerator.Status.LogicalSliced.Count
			}
			// Hardware health alone does not protect a card held in another allocation mode:
			// kubelet picks tokens freely (GetPreferredAllocation does run, but its answer is
			// only a hint kubelet is free to ignore), so a held card still
			// advertised as Healthy WILL eventually be handed to an opposite-mode pod, whose
			// Allocate then fails with a permanent UnexpectedAdmissionError. Keep the held
			// card's tokens advertised (removing them would strand kubelet's checkpointed
			// allocations on re-registration) but report them Unhealthy — kubelet never assigns
			// Unhealthy devices to new pods, while the holding pod's existing allocation is
			// unaffected. The hold is read from the ledger Status AND the in-process
			// reservation, so a just-reserved card is withheld in the same ListAndWatch cycle.
			// The Visibility server is exempt: the SSH sidecar must co-allocate the very card
			// its workload holds, whatever mode that hold is.
			health := deviceplugin.Healthy
			if devAccelerator.Status.Unhealthy {
				health = deviceplugin.Unhealthy
			} else if s.AllocationMode != workercore.DeviceAllocationModeVisibility {
				if held, _ := s.cardHeldInOtherMode(devs, res); held {
					health = deviceplugin.Unhealthy
				}
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
			ids := res.GetDeviceIds(s.AllocationMode, poolSize)
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

// getPartitionListAndWatchResponse publishes the partition pool. Its tokens are a fungible
// node-level count — Allocate chooses the card (F2) — so health answers "how many more
// instances can this node host", never "which card is free". That also means a partition token
// carries no NUMA hint: it names no card, so a hint would tell the TopologyManager something
// the token cannot honor.
//
// Every partitioned card advertises its full ceiling of IDs, always. IDs are never removed:
// removing one strands the kubelet's checkpointed allocation for whatever container held it.
// What varies is how many are Healthy — summed over the node's partitioned cards,
// allocated + remaining, where remaining is the largest number of further instances a card can
// still host over its profiles. The allocated term is required: the kubelet publishes the
// healthy count as allocatable and the scheduler then subtracts the requests of the Pods
// already on the node, so a bare remaining would lose one slot per live instance. A node with
// no room left therefore advertises exactly its live instance count, which the scheduler
// reduces to a free view of zero.
//
// The healthy SET is part of the contract, not just its size. The kubelet checkpoints the exact
// IDs it offered a container and, on any later allocation for it, refuses to proceed unless
// every one is still healthy — the check runs before the "no new devices needed" shortcut, and
// sanitizeNodeAllocatable does not rescue it. So every ID a live allocation holds stays Healthy
// for that allocation's life, and the free room is granted as a stable prefix of the rest: two
// cycles over an unchanged ledger publish the identical set.
func (s *ResourceServer) getPartitionListAndWatchResponse(
	ctx context.Context, devs *workercore.Devices,
) (*ListAndWatchResponse, error) {
	held, err := s.Reconciler.liveDeviceIDs(ctx)
	if err != nil {
		return nil, err
	}

	var (
		ids  []string
		room int
	)
	for i := range devs.Spec.Groups {
		devGroup := &devs.Spec.Groups[i]
		if devGroup.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range devGroup.Accelerators {
			devAccelerator := &devGroup.Accelerators[j]
			if !device.IsPartitioned(devAccelerator.Status) {
				continue
			}
			res := Resource{Group: devGroup.ID, Device: devAccelerator.ID}
			ceiling := devAccelerator.Status.PhysicalSliced.Count
			ids = append(ids, res.GetDeviceIds(s.AllocationMode, ceiling)...)
			if devAccelerator.Status.Unhealthy {
				// A broken card keeps its IDs — a live allocation may still hold one — but
				// offers no room for a new instance.
				continue
			}
			room += partitionRoomOf(devs, res, ceiling)
		}
	}

	// Held IDs are Healthy whatever the room says, so the free grant comes out of what is left
	// after them; counting them first is what makes the two rules compose instead of compete.
	granted := 0
	for i := range ids {
		if held.Has(ids[i]) {
			granted++
		}
	}
	resp := &deviceplugin.ListAndWatchResponse{Devices: make([]*deviceplugin.Device, 0, len(ids))}
	for i := range ids {
		health := deviceplugin.Unhealthy
		switch {
		case held.Has(ids[i]):
			health = deviceplugin.Healthy
		case granted < room:
			health = deviceplugin.Healthy
			granted++
		}
		resp.Devices = append(resp.Devices, &deviceplugin.Device{ID: ids[i], Health: health})
	}
	return resp, nil
}

// partitionRoomOf returns how many instances a card currently accounts for: the ones it already
// carries plus the largest number of further ones it can still host over its profiles, per the
// placement-aware ledger. A card whose ledger has not been published yet — a fresh node, or a
// device manager still rolling out — falls back to its static ceiling, so the node advertises
// its full room rather than none.
func partitionRoomOf(devs *workercore.Devices, res Resource, ceiling int32) int {
	for i := range devs.Status.Groups {
		grp := &devs.Status.Groups[i]
		if grp.ID != res.Group {
			continue
		}
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if acc.ID != res.Device {
				continue
			}
			if len(acc.AllocatedProfiles) == 0 && len(acc.RemainingProfiles) == 0 {
				return int(ceiling)
			}
			var allocated, remaining int32
			for k := range acc.AllocatedProfiles {
				allocated += acc.AllocatedProfiles[k].Count
			}
			for k := range acc.RemainingProfiles {
				remaining = max(remaining, acc.RemainingProfiles[k].Count)
			}
			return int(allocated + remaining)
		}
	}
	return int(ceiling)
}

// GetPreferredAllocation returns a preferred set of devices to allocate from a list of available ones.
// The resulting preferred allocation is not guaranteed to be the allocation ultimately performed by the Device Manager.
// It is only designed to help the Device Manager make a more informed allocation decision when possible.
func (s *ResourceServer) GetPreferredAllocation(ctx context.Context, req *PreferredAllocationRequest) (*PreferredAllocationResponse, error) {
	// The visibility and partition token pools are flat and interchangeable — a visibility
	// token resolves to the pod's already-reserved device, and a partition token to whatever
	// card Allocate chooses (F2) — so neither has a preference to express. Return an empty
	// response (kubelet picks freely) rather than run the sliced per-card bin-fit, which
	// assumes tokens map to the real devices they name.
	if s.AllocationMode == workercore.DeviceAllocationModeVisibility ||
		s.AllocationMode == workercore.DeviceAllocationModePartitioned {
		return &PreferredAllocationResponse{
			ContainerResponses: []*ContainerPreferredAllocationResponse{{}},
		}, nil
	}

	ctrReq := req.GetContainerRequests()[0]

	resName := s.GetResourceName()
	resQuantity := *resource.NewQuantity(int64(ctrReq.GetAllocationSize()), resource.DecimalSI)
	// Advisory path: do not skip reserved containers (kubelet may ignore this hint, and the
	// authoritative identification with skip-reserved happens in Allocate).
	pod, ctr, err := s.Reconciler.getAllocatingPodWithRetry(ctx, _AllocationMatch{
		ResourceName: resName,
		Quantity:     resQuantity,
	})
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

// statusRemainingOf returns the units the Devices ledger still reports free on a physical card.
// A card absent from the Status is treated as untouched, i.e. a whole card free.
func statusRemainingOf(devs *workercore.Devices, res Resource) int32 {
	for i := range devs.Status.Groups {
		grp := &devs.Status.Groups[i]
		if grp.ID != res.Group {
			continue
		}
		for j := range grp.Accelerators {
			if grp.Accelerators[j].ID == res.Device {
				return grp.Accelerators[j].Remaining
			}
		}
	}
	return nodefeature.ResourceMaxUnits
}

// candidateFeasible returns the test that tells two otherwise identical Allocate candidates
// apart. The RPC carries only a resource name and a device-ID count, so two pending containers
// asking for the same family are indistinguishable to it even when they demand very different
// things — and picking the wrong one actuates the wrong request. This narrows the choice using
// the one thing the plugin can check: whether the candidate's demand can still be served by
// the node, per the ledger.
//
// A logical slice's demand is its per-card ".sliced.units", the normalized budget the Pod
// webhook folded its memory request into, tested against the cards kubelet offered. A
// partition's demand is its profile geometry, tested against the whole node — the offered
// tokens name no card the allocation will use, so only the node's free placements can rule a
// candidate out. The exclusive and shared families carry no such dimension — every candidate
// for them really is interchangeable — so they get no test.
func (s *ResourceServer) candidateFeasible(
	devs *workercore.Devices,
	cards map[Resource][]ResourceUnit,
	occupied map[Resource][]workercore.AcceleratorPhysicalPlacement,
) func(*core.Pod, *core.Container) bool {
	switch s.AllocationMode {
	case workercore.DeviceAllocationModeSliced:
		unitsResName := nodefeature.GetAcceleratableSlicedUnitsResourceName(s.Manufacturer)
		return func(_ *core.Pod, ctr *core.Container) bool {
			q, ok := ctr.Resources.Limits[unitsResName]
			if !ok || q.Value() <= 0 {
				// A slice the Pod webhook did not shape carries no per-card budget, so there
				// is nothing to test it against.
				return true
			}
			units := int32(min(q.Value(), int64(nodefeature.ResourceMaxUnits)))
			for res := range cards {
				if statusRemainingOf(devs, res) < units {
					return false
				}
			}
			return true
		}
	case workercore.DeviceAllocationModePartitioned:
		return func(_ *core.Pod, ctr *core.Container) bool {
			profile, ok := partitionProfileOf(ctr)
			if !ok {
				// A partition request the Pod webhook did not shape names no geometry, so
				// there is nothing to test it against.
				return true
			}
			candidates, _ := s.partitionCandidates(devs, occupied, profile)
			_, placeable := device.SelectPartitionPlacements(candidates, 1)
			return placeable
		}
	}
	return nil
}

// partitionCandidates builds the placement decision's input from the live state: every
// partitioned card of this server's manufacturer that offers the profile, with the profile's
// legal placements from the card's detect-time capability and the intervals already taken.
// The second return maps a candidate ID back to its resource, since the selector deals only
// in opaque IDs. A card offering no placement for the profile is left out entirely, so the
// selector never has to know why.
func (s *ResourceServer) partitionCandidates(
	devs *workercore.Devices,
	occupied map[Resource][]workercore.AcceleratorPhysicalPlacement,
	profile string,
) ([]device.PartitionCandidate, map[string]Resource) {
	var candidates []device.PartitionCandidate
	byID := make(map[string]Resource)
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		if grp.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if !device.IsPartitioned(acc.Status) || acc.Status.Unhealthy {
				continue
			}
			possible := profilePlacements(acc.Status.PhysicalSliced.Profiles, profile)
			if len(possible) == 0 {
				continue
			}
			res := Resource{Group: grp.ID, Device: acc.ID}
			// A card another mode holds is not a candidate: the cross-mode invariant would
			// reject the allocation right after the decision.
			if held, _ := s.cardHeldInOtherMode(devs, res); held {
				continue
			}
			byID[res.String()] = res
			candidates = append(candidates, device.PartitionCandidate{
				ID:       res.String(),
				Possible: possible,
				Occupied: occupied[res],
			})
		}
	}
	return candidates, byID
}

// profilePlacements returns a card's legal placements for one partition profile, or nil when
// the card does not offer it (or has not published its placement ledger yet).
func profilePlacements(
	profiles []workercore.AcceleratorPhysicalSlicedProfile, profile string,
) []workercore.AcceleratorPhysicalPlacement {
	for i := range profiles {
		if profiles[i].Name == profile {
			return profiles[i].Placements
		}
	}
	return nil
}

// unitsResourceName returns the fine-grained counting key of this server's family — the one
// the Pod webhook folds a request's memory budget into — or "" for a family that has none.
func (s *ResourceServer) unitsResourceName() core.ResourceName {
	switch s.AllocationMode {
	case workercore.DeviceAllocationModeSliced:
		return nodefeature.GetAcceleratableSlicedUnitsResourceName(s.Manufacturer)
	case workercore.DeviceAllocationModePartitioned:
		return nodefeature.GetAcceleratablePartitionedUnitsResourceName(s.Manufacturer)
	}
	return ""
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
		profile             string
		allocatedStatus     workercore.DevicesStatus
		allocatedAllocation = make(map[Resource]int32)
	)
	if err := func() error {
		s.Reconciler.allocateMutex.Lock()
		defer s.Reconciler.allocateMutex.Unlock()

		var err error
		// The ledger is read first: it is what tells two otherwise identical candidates apart
		// in the feasibility test below.
		devs, err = s.Reconciler.getDevices(ctx)
		if err != nil {
			s.Logger.Error(err, "get devices for allocation")
			return grpcstatus.Errorf(grpccodes.Internal, "get devices for allocation: %v", err)
		}

		// The Partitioned family decides the placement itself, so it needs the node's live
		// per-card occupancy — the annotations every live allocation carries, unioned with the
		// selections in-flight allocations have already published. Reading it once here keeps
		// the candidate test and the placement decision on one snapshot.
		var occupied map[Resource][]workercore.AcceleratorPhysicalPlacement
		if s.AllocationMode == workercore.DeviceAllocationModePartitioned {
			occupied, err = s.occupiedPhysicalPlacements(ctx)
			if err != nil {
				s.Logger.Error(err, "get occupied placements for allocation")
				return grpcstatus.Errorf(grpccodes.Internal, "get occupied placements for allocation: %v", err)
			}
		}

		pod, ctr, err = s.Reconciler.getAllocatingPod(ctx, _AllocationMatch{
			ResourceName: resName,
			Quantity:     resQuantity,
			SkipReserved: true,
			Feasible:     s.candidateFeasible(devs, allocatedResUnitsMap, occupied),
		})
		if err != nil {
			s.Logger.Error(err, "get allocating pod for allocation")
			return grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for allocation: %v", err)
		}

		// Each token placed on a card commits the container's per-card counting units — the
		// normalized value the Pod webhook folds the memory budget into (".sliced.units" for a
		// logical slice, ".partitioned.units" for a partition). Recording that real cost (not
		// the loose token count) keeps the per-card ledger honest, so the node-devices admission
		// check refuses a card whose committed units would exceed capacity and the InstanceType
		// views report the true remaining. Fall back to the token count when the units request
		// is absent (a Pod the webhook did not shape).
		unitsPerToken := int64(1)
		if unitsResName := s.unitsResourceName(); unitsResName != "" {
			if q, ok := ctr.Resources.Limits[unitsResName]; ok && q.Value() > 0 {
				unitsPerToken = q.Value()
			}
		}

		// Which cards this allocation lands on. The three card-bound families take the cards
		// kubelet's tokens name. Partitioned reads the tokens as a quantity and chooses the
		// cards itself, against the live occupancy: kubelet cannot know which card can host a
		// given geometry, and a wrong pick is the one placement error the plugin can repair
		// rather than reject. A rejection here therefore means the node has no room at all.
		var (
			placements map[Resource][]workercore.AcceleratorPhysicalPlacement
			cardTokens = allocatedResUnitsMap
		)
		if s.AllocationMode == workercore.DeviceAllocationModePartitioned {
			cardTokens = nil // decided below, from the prior allocation or a fresh placement
			var ok bool
			if profile, ok = partitionProfileOf(ctr); !ok {
				s.Logger.Error(nil, "partition allocation without a profile",
					"pod", kubemeta.GetNamespacedNameKey(pod), "container", ctr.Name)
				return grpcstatus.Errorf(grpccodes.FailedPrecondition,
					"container %q of pod %s requests %s but names no partition profile",
					ctr.Name, kubemeta.GetNamespacedNameKey(pod), resName)
			}
			// A retried Allocate must land back on the card it already used. The kubelet
			// re-runs Allocate for a container whose checkpoint it lost — a restart while the
			// container was stopped — and by then this container's own placement is part of
			// the node's occupancy. Deciding afresh would read it as somebody else's: a
			// whole-card profile would report the node exhausted, and a node with a free
			// sibling would place on THAT card, bypassing the vendor's per-(pod, container,
			// card) reuse marker and carving a second instance. The card-bound families get
			// this for free, since kubelet re-offers the tokens it checkpointed.
			if prior, held := s.priorPartitionAllocation(pod, ctr.Name); held {
				cardTokens, placements = priorPartitionTokens(prior)
			}
			if cardTokens == nil {
				candidates, byID := s.partitionCandidates(devs, occupied, profile)
				selections, placed := device.SelectPartitionPlacements(candidates, len(allocatedDeviceIDs))
				if !placed {
					s.Logger.Error(nil, "no card can host the requested partition profile",
						"pod", kubemeta.GetNamespacedNameKey(pod), "profile", profile,
						"instances", len(allocatedDeviceIDs))
					return grpcstatus.Errorf(grpccodes.ResourceExhausted,
						"no card on this node can host %d instance(s) of partition profile %q",
						len(allocatedDeviceIDs), profile)
				}
				cardTokens, placements = partitionTokens(selections, byID)
			}
		}

		// Enforce the per-card cross-mode invariant at the authoritative on-node gate: refuse a
		// card another mode already holds (per the ledger Status OR the in-process reservation),
		// so an exclusive tenant truly owns its card on every path, Kueue or raw. Free (None) and
		// same-mode (e.g. sliced-on-sliced) cards pass. The partition selector already skipped
		// held cards, so this re-checks its own choice rather than kubelet's.
		for res := range cardTokens {
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
				resUnits, existed := cardTokens[res]
				if !existed {
					continue
				}
				var allocated int32
				switch s.AllocationMode {
				default:
					allocated = nodefeature.ResourceMaxUnits // a whole card
				case workercore.DeviceAllocationModeShared:
					allocated = nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize // units per owner
				case workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModePartitioned:
					// Real per-card units this container commits on the card, so the ledger
					// reflects capacity rather than the loose token count. A partition needs its
					// own branch here for the same reason a slice does, and for one more: the
					// default charges a WHOLE card, which would make a single small instance look
					// like it owned the card and hide the rest of its geometry from every
					// consumer of the scalar remaining.
					allocated = int32(min(unitsPerToken*int64(len(resUnits)), int64(nodefeature.ResourceMaxUnits)))
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

		// Publish the partition selection — card, profile and the intervals it intends to
		// occupy — into the allocation BEFORE the reservation below, so the next serialized
		// Allocate decides against a card that is already spoken for rather than one that
		// merely has not been carved yet. Actuation happens after the mutex is released, and
		// overwrites the intent with the interval the hardware actually gave.
		if len(placements) > 0 {
			applyPhysicalPlacements(&allocatedStatus, profile, placements)
		}

		// Reserve the cards in-process before releasing the mutex: the card is taken the instant
		// the check passes, so the next serialized Allocate observes it (cross-mode check and
		// getAllocatingPod's skip-reserved), and the SSH sidecar's visibility Allocate can
		// co-allocate the same physical device without racing the annotation's cache propagation.
		// The offered device IDs ride along so ListAndWatch can keep advertising exactly them
		// Healthy for as long as this allocation lives.
		s.Reconciler.reserveDevices(pod.UID, ctr.Name, allocatedStatus, allocatedDeviceIDs)
		return nil
	}(); err != nil {
		return nil, err
	}

	// Materialize the hardware partition(s) via the responder's actuator — under its own
	// per-card lock — and record each chosen placement upward in allocatedStatus BEFORE the
	// annotation patch, so the reconciler's ledger can reconstruct the card's occupied set. The
	// actuator may land on a different interval of the selected card than the one published as
	// the intent (it can adopt an instance the hardware already carries), so the reservation is
	// re-published with what actually happened. The node mutex is already released; the
	// per-card lock serializes only same-card creates, so sibling cards proceed in parallel. A
	// responder that cannot actuate fails the allocation rather than starting a container with
	// no partition.
	var physical *PhysicalSlicedAllocation
	if profile != "" {
		actuator, canActuate := s.Responder.(PhysicalSlicedResponder)
		if !canActuate {
			s.Reconciler.releaseReservation(pod.UID, ctr.Name)
			return nil, grpcstatus.Errorf(grpccodes.Internal,
				"responder cannot actuate partition profile %q", profile)
		}
		var actErr error
		physical, actErr = actuator.ActuatePhysicalSliced(ctx, pod, ctr, devs, allocatedAllocation, profile)
		if actErr != nil {
			s.Reconciler.releaseReservation(pod.UID, ctr.Name)
			s.Logger.Error(actErr, "actuate partition for allocation", "pod", kubemeta.GetNamespacedNameKey(pod))
			return nil, grpcstatus.Errorf(grpccodes.Internal, "actuate partition: %v", actErr)
		}
		applyPhysicalPlacements(&allocatedStatus, physical.Profile, physical.Placements)
		s.Reconciler.reserveDevices(pod.UID, ctr.Name, allocatedStatus, allocatedDeviceIDs)
	}

	// Persist the durable allocation annotation outside the mutex (I/O). On failure roll back the
	// reservation written above: with the annotation absent, the Pod-delete watch (gated on it)
	// would never enqueue a prune, so the card would stay stranded for the opposite mode. Kubelet
	// does not start the container on this error, so freeing the reservation now is safe and keeps
	// the release counting honest. A physical partition materialized above is also torn down, so
	// no half-owned instance persists past a failed patch.
	if err := s.Reconciler.patchAllocatingPod(ctx, pod, ctr.Name, allocatedStatus, allocatedDeviceIDs); err != nil {
		if physical != nil && physical.Rollback != nil {
			physical.Rollback()
		}
		s.Reconciler.releaseReservation(pod.UID, ctr.Name)
		s.Logger.Error(err, "patch allocating pod for allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "patch allocating pod for allocation: %v", err)
	}

	// A physical-slice allocation injects only the partition's visible-devices env the actuator
	// already assembled (no logical-slice artifacts), so it bypasses the logical-slice responder.
	var ctrResp *ContainerAllocateResponse
	if physical != nil {
		ctrResp = physical.Response
	} else {
		var err error
		ctrResp, err = s.Responder.GetContainerAllocateResponse(ctx, pod, ctr, devs, allocatedAllocation)
		if err != nil {
			s.Logger.Error(err, "get container allocate response")
			return nil, err
		}
	}

	resp := &AllocateResponse{
		ContainerResponses: []*ContainerAllocateResponse{ctrResp},
	}
	s.Logger.Info("allocate response",
		"pod", kubemeta.GetNamespacedNameKey(pod),
		"response", resp)
	return resp, nil
}

// partitionProfileOf returns the single "<base>.partitioned.<kind>-<profile>" profile a
// container requests, and whether it requests one. The Pod webhook guarantees at most one
// distinct profile per container, so the first match is authoritative.
func partitionProfileOf(ctr *core.Container) (string, bool) {
	for name := range ctr.Resources.Limits {
		if profile, ok := nodefeature.PartitionedProfileOf(name); ok {
			return profile, true
		}
	}
	return "", false
}

// priorPartitionAllocation returns the partition this container already holds: the in-process
// reservation first, then the durable annotation, which is the only record that survives a
// device-manager restart. It is what makes a retried Allocate idempotent.
func (s *ResourceServer) priorPartitionAllocation(
	pod *core.Pod, container string,
) (workercore.DevicesStatus, bool) {
	if reserved, ok := s.Reconciler.reservedDevices(pod.UID, container); ok && len(reserved.Groups) > 0 {
		return reserved, true
	}
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		// The annotation is unreadable; say so loudly and decide afresh, which is the only
		// thing left to do — but the operator needs to know the card may be double-carved.
		s.Logger.Error(err, "read the allocation annotation of an allocating pod; deciding a fresh placement",
			"pod", kubemeta.GetNamespacedNameKey(pod), "container", container)
		return workercore.DevicesStatus{}, false
	}
	if held, ok := allocations[container]; ok && len(held.Devices.Groups) > 0 {
		return held.Devices, true
	}
	return workercore.DevicesStatus{}, false
}

// priorPartitionTokens re-derives Allocate's two per-card maps from an allocation this
// container already holds, so a retry reuses the card and interval it recorded.
func priorPartitionTokens(
	prior workercore.DevicesStatus,
) (map[Resource][]ResourceUnit, map[Resource][]workercore.AcceleratorPhysicalPlacement) {
	tokens := make(map[Resource][]ResourceUnit)
	placements := make(map[Resource][]workercore.AcceleratorPhysicalPlacement)
	for i := range prior.Groups {
		grp := &prior.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			res := Resource{Group: grp.ID, Device: acc.ID}
			tokens[res] = append(tokens[res], ResourceUnit{Resource: res, Index: uint64(len(tokens[res]))})
			if len(acc.AllocatedPhysicalPlacements) > 0 {
				placements[res] = acc.AllocatedPhysicalPlacements
			}
		}
	}
	return tokens, placements
}

// partitionTokens turns the placement decision into the two per-card maps the rest of Allocate
// works in: one token per chosen card (the ledger cost is per token, and a partition request
// takes exactly one instance per card), and the intervals each card's instance intends to
// occupy.
func partitionTokens(
	selections []device.PartitionSelection, byID map[string]Resource,
) (map[Resource][]ResourceUnit, map[Resource][]workercore.AcceleratorPhysicalPlacement) {
	tokens := make(map[Resource][]ResourceUnit, len(selections))
	placements := make(map[Resource][]workercore.AcceleratorPhysicalPlacement, len(selections))
	for i := range selections {
		res := byID[selections[i].ID]
		tokens[res] = append(tokens[res], ResourceUnit{Resource: res, Index: uint64(len(tokens[res]))})
		placements[res] = append(placements[res], selections[i].Placement)
	}
	return tokens, placements
}

// occupiedPhysicalPlacements unions the two records of what a node's cards already carry: the
// placements live allocations recorded in their Pod annotations, and the selections in-flight
// allocations published into their reservation before the annotation could land. The union is
// what a placement decision must read — the annotation source alone lags by a cache round-trip,
// and the reservation alone forgets everything across a restart.
func (s *ResourceServer) occupiedPhysicalPlacements(
	ctx context.Context,
) (map[Resource][]workercore.AcceleratorPhysicalPlacement, error) {
	occupied, err := s.Reconciler.LivePhysicalOccupied(ctx)
	if err != nil {
		return nil, err
	}
	for res, placements := range s.Reconciler.reservedPhysicalOccupied() {
		occupied[res] = append(occupied[res], placements...)
	}
	return occupied, nil
}

// applyPhysicalPlacements records the actuator's chosen per-card placement into the allocation
// status accelerators, so the annotation patch carries the physical ledger's occupied source
// (AllocatedPhysicalProfile + AllocatedPhysicalPlacements, unioned by the reconciler).
func applyPhysicalPlacements(
	status *workercore.DevicesStatus,
	profile string,
	placements map[Resource][]workercore.AcceleratorPhysicalPlacement,
) {
	for i := range status.Groups {
		grp := &status.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			res := Resource{Group: grp.ID, Device: acc.ID}
			if p, ok := placements[res]; ok {
				acc.AllocatedPhysicalProfile = profile
				acc.AllocatedPhysicalPlacements = p
			}
		}
	}
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
	// Do not skip reserved containers: visibility re-finds its own pod (whose workload container
	// already recorded a reservation) to co-allocate the same physical device to the sidecar.
	pod, ctr, err := s.Reconciler.getAllocatingPod(ctx, _AllocationMatch{
		ResourceName: resName,
		Quantity:     resQuantity,
	})
	if err != nil {
		s.Logger.Error(err, "get allocating pod for visibility allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for visibility allocation: %v", err)
	}

	// Take the pod's accelerator reservation held by a container other than this sidecar: the
	// sidecar co-allocates what the workload holds, and its own visibility request reserves
	// nothing. Accelerator claims live in exactly one container group, so there is only one.
	reserved, owner, ok := s.Reconciler.reservedAcceleratorDevices(pod.UID, ctr.Name)
	if !ok || len(reserved.Groups) == 0 {
		// Fail closed: the workload container's allocation has not been recorded yet (or was
		// pruned). Returning an error rejects this admission rather than emitting an empty
		// visible-devices env a runtime could read as "all devices". Because main is always
		// allocated before sshd in the same pod, this path should not occur in practice; if it
		// ever does, recovery is the controller recreating the Pod.
		err = fmt.Errorf("no accelerator devices reserved for pod %s by a container other than %q "+
			"(owner=%q); refusing to grant visibility",
			kubemeta.GetNamespacedNameKey(pod), ctr.Name, owner)
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
	var reservedCards []string
	allocated := make(map[Resource]int32)
	for i := range reserved.Groups {
		grp := &reserved.Groups[i]
		for j := range grp.Accelerators {
			reservedCount++
			res := Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}
			reservedCards = append(reservedCards, res.String())
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
		err = fmt.Errorf("visibility reservation for pod %s held by container %q does not match the "+
			"request (cards=%v, reserved=%d, present=%d, requested=%d); refusing to grant visibility",
			kubemeta.GetNamespacedNameKey(pod), owner, reservedCards, reservedCount, len(allocated), requestCount)
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
