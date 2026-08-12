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
// accelerator's physical-slice profile inventory: it drops the +me (dedicated media engines)
// and +gfx (graphics-capable) variants, derives each kept profile's canonical name and
// slice geometry, and de-duplicates by name.
//
// infos holds one entry per successfully probed GI profile id; the caller skips ids that
// NVML reports as unsupported.
//
// placementsFor returns a profile's full empty-accelerator legal placement set, keyed by the
// profile's own probed GI profile id (info.Id) — the authoritative id that distinguishes
// the same-compute-slice REV profiles (1g.5gb id 0 vs 1g.10gb id 7) a compute-slice count
// alone cannot. It is injected so this derivation stays hardware-free and unit-testable;
// detectMigProfiles passes a map-backed closure.
//
// A profile is published only if the driver enumerated at least one legal placement of
// positive length for it, and the published memory-slice span is that length. Every legal
// placement of a profile is exactly as long as that profile's span, so the length is the
// driver's own authoritative answer, and it is the only source: the span is what the
// allocator matches a leftover instance's identity by and creates an instance with, so a
// span this derivation computed rather than read would be a guess in the one place a guess
// can hand out somebody else's partition. Nothing else can supply it — a profile's
// compute-slice count cannot (the whole-accelerator profile reports seven compute slices while
// occupying all eight memory ones), and dividing accelerator memory by an assumed slice count
// assumes exactly the number that cannot be recovered.
//
// So a profile the driver placed nowhere is dropped and named in the returned error.
// Nothing is forfeited: the pool's per-profile ledger is placement-derived and slot
// selection has nothing to choose from, so such a profile was a requestable key whose
// allocation could only fail. An absent placement set here always means "the driver
// enumerated none" — a profile whose placement query failed never reaches this derivation,
// and both now cost the profile its place in the inventory rather than only one of them.
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
	placementsFor func(giProfileID uint32) []device.AcceleratorPlacement,
) ([]device.AcceleratorPhysicalSlicedProfile, error) {
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

		// Refused before the de-duplication, so a profile with no span never claims a name a
		// sibling id could still publish.
		var placements []device.AcceleratorPlacement
		if placementsFor != nil {
			placements = placementsFor(info.Id)
		}
		if len(placements) == 0 || placements[0].Length <= 0 {
			errs = append(errs, fmt.Errorf(
				"profile %d named %q: driver enumerated no legal placement, so its memory-slice span is unknown",
				info.Id, name))
			continue
		}

		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}

		profiles = append(profiles, device.AcceleratorPhysicalSlicedProfile{
			Name:          name,
			MemoryMib:     int64(info.MemorySizeMB),
			ComputeSlices: int32(info.SliceCount),
			MemorySlices:  placements[0].Length,
			Count:         int32(info.InstanceCount),
			Placements:    placements,
		})
	}
	return profiles, errors.Join(errs...)
}

// migPlacementsFromNVML converts NVML GPU-instance placement slots to the operator
// placement type. It returns nil for an empty input so a profile with no enumerated
// placements omits the field.
func migPlacementsFromNVML(slots []nvml.GpuInstancePlacement) []device.AcceleratorPlacement {
	if len(slots) == 0 {
		return nil
	}
	out := make([]device.AcceleratorPlacement, len(slots))
	for i := range slots {
		out[i] = device.AcceleratorPlacement{
			Start:  int32(slots[i].Start),
			Length: int32(slots[i].Size),
		}
	}
	return out
}

