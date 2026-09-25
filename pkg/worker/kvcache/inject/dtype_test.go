package inject

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dtypeValues returns every value a rendered command line gives the KV cache dtype flag. The flag is
// spelled out rather than read from KVCacheDtypeArg, because the engines parse that spelling and a
// wrong constant would otherwise pass against itself.
func dtypeValues(args []string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--kv-cache-dtype" {
			values = append(values, args[i+1])
		}
	}

	return values
}

// TestRenderDtype pins where the Binding's dtype reaches the engine: once, verbatim, on every engine
// that renders a store, and nowhere a store is not rendered.
func TestRenderDtype(t *testing.T) {
	cases := []struct {
		name  string
		input Input
		want  []string
	}{
		{
			name:  "vllm renders the dtype beside its store",
			input: Input{Engine: EngineVLLM, Connection: testConnectionFor(EngineVLLM), Dtype: "fp8_e4m3"},
			want:  []string{"fp8_e4m3"},
		},
		{
			name:  "vllm-ascend renders it too",
			input: Input{Engine: EngineVLLMAscend, Connection: testConnectionFor(EngineVLLMAscend), Dtype: "bfloat16"},
			want:  []string{"bfloat16"},
		},
		{
			name:  "sglang renders the dtype beside its store",
			input: Input{Engine: EngineSGLang, Connection: testConnectionFor(EngineSGLang), Dtype: "bfloat16"},
			want:  []string{"bfloat16"},
		},
		{
			name: "a vllm pair on a store renders it once",
			input: Input{
				Engine: EngineVLLM, Role: RolePrefill, Disaggregated: true, KVTransfer: true,
				Connection: testConnectionFor(EngineVLLM), Dtype: "bfloat16",
			},
			want: []string{"bfloat16"},
		},
		{
			name: "an sglang decode half on a store renders it",
			input: Input{
				Engine: EngineSGLang, Role: RoleDecode, Disaggregated: true,
				Connection: testConnectionFor(EngineSGLang), Dtype: "bfloat16",
			},
			want: []string{"bfloat16"},
		},
		{
			name:  "a spelling one engine rejects is still passed verbatim",
			input: Input{Engine: EngineVLLM, Connection: testConnectionFor(EngineVLLM), Dtype: "bf16"},
			want:  []string{"bf16"},
		},
		{
			name:  "no dtype renders no flag",
			input: Input{Engine: EngineVLLM, Connection: testConnectionFor(EngineVLLM)},
		},
		{
			name:  "a point-to-point leg without a store renders no flag",
			input: Input{Engine: EngineVLLM, Role: RolePrefill, Disaggregated: true, KVTransfer: true, Dtype: "bfloat16"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.want, dtypeValues(result.Args))
		})
	}
}
