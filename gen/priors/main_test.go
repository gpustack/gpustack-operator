package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// measuredRows is a public benchmark sweep of one shape.
//
// IT IS WRITTEN AS THE SOURCE PUBLISHED IT, because a fixture in another shape
// proves nothing about the shape the reader reads.
const measuredRows = `[
  {
    "model": "dsr1",
    "canonical_model": "deepseek-ai/DeepSeek-R1-0528",
    "framework": "sglang",
    "hardware": "mi325x",
    "precision": "fp8",
    "spec_method": "none",
    "is_multinode": false,
    "disagg": false,
    "conc": 4,
    "isl": 8192,
    "osl": 1024,
    "date": "2026-06-11",
    "prefill_tp": 8,
    "prefill_ep": 1,
    "num_prefill_gpu": 8,
    "metrics": {
      "tput_per_gpu": 500,
      "input_tput_per_gpu": 450,
      "output_tput_per_gpu": 50,
      "mean_itl": 0.01,
      "mean_tpot": 0.01,
      "median_ttft": 0.1,
      "p90_ttft": 0.2,
      "p99_ttft": 0.3,
      "median_itl": 0.01,
      "p90_itl": 0.012,
      "p99_itl": 0.015
    }
  },
  {
    "model": "dsr1",
    "canonical_model": "deepseek-ai/DeepSeek-R1-0528",
    "framework": "sglang",
    "hardware": "mi325x",
    "precision": "fp8",
    "spec_method": "none",
    "is_multinode": false,
    "disagg": false,
    "conc": 8,
    "isl": 8192,
    "osl": 1024,
    "date": "2026-06-11",
    "prefill_tp": 8,
    "prefill_ep": 1,
    "num_prefill_gpu": 8,
    "metrics": {
      "tput_per_gpu": 900,
      "input_tput_per_gpu": 800,
      "output_tput_per_gpu": 100,
      "mean_itl": 0.01,
      "mean_tpot": 0.01,
      "median_ttft": 0.2,
      "p90_ttft": 0.3,
      "p99_ttft": 0.4,
      "median_itl": 0.011,
      "p90_itl": 0.013,
      "p99_itl": 0.016
    }
  }
]`

// twoProfileCatalog is a catalog that holds two profiles of two shapes.
const twoProfileCatalog = `schemaVersion: gpustack.ai/v1
profiles:
  - role: Server
    model: org/b-model
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points:
      - concurrency: 8
        totalTokensPerSecond: 100
        inputTokensPerSecond: 90
        outputTokensPerSecond: 10
        ttftMilliseconds:
          p95: 100
        itlMilliseconds:
          p95: 10
  - role: Server
    model: org/a-model
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points:
      - concurrency: 8
        totalTokensPerSecond: 100
        inputTokensPerSecond: 90
        outputTokensPerSecond: 10
        ttftMilliseconds:
          p95: 100
        itlMilliseconds:
          p95: 10
`

// threeProfileCatalog is a catalog that holds three profiles of three shapes.
//
// THE TWO PROFILES OF THE SAME MODEL DIFFER IN THE ENGINE, because a filter
// that names a model and an engine has to retire only one of them.
const threeProfileCatalog = `schemaVersion: gpustack.ai/v1
profiles:
  - role: Server
    model: deepseek-ai/DeepSeek-R1-0528
    quantization: fp8
    engine: sglang
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points:
      - concurrency: 8
        totalTokensPerSecond: 100
        inputTokensPerSecond: 90
        outputTokensPerSecond: 10
        ttftMilliseconds:
          p95: 100
        itlMilliseconds:
          p95: 10
  - role: Server
    model: deepseek-ai/DeepSeek-R1-0528
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points:
      - concurrency: 8
        totalTokensPerSecond: 100
        inputTokensPerSecond: 90
        outputTokensPerSecond: 10
        ttftMilliseconds:
          p95: 100
        itlMilliseconds:
          p95: 10
  - role: Server
    model: org/other
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points:
      - concurrency: 8
        totalTokensPerSecond: 100
        inputTokensPerSecond: 90
        outputTokensPerSecond: 10
        ttftMilliseconds:
          p95: 100
        itlMilliseconds:
          p95: 10
`

// writeCatalog writes a catalog file and returns its path.
func writeCatalog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "priors.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the catalog: %v", err)
	}
	return path
}

