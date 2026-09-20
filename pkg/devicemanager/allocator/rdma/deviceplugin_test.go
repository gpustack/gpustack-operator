package rdma

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The RDMA servers are deviceplugin's own unexported type behind its Server interface, so they are
// not assertable the way a vendor's concrete server is. The allocator keeps each one's mode beside
// it, and that pair — the server NewRDMAServer built for a mode and the mode it was asked for — is
// what a case asserts on: which modes are served at all. Nothing else about the servers varies with
// the options.
func modesOf(t *testing.T, a device.Allocator) []workercore.DeviceAllocationMode {
	t.Helper()
	agg, ok := a.(*aggregated)
	require.True(t, ok)
	// Every mode the allocator selected must own a server: the server constructor refuses a mode
	// with no resource name, and that refusal must surface here rather than silently shortening
	// the set.
	require.Len(t, agg.servers, len(agg.modes), "every selected mode must own a server")
	modes := make([]workercore.DeviceAllocationMode, 0, len(agg.servers))
	for i := range agg.servers {
		modes = append(modes, agg.modes[i])
	}
	return modes
}

// TestNew_ServerSet pins which families this allocator registers a device-plugin server for, and
// that each control flag removes exactly its own. Exclusive is ungated as on the vendor side: it is
// the mode a plain request lands in, so it has no flag to be dropped by. No visibility family
// exists, because a visibility allocation names an endpoint another container of the same Pod holds
// and no RDMA allocation record is written to answer that from.
func TestNew_ServerSet(t *testing.T) {
	cases := []struct {
		name string
		opts device.AllocatorOptions
		want []workercore.DeviceAllocationMode
	}{
		{
			name: "every family by default",
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
			},
		},
		{
			name: "--no-shared drops only the shared server",
			opts: device.AllocatorOptions{NoShared: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
			},
		},
		{
			name: "--no-sliced drops only the sliced server",
			opts: device.AllocatorOptions{NoSliced: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModePartitioned,
			},
		},
		{
			name: "--no-partitioned drops only the partitioned server",
			opts: device.AllocatorOptions{NoPartitioned: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
			},
		},
		{
			name: "every switch off leaves the exclusive server alone",
			opts: device.AllocatorOptions{NoShared: true, NoSliced: true, NoPartitioned: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, modesOf(t, New(c.opts)))
		})
	}
}

// The gate every mode is served behind, stated at its source: an RDMA resource name exists for the
// four serving families and for no other mode. NewRDMAServer refuses a mode with no name, which is
// the mechanism the set above is built from, so a mode that gains a name is served the moment it
// does and a mode that loses one is refused rather than registered empty.
func TestNew_ServesOnlyModesWithAResourceName(t *testing.T) {
	assert.Empty(t, nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeVisibility),
		"visibility has no RDMA resource name, so it is not a family this allocator serves")
}

// fakeServer stands in for one RDMA server behind the Server interface. It counts the starts and
// the sockets they were handed, and a start it does not fail blocks until its context ends — the
// shape a serving server has, which is what keeps a run alive for Stop to end.
type fakeServer struct {
	err error

	mu      sync.Mutex
	starts  int
	sockets []string
	started chan struct{}
}

func newFakeServer() *fakeServer {
	// Buffered so a mutated Start that launches a server more than once is recorded by the
	// start count below rather than deadlocking or panicking the second signal.
	return &fakeServer{started: make(chan struct{}, 8)}
}

func (f *fakeServer) Start(ctx context.Context, kubeSocket string) error {
	f.mu.Lock()
	f.starts++
	f.sockets = append(f.sockets, kubeSocket)
	f.mu.Unlock()

	if f.err != nil {
		return f.err
	}
	f.started <- struct{}{}
	<-ctx.Done()
	return nil
}

func (f *fakeServer) Stop() {}

func (f *fakeServer) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeServer) socketsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sockets...)
}

// TestStart_RunsEveryServerOnceAgainstTheKubeSocket pins the half of the wiring the construction
// test cannot see: every registered server is started exactly once, each handed the kube socket the
// allocator was built with, and the caller's cancellation is what the run reports — the servers
// themselves hold no failure.
func TestStart_RunsEveryServerOnceAgainstTheKubeSocket(t *testing.T) {
	f1, f2 := newFakeServer(), newFakeServer()
	agg := &aggregated{
		logger:     logr.Discard(),
		servers:    []deviceplugin.Server{f1, f2},
		kubeSocket: "test-kube.sock",
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() {
		errCh <- agg.Start(ctx)
	}()

	for _, f := range []*fakeServer{f1, f2} {
		select {
		case <-f.started:
		case <-time.After(30 * time.Second):
			t.Fatal("a registered server was never started")
		}
	}

	cancel()
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}

	for _, f := range []*fakeServer{f1, f2} {
		assert.Equal(t, 1, f.startCount(), "each server is started once")
		assert.Equal(t, []string{"test-kube.sock"}, f.socketsSeen(), "each start is handed the kube socket")
	}
}

// TestStart_ReturnsAServerFailure pins that a server that cannot serve surfaces as this
// allocator's own failure, promptly: the caller that starts one allocator per family shares one
// fate with it, so a failure that stopped here — or surfaced only when the process went down —
// would strand the family as a silent no-op.
func TestStart_ReturnsAServerFailure(t *testing.T) {
	boomer := newFakeServer()
	boomer.err = errors.New("listen: no such directory")
	sibling := newFakeServer()
	agg := &aggregated{
		logger:  logr.Discard(),
		servers: []deviceplugin.Server{boomer, sibling},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- agg.Start(context.Background())
	}()

	select {
	case err := <-errCh:
		require.ErrorContains(t, err, "listen:")
	case <-time.After(30 * time.Second):
		t.Fatal("a failing server's error never surfaced from Start")
	}
	// The sibling was started before the failure ended the run, and the run's teardown is what
	// ended it — not a second start.
	assert.Equal(t, 1, sibling.startCount())
}

// TestStop_EndsTheRun pins that Stop is what ends a live run, reported as a clean return: being
// stopped is not an outcome this allocator reports as a failure.
func TestStop_EndsTheRun(t *testing.T) {
	f := newFakeServer()
	agg := &aggregated{
		logger:  logr.Discard(),
		servers: []deviceplugin.Server{f},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- agg.Start(context.Background())
	}()

	select {
	case <-f.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the server was never started")
	}

	agg.Stop()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	assert.Equal(t, 1, f.startCount())
}
