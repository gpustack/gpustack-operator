package detector

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

type Options struct {
	FlagOptions

	// Control.
	NoPCICheck    bool
	Manufacturers []string
	NoFastFailed  bool
	MonitorPeriod time.Duration
}

func NewOptions() *Options {
	return &Options{
		// Control.
		NoPCICheck:    false,
		Manufacturers: nodefeature.GetKnownAcceleratableManufacturers(),
		NoFastFailed:  false,
		MonitorPeriod: 15 * time.Second,
	}
}

type (
	FlagOptions struct {
		noMonitorOptions bool
	}

	FlagOption func(opts *FlagOptions)
)

func WithoutMonitorOptions() FlagOption {
	return func(opts *FlagOptions) {
		opts.noMonitorOptions = true
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet, opts ...FlagOption) {
	for i := range opts {
		opts[i](&o.FlagOptions)
	}

	// Control.
	fs.BoolVar(&o.NoPCICheck, "no-pci-check", o.NoPCICheck,
		"disable pci check.")
	fs.StringSliceVar(&o.Manufacturers, "manufacturer", o.Manufacturers,
		"comma separated list of manufacturers to detect.")
	fs.BoolVar(&o.NoFastFailed, "no-fast-failed", o.NoFastFailed,
		"disable fast failed, "+
			"which means the detector will not fail immediately when --manufacturer configured one manufacturer.")
	if !o.noMonitorOptions {
		fs.DurationVar(&o.MonitorPeriod, "monitor-period", o.MonitorPeriod,
			"the period at which the monitor samples the devices, only the latest sample is kept.")
	}
}

func (o *Options) Validate(_ context.Context) error {
	// Control.
	if len(o.Manufacturers) != 0 {
		knownManufacturers := nodefeature.GetKnownAcceleratableManufacturers()
		if !sets.New[string](knownManufacturers...).HasAll(o.Manufacturers...) {
			return errors.New("--manufacturer: should be a comma separated list of valid manufacturers, " +
				"valid manufacturers: " + strings.Join(knownManufacturers, ","))
		}
	}
	if !o.noMonitorOptions {
		if o.MonitorPeriod <= 1*time.Second {
			return errors.New("--monitor-period: should be greater than 1s")
		}
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	return &Config{
		NoPCICheck:    o.NoPCICheck,
		Manufacturers: sets.New(o.Manufacturers...),
		NoFastFailed:  o.NoFastFailed,
		MonitorPeriod: o.MonitorPeriod,
	}, nil
}
