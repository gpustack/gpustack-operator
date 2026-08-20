package devicemanager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/manager"
)

// _FakeCtrlManager stands in for the controller manager. Only the two methods below are reached
// before the controller setup fails, so the rest stays the embedded nil interface — the shape
// pkg/manager's own tests use for the same reason.
//
// Its scheme is empty on purpose. The reconciler's first act is to index a field on a Pod, and a
// scheme that does not know Pods makes that fail, which is how a subsystem is made to fail here
// without a cluster or a fake client.
type _FakeCtrlManager struct {
	ctrl.Manager
}

func (_FakeCtrlManager) GetScheme() *runtime.Scheme { return runtime.NewScheme() }

func (_FakeCtrlManager) GetWebhookServer() ctrlwebhook.Server {
	return ctrlwebhook.NewServer(ctrlwebhook.Options{})
}

// TestManager_StartEndsWhenASubsystemFails pins that the device manager's own task group is over as
// soon as one of its subsystems is.
//
// All four — the detector, the allocator, the metrics exporter and the controller manager — are meant
// to last the process, and three of them here are doing what they do for the whole of it: the
// detector is between ticks, and the two behind WaitForReady are waiting on a manager that never
// starts. A group that handed each task the caller's context would therefore leave them waiting on a
// caller that is still very much alive, and Start would never return — the process staying up, and
// its Pod passing its liveness probe, with a dead subsystem inside it.
func TestManager_StartEndsWhenASubsystemFails(t *testing.T) {
	// The detector reports its findings to the Node, and gives up on the missing name before it
	// reaches a kube client — so it loops harmlessly for as long as this test runs rather than
	// panicking on a client it was never given.
	t.Setenv("KUBERNETES_NODE_NAME", "")

	// No manufacturers means no detectors, so detection itself is a no-op on any platform; the tick
	// is an hour away so it happens once.
	det, err := detector.New(&detector.Config{
		Manufacturers: sets.New[string](),
		MonitorPeriod: time.Hour,
	})
	require.NoError(t, err)
	alc, err := allocator.New(&allocator.Config{})
	require.NoError(t, err)

	m := &Manager{
		Manager: &manager.Manager{
			CtrlManager: manager.CtrlManager{Manager: _FakeCtrlManager{}},
		},
		Detector:  det,
		Allocator: alc,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- m.Start(ctx)
	}()

	select {
	case err := <-errCh:
		// Reported rather than swallowed: the device manager's caller is what decides the process
		// exits, and it can only decide that if the failure reaches it.
		require.Error(t, err, "a subsystem that failed must be reported, not waited on")
		assert.Contains(t, err.Error(), "setup controllers")
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after one of its subsystems failed")
	}
}
