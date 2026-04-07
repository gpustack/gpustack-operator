package deviceplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

const (
	MaxUnits = devicefeature.ResourceMaxUnits

	_MaxSizeInExclusive = 1
	_StepInExclusive    = MaxUnits / _MaxSizeInExclusive

	_MaxSizeInShared = devicefeature.SharedResourceMaxSize
	_StepInShared    = MaxUnits / _MaxSizeInShared

	_MaxSizeInPartitioned  = devicefeature.SlicedResourceMaxSize
	_MinUnitsInPartitioned = MaxUnits / _MaxSizeInPartitioned
	_StepInPartitioned     = 1
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

func (in Resource) GetDeviceIds(mode workercore.DeviceAllocationMode) []string {
	str := in.String() + ":"

	if mode == workercore.DeviceAllocationModeExclusive {
		return []string{str + "0000"}
	}

	if mode == workercore.DeviceAllocationModeShared {
		devIDs := make([]string, 0, MaxUnits/_StepInShared)
		for i := uint64(0); i < MaxUnits; i += _StepInShared {
			devIDs = append(devIDs, str+padIndex(i))
		}
		return devIDs
	}

	devIDs := make([]string, MaxUnits/_StepInPartitioned)
	for i := uint64(0); i < MaxUnits; i += _StepInPartitioned {
		devIDs[i] = str + padIndex(i)
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

// PadPartitionedAllocationSize pads the allocation size to the nearest power of two up to max partitions.
func PadPartitionedAllocationSize(allocationSize, maxPartitions int32) int32 {
	powers := devicefeature.PowersOfTwoUpTo(maxPartitions)
	if powers[len(powers)-1] == maxPartitions {
		powers = powers[:len(powers)-1]
	}
	for i := range powers {
		expectedAllocationSize := _MinUnitsInPartitioned * powers[i]
		if allocationSize <= expectedAllocationSize {
			return expectedAllocationSize
		}
	}
	return MaxUnits
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
