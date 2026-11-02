// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// listWatchBound is how long a stream assertion waits for its event. Long enough for a loaded
// machine under the race detector, short enough that a missing event fails the case instead of
// hanging it.
const listWatchBound = 15 * time.Second

// listWatchSetupBound is how long establishing the stream may take. The ListAndWatch call does
// not return until gRPC holds a ready transport, so a client that cannot dial would block the
// test here without a bound of its own. A failed or abandoned setup cancels the stream context,
// which is what releases the blocked call.
const listWatchSetupBound = 15 * time.Second

// listWatchDevice is one advertised device reduced to what a stream assertion reads: the token
// and its health.
type listWatchDevice struct {
	ID     string
	Health string
}

// listWatchDevices renders a response's devices in order.
func listWatchDevices(resp *ListAndWatchResponse) []listWatchDevice {
	out := make([]listWatchDevice, 0, len(resp.Devices))
	for _, d := range resp.Devices {
		out = append(out, listWatchDevice{ID: d.GetID(), Health: d.GetHealth()})
	}
	return out
}

// listWatchStream reads one real gRPC ListAndWatch stream from a receiver goroutine, so a
// response the test has not consumed yet never blocks the serving handler. Cleanup cancels the
// stream and joins the receiver.
type listWatchStream struct {
	stream grpc.ServerStreamingClient[ListAndWatchResponse]
	cancel context.CancelFunc
	msgs   chan *ListAndWatchResponse
	ended  chan error
	done   chan struct{}
}

// listWatchSetup is one ListAndWatch establishment outcome relayed from the setup goroutine.
type listWatchSetup struct {
	stream grpc.ServerStreamingClient[ListAndWatchResponse]
	err    error
}

func newListWatchStream(t *testing.T, client deviceplugin.DevicePluginClient) *listWatchStream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	// The establishment call blocks until gRPC holds a ready transport, so run it under
	// listWatchSetupBound instead of letting an undialable client hang the test. The channel is
	// buffered so the goroutine finishes even after an abandoned setup.
	setup := make(chan listWatchSetup, 1)
	go func() {
		stream, err := client.ListAndWatch(ctx, &Empty{})
		setup <- listWatchSetup{stream: stream, err: err}
	}()

	var stream grpc.ServerStreamingClient[ListAndWatchResponse]
	select {
	case res := <-setup:
		if res.err != nil {
			cancel()
		}
		require.NoError(t, res.err)
		stream = res.stream
	case <-time.After(listWatchSetupBound):
		cancel()
		t.Fatalf("list and watch setup did not return within %v; the client cannot dial",
			listWatchSetupBound)
	}

	s := &listWatchStream{
		stream: stream,
		cancel: cancel,
		msgs:   make(chan *ListAndWatchResponse, 8),
		ended:  make(chan error, 1),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		for {
			resp, err := s.stream.Recv()
			if err != nil {
				s.ended <- err
				return
			}
			select {
			case s.msgs <- resp:
			case <-ctx.Done():
				s.ended <- ctx.Err()
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(listWatchBound):
			t.Error("list and watch receiver did not return")
		}
	})

	return s
}

// next returns the stream's next response, failing the test when the stream ends first or the
// bound passes.
func (s *listWatchStream) next(t *testing.T) *ListAndWatchResponse {
	t.Helper()

	select {
	case resp := <-s.msgs:
		return resp
	case err := <-s.ended:
		t.Fatalf("list and watch stream ended early: %v", err)
	case <-time.After(listWatchBound):
		t.Fatal("no list and watch response within the bound")
	}
	return nil
}

// awaitEnd waits for the stream's terminal error, failing the test when the stream stays alive
// past the bound.
func (s *listWatchStream) awaitEnd(t *testing.T) error {
	t.Helper()

	select {
	case err := <-s.ended:
		return err
	case <-time.After(listWatchBound):
		t.Fatal("the list and watch stream did not terminate within the bound")
	}
	return nil
}

