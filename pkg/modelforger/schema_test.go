package modelforger

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelkernelkey"
	"gpustack.ai/gpustack/pkg/modelmanager/ingest"
)

func sampleValidKernelKey(t *testing.T, opFamily string) string {
	t.Helper()
	k, err := modelkernelkey.Build(modelkernelkey.BuildSpec{
		Engine:        "vLLM",
		EngineVersion: "0.8.5",
		Manufacturer:  "nvidia",
		Arch:          "sm_90a",
		OpFamily:      opFamily,
		ConstAxes:     map[string]int64{"hidden_size": 4096},
		InputDtypes:   []string{"bfloat16"},
		OutputDtypes:  []string{"bfloat16"},
	})
	require.NoError(t, err)
	return k.String()
}

func TestSchemaVersionsMatchModelManagerIngest(t *testing.T) {
	t.Parallel()
	require.Equal(t, ingest.DispatchManifestSchemaVersion, DispatchManifestSchemaVersion)
	require.Equal(t, ingest.VerificationReportSchemaVersion, VerificationReportSchemaVersion)
}

func TestValidateRelativePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "valid root file", path: "dispatch-manifest.json"},
		{name: "valid nested file", path: "kernels/rmsnorm_mock_v1/kernel.py"},
		{name: "empty", path: "", wantErr: true},
		{name: "absolute", path: "/etc/passwd", wantErr: true},
		{name: "parent traversal", path: "../secret.json", wantErr: true},
		{name: "nested traversal", path: "kernels/../../etc/passwd", wantErr: true},
		{name: "dot segment", path: "./kernels/a.py", wantErr: true},
		{name: "backslash", path: `kernels\a.py`, wantErr: true},
		{name: "control char", path: "kernels/a\x00.py", wantErr: true},
		{name: "exceeds max length", path: strings.Repeat("a", MaxRelativePathLength+1), wantErr: true},
		{name: "exceeds max depth", path: strings.Repeat("a/", MaxRelativePathDepth) + "b.py", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRelativePath(tc.path)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateForgeRequest(t *testing.T) {
	t.Parallel()

	validKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	baseReq := func() *ForgeRequest {
		return &ForgeRequest{
			SchemaVersion:       ForgeRequestSchemaVersion,
			Engine:              "vLLM",
			EngineVersion:       "0.8.5",
			Manufacturer:        "nvidia",
			Architecture:        "sm_90a",
			TargetKernelKeys:    []string{validKey},
			BaselineLatenciesUs: map[string]float64{validKey: 120.0},
			MinSpeedupRatio:     1.05,
			Atol:                1e-2,
			Rtol:                1e-2,
		}
	}

	cases := []struct {
		name    string
		mutate  func(*ForgeRequest)
		wantErr string
	}{
		{name: "valid request", mutate: func(*ForgeRequest) {}},
		{name: "unsupported schema", mutate: func(r *ForgeRequest) { r.SchemaVersion = "bad/v0" }, wantErr: "schemaVersion"},
		{name: "empty target keys", mutate: func(r *ForgeRequest) { r.TargetKernelKeys = nil }, wantErr: "must not be empty"},
		{
			name:    "excluded GEMM key",
			mutate:  func(r *ForgeRequest) { r.TargetKernelKeys = []string{"vllm/0.8/nvidia/sm_90a/gemm/0123456789abcdef"} },
			wantErr: "excluded",
		},
		{
			name: "excluded collective key",
			mutate: func(r *ForgeRequest) {
				r.TargetKernelKeys = []string{"vllm/0.8/nvidia/sm_90a/all_reduce/0123456789abcdef"}
			},
			wantErr: "excluded",
		},
		{
			name:    "duplicate key",
			mutate:  func(r *ForgeRequest) { r.TargetKernelKeys = []string{validKey, validKey} },
			wantErr: "duplicate",
		},
		{
			name:    "arch mismatch",
			mutate:  func(r *ForgeRequest) { r.Architecture = "sm_80" },
			wantErr: "arch mismatch",
		},
		{
			name:    "speedup below 1.01",
			mutate:  func(r *ForgeRequest) { r.MinSpeedupRatio = 1.00 },
			wantErr: "minSpeedupRatio",
		},
		{
			name:    "negative baseline latency",
			mutate:  func(r *ForgeRequest) { r.BaselineLatenciesUs[validKey] = -1 },
			wantErr: "baselineLatenciesUs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := baseReq()
			tc.mutate(req)
			err := ValidateForgeRequest(req)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
	require.Error(t, ValidateForgeRequest(nil))
}

func TestValidateDefinitionWorkloadSolution(t *testing.T) {
	t.Parallel()

	validKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	hidden := int64(4096)

	validDef := func() *Definition {
		return &Definition{
			Name:           "rmsnorm_hidden4096",
			OpType:         modelkernelkey.OpFamilyRMSNorm,
			LocalKernelKey: validKey,
			Axes: map[string]AxisSpec{
				"hidden_size": {Type: "const", Value: &hidden},
				"num_tokens":  {Type: "var"},
			},
			Inputs: map[string]TensorSpec{
				"x":      {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
				"weight": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"},
			},
			Outputs: map[string]TensorSpec{
				"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
			},
			Reference: "def run(x, weight): return x * weight",
		}
	}

	require.NoError(t, ValidateDefinition(validDef()))
	require.Error(t, ValidateDefinition(nil))

	defGemm := validDef()
	defGemm.OpType = "fp8_blockwise_gemm"
	require.Error(t, ValidateDefinition(defGemm))

	defVarConst := validDef()
	toks := int64(16)
	defVarConst.Axes["num_tokens"] = AxisSpec{Type: "const", Value: &toks}
	require.Error(t, ValidateDefinition(defVarConst))

	defMissingAxis := validDef()
	defMissingAxis.Inputs["x"] = TensorSpec{Shape: []string{"unknown_axis"}, Dtype: "bfloat16"}
	require.Error(t, ValidateDefinition(defMissingAxis))

	wl := &Workload{
		UUID:               "wl-1",
		Definition:         "rmsnorm_hidden4096",
		LocalKernelKey:     validKey,
		Axes:               map[string]int64{"num_tokens": 64},
		Inputs:             map[string]WorkloadInput{"x": {Type: "random"}},
		ObservedDurationUs: 32.1,
		CallCount:          4,
	}
	require.NoError(t, ValidateWorkload(wl))
	require.Error(t, ValidateWorkload(nil))
	wlBad := *wl
	wlBad.Axes = map[string]int64{"num_tokens": 0}
	require.Error(t, ValidateWorkload(&wlBad))

	sol := &Solution{
		Name:           "rmsnorm_mock_v1",
		Definition:     "rmsnorm_hidden4096",
		LocalKernelKey: validKey,
		Author:         "tester",
		Spec: SolutionSpec{
			Language:       "python",
			TargetHardware: []string{"nvidia:sm_90a"},
			EntryPoint:     "run",
		},
		Sources: []SolutionSource{{Path: "kernels/rmsnorm_mock_v1/kernel.py"}},
	}
	require.NoError(t, ValidateSolution(sol))
	require.Error(t, ValidateSolution(nil))
	solEscape := *sol
	solEscape.Sources = []SolutionSource{{Path: "../escape.py"}}
	require.Error(t, ValidateSolution(&solEscape))
}

func TestValidateTraceAndEvaluationThresholds(t *testing.T) {
	t.Parallel()

	thresholds := DefaultVerificationThresholds()
	baseTrace := func() *Trace {
		return &Trace{
			Definition: "rmsnorm_hidden4096",
			Workload: Workload{
				UUID:   "wl-1",
				Axes:   map[string]int64{"num_tokens": 64},
				Inputs: map[string]WorkloadInput{"x": {Type: "random"}},
			},
			Solution: "rmsnorm_mock_v1",
			Evaluation: TraceEvaluation{
				Status: StatusPassed,
				Correctness: &TraceCorrectness{
					MaxAbsError:      1e-4,
					MaxRelativeError: 1e-3,
				},
				Performance: &TracePerformance{
					LatencyMs:          0.08,
					ReferenceLatencyMs: 0.10,
					SpeedupFactor:      1.25,
				},
			},
		}
	}

	boolPtr := func(v bool) *bool { return &v }
	cases := []struct {
		name    string
		mutate  func(*Trace)
		wantErr string
	}{
		{name: "valid trace", mutate: func(*Trace) {}},
		{name: "non-PASSED status", mutate: func(tr *Trace) { tr.Evaluation.Status = "INCORRECT_NUMERICAL" }, wantErr: "PASSED"},
		{
			name: "bf16 1-ULP max_abs_error > atol with max_relative_error <= rtol passes",
			mutate: func(tr *Trace) {
				tr.Evaluation.Correctness.MaxAbsError = 0.015625
				tr.Evaluation.Correctness.MaxRelativeError = 0.0078
			},
		},
		{
			name: "near-atol max_abs_error <= atol with raw max_relative_error > rtol passes",
			mutate: func(tr *Trace) {
				tr.Evaluation.Correctness.MaxAbsError = 0.0002
				tr.Evaluation.Correctness.MaxRelativeError = 0.0198
			},
		},
		{
			name: "both max_abs_error > atol and max_relative_error > rtol fails",
			mutate: func(tr *Trace) {
				tr.Evaluation.Correctness.MaxAbsError = 0.05
				tr.Evaluation.Correctness.MaxRelativeError = 0.05
			},
			wantErr: "atol",
		},
		{
			name: "allclose_passed false fails",
			mutate: func(tr *Trace) {
				tr.Evaluation.Correctness.AllClosePassed = boolPtr(false)
				tr.Evaluation.Correctness.MaxAbsError = 1e-4
				tr.Evaluation.Correctness.MaxRelativeError = 1e-4
			},
			wantErr: "allclose_passed",
		},
		{
			name: "allclose_passed true with bf16 1-ULP passes",
			mutate: func(tr *Trace) {
				tr.Evaluation.Correctness.AllClosePassed = boolPtr(true)
				tr.Evaluation.Correctness.MaxAbsError = 0.015625
				tr.Evaluation.Correctness.MaxRelativeError = 0.0028
			},
		},
		{name: "negative max_abs_error fails", mutate: func(tr *Trace) { tr.Evaluation.Correctness.MaxAbsError = -0.001 }, wantErr: "max_abs_error"},
		{name: "negative max_relative_error fails", mutate: func(tr *Trace) { tr.Evaluation.Correctness.MaxRelativeError = -0.001 }, wantErr: "max_relative_error"},
		{name: "speedup_factor < minSpeedupRatio", mutate: func(tr *Trace) { tr.Evaluation.Performance.SpeedupFactor = 1.02 }, wantErr: "speedup_factor"},
		{name: "NaN speedup", mutate: func(tr *Trace) { tr.Evaluation.Performance.SpeedupFactor = math.NaN() }, wantErr: "speedup_factor"},
		{name: "missing correctness", mutate: func(tr *Trace) { tr.Evaluation.Correctness = nil }, wantErr: "correctness"},
		{name: "missing performance", mutate: func(tr *Trace) { tr.Evaluation.Performance = nil }, wantErr: "performance"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := baseTrace()
			tc.mutate(tr)
			err := ValidateTrace(tr, thresholds)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateDispatchManifestAndVerificationReport(t *testing.T) {
	t.Parallel()

	validKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)

	manifest := &DispatchManifest{
		SchemaVersion: DispatchManifestSchemaVersion,
		Compatibility: DispatchCompatibility{
			Engine:        "vLLM",
			EngineVersion: "0.8.5",
			Manufacturer:  "nvidia",
			Architecture:  "sm_90a",
		},
		Checksums: map[string]string{
			"kernels/rmsnorm_mock_v1/kernel.py": digestA,
		},
		Entries: []DispatchManifestEntry{
			{
				LocalKernelKey:    validKey,
				Definition:        "rmsnorm_hidden4096",
				SolutionPath:      "solutions/rmsnorm_mock_v1.json",
				SolutionSHA256:    digestA,
				LibraryPath:       "kernels/rmsnorm_mock_v1/kernel.py",
				LibrarySHA256:     digestA,
				Entrypoint:        "run",
				TracePath:         "traces/rmsnorm_mock_v1.json",
				TraceSHA256:       digestB,
				BaselineLatencyUs: 100,
				ForgedLatencyUs:   80,
				SpeedupRatio:      1.25,
			},
		},
	}

	require.NoError(t, ValidateDispatchManifest(manifest, 1.05))
	require.Error(t, ValidateDispatchManifest(manifest, 1.50))

	badManifest := *manifest
	badManifest.Entries = append([]DispatchManifestEntry(nil), manifest.Entries...)
	badManifest.Entries[0].LibraryPath = "../outside.py"
	require.Error(t, ValidateDispatchManifest(&badManifest, 1.05))

	report := &VerificationReport{
		SchemaVersion:          VerificationReportSchemaVersion,
		OverallStatus:          StatusPassed,
		DispatchManifestDigest: digestA,
		PassedCount:            1,
		FailedCount:            0,
		Thresholds:             DefaultVerificationThresholds(),
		Entries: []VerificationEntryReport{
			{
				Index:          0,
				LocalKernelKey: validKey,
				Definition:     "rmsnorm_hidden4096",
				Passed:         true,
				Reason:         "ok",
				Evaluation: &TraceEvaluation{
					Status:      StatusPassed,
					Correctness: &TraceCorrectness{MaxAbsError: 0, MaxRelativeError: 0},
					Performance: &TracePerformance{LatencyMs: 0.08, ReferenceLatencyMs: 0.10, SpeedupFactor: 1.25},
				},
			},
		},
	}

	require.NoError(t, ValidateVerificationReport(report, digestA))
	require.Error(t, ValidateVerificationReport(report, digestB))

	failedReport := *report
	failedReport.OverallStatus = StatusFailed
	require.Error(t, ValidateVerificationReport(&failedReport, digestA))

	// Exercise remaining schema validation error branches cleanly via table cases.
	require.Error(t, ValidateSHA256Digest("md5:123"))
	require.Error(t, ValidateThresholds(VerificationThresholds{MinSpeedupRatio: 1.05, Atol: -1, Rtol: 0}))
	require.Error(t, ValidateThresholds(VerificationThresholds{MinSpeedupRatio: 1.05, Atol: 0, Rtol: math.NaN()}))
	require.Error(t, ValidateTrace(nil, DefaultVerificationThresholds()))
	require.Error(t, ValidateEvaluation(nil, DefaultVerificationThresholds()))
	require.Error(t, ValidateDispatchManifest(nil, 1.05))
	require.Error(t, ValidateVerificationReport(nil, ""))

	badSchemaDM := *manifest
	badSchemaDM.SchemaVersion = "wrong/v0"
	require.Error(t, ValidateDispatchManifest(&badSchemaDM, 1.05))

	dupDM := *manifest
	dupDM.Entries = []DispatchManifestEntry{manifest.Entries[0], manifest.Entries[0]}
	require.Error(t, ValidateDispatchManifest(&dupDM, 1.05))

	badChecksumDM := *manifest
	badChecksumDM.Checksums = map[string]string{"../escape": digestA}
	require.Error(t, ValidateDispatchManifest(&badChecksumDM, 1.05))

	badChecksumValDM := *manifest
	badChecksumValDM.Checksums = map[string]string{"kernels/a.py": "sha256:short"}
	require.Error(t, ValidateDispatchManifest(&badChecksumValDM, 1.05))

	emptyDM := *manifest
	emptyDM.Entries = nil
	require.Error(t, ValidateDispatchManifest(&emptyDM, 1.05))

	badCountRep := *report
	badCountRep.PassedCount = 2
	require.Error(t, ValidateVerificationReport(&badCountRep, digestA))

	badSchemaRep := *report
	badSchemaRep.SchemaVersion = "wrong/v0"
	require.Error(t, ValidateVerificationReport(&badSchemaRep, digestA))

	badEvalRep := *report
	badEvalRep.Entries = []VerificationEntryReport{
		{
			Index:          0,
			LocalKernelKey: validKey,
			Passed:         true,
			Evaluation: &TraceEvaluation{
				Status:      StatusPassed,
				Correctness: &TraceCorrectness{MaxAbsError: 99, MaxRelativeError: 99},
				Performance: &TracePerformance{SpeedupFactor: 1.25},
			},
		},
	}
	require.Error(t, ValidateVerificationReport(&badEvalRep, digestA))

	// Additional Definition, Workload, Solution field validation checks.
	hidden := int64(4096)
	badDef := &Definition{
		Name:   "rmsnorm_x",
		OpType: modelkernelkey.OpFamilyRMSNorm,
		Axes:   map[string]AxisSpec{"hidden_size": {Type: "const", Value: &hidden}, "bad": {Type: "other"}},
	}
	require.Error(t, ValidateDefinition(badDef))
	badDefVar := &Definition{
		Name:   "rmsnorm_x",
		OpType: modelkernelkey.OpFamilyRMSNorm,
		Axes:   map[string]AxisSpec{"num_tokens": {Type: "var", Value: &hidden}},
	}
	require.Error(t, ValidateDefinition(badDefVar))
	badDefMismatchKey := &Definition{
		Name:           "rmsnorm_x",
		OpType:         modelkernelkey.OpFamilyRMSNorm,
		LocalKernelKey: sampleValidKernelKey(t, modelkernelkey.OpFamilyLayerNorm),
		Axes:           map[string]AxisSpec{"hidden_size": {Type: "const", Value: &hidden}},
		Inputs:         map[string]TensorSpec{"x": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"}},
		Outputs:        map[string]TensorSpec{"out": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"}},
		Reference:      "def run(x): return x",
	}
	require.Error(t, ValidateDefinition(badDefMismatchKey))

	require.Error(t, ValidateWorkload(&Workload{}))
	require.Error(t, ValidateWorkload(&Workload{UUID: "wl", Inputs: map[string]WorkloadInput{"x": {Type: ""}}}))
	require.Error(t, ValidateWorkload(&Workload{UUID: "wl", Inputs: map[string]WorkloadInput{"x": {Type: "random"}}, ObservedDurationUs: -1}))
	require.Error(t, ValidateWorkload(&Workload{UUID: "wl", Inputs: map[string]WorkloadInput{"x": {Type: "random"}}, CallCount: -1}))

	require.Error(t, ValidateSolution(&Solution{}))
	require.Error(t, ValidateSolution(&Solution{Name: "s", Definition: "d", Author: "a"}))
	require.Error(t, ValidateSolution(&Solution{
		Name: "s", Definition: "d", Author: "a",
		Spec: SolutionSpec{Language: "python", EntryPoint: "run", TargetHardware: []string{"nvidia:sm_90a"}},
	}))

	// Entry field validation checks on DispatchManifest.
	entryBadCases := []func(*DispatchManifestEntry){
		func(e *DispatchManifestEntry) { e.Definition = "" },
		func(e *DispatchManifestEntry) { e.SolutionPath = "../bad" },
		func(e *DispatchManifestEntry) { e.SolutionSHA256 = "bad" },
		func(e *DispatchManifestEntry) { e.LibrarySHA256 = "bad" },
		func(e *DispatchManifestEntry) { e.Entrypoint = "" },
		func(e *DispatchManifestEntry) { e.TracePath = "" },
		func(e *DispatchManifestEntry) { e.TraceSHA256 = "bad" },
		func(e *DispatchManifestEntry) { e.BaselineLatencyUs = -1 },
		func(e *DispatchManifestEntry) { e.ForgedLatencyUs = -1 },
		func(e *DispatchManifestEntry) { e.SpeedupRatio = 0 },
	}
	for _, mutate := range entryBadCases {
		dmCopy := *manifest
		dmCopy.Entries = []DispatchManifestEntry{manifest.Entries[0]}
		mutate(&dmCopy.Entries[0])
		require.Error(t, ValidateDispatchManifest(&dmCopy, 1.05))
	}
}
