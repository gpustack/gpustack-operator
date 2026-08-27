package ascend

import (
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
		// A libdcmi that never loaded offers no container-share API either, so the preflight must
		// read this the same way it reads an absent entry point rather than trying to write past it.
		return shareModeError("dcmi init failed", d.initRet, d.initRet.IsAPIUnavailable())
	}
	return nil
}

// shareFlagUndeclared reports whether this failure means the API generation has no container-share
// flag at all. It takes the generation rather than reading it so that the whole verdict is a pure
// function of its two inputs and can be table-tested; its caller is what knows where to get them.
//
// The verdict is deliberately **conjunctive**, and both halves are load-bearing:
//
//   - The generation must be V2, because that is the generation whose header declares no
//     container-share entry point. No return code says this on its own: a V2 driver still exports the
//     V1 entry points and answers NOT_SUPPORT, which is a code the V1 generation also uses to mean
//     "this device does not do that", so a code alone cannot tell the two apart.
//   - The code must also be one that means this call is not served — NOT_SUPPORT, which is what a V2
//     driver answers for a V1 entry point, or FUNCTION_NOT_FOUND, which is an entry point that is not
//     there at all. Without this half, every failure on a V2 host reads as "no such flag": a device
//     the DM lacks the privilege to query (OPER_NOT_PERMITTED) or one that timed out would be waved
//     through as though the API had been consulted, and the reason logged beside the verdict would
//     contradict it. Those are ordinary failures, and the existing classification handles them —
//     retry through the write, then refuse with a remedy if that fails too.
//
// LIBRARY_NOT_FOUND is excluded on purpose, even on V2: a library that never loaded has told us
// nothing about any generation, and that path must refuse.
//
// Callers read the generation on each call rather than keeping it from construction, matching the
// binding: this driver retries its init per call, so the generation can become known after an init
// that failed.
func shareFlagUndeclared(version dcmi.APIVersion, ret dcmi.Return) bool {
	if version != dcmi.APIVersionV2 {
		return false
	}
	return ret == dcmi.ERROR_NOT_SUPPORT || ret == dcmi.ERROR_FUNCTION_NOT_FOUND
}

func (d *dcmiShareDriver) GetShareEnabled(cardID, deviceID int32) (bool, error) {
	dev, err := d.device(cardID, deviceID)
	if err != nil {
		return false, err
	}
	enabled, ret := dev.GetShareEnabled()
	if !ret.IsSuccess() {
		if shareFlagUndeclared(d.lib.APIVersion(), ret) {
			return false, shareNotDeclaredError("dcmi get device share enable", ret)
		}
		return false, shareModeError("dcmi get device share enable", ret, ret.IsAPIUnavailable())
	}
	return enabled, nil
}

func (d *dcmiShareDriver) SetShareEnabled(cardID, deviceID int32, enabled bool) error {
	dev, err := d.device(cardID, deviceID)
	if err != nil {
		return err
	}
	if ret := dev.SetShareEnabled(enabled); !ret.IsSuccess() {
		if shareFlagUndeclared(d.lib.APIVersion(), ret) {
			return shareNotDeclaredError("dcmi set device share enable", ret)
		}
		return shareModeError("dcmi set device share enable", ret, ret.IsAPIUnavailable())
	}
	return nil
}
