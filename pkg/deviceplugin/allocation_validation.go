package deviceplugin

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The strict rebuild exists because the ordinary one is a best-effort answer and a retirement needs
// a different one. mergePodAllocations cannot fail: an unreadable annotation, a card the inventory
// does not know and a mode conflict are all logged and skipped, and a holder that drops out of the
// merge reads FREE while its containers still hold the hardware. That is the right trade for a
// rebuild feeding kubelet, and the wrong one for a predicate whose job is to decide whether capacity
// was released. This path runs the same arithmetic over the same inputs and returns the error
// instead of swallowing it, so an incomplete input holds the operation rather than clearing it.
//
// Nothing here re-derives the accounting. heldAllocation, applyAllocatedStatus,
// accumulatePhysicalOccupied and foldPhysicalLedger are the existing functions, called the same way
// the ordinary rebuild calls them, so a strict result and a published ledger cannot disagree
// because of arithmetic that exists twice.

// BuildDesiredStatusStrict is BuildDesiredStatus for a caller that must not be told a half-answer.
//
// It validates every pod's record before merging any of it, so a single bad record fails the whole
// rebuild instead of quietly removing one holder from the ledger.
func BuildDesiredStatusStrict(
	logger logr.Logger, devs *workercore.Devices, podList *core.PodList,
) (workercore.DevicesStatus, []string, error) {
	// The inventory is the other half of the input and it is validated once, up front: a duplicate
	// or self-inconsistent card identity makes every later comparison ambiguous, and an arithmetic
	// fold over an ambiguous inventory produces a confident wrong answer rather than a visible one.
	if err := validateDeviceInventory(devs); err != nil {
		return workercore.DevicesStatus{}, nil, err
	}

	for i := range podList.Items {
		if err := validatePodAllocationRecord(&podList.Items[i], devs); err != nil {
			return workercore.DevicesStatus{}, nil, err
		}
	}

	// The reducer's own errors come from the same arithmetic the ordinary rebuild runs, returned
	// rather than logged.
	desired, livePodUIDs, err := mergePodAllocationsStrict(devs, podList)
	if err != nil {
		return workercore.DevicesStatus{}, nil, err
	}

	return desired, livePodUIDs, nil
}

// validateDeviceInventory rejects an inventory whose own identities contradict.
//
// A card is identified by (group ID, device ID) and additionally carries an index and a
// manufacturer. A duplicate identity means two spec rows claim one card; an index that disagrees
// with its position means a later lookup by index resolves to a different card than a lookup by ID.
func validateDeviceInventory(devs *workercore.Devices) error {
	byID := make(map[string]struct{}, len(devs.Spec.Groups))
	for i := range devs.Spec.Groups {
		group := &devs.Spec.Groups[i]
		if group.ID == "" {
			return fmt.Errorf("accelerator group %d carries no ID", i)
		}
		if _, seen := byID[group.ID]; seen {
			return fmt.Errorf("accelerator group %q is declared twice", group.ID)
		}
		byID[group.ID] = struct{}{}

		ids := make(map[string]struct{}, len(group.Accelerators))
		// AN INDEX IS VENDOR-LOCAL. Two vendors both number their first card 0, and that is what the
		// detector writes, so uniqueness is scoped to the group and not asserted across the card.
		// Demanding it globally would reject a node with an NVIDIA and an AMD device on it.
		indices := make(map[uint32]struct{}, len(group.Accelerators))
		for j := range group.Accelerators {
			card := &group.Accelerators[j]
			if card.ID == "" {
				return fmt.Errorf("accelerator %d of group %q carries no ID", j, group.ID)
			}
			if _, seen := ids[card.ID]; seen {
				return fmt.Errorf("accelerator %q is declared twice in group %q", card.ID, group.ID)
			}
			ids[card.ID] = struct{}{}
			if _, seen := indices[card.Index]; seen {
				return fmt.Errorf(
					"accelerator index %d is declared twice in group %q", card.Index, group.ID)
			}
			indices[card.Index] = struct{}{}
		}
	}

	return nil
}

