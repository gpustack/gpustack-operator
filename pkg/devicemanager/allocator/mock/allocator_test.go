package mock

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mockalloc-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type fakeKubeletRegistration struct {
	kubeletdeviceplugin.UnimplementedRegistrationServer

	mu       sync.Mutex
	requests []*kubeletdeviceplugin.RegisterRequest
	notifyCh chan *kubeletdeviceplugin.RegisterRequest
}

func startFakeKubelet(t *testing.T, dir string) (string, *fakeKubeletRegistration) {
	t.Helper()
	sock := filepath.Join(dir, "kubelet.sock")
	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)

	reg := &fakeKubeletRegistration{
		notifyCh: make(chan *kubeletdeviceplugin.RegisterRequest, 32),
	}
	srv := grpc.NewServer()
	kubeletdeviceplugin.RegisterRegistrationServer(srv, reg)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return sock, reg
}

func (f *fakeKubeletRegistration) Register(
	_ context.Context, req *kubeletdeviceplugin.RegisterRequest,
) (*kubeletdeviceplugin.Empty, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	select {
	case f.notifyCh <- req:
	default:
	}
	return &kubeletdeviceplugin.Empty{}, nil
}

func (f *fakeKubeletRegistration) registeredResources() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.requests))
	for _, r := range f.requests {
		out[r.ResourceName] = r.Endpoint
	}
	return out
}

