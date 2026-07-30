package ascend

import (
	"fmt"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

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
// one container, for the card a logical slice is about to land on. It reads first and writes
// only when the flag is off, so a card already carrying a slice costs one query.
//
// Without the flag the second sliced container on a card still starts, and then its workload
// fails inside the container on the device open with a driver-internal error that names
// neither the card nor the flag. Failing the allocation instead trades that for a diagnosis,
// which is why a card whose flag cannot be turned on is refused rather than admitted.
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

	enabled, err := s.share.GetShareEnabled(cardID, deviceID)
	if err != nil {
		return fmt.Errorf("read container-share mode of card %d device %d: %w", cardID, deviceID, err)
	}
	if enabled {
		return nil
	}

	if err = s.share.SetShareEnabled(cardID, deviceID, true); err != nil {
		return fmt.Errorf(
			"enable container-share mode of card %d device %d: %w; "+
				"enable it on the host with `npu-smi set -t device-share -i %d -c %d -d 1`",
			cardID, deviceID, err, cardID, deviceID)
	}
	return nil
}
