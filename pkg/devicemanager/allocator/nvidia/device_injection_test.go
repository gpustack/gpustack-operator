package nvidia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// The channel resolution itself is the shared package's, and is covered there. What has to be
// asserted here is the wiring: that this manufacturer's vocabulary reaches the resolver, and that the
// resolver reaches the responder that renders the grant.

// testPodNamingNoRuntimeClass is the shape auto reads: a Pod that leaves the handler to the engine's
// default.
func testPodNamingNoRuntimeClass() *core.Pod {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default"}}
}

func TestInjectionConfig(t *testing.T) {
	assert.Equal(t, Manufacturer, injectionConfig.Manufacturer)
	assert.Equal(t, "nvidia.com/gpu", injectionConfig.CDIKind,
		"the kind nvidia-ctk publishes whole accelerators under")
	assert.Equal(t, "NVIDIA_VISIBLE_DEVICES", injectionConfig.VisibleDevicesEnv)
	assert.Equal(t, "GPUSTACK_NVIDIA_DEVICE_INJECTION_STRATEGY",
		deviceplugin.InjectionStrategyEnv(injectionConfig.Manufacturer))
}

// A server built with a resolver has to render through it. Building the server and asking it for a
// response is the only assertion that proves the wiring rather than the resolver in isolation: a
// signature that takes one and never stores it compiles, passes every test in the shared package, and
// silently keeps every node on the default.
func TestServerRendersThroughItsResolver(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia.yaml"),
		[]byte("cdiVersion: 0.7.0\nkind: nvidia.com/gpu\ndevices:\n  - name: "+testGPUUUID0+"\n"), 0o600))
	orig := deviceplugin.CDISpecDirs
	deviceplugin.CDISpecDirs = []string{dir}
	t.Cleanup(func() { deviceplugin.CDISpecDirs = orig })
	t.Setenv(deviceplugin.InjectionStrategyEnv(Manufacturer), "cdi-annotations")

	resolver, err := deviceplugin.NewInjectionResolver(injectionConfig)
	require.NoError(t, err)

	s, ok := newServer(klog.Background(), workercore.DeviceAllocationModeExclusive, nil, resolver).(*server)
	require.True(t, ok)

	resp, err := s.GetContainerAllocateResponse(context.Background(), testPodNamingNoRuntimeClass(),
		&core.Container{Name: "main"}, nvidiaDevices("12.4", 24576, testGPUUUID0),
		map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1})
	require.NoError(t, err)
	assert.Equal(t, "nvidia.com/gpu="+testGPUUUID0, resp.Annotations["cdi.k8s.io/gpustack-nvidia"])
	assert.NotContains(t, resp.Envs, "NVIDIA_VISIBLE_DEVICES")
}

// Neither channel reports an empty grant on its own: the environment variable would carry an empty
// value and the annotation an empty device list, so the response would be a success the container
// cannot use. The AMD responder refuses the same shape.
func TestServerRefusesAGrantThatResolvedToNoAccelerator(t *testing.T) {
	s, ok := newServer(klog.Background(), workercore.DeviceAllocationModeExclusive, nil, nil).(*server)
	require.True(t, ok)

	resp, err := s.GetContainerAllocateResponse(context.Background(), testPodNamingNoRuntimeClass(),
		&core.Container{Name: "main"}, nvidiaDevices("12.4", 24576, testGPUUUID0),
		map[deviceplugin.Resource]int32{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no allocated accelerator found")
	assert.Nil(t, resp)
}
