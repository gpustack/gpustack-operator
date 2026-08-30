//go:build linux

package hygon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/dmi"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// migSliceWidths is the instance-width space a profile lookup sweeps. The library's profile query
// takes a width rather than a profile id, and a card offering no profile of some width answers with
// an absent code -- every measured card has such a hole, at three slices.
var migSliceWidths = []uint32{1, 2, 3, 4}

// newMigDriver builds the real driver. It is linux-only because the darwin test binary, which links
// Go's plugin package, cannot host this allocator's vendor bindings; the darwin build compiles
// against the stub in mig_driver_other.go instead.
func newMigDriver() migDriver {
	l := dmi.New()
	return &dmiMigDriver{lib: l, initRet: l.Init(klog.Background())}
}

// dmiMigDriver drives the vendor Multi-Instance binding over the partition lifecycle.
//
// Every enumeration reports either a state it can prove whole or an error. It never degrades to
// partial state on a failed query: a live partition that reads as absent has its ownership record
// removed as "already gone", its placement handed out a second time, and a record-less one leaks
// past the orphan collector.
type dmiMigDriver struct {
	lib *dmi.DMI
	// initRet captures the library's initialization result, so every operation reports one
	// actionable root cause when the library could not be loaded instead of failing obscurely
	// deeper in a call that cannot possibly work.
	initRet dmi.Return
}

// device resolves one accelerator's Multi-Instance handle from its PCI address.
//
// The address is the only bridge: this library serves no UUID lookup and answers its own PCI query
// with an empty string, so a caller holding an identity from anywhere else reaches the card here or
// not at all.
func (d *dmiMigDriver) device(pciBusID string) (dmi.Device, error) {
	if !d.initRet.IsSuccess() {
		return dmi.Device{}, fmt.Errorf(
			"hygon multi-instance library unavailable (%v); the node cannot serve partitions", d.initRet)
	}
	dev, ret := d.lib.GetDeviceHandleByPciBusId(pciBusID)
	if !ret.IsSuccess() {
		return dmi.Device{}, fmt.Errorf("resolve card %s: %w", pciBusID, ret)
	}
	return dev, nil
}

// migProfile is one of a card's GPU-instance profiles, under the two things the lifecycle needs: the
// vendor id that addresses it, and the width that selects the whole-GI compute profile inside it.
type migProfile struct {
	id     uint32
	slices uint32
}

// resolveMigProfile finds the profile a requested name refers to.
//
// The sweep is by width because the library's query takes one, and each answer carries its own id --
// the two are unrelated, so a name can only be matched by reading every width the card offers. A
// width the card disclaims is skipped as the answer it is; a width it could not answer for is an
// error, because a profile missing from this sweep is a profile the request would be refused for.
func resolveMigProfile(dev dmi.Device, profile string) (migProfile, error) {
	want := nodefeature.NormalizePartitionedProfileName(profile)
	if want == "" {
		return migProfile{}, fmt.Errorf("empty partition profile requested")
	}
	for _, width := range migSliceWidths {
		info, ret := dev.GetGpuInstanceProfileInfoBySliceCount(width)
		if !ret.IsSuccess() {
			if ret.ReportsAbsent() {
				continue
			}
			return migProfile{}, fmt.Errorf("read %d-slice profile: %w", width, ret)
		}
		if nodefeature.NormalizePartitionedProfileName(migProfileName(info)) != want {
			continue
		}
		return migProfile{id: info.Id, slices: info.Gpu_slice_count}, nil
	}
	return migProfile{}, fmt.Errorf("card offers no partition profile named %q", profile)
}

// cardProfiles returns every GPU-instance profile the card offers, so the live-instance sweep can
// ask about each of them. A partial catalog is an error: an instance of a profile missed here is an
// instance whose placement would be handed out again.
func cardProfiles(dev dmi.Device) ([]migProfile, error) {
	var profiles []migProfile
	for _, width := range migSliceWidths {
		info, ret := dev.GetGpuInstanceProfileInfoBySliceCount(width)
		if !ret.IsSuccess() {
			if ret.ReportsAbsent() {
				continue
			}
			return nil, fmt.Errorf("read %d-slice profile: %w", width, ret)
		}
		profiles = append(profiles, migProfile{id: info.Id, slices: info.Gpu_slice_count})
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("card offers no partition profile at all")
	}
	return profiles, nil
}

