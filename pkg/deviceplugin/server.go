package deviceplugin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
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

// ResourceServer serves one (manufacturer, allocation mode) pair to kubelet over the device-plugin
// gRPC API: it advertises that family's tokens, answers the allocation hint, and performs the
// allocation. One is registered per mode, and every one of them shares a single DevicesReconciler —
// which is what lets the node-wide allocate mutex and the per-card cross-mode invariant reach
// across the modes rather than only within one. Responder carries the vendor-specific half of a
// response; Allocate refuses the request outright when it is unset.
type ResourceServer struct {
	deviceplugin.UnimplementedDevicePluginServer

	Logger         klog.Logger
	Manufacturer   string
	AllocationMode workercore.DeviceAllocationMode
	Reconciler     *DevicesReconciler
	Responder      ContainerAllocateResponder

	server *grpc.Server
}

// ResourceName returns the resource name to be registered to the Device Manager based on the kind and name.
func (s *ResourceServer) ResourceName() core.ResourceName {
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
		grp := &devs.Spec.Groups[i]
		if grp.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			res := Resource{
				Group:  grp.ID,
				Device: acc.ID,
			}
			resp.Devices = append(resp.Devices, s.advertiseCard(devs, acc, res)...)
		}
	}

	return resp, nil
}

// advertiseCard returns the tokens one card contributes to this family's ListAndWatch response, or
// nothing at all when the card lies outside the family's scope.
//
// This server's family draws tokens only from the card population that can physically serve it, and
// sizes its pool from that card's own capability: logical slicing and hardware partitioning are
// exclusive card states, and a partitioned card is no longer available as a whole card. Scope, not
// health, is the mechanism — the populations are physically exclusive, so a card skipped this way
// could never become servable while it stays in that state. Visibility is exempt and advertised
// everywhere: a visibility request must co-allocate the very card its owner holds, whatever state
// that card is in.
func (s *ResourceServer) advertiseCard(
	devs *workercore.Devices, acc *workercore.Accelerator, res Resource,
) []*deviceplugin.Device {
	var poolSize int32
	switch s.AllocationMode {
	case workercore.DeviceAllocationModeExclusive, workercore.DeviceAllocationModeShared:
		if !device.IsWholeCardCapable(acc.Status) {
			return nil
		}
	case workercore.DeviceAllocationModeSliced:
		if !device.IsLogicallySliceable(acc.Status) {
			return nil
		}
		poolSize = acc.Status.LogicalSliced.Count
	}

	// Hardware health alone does not protect a card held in another allocation mode: kubelet picks
	// tokens freely (GetPreferredAllocation does run, but its answer is only a hint kubelet is free
	// to ignore), so a held card still advertised as Healthy WILL eventually be handed to an
	// opposite-mode pod, whose Allocate then fails with a permanent UnexpectedAdmissionError. Keep
	// the held card's tokens advertised — removing them would strand kubelet's checkpointed
	// allocations on re-registration — but report them Unhealthy, which kubelet never assigns to new
	// pods while leaving the holding pod's existing allocation unaffected. The hold is read from the
	// ledger Status AND the in-process reservation, so a just-reserved card is withheld in the same
	// ListAndWatch cycle. The Visibility server is exempt: a visibility request must co-allocate the
	// very card its owner holds, whatever mode that hold is.
	health := deviceplugin.Healthy
	if acc.Status.Unhealthy {
		health = deviceplugin.Unhealthy
	} else if s.AllocationMode != workercore.DeviceAllocationModeVisibility {
		if held, _ := s.cardHeldInOtherMode(devs, res); held {
			health = deviceplugin.Unhealthy
		}
	}

	var topology *deviceplugin.TopologyInfo
	if numa := binding.StrRangeToList(acc.Topology.NumaAffinity); len(numa) > 0 {
		topology = &deviceplugin.TopologyInfo{
			Nodes: slicex.Transform(numa, func(n int) *deviceplugin.NUMANode {
				return &deviceplugin.NUMANode{
					ID: int64(n),
				}
			}),
		}
	}

	ids := res.DeviceIDs(s.AllocationMode, poolSize)
	devices := make([]*deviceplugin.Device, 0, len(ids))
	for k := range ids {
		devices = append(devices,
			&deviceplugin.Device{
				ID:       ids[k],
				Health:   health,
				Topology: topology,
			},
		)
	}

	return devices
}

