package deviceplugin

import (
	"fmt"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// Resource identifies one physical accelerator card by the devices group it was detected in
// and its own ID within that group. Every per-card ledger in this package is keyed by it, so
// it must remain a comparable value type: adding a slice, map or pointer field would make it
// illegal as a map key and break those ledgers at compile time.
type Resource struct {
	// Group is the ID of the devices group.
	Group string
	// Device is the ID of the device.
	Device string
}

// String renders the card as "<group>:<device>". It names a whole card, never a token on one —
// ResourceUnit.String appends the index that makes a device ID kubelet can match.
func (in Resource) String() string {
	return in.Group + ":" + in.Device
}

// DeviceIDs returns the device IDs one card advertises for mode. poolSize is the card's
// own token count for the pooled modes (Sliced, Partitioned) and is ignored by the others;
// a non-positive poolSize advertises no tokens at all. A mode with no token shape of its own
// advertises nothing.
func (in Resource) DeviceIDs(mode workercore.DeviceAllocationMode, poolSize int32) []string {
	str := in.String() + ":"

	switch mode {
	case workercore.DeviceAllocationModeExclusive:
		return []string{str + "0000"}

	case workercore.DeviceAllocationModeShared:
		// One device ID per shared owner; indices step by D/maxOwners units.
		const step = uint64(nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize)
		devIDs := make([]string, 0, nodefeature.SharedResourceMaxSize)
		for i := uint64(0); i < nodefeature.ResourceMaxUnits; i += step {
			devIDs = append(devIDs, str+padIndex(i))
		}
		return devIDs

	case workercore.DeviceAllocationModeVisibility:
		// Visibility advertises, per card, a flat pool sized to SlicedResourceMaxSize — the
		// most slices (hence co-allocating containers) a card can host — so one can always
		// co-allocate visibility to the card its owner was placed on. The tokens are
		// interchangeable:
		// the visibility Allocate ignores which token kubelet picked and reads the pod's
		// reserved device instead, so this never gates scheduling and consumes no ledger unit.
		devIDs := make([]string, nodefeature.SlicedResourceMaxSize)
		for i := 0; i < nodefeature.SlicedResourceMaxSize; i++ {
			devIDs[i] = str + padIndex(uint64(i))
		}
		return devIDs

	case workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModePartitioned:
		// Both pooled modes advertise a flat pool of interchangeable per-card tokens, one per
		// possible concurrent claim on the card, differing only in how the caller sizes it.
		// Sliced is a coarse, loose injection-token pool that only needs to be >= the real max
		// concurrency so it never gates scheduling — the binding constraint is the
		// ".sliced.units" capacity the Pod webhook folds each slice's memory request into.
		if poolSize <= 0 {
			return nil
		}
		devIDs := make([]string, poolSize)
		for i := int32(0); i < poolSize; i++ {
			devIDs[i] = str + padIndex(uint64(i))
		}
		return devIDs
	}

	return nil
}

// padIndex renders a token index in the fixed four-digit form every device ID carries. The
// width is shared by the IDs DeviceIDs advertises and the ones ResourceUnit.String names, and
// kubelet matches the exact string it offered — so the two must never disagree on it.
func padIndex(idx uint64) string {
	return strconvx.PadZeroUint(idx, 4)
}

// ResourceUnit identifies one allocatable token on one card: a Resource plus the token's index
// within that card's advertised pool. The index is a position in that pool, not a hardware
// address — the pooled modes advertise interchangeable tokens, so which index a container is
// handed distinguishes concurrent claims on the card and means nothing beyond that.
type ResourceUnit struct {
	Resource

	// Index is the logic unit of the resource.
	Index uint64
}

// String returns the device ID naming this unit, "<group>:<device>:<index>" with the index
// zero-padded exactly as DeviceIDs advertises it. A unit names one token of one card, so it must
// carry the index: without it the result is a bare card, which ParseResourceUnit
// rejects and kubelet cannot match against the tokens it offered.
func (in ResourceUnit) String() string {
	return in.Resource.String() + ":" + padIndex(in.Index)
}

// ParseResourceUnit parses a device ID into a ResourceUnit.
func ParseResourceUnit(id string) (ResourceUnit, error) {
	ps := strings.Split(id, ":")
	if len(ps) != 3 {
		return ResourceUnit{}, fmt.Errorf("invalid device ID format: %q", id)
	}
	if ps[0] == "" || ps[1] == "" {
		return ResourceUnit{}, fmt.Errorf("group/device cannot be empty: %q", id)
	}
	idx, err := strconv.ParseUint(ps[2], 10, 64)
	if err != nil {
		return ResourceUnit{}, fmt.Errorf("invalid index: %q", ps[2])
	}
	return ResourceUnit{
		Resource: Resource{
			Group:  ps[0],
			Device: ps[1],
		},
		Index: idx,
	}, nil
}
