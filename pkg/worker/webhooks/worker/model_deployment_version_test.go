package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
)

func TestModelDeploymentWebhook_DefaultEngineVersions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		md            func() *workercore.ModelDeployment
		wantEngineVer string
		wantRoleVers  []string
	}{
		{
			name: "backfills spec.engine.version from gpustack/runner tag",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "0.34.0",
			wantRoleVers:  []string{""},
		},
		{
			name: "backfills role override and leaves role without image empty to inherit",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill"
						r.Kind = workercore.ModelDeploymentRoleKindPrefill
						r.Image = "gpustack/runner:cann9.2-a3-vllm0.32.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode"
						r.Kind = workercore.ModelDeploymentRoleKindDecode
						r.Resources = cards(1)
					}),
				)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = ""
				return md
			},
			wantEngineVer: "0.34.0",
			wantRoleVers:  []string{"0.32.0", ""},
		},
		{
			name: "never overwrites user-supplied versions",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Roles[0].Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Roles[0].EngineVersion = "0.31.0"
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "0.32.0",
			wantRoleVers:  []string{"0.31.0"},
		},
		{
			name: "leaves version empty for custom, digest-only, or mismatched-engine images",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "digest"
						r.Image = "gpustack/runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "wrong-engine"
						r.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
						r.Resources = cards(1)
					}),
				)
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				return md
			},
			wantEngineVer: "",
			wantRoleVers:  []string{"", ""},
		},
		{
			name: "backfills uniformly when version is cleared on same runner image",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Roles[0].EngineVersion = ""
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "0.32.0",
			wantRoleVers:  []string{"0.34.0"},
		},
		{
			name: "deleting deployment skips version defaulting",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				now := meta.Now()
				md.DeletionTimestamp = &now
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "",
			wantRoleVers:  []string{""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
			md := tc.md()

			require.NoError(t, w.Default(context.Background(), md))
			assert.Equal(t, tc.wantEngineVer, md.Spec.Engine.Version)
			gotRoles := make([]string, len(md.Spec.Roles))
			for i := range md.Spec.Roles {
				gotRoles[i] = md.Spec.Roles[i].EngineVersion
			}
			assert.Equal(t, tc.wantRoleVers, gotRoles)
		})
	}
}

func TestModelDeploymentWebhook_ValidateEngineVersions(t *testing.T) {
	t.Parallel()

	newValid := func(mutate func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
		md.Spec.Engine.Version = "0.34.0"
		md.Spec.Roles[0].Resources = cards(1)
		if mutate != nil {
			mutate(md)
		}
		return md
	}

	cases := []struct {
		name      string
		old       *workercore.ModelDeployment
		md        *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "create with matching runner tag and version is admitted",
			md:   newValid(nil),
		},
		{
			name: "create with custom image and explicit version is admitted",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = "0.34.0"
			}),
		},
		{
			name: "create with custom image and empty version is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
			}),
			wantField: "spec.engine.version",
			wantMsg:   `cannot determine vLLM engine version from image "my-reg/vllm:custom"`,
		},
		{
			name: "create with custom role image and empty versions is refused on role engineVersion",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Image = "my-reg/vllm:role-custom"
			}),
			wantField: "spec.roles[0].engineVersion",
			wantMsg:   `cannot determine vLLM engine version from image "my-reg/vllm:role-custom"`,
		},
		{
			name: "create with parseable runner engine image and custom role image without role engineVersion is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "0.34.0"
				md.Spec.Roles[0].Image = "my-reg/vllm-ascend:0.23.0"
				md.Spec.Roles[0].EngineVersion = ""
			}),
			wantField: "spec.roles[0].engineVersion",
			wantMsg:   `cannot determine vLLM engine version from image "my-reg/vllm-ascend:0.23.0"`,
		},
		{
			name: "create with unmanaged role and empty version is admitted",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Command = []string{"python", "-m", "vllm"}
			}),
		},
		{
			name: "create with invalid spec.engine.version syntax is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = "latest"
			}),
			wantField: "spec.engine.version",
			wantMsg:   "must match major.minor",
		},
		{
			name: "create with invalid role engineVersion syntax is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].EngineVersion = "v0.34.0"
			}),
			wantField: "spec.roles[0].engineVersion",
			wantMsg:   "must match major.minor",
		},
		{
			name: "create with runner image disagreeing with spec.engine.version is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "0.32.0"
			}),
			wantField: "spec.engine.version",
			wantMsg:   "disagrees with runner image",
		},
		{
			name: "create with role runner image disagreeing with role engineVersion is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Image = "gpustack/runner:cann9.2-a3-vllm0.32.0"
				md.Spec.Roles[0].EngineVersion = "0.30.0"
			}),
			wantField: "spec.roles[0].engineVersion",
			wantMsg:   "disagrees with runner image",
		},
		{
			name: "create with inherited runner image disagreeing with role engineVersion is refused",
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "0.34.0"
				md.Spec.Roles[0].Image = ""
				md.Spec.Roles[0].EngineVersion = "0.32.0"
			}),
			wantField: "spec.roles[0].engineVersion",
			wantMsg:   "disagrees with runner image",
		},
		{
			name: "update changing spec.engine.image while leaving stale spec.engine.version is refused",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Engine.Version = "0.32.0"
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "0.32.0"
			}),
			wantField: "spec.engine.version",
			wantMsg:   "update or clear spec.engine.version alongside spec.engine.image",
		},
		{
			name: "update with empty version on managed custom image is refused uniformly",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Replicas = 8
			}),
			wantField: "spec.engine.version",
			wantMsg:   `cannot determine vLLM engine version from image "my-reg/vllm:custom"`,
		},
		{
			name: "update converting unmanaged role to managed requires version",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Command = []string{"python", "-m", "vllm"}
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Command = nil
			}),
			wantField: "spec.engine.version",
			wantMsg:   `cannot determine vLLM engine version from image "my-reg/vllm:custom"`,
		},
		{
			name: "update with DeletionTimestamp short-circuits version and router checks",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:custom"
				md.Spec.Engine.Version = ""
				md.Spec.Router = &workercore.ModelDeploymentRouter{
					Name:      workercore.ModelDeploymentRouterSGL,
					ExtraArgs: []string{"--host=0.0.0.0"},
				}
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				now := meta.Now()
				md.DeletionTimestamp = &now
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "not-even-valid"
				md.Spec.Router = &workercore.ModelDeploymentRouter{
					Name:      workercore.ModelDeploymentRouterSGL,
					ExtraArgs: []string{"--host=0.0.0.0"},
				}
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
			var err error
			if tc.old == nil {
				_, err = w.ValidateCreate(context.Background(), tc.md)
			} else {
				_, err = w.ValidateUpdate(context.Background(), tc.old, tc.md)
			}
			if tc.wantMsg == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
			if tc.wantField != "" {
				assertInvalidField(t, err, tc.wantField)
			}
		})
	}
}

