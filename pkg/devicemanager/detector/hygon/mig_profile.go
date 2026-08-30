package hygon

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/dmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// migSliceWidths is the GPU-instance profile space this detector sweeps: one entry per instance
// width the vendor's enumeration can hold, in GPU slices.
//
// The sweep is by WIDTH rather than by profile id because that is what the library's profile query
// takes; see dmi.Device.GetGpuInstanceProfileInfoBySliceCount. A card offering no profile of some
// width answers with an absent code, which is the ordinary case rather than a fault: every card
// measured offers a one-, two- and four-slice profile and nothing three slices wide.
var migSliceWidths = []uint32{1, 2, 3, 4}

// migModeEnabled reports whether the node has Multi-Instance mode on, and whether that could be
// established at all.
//
// The mode is a property of the NODE. The vendor's mode switch takes no device selector and the
// library's query takes no handle, so there is no such thing as one card of a host being partitioned
// while another is not — which is the deepest difference from NVIDIA's MIG and the reason this is
// read once per detect pass rather than per accelerator.
//
// It is read per pass rather than once per process because an administrator flips the mode out of
// band, and a detector that cached the answer would keep publishing the capability the node had when
// the device manager started.
// A host whose driver predates Multi-Instance support has no library to load and no mode to report,
// and that is an ANSWER rather than a failure: it is every Hygon node that has not been put into MIG
// mode, which is nearly all of them. A library that is present and would not answer is the other
// case entirely, and the two must not be conflated — reading a MIG-enabled node as unpartitioned
// makes it advertise whole-card and logical-slice capacity no container on it can be served from.
func (in *hygon) migModeEnabled() (enabled, known bool) {
	current, _, ret := in.dmi.GetSystemMigMode()
	switch {
	case ret.IsSuccess():
		return current == dmi.DEVICE_MIG_ENABLE, true
	case ret.IsAPIUnavailable():
		in.logger.V(3).Info("this node offers no multi-instance library, so it serves no partitions",
			"return", ret, "library", in.dmi.Path())
		return false, true
	default:
		in.logger.Error(ret, "could not read the node's multi-instance mode, so neither capability is published",
			"library", in.dmi.Path())
		return false, false
	}
}

// migCardCapability is what one card's Multi-Instance handle answered: the slicing capability it
// publishes, and the card's own compute-unit count.
//
// The core count travels with the profiles because on a node in Multi-Instance mode it can only be
// learned here; see migCardCores. Zero means it could not be established, and the caller leaves
// whatever it already had rather than overwriting a good figure with nothing.
type migCardCapability struct {
	sliced device.AcceleratorPhysicalSliced
	cores  uint32
}

// detectCardMigProfiles resolves one card's Multi-Instance handle and returns what it publishes.
//
// The card is reached by its PCI address because that is the only identity the two libraries share:
// the rest of this detector enumerates cards through RSMI, and the Multi-Instance library serves no
// UUID at all and answers its own PCI query with an empty string. A card that cannot be reached
// reports no capability rather than an empty one, so it is excluded from the partition family
// instead of advertising a family with nothing behind it.
func (in *hygon) detectCardMigProfiles(pciBusID string, logger klog.Logger) migCardCapability {
	dev, ret := in.dmi.GetDeviceHandleByPciBusId(pciBusID)
	if !ret.IsSuccess() {
		logger.Error(ret, "failed to reach a card through the multi-instance library, so it publishes no"+
			" partition profiles even though the node is in multi-instance mode", "pci", pciBusID)
		return migCardCapability{}
	}

	infos, err := probeMigProfiles(dev.GetGpuInstanceProfileInfoBySliceCount)
	if err != nil {
		logger.Error(err,
			"left partition profiles out of a card's inventory because the driver could not answer for "+
				"them; a profile absent here is absent from the node's capacity, its flavor and its "+
				"InstanceType, so a partition of it can stop being nameable and a new request for it is refused")
	}

	return migCardCapability{
		sliced: physicalSliced(migProfilesOf(infos, dev, logger)),
		cores:  migCardCores(infos),
	}
}

