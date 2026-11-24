//go:build linux

package hygon

import (
	"errors"
	"fmt"
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
