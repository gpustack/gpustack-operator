//go:build linux

package thead

import (
	"fmt"
	"strings"
	"sync"

	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// newMigDriver returns the real vendor-management-library-backed partition driver. It is
// linux-only: the device-manager runs only on linux, and linking the cgo vendor binding into a
// darwin test binary (which links Go's plugin package) aborts at dyld load on the unresolved
// vendor symbols, so the darwin build uses the stub in mig_driver_other.go instead.
func newMigDriver() migDriver {
	l := hgml.New()
	return &hgmlMigDriver{
		lib:      l,
		initRet:  l.Init(),
		profiles: make(map[string][]hgml.GpuInstanceProfileInfo_v3),
	}
}

// hgmlMigDriver is the real migDriver, driving the vendor management binding on an accelerator
// addressed by its UUID over the partition GPU/compute-instance lifecycle wrappers.
//
// Every enumeration here reports either a state it can prove whole or an error. It never degrades
// to partial state on a failed query, which is where it is deliberately stricter than the vendor
// implementation it mirrors: a live partition that reads as absent has its ownership marker
// removed as "already gone", its occupied placement handed out a second time, and a marker-less
// one leaks past the orphan collector. The create/reuse sequence and the identity resolution are
// verified on real hardware; the platform-independent core is table-tested with a fake driver.
type hgmlMigDriver struct {
	lib *hgml.HGML
	// initRet captures the library's initialization result so every operation reports one
	// actionable root cause when the library failed to load or initialize, instead of failing
	// obscurely deeper in a call that cannot possibly work.
	initRet hgml.Return

	// profiles caches each accelerator's partition-profile catalog under profilesMu. The catalog
	// is a property of the accelerator and of its partitioning mode, and both are fixed for this
	// process's life: a mode change is only ever picked up by a restart. Caching it is worth the state
	// because probing it walks the vendor's whole profile id space — 85 ids, up to three library calls
	// each, so ~255 calls — and that walk happens on every allocation and on every reclaim pass, the
	// latter with an accelerator's lock held.
	//
	// An EMPTY catalog is deliberately not cached. That is what an accelerator reads as while its
	// partitioning mode is off, and remembering it would outlive the restart that turns the mode on.
	// The lock is real rather than theoretical: one driver value is shared by the partitioned and the
	// visibility servers.
	profilesMu sync.Mutex
	profiles   map[string][]hgml.GpuInstanceProfileInfo_v3
}

// cardProfileCatalogue returns the accelerator's partition-profile catalog, probing the driver
// only the first time it is asked for an accelerator that offers any.
func (d *hgmlMigDriver) cardProfileCatalogue(dev hgml.Device, cardUUID string) ([]hgml.GpuInstanceProfileInfo_v3, error) {
	d.profilesMu.Lock()
	cached, ok := d.profiles[cardUUID]
	d.profilesMu.Unlock()
	if ok {
		return cached, nil
	}

	probed, err := cardProfiles(dev, cardUUID)
	if err != nil {
		return nil, err
	}
	if len(probed) == 0 {
		return probed, nil
	}

	d.profilesMu.Lock()
	d.profiles[cardUUID] = probed
	d.profilesMu.Unlock()
	return probed, nil
}

// CardInstances enumerates one accelerator's live GPU instances. It is the whole-node enumeration's
// unit of work, and the reclaim loop's verification re-read calls it directly so that read costs one
// accelerator rather than the node while it holds that accelerator's lock.
func (d *hgmlMigDriver) CardInstances(cardUUID string) ([]migInstance, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return nil, err
	}
	return d.cardInstances(dev, cardUUID)
}

// cardInstances is CardInstances over a handle the caller already resolved, so the whole-node walk
// does not resolve every accelerator twice.
func (d *hgmlMigDriver) cardInstances(dev hgml.Device, cardUUID string) ([]migInstance, error) {
	partitioning, err := migModeEnabled(dev, cardUUID)
	if err != nil {
		return nil, err
	}
	if !partitioning {
		return nil, nil
	}
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return nil, err
	}
	identityByGI, err := migIdentities(dev, cardUUID)
	if err != nil {
		return nil, err
	}
	return liveInstances(dev, cardUUID, infos, identityByGI)
}

