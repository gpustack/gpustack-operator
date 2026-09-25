// Package modelmanager is the node component that delivers model weights from a node-local cache:
// a CSI node plugin serving inline ephemeral volumes, with the cache's store, downloader and
// collection behind it, and the node's NodeModelStore status as its report.
package modelmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand returns the cobra command of the model manager.
func NewCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use: "model-manager",
		Aliases: []string{
			"mm",
		},
		Short: "materializes, verifies and mounts model weights from the node's cache.",
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
			if err := m.Prepare(ctx); err != nil {
				return fmt.Errorf("prepare manager: %w", err)
			}
			if err := m.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	o.AddFlags(c.Flags())

	return c
}
