package deviceplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

type Resource struct {
	// Group is the ID of the devices group.
	Group string
	// Device is the ID of the device.
	Device string
}

func (in Resource) String() string {
	return in.Group + ":" + in.Device
}

func (in Resource) GetDeviceIds(mode workercore.DeviceAllocationMode, maxPartitions int32) []string {
	str := in.String() + ":"

	if mode == workercore.DeviceAllocationModeExclusive {
		return []string{str + "0000"}
	}

	if mode == workercore.DeviceAllocationModeShared {
		// One device ID per shared owner; indices step by D/maxOwners units.
		const step = uint64(nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize)
		devIDs := make([]string, 0, nodefeature.SharedResourceMaxSize)
		for i := uint64(0); i < nodefeature.ResourceMaxUnits; i += step {
			devIDs = append(devIDs, str+padIndex(i))
		}
		return devIDs
	}

	// Sliced advertises a coarse, loose injection-token pool sized by the card's
	// hardware MaxPartitions. It only needs to be >= the real max concurrency (the
	// admin partition count N, which the Webhook bounds to <= MaxPartitions) so it
	// never blocks; the binding constraint is always the ".sliced.units" capacity.
	n := maxPartitions
	if n < 1 {
		n = 1
	}
	devIDs := make([]string, n)
	for i := int32(0); i < n; i++ {
		devIDs[i] = str + padIndex(uint64(i))
	}
	return devIDs
}

func padIndex(idx uint64) string {
	idxStr := strconv.FormatUint(idx, 10)
	switch {
	case idx < 10:
		return "000" + idxStr
	case idx < 100:
		return "00" + idxStr
	case idx < 1000:
		return "0" + idxStr
	default:
		return idxStr
	}
}

// PadSlicedUnits rounds a raw ".sliced.units" request up to the nearest whole-slice
// boundary D/2^k that a card of maxPartitions can physically provide:
//   - the result is the smallest D/2^k >= units, with 2^k in [1, maxPartitions];
//   - a request finer than the hardware (units < D/maxPartitions) rounds up to the
//     finest hardware slice D/maxPartitions;
//   - a request of a whole card or larger caps at D.
//
// It lets the sliced injection allocator map an arbitrary, unvalidated raw-Pod
// ".sliced.units" request onto a real partition without a Pod admission webhook.
func PadSlicedUnits(units, maxPartitions int64) int64 {
	const d = nodefeature.ResourceMaxUnits
	if units >= d {
		return d
	}
	// Floor maxPartitions to a power of two (the finest slice the hardware offers).
	maxP := int64(1)
	for maxP<<1 <= maxPartitions {
		maxP <<= 1
	}
	if units <= d/maxP {
		return d / maxP
	}
	// Walk slice sizes from finest (maxP) to whole card (1); the first boundary
	// D/p that covers the request is the padded upper bound.
	for p := maxP; p >= 1; p >>= 1 {
		if v := d / p; v >= units {
			return v
		}
	}
	return d
}

// SliceRatio derives the per-card fraction R = units / D from a container's
// ".sliced.units" request, where D = nodefeature.ResourceMaxUnits. It is the single
// source for every soft-slice quota (the compute percent and each per-card memory
// limit). A missing or non-positive request is an error: a sliced allocate must fail
// loudly rather than silently expose the whole card.
func SliceRatio(ctr *core.Container, unitsResName core.ResourceName) (float64, error) {
	q, ok := ctr.Resources.Limits[unitsResName]
	if !ok {
		return 0, fmt.Errorf("container %q has no %s request", ctr.Name, unitsResName)
	}
	units := q.Value()
	if units <= 0 {
		return 0, fmt.Errorf("container %q has non-positive %s request: %d", ctr.Name, unitsResName, units)
	}
	return float64(units) / float64(nodefeature.ResourceMaxUnits), nil
}

// FloorPercent converts a per-card fraction R into the integer compute percent the
// soft-slicing runtimes expect (HAMi-core CUDA_DEVICE_SM_LIMIT, vcann-rt aicore-quota),
// rounding down: floor(R*100).
func FloorPercent(r float64) int {
	return int(r * 100)
}

type ResourceUnit struct {
	Resource

	// Index is the logic unit of the resource.
	Index uint64
}