// ready reports the captured initialization failure, so a driver that cannot work says so on
// every entry point rather than half-working.
func (d *hgmlMigDriver) ready() error {
	if !d.initRet.IsSuccess() {
		return fmt.Errorf("vendor management library init failed: %s", d.initRet.Error())
	}
	return nil
}

// device resolves an accelerator handle from the UUID the operator addresses accelerators by.
func (d *hgmlMigDriver) device(cardUUID string) (hgml.Device, error) {
	if err := d.ready(); err != nil {
		return hgml.Device{}, err
	}
	dev, ret := d.lib.DeviceGetHandleByUUID(cardUUID)
	if !ret.IsSuccess() {
		return hgml.Device{}, fmt.Errorf("get device handle for card %s: %s", cardUUID, ret.Error())
	}
	return dev, nil
}

// driverReportsAbsent reports whether a return code is the driver's own statement that the probed
// id or index names nothing on this accelerator, which is the normal answer for most of a probe
// space. Only these three codes are skipped during a probe; every other failure means the query
// itself could not be answered, and an enumeration that cannot be proven complete must fail instead.
// A missing library symbol is deliberately not in this set: it makes every id unreadable rather than
// absent, and silently reporting an empty accelerator for it is the failure this contract exists to
// prevent.
func driverReportsAbsent(ret hgml.Return) bool {
	switch ret {
	case hgml.ERROR_NOT_SUPPORTED, hgml.ERROR_NOT_FOUND, hgml.ERROR_INVALID_ARGUMENT:
		return true
	default:
		return false
	}
}

// driverReportsInUse reports whether a destroy was rejected because a residual process still holds
// the instance — a bounded, retryable partial failure the reclaim loop recognizes, as opposed to a
// permanent one.
func driverReportsInUse(ret hgml.Return) bool {
	return ret == hgml.ERROR_IN_USE
}

// migModeEnabled reports whether the accelerator is currently in the partitioning mode, so one that
// provably holds no partition is skipped by the global enumeration. It errors when the mode itself
// could not be read: an unreadable accelerator's partition state is unknown, and treating unknown as
// "holds nothing" is the same "a live partition reads as absent" failure in another costume. An
// accelerator whose driver answers that the mode is unsupported is not partitionable, which is an
// answer.
func migModeEnabled(dev hgml.Device, cardUUID string) (bool, error) {
	current, _, ret := dev.GetMigMode()
	if ret.IsSuccess() {
		return current == hgml.DEVICE_MIG_ENABLE, nil
	}
	if ret == hgml.ERROR_NOT_SUPPORTED {
		return false, nil
	}
	return false, fmt.Errorf("card %s: read partitioning mode: %s", cardUUID, ret.Error())
}

// cardProfiles probes the accelerator's whole GPU-instance profile space and returns every profile
// the driver answered for. The probe takes a profile ENUM INDEX, while every later call takes the
// creation id the probe returns in the profile's own record, and the two differ on this hardware —
// so no id here is ever computed, only carried.
//
// It returns every profile, including the media and graphics variants GPUStack does not offer: the
// occupancy view must account for a partition of any profile, since a wide instance blocks a narrow
// one's placement. Only the name resolution filters the variants out.
func cardProfiles(dev hgml.Device, cardUUID string) ([]hgml.GpuInstanceProfileInfo_v3, error) {
	infos := make([]hgml.GpuInstanceProfileInfo_v3, 0, hgml.GPU_INSTANCE_PROFILE_COUNT)
	for id := uint32(0); id < hgml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if ret.IsSuccess() {
			infos = append(infos, info)
			continue
		}
		if driverReportsAbsent(ret) {
			continue
		}
		return nil, fmt.Errorf("card %s: probe gpu-instance profile %d: %s", cardUUID, id, ret.Error())
	}
	return infos, nil
}

