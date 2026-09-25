//go:build !linux

package driver

import "errors"

// HostMounter is Linux-only: the plugin runs on Linux nodes.
type HostMounter struct{}

var errNotLinux = errors.New("mounting is Linux-only")

func (HostMounter) IsMountPoint(string) (bool, error) { return false, errNotLinux }
func (HostMounter) IsReadOnly(string) (bool, error)   { return false, errNotLinux }
func (HostMounter) BindReadOnly(string, string) error { return errNotLinux }
func (HostMounter) Unmount(string) error              { return errNotLinux }
