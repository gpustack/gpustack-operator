package strconvx

import (
	"strconv"
	"strings"
)

// ParseUintList parses a comma-separated string into a slice of uint64.
func ParseUintList(s string) ([]uint64, error) {
	var result []uint64
	for _, seg := range strings.Split(s, ",") {
		n, err := strconv.ParseUint(seg, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, nil
}
