package ascend

import (
	"errors"
	"fmt"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// errShareUnsupported marks a container-share call that failed because this driver has no such dcmi
// entry point, or because libdcmi never loaded at all, as opposed to failing for something about the
// device or the moment. It is the one failure no retry, no write and no `npu-smi` command can get
// past, so the preflight refuses on it without touching the device and without offering a
// remediation that could not work.
//
// It is a package sentinel rather than a dcmi return code because the core below is compiled on
// darwin, where binding/dcmi cannot be linked; the build-tagged driver classifies the code and marks
// the error, and the core reads the mark.
var errShareUnsupported = errors.New("this driver has no dcmi container-share API")

// shareModeError renders a failed driver call on the container-share path, marking it with
// errShareUnsupported when apiUnavailable says the call could not be made at all.
//
// The verdict arrives as a bool because only the build-tagged driver can read a dcmi return code,
// while this file must stay importable by a darwin test binary. Splitting it that way is what puts
// the marking itself under test: the seam keeps the one-line code-to-bool classification, and the
// decision that hangs off it is exercised here.
func shareModeError(what string, cause error, apiUnavailable bool) error {
	if apiUnavailable {
		return fmt.Errorf("%s: %w: %w", what, cause, errShareUnsupported)
	}
	return fmt.Errorf("%s: %w", what, cause)
}

// shareDriver is the dcmi actuator seam behind a _linux.go/_other.go build tag: the real impl
// drives binding/dcmi on linux; the darwin stub errors, so the dcmi-free injection core is
// table-tested with a fake driver. A device is addressed by the (card, device-in-card) pair
// dcmi names it by, which the detector already recorded on the accelerator.
type shareDriver interface {
	// GetShareEnabled reports whether container-share mode is on for the device.
	GetShareEnabled(cardID, deviceID int32) (bool, error)
	// SetShareEnabled turns container-share mode on or off for the device. It writes host
	// driver state that outlives the process, so callers write only after a read says the
	// flag disagrees.
	SetShareEnabled(cardID, deviceID int32, enabled bool) error
}

// ensureShareEnabled turns on the driver flag that lets one device be mounted into more than
// one container, for the accelerator a logical slice is about to land on. It reads first and
// writes only when the flag is off, so an accelerator already carrying a slice costs one query.
//
// Without the flag the second sliced container on an accelerator still starts, and then its
// workload fails inside the container on the device open with a driver-internal error that names
// neither the accelerator nor the flag. Failing the allocation instead trades that for a
// diagnosis, which is why an accelerator whose flag cannot be turned on is refused rather than
// admitted.
//
// The read is classified rather than treated as fatal on its own. A read that reports the dcmi
// entry point missing refuses the allocation without touching the device — including one whose flag
// is already on, since a driver this code cannot query is one it cannot manage. Any other read
// failure says nothing about whether the API exists, so the write is still attempted: the write is
// what makes the flag known, so a read this code could not trust is not a reason to refuse an
// allocation the write would have completed — only a reason to say so if the write then fails too.
func (s *server) ensureShareEnabled(accel *workercore.Accelerator) error {
	if s.share == nil {
		return fmt.Errorf("container-share actuator not configured")
	}
	// The Ascend detector records dcmi's own addressing in PhysicalIndexes as
	// {physical id, card id, device id in card}.
	if len(accel.PhysicalIndexes) < 3 {
		return fmt.Errorf("accelerator %q carries no dcmi card/device index", accel.ID)
	}
	cardID, deviceID := int32(accel.PhysicalIndexes[1]), int32(accel.PhysicalIndexes[2])

	enabled, readErr := s.share.GetShareEnabled(cardID, deviceID)
	switch {
	case errors.Is(readErr, errShareUnsupported):
		return fmt.Errorf("card %d device %d cannot be shared: %w", cardID, deviceID, readErr)
	case readErr == nil && enabled:
		return nil
	}

	err := s.share.SetShareEnabled(cardID, deviceID, true)
	if err == nil {
		if readErr != nil {
			// The flag is on now, so the allocation stands — but a read this code could not trust
			// will fail again on the next Allocate, re-writing host state every time, so it must not
			// pass in silence.
			s.Logger.Info("enabled container-share mode without a trustworthy read",
				"card", cardID, "device", deviceID, "readError", readErr.Error())
		}
		return nil
	}
	if readErr != nil {
		err = fmt.Errorf("%w (the flag read failed too: %w)", err, readErr)
	}
	// The write can report the absence the read did not: dcmi resolves each symbol independently.
	// Offering a command in that case would send the operator after a fix that adds no missing
	// symbol, so the two outcomes are told apart here as well.
	if errors.Is(err, errShareUnsupported) {
		return fmt.Errorf("card %d device %d cannot be shared: %w", cardID, deviceID, err)
	}
	return fmt.Errorf(
		"enable container-share mode of card %d device %d: %w; "+
			"enable it on the host with `npu-smi set -t device-share -i %d -c %d -d 1`",
		cardID, deviceID, err, cardID, deviceID)
}
