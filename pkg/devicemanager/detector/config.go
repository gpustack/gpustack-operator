package detector

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	detectormock "gpustack.ai/gpustack/pkg/devicemanager/detector/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
)

type Config struct {
	NoPCICheck              bool
	Manufacturers           sets.Set[string]
	NoFastFailed            bool
	MonitorPeriod           time.Duration
	DetectedManufacturersCh chan<- sets.Set[string]
	MockDevicesFile         string
	MockSpecStore           *mockspec.Store
	MockPartitionProvider   detectormock.PartitionProvider
}

func (c *Config) Apply(_ context.Context) (*Detector, error) {
	return New(c)
}