// CardState implements migDriver.
func (d *dmiMigDriver) CardState(pciBusID, profile string) (migCardState, error) {
	dev, err := d.device(pciBusID)
	if err != nil {
		return migCardState{}, err
	}

	want, err := resolveMigProfile(dev, profile)
	if err != nil {
		return migCardState{}, fmt.Errorf("card %s: %w", pciBusID, err)
	}

	slots, ret := dev.GetGpuInstancePossiblePlacements(want.id)
	if !ret.IsSuccess() {
		return migCardState{}, fmt.Errorf("card %s: read placements of profile %d: %w", pciBusID, want.id, ret)
	}
	possible := make([]migPlacement, 0, len(slots))
	for _, s := range slots {
		possible = append(possible, migPlacement{Start: int32(s.Start), Length: int32(s.Size)})
	}

	live, err := d.liveInstances(dev, pciBusID)
	if err != nil {
		return migCardState{}, err
	}

	return migCardState{ProfileID: want.id, Possible: possible, Live: live}, nil
}

// liveInstances enumerates every GPU instance on the card, of every profile.
//
// Every profile is asked about because the query filters by profile id: asking about one and reading
// the answer as "the card's instances" would miss every instance of another width, and the placement
// selection that consumes this would then choose an occupied run.
func (d *dmiMigDriver) liveInstances(dev dmi.Device, pciBusID string) ([]migInstance, error) {
	index, ret := dev.GetIndex()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("card %s: read device index: %w", pciBusID, ret)
	}

	profiles, err := cardProfiles(dev)
	if err != nil {
		return nil, fmt.Errorf("card %s: %w", pciBusID, err)
	}

	var live []migInstance
	for _, p := range profiles {
		gis, ret := dev.GetGpuInstances(p.id)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("card %s: list instances of profile %d: %w", pciBusID, p.id, ret)
		}
		for _, gi := range gis {
			info, ret := gi.GetInfo()
			if !ret.IsSuccess() {
				return nil, fmt.Errorf("card %s: read a gpu instance of profile %d: %w", pciBusID, p.id, ret)
			}

			// A GPU instance with no compute instance yet is a half-built partition: the allocator
			// always creates both, so it can only be one this process was interrupted mid-create. It
			// occupies its placement all the same, so it is reported -- with no identity, which is
			// what keeps it out of adoption and lets the orphan collector clean it up.
			ciID, uuid, confPath, found, err := d.computeInstanceOf(gi, p, index, info.Id)
			if err != nil {
				return nil, fmt.Errorf("card %s: gpu instance %d: %w", pciBusID, info.Id, err)
			}
			inst := migInstance{
				GiID:      info.Id,
				ProfileID: info.Profile_id,
				Placement: migPlacement{Start: int32(info.Placement.Start), Length: int32(info.Placement.Size)},
			}
			if found {
				inst.CiID, inst.UUID, inst.ConfPath = ciID, uuid, confPath
			}
			live = append(live, inst)
		}
	}
	return live, nil
}

// computeInstanceOf returns the whole-GI compute instance inside a GPU instance, with the identity
// its registry file carries.
//
// Only the whole-GI profile is asked for: that is the only shape this allocator ever creates, since
// a container here can use exactly one instance and a narrower compute instance would hand it less
// of the GPU instance than the profile it was granted promises.
func (d *dmiMigDriver) computeInstanceOf(
	gi dmi.GpuInstance, p migProfile, deviceIndex, giID uint32,
) (ciID uint32, uuid, confPath string, found bool, err error) {
	info, ret := gi.GetComputeInstanceProfileInfoBySliceCount(p.slices, 0)
	if !ret.IsSuccess() {
		if ret.ReportsAbsent() {
			return 0, "", "", false, nil
		}
		return 0, "", "", false, fmt.Errorf("read the whole-instance compute profile: %w", ret)
	}

	cis, ret := gi.GetComputeInstances(info.Id)
	if !ret.IsSuccess() {
		return 0, "", "", false, fmt.Errorf("list compute instances of profile %d: %w", info.Id, ret)
	}
	if len(cis) == 0 {
		return 0, "", "", false, nil
	}

	ciInfo, ret := cis[0].GetInfo()
	if !ret.IsSuccess() {
		return 0, "", "", false, fmt.Errorf("read a compute instance: %w", ret)
	}

	path := migConfPath(deviceIndex, giID, ciInfo.Id)
	id, err := readMigConfUUID(path)
	if err != nil {
		return 0, "", "", false, fmt.Errorf("read the instance registry file %q: %w", path, err)
	}
	if id == "" {
		return 0, "", "", false, fmt.Errorf("the instance registry file %q carries no identity", path)
	}
	return ciInfo.Id, id, path, true, nil
}

