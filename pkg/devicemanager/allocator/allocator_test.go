package allocator

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	allocatormock "gpustack.ai/gpustack/pkg/devicemanager/allocator/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dmalloc-*")
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
		notifyCh: make(chan *kubeletdeviceplugin.RegisterRequest, 64),
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

func newFakeReconciler(nodeName string, objs ...ctrlcli.Object) *deviceplugin.DevicesReconciler {
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
	}
}

func writeTempMockFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mock.yaml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestOptions_MockDevicesFile(t *testing.T) {
	sockDir := shortSocketDir(t)
	kubeSock, _ := startFakeKubelet(t, sockDir)
	validSpec := writeTempMockFile(t, "preset: nvidia-h100\ncount: 1\n")
	missingSpec := filepath.Join(t.TempDir(), "missing.yaml")

	cases := []struct {
		name             string
		envVal           string
		withoutMockFlags bool
		args             []string
		wantErr          string
		wantMockStore    bool
	}{
		{
			name:          "default disabled",
			args:          []string{"--kube-socket=" + kubeSock},
			wantMockStore: false,
		},
		{
			name:          "env var enables mock mode",
			envVal:        validSpec,
			args:          []string{"--kube-socket=" + kubeSock},
			wantMockStore: true,
		},
		{
			name:          "cli flag overrides invalid env var",
			envVal:        missingSpec,
			args:          []string{"--kube-socket=" + kubeSock, "--mock-devices-file=" + validSpec},
			wantMockStore: true,
		},
		{
			name:    "invalid mock file fails validation",
			args:    []string{"--kube-socket=" + kubeSock, "--mock-devices-file=" + missingSpec},
			wantErr: "--mock-devices-file",
		},
		{
			name:             "WithoutMockDevicesOptions avoids duplicate flag registration on shared FlagSet",
			envVal:           missingSpec,
			withoutMockFlags: true,
			args:             []string{"--kube-socket=" + kubeSock, "--mock-devices-file=" + validSpec},
			wantMockStore:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(MockDevicesFileEnv, tc.envVal)
			opts := NewOptions()
			fs := pflag.NewFlagSet("allocator-test", pflag.ContinueOnError)
			var externalMock string
			if tc.withoutMockFlags {
				fs.StringVar(&externalMock, "mock-devices-file", "", "external mock flag")
				opts.AddFlags(fs, WithoutMockDevicesOptions())
			} else {
				opts.AddFlags(fs)
			}
			require.NoError(t, fs.Parse(tc.args))

			err := opts.Validate(context.Background())
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			cfg, err := opts.Complete(context.Background())
			require.NoError(t, err)
			if tc.wantMockStore {
				assert.NotNil(t, cfg.MockSpecStore)
				assert.NotNil(t, cfg.MockPartitionStore)
			} else {
				assert.Nil(t, cfg.MockSpecStore)
				assert.Nil(t, cfg.MockPartitionStore)
			}
		})
	}
}

func TestNew_CreatorSelection(t *testing.T) {
	specPath := writeTempMockFile(t, "preset: nvidia-h100-mig\ncount: 1\n")
	specStore, err := mockspec.NewStore(specPath)
	require.NoError(t, err)
	partStore := allocatormock.NewPartitionStore()
	rec := newFakeReconciler("node-select")

	t.Run("default disabled keeps production creators with zero mock state", func(t *testing.T) {
		a, err := New(&Config{})
		require.NoError(t, err)
		assert.Equal(t, len(supportedAllocatorCreators), len(a.allocatorCreators))
		assert.Equal(t, rdmaAllocatorCreator == nil, a.rdmaCreator == nil)
	})

	t.Run("mock enabled wires all known manufacturers and mock RDMA creator", func(t *testing.T) {
		a, err := New(&Config{
			MockSpecStore:      specStore,
			MockPartitionStore: partStore,
			MockOptions:        []allocatormock.Option{allocatormock.WithReconciler(rec)},
		})
		require.NoError(t, err)

		known := nodefeature.GetKnownAcceleratableManufacturers()
		require.Len(t, a.allocatorCreators, len(known))
		for _, mfr := range known {
			require.NotNil(t, a.allocatorCreators[mfr], "missing mock creator for %s", mfr)
		}
		require.NotNil(t, a.rdmaCreator, "mock RDMA creator must be active on all platforms")
	})
}

