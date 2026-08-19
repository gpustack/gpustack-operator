package nvidia

import (
	"fmt"
	"strings"
	"sync"

	"gpustack.ai/gpustack/binding/nvml"
)

// newMigDriver returns the real NVML-backed MIG driver. It is linux-only: the device-manager
// runs only on linux, and linking the cgo binding/nvml into a darwin test binary (which links
// Go's plugin package) aborts at dyld load on the unresolved NVML symbols, so the darwin build
// uses the stub in mig_driver_other.go instead.
func newMigDriver() migDriver {
	l := nvml.New()
	return &nvmlMigDriver{
		lib:      l,
		initRet:  l.Init(),
		profiles: make(map[string][]nvml.GpuInstanceProfileInfo_v3),
	}
}

// nvmlMigDriver is the real migDriver, driving binding/nvml on an accelerator addressed by GPU
// UUID over the MIG GPU/compute-instance lifecycle wrappers. The exact create/reuse sequence and
// the MIG-device UUID resolution are validated on real hardware (F8 e2e); all unit tests use a
// fake driver.
type nvmlMigDriver struct {
	lib *nvml.NVML
	// initRet captures nvmlInit's result so the first device() call reports a single,
	// actionable root cause when the library failed to load/initialize.
	initRet nvml.Return

	// profiles caches each accelerator's MIG profile catalog under profilesMu. The catalog is a
	// property of the accelerator and of its MIG mode, and both are fixed for this process's life: a
	// mode change is only ever picked up by a reset and a restart. Caching it is worth the state
	// because probing it walks the whole profile id space and that walk happens on every allocation
	// and on every reclaim pass, the latter with an accelerator's lock held.
	//
	// An EMPTY catalog is deliberately not cached. That is what an accelerator reads as while MIG
	// is off, and remembering it would outlive the restart that turns MIG on. The lock is real
	// rather than theoretical: one driver value is shared by the partitioned and the visibility
	// servers.
	profilesMu sync.Mutex
	profiles   map[string][]nvml.GpuInstanceProfileInfo_v3
}

// cardProfileCatalogue returns the accelerator's MIG profile catalog, probing NVML only the first
// time it is asked for an accelerator that offers any.
func (d *nvmlMigDriver) cardProfileCatalogue(dev nvml.Device, cardUUID string) ([]nvml.GpuInstanceProfileInfo_v3, error) {
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

// CardInstances enumerates one accelerator's live GPU instances. It is the whole-node
// enumeration's unit of work, and the reclaim loop's verification re-read calls it directly so that
// read costs one accelerator rather than the node while it holds that accelerator's lock.
func (d *nvmlMigDriver) CardInstances(cardUUID string) ([]migInstance, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return nil, err
	}
	return d.cardInstances(dev, cardUUID)
}

// cardInstances is CardInstances over a handle the caller already resolved, so the whole-node walk
// does not resolve every accelerator twice. An accelerator whose driver disclaims MIG contributes
// nothing.
func (d *nvmlMigDriver) cardInstances(dev nvml.Device, cardUUID string) ([]migInstance, error) {
	uuidByGI, err := d.migUUIDs(dev, cardUUID)
	if err != nil {
		return nil, err
	}
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return nil, err
	}
	return liveInstances(dev, cardUUID, infos, uuidByGI)
}