// migCardCores derives the card's full compute-unit count from its partition profiles.
//
// It is needed because HSA cannot supply it on a node in Multi-Instance mode. The HSA runtime
// exposes exactly ONE instance to any process there -- measured on an eight-card node, where it
// reported a single agent with the 20 compute units of a one-slice instance rather than eight agents
// with the cards' 80 -- so a detector reading compute units from HSA on such a node publishes a
// quarter of the node's compute and gets no agent at all for seven of its cards.
//
// Each profile carries the whole card's count in factored form: a profile's own compute units times
// the number of such instances the card holds fills the card exactly, and every profile of a
// measured card agreed on the product (20x4, 40x2 and 80x1 all being 80). The largest product is
// taken so a single profile misreporting its maximum cannot understate the card.
func migCardCores(infos []dmi.GpuInstanceProfileInfo) uint32 {
	var cores uint32
	for _, info := range infos {
		if total := info.Cu_count * info.Gi_count_max; total > cores {
			cores = total
		}
	}
	return cores
}

// migProfilesOf turns probed profiles into the published inventory: it resolves their placements and
// derives what survives.
func migProfilesOf(
	infos []dmi.GpuInstanceProfileInfo, dev dmi.Device, logger klog.Logger,
) []device.AcceleratorPhysicalSlicedProfile {
	// Each profile's empty-card legal placement set is cached at detect time so the reconciler can
	// derive the card's remaining profiles by subtracting the occupied intervals it reconstructs
	// from Pod annotations, with no per-reconcile device query.
	answered, placementsByID, err := migPlacementsByProfile(infos, dev.GetGpuInstancePossiblePlacements)
	if err != nil {
		logger.Error(err, "dropped partition profiles whose possible placements are unreadable")
	}

	profiles, rejected := deriveSlicedProfiles(answered,
		func(profileID uint32) []device.AcceleratorPlacement {
			return placementsByID[profileID]
		})
	for _, reason := range rejected {
		logger.Info("dropped unpublishable partition profile", "reason", reason)
	}
	return profiles
}

// probeMigProfiles walks the instance-width space and separates the three answers a probe can give:
// a profile, a width the driver disclaims, and a width it could not answer for.
//
// The disclaimed widths are the ordinary case -- the space is a fixed enumeration and a card offers
// a few of it -- so they are skipped without comment, and only the unanswered ones are joined into
// the returned error. That leaves the error meaning "this card's inventory is short" and nothing
// else.
//
// probe is injected so the walk stays hardware-free and unit-testable.
func probeMigProfiles(
	probe func(sliceCount uint32) (dmi.GpuInstanceProfileInfo, dmi.Return),
) ([]dmi.GpuInstanceProfileInfo, error) {
	var (
		infos      []dmi.GpuInstanceProfileInfo
		unreadable []error
	)
	for _, width := range migSliceWidths {
		info, ret := probe(width)
		if !ret.IsSuccess() {
			if !ret.ReportsAbsent() {
				unreadable = append(unreadable, fmt.Errorf("%d-slice profile: %w", width, ret))
			}
			continue
		}
		infos = append(infos, info)
	}
	return infos, errors.Join(unreadable...)
}

