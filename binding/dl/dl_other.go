//go:build !linux

package dl

import (
	"fmt"
)

// Path is NOT supported on non-Linux platforms.
// For example, on freebsd (darwin) systems, dladdr should be used instead of
// dlinfo which is used on linux.
// See for example: https://github.com/Manu343726/siplasplas/issues/82
// For now we return an error.
func (dl *DynamicLibrary) Path() (string, error) {
	return "", fmt.Errorf("not implemented")
}