// migPlacementsByProfile resolves every probed profile's empty-accelerator legal placement set up
// front, and returns only the profiles whose query the driver actually answered, together
// with their placement sets keyed by the profile's own probed GI profile id.
//
// Separating a driver that enumerates no placement from a query that failed is the point:
// a lookup collapsing a failure to an empty set makes an unreadable accelerator indistinguishable
// from a placement-free one, and a failure would then be reported as a fact about the accelerator.
// A profile whose query failed is therefore withheld from the returned set — it could not be
// admitted without a placement set anyway — and named in the returned error, while a profile
// the driver answered with nothing is kept here, with a nil placement set, and refused by the
// derivation instead. The two reach the same outcome by different routes on purpose: this one
// reports an unreadable accelerator, that one an unplaceable profile. The errors of all failing ids
// are joined so one failure does not hide the rest.
//
// query is injected so the resolution stays hardware-free and unit-testable.
func migPlacementsByProfile(
	infos []nvml.GpuInstanceProfileInfo_v3,
	query func(giProfileID uint32) ([]nvml.GpuInstancePlacement, nvml.Return),
) ([]nvml.GpuInstanceProfileInfo_v3, map[uint32][]device.AcceleratorPlacement, error) {
	answered := make([]nvml.GpuInstanceProfileInfo_v3, 0, len(infos))
	byID := make(map[uint32][]device.AcceleratorPlacement, len(infos))
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

// driverReportsAbsent reports whether a non-success return is the driver ANSWERING that it has
// nothing at the id asked about, rather than failing to answer at all. It draws the same line the
// placement resolution above rests on: an id the driver disclaims is inventory information, while an
// id it could not read leaves the inventory short of a profile the accelerator may well offer. The
// two are
// worth separating because the inventory is published either way, and a short one is
// indistinguishable from an accelerator that never had the profile.
func driverReportsAbsent(ret nvml.Return) bool {
	switch ret {
	case nvml.ERROR_NOT_SUPPORTED, nvml.ERROR_NOT_FOUND, nvml.ERROR_INVALID_ARGUMENT:
		return true
	default:
		return false
	}
}

// probeMigProfiles walks the whole GPU instance profile-id space and separates the three answers a
// probe can give: a profile, an id the driver disclaims, and an id it could not answer for. The
// disclaimed ids are the ordinary case — the space is a fixed enumeration and an accelerator offers a
// few of
// it — so they are skipped without comment, and only the unanswered ones are joined into the returned
// error, which leaves that error meaning "this accelerator's inventory is short" and nothing else.
//
// probe is injected so the walk stays hardware-free and unit-testable, as the placement resolution is.
func probeMigProfiles(
	probe func(giProfileID uint32) (nvml.GpuInstanceProfileInfo_v3, nvml.Return),
) ([]nvml.GpuInstanceProfileInfo_v3, error) {
	var infos []nvml.GpuInstanceProfileInfo_v3
	var unreadable []error
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := probe(id)
		if !ret.IsSuccess() {
			if !driverReportsAbsent(ret) {
				unreadable = append(unreadable, fmt.Errorf("profile %d: %s", id, ret.Error()))
			}
			continue
		}
		infos = append(infos, info)
	}
	return infos, errors.Join(unreadable...)
}

// detectMigProfiles probes every GPU instance profile id on the device and returns the accelerator's
// physical-slice profile inventory (filtered and derived by deriveSlicedProfiles). An id the driver
// disclaims is skipped as the answer it is.
//
// An id the driver could not answer for is also skipped — a profile cannot be published without the
// geometry the probe carries — but it is reported rather than dropped in silence, because what it
// costs is not local. A profile missing from this inventory is missing from the accelerator's Devices
// record, from the node's capacity keys, from its flavor and from its InstanceType: a Pod already
// holding a MIG instance of that profile stops being able to have it named, and a new request for it
// is refused by admission as a profile the accelerator does not offer. The disclaimed ids are
// filtered out
// first precisely so that what remains is a driver fault worth an error rather than a routine answer.
func detectMigProfiles(dev nvml.Device) []device.AcceleratorPhysicalSlicedProfile {
	infos, probeErr := probeMigProfiles(dev.GetGpuInstanceProfileInfo)
	if probeErr != nil {
		uuid, _ := dev.GetUUID()
		klog.Background().Error(probeErr,
			"Left MIG profiles out of a card's inventory because the driver could not answer for them; "+
				"a profile absent here is absent from the node's capacity, its flavor and its InstanceType, "+
				"so a MIG instance of it can stop being nameable and a new request for it is refused",
			"device", uuid)
	}

	// Cache each profile's empty-accelerator legal placement set at detect time so the reconciler
	// can derive RemainingProfiles by pure arithmetic (subtracting annotation-derived occupied)
	// without any per-reconcile NVML. Queried by the profile's own probed id, which
	// distinguishes the same-compute-slice REV profiles a slice count cannot.
	//
	// A profile whose placement query failed is withheld rather than published from an
	// unverified geometry. The failure names the accelerator and the profile ids so the capacity
	// missing from the inventory is diagnosable instead of merely absent.
	answered, placementsByID, err := migPlacementsByProfile(infos, dev.GetGpuInstancePossiblePlacements)
	if err != nil {
		uuid, _ := dev.GetUUID()
		klog.Background().Error(err, "Dropped MIG profiles whose possible placements are unreadable",
			"device", uuid)
	}

	placementsFor := func(giProfileID uint32) []device.AcceleratorPlacement {
		return placementsByID[giProfileID]
	}

	// A profile the driver did not name is dropped rather than published under an invented
	// name the allocator's name probe could never resolve, and so is one the driver placed
	// nowhere, whose memory-slice span would otherwise have to be guessed. The rejection
	// names the accelerator and the profile ids so the omitted capacity is diagnosable instead of
	// merely absent.
	profiles, err := deriveSlicedProfiles(answered, placementsFor)
	if err != nil {
		uuid, _ := dev.GetUUID()
		klog.Background().Error(err, "Dropped unpublishable MIG profiles", "device", uuid)
	}
	return profiles
}

// maxProfileCount returns the accelerator's physical-slice ceiling — the largest per-profile Count
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