func TestModelDeploymentWebhook_UpdateDefaultAndValidateRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("updating runner image with cleared version backfills new version", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		old.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
		old.Spec.Engine.Version = "0.32.0"
		old.Spec.Roles[0].Resources = cards(1)

		updated := old.DeepCopy()
		updated.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
		updated.Spec.Engine.Version = ""

		require.NoError(t, w.Default(context.Background(), updated))
		assert.Equal(t, "0.34.0", updated.Spec.Engine.Version)
		_, err := w.ValidateUpdate(context.Background(), old, updated)
		assert.NoError(t, err)
	})

	t.Run("clearing spec.engine.version on unchanged runner image re-backfills on update", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		old.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
		old.Spec.Engine.Version = "0.34.0"
		old.Spec.Roles[0].Resources = cards(1)

		updated := old.DeepCopy()
		updated.Spec.Engine.Version = ""

		require.NoError(t, w.Default(context.Background(), updated))
		assert.Equal(t, "0.34.0", updated.Spec.Engine.Version)
		_, err := w.ValidateUpdate(context.Background(), old, updated)
		assert.NoError(t, err)
	})

	t.Run("clearing roles[i].image and roles[i].engineVersion inherits runner spec.engine.version", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		old.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
		old.Spec.Engine.Version = ""
		old.Spec.Roles[0].Image = "gpustack/runner:cuda13.0-vllm0.32.0"
		old.Spec.Roles[0].EngineVersion = "0.32.0"
		old.Spec.Roles[0].Resources = cards(1)

		updated := old.DeepCopy()
		updated.Spec.Roles[0].Image = ""
		updated.Spec.Roles[0].EngineVersion = ""

		require.NoError(t, w.Default(context.Background(), updated))
		assert.Equal(t, "0.34.0", updated.Spec.Engine.Version)
		assert.Equal(t, "", updated.Spec.Roles[0].EngineVersion)
		_, err := w.ValidateUpdate(context.Background(), old, updated)
		assert.NoError(t, err)
	})

	t.Run("removing roles[i].command on runner image backfills version on update", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		old.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
		old.Spec.Engine.Version = ""
		old.Spec.Roles[0].Command = []string{"vllm", "serve"}
		old.Spec.Roles[0].Resources = cards(1)

		updated := old.DeepCopy()
		updated.Spec.Roles[0].Command = nil

		require.NoError(t, w.Default(context.Background(), updated))
		assert.Equal(t, "0.34.0", updated.Spec.Engine.Version)
		_, err := w.ValidateUpdate(context.Background(), old, updated)
		assert.NoError(t, err)
	})
}

