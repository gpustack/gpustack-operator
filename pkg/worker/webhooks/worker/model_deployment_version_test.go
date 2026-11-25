package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
)

func admissionUpdateContext(t *testing.T, old *workercore.ModelDeployment) context.Context {
	t.Helper()
	var raw []byte
	if old != nil {
		var err error
		raw, err = json.Marshal(old)
		require.NoError(t, err)
	}
	return ctrladmission.NewContextWithRequest(context.Background(), ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			OldObject: runtime.RawExtension{Raw: raw},
		},
	})
}

func TestModelDeploymentWebhook_DefaultEngineVersions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		old             *workercore.ModelDeployment
		useUpdateCtx    bool
		rawOldOverride  []byte
		md              func() *workercore.ModelDeployment
		wantEngineVer   string
		wantRoleVers    []string
		wantErrContains string
	}{
		{
			name: "create backfills spec.engine.version from gpustack/runner tag",
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
			name: "create backfills role override and leaves role without image empty to inherit",
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
			name: "create never overwrites user-supplied versions",
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
			name: "create leaves version empty for custom, digest-only, or mismatched-engine images",
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
			name:         "update scaling legacy versionless CR never backfills empty versions",
			useUpdateCtx: true,
			old: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Roles[0].EngineVersion = ""
				md.Spec.Roles[0].Resources = cards(1)
				return md
			}(),
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Roles[0].EngineVersion = ""
				md.Spec.Roles[0].Replicas = 6
				md.Spec.Roles[0].Pause = true
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "",
			wantRoleVers:  []string{""},
		},
		{
			name:         "update backfills spec.engine.version when spec.engine.image changes and version is cleared",
			useUpdateCtx: true,
			old: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Roles[0].Resources = cards(1)
				return md
			}(),
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
			name:         "update does not overwrite non-empty version when image changes",
			useUpdateCtx: true,
			old: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Roles[0].Resources = cards(1)
				return md
			}(),
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "0.32.0"
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "0.32.0",
			wantRoleVers:  []string{""},
		},
		{
			name:         "update matches roles by name and backfills only changed or newly added role images",
			useUpdateCtx: true,
			old: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill"
						r.Kind = workercore.ModelDeploymentRoleKindPrefill
						r.Image = "gpustack/runner:cuda13.0-vllm0.31.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode"
						r.Kind = workercore.ModelDeploymentRoleKindDecode
						r.Image = "gpustack/runner:cuda13.0-vllm0.31.0"
						r.Resources = cards(1)
					}),
				)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.31.0"
				md.Spec.Engine.Version = ""
				return md
			}(),
			md: func() *workercore.ModelDeployment {
				// Reorder roles: decode first (unchanged), prefill second (changed), new role third.
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM,
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "decode"
						r.Kind = workercore.ModelDeploymentRoleKindDecode
						r.Image = "gpustack/runner:cuda13.0-vllm0.31.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "prefill"
						r.Kind = workercore.ModelDeploymentRoleKindPrefill
						r.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
						r.Resources = cards(1)
					}),
					role(func(r *workercore.ModelDeploymentRole) {
						r.Name = "extra"
						r.Kind = workercore.ModelDeploymentRoleKindEncode
						r.Image = "gpustack/runner:cuda13.0-vllm0.33.0"
						r.Resources = cards(1)
					}),
				)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.31.0"
				md.Spec.Engine.Version = ""
				return md
			},
			wantEngineVer: "",
			wantRoleVers:  []string{"", "0.34.0", "0.33.0"},
		},
		{
			name:         "update with empty OldObject does not backfill",
			useUpdateCtx: true,
			old:          nil,
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantEngineVer: "",
			wantRoleVers:  []string{""},
		},
		{
			name:           "update with corrupt OldObject returns unmarshal error",
			useUpdateCtx:   true,
			rawOldOverride: []byte("{invalid"),
			md: func() *workercore.ModelDeployment {
				md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Resources = cards(1)
				return md
			},
			wantErrContains: "unmarshal old ModelDeployment",
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

			ctx := context.Background()
			if tc.useUpdateCtx {
				if tc.rawOldOverride != nil {
					ctx = ctrladmission.NewContextWithRequest(context.Background(), ctrladmission.Request{
						AdmissionRequest: admissionv1.AdmissionRequest{
							Operation: admissionv1.Update,
							OldObject: runtime.RawExtension{Raw: tc.rawOldOverride},
						},
					})
				} else {
					ctx = admissionUpdateContext(t, tc.old)
				}
			}

			err := w.Default(ctx, md)
			if tc.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrContains)
				return
			}
			require.NoError(t, err)
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
			name: "update scaling and pausing legacy versionless CR succeeds",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:legacy"
				md.Spec.Engine.Version = ""
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:legacy"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Replicas = 8
				md.Spec.Roles[0].Pause = true
			}),
		},
		{
			name: "update converting unmanaged legacy role to managed requires version",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:legacy"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Command = []string{"python", "-m", "vllm"}
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:legacy"
				md.Spec.Engine.Version = ""
				md.Spec.Roles[0].Command = nil
			}),
			wantField: "spec.engine.version",
			wantMsg:   `cannot determine vLLM engine version from image "my-reg/vllm:legacy"`,
		},
		{
			name: "update with DeletionTimestamp short-circuits version checks on legacy CR",
			old: newValid(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = "my-reg/vllm:legacy"
				md.Spec.Engine.Version = ""
			}),
			md: newValid(func(md *workercore.ModelDeployment) {
				now := meta.Now()
				md.DeletionTimestamp = &now
				md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
				md.Spec.Engine.Version = "not-even-valid"
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

func TestModelDeploymentWebhook_UpdateImageClearsVersionRoundTrip(t *testing.T) {
	t.Parallel()

	w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingAcceleratorType("h20-8x", 8)})
	old := modelDeployment(workercore.ModelDeploymentEngineVLLM)
	old.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.32.0"
	old.Spec.Engine.Version = "0.32.0"
	old.Spec.Roles[0].Resources = cards(1)

	updated := old.DeepCopy()
	updated.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
	updated.Spec.Engine.Version = ""

	require.NoError(t, w.Default(admissionUpdateContext(t, old), updated))
	assert.Equal(t, "0.34.0", updated.Spec.Engine.Version)
	_, err := w.ValidateUpdate(context.Background(), old, updated)
	assert.NoError(t, err)
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
			name: "PD decode-only kv_aware enforces >= 0.31.0 on decode",
			md: func() *workercore.ModelDeployment {
				return makePD("0.30.0", "0.30.0", []string{"--decode-policy", "kv_aware"})
			},
			wantField: "spec.roles[1].engineVersion",
			wantMsg:   `kv_aware routing on role "decode" requires vLLM >= 0.31.0`,
		},
		{
			name: "PD --policy kv_aware inherits onto decode and refuses decode hash override",
			md: func() *workercore.ModelDeployment {
				md := makePD("0.32.0", "0.32.0", []string{"--policy", "kv_aware"})
				md.Spec.Roles[1].ExtraArgs = []string{"--prefix-caching-hash-algo=sha256"}
				return md
			},
			wantField: "spec.roles[1].extraArgs[0]",
			wantMsg:   "is set by the operator for engine",
		},
		{
			name: "PD --policy kv_aware with --decode-policy cache_aware exempts decode",
			md: func() *workercore.ModelDeployment {
				md := makePD("0.32.0", "0.29.0", []string{"--policy", "kv_aware", "--decode-policy", "cache_aware"})
				md.Spec.Roles[1].ExtraArgs = []string{"--prefix-caching-hash-algo=sha256"}
				return md
			},
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
		name    string
		md      func() *workercore.ModelDeployment
		wantErr string
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
			name:    "Ascend P/D mixing legacy unset (V1) and 0.30.0 (V2) is refused",
			md:      func() *workercore.ModelDeployment { return buildPD("ascend-8x", "", "", "0.30.0") },
			wantErr: "cannot mix MooncakeConnectorV1 (< 0.30.0) and MooncakeConnectorV2 (>= 0.30.0)",
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newModelDeploymentWebhookWith(epdPlatformTypes())
			md := tc.md()
			// Validate via full ValidateUpdate with same old (for legacy empty-version test)
			// or ValidateCreate when all managed roles carry versions.
			var err error
			if md.Spec.Roles[0].EngineVersion == "" && md.Spec.Engine.Version == "" {
				_, err = w.ValidateUpdate(context.Background(), md.DeepCopy(), md)
			} else {
				_, err = w.ValidateCreate(context.Background(), md)
			}
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assertInvalidField(t, err, "spec.roles")
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
			name:       "vllm-router PD --decode-policy kv_aware reserves on Decode only",
			engine:     workercore.ModelDeploymentEngineVLLM,
			routerName: workercore.ModelDeploymentRouterVLLM,
			routerArgs: []string{"--decode-policy", "kv_aware"},
			kinds: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			wantReserved: map[string]bool{
				"Prefill": false,
				"Decode":  true,
			},
		},
		{
			name:       "vllm-router EPD --policy kv_aware reserves on Prefill and Decode, not Encode",
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
				"Decode":  true,
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

	t.Run("vllm-router --decode-policy kv_aware admission refuses --port=5558 on Decode", func(t *testing.T) {
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
			ExtraArgs: []string{"--decode-policy", "kv_aware"},
		}
		_, err := w.ValidateCreate(context.Background(), md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "decode" passes --port=5558`)
		assert.Contains(t, err.Error(), "which the operator reserves")
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
			name:      "empty literal falls back to default 5601 check",
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: ""}},
			ports:     []workercore.ModelDeploymentPort{{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP}},
			wantField: "spec.roles[0].ports",
			wantMsg:   "serves HTTP on port 5601",
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
}