func TestAllocator_MockStartAndManufacturerWakeup(t *testing.T) {
	const nodeName = "node-mock-alloc"
	t.Setenv("KUBERNETES_NODE_NAME", nodeName)

	sockDir := shortSocketDir(t)
	kubeSock, fakeKube := startFakeKubelet(t, sockDir)

	specPath := writeTempMockFile(t, `
manufacturers:
  nvidia:
    preset: nvidia-h100-mig
    count: 1
rdmaDevices:
  - name: mlx5_0
    driver: mlx5_core
    pciBusId: "0000:10:00.0"
    numaAffinity: "0"
    ports:
      - number: 1
        netDevs: ["eth0"]
        linkLayer: InfiniBand
        state: ACTIVE
`)
	specStore, err := mockspec.NewStore(specPath)
	require.NoError(t, err)
	partStore := allocatormock.NewPartitionStore()

	resolved := specStore.Current().Document.ResolveNode(nodeName)
	groups := resolved.Manufacturers["nvidia"].BuildGroups()
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Accelerators, 1)
	grpID := groups[0].ID
	cardID := groups[0].Accelerators[0].ID
	prof := groups[0].Accelerators[0].Status.PhysicalSliced.Profiles[0]

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: groups,
			Interfaces: []workercore.DeviceInterface{{
				Name:       "eth0",
				PciBusID:   "0000:10:00.0",
				Up:         true,
				RDMA:       true,
				RDMADevice: "mlx5_0",
				Link:       &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
			}},
		},
	}

	partResName := nodefeature.GetAcceleratableResourceName("nvidia", workercore.DeviceAllocationModePartitioned)
	profResName := nodefeature.GetAcceleratablePartitionedProfileResourceName("nvidia", prof.Name)
	qty := resource.MustParse("1")
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "pod-mig",
			Namespace: "default",
			UID:       types.UID("uid-pod-mig"),
		},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "main",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						partResName: qty,
						profResName: qty,
					},
				},
			}},
		},
	}

	rec := newFakeReconciler(nodeName, devs, pod)
	manusCh := make(chan sets.Set[string], 4)

	alc, err := New(&Config{
		KubeSocket:              kubeSock,
		DetectedManufacturersCh: manusCh,
		MockSpecStore:           specStore,
		MockPartitionStore:      partStore,
		MockOptions:             []allocatormock.Option{allocatormock.WithReconciler(rec)},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- alc.Start(ctx)
	}()

	// 1. Mock RDMA registers all 3 modes immediately before any manufacturer is published.
	for range 3 {
		select {
		case <-fakeKube.notifyCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for mock RDMA server registration")
		}
	}
	registered := fakeKube.registeredResources()
	assert.Contains(t, registered, "device.gpustack.ai/rdma")
	assert.Contains(t, registered, "device.gpustack.ai/rdma.shared")
	assert.Contains(t, registered, "device.gpustack.ai/rdma.partitioned")

	// Verify mock RDMA Allocate over gRPC returns zero host devices/mounts.
	rdmaSock := filepath.Join(sockDir, registered["device.gpustack.ai/rdma"])
	rdmaConn, err := grpc.NewClient("unix://"+rdmaSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rdmaConn.Close() })
	rdmaToken := deviceplugin.Resource{Group: "eth0", Device: "eth0"}.DeviceIDs(
		workercore.DeviceAllocationModeExclusive, 0,
	)[0]
	rdmaResp, err := kubeletdeviceplugin.NewDevicePluginClient(rdmaConn).Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{rdmaToken},
		}},
	})
	require.NoError(t, err)
	require.Len(t, rdmaResp.ContainerResponses, 1)
	assert.Empty(t, rdmaResp.ContainerResponses[0].Devices)
	assert.Empty(t, rdmaResp.ContainerResponses[0].Mounts)
	assert.Equal(t, "mlx5_0", rdmaResp.ContainerResponses[0].Envs["GPUSTACK_RDMA_DEVICES"])

	// 2. Wake up NVIDIA via detectedManufacturersCh and verify caller's set is not mutated.
	sentSet := sets.New("nvidia")
	manusCh <- sentSet

	for range 5 {
		select {
		case <-fakeKube.notifyCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for mock NVIDIA server registration")
		}
	}
	registered = fakeKube.registeredResources()
	assert.Contains(t, registered, "nvidia.com/gpu")
	assert.Contains(t, registered, "nvidia.com/gpu.shared")
	assert.Contains(t, registered, "nvidia.com/gpu.sliced")
	assert.Contains(t, registered, "nvidia.com/gpu.partitioned")
	assert.Contains(t, registered, "device.gpustack.ai/nvidia.visibility")

	// Re-sending the same manufacturer keeps it running without mutating caller's set.
	manusCh <- sentSet
	assert.True(t, sentSet.Has("nvidia"))

	// Allocate a virtual partition through the registered partitioned gRPC socket and confirm shared PartitionStore updates.
	partSock := filepath.Join(sockDir, registered["nvidia.com/gpu.partitioned"])
	partConn, err := grpc.NewClient("unix://"+partSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = partConn.Close() })
	partToken := deviceplugin.Resource{Group: grpID, Device: cardID}.DeviceIDs(
		workercore.DeviceAllocationModePartitioned, groups[0].Accelerators[0].Status.PhysicalSliced.Count,
	)[0]
	partResp, err := kubeletdeviceplugin.NewDevicePluginClient(partConn).Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{partToken},
		}},
	})
	require.NoError(t, err)
	require.Len(t, partResp.ContainerResponses, 1)
	assert.Empty(t, partResp.ContainerResponses[0].Devices)
	assert.Empty(t, partResp.ContainerResponses[0].Mounts)
	assert.Equal(t, 1, partStore.Count())

	// 3. Undetecting nvidia stops its servers cleanly; canceling ctx ends Start.
	manusCh <- sets.New[string]()
	cancel()

	select {
	case startErr := <-errCh:
		assert.ErrorIs(t, startErr, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Allocator.Start did not return after context cancellation")
	}
}
