package allocator

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"
)

type Config struct {
	KubeSocket              string
	NoShared                bool
	NoSliced                bool
	SlicingPolicy           string
	DetectedManufacturersCh <-chan sets.Set[string]
}

func (c *Config) Apply(_ context.Context) (*Allocator, error) {
	return New(c)
}
