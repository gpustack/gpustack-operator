package amd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/amdsmi"
)

// TestIsUnavailableReading pins which driver readings are the vendor's "unavailable" answer and
// which are measurements, because the two are told apart by value alone: the call that produced
// either one succeeded.
//
// The cases are written as the two field widths that reach it, not as bare numbers, because the
// width is the whole reason one constant serves both. GpuMetrics reports utilization and
// temperature in uint16, where the sentinel is that type's own maximum; PowerInfo reports wattage in
// uint32, and the driver writes the same 65535 into the wider field rather than that type's
// maximum.
func TestIsUnavailableReading(t *testing.T) {
	cases := []struct {
		name  string
		value uint32
		want  bool
	}{
		{
			name:  "a uint16 metrics field the driver did not fill",
			value: uint32(^uint16(0)),
			want:  true,
		},
		{
			// What an SR-IOV virtual function answers for power: it exposes no host-side telemetry
			// to its guest, and reports this instead of failing the call.
			name:  "a uint32 power field the driver wrote the sentinel into",
			value: 0xFFFF,
			want:  true,
		},
		{
			// Deliberately not caught. AMD's own binding compares the whole power block against
			// 65535, and widening this to the uint32 maximum would stop catching the reading that
			// is actually reported. A field whose maximum this is has no known sentinel here.
			name:  "a uint32 field at its own maximum, which is not the convention",
			value: ^uint32(0),
			want:  false,
		},
		{
			name:  "an ordinary reading",
			value: 42,
			want:  false,
		},
		{
			// The value an unavailable reading is reported as, so it must not read back as one:
			// otherwise a real idle card and an unreadable one become the same answer twice over.
			name:  "an idle reading of zero",
			value: 0,
			want:  false,
		},
		{
			name:  "the reading just below the sentinel",
			value: 0xFFFE,
			want:  false,
		},
		{
			name:  "the reading just above the sentinel",
			value: 0x10000,
			want:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isUnavailableReading(c.value))
		})
	}
}

// TestEvaluateDeviceUnhealthy pins the AMD health rule: an uncorrectable UMC ECC error makes the
// card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name     string
		eccCount amdsmi.ErrorCount
		ret      amdsmi.Return
		want     bool
	}{
		{
			name:     "no uncorrectable ECC error is healthy",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 0},
			ret:      amdsmi.STATUS_SUCCESS,
			want:     false,
		},
		{
			name:     "an uncorrectable ECC error is unhealthy",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 3},
			ret:      amdsmi.STATUS_SUCCESS,
			want:     true,
		},
		{
			name:     "an unsupported ECC query is healthy even with a nonzero count",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 3},
			ret:      amdsmi.STATUS_NOT_SUPPORTED,
			want:     false,
		},
		{
			name:     "an ECC query whose driver could not be loaded is healthy even with a nonzero count",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 3},
			ret:      amdsmi.STATUS_FAIL_LOAD_MODULE,
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccCount, tc.ret))
		})
	}
}