// CreateInstance implements migDriver.
//
// The two halves are created together because a GPU instance without its compute instance is not a
// usable partition -- the vendor runtime activates a container from the compute instance's file, and
// there would be none. A failure after the GPU instance exists therefore destroys it rather than
// leaving a half-built partition occupying a placement.
func (d *dmiMigDriver) CreateInstance(pciBusID, profile string, slot migPlacement) (migInstance, error) {
	dev, err := d.device(pciBusID)
	if err != nil {
		return migInstance{}, err
	}

	want, err := resolveMigProfile(dev, profile)
	if err != nil {
		return migInstance{}, fmt.Errorf("card %s: %w", pciBusID, err)
	}

	index, ret := dev.GetIndex()
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("card %s: read device index: %w", pciBusID, ret)
	}

	gi, ret := dev.CreateGpuInstanceWithPlacement(want.id, dmi.GpuInstancePlacement{
		Start: uint32(slot.Start),
		Size:  uint32(slot.Length),
	})
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("card %s: create gpu instance of profile %d at %d:%d: %w",
			pciBusID, want.id, slot.Start, slot.Length, ret)
	}

	inst, err := d.completeInstance(gi, want, index)
	if err != nil {
		if r := gi.Destroy(); !r.IsSuccess() {
			return migInstance{}, fmt.Errorf(
				"card %s: %w (and the half-built gpu instance could not be removed: %v)", pciBusID, err, r)
		}
		return migInstance{}, fmt.Errorf("card %s: %w", pciBusID, err)
	}
	return inst, nil
}

// completeInstance creates the whole-GI compute instance and reads back the identity the vendor
// assigned it.
//
// IT UNDOES ITS OWN COMPUTE INSTANCE ON THE WAY OUT. The teardown order is forced -- a GPU instance
// still holding one cannot be removed -- so a compute instance left behind here does not merely leak
// itself: it makes the caller's own rollback fail, stranding the GPU instance too. Nothing then
// attributes either, since the ownership record the reservation would have written was never
// reached, and the placement stays occupied until the node is rebuilt.
func (d *dmiMigDriver) completeInstance(
	gi dmi.GpuInstance, p migProfile, deviceIndex uint32,
) (inst migInstance, err error) {
	giInfo, ret := gi.GetInfo()
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("read the created gpu instance: %w", ret)
	}

	ciProfile, ret := gi.GetComputeInstanceProfileInfoBySliceCount(p.slices, 0)
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("read the whole-instance compute profile: %w", ret)
	}

	ci, ret := gi.CreateComputeInstance(ciProfile.Id)
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("create the compute instance of profile %d: %w", ciProfile.Id, ret)
	}
	defer func() {
		if err == nil {
			return
		}
		if r := ci.Destroy(); !r.IsSuccess() {
			err = fmt.Errorf(
				"%w (and the compute instance could not be removed, so the gpu instance holding it"+
					" cannot be either: %v)", err, r)
		}
	}()

	ciInfo, ret := ci.GetInfo()
	if !ret.IsSuccess() {
		return migInstance{}, fmt.Errorf("read the created compute instance: %w", ret)
	}

	path := migConfPath(deviceIndex, giInfo.Id, ciInfo.Id)
	uuid, cerr := readMigConfUUID(path)
	if cerr != nil {
		return migInstance{}, fmt.Errorf("read the instance registry file %q: %w", path, cerr)
	}
	if uuid == "" {
		return migInstance{}, fmt.Errorf("the instance registry file %q carries no identity", path)
	}

	return migInstance{
		GiID:      giInfo.Id,
		CiID:      ciInfo.Id,
		ProfileID: giInfo.Profile_id,
		Placement: migPlacement{Start: int32(giInfo.Placement.Start), Length: int32(giInfo.Placement.Size)},
		UUID:      uuid,
		ConfPath:  path,
	}, nil
}

// DestroyInstance implements migDriver.
//
// The order is forced by the vendor: a GPU instance still holding a compute instance cannot be
// removed. The identity is re-verified first, under the caller's accelerator lock, so a partition
// recreated at the same ids since the snapshot is never torn down in place of the one recorded --
// and because this vendor issues a fresh identity on every create, that check is complete.
func (d *dmiMigDriver) DestroyInstance(pciBusID string, inst migInstance) error {
	dev, err := d.device(pciBusID)
	if err != nil {
		return err
	}

	live, err := d.liveInstances(dev, pciBusID)
	if err != nil {
		return err
	}

	var found *migInstance
	for i := range live {
		if live[i].GiID == inst.GiID {
			found = &live[i]
			break
		}
	}
	if found == nil {
		// Already gone. Reporting success is what makes a repeated teardown idempotent, which the
		// reclaim loop relies on.
		return nil
	}
	if inst.UUID != "" && found.UUID != "" && found.UUID != inst.UUID {
		return fmt.Errorf(
			"card %s: gpu instance %d now carries identity %q, not the recorded %q (id reused): refusing to destroy it",
			pciBusID, inst.GiID, found.UUID, inst.UUID)
	}

	profiles, err := cardProfiles(dev)
	if err != nil {
		return fmt.Errorf("card %s: %w", pciBusID, err)
	}

	for _, p := range profiles {
		gis, ret := dev.GetGpuInstances(p.id)
		if !ret.IsSuccess() {
			return fmt.Errorf("card %s: list instances of profile %d: %w", pciBusID, p.id, ret)
		}
		for _, gi := range gis {
			info, ret := gi.GetInfo()
			if !ret.IsSuccess() || info.Id != inst.GiID {
				continue
			}
			return d.destroyGpuInstance(pciBusID, gi, p)
		}
	}
	return nil
}

