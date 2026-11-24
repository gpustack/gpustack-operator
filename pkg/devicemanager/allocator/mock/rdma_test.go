package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestNewRDMAAllocator_Modes(t *testing.T) {
	cases := []struct {
		name      string
		opts      device.AllocatorOptions
		wantModes []workercore.DeviceAllocationMode
	}{
		{
			name: "default enables exclusive, shared, and partitioned",
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModePartitioned,
			},
		},
		{
			name: "NoShared drops shared",
			opts: device.AllocatorOptions{NoShared: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModePartitioned,
			},
		},
		{
			name: "NoSliced has no effect on RDMA",
			opts: device.AllocatorOptions{NoSliced: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModePartitioned,
			},
		},
		{
			name: "NoPartitioned drops partitioned",
			opts: device.AllocatorOptions{NoPartitioned: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := newFakeReconciler("node-rdma")
			a := NewRDMAAllocator(c.opts, WithReconciler(rec))
			assert.Equal(t, RDMAFamilyName, a.Name())
			assert.Equal(t, c.wantModes, a.Modes())
			assert.Len(t, a.Servers(), len(c.wantModes))
		})
	}

	t.Run("NewRDMAServer rejects unsupported visibility and sliced modes", func(t *testing.T) {
		rec, _ := newFakeReconciler("node-rdma")
		_, err := NewRDMAServer(logr.Discard(), workercore.DeviceAllocationModeVisibility, rec)
		require.Error(t, err)
		_, err = NewRDMAServer(logr.Discard(), workercore.DeviceAllocationModeSliced, rec)
		require.Error(t, err)
	})
}

func TestRDMAAllocator_EndToEndServeAndAllocateZeroDevices(t *testing.T) {
	sockDir := shortSocketDir(t)
	kubeSock, fakeKube := startFakeKubelet(t, sockDir)

	const nodeName = "node-rdma-e2e"
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "ascend-910b2",
				Manufacturer: nodefeature.ManufacturerAscend,
				Name:         "Ascend 910B2",
				Memory:       65536,
			}},
			Interfaces: []workercore.DeviceInterface{
				{
					Name:         "eth0",
					PciBusID:     "0000:1a:00.0",
					NumaAffinity: "0",
					RDMA:         true,
					RDMADevice:   "mlx5_0",
					Link:         &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
				},
				{
					Name:         "eth1",
					PciBusID:     "0000:1b:00.0",
					NumaAffinity: "1",
					SRIOV:        true,
					RDMA:         true,
					VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{{
						Name:       "eth1v0",
						RDMADevice: "mlx5_2",
						RDMA:       true,
						Link:       &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
					}},
				},
			},
		},
	}

	// Also configure a netdev-free verbs device via mockspec Store to verify SpecStore fallback.
	specFile := filepath.Join(t.TempDir(), "rdma.yaml")
	require.NoError(t, os.WriteFile(specFile, []byte(`
rdmaDevices:
  - name: mlx5_1
    pciBusId: "0000:1c:00.0"
    ports:
      - number: 1
        state: ACTIVE
        physicalState: LinkUp
`), 0o600))
	specStore, err := mockspec.NewStore(specFile)
	require.NoError(t, err)

	rec, _ := newFakeReconciler(nodeName, devs)
	rdmaAlloc := RDMAAllocatorCreator(
		WithReconciler(rec),
		WithSpecStore(specStore, nodeName),
	)(device.AllocatorOptions{
		Logger:     logr.Discard(),
		KubeSocket: kubeSock,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- rdmaAlloc.Start(ctx) }()

	for range 3 {
		select {
		case <-fakeKube.notifyCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for mock RDMA servers to register")
		}
	}

	reg := fakeKube.registeredResources()
	exRes := string(nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeExclusive))
	partRes := string(nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModePartitioned))
	require.Contains(t, reg, exRes)
	require.Contains(t, reg, partRes)

	// Dial Exclusive RDMA gRPC server and verify ListAndWatch, GetPreferredAllocation, and Allocate.
	exConn, err := grpc.NewClient("unix://"+filepath.Join(sockDir, reg[exRes]),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = exConn.Close() })
	exCli := kubeletdeviceplugin.NewDevicePluginClient(exConn)

	optsResp, err := exCli.GetDevicePluginOptions(ctx, &kubeletdeviceplugin.Empty{})
	require.NoError(t, err)
	assert.True(t, optsResp.GetPreferredAllocationAvailable)

	lwCtx, lwCancel := context.WithTimeout(ctx, 5*time.Second)
	defer lwCancel()
	lw, err := exCli.ListAndWatch(lwCtx, &kubeletdeviceplugin.Empty{})
	require.NoError(t, err)
	lwResp, err := lw.Recv()
	require.NoError(t, err)
	require.NotEmpty(t, lwResp.Devices)

	prefResp, err := exCli.GetPreferredAllocation(ctx, &kubeletdeviceplugin.PreferredAllocationRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerPreferredAllocationRequest{{
			AvailableDeviceIDs: []string{"eth0:eth0:0000"},
			AllocationSize:     1,
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"eth0:eth0:0000"}, prefResp.ContainerResponses[0].DeviceIDs)

	allocResp, err := exCli.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{"eth0:eth0:0000", "mlx5_1:mlx5_1:0000"},
		}},
	})
	require.NoError(t, err)
	ctr := allocResp.ContainerResponses[0]
	assert.Empty(t, ctr.Devices, "mock RDMA must mount zero /dev/infiniband/* nodes")
	assert.Empty(t, ctr.Mounts, "mock RDMA must mount zero host driver paths even on Ascend node")
	assert.Equal(t, "=mlx5_0,mlx5_1", ctr.Envs["NCCL_IB_HCA"])
	assert.Equal(t, "mlx5_0,mlx5_1", ctr.Envs["GPUSTACK_RDMA_DEVICES"])
	assert.Equal(t, "mlx5_0,mlx5_1", ctr.Envs[EnvMockAllocatedDevices])
	assert.Equal(t, "exclusive", ctr.Envs[EnvMockAllocationMode])
	assert.Equal(t, RDMAFamilyName, ctr.Envs[EnvMockManufacturer])

	// Invalid RDMA token is refused cleanly.
	_, err = exCli.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{"unknown:unknown:0000"},
		}},
	})
	require.Error(t, err)

	// Dial Partitioned RDMA gRPC server and allocate VF endpoint.
	partConn, err := grpc.NewClient("unix://"+filepath.Join(sockDir, reg[partRes]),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = partConn.Close() })
	partCli := kubeletdeviceplugin.NewDevicePluginClient(partConn)

	vfResp, err := partCli.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{"eth1:eth1v0:0000"},
		}},
	})
	require.NoError(t, err)
	vfCtr := vfResp.ContainerResponses[0]
	assert.Empty(t, vfCtr.Devices)
	assert.Empty(t, vfCtr.Mounts)
	assert.Equal(t, "=mlx5_2", vfCtr.Envs["NCCL_IB_HCA"])
	assert.Equal(t, "mlx5_2", vfCtr.Envs["GPUSTACK_RDMA_DEVICES"])
	assert.Equal(t, "partitioned", vfCtr.Envs[EnvMockAllocationMode])

	rdmaAlloc.Stop()
	require.NoError(t, <-errCh)
}

