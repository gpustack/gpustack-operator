package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const testArtifactRevision = "0123456789abcdef0123456789abcdef01234567"

func testPvcArtifactRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryPVC, ClaimName: "models", Path: "qwen/72b",
	}
}

func testEngineArtifactRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery:   workercore.ModelDeploymentModelDeliveryEngine,
		Hub:        modelDeploymentHubHuggingFace,
		Repository: "Qwen/Qwen2.5-72B-Instruct", Revision: testArtifactRevision, SecretName: "hf-token",
		SizeBytes: 100 << 30, Endpoint: "https://hub.example", HTTPSProxy: "http://proxy:3128", NoProxy: "svc",
	}
}

func testModelScopeEngineRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery:   workercore.ModelDeploymentModelDeliveryEngine,
		Hub:        modelDeploymentHubModelScope,
		Repository: "qwen/Qwen2.5-72B-Instruct", Revision: testArtifactRevision, SecretName: "ms-token",
		SizeBytes: 100 << 30, Endpoint: "https://www.modelscope.cn", HTTPSProxy: "http://proxy:3128", NoProxy: "svc",
	}
}

func testNodeArtifactRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryNode, ArtifactName: "qwen", ArtifactUID: "uid-qwen",
		ManifestDigest: testArtifactDigest, Repository: "Qwen/Qwen2.5-72B-Instruct", Revision: testArtifactRevision,
		SecretName: "hf-token", SizeBytes: 100 << 30,
	}
}

func testImageArtifactRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery:       workercore.ModelDeploymentModelDeliveryImage,
		ImageReference: "registry.example.com/team/qwen@sha256:" + strings.Repeat("a", 64),
	}
}

// TestRenderModelDeploymentArtifactImageVolume renders an image artifact's delivery: one image
// volume, the digest-pinned reference, the model path read-only and whole, and none of the engine
// download's machinery — no cache volume, no download environment, no revision argument.
func TestRenderModelDeploymentArtifactImageVolume(t *testing.T) {
	render := testImageArtifactRender()
	cases := []struct {
		name     string
		takeOver bool
	}{
		{name: "an image artifact is mounted whole and read-only"},
		{name: "a take-over role still gets the image"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				if c.takeOver {
					md.Spec.Roles[0].Command = []string{"sh", "-c", "serve"}
				}
			})
			pod := renderWithArtifact(t, md, render)
			main := &pod.Spec.Containers[0]

			vol, mount := findVolume(pod, modelDeploymentModelVolumeName), findMount(main, modelDeploymentModelVolumeName)
			require.NotNil(t, vol)
			require.NotNil(t, mount)
			require.NotNil(t, vol.Image, "the weights volume is an image volume")
			assert.Equal(t, render.ImageReference, vol.Image.Reference)
			assert.Empty(t, vol.Image.PullPolicy, "a digest-pinned reference keeps the default policy")
			assert.Equal(t, ModelDeploymentModelMountPath, mount.MountPath)
			assert.True(t, mount.ReadOnly)
			assert.Empty(t, mount.SubPath, "the image root is mounted whole")
			assert.Nil(t, findVolume(pod, modelDeploymentModelCacheVolumeName),
				"kubelet's image store is the whole delivery, so there is no cache to size")
			for _, env := range main.Env {
				assert.NotEqual(t, modelDeploymentHFHomeEnv, env.Name, "no download environment")
				assert.NotEqual(t, modelDeploymentHFTokenEnv, env.Name)
			}
			assert.NotContains(t, main.Command, modelDeploymentRevisionArg)
		})
	}
}

func renderWithArtifact(
	t *testing.T, md *workercore.ModelDeployment, artifact *ModelDeploymentArtifactRender,
) *core.Pod {
	t.Helper()
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
		Artifact:     artifact,
	})
	require.NoError(t, err)

	return pod
}

func findVolume(pod *core.Pod, name string) *core.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}

	return nil
}

func findMount(c *core.Container, name string) *core.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}

	return nil
}

func findEnv(c *core.Container, name string) *core.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}

	return nil
}

