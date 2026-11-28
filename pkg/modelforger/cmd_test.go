package modelforger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelkernelkey"
	"gpustack.ai/gpustack/pkg/utils/json"
)

func setupMockInputDir(t *testing.T) (string, string) {
	t.Helper()

	inputDir := filepath.Join(t.TempDir(), "input")
	rmsKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	ropeKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRopeKVAppend)
	hidden := int64(4096)

	req := ForgeRequest{
		SchemaVersion:       ForgeRequestSchemaVersion,
		Engine:              "vLLM",
		EngineVersion:       "0.8.5",
		Manufacturer:        "nvidia",
		Architecture:        "sm_90a",
		TargetKernelKeys:    []string{ropeKey, rmsKey},
		BaselineLatenciesUs: map[string]float64{rmsKey: 120.0},
		MinSpeedupRatio:     1.05,
		Atol:                1e-2,
		Rtol:                1e-2,
	}
	def := Definition{
		Name:           "rmsnorm_hidden4096",
		OpType:         modelkernelkey.OpFamilyRMSNorm,
		LocalKernelKey: rmsKey,
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
	wl := Workload{
		UUID:               "wl-rms-1",
		Definition:         def.Name,
		LocalKernelKey:     rmsKey,
		Axes:               map[string]int64{"num_tokens": 128},
		Inputs:             map[string]WorkloadInput{"x": {Type: "random"}, "weight": {Type: "random"}},
		ObservedDurationUs: 120.0,
		CallCount:          8,
	}

	require.NoError(t, os.MkdirAll(filepath.Join(inputDir, "definitions"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(inputDir, "workloads"), 0o750))

	reqBytes, err := json.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inputDir, ForgeRequestFileName), reqBytes, 0o600))

	defBytes, err := json.Marshal(def)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inputDir, "definitions", "rmsnorm_hidden4096.json"), defBytes, 0o600))

	wlBytes, err := json.Marshal(wl)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inputDir, "workloads", "rmsnorm_hidden4096.jsonl"), append(wlBytes, '\n'), 0o600))

	return inputDir, rmsKey
}

func TestForgeMockAndVerify_EndToEndPipeline(t *testing.T) {
	t.Parallel()

	inputDir, rmsKey := setupMockInputDir(t)
	outputDir := filepath.Join(t.TempDir(), "output")
	reportPath := filepath.Join(t.TempDir(), "verified", VerificationReportFileName)

	// Run model-forger mock via top-level Cobra command to test CLI subcommand and flag wiring.
	rootMockCmd := NewCommand()
	require.Equal(t, "model-forger", rootMockCmd.Use)
	require.True(t, rootMockCmd.Hidden)
	rootMockCmd.SetArgs([]string{
		"mock",
		"--input-dir", inputDir,
		"--output-dir", outputDir,
	})
	require.NoError(t, rootMockCmd.Execute())

	// Confirm deterministic rmsnorm_mock_v1 artifacts were generated.
	require.FileExists(t, filepath.Join(outputDir, "kernels", "rmsnorm_mock_v1", "kernel.py"))
	require.FileExists(t, filepath.Join(outputDir, "solutions", "rmsnorm_mock_v1.json"))
	require.FileExists(t, filepath.Join(outputDir, "traces", "rmsnorm_mock_v1.json"))
	require.FileExists(t, filepath.Join(outputDir, DispatchManifestFileName))

	// Run model-forger verify via top-level Cobra command.
	rootVerifyCmd := NewCommand()
	rootVerifyCmd.SetArgs([]string{
		"verify",
		"--input-dir", inputDir,
		"--output-dir", outputDir,
		"--report-path", reportPath,
	})
	require.NoError(t, rootVerifyCmd.Execute())

	// Parse and validate emitted verification-report.json.
	reportRaw, err := os.ReadFile(reportPath)
	require.NoError(t, err)
	var report VerificationReport
	require.NoError(t, json.Unmarshal(reportRaw, &report))
	require.NoError(t, ValidateVerificationReport(&report, ""))
	require.Equal(t, VerificationReportSchemaVersion, report.SchemaVersion)
	require.Equal(t, StatusPassed, report.OverallStatus)
	require.Equal(t, 1, report.PassedCount)
	require.Equal(t, 0, report.FailedCount)
	require.Len(t, report.Entries, 1)
	require.Equal(t, rmsKey, report.Entries[0].LocalKernelKey)
	require.True(t, report.Entries[0].Passed)
	require.NotNil(t, report.Entries[0].Evaluation)
	require.InDelta(t, 1.25, report.Entries[0].Evaluation.Performance.SpeedupFactor, 1e-9)
}