// migPlacementsByProfile resolves every probed profile's empty-card legal placement set up front,
// and returns only the profiles whose query the driver actually answered, keyed by profile id.
//
// Separating a driver that enumerates no placement from a query that failed is the point: a lookup
// collapsing a failure to an empty set makes an unreadable card indistinguishable from a
// placement-free one, and the derivation would then publish a span from unverified geometry. A
// profile whose query failed is therefore withheld -- it could not be admitted without a placement
// set anyway -- and named in the returned error, while a profile the driver answered with nothing is
// kept with a nil set. The errors of all failing profiles are joined so one failure does not hide
// the rest.
//
// query is injected so the resolution stays hardware-free and unit-testable.
func migPlacementsByProfile(
	infos []dmi.GpuInstanceProfileInfo,
	query func(profileID uint32) ([]dmi.GpuInstancePlacement, dmi.Return),
) ([]dmi.GpuInstanceProfileInfo, map[uint32][]device.AcceleratorPlacement, error) {
	answered := make([]dmi.GpuInstanceProfileInfo, 0, len(infos))
	byID := make(map[uint32][]device.AcceleratorPlacement, len(infos))
	var errs []error
	for _, info := range infos {
		slots, ret := query(info.Id)
		if !ret.IsSuccess() {
			errs = append(errs, fmt.Errorf("profile %d: %w", info.Id, ret))
			continue
		}
		answered = append(answered, info)
		byID[info.Id] = migPlacementsFromDMI(slots)
	}
	return answered, byID, errors.Join(errs...)
}

