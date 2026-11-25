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
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
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

func TestRDMAServer_SelectionPolicyWithholdingAndPinnedPortFormatting(t *testing.T) {
	const nodeName = "node-rdma-policy-mock"

	baseNode := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name:   nodeName,
			UID:    types.UID("uid-" + nodeName),
			Labels: map[string]string{"rdma-fabric": "ib"},
		},
	}

	baseDevs := &workercore.Devices{
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
					Name:         "ib0",
					PciBusID:     "0000:10:00.0",
					NumaAffinity: "0",
					RDMA:         true,
					RDMADevice:   "mlx5_0",
					Link:         &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
				},
				{
					Name:         "eth1",
					PciBusID:     "0000:11:00.0",
					NumaAffinity: "1",
					RDMA:         true,
					RDMADevice:   "mlx5_1",
					Link:         &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
				},
			},
		},
	}

	activePorts := []device.RDMAPort{
		{Number: 1, State: "ACTIVE", PhysicalState: "LinkUp"},
		{Number: 2, State: "ACTIVE", PhysicalState: "LinkUp"},
	}
	specYAML := `
rdmaDevices:
  - name: mlx5_0
    pciBusId: "0000:10:00.0"
    ports:
      - number: 1
        state: ACTIVE
        physicalState: LinkUp
      - number: 2
        state: ACTIVE
        physicalState: LinkUp
  - name: mlx5_1
    pciBusId: "0000:11:00.0"
    ports:
      - number: 1
        state: ACTIVE
        physicalState: LinkUp
      - number: 2
        state: ACTIVE
        physicalState: LinkUp
  - name: mlx5_ext
    pciBusId: "0000:12:00.0"
    owner: External
    ports:
      - number: 1
        state: ACTIVE
        physicalState: LinkUp
`

	cases := []struct {
		name            string
		mode            workercore.DeviceAllocationMode
		useSpecStoreInv bool
		policies        []workercore.DevicesRDMASelectionPolicy
		grantIDs        []string
		wantPreferred   []string
		wantNCCL        string
		wantRDMADevs    string
		wantMockDevs    string
		wantHealthByID  map[string]string
	}{
		{
			name:            "default no-policy selects owned HCAs and excludes external owner",
			mode:            workercore.DeviceAllocationModeExclusive,
			useSpecStoreInv: true,
			grantIDs:        []string{"ib0:ib0:0000", "eth1:eth1:0000"},
			wantPreferred:   []string{"ib0:ib0:0000"},
			wantNCCL:        "=mlx5_0,mlx5_1",
			wantRDMADevs:    "mlx5_0,mlx5_1",
			wantMockDevs:    "mlx5_0,mlx5_1",
			wantHealthByID: map[string]string{
				"ib0:ib0:0000":   kubeletdeviceplugin.Healthy,
				"eth1:eth1:0000": kubeletdeviceplugin.Healthy,
			},
		},
		{
			name: "policy include filters out unselected HCA in mixed grant and preferred allocation",
			mode: workercore.DeviceAllocationModeShared,
			policies: []workercore.DevicesRDMASelectionPolicy{{
				ObjectMeta: meta.ObjectMeta{Name: "select-mlx5-1", UID: "uid-pol-1", Generation: 1},
				Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"rdma-fabric": "ib"}},
					Include:      []string{"mlx5_1"},
				},
			}},
			grantIDs:      []string{"ib0:ib0:0000", "eth1:eth1:0000"},
			wantPreferred: []string{"eth1:eth1:0000"},
			wantNCCL:      "=mlx5_1",
			wantRDMADevs:  "mlx5_1",
			wantMockDevs:  "mlx5_1",
			wantHealthByID: map[string]string{
				"ib0:ib0:0000":   kubeletdeviceplugin.Unhealthy,
				"eth1:eth1:0000": kubeletdeviceplugin.Healthy,
			},
		},
		{
			name: "policy withholding every granted HCA returns empty grant envs without error",
			mode: workercore.DeviceAllocationModeExclusive,
			policies: []workercore.DevicesRDMASelectionPolicy{{
				ObjectMeta: meta.ObjectMeta{Name: "exclude-mlx5-0", UID: "uid-pol-2", Generation: 1},
				Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"rdma-fabric": "ib"}},
					Exclude:      []string{"mlx5_0"},
				},
			}},
			grantIDs:      []string{"ib0:ib0:0000"},
			wantPreferred: []string{"eth1:eth1:0000"},
			wantNCCL:      "",
			wantRDMADevs:  "",
			wantMockDevs:  "",
			wantHealthByID: map[string]string{
				"ib0:ib0:0000":   kubeletdeviceplugin.Unhealthy,
				"eth1:eth1:0000": kubeletdeviceplugin.Healthy,
			},
		},
		{
			name:            "pinned port formats NCCL_IB_HCA as name:port while keeping plain and mock lists bare",
			mode:            workercore.DeviceAllocationModeShared,
			useSpecStoreInv: true,
			policies: []workercore.DevicesRDMASelectionPolicy{{
				ObjectMeta: meta.ObjectMeta{Name: "pin-port-1", UID: "uid-pol-3", Generation: 1},
				Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"rdma-fabric": "ib"}},
					Include:      []string{"mlx5_0", "mlx5_1"},
					Port:         ptr.To(int32(1)),
				},
			}},
			grantIDs:      []string{"ib0:ib0:0000", "eth1:eth1:0000"},
			wantPreferred: []string{"ib0:ib0:0000"},
			wantNCCL:      "=mlx5_0:1,mlx5_1:1",
			wantRDMADevs:  "mlx5_0,mlx5_1",
			wantMockDevs:  "mlx5_0,mlx5_1",
			wantHealthByID: map[string]string{
				"ib0:ib0:0000":   kubeletdeviceplugin.Healthy,
				"eth1:eth1:0000": kubeletdeviceplugin.Healthy,
			},
		},
		{
			name: "exclude with pinned port 2 withholds excluded HCA and suffixes surviving HCA with port 2",
			mode: workercore.DeviceAllocationModeExclusive,
			policies: []workercore.DevicesRDMASelectionPolicy{{
				ObjectMeta: meta.ObjectMeta{Name: "exclude-0-port-2", UID: "uid-pol-4", Generation: 1},
				Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"rdma-fabric": "ib"}},
					Exclude:      []string{"mlx5_0"},
					Port:         ptr.To(int32(2)),
				},
			}},
			grantIDs:      []string{"ib0:ib0:0000", "eth1:eth1:0000"},
			wantPreferred: []string{"eth1:eth1:0000"},
			wantNCCL:      "=mlx5_1:2",
			wantRDMADevs:  "mlx5_1",
			wantMockDevs:  "mlx5_1",
			wantHealthByID: map[string]string{
				"ib0:ib0:0000":   kubeletdeviceplugin.Unhealthy,
				"eth1:eth1:0000": kubeletdeviceplugin.Healthy,
			},
		},
		{
			name: "conflicting policies fail closed withholding all HCAs",
			mode: workercore.DeviceAllocationModeExclusive,
			policies: []workercore.DevicesRDMASelectionPolicy{
				{
					ObjectMeta: meta.ObjectMeta{Name: "conflict-a", UID: "uid-pol-a", Generation: 1},
					Spec: workercore.DevicesRDMASelectionPolicySpec{
						NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"rdma-fabric": "ib"}},
						Include:      []string{"mlx5_0"},
					},
				},
				{
					ObjectMeta: meta.ObjectMeta{Name: "conflict-b", UID: "uid-pol-b", Generation: 1},
					Spec: workercore.DevicesRDMASelectionPolicySpec{
						NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"rdma-fabric": "ib"}},
						Include:      []string{"mlx5_1"},
					},
				},
			},
			grantIDs:      []string{"ib0:ib0:0000", "eth1:eth1:0000"},
			wantPreferred: nil,
			wantNCCL:      "",
			wantRDMADevs:  "",
			wantMockDevs:  "",
			wantHealthByID: map[string]string{
				"ib0:ib0:0000":   kubeletdeviceplugin.Unhealthy,
				"eth1:eth1:0000": kubeletdeviceplugin.Unhealthy,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []ctrlcli.Object{baseNode.DeepCopy(), baseDevs.DeepCopy()}
			for i := range tc.policies {
				p := tc.policies[i].DeepCopy()
				objs = append(objs, p)
			}
			rec, cli := newFakeReconciler(nodeName, objs...)

			invFn := func() *device.RDMASnapshot {
				return &device.RDMASnapshot{
					Revision: "rev-1",
					Complete: true,
					Devices: []device.RDMADevice{
						{Name: "mlx5_0", PciBusID: "0000:10:00.0", Ports: activePorts},
						{Name: "mlx5_1", PciBusID: "0000:11:00.0", Ports: activePorts},
						{Name: "mlx5_ext", PciBusID: "0000:12:00.0", Owner: device.RDMADeviceOwnerExternal, Ports: activePorts},
					},
				}
			}

			var mockOpts []Option
			if tc.useSpecStoreInv {
				specPath := filepath.Join(t.TempDir(), "rdma-policy.yaml")
				require.NoError(t, os.WriteFile(specPath, []byte(specYAML), 0o600))
				store, err := mockspec.NewStore(specPath)
				require.NoError(t, err)
				mockOpts = append(mockOpts, WithSpecStore(store, nodeName))
			} else {
				rec.RDMAInterfaceInventory = invFn
			}

			obs := &deviceplugin.RDMASelectionObserver{
				NodeName:               nodeName,
				Client:                 cli,
				RDMAInterfaceInventory: invFn,
				Reconciler:             rec,
			}
			_, err := obs.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			})
			require.NoError(t, err)

			srv, err := NewRDMAServer(logr.Discard(), tc.mode, rec, mockOpts...)
			require.NoError(t, err)
			dpSrv := srv.(kubeletdeviceplugin.DevicePluginServer)

			// Verify ListAndWatch health marks reflect policy eligibility.
			lwCtx, lwCancel := context.WithCancel(context.Background())
			sent := make(chan *kubeletdeviceplugin.ListAndWatchResponse, 2)
			lwDone := make(chan error, 1)
			go func() {
				lwDone <- dpSrv.ListAndWatch(&kubeletdeviceplugin.Empty{}, &mockListAndWatchStream{
					ctx:  lwCtx,
					sent: sent,
				})
			}()
			var lwResp *kubeletdeviceplugin.ListAndWatchResponse
			select {
			case lwResp = <-sent:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for initial ListAndWatch response")
			}
			lwCancel()
			<-lwDone

			gotHealth := make(map[string]string, len(lwResp.Devices))
			for _, d := range lwResp.Devices {
				gotHealth[d.ID] = d.Health
			}
			for id, wantHealth := range tc.wantHealthByID {
				assert.Equal(t, wantHealth, gotHealth[id], "device %s health", id)
			}

			// Verify GetPreferredAllocation honors policy eligibility.
			prefResp, err := dpSrv.GetPreferredAllocation(context.Background(), &kubeletdeviceplugin.PreferredAllocationRequest{
				ContainerRequests: []*kubeletdeviceplugin.ContainerPreferredAllocationRequest{{
					AvailableDeviceIDs: []string{"ib0:ib0:0000", "eth1:eth1:0000"},
					AllocationSize:     1,
				}},
			})
			require.NoError(t, err)
			require.Len(t, prefResp.ContainerResponses, 1)
			assert.Equal(t, tc.wantPreferred, prefResp.ContainerResponses[0].DeviceIDs)

			// Verify Allocate enforces isHCAEligible, formats pinned-port NCCL_IB_HCA, and injects zero host devices/mounts.
			allocResp, err := dpSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
				ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
					DevicesIds: tc.grantIDs,
				}},
			})
			require.NoError(t, err)
			require.Len(t, allocResp.ContainerResponses, 1)
			ctr := allocResp.ContainerResponses[0]
			assert.Empty(t, ctr.Devices, "mock RDMA must inject zero /dev/infiniband/* device nodes")
			assert.Empty(t, ctr.Mounts, "mock RDMA must inject zero host mounts")
			assert.Equal(t, map[string]string{
				"NCCL_IB_HCA":           tc.wantNCCL,
				"GPUSTACK_RDMA_DEVICES": tc.wantRDMADevs,
				EnvMockAllocatedDevices: tc.wantMockDevs,
				EnvMockAllocationMode:   device.PreflightModeOf(tc.mode),
				EnvMockManufacturer:     RDMAFamilyName,
			}, ctr.Envs)
		})
	}
}

type mockListAndWatchStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan<- *kubeletdeviceplugin.ListAndWatchResponse
}

func (m *mockListAndWatchStream) Context() context.Context {
	return m.ctx
}

func (m *mockListAndWatchStream) Send(resp *kubeletdeviceplugin.ListAndWatchResponse) error {
	select {
	case m.sent <- resp:
		return nil
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
}
