package stringx

import "strings"

// CompareNumeric compares two non-negative decimal-integer strings by numeric
// value, without parsing them into a fixed-width integer, so arbitrarily large
// values compare correctly. It returns -1 if a < b, 0 if equal, +1 if a > b.
//
// Leading zeros are ignored, so "007" compares equal to "7". An empty string or
// an all-zero string ("", "0", "00") is zero. An operand containing any non-digit
// is treated as zero, so malformed input never sorts above a real value.
func CompareNumeric(a, b string) int {
	a, b = normalizeNumeric(a), normalizeNumeric(b)
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// normalizeNumeric strips a pure-digit string's leading zeros, returning "" for
// zero. A string containing any non-digit normalizes to "" (treated as zero).
func normalizeNumeric(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
	}
	return strings.TrimLeft(s, "0")
}
