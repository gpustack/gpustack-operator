package ascend

import (
	"context"
	"errors"
	"fmt"
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

// An accelerator already in container-share mode is read and left alone: the flag lives in the driver,
// so re-writing it on every allocation would be a pointless host mutation.
func TestEnsureShareEnabled_AlreadyOn(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: true}}
	s := newSlicedServerWithShare(share)

	require.NoError(t, allocateSlice(t, s, "uid-share-on"))
	assert.Equal(t, [][2]int32{{0, 0}}, share.getCalls, "reads the flag of the allocated card")
	assert.Empty(t, share.setCalls, "must not write a flag that is already on")
}

// An accelerator with the mode off is turned on once, and a second allocation onto the same one sees
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

// A flag that cannot be turned on fails the allocation, naming the dcmi (card, device) pair and
// the manual remedy, rather than admitting a pod whose workload would die on the device open.
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

// The marking is the whole contract between the build-tagged driver and the core: the driver reads
// the vendor code, and everything downstream reads only this sentinel. Without this test, dropping
// the sentinel from shareModeError would leave every other test in this file green while production
// silently lost the ability to refuse.
func TestShareModeError(t *testing.T) {
	cause := errors.New("FUNCTION_NOT_FOUND")

	marked := shareModeError("dcmi get device share enable", cause, true)
	assert.ErrorIs(t, marked, errShareUnsupported, "an unavailable API must carry the sentinel")
	assert.ErrorIs(t, marked, cause, "and must keep the vendor's own reason")

	plain := shareModeError("dcmi get device share enable", cause, false)
	assert.NotErrorIs(t, plain, errShareUnsupported, "anything else must not carry it")
	assert.ErrorIs(t, plain, cause)
}

// A driver whose libdcmi has no container-share entry point is refused without the device being
// written to, and without an `npu-smi` command being offered: no command adds an API the driver does
// not carry.
//
// The flag's own state is not varied here: a failed read returns no state at all, so the refusal is
// decided by the read alone and a second run with the flag on would exercise the same code.
func TestEnsureShareEnabled_APIAbsent(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{
		enabled: map[[2]int32]bool{},
		getErr:  fmt.Errorf("dcmi get device share enable: FUNCTION_NOT_FOUND: %w", errShareUnsupported),
	}
	s := newSlicedServerWithShare(share)

	err := allocateSlice(t, s, "uid-share-absent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "card 0 device 0 cannot be shared")
	assert.Contains(t, err.Error(), "FUNCTION_NOT_FOUND")
	assert.NotContains(t, err.Error(), "npu-smi", "no command repairs an absent API, so none is offered")
	assert.Empty(t, share.setCalls, "an absent API is never written to")
}

// dcmi resolves each symbol independently, so the absence can surface on the write after a read that
// worked. Offering `npu-smi` there would send the operator after a fix that adds no missing symbol.
func TestEnsureShareEnabled_APIAbsentOnTheWrite(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServerWithShare(&fakeShareDriver{
		enabled: map[[2]int32]bool{{0, 0}: false},
		setErr:  fmt.Errorf("dcmi set device share enable: FUNCTION_NOT_FOUND: %w", errShareUnsupported),
	})

	err := allocateSlice(t, s, "uid-share-absent-setter")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "card 0 device 0 cannot be shared")
	assert.NotContains(t, err.Error(), "npu-smi", "no command repairs an absent API, so none is offered")
}

// A read that merely failed says nothing about whether the API exists, so the write still runs and
// the allocation succeeds. Refusing here instead would fail an allocation the write completes.
func TestEnsureShareEnabled_TransientGetFailureStillWrites(t *testing.T) {
	redirectLogicalSliceDirs(t)
	share := &fakeShareDriver{
		enabled: map[[2]int32]bool{},
		getErr:  errors.New("dcmi get device share enable: TIME_OUT"),
	}
	s := newSlicedServerWithShare(share)

	require.NoError(t, allocateSlice(t, s, "uid-share-transient"))
	assert.Equal(t, [][2]int32{{0, 0}}, share.setCalls, "the write is attempted despite the failed read")
	assert.True(t, share.enabled[[2]int32{0, 0}], "and it leaves the flag on")
}

// When the write fails too, both failures reach the operator: the write's own reason, the read that
// could not rule it out beforehand, and the manual remedy.
func TestEnsureShareEnabled_TransientGetThenSetFails(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServerWithShare(&fakeShareDriver{
		enabled: map[[2]int32]bool{},
		getErr:  errors.New("dcmi get device share enable: TIME_OUT"),
		setErr:  errors.New("dcmi set device share enable: OPER_NOT_PERMITTED"),
	})

	err := allocateSlice(t, s, "uid-share-bothfail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPER_NOT_PERMITTED")
	assert.Contains(t, err.Error(), "the flag read failed too: dcmi get device share enable: TIME_OUT")
	assert.Contains(t, err.Error(), "npu-smi set -t device-share -i 0 -c 0 -d 1")
}

// An accelerator carrying no dcmi addressing cannot be targeted, so the allocation fails rather
// than guessing a dcmi card.
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

// Shared and visibility put a second container on an accelerator, so both must turn the flag on the
// same way a slice does -- and across every accelerator they were granted, not just the first.
func TestSharedAndVisibilityEnableShare(t *testing.T) {
	for _, mode := range []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			share := &fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: false, {1, 0}: true}}
			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Manufacturer:   Manufacturer,
					AllocationMode: mode,
				},
				share: share,
			}

			pod, ctr := slicedPod("uid-"+mode.String(), "train", 10, 25)
			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr,
				ascendDevicesFixture(), map[deviceplugin.Resource]int32{
					{Group: "910b2", Device: testAccelID0}: 1,
					{Group: "910b2", Device: testAccelID1}: 1,
				})
			require.NoError(t, err)

			// The env names the accelerators by driver index, while the flag below is addressed by
			// the card/device pair, so the two carry different numbers.
			assert.Equal(t, "3,7", resp.Envs["ASCEND_VISIBLE_DEVICES"])
			assert.Empty(t, resp.Mounts, "no vcann-rt artifacts outside the sliced path")
			assert.ElementsMatch(t, [][2]int32{{0, 0}, {1, 0}}, share.getCalls,
				"reads the flag of every granted card")
			assert.Equal(t, [][2]int32{{0, 0}}, share.setCalls,
				"writes only the card whose flag was off")
		})
	}
}

// A shared allocation whose flag cannot be set is refused, rather than admitting a pod whose
// workload would then fail on the device open.
func TestSharedShareEnableFailureIsFatal(t *testing.T) {
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeShared,
		},
		share: &fakeShareDriver{
			enabled: map[[2]int32]bool{{0, 0}: false},
			setErr:  errors.New("dcmi set device share enable: OPER_NOT_PERMITTED"),
		},
	}

	pod, ctr := slicedPod("uid-shared-setfail", "train", 10, 25)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, ascendDevicesFixture(),
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "npu-smi set -t device-share -i 0 -c 0 -d 1")
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

	assert.Equal(t, "3", resp.Envs["ASCEND_VISIBLE_DEVICES"])
	assert.Empty(t, share.getCalls)
	assert.Empty(t, share.setCalls)
}