func TestRenderModelDeploymentArtifactCommand(t *testing.T) {
	const served = "Qwen/Qwen2.5-72B-Instruct"
	cases := []struct {
		name      string
		engine    string
		artifact  *ModelDeploymentArtifactRender
		extraArgs []string
		wantHead  []string
		wantArgs  []string
		absent    []string
	}{
		{
			name: "vLLM on a claim", engine: workercore.ModelDeploymentEngineVLLM, artifact: testPvcArtifactRender(),
			wantHead: []string{"vllm", "serve", ModelDeploymentModelMountPath},
			wantArgs: []string{"--served-model-name", served},
			absent:   []string{"--revision"},
		},
		{
			name: "SGLang on a claim", engine: workercore.ModelDeploymentEngineSGLang, artifact: testPvcArtifactRender(),
			wantHead: []string{"python3", "-m", "sglang.launch_server", "--model-path", ModelDeploymentModelMountPath},
			wantArgs: []string{"--served-model-name", served},
		},
		{
			name: "vLLM downloading the pinned commit", engine: workercore.ModelDeploymentEngineVLLM, artifact: testEngineArtifactRender(),
			wantHead: []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct", "--revision", testArtifactRevision},
			wantArgs: []string{"--served-model-name", served},
		},
		{
			name: "SGLang downloading the pinned commit", engine: workercore.ModelDeploymentEngineSGLang, artifact: testEngineArtifactRender(),
			wantHead: []string{"python3", "-m", "sglang.launch_server", "--model-path", "Qwen/Qwen2.5-72B-Instruct", "--enable-metrics", "--revision", testArtifactRevision},
			wantArgs: []string{"--served-model-name", served},
		},
		{
			name: "vLLM on the node's published tree", engine: workercore.ModelDeploymentEngineVLLM, artifact: testNodeArtifactRender(),
			wantHead: []string{"vllm", "serve", ModelDeploymentModelMountPath},
			wantArgs: []string{"--served-model-name", served},
			absent:   []string{"--revision"},
		},
		{
			name: "SGLang on the node's published tree", engine: workercore.ModelDeploymentEngineSGLang, artifact: testNodeArtifactRender(),
			wantHead: []string{"python3", "-m", "sglang.launch_server", "--model-path", ModelDeploymentModelMountPath},
			wantArgs: []string{"--served-model-name", served},
			absent:   []string{"--revision"},
		},
		{
			name: "a role stating the served name keeps its own and gets no second one", engine: workercore.ModelDeploymentEngineVLLM,
			artifact: testPvcArtifactRender(), extraArgs: []string{"--served-model-name=" + served},
			wantHead: []string{"vllm", "serve", ModelDeploymentModelMountPath},
			absent:   []string{"--served-model-name"},
		},
		{
			name: "no artifact renders the served name as the model and no flag", engine: workercore.ModelDeploymentEngineVLLM,
			wantHead: []string{"vllm", "serve", served},
			absent:   []string{"--served-model-name", "--revision"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = c.engine
				md.Spec.Roles[0].ExtraArgs = c.extraArgs
			})
			cmd := renderWithArtifact(t, md, c.artifact).Spec.Containers[0].Command

			require.GreaterOrEqual(t, len(cmd), len(c.wantHead))
			assert.Equal(t, c.wantHead, cmd[:len(c.wantHead)])
			if c.wantArgs != nil {
				assert.Contains(t, cmd, c.wantArgs[0])
				assert.Equal(t, c.wantArgs[1], argValue(t, cmd, c.wantArgs[0]))
			}
			for _, flag := range c.absent {
				assert.NotContains(t, cmd, flag)
			}
		})
	}
}

func TestRenderModelDeploymentArtifactVolumes(t *testing.T) {
	cases := []struct {
		name      string
		artifact  *ModelDeploymentArtifactRender
		takeOver  bool
		wantClaim bool
		wantCache string
	}{
		{name: "a claim is mounted read-only at its directory", artifact: testPvcArtifactRender(), wantClaim: true},
		{name: "a take-over role still gets the claim", artifact: testPvcArtifactRender(), takeOver: true, wantClaim: true},
		{name: "an engine download gets a size-limited cache", artifact: testEngineArtifactRender(), wantCache: "110Gi"},
		{
			name: "a small download still gets a gibibyte of headroom",
			artifact: func() *ModelDeploymentArtifactRender {
				a := testEngineArtifactRender()
				a.SizeBytes = 1 << 20
				return a
			}(),
			wantCache: "1025Mi",
		},
		{name: "a take-over role downloads nothing for the operator", artifact: testEngineArtifactRender(), takeOver: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				if c.takeOver {
					md.Spec.Roles[0].Command = []string{"sh", "-c", "serve"}
				}
			})
			pod := renderWithArtifact(t, md, c.artifact)
			main := &pod.Spec.Containers[0]

			claim, claimMount := findVolume(pod, modelDeploymentModelVolumeName), findMount(main, modelDeploymentModelVolumeName)
			cache, cacheMount := findVolume(pod, modelDeploymentModelCacheVolumeName), findMount(main, modelDeploymentModelCacheVolumeName)
			if c.wantClaim {
				require.NotNil(t, claim)
				require.NotNil(t, claimMount)
				assert.Equal(t, "models", claim.PersistentVolumeClaim.ClaimName)
				assert.True(t, claim.PersistentVolumeClaim.ReadOnly)
				assert.Equal(t, ModelDeploymentModelMountPath, claimMount.MountPath)
				assert.Equal(t, "qwen/72b", claimMount.SubPath)
				assert.True(t, claimMount.ReadOnly)
			} else {
				assert.Nil(t, claim)
			}
			if c.wantCache == "" {
				assert.Nil(t, cache)
				return
			}
			require.NotNil(t, cache)
			require.NotNil(t, cacheMount)
			assert.Equal(t, ModelDeploymentModelCachePath, cacheMount.MountPath)
			assert.Zero(t, cache.EmptyDir.SizeLimit.Cmp(resource.MustParse(c.wantCache)), cache.EmptyDir.SizeLimit.String())
		})
	}
}

func TestRenderModelDeploymentArtifactEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		artifact  *ModelDeploymentArtifactRender
		userEnv   []workercore.ModelDeploymentEnvVar
		takeOver  bool
		wantValue map[string]string
		wantToken bool
		absent    []string
	}{
		{
			name: "an engine download reaches the Hub with the artifact's Secret", artifact: testEngineArtifactRender(),
			wantValue: map[string]string{
				"HF_HOME": ModelDeploymentModelCachePath, "HF_ENDPOINT": "https://hub.example",
				"HTTPS_PROXY": "http://proxy:3128", "NO_PROXY": "svc",
			},
			wantToken: true,
		},
		{
			name: "a public artifact gets no token", wantValue: map[string]string{"HF_HOME": ModelDeploymentModelCachePath},
			artifact: func() *ModelDeploymentArtifactRender {
				a := testEngineArtifactRender()
				a.SecretName, a.HTTPSProxy, a.NoProxy = "", "", ""
				return a
			}(),
			absent: []string{"HF_TOKEN", "HTTPS_PROXY", "NO_PROXY"},
		},
		{
			name: "a role's own proxy wins, and an owned name stored before the rule never does", artifact: testEngineArtifactRender(),
			userEnv: []workercore.ModelDeploymentEnvVar{
				{Name: "HTTPS_PROXY", Value: "http://mine:3128"}, {Name: "HF_ENDPOINT", Value: "https://elsewhere"},
			},
			wantValue: map[string]string{"HTTPS_PROXY": "http://mine:3128", "HF_ENDPOINT": "https://hub.example"},
			wantToken: true,
		},
		{name: "a claim adds nothing", artifact: testPvcArtifactRender(), absent: []string{"HF_HOME", "HF_ENDPOINT", "HF_TOKEN"}},
		{
			name: "node delivery adds nothing: the token reaches the plugin through kubelet", artifact: testNodeArtifactRender(),
			absent: []string{"HF_HOME", "HF_ENDPOINT", "HF_TOKEN", "HTTPS_PROXY", "NO_PROXY"},
		},
		{
			name: "a take-over role gets nothing", artifact: testEngineArtifactRender(), takeOver: true,
			absent: []string{"HF_HOME", "HF_ENDPOINT", "HF_TOKEN", "HTTPS_PROXY"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Env = c.userEnv
				if c.takeOver {
					md.Spec.Roles[0].Command = []string{"sh", "-c", "serve"}
				}
			})
			main := &renderWithArtifact(t, md, c.artifact).Spec.Containers[0]

			for name, want := range c.wantValue {
				e := findEnv(main, name)
				require.NotNil(t, e, name)
				assert.Equal(t, want, e.Value, name)
			}
			if c.wantToken {
				e := findEnv(main, "HF_TOKEN")
				require.NotNil(t, e)
				assert.Empty(t, e.Value, "the token is never a literal")
				require.NotNil(t, e.ValueFrom)
				assert.Equal(t, "hf-token", e.ValueFrom.SecretKeyRef.Name)
				assert.Equal(t, "token", e.ValueFrom.SecretKeyRef.Key)
			}
			for _, name := range c.absent {
				assert.Nil(t, findEnv(main, name), name)
			}
		})
	}
}