// InstanceProcesses counts the compute processes running on one partition. The count is read off the
// MIG device handles addressing that partition, which are the only handles carrying a partition's own
// process list — the accelerator's own query answers for every tenant of the card.
//
// A partition can be subdivided further, and each of those subdivisions is a handle of its own, so
// every handle addressing the partition is asked and the first one answering with a process ends the
// walk: a caller deciding whether to destroy the partition is asking whether anything at all runs on
// it, and stopping at the first handle would let an idle subdivision speak for a busy one. An index
// the driver disclaims is skipped as the answer it is; every other failure to read a handle is an
// error, because a handle skipped in silence could be the one carrying the process.
//
// The handles are matched by GPU-instance id rather than by identity string, because an orphan
// discovered by enumeration alone may carry no identity at all, while its id is what named it in the
// first place.
//
// A partition no handle addresses is an error, never a zero count: a caller deciding whether to
// destroy it must not read "cannot ask" as "nobody is using it". No handle appears at all unless the
// container is allowed to see the driver's MIG capabilities.
func (d *nvmlMigDriver) InstanceProcesses(cardUUID string, inst migInstance) (int, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return 0, err
	}

	count, ret := dev.GetMaxMigDeviceCount()
	if !ret.IsSuccess() {
		if driverReportsAbsent(ret) {
			return 0, fmt.Errorf("card %s: accelerator addresses no mig device: %w", cardUUID, errNoAddressableDevice)
		}
		return 0, fmt.Errorf("card %s: get max mig device count: %w", cardUUID, ret)
	}
	addressed := false
	for i := 0; i < count; i++ {
		mig, ret := dev.GetMigDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			if driverReportsAbsent(ret) {
				continue
			}
			return 0, fmt.Errorf("card %s: get mig device handle %d: %w", cardUUID, i, ret)
		}
		if mig == nil {
			return 0, fmt.Errorf(
				"card %s: mig device handle %d is absent though the driver reported success", cardUUID, i)
		}
		giID, ret := mig.GetGpuInstanceId()
		if !ret.IsSuccess() {
			return 0, fmt.Errorf("card %s: get owning gpu-instance id of mig device %d: %w", cardUUID, i, ret)
		}
		if giID != inst.GiID {
			continue
		}
		addressed = true
		procs, ret := mig.GetComputeRunningProcesses()
		if !ret.IsSuccess() {
			return 0, fmt.Errorf(
				"card %s: list compute processes on gpu instance %d: %w", cardUUID, inst.GiID, ret)
		}
		if len(procs) > 0 {
			return len(procs), nil
		}
	}
	if !addressed {
		return 0, fmt.Errorf(
			"card %s: gpu instance %d: %w", cardUUID, inst.GiID, errNoAddressableDevice)
	}
	return 0, nil
}

func (d *nvmlMigDriver) device(cardUUID string) (nvml.Device, error) {
	if !d.initRet.IsSuccess() {
		return nvml.Device{}, fmt.Errorf("nvml init failed: %w", d.initRet)
	}
	dev, ret := d.lib.DeviceGetHandleByUUID(cardUUID)
	if !ret.IsSuccess() {
		return nvml.Device{}, fmt.Errorf("get nvml handle for %s: %w", cardUUID, ret)
	}
	return dev, nil
}

// profileID matches profile by name against the accelerator's probed GPU-instance profiles and
// returns its NVML GPU-instance profile id, skipping the +me/+gfx variants GPUStack does not
// expose (the same filter the detector applies). It disambiguates the same-compute REV
// profiles (1g.5gb vs 1g.10gb) a compute-slice count alone cannot.
func (d *nvmlMigDriver) profileID(dev nvml.Device, profile string) (uint32, error) {
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			continue
		}
		name := info.GetName()
		if strings.ContainsRune(name, '+') || info.Capabilities&nvml.GPU_INSTANCE_PROFILE_CAPS_GFX != 0 {
			continue
		}
		if name == profile {
			return info.Id, nil
		}
	}
	return 0, fmt.Errorf("card has no gpu-instance profile named %q", profile)
}

// driverReportsAbsent reports whether a non-success return is NVML ANSWERING that there is nothing
// at the id asked about, as opposed to failing to answer at all. Everything below rests on the
// distinction: a disclaimed id is state, and an unreadable one is a hole in a view whose completeness
// each caller here depends on.
func driverReportsAbsent(ret nvml.Return) bool {
	switch ret {
	case nvml.ERROR_NOT_SUPPORTED, nvml.ERROR_NOT_FOUND, nvml.ERROR_INVALID_ARGUMENT:
		return true
	default:
		return false
	}
}

