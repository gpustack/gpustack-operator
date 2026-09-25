//go:build !linux

package store

import "errors"

// Device is Linux-only: the plugin runs on Linux nodes.
func Device(string) (string, error) {
	return "", errors.New("the model cache runs on Linux only")
}