// getPartitionListAndWatchResponse publishes the partition pool. Its tokens are a fungible
// node-level count — Allocate chooses the card — so health answers "how many more
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
		grp := &devs.Spec.Groups[i]
		if grp.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if !device.IsPartitioned(acc.Status) {
				continue
			}
			res := Resource{Group: grp.ID, Device: acc.ID}
			ceiling := acc.Status.PhysicalSliced.Count
			ids = append(ids, res.DeviceIDs(s.AllocationMode, ceiling)...)
			if acc.Status.Unhealthy {
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
			if !device.PartitionLedgerReady(acc) {
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
	// card Allocate chooses — so neither has a preference to express. Return an empty
	// response (kubelet picks freely) rather than run the sliced per-card bin-fit, which
	// assumes tokens map to the real devices they name.
	if s.AllocationMode == workercore.DeviceAllocationModeVisibility ||
		s.AllocationMode == workercore.DeviceAllocationModePartitioned {
		return &PreferredAllocationResponse{
			ContainerResponses: []*ContainerPreferredAllocationResponse{{}},
		}, nil
	}

	ctrReq := req.GetContainerRequests()[0]

	resName := s.ResourceName()
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
	availableResUnitsMap, err := parseResourceUnitsByCard(ctrReq.GetAvailableDeviceIDs())
	if err != nil {
		return nil, fmt.Errorf("convert available device id %w", err)
	}
	mustIncludedResUnitsMap, err := parseResourceUnitsByCard(ctrReq.GetMustIncludeDeviceIDs())
	if err != nil {
		return nil, fmt.Errorf("convert must include device id %w", err)
	}

	allocationSize := ctrReq.GetAllocationSize()
	preferredDeviceIDsSet := preferredAcceleratorIDsOf(pod, devs)

	// For sliced, every card this HINT offers must still have this container's per-card
	// ".sliced.units" (the memory budget the Pod webhook folded in) free. That constrains the hint,
	// not the outcome: GetPreferredAllocation is advisory, and Allocate refuses a card another mode
	// holds but never one merely short of room, so a kubelet that declines the hint can still
	// over-commit a card. The ledger records sliced allocations in real units, so a card carrying a
	// slice reports Remaining below a fresh card — which is also what orders the candidates below.
	// Zero → no per-card bin-fit (a Pod the webhook did not shape); the loop then behaves as before.
	slicedUnits := int32(0)
	if s.AllocationMode == workercore.DeviceAllocationModeSliced && ctr != nil {
		if q, ok := ctr.Resources.Limits[nodefeature.GetAcceleratableSlicedUnitsResourceName(s.Manufacturer)]; ok {
			slicedUnits = int32(min(q.Value(), int64(nodefeature.ResourceMaxUnits)))
		}
	}

	sel := s.selectPreferredUnits(devs, availableResUnitsMap, mustIncludedResUnitsMap,
		preferredDeviceIDsSet, allocationSize, slicedUnits)
	selectedResUnits, remainingSize := sel.Selected, sel.RemainingSize

	if preferredDeviceIDsSet.Len() > 0 {
		s.Logger.Error(nil, "not enough preferred devices: %v", preferredDeviceIDsSet.UnsortedList())
		if len(sel.Unselected) == 0 {
			return &ContainerPreferredAllocationResponse{}, nil
		}
		// remainingSize can be negative: the walk keeps taking preferred cards after the claim is
		// full, because it only stops early once the preferred set is ALSO empty — and a
		// "accelerator.preferred-id" annotation is taken at face value, so an ID naming no card on
		// this node never clears from that set. A pod rescheduled onto a different node carries
		// exactly such an annotation. Without the lower bound the slice below panics on a negative
		// index and takes the device-manager down with it.
		if remainingSize > 0 && remainingSize <= int32(len(sel.Unselected)) {
			s.Logger.Info("try to allocate from unselected devices since preferred devices are not enough")
			selectedResUnits = append(selectedResUnits, sel.Unselected[:remainingSize]...)
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

// parseResourceUnitsByCard parses device IDs into their ResourceUnits, grouped by the card each one
// names. The IDs are sorted first, so a card's units land in a stable order and a caller taking the
// first of them gets the same token on every call.
func parseResourceUnitsByCard(deviceIDs []string) (map[Resource][]ResourceUnit, error) {
	sort.Strings(deviceIDs)

	byCard := make(map[Resource][]ResourceUnit)
	for i := range deviceIDs {
		resUnit, err := ParseResourceUnit(deviceIDs[i])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", deviceIDs[i], err)
		}
		byCard[resUnit.Resource] = append(byCard[resUnit.Resource], resUnit)
	}

	return byCard, nil
}

// _PreferredSelection is what one walk of this node's cards produced for an allocation hint.
type _PreferredSelection struct {
	// Selected are the tokens the hint will offer, at most one per card.
	Selected []ResourceUnit
	// Unselected are the tokens of cards passed over: one that cannot fit this slice's per-card
	// units, or one outside the preferred set. Only the preferred-accelerator fallback draws on them.
	Unselected []ResourceUnit
	// RemainingSize is how much of the requested allocation the walk could not satisfy.
	RemainingSize int32
}

// selectPreferredUnits walks this manufacturer's cards in packing order and takes one token per
// card until the claim is full and every preferred card has been honored, which is the point it
// returns from. Returning is what lets it leave both loops at once; the two used to need a goto.
//
// preferredDeviceIDsSet is narrowed in place as cards are honored, so what remains in it after the
// walk is exactly the preferred cards that could not be met — which is how the caller decides
// whether to fall back. RemainingSize is returned by value rather than shared, so the loop's exit
// stays one expression and no part of this walk becomes server state.
func (s *ResourceServer) selectPreferredUnits(
	devs *workercore.Devices,
	availableResUnitsMap, mustIncludedResUnitsMap map[Resource][]ResourceUnit,
	preferredDeviceIDsSet sets.Set[string],
	allocationSize, slicedUnits int32,
) _PreferredSelection {
	sel := _PreferredSelection{
		Selected:      make([]ResourceUnit, 0, allocationSize),
		RemainingSize: allocationSize,
	}
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		if grp.Manufacturer != s.Manufacturer {
			continue
		}
		for _, j := range slicedPackingOrder(devs, grp, mustIncludedResUnitsMap, slicedUnits) {
			acc := &grp.Accelerators[j]
			res := Resource{
				Group:  grp.ID,
				Device: acc.ID,
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

			// Exclusive, shared and sliced all select one device unit (token) per card; the
			// per-card concurrency/units accounting lives elsewhere (Kueue credits and the
			// ".sliced.units" capacity), not in the device plugin.
			if mustUnits, existed := mustIncludedResUnitsMap[res]; existed {
				// Only the first must-include unit per card is consumed (one token). The
				// must-include set is what kubelet has already allocated to this container, and it
				// intersects the hint with the still-available set, so echoing such a token cannot
				// win the card back. Echoing it still matters: it spends one of the claim's slots,
				// which is what leaves that intersection holding exactly the devices kubelet still
				// needs rather than a wider set it would then pick from arbitrarily. This is why
				// slicedPackingOrder visits these cards first.
				//
				// It is echoed whatever the ledger reports free — the fit check below deliberately
				// sits on the other branch. This token's units are ALREADY counted in that card's
				// Remaining, so measuring it against Remaining charges the container for its own
				// claim a second time and would drop a token the response has to carry.
				preferredDeviceIDsSet.Delete(res.Device)
				sel.Selected = append(sel.Selected, mustUnits[0])
			} else {
				// Defer a card that cannot fit this slice's per-card units without over-committing
				// it (its ledger Remaining is below the request). That list is consumed only on the
				// preferred-accelerator path, when the annotation's cards could not all be selected;
				// with no annotation a claim no card fits yields an empty hint and these cards are
				// never used.
				if slicedUnits > 0 && statusRemainingOf(devs, res) < slicedUnits {
					sel.Unselected = append(sel.Unselected, resUnits[0])
					continue
				}
				if preferredDeviceIDsSet.Len() != 0 && !preferredDeviceIDsSet.Has(res.Device) {
					sel.Unselected = append(sel.Unselected, resUnits[0])
					continue
				}
				preferredDeviceIDsSet.Delete(res.Device)
				sel.Selected = append(sel.Selected, resUnits[0])
			}
			sel.RemainingSize--
			if preferredDeviceIDsSet.Len() == 0 && sel.RemainingSize <= 0 {
				return sel
			}
		}
	}

	return sel
}

// slicedPackingOrder returns the positions of grp's accelerators in the order a claim of
// slicedUnits should try them. A non-positive slicedUnits leaves the cards in their natural order.
//
// A card kubelet already allocated to this container (mustInclude) comes first whatever its
// occupancy — see the must-include branch in the caller for why echoing it matters.
//
// The rest pack rather than spread: among the cards whose ledger can still take the request, the
// fullest one goes first, so a card already carrying a slice is filled before an untouched sibling
// is broken into. That keeps whole cards whole, so a later large claim still has somewhere to go —
// spreading instead strands a node with plenty of free memory unable to host one big slice. It is
// the rule device.SelectPartitionPlacements already applies to hardware partitions, for the same
// reason. Cards that cannot take the request sort last, emptiest first, so that if the caller's
// preferred-accelerator path has to fall back to one of them it over-commits the least. Ties break
// on the lower position, so two identical requests against identical state place identically.
func slicedPackingOrder(
	devs *workercore.Devices,
	grp *workercore.DevicesGroup,
	mustInclude map[Resource][]ResourceUnit,
	slicedUnits int32,
) []int {
	order := make([]int, len(grp.Accelerators))
	for j := range order {
		order[j] = j
	}
	if slicedUnits <= 0 {
		return order
	}

	resourceAt := func(j int) Resource {
		return Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		resA, resB := resourceAt(a), resourceAt(b)
		_, mustA := mustInclude[resA]
		_, mustB := mustInclude[resB]
		if c := slicex.CompareTrueFirst(mustA, mustB); c != 0 {
			return c
		}
		remainingA, remainingB := statusRemainingOf(devs, resA), statusRemainingOf(devs, resB)
		fitsA, fitsB := remainingA >= slicedUnits, remainingB >= slicedUnits
		if c := slicex.CompareTrueFirst(fitsA, fitsB); c != 0 {
			return c
		}
		if fitsA {
			return cmp.Compare(remainingA, remainingB)
		}
		return cmp.Compare(remainingB, remainingA)
	})
	return order
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

// partitionProfileUnits folds a partition profile into the per-card counting units one instance
// of it commits, from the capability the detector published: the profile's own per-instance
// memory as a share of one whole card's. It is the same VRAM-anchored fold the Pod webhook
// performs, so a partition costs the same whether or not the webhook shaped the request.
//
// The profile is a property of the product, so the first card offering it answers for the group.
// Zero means the group cannot answer — an unknown profile, or a capability published before its
// memory detail was — and the caller keeps whatever budget it already had rather than charging a
// figure it could not derive.
func (s *ResourceServer) partitionProfileUnits(devs *workercore.Devices, profile string) int64 {
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		if grp.Manufacturer != s.Manufacturer || grp.Memory == 0 {
			continue
		}
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if !device.IsPartitioned(acc.Status) {
				continue
			}
			for k := range acc.Status.PhysicalSliced.Profiles {
				prof := &acc.Status.PhysicalSliced.Profiles[k]
				if prof.Name == profile && prof.MemoryMib > 0 {
					return nodefeature.MemoryMibToUnits(prof.MemoryMib, int64(grp.Memory))
				}
			}
		}
	}
	return 0
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
		resUnit, err := ParseResourceUnit(allocatedDeviceIDs[i])
		if err != nil {
			s.Logger.Error(err, "convert device id", "device id", allocatedDeviceIDs[i])
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "invalid device id %q: %v", allocatedDeviceIDs[i], err)
		}
		allocatedResUnitsMap[resUnit.Resource] = append(allocatedResUnitsMap[resUnit.Resource], resUnit)
	}

	// A responder that places logical geometry is called twice: once inside the mutex to pick the
	// window, once after it is released to render the response from what was picked. Resolved once
	// here so the two calls cannot disagree about whether this responder places at all.
	logicalPlacer, placesLogical := s.Responder.(LogicalSlicedResponder)
	placesLogical = placesLogical && s.AllocationMode == workercore.DeviceAllocationModeSliced

	// Identify the pod, enforce the cross-mode invariant, and reserve the cards under the node
	// allocate mutex (see DevicesReconciler.allocateMutex). Holding it across the whole section
	// makes a concurrent Allocate batch (e.g. Kueue admitting identical Pods together) resolve to
	// DISTINCT pods — getAllocatingPod skips pods a prior Allocate already reserved, and that
	// reservation is written there before the next Allocate reads it — and stops an opposite-mode
	// Allocate for the same card from interleaving between the check and the reservation (TOCTOU).
	//
	// The lock is taken here rather than inside decideAllocation so the serialized region is one
	// named call at the point it is entered, and defer releases it on every path including a
	// panic: a leaked allocate mutex would hang every later Allocate on this node.
	decision, err := func() (_AllocationDecision, error) {
		s.Reconciler.allocateMutex.Lock()
		defer s.Reconciler.allocateMutex.Unlock()

		return s.decideAllocation(ctx, allocatedDeviceIDs, allocatedResUnitsMap, logicalPlacer, placesLogical)
	}()
	if err != nil {
		return nil, err
	}

	physical, err := s.actuatePartition(ctx, &decision, allocatedDeviceIDs)
	if err != nil {
		return nil, err
	}

	ctrResp, err := s.renderPrePatchResponse(ctx, &decision, physical, logicalPlacer, placesLogical)
	if err != nil {
		return nil, err
	}

	if err = s.persistAllocation(ctx, &decision, allocatedDeviceIDs, physical); err != nil {
		return nil, err
	}

	// Every other responder keeps its position after the patch — see renderPrePatchResponse for why
	// moving it earlier would strand hardware some vendors create inside it.
	if ctrResp == nil {
		if ctrResp, err = s.Responder.GetContainerAllocateResponse(
			ctx, decision.Pod, decision.Container, decision.Devices, decision.Allocation); err != nil {
			s.Logger.Error(err, "get container allocate response")
			return nil, err
		}
	}

	resp := &AllocateResponse{ContainerResponses: []*ContainerAllocateResponse{ctrResp}}
	s.Logger.Info("allocate response", "pod", kubemeta.GetNamespacedNameKey(decision.Pod), "response", resp)

	return resp, nil
}