type ResourceRange struct {
	Resource

	// Start is the starting index of the resource range, inclusive.
	Start uint64
	// End is the ending index of the resource range, exclusive.
	End uint64
}

// Length returns the length of the resource range,
// which is the number of resource units in the range.
func (in ResourceRange) Length() uint64 {
	if in.End <= in.Start {
		return 0
	}
	return in.End - in.Start
}

// CountDeviceIds returns the device IDs in the resource range up to the specified size,
// and a boolean indicating whether the resource range has enough devices to meet the size requirement.
func (in ResourceRange) CountDeviceIds(size uint64) ([]string, bool) {
	str := in.String() + ":"
	devIDs := make([]string, min(in.Length(), size))
	for i := uint64(0); i < min(in.Length(), size); i++ {
		devIDs[i] = str + padIndex(in.Start+i)
	}
	return devIDs, in.Length() >= size
}

// GetResourceRangeFromResourceUnits converts a list of ResourceUnit to a list of ResourceRange
// by merging adjacent indexes into ranges.
func GetResourceRangeFromResourceUnits(resUnits []ResourceUnit) (resRanges []ResourceRange) {
	// Merge adjacent device indexes into a range to minimize the number of calls.
	resIndexes := make(map[Resource][]uint64)
	for i := range resUnits {
		resUnit := &resUnits[i]
		resIndexes[resUnit.Resource] = append(resIndexes[resUnit.Resource], resUnit.Index)
	}
	for res, indexes := range resIndexes {
		// Sort the indexes to ensure adjacent ones are next to each other.
		sort.Slice(indexes, func(i, j int) bool {
			return indexes[i] < indexes[j]
		})
		start := indexes[0]
		prev := start
		for _, idx := range indexes[1:] {
			if idx == prev+1 {
				prev = idx
				continue
			}
			resRanges = append(resRanges, ResourceRange{
				Resource: res,
				Start:    start,
				End:      prev + 1,
			})
			start = idx
			prev = idx
		}
		resRanges = append(resRanges, ResourceRange{
			Resource: res,
			Start:    start,
			End:      prev + 1,
		})
	}
	return resRanges
}

// ConvertResourceUnitFromDeviceIds converts the device ID to a ResourceUnit.
func ConvertResourceUnitFromDeviceIds(id string) (ResourceUnit, error) {
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

// NewDevice creates a DeviceSpec with the given path and permissions.
func NewDevice(path, permissions string) *DeviceSpec {
	if !osx.Exists(path) {
		return nil
	}
	return &DeviceSpec{
		ContainerPath: path,
		HostPath:      path,
		Permissions:   permissions,
	}
}

// NewRWDevice creates a DeviceSpec with read-write permissions.
func NewRWDevice(path string) *DeviceSpec {
	return NewDevice(path, "rw")
}

// NewRWDevicef creates a DeviceSpec with read-write permissions and a formatted path.
func NewRWDevicef(format string, args ...any) *DeviceSpec {
	return NewDevice(fmt.Sprintf(format, args...), "rw")
}

// NewDevicesIn creates a list of DeviceSpec for all files in the specified directory with the given permissions.
func NewDevicesIn(dir, permissions string) []*DeviceSpec {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	devs := make([]*DeviceSpec, 0, len(entries))
	for i := range entries {
		if entries[i].IsDir() {
			continue
		}
		path := filepath.Join(dir, entries[i].Name())
		devs = append(devs, &DeviceSpec{
			ContainerPath: path,
			HostPath:      path,
			Permissions:   permissions,
		})
	}

	return devs
}

// NewRWDevicesIn creates a list of DeviceSpec with read-write permissions for all files in the specified directory.
func NewRWDevicesIn(dir string) []*DeviceSpec {
	return NewDevicesIn(dir, "rw")
}

// NewMount creates a Mount with the given path and permissions.
func NewMount(path string, readOnly bool) *Mount {
	if !osx.Exists(path) {
		return nil
	}
	return &Mount{
		ContainerPath: path,
		HostPath:      path,
		ReadOnly:      readOnly,
	}
}

// NewROMount creates a Mount with read-only permissions.
func NewROMount(path string) *Mount {
	return NewMount(path, true)
}

// NewROMountf creates a Mount with read-only permissions and a formatted path.
func NewROMountf(format string, args ...any) *Mount {
	return NewMount(fmt.Sprintf(format, args...), true)
}
