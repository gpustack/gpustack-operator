package manager

import (
	"context"
	"time"

	"k8s.io/client-go/rest"
)

type (
	// ConstructRestConfigFunc defines a function type for constructing rest.Config for a given cluster.
	ConstructRestConfigFunc = func(cluster string) (*rest.Config, error)

	Config struct {
		ConstructRestConfig ConstructRestConfigFunc
		ResyncPeriod        time.Duration
	}
)

func (c *Config) Apply(ctx context.Context) (Manager, error) {
	return New(ctx, c)
}