// isMediaOrGraphicsProfile reports whether a probed profile is a media-engine or graphics variant,
// which GPUStack neither publishes nor allocates: only a plain profile whose compute instance can
// span the whole GPU instance is requestable. Every variant carries a "+..." suffix in its name, and
// the graphics capability bit is the backstop for one whose naming differs. The profile ids are
// deliberately not consulted: this vendor keeps the upstream numbering in its header without
// assigning its ids the upstream meaning, so an id range would encode a coincidence. This mirrors
// the filter the detector applies when it publishes the profiles, so the two ends of the round trip
// consider the same set of profiles requestable.
func isMediaOrGraphicsProfile(info hgml.GpuInstanceProfileInfo_v3, name string) bool {
	return strings.ContainsRune(name, '+') ||
		info.Capabilities&hgml.GPU_INSTANCE_PROFILE_CAPS_GFX != 0
}

// sameProfileOffer reports whether two profiles describe the same offer in every published field.
// Equality is what makes resolving one requested name to either of them safe; anything else is a
// contradiction the caller must refuse.
func sameProfileOffer(a, b hgml.GpuInstanceProfileInfo_v3) bool {
	return a.SliceCount == b.SliceCount &&
		a.MemorySizeMB == b.MemorySizeMB &&
		a.InstanceCount == b.InstanceCount
}

// matchProfile resolves a requested profile name to the accelerator's own GPU-instance profile
// record, by comparing normalized names over the probed profiles. The name is the only key: a
// compute-slice count cannot pick a profile, because this vendor's ids carry no slice-count meaning
// and several profiles can share a width.
//
// Both ends of the round trip normalize through the one shared function: the detector publishes a
// profile's resource key from the normalized driver name, and this resolves that key back to a raw
// driver id by normalizing the driver's names the same way. A second copy of those rules that
// drifted would leave a published profile silently unrequestable.
//
// Two profiles normalizing to one requested name are resolvable only while they describe the same
// offer — the detector withholds a name whose readings disagree, so a disagreement reaching here is
// a drift between what was published and what the driver now reports, and resolving it either way
// would hand out a partition that is not what was asked for.
func matchProfile(
	infos []hgml.GpuInstanceProfileInfo_v3, cardUUID, profile string,
) (hgml.GpuInstanceProfileInfo_v3, error) {
	want := nodefeature.NormalizePartitionedProfileName(profile)

	var (
		matched hgml.GpuInstanceProfileInfo_v3
		found   bool
	)
	for i := range infos {
		info := infos[i]
		raw := info.GetName()
		if isMediaOrGraphicsProfile(info, raw) {
			continue
		}
		if nodefeature.NormalizePartitionedProfileName(raw) != want {
			continue
		}
		switch {
		case !found:
			matched, found = info, true
		case info.Id == matched.Id:
		case !sameProfileOffer(matched, info):
			return hgml.GpuInstanceProfileInfo_v3{}, fmt.Errorf(
				"card %s: profile %q is reported by two gpu-instance profiles describing different offers"+
					" (id %d: %d compute slices, %d MiB, count %d; id %d: %d compute slices, %d MiB, count %d): fail closed",
				cardUUID, profile,
				matched.Id, matched.SliceCount, matched.MemorySizeMB, matched.InstanceCount,
				info.Id, info.SliceCount, info.MemorySizeMB, info.InstanceCount)
		case info.Id < matched.Id:
			matched = info
		}
	}
	if !found {
		return hgml.GpuInstanceProfileInfo_v3{}, fmt.Errorf(
			"card %s has no gpu-instance profile named %q", cardUUID, profile)
	}
	return matched, nil
}

// migIdentity is a live partition's addressing record as one partition device reports it: the
// identity string a container is given, and the compute-instance id whose capability node that
// identity addresses. Both are read off the same partition-device handle, so they cannot describe
// two different compute instances — handing a container one partition's identity with another's
// capability node is an isolation failure, not a mislabeling.
type migIdentity struct {
	UUID string
	CiID uint32
}