// migUUIDs maps each live GPU-instance id on the device to its MIG-device UUID (the
// NVIDIA_VISIBLE_DEVICES value), by enumerating the accelerator's MIG device handles and reading
// each one's owning GPU-instance id. A GI with no materialized MIG device yet is simply absent —
// that is a GPU instance without its compute instance, which addresses nothing and which reclaim
// destroys.
//
// An accelerator whose driver disclaims MIG devices altogether yields an empty map and no error:
// that is a plain GPU, and the enumeration above it walks every accelerator on the node. Every
// other failure IS an error, because a handle in hand whose owner or identity cannot be read leaves
// the map missing a live partition — and a missing identity is exactly what makes a live partition
// look reclaimable, or makes a destroy verify against nothing.
func (d *nvmlMigDriver) migUUIDs(dev nvml.Device, cardUUID string) (map[uint32]string, error) {
	count, ret := dev.GetMaxMigDeviceCount()
	if !ret.IsSuccess() {
		if driverReportsAbsent(ret) {
			return map[uint32]string{}, nil
		}
		return nil, fmt.Errorf("card %s: get max mig device count: %w", cardUUID, ret)
	}
	out := make(map[uint32]string, count)
	for i := 0; i < count; i++ {
		mig, ret := dev.GetMigDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			if driverReportsAbsent(ret) {
				continue
			}
			return nil, fmt.Errorf("card %s: get mig device handle %d: %w", cardUUID, i, ret)
		}
		if mig == nil {
			return nil, fmt.Errorf(
				"card %s: mig device handle %d is absent though the driver reported success", cardUUID, i)
		}
		// Read the owning GPU-instance id directly from the MIG device handle; resolving
		// the full instance needs the parent device and fails (INVALID_ARGUMENT) here.
		giID, ret := mig.GetGpuInstanceId()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("card %s: get owning gpu-instance id of mig device %d: %w", cardUUID, i, ret)
		}
		uuid, ret := mig.GetUUID()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("card %s: get identity of mig device %d: %w", cardUUID, i, ret)
		}
		if prev, dup := out[giID]; dup {
			return nil, fmt.Errorf(
				"card %s: gpu instance %d owns more than one mig device (%s and %s): refusing to address it by either",
				cardUUID, giID, prev, uuid)
		}
		out[giID] = uuid
	}
	return out, nil
}

// cardProfiles probes every GPU-instance profile id on the accelerator and returns the ones the
// driver answered for. An id the driver disclaims is skipped as the answer it is; an id it could
// not read fails the whole probe, because a profile missing from this set takes its live instances
// with it and every caller here reads the result as complete.
func cardProfiles(dev nvml.Device, cardUUID string) ([]nvml.GpuInstanceProfileInfo_v3, error) {
	var infos []nvml.GpuInstanceProfileInfo_v3
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			if driverReportsAbsent(ret) {
				continue
			}
			return nil, fmt.Errorf("card %s: get gpu-instance profile info %d: %w", cardUUID, id, ret)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// liveInstances enumerates every live GPU instance on the accelerator across all profiles, each
// carrying its compute-slice count, its placement and its MIG-device identity. Occupancy must span
// every profile, because an instance of one profile occupies a placement another profile could
// otherwise use.
//
// A failed instance query is an error rather than a skipped profile. Two callers make that
// load-bearing: the allocation path subtracts these placements to pick a free slot, so a profile
// silently missing hands out a slot that is already taken; and reclaim reads absence from this set as
// "already gone" and removes the ownership marker, so a live partition reading as absent leaks with no
// owner and its placement is handed out a second time.
func liveInstances(
	dev nvml.Device, cardUUID string, infos []nvml.GpuInstanceProfileInfo_v3, uuidByGI map[uint32]string,
) ([]migInstance, error) {
	var live []migInstance
	for i := range infos {
		info := infos[i]
		gis, ret := dev.GetGpuInstances(info.Id)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("card %s: list live gpu instances of profile %d: %w", cardUUID, info.Id, ret)
		}
		for j := range gis {
			gi := gis[j].GetInfo()
			if gi.ProfileId != info.Id {
				return nil, fmt.Errorf(
					"card %s: gpu instance %d reports profile %d while enumerated under profile %d: fail closed",
					cardUUID, gi.Id, gi.ProfileId, info.Id)
			}
			live = append(live, migInstance{
				GiID:          gi.Id,
				ComputeSlices: int32(info.SliceCount),
				Placement:     migPlacement{Start: int32(gi.Placement.Start), Length: int32(gi.Placement.Size)},
				UUID:          uuidByGI[gi.Id],
			})
		}
	}
	return live, nil
}

func (d *nvmlMigDriver) CardState(cardUUID, profile string, _, _ int32) (migCardState, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return migCardState{}, err
	}

	targetID, err := d.profileID(dev, profile)
	if err != nil {
		return migCardState{}, fmt.Errorf("card %s: %w", cardUUID, err)
	}
	possibleSlots, ret := dev.GetGpuInstancePossiblePlacements(targetID)
	if !ret.IsSuccess() {
		return migCardState{}, fmt.Errorf("card %s: get possible placements: %w", cardUUID, ret)
	}
	possible := make([]migPlacement, 0, len(possibleSlots))
	for i := range possibleSlots {
		possible = append(possible, migPlacement{Start: int32(possibleSlots[i].Start), Length: int32(possibleSlots[i].Size)})
	}

	uuidByGI, err := d.migUUIDs(dev, cardUUID)
	if err != nil {
		return migCardState{}, err
	}

	// Collect every live GPU instance across all profiles, so occupancy accounts for
	// partitions of any profile (a 3g.20gb blocks a 1g.10gb's slot).
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return migCardState{}, err
	}
	live, err := liveInstances(dev, cardUUID, infos, uuidByGI)
	if err != nil {
		return migCardState{}, err
	}

	return migCardState{Possible: possible, Live: live}, nil
}