// _AllocationDecision is what one workload Allocate settles while it holds the node allocate
// mutex: the pod and container the request was matched to, the ledger snapshot it decided against,
// and the allocation it reserved.
type _AllocationDecision struct {
	Pod       *core.Pod
	Container *core.Container
	// Devices is the ledger snapshot every decision here was made against, carried forward so no
	// later phase re-reads a node that may since have moved on.
	Devices *workercore.Devices
	// Profile is the partition profile this allocation intends; empty for every other family.
	Profile string
	// Status is the allocation as reserved. The actuation phase overwrites its physical placements
	// with the interval the hardware actually gave, then re-publishes it.
	Status workercore.DevicesStatus
	// Allocation is the per-card units charge the responders are handed.
	Allocation map[Resource]int32
	// LogicalPlacements is the geometry picked under the mutex. GetLogicalSlicedResponse consumes
	// it rather than recomputing it: only one of the two can be authoritative.
	LogicalPlacements LogicalPlacements
}

// decideAllocation runs the identify → cross-mode check → reserve section of a workload Allocate.
// The caller holds the node allocate mutex around this call, so nothing here may perform durable
// I/O: every read is served from the informer cache, and the one write it makes is the in-process
// reservation, which must be published before the mutex is released so the next serialized
// Allocate observes it. The annotation patch, the vendor actuation and every response rendering
// run after it returns, off the serialized path.
func (s *ResourceServer) decideAllocation(
	ctx context.Context,
	deviceIDs []string,
	tokensByCard map[Resource][]ResourceUnit,
	logicalPlacer LogicalSlicedResponder,
	placesLogical bool,
) (_AllocationDecision, error) {
	var d _AllocationDecision

	// The ledger is read first: it is what tells two otherwise identical candidates apart in the
	// feasibility test below.
	devs, occupied, occupiedLogical, err := s.snapshotForAllocate(ctx, placesLogical)
	if err != nil {
		return d, err
	}
	d.Devices = devs

	d.Pod, d.Container, err = s.Reconciler.getAllocatingPod(ctx, _AllocationMatch{
		ResourceName: s.ResourceName(),
		Quantity:     *resource.NewQuantity(int64(len(deviceIDs)), resource.DecimalSI),
		SkipReserved: true,
		Feasible:     s.candidateFeasible(devs, tokensByCard, occupied),
	})
	if err != nil {
		s.Logger.Error(err, "get allocating pod for allocation")
		return d, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for allocation: %v", err)
	}

	unitsPerToken, unitsRequested := s.requestedUnits(d.Container)

	var placements map[Resource][]workercore.AcceleratorPhysicalPlacement
	if s.AllocationMode == workercore.DeviceAllocationModePartitioned {
		tokensByCard, placements, unitsPerToken, err = s.choosePartitionCards(&d, occupied, deviceIDs, unitsPerToken, unitsRequested)
		if err != nil {
			return d, err
		}
	}

	if err = s.rejectCrossModeCards(devs, tokensByCard, d.Pod); err != nil {
		return d, err
	}

	d.Status, d.Allocation = s.accumulateAllocation(devs, tokensByCard, unitsPerToken)

	// Publish the logical slice's geometry into the allocation before the reservation, so the next
	// serialized Allocate places against a card whose window is already spoken for. Choosing it
	// later — in the responder, after the mutex — cannot be made race-free.
	if placesLogical {
		if d.LogicalPlacements, err = s.placeLogicalSlice(ctx, &d, logicalPlacer, occupiedLogical); err != nil {
			return d, err
		}
		applyLogicalPlacements(&d.Status, d.LogicalPlacements)
	}

	// Publish the partition selection — card, profile and the intervals it intends to occupy —
	// before the reservation for the same reason, so the next serialized Allocate decides against a
	// card already spoken for rather than one that merely has not been carved yet. Actuation runs
	// after the mutex is released and overwrites the intent with what the hardware actually gave.
	if len(placements) > 0 {
		applyPhysicalPlacements(&d.Status, d.Profile, placements)
	}

	// Reserve the cards in-process before releasing the mutex: the card is taken the instant the
	// check passes, so the next serialized Allocate observes it (the cross-mode check and
	// getAllocatingPod's skip-reserved), and a visibility Allocate can co-allocate the same physical
	// device without racing the annotation's cache propagation. The offered device IDs ride along so
	// ListAndWatch can keep advertising exactly them Healthy for as long as this allocation lives.
	s.Reconciler.reserveDevices(d.Pod.UID, d.Container.Name, d.Status, deviceIDs)

	return d, nil
}

