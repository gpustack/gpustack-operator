package system

import (
	"path/filepath"
	"time"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// DataDir is the path to expose the data.
var _DataDir = osx.Getenv("GPUSTACK_DATA_DIR", "/var/lib/gpustack")

// DataDir returns the path to the data directory.
func DataDir() string {
	return _DataDir
}

// SubDataDir returns the path to the subdirectory of DataDir.
func SubDataDir(sub string, others ...string) string {
	if isRunningInsideContainer() {
		return filepath.Join(_DataDir, sub, filepath.Join(others...))
	}
	// NB(thxCode): nice for development.
	return osx.SubTempDir(filepath.Join(time.Now().Format(time.DateOnly), _DataDir, sub, filepath.Join(others...)))
}

// ConfDir is the path to access the metadata.
var _ConfDir = osx.Getenv("GPUSTACK_CONF_DIR", "/etc/gpustack")

// ConfDir returns the path to the config directory.
func ConfDir() string {
	return _ConfDir
}

// SubConfDir returns the path to the subdirectory of ConfDir.
func SubConfDir(sub string, others ...string) string {
	if isRunningInsideContainer() {
		return filepath.Join(_ConfDir, sub, filepath.Join(others...))
	}
	// NB(thxCode): nice for development.
	return osx.SubTempDir(filepath.Join(time.Now().Format(time.DateOnly), _ConfDir, sub, filepath.Join(others...)))
}

func isRunningInsideContainer() bool {
	return osx.Getenv("_RUNNING_INSIDE_CONTAINER_", "false") == "true"
}
