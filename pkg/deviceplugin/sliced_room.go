package deviceplugin

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// SlicedAllocateGateEnv turns the sliced Allocate refusal off when set to "false": a logical slice
// kubelet hands a card without room is then allocated anyway and the ledger clamps that card's
// Remaining at zero, as it did before the refusal existed.
const SlicedAllocateGateEnv = "GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE"

// _SlicedOccupancy is what the node's live logical slices hold, per card and per holding container.
// Keeping the holder is what lets one container be judged against everyone but itself: a retried
// Allocate replaces the container's own record rather than adding to it.
type _SlicedOccupancy map[Resource]map[_ReservationKey]int32

// slicedOccupancy reads the node's logical-slice occupancy from both records of what a container
// holds, and counts each container once. The in-process reservation lands before the annotation and
// the annotation outlives a restart, so either alone misses allocations; a container found in both is
// read the way priorClaimOf reads it — reservation, then a give-back still pending, then annotation.
//
// A Pod in a terminal phase is skipped. kubelet has stopped its containers and returned their tokens,
// and the per-process memory limit went with them, so what it records no longer occupies the card.
// The Devices ledger keeps charging such a Pod until it is deleted; that is the ledger's rule and is
// left alone here. A reservation whose Pod is no longer listed is skipped as well: the Pod is gone,
// and the next reconcile prunes the reservation.
//
// Every read is served from the informer cache and from memory, which is what makes it legal under
// the allocate mutex.
func (s *ResourceServer) slicedOccupancy(ctx context.Context) (_SlicedOccupancy, error) {
	podList := new(core.PodList)
	err := s.Reconciler.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: s.Reconciler.NodeName},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		return nil, fmt.Errorf("list pods with node name: %w", err)
	}

	occupied := make(_SlicedOccupancy)
	for i := range podList.Items {
		pod := &podList.Items[i]
		if p := pod.Status.Phase; p == core.PodSucceeded || p == core.PodFailed {
			continue
		}
		// An unreadable annotation contributes no names, and priorClaimOf reads it as holding
		// nothing: the ledger drops such a Pod too, and says so loudly when it does.
		allocations, _ := AllocatedAcceleratorsOf(pod)
		holders := sets.New[string]()
		for name := range allocations {
			holders.Insert(name)
		}
		for name := range s.Reconciler.reservationsFor(pod.UID) {
			holders.Insert(name)
		}
		for _, name := range sets.List(holders) {
			if held := s.priorClaimOf(pod, name); held != nil {
				occupied.add(_ReservationKey{PodUID: pod.UID, Container: name}, held.Devices)
			}
		}
	}
	return occupied, nil
}

// add records the logical slices one container holds.
func (o _SlicedOccupancy) add(holder _ReservationKey, held workercore.DevicesStatus) {
	for i := range held.Groups {
		grp := &held.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if acc.Mode != workercore.DeviceAllocationModeSliced {
				continue
			}
			res := Resource{Group: grp.ID, Device: acc.ID}
			if o[res] == nil {
				o[res] = make(map[_ReservationKey]int32)
			}
			o[res][holder] += acc.Allocated
		}
	}
}

// heldExcept returns the units and the number of slices that holders other than self hold on a card.
//
// A slice is counted per holding container, the unit the ledger's AllocatedSlices counts in too. On
// the scheduling chain that is one token: the Pod webhook sets ".sliced" to exactly 1. A container
// outside the chain that asks for several tokens and gets two of them on one card counts once here,
// so the count can read a slot free that is taken. The card is not overfilled all the same: kubelet
// hands out at most LogicalSliced.Count tokens per card, whatever this count says.
func (o _SlicedOccupancy) heldExcept(res Resource, self _ReservationKey) (units, slices int32) {
	for holder, u := range o[res] {
		if holder == self {
			continue
		}
		units += u
		slices++
	}
	return units, slices
}

// _SlicedRoom answers, for one container, how much room each card has for its logical slice. Every
// decision about where a slice may go reads it — the allocation hint, the candidate test and the
// refusal — so they cannot disagree about a card.
//
// Without an occupancy it reads the Devices ledger's Remaining, which is what every one of those
// decisions read before the occupancy existed; that is the behavior the gate's off switch restores.
type _SlicedRoom struct {
	devs     *workercore.Devices
	occupied _SlicedOccupancy
	self     _ReservationKey
}

// remaining returns the units a card still has for this container's slice.
func (r _SlicedRoom) remaining(res Resource) int32 {
	if r.occupied == nil {
		return statusRemainingOf(r.devs, res)
	}
	units, _ := r.occupied.heldExcept(res, r.self)
	return max(nodefeature.ResourceMaxUnits-units, 0)
}