func (d *nvmlMigDriver) CreateInstance(
	cardUUID, profile string, computeSlices, _ int32, slot migPlacement,
) (migInstance, error) {
	dev, err := d.device(cardUUID)
	if err != nil {
		return migInstance{}, err
	}

	targetID, err := d.profileID(dev, profile)
	if err != nil {
		return migInstance{}, fmt.Errorf("card %s: %w", cardUUID, err)
	}
	ciIDs, ok := nvml.MigProfileIDsForComputeSlices(computeSlices)
	if !ok {
		return migInstance{}, fmt.Errorf("card %s: no compute-instance profile for %d compute slices", cardUUID, computeSlices)
	}

	giInfo := &nvml.GpuInstanceProfileInfo{Id: targetID}
	placement := &nvml.GpuInstancePlacement{Start: uint32(slot.Start), Size: uint32(slot.Length)}
	gi, ret := dev.CreateGpuInstanceWithPlacement(giInfo, placement)
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("card %s: create gpu instance: %w", cardUUID, ret)
	}
	giID := gi.GetInfo().Id

	ciInfo, ret := gi.GetComputeInstanceProfileInfo(ciIDs.ComputeInstanceProfileID, ciIDs.ComputeInstanceEngineProfileID)
	if !ret.IsSuccess() {
		_ = gi.Destroy()
		return migInstance{}, fmt.Errorf("card %s: get compute-instance profile info: %w", cardUUID, ret)
	}
	ci, ret := gi.CreateComputeInstance(&ciInfo)
	if !ret.IsSuccess() {
		// Mirror mig-parted's cleanup: a GI without its CI is unusable, so destroy it.
		_ = gi.Destroy()
		return migInstance{}, fmt.Errorf("card %s: create compute instance: %w", cardUUID, ret)
	}

	// Resolve the MIG-device UUID (NVIDIA_VISIBLE_DEVICES) for the just-created GI. A read that
	// fails is rolled back like any other failure here: the pair is unusable without the identity
	// the container is addressed by, and leaving it behind would strand the placement.
	uuidByGI, err := d.migUUIDs(dev, cardUUID)
	if err != nil {
		_ = ci.Destroy()
		_ = gi.Destroy()
		return migInstance{}, err
	}
	uuid := uuidByGI[giID]
	if uuid == "" {
		_ = ci.Destroy()
		_ = gi.Destroy()
		return migInstance{}, fmt.Errorf("card %s: created gpu instance %d has no mig-device uuid", cardUUID, giID)
	}

	return migInstance{
		GiID:          giID,
		CiID:          ci.GetInfo().Id,
		ComputeSlices: computeSlices,
		Placement:     slot,
		UUID:          uuid,
	}, nil
}

// ListInstances enumerates every live GPU instance on every MIG-capable accelerator, resolving each
// one's MIG-device UUID, so reclaim's orphan GC can find a marker-less GI on a drained accelerator.
// It mirrors CardState's Live loop but across all accelerators and without the per-profile possible
// placements (orphan destroy needs only the ids + compute slices, not a slot to fill).
//
// An accelerator the driver answers has no MIG devices holds no partition and contributes nothing;
// an accelerator whose handle, identity, profiles, mig devices or instances could not be READ fails
// the whole enumeration. The difference matters because of what the callers do with absence:
// reclaim reads a missing GPU instance as one already gone and removes its ownership marker, and
// reads an accelerator contributing nothing as a drained accelerator whose orphans it may collect.
// A list quietly short of one accelerator's partitions therefore destroys or double-books exactly
// what it could not see.
func (d *nvmlMigDriver) ListInstances() ([]migLiveInstance, error) {
	if !d.initRet.IsSuccess() {
		return nil, fmt.Errorf("nvml init failed: %w", d.initRet)
	}
	count, ret := d.lib.DeviceGetCount()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("get device count: %w", ret)
	}
	var out []migLiveInstance
	for i := 0; i < count; i++ {
		dev, ret := d.lib.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get device handle at index %d: %w", i, ret)
		}
		cardUUID, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get card uuid at device index %d: %w", i, ret)
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

