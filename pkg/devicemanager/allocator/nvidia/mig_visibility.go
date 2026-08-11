package nvidia

import (
	"context"
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// The capability is resolved by a runtime type assertion, so implementing only half of it is not a
// build error — it is a partition family that silently stops being served, reported as a responder
// that cannot actuate. Assert it here to make that a compile error instead.
var _ deviceplugin.PhysicalSlicedResponder = (*server)(nil)

// GetPhysicalSlicedVisibilityResponse names the MIG partitions the owner container already holds
// on the allocated accelerators, for the container co-allocating them. The identity comes from the
// owner's on-disk ownership markers — the record that survives a device-manager restart — and
// each one is proven to still describe a live partition before it is injected.
//
// It fails closed on anything it cannot prove. A visibility allocation is a device-cgroup grant
// and nothing else, so naming the parent accelerator instead would open every partition carved on
// it, including other tenants'.
func (s *server) GetPhysicalSlicedVisibilityResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	owner string,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if s.mig == nil {
		return nil, fmt.Errorf("mig driver not configured; cannot name the partition container %q holds", owner)
	}

	// Resolve in devs order — the order the owner's own response used — so the co-allocating
	// container's NVIDIA_VISIBLE_DEVICES is identical to the owner's, accelerator for accelerator.
	cards := allocatedAccelerators(devs, allocated)
	if len(cards) == 0 {
		return nil, fmt.Errorf("no allocated card for visibility container %q", ctr.Name)
	}

	uuids := make([]string, 0, len(cards))
	for _, cardUUID := range cards {
		uuid, err := s.livePartitionUUID(devs, string(pod.UID), owner, cardUUID)
		if err != nil {
			return nil, err
		}
		uuids = append(uuids, uuid)
	}

	return &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": strings.Join(uuids, ",")},
	}, nil
}

// livePartitionUUID returns the MIG-device UUID the owner container holds on the accelerator: read
// from its ownership marker, then verified against the accelerator's live state. A missing,
// malformed, wrong-accelerator, profile-less or dead record is an error, never a fallback.
func (s *server) livePartitionUUID(devs *workercore.Devices, podUID, owner, cardUUID string) (string, error) {
	path := markerPath(podUID, owner, cardUUID)
	m, err := parseMarker(path)
	if err != nil {
		return "", fmt.Errorf("read the partition ownership marker of container %q on card %s: %w",
			owner, cardUUID, err)
	}
	if m.Card != cardUUID {
		return "", fmt.Errorf("marker %q records card %s, not %s: fail closed", path, m.Card, cardUUID)
	}
	// The accelerator's own detect-time capability supplies the geometry the state read needs; an
	// accelerator that no longer offers the recorded profile cannot be asked about it, so fail
	// closed rather than guess.
	computeSlices, memorySlices, ok := profileGeometry(devs, cardUUID, m.Profile)
	if !ok {
		return "", fmt.Errorf("card %s no longer offers the profile %q recorded by marker %q: fail closed",
			cardUUID, m.Profile, path)
	}

	// Snapshot the accelerator's state under the accelerator lock — the same critical section
	// reserveMigInstance takes — so a concurrent create or reclaim cannot tear this one read. The
	// checks below run on the snapshot, outside the lock: they can only turn a still-live partition
	// into a rejection, never the reverse, and a reclaim landing right after the unlock is the
	// window the spec already records rather than one this read introduces.
	unlock := lockCard(cardUUID)
	state, err := s.mig.CardState(cardUUID, m.Profile, computeSlices, memorySlices)
	unlock()
	if err != nil {
		return "", fmt.Errorf("read card %s state: %w", cardUUID, err)
	}

	liveInst, ok := findLiveInstance(state, m.GiID)
	if !ok {
		return "", fmt.Errorf("marker %q references missing gpu instance %d on card %s: fail closed",
			path, m.GiID, cardUUID)
	}
	// The same identity guard the create's retry path applies: a destroyed GPU instance's id can
	// be reassigned, so a UUID that no longer matches means the marker names somebody else's
	// partition — or none at all.
	if liveInst.UUID != m.MigUUID {
		return "", fmt.Errorf("marker %q gpu instance %d uuid %q no longer matches live uuid %q (id reused): fail closed",
			path, m.GiID, m.MigUUID, liveInst.UUID)
	}
	return m.MigUUID, nil
}