// snapshotForAllocate reads the one consistent view every decision in decideAllocation is made
// against: the Devices ledger, plus the node's live per-card occupancy for the families that choose
// a position on the card themselves. Reading it once keeps the candidate test and the placement
// decision on the same snapshot. Every read is served from the informer cache, which is what makes
// it legal under the allocate mutex.
func (s *ResourceServer) snapshotForAllocate(ctx context.Context, placesLogical bool) (
	*workercore.Devices, map[Resource][]workercore.AcceleratorPhysicalPlacement, LogicalPlacements, error,
) {
	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		s.Logger.Error(err, "get devices for allocation")
		return nil, nil, nil, grpcstatus.Errorf(grpccodes.Internal, "get devices for allocation: %v", err)
	}

	// The Partitioned family decides the placement itself, so it needs the node's live per-card
	// occupancy: the annotations every live allocation carries, unioned with the selections
	// in-flight allocations have already published.
	var occupied map[Resource][]workercore.AcceleratorPhysicalPlacement
	if s.AllocationMode == workercore.DeviceAllocationModePartitioned {
		if occupied, err = s.occupiedPhysicalPlacements(ctx); err != nil {
			s.Logger.Error(err, "get occupied placements for allocation")
			return nil, nil, nil, grpcstatus.Errorf(grpccodes.Internal,
				"get occupied placements for allocation: %v", err)
		}
	}

	// A logical slice that occupies a position on the card needs the same snapshot in its own key
	// space: what every live allocation already occupies, so this one can be placed beside them
	// rather than on top of them.
	var occupiedLogical LogicalPlacements
	if placesLogical {
		if occupiedLogical, err = s.occupiedLogicalPlacements(ctx); err != nil {
			s.Logger.Error(err, "get occupied logical placements for allocation")
			return nil, nil, nil, grpcstatus.Errorf(grpccodes.Internal,
				"get occupied logical placements for allocation: %v", err)
		}
	}

	return devs, occupied, occupiedLogical, nil
}

// requestedUnits reports the per-card counting units one token commits — the normalized value the
// Pod webhook folds the memory budget into (".sliced.units" for a logical slice,
// ".partitioned.units" for a partition) — and whether the container requested it at all. Recording
// that real cost rather than the loose token count keeps the per-card ledger honest, so the
// node-devices admission check refuses a card whose committed units would exceed capacity and the
// InstanceType views report the true remaining. It falls back to one unit per token for a Pod the
// webhook did not shape; whether that fallback is usable is the caller's judgement, since for a
// partition one token is a whole card.
func (s *ResourceServer) requestedUnits(ctr *core.Container) (int64, bool) {
	if unitsResName := s.unitsResourceName(); unitsResName != "" {
		if q, ok := ctr.Resources.Limits[unitsResName]; ok && q.Value() > 0 {
			return q.Value(), true
		}
	}

	return 1, false
}