func TestForgeVerify_RejectsTamperingSymlinksHardlinksAndThresholds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		tamper       func(t *testing.T, outputDir string)
		verifyOpts   func(opts *ForgeVerifyOptions)
		wantErrMatch string
	}{
		{
			name: "checksum mismatch on kernel.py",
			tamper: func(t *testing.T, outputDir string) {
				p := filepath.Join(outputDir, "kernels", "rmsnorm_mock_v1", "kernel.py")
				require.NoError(t, os.WriteFile(p, []byte("def run(x): return x + 1\n"), 0o600))
			},
			wantErrMatch: "checksum mismatch",
		},
		{
			name: "symlink kernel.py rejected",
			tamper: func(t *testing.T, outputDir string) {
				target := filepath.Join(outputDir, "real_kernel.py")
				p := filepath.Join(outputDir, "kernels", "rmsnorm_mock_v1", "kernel.py")
				raw, err := os.ReadFile(p)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(target, raw, 0o600))
				require.NoError(t, os.Remove(p))
				require.NoError(t, os.Symlink(target, p))
			},
			wantErrMatch: "not a regular file",
		},
		{
			name: "hardlink kernel.py rejected",
			tamper: func(t *testing.T, outputDir string) {
				p := filepath.Join(outputDir, "kernels", "rmsnorm_mock_v1", "kernel.py")
				linkPath := filepath.Join(outputDir, "hardlink_kernel.py")
				require.NoError(t, os.Link(p, linkPath))
			},
			wantErrMatch: "hardlink count",
		},
		{
			name: "speedup threshold override rejects 1.25",
			tamper: func(*testing.T, string) {
			},
			verifyOpts: func(opts *ForgeVerifyOptions) {
				high := 1.50
				opts.MinSpeedupRatio = &high
			},
			wantErrMatch: "speedup_factor",
		},
		{
			name: "non-PASSED trace rejected",
			tamper: func(t *testing.T, outputDir string) {
				tracePath := filepath.Join(outputDir, "traces", "rmsnorm_mock_v1.json")
				raw, err := os.ReadFile(tracePath)
				require.NoError(t, err)
				var tr Trace
				require.NoError(t, json.Unmarshal(raw, &tr))
				tr.Evaluation.Status = StatusFailed
				updated, err := marshalPrettyJSON(tr)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(tracePath, updated, 0o600))

				// Re-sign manifest checksum so verifier reaches Trace status validation.
				dmPath := filepath.Join(outputDir, DispatchManifestFileName)
				dmRaw, err := os.ReadFile(dmPath)
				require.NoError(t, err)
				var dm DispatchManifest
				require.NoError(t, json.Unmarshal(dmRaw, &dm))
				newDigest := sha256DigestBytes(updated)
				dm.Entries[0].TraceSHA256 = newDigest
				dm.Checksums[dm.Entries[0].TracePath] = newDigest
				dmUpdated, err := marshalPrettyJSON(dm)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(dmPath, dmUpdated, 0o600))
			},
			wantErrMatch: "PASSED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inputDir, _ := setupMockInputDir(t)
			outputDir := filepath.Join(t.TempDir(), "output")
			reportPath := filepath.Join(t.TempDir(), "verified", VerificationReportFileName)

			require.NoError(t, RunForgeMock(ForgeMockOptions{
				InputDir:  inputDir,
				OutputDir: outputDir,
			}))
			tc.tamper(t, outputDir)

			opts := ForgeVerifyOptions{
				InputDir:   inputDir,
				OutputDir:  outputDir,
				ReportPath: reportPath,
			}
			if tc.verifyOpts != nil {
				tc.verifyOpts(&opts)
			}

			report, err := RunForgeVerify(opts)
			require.Error(t, err)
			require.NotNil(t, report)
			require.Equal(t, StatusFailed, report.OverallStatus)
			require.Equal(t, 0, report.PassedCount)
			require.Len(t, report.Entries, 1)
			require.False(t, report.Entries[0].Passed)
			require.Contains(t, report.Entries[0].Reason, tc.wantErrMatch)
			require.FileExists(t, reportPath)
		})
	}
}

