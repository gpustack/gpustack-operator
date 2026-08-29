package preflight

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"
)

type Config struct {
	NoPCICheck    bool
	Manufacturers sets.Set[string]
	DryRun        bool
	ProbeImage    string
	HostRoot      string
	Runtime       string
}

func (c *Config) Apply(_ context.Context) (*Preflighter, error) {
	return New(c)
}
