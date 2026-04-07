package controller

import (
	"context"
	"fmt"

	"github.com/davecgh/go-spew/spew"

	"gpustack.ai/gpustack/pkg/manager"
)

// ExecuteSetup executes the given setup to configure the controller.
func ExecuteSetup(ctx context.Context, mgr manager.CtrlManager, setups []Setup) error {
	for i := range setups {
		opts := SetupOptions{
			Manager: mgr,
		}
		err := setups[i].SetupController(ctx, opts)
		if err != nil {
			return fmt.Errorf("controller setup: %s: %w", spew.Sdump(setups[i]), err)
		}
	}
	return nil
}