func TestForgeHold_ExitsOnSignalOrDeadline(t *testing.T) {
	t.Parallel()

	// Invalid duration rejected.
	require.Error(t, RunForgeHold(context.Background(), 0))

	// Exits cleanly when deadline elapses.
	start := time.Now()
	require.NoError(t, RunForgeHold(context.Background(), 20*time.Millisecond))
	require.GreaterOrEqual(t, time.Since(start), 15*time.Millisecond)

	// Exits cleanly on context cancellation via top-level cobra command.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	cmd := NewCommand()
	cmd.SetArgs([]string{"hold", "--max-duration", "10s"})
	require.NoError(t, cmd.ExecuteContext(ctx))
}

func TestForgeMockAndVerify_MinimalInputAndFailureModes(t *testing.T) {
	t.Parallel()

	// model-forger mock with only forge-request.json (no definitions/ or workloads/ dirs) and non-rmsnorm key.
	inputDir := filepath.Join(t.TempDir(), "minimal-in")
	outputDir := filepath.Join(t.TempDir(), "minimal-out")
	reportPath := filepath.Join(t.TempDir(), "verified", VerificationReportFileName)
	require.NoError(t, os.MkdirAll(inputDir, 0o750))

	ropeKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRopeKVAppend)
	req := ForgeRequest{
		SchemaVersion:    ForgeRequestSchemaVersion,
		Engine:           "vLLM",
		EngineVersion:    "0.8.5",
		Manufacturer:     "nvidia",
		Architecture:     "sm_90a",
		TargetKernelKeys: []string{ropeKey},
		MinSpeedupRatio:  1.05,
		Atol:             1e-2,
		Rtol:             1e-2,
	}
	reqBytes, err := json.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inputDir, ForgeRequestFileName), reqBytes, 0o600))

	require.NoError(t, RunForgeMock(ForgeMockOptions{
		InputDir:  inputDir,
		OutputDir: outputDir,
	}))
	require.FileExists(t, filepath.Join(outputDir, "kernels", "rope_kv_append_mock_v1", "kernel.py"))

	// Verify via CLI with explicit threshold overrides.
	verifyCmd := NewForgeVerifyCommand()
	verifyCmd.SetArgs([]string{
		"--input-dir", inputDir,
		"--output-dir", outputDir,
		"--report-path", reportPath,
		"--min-speedup-ratio", "1.10",
		"--atol", "0.001",
		"--rtol", "0.001",
	})
	require.NoError(t, verifyCmd.Execute())

	// Validation error branches for model-forger mock.
	require.Error(t, RunForgeMock(ForgeMockOptions{}))
	require.Error(t, RunForgeMock(ForgeMockOptions{InputDir: inputDir}))
	require.Error(t, RunForgeMock(ForgeMockOptions{InputDir: inputDir, OutputDir: outputDir, SpeedupFactor: -1}))
	require.Error(t, RunForgeMock(ForgeMockOptions{InputDir: filepath.Join(t.TempDir(), "missing"), OutputDir: outputDir}))

	// Missing dispatch-manifest.json writes FAILED report and returns error.
	emptyOut := t.TempDir()
	failReport := filepath.Join(t.TempDir(), "fail-report.json")
	rep, err := RunForgeVerify(ForgeVerifyOptions{
		OutputDir:  emptyOut,
		ReportPath: failReport,
	})
	require.Error(t, err)
	require.Equal(t, StatusFailed, rep.OverallStatus)
	require.FileExists(t, failReport)

	// Unsupported schemaVersion and empty entries in dispatch-manifest.json.
	badSchemaOut := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(badSchemaOut, DispatchManifestFileName),
		[]byte(`{"schemaVersion":"bad/v0","entries":[]}`),
		0o600,
	))
	rep, err = RunForgeVerify(ForgeVerifyOptions{OutputDir: badSchemaOut})
	require.Error(t, err)
	require.Equal(t, StatusFailed, rep.OverallStatus)

	emptyEntriesOut := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(emptyEntriesOut, DispatchManifestFileName),
		[]byte(`{"schemaVersion":"gpustack-forge-dispatch/v1","entries":[]}`),
		0o600,
	))
	rep, err = RunForgeVerify(ForgeVerifyOptions{OutputDir: emptyEntriesOut})
	require.Error(t, err)
	require.Equal(t, StatusFailed, rep.OverallStatus)

	// Invalid output-dir returns error directly.
	_, err = RunForgeVerify(ForgeVerifyOptions{})
	require.Error(t, err)

	// Malformed dispatch-manifest.json returns FAILED report.
	badJSONOut := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(badJSONOut, DispatchManifestFileName), []byte("{bad"), 0o600))
	rep, err = RunForgeVerify(ForgeVerifyOptions{OutputDir: badJSONOut})
	require.Error(t, err)
	require.Equal(t, StatusFailed, rep.OverallStatus)

	// Entry-level rejection cases in model-forger verify: bad localKernelKey, incompatible key, empty entrypoint,
	// missing solution file, malformed solution JSON, missing trace file, and malformed trace JSON.
	badEntries := []struct {
		name  string
		entry DispatchManifestEntry
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "invalid localKernelKey",
			entry: DispatchManifestEntry{LocalKernelKey: "bad/key"},
		},
		{
			name:  "incompatible arch localKernelKey",
			entry: DispatchManifestEntry{LocalKernelKey: "vllm/0.8/nvidia/sm_80/rmsnorm/0123456789abcdef", Entrypoint: "run"},
		},
		{
			name:  "empty entrypoint",
			entry: DispatchManifestEntry{LocalKernelKey: ropeKey, Entrypoint: ""},
		},
		{
			name: "missing solution file",
			entry: DispatchManifestEntry{
				LocalKernelKey: ropeKey,
				Entrypoint:     "run",
				LibraryPath:    "kernels/k.py",
				SolutionPath:   "solutions/missing.json",
			},
			setup: func(t *testing.T, dir string) {
				require.NoError(t, writeOutputRegularFile(dir, "kernels/k.py", []byte("def run(x): return x\n")))
			},
		},
		{
			name: "invalid solution JSON",
			entry: DispatchManifestEntry{
				LocalKernelKey: ropeKey,
				Entrypoint:     "run",
				LibraryPath:    "kernels/k.py",
				SolutionPath:   "solutions/bad.json",
			},
			setup: func(t *testing.T, dir string) {
				require.NoError(t, writeOutputRegularFile(dir, "kernels/k.py", []byte("def run(x): return x\n")))
				require.NoError(t, writeOutputRegularFile(dir, "solutions/bad.json", []byte("{bad")))
			},
		},
		{
			name: "invalid trace JSON",
			entry: DispatchManifestEntry{
				LocalKernelKey: ropeKey,
				Entrypoint:     "run",
				LibraryPath:    "kernels/k.py",
				SolutionPath:   "solutions/ok.json",
				TracePath:      "traces/bad.json",
			},
			setup: func(t *testing.T, dir string) {
				require.NoError(t, writeOutputRegularFile(dir, "kernels/k.py", []byte("def run(x): return x\n")))
				solBytes, mErr := marshalPrettyJSON(Solution{
					Name: "s", Definition: "d", Author: "a",
					Spec:    SolutionSpec{Language: "python", TargetHardware: []string{"nvidia:sm_90a"}, EntryPoint: "run"},
					Sources: []SolutionSource{{Path: "kernels/k.py"}},
				})
				require.NoError(t, mErr)
				require.NoError(t, writeOutputRegularFile(dir, "solutions/ok.json", solBytes))
				require.NoError(t, writeOutputRegularFile(dir, "traces/bad.json", []byte("{bad")))
			},
		},
	}

	for _, tc := range badEntries {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			dm := DispatchManifest{
				SchemaVersion: DispatchManifestSchemaVersion,
				Compatibility: DispatchCompatibility{
					Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_90a",
				},
				Entries: []DispatchManifestEntry{tc.entry},
			}
			dmBytes, mErr := marshalPrettyJSON(dm)
			require.NoError(t, mErr)
			require.NoError(t, writeOutputRegularFile(dir, DispatchManifestFileName, dmBytes))

			r, vErr := RunForgeVerify(ForgeVerifyOptions{OutputDir: dir})
			require.Error(t, vErr)
			require.Equal(t, StatusFailed, r.OverallStatus)
			require.Len(t, r.Entries, 1)
			require.False(t, r.Entries[0].Passed)
		})
	}
}

