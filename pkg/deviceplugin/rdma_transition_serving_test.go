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
)

// TestRDMAServingWithoutOwner checks the raw serving primitive, without a node owner.
// Two independent servers use the same socket path. The replacement does not stop
// an established connection to the first server. A future owner must fence that
// connection rather than treat a replacement socket as proof of fencing.
func TestRDMAServingWithoutOwner(t *testing.T) {
	cases := []struct {
		name        string
		mode        workercore.DeviceAllocationMode
		id          string
		env         string
		partitioned bool
	}{
		{
			name: "exclusive",
			mode: workercore.DeviceAllocationModeExclusive,
			id:   "ib0:ib0:0000",
			env:  "GPUSTACK_RDMA_DEVICES",
		},
		{
			name: "shared",
			mode: workercore.DeviceAllocationModeShared,
			id:   "ib0:ib0:0000",
			env:  "GPUSTACK_RDMA_DEVICES",
		},
		{
			name:        "partitioned",
			mode:        workercore.DeviceAllocationModePartitioned,
			id:          "pf0:vf0:0000",
			env:         "GPUSTACK_RDMA_DEVICES",
			partitioned: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRdmaAllocateFixture(t)
			f.mapVerbs("mlx5_1", "uverbs1")
			f.mapVerbs("mlx5_2", "uverbs2")
			f.writeNode("uverbs1")
			f.writeNode("uverbs2")

			dir := pluginDir(t)
			kubelet, stopKubelet := startFakeKubelet(t, dir)
			t.Cleanup(stopKubelet)
			kubeSocket := filepath.Join(dir, "kubelet.sock")

			newServer := func(hca string) *rdmaServer {
				const nodeName = "node-rdma-owner"
				iface := wholeFunctionIface("ib0", "0", hca, nil)
				if tc.partitioned {
					iface = sriovPF("pf0", "0", rdmaVF("vf0", "0", hca, nil))
				}
				rec := &DevicesReconciler{
					NodeName: nodeName,
					Client:   nodeFixture(rdmaDevices(nodeName, iface)),
				}
				return rdmaTestServer(t, tc.mode, rec)
			}
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

			old := newServer("mlx5_1")
			stopOld := start(old)
			require.Eventually(t, func() bool {
				return old.isRegistered() && len(kubelet.registrations()) == 1
			}, 10*time.Second, 20*time.Millisecond)
			socket := filepath.Join(dir, old.servingOptions().SocketName)

			// Permit only the first dial, so a disconnected old client cannot silently
			// reconnect to the replacement at the same path.
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
			oldClient := deviceplugin.NewDevicePluginClient(oldConn)

			allocate := func(client deviceplugin.DevicePluginClient) (*AllocateResponse, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				return client.Allocate(ctx, allocateRequest(tc.id))
			}
			checkGrant := func(client deviceplugin.DevicePluginClient, hca, node string) {
				t.Helper()
				resp, err := allocate(client)
				require.NoError(t, err)
				require.Len(t, resp.ContainerResponses, 1)
				grant := resp.ContainerResponses[0]
				require.Equal(t, hca, grant.Envs[tc.env])
				require.Equal(t, "="+hca, grant.Envs["NCCL_IB_HCA"])
				require.Equal(t, []rdmaDeviceSpec{{
					HostPath: f.nodePath(node), ContainerPath: f.nodePath(node), Permissions: "rw",
				}}, deviceSpecsOf(grant))
			}
			checkGrant(oldClient, "mlx5_1", "uverbs1")

			replacement := newServer("mlx5_2")
			start(replacement)
			require.Eventually(t, func() bool {
				return replacement.isRegistered() && len(kubelet.registrations()) >= 2
			}, 10*time.Second, 20*time.Millisecond)

			newConn, err := grpc.NewClient("unix://"+socket,
				grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = newConn.Close() })
			newClient := deviceplugin.NewDevicePluginClient(newConn)
			checkGrant(newClient, "mlx5_2", "uverbs2")
			checkGrant(oldClient, "mlx5_1", "uverbs1")
			require.EqualValues(t, 1, oldDials.Load())

			// Stopping the old server is a negative control for the old connection.
			// It must not disturb the replacement's already established connection.
			stopOld()
			_, err = allocate(oldClient)
			require.Error(t, err)
			checkGrant(newClient, "mlx5_2", "uverbs2")
		})
	}
}
