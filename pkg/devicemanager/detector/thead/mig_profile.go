package thead

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// deriveSlicedProfiles turns a probed set of GPU-instance profiles into the card's
// physical-slice profile inventory: it drops the media-engine and graphics variants,
// normalizes each kept profile's name into the bare geometry its resource key carries, and
// derives the slice geometry. It returns the inventory together with one human-readable
// reason per profile it refused, for the caller to log — the refusal is a decision this
// function makes, so it is returned rather than logged here, which keeps it assertable.
//
// infos holds one entry per successfully probed profile id; the caller skips ids the driver
// reports as unsupported.
//
// placementsFor returns a profile's full empty-card legal placement set to cache in
// Placements, keyed by the profile's own probed profile id — the authoritative id, since the
// vendor does not assign its ids the upstream slice-count meaning, so profiles of equal
// compute width are told apart by id alone. It is injected so this derivation stays
// hardware-free and unit-testable.
//
// A profile is published only if the driver enumerated at least one legal placement of
// positive length for it, and the published memory-slice span is that length. Every legal
// placement of a profile is exactly as long as that profile's span, so the length is the
// driver's own authoritative answer, and it is the only source: the span is what the allocator
// matches a leftover instance's identity by and creates an instance with, so a span this
// derivation computed rather than read would be a guess in the one place a guess can hand out
// somebody else's partition. Nothing else can supply it — a profile's compute-slice count
// cannot, and dividing card memory by an assumed slice count assumes exactly the number that
// cannot be recovered. An absent placement set here always means "the driver enumerated none"
// — a profile whose placement query failed never reaches this derivation, and both cost the
// profile its place in the inventory rather than only one of them.
//
// Three refusals are deliberately stricter than the vendor implementation this mirrors. The
// last two exist because the shared group aggregation merges profiles by name, sums their
// counts and keeps the first one's memory:
//   - A profile the driver placed nowhere forfeits nothing by being dropped: the pool's
//     per-profile ledger is placement-derived and slot selection has nothing to choose from,
//     so it was a requestable key whose allocation could only fail.
//   - A profile whose normalized name cannot form a valid resource-name segment, the
//     nameless records included, is unrequestable: the published key is the name, and the
//     driver seam resolves that key back to a raw id by matching names, so a name that
//     cannot be published — or one synthesized here — could never match the driver.
//   - Two raw names normalizing to one are the same profile only if every published field
//     agrees. When they disagree neither reading can be trusted, and publishing either would
//     silently misstate the pool's capacity and its Kueue credits, so the name is withheld
//     entirely rather than aggregated.
func deriveSlicedProfiles(
	infos []hgml.GpuInstanceProfileInfo_v3,
	placementsFor func(giProfileID uint32) []device.AcceleratorPhysicalPlacement,
) (profiles []device.AcceleratorPhysicalSlicedProfile, rejected []string) {
	seen := make(map[string]int, len(infos))
	withheld := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		raw := info.GetName()
		if isMediaOrGraphicsVariant(info, raw) {
			continue
		}

		name := nodefeature.NormalizePartitionedProfileName(raw)
		if nodefeature.GetAcceleratablePartitionedProfileResourceName(Manufacturer, name) == "" {
			rejected = append(rejected, fmt.Sprintf(
				"profile %d named %q yields no valid resource name, so it cannot be requested", info.Id, raw))
			continue
		}

		// Refused before the name bookkeeping below, so a profile with no span never claims a
		// name a sibling id could still publish, and never withholds one.
		var placements []device.AcceleratorPhysicalPlacement
		if placementsFor != nil {
			placements = placementsFor(info.Id)
		}
		if len(placements) == 0 || placements[0].Length <= 0 {
			rejected = append(rejected, fmt.Sprintf(
				"profile %d named %q has no legal placement, so its memory-slice span is unknown", info.Id, raw))
			continue
		}

		p := device.AcceleratorPhysicalSlicedProfile{
			Name:          name,
			MemoryMib:     int64(info.MemorySizeMB),
			ComputeSlices: int32(info.SliceCount),
			MemorySlices:  placements[0].Length,
			Count:         int32(info.InstanceCount),
			Placements:    placements,
		}

		if _, dropped := withheld[name]; dropped {
			continue
		}
		if idx, dup := seen[name]; dup {
			if profileEqual(profiles[idx], p) {
				continue
			}
			rejected = append(rejected, fmt.Sprintf(
				"profile %d named %q normalizes to %q, which another profile already exposes with a"+
					" different geometry, memory, count or placement set; both are withheld",
				info.Id, raw, name))
			withheld[name] = struct{}{}
			profiles = slices.Delete(profiles, idx, idx+1)
			for n, i := range seen {
				if i > idx {
					seen[n] = i - 1
				}
			}
			delete(seen, name)
			continue
		}
		seen[name] = len(profiles)
		profiles = append(profiles, p)
	}
	return profiles, rejected
}