func TestModelDeploymentWebhook_SGLRouterOwnedArgsAndMatrix(t *testing.T) {
	t.Parallel()

	for _, flag := range router.SGLOwnedFlags() {
		t.Run("owned_flag_"+flag, func(t *testing.T) {
			t.Parallel()
			md := modelDeployment(workercore.ModelDeploymentEngineSGLang)
			md.Spec.Engine.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
			md.Spec.Engine.Version = "0.5.23"
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name:      workercore.ModelDeploymentRouterSGL,
				ExtraArgs: []string{flag + "=val"},
			}
			errs := validateModelDeploymentRouter(md)
			require.NotEmpty(t, errs, "flag %s must be owned by sgl-router", flag)
			assert.Contains(t, errs.ToAggregate().Error(), "is set by the operator for router")
		})
	}

	t.Run("non-owned sgl-router flags are admitted", func(t *testing.T) {
		t.Parallel()
		md := modelDeployment(workercore.ModelDeploymentEngineSGLang)
		md.Spec.Engine.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
		md.Spec.Engine.Version = "0.5.23"
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:      workercore.ModelDeploymentRouterSGL,
			ExtraArgs: []string{"--policy=cache_aware", "--tokenizer-path=/models", "--log-level=debug"},
		}
		assert.Empty(t, validateModelDeploymentRouter(md))
	})

	t.Run("sgl-router rejects disaggregationThresholdTokens", func(t *testing.T) {
		t.Parallel()
		md := modelDeployment(workercore.ModelDeploymentEngineSGLang)
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:                          workercore.ModelDeploymentRouterSGL,
			DisaggregationThresholdTokens: ptr.To(int32(128)),
		}
		errs := validateModelDeploymentRouter(md)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "has no prompt-token threshold to set")
	})

	t.Run("sgl-router rejects vLLM engine", func(t *testing.T) {
		t.Parallel()
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name: workercore.ModelDeploymentRouterSGL,
		}
		errs := validateModelDeploymentRouter(md)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), `router "sgl-router" does not front engine "vLLM"`)
	})

	t.Run("managed SGLang role rejects --kv-events-config in extraArgs", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		md := modelDeployment(workercore.ModelDeploymentEngineSGLang)
		md.Spec.Engine.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
		md.Spec.Engine.Version = "0.5.23"
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
		md.Spec.Roles[0].Resources = cards(1)
		md.Spec.Roles[0].ExtraArgs = []string{`--kv-events-config={"publisher":"zmq"}`}
		_, err := w.ValidateCreate(context.Background(), md)
		require.Error(t, err)
		assertInvalidField(t, err, "spec.roles[0].extraArgs[0]")
		assert.Contains(t, err.Error(), `"--kv-events-config"`)
	})
}