// choosePartitionCards decides which cards a partition allocation lands on, and at what unit cost.
// This family reads kubelet's tokens as a quantity and picks the cards itself, against the live
// occupancy: kubelet cannot know which card can host a given geometry, and a wrong pick is the one
// placement error the plugin can repair rather than reject — so a rejection here means the node has
// no room at all. It records the profile on d and returns the chosen cards, the intervals they
// intend to occupy, and the unit cost, which it may fold from the profile.
func (s *ResourceServer) choosePartitionCards(
	d *_AllocationDecision,
	occupied map[Resource][]workercore.AcceleratorPhysicalPlacement,
	deviceIDs []string,
	unitsPerToken int64,
	unitsRequested bool,
) (map[Resource][]ResourceUnit, map[Resource][]workercore.AcceleratorPhysicalPlacement, int64, error) {
	profile, ok := partitionProfileOf(d.Container)
	if !ok {
		s.Logger.Error(nil, "partition allocation without a profile",
			"pod", kubemeta.GetNamespacedNameKey(d.Pod), "container", d.Container.Name)
		return nil, nil, unitsPerToken, grpcstatus.Errorf(grpccodes.FailedPrecondition,
			"container %q of pod %s requests %s but names no partition profile",
			d.Container.Name, kubemeta.GetNamespacedNameKey(d.Pod), s.ResourceName())
	}
	d.Profile = profile

	// A partition names its own cost even when nothing shaped the request. The Pod webhook is scoped
	// to queued Pods, so a partition Pod submitted outside the scheduling chain carries no units key
	// — and for this family the token count is not a usable stand-in: one token is one whole card,
	// which would charge a single unit for a partition that occupies half the card or all of it,
	// leaving a card carved to capacity reading as untouched in the scalar ledger. The profile is
	// the missing budget, so fold it here the way the webhook would have.
	if !unitsRequested {
		if units := s.partitionProfileUnits(d.Devices, profile); units > 0 {
			unitsPerToken = units
		}
	}

	// A retried Allocate must land back on the card it already used. The kubelet re-runs Allocate
	// for a container whose checkpoint it lost — a restart while the container was stopped — and by
	// then this container's own placement is part of the node's occupancy. Deciding afresh would
	// read it as somebody else's: a whole-card profile would report the node exhausted, and a node
	// with a free sibling would place on THAT card, bypassing the vendor's per-(pod, container,
	// card) reuse marker and carving a second instance. The card-bound families get this for free,
	// since kubelet re-offers the tokens it checkpointed.
	var (
		cardTokens map[Resource][]ResourceUnit
		placements map[Resource][]workercore.AcceleratorPhysicalPlacement
	)
	if prior, held := s.priorAllocationOf(d.Pod, d.Container.Name); held {
		cardTokens, placements = priorPartitionTokens(prior)
	}
	if cardTokens == nil {
		candidates, byID := s.partitionCandidates(d.Devices, occupied, profile)
		selections, placed := device.SelectPartitionPlacements(candidates, len(deviceIDs))
		if !placed {
			s.Logger.Error(nil, "no card can host the requested partition profile",
				"pod", kubemeta.GetNamespacedNameKey(d.Pod), "profile", profile,
				"instances", len(deviceIDs))
			return nil, nil, unitsPerToken, grpcstatus.Errorf(grpccodes.ResourceExhausted,
				"no card on this node can host %d instance(s) of partition profile %q",
				len(deviceIDs), profile)
		}
		cardTokens, placements = partitionTokens(selections, byID)
	}

	return cardTokens, placements, unitsPerToken, nil
}

// rejectCrossModeCards enforces the per-card cross-mode invariant at the authoritative on-node
// gate: it refuses a card another mode already holds, per the ledger Status OR the in-process
// reservation, so an exclusive tenant truly owns its card on every path, Kueue or raw. Free (None)
// and same-mode cards (e.g. sliced-on-sliced) pass. The partition selector already skipped held
// cards, so for that family this re-checks its own choice rather than kubelet's.
func (s *ResourceServer) rejectCrossModeCards(
	devs *workercore.Devices, cardTokens map[Resource][]ResourceUnit, pod *core.Pod,
) error {
	for res := range cardTokens {
		if held, mode := s.cardHeldInOtherMode(devs, res); held {
			s.Logger.Error(nil, "cross-mode allocation rejected",
				"pod", kubemeta.GetNamespacedNameKey(pod), "card", res.String(),
				"heldMode", mode.String(), "requestedMode", s.AllocationMode.String())
			return grpcstatus.Errorf(grpccodes.FailedPrecondition,
				"card %s is held in %s mode, cannot allocate it in %s mode", res, mode, s.AllocationMode)
		}
	}

	return nil
}

// accumulateAllocation charges each card the tokens land on for the units this container commits,
// building the allocation status the reservation publishes and the annotation persists.
func (s *ResourceServer) accumulateAllocation(
	devs *workercore.Devices, cardTokens map[Resource][]ResourceUnit, unitsPerToken int64,
) (workercore.DevicesStatus, map[Resource]int32) {
	var (
		allocatedStatus     workercore.DevicesStatus
		allocatedAllocation = make(map[Resource]int32)
	)
	for i := range devs.Spec.Groups {
		specGrp := &devs.Spec.Groups[i]
		if specGrp.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range specGrp.Accelerators {
			acc := &specGrp.Accelerators[j]
			res := Resource{
				Group:  specGrp.ID,
				Device: acc.ID,
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
				// Real per-card units this container commits on the card, so the ledger reflects
				// capacity rather than the loose token count. A partition needs its own branch here
				// for the same reason a slice does, and for one more: the default charges a WHOLE
				// card, which would make a single small instance look like it owned the card and
				// hide the rest of its geometry from every consumer of the scalar remaining.
				allocated = int32(min(unitsPerToken*int64(len(resUnits)), int64(nodefeature.ResourceMaxUnits)))
			}
			if allocated > nodefeature.ResourceMaxUnits {
				allocated = nodefeature.ResourceMaxUnits
			}
			if len(allocatedStatus.Groups) == 0 || allocatedStatus.Groups[len(allocatedStatus.Groups)-1].ID != specGrp.ID {
				allocatedStatus.Groups = append(allocatedStatus.Groups, workercore.DevicesAllocationGroup{
					ID:           specGrp.ID,
					Manufacturer: specGrp.Manufacturer,
				})
			}
			statusGrp := &allocatedStatus.Groups[len(allocatedStatus.Groups)-1]
			statusGrp.Accelerators = append(statusGrp.Accelerators, workercore.AcceleratorAllocation{
				ID:        acc.ID,
				Index:     acc.Index,
				Mode:      s.AllocationMode,
				Allocated: allocated,
			})
			allocatedAllocation[res] = allocated
		}
	}

	return allocatedStatus, allocatedAllocation
}

