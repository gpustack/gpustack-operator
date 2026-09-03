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
