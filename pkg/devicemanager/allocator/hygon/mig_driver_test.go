//go:build linux

package hygon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/dmi"
)

func TestIsUnqueryableReturn(t *testing.T) {
	cases := []struct {
		name string
		ret  dmi.Return
		want bool
	}{
		{name: "gpu is lost", ret: dmi.ERROR_GPU_IS_LOST, want: true},
		{name: "unknown", ret: dmi.ERROR_UNKNOWN, want: true},
		{name: "reset required", ret: dmi.ERROR_RESET_REQUIRED, want: true},
		{name: "insufficient power", ret: dmi.ERROR_INSUFFICIENT_POWER, want: true},
		{name: "irq issue", ret: dmi.ERROR_IRQ_ISSUE, want: true},
		{name: "no permission", ret: dmi.ERROR_NO_PERMISSION, want: false},
		{name: "invalid argument", ret: dmi.ERROR_INVALID_ARGUMENT, want: false},
		{name: "uninitialized", ret: dmi.ERROR_UNINITIALIZED, want: false},
		{name: "not supported", ret: dmi.ERROR_NOT_SUPPORTED, want: false},
		{name: "not found", ret: dmi.ERROR_NOT_FOUND, want: false},
		{name: "driver not loaded", ret: dmi.ERROR_DRIVER_NOT_LOADED, want: false},
		{name: "library not found", ret: dmi.ERROR_LIBRARY_NOT_FOUND, want: false},
		{name: "function not found", ret: dmi.ERROR_FUNCTION_NOT_FOUND, want: false},
		{name: "version mismatch", ret: dmi.ERROR_LIB_RM_VERSION_MISMATCH, want: false},
		{name: "not ready", ret: dmi.ERROR_NOT_READY, want: false},
		{name: "in use", ret: dmi.ERROR_IN_USE, want: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isUnqueryableReturn(c.ret))
			wrapped := fmt.Errorf("card 0: query failed: %w", c.ret)
			assert.Equal(t, c.want, isUnqueryableError(wrapped))
		})
	}

	assert.False(t, isUnqueryableError(errors.New("plain error")))
}

func TestDmiMigDriverReadyRetriesAfterFailure(t *testing.T) {
	var attempts atomic.Int32
	drv := &dmiMigDriver{
		initFn: func() dmi.Return {
			if attempts.Add(1) < 3 {
				return dmi.ERROR_DRIVER_NOT_LOADED
			}
			return dmi.SUCCESS
		},
	}

	require.ErrorIs(t, drv.ready(), dmi.ERROR_DRIVER_NOT_LOADED)
	require.ErrorIs(t, drv.ready(), dmi.ERROR_DRIVER_NOT_LOADED)
	require.NoError(t, drv.ready())
	assert.Equal(t, int32(3), attempts.Load())

	// Subsequent calls must reuse the initialized state without calling initFn again.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, drv.ready())
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(3), attempts.Load())
	require.ErrorIs(t, (&dmiMigDriver{}).ready(), dmi.ERROR_UNINITIALIZED)
}