func TestRDMAServer_InventoryAugmentationExternalOwnerAndStop(t *testing.T) {
	const nodeName = "node-rdma-inv"
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Interfaces: []workercore.DeviceInterface{{
				Name:       "pf0",
				PciBusID:   "0000:3b:00.0",
				RDMA:       true,
				RDMADevice: "mlx5_pf0",
			}},
		},
	}
	rec, _ := newFakeReconciler(nodeName, devs)
	rec.RDMAInterfaceInventory = func() *device.RDMASnapshot {
		return &device.RDMASnapshot{
			Complete: true,
			Devices: []device.RDMADevice{
				{
					Name:  "mlx5_ext",
					Owner: device.RDMADeviceOwnerExternal,
				},
				{
					Name:            "mlx5_vf0",
					PciBusID:        "0000:3b:00.2",
					ParentPciBusID:  "0000:3b:00.0",
					VirtualFunction: true,
				},
				{
					Name:            "mlx5_orphan_vf",
					PciBusID:        "0000:4b:00.2",
					VirtualFunction: true,
				},
			},
		}
	}

	srv, err := NewRDMAServer(logr.Discard(), workercore.DeviceAllocationModePartitioned, rec)
	require.NoError(t, err)
	// Calling Stop before Start is a safe no-op.
	srv.Stop()

	sockDir := shortSocketDir(t)
	kubeSock, fakeKube := startFakeKubelet(t, sockDir)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx, kubeSock) }()

	select {
	case <-fakeKube.notifyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for RDMAServer registration")
	}

	dpSrv := srv.(kubeletdeviceplugin.DevicePluginServer)

	// Empty container requests and empty device IDs are rejected with InvalidArgument.
	_, err = dpSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{})
	require.Error(t, err)
	_, err = dpSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{}},
	})
	require.Error(t, err)

	// Synthesized VF attached to pf0 plus orphan VF resolve cleanly with zero host devices/mounts.
	resp, err := dpSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{"pf0:mlx5_vf0:0000", "mlx5_orphan_vf:mlx5_orphan_vf:0000"},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "=mlx5_orphan_vf,mlx5_vf0", resp.ContainerResponses[0].Envs["NCCL_IB_HCA"])
	assert.Empty(t, resp.ContainerResponses[0].Devices)
	assert.Empty(t, resp.ContainerResponses[0].Mounts)

	// Explicit srv.Stop() terminates serving cleanly.
	srv.Stop()
	require.NoError(t, <-errCh)
}
