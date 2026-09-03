package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// TestSetupsCarryTheKVCachePoolReconciler guards the one failure mode this list has: a reconciler
// left out of it COMPILES, and then does nothing. Nothing else in the build says so — the type
// exists, its SetupController is correct, and no pass ever runs.
func TestSetupsCarryTheKVCachePoolReconciler(t *testing.T) {
	assert.NotNil(t, Get[*worker.KVCachePoolReconciler]())
}

// TestSetup_ModelDeploymentIsRegistered guards the wiring rather than the behavior.
//
// A reconciler that is written but never added to this list compiles, passes every one of its own
// unit tests, and does nothing at all on a cluster — the ModelDeployment objects a user creates
// would simply sit there. Nothing else in the build catches that, so it is asserted here, against
// the registry itself rather than against a copy of it.
func TestSetup_ModelDeploymentIsRegistered(t *testing.T) {
	assert.NotNil(t, Get[*worker.ModelDeploymentReconciler]())
}
