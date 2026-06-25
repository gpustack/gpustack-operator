package system

import (
	"path/filepath"
	"time"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// DataDir is the path to expose the data,
// mounted from the host, which is used to store the data of gpustack.
var _DataDir = osx.Getenv("GPUSTACK_DATA_DIR", "/var/lib/gpustack")

// DataDir returns the path to the data directory,
// mounted from the host, which is used to store the data of gpustack.
func DataDir() string {
	return _DataDir
}

// SubDataDir returns the path to the subdirectory of DataDir,
// mounted from the host, which is used to store the data of gpustack.
func SubDataDir(sub string, others ...string) string {
	if IsRunningInContainer() {
		return filepath.Join(_DataDir, "operator", sub, filepath.Join(others...))
	}
	// NB(thxCode): nice for development.
	return osx.SubTempDir(filepath.Join(time.Now().Format(time.DateOnly), _DataDir, "operator", sub, filepath.Join(others...)))
}

// ConfDir is the path to access the metadata,
// internal configuration, and other configuration files of gpustack.
var _ConfDir = osx.Getenv("GPUSTACK_CONF_DIR", "/etc/gpustack")

// ConfDir returns the path to the config directory,
// internal configuration, and other configuration files of gpustack.
func ConfDir() string {
	return _ConfDir
}

// SubConfDir returns the path to the subdirectory of ConfDir,
// internal configuration, and other configuration files of gpustack.
func SubConfDir(sub string, others ...string) string {
	if IsRunningInContainer() {
		return filepath.Join(_ConfDir, sub, filepath.Join(others...))
	}
	// NB(thxCode): nice for development.
	return osx.SubTempDir(filepath.Join(time.Now().Format(time.DateOnly), _ConfDir, sub, filepath.Join(others...)))
}

// IsRunningInContainer returns true if the process is running inside a container.
func IsRunningInContainer() bool {
	return osx.Getenv("_RUNNING_INSIDE_CONTAINER_", "false") == "true"
}

// IsRunningAsPrivileged returns true if the process is running as privileged.
func IsRunningAsPrivileged() bool {
	return osx.ExistsDevice("/dev/kmsg")
}
