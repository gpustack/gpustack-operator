package nvidia

import (
	"errors"
	"fmt"
	"strings"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
)

// deriveSlicedProfiles turns a probed set of NVIDIA MIG GPU-instance profiles into the
// card's physical-slice profile inventory: it drops the +me (dedicated media engines)
// and +gfx (graphics-capable) variants, derives each kept profile's canonical name and
// slice geometry, and de-duplicates by name.
//
// infos holds one entry per successfully probed GI profile id; the caller skips ids that
// NVML reports as unsupported. cardMemoryMiB is the card's total memory in MiB, used to
// express a profile's memory size in memory-slice units on a card assumed to hold eight
// of them.
//
// placementsFor, when non-nil, returns a profile's full empty-card legal placement set to
// cache in Placements, keyed by the profile's own probed GI profile id (info.Id) — the
// authoritative id that distinguishes the same-compute-slice REV profiles (1g.5gb id 0 vs
// 1g.10gb id 7) a compute-slice count alone cannot. It is injected so this derivation
// stays hardware-free and unit-testable; detectMigProfiles passes a map-backed closure.
//
// The published memory-slice span comes from a placement's length whenever the driver
// enumerates one: every legal placement of a profile is exactly as long as that profile's
// span, making it the driver's own authoritative answer, whereas the memory division
// assumes an eight-slice card. The division survives as the fallback for a driver that
// enumerates no placement at all. The number of slices a card holds cannot be recovered
// from a profile's compute-slice count instead: the whole-card profile reports seven
// compute slices while occupying all eight memory ones. An absent placement set here
// always means "the driver enumerated none" — a profile whose placement query failed never
// reaches this derivation.
//
// Only a profile the driver itself named is published; a nameless one is dropped and named
// in the returned error, whose ids the caller reports. A published name becomes the
// profile's resource key, and the allocator resolves a requested key back to a vendor
// profile id by probing every id and comparing the driver's own names — through the very
// same accessor this detection uses. So "nameless here" implies "nameless there" for that
// id, on every driver: a name this derivation invented could never match, and the profile
// would be requestable, admitted, and then fail at allocation. Dropping it moves that
// failure to detection, where it is diagnosable, and forfeits no capacity that could ever
// have been allocated. The errors of all nameless ids are joined so one does not hide the
// rest.
func deriveSlicedProfiles(
	infos []nvml.GpuInstanceProfileInfo_v3,
	cardMemoryMiB uint64,
	placementsFor func(giProfileID uint32) []device.AcceleratorPhysicalPlacement,
) ([]device.AcceleratorPhysicalSlicedProfile, error) {
	perSlice := cardMemoryMiB / 8

	seen := make(map[string]struct{}, len(infos))
	var profiles []device.AcceleratorPhysicalSlicedProfile
	var errs []error
	for _, info := range infos {
		name := info.GetName()
		if name == "" {
			errs = append(errs, fmt.Errorf("profile %d: driver reported no name", info.Id))
			continue
		}
		if isMediaOrGraphicsVariant(info, name) {
			continue
		}

		// Round the memory size to whole memory slices: round(MemorySizeMB / perSlice).
		var memorySlices int32
		if perSlice > 0 {
			memorySlices = int32((info.MemorySizeMB + perSlice/2) / perSlice)
		}

		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}

		p := device.AcceleratorPhysicalSlicedProfile{
			Name:          name,
			MemoryMib:     int64(info.MemorySizeMB),
			ComputeSlices: int32(info.SliceCount),
			MemorySlices:  memorySlices,
			Count:         int32(info.InstanceCount),
		}
		if placementsFor != nil {
			p.Placements = placementsFor(info.Id)
			if len(p.Placements) > 0 && p.Placements[0].Length > 0 {
				p.MemorySlices = p.Placements[0].Length
			}
		}
		profiles = append(profiles, p)
	}
	return profiles, errors.Join(errs...)
}

// migPlacementsFromNVML converts NVML GPU-instance placement slots to the operator
// placement type. It returns nil for an empty input so a profile with no enumerated
// placements omits the field.
func migPlacementsFromNVML(slots []nvml.GpuInstancePlacement) []device.AcceleratorPhysicalPlacement {
	if len(slots) == 0 {
		return nil
	}
	out := make([]device.AcceleratorPhysicalPlacement, len(slots))
	for i := range slots {
		out[i] = device.AcceleratorPhysicalPlacement{
			Start:  int32(slots[i].Start),
			Length: int32(slots[i].Size),
		}
	}
	return out
}