// validatePodAllocationRecord rejects one pod's record before any of it is merged.
//
// The checks are ordered so the cheapest identity facts are read first, and each failure names the
// pod and the container it came from, because the whole point of holding is that an operator has to
// be able to find the thing that is wrong.
func validatePodAllocationRecord(pod *core.Pod, devs *workercore.Devices) error {
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		return fmt.Errorf("pod %s: read its allocation record: %w", ctrlcli.ObjectKeyFromObject(pod), err)
	}
	if allocations == nil {
		// A pod with no record at all is a pod that has not been allocated yet. That is a normal
		// state on its own, and the caller decides what it means; what must never happen is a
		// caller reading it as "this pod held nothing" for a pod that asked for a card.
		return nil
	}
	if pod.Spec.NodeName == "" {
		return fmt.Errorf("pod %s: holds an allocation record but is not on a node",
			ctrlcli.ObjectKeyFromObject(pod))
	}

	// The per-container records are read directly rather than through Aggregate: the aggregate is
	// exactly the fold that discards a container's own group identity, so validating it would be
	// validating the output of the thing under test.
	// An EXPLICIT NULL is distinguished from an absent entry before anything else. The decoded map
	// holds values, not pointers, so a null and an empty record are the same zero value by the time
	// they arrive; reading the raw annotation back is what keeps the difference visible.
	if null := nullContainersIn(pod); null != "" {
		return fmt.Errorf("pod %s container %q: allocation record is null",
			ctrlcli.ObjectKeyFromObject(pod), null)
	}

	for container, record := range allocations {
		if !podDeclaresContainer(pod, container) {
			return fmt.Errorf("pod %s container %q: holds an allocation record but declares no such container",
				ctrlcli.ObjectKeyFromObject(pod), container)
		}
		if err := validateRecordedClaims(pod, container, devs, record.Devices); err != nil {
			return err
		}
	}

	return nil
}

// nullContainersIn names the first container whose recorded value is an explicit JSON null.
func nullContainersIn(pod *core.Pod) string {
	raw, present := pod.Annotations[AllocatedAcceleratorAnnoKey]
	if !present || raw == "" {
		return ""
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		// A shape that is not an object at all is reported by the ordinary decode below, which has
		// the error text this package already uses.
		return ""
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if string(entries[name]) == "null" {
			return name
		}
	}

	return ""
}

// podDeclaresContainer reports whether the pod really has the container a record names. A record
// for a container that does not exist cannot be reconciled against anything.
func podDeclaresContainer(pod *core.Pod, container string) bool {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == container {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == container {
			return true
		}
	}

	return false
}

