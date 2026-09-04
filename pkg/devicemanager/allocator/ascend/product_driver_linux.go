package ascend

import (
	"fmt"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/devicemanager/ascendproduct"
)

// newProductDriver returns the real dcmi-backed product reader. It is linux-only for the reason
// newShareDriver is: the device-manager runs only on linux, and linking the cgo binding/dcmi into a
// darwin test binary aborts at dyld load on the unresolved DCMI symbols, so the darwin build uses
// the stub in product_driver_other.go instead.
//
// It holds its own dcmi handle rather than borrowing the container-share driver's. The wrapper never
// unloads a library it already holds, so a second initializer cannot blank the first one's function
// pointers; keeping the two seams apart is what lets each be faked on its own.
func newProductDriver(logger klog.Logger) ascendproduct.Driver {
	return &dcmiProductDriver{lib: dcmi.New(binding.WithLogger(logger)), logger: logger}
}

// dcmiProductDriver is the real ascendproduct.Driver, addressing a device by the (card,
// device-in-card) pair dcmi names it by. Its behavior against a real driver is proven by the e2e
// run: a darwin test binary cannot link binding/dcmi at all.
type dcmiProductDriver struct {
	lib    *dcmi.DCMI
	logger klog.Logger
}

// device initializes the library and resolves a handle.
//
// Unlike the container-share driver this keeps no init result: the resolver above remembers only
// answers, so a failed read is already retried on the next allocation, and a successful one is
// remembered and never asks again. Re-running the vendor init is cheap for a library the wrapper
// already holds -- it re-runs the init alone, never the load -- so at most a couple of these ever
// happen on a node.
func (d *dcmiProductDriver) device(cardID, deviceID int32) (dcmi.Device, error) {
	if ret := d.lib.Init(d.logger); !ret.IsSuccess() {
		return dcmi.Device{}, fmt.Errorf("dcmi init failed: %w", ret)
	}

	return d.lib.GetDeviceHandleByCardAndIndex(cardID, deviceID), nil
}

func (d *dcmiProductDriver) MainboardID(cardID, deviceID int32) (uint32, error) {
	dev, err := d.device(cardID, deviceID)
	if err != nil {
		return 0, err
	}
	mainboardID, ret := dev.GetMainboardId()
	if !ret.IsSuccess() {
		return 0, fmt.Errorf("dcmi get mainboard id: %w", ret)
	}

	return mainboardID, nil
}

func (d *dcmiProductDriver) SuperPodType(cardID, deviceID int32) (uint32, error) {
	dev, err := d.device(cardID, deviceID)
	if err != nil {
		return 0, err
	}
	info, ret := dev.GetSuperPodInfo()
	if !ret.IsSuccess() {
		return 0, fmt.Errorf("dcmi get super pod info: %w", ret)
	}

	return uint32(info.Super_pod_type), nil
}