func TestModelDeploymentWebhook_VLLMRouterKVAware(t *testing.T) {
	t.Parallel()

	makeServer := func(version string, routerArgs, roleArgs []string, roleEnv []workercore.ModelDeploymentEnvVar) *workercore.ModelDeployment {
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		md.Spec.Engine.Version = version
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:      workercore.ModelDeploymentRouterVLLM,
			ExtraArgs: routerArgs,
		}
		md.Spec.Roles[0].Resources = cards(1)
		md.Spec.Roles[0].ExtraArgs = roleArgs
		md.Spec.Roles[0].Env = roleEnv
		return md
	}

	makePD := func(prefillVer, decodeVer string, routerArgs []string) *workercore.ModelDeployment {
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "prefill"
				r.Kind = workercore.ModelDeploymentRoleKindPrefill
				r.EngineVersion = prefillVer
				r.Resources = cards(1)
			}),
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "decode"
				r.Kind = workercore.ModelDeploymentRoleKindDecode
				r.EngineVersion = decodeVer
				r.Resources = cards(1)
			}),
		)
		md.Spec.Engine.Version = ""
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:      workercore.ModelDeploymentRouterVLLM,
			ExtraArgs: routerArgs,
		}
		return md
	}

	cases := []struct {
		name      string
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "default cache_aware allows vLLM < 0.31.0 and user hash flag/env",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.29.0", nil,
					[]string{"--prefix-caching-hash-algo=sha256"},
					[]workercore.ModelDeploymentEnvVar{{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"}})
			},
		},
		{
			name: "server kv_aware admits vLLM 0.31.0",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.31.0", []string{"--policy", "kv_aware"}, nil, nil)
			},
		},
		{
			name: "server kv_aware admits vLLM 0.31.0rc1",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.31.0rc1", []string{"--policy=kv_aware"}, nil, nil)
			},
		},
		{
			name: "server kv_aware refuses vLLM 0.30.0",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.30.0", []string{"--policy=kv_aware"}, nil, nil)
			},
			wantField: "spec.engine.version",
			wantMsg:   "requires vLLM >= 0.31.0",
		},
		{
			name: "server kv_aware refuses empty legacy version",
			md: func() *workercore.ModelDeployment {
				return makeServer("", []string{"--policy=kv_aware"}, nil, nil)
			},
			wantField: "spec.engine.version",
			wantMsg:   "requires vLLM >= 0.31.0",
		},
		{
			name: "server kv_aware refuses --prefix-caching-hash-algo",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.32.0", []string{"--policy=kv_aware"},
					[]string{"--prefix-caching-hash-algo=sha256"}, nil)
			},
			wantField: "spec.roles[0].extraArgs[0]",
			wantMsg:   "is set by the operator for engine",
		},
		{
			name: "server kv_aware refuses underscore spelling --prefix_caching_hash_algo",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.32.0", []string{"--policy=kv_aware"},
					[]string{"--prefix_caching_hash_algo", "sha256"}, nil)
			},
			wantField: "spec.roles[0].extraArgs[0]",
			wantMsg:   "is set by the operator for engine",
		},
		{
			name: "server kv_aware refuses VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES",
			md: func() *workercore.ModelDeployment {
				return makeServer("0.32.0", []string{"--policy=kv_aware"}, nil,
					[]workercore.ModelDeploymentEnvVar{{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"}})
			},
			wantField: "spec.roles[0].env[0]",
			wantMsg:   "is set by the operator for engine",
		},
		{
			name: "server kv_aware exempts unmanaged role with command",
			md: func() *workercore.ModelDeployment {
				md := makeServer("0.29.0", []string{"--policy=kv_aware"},
					[]string{"--prefix-caching-hash-algo=sha256"},
					[]workercore.ModelDeploymentEnvVar{{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"}})
				md.Spec.Roles[0].Command = []string{"vllm", "serve"}
				return md
			},
		},
		{
			name: "PD prefill-only kv_aware allows decode < 0.31.0 and decode hash overrides",
			md: func() *workercore.ModelDeployment {
				md := makePD("0.31.0", "0.30.0", []string{"--prefill-policy", "kv_aware"})
				md.Spec.Roles[1].ExtraArgs = []string{"--prefix-caching-hash-algo=sha256"}
				md.Spec.Roles[1].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"},
				}
				return md
			},
		},
		{
			name: "PD decode-only kv_aware never gates decode version or decode hash overrides",
			md: func() *workercore.ModelDeployment {
				md := makePD("0.30.0", "0.30.0", []string{"--decode-policy", "kv_aware"})
				md.Spec.Roles[1].ExtraArgs = []string{"--prefix-caching-hash-algo=sha256"}
				md.Spec.Roles[1].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"},
				}
				return md
			},
		},
		{
			name: "PD --policy kv_aware exempts decode (< 0.31.0 and hash override allowed)",
			md: func() *workercore.ModelDeployment {
				md := makePD("0.32.0", "0.29.0", []string{"--policy", "kv_aware"})
				md.Spec.Roles[1].ExtraArgs = []string{"--prefix-caching-hash-algo=sha256"}
				md.Spec.Roles[1].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"},
				}
				return md
			},
		},
		{
			name: "PD --policy kv_aware enforces >= 0.31.0 on prefill",
			md: func() *workercore.ModelDeployment {
				return makePD("0.30.0", "0.32.0", []string{"--policy", "kv_aware"})
			},
			wantField: "spec.roles[0].engineVersion",
			wantMsg:   `kv_aware routing on role "prefill" requires vLLM >= 0.31.0`,
		},
		{
			name: "PD --policy kv_aware refuses prefill hash override",
			md: func() *workercore.ModelDeployment {
				md := makePD("0.32.0", "0.29.0", []string{"--policy", "kv_aware"})
				md.Spec.Roles[0].ExtraArgs = []string{"--prefix-caching-hash-algo=sha256"}
				return md
			},
			wantField: "spec.roles[0].extraArgs[0]",
			wantMsg:   "is set by the operator for engine",
		},
		{
			name: "DeletionTimestamp short-circuits kv_aware checks",
			md: func() *workercore.ModelDeployment {
				md := makeServer("0.29.0", []string{"--policy=kv_aware"},
					[]string{"--prefix-caching-hash-algo=sha256"},
					[]workercore.ModelDeploymentEnvVar{{Name: "VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES", Value: "1"}})
				now := meta.Now()
				md.DeletionTimestamp = &now
				return md
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := validateModelDeploymentVLLMRouterKVAware(tc.md())
			if tc.wantMsg == "" {
				assert.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tc.wantMsg)
			if tc.wantField != "" {
				var fields []string
				for _, e := range errs {
					fields = append(fields, e.Field)
				}
				assert.Contains(t, fields, tc.wantField)
			}
		})
	}
}

func TestModelDeploymentWebhook_AscendPDMixedV1V2Guard(t *testing.T) {
	t.Parallel()

	buildPD := func(accType, engineVer, prefillVer, decodeVer string) *workercore.ModelDeployment {
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "prefill"
				r.Kind = workercore.ModelDeploymentRoleKindPrefill
				r.AcceleratorType = accType
				r.EngineVersion = prefillVer
				r.Resources = cards(1)
			}),
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "decode"
				r.Kind = workercore.ModelDeploymentRoleKindDecode
				r.AcceleratorType = accType
				r.EngineVersion = decodeVer
				r.Resources = cards(1)
			}),
		)
		md.Spec.Engine.Version = engineVer
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		return md
	}

	cases := []struct {
		name       string
		md         func() *workercore.ModelDeployment
		wantErr    string
		wantFields []string
	}{
		{
			name:    "Ascend P/D mixing 0.32.0 (V2) and 0.29.0 (V1) is refused",
			md:      func() *workercore.ModelDeployment { return buildPD("ascend-8x", "", "0.32.0", "0.29.0") },
			wantErr: "cannot mix MooncakeConnectorV1 (< 0.30.0) and MooncakeConnectorV2 (>= 0.30.0)",
		},
		{
			name:    "Ascend P/D mixing 0.23.0 (V1) and 0.30.0rc1 (V2) is refused",
			md:      func() *workercore.ModelDeployment { return buildPD("ascend-8x", "", "0.23.0", "0.30.0rc1") },
			wantErr: "cannot mix MooncakeConnectorV1 (< 0.30.0) and MooncakeConnectorV2 (>= 0.30.0)",
		},
		{
			name:       "Ascend P/D mixing unset (V1) and 0.30.0 (V2) is refused",
			md:         func() *workercore.ModelDeployment { return buildPD("ascend-8x", "", "", "0.30.0") },
			wantErr:    "cannot mix MooncakeConnectorV1 (< 0.30.0) and MooncakeConnectorV2 (>= 0.30.0)",
			wantFields: []string{"spec.engine.version", "spec.roles"},
		},
		{
			name: "Ascend P/D both V2 (0.30.0rc1 and 0.32.0) is admitted",
			md:   func() *workercore.ModelDeployment { return buildPD("ascend-8x", "", "0.30.0rc1", "0.32.0") },
		},
		{
			name: "Ascend P/D both V1 (0.23.0 and 0.29.0) is admitted",
			md:   func() *workercore.ModelDeployment { return buildPD("ascend-8x", "", "0.23.0", "0.29.0") },
		},
		{
			name: "NVIDIA P/D mixing 0.32.0 and 0.29.0 is admitted",
			md:   func() *workercore.ModelDeployment { return buildPD("nvidia-8x", "", "0.32.0", "0.29.0") },
		},
		{
			name: "Ascend P/D with unmanaged decode is admitted",
			md: func() *workercore.ModelDeployment {
				md := buildPD("ascend-8x", "", "0.32.0", "0.29.0")
				md.Spec.Roles[1].Command = []string{"vllm", "serve"}
				return md
			},
		},
		{
			name: "Ascend P/D with DeletionTimestamp short-circuits",
			md: func() *workercore.ModelDeployment {
				md := buildPD("ascend-8x", "", "0.32.0", "0.29.0")
				now := meta.Now()
				md.DeletionTimestamp = &now
				return md
			},
		},
		{
			name: "Ascend multi-role P/D catches mismatch on earlier prefill-a (0.29.0) before prefill-b (0.30.0)",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill-a"
						r.Kind = workercore.ModelDeploymentRoleKindPrefill
						r.AcceleratorType = "ascend-8x"
						r.EngineVersion = "0.29.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill-b"
						r.Kind = workercore.ModelDeploymentRoleKindPrefill
						r.AcceleratorType = "ascend-8x"
						r.EngineVersion = "0.30.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode"
						r.Kind = workercore.ModelDeploymentRoleKindDecode
						r.AcceleratorType = "ascend-8x"
						r.EngineVersion = "0.30.0"
						r.Resources = cards(1)
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			},
			wantErr:    `across roles "prefill-a" (0.29.0) and "decode" (0.30.0)`,
			wantFields: []string{"spec.roles[1].kind", "spec.roles"},
		},
		{
			name: "Ascend multi-role P/D catches mismatch on earlier decode-a (0.29.0) before decode-b (0.30.0)",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill"
						r.Kind = workercore.ModelDeploymentRoleKindPrefill
						r.AcceleratorType = "ascend-8x"
						r.EngineVersion = "0.30.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode-a"
						r.Kind = workercore.ModelDeploymentRoleKindDecode
						r.AcceleratorType = "ascend-8x"
						r.EngineVersion = "0.29.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode-b"
						r.Kind = workercore.ModelDeploymentRoleKindDecode
						r.AcceleratorType = "ascend-8x"
						r.EngineVersion = "0.30.0"
						r.Resources = cards(1)
					}),
				)
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				return md
			},
			wantErr:    `across roles "prefill" (0.30.0) and "decode-a" (0.29.0)`,
			wantFields: []string{"spec.roles[2].kind", "spec.roles"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newModelDeploymentWebhookWith(epdPlatformTypes())
			md := tc.md()
			_, err := w.ValidateCreate(context.Background(), md)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			if len(tc.wantFields) == 0 {
				assertInvalidField(t, err, "spec.roles")
				return
			}
			for _, f := range tc.wantFields {
				assert.Contains(t, err.Error(), f)
			}
		})
	}
}

