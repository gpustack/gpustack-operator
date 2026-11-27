package forger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/worker/kernelkey"
)

func setupMockInputDir(t *testing.T) (string, string) {
	t.Helper()

	inputDir := filepath.Join(t.TempDir(), "input")
	rmsKey := sampleValidKernelKey(t, kernelkey.OpFamilyRMSNorm)
	ropeKey := sampleValidKernelKey(t, kernelkey.OpFamilyRopeKVAppend)
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
		OpType:         kernelkey.OpFamilyRMSNorm,
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

	// Run forge-mock via Cobra command to test CLI flag wiring too.
	mockCmd := NewForgeMockCommand()
	mockCmd.SetArgs([]string{
		"--input-dir", inputDir,
		"--output-dir", outputDir,
	})
	require.NoError(t, mockCmd.Execute())

	// Confirm deterministic rmsnorm_mock_v1 artifacts were generated.
	require.FileExists(t, filepath.Join(outputDir, "kernels", "rmsnorm_mock_v1", "kernel.py"))
	require.FileExists(t, filepath.Join(outputDir, "solutions", "rmsnorm_mock_v1.json"))
	require.FileExists(t, filepath.Join(outputDir, "traces", "rmsnorm_mock_v1.json"))
	require.FileExists(t, filepath.Join(outputDir, DispatchManifestFileName))

	// Run forge-verify via Cobra command.
	verifyCmd := NewForgeVerifyCommand()
	verifyCmd.SetArgs([]string{
		"--input-dir", inputDir,
		"--output-dir", outputDir,
		"--report-path", reportPath,
	})
	require.NoError(t, verifyCmd.Execute())

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

	// Exits cleanly on context cancellation via cobra command.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	cmd := NewForgeHoldCommand()
	cmd.SetArgs([]string{"--max-duration", "10s"})
	require.NoError(t, cmd.ExecuteContext(ctx))
}

func TestForgeMockAndVerify_MinimalInputAndFailureModes(t *testing.T) {
	t.Parallel()

	// forge-mock with only forge-request.json (no definitions/ or workloads/ dirs) and non-rmsnorm key.
	inputDir := filepath.Join(t.TempDir(), "minimal-in")
	outputDir := filepath.Join(t.TempDir(), "minimal-out")
	reportPath := filepath.Join(t.TempDir(), "verified", VerificationReportFileName)
	require.NoError(t, os.MkdirAll(inputDir, 0o750))

	ropeKey := sampleValidKernelKey(t, kernelkey.OpFamilyRopeKVAppend)
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

	// Validation error branches for forge-mock.
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

	// Entry-level rejection cases in forge-verify: bad localKernelKey, incompatible key, empty entrypoint,
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
			entry: DispatchManifestEntry{LocalKernelKey: "vllm/0.8.5/nvidia/sm_80/rmsnorm/0123456789abcdef", Entrypoint: "run"},
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
