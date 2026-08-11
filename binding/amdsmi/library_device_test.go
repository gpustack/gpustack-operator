package amdsmi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// asicInfoWithSerial builds an AsicInfo carrying serial as its NUL-terminated Asic_serial, the way
// the vendor library fills the field.
func asicInfoWithSerial(serial string) AsicInfo {
	var info AsicInfo
	for i := 0; i < len(serial) && i < len(info.Asic_serial)-1; i++ {
		info.Asic_serial[i] = int8(serial[i])
	}
	return info
}

// The serial is the accelerator's identity: the detector publishes it as the accelerator ID, and the
// ROCm runtime matches an agent against exactly that string. So every byte of it has to survive, and
// a prefix has to be removed only when it is actually there — the vendor library reports the serial
// bare, and cutting two characters unconditionally silently renames every AMD accelerator.
func TestGetAsicSerialAndUniqueID(t *testing.T) {
	testCases := []struct {
		name     string
		serial   string
		expected string
		uniqueID string
	}{
		{
			name:     "bare serial keeps every digit",
			serial:   "5C88007D760374F3",
			expected: "5c88007d760374f3",
			uniqueID: "GPU-5c88007d760374f3",
		},
		{
			name:     "0x-prefixed serial loses only the prefix",
			serial:   "0x5C88007D760374F3",
			expected: "5c88007d760374f3",
			uniqueID: "GPU-5c88007d760374f3",
		},
		{
			name:     "unavailable serial has no identity",
			serial:   "N/A",
			expected: "",
			uniqueID: "",
		},
		{
			// The header documents this sentinel for exactly this field. Letting it through would give
			// every accelerator whose serial the library cannot read the same identity.
			name:     "the vendor's unsupported sentinel has no identity",
			serial:   "0xFFFFFFFF",
			expected: "",
			uniqueID: "",
		},
		{
			name:     "a wider all-ones sentinel has no identity either",
			serial:   "0xFFFFFFFFFFFFFFFF",
			expected: "",
			uniqueID: "",
		},
		{
			// The sentinel check must not swallow a real serial that merely begins or ends with f.
			name:     "a real serial bounded by f digits survives",
			serial:   "ffff5c88007df",
			expected: "ffff5c88007df",
			uniqueID: "GPU-ffff5c88007df",
		},
		{
			name:     "empty serial has no identity and does not panic",
			serial:   "",
			expected: "",
			uniqueID: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			info := asicInfoWithSerial(tc.serial)
			assert.Equal(t, tc.expected, info.GetAsicSerial())
			assert.Equal(t, tc.uniqueID, info.GetUniqueId())
		})
	}
}
