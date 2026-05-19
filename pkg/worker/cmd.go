package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand returns a new cobra command for the worker.
func NewCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use: "worker",
		Aliases: []string{
			"w",
		},
		Short: "assists in managing GPUStack Kubernetes resources.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate(c.Context())
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete(ctx)
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			w, err := cfg.Apply(ctx)
			if err != nil {
				return fmt.Errorf("apply config: %w", err)
			}
			err = w.Prepare(ctx)
			if err != nil {
				return fmt.Errorf("prepare worker: %w", err)
			}
			err = w.Start(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	o.AddFlags(c.Flags())

	return c
}
