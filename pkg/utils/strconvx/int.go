package strconvx

import (
	"strconv"

	"gpustack.ai/gpustack/pkg/utils/typex"
)

// Atoi is similar to strconv.Atoi,
// but it returns any integer type.
func Atoi[T typex.Integer](s string) (T, error) {
	i, err := strconv.Atoi(s)
	return T(i), err
}

// Itoa is similar to strconv.Itoa,
// but it accepts any integer type.
func Itoa[T typex.Integer](i T) string {
	return strconv.Itoa(int(i))
}

// FormatInt is similar to strconv.FormatInt,
// but it accepts any integer type.
func FormatInt[T typex.SignedInteger](i T, base int) string {
	return strconv.FormatInt(int64(i), base)
}

// FormatUint is similar to strconv.FormatUint,
// but it accepts any unsigned integer type.
func FormatUint[T typex.UnsignedInteger](i T, base int) string {
	return strconv.FormatUint(uint64(i), base)
}

// ParseInt is similar to strconv.ParseInt,
// but it returns any integer type.
func ParseInt[T typex.SignedInteger](s string, base, bitSize int) (T, error) {
	i, err := strconv.ParseInt(s, base, bitSize)
	return T(i), err
}

// ParseUint is similar to strconv.ParseUint,
// but it returns any unsigned integer type.
func ParseUint[T typex.UnsignedInteger](s string, base, bitSize int) (T, error) {
	u, err := strconv.ParseUint(s, base, bitSize)
	return T(u), err
}
