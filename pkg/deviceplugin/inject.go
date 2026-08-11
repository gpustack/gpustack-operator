package deviceplugin

import (
	"fmt"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// Logical-slicing host paths. The device-manager init container stages the in-image
// /etc/gpustack/lib tree onto the host at OperatorLibDir; per-container working
// directories live under OperatorPodsDir. They are package vars (not consts) so
// tests can redirect them to a temporary directory.
var (
	OperatorLibDir  = "/var/lib/gpustack/operator/lib"
	OperatorPodsDir = "/var/lib/gpustack/operator/pods"
)

// PodWorkDir returns the host working directory for a sliced container,
// <OperatorPodsDir>/<podUID>/c-<container>.
func PodWorkDir(podUID, containerName string) string {
	return filepath.Join(OperatorPodsDir, podUID, "c-"+containerName)
}

// NewDevice creates a DeviceSpec with the given path and permissions.
func NewDevice(path, permissions string) *DeviceSpec {
	if !osx.Exists(path) {
		return nil
	}
	return &DeviceSpec{
		ContainerPath: path,
		HostPath:      path,
		Permissions:   permissions,
	}
}

// NewRWDevice creates a DeviceSpec with read-write permissions.
func NewRWDevice(path string) *DeviceSpec {
	return NewDevice(path, "rw")
}

// NewRWDevicef creates a DeviceSpec with read-write permissions and a formatted path.
func NewRWDevicef(format string, args ...any) *DeviceSpec {
	return NewDevice(fmt.Sprintf(format, args...), "rw")
}

// NewDevicesIn creates a list of DeviceSpec for all files in the specified directory with the given permissions.
func NewDevicesIn(dir, permissions string) []*DeviceSpec {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	devs := make([]*DeviceSpec, 0, len(entries))
	for i := range entries {
		if entries[i].IsDir() {
			continue
		}
		path := filepath.Join(dir, entries[i].Name())
		devs = append(devs, &DeviceSpec{
			ContainerPath: path,
			HostPath:      path,
			Permissions:   permissions,
		})
	}

	return devs
}

// NewRWDevicesIn creates a list of DeviceSpec with read-write permissions for all files in the specified directory.
func NewRWDevicesIn(dir string) []*DeviceSpec {
	return NewDevicesIn(dir, "rw")
}

// NewMount creates a Mount with the given path and permissions.
func NewMount(path string, readOnly bool) *Mount {
	if !osx.Exists(path) {
		return nil
	}
	return &Mount{
		ContainerPath: path,
		HostPath:      path,
		ReadOnly:      readOnly,
	}
}

// NewROMount creates a Mount with read-only permissions.
func NewROMount(path string) *Mount {
	return NewMount(path, true)
}

// NewROMountf creates a Mount with read-only permissions and a formatted path.
func NewROMountf(format string, args ...any) *Mount {
	return NewMount(fmt.Sprintf(format, args...), true)
}