// placeLogicalSlice picks the geometry this container will occupy on each allocated card, reusing
// the window it already holds when the kubelet is re-running an Allocate whose checkpoint it lost:
// by then the container's own window is part of the node's occupancy, so deciding afresh would read
// it as somebody else's, move the container to a different window, and strand the first one until
// the Pod goes away.
func (s *ResourceServer) placeLogicalSlice(
	ctx context.Context,
	d *_AllocationDecision,
	logicalPlacer LogicalSlicedResponder,
	occupiedLogical LogicalPlacements,
) (LogicalPlacements, error) {
	if prior, held := s.priorLogicalPlacements(d.Pod, d.Container.Name); held {
		return prior, nil
	}

	placements, err := logicalPlacer.PlaceLogicalSliced(
		ctx, d.Pod, d.Container, d.Devices, d.Allocation, occupiedLogical)
	if err != nil {
		s.Logger.Error(err, "place logical slice for allocation",
			"pod", kubemeta.GetNamespacedNameKey(d.Pod), "container", d.Container.Name)
		return nil, grpcstatus.Errorf(grpccodes.Internal, "place logical slice: %v", err)
	}

	return placements, nil
}

// actuatePartition materializes the hardware partition(s) for an allocation that names a profile,
// via the responder's actuator under that responder's own per-card lock, and records each chosen
// placement upward in d.Status BEFORE the annotation patch, so the reconciler's ledger can
// reconstruct the card's occupied set. The actuator may land on a different interval of the selected
// card than the one published as the intent — it can adopt an instance the hardware already
// carries — so the reservation is re-published with what actually happened. The node allocate mutex
// is already released by now; the per-card lock serializes only same-card creates, so sibling cards
// proceed in parallel. A responder that cannot actuate fails the allocation rather than starting a
// container with no partition. Every family that names no profile gets a nil allocation and no work.
func (s *ResourceServer) actuatePartition(
	ctx context.Context, d *_AllocationDecision, deviceIDs []string,
) (*PhysicalSlicedAllocation, error) {
	if d.Profile == "" {
		return nil, nil
	}

	actuator, canActuate := s.Responder.(PhysicalSlicedResponder)
	if !canActuate {
		s.Reconciler.releaseReservation(d.Pod.UID, d.Container.Name)

		return nil, grpcstatus.Errorf(grpccodes.Internal,
			"responder cannot actuate partition profile %q", d.Profile)
	}

	physical, err := actuator.ActuatePhysicalSliced(
		ctx, d.Pod, d.Container, d.Devices, d.Allocation, d.Profile)
	if err != nil {
		s.Reconciler.releaseReservation(d.Pod.UID, d.Container.Name)
		s.Logger.Error(err, "actuate partition for allocation", "pod", kubemeta.GetNamespacedNameKey(d.Pod))

		return nil, grpcstatus.Errorf(grpccodes.Internal, "actuate partition: %v", err)
	}
	applyPhysicalPlacements(&d.Status, physical.Profile, physical.Placements)
	s.Reconciler.reserveDevices(d.Pod.UID, d.Container.Name, d.Status, deviceIDs)

	return physical, nil
}

// renderPrePatchResponse renders the container response for the two families whose response may be
// built BEFORE the durable patch, and returns nil for every other one, which is rendered after it.
//
// A logical-slice response qualifies because it is the response whose failure would otherwise leave
// a CU window recorded on a card for a container kubelet never starts, and it creates nothing but a
// directory, so rendering it early costs nothing if the patch then fails.
//
// Moving the OTHER responders here would be a regression, and a quiet one. Cambricon and MetaX
// materialize a subdevice and an on-disk ownership marker inside GetContainerAllocateResponse; were
// that to run before a patch that then failed, the hardware would exist while the ledger said the
// card was free, and their reclaimers — which preserve an instance whose Pod is still live — would
// keep it that way. They stay below the patch, and the strand their own failure can leave is
// pre-existing and out of scope here.
//
// A physical-slice allocation injects only the partition's visible-devices env the actuator already
// assembled (no logical-slice artifacts), so it bypasses the responder entirely.
func (s *ResourceServer) renderPrePatchResponse(
	ctx context.Context,
	d *_AllocationDecision,
	physical *PhysicalSlicedAllocation,
	logicalPlacer LogicalSlicedResponder,
	placesLogical bool,
) (*ContainerAllocateResponse, error) {
	switch {
	case physical != nil:
		return physical.Response, nil

	case placesLogical:
		// The placement is consumed, never recomputed: what the container is told must be what the
		// ledger recorded, and only one of the two can be authoritative.
		ctrResp, err := logicalPlacer.GetLogicalSlicedResponse(
			ctx, d.Pod, d.Container, d.Devices, d.Allocation, d.LogicalPlacements)
		if err != nil {
			s.Reconciler.releaseReservation(d.Pod.UID, d.Container.Name)
			s.Logger.Error(err, "get logical sliced response")

			return nil, err
		}

		return ctrResp, nil
	}

	return nil, nil
}

// persistAllocation writes the durable allocation annotation, which runs outside the node allocate
// mutex because it is I/O. On failure it rolls back the reservation taken under the mutex: with the
// annotation absent, the Pod-delete watch (gated on it) would never enqueue a prune, so the card
// would stay stranded for the opposite mode. Kubelet does not start the container on this error, so
// freeing the reservation now is safe and keeps the release counting honest. A partition
// materialized beforehand is torn down too, so no half-owned instance persists past a failed patch.
func (s *ResourceServer) persistAllocation(
	ctx context.Context,
	d *_AllocationDecision,
	deviceIDs []string,
	physical *PhysicalSlicedAllocation,
) error {
	err := s.Reconciler.patchAllocatingPod(ctx, d.Pod, d.Container.Name, d.Status, deviceIDs)
	if err == nil {
		return nil
	}

	if physical != nil && physical.Rollback != nil {
		physical.Rollback()
	}
	s.Reconciler.releaseReservation(d.Pod.UID, d.Container.Name)
	s.Logger.Error(err, "patch allocating pod for allocation")

	return grpcstatus.Errorf(grpccodes.Internal, "patch allocating pod for allocation: %v", err)
}

// partitionProfileOf returns the single "<base>.partitioned.<kind>-<profile>" profile a
// container requests, and whether it requests one. The Pod webhook guarantees at most one
// distinct profile per container, so the first match is authoritative.
//
// The name comes back in the manufacturer's own spelling, not the key's published one: every
// consumer here matches it against the Devices record, records it in the allocation and the
// ownership marker, or hands it to the vendor library, and a name the library does not
// recognize cannot create a partition.
func partitionProfileOf(ctr *core.Container) (string, bool) {
	for name := range ctr.Resources.Limits {
		if profile, ok := nodefeature.VendorPartitionedProfileOf(name); ok {
			return profile, true
		}
	}
	return "", false
}

