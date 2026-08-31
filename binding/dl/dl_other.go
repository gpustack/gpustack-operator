//go:build !linux

package dl

import (
	"fmt"
)

// RTLD_DEEPBIND is a GNU extension, so there is no such flag to ask for on the platforms this file
// covers. It is defined as a no-op rather than left out so that a caller needing it where it exists
// still compiles here, and is handed exactly the flags it would have passed without it — otherwise
// every such caller carries a platform seam of its own for a flag this package already owns.
const RTLD_DEEPBIND = 0

// Path is NOT supported on non-Linux platforms.
// For example, on freebsd (darwin) systems, dladdr should be used instead of
// dlinfo which is used on linux.
// See for example: https://github.com/Manu343726/siplasplas/issues/82
// For now we return an error.
func (dl *DynamicLibrary) Path() (string, error) {
	return "", fmt.Errorf("not implemented")
}