// migIdentities maps each GPU-instance id on the accelerator to its partition's addressing record,
// by enumerating its partition-device handles and reading each one's owning GPU-instance id. A
// GPU instance with no partition device materialized yet is simply absent from the map — that is a
// GPU instance without its compute instance, which addresses nothing and which reclaim destroys.
//
// Every failure past the driver's own "no device at this index" answer is an error: a handle in
// hand whose identity, compute instance or owner cannot be read leaves the map missing a live
// partition, and a missing identity is what makes a live partition look reclaimable. The
// compute-instance id is read, never defaulted, for the same reason the profile id is: 0 is a legal
// id, so a zero left behind by an unreadable query would address partition ci0 — either a path that
// does not exist, or a different compute instance's capability node.
//
// Two partition devices reporting the same owning GPU instance is an error rather than a last-one-wins
// overwrite. GPUStack carves one compute instance per GPU instance, so a second is something it did
// not create, and the map has exactly one slot to describe it with: whichever the driver happened to
// return last would become that GPU instance's recorded address. Reclaim would then read a
// compute-instance id its marker never recorded and drop the marker without destroying, leaking the
// partition, while a reserve for the same accelerator would fail closed on the mismatch and keep
// failing for as long as the extra device exists. Refusing names the accelerator instead of silently
// addressing the wrong half of it.
func migIdentities(dev hgml.Device, cardUUID string) (map[uint32]migIdentity, error) {
	count, ret := dev.GetMaxMigDeviceCount()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("card %s: get max partition-device count: %s", cardUUID, ret.Error())
	}

	out := make(map[uint32]migIdentity, count)
	for i := 0; i < count; i++ {
		mig, ret := dev.GetMigDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			if driverReportsAbsent(ret) {
				continue
			}
			return nil, fmt.Errorf("card %s: get partition-device handle %d: %s", cardUUID, i, ret.Error())
		}
		if mig == nil {
			return nil, fmt.Errorf(
				"card %s: partition-device handle %d is absent though the driver reported success", cardUUID, i)
		}
		// Read the owning GPU-instance id from the partition-device handle directly; resolving the
		// full instance needs the parent accelerator handle and fails on this one.
		giID, ret := mig.GetGpuInstanceId()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf(
				"card %s: get owning gpu-instance id of partition device %d: %s", cardUUID, i, ret.Error())
		}
		ciID, ret := mig.GetComputeInstanceId()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf(
				"card %s: get compute-instance id of partition device %d: %s", cardUUID, i, ret.Error())
		}
		uuid, ret := mig.GetUUID()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("card %s: get identity of partition device %d: %s", cardUUID, i, ret.Error())
		}
		if prev, dup := out[giID]; dup {
			return nil, fmt.Errorf(
				"card %s: gpu instance %d owns more than one partition device (%s ci%d and %s ci%d): "+
					"refusing to address it by either",
				cardUUID, giID, prev.UUID, prev.CiID, uuid, ciID)
		}
		out[giID] = migIdentity{UUID: uuid, CiID: ciID}
	}
	return out, nil
}

