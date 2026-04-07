package settings

import (
	"gpustack.ai/gpustack/pkg/setting"
)

var settings = setting.Settings{}

// Indexer returns the index function of the settings,
// which can be used to index the settings in other packages.
func Indexer() setting.IndexFunc {
	return settings.Index
}

// the built-in settings for worker.
var (
	ContainerRegistry = settings.NewEditable(
		"container-registry",
		"Indicates the registry to pull images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
		setting.AllowContainerRegistry(),
	)
	ContainerNamespace = settings.NewEditable(
		"container-namespace",
		"Indicates the namespace to pull images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
	)
)
