package detector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// MockDevicesFileEnv names the environment variable supplying the default for --mock-devices-file.
const MockDevicesFileEnv = "GPUSTACK_MOCK_DEVICES_FILE"

type Options struct {
	FlagOptions

	// Control.
	NoPCICheck      bool
	Manufacturers   []string
	NoFastFailed    bool
	MonitorPeriod   time.Duration
	MockDevicesFile string
}

func NewOptions() *Options {
	return &Options{
		// Control.
		NoPCICheck:      false,
		Manufacturers:   nodefeature.GetKnownAcceleratableManufacturers(),
		NoFastFailed:    false,
		MonitorPeriod:   15 * time.Second,
		MockDevicesFile: osx.Getenv(MockDevicesFileEnv),
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
		"publish and report a first detect pass that found nothing, "+
			"instead of holding it back and detecting again; "+
			"only applies when --manufacturer names exactly one manufacturer.")
	fs.StringVar(&o.MockDevicesFile, "mock-devices-file", o.MockDevicesFile,
		"path to a YAML or JSON mock devices specification file for CPU-only testing "+
			"(defaults to "+MockDevicesFileEnv+").")
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
	if o.MockDevicesFile != "" {
		if _, err := mockspec.NewStore(o.MockDevicesFile); err != nil {
			return fmt.Errorf("--mock-devices-file: %w", err)
		}
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	cfg := &Config{
		NoPCICheck:    o.NoPCICheck,
		Manufacturers: sets.New(o.Manufacturers...),
		NoFastFailed:  o.NoFastFailed,
		MonitorPeriod: o.MonitorPeriod,
	}
	if o.MockDevicesFile != "" {
		specStore, err := mockspec.NewStore(o.MockDevicesFile, mockspec.WithLogger(logger.WithName("mockspec")))
		if err != nil {
			return nil, fmt.Errorf("load mock devices file: %w", err)
		}
		cfg.MockDevicesFile = o.MockDevicesFile
		cfg.MockSpecStore = specStore
	}
	return cfg, nil
}

// NewDetectCommand returns a cobra command that runs one detect pass and encodes DevicesGroupList YAML.
func NewDetectCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use:   "detect",
		Short: "detect local devices once.",
		PreRunE: func(c *cobra.Command, _ []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			d, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}

			enc := yaml.NewEncoder(c.OutOrStdout())
			enc.SetIndent(4)
			// The manufacturers this pass could not detect are logged by it, and a one-shot detect
			// has no next pass to come back on.
			grpList, _ := d.DetectAccelerator(ctx)
			return enc.Encode(grpList)
		},
	}

	o.AddFlags(c.Flags(), WithoutMonitorOptions())

	return c
}

// NewMonitorCommand returns a cobra command that runs one monitor pass and encodes MetricsGroupList YAML.
func NewMonitorCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use:   "monitor",
		Short: "monitor local devices once.",
		PreRunE: func(c *cobra.Command, _ []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			d, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}

			enc := yaml.NewEncoder(c.OutOrStdout())
			enc.SetIndent(4)
			// The manufacturers this pass could not measure are logged by it, and a one-shot
			// monitor has no next pass to keep them out of.
			grpList, _ := d.MonitorAccelerator(ctx)
			return enc.Encode(grpList)
		},
	}

	o.AddFlags(c.Flags(), WithoutMonitorOptions())

	return c
}
