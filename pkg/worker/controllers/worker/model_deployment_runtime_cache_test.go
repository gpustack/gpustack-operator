package worker

import (
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The environment names an engine's startup caches honor, and the subdirectory of the cache
// mount each one is rendered to.
//
// The names are spelled out here rather than taken from the implementation so that a change to
// either side is a visible difference: an upstream cache that moved to another variable would
// otherwise silently keep pointing at the container's writable layer.
var runtimeCacheEnvLayout = []struct {
	name   string
	subDir string
}{
	{name: "HF_HUB_CACHE", subDir: "hf-hub"},
	{name: "PIP_CACHE_DIR", subDir: "pip"},
	{name: "TORCH_HOME", subDir: "torch"},
	{name: "FLASHINFER_WORKSPACE_BASE", subDir: "flashinfer"},
	{name: "TORCHINDUCTOR_CACHE_DIR", subDir: "inductor"},
	{name: "TRITON_CACHE_DIR", subDir: "triton"},
}

// TestModelDeploymentRuntimeCacheDefaultedEnv pins where each engine is told its startup caches
// live. Every value is a subdirectory of the host-backed cache mount, so the bytes land on the
// node's disk instead of on the container's writable layer, which is the layer the pod's
// ephemeral-storage limit measures.
func TestModelDeploymentRuntimeCacheDefaultedEnv(t *testing.T) {
	want := make([]core.EnvVar, 0, len(runtimeCacheEnvLayout))
	for _, c := range runtimeCacheEnvLayout {
		want = append(want, core.EnvVar{
			Name:  c.name,
			Value: path.Join(ModelDeploymentCompileCacheMountPath, c.subDir),
		})
	}

	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang,
	} {
		t.Run(engine+" is told where its caches live", func(t *testing.T) {
			assert.Equal(t, want, modelDeploymentRuntimeCacheDefaultedEnv(engine))
		})
	}

	t.Run("an engine the operator does not know gets nothing rather than a wrong path",
		func(t *testing.T) {
			assert.Nil(t, modelDeploymentRuntimeCacheDefaultedEnv("Triton"))
		})
}

// TestModelDeploymentRuntimeCacheEnvStaysInsideTheCacheMount pins the two properties that make
// the redirect work: every value is inside the mount, and no two caches share a directory, so
// one cache cannot write over another's files.
func TestModelDeploymentRuntimeCacheEnvStaysInsideTheCacheMount(t *testing.T) {
	env := modelDeploymentRuntimeCacheDefaultedEnv(workercore.ModelDeploymentEngineVLLM)
	require.NotEmpty(t, env)

	seen := map[string]string{}
	for _, e := range env {
		assert.True(t, strings.HasPrefix(e.Value, ModelDeploymentCompileCacheMountPath+"/"),
			"%s points outside the cache mount: %s", e.Name, e.Value)
		other, shared := seen[e.Value]
		assert.False(t, shared, "%s and %s would share one directory", e.Name, other)
		seen[e.Value] = e.Name
	}
}

// TestRenderModelDeploymentPod_RuntimeCacheEnvPointsAtTheHostBackedMount checks the redirect
// where it is meant to take effect: the container's environment names a path inside the volume
// the operator mounted, so what an engine writes there is the node's disk and not the pod's.
func TestRenderModelDeploymentPod_RuntimeCacheEnvPointsAtTheHostBackedMount(t *testing.T) {
	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang,
	} {
		t.Run(engine, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = engine
				md.Spec.KVCache = nil
			})

			pod := renderOne(t, md, newRenderInstanceType())

			volume := findVolume(pod, ModelDeploymentCompileCacheVolumeName)
			require.NotNil(t, volume, "the managed role carries the cache volume")
			mount := findMount(&pod.Spec.Containers[0], ModelDeploymentCompileCacheVolumeName)
			require.NotNil(t, mount, "the cache volume is mounted into the engine's container")
			require.Equal(t, ModelDeploymentCompileCacheMountPath, mount.MountPath)

			for _, c := range runtimeCacheEnvLayout {
				value, found := envValue(pod, c.name)
				require.True(t, found, "%s is not rendered into the container", c.name)
				assert.Equal(t, path.Join(mount.MountPath, c.subDir), value)
				assert.True(t, strings.HasPrefix(value, mount.MountPath+"/"),
					"%s points outside the mounted cache: %s", c.name, value)
			}
		})
	}
}

// TestRenderModelDeploymentPod_RuntimeCacheEnvIsDefaultedNotOwned pins who decides where a
// cache lives. Defaulted is the load-bearing half: an owned name would be refused at
// admission, so a user who mounts the cache elsewhere, or who runs an image whose engine
// spells the variable differently, could not say so at all.
func TestRenderModelDeploymentPod_RuntimeCacheEnvIsDefaultedNotOwned(t *testing.T) {
	for _, c := range runtimeCacheEnvLayout {
		t.Run(c.name+" is a default, not a refusal", func(t *testing.T) {
			assert.True(t, ModelDeploymentDefaultsEnv(c.name))
			for _, engine := range []string{
				workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang,
			} {
				assert.False(t, ModelDeploymentOwnsEnv(engine, c.name),
					"%s must not be owned on %s, or a user could not redirect the cache",
					c.name, engine)
			}
		})
	}

	t.Run("a user entry replaces the default", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
				{Name: "TRITON_CACHE_DIR", Value: "/mnt/scratch/triton"},
			}
		})

		pod := renderOne(t, md, newRenderInstanceType())

		value, found := envValue(pod, "TRITON_CACHE_DIR")
		require.True(t, found)
		assert.Equal(t, "/mnt/scratch/triton", value)
	})

	t.Run("a take-over role is told nothing about a cache it does not have", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
		})

		pod := renderOne(t, md, newRenderInstanceType())

		for _, c := range runtimeCacheEnvLayout {
			_, found := envValue(pod, c.name)
			assert.False(t, found, "%s must not be rendered for a take-over role", c.name)
		}
	})
}
