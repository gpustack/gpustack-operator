package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// allOnesParallelism is the parse of an empty book: every degree at its engine's default of
// one, no mode and no wiring sighted. The cases below start from it so each one names only the
// sightings its own input produces.
func allOnesParallelism() ModelDeploymentDeclaredParallelism {
	return ModelDeploymentDeclaredParallelism{
		TensorParallel:           1,
		PipelineParallel:         1,
		DataParallel:             1,
		PrefillContextParallel:   1,
		DecodeContextParallel:    1,
		ExpertParallel:           1,
		AttentionContextParallel: 1,
		MoEDataParallel:          1,
		DWDPSize:                 1,
	}
}

func TestParseModelDeploymentDeclaredParallelism(t *testing.T) {
	t.Parallel()

	vllm := workercore.ModelDeploymentEngineVLLM
	sglang := workercore.ModelDeploymentEngineSGLang

	cases := []struct {
		name string
		// engine is a table key, or a name no table knows.
		engine    string
		extraArgs []string
		env       []workercore.ModelDeploymentEnvVar
		// set mutates the all-ones expectation; nil expects an empty book back.
		set func(*ModelDeploymentDeclaredParallelism)
		// wantErr is a substring of the returned error; empty expects none.
		wantErr string
	}{
		{
			name:   "vllm: empty books stay all ones",
			engine: vllm,
		},
		{
			name:      "vllm: startup capacity does not change running degrees",
			engine:    vllm,
			extraArgs: []string{"--elastic-ep-max-dp-size=8"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.ElasticEPMaxDataParallel = 8 },
		},
		{
			name:      "vllm: long flag with separate value",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: long flag with = value",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size=2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: underscore spelling",
			engine:    vllm,
			extraArgs: []string{"--tensor_parallel_size", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: underscore spelling with = value",
			engine:    vllm,
			extraArgs: []string{"--tensor_parallel_size=2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: short alias with separate value",
			engine:    vllm,
			extraArgs: []string{"-tp", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: short alias with = value",
			engine:    vllm,
			extraArgs: []string{"-tp=2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: abbreviation with a glued value is refused",
			engine:    vllm,
			extraArgs: []string{"-t2"},
			wantErr:   "carries a glued value",
		},
		{
			name:      "vllm: registered spelling with a glued value is refused through its abbreviation",
			engine:    vllm,
			extraArgs: []string{"-tp2"},
			wantErr:   "carries a glued value",
		},
		{
			name:      "vllm: mode abbreviation with a glued value is refused",
			engine:    vllm,
			extraArgs: []string{"-e2"},
			wantErr:   "carries a glued value",
		},
		{
			name:      "vllm: single-dash abbreviation resolves",
			engine:    vllm,
			extraArgs: []string{"-t", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: single-dash abbreviation with = is refused as version-dependent",
			engine:    vllm,
			extraArgs: []string{"-t=2"},
			wantErr:   "depends on the image",
		},
		{
			name:      "vllm: prefill-context single-dash abbreviation resolves",
			engine:    vllm,
			extraArgs: []string{"-pc", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.PrefillContextParallel = 2 },
		},
		{
			name:      "vllm: prefill-context abbreviation with = is refused as version-dependent",
			engine:    vllm,
			extraArgs: []string{"-pc=2"},
			wantErr:   "depends on the image",
		},
		{
			name:      "vllm: mode single-dash abbreviation records",
			engine:    vllm,
			extraArgs: []string{"-e"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Modes = []string{"--enable-expert-parallel"} },
		},
		{
			name:      "vllm: mode abbreviation with = passes through",
			engine:    vllm,
			extraArgs: []string{"-e=x"},
		},
		{
			name:      "vllm: diffusion-config exact spelling shadows the decode prefix",
			engine:    vllm,
			extraArgs: []string{"-dc", "{}"},
		},
		{
			name:      "vllm: ambiguous single-dash prefix passes through",
			engine:    vllm,
			extraArgs: []string{"-d", "2"},
		},
		{
			name:      "vllm: single-letter alias concatenated value records",
			engine:    vllm,
			extraArgs: []string{"-n2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--nnodes"}; p.NodeCount = 2 },
		},
		{
			name:      "vllm: node-rank concatenated value records",
			engine:    vllm,
			extraArgs: []string{"-r0"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--node-rank"} },
		},
		{
			name:      "vllm: single-letter alias with = value records",
			engine:    vllm,
			extraArgs: []string{"-n=2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--nnodes"}; p.NodeCount = 2 },
		},
		{
			name:      "vllm: underscore unique prefix resolves",
			engine:    vllm,
			extraArgs: []string{"--tensor_parallel", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: unique prefix resolves",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: unique prefix with = value resolves",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel=2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: pipeline and context degrees",
			engine:    vllm,
			extraArgs: []string{"-pp", "2", "-pcp", "3", "-dcp", "4"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.PipelineParallel = 2
				p.PrefillContextParallel = 3
				p.DecodeContextParallel = 4
			},
		},
		{
			name:      "vllm: data parallel with local share",
			engine:    vllm,
			extraArgs: []string{"-dp", "2", "-dpl", "2"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.DataParallel = 2
				p.DataParallelLocal = 2
			},
		},
		{
			name:      "vllm: repeat resolves last wins",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "2", "--tensor-parallel-size", "4"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 4 },
		},
		{
			name:      "vllm: repeat across spellings resolves last wins",
			engine:    vllm,
			extraArgs: []string{"-tp", "2", "--tensor_parallel_size=3"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 3 },
		},
		{
			name:      "vllm: ambiguous data-parallel prefix passes through",
			engine:    vllm,
			extraArgs: []string{"--data-parallel", "2"},
		},
		{
			name:      "vllm: ambiguous size prefix passes through",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-s", "2"},
		},
		{
			name:      "vllm: unique local prefix resolves",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-size-l", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.DataParallelLocal = 2 },
		},
		{
			name:      "vllm: mode records and consumes nothing",
			engine:    vllm,
			extraArgs: []string{"--enable-expert-parallel", "--tensor-parallel-size", "2"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.TensorParallel = 2
				p.Modes = []string{"--enable-expert-parallel"}
			},
		},
		{
			name:      "vllm: mode short alias records",
			engine:    vllm,
			extraArgs: []string{"-ep"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Modes = []string{"--enable-expert-parallel"} },
		},
		{
			name:      "vllm: mode with explicit value passes through",
			engine:    vllm,
			extraArgs: []string{"--enable-expert-parallel=true"},
		},
		{
			name:      "vllm: mode records once across spellings",
			engine:    vllm,
			extraArgs: []string{"-ep", "--enable-expert-parallel"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Modes = []string{"--enable-expert-parallel"} },
		},
		{
			name:      "vllm: wiring records and consumes its value",
			engine:    vllm,
			extraArgs: []string{"--nnodes", "2", "--tensor-parallel-size", "2"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.TensorParallel = 2
				p.NodeCount = 2
				p.Wiring = []string{"--nnodes"}
			},
		},
		{
			name:      "vllm: wiring short aliases record",
			engine:    vllm,
			extraArgs: []string{"-n", "2", "-r", "0"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.Wiring = []string{"--nnodes", "--node-rank"}
				p.NodeCount = 2
			},
		},
		{
			name:      "vllm: boolean wiring consumes nothing",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-external-lb", "--tensor-parallel-size", "2"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.TensorParallel = 2
				p.Wiring = []string{"--data-parallel-external-lb"}
			},
		},
		{
			name:      "vllm: wiring = form records",
			engine:    vllm,
			extraArgs: []string{"--master-addr=10.0.0.1"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--master-addr"} },
		},
		{
			name:      "vllm: boolean wiring with explicit value passes through",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-external-lb=true"},
		},
		{
			name:      "vllm: master port wiring records",
			engine:    vllm,
			extraArgs: []string{"--master-port", "29500"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--master-port"} },
		},
		{
			name:   "vllm: dp wiring family records",
			engine: vllm,
			extraArgs: []string{
				"-dpn", "0", "-dpr", "1", "-dpa", "10.0.0.1", "-dpp", "29551",
				"-dpb", "mp", "-dph", "-dpe", "-dpm",
			},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.Wiring = []string{
					"--data-parallel-rank", "--data-parallel-start-rank", "--data-parallel-address",
					"--data-parallel-rpc-port", "--data-parallel-backend", "--data-parallel-hybrid-lb",
					"--data-parallel-external-lb", "--data-parallel-multi-port-external-lb",
				}
			},
		},
		{
			name:      "vllm: underscore wiring spelling records",
			engine:    vllm,
			extraArgs: []string{"--data_parallel_rank", "0"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--data-parallel-rank"} },
		},
		{
			name:      "vllm: recognition stops at a bare --",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "2", "--", "--tensor-parallel-size", "4"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "vllm: unknown tokens pass through",
			engine:    vllm,
			extraArgs: []string{"--served-model-name", "foo", "positional", "", "--unknown-flag", "5"},
		},
		{
			name:      "vllm: flag-like value is not consumed",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "-x"},
			wantErr:   "has no value",
		},
		{
			name:      "vllm: value missing at end of list",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size"},
			wantErr:   "has no value",
		},
		{
			name:      "vllm: non-integer value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "two"},
			wantErr:   "is not an integer",
		},
		{
			name:      "vllm: zero value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "0"},
			wantErr:   "below 1",
		},
		{
			name:      "vllm: negative value is consumed and refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "-2"},
			wantErr:   "below 1",
		},
		{
			name:      "vllm: negative = value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size=-2"},
			wantErr:   "below 1",
		},
		{
			name:      "vllm: bare fractional negative is consumed as a value",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "-.5"},
			wantErr:   "is not an integer",
		},
		{
			name:      "vllm: padded value parses with the engine alphabet",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", " 3 "},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 3 },
		},
		{
			name:      "vllm: underscored value parses with the engine alphabet",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "1_0"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 10 },
		},
		{
			name:      "vllm: signed value parses with the engine alphabet",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "+3"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 3 },
		},
		{
			name:      "vllm: leading underscore value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "_3"},
			wantErr:   "is not an integer",
		},
		{
			name:      "vllm: trailing underscore value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "3_"},
			wantErr:   "is not an integer",
		},
		{
			name:      "vllm: doubled underscore value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "3__0"},
			wantErr:   "is not an integer",
		},
		{
			name:      "vllm: value beyond the host int refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size", "99999999999999999999999999"},
			wantErr:   "out of range",
		},
		{
			name:      "vllm: empty = value refused",
			engine:    vllm,
			extraArgs: []string{"--tensor-parallel-size="},
			wantErr:   "is not an integer",
		},
		{
			name:      "vllm: local data parallel allows zero",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-size-local", "0"},
		},
		{
			name:      "vllm: local data parallel below zero refused",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-size-local", "-1"},
			wantErr:   "below 0",
		},
		{
			name:      "vllm: wiring value missing at end of list still records",
			engine:    vllm,
			extraArgs: []string{"--nnodes"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--nnodes"} },
		},
		{
			name:      "vllm: wiring does not consume a following flag",
			engine:    vllm,
			extraArgs: []string{"--nnodes", "--tensor-parallel-size", "2"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.TensorParallel = 2
				p.Wiring = []string{"--nnodes"}
			},
		},
		{
			name:      "vllm: a bare -- in first position ends recognition",
			engine:    vllm,
			extraArgs: []string{"--", "-tp", "2"},
		},
		{
			name:      "vllm: repeated degree missing its last value refused",
			engine:    vllm,
			extraArgs: []string{"-tp", "2", "-tp"},
			wantErr:   "has no value",
		},
		{
			name:   "vllm env: supplies data parallel when the cli is silent",
			engine: vllm,
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
			set:    func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 3 },
		},
		{
			name:      "vllm env: cli above one wins",
			engine:    vllm,
			extraArgs: []string{"-dp", "2"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 2 },
		},
		{
			name:      "vllm env: cli at one does not win",
			engine:    vllm,
			extraArgs: []string{"-dp", "1"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 3 },
		},
		{
			name:      "vllm env: a local zero blocks the fallback",
			engine:    vllm,
			extraArgs: []string{"--data-parallel-size-local", "0"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
		},
		{
			name:    "vllm env: malformed when effective refused",
			engine:  vllm,
			env:     []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "three"}},
			wantErr: "VLLM_DP_SIZE",
		},
		{
			name:      "vllm env: malformed but cli wins passes",
			engine:    vllm,
			extraArgs: []string{"-dp", "2"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "three"}},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 2 },
		},
		{
			name:    "vllm env: below one when effective refused",
			engine:  vllm,
			env:     []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "0"}},
			wantErr: "below 1",
		},
		{
			name:   "vllm env: repeats resolve last wins",
			engine: vllm,
			env: []workercore.ModelDeploymentEnvVar{
				{Name: "VLLM_DP_SIZE", Value: "3"},
				{Name: "VLLM_DP_SIZE", Value: "4"},
			},
			set: func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 4 },
		},
		{
			name:   "vllm env: padded value parses with the engine alphabet",
			engine: vllm,
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: " 3 "}},
			set:    func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 3 },
		},
		{
			name:   "vllm env: underscored value parses with the engine alphabet",
			engine: vllm,
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "1_0"}},
			set:    func(p *ModelDeploymentDeclaredParallelism) { p.DataParallel = 10 },
		},
		{
			name:    "vllm env: malformed alphabet refused",
			engine:  vllm,
			env:     []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "_3"}},
			wantErr: "is not an integer",
		},
		{
			name:      "vllm env: a declared local share does not block the fallback",
			engine:    vllm,
			extraArgs: []string{"-dpl", "2"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.DataParallel = 3
				p.DataParallelLocal = 2
			},
		},
		{
			name:      "vllm env: cli at one with a local zero keeps the cli path",
			engine:    vllm,
			extraArgs: []string{"-dp", "1", "-dpl", "0"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
		},
		{
			name:      "vllm env: malformed with a declared local share refused",
			engine:    vllm,
			extraArgs: []string{"-dpl", "2"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "three"}},
			wantErr:   "VLLM_DP_SIZE",
		},
		{
			name:      "vllm env: a local zero overridden by a later share unblocks the fallback",
			engine:    vllm,
			extraArgs: []string{"-dpl", "0", "-dpl", "2"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.DataParallel = 3
				p.DataParallelLocal = 2
			},
		},
		{
			name:   "vllm env: unrelated entries ignored",
			engine: vllm,
			env:    []workercore.ModelDeploymentEnvVar{{Name: "SOME_OTHER_VAR", Value: "3"}},
		},
		{
			name:      "sglang: tp degree",
			engine:    sglang,
			extraArgs: []string{"--tp-size", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "sglang: long alias",
			engine:    sglang,
			extraArgs: []string{"--tensor-parallel-size", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "sglang: unique prefix resolves",
			engine:    sglang,
			extraArgs: []string{"--tp", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.TensorParallel = 2 },
		},
		{
			name:      "sglang: same-option double spelling prefix is ambiguous",
			engine:    sglang,
			extraArgs: []string{"--t", "2"},
		},
		{
			name:      "sglang: underscore spelling passes through",
			engine:    sglang,
			extraArgs: []string{"--tp_size", "2"},
		},
		{
			name:   "sglang: full degree set",
			engine: sglang,
			extraArgs: []string{
				"--pp-size", "2", "--dp-size", "2", "--ep-size", "2", "--dcp-size", "3",
				"--attn-cp-size", "2", "--moe-dp-size", "2", "--dwdp-size", "2",
			},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.PipelineParallel = 2
				p.DataParallel = 2
				p.ExpertParallel = 2
				p.DecodeContextParallel = 3
				p.AttentionContextParallel = 2
				p.MoEDataParallel = 2
				p.DWDPSize = 2
			},
		},
		{
			name:      "sglang: ep exact spelling wins over its prefix",
			engine:    sglang,
			extraArgs: []string{"--ep", "2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.ExpertParallel = 2 },
		},
		{
			name:      "sglang: expert long alias with = value",
			engine:    sglang,
			extraArgs: []string{"--expert-parallel-size=2"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.ExpertParallel = 2 },
		},
		{
			name:      "sglang: modes record",
			engine:    sglang,
			extraArgs: []string{"--enable-dp-attention", "--enable-prefill-cp"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.Modes = []string{"--enable-dp-attention", "--enable-prefill-cp"}
			},
		},
		{
			name:      "sglang: wiring records, the nccl alias canonicalizes",
			engine:    sglang,
			extraArgs: []string{"--nnodes", "2", "--dist-init-addr", "a:1", "--nccl-init-addr", "b:2"},
			set: func(p *ModelDeploymentDeclaredParallelism) {
				p.Wiring = []string{"--nnodes", "--dist-init-addr"}
				p.NodeCount = 2
			},
		},
		{
			name:      "sglang: node-rank wiring records",
			engine:    sglang,
			extraArgs: []string{"--node-rank", "1"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--node-rank"} },
		},
		{
			name:   "sglang: env carries no dp spelling",
			engine: sglang,
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
		},
		{
			name:      "sglang: missing value refused",
			engine:    sglang,
			extraArgs: []string{"--tp-size"},
			wantErr:   "has no value",
		},
		{
			name:      "sglang: zero refused",
			engine:    sglang,
			extraArgs: []string{"--tp-size", "0"},
			wantErr:   "below 1",
		},
		{
			name:      "engine with no table parses to all ones",
			engine:    "tensorrt",
			extraArgs: []string{"--tensor-parallel-size", "2"},
			env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "3"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseModelDeploymentDeclaredParallelism(tc.engine, tc.extraArgs, tc.env)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)

			want := allOnesParallelism()
			if tc.set != nil {
				tc.set(&want)
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestReadModelDeploymentRoleParallelism pins the status read of one role's own argument
// stream: a degree is present only when the role declares it, an explicit 1 and an explicit
// local 0 are preserved values, the effective environment is a declaration like any other,
// a malformed declaration leaves the source Unknown with the reason instead of a defaulted
// degree, and a role that replaced its command line is not read at all.
func TestReadModelDeploymentRoleParallelism(t *testing.T) {
	t.Parallel()

	vllm := workercore.ModelDeploymentEngineVLLM
	sglang := workercore.ModelDeploymentEngineSGLang

	testCases := []struct {
		name   string
		engine string
		role   workercore.ModelDeploymentRole
		want   workercore.ModelDeploymentRoleParallelismStatus
		// wantAnyReason pins a refusal without pinning its wording: the message a degree
		// refused for size carries differs between 64-bit and 32-bit builds, while the
		// refusal itself does not.
		wantAnyReason bool
	}{
		{
			name:   "a role that declares nothing reads complete with no degrees",
			engine: vllm,
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "an explicit 1 is a preserved value, not a default",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--tensor-parallel-size", "1"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(1)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "an explicit local 0 is the preserved external sentinel",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--data-parallel-size-local", "0"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					DataParallelLocal: ptr.To(int32(0)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "the effective environment declares the degree",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				Env: []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "4"}},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					DataParallel: ptr.To(int32(4)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "the command line drives data parallelism and the environment is not read",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--data-parallel-size", "2"},
				Env:       []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "garbage"}},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					DataParallel: ptr.To(int32(2)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "the last spelling of a repeated degree wins",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--tp", "2", "--tensor-parallel-size", "4"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(4)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "everything after the argv terminator is not an argument",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--tensor-parallel-size", "2", "--", "--tensor-parallel-size", "8"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(2)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "a malformed degree leaves the source Unknown with the reason",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--tensor-parallel-size", "banana"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceUnknown,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:             workercore.ModelDeploymentParallelismSourceKindUnknown,
					UnreadableReason: `the --tensor-parallel-size value "banana" is not an integer`,
				},
			},
		},
		{
			name:   "a degree beyond the status degree fields refuses the whole reading",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--tensor-parallel-size=3000000000"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceUnknown,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind: workercore.ModelDeploymentParallelismSourceKindUnknown,
				},
			},
			wantAnyReason: true,
		},
		{
			name:   "an unreadable effective environment is an unreadable source",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				Env: []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "banana"}},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceUnknown,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:             workercore.ModelDeploymentParallelismSourceKindUnknown,
					UnreadableReason: `the VLLM_DP_SIZE value "banana" is not an integer`,
				},
			},
		},
		{
			name:   "a role that replaced its command line is not read at all",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				Command:   []string{"vllm", "serve", "--tensor-parallel-size", "8"},
				ExtraArgs: []string{"--tensor-parallel-size", "2"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceUnknown,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:             workercore.ModelDeploymentParallelismSourceKindUnknown,
					UnreadableReason: modelDeploymentReasonUnmanaged,
				},
			},
		},
		{
			name:   "an observed mode is a present-only boolean",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--enable-expert-parallel", "--tensor-parallel-size", "2"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(2)),
				},
				Modes:       map[string]bool{"expertParallel": true},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "the sglang mode set reads under its own keys",
			engine: sglang,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--enable-dp-attention", "--enable-prefill-cp", "--tp-size", "2"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(2)),
				},
				Modes:       map[string]bool{"dpAttention": true, "prefillContextParallel": true},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "an external balance flag derives External",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--data-parallel-external-lb"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceExternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "an assigned data-parallel rank derives External",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--data-parallel-rank", "0"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceExternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "a hybrid balance flag derives Hybrid",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--data-parallel-hybrid-lb"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceHybrid,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "a multi-port balance flag derives MultiPort",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--data-parallel-multi-port-external-lb"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceMultiPort,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "conflicting balance shapes derive Unknown with the flags named",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--data-parallel-external-lb", "--data-parallel-hybrid-lb"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceUnknown,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
					UnreadableReason: "the balance flags --data-parallel-external-lb and " +
						"--data-parallel-hybrid-lb derive to different shapes",
				},
			},
		},
		{
			name:   "a two-role spread keeps its internal balance",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--nnodes", "2", "--node-rank", "1", "--tensor-parallel-size", "2"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				Declared: workercore.ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(2)),
				},
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "an assigned start rank alone does not select external routing",
			engine: vllm,
			role:   workercore.ModelDeploymentRole{ExtraArgs: []string{"--data-parallel-start-rank=1"}},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceInternal,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:     workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete: true,
				},
			},
		},
		{
			name:   "start rank and external balancer conflict",
			engine: vllm,
			role: workercore.ModelDeploymentRole{
				ExtraArgs: []string{"--data-parallel-start-rank=1", "--data-parallel-external-lb"},
			},
			want: workercore.ModelDeploymentRoleParallelismStatus{
				LoadBalance: workercore.ModelDeploymentLoadBalanceUnknown,
				Source: workercore.ModelDeploymentParallelismSourceStatus{
					Kind:             workercore.ModelDeploymentParallelismSourceKindExtraArgs,
					Complete:         true,
					UnreadableReason: "the balance flags --data-parallel-external-lb and --data-parallel-start-rank derive to different shapes",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ReadModelDeploymentRoleParallelism(tc.engine, &tc.role)
			if tc.wantAnyReason {
				reason := got.Source.UnreadableReason
				got.Source.UnreadableReason = ""
				assert.Equal(t, tc.want, got)
				assert.NotEmpty(t, reason, "the refusal names what could not be established")

				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestModelDeploymentDegreeSlotsComplete pins the degree-slot table against its constants:
// every slot constant in the status field order has a row, and every row reads and writes
// its field — a slot added without a row fails here instead of surfacing as a missing degree
// or a panic in the conversion.
func TestModelDeploymentDegreeSlotsComplete(t *testing.T) {
	t.Parallel()

	if int(modelDeploymentDegreeSlotCount) != len(modelDeploymentDegreeSlots) {
		t.Fatalf("the slot table has %d rows for %d slot constants",
			len(modelDeploymentDegreeSlots), modelDeploymentDegreeSlotCount)
	}
	for slot, table := range modelDeploymentDegreeSlots {
		if table.of == nil || table.set == nil {
			t.Errorf("slot %d (%s) has an incomplete table row", slot, table.name)
		}
	}
}

// TestModelDeploymentDegreeSlotsMatchTheirSlots pins the slot table's literal order to the
// slot constants it is indexed by: the table's correctness depends on entry i describing the
// slot named modelDeploymentSlot<i>, and nothing else in the compile enforces it.
func TestModelDeploymentDegreeSlotsMatchTheirSlots(t *testing.T) {
	expected := [modelDeploymentDegreeSlotCount]string{
		modelDeploymentSlotTensorParallel:           "--tensor-parallel-size",
		modelDeploymentSlotPipelineParallel:         "--pipeline-parallel-size",
		modelDeploymentSlotDataParallel:             "--data-parallel-size",
		modelDeploymentSlotDataParallelLocal:        "--data-parallel-size-local",
		modelDeploymentSlotPrefillContextParallel:   "--prefill-context-parallel-size",
		modelDeploymentSlotDecodeContextParallel:    "--decode-context-parallel-size",
		modelDeploymentSlotExpertParallel:           "--ep-size",
		modelDeploymentSlotAttentionContextParallel: "--attn-cp-size",
		modelDeploymentSlotMoEDPSize:                "--moe-dp-size",
		modelDeploymentSlotDWDPSize:                 "--dwdp-size",
	}
	require.Len(t, modelDeploymentDegreeSlots[:], int(modelDeploymentDegreeSlotCount))
	for slot, entry := range modelDeploymentDegreeSlots {
		assert.Equal(t, expected[modelDeploymentDegreeSlot(slot)], entry.name,
			"slot %d carries the wrong flag; the table order diverged from the constants", slot)
	}
}
