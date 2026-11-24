package allocator

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"

	allocatormock "gpustack.ai/gpustack/pkg/devicemanager/allocator/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
)

type Config struct {
	KubeSocket              string
	HostRoot                string
	NoShared                bool
	NoSliced                bool
	NoPartitioned           bool
	DetectedManufacturersCh <-chan sets.Set[string]
	MockDevicesFile         string
	MockSpecStore           *mockspec.Store
	MockPartitionStore      *allocatormock.PartitionStore
	MockOptions             []allocatormock.Option
}

func (c *Config) Apply(_ context.Context) (*Allocator, error) {
	return New(c)
}
