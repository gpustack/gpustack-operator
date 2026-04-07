package controllers

import (
	"context"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/manager"
)

// setups is the list of controller setup functions.
var setups = []controller.Setup{
	new(deviceplugin.DevicesReconciler),
}

// Get returns the controller setup of the specified type.
func Get[T any]() T {
	for i := range setups {
		if s, ok := setups[i].(T); ok {
			return s
		}
	}
	var zero T
	return zero
}

// Setup sets up all controllers.
func Setup(ctx context.Context, mgr manager.CtrlManager) error {
	return controller.ExecuteSetup(ctx, mgr, setups)
}
