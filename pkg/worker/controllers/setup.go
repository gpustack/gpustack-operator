package controllers

import (
	"context"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// setups is the list of controller setup functions.
var setups = []controller.Setup{
	new(worker.ClusterQueueReconciler),
	new(worker.CohortReconciler),
	new(worker.InstanceEntranceReconciler),
	new(worker.LocalQueueReconciler),
	new(worker.NodeFeatureReconciler),
	new(worker.ResourceFlavorReconciler),
	new(worker.ResourceFlavorCleanupReconciler),
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
