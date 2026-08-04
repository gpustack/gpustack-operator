package nvidia

import (
	"fmt"
	"strings"

	"gpustack.ai/gpustack/binding/nvml"
)

// newMigDriver returns the real NVML-backed MIG driver. It is linux-only: the device-manager
// runs only on linux, and linking the cgo binding/nvml into a darwin test binary (which links
// Go's plugin package) aborts at dyld load on the unresolved NVML symbols, so the darwin build
// uses the stub in mig_driver_other.go instead.
func newMigDriver() migDriver {
	l := nvml.New()
	return &nvmlMigDriver{lib: l, initRet: l.Init()}
}

// nvmlMigDriver is the real migDriver, driving binding/nvml on a card addressed by GPU UUID
// over the MIG GPU/compute-instance lifecycle wrappers. The exact create/reuse sequence and
// the MIG-device UUID resolution are validated on real hardware (F8 e2e); all unit tests use a
// fake driver.
type nvmlMigDriver struct {
	lib *nvml.NVML
	// initRet captures nvmlInit's result so the first device() call reports a single,
	// actionable root cause when the library failed to load/initialize.
	initRet nvml.Return
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

// profileID matches profile by name against the card's probed GPU-instance profiles and
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

// migUUIDs maps each live GPU-instance id on the device to its MIG-device UUID (the
// NVIDIA_VISIBLE_DEVICES value), by enumerating the card's MIG device handles and reading each
// one's owning GPU-instance id. A GI with no materialized MIG device yet is simply absent.
func (d *nvmlMigDriver) migUUIDs(dev nvml.Device) map[uint32]string {
	out := make(map[uint32]string)
	count, ret := dev.GetMaxMigDeviceCount()
	if !ret.IsSuccess() {
		return out
	}
	for i := 0; i < count; i++ {
		mig, ret := dev.GetMigDeviceHandleByIndex(i)
		if !ret.IsSuccess() || mig == nil {
			continue
		}
		// Read the owning GPU-instance id directly from the MIG device handle; resolving
		// the full instance needs the parent device and fails (INVALID_ARGUMENT) here.
		giID, ret := mig.GetGpuInstanceId()
		if !ret.IsSuccess() {
			continue
		}
		uuid, ret := mig.GetUUID()
		if !ret.IsSuccess() {
			continue
		}
		out[giID] = uuid
	}
	return out
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

	uuidByGI := d.migUUIDs(dev)

	// Collect every live GPU instance across all profiles, so occupancy accounts for
	// partitions of any profile (a 3g.20gb blocks a 1g.10gb's slot).
	var live []migInstance
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			continue
		}
		gis, ret := dev.GetGpuInstances(info.Id)
		if !ret.IsSuccess() {
			continue
		}
		for j := range gis {
			gi := gis[j].GetInfo()
			live = append(live, migInstance{
				GiID:          gi.Id,
				ComputeSlices: int32(info.SliceCount),
				Placement:     migPlacement{Start: int32(gi.Placement.Start), Length: int32(gi.Placement.Size)},
				UUID:          uuidByGI[gi.Id],
			})
		}
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

	// Resolve the MIG-device UUID (NVIDIA_VISIBLE_DEVICES) for the just-created GI.
	uuid := d.migUUIDs(dev)[giID]
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

// ListInstances enumerates every live GPU instance on every MIG-capable card, resolving each
// one's MIG-device UUID, so reclaim's orphan GC can find a marker-less GI on a drained card. It
// mirrors CardState's Live loop but across all cards and without the per-profile possible
// placements (orphan destroy needs only the ids + compute slices, not a slot to fill).
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
			continue
		}
		cardUUID, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			continue
		}
		uuidByGI := d.migUUIDs(dev)
		for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
			info, ret := dev.GetGpuInstanceProfileInfo(id)
			if !ret.IsSuccess() {
				continue
			}
			gis, ret := dev.GetGpuInstances(info.Id)
			if !ret.IsSuccess() {
				continue
			}
			for j := range gis {
				gi := gis[j].GetInfo()
				out = append(out, migLiveInstance{
					Card: cardUUID,
					Inst: migInstance{
						GiID:          gi.Id,
						ComputeSlices: int32(info.SliceCount),
						Placement:     migPlacement{Start: int32(gi.Placement.Start), Length: int32(gi.Placement.Size)},
						UUID:          uuidByGI[gi.Id],
					},
				})
			}
		}
	}
	return out, nil
}

func (d *nvmlMigDriver) DestroyInstance(cardUUID string, inst migInstance) error {
	dev, err := d.device(cardUUID)
	if err != nil {
		return err
	}
	ciIDs, ok := nvml.MigProfileIDsForComputeSlices(inst.ComputeSlices)
	if !ok {
		return fmt.Errorf("card %s: no compute-instance profile for %d compute slices", cardUUID, inst.ComputeSlices)
	}

	// Find the live GPU instance by id across profiles, then destroy its compute instances
	// before the instance itself (the F1 reverse sequence).
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			continue
		}
		gis, ret := dev.GetGpuInstances(info.Id)
		if !ret.IsSuccess() {
			continue
		}
		for j := range gis {
			if gis[j].GetInfo().Id != inst.GiID {
				continue
			}
			cis, ret := gis[j].GetComputeInstances(ciIDs.ComputeInstanceProfileID)
			if ret.IsSuccess() {
				for k := range cis {
					if r := cis[k].Destroy(); !r.IsSuccess() {
						if r == nvml.ERROR_IN_USE {
							return fmt.Errorf("card %s: destroy compute instance: %w", cardUUID, errInstanceInUse)
						}
						return fmt.Errorf("card %s: destroy compute instance: %w", cardUUID, r)
					}
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
