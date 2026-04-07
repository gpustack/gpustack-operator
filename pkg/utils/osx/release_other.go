//go:build !linux

package osx

// GetRelease returns the OS release string, e.g. "5.15.0-1051-azure".
func GetRelease() string {
	return ""
}
