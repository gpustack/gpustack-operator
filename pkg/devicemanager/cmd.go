package devicemanager

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/devicemanager/preflight"
)

// NewCommand returns a new cobra command for the device manager.
func NewCommand() *cobra.Command {
	c := &cobra.Command{
		Use: "device-manager",
		Aliases: []string{
			"dm",
		},
		Short: "assists in managing hardware resources.",
	}

	c.AddCommand(newServeCommand())
	c.AddCommand(newDetectCommand())
	c.AddCommand(newMonitorCommand())
	c.AddCommand(newPreflightCommand())

	return c
}

func newServeCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use:   "serve",
		Short: "serve manager to detect/monitor local devices, report to Kubernetes and provide device injection.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			m, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}
			err = m.Prepare(ctx)
			if err != nil {
				return fmt.Errorf("prepare manager: %w", err)
			}
			err = m.Start(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	o.AddFlags(c.Flags())

	return c
}

func newDetectCommand() *cobra.Command {
	o := detector.NewOptions()

	c := &cobra.Command{
		Use:   "detect",
		Short: "detect local devices once.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			d, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}

			enc := yaml.NewEncoder(os.Stdout)
			enc.SetIndent(4)
			// The manufacturers this pass could not detect are logged by it, and a one-shot detect
			// has no next pass to come back on.
			grpList, _ := d.DetectAccelerator(ctx)
			return enc.Encode(grpList)
		},
	}

	o.AddFlags(c.Flags(), detector.WithoutMonitorOptions())

	return c
}

// newPreflightCommand is a sibling of detect, not a flag on it. Detecting is a pure read that is
// safe to run anywhere, while this command starts containers that hold an accelerator and asks a
// driver to toggle a mode and put it back — and a diagnostic named "detect" must never do either.
func newPreflightCommand() *cobra.Command {
	o := preflight.NewOptions()

	c := &cobra.Command{
		Use:   "preflight",
		Short: "check once whether local devices can serve the allocation modes their allocators offer.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			p, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}

			// Every manufacturer is reported, including the ones nothing was read for, so the
			// result never has to be read as a pass by omission. Reporting also decides the exit
			// code, which is what a script reads instead of the document.
			return preflight.Report(os.Stdout, p.PreflightAccelerator(ctx))
		},
	}

	o.AddFlags(c.Flags())

	return c
}

func newMonitorCommand() *cobra.Command {
	o := detector.NewOptions()

	c := &cobra.Command{
		Use:   "monitor",
		Short: "monitor local devices once.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			d, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}

			enc := yaml.NewEncoder(os.Stdout)
			enc.SetIndent(4)
			// The manufacturers this pass could not measure are logged by it, and a one-shot
			// monitor has no next pass to keep them out of.
			grpList, _ := d.MonitorAccelerator(ctx)
			return enc.Encode(grpList)
		},
	}

	o.AddFlags(c.Flags(), detector.WithoutMonitorOptions())

	return c
}
