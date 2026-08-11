package thead

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// The capability is resolved by a runtime type assertion, so implementing only half of it is not a
// build error — it is a partition family that silently stops being served, reported as a responder
// that cannot actuate. Assert it here to make that a compile error instead.
var _ deviceplugin.PhysicalSlicedResponder = (*server)(nil)

// GetPhysicalSlicedVisibilityResponse returns the device specifications showing the partitions the
// owner container already holds on the allocated accelerators, for the container co-allocating
// them. The identity comes from the owner's on-disk ownership markers — the record that survives a
// device-manager restart — and each one is proven to still describe a live partition before its
// nodes are injected.
//
// The response carries device nodes rather than an environment variable: this vendor has no
// container-runtime hook, so the nodes are the whole of the container's access. The co-allocating
// container is given exactly what the owner's own response carried — the shared control nodes once,
// then each accelerator's own node and the capability nodes of its partition's GPU and compute
// instances — resolved through the same helpers the actuator uses, and in the same accelerator
// order, so the two responses are assembled the same way and read the same.
//
// It fails closed on anything it cannot prove. Naming the parent accelerator instead would grant the
// co-allocating container every partition carved on it, including other tenants'.
func (s *server) GetPhysicalSlicedVisibilityResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	owner string,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if s.mig == nil {
		return nil, fmt.Errorf("mig driver not configured; cannot address the partition container %q holds", owner)
	}

	// Resolve in devs order — the order the owner's own response used — so the co-allocating
	// container's device set is identical to the owner's, accelerator for accelerator.
	cards := allocatedAccelerators(devs, allocated)
	if len(cards) == 0 {
		return nil, fmt.Errorf("no allocated card for visibility container %q", ctr.Name)
	}

	// The shared control nodes are needed by every accelerator's partition and are per container
	// rather than per accelerator, so they are verified once, up front.
	sharedPaths := sharedControlNodePaths()
	devices := make([]*deviceplugin.DeviceSpec, 0, len(sharedPaths)+3*len(cards))
	for _, path := range sharedPaths {
		spec, err := requireDeviceNode(path)
		if err != nil {
			return nil, err
		}
		devices = append(devices, spec)
	}

	for _, cardUUID := range cards {
		// The accelerator's card ordinal names both its device node and its procfs capability subtree,
		// and it is proven to reach the accelerator the detector measured before either is built —
		// through the same guard the actuator cleared, so this response addresses the accelerator exactly
		// as the owner's own did. Addressing an unproven ordinal would show this container a neighboring
		// accelerator's partition, which is the very isolation this response exists to keep.
		ordinal, cardNode, err := requireCardNode(devs, cardUUID)
		if err != nil {
			return nil, err
		}

		inst, err := s.liveOwnedInstance(devs, string(pod.UID), owner, cardUUID)
		if err != nil {
			return nil, err
		}

		specs, derr := partitionDeviceSpecs(ordinal, cardNode, inst)
		if derr != nil {
			return nil, fmt.Errorf("card %s partition device nodes: %w", cardUUID, derr)
		}
		devices = append(devices, specs...)
	}

	return &deviceplugin.ContainerAllocateResponse{Devices: devices}, nil
}

// liveOwnedInstance returns the partition the owner container holds on the accelerator cardUUID:
// read from its ownership marker, then verified against that accelerator's live state. A missing,
// malformed, wrong-accelerator, unknown-profile, dead or id-reused record is an error, never a
// fallback.
//
// The instance returned is the marker's own, not the driver's: the driver enumerates GPU instances,
// which cannot report the compute instance inside one, so the seam's live record carries no
// compute-instance id at all and the durable marker is the only source of it. The driver decides
// whether that record is still true — the identity string is cross-checked against the live
// instance — while the record supplies the ids the device nodes are keyed by.
func (s *server) liveOwnedInstance(devs *workercore.Devices, podUID, owner, cardUUID string) (migInstance, error) {
	path := markerPath(deviceplugin.OperatorPodsDir, podUID, owner, cardUUID)
	m, err := parseMarker(path)
	if err != nil {
		return migInstance{}, fmt.Errorf("read the partition ownership marker of container %q on card %s: %w",
			owner, cardUUID, err)
	}
	if m.Card != cardUUID {
		return migInstance{}, fmt.Errorf("marker %q records card %s, not %s: fail closed", path, m.Card, cardUUID)
	}
	// The accelerator's own detect-time capability supplies the geometry the state read needs; an
	// accelerator that no longer offers the recorded profile cannot be asked about it, so fail closed
	// rather than guess.
	computeSlices, memorySlices, ok := profileGeometry(devs, cardUUID, m.Profile)
	if !ok {
		return migInstance{}, fmt.Errorf("card %s no longer offers the profile %q recorded by marker %q: fail closed",
			cardUUID, m.Profile, path)
	}

	// Snapshot the accelerator's state under its lock — the same critical section reserveMigInstance
	// takes — so a concurrent create or reclaim cannot tear this one read. The checks below run on
	// the snapshot, outside the lock: they can only turn a still-live partition into a rejection,
	// never the reverse.
	unlock := lockCard(cardUUID)
	state, err := s.mig.CardState(cardUUID, m.Profile, computeSlices, memorySlices)
	unlock()
	if err != nil {
		return migInstance{}, fmt.Errorf("read card %s state: %w", cardUUID, err)
	}

	liveInst, ok := findLiveInstance(state, m.GiID)
	if !ok {
		return migInstance{}, fmt.Errorf("marker %q references missing gpu instance %d on card %s: fail closed",
			path, m.GiID, cardUUID)
	}
	// The same identity guard the reservation and reclaim paths apply: a destroyed GPU instance's id
	// can be reassigned, so an identity that no longer matches means the marker names somebody
	// else's partition — or none at all.
	if liveInst.UUID != m.MigUUID {
		return migInstance{}, fmt.Errorf(
			"marker %q gpu instance %d uuid %q no longer matches live uuid %q (id reused): fail closed",
			path, m.GiID, m.MigUUID, liveInst.UUID)
	}
	return m.instance(), nil
}
