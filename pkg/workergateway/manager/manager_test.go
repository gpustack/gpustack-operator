package manager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	worker "gpustack.ai/gpustack/api/worker/v1"
)

// TestWorkerSubscribe_BlocksWithoutInformers guards the lifecycle fix for a worker subscribed with
// no GVKs (empty informer set): Subscribe must stay blocked until the worker is canceled, so
// SubscribeWorker does not immediately delete and re-subscribe it. Such a worker stays registered
// and reachable through the live-list proxy.
func TestWorkerSubscribe_BlocksWithoutInformers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wk := &_Worker{
		Context:   ctx,
		Cancel:    cancel,
		Cluster:   "test",
		Informers: make(map[schema.GroupVersionKind]cache.SharedIndexInformer),
	}

	done := make(chan error, 1)
	go func() { done <- wk.Subscribe() }()

	// With no informers Subscribe must not return on its own.
	select {
	case err := <-done:
		t.Fatalf("Subscribe returned before cancel: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	assert.True(t, wk.AllReady.Load(), "a zero-informer worker should report ready")

	// Canceling, as Unsubscribe does, must unblock Subscribe.
	wk.Cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after cancel")
	}
}

// TestDefaultInformerFactories_IncludesInstanceTypeFlavor guards the wiring the flavor watch depends
// on: a worker subscribed with the InstanceTypeFlavor GVK must get a watch-backed informer, not just
// the live-list fallback (which delivers no watch events).
func TestDefaultInformerFactories_IncludesInstanceTypeFlavor(t *testing.T) {
	gvk := worker.SchemeGroupVersionKind("InstanceTypeFlavor")
	_, ok := defaultInformerFactories[gvk]
	assert.True(t, ok, "InstanceTypeFlavor must have an informer factory registered")
}