// destroyGpuInstance removes a GPU instance's compute instances and then the instance itself.
func (d *dmiMigDriver) destroyGpuInstance(pciBusID string, gi dmi.GpuInstance, p migProfile) error {
	ciProfile, ret := gi.GetComputeInstanceProfileInfoBySliceCount(p.slices, 0)
	if ret.IsSuccess() {
		cis, ret := gi.GetComputeInstances(ciProfile.Id)
		if !ret.IsSuccess() {
			return fmt.Errorf("card %s: list compute instances: %w", pciBusID, ret)
		}
		for _, ci := range cis {
			if ret := ci.Destroy(); !ret.IsSuccess() {
				return migDestroyError(pciBusID, "compute instance", ret)
			}
		}
	} else if !ret.ReportsAbsent() {
		return fmt.Errorf("card %s: read the whole-instance compute profile: %w", pciBusID, ret)
	}

	if ret := gi.Destroy(); !ret.IsSuccess() {
		return migDestroyError(pciBusID, "gpu instance", ret)
	}
	return nil
}

// migDestroyError wraps a refused teardown, marking the one case a caller must not retry through.
//
// The driver refusing because something still holds the partition is the authoritative answer to
// "is anyone using it" -- this library serves no per-partition process query at all, so there is
// nothing else to ask.
func migDestroyError(pciBusID, what string, ret dmi.Return) error {
	if ret == dmi.ERROR_IN_USE {
		return fmt.Errorf("card %s: destroy %s: %w: %v", pciBusID, what, errInstanceInUse, ret)
	}
	return fmt.Errorf("card %s: destroy %s: %w", pciBusID, what, ret)
}

// ListInstances implements migDriver.
func (d *dmiMigDriver) ListInstances() ([]migLiveInstance, error) {
	if !d.initRet.IsSuccess() {
		return nil, fmt.Errorf(
			"hygon multi-instance library unavailable (%v); the node cannot serve partitions", d.initRet)
	}

	count, ret := d.lib.GetDeviceCount()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("count cards: %w", ret)
	}

	var out []migLiveInstance
	for i := uint32(0); i < count; i++ {
		pciBusID, err := migCardPciBusID(i)
		if err != nil {
			return nil, err
		}
		dev, ret := d.lib.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("card %d: resolve handle: %w", i, ret)
		}
		live, err := d.liveInstances(dev, pciBusID)
		if err != nil {
			return nil, err
		}
		for _, inst := range live {
			out = append(out, migLiveInstance{PciBusID: pciBusID, Instance: inst})
		}
	}
	return out, nil
}

// migCardPciBusID reads the PCI address the vendor records for a device index.
//
// The library's own PCI query returns success and writes an empty string, so this one-line file is
// the mapping. It exists only while the node is in Multi-Instance mode, which is the only time this
// driver runs at all.
func migCardPciBusID(index uint32) (string, error) {
	path := filepath.Join(migConfigDir, fmt.Sprintf("dev%d", index))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the pci address of card %d from %q: %w", index, path, err)
	}
	addr := strings.TrimSpace(string(data))
	if addr == "" {
		return "", fmt.Errorf("the pci address file %q of card %d is empty", path, index)
	}
	return addr, nil
}

// migConfPath names a compute instance's registry file, which is both its identity's home and the
// artifact a container binds to use it.
func migConfPath(deviceIndex, giID, ciID uint32) string {
	return filepath.Join(migConfigDir, "ci", fmt.Sprintf("dev%dgi%dci%d.conf", deviceIndex, giID, ciID))
}

// readMigConfUUID returns the identity a compute-instance registry file carries.
//
// The file is the concatenation of its GPU instance's fields and its own, so several keys repeat;
// the identity appears once and only in the compute-instance half, which is why it is read by key.
func readMigConfUUID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if !ok || strings.TrimSpace(key) != "mig_uuid" {
			continue
		}
		return strings.TrimSpace(value), nil
	}
	return "", s.Err()
}

// migProfileName renders a profile's fixed-width name field. The generated struct types it as int8
// rather than byte, and the vendor pads it with NULs.
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