// priorAllocationOf returns the allocation this container already holds: the in-process
// reservation first, then the durable annotation, which is the only record that survives a
// device-manager restart. It is what makes a retried Allocate idempotent, for a partition's card
// and interval and for a logical slice's window alike — the lookup is mode-agnostic, and each
// caller reads out the part of the record it owns.
func (s *ResourceServer) priorAllocationOf(
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

// coAllocatedAccelerators returns the accelerator devices a pod holds in a container other than
// self — what a visibility request co-allocates — together with that container's name. It reads
// the in-process reservation first and falls back to the Pod's durable allocation annotation, the
// only record that survives a device-manager restart landing between the owner's Allocate and
// this one. Both sources are resolved by the same owner pick, so the devices and the name always
// describe one container.
//
// An unreadable annotation is an error rather than an empty result: the caller must reject the
// admission, never fall through to a response it cannot substantiate. A pod with no record in
// either source yields no devices and no error, which the caller's own gate rejects.
func (s *ResourceServer) coAllocatedAccelerators(
	pod *core.Pod, self string,
) (workercore.DevicesStatus, string, error) {
	if reserved, owner, ok := s.Reconciler.reservedAcceleratorDevices(pod.UID, self); ok && len(reserved.Groups) > 0 {
		return reserved, owner, nil
	}
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		return workercore.DevicesStatus{}, "", fmt.Errorf(
			"read the allocation annotation of pod %s: %w", kubemeta.GetNamespacedNameKey(pod), err)
	}
	names := make([]string, 0, len(allocations))
	for name := range allocations {
		names = append(names, name)
	}
	owner, ok := pickAcceleratorOwner(names, self)
	if !ok {
		return workercore.DevicesStatus{}, "", nil
	}
	return allocations[owner].Devices, owner, nil
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
			// The profile is what tells the two ledgers sharing this field apart, and it is checked
			// here for the same reason every other reader checks it. This path cannot meet a
			// logical entry today — it is reached only for a container that requested a partition
			// profile, and a container requests one family or the other — but "cannot happen today"
			// is not what the other readers rely on, and it should not be what this one does.
			if acc.AllocatedPhysicalProfile != "" && len(acc.AllocatedPhysicalPlacements) > 0 {
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

// occupiedLogicalPlacements unions the same two records the physical placement decision reads, in
// the logical ledger's own key space: the geometry live allocations recorded in their Pod
// annotations, and the geometry in-flight allocations published into their reservation before the
// annotation could land. Neither source alone is enough — the annotation lags by a cache
// round-trip, the reservation forgets everything across a restart.
//
// An allocation legitimately appears in BOTH once its annotation has propagated. That is harmless
// for a decision reading overlap as a yes/no, which is what the physical ledger does; it is not
// harmless for one that ranks candidates by how much they overlap, so a placer that ranks must
// merge before it measures.
func (s *ResourceServer) occupiedLogicalPlacements(ctx context.Context) (LogicalPlacements, error) {
	occupied, err := s.Reconciler.LiveLogicalOccupied(ctx)
	if err != nil {
		return nil, err
	}
	for id, placements := range s.Reconciler.reservedLogicalOccupied() {
		occupied[id] = append(occupied[id], placements...)
	}
	return occupied, nil
}

// applyLogicalPlacements records the geometry the responder chose into the allocation status, so
// the reservation publishes it and the annotation patch persists it. Keyed by accelerator UUID —
// see LogicalPlacements for why the group is deliberately not part of that key.
//
// It writes AllocatedPhysicalPlacements and leaves AllocatedPhysicalProfile empty, which is what
// tells the two ledgers apart: the physical accumulator skips an entry with no profile, and the
// logical one requires exactly that shape.
func applyLogicalPlacements(status *workercore.DevicesStatus, placements LogicalPlacements) {
	for i := range status.Groups {
		grp := &status.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if p, ok := placements[acc.ID]; ok && len(p) > 0 {
				acc.AllocatedPhysicalPlacements = p
			}
		}
	}
}

// priorLogicalPlacements returns the geometry this container already holds, so a retried Allocate
// reuses its own window instead of reading it as somebody else's.
func (s *ResourceServer) priorLogicalPlacements(pod *core.Pod, container string) (LogicalPlacements, bool) {
	prior, held := s.priorAllocationOf(pod, container)
	if !held {
		return nil, false
	}
	placements := make(LogicalPlacements)
	accumulateLogicalOccupied(prior, placements)
	if len(placements) == 0 {
		return nil, false
	}
	return placements, true
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

// allocateVisibility serves a visibility request: it does not select a device but reuses the
// physical device(s) another container of the same Pod — its owner — was already allocated,
// recorded in the in-process reservation. It returns only the vendor visible-devices response
// (via the Responder), consuming no ledger units and writing no allocation status. It fails
// closed when no allocation can be resolved, rather than emitting an empty visible-devices env
// a runtime could interpret as "all devices".
func (s *ResourceServer) allocateVisibility(ctx context.Context, req *AllocateRequest) (*AllocateResponse, error) {
	ctrReq := req.GetContainerRequests()[0]

	resName := s.ResourceName()
	resQuantity := *resource.NewQuantity(int64(len(ctrReq.GetDevicesIds())), resource.DecimalSI)
	pod, ctr, err := s.claimVisibilityContainer(ctx, resName, resQuantity)
	if err != nil {
		s.Logger.Error(err, "get allocating pod for visibility allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for visibility allocation: %v", err)
	}
	// Everything below can still reject this admission, and kubelet retries the same container.
	// Keep the grant only if one is actually issued, so the retry resolves back to this container
	// rather than to another pod's.
	granted := false
	defer func() {
		if !granted {
			s.Reconciler.revokeVisibility(pod.UID, ctr.Name)
		}
	}()

	// Take the accelerator allocation the pod holds in a container other than this one: a
	// visibility request co-allocates what its owner holds and reserves nothing of its own.
	// Accelerator claims live in exactly one container group, so there is only one.
	reserved, owner, err := s.coAllocatedAccelerators(pod, ctr.Name)
	if err != nil {
		s.Logger.Error(err, "resolve the visibility co-allocation")
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "%v", err)
	}
	if len(reserved.Groups) == 0 {
		// Fail closed: the owner container's allocation is recorded in neither the in-process
		// reservation nor the durable annotation. Returning an error rejects this admission
		// rather than emitting an empty visible-devices env a runtime could read as "all
		// devices". Admission walks a Pod's init containers and then its containers in spec
		// order, and every producer of a visibility claim today emits the owner ahead of it, so
		// this path should not occur in practice; if it ever does, recovery is the controller
		// recreating the Pod. Ordering the visibility container first — an init container with
		// an always-on restart policy, say — would make it unresolvable every time, so a
		// producer that wants that must give this path a source that does not depend on order.
		err = fmt.Errorf("no accelerator devices allocated for pod %s by a container other than %q "+
			"(owner=%q); refusing to grant visibility",
			kubemeta.GetNamespacedNameKey(pod), ctr.Name, owner)
		s.Logger.Error(err, "visibility allocation without a co-allocation")
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "%v", err)
	}

	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		s.Logger.Error(err, "get devices for visibility allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get devices for visibility allocation: %v", err)
	}

	allocated, err := s.validateVisibilityReservation(devs, reserved, pod, owner, len(ctrReq.GetDevicesIds()))
	if err != nil {
		return nil, err
	}

	ctrResp, err := s.visibilityResponse(ctx, pod, ctr, devs, allocated, owner)
	if err != nil {
		s.Logger.Error(err, "get container visibility allocate response")
		return nil, err
	}

	resp := &AllocateResponse{
		ContainerResponses: []*ContainerAllocateResponse{ctrResp},
	}
	granted = true
	s.Logger.Info("visibility allocate response",
		"pod", kubemeta.GetNamespacedNameKey(pod),
		"response", resp)
	return resp, nil
}