// shortage explains why a card cannot give this container a slice of need units, or returns "" when
// it can. A slice needs a free slot as well as free units: a card hosts at most its LogicalSliced.Count
// slices whatever its memory says. Exactly filling a card fits.
func (r _SlicedRoom) shortage(res Resource, need int32) string {
	if r.occupied != nil {
		_, slices := r.occupied.heldExcept(res, r.self)
		if capability, ok := capabilityOf(r.devs, res); ok && device.IsLogicallySliceable(capability) &&
			device.LogicalSlotsFree(capability, slices) <= 0 {
			return fmt.Sprintf("it already hosts %d of its %d slices", slices, capability.LogicalSliced.Count)
		}
	}
	if free := r.remaining(res); free < need {
		return fmt.Sprintf("it has %d of %d units free and the slice needs %d",
			free, nodefeature.ResourceMaxUnits, need)
	}
	return ""
}

// capabilityOf returns the capability the Devices spec reports for a card.
func capabilityOf(devs *workercore.Devices, res Resource) (workercore.AcceleratorStatus, bool) {
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		if grp.ID != res.Group {
			continue
		}
		for j := range grp.Accelerators {
			if grp.Accelerators[j].ID == res.Device {
				return grp.Accelerators[j].Status, true
			}
		}
	}
	return workercore.AcceleratorStatus{}, false
}

// slicedRoomFor returns the room view for one container of this sliced server, reading the
// occupancy unless the refusal is switched off.
func (s *ResourceServer) slicedRoomFor(
	devs *workercore.Devices, occupied _SlicedOccupancy, pod *core.Pod, ctr *core.Container,
) _SlicedRoom {
	room := _SlicedRoom{devs: devs, occupied: occupied}
	if pod != nil && ctr != nil {
		room.self = _ReservationKey{PodUID: pod.UID, Container: ctr.Name}
	}
	return room
}

// unitsCharged returns the units the ledger records for tokens tokens of one card at unitsPerToken
// each, capped at a whole card. The refusal and the ledger both use it, so the refusal fires exactly
// where the ledger would otherwise have to clamp.
func unitsCharged(unitsPerToken int64, tokens int) int32 {
	perToken := min(unitsPerToken, int64(nodefeature.ResourceMaxUnits))
	return int32(min(perToken*int64(tokens), int64(nodefeature.ResourceMaxUnits)))
}

// rejectSlicedOvercommit refuses a logical slice that a card kubelet handed it cannot hold, with
// FailedPrecondition, the code the shared family's repeated-card refusal uses: kubelet fails the Pod
// with UnexpectedAdmissionError and does not start it.
//
// kubelet picks a card freely whenever the allocation hint cannot be met or is not taken, so this is
// the one layer every path reaches — a Pod outside the scheduling chain, or a hint kubelet declined.
// Allocating anyway would hand the container a memory limit the card cannot honor next to its
// neighbours' and clamp the ledger at zero, which hides the overcommit from every check above.
//
// It is judged against the room excluding the container's own record, so a retry for a container
// that already holds a slice does not count that slice twice. It reads the same room the candidate
// test reads, so when it refuses, no candidate the call could have been for fits the card either.
func (s *ResourceServer) rejectSlicedOvercommit(
	d *_AllocationDecision, occupied _SlicedOccupancy,
	tokensByCard map[Resource][]ResourceToken, unitsPerToken int64,
) error {
	if occupied == nil {
		return nil
	}
	room := s.slicedRoomFor(d.Devices, occupied, d.Pod, d.Container)
	cards := make([]Resource, 0, len(tokensByCard))
	for res := range tokensByCard {
		cards = append(cards, res)
	}
	slices.SortFunc(cards, func(a, b Resource) int { return cmp.Compare(a.String(), b.String()) })
	for _, res := range cards {
		need := unitsCharged(unitsPerToken, len(tokensByCard[res]))
		why := room.shortage(res, need)
		if why == "" {
			continue
		}
		s.Logger.Error(nil, "sliced allocation the card cannot hold rejected",
			"pod", kubemeta.GetNamespacedNameKey(d.Pod), "container", d.Container.Name,
			"card", res.String(), "reason", why)
		return grpcstatus.Errorf(grpccodes.FailedPrecondition,
			"container %q of pod %s cannot take a logical slice of card %s: %s",
			d.Container.Name, kubemeta.GetNamespacedNameKey(d.Pod), res, why)
	}
	return nil
}