func TestListInstancesAcrossCards(t *testing.T) {
	inst0 := migLiveInstance{PciBusID: "0000:01:00.0", Instance: migInstance{GiID: 1}}
	inst2 := migLiveInstance{PciBusID: "0000:03:00.0", Instance: migInstance{GiID: 2}}

	cases := []struct {
		name    string
		count   int
		results []struct {
			insts []migLiveInstance
			err   error
		}
		want        []migLiveInstance
		wantSkipped []int
		wantErr     error
	}{
		{
			name:  "zero cards returns empty without error",
			count: 0,
		},
		{
			name:  "skips single unqueryable card and returns healthy cards",
			count: 3,
			results: []struct {
				insts []migLiveInstance
				err   error
			}{
				{insts: []migLiveInstance{inst0}},
				{err: fmt.Errorf("card 1: %w", dmi.ERROR_GPU_IS_LOST)},
				{insts: []migLiveInstance{inst2}},
			},
			want:        []migLiveInstance{inst0, inst2},
			wantSkipped: []int{1},
		},
		{
			name:  "healthy card with zero instances counts as queried when sibling is lost",
			count: 2,
			results: []struct {
				insts []migLiveInstance
				err   error
			}{
				{insts: nil},
				{err: fmt.Errorf("card 1: %w", dmi.ERROR_GPU_IS_LOST)},
			},
			wantSkipped: []int{1},
		},
		{
			name:  "all cards unqueryable returns last error",
			count: 2,
			results: []struct {
				insts []migLiveInstance
				err   error
			}{
				{err: fmt.Errorf("card 0: %w", dmi.ERROR_GPU_IS_LOST)},
				{err: fmt.Errorf("card 1: %w", dmi.ERROR_RESET_REQUIRED)},
			},
			wantSkipped: []int{0, 1},
			wantErr:     dmi.ERROR_RESET_REQUIRED,
		},
		{
			name:  "non-card-specific error fails immediately",
			count: 3,
			results: []struct {
				insts []migLiveInstance
				err   error
			}{
				{insts: []migLiveInstance{inst0}},
				{err: fmt.Errorf("card 1: %w", dmi.ERROR_NO_PERMISSION)},
				{insts: []migLiveInstance{inst2}},
			},
			wantErr: dmi.ERROR_NO_PERMISSION,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var skipped []int
			got, err := listInstancesAcrossCards(
				c.count,
				func(i int) ([]migLiveInstance, error) {
					return c.results[i].insts, c.results[i].err
				},
				isUnqueryableError,
				func(i int, _ error) {
					skipped = append(skipped, i)
				},
			)
			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, c.want, got)
			}
			assert.Equal(t, c.wantSkipped, skipped)
		})
	}
}

func TestFindGpuInstanceByID(t *testing.T) {
	profiles := []migProfile{{id: 3, slices: 1}}

	cases := []struct {
		name     string
		handles  []int
		infoByID map[int]struct {
			info dmi.GpuInstanceInfo
			ret  dmi.Return
		}
		targetGiID uint32
		wantFound  bool
		wantErr    dmi.Return
	}{
		{
			name:    "returns error when GetInfo fails instead of reporting absent",
			handles: []int{1},
			infoByID: map[int]struct {
				info dmi.GpuInstanceInfo
				ret  dmi.Return
			}{
				1: {ret: dmi.ERROR_GPU_IS_LOST},
			},
			targetGiID: 7,
			wantErr:    dmi.ERROR_GPU_IS_LOST,
		},
		{
			name:    "finds matching GPU instance",
			handles: []int{1, 2},
			infoByID: map[int]struct {
				info dmi.GpuInstanceInfo
				ret  dmi.Return
			}{
				1: {info: dmi.GpuInstanceInfo{Id: 3}, ret: dmi.SUCCESS},
				2: {info: dmi.GpuInstanceInfo{Id: 7}, ret: dmi.SUCCESS},
			},
			targetGiID: 7,
			wantFound:  true,
		},
		{
			name:    "reports absent cleanly when all handles succeed and none match",
			handles: []int{1},
			infoByID: map[int]struct {
				info dmi.GpuInstanceInfo
				ret  dmi.Return
			}{
				1: {info: dmi.GpuInstanceInfo{Id: 3}, ret: dmi.SUCCESS},
			},
			targetGiID: 7,
			wantFound:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, found, err := findGpuInstanceByID(
				"0000:01:00.0",
				profiles,
				c.targetGiID,
				func(uint32) ([]int, dmi.Return) { return c.handles, dmi.SUCCESS },
				func(h int) (dmi.GpuInstanceInfo, dmi.Return) {
					entry := c.infoByID[h]
					return entry.info, entry.ret
				},
			)
			if c.wantErr != dmi.SUCCESS {
				require.ErrorIs(t, err, c.wantErr)
				assert.False(t, found)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.wantFound, found)
		})
	}
}

