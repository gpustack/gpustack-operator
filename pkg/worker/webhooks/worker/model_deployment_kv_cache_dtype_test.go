package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
)

// TestValidateModelDeploymentKVCacheDtype pins which role arguments naming the KV cache dtype are
// refused: only on a pool-attached role whose command line the operator builds, and only while the
// operator owns the flag.
func TestValidateModelDeploymentKVCacheDtype(t *testing.T) {
	withArgs := func(engine string, args ...string) *workercore.ModelDeployment {
		return modelDeployment(engine, role(func(r *workercore.ModelDeploymentRole) { r.ExtraArgs = args }))
	}

	cases := []struct {
		name      string
		md        *workercore.ModelDeployment
		unowned   bool
		wantField string
		wantValue string
	}{
		{
			name:      "vllm, the flag spelled out",
			md:        withArgs(workercore.ModelDeploymentEngineVLLM, "--max-model-len=4096", "--kv-cache-dtype", "fp8"),
			wantField: "spec.roles[0].extraArgs[1]",
			wantValue: "--kv-cache-dtype",
		},
		{
			name:      "vllm, its underscore spelling",
			md:        withArgs(workercore.ModelDeploymentEngineVLLM, "--kv_cache_dtype=fp8"),
			wantField: "spec.roles[0].extraArgs[0]",
			wantValue: "--kv_cache_dtype=fp8",
		},
		{
			name:      "vllm, an abbreviation argparse resolves",
			md:        withArgs(workercore.ModelDeploymentEngineVLLM, "--kv-cache-dt=fp8"),
			wantField: "spec.roles[0].extraArgs[0]",
			wantValue: "--kv-cache-dt=fp8",
		},
		{
			name:      "sglang",
			md:        withArgs(workercore.ModelDeploymentEngineSGLang, "--kv-cache-dtype=fp8_e4m3"),
			wantField: "spec.roles[0].extraArgs[0]",
			wantValue: "--kv-cache-dtype=fp8_e4m3",
		},
		{
			// The baseline every refusal above is measured against: the same role on the same
			// pool, with another flag of the same stem.
			name: "a longer flag of the same stem is the role's own",
			md:   withArgs(workercore.ModelDeploymentEngineVLLM, "--kv-cache-dtype-skip-layers", "0"),
		},
		{
			name: "no pool attached",
			md: func() *workercore.ModelDeployment {
				md := withArgs(workercore.ModelDeploymentEngineVLLM, "--kv-cache-dtype=fp8")
				md.Spec.KVCache = nil
				return md
			}(),
		},
		{
			name: "a role that replaced its command line",
			md: modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.Command = []string{"vllm", "serve", "--kv-cache-dtype=fp8"}
				r.ExtraArgs = []string{"--kv-cache-dtype=fp8"}
			})),
		},
		{
			name: "a deployment being deleted",
			md: func() *workercore.ModelDeployment {
				md := withArgs(workercore.ModelDeploymentEngineVLLM, "--kv-cache-dtype=fp8")
				md.DeletionTimestamp = &meta.Time{}
				return md
			}(),
		},
		{
			name:    "the setting off",
			md:      withArgs(workercore.ModelDeploymentEngineVLLM, "--kv-cache-dtype=fp8"),
			unowned: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateModelDeploymentKVCacheDtype(tc.md, !tc.unowned)
			if tc.wantField == "" {
				assert.Empty(t, errs)

				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, tc.wantField, errs[0].Field)
			assert.Equal(t, tc.wantValue, errs[0].BadValue)
			assert.Contains(t, errs[0].Detail, `"--kv-cache-dtype" is set by the operator while spec.kvCache names a pool`)
		})
	}
}

// TestModelDeploymentWebhook_KVCacheDtypeReadsTheSetting runs the whole handler, so a rule that
// stopped being called, or stopped reading the setting, fails here rather than passing its own
// table.
func TestModelDeploymentWebhook_KVCacheDtypeReadsTheSetting(t *testing.T) {
	const refusal = `"--kv-cache-dtype" is set by the operator`

	cases := []struct {
		name       string
		owned      string
		update     bool
		wantRefuse bool
	}{
		{name: "create, the setting unset reads its default", wantRefuse: true},
		{name: "create, the setting on", owned: "true", wantRefuse: true},
		{name: "update, the setting on", owned: "true", update: true, wantRefuse: true},
		{name: "create, the setting off", owned: "false"},
		{name: "update, the setting off", owned: "false", update: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.owned != "" {
				settingtest.MergeDelegatedSettings(t, map[string]string{
					"model-deployment-kv-cache-dtype-owned": tc.owned,
				})
			}
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{servingInstanceType("h20-8x", 8)})
			md := modelDeployment(workercore.ModelDeploymentEngineVLLM, role(func(r *workercore.ModelDeploymentRole) {
				r.ExtraArgs = []string{"--kv-cache-dtype=fp8"}
			}))

			var err error
			if tc.update {
				_, err = w.ValidateUpdate(context.Background(), md.DeepCopy(), md)
			} else {
				_, err = w.ValidateCreate(context.Background(), md)
			}
			if tc.wantRefuse {
				require.Error(t, err)
				assert.Contains(t, err.Error(), refusal)

				return
			}
			assert.NoError(t, err)
		})
	}
}