// profileEqual reports whether two derived profiles are the same offer in every field the
// node publishes. Equality is what makes collapsing them safe; anything else is a collision.
func profileEqual(a, b device.AcceleratorPhysicalSlicedProfile) bool {
	return a.Name == b.Name &&
		a.MemoryMib == b.MemoryMib &&
		a.ComputeSlices == b.ComputeSlices &&
		a.MemorySlices == b.MemorySlices &&
		a.Count == b.Count &&
		slices.Equal(a.Placements, b.Placements)
}

// rejectDivergentGroupProfiles withholds, from every card of one group, each profile name the
// group's cards do not agree on, and returns one reason per withheld name for the caller to
// log. It runs before the shared group aggregation, which merges profiles by name, sums their
// per-card counts and keeps the first card's memory — so two cards exposing one name with
// different geometry, memory, count or placements would publish capacity and Kueue credits
// that describe neither card. Each card's physical ceiling is recomputed from what survives,
// so a ceiling can never outlive the profile it was taken from, and a card left with no
// profile reports no physical capability rather than an empty one.
func rejectDivergentGroupProfiles(group *device.DevicesGroup) (rejected []string) {
	first := make(map[string]device.AcceleratorPhysicalSlicedProfile)
	divergent := make(map[string]struct{})
	for i := range group.Accelerators {
		for _, p := range group.Accelerators[i].Status.PhysicalSliced.Profiles {
			seen, ok := first[p.Name]
			if !ok {
				first[p.Name] = p
				continue
			}
			if _, known := divergent[p.Name]; !known && !profileEqual(seen, p) {
				divergent[p.Name] = struct{}{}
				rejected = append(rejected, fmt.Sprintf(
					"profile %q is exposed with a different geometry, memory, count or placement set by"+
						" card %s than by another card of the same group; it is withheld from the group",
					p.Name, group.Accelerators[i].ID))
			}
		}
	}
	if len(divergent) == 0 {
		return nil
	}

	for i := range group.Accelerators {
		sliced := &group.Accelerators[i].Status.PhysicalSliced
		kept := slices.DeleteFunc(sliced.Profiles, func(p device.AcceleratorPhysicalSlicedProfile) bool {
			_, drop := divergent[p.Name]
			return drop
		})
		if len(kept) == 0 {
			sliced.Profiles = nil
			sliced.Count = 0
			continue
		}
		sliced.Profiles = kept
		sliced.Count = maxProfileCount(kept)
	}
	return rejected
}

