package devicemanager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	allocatormock "gpustack.ai/gpustack/pkg/devicemanager/allocator/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/devicemanager/kuberess"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/varx"
)

type mockIntegrationKubelet struct {
	kubeletdeviceplugin.UnimplementedRegistrationServer

	mu       sync.Mutex
	requests []*kubeletdeviceplugin.RegisterRequest
	notifyCh chan *kubeletdeviceplugin.RegisterRequest
}

func startMockIntegrationKubelet(t *testing.T, dir string) (string, *mockIntegrationKubelet, func()) {
	t.Helper()
	sock := filepath.Join(dir, "kubelet.sock")
	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)

	reg := &mockIntegrationKubelet{
		notifyCh: make(chan *kubeletdeviceplugin.RegisterRequest, 64),
	}
	srv := grpc.NewServer()
	kubeletdeviceplugin.RegisterRegistrationServer(srv, reg)
	go func() { _ = srv.Serve(lis) }()

	var stopOnce sync.Once
	stopFn := func() {
		stopOnce.Do(func() {
			srv.Stop()
			_ = lis.Close()
		})
	}
	t.Cleanup(stopFn)
	return sock, reg, stopFn
}

func (k *mockIntegrationKubelet) Register(
	_ context.Context, req *kubeletdeviceplugin.RegisterRequest,
) (*kubeletdeviceplugin.Empty, error) {
	k.mu.Lock()
	k.requests = append(k.requests, req)
	k.mu.Unlock()
	select {
	case k.notifyCh <- req:
	default:
	}
	return &kubeletdeviceplugin.Empty{}, nil
}

func (k *mockIntegrationKubelet) registeredResources() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]string, len(k.requests))
	for _, r := range k.requests {
		out[r.ResourceName] = r.Endpoint
	}
	return out
}

func shortIntegrationSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dmmock-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func atomicWriteSpec(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".next"
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func recvInitialListAndWatch(
	t *testing.T,
	ctx context.Context,
	cli kubeletdeviceplugin.DevicePluginClient,
) []*kubeletdeviceplugin.Device {
	t.Helper()
	lwCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream, err := cli.ListAndWatch(lwCtx, &kubeletdeviceplugin.Empty{})
	require.NoError(t, err)
	resp, err := stream.Recv()
	require.NoError(t, err)
	return resp.GetDevices()
}

func snapshotGoroutineIDs() sets.Set[string] {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	ids := sets.New[string]()
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		firstLine, _, _ := strings.Cut(block, "\n")
		if id, ok := parseGoroutineID(firstLine); ok {
			ids.Insert(id)
		}
	}
	return ids
}

func leakedModuleGoroutines(baseline sets.Set[string]) []string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	var leaked []string
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		firstLine, _, _ := strings.Cut(block, "\n")
		id, ok := parseGoroutineID(firstLine)
		if !ok || baseline.Has(id) || strings.Contains(block, "leakedModuleGoroutines") {
			continue
		}
		if strings.Contains(block, "gpustack.ai/gpustack/pkg/devicemanager") ||
			strings.Contains(block, "gpustack.ai/gpustack/pkg/deviceplugin") {
			leaked = append(leaked, block)
		}
	}
	return leaked
}

func parseGoroutineID(header string) (string, bool) {
	const prefix = "goroutine "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	rest := header[len(prefix):]
	id, _, ok := strings.Cut(rest, " ")
	return id, ok && id != ""
}

