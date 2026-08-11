package deviceplugin

import (
	"fmt"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// Resource is how Kubernetes sees one Device: the devices group it was detected in, plus its own
// ID within that group.
//
//   - It is the device-plugin and controller vocabulary. The Device Manager manages Devices;
//     Kubernetes consumes them as Resources. See the package doc of pkg/device for the full
//     layering.
//   - It names a Device, not an Accelerator. Every Device this package serves today is an
//     Accelerator, but the layering puts other devices — an InfiniBand port, a link port —
//     beside them, and they will reach Kubernetes through this same type. Narrowing the field
//     to the subtype in use today would have to be undone to admit them.
//   - Every per-device ledger in this package is keyed by it, so it must remain a comparable
//     value type: adding a slice, map or pointer field would make it illegal as a map key and
//     break those ledgers at compile time.
type Resource struct {
	// Group is the ID of the devices group.
	Group string
	// Device is the ID of the device within that group.
	Device string
}

// String renders the device as "<group>:<device>". It names a whole device, never a token on one
// — ResourceToken.String appends the index that makes a device ID kubelet can match.
func (in Resource) String() string {
	return in.Group + ":" + in.Device
}

// DeviceIDs returns the device IDs one accelerator advertises for mode. poolSize is the
// accelerator's own token count for the pooled modes (Sliced, Partitioned) and is ignored by the
// others; a non-positive poolSize advertises no tokens at all. A mode with no token shape of its
// own advertises nothing.
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
		// Visibility advertises, per accelerator, a flat pool sized to SlicedResourceMaxSize —
		// the most slices (hence co-allocating containers) an accelerator can host — so one can
		// always co-allocate visibility to the accelerator its owner was placed on. The tokens
		// are interchangeable: the visibility Allocate ignores which token kubelet picked and
		// reads the pod's reserved device instead, so this never gates scheduling and consumes
		// no ledger unit.
		devIDs := make([]string, nodefeature.SlicedResourceMaxSize)
		for i := 0; i < nodefeature.SlicedResourceMaxSize; i++ {
			devIDs[i] = str + padIndex(uint64(i))
		}
		return devIDs

	case workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModePartitioned:
		// Both pooled modes advertise a flat pool of interchangeable per-accelerator tokens, one
		// per possible concurrent claim on the accelerator, differing only in how the caller
		// sizes it.
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
// width is shared by the IDs DeviceIDs advertises and the ones ResourceToken.String names, and
// kubelet matches the exact string it offered — so the two must never disagree on it.
func padIndex(idx uint64) string {
	return strconvx.PadZeroUint(idx, 4)
}

// ResourceToken identifies one allocatable token on one accelerator: a Resource plus the token's
// index within that accelerator's advertisement. It is what a kubelet device ID parses into, and
// what renders back to one.
type ResourceToken struct {
	Resource

	// Index is what distinguishes this token from the accelerator's other ones. What it counts
	// depends on the allocation mode:
	//
	//   - Sliced, Partitioned and Visibility advertise interchangeable tokens, so the index is a
	//     position in that pool and nothing more: it tells concurrent claims on the accelerator
	//     apart and carries no hardware meaning.
	//   - Shared advertises one token per owner, and the index is a credit offset into the
	//     accelerator's denominator: it steps by ResourceMaxUnits / SharedResourceMaxSize
	//     (160000), so consecutive Shared tokens are 160000 apart rather than 1.
	Index uint64
}

// String returns the device ID naming this token, "<group>:<device>:<index>" with the index
// zero-padded exactly as DeviceIDs advertises it. A token names one claim on one device, so it
// must carry the index: without it the result is a bare device, which ParseResourceToken rejects
// and kubelet cannot match against the tokens it offered.
func (in ResourceToken) String() string {
	return in.Resource.String() + ":" + padIndex(in.Index)
}

// ParseResourceToken parses a device ID into a ResourceToken.
func ParseResourceToken(id string) (ResourceToken, error) {
	ps := strings.Split(id, ":")
	if len(ps) != 3 {
		return ResourceToken{}, fmt.Errorf("invalid device ID format: %q", id)
	}
	if ps[0] == "" || ps[1] == "" {
		return ResourceToken{}, fmt.Errorf("group/device cannot be empty: %q", id)
	}
	idx, err := strconv.ParseUint(ps[2], 10, 64)
	if err != nil {
		return ResourceToken{}, fmt.Errorf("invalid index: %q", ps[2])
	}
	return ResourceToken{
		Resource: Resource{
			Group:  ps[0],
			Device: ps[1],
		},
		Index: idx,
	}, nil
}
