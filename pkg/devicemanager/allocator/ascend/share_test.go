package ascend

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// fakeShareDriver stands in for binding/dcmi, which a darwin test binary cannot link. It
// records every write so a test can prove the responder reads before it writes, and writes
// only when the flag is off.
type fakeShareDriver struct {
	enabled  map[[2]int32]bool
	getErr   error
	setErr   error
	getCalls [][2]int32
	setCalls [][2]int32
}

func (d *fakeShareDriver) GetShareEnabled(cardID, deviceID int32) (bool, error) {
	d.getCalls = append(d.getCalls, [2]int32{cardID, deviceID})
	if d.getErr != nil {
		return false, d.getErr
	}
	return d.enabled[[2]int32{cardID, deviceID}], nil
}

func (d *fakeShareDriver) SetShareEnabled(cardID, deviceID int32, enabled bool) error {
	d.setCalls = append(d.setCalls, [2]int32{cardID, deviceID})
	if d.setErr != nil {
		return d.setErr
	}
	d.enabled[[2]int32{cardID, deviceID}] = enabled
	return nil
}

// allocateSlice runs one sliced allocation of accelerator index 0 (dcmi card 0, device 0).
func allocateSlice(t *testing.T, s *server, uid string) error {
	t.Helper()
	pod, ctr := slicedPod(uid, "train", 10, 25)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, ascendDevicesFixture(),
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	return err
}

// A card already in container-share mode is read and left alone: the flag lives in the driver,
// so re-writing it on every allocation would be a pointless host mutation.
func TestEnsureShareEnabled_AlreadyOn(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: true}}
	s := newSlicedServerWithShare(share)

	require.NoError(t, allocateSlice(t, s, "uid-share-on"))
	assert.Equal(t, [][2]int32{{0, 0}}, share.getCalls, "reads the flag of the allocated card")
	assert.Empty(t, share.setCalls, "must not write a flag that is already on")
}

// A card with the mode off is turned on once, and a second allocation onto the same card sees
// it on and writes nothing more.
func TestEnsureShareEnabled_TurnsOnThenIdempotent(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: false}}
	s := newSlicedServerWithShare(share)

	require.NoError(t, allocateSlice(t, s, "uid-share-off"))
	assert.Equal(t, [][2]int32{{0, 0}}, share.setCalls, "turns the flag on for the allocated card")
	assert.True(t, share.enabled[[2]int32{0, 0}], "the flag ends up on")

	require.NoError(t, allocateSlice(t, s, "uid-share-off-again"))
	assert.Len(t, share.setCalls, 1, "the second allocation writes nothing")
	assert.Len(t, share.getCalls, 2, "but still reads")
}

// A flag that cannot be turned on fails the allocation, naming the card and the manual remedy,
// rather than admitting a pod whose workload would die on the device open.
func TestEnsureShareEnabled_SetFails(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServerWithShare(&fakeShareDriver{
		enabled: map[[2]int32]bool{{0, 0}: false},
		setErr:  errors.New("dcmi set device share enable: OPER_NOT_PERMITTED"),
	})

	err := allocateSlice(t, s, "uid-share-setfail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "card 0 device 0")
	assert.Contains(t, err.Error(), "npu-smi set -t device-share -i 0 -c 0 -d 1")
}

// A driver that cannot even be read fails the allocation: proceeding would gamble the pod on an
// unknown flag.
func TestEnsureShareEnabled_GetFails(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{
		enabled: map[[2]int32]bool{},
		getErr:  errors.New("dcmi get device share enable: FUNCTION_NOT_FOUND"),
	}
	s := newSlicedServerWithShare(share)

	err := allocateSlice(t, s, "uid-share-getfail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read container-share mode of card 0 device 0")
	assert.Empty(t, share.setCalls, "a failed read must not be followed by a blind write")
}

// An accelerator carrying no dcmi addressing cannot be targeted, so the allocation fails rather
// than guessing a card.
func TestEnsureShareEnabled_MissingPhysicalIndexes(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{enabled: map[[2]int32]bool{}}
	s := newSlicedServerWithShare(share)
	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{0}

	pod, ctr := slicedPod("uid-share-noidx", "train", 10, 25)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no dcmi card/device index")
	assert.Empty(t, share.getCalls)
}

// Exclusive allocation must never reach the container-share write. What keeps it out is the
// mode check in GetContainerAllocateResponse, not the absence of a driver — so hand the
// exclusive responder a driver that errors on every call and require the allocation to
// succeed without consulting it.
func TestExclusiveAllocationNeverTouchesShare(t *testing.T) {
	share := &fakeShareDriver{
		enabled: map[[2]int32]bool{},
		getErr:  errors.New("the exclusive path must not read the container-share flag"),
	}
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
		share: share,
	}

	pod, ctr := slicedPod("uid-exclusive", "train", 10, 25)
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, ascendDevicesFixture(),
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)

	assert.Equal(t, "0", resp.Envs["ASCEND_VISIBLE_DEVICES"])
	assert.Empty(t, share.getCalls)
	assert.Empty(t, share.setCalls)
}
