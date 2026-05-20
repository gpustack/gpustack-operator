package workergateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand returns a new cobra command for the worker gateway.
func NewCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use: "worker-gateway",
		Aliases: []string{
			"wg",
		},
		Short: "aggregates resources from upstream Kubernetes clusters.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			wg, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}
			if err = wg.Prepare(ctx); err != nil {
				return fmt.Errorf("prepare: %w", err)
			}
			err = wg.Start(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	o.AddFlags(c.Flags())

	return c
}