func TestModelDeploymentWebhook_KVEventsPortReservations(t *testing.T) {
	t.Parallel()

	makeRoles := func(kinds ...workercore.ModelDeploymentRoleKind) []workercore.ModelDeploymentRole {
		roles := make([]workercore.ModelDeploymentRole, 0, len(kinds))
		for _, k := range kinds {
			name := string(k)
			if name == "" {
				name = "server"
			}
			roles = append(roles, role(func(r *workercore.ModelDeploymentRole) {
				r.Name = name
				r.Kind = k
				r.Resources = cards(1)
			}))
		}
		return roles
	}

	cases := []struct {
		name         string
		engine       string
		routerName   string
		routerArgs   []string
		kinds        []workercore.ModelDeploymentRoleKind
		wantReserved map[string]bool
	}{
		{
			name:       "llm-d-router + vLLM reserves on Server",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterLLMD,
			kinds:      []workercore.ModelDeploymentRoleKind{workercore.ModelDeploymentRoleKindServer},
			wantReserved: map[string]bool{
				"Server": true,
			},
		},
		{
			name:       "llm-d-router + vLLM EPD reserves on Prefill only",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterLLMD,
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Encode":  false,
				"Prefill": true,
				"Decode":  false,
			},
		},
		{
			name:       "llm-d-router + SGLang reserves on no role",
			engine:     workercore.ModelDeploymentEngineSGLang,
			routerName: workercore.ModelDeploymentRouterLLMD,
			kinds:      []workercore.ModelDeploymentRoleKind{workercore.ModelDeploymentRoleKindServer},
			wantReserved: map[string]bool{
				"Server": false,
			},
		},
		{
			name:       "sgl-router + SGLang reserves on Server",
			engine:     workercore.ModelDeploymentEngineSGLang,
			routerName: workercore.ModelDeploymentRouterSGL,
			kinds:      []workercore.ModelDeploymentRoleKind{workercore.ModelDeploymentRoleKindServer},
			wantReserved: map[string]bool{
				"Server": true,
			},
		},
		{
			name:       "sgl-router + SGLang EPD reserves on Prefill only",
			engine:     workercore.ModelDeploymentEngineSGLang,
			routerName: workercore.ModelDeploymentRouterSGL,
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Encode":  false,
				"Prefill": true,
				"Decode":  false,
			},
		},
		{
			name:       "sglang-gateway + SGLang reserves on no role",
			engine:     workercore.ModelDeploymentEngineSGLang,
			routerName: workercore.ModelDeploymentRouterSGLangGateway,
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Prefill": false,
				"Decode":  false,
			},
		},
		{
			name:       "vllm-router default cache_aware reserves on no role",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterVLLM,
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Prefill": false,
				"Decode":  false,
			},
		},
		{
			name:       "vllm-router Server --policy kv_aware reserves on Server",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterVLLM,
			routerArgs: []string{"--policy=kv_aware"},
			kinds:      []workercore.ModelDeploymentRoleKind{workercore.ModelDeploymentRoleKindServer},
			wantReserved: map[string]bool{
				"Server": true,
			},
		},
		{
			name:       "vllm-router PD --prefill-policy kv_aware reserves on Prefill only",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterVLLM,
			routerArgs: []string{"--prefill-policy", "kv_aware"},
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Prefill": true,
				"Decode":  false,
			},
		},
		{
			name:       "vllm-router PD --decode-policy kv_aware reserves on no role",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterVLLM,
			routerArgs: []string{"--decode-policy", "kv_aware"},
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Prefill": false,
				"Decode":  false,
			},
		},
		{
			name:       "vllm-router EPD --policy kv_aware reserves on Prefill only, not Encode or Decode",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterVLLM,
			routerArgs: []string{"--policy=kv_aware"},
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Encode":  false,
				"Prefill": true,
				"Decode":  false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := modelDeployment(tc.engine, makeRoles(tc.kinds...)...)
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name:      tc.routerName,
				ExtraArgs: tc.routerArgs,
			}
			for i := range md.Spec.Roles {
				r := &md.Spec.Roles[i]
				reserved := modelDeploymentRouterReservedPorts(md, r)
				want := tc.wantReserved[r.Name]
				if want {
					assert.Contains(t, reserved, inject.VLLMKVEventsPort, "role %s must reserve 5557", r.Name)
					assert.Contains(t, reserved, inject.VLLMKVEventsReplayPort, "role %s must reserve 5558", r.Name)
				} else {
					assert.NotContains(t, reserved, inject.VLLMKVEventsPort, "role %s must not reserve 5557", r.Name)
					assert.NotContains(t, reserved, inject.VLLMKVEventsReplayPort, "role %s must not reserve 5558", r.Name)
				}
			}
		})
	}

	t.Run("sgl-router admission refuses port 5557 on SGLang Server", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		md := modelDeployment(workercore.ModelDeploymentEngineSGLang)
		md.Spec.Engine.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
		md.Spec.Engine.Version = "0.5.23"
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
		md.Spec.Roles[0].Resources = cards(1)
		md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: inject.VLLMKVEventsPort, Protocol: core.ProtocolTCP}}
		_, err := w.ValidateCreate(context.Background(), md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "which the operator reserves")
	})

	t.Run("vllm-router --policy kv_aware refuses --port=5558 on Prefill and allows --port=5558 on Decode", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
		md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "prefill"
				r.Kind = workercore.ModelDeploymentRoleKindPrefill
				r.Resources = cards(1)
				r.ExtraArgs = []string{"--port=5558"}
			}),
			role(func(r *workercore.ModelDeploymentRole) {
				r.Name = "decode"
				r.Kind = workercore.ModelDeploymentRoleKindDecode
				r.Resources = cards(1)
				r.ExtraArgs = []string{"--port=5558"}
			}),
		)
		md.Spec.Engine.Version = "0.32.0"
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:      workercore.ModelDeploymentRouterVLLM,
			ExtraArgs: []string{"--policy", "kv_aware"},
		}
		_, err := w.ValidateCreate(context.Background(), md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "prefill" passes --port=5558`)
		assert.NotContains(t, err.Error(), `role "decode" passes --port=5558`)

		// With --decode-policy kv_aware alone, neither role publishes KV events so --port=5558 is allowed on both.
		mdDecodeOnly := md.DeepCopy()
		mdDecodeOnly.Spec.Router.ExtraArgs = []string{"--decode-policy", "kv_aware"}
		_, err = w.ValidateCreate(context.Background(), mdDecodeOnly)
		assert.NoError(t, err)
	})

	t.Run("multi-DP reserves full global rank span [5557..5557+2*total-1] across vLLM and SGLang while llm-d-router skips multi-Pod vLLM DP", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})

		// vLLM single-Pod --data-parallel-size 2 reserves 5557..5560.
		vllmDP2 := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		vllmDP2.Spec.Engine.Version = "0.32.0"
		vllmDP2.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		vllmDP2.Spec.Roles[0].Resources = cards(2)
		vllmDP2.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-size", "2"}
		assert.Equal(t, []int32{5557, 5558, 5559, 5560}, modelDeploymentRouterReservedPorts(vllmDP2, &vllmDP2.Spec.Roles[0]))
		vllmDP2.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 5560, Protocol: core.ProtocolTCP}}
		_, err := w.ValidateCreate(context.Background(), vllmDP2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "declares port 5560, which the operator reserves")

		// vLLM External DP (--data-parallel-size 4 --data-parallel-rank $(GPUSTACK_MEMBER_INDEX), replicaSize: 4)
		// and Hybrid DP (--data-parallel-size 4 --data-parallel-size-local 2 --data-parallel-hybrid-lb):
		// reserve 5557..5564 under vllm-router kv_aware, and reserve nothing under llm-d-router (NotApplicable).
		for _, shape := range []struct {
			name        string
			replicaSize int32
			extraArgs   []string
		}{
			{
				name:        "External DP",
				replicaSize: 4,
				extraArgs:   []string{"--data-parallel-size", "4", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"},
			},
			{
				name:        "Hybrid DP",
				replicaSize: 2,
				extraArgs:   []string{"--data-parallel-size", "4", "--data-parallel-size-local", "2", "--data-parallel-hybrid-lb"},
			},
		} {
			vllmKVAware := modelDeployment(workercore.ModelDeploymentEngineVLLM)
			vllmKVAware.Spec.Engine.Version = "0.32.0"
			vllmKVAware.Spec.Router = &workercore.ModelDeploymentRouter{
				Name:      workercore.ModelDeploymentRouterVLLM,
				ExtraArgs: []string{"--policy=kv_aware"},
			}
			vllmKVAware.Spec.Roles[0].ReplicaSize = shape.replicaSize
			vllmKVAware.Spec.Roles[0].Resources = cards(2)
			vllmKVAware.Spec.Roles[0].ExtraArgs = shape.extraArgs
			assert.Equal(t, []int32{5557, 5558, 5559, 5560, 5561, 5562, 5563, 5564},
				modelDeploymentRouterReservedPorts(vllmKVAware, &vllmKVAware.Spec.Roles[0]), shape.name)

			vllmLLMD := vllmKVAware.DeepCopy()
			vllmLLMD.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			assert.Empty(t, modelDeploymentRouterReservedPorts(vllmLLMD, &vllmLLMD.Spec.Roles[0]), shape.name)
		}

		// SGLang single-Pod --dp-size 2 and External DP --attn-dp-size 2 --nnodes 2 both reserve 5557..5560.
		sglDP2 := modelDeployment(workercore.ModelDeploymentEngineSGLang)
		sglDP2.Spec.Engine.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
		sglDP2.Spec.Engine.Version = "0.5.23"
		sglDP2.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
		sglDP2.Spec.Roles[0].Resources = cards(2)
		sglDP2.Spec.Roles[0].ExtraArgs = []string{"--dp-size", "2"}
		assert.Equal(t, []int32{5557, 5558, 5559, 5560}, modelDeploymentRouterReservedPorts(sglDP2, &sglDP2.Spec.Roles[0]))

		sglExtDP := sglDP2.DeepCopy()
		sglExtDP.Spec.Roles[0].ReplicaSize = 2
		sglExtDP.Spec.Roles[0].Resources = cards(1)
		sglExtDP.Spec.Roles[0].ExtraArgs = []string{"--attn-dp-size", "2", "--nnodes", "2"}
		sglExtDP.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_RUST_SERVER", Value: "1"}}
		assert.Equal(t, []int32{5557, 5558, 5559, 5560}, modelDeploymentRouterReservedPorts(sglExtDP, &sglExtDP.Spec.Roles[0]))

		// SGLang --dp-size 4 --nnodes 2 --pp-size 2 reserves 5557..5564 (never divides 4 by nnodes=2).
		sglPP := sglDP2.DeepCopy()
		sglPP.Spec.Roles[0].ReplicaSize = 2
		sglPP.Spec.Roles[0].Resources = cards(4)
		sglPP.Spec.Roles[0].ExtraArgs = []string{"--dp-size", "4", "--nnodes", "2", "--pp-size", "2"}
		assert.Equal(t, []int32{5557, 5558, 5559, 5560, 5561, 5562, 5563, 5564},
			modelDeploymentRouterReservedPorts(sglPP, &sglPP.Spec.Roles[0]))
	})

	t.Run("DeletionTimestamp skips new vllm-router and sgl-router KV event reservations", func(t *testing.T) {
		t.Parallel()
		now := meta.Now()

		vllmMD := modelDeployment(workercore.ModelDeploymentEngineVLLM)
		vllmMD.DeletionTimestamp = &now
		vllmMD.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:      workercore.ModelDeploymentRouterVLLM,
			ExtraArgs: []string{"--policy=kv_aware"},
		}
		assert.NotContains(t, modelDeploymentRouterReservedPorts(vllmMD, &vllmMD.Spec.Roles[0]), inject.VLLMKVEventsPort)

		sglMD := modelDeployment(workercore.ModelDeploymentEngineSGLang)
		sglMD.DeletionTimestamp = &now
		sglMD.Spec.Router = &workercore.ModelDeploymentRouter{
			Name: workercore.ModelDeploymentRouterSGL,
		}
		assert.NotContains(t, modelDeploymentRouterReservedPorts(sglMD, &sglMD.Spec.Roles[0]), inject.VLLMKVEventsPort)
	})
}

func TestModelDeploymentWebhook_EPDSideChannelPortOverrides(t *testing.T) {
	t.Parallel()

	row, admitted := workerctrl.ModelDeploymentEPDRowFor(
		workercore.ModelDeploymentRouterLLMD,
		workercore.ModelDeploymentEngineVLLM,
		nodefeature.ManufacturerAMD,
	)
	require.True(t, admitted)

	cases := []struct {
		name      string
		env       []workercore.ModelDeploymentEnvVar
		ports     []workercore.ModelDeploymentPort
		extraArgs []string
		wantField string
		wantMsg   string
	}{
		{
			name:      "unset env refuses HTTP on default 5601",
			ports:     []workercore.ModelDeploymentPort{{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP}},
			wantField: "spec.roles[0].ports",
			wantMsg:   "serves HTTP on port 5601",
		},
		{
			name:  "explicit 0 (ephemeral) skips 5601 collision check",
			env:   []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "0"}},
			ports: []workercore.ModelDeploymentPort{{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP}},
		},
		{
			name:  "explicit 5602 allows HTTP on 5601",
			env:   []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "5602"}},
			ports: []workercore.ModelDeploymentPort{{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP}},
		},
		{
			name:      "explicit 5602 refuses declared HTTP port 5602",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "5602"}},
			ports:     []workercore.ModelDeploymentPort{{Port: 5602, Protocol: core.ProtocolTCP}},
			wantField: "spec.roles[0].ports",
			wantMsg:   "serves HTTP on port 5602",
		},
		{
			name:      "explicit 5602 refuses --port=5602 in extraArgs",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "5602"}},
			extraArgs: []string{"--port=5602"},
			wantField: "spec.roles[0].extraArgs",
			wantMsg:   "serves HTTP on port 5602",
		},
		{
			name:      "empty literal is rejected cleanly",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: ""}},
			ports:     []workercore.ModelDeploymentPort{{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP}},
			wantField: "spec.roles[0].env[0].value",
			wantMsg:   "must be 0 (ephemeral) or a valid TCP port (1-65535)",
		},
		{
			name:      "non-numeric value is rejected cleanly",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "abc"}},
			wantField: "spec.roles[0].env[0].value",
			wantMsg:   "must be 0 (ephemeral) or a valid TCP port (1-65535)",
		},
		{
			name:      "out-of-range port 70000 is rejected cleanly",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "70000"}},
			wantField: "spec.roles[0].env[0].value",
			wantMsg:   "must be 0 (ephemeral) or a valid TCP port (1-65535)",
		},
		{
			name:      "negative port is rejected cleanly",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "-1"}},
			wantField: "spec.roles[0].env[0].value",
			wantMsg:   "must be 0 (ephemeral) or a valid TCP port (1-65535)",
		},
		{
			name:      "explicit 8000 refuses default HTTP port 8000 when ports and --port are omitted",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "8000"}},
			wantField: "spec.roles[0].env[0].value",
			wantMsg:   "serves HTTP on port 8000",
		},
		{
			name: "last env entry wins when duplicated",
			env: []workercore.ModelDeploymentEnvVar{
				{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: fmt.Sprintf("%d", inject.VLLMECSideChannelPort)},
				{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "0"},
			},
			ports: []workercore.ModelDeploymentPort{{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := epdNetworkRowDeployment(func(md *workercore.ModelDeployment) {
				enc := epdRoleByName(md, "encode")
				enc.Env = tc.env
				enc.Ports = tc.ports
				enc.ExtraArgs = tc.extraArgs
			})

			errs := validateModelDeploymentEPDSideChannelPort(md, row)
			if tc.wantMsg == "" {
				assert.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tc.wantMsg)
			if tc.wantField != "" {
				var fields []string
				for _, e := range errs {
					fields = append(fields, e.Field)
				}
				assert.Contains(t, fields, tc.wantField)
			}
		})
	}

	t.Run("full admission rejects Encode omitting ports and --port with VLLM_EC_SIDE_CHANNEL_PORT=8000", func(t *testing.T) {
		t.Parallel()
		w := newModelDeploymentWebhookWith(epdPlatformTypes())
		md := epdNetworkRowDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Version = "0.34.0"
			enc := epdRoleByName(md, "encode")
			enc.Ports = nil
			enc.ExtraArgs = nil
			enc.Env = []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "8000"}}
		})
		_, err := w.ValidateCreate(context.Background(), md)
		require.Error(t, err)
		assertInvalidField(t, err, "spec.roles[0].env[0].value")
		assert.Contains(t, err.Error(), "serves HTTP on port 8000")
	})
}

func TestModelDeploymentWebhook_LLMDRouterModelNameAtSign(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "llm-d-router + managed vLLM Server rejects @ in spec.model.name",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Model.Name = "org/qwen3@fp8"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantField: "spec.model.name",
			wantMsg:   `must not contain "@" when llm-d-router KV cache events are enabled`,
		},
		{
			name: "llm-d-router + SGLang allows @ in spec.model.name (KV events NotApplicable)",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineSGLang)
				md.Spec.Model.Name = "org/qwen3@fp8"
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-sglang0.5.23"
				md.Spec.Engine.Version = "0.5.23"
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
		},
		{
			name: "llm-d-router + unmanaged vLLM role allows @ in spec.model.name",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Model.Name = "org/qwen3@fp8"
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "org/qwen3@fp8"}
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
		},
		{
			name: "llm-d-router + multi-Pod vLLM External DP allows @ in spec.model.name (KV events disabled)",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Model.Name = "org/qwen3@fp8"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].Resources = cards(1)
				md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-size", "2", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
				return md
			},
		},
		{
			name: "vllm-router kv_aware allows @ in spec.model.name",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Model.Name = "org/qwen3@fp8"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Router = &workercore.ModelDeploymentRouter{
					Name:      workercore.ModelDeploymentRouterVLLM,
					ExtraArgs: []string{"--policy=kv_aware"},
				}
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
		},
		{
			name: "DeletionTimestamp short-circuits @ check",
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				now := meta.Now()
				md.DeletionTimestamp = &now
				md.Spec.Model.Name = "org/qwen3@fp8"
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
			_, err := w.ValidateCreate(context.Background(), tc.md())
			if tc.wantMsg == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assertInvalidField(t, err, tc.wantField)
		})
	}
}
