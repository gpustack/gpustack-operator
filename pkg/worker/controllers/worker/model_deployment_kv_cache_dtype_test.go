package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
)

// kvCacheDtypeEntries returns every command-line entry naming the KV cache dtype, each with its value
// joined on, in argv order: the order is what decides which one the engine keeps.
func kvCacheDtypeEntries(argv []string) []string {
	var entries []string
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "--kv-cache-dtype" && i+1 < len(argv):
			entries = append(entries, argv[i]+"="+argv[i+1])
			i++
		case strings.HasPrefix(argv[i], "--kv-cache-dtype="):
			entries = append(entries, argv[i])
		}
	}

	return entries
}

// TestModelDeploymentKVCacheDtype_ReachesEveryReplica runs the reconciler and reads the rendered
// Pods, because a synthesis-level case would stay green if the resolution stopped forwarding the
// Binding's dtype.
func TestModelDeploymentKVCacheDtype_ReachesEveryReplica(t *testing.T) {
	cases := []struct {
		name string
		// owned seeds the setting; empty leaves it unset, which reads its default.
		owned  string
		mutate func(*workercore.ModelDeployment)
		want   []string
	}{
		{
			name: "an unset setting reads its default and renders the Binding's dtype",
			want: []string{"--kv-cache-dtype=bfloat16"},
		},
		{
			name:  "the setting on renders the Binding's dtype",
			owned: "true",
			want:  []string{"--kv-cache-dtype=bfloat16"},
		},
		{
			name:  "sglang is handed the same flag",
			owned: "true",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Image = "lmsysorg/sglang:v0.5.18"
			},
			want: []string{"--kv-cache-dtype=bfloat16"},
		},
		{
			name:  "the setting off renders nothing, as before the flag was owned",
			owned: "false",
		},
		{
			// Admission refuses this shape now; one stored before it keeps its own value, which the
			// engine keeps because it comes later on the command line.
			name:  "a stored role's own value stays after the rendered one",
			owned: "true",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--kv-cache-dtype=fp8"}
			},
			want: []string{"--kv-cache-dtype=bfloat16", "--kv-cache-dtype=fp8"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.owned != "" {
				settingtest.MergeDelegatedSettings(t, map[string]string{
					"model-deployment-kv-cache-dtype-owned": tc.owned,
				})
			}
			var mutate []func(*workercore.ModelDeployment)
			if tc.mutate != nil {
				mutate = append(mutate, tc.mutate)
			}
			cli := newModelDeploymentClient(newRenderDeployment(mutate...), newRenderInstanceType(),
				newRenderBinding(), newRenderPool(), newRenderBackend())

			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			pods := replicaPods(t, cli)
			require.NotEmpty(t, pods)
			for i := range pods {
				argv := pods[i].Spec.Containers[0].Command
				require.NotEmpty(t, argv, "%s renders no command line", pods[i].Name)
				assert.Equal(t, tc.want, kvCacheDtypeEntries(argv), "%s", pods[i].Name)
			}
		})
	}
}
