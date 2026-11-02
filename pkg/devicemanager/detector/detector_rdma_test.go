package detector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/varx"
)

func TestStartRDMAEntry(t *testing.T) {
	observed := []device.RDMADevice{{Name: "mlx5_0", Ports: []device.RDMAPort{{Number: 1, NetDevs: []string{"ib0"}}}}}
	cases := []struct {
		name         string
		noFastFailed bool
		waitMonitor  bool
		failedGet    bool
		failedRead   bool
	}{
		{name: "first accelerator remains unavailable"},
		{name: "report fails before reading node", noFastFailed: true, waitMonitor: true},
		{name: "node GET fails", noFastFailed: true, waitMonitor: true, failedGet: true},
		{name: "first failed observation remains unknown", failedRead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var client *kubefake.Clientset
			if tc.failedGet {
				t.Setenv("KUBERNETES_NODE_NAME", "missing-node")
				require.Nil(t, system.LoopbackKubeClient.Get())
				client = kubefake.NewSimpleClientset()
				system.LoopbackKubeClient.Configure(client)
				t.Cleanup(func() { system.LoopbackKubeClient = varx.Once[kubernetes.Interface]{} })
			} else {
				t.Setenv("KUBERNETES_NODE_NAME", "")
			}
			fake := newScriptedDetector("alpha", []scriptedPass{{}}, nil)
			d := &Detector{
				manufacturers: sets.New("alpha"), noFastFailed: tc.noFastFailed,
				detectors: []device.Detector{fake}, monitorPeriod: 10 * time.Millisecond,
				rdmaInterfaceReader: func() ([]device.RDMADevice, error) {
					if tc.failedRead {
						return nil, errors.New("class read failed")
					}
					return observed, nil
				},
			}
			if interfaces, err := DetectInterfaces(); err == nil {
				d.reportedInterfaces, d.reportedInterfacesKnown = interfaces, true
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = d.Start(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			if tc.waitMonitor {
				awaitPasses(t, fake.monitored, 2, "monitor")
			} else {
				awaitPasses(t, fake.detected, 2, "detect")
			}
			cancel()
			<-done
			if tc.failedGet {
				require.NotEmpty(t, client.Actions(), "the node GET must run")
				assert.Equal(t, "get", client.Actions()[0].GetVerb())
				assert.Equal(t, "nodes", client.Actions()[0].GetResource().Resource)
			}
			got := d.RDMAInterfaceSnapshot()
			if tc.failedRead {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.True(t, got.Complete)
			assert.Equal(t, observed, got.Devices)
			assert.Equal(t, device.RDMASnapshotRevision(observed), got.Revision)
			assert.False(t, got.LastSuccessfulTime.IsZero())
			assert.Empty(t, got.Message)
		})
	}
}

func TestStartRDMAMonitorSequence(t *testing.T) {
	first := []device.RDMADevice{{
		Name: "mlx5_0", Ports: []device.RDMAPort{{Number: 1, NetDevs: []string{"ib0"}, GIDs: []string{"gid0"}}},
	}}
	second := []device.RDMADevice{{Name: "mlx5_1", Ports: []device.RDMAPort{{Number: 2, PKeys: []string{"key1"}}}}}
	failure := errors.New("class read failed")
	type reading struct {
		devices []device.RDMADevice
		err     error
	}
	type step struct {
		read        reading
		wantDevices []device.RDMADevice
		complete    bool
		retainTime  bool
	}
	cases := []struct {
		name            string
		steps           []step
		stableDetecting bool
	}{
		{
			name: "monitor failure followed by detect recovery",
			steps: []step{
				{read: reading{devices: first}, wantDevices: first, complete: true},
				{read: reading{err: failure}, wantDevices: first, retainTime: true},
				{read: reading{devices: second}, wantDevices: second, complete: true},
			},
		},
		{
			name: "monitor recovery followed by detect failure",
			steps: []step{
				{read: reading{err: failure}, wantDevices: first, retainTime: true},
				{read: reading{devices: second}, wantDevices: second, complete: true},
				{read: reading{err: failure}, wantDevices: second, retainTime: true},
			},
		},
		{
			name: "stable failures remain in monitor",
			steps: []step{
				{read: reading{err: failure}, wantDevices: first, retainTime: true},
				{read: reading{err: failure}, wantDevices: first, retainTime: true},
				{read: reading{err: failure}, wantDevices: first, retainTime: true},
			},
			stableDetecting: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_NODE_NAME", "")
			ctx, cancel := context.WithCancel(context.Background())
			requests := make(chan chan reading)
			fake := newScriptedDetector("alpha", []scriptedPass{{}}, nil)
			d := &Detector{
				manufacturers: sets.New("alpha"), noFastFailed: true,
				detectors: []device.Detector{fake}, monitorPeriod: 10 * time.Millisecond,
				rdmaInterfaceReader: func() ([]device.RDMADevice, error) {
					reply := make(chan reading)
					select {
					case requests <- reply:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					select {
					case result := <-reply:
						return result.devices, result.err
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				},
			}
			if interfaces, err := DetectInterfaces(); err == nil {
				d.reportedInterfaces, d.reportedInterfacesKnown = interfaces, true
			}
			previous := d.refreshRDMAInterfaces(first, nil, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
			done := make(chan struct{})
			go func() { defer close(done); _ = d.Start(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			nextRequest := func() chan reading {
				t.Helper()
				select {
				case reply := <-requests:
					return reply
				case <-time.After(detectorTimeout):
					t.Fatal("RDMA read did not start")
					return nil
				}
			}
			reply := nextRequest()
			for _, s := range tc.steps {
				reply <- s.read
				reply = nextRequest()
				got := d.RDMAInterfaceSnapshot()
				require.NotNil(t, got)
				assert.Equal(t, s.wantDevices, got.Devices)
				assert.Equal(t, s.complete, got.Complete)
				assert.Equal(t, device.RDMASnapshotRevision(s.wantDevices), got.Revision)
				if s.retainTime {
					assert.Equal(t, previous.LastSuccessfulTime, got.LastSuccessfulTime)
					assert.Equal(t, failure.Error(), got.Message)
				} else {
					assert.True(t, got.LastSuccessfulTime.After(previous.LastSuccessfulTime))
					assert.Empty(t, got.Message)
				}
				previous = got
			}
			if tc.stableDetecting {
				assert.Equal(t, int64(1), fake.detectCalls.Load(), "stable failures must not repeat detection")
			}
			cancel()
			<-done
			assert.Equal(t, previous, d.RDMAInterfaceSnapshot(), "cancellation must not replace the last observation")
		})
	}
}

func TestStartRDMAPeriodicDetect(t *testing.T) {
	t.Setenv("KUBERNETES_NODE_NAME", "")
	ctx, cancel := context.WithCancel(context.Background())
	requests := make(chan struct{})
	observed := []device.RDMADevice{{Name: "mlx5_0"}}
	fake := newScriptedDetector("alpha", []scriptedPass{{}}, nil)
	d := &Detector{
		manufacturers: sets.New("alpha"), noFastFailed: true,
		detectors: []device.Detector{fake}, monitorPeriod: time.Millisecond,
		rdmaInterfaceReader: func() ([]device.RDMADevice, error) {
			select {
			case requests <- struct{}{}:
				return observed, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	if interfaces, err := DetectInterfaces(); err == nil {
		d.reportedInterfaces, d.reportedInterfacesKnown = interfaces, true
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	// Read through one complete monitor period and the next detection pass.
	for i := 0; i < detectEveryNMonitorRounds+3; i++ {
		select {
		case <-requests:
		case <-time.After(detectorTimeout):
			t.Fatal("periodic detection did not resume")
		}
	}
	cancel()
	<-done
	assert.GreaterOrEqual(t, fake.detectCalls.Load(), int64(2))
	got := d.RDMAInterfaceSnapshot()
	require.NotNil(t, got)
	assert.True(t, got.Complete)
	assert.Equal(t, observed, got.Devices)
	assert.Equal(t, device.RDMASnapshotRevision(observed), got.Revision)
	assert.False(t, got.LastSuccessfulTime.IsZero())
	assert.Empty(t, got.Message)
}
