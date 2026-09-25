//go:build !linux

package gc

import "errors"

// Statfs is Linux-only: the plugin runs on Linux nodes.
func Statfs(string) (Usage, error) {
	return Usage{}, errors.New("the model cache runs on Linux only")
}