func TestConfig_WireMockStores(t *testing.T) {
	validSpec := filepath.Join(t.TempDir(), "mock.yaml")
	require.NoError(t, os.WriteFile(validSpec, []byte("preset: nvidia-h100\ncount: 1\n"), 0o600))
	missingSpec := filepath.Join(t.TempDir(), "missing.yaml")

	preStore, err := mockspec.NewStore(validSpec)
	require.NoError(t, err)
	prePart := allocatormock.NewPartitionStore()
	customProv := allocatormock.NewPartitionStore()

	cases := []struct {
		name    string
		build   func() *Config
		wantErr bool
		verify  func(t *testing.T, cfg *Config)
	}{
		{
			name: "nil subconfigs are safe no-ops",
			build: func() *Config {
				return &Config{}
			},
			verify: func(t *testing.T, cfg *Config) {
				assert.Nil(t, cfg.DetectorConfig)
				assert.Nil(t, cfg.AllocatorConfig)
			},
		},
		{
			name: "disabled mock mode leaves stores nil",
			build: func() *Config {
				return &Config{
					DetectorConfig:  &detector.Config{},
					AllocatorConfig: &allocator.Config{},
				}
			},
			verify: func(t *testing.T, cfg *Config) {
				assert.Nil(t, cfg.DetectorConfig.MockSpecStore)
				assert.Nil(t, cfg.DetectorConfig.MockPartitionProvider)
				assert.Nil(t, cfg.AllocatorConfig.MockSpecStore)
				assert.Nil(t, cfg.AllocatorConfig.MockPartitionStore)
			},
		},
		{
			name: "mock file on detector config creates and shares spec and partition stores",
			build: func() *Config {
				return &Config{
					DetectorConfig:  &detector.Config{MockDevicesFile: validSpec},
					AllocatorConfig: &allocator.Config{},
				}
			},
			verify: func(t *testing.T, cfg *Config) {
				require.NotNil(t, cfg.DetectorConfig.MockSpecStore)
				assert.Same(t, cfg.DetectorConfig.MockSpecStore, cfg.AllocatorConfig.MockSpecStore)
				require.NotNil(t, cfg.AllocatorConfig.MockPartitionStore)
				assert.Same(t, cfg.AllocatorConfig.MockPartitionStore, cfg.DetectorConfig.MockPartitionProvider)
			},
		},
		{
			name: "mock file on allocator config creates and shares stores",
			build: func() *Config {
				return &Config{
					DetectorConfig:  &detector.Config{},
					AllocatorConfig: &allocator.Config{MockDevicesFile: validSpec},
				}
			},
			verify: func(t *testing.T, cfg *Config) {
				require.NotNil(t, cfg.DetectorConfig.MockSpecStore)
				assert.Same(t, cfg.DetectorConfig.MockSpecStore, cfg.AllocatorConfig.MockSpecStore)
				require.NotNil(t, cfg.AllocatorConfig.MockPartitionStore)
				assert.Same(t, cfg.AllocatorConfig.MockPartitionStore, cfg.DetectorConfig.MockPartitionProvider)
			},
		},
		{
			name: "pre-populated stores and custom partition provider are preserved",
			build: func() *Config {
				return &Config{
					DetectorConfig: &detector.Config{
						MockPartitionProvider: customProv,
					},
					AllocatorConfig: &allocator.Config{
						MockSpecStore:      preStore,
						MockPartitionStore: prePart,
					},
				}
			},
			verify: func(t *testing.T, cfg *Config) {
				assert.Same(t, preStore, cfg.DetectorConfig.MockSpecStore)
				assert.Same(t, preStore, cfg.AllocatorConfig.MockSpecStore)
				assert.Same(t, prePart, cfg.AllocatorConfig.MockPartitionStore)
				assert.Same(t, customProv, cfg.DetectorConfig.MockPartitionProvider)
			},
		},
		{
			name: "missing mock devices file returns error",
			build: func() *Config {
				return &Config{
					DetectorConfig:  &detector.Config{MockDevicesFile: missingSpec},
					AllocatorConfig: &allocator.Config{},
				}
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.build()
			err := cfg.wireMockStores()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			tc.verify(t, cfg)
		})
	}
}