// TestRDMAServingListWatchWithoutOwner documents what the serving primitive itself does when two
// RDMA servers take one socket path in turn: registering on the path does not fence the first
// server, and an established ListAndWatch stream keeps receiving that server's updates after the
// replacement has taken the path and registered. Only stopping the old server ends the stream,
// which is the negative control. A node-level owner therefore cannot treat socket replacement as
// proof that the previous server or its connections are gone; it has to fence them itself.
//
// The case runs on a real local Unix socket with real gRPC connections and the project's fake
// kubelet. It describes local process behavior only and proves nothing about a real kubelet, the
// Linux socket layer, or security.
func TestRDMAServingListWatchWithoutOwner(t *testing.T) {
	cases := []struct {
		name        string
		mode        workercore.DeviceAllocationMode
		partitioned bool
		// oldPrefix and newPrefix are the endpoint token prefixes the old and the replacement
		// server advertise, spelled from the fixture inventory rather than derived from anything
		// the streams deliver.
		oldPrefix string
		newPrefix string
	}{
		{
			name:      "exclusive",
			mode:      workercore.DeviceAllocationModeExclusive,
			oldPrefix: "ib0:ib0",
			newPrefix: "eth1:eth1",
		},
		{
			name:      "shared",
			mode:      workercore.DeviceAllocationModeShared,
			oldPrefix: "ib0:ib0",
			newPrefix: "eth1:eth1",
		},
		{
			name:        "partitioned",
			mode:        workercore.DeviceAllocationModePartitioned,
			partitioned: true,
			oldPrefix:   "pf0:vf0",
			newPrefix:   "pf0:vf1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const nodeName = "node-rdma-listwatch-owner"

			// advertisement is the response one endpoint's link verdict produces in this case's
			// mode: one token per endpoint, or the shared family's flat token sequence.
			advertisement := func(prefix, health string) []listWatchDevice {
				ids := []string{prefix + ":0000"}
				if tc.mode == workercore.DeviceAllocationModeShared {
					ids = make([]string, 0, nodefeature.RDMAEndpointPoolSize)
					for i := 0; i < nodefeature.RDMAEndpointPoolSize; i++ {
						ids = append(ids, fmt.Sprintf("%s:%04d", prefix, i))
					}
				}
				out := make([]listWatchDevice, 0, len(ids))
				for _, id := range ids {
					out = append(out, listWatchDevice{ID: id, Health: health})
				}
				return out
			}

			newServer := func(ifaceName, vfName, hca string) (
				*rdmaServer, *DevicesReconciler, *workercore.Devices,
			) {
				var iface workercore.DeviceInterface
				if tc.partitioned {
					iface = sriovPF("pf0", "0", rdmaVF(vfName, "0", hca,
						&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK}))
				} else {
					iface = wholeFunctionIface(ifaceName, "0", hca,
						&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK})
				}
				devs := rdmaDevices(nodeName, iface)
				rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(devs)}
				return rdmaTestServer(t, tc.mode, rec), rec, devs
			}

			// failLink marks the inventory's one endpoint's link Failed and pushes the update the
			// way a reconcile does: the broadcast wakes the stream's subscriber, which rebuilds
			// the advertisement from the record as it now stands.
			failLink := func(rec *DevicesReconciler, devs *workercore.Devices) {
				t.Helper()
				if tc.partitioned {
					devs.Spec.Interfaces[0].VirtualFunctions[0].Link = &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateFailed}
				} else {
					devs.Spec.Interfaces[0].Link = &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateFailed}
				}
				require.NoError(t, rec.Client.Update(context.Background(), devs))
				rec.notifiersMutex.Lock()
				rec.lastLivePodUIDs = []string{"pod-listwatch"}
				rec.notifiersMutex.Unlock()
				rec.notifyListeners()
			}

			dir := pluginDir(t)
			kubelet, stopKubelet := startFakeKubelet(t, dir)
			t.Cleanup(stopKubelet)
			kubeSocket := filepath.Join(dir, "kubelet.sock")

			start := func(s *rdmaServer) func() {
				returned := make(chan error, 1)
				go func() { returned <- s.Start(context.Background(), kubeSocket) }()
				var once sync.Once
				stop := func() {
					once.Do(func() {
						s.Stop()
						select {
						case err := <-returned:
							require.NoError(t, err)
						case <-time.After(10 * time.Second):
							t.Error("server did not stop")
						}
					})
				}
				t.Cleanup(stop)
				return stop
			}

			old, oldRec, oldDevs := newServer("ib0", "vf0", "mlx5_1")
			stopOld := start(old)
			require.Eventually(t, func() bool {
				return old.isRegistered() && len(kubelet.registrations()) == 1
			}, 10*time.Second, 20*time.Millisecond)
			socket := filepath.Join(dir, old.servingOptions().SocketName)

			// Permit only the first dial, so a disconnected old client cannot silently
			// reconnect to whatever listens at the path by then.
			var oldDials atomic.Int32
			oldConn, err := grpc.NewClient("passthrough:///old-rdma",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
					if oldDials.Add(1) != 1 {
						return nil, fmt.Errorf("old connection cannot reconnect")
					}
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				}),
			)
			require.NoError(t, err)
			t.Cleanup(func() { _ = oldConn.Close() })
			oldStream := newListWatchStream(t, deviceplugin.NewDevicePluginClient(oldConn))

			// The old server's initial advertisement, read before the replacement exists.
			require.Equal(t, advertisement(tc.oldPrefix, deviceplugin.Healthy),
				listWatchDevices(oldStream.next(t)))

			replacement, newRec, newDevs := newServer("eth1", "vf1", "mlx5_2")
			start(replacement)
			require.Eventually(t, func() bool {
				return replacement.isRegistered() && len(kubelet.registrations()) >= 2
			}, 10*time.Second, 20*time.Millisecond)

			newConn, err := grpc.NewClient("unix://"+socket,
				grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = newConn.Close() })
			newStream := newListWatchStream(t, deviceplugin.NewDevicePluginClient(newConn))

			// The new connection reads the replacement's own advertisement, different from the
			// old server's.
			require.Equal(t, advertisement(tc.newPrefix, deviceplugin.Healthy),
				listWatchDevices(newStream.next(t)))

			// With the replacement serving the path, the old server is triggered for one
			// observable update. The old stream must still receive it over the connection the
			// old server holds, naming the old endpoint and the failed link.
			failLink(oldRec, oldDevs)
			require.Equal(t, advertisement(tc.oldPrefix, deviceplugin.Unhealthy),
				listWatchDevices(oldStream.next(t)))

			// The old client dialed once: it never reconnected to the replacement at the path.
			require.EqualValues(t, 1, oldDials.Load(),
				"the old client reconnected instead of keeping its established connection")

			// Negative control: stopping the old server is what actually ends the old stream.
			stopOld()
			require.Error(t, oldStream.awaitEnd(t))

			notifierCount := func(rec *DevicesReconciler) int {
				rec.notifiersMutex.RLock()
				defer rec.notifiersMutex.RUnlock()

				return len(rec.notifiers)
			}
			require.Eventually(t, func() bool { return notifierCount(oldRec) == 0 },
				15*time.Second, 20*time.Millisecond,
				"the old stream kept its subscription after the old server stopped")

			// The replacement survives the old server's stop and still reports its own state.
			failLink(newRec, newDevs)
			require.Equal(t, advertisement(tc.newPrefix, deviceplugin.Unhealthy),
				listWatchDevices(newStream.next(t)))
		})
	}
}