func newFakeReconciler(nodeName string, objs ...ctrlcli.Object) (*deviceplugin.DevicesReconciler, ctrlcli.Client) {
	tracker := k8stesting.NewObjectTracker(
		scheme.Scheme, serializer.NewCodecFactory(scheme.Scheme).UniversalDecoder(),
	)
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjectTracker(tracker).
		WithStatusSubresource(&workercore.Devices{}).
		WithObjects(objs...).
		WithIndex(&core.Pod{}, deviceplugin.IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()
	return &deviceplugin.DevicesReconciler{
		NodeName: nodeName,
		Client:   cli,
	}, cli
}

func TestVisibleDevicesEnv(t *testing.T) {
	cases := []struct {
		manufacturer string
		want         string
	}{
		{manufacturer: nodefeature.ManufacturerNVIDIA, want: "NVIDIA_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerAMD, want: "ROCR_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerAscend, want: "ASCEND_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerCambricon, want: "CAMBRICON_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerHygon, want: "HIP_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerIluvatar, want: "IX_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerMetaX, want: "MACA_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerMThreads, want: "MTHREADS_VISIBLE_DEVICES"},
		{manufacturer: nodefeature.ManufacturerTHead, want: "PPU_VISIBLE_DEVICES"},
		{manufacturer: "custom", want: "CUSTOM_VISIBLE_DEVICES"},
	}

	for _, c := range cases {
		t.Run(c.manufacturer, func(t *testing.T) {
			assert.Equal(t, c.want, VisibleDevicesEnv(c.manufacturer))
		})
	}
}

func TestNewAllocator_ServerModes(t *testing.T) {
	cases := []struct {
		name         string
		manufacturer string
		opts         device.AllocatorOptions
		wantModes    []workercore.DeviceAllocationMode
		wantPart     bool
	}{
		{
			name:         "nvidia enables all 5 modes including partitioned by default",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: true,
		},
		{
			name:         "hygon enables all 5 modes including partitioned by default",
			manufacturer: nodefeature.ManufacturerHygon,
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: true,
		},
		{
			name:         "thead enables all 5 modes including partitioned by default",
			manufacturer: nodefeature.ManufacturerTHead,
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: true,
		},
		{
			name:         "amd skips partitioned because amd has no partition kind",
			manufacturer: nodefeature.ManufacturerAMD,
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: false,
		},
		{
			name:         "ascend skips partitioned because ascend has no partition kind",
			manufacturer: nodefeature.ManufacturerAscend,
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: false,
		},
		{
			name:         "nvidia with NoShared drops shared only",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			opts:         device.AllocatorOptions{NoShared: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: true,
		},
		{
			name:         "nvidia with NoSliced drops sliced only",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			opts:         device.AllocatorOptions{NoSliced: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: true,
		},
		{
			name:         "nvidia with NoPartitioned drops partitioned and leaves store nil when unconfigured",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			opts:         device.AllocatorOptions{NoPartitioned: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: false,
		},
		{
			name:         "nvidia with all optional flags disabled keeps exclusive and visibility",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			opts:         device.AllocatorOptions{NoShared: true, NoSliced: true, NoPartitioned: true},
			wantModes: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeVisibility,
			},
			wantPart: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := newFakeReconciler("node-0")
			alloc := NewAllocator(c.manufacturer, c.opts, WithReconciler(rec))
			assert.Equal(t, c.manufacturer, alloc.Name())
			assert.Equal(t, c.wantModes, alloc.Modes())
			assert.Len(t, alloc.Servers(), len(c.wantModes))
			if c.wantPart {
				assert.NotNil(t, alloc.PartitionStore())
			} else {
				assert.Nil(t, alloc.PartitionStore())
			}
		})
	}
}

func TestSupportedAllocatorCreators(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "mock.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte("preset: nvidia-h100\ncount: 2\n"), 0o600))
	specStore, err := mockspec.NewStore(specPath)
	require.NoError(t, err)

	sharedPartitions := NewPartitionStore()
	rec, _ := newFakeReconciler("node-creators")
	creators := SupportedAllocatorCreators(
		WithReconciler(rec),
		WithPartitionStore(sharedPartitions),
		WithSpecStore(specStore, "node-creators"),
	)

	known := nodefeature.GetKnownAcceleratableManufacturers()
	require.Len(t, creators, len(known))
	for _, mfr := range known {
		creator := creators[mfr]
		require.NotNil(t, creator, "missing creator for %s", mfr)
		a := creator(device.AllocatorOptions{Logger: logr.Discard()})
		assert.Equal(t, mfr, a.Name())
		typed, ok := a.(*Allocator)
		require.True(t, ok)
		assert.Same(t, sharedPartitions, typed.PartitionStore())
		assert.Same(t, specStore, typed.SpecStore())
	}
}

func TestAllocator_AllManufacturers_AllocateZeroHostDevicesAndMounts(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
	}

	for _, mfr := range nodefeature.GetKnownAcceleratableManufacturers() {
		for _, mode := range modes {
			t.Run(mfr+"_"+mode.String(), func(t *testing.T) {
				const nodeName = "node-mfr"
				devs := &workercore.Devices{
					ObjectMeta: meta.ObjectMeta{Name: nodeName},
					Spec: workercore.DevicesSpec{
						Groups: []workercore.DevicesGroup{
							{
								ID:           mfr + "-grp-b",
								Manufacturer: mfr,
								Name:         "Mock Model B",
								Memory:       32768,
								Accelerators: []workercore.Accelerator{
									{
										ID:              mfr + "-card-2",
										Index:           2,
										PhysicalIndexes: []uint32{2, 130, 0},
										Status: workercore.AcceleratorStatus{
											LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4},
										},
									},
								},
							},
							{
								ID:           mfr + "-grp-a",
								Manufacturer: mfr,
								Name:         "Mock Model A",
								Memory:       65536,
								Accelerators: []workercore.Accelerator{
									{
										ID:              mfr + "-card-0",
										Index:           0,
										PhysicalIndexes: []uint32{0, 128, 0},
										Status: workercore.AcceleratorStatus{
											LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4},
										},
									},
								},
							},
						},
					},
				}

				resName := nodefeature.GetAcceleratableResourceName(mfr, mode)
				pod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{
						Name:      "pod-main",
						Namespace: "default",
						UID:       types.UID("uid-" + mfr + "-" + mode.String()),
					},
					Spec: core.PodSpec{
						NodeName: nodeName,
						Containers: []core.Container{
							{
								Name: "main",
								Resources: core.ResourceRequirements{
									Limits: core.ResourceList{
										resName: resource.MustParse("2"),
									},
								},
							},
							{
								Name: "sidecar",
								Resources: core.ResourceRequirements{
									Limits: core.ResourceList{
										nodefeature.GetAcceleratableResourceName(mfr, workercore.DeviceAllocationModeVisibility): resource.MustParse("2"),
									},
								},
							},
						},
					},
				}

				rec, _ := newFakeReconciler(nodeName, devs, pod)
				alloc := NewAllocator(mfr, device.AllocatorOptions{Logger: logr.Discard()}, WithReconciler(rec))

				var modeSrv, visSrv kubeletdeviceplugin.DevicePluginServer
				for i, m := range alloc.Modes() {
					if m == mode {
						modeSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
					}
					if m == workercore.DeviceAllocationModeVisibility {
						visSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
					}
				}
				require.NotNil(t, modeSrv)
				require.NotNil(t, visSrv)

				// Allocate in reverse group order to verify AllocatedAccelerators index sorting (card-0 before card-2).
				token0 := deviceplugin.Resource{Group: mfr + "-grp-a", Device: mfr + "-card-0"}.DeviceIDs(mode, 4)[0]
				token2 := deviceplugin.Resource{Group: mfr + "-grp-b", Device: mfr + "-card-2"}.DeviceIDs(mode, 4)[0]

				resp, err := modeSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
					ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
						DevicesIds: []string{token2, token0},
					}},
				})
				require.NoError(t, err)
				require.Len(t, resp.ContainerResponses, 1)

				ctrResp := resp.ContainerResponses[0]
				assert.Empty(t, ctrResp.Devices, "must mount zero /dev/* host devices")
				assert.Empty(t, ctrResp.Mounts, "must mount zero ld.so.preload or host paths")
				assert.Empty(t, ctrResp.Annotations)
				assert.Empty(t, ctrResp.CdiDevices)

				wantIDs := mfr + "-card-0," + mfr + "-card-2"
				assert.Equal(t, wantIDs, ctrResp.Envs[EnvMockAllocatedDevices])
				assert.Equal(t, device.PreflightModeOf(mode), ctrResp.Envs[EnvMockAllocationMode])
				assert.Equal(t, mfr, ctrResp.Envs[EnvMockManufacturer])
				assert.Equal(t, wantIDs, ctrResp.Envs[VisibleDevicesEnv(mfr)])

				// Now co-allocate via the visibility server on the same Pod.
				visTokens := deviceplugin.Resource{Group: mfr + "-grp-a", Device: mfr + "-card-0"}.DeviceIDs(
					workercore.DeviceAllocationModeVisibility, 0,
				)[:2]
				visResp, err := visSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
					ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
						DevicesIds: visTokens,
					}},
				})
				require.NoError(t, err)
				require.Len(t, visResp.ContainerResponses, 1)

				visCtrResp := visResp.ContainerResponses[0]
				assert.Empty(t, visCtrResp.Devices, "visibility must mount zero /dev/* host devices")
				assert.Empty(t, visCtrResp.Mounts, "visibility must mount zero ld.so.preload or host paths")
				assert.Equal(t, wantIDs, visCtrResp.Envs[EnvMockAllocatedDevices])
				assert.Equal(t, "visibility", visCtrResp.Envs[EnvMockAllocationMode])
				assert.Equal(t, mfr, visCtrResp.Envs[EnvMockManufacturer])
				assert.Equal(t, wantIDs, visCtrResp.Envs[VisibleDevicesEnv(mfr)])
			})
		}
	}
}