func writeCandidateEntryFiles(
	t *testing.T, dir, slug, defName, kernelKey string, speedup float64,
) DispatchManifestEntry {
	t.Helper()

	libRel := filepath.ToSlash(filepath.Join("kernels", slug, "kernel.py"))
	solRel := filepath.ToSlash(filepath.Join("solutions", slug+".json"))
	traceRel := filepath.ToSlash(filepath.Join("traces", slug+".json"))

	libBytes := []byte("def run(x):\n    return x\n")
	require.NoError(t, writeOutputRegularFile(dir, libRel, libBytes))

	solBytes, err := marshalPrettyJSON(Solution{
		Name:           slug,
		Definition:     defName,
		LocalKernelKey: kernelKey,
		Author:         "tester",
		Spec: SolutionSpec{
			Language:       "python",
			TargetHardware: []string{"nvidia:sm_90a"},
			EntryPoint:     "run",
		},
		Sources: []SolutionSource{{Path: libRel}},
	})
	require.NoError(t, err)
	require.NoError(t, writeOutputRegularFile(dir, solRel, solBytes))

	traceBytes, err := marshalPrettyJSON(Trace{
		Definition: defName,
		Workload: Workload{
			UUID:           "wl-" + slug,
			Definition:     defName,
			LocalKernelKey: kernelKey,
			Axes:           map[string]int64{"num_tokens": 64},
			Inputs:         map[string]WorkloadInput{"x": {Type: "random"}},
		},
		Solution: slug,
		Evaluation: TraceEvaluation{
			Status:      StatusPassed,
			Correctness: &TraceCorrectness{MaxAbsError: 1e-4, MaxRelativeError: 1e-3},
			Performance: &TracePerformance{LatencyMs: 0.08, ReferenceLatencyMs: 0.10, SpeedupFactor: speedup},
		},
	})
	require.NoError(t, err)
	require.NoError(t, writeOutputRegularFile(dir, traceRel, traceBytes))

	return DispatchManifestEntry{
		LocalKernelKey:    kernelKey,
		Definition:        defName,
		SolutionPath:      solRel,
		SolutionSHA256:    sha256DigestBytes(solBytes),
		LibraryPath:       libRel,
		LibrarySHA256:     sha256DigestBytes(libBytes),
		Entrypoint:        "run",
		TracePath:         traceRel,
		TraceSHA256:       sha256DigestBytes(traceBytes),
		BaselineLatencyUs: 100,
		ForgedLatencyUs:   80,
		SpeedupRatio:      speedup,
	}
}

