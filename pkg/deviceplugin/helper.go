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

// Logical-slicing host paths. The device-manager init container stages the in-image
// /etc/gpustack/lib tree onto the host at OperatorLibDir; per-container working
// directories live under OperatorPodsDir. They are package vars (not consts) so
// tests can redirect them to a temporary directory.
var (
	OperatorLibDir  = "/var/lib/gpustack/operator/lib"
	OperatorPodsDir = "/var/lib/gpustack/operator/pods"
)

// PodWorkDir returns the host working directory for a sliced container,
// <OperatorPodsDir>/<podUID>/c-<container>.
func PodWorkDir(podUID, containerName string) string {
	return filepath.Join(OperatorPodsDir, podUID, "c-"+containerName)
}

type Resource struct {
	// Group is the ID of the devices group.
	Group string
	// Device is the ID of the device.
	Device string
}

func (in Resource) String() string {
	return in.Group + ":" + in.Device
}

// GetDeviceIds returns the device IDs one card advertises for mode. poolSize is the card's
// own token count for the pooled modes (Sliced, Partitioned) and is ignored by the others;
// a non-positive poolSize advertises no tokens at all. A mode with no token shape of its own
// advertises nothing.
func (in Resource) GetDeviceIds(mode workercore.DeviceAllocationMode, poolSize int32) []string {
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

// SlicedCoresPercent reads the per-card compute (SM) percent a sliced container asks
// for from its ".sliced.cores-percentage" request, defaulting to 100 (a whole card's
// compute) when absent or non-positive. Slices may oversubscribe compute, so each
// slice may request up to 100%. It drives HAMi-core CUDA_DEVICE_SM_LIMIT / vcann-rt
// aicore-quota directly (already a percent, no fraction conversion).
func SlicedCoresPercent(ctr *core.Container, coresResName core.ResourceName) int {
	if q, ok := ctr.Resources.Limits[coresResName]; ok {
		if v := q.Value(); v > 0 {
			return int(v)
		}
	}
	return 100
}

// SlicedMemoryMib derives the per-card VRAM limit (MiB) a sliced container asks for,
// given the card's total VRAM (cardVRAMMib). ".sliced.memory-percentage" takes
// precedence — floor(pct/100 * cardVRAMMib) — over the absolute ".sliced.memory-mib";
// the result is capped at the card's VRAM. It errors when neither is set, so a sliced
// allocate fails loudly rather than silently exposing the whole card's VRAM. Memory is
// the non-oversubscribable anchor, so percentage wins here exactly as the Pod webhook
// folds it into ".sliced.units" — keeping the credit accounting and the real limit
// consistent.
func SlicedMemoryMib(ctr *core.Container, memPctResName, memMibResName core.ResourceName, cardVRAMMib int64) (int64, error) {
	if q, ok := ctr.Resources.Limits[memPctResName]; ok {
		if pct := q.Value(); pct > 0 {
			return min(cardVRAMMib*pct/100, cardVRAMMib), nil
		}
	}
	if q, ok := ctr.Resources.Limits[memMibResName]; ok {
		if mib := q.Value(); mib > 0 {
			return min(mib, cardVRAMMib), nil
		}
	}
	return 0, fmt.Errorf("container %q has no %s or %s request", ctr.Name, memPctResName, memMibResName)
}

// ContainerEnvDeclared reports whether the container explicitly declares an env var named
// name in its Env list, so a caller can skip injecting a default and leave that value
// authoritative. Only explicit Env entries are checked; EnvFrom-sourced variables are not.
func ContainerEnvDeclared(ctr *core.Container, name string) bool {
	for i := range ctr.Env {
		if ctr.Env[i].Name == name {
			return true
		}
	}
	return false
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