// writeRows writes a measured sweep and returns its path.
func writeRows(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rows.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the rows: %v", err)
	}
	return path
}

// readProfiles returns the profiles the catalog holds.
func readProfiles(t *testing.T, path string) []priors.ProfileDocument {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	document, err := priors.ParseCatalogDocument(raw)
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	return document.Profiles
}

// readModels returns the models of the profiles the catalog holds.
//
// IT NAMES THE MODELS AND NOT THE PROFILES, because a profile is known by its
// shape and the shape of a profile the catalog holds is what the reader checks.
func readModels(t *testing.T, path string) []string {
	t.Helper()
	profiles := readProfiles(t, path)
	models := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		models = append(models, profile.Model)
	}
	return models
}

// TestFormatCatalogWritesTheCanonicalCatalog asserts the default mode writes
// the catalog back in the canonical form.
//
// IT MATTERS BECAUSE A CATALOG THAT IS WRITTEN BY HAND AND CHECKED BY A MACHINE
// IS A CATALOG WHOSE TWO SPELLINGS CANNOT DRIFT APART.
func TestFormatCatalogWritesTheCanonicalCatalog(t *testing.T) {
	path := writeCatalog(t, twoProfileCatalog)

	if err := formatCatalog(path); err != nil {
		t.Fatalf("format the catalog: %v", err)
	}
	profiles := readProfiles(t, path)
	if len(profiles) != 2 {
		t.Fatalf("the catalog holds %d profiles, want 2", len(profiles))
	}
	if profiles[0].Model != "org/a-model" {
		t.Fatalf("the first profile measures the model %q", profiles[0].Model)
	}
	if profiles[0].Topology.GPUsPerReplica != 8 ||
		profiles[0].Topology.TensorParallel != 8 ||
		profiles[0].Topology.ExpertParallel != 1 {
		t.Fatalf("the first profile measured the topology %+v", profiles[0].Topology)
	}

	// THE SECOND RUN WRITES THE SAME BYTES, because a catalog that changes on
	// every run is a file nobody can diff.
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if err := formatCatalog(path); err != nil {
		t.Fatalf("format the catalog: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two writes of one catalog differ")
	}
}

// TestMergeIntoAddsTheMeasuredProfiles asserts the merge mode writes the
// measured profiles into the catalog.
//
// IT MATTERS BECAUSE THE CATALOG IS BUILT FROM MEASUREMENTS AND NOT FROM
// ASSERTIONS, and the merge is how a measurement reaches the catalog.
func TestMergeIntoAddsTheMeasuredProfiles(t *testing.T) {
	path := writeCatalog(t, "schemaVersion: gpustack.ai/v1\nprofiles: []\n")
	source := writeRows(t, measuredRows)

	if err := mergeInto(path, source); err != nil {
		t.Fatalf("merge the rows: %v", err)
	}
	profiles := readProfiles(t, path)
	if len(profiles) != 1 {
		t.Fatalf("the catalog holds %d profiles, want 1", len(profiles))
	}
	profile := profiles[0]
	if profile.Model != "deepseek-ai/DeepSeek-R1-0528" {
		t.Fatalf("the profile measures the model %q", profile.Model)
	}
	if profile.Quantization != "fp8" {
		t.Fatalf("the profile measures the recipe %q", profile.Quantization)
	}
	if profile.Engine != "sglang" {
		t.Fatalf("the profile measured the engine %q", profile.Engine)
	}
	if profile.Hardware != "amd-mi325x" {
		t.Fatalf("the profile measured the accelerator %q", profile.Hardware)
	}
	if profile.Topology.GPUsPerReplica != 8 ||
		profile.Topology.TensorParallel != 8 ||
		profile.Topology.ExpertParallel != 1 {
		t.Fatalf("the profile measured the topology %+v", profile.Topology)
	}
	if len(profile.Points) != 2 {
		t.Fatalf("the profile holds %d points, want 2", len(profile.Points))
	}
	if profile.Points[0].TotalTokensPerSecond != 4000 {
		t.Fatalf("the first point sustained %v tokens per second, want 4000", profile.Points[0].TotalTokensPerSecond)
	}
}