func TestForgeVerify_MultiEntryManifestRequiresAllPass(t *testing.T) {
	t.Parallel()

	rmsKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	ropeKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRopeKVAppend)

	cases := []struct {
		name        string
		secondSpeed float64
		tamper      func(t *testing.T, dir string)
		wantPassed  int
		wantFailed  int
		wantStatus  string
		wantErr     bool
	}{
		{
			name:        "all entries pass",
			secondSpeed: 1.20,
			wantPassed:  2,
			wantFailed:  0,
			wantStatus:  StatusPassed,
			wantErr:     false,
		},
		{
			name:        "mixed pass and speedup failure is rejected",
			secondSpeed: 1.01,
			wantPassed:  1,
			wantFailed:  1,
			wantStatus:  StatusFailed,
			wantErr:     true,
		},
		{
			name:        "mixed pass and checksum failure is rejected",
			secondSpeed: 1.20,
			tamper: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "kernels", "rope_v1", "kernel.py"),
					[]byte("def run(x):\n    return x + 1\n"),
					0o600,
				))
			},
			wantPassed: 1,
			wantFailed: 1,
			wantStatus: StatusFailed,
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			reportPath := filepath.Join(t.TempDir(), VerificationReportFileName)
			e1 := writeCandidateEntryFiles(t, dir, "rmsnorm_v1", "rmsnorm_hidden4096", rmsKey, 1.25)
			e2 := writeCandidateEntryFiles(t, dir, "rope_v1", "rope_hidden4096", ropeKey, tc.secondSpeed)

			dm := DispatchManifest{
				SchemaVersion: DispatchManifestSchemaVersion,
				Compatibility: DispatchCompatibility{
					Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_90a",
				},
				Checksums: map[string]string{
					e1.LibraryPath:  e1.LibrarySHA256,
					e1.SolutionPath: e1.SolutionSHA256,
					e1.TracePath:    e1.TraceSHA256,
					e2.LibraryPath:  e2.LibrarySHA256,
					e2.SolutionPath: e2.SolutionSHA256,
					e2.TracePath:    e2.TraceSHA256,
				},
				Entries: []DispatchManifestEntry{e1, e2},
			}
			dmBytes, err := marshalPrettyJSON(dm)
			require.NoError(t, err)
			require.NoError(t, writeOutputRegularFile(dir, DispatchManifestFileName, dmBytes))
			if tc.tamper != nil {
				tc.tamper(t, dir)
			}

			report, err := RunForgeVerify(ForgeVerifyOptions{
				OutputDir:  dir,
				ReportPath: reportPath,
			})
			require.NotNil(t, report)
			require.Equal(t, tc.wantStatus, report.OverallStatus)
			require.Equal(t, tc.wantPassed, report.PassedCount)
			require.Equal(t, tc.wantFailed, report.FailedCount)
			require.Len(t, report.Entries, 2)
			require.FileExists(t, reportPath)
			if tc.wantErr {
				require.Error(t, err)
				require.Error(t, ValidateVerificationReport(report, ""))
				forgedPassed := *report
				forgedPassed.OverallStatus = StatusPassed
				require.Error(t, ValidateVerificationReport(&forgedPassed, ""))
			} else {
				require.NoError(t, err)
				require.NoError(t, ValidateVerificationReport(report, ""))
			}
		})
	}
}

