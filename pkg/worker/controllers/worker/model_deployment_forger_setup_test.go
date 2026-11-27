package worker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/worker/controllers"
	"gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

func TestModelDeploymentForger_ControllersRegisteredInSetup(t *testing.T) {
	assert.NotNil(t, controllers.Get[*worker.ModelKernelArtifactReconciler](),
		"ModelKernelArtifactReconciler must be registered in pkg/worker/controllers")
	assert.NotNil(t, controllers.Get[*worker.ModelDeploymentForgerReconciler](),
		"ModelDeploymentForgerReconciler must be registered in pkg/worker/controllers")
}