// migPlacementsByProfile resolves every probed profile's empty-card legal placement set up
// front, and returns only the profiles whose query the driver actually answered, together
// with their placement sets keyed by the profile's own probed GI profile id.
//
// Separating a driver that enumerates no placement from a query that failed is the point:
// a lookup collapsing a failure to an empty set makes an unreadable card indistinguishable
// from a placement-free one, and the derivation would then publish a span from unverified
// geometry. A profile whose query failed is therefore withheld from the returned set — it
// could not be admitted without a placement set anyway — and named in the returned error,
// while a profile the driver answered with nothing is kept with a nil placement set. The
// errors of all failing ids are joined so one failure does not hide the rest.
//
// query is injected so the resolution stays hardware-free and unit-testable.
func migPlacementsByProfile(
	infos []nvml.GpuInstanceProfileInfo_v3,
	query func(giProfileID uint32) ([]nvml.GpuInstancePlacement, nvml.Return),
) ([]nvml.GpuInstanceProfileInfo_v3, map[uint32][]device.AcceleratorPhysicalPlacement, error) {
	answered := make([]nvml.GpuInstanceProfileInfo_v3, 0, len(infos))
	byID := make(map[uint32][]device.AcceleratorPhysicalPlacement, len(infos))
	var errs []error
	for _, info := range infos {
		slots, ret := query(info.Id)
		if !ret.IsSuccess() {
			errs = append(errs, fmt.Errorf("profile %d: %s", info.Id, ret.Error()))
			continue
		}
		answered = append(answered, info)
		byID[info.Id] = migPlacementsFromNVML(slots)
	}
	return answered, byID, errors.Join(errs...)
}

// isMediaOrGraphicsVariant reports whether a probed profile is a media-engine (+me,
// +me.all) or graphics (+gfx) variant, which GPUStack does not expose. Only the plain
// "<C>g.<M>gb" profiles are kept; every variant carries a "+..." suffix, so the presence
// of a "+" in the NVML Name is the discriminator — the profile ids are not a stable
// cross-generation taxonomy (on Hopper the base ids are the +me variants). The V3 GFX
// capability bit is a backstop for a graphics profile whose naming differs. The name is
// always populated here: a nameless profile is dropped before the classification, since
// it cannot be published at all.
func isMediaOrGraphicsVariant(info nvml.GpuInstanceProfileInfo_v3, name string) bool {
	return strings.ContainsRune(name, '+') ||
		info.Capabilities&nvml.GPU_INSTANCE_PROFILE_CAPS_GFX != 0
}

// detectMigProfiles probes every GPU instance profile id on the device and returns the card's
// physical-slice profile inventory (filtered and derived by deriveSlicedProfiles). Unsupported
// ids surface as non-success returns and are skipped.
func detectMigProfiles(dev nvml.Device, cardMemoryMiB uint64) []device.AcceleratorPhysicalSlicedProfile {
	var infos []nvml.GpuInstanceProfileInfo_v3
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			continue
		}
		infos = append(infos, info)
	}

	// Cache each profile's empty-card legal placement set at detect time so the reconciler
	// can derive RemainingProfiles by pure arithmetic (subtracting annotation-derived occupied)
	// without any per-reconcile NVML. Queried by the profile's own probed id, which
	// distinguishes the same-compute-slice REV profiles a slice count cannot.
	//
	// A profile whose placement query failed is withheld rather than published from an
	// unverified geometry. The failure names the card and the profile ids so the capacity
	// missing from the inventory is diagnosable instead of merely absent.
	answered, placementsByID, err := migPlacementsByProfile(infos, dev.GetGpuInstancePossiblePlacements)
	if err != nil {
		uuid, _ := dev.GetUUID()
		klog.Background().Error(err, "Dropped MIG profiles whose possible placements are unreadable",
			"device", uuid)
	}

	placementsFor := func(giProfileID uint32) []device.AcceleratorPhysicalPlacement {
		return placementsByID[giProfileID]
	}

	// A profile the driver did not name is dropped rather than published under an invented
	// name the allocator's name probe could never resolve. The rejection names the card and
	// the profile ids so the omitted capacity is diagnosable instead of merely absent.
	profiles, err := deriveSlicedProfiles(answered, cardMemoryMiB, placementsFor)
	if err != nil {
		uuid, _ := dev.GetUUID()
		klog.Background().Error(err, "Dropped MIG profiles the driver did not name",
			"device", uuid)
	}
	return profiles
}

// maxProfileCount returns the card's physical-slice ceiling — the largest per-profile Count
// (e.g. 7 on A100, from 7x 1g.5gb). Zero for an empty profile list.
func maxProfileCount(profiles []device.AcceleratorPhysicalSlicedProfile) int32 {
	var ceiling int32
	for _, p := range profiles {
		if p.Count > ceiling {
			ceiling = p.Count
		}
	}
	return ceiling
}