// liveInstances enumerates every live GPU instance on the accelerator across all profiles, each
// carrying the raw profile id it was carved from, its compute-slice count, its memory-slice
// placement and its partition's addressing record — the identity string and the compute-instance
// id. Occupancy must span every profile, because an instance of one profile occupies placements
// another profile could otherwise use.
//
// The addressing record matters beyond occupancy: one of these instances is what adoption reuses,
// and the marker it writes is what the allocation resolves a capability node from. An adopted
// instance whose compute-instance id were left at zero would address ci0 — a path that either does
// not exist, failing the allocation and re-failing it on every retry until reclaim, or belongs to
// another compute instance, which hands the container the wrong partition's node.
//
// A failed instance query is an error rather than a skipped profile. The raw profile id is read
// from the instance itself, never defaulted: the vendor numbering makes 0 a legal id, so a zero
// left behind by an unreadable query would read as a real profile and could be adopted for it. An
// instance reporting a profile other than the one it was enumerated under is a driver
// self-contradiction, and resolving it either way could adopt an instance for a profile it is not.
func liveInstances(
	dev hgml.Device, cardUUID string, infos []hgml.GpuInstanceProfileInfo_v3,
	identityByGI map[uint32]migIdentity,
) ([]migInstance, error) {
	var live []migInstance
	for i := range infos {
		info := infos[i]
		gis, ret := dev.GetGpuInstances(info.Id)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf(
				"card %s: list live gpu instances of profile %d: %s", cardUUID, info.Id, ret.Error())
		}
		for j := range gis {
			gi := gis[j].GetInfo()
			if gi.ProfileId != info.Id {
				return nil, fmt.Errorf(
					"card %s: gpu instance %d reports profile %d while enumerated under profile %d: fail closed",
					cardUUID, gi.Id, gi.ProfileId, info.Id)
			}
			identity := identityByGI[gi.Id]
			live = append(live, migInstance{
				GiID:          gi.Id,
				CiID:          identity.CiID,
				ProfileID:     gi.ProfileId,
				ComputeSlices: int32(info.SliceCount),
				Placement:     migPlacement{Start: int32(gi.Placement.Start), Length: int32(gi.Placement.Size)},
				UUID:          identity.UUID,
			})
		}
	}
	return live, nil
}

// wholeGiComputeProfile discovers, on a just-created GPU instance, the compute-instance profile
// that spans the whole GPU instance — the only shape GPUStack creates, since a container is given
// the instance entire. It is discovered by probing the GPU instance's own compute-instance profile
// space and keeping the profile as wide as the GPU instance, never a constant: this vendor assigns
// no upstream meaning to its ids, so a hard-coded id would encode a coincidence.
//
// The probe walks the profile *index* space, and an index is not an identity: this driver answers
// several indices with one and the same profile, so a candidate already seen by its id is skipped.
// Counting an alias as a second candidate would fail closed on a product that offers exactly one
// profile — which is what a whole-accelerator partition ran into, where two indices both answered
// with the driver's single default profile.
//
// Genuine ambiguity still fails closed. Unlike a GPU-instance profile, a compute-instance profile
// probed before its instance exists carries neither a name nor capability bits, so a plain
// whole-instance profile cannot be told apart from a whole-instance media or graphics variant here.
// Creating from a guess would hand a container a partition of the wrong kind, so more than one
// distinctly identified candidate is reported with every candidate's id and width, which is the
// evidence needed to decide whether this product needs a name-carrying probe at all.
func wholeGiComputeProfile(
	gi hgml.GpuInstance, cardUUID string, giProfile hgml.GpuInstanceProfileInfo_v3,
) (hgml.ComputeInstanceProfileInfo, error) {
	var candidates []hgml.ComputeInstanceProfileInfo
	seen := make(map[uint32]struct{})
	for ciID := uint32(0); ciID < hgml.COMPUTE_INSTANCE_PROFILE_COUNT; ciID++ {
		info, ret := gi.GetComputeInstanceProfileInfo(ciID, hgml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
		if !ret.IsSuccess() {
			if driverReportsAbsent(ret) {
				continue
			}
			return hgml.ComputeInstanceProfileInfo{}, fmt.Errorf(
				"card %s: probe compute-instance profile %d of gpu-instance profile %d: %s",
				cardUUID, ciID, giProfile.Id, ret.Error())
		}
		if info.SliceCount != giProfile.SliceCount {
			continue
		}
		if _, alias := seen[info.Id]; alias {
			continue
		}
		seen[info.Id] = struct{}{}
		candidates = append(candidates, info)
	}

	switch len(candidates) {
	case 0:
		return hgml.ComputeInstanceProfileInfo{}, fmt.Errorf(
			"card %s: gpu-instance profile %d (%d compute slices) offers no compute-instance profile"+
				" spanning the whole gpu instance",
			cardUUID, giProfile.Id, giProfile.SliceCount)
	case 1:
		return candidates[0], nil
	default:
		return hgml.ComputeInstanceProfileInfo{}, fmt.Errorf(
			"card %s: gpu-instance profile %d (%d compute slices) offers %d compute-instance profiles spanning"+
				" the whole gpu instance (%s), and a pre-create probe carries no name to tell the plain one from a"+
				" media or graphics variant: fail closed",
			cardUUID, giProfile.Id, giProfile.SliceCount, len(candidates), describeComputeProfiles(candidates))
	}
}

// describeComputeProfiles renders compute-instance profile candidates as "id N: M compute slices"
// entries, so an ambiguity is diagnosable from one log line.
func describeComputeProfiles(infos []hgml.ComputeInstanceProfileInfo) string {
	parts := make([]string, 0, len(infos))
	for i := range infos {
		parts = append(parts, fmt.Sprintf("id %d: %d compute slices", infos[i].Id, infos[i].SliceCount))
	}
	return strings.Join(parts, "; ")
}

func (d *hgmlMigDriver) CardState(cardUUID, profile string, _, _ int32) (migCardState, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return migCardState{}, err
	}
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return migCardState{}, err
	}
	matched, err := matchProfile(infos, cardUUID, profile)
	if err != nil {
		return migCardState{}, err
	}

	slots, ret := dev.GetGpuInstancePossiblePlacements(matched.Id)
	if !ret.IsSuccess() {
		return migCardState{}, fmt.Errorf(
			"card %s: get possible placements of profile %d: %s", cardUUID, matched.Id, ret.Error())
	}
	possible := make([]migPlacement, 0, len(slots))
	for i := range slots {
		possible = append(possible, migPlacement{Start: int32(slots[i].Start), Length: int32(slots[i].Size)})
	}

	identityByGI, err := migIdentities(dev, cardUUID)
	if err != nil {
		return migCardState{}, err
	}
	live, err := liveInstances(dev, cardUUID, infos, identityByGI)
	if err != nil {
		return migCardState{}, err
	}

	return migCardState{ProfileID: matched.Id, Possible: possible, Live: live}, nil
}