// validateVisibilityReservation turns the owner container's reservation into the "allocated" set a
// visibility response is rendered from, and fails closed unless every reserved device is still
// present on the node AND the reservation matches this request exactly.
//
// A partial or stale reservation — a reserved device gone from the inventory — or a request whose
// device count differs from the reservation would otherwise grant a different device set than the
// owner holds. Refusing is better than emitting a degraded, or empty, visible-devices env, which a
// runtime could read as "all devices".
//
// The reserved devices are reused verbatim as the allocated set so the Responder emits the vendor
// visible-devices env for exactly them, with no sliced-mode mounts or limit. Nothing is patched or
// reserved here: visibility consumes no ledger units and holds no reservation of its own.
func (s *ResourceServer) validateVisibilityReservation(
	devs *workercore.Devices,
	reserved workercore.DevicesStatus,
	pod *core.Pod,
	owner string,
	requestCount int,
) (map[Resource]int32, error) {
	present := make(map[Resource]struct{})
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			present[Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}] = struct{}{}
		}
	}

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

	if len(allocated) != reservedCount || reservedCount != requestCount {
		err := fmt.Errorf("visibility reservation for pod %s held by container %q does not match the "+
			"request (cards=%v, reserved=%d, present=%d, requested=%d); refusing to grant visibility",
			kubemeta.GetNamespacedNameKey(pod), owner, reservedCards, reservedCount, len(allocated), requestCount)
		s.Logger.Error(err, "visibility allocation with mismatched or stale reservation")

		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "%v", err)
	}

	return allocated, nil
}

// claimVisibilityContainer identifies the container a visibility Allocate is serving and records
// the grant, under the node allocate mutex so the record is written before the next call in a
// concurrent batch reads it (see DevicesReconciler.allocateMutex).
//
// Reserved containers are deliberately NOT skipped: visibility re-finds its own pod, whose owner
// container already recorded a reservation. Already-granted ones are, and that is the whole reason
// the grant exists — a batch of identical Pods offers the search several indistinguishable
// visibility containers, and without a per-container claim every one of them resolves to the
// oldest pending pod, granting the later ones the first pod's cards and, when its owner is
// partition-backed, the first pod's partition.
func (s *ResourceServer) claimVisibilityContainer(
	ctx context.Context, resName core.ResourceName, resQuantity resource.Quantity,
) (*core.Pod, *core.Container, error) {
	s.Reconciler.allocateMutex.Lock()
	defer s.Reconciler.allocateMutex.Unlock()

	pod, ctr, err := s.Reconciler.getAllocatingPod(ctx, _AllocationMatch{
		ResourceName: resName,
		Quantity:     resQuantity,
		SkipGranted:  true,
	})
	if err != nil {
		return nil, nil, err
	}
	s.Reconciler.grantVisibility(pod.UID, ctr.Name)
	return pod, ctr, nil
}

// visibilityResponse renders the visibility container's response over the cards its owner holds.
//
// A partition-backed owner is answered by the responder's partition capability, because a
// visibility allocation is a device-cgroup grant and nothing else: naming the parent card would
// open every partition carved on it, including other tenants'. The trigger is the owner
// container's own resource request, which is in the Pod spec from the start and therefore immune
// to the order in which the workload's Allocate publishes and re-publishes its reservation.
//
// Everything the branch cannot establish rejects the admission — an owner container missing from
// the Pod spec, a responder without the capability, a capability that errors. Every other family
// keeps the card-based response unchanged.
func (s *ResourceServer) visibilityResponse(
	ctx context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[Resource]int32,
	owner string,
) (*ContainerAllocateResponse, error) {
	ownerCtr := containerByName(pod, owner)
	if ownerCtr == nil {
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition,
			"pod %s has no container %q to co-allocate from; refusing to grant visibility",
			kubemeta.GetNamespacedNameKey(pod), owner)
	}
	profile, partitioned := partitionProfileOf(ownerCtr)
	if !partitioned {
		return s.Responder.GetContainerAllocateResponse(ctx, pod, ctr, devs, allocated)
	}

	responder, canRespond := s.Responder.(PhysicalSlicedResponder)
	if !canRespond {
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition,
			"responder cannot name the partition profile %q container %q holds in pod %s; "+
				"refusing to grant visibility",
			profile, owner, kubemeta.GetNamespacedNameKey(pod))
	}
	ctrResp, err := responder.GetPhysicalSlicedVisibilityResponse(ctx, pod, ctr, devs, allocated, owner)
	if err != nil {
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition,
			"name the partition profile %q container %q holds in pod %s: %v; refusing to grant visibility",
			profile, owner, kubemeta.GetNamespacedNameKey(pod), err)
	}
	return ctrResp, nil
}

// containerByName returns the pod's container with the given name, init containers first, as the
// allocating-pod scan visits them. It returns nil when the pod declares no such container.
func containerByName(pod *core.Pod, name string) *core.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// Start listens on this server's own socket beside kubelet's, serves the device-plugin gRPC API on
// it, and once the server reports ready registers the resource name with kubelet. It blocks until
// ctx is canceled or either half fails, removing the socket on the way out. Calling it on a server
// that is already running starts nothing and simply blocks until ctx is done, so a supervisor that
// retries cannot end up with two servers on one socket.
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
		ResourceName: string(s.ResourceName()),
		Options:      regOpts,
	}
	if _, err = regCli.Register(ctx, regReq); err != nil {
		s.Logger.Error(err, "register to kubelet", "socket", kubeSocket)
		return err
	}

	return nil
}

// Stop stops the gRPC server and clears it, so a later Start may serve again. It returns
// immediately and is a no-op, logged, on a server that was never started. In-flight Allocates are
// abandoned rather than drained: kubelet retries an allocation it did not get an answer for, and
// the reservations this server took are pruned by the reconciler once their Pods are gone.
func (s *ResourceServer) Stop() {
	if s.server == nil {
		s.Logger.Errorf(nil, "server not started")
		return
	}

	s.Logger.Info("stopping")
	s.server.Stop()
	s.server = nil
}