func TestForgeVerify_BindsTargetKernelKeysDefinitionsAndCompatibility(t *testing.T) {
	t.Parallel()

	rmsKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	lnKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyLayerNorm)
	ropeKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRopeKVAppend)
	hidden := int64(4096)

	writeRequestAndDefs := func(t *testing.T, targetKeys []string, arch string) string {
		t.Helper()
		inputDir := filepath.Join(t.TempDir(), "input")
		require.NoError(t, os.MkdirAll(filepath.Join(inputDir, "definitions"), 0o750))

		req := ForgeRequest{
			SchemaVersion:    ForgeRequestSchemaVersion,
			Engine:           "vLLM",
			EngineVersion:    "0.8.5",
			Manufacturer:     "nvidia",
			Architecture:     arch,
			TargetKernelKeys: targetKeys,
			MinSpeedupRatio:  1.05,
			Atol:             1e-2,
			Rtol:             1e-2,
		}
		reqBytes, err := json.Marshal(req)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(inputDir, ForgeRequestFileName), reqBytes, 0o600))

		rmsDef := Definition{
			Name:           "rmsnorm_d4096",
			OpType:         modelkernelkey.OpFamilyRMSNorm,
			LocalKernelKey: rmsKey,
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
		defBytes, err := json.Marshal(rmsDef)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(inputDir, "definitions", "rmsnorm_d4096.json"), defBytes, 0o600))

		mismatchedOpDef := Definition{
			Name:           "wrong_op_def",
			OpType:         modelkernelkey.OpFamilyRMSNorm,
			LocalKernelKey: lnKey,
			Axes: map[string]AxisSpec{
				"hidden_size": {Type: "const", Value: &hidden},
				"num_tokens":  {Type: "var"},
			},
			Inputs: map[string]TensorSpec{
				"x": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
			},
			Outputs: map[string]TensorSpec{
				"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
			},
			Reference: "def run(x): return x",
		}
		misBytes, err := json.Marshal(mismatchedOpDef)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(inputDir, "definitions", "wrong_op_def.json"), misBytes, 0o600))

		return inputDir
	}

	cases := []struct {
		name         string
		targetKeys   []string
		reqArch      string
		manifestComp DispatchCompatibility
		entryDef     string
		entryKey     string
		wantPass     bool
		wantErrSub   string
	}{
		{
			name:       "accepts matching targetKernelKey, definition, and ArchCompatible sm_90 vs sm_90a",
			targetKeys: []string{rmsKey},
			reqArch:    "sm_90",
			manifestComp: DispatchCompatibility{
				Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_90a",
			},
			entryDef: "rmsnorm_d4096",
			entryKey: rmsKey,
			wantPass: true,
		},
		{
			name:       "rejects entry localKernelKey not in forge-request targetKernelKeys",
			targetKeys: []string{rmsKey},
			reqArch:    "sm_90a",
			manifestComp: DispatchCompatibility{
				Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_90a",
			},
			entryDef:   "rope_def",
			entryKey:   ropeKey,
			wantErrSub: "targetKernelKeys",
		},
		{
			name:       "rejects definition.local_kernel_key mismatch (cross-op substitution S2)",
			targetKeys: []string{rmsKey, lnKey},
			reqArch:    "sm_90a",
			manifestComp: DispatchCompatibility{
				Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_90a",
			},
			entryDef:   "rmsnorm_d4096",
			entryKey:   lnKey,
			wantErrSub: "local_kernel_key",
		},
		{
			name:       "rejects definition.op_type mismatch against parsed opFamily",
			targetKeys: []string{rmsKey, lnKey},
			reqArch:    "sm_90a",
			manifestComp: DispatchCompatibility{
				Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_90a",
			},
			entryDef:   "wrong_op_def",
			entryKey:   lnKey,
			wantErrSub: "op_type",
		},
		{
			name:       "rejects manifest.compatibility engineVersion mismatch with forge-request",
			targetKeys: []string{rmsKey},
			reqArch:    "sm_90a",
			manifestComp: DispatchCompatibility{
				Engine: "vLLM", EngineVersion: "0.9.0", Manufacturer: "nvidia", Architecture: "sm_90a",
			},
			entryDef:   "rmsnorm_d4096",
			entryKey:   rmsKey,
			wantErrSub: "compatibility",
		},
		{
			name:       "rejects manifest.compatibility architecture mismatch with forge-request",
			targetKeys: []string{rmsKey},
			reqArch:    "sm_90a",
			manifestComp: DispatchCompatibility{
				Engine: "vLLM", EngineVersion: "0.8.5", Manufacturer: "nvidia", Architecture: "sm_80",
			},
			entryDef:   "rmsnorm_d4096",
			entryKey:   rmsKey,
			wantErrSub: "compatibility",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inputDir := writeRequestAndDefs(t, tc.targetKeys, tc.reqArch)
			outDir := filepath.Join(t.TempDir(), "output")
			reportPath := filepath.Join(t.TempDir(), VerificationReportFileName)

			entry := writeCandidateEntryFiles(t, outDir, "cand_v1", tc.entryDef, tc.entryKey, 1.25)
			dm := DispatchManifest{
				SchemaVersion: DispatchManifestSchemaVersion,
				Compatibility: tc.manifestComp,
				Checksums: map[string]string{
					entry.LibraryPath:  entry.LibrarySHA256,
					entry.SolutionPath: entry.SolutionSHA256,
					entry.TracePath:    entry.TraceSHA256,
				},
				Entries: []DispatchManifestEntry{entry},
			}
			dmBytes, err := marshalPrettyJSON(dm)
			require.NoError(t, err)
			require.NoError(t, writeOutputRegularFile(outDir, DispatchManifestFileName, dmBytes))

			report, verifyErr := RunForgeVerify(ForgeVerifyOptions{
				InputDir:   inputDir,
				OutputDir:  outDir,
				ReportPath: reportPath,
			})
			require.NotNil(t, report)
			if tc.wantPass {
				require.NoError(t, verifyErr)
				require.Equal(t, StatusPassed, report.OverallStatus)
				return
			}
			require.Error(t, verifyErr)
			require.Equal(t, StatusFailed, report.OverallStatus)
			combined := verifyErr.Error()
			if len(report.Entries) > 0 {
				combined += " " + report.Entries[0].Reason
			}
			require.Contains(t, combined, tc.wantErrSub)
		})
	}
}

