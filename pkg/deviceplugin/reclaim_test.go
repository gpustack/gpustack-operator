package deviceplugin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// reclaimLoop must (1) never reconcile on a resync before the first broadcast seeds
// the live set, (2) reconcile with the broadcast's set on every broadcast, (3)
// reconcile against the last seeded set on a resync tick, and (4) stop on ctx cancel.
func Test_reclaimLoop(t *testing.T) {
	calls := make(chan []string, 8)
	reconcile := func(live []string) { calls <- live }

	notifier := make(chan []string)
	resync := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		reclaimLoop(ctx, reconcile, notifier, resync)
		close(done)
	}()

	// A resync before any broadcast (unseeded) must not reconcile. The unbuffered send
	// synchronizes with the loop's receive, so the loop has processed it once this returns.
	resync <- time.Time{}
	select {
	case <-calls:
		t.Fatal("resync before seeding must not reconcile")
	case <-time.After(50 * time.Millisecond):
	}

	// A broadcast seeds the live set and reconciles with it.
	notifier <- []string{"pod-a"}
	assert.Equal(t, []string{"pod-a"}, <-calls)

	// A resync now reconciles against the last seeded set.
	resync <- time.Time{}
	assert.Equal(t, []string{"pod-a"}, <-calls)

	// A newer broadcast updates the seeded set for later resyncs.
	notifier <- []string{}
	assert.Empty(t, <-calls)
	resync <- time.Time{}
	assert.Empty(t, <-calls)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop on ctx cancel")
	}
}