func TestServer_GetContainerAllocateResponse_Refusals(t *testing.T) {
	rec, _ := newFakeReconciler("node-err")
	alloc := NewAllocator(nodefeature.ManufacturerNVIDIA, device.AllocatorOptions{Logger: logr.Discard()}, WithReconciler(rec))
	responder := alloc.Servers()[0].(deviceplugin.ContainerAllocateResponder)

	validDevs := &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{
					{ID: "GPU-0", Index: 0},
					{ID: "", Index: 1},
				},
			}},
		},
	}

	cases := []struct {
		name      string
		devs      *workercore.Devices
		allocated map[deviceplugin.Resource]int32
		wantErr   string
	}{
		{
			name:      "nil devices inventory",
			devs:      nil,
			allocated: map[deviceplugin.Resource]int32{{Group: "grp-0", Device: "GPU-0"}: 1},
			wantErr:   "devices inventory is nil",
		},
		{
			name:      "empty allocated map",
			devs:      validDevs,
			allocated: nil,
			wantErr:   "no allocated accelerator found",
		},
		{
			name: "missing accelerator from inventory",
			devs: validDevs,
			allocated: map[deviceplugin.Resource]int32{
				{Group: "grp-0", Device: "GPU-0"}:       1,
				{Group: "grp-0", Device: "GPU-missing"}: 1,
			},
			wantErr: "resolved 1 of 2",
		},
		{
			name: "accelerator with empty id",
			devs: validDevs,
			allocated: map[deviceplugin.Resource]int32{
				{Group: "grp-0", Device: ""}: 1,
			},
			wantErr: "empty id",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := responder.GetContainerAllocateResponse(
				context.Background(),
				&core.Pod{},
				&core.Container{Name: "main"},
				c.devs,
				c.allocated,
			)
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.ErrorContains(t, err, c.wantErr)
		})
	}
}

