package cambricon

import (
	"fmt"

	"gpustack.ai/gpustack/binding/cndev"
)

// newSMLUDriver returns the real cnDev-backed sMLU driver. It is linux-only: the
// device-manager runs only on linux, and linking the cnDev cgo into a darwin test binary
// aborts at dyld load (the .so-less host cannot resolve the flat-namespace symbols), so
// the darwin build uses the stub in smlu_driver_other.go instead.
func newSMLUDriver() smluDriver {
	l := cndev.New()
	return &cndevSMLUDriver{lib: l, initRet: l.Init()}
}

// cndevSMLUDriver is the real smluDriver, driving cnDev on a card addressed by PCI bus ID
// over the exported sMLU wrappers.
//
// The exact sMLU field semantics and device-node set are a documented hardware open
// question (see the spec): the calls below follow the vendor documentation but are
// unvalidated on real hardware, which is why every op goes through the seam so only this
// type changes when the contract is confirmed. All unit tests use a fake driver.
type cndevSMLUDriver struct {
	lib *cndev.CNDev
	// initRet captures cndevInit's result so the first device() call reports a single,
	// actionable root cause when the library failed to load/initialize.
	initRet cndev.Return
}

func (d *cndevSMLUDriver) device(card string) (cndev.Device, error) {
	if !d.initRet.IsSuccess() {
		return cndev.Device{}, fmt.Errorf("cndev init failed: %w", d.initRet)
	}
	dev, ret := d.lib.GetDeviceHandleByPciBusId(card)
	if !ret.IsSuccess() {
		return cndev.Device{}, fmt.Errorf("get cndev handle for %s: %w", card, ret)
	}
	return dev, nil
}

func (d *cndevSMLUDriver) EnsureSMLUMode(card string) error {
	dev, err := d.device(card)
	if err != nil {
		return err
	}
	if mode, ret := dev.GetSMLUMode(); ret.IsSuccess() && mode.SmluMode == uint32(cndev.FEATURE_ENABLED) {
		return nil
	}
	if ret := dev.SetSMLUMode(true); !ret.IsSuccess() {
		return fmt.Errorf("card %s: set smlu mode: %w", card, ret)
	}
	return nil
}

func (d *cndevSMLUDriver) CreateProfile(card string, coresPct int, memMiB int64) (int32, error) {
	dev, err := d.device(card)
	if err != nil {
		return 0, err
	}
	mluQuota, memorySize := smluSetFor(coresPct, memMiB)
	id, ret := dev.CreateSMluProfile(cndev.SMluSet{MluQuota: mluQuota, MemorySize: memorySize})
	if !ret.IsSuccess() {
		return 0, fmt.Errorf("card %s: create smlu profile: %w", card, ret)
	}
	return id, nil
}

func (d *cndevSMLUDriver) DestroyProfile(card string, profileID int32) error {
	dev, err := d.device(card)
	if err != nil {
		return err
	}
	if ret := dev.DestroySMluProfile(profileID); !ret.IsSuccess() {
		return fmt.Errorf("card %s: destroy smlu profile %d: %w", card, profileID, ret)
	}
	return nil
}

func (d *cndevSMLUDriver) CreateInstance(card string, profileID int32, name string) (smluInstance, error) {
	dev, err := d.device(card)
	if err != nil {
		return smluInstance{}, err
	}
	if ret := dev.CreateSMluInstance(uint32(profileID), name); !ret.IsSuccess() {
		return smluInstance{}, fmt.Errorf("card %s: create smlu instance %q: %w", card, name, ret)
	}
	// Read the created instance back to recover its device node.
	infos, ret := dev.GetAllSMluInstanceInfo()
	if !ret.IsSuccess() {
		return smluInstance{}, fmt.Errorf("card %s: list smlu instances after create: %w", card, ret)
	}
	for i := range infos {
		if infos[i].GetInstanceName() == name {
			return instanceFromInfo(card, infos[i]), nil
		}
	}
	return smluInstance{}, fmt.Errorf("card %s: created smlu instance %q not found", card, name)
}

func (d *cndevSMLUDriver) DestroyInstance(card, name string) error {
	dev, err := d.device(card)
	if err != nil {
		return err
	}
	if ret := dev.DestroySMluInstanceByName(name); !ret.IsSuccess() && ret != cndev.ERROR_NOT_FOUND {
		return fmt.Errorf("card %s: destroy smlu instance %q: %w", card, name, ret)
	}
	return nil
}

func (d *cndevSMLUDriver) ListInstances() ([]smluInstance, error) {
	if !d.initRet.IsSuccess() {
		return nil, fmt.Errorf("cndev init failed: %w", d.initRet)
	}
	count, ret := d.lib.GetDeviceCount()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("get cndev device count: %w", ret)
	}
	var out []smluInstance
	for i := 0; i < count; i++ {
		// Fail closed on any per-device error: a partial list would let the reclaim loop's
		// profile GC treat a still-referenced profile as orphaned and destroy it.
		dev, ret := d.lib.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get cndev handle for device %d: %w", i, ret)
		}
		pcie, ret := dev.GetPCIeInfoV().V2()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get pcie info for device %d: %w", i, ret)
		}
		card := pcie.GetBusId()
		infos, ret := dev.GetAllSMluInstanceInfo()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("list smlu instances on %s: %w", card, ret)
		}
		for j := range infos {
			out = append(out, instanceFromInfo(card, infos[j]))
		}
	}
	return out, nil
}

func (d *cndevSMLUDriver) ListProfiles() ([]profileKey, error) {
	if !d.initRet.IsSuccess() {
		return nil, fmt.Errorf("cndev init failed: %w", d.initRet)
	}
	count, ret := d.lib.GetDeviceCount()
	if !ret.IsSuccess() {
		return nil, fmt.Errorf("get cndev device count: %w", ret)
	}
	var out []profileKey
	for i := 0; i < count; i++ {
		dev, ret := d.lib.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get cndev handle for device %d: %w", i, ret)
		}
		pcie, ret := dev.GetPCIeInfoV().V2()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("get pcie info for device %d: %w", i, ret)
		}
		card := pcie.GetBusId()
		ids, ret := dev.GetSMluProfileIds()
		if !ret.IsSuccess() {
			return nil, fmt.Errorf("list smlu profiles on %s: %w", card, ret)
		}
		for _, id := range ids {
			out = append(out, profileKey{card: card, profileID: id})
		}
	}
	return out, nil
}

// instanceFromInfo builds an smluInstance from a cnDev SMluInfo record on card. The quota
// is read from the MAX (index 0) slot; memory is bytes, converted back to MiB.
func instanceFromInfo(card string, info cndev.SMluInfo) smluInstance {
	return smluInstance{
		card:      card,
		name:      info.GetInstanceName(),
		profileID: info.ProfileId,
		coresPct:  int(info.MluQuota[0]),
		memMiB:    int64(info.MemorySize[0] >> 20),
		devNode:   info.GetDevNodeName(),
	}
}