// CreateInstance materializes a GPU instance of the named profile at the given placement and then
// the compute instance spanning it, and reports the partition's ids, raw profile id, geometry and
// identity as the driver itself reports them rather than as they were requested.
//
// Every failure after the GPU instance exists tears it down (its compute instance first, when one
// was created), so a half-built partition never survives to be adopted: a GPU instance without its
// compute instance addresses nothing, and a partition with no identity string could not be handed
// to a container nor recorded in a marker that fails closed.
func (d *hgmlMigDriver) CreateInstance(
	cardUUID, profile string, _, _ int32, slot migPlacement,
) (migInstance, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return migInstance{}, err
	}
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return migInstance{}, err
	}
	matched, err := matchProfile(infos, cardUUID, profile)
	if err != nil {
		return migInstance{}, err
	}

	placement := &hgml.GpuInstancePlacement{Start: uint32(slot.Start), Size: uint32(slot.Length)}
	gi, ret := dev.CreateGpuInstanceWithPlacement(&hgml.GpuInstanceProfileInfo{Id: matched.Id}, placement)
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf(
			"card %s: create gpu instance of profile %d at placement %d:%d: %s",
			cardUUID, matched.Id, slot.Start, slot.Length, ret.Error())
	}
	giInfo := gi.GetInfo()
	if giInfo.ProfileId != matched.Id {
		_ = gi.Destroy()
		return migInstance{}, fmt.Errorf(
			"card %s: created gpu instance %d reports profile %d, not the requested %d: fail closed",
			cardUUID, giInfo.Id, giInfo.ProfileId, matched.Id)
	}

	ciProfile, err := wholeGiComputeProfile(gi, cardUUID, matched)
	if err != nil {
		_ = gi.Destroy()
		return migInstance{}, err
	}
	ci, ret := gi.CreateComputeInstance(&ciProfile)
	if !ret.IsSuccess() {
		_ = gi.Destroy()
		return migInstance{}, fmt.Errorf(
			"card %s: create compute instance of profile %d on gpu instance %d: %s",
			cardUUID, ciProfile.Id, giInfo.Id, ret.Error())
	}

	identityByGI, err := migIdentities(dev, cardUUID)
	if err != nil {
		_ = ci.Destroy()
		_ = gi.Destroy()
		return migInstance{}, err
	}
	uuid := identityByGI[giInfo.Id].UUID
	if uuid == "" {
		_ = ci.Destroy()
		_ = gi.Destroy()
		return migInstance{}, fmt.Errorf(
			"card %s: created gpu instance %d has no partition identity", cardUUID, giInfo.Id)
	}

	return migInstance{
		GiID:          giInfo.Id,
		CiID:          ci.GetInfo().Id,
		ProfileID:     giInfo.ProfileId,
		ComputeSlices: int32(matched.SliceCount),
		Placement: migPlacement{
			Start:  int32(giInfo.Placement.Start),
			Length: int32(giInfo.Placement.Size),
		},
		UUID: uuid,
	}, nil
}