func TestAllocator_LifecycleAndSocketRegistration(t *testing.T) {
	sockDir := shortSocketDir(t)
	kubeSock, fakeKube := startFakeKubelet(t, sockDir)

	const nodeName = "node-serve"
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "nvidia-h100",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Name:         "NVIDIA H100",
				Memory:       81559,
				Accelerators: []workercore.Accelerator{{
					ID:    "GPU-h100-0",
					Index: 0,
					Status: workercore.AcceleratorStatus{
						LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4},
					},
				}},
			}},
		},
	}
	rec, _ := newFakeReconciler(nodeName, devs)

	alloc := AllocatorCreator(nodefeature.ManufacturerNVIDIA, WithReconciler(rec))(device.AllocatorOptions{
		Logger:     logr.Discard(),
		KubeSocket: kubeSock,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- alloc.Start(ctx)
	}()

	// Expect all 5 NVIDIA resources to register with kubelet.
	for range 5 {
		select {
		case <-fakeKube.notifyCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for mock allocator servers to register with kubelet")
		}
	}

	registered := fakeKube.registeredResources()
	assert.Contains(t, registered, "nvidia.com/gpu")
	assert.Contains(t, registered, "nvidia.com/gpu.shared")
	assert.Contains(t, registered, "nvidia.com/gpu.sliced")
	assert.Contains(t, registered, "nvidia.com/gpu.partitioned")
	assert.Contains(t, registered, "device.gpustack.ai/nvidia.visibility")

	// Dial the registered exclusive socket and verify ListAndWatch over real gRPC.
	exSock := filepath.Join(sockDir, registered["nvidia.com/gpu"])
	conn, err := grpc.NewClient("unix://"+exSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	dpCli := kubeletdeviceplugin.NewDevicePluginClient(conn)
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	lw, err := dpCli.ListAndWatch(streamCtx, &kubeletdeviceplugin.Empty{})
	require.NoError(t, err)
	lwResp, err := lw.Recv()
	require.NoError(t, err)
	require.Len(t, lwResp.Devices, 1)
	assert.Equal(t, "nvidia-h100:GPU-h100-0:0000", lwResp.Devices[0].ID)
	assert.Equal(t, kubeletdeviceplugin.Healthy, lwResp.Devices[0].Health)

	// Concurrent Stop calls must be race-safe and idempotent, causing Start to return nil cleanly.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			alloc.Stop()
		}()
	}
	wg.Wait()

	select {
	case startErr := <-errCh:
		require.NoError(t, startErr)
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	// Calling Start after Stop must be a safe no-op returning nil.
	require.NoError(t, alloc.Start(context.Background()))
}
