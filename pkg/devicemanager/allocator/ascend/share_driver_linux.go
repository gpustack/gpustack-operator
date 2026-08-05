package ascend

import (
	"fmt"
	"sync"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/dcmi"
)

// newShareDriver returns the real dcmi-backed container-share driver. It is linux-only: the
// device-manager runs only on linux, and linking the cgo binding/dcmi into a darwin test binary
// (which links Go's plugin package) aborts at dyld load on the unresolved DCMI symbols, so the
// darwin build uses the stub in share_driver_other.go instead.
//
// Initializing here needs no coordination with the detector, which initializes the same
// process-wide library on its own schedule: the wrapper never unloads a library it already
// holds, so neither caller can blank the other's function pointers and neither has to run
// first.
func newShareDriver(logger klog.Logger) shareDriver {
	l := dcmi.New(binding.WithLogger(logger))
	return &dcmiShareDriver{lib: l, logger: logger, initRet: l.Init(logger)}
}

// dcmiShareDriver is the real shareDriver, addressing a device by the (card, device-in-card)
// pair dcmi names it by. Its behavior against a real driver is proven by the e2e run: a darwin
// test binary cannot link binding/dcmi at all.
type dcmiShareDriver struct {
	lib    *dcmi.DCMI
	logger klog.Logger

	// mu guards initRet, which is retried rather than fixed at construction. One driver instance
	// is shared by the sliced, shared and visibility servers, each with its own gRPC listener, so
	// two Allocate calls can arrive at once.
	mu sync.Mutex
	// initRet is the library's last init result, reported as the root cause when a call cannot
	// proceed rather than letting a bare missing-symbol error stand in for it.
	initRet dcmi.Return
}

func (d *dcmiShareDriver) device(cardID, deviceID int32) (dcmi.Device, error) {
	if err := d.ready(); err != nil {
		return dcmi.Device{}, err
	}
	return d.lib.GetDeviceHandleByCardAndIndex(cardID, deviceID), nil
}

// ready reports whether the library is initialized, retrying once per call while it is not. A
// driver that was briefly unready at construction would otherwise sink every allocation on the
// node until the device-manager restarted. The retry is cheap and safe because the wrapper never
// unloads a library it already holds -- it re-runs only the vendor init, which is the part that
// can have failed.
func (d *dcmiShareDriver) ready() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.initRet.IsSuccess() {
		return nil
	}
	if d.initRet = d.lib.Init(d.logger); !d.initRet.IsSuccess() {
		return fmt.Errorf("dcmi init failed: %w", d.initRet)
	}
	return nil
}

func (d *dcmiShareDriver) GetShareEnabled(cardID, deviceID int32) (bool, error) {
	dev, err := d.device(cardID, deviceID)
	if err != nil {
		return false, err
	}
	enabled, ret := dev.GetShareEnabled()
	if !ret.IsSuccess() {
		return false, fmt.Errorf("dcmi get device share enable: %w", ret)
	}
	return enabled, nil
}

func (d *dcmiShareDriver) SetShareEnabled(cardID, deviceID int32, enabled bool) error {
	dev, err := d.device(cardID, deviceID)
	if err != nil {
		return err
	}
	if ret := dev.SetShareEnabled(enabled); !ret.IsSuccess() {
		return fmt.Errorf("dcmi set device share enable: %w", ret)
	}
	return nil
}