func TestForgeMock_EmitsFamilyAppropriateSignaturesAndMockMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		opFamily      string
		wantSigSubstr string
	}{
		{opFamily: modelkernelkey.OpFamilyRMSNorm, wantSigSubstr: "def run(x, weight=None, eps=1e-6):"},
		{opFamily: modelkernelkey.OpFamilyLayerNorm, wantSigSubstr: "def run(x, weight=None, bias=None, eps=1e-5):"},
		{opFamily: modelkernelkey.OpFamilyActivationQuant, wantSigSubstr: "def run(x):"},
		{opFamily: modelkernelkey.OpFamilyRopeKVAppend, wantSigSubstr: "def run(q, k, cos, sin):"},
		{opFamily: modelkernelkey.OpFamilyMoERouterTopK, wantSigSubstr: "def run(logits, top_k=2):"},
		{opFamily: modelkernelkey.OpFamilyPagedAttention, wantSigSubstr: "def run(q, k_cache, v_cache):"},
	}

	for _, tc := range cases {
		t.Run(tc.opFamily, func(t *testing.T) {
			t.Parallel()
			inputDir := filepath.Join(t.TempDir(), "in")
			outputDir := filepath.Join(t.TempDir(), "out")
			require.NoError(t, os.MkdirAll(inputDir, 0o750))

			key := sampleValidKernelKey(t, tc.opFamily)
			req := ForgeRequest{
				SchemaVersion:    ForgeRequestSchemaVersion,
				Engine:           "vLLM",
				EngineVersion:    "0.8.5",
				Manufacturer:     "nvidia",
				Architecture:     "sm_90a",
				TargetKernelKeys: []string{key},
				MinSpeedupRatio:  1.05,
				Atol:             1e-2,
				Rtol:             1e-2,
			}
			reqBytes, err := json.Marshal(req)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(inputDir, ForgeRequestFileName), reqBytes, 0o600))

			require.NoError(t, RunForgeMock(ForgeMockOptions{
				InputDir:  inputDir,
				OutputDir: outputDir,
			}))

			solID := tc.opFamily + "_mock_v1"
			kernelBytes, err := os.ReadFile(filepath.Join(outputDir, "kernels", solID, "kernel.py"))
			require.NoError(t, err)
			require.Contains(t, string(kernelBytes), tc.wantSigSubstr)

			solBytes, err := os.ReadFile(filepath.Join(outputDir, "solutions", solID+".json"))
			require.NoError(t, err)
			var solMap map[string]any
			require.NoError(t, json.Unmarshal(solBytes, &solMap))
			require.Equal(t, true, solMap["mock"], "solutions/<sol>.json must carry mock=true")

			dmBytes, err := os.ReadFile(filepath.Join(outputDir, DispatchManifestFileName))
			require.NoError(t, err)
			var dmMap map[string]any
			require.NoError(t, json.Unmarshal(dmBytes, &dmMap))
			entries, ok := dmMap["entries"].([]any)
			require.True(t, ok)
			require.Len(t, entries, 1)
			entryMap, ok := entries[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, true, entryMap["mock"], "dispatch-manifest entry must carry mock=true")

			// Verify model-forger verify still passes cleanly on every mock family output.
			report, err := RunForgeVerify(ForgeVerifyOptions{
				InputDir:  inputDir,
				OutputDir: outputDir,
			})
			require.NoError(t, err)
			require.Equal(t, StatusPassed, report.OverallStatus)
		})
	}
}

