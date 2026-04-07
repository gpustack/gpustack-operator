package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand returns a new cobra command for the server.
func NewCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use:   "server",
		Short: "serves/controls GPUStack Kubernetes resources.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			s, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}
			err = s.Prepare(ctx)
			if err != nil {
				return fmt.Errorf("prepare server: %w", err)
			}
			err = s.Start(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	o.AddFlags(c.Flags())

	return c
}
