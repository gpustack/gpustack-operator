package detector

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/devicefeature"
)

type Options struct {
	FlagOptions

	// Control.
	NoPCICheck     bool
	Manufacturers  []string
	NoFastFailed   bool
	MonitorPeriod  time.Duration
	MonitorHistory time.Duration
}

func NewOptions() *Options {
	return &Options{
		// Control.
		NoPCICheck:     false,
		Manufacturers:  devicefeature.GetKnownManufacturers(),
		NoFastFailed:   false,
		MonitorPeriod:  5 * time.Second,
		MonitorHistory: 5 * time.Minute,
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
			"the period at which the monitor checks the devices.")
		fs.DurationVar(&o.MonitorHistory, "monitor-history", o.MonitorHistory,
			"how long of the monitor history to keep, should be greater than or equal to the monitor period.")
	}
}

func (o *Options) Validate(_ context.Context) error {
	// Control.
	if len(o.Manufacturers) != 0 {
		knownManufacturers := devicefeature.GetKnownManufacturers()
		if !sets.New[string](knownManufacturers...).HasAll(o.Manufacturers...) {
			return errors.New("--manufacturer: should be a comma separated list of valid manufacturers, " +
				"valid manufacturers: " + strings.Join(knownManufacturers, ","))
		}
	}
	if !o.noMonitorOptions {
		if o.MonitorPeriod <= 1*time.Second {
			return errors.New("--monitor-period: should be greater than 1s")
		}
		if o.MonitorHistory < o.MonitorPeriod {
			return errors.New("--monitor-history: should be greater than or equal to --monitor-period")
		}
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	return &Config{
		NoPCICheck:     o.NoPCICheck,
		Manufacturers:  sets.New(o.Manufacturers...),
		NoFastFailed:   o.NoFastFailed,
		MonitorPeriod:  o.MonitorPeriod,
		MonitorHistory: uint64(o.MonitorHistory / o.MonitorPeriod),
	}, nil
}