// migPlacementsFromHGML converts the vendor library's GPU-instance placement slots to the
// operator placement type. It returns nil for an empty input so a profile with no enumerated
// placements omits the field.
func migPlacementsFromHGML(slots []hgml.GpuInstancePlacement) []device.AcceleratorPhysicalPlacement {
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
// with their placement sets keyed by the profile's own probed profile id.
//
// Separating a driver that enumerates no placement from a query that failed is the point: a
// lookup collapsing a failure to an empty set makes an unreadable card indistinguishable
// from a placement-free one, and the derivation would then publish a span from unverified
// geometry. A profile whose query failed is therefore withheld from the returned set — it
// could not be admitted without a placement set anyway — and named in the returned error,
// while a profile the driver answered with nothing is kept with a nil placement set. The
// errors of all failing ids are joined so one failure does not hide the rest.
//
// query is injected so the resolution stays hardware-free and unit-testable.
func migPlacementsByProfile(
	infos []hgml.GpuInstanceProfileInfo_v3,
	query func(giProfileID uint32) ([]hgml.GpuInstancePlacement, hgml.Return),
) ([]hgml.GpuInstanceProfileInfo_v3, map[uint32][]device.AcceleratorPhysicalPlacement, error) {
	answered := make([]hgml.GpuInstanceProfileInfo_v3, 0, len(infos))
	byID := make(map[uint32][]device.AcceleratorPhysicalPlacement, len(infos))
	var errs []error
	for _, info := range infos {
		slots, ret := query(info.Id)
		if !ret.IsSuccess() {
			errs = append(errs, fmt.Errorf("profile %d: %s", info.Id, ret.Error()))
			continue
		}
		answered = append(answered, info)
		byID[info.Id] = migPlacementsFromHGML(slots)
	}
	return answered, byID, errors.Join(errs...)
}

// isMediaOrGraphicsVariant reports whether a probed profile is a media-engine or graphics
// variant, which GPUStack does not offer: only the plain profiles whose compute instance can
// span the whole GPU instance are requestable. Every variant carries a "+..." suffix in its
// name, so a "+" is the discriminator, and the graphics capability bit is a backstop for a
// graphics profile whose naming differs. The profile ids are deliberately not consulted: the
// vendor keeps the upstream numbering in its header but does not assign its ids the upstream
// meaning, so an id range would encode a coincidence. A nameless profile is therefore
// indistinguishable from a variant — which costs nothing, because a nameless profile is
// unpublishable anyway and the derivation drops it.
func isMediaOrGraphicsVariant(info hgml.GpuInstanceProfileInfo_v3, name string) bool {
	return strings.ContainsRune(name, '+') ||
		info.Capabilities&hgml.GPU_INSTANCE_PROFILE_CAPS_GFX != 0
}

// driverReportsAbsent reports whether a non-success return is the driver ANSWERING that it has
// nothing at the id asked about, rather than failing to answer at all. It draws the same line the
// placement resolution above rests on: an id the driver disclaims is inventory information, while an
// id it could not read leaves the inventory short of a profile the card may well offer. The two are
// worth separating because the inventory is published either way, and a short one is
// indistinguishable from a card that never had the profile.
func driverReportsAbsent(ret hgml.Return) bool {
	switch ret {
	case hgml.ERROR_NOT_SUPPORTED, hgml.ERROR_NOT_FOUND, hgml.ERROR_INVALID_ARGUMENT:
		return true
	default:
		return false
	}
}

// probeMigProfiles walks the whole GPU-instance profile-id space and separates the three answers a
// probe can give: a profile, an id the driver disclaims, and an id it could not answer for. The
// disclaimed ids are the ordinary case — the space is a fixed enumeration and a card offers a few of
// it — so they are skipped without comment, and only the unanswered ones are joined into the returned
// error, which leaves that error meaning "this card's inventory is short" and nothing else.
//
// probe is injected so the walk stays hardware-free and unit-testable, as the placement resolution is.
func probeMigProfiles(
	probe func(giProfileID uint32) (hgml.GpuInstanceProfileInfo_v3, hgml.Return),
) ([]hgml.GpuInstanceProfileInfo_v3, error) {
	var infos []hgml.GpuInstanceProfileInfo_v3
	var unreadable []error
	for id := uint32(0); id < hgml.GPU_INSTANCE_PROFILE_COUNT; id++ {
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

// detectMigProfiles probes every GPU-instance profile id on the device and returns the card's
// physical-slice profile inventory. An id the driver disclaims is skipped as the answer it is; every
// id that answers is carried by its own value, never by one derived from it.
//
// An id the driver could not answer for is also skipped — a profile cannot be published without the
// geometry the probe carries — but it is reported rather than dropped in silence, because what it
// costs is not local. A profile missing from this inventory is missing from the card's Devices
// record, from the node's capacity keys, from its flavor and from its InstanceType: a Pod already
// holding a partition of that profile stops being able to have it named, and a new request for it is
// refused by admission as a profile the card does not offer. The disclaimed ids are filtered out
// first precisely so that what remains is a driver fault worth an error rather than a routine answer.
func detectMigProfiles(dev hgml.Device, logger klog.Logger) []device.AcceleratorPhysicalSlicedProfile {
	infos, err := probeMigProfiles(dev.GetGpuInstanceProfileInfo)
	if err != nil {
		logger.Error(err,
			"left partition profiles out of a card's inventory because the driver could not answer for "+
				"them; a profile absent here is absent from the node's capacity, its flavor and its "+
				"InstanceType, so a partition of it can stop being nameable and a new request for it is refused")
	}

	// Cache each profile's empty-card legal placement set at detect time so the reconciler
	// can derive the card's remaining profiles by pure arithmetic (subtracting the occupied
	// intervals it reconstructs from Pod annotations) without any per-reconcile device query.
	//
	// A profile whose placement query failed is withheld rather than published from an
	// unverified geometry. The failure is logged with the profile ids so the capacity missing
	// from the inventory is diagnosable instead of merely absent.
	answered, placementsByID, err := migPlacementsByProfile(infos, dev.GetGpuInstancePossiblePlacements)
	if err != nil {
		logger.Error(err, "dropped partition profiles whose possible placements are unreadable")
	}

	profiles, rejected := deriveSlicedProfiles(answered,
		func(giProfileID uint32) []device.AcceleratorPhysicalPlacement {
			return placementsByID[giProfileID]
		})
	for _, reason := range rejected {
		logger.Info("dropped unpublishable partition profile", "reason", reason)
	}
	return profiles
}

// physicalSliced presents a card's detected partition profiles as its physical slicing
// capability, and reports no capability at all when the driver offered none: an inventory
// with nothing in it describes a card that cannot be partitioned, and advertising the
// capability anyway would publish a family with nothing behind it.
func physicalSliced(profiles []device.AcceleratorPhysicalSlicedProfile) device.AcceleratorPhysicalSliced {
	if len(profiles) == 0 {
		return device.AcceleratorPhysicalSliced{}
	}
	return device.AcceleratorPhysicalSliced{
		Profiles: profiles,
		Count:    maxProfileCount(profiles),
	}
}

// maxProfileCount returns the card's physical-slice ceiling — the largest per-profile Count.
// Zero for an empty profile list.
func maxProfileCount(profiles []device.AcceleratorPhysicalSlicedProfile) int32 {
	var ceiling int32
	for _, p := range profiles {
		if p.Count > ceiling {
			ceiling = p.Count
		}
	}
	return ceiling
}