// DestroyInstance tears down the MIG instance the caller snapshotted, under the accelerator lock
// the caller holds. It re-reads the accelerator's live set inside that critical section and
// verifies the GPU-instance id still carries the recorded identity before destroying anything: a
// destroyed instance's id can be reassigned by NVML, so a snapshot that aged by one allocation can
// point at a different — possibly live — instance. On a mismatch nothing is destroyed and the
// contradiction is returned.
//
// An id absent from a COMPLETE enumeration is an instance that is already gone, which is a success:
// the reclaim loop's removal of the ownership marker depends on that idempotence. Which is exactly why
// an incomplete enumeration may not reach that return — a profile or instance query that failed used
// to fall through to it and report a destroy that never happened, after which the marker was removed
// as reclaimed and the live instance leaked with no owner.
func (d *nvmlMigDriver) DestroyInstance(cardUUID string, inst migInstance) error {
	dev, err := d.device(cardUUID)
	if err != nil {
		return err
	}
	ciIDs, ok := nvml.MigProfileIDsForComputeSlices(inst.ComputeSlices)
	if !ok {
		return fmt.Errorf("card %s: no compute-instance profile for %d compute slices", cardUUID, inst.ComputeSlices)
	}
	infos, err := d.cardProfileCatalogue(dev, cardUUID)
	if err != nil {
		return err
	}
	uuidByGI, err := d.migUUIDs(dev, cardUUID)
	if err != nil {
		return err
	}

	// Find the live GPU instance by id across profiles, then destroy its compute instances
	// before the instance itself (the F1 reverse sequence).
	for i := range infos {
		gis, ret := dev.GetGpuInstances(infos[i].Id)
		if !ret.IsSuccess() {
			return fmt.Errorf("card %s: list live gpu instances of profile %d: %w", cardUUID, infos[i].Id, ret)
		}
		for j := range gis {
			giInfo := gis[j].GetInfo()
			if giInfo.Id != inst.GiID {
				continue
			}
			if verr := verifyInstanceIdentity(cardUUID, inst, giInfo, uuidByGI[giInfo.Id]); verr != nil {
				return verr
			}
			cis, ret := gis[j].GetComputeInstances(ciIDs.ComputeInstanceProfileID)
			if !ret.IsSuccess() && !driverReportsAbsent(ret) {
				return fmt.Errorf(
					"card %s: list compute instances on gpu instance %d: %w", cardUUID, inst.GiID, ret)
			}
			for k := range cis {
				if r := cis[k].Destroy(); !r.IsSuccess() {
					if r == nvml.ERROR_IN_USE {
						return fmt.Errorf("card %s: destroy compute instance: %w", cardUUID, errInstanceInUse)
					}
					return fmt.Errorf("card %s: destroy compute instance: %w", cardUUID, r)
				}
			}
			if r := gis[j].Destroy(); !r.IsSuccess() {
				if r == nvml.ERROR_IN_USE {
					return fmt.Errorf("card %s: destroy gpu instance %d: %w", cardUUID, inst.GiID, errInstanceInUse)
				}
				return fmt.Errorf("card %s: destroy gpu instance %d: %w", cardUUID, inst.GiID, r)
			}
			return nil
		}
	}
	return nil
}

// verifyInstanceIdentity checks that the live GPU instance at a recorded id is still the instance that
// was recorded, by its identity string and its placement. The placement test sits beside the identity
// test as an inconsistency trap: it is redundant against a self-consistent driver, and an instance
// matching one while contradicting the other is exactly the unprovable state a destroy must refuse
// rather than resolve.
func verifyInstanceIdentity(
	cardUUID string, inst migInstance, live nvml.GpuInstanceInfo, liveUUID string,
) error {
	if liveUUID != inst.UUID {
		return fmt.Errorf(
			"card %s: gpu instance %d now carries mig device %q, not the recorded %q: refusing to destroy",
			cardUUID, inst.GiID, liveUUID, inst.UUID)
	}
	if int32(live.Placement.Start) != inst.Placement.Start || int32(live.Placement.Size) != inst.Placement.Length {
		return fmt.Errorf(
			"card %s: gpu instance %d now occupies slices [%d,%d), not the recorded [%d,%d): refusing to destroy",
			cardUUID, inst.GiID,
			live.Placement.Start, live.Placement.Start+live.Placement.Size,
			inst.Placement.Start, inst.Placement.Start+inst.Placement.Length)
	}
	return nil
}