// ListInstances enumerates every live GPU instance across the node's partitioning accelerators, each
// carrying its accelerator id, so the orphan collector can find a marker-less instance on a drained
// accelerator. A GPU instance carries no operator tag, so this is the only way an untracked one is
// ever seen.
//
// An accelerator the driver answers is not in the partitioning mode holds no partition and is
// skipped; one whose mode, profiles, identities or instances could not be read fails the whole
// enumeration, because a caller acting on a list missing one accelerator's partitions would destroy
// or double-book exactly what it could not see.
func (d *hgmlMigDriver) ListInstances() ([]migLiveInstance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	count, ret := d.lib.DeviceGetCount()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("get device count: %s", ret.Error())
	}

	var out []migLiveInstance
	for i := 0; i < count; i++ {
		dev, ret := d.lib.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get device handle at index %d: %s", i, ret.Error())
		}
		cardUUID, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get card uuid at device index %d: %s", i, ret.Error())
		}

		live, err := d.cardInstances(dev, cardUUID)
		if err != nil {
			return nil, err
		}
		for j := range live {
			out = append(out, migLiveInstance{Card: cardUUID, Inst: live[j]})
		}
	}
	return out, nil
}

// DestroyInstance tears down the partition the caller snapshotted, under the accelerator lock the
// caller holds. It re-reads that accelerator's live set inside the critical section and verifies the
// GPU-instance id still carries the recorded identity before destroying anything: a destroyed
// instance's id can be reassigned, so a snapshot that aged by one allocation can point at a
// different — possibly live — partition. On a mismatch nothing is destroyed and the contradiction is
// returned.
//
// An id absent from a complete enumeration is a partition that is already gone, which is a success:
// the reclaim loop's removal of the ownership marker depends on that idempotence.
func (d *hgmlMigDriver) DestroyInstance(cardUUID string, inst migInstance) error {
	dev, err := d.device(cardUUID)
	if err != nil {
		return err
	}
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return err
	}
	identityByGI, err := migIdentities(dev, cardUUID)
	if err != nil {
		return err
	}

	for i := range infos {
		gis, ret := dev.GetGpuInstances(infos[i].Id)
		if !ret.IsSuccess() {
			return fmt.Errorf(
				"card %s: list live gpu instances of profile %d: %s", cardUUID, infos[i].Id, ret.Error())
		}
		for j := range gis {
			giInfo := gis[j].GetInfo()
			if giInfo.Id != inst.GiID {
				continue
			}
			if verr := verifyInstanceIdentity(cardUUID, inst, giInfo, identityByGI[giInfo.Id].UUID); verr != nil {
				return verr
			}
			return destroyGpuInstance(cardUUID, gis[j], inst.GiID)
		}
	}
	return nil
}