// validateRecordedClaims checks one container's recorded accelerators against the inventory.
//
// Aggregate() is not trusted to have preserved the manufacturer, so the groups are read from the
// pod's own record rather than from an aggregate, and every card is resolved in the inventory
// before any of it is merged.
func validateRecordedClaims(
	pod *core.Pod, container string, devs *workercore.Devices, status workercore.DevicesStatus,
) error {
	for gi := range status.Groups {
		group := &status.Groups[gi]
		spec := findDeviceGroup(devs, group.ID)
		if spec == nil {
			return fmt.Errorf("pod %s container %q: names accelerator group %q, which the inventory does not have",
				ctrlcli.ObjectKeyFromObject(pod), container, group.ID)
		}
		if group.Manufacturer != spec.Manufacturer {
			return fmt.Errorf(
				"pod %s container %q: group %q claims manufacturer %q, the inventory says %q",
				ctrlcli.ObjectKeyFromObject(pod), container, group.ID, group.Manufacturer, spec.Manufacturer)
		}

		seen := make(map[string]struct{}, len(group.Accelerators))
		for ai := range group.Accelerators {
			card := &group.Accelerators[ai]
			cardSpec := findDeviceCard(spec, card.ID)
			if cardSpec == nil {
				return fmt.Errorf(
					"pod %s container %q: names accelerator %q in group %q, which the inventory does not have",
					ctrlcli.ObjectKeyFromObject(pod), container, card.ID, group.ID)
			}
			if _, duplicate := seen[card.ID]; duplicate {
				return fmt.Errorf("pod %s container %q: names accelerator %q in group %q twice",
					ctrlcli.ObjectKeyFromObject(pod), container, card.ID, group.ID)
			}
			seen[card.ID] = struct{}{}
			if card.Index != cardSpec.Index {
				return fmt.Errorf(
					"pod %s container %q: accelerator %q in group %q records index %d, the inventory says %d",
					ctrlcli.ObjectKeyFromObject(pod), container, card.ID, group.ID, card.Index, cardSpec.Index)
			}
			if err := validateCardModeAndUnits(pod, container, card); err != nil {
				return err
			}
			if err := validateCardPlacement(pod, container, card); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateCardModeAndUnits rejects a mode this operator never writes, and units outside what a
// record can legitimately say.
//
// The modes that reach an annotation are the four the allocator writes. None and Visibility are
// rejected: None means "not allocated", which is the published state rather than a claim, and
// Visibility is an internal grant that carries no accounting at all. Units are capped rather than
// merely checked, because a container may legitimately ask for a fraction of a shared card, and
// the cap is what makes a larger recorded number read as the card's whole capacity.
func validateCardModeAndUnits(
	pod *core.Pod, container string, card *workercore.AcceleratorAllocation,
) error {
	switch card.Mode {
	case workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
		workercore.DeviceAllocationModePartitioned:
	default:
		return fmt.Errorf("pod %s container %q: accelerator %q records mode %q, which is not one this operator writes",
			ctrlcli.ObjectKeyFromObject(pod), container, card.ID, card.Mode.String())
	}

	if card.Allocated < 0 || card.Remaining < 0 {
		return fmt.Errorf("pod %s container %q: accelerator %q records negative units (allocated %d, remaining %d)",
			ctrlcli.ObjectKeyFromObject(pod), container, card.ID, card.Allocated, card.Remaining)
	}
	if limit := int32(nodefeature.ResourceMaxUnits); card.Allocated > limit {
		return fmt.Errorf(
			"pod %s container %q: accelerator %q records %d allocated units, more than the %d a card holds",
			ctrlcli.ObjectKeyFromObject(pod), container, card.ID, card.Allocated, limit)
	}
	// A SHARED SLICE'S UNIT BOUND IS THE CARD DIVIDED BY THE OWNER LIMIT. SharedResourceMaxSize is
	// how many owners a shared card admits, not how many units one of them may hold, and reading it
	// as a unit bound would reject every legitimate shared claim on a real card.
	if card.Mode == workercore.DeviceAllocationModeShared {
		if limit := int32(nodefeature.ResourceMaxUnits) / int32(nodefeature.SharedResourceMaxSize); card.Allocated > limit {
			return fmt.Errorf(
				"pod %s container %q: shared accelerator %q records %d units, more than the %d a shared slice holds",
				ctrlcli.ObjectKeyFromObject(pod), container, card.ID, card.Allocated, limit)
		}
	}

	return nil
}

// validateCardPlacement rejects a physical record that is half-written.
//
// A physical record is a profile together with the intervals its instance occupies, and neither
// half is one on its own. The profile names which slice and the placements say where it sits, so a
// record carrying one without the other cannot be folded and must not be folded as if it could.
// The placement is transport, not published output: it is validated as an input and never compared
// against a published row.
func validateCardPlacement(
	pod *core.Pod, container string, card *workercore.AcceleratorAllocation,
) error {
	key := ctrlcli.ObjectKeyFromObject(pod)
	switch {
	case card.AllocatedPhysicalProfile == "" && len(card.AllocatedPhysicalPlacements) == 0:
		return nil
	case card.AllocatedPhysicalProfile == "":
		return fmt.Errorf("pod %s container %q: accelerator %q records placement intervals but no profile",
			key, container, card.ID)
	case len(card.AllocatedPhysicalPlacements) == 0:
		return fmt.Errorf("pod %s container %q: accelerator %q records profile %q but no placement intervals",
			key, container, card.ID, card.AllocatedPhysicalProfile)
	default:
		return nil
	}
}

func findDeviceGroup(devs *workercore.Devices, id string) *workercore.DevicesGroup {
	for i := range devs.Spec.Groups {
		if devs.Spec.Groups[i].ID == id {
			return &devs.Spec.Groups[i]
		}
	}

	return nil
}

func findDeviceCard(group *workercore.DevicesGroup, id string) *workercore.Accelerator {
	for i := range group.Accelerators {
		if group.Accelerators[i].ID == id {
			return &group.Accelerators[i]
		}
	}

	return nil
}