// migPlacementsFromDMI converts the library's placement slots to the operator placement type. It
// returns nil for an empty input so a profile with no enumerated placements omits the field.
func migPlacementsFromDMI(slots []dmi.GpuInstancePlacement) []device.AcceleratorPlacement {
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

// deriveSlicedProfiles turns a probed set of GPU-instance profiles into the card's physical-slice
// profile inventory: it normalizes each profile's name into the bare geometry its resource key
// carries and derives the slice geometry. It returns the inventory together with one human-readable
// reason per profile it refused, for the caller to log -- the refusal is a decision this function
// makes, so it is returned rather than logged here, which keeps it assertable.
//
// placementsFor returns a profile's full empty-card legal placement set, keyed by the profile's own
// probed id. It is injected so this derivation stays hardware-free and unit-testable.
//
// A profile is published only if the driver enumerated at least one legal placement of positive
// length for it, and the published memory-slice span is that length. Every legal placement of a
// profile is exactly as long as that profile's span, so the length is the driver's own authoritative
// answer -- and the span is what the allocator matches a leftover instance's identity by and creates
// an instance with, so a span this derivation computed rather than read would be a guess in the one
// place a guess can hand out somebody else's partition.
//
// Two refusals exist because the shared group aggregation merges profiles by name, sums their counts
// and keeps the first one's memory:
//   - A profile whose normalized name cannot form a valid resource-name segment, the nameless
//     records included, is unrequestable: the published key is the name, and the allocator resolves
//     that key back to a raw profile id by matching names, so a name that cannot be published -- or
//     one synthesized here -- could never match the driver.
//   - Two raw names normalizing to one are the same profile only if every published field agrees.
//     When they disagree neither reading can be trusted, and publishing either would silently
//     misstate the card's capacity and its Kueue credits, so the name is withheld entirely rather
//     than aggregated.
func deriveSlicedProfiles(
	infos []dmi.GpuInstanceProfileInfo,
	placementsFor func(profileID uint32) []device.AcceleratorPlacement,
) (profiles []device.AcceleratorPhysicalSlicedProfile, rejected []string) {
	// Grouped by name first, then emitted, so a disagreement discovered late still withholds the
	// name rather than leaving the first reading published.
	type candidate struct {
		raw     string
		id      uint32
		profile device.AcceleratorPhysicalSlicedProfile
	}
	var (
		order  []string
		byName = make(map[string][]candidate, len(infos))
	)

	for _, info := range infos {
		raw := migProfileName(info)

		name := nodefeature.NormalizePartitionedProfileName(raw)
		if nodefeature.GetAcceleratablePartitionedProfileResourceName(Manufacturer, name) == "" {
			rejected = append(rejected, fmt.Sprintf(
				"profile %d named %q yields no valid resource name, so it cannot be requested", info.Id, raw))
			continue
		}

		placements := placementsFor(info.Id)
		if len(placements) == 0 || placements[0].Length <= 0 {
			rejected = append(rejected, fmt.Sprintf(
				"profile %d named %q has no legal placement, so its memory-slice span is unknown", info.Id, raw))
			continue
		}

		if _, seen := byName[name]; !seen {
			order = append(order, name)
		}
		byName[name] = append(byName[name], candidate{
			raw: raw,
			id:  info.Id,
			profile: device.AcceleratorPhysicalSlicedProfile{
				Name: name,
				// The vendor's field is spelled MB but carries MiB: a four-slice profile reports the
				// same 65520 the card's own memory total reads, in the MiB the rest of this detector
				// already uses.
				MemoryMib:     int64(info.Memory_size_MB),
				ComputeSlices: int32(info.Gpu_slice_count),
				MemorySlices:  placements[0].Length,
				Count:         int32(info.Gi_count_max),
				Placements:    placements,
			},
		})
	}

	for _, name := range order {
		group := byName[name]
		divergent := slices.ContainsFunc(group[1:], func(c candidate) bool {
			return !profileEqual(group[0].profile, c.profile)
		})
		if divergent {
			raws := make([]string, len(group))
			for i, c := range group {
				raws[i] = fmt.Sprintf("%d (%q)", c.id, c.raw)
			}
			rejected = append(rejected, fmt.Sprintf(
				"profiles %s normalize to %q with a different geometry, memory, count or placement set;"+
					" all of them are withheld", strings.Join(raws, ", "), name))
			continue
		}
		profiles = append(profiles, group[0].profile)
	}
	return profiles, rejected
}

// profileEqual reports whether two derived profiles are the same offer in every field the node
// publishes. Equality is what makes collapsing them safe; anything else is a collision.
func profileEqual(a, b device.AcceleratorPhysicalSlicedProfile) bool {
	return a.Name == b.Name &&
		a.MemoryMib == b.MemoryMib &&
		a.ComputeSlices == b.ComputeSlices &&
		a.MemorySlices == b.MemorySlices &&
		a.Count == b.Count &&
		slices.Equal(a.Placements, b.Placements)
}

// migProfileName renders the profile's fixed-width name field as a Go string. The generated struct
// types it as int8 rather than byte, and the vendor pads it with NULs.
func migProfileName(info dmi.GpuInstanceProfileInfo) string {
	out := make([]byte, 0, len(info.Name))
	for _, c := range info.Name {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// physicalSliced presents a card's detected partition profiles as its physical slicing capability,
// and reports no capability at all when the driver offered none: an inventory with nothing in it
// describes a card that cannot be partitioned, and advertising the capability anyway would publish a
// family with nothing behind it.
func physicalSliced(profiles []device.AcceleratorPhysicalSlicedProfile) device.AcceleratorPhysicalSliced {
	if len(profiles) == 0 {
		return device.AcceleratorPhysicalSliced{}
	}
	return device.AcceleratorPhysicalSliced{
		Profiles: profiles,
		Count:    maxProfileCount(profiles),
	}
}

// maxProfileCount returns the card's physical-slice ceiling -- the largest per-profile Count. Zero
// for an empty profile list.
func maxProfileCount(profiles []device.AcceleratorPhysicalSlicedProfile) int32 {
	var ceiling int32
	for _, p := range profiles {
		if p.Count > ceiling {
			ceiling = p.Count
		}
	}
	return ceiling
}

// rejectDivergentGroupProfiles withholds, from every card of one group, each profile name the
// group's cards do not agree on, and returns one reason per withheld name for the caller to log.
//
// It runs before the shared group aggregation, which merges profiles by name, sums their per-card
// counts and keeps the first card's memory -- so two cards exposing one name with different
// geometry, memory, count or placements would publish capacity and Kueue credits that describe
// neither. Each card's physical ceiling is recomputed from what survives, so a ceiling can never
// outlive the profile it was taken from, and a card left with no profile reports no physical
// capability rather than an empty one.
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