func TestListCardInstancesUsesDeviceIndexAndIsolatesMissingConfig(t *testing.T) {
	root := t.TempDir()
	restore := migConfigDir
	migConfigDir = root
	t.Cleanup(func() { migConfigDir = restore })

	require.NoError(t, os.MkdirAll(filepath.Join(root, "ci"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dev0"), []byte("0000:01:00.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dev2"), []byte("0000:03:00.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dev3"), []byte("   \n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ci", "dev0gi1ci0.conf"), []byte("mig_uuid: MIG-0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ci", "dev2gi2ci0.conf"), []byte("other: val\n"), 0o600))

	// When DMI compacts handles after card 1 drops, slot 1 reports dev.GetIndex() == 2 and must
	// resolve dev2 ("0000:03:00.0") rather than dev1.
	got, err := listCardInstances(
		1,
		func() (uint32, dmi.Return) { return 2, dmi.SUCCESS },
		migCardPciBusID,
		func(pciBusID string) ([]migInstance, error) {
			assert.Equal(t, "0000:03:00.0", pciBusID)
			return []migInstance{{GiID: 9}}, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, []migLiveInstance{{PciBusID: "0000:03:00.0", Instance: migInstance{GiID: 9}}}, got)

	// Missing dev<idx>, empty dev<idx>, missing CI .conf, and empty-identity CI .conf are all
	// isolated per card by listInstancesAcrossCards so healthy siblings still list.
	var skipped []int
	listed, err := listInstancesAcrossCards(
		5,
		func(slot int) ([]migLiveInstance, error) {
			switch slot {
			case 0:
				return listCardInstances(0, func() (uint32, dmi.Return) { return 0, dmi.SUCCESS }, migCardPciBusID,
					func(string) ([]migInstance, error) {
						id, path, err := readMigInstanceIdentity(0, 1, 0)
						if err != nil {
							return nil, err
						}
						return []migInstance{{GiID: 1, UUID: id, ConfPath: path}}, nil
					})
			case 1:
				// Missing dev1 file.
				return listCardInstances(1, func() (uint32, dmi.Return) { return 1, dmi.SUCCESS }, migCardPciBusID, nil)
			case 2:
				// CI .conf exists but carries no mig_uuid.
				return listCardInstances(2, func() (uint32, dmi.Return) { return 2, dmi.SUCCESS }, migCardPciBusID,
					func(string) ([]migInstance, error) {
						_, _, err := readMigInstanceIdentity(2, 2, 0)
						return nil, err
					})
			case 3:
				// Empty dev3 file.
				return listCardInstances(3, func() (uint32, dmi.Return) { return 3, dmi.SUCCESS }, migCardPciBusID, nil)
			case 4:
				// Missing CI .conf file.
				return listCardInstances(4, func() (uint32, dmi.Return) { return 0, dmi.SUCCESS }, migCardPciBusID,
					func(string) ([]migInstance, error) {
						_, _, err := readMigInstanceIdentity(0, 99, 0)
						return nil, err
					})
			default:
				return nil, nil
			}
		},
		isUnqueryableError,
		func(i int, _ error) { skipped = append(skipped, i) },
	)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3, 4}, skipped)
	require.Len(t, listed, 1)
	assert.Equal(t, "0000:01:00.0", listed[0].PciBusID)
	assert.Equal(t, "MIG-0", listed[0].Instance.UUID)

	// Permission errors on registry files are NOT card-unqueryable and must abort listing.
	permErr := wrapMigConfigReadError("read registry", os.ErrPermission)
	assert.False(t, isUnqueryableError(permErr))
	_, err = listInstancesAcrossCards(
		2,
		func(slot int) ([]migLiveInstance, error) {
			if slot == 1 {
				return nil, permErr
			}
			return listed, nil
		},
		isUnqueryableError,
		nil,
	)
	require.ErrorIs(t, err, os.ErrPermission)
}
