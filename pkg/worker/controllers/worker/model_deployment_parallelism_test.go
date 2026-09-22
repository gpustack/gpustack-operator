package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--nnodes"} },
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
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--nnodes"} },
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
				p.Wiring = []string{"--nnodes"}
			},
		},
		{
			name:      "vllm: wiring short aliases record",
			engine:    vllm,
			extraArgs: []string{"-n", "2", "-r", "0"},
			set:       func(p *ModelDeploymentDeclaredParallelism) { p.Wiring = []string{"--nnodes", "--node-rank"} },
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