// verifyInstanceIdentity checks that the live GPU instance at a recorded id is still the partition
// that was recorded, by its raw profile id, its identity string and its placement. The placement
// test sits beside the identity test as an inconsistency trap: it is redundant against a
// self-consistent driver, and an instance matching one while contradicting the other is exactly the
// unprovable state a destroy must refuse rather than resolve.
func verifyInstanceIdentity(
	cardUUID string, inst migInstance, live hgml.GpuInstanceInfo, liveUUID string,
) error {
	if live.ProfileId != inst.ProfileID {
		return fmt.Errorf(
			"card %s: gpu instance %d now carries profile %d, not the recorded %d: refusing to destroy",
			cardUUID, inst.GiID, live.ProfileId, inst.ProfileID)
	}
	if liveUUID != inst.UUID {
		return fmt.Errorf(
			"card %s: gpu instance %d now carries identity %q, not the recorded %q: refusing to destroy",
			cardUUID, inst.GiID, liveUUID, inst.UUID)
	}
	if int32(live.Placement.Start) != inst.Placement.Start ||
		int32(live.Placement.Size) != inst.Placement.Length {
		return fmt.Errorf(
			"card %s: gpu instance %d now occupies %d:%d, not the recorded %d:%d: refusing to destroy",
			cardUUID, inst.GiID,
			live.Placement.Start, live.Placement.Size, inst.Placement.Start, inst.Placement.Length)
	}
	return nil
}

// destroyGpuInstance removes a GPU instance's compute instances and then the instance itself, in
// that order, because the driver rejects a GPU instance that still holds one. A busy rejection on
// either is mapped onto the shared in-use error, so the reclaim loop treats it as the bounded,
// retryable partial failure it is instead of a permanent one.
//
// It removes every compute instance the GPU instance reports under any compute-instance profile
// INDEX, not only the whole-instance index GPUStack creates from. That is deliberately wider than
// the vendor implementation this mirrors: an out-of-band or partially created compute instance of
// another index would leave the GPU instance's own teardown rejected as busy forever, turning a
// bounded retry into a permanently blocked reclamation that nothing on the node can clear.
//
// "Any profile" is any of those indexes and not any engine profile — a distinction worth stating,
// because the enumeration underneath resolves each index through the SHARED engine profile alone,
// so a reader could take this loop for a narrower sweep than it is. It is not narrower: SHARED is
// the only engine profile the vendor header defines (COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT is 1, as
// it is upstream), so there is no engine a compute instance could exist under and stay unseen. If a
// later header adds one, this sweep stops being exhaustive and the busy-forever case returns.
func destroyGpuInstance(cardUUID string, gi hgml.GpuInstance, giID uint32) error {
	for ciID := uint32(0); ciID < hgml.COMPUTE_INSTANCE_PROFILE_COUNT; ciID++ {
		cis, ret := gi.GetComputeInstances(ciID)
		if !ret.IsSuccess() {
			if driverReportsAbsent(ret) {
				continue
			}
			return fmt.Errorf(
				"card %s: list compute instances of profile %d on gpu instance %d: %s",
				cardUUID, ciID, giID, ret.Error())
		}
		for k := range cis {
			ciInfo := cis[k].GetInfo()
			if r := cis[k].Destroy(); !r.IsSuccess() {
				if driverReportsInUse(r) {
					return fmt.Errorf(
						"card %s: destroy compute instance %d on gpu instance %d: %w",
						cardUUID, ciInfo.Id, giID, errInstanceInUse)
				}
				return fmt.Errorf(
					"card %s: destroy compute instance %d on gpu instance %d: %s",
					cardUUID, ciInfo.Id, giID, r.Error())
			}
		}
	}

	if r := gi.Destroy(); !r.IsSuccess() {
		if driverReportsInUse(r) {
			return fmt.Errorf("card %s: destroy gpu instance %d: %w", cardUUID, giID, errInstanceInUse)
		}
		return fmt.Errorf("card %s: destroy gpu instance %d: %s", cardUUID, giID, r.Error())
	}
	return nil
}
