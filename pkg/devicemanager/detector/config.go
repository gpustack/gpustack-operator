package detector

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
)

type Config struct {
	NoPCICheck              bool
	Manufacturers           sets.Set[string]
	MonitorPeriod           time.Duration
	MonitorHistory          uint64
	DetectedManufacturersCh chan<- sets.Set[string]
}

func (c *Config) Apply(_ context.Context) (*Detector, error) {
	return New(c)
}