// TestMergeIntoReplacesTheProfileItHolds asserts a measurement of a shape
// replaces the measurement the catalog held.
//
// IT MATTERS BECAUSE ONE SHAPE MEASURED TWICE IS ONE PROFILE AND NOT TWO.
func TestMergeIntoReplacesTheProfileItHolds(t *testing.T) {
	path := writeCatalog(t, "schemaVersion: gpustack.ai/v1\nprofiles: []\n")
	source := writeRows(t, measuredRows)

	if err := mergeInto(path, source); err != nil {
		t.Fatalf("merge the rows: %v", err)
	}
	if err := mergeInto(path, source); err != nil {
		t.Fatalf("merge the rows: %v", err)
	}
	profiles := readProfiles(t, path)
	if len(profiles) != 1 {
		t.Fatalf("the catalog holds %d profiles, want 1", len(profiles))
	}
	if len(profiles[0].Points) != 2 {
		t.Fatalf("the profile holds %d points, want 2", len(profiles[0].Points))
	}
}

// TestMergeIntoRefusesASweepItCannotUse asserts the merge mode writes nothing
// when the source measured nothing the catalog can hold.
//
// IT MATTERS BECAUSE A CATALOG THAT HOLDS A PROFILE OF A MEASUREMENT IT NEVER
// MADE IS A CATALOG NOBODY CAN ACCOUNT FOR.
func TestMergeIntoRefusesASweepItCannotUse(t *testing.T) {
	path := writeCatalog(t, "schemaVersion: gpustack.ai/v1\nprofiles: []\n")
	source := writeRows(t, `[{"model":"dsr1","framework":"trt","hardware":"mi325x","precision":"fp8","conc":4}]`)

	if err := mergeInto(path, source); err != nil {
		t.Fatalf("merge the rows: %v", err)
	}
	profiles := readProfiles(t, path)
	if len(profiles) != 0 {
		t.Fatalf("the catalog holds %d profiles, want 0", len(profiles))
	}
}

// TestMergeIntoRefusesRowsItCannotRead asserts the merge mode refuses a file
// it cannot read.
//
// IT MATTERS BECAUSE A MERGE THAT SILENTLY ADDED NOTHING WOULD LOOK LIKE A
// SOURCE THAT MEASURED NOTHING.
func TestMergeIntoRefusesRowsItCannotRead(t *testing.T) {
	path := writeCatalog(t, "schemaVersion: gpustack.ai/v1\nprofiles: []\n")
	source := writeRows(t, "not json")

	if err := mergeInto(path, source); err == nil {
		t.Fatal("the unreadable rows were merged")
	}
}

// TestRetireFromRemovesTheProfilesTheFilterNames asserts the retire mode
// removes the profiles the filter names and keeps the rest.
//
// IT MATTERS BECAUSE A CATALOG THAT LOSES A PROFILE SILENTLY IS A CATALOG
// NOBODY CAN ACCOUNT FOR, and a filter that names a family of profiles has to
// retire the whole family.
func TestRetireFromRemovesTheProfilesTheFilterNames(t *testing.T) {
	cases := []struct {
		name   string
		filter retireFilter
		want   []string
	}{
		{
			name:   "a filter that names nothing retires nothing",
			filter: retireFilter{Model: "org/unknown-model"},
			want: []string{
				"deepseek-ai/DeepSeek-R1-0528",
				"deepseek-ai/DeepSeek-R1-0528",
				"org/other",
			},
		},
		{
			name:   "a model filter retires every profile of that model",
			filter: retireFilter{Model: "deepseek-ai/DeepSeek-R1-0528"},
			want:   []string{"org/other"},
		},
		{
			name: "a model and an engine filter retire only the profiles that match both",
			filter: retireFilter{
				Model:  "deepseek-ai/DeepSeek-R1-0528",
				Engine: "sglang",
			},
			want: []string{"deepseek-ai/DeepSeek-R1-0528", "org/other"},
		},
		{
			name:   "a hardware filter retires the profiles of that accelerator",
			filter: retireFilter{Hardware: "amd-mi325x"},
			want:   []string{},
		},
		{
			name:   "a tensor parallel filter retires the profiles of that width",
			filter: retireFilter{TensorParallel: 8},
			want:   []string{},
		},
		{
			name:   "a filter that names another width retires nothing",
			filter: retireFilter{TensorParallel: 4},
			want: []string{
				"deepseek-ai/DeepSeek-R1-0528",
				"deepseek-ai/DeepSeek-R1-0528",
				"org/other",
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeCatalog(t, threeProfileCatalog)

			if err := retireFrom(path, testCase.filter); err != nil {
				t.Fatalf("retire the profiles: %v", err)
			}
			models := readModels(t, path)
			if !slices.Equal(models, testCase.want) {
				t.Fatalf("the catalog holds the models %v, want %v", models, testCase.want)
			}
		})
	}
}