// TestRenderModelDeploymentArtifactModelScopeEnvironment renders a ModelScope download's
// environment: the SDK's own names, the endpoint as the bare host the SDK prefixes, the engine's
// use-switch, and the token from the artifact's Secret. No Hugging Face name appears.
func TestRenderModelDeploymentArtifactModelScopeEnvironment(t *testing.T) {
	cases := []struct {
		name    string
		engine  string
		wantUse string
		notUse  string
		absent  []string
	}{
		{
			name: "a vLLM download flips vLLM's switch", engine: workercore.ModelDeploymentEngineVLLM,
			wantUse: "VLLM_USE_MODELSCOPE", notUse: "SGLANG_USE_MODELSCOPE",
			absent: []string{"HF_HOME", "HF_ENDPOINT", "HF_TOKEN"},
		},
		{
			name: "a SGLang download flips SGLang's switch", engine: workercore.ModelDeploymentEngineSGLang,
			wantUse: "SGLANG_USE_MODELSCOPE", notUse: "VLLM_USE_MODELSCOPE",
			absent: []string{"HF_HOME", "HF_ENDPOINT", "HF_TOKEN"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = c.engine
			})
			main := &renderWithArtifact(t, md, testModelScopeEngineRender()).Spec.Containers[0]

			for name, want := range map[string]string{
				"MODELSCOPE_CACHE":  ModelDeploymentModelCachePath,
				"MODELSCOPE_DOMAIN": "www.modelscope.cn",
				c.wantUse:           "true",
				"HTTPS_PROXY":       "http://proxy:3128",
			} {
				e := findEnv(main, name)
				require.NotNil(t, e, name)
				assert.Equal(t, want, e.Value, name)
			}
			assert.Nil(t, findEnv(main, c.notUse), c.notUse)
			token := findEnv(main, "MODELSCOPE_API_TOKEN")
			require.NotNil(t, token)
			assert.Empty(t, token.Value, "the token is never a literal")
			require.NotNil(t, token.ValueFrom)
			assert.Equal(t, "ms-token", token.ValueFrom.SecretKeyRef.Name)
			assert.Equal(t, "token", token.ValueFrom.SecretKeyRef.Key)
			for _, name := range c.absent {
				assert.Nil(t, findEnv(main, name), name)
			}
		})
	}

	t.Run("a public artifact gets no token", func(t *testing.T) {
		render := testModelScopeEngineRender()
		render.SecretName, render.HTTPSProxy, render.NoProxy = "", "", ""
		main := &renderWithArtifact(t, newRenderDeployment(), render).Spec.Containers[0]

		assert.Nil(t, findEnv(main, "MODELSCOPE_API_TOKEN"))
		assert.Nil(t, findEnv(main, "HTTPS_PROXY"))
		assert.NotNil(t, findEnv(main, "VLLM_USE_MODELSCOPE"))
	})
}

func TestRenderModelDeploymentArtifactEphemeralStorage(t *testing.T) {
	cases := []struct {
		name      string
		artifact  *ModelDeploymentArtifactRender
		wantLimit string
	}{
		{name: "an engine download raises the limit by its cache", artifact: testEngineArtifactRender(), wantLimit: "125Gi"},
		{name: "a claim leaves it alone", artifact: testPvcArtifactRender(), wantLimit: "15Gi"},
		{name: "node delivery leaves it alone", artifact: testNodeArtifactRender(), wantLimit: "15Gi"},
		{name: "no artifact leaves it alone", wantLimit: "15Gi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := renderWithArtifact(t, newRenderDeployment(), c.artifact).Spec.Containers[0].Resources

			limit := res.Limits[core.ResourceEphemeralStorage]
			assert.Zero(t, limit.Cmp(resource.MustParse(c.wantLimit)), limit.String())
			request := res.Requests[core.ResourceEphemeralStorage]
			assert.Zero(t, request.Cmp(resource.MustParse("15Gi")), "the request is unchanged: %s", request.String())
		})
	}
}

func TestRenderModelDeploymentArtifactNodeVolume(t *testing.T) {
	for _, takeOver := range []bool{false, true} {
		t.Run(fmt.Sprintf("take-over %v", takeOver), func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				if takeOver {
					md.Spec.Roles[0].Command = []string{"sh", "-c", "serve"}
				}
			})
			pod := renderWithArtifact(t, md, testNodeArtifactRender())
			main := &pod.Spec.Containers[0]

			vol, mount := findVolume(pod, modelDeploymentModelVolumeName), findMount(main, modelDeploymentModelVolumeName)
			require.NotNil(t, vol, "the take-over tier gets the mount as well")
			require.NotNil(t, vol.CSI)
			assert.Equal(t, "model.csi.gpustack.ai", vol.CSI.Driver)
			assert.True(t, *vol.CSI.ReadOnly)
			assert.Equal(t, map[string]string{
				"artifact": "qwen", "artifactUID": "uid-qwen", "manifestDigest": testArtifactDigest,
			}, vol.CSI.VolumeAttributes)
			require.NotNil(t, vol.CSI.NodePublishSecretRef)
			assert.Equal(t, "hf-token", vol.CSI.NodePublishSecretRef.Name)
			require.NotNil(t, mount)
			assert.Equal(t, ModelDeploymentModelMountPath, mount.MountPath)
			assert.True(t, mount.ReadOnly)
			assert.Empty(t, mount.SubPath)
			assert.Nil(t, findVolume(pod, modelDeploymentModelCacheVolumeName), "no engine cache")
		})
	}
	t.Run("a public artifact names no Secret", func(t *testing.T) {
		a := testNodeArtifactRender()
		a.SecretName = ""
		vol := findVolume(renderWithArtifact(t, newRenderDeployment(), a), modelDeploymentModelVolumeName)
		require.NotNil(t, vol)
		assert.Nil(t, vol.CSI.NodePublishSecretRef)
	})
}