// TestMockDeviceManager_DetectorToAllocatorHotReloadIntegration wires mock Detector and mock
// Allocator end-to-end through devicemanager.Options and devicemanager.Config.wireMockStores
// against a shared fake Kubernetes ObjectTracker and a real Unix-socket fake kubelet
// RegistrationServer, driving live file hot-reload transitions from healthy baseline through
// single-card unqueryable retention, total failure preservation, recovery, shrink, and shutdown.
func TestMockDeviceManager_DetectorToAllocatorHotReloadIntegration(t *testing.T) {
	const (
		nodeName       = "node-mock-integration"
		stepTimeout    = 15 * time.Second
		pollInterval   = 15 * time.Millisecond
		monitorCadence = 15 * time.Millisecond
	)

	t.Setenv("KUBERNETES_NODE_NAME", nodeName)
	t.Setenv(detector.MockDevicesFileEnv, "")
	t.Setenv(allocator.MockDevicesFileEnv, "")

	require.Nil(t, system.LoopbackKubeClient.Get())
	require.Nil(t, system.LoopbackCtrlClient.Get())
	t.Cleanup(func() {
		system.LoopbackKubeClient = varx.Once[kubernetes.Interface]{}
		system.LoopbackCtrlClient = varx.Once[ctrlcli.Client]{}
	})

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: nodeName,
			UID:  types.UID("uid-" + nodeName),
			Labels: map[string]string{
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
			},
		},
	}
	fakeKubeCli := kubefake.NewSimpleClientset(node)
	system.LoopbackKubeClient.Configure(fakeKubeCli)

	// controller-runtime's fake client pluralizes Kind "Devices" as "deviceses" via
	// UnsafeGuessKindToResource, whereas the generated typed client stores CRD objects under
	// "devices". Bridging Devices Get calls through an interceptor lets Allocator and
	// DevicesReconciler observe live Devices writes from Detector without sharing unsynchronized
	// internal tracker state across the two fake client implementations.
	ctrlCli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(node.DeepCopy()).
		WithStatusSubresource(&workercore.Devices{}).
		WithIndex(&core.Pod{}, deviceplugin.IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			Get: func(
				ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, getOpts ...ctrlcli.GetOption,
			) error {
				if devs, ok := obj.(*workercore.Devices); ok {
					got, getErr := fakeKubeCli.WorkerV1alpha1().Devices().Get(ctx, key.Name, meta.GetOptions{})
					if getErr != nil {
						return getErr
					}
					got.DeepCopyInto(devs)
					return nil
				}
				return c.Get(ctx, key, obj, getOpts...)
			},
		}).
		Build()
	system.LoopbackCtrlClient.Configure(ctrlCli)

	rec := &deviceplugin.DevicesReconciler{
		NodeName: nodeName,
		Client:   ctrlCli,
	}

	sockDir := shortIntegrationSocketDir(t)
	kubeSock, fakeKubelet, stopFakeKubelet := startMockIntegrationKubelet(t, sockDir)

	specPath := filepath.Join(t.TempDir(), "mock-devices.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(`
manufacturers:
  nvidia:
    preset: nvidia-h100
    count: 2
interfaces:
  - name: eth-rdma0
    bus: pci
    pciBusId: "0000:10:00.0"
    numaAffinity: "0"
    mtu: 9000
    up: true
    rdma: true
    rdmaDevice: mlx5_0
    link:
      state: OK
rdmaDevices:
  - name: mlx5_0
    driver: mlx5_core
    pciBusId: "0000:10:00.0"
    numaAffinity: "0"
    ports:
      - number: 1
        netDevs: ["eth-rdma0"]
        linkLayer: InfiniBand
        state: ACTIVE
        physicalState: LinkUp
`), 0o600))

	// Parse and validate top-level devicemanager.Options, then wire shared stores via Config.wireMockStores.
	opts := NewOptions()
	fs := pflag.NewFlagSet("device-manager-integration", pflag.ContinueOnError)
	opts.AddFlags(fs)
	require.NoError(t, fs.Parse([]string{
		"--mock-devices-file=" + specPath,
		"--kube-socket=" + kubeSock,
		"--manufacturer=nvidia",
		"--monitor-period=2s",
	}))
	require.NoError(t, opts.Validate(context.Background()))
	assert.Equal(t, specPath, opts.AllocatorOptions.MockDevicesFile)

	detCfg, err := opts.DetectorOptions.Complete(context.Background())
	require.NoError(t, err)
	alcCfg, err := opts.AllocatorOptions.Complete(context.Background())
	require.NoError(t, err)
	assert.Nil(t, alcCfg.MockSpecStore, "WithoutMockDevicesOptions defers store wiring to Config")

	detectedManufacturersCh := make(chan sets.Set[string], 8)
	detCfg.MonitorPeriod = monitorCadence
	detCfg.DetectedManufacturersCh = detectedManufacturersCh
	alcCfg.DetectedManufacturersCh = detectedManufacturersCh
	alcCfg.MockOptions = []allocatormock.Option{allocatormock.WithReconciler(rec)}

	dmCfg := &Config{
		DetectorConfig:  detCfg,
		AllocatorConfig: alcCfg,
	}
	require.NoError(t, dmCfg.wireMockStores())
	require.NotNil(t, dmCfg.DetectorConfig.MockSpecStore)
	assert.Same(t, dmCfg.DetectorConfig.MockSpecStore, dmCfg.AllocatorConfig.MockSpecStore)
	assert.Same(t, dmCfg.AllocatorConfig.MockPartitionStore, dmCfg.DetectorConfig.MockPartitionProvider)

	det, err := dmCfg.DetectorConfig.Apply(context.Background())
	require.NoError(t, err)
	alc, err := dmCfg.AllocatorConfig.Apply(context.Background())
	require.NoError(t, err)
	rec.RDMAInterfaceInventory = det.RDMAInterfaceSnapshot

	baselineGoroutines := snapshotGoroutineIDs()

	runCtx, cancel := context.WithCancel(context.Background())
	detErrCh := make(chan error, 1)
	alcErrCh := make(chan error, 1)
	go func() { detErrCh <- det.Start(runCtx) }()
	go func() { alcErrCh <- alc.Start(runCtx) }()
	t.Cleanup(func() {
		cancel()
	})

	// 1. Verify NodeFeature + Devices materialize, detectedManufacturersCh wakes Allocator, and
	// kubelet receives all 8 DevicePlugin registrations (3 RDMA modes + 5 NVIDIA modes).
	for range 8 {
		select {
		case <-fakeKubelet.notifyCh:
		case <-time.After(stepTimeout):
			t.Fatalf("timed out waiting for kubelet DevicePlugin registrations; got %v",
				fakeKubelet.registeredResources())
		}
	}

	registered := fakeKubelet.registeredResources()
	for _, wantRes := range []string{
		"nvidia.com/gpu",
		"nvidia.com/gpu.shared",
		"nvidia.com/gpu.sliced",
		"nvidia.com/gpu.partitioned",
		"device.gpustack.ai/nvidia.visibility",
		"device.gpustack.ai/rdma",
		"device.gpustack.ai/rdma.shared",
		"device.gpustack.ai/rdma.partitioned",
	} {
		assert.Contains(t, registered, wantRes)
	}

	var initialDevs *workercore.Devices
	require.Eventually(t, func() bool {
		d, getErr := fakeKubeCli.WorkerV1alpha1().Devices().Get(runCtx, nodeName, meta.GetOptions{})
		if getErr != nil || len(d.Spec.Groups) != 1 || len(d.Spec.Groups[0].Accelerators) != 2 {
			return false
		}
		if det.MonitorSnapshot() == nil || det.RDMAInterfaceSnapshot() == nil {
			return false
		}
		initialDevs = d
		return true
	}, stepTimeout, pollInterval)

	assert.Equal(t, "true", initialDevs.Annotations[detector.MockDevicesAnnotationKey])
	grpID := initialDevs.Spec.Groups[0].ID
	card0ID := initialDevs.Spec.Groups[0].Accelerators[0].ID
	card1ID := initialDevs.Spec.Groups[0].Accelerators[1].ID
	assert.Equal(t, uint32(0), initialDevs.Spec.Groups[0].Accelerators[0].Index)
	assert.Equal(t, uint32(1), initialDevs.Spec.Groups[0].Accelerators[1].Index)
	assert.False(t, initialDevs.Spec.Groups[0].Accelerators[0].Status.Unhealthy)
	assert.False(t, initialDevs.Spec.Groups[0].Accelerators[1].Status.Unhealthy)

	nf, err := fakeKubeCli.NfdV1alpha1().NodeFeatures(kuberess.SystemNamespaceName).Get(
		runCtx, nodeName+"-gpustack-device-manager", meta.GetOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, "true", nf.Spec.Labels[nodefeature.NodeRDMACapableLabelKey])
	featKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-" + grpID
	assert.Equal(t, "2", nf.Spec.Labels[featKey+".count"])

	// 2. Healthy baseline: verify ListAndWatch and mock Allocate over real Unix sockets.
	gpuConn, err := grpc.NewClient(
		"unix://"+filepath.Join(sockDir, registered["nvidia.com/gpu"]),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func() { _ = gpuConn.Close() }()
	gpuCli := kubeletdeviceplugin.NewDevicePluginClient(gpuConn)

	card0Token := deviceplugin.Resource{Group: grpID, Device: card0ID}.DeviceIDs(
		workercore.DeviceAllocationModeExclusive, 0,
	)[0]
	card1Token := deviceplugin.Resource{Group: grpID, Device: card1ID}.DeviceIDs(
		workercore.DeviceAllocationModeExclusive, 0,
	)[0]

	baselineAdvertised := recvInitialListAndWatch(t, runCtx, gpuCli)
	require.Len(t, baselineAdvertised, 2)
	assert.Equal(t, card0Token, baselineAdvertised[0].ID)
	assert.Equal(t, kubeletdeviceplugin.Healthy, baselineAdvertised[0].Health)
	assert.Equal(t, card1Token, baselineAdvertised[1].ID)
	assert.Equal(t, kubeletdeviceplugin.Healthy, baselineAdvertised[1].Health)

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "pod-gpu-0",
			Namespace: "default",
			UID:       types.UID("uid-pod-gpu-0"),
		},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "main",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
					},
				},
			}},
		},
	}
	require.NoError(t, ctrlCli.Create(runCtx, pod))

	gpuAllocResp, err := gpuCli.Allocate(runCtx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{card0Token},
		}},
	})
	require.NoError(t, err)
	require.Len(t, gpuAllocResp.ContainerResponses, 1)
	gpuCtr := gpuAllocResp.ContainerResponses[0]
	assert.Empty(t, gpuCtr.Devices, "mock GPU Allocate must inject zero /dev/* nodes")
	assert.Empty(t, gpuCtr.Mounts, "mock GPU Allocate must inject zero host mounts")
	assert.Equal(t, card0ID, gpuCtr.Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, card0ID, gpuCtr.Envs[allocatormock.EnvMockAllocatedDevices])
	assert.Equal(t, "exclusive", gpuCtr.Envs[allocatormock.EnvMockAllocationMode])
	assert.Equal(t, "nvidia", gpuCtr.Envs[allocatormock.EnvMockManufacturer])

	rdmaConn, err := grpc.NewClient(
		"unix://"+filepath.Join(sockDir, registered["device.gpustack.ai/rdma"]),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func() { _ = rdmaConn.Close() }()
	rdmaCli := kubeletdeviceplugin.NewDevicePluginClient(rdmaConn)

	rdmaAllocResp, err := rdmaCli.Allocate(runCtx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{
			DevicesIds: []string{"eth-rdma0:eth-rdma0:0000"},
		}},
	})
	require.NoError(t, err)
	require.Len(t, rdmaAllocResp.ContainerResponses, 1)
	rdmaCtr := rdmaAllocResp.ContainerResponses[0]
	assert.Empty(t, rdmaCtr.Devices, "mock RDMA Allocate must inject zero /dev/infiniband/* nodes")
	assert.Empty(t, rdmaCtr.Mounts, "mock RDMA Allocate must inject zero host mounts")
	assert.Equal(t, "=mlx5_0", rdmaCtr.Envs["NCCL_IB_HCA"])
	assert.Equal(t, "mlx5_0", rdmaCtr.Envs["GPUSTACK_RDMA_DEVICES"])
	assert.Equal(t, "mlx5_0", rdmaCtr.Envs[allocatormock.EnvMockAllocatedDevices])
	assert.Equal(t, "exclusive", rdmaCtr.Envs[allocatormock.EnvMockAllocationMode])
	assert.Equal(t, "rdma", rdmaCtr.Envs[allocatormock.EnvMockManufacturer])

	// 3a. Hot-reload: single card queryState=unqueryable retains card 0 in-place with
	// Status.Unhealthy=true and leaves sibling card 1 Index=1 unchanged.
	atomicWriteSpec(t, specPath, `
manufacturers:
  nvidia:
    preset: nvidia-h100
    count: 2
    cardOverrides:
      0:
        queryState: unqueryable
`)
	require.Eventually(t, func() bool {
		cur, getErr := fakeKubeCli.WorkerV1alpha1().Devices().Get(runCtx, nodeName, meta.GetOptions{})
		if getErr != nil || len(cur.Spec.Groups) != 1 || len(cur.Spec.Groups[0].Accelerators) != 2 {
			return false
		}
		acc := cur.Spec.Groups[0].Accelerators
		return acc[0].ID == card0ID && acc[0].Index == 0 && acc[0].Status.Unhealthy &&
			acc[1].ID == card1ID && acc[1].Index == 1 && !acc[1].Status.Unhealthy
	}, stepTimeout, pollInterval)

	unqueryableAdvertised := recvInitialListAndWatch(t, runCtx, gpuCli)
	require.Len(t, unqueryableAdvertised, 2)
	assert.Equal(t, kubeletdeviceplugin.Unhealthy, unqueryableAdvertised[0].Health)
	assert.Equal(t, kubeletdeviceplugin.Healthy, unqueryableAdvertised[1].Health)

	// 3b. Hot-reload: totalFailure=true preserves lastDetected without wiping cards or retiring
	// the mock NVIDIA allocator. A marker interface update proves a full detect+report round ran
	// after reloading the totalFailure spec.
	revBeforeTotalFailure := dmCfg.DetectorConfig.MockSpecStore.Current().Revision
	atomicWriteSpec(t, specPath, `
manufacturers:
  nvidia:
    preset: nvidia-h100
    count: 2
    fault:
      totalFailure: true
interfaces:
  - name: eth-total-failure-marker
    bus: pci
    pciBusId: "0000:20:00.0"
    up: true
`)
	require.Eventually(t, func() bool {
		if dmCfg.DetectorConfig.MockSpecStore.Current().Revision <= revBeforeTotalFailure {
			return false
		}
		cur, getErr := fakeKubeCli.WorkerV1alpha1().Devices().Get(runCtx, nodeName, meta.GetOptions{})
		if getErr != nil || len(cur.Spec.Interfaces) != 1 {
			return false
		}
		return cur.Spec.Interfaces[0].Name == "eth-total-failure-marker"
	}, stepTimeout, pollInterval)

	devsDuringTotalFailure, err := fakeKubeCli.WorkerV1alpha1().Devices().Get(runCtx, nodeName, meta.GetOptions{})
	require.NoError(t, err)
	require.Len(t, devsDuringTotalFailure.Spec.Groups, 1)
	require.Len(t, devsDuringTotalFailure.Spec.Groups[0].Accelerators, 2,
		"totalFailure must preserve lastDetected cards rather than wiping Devices")
	assert.Equal(t, card0ID, devsDuringTotalFailure.Spec.Groups[0].Accelerators[0].ID)
	assert.Equal(t, card1ID, devsDuringTotalFailure.Spec.Groups[0].Accelerators[1].ID)

	duringFailureAdvertised := recvInitialListAndWatch(t, runCtx, gpuCli)
	require.Len(t, duringFailureAdvertised, 2, "mock NVIDIA allocator must stay active during totalFailure")

	// 3c. Hot-reload: recovery (queryState=ok, totalFailure=false) clears Status.Unhealthy.
	atomicWriteSpec(t, specPath, `
manufacturers:
  nvidia:
    preset: nvidia-h100
    count: 2
    fault:
      totalFailure: false
`)
	require.Eventually(t, func() bool {
		cur, getErr := fakeKubeCli.WorkerV1alpha1().Devices().Get(runCtx, nodeName, meta.GetOptions{})
		if getErr != nil || len(cur.Spec.Groups) != 1 || len(cur.Spec.Groups[0].Accelerators) != 2 {
			return false
		}
		acc := cur.Spec.Groups[0].Accelerators
		return acc[0].ID == card0ID && acc[0].Index == 0 && !acc[0].Status.Unhealthy &&
			acc[1].ID == card1ID && acc[1].Index == 1 && !acc[1].Status.Unhealthy
	}, stepTimeout, pollInterval)

	recoveredAdvertised := recvInitialListAndWatch(t, runCtx, gpuCli)
	require.Len(t, recoveredAdvertised, 2)
	assert.Equal(t, kubeletdeviceplugin.Healthy, recoveredAdvertised[0].Health)
	assert.Equal(t, kubeletdeviceplugin.Healthy, recoveredAdvertised[1].Health)

	// 3d. Hot-reload: clean shrink (count: 1) prunes the removed slot from Devices.
	atomicWriteSpec(t, specPath, `
manufacturers:
  nvidia:
    preset: nvidia-h100
    count: 1
`)
	require.Eventually(t, func() bool {
		cur, getErr := fakeKubeCli.WorkerV1alpha1().Devices().Get(runCtx, nodeName, meta.GetOptions{})
		if getErr != nil || len(cur.Spec.Groups) != 1 || len(cur.Spec.Groups[0].Accelerators) != 1 {
			return false
		}
		acc := cur.Spec.Groups[0].Accelerators[0]
		return acc.ID == card0ID && acc.Index == 0 && !acc.Status.Unhealthy
	}, stepTimeout, pollInterval)

	shrunkAdvertised := recvInitialListAndWatch(t, runCtx, gpuCli)
	require.Len(t, shrunkAdvertised, 1)
	assert.Equal(t, card0Token, shrunkAdvertised[0].ID)

	// 4. Clean shutdown on context cancel with zero goroutine leaks.
	require.NoError(t, gpuConn.Close())
	require.NoError(t, rdmaConn.Close())
	cancel()

	select {
	case detErr := <-detErrCh:
		assert.True(t, detErr == nil || errors.Is(detErr, context.Canceled), "unexpected detector error: %v", detErr)
	case <-time.After(stepTimeout):
		t.Fatal("Detector.Start did not exit after context cancellation")
	}

	select {
	case alcErr := <-alcErrCh:
		assert.True(t, alcErr == nil || errors.Is(alcErr, context.Canceled), "unexpected allocator error: %v", alcErr)
	case <-time.After(stepTimeout):
		t.Fatal("Allocator.Start did not exit after context cancellation")
	}

	stopFakeKubelet()

	var remainingLeaks []string
	require.Eventually(t, func() bool {
		remainingLeaks = leakedModuleGoroutines(baselineGoroutines)
		return len(remainingLeaks) == 0
	}, 5*time.Second, 20*time.Millisecond)
	assert.Empty(t, remainingLeaks)
}