func TestForgeVerify_FailsClosedOnMissingChecksumAndPreservesThresholdDefaults(t *testing.T) {
	t.Parallel()

	t.Run("checkExpectedChecksum fails closed when no digest is provided", func(t *testing.T) {
		t.Parallel()
		err := checkExpectedChecksum("kernels/op/kernel.py", "sha256:"+strings.Repeat("a", 64), "", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no expected checksum")
	})

	t.Run("loadVerifyThresholds preserves default Atol and Rtol when unset in forge-request.json", func(t *testing.T) {
		t.Parallel()
		inDir := t.TempDir()
		raw := []byte(`{"engine":"vLLM","engineVersion":"0.8.5","manufacturer":"nvidia","architecture":"sm_90a","targetKernelKeys":["k"],"minSpeedupRatio":1.2}`)
		require.NoError(t, os.WriteFile(filepath.Join(inDir, ForgeRequestFileName), raw, 0o600))

		_, th, err := loadVerifyThresholds(ForgeVerifyOptions{InputDir: inDir})
		require.NoError(t, err)
		require.InDelta(t, 1.2, th.MinSpeedupRatio, 1e-9)
		require.InDelta(t, DefaultAtol, th.Atol, 1e-9)
		require.InDelta(t, DefaultRtol, th.Rtol, 1e-9)
	})

	t.Run("hashRegularSingleLinkFile rejects empty library file", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outDir, "empty.so"), nil, 0o600))
		_, err := hashRegularSingleLinkFile(outDir, "empty.so")
		require.Error(t, err)
		require.Contains(t, err.Error(), "out of bounds")
	})
}