// TestRetireWithNoFilterFails asserts a retirement that names nothing is
// refused.
//
// IT MATTERS BECAUSE A RETIREMENT THAT NAMES NOTHING RETIRES EVERY PROFILE, and
// A CATALOG THAT LOST EVERY PROFILE SILENTLY IS A CATALOG NOBODY CAN ACCOUNT
// FOR.
func TestRetireWithNoFilterFails(t *testing.T) {
	path := writeCatalog(t, threeProfileCatalog)

	err := run(path, "", true, retireFilter{})
	if err == nil {
		t.Fatal("the retirement without a filter was accepted")
	}
	if !strings.Contains(err.Error(), "--retire needs at least one of") {
		t.Fatalf("the refusal %q does not name the filters", err)
	}
}

// TestRunRetiresTheProfilesTheFilterNames asserts the retire mode of the
// command applies the filter it was given.
//
// IT MATTERS BECAUSE THE FLAGS ARE THE ONLY WAY A PERSON NAMES A RETIREMENT.
func TestRunRetiresTheProfilesTheFilterNames(t *testing.T) {
	path := writeCatalog(t, threeProfileCatalog)

	if err := run(path, "", true, retireFilter{Model: "deepseek-ai/DeepSeek-R1-0528"}); err != nil {
		t.Fatalf("retire the model: %v", err)
	}
	models := readModels(t, path)
	want := []string{"org/other"}
	if !slices.Equal(models, want) {
		t.Fatalf("the catalog holds the models %v, want %v", models, want)
	}
}

// TestReadDocumentReadsAnEmptyCatalogWhenTheFileIsMissing asserts a catalog
// that does not exist yet is a catalog with no profiles.
//
// IT MATTERS BECAUSE THE FIRST MERGE RUNS AGAINST A CATALOG THAT DOES NOT
// EXIST YET.
func TestReadDocumentReadsAnEmptyCatalogWhenTheFileIsMissing(t *testing.T) {
	document, err := readDocument(filepath.Join(t.TempDir(), "priors.yaml"))
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if document.SchemaVersion != priors.SchemaVersion {
		t.Fatalf("the catalog carries the schema %q", document.SchemaVersion)
	}
	if len(document.Profiles) != 0 {
		t.Fatalf("the catalog holds %d profiles, want 0", len(document.Profiles))
	}
}

// TestResolvePathKeepsAnAbsolutePath asserts a file outside the tree is the
// file the operator pointed at.
//
// IT MATTERS BECAUSE A PATH THAT IS JOINED ONTO THE TREE NAMES A FILE THAT IS
// NOT THERE.
func TestResolvePathKeepsAnAbsolutePath(t *testing.T) {
	if got := resolvePath("/tree", "/tmp/rows.json"); got != "/tmp/rows.json" {
		t.Fatalf("the path resolved to %q", got)
	}
	if got := resolvePath("/tree", "rows.json"); got != "/tree/rows.json" {
		t.Fatalf("the path resolved to %q", got)
	}
}

// TestTheMergedCatalogIsReadable asserts the catalog the merge writes is a
// catalog the operator reads.
//
// IT MATTERS BECAUSE A CATALOG THE READER REFUSES REFUSES EVERY RESOLUTION.
func TestTheMergedCatalogIsReadable(t *testing.T) {
	path := writeCatalog(t, "schemaVersion: gpustack.ai/v1\nprofiles: []\n")
	source := writeRows(t, measuredRows)
	if err := mergeInto(path, source); err != nil {
		t.Fatalf("merge the rows: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if _, err := priors.ParseCatalog(raw); err != nil {
		t.Fatalf("the merged catalog does not parse: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err == nil {
		t.Fatal("the catalog is not YAML")
	}
}
