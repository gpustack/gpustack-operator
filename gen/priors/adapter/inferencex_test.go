package adapter

import (
	"strings"
	"testing"

	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// row builds a measured row of a public benchmark.
//
// IT FILLS THE FIELDS THE CATALOG READS AND LEAVES THE REST AT THEIR ZERO
// VALUE, because a row that carries more than the catalog reads would make the
// fixture say more than the reader does.
func row(model, framework, hardware, precision string, concurrency, gpus int) InferenceXRow {
	return InferenceXRow{
		Model:                 model,
		CanonicalModel:        canonical(model),
		Framework:             framework,
		Hardware:              hardware,
		Precision:             precision,
		Concurrency:           concurrency,
		InputLength:           8192,
		OutputLength:          1024,
		Date:                  "2026-06-11",
		PrefillTensorParallel: 8,
		NumPrefillGPU:         gpus,
		Metrics: InferenceXMetrics{
			ThroughputPerGPU:       500,
			InputThroughputPerGPU:  450,
			OutputThroughputPerGPU: 50,
			MeanITL:                0.01,
			MeanTPOT:               0.01,
			MedianTTFT:             0.1,
			P90TTFT:                0.2,
			P99TTFT:                0.3,
			MedianITL:              0.01,
			P90ITL:                 0.012,
			P99ITL:                 0.015,
		},
	}
}

// canonical returns the repository of a model.
func canonical(model string) string {
	switch model {
	case "dsr1":
		return "deepseek-ai/DeepSeek-R1-0528"
	default:
		return model
	}
}

// TestImportInferenceXRowsBuildsOneProfile asserts the rows of one shape become
// one profile.
//
// IT MATTERS BECAUSE ONE SHAPE MEASURED TWICE IS ONE PROFILE AND NOT TWO, and a
// catalog that holds a profile per measurement cannot be read.
func TestImportInferenceXRowsBuildsOneProfile(t *testing.T) {
	rows := []InferenceXRow{
		row("dsr1", "sglang", "mi325x", "fp8", 4, 8),
		row("dsr1", "sglang", "mi325x", "fp8", 8, 8),
		row("dsr1", "sglang", "mi325x", "fp8", 16, 8),
	}
	documents, summary := ImportInferenceXRows(rows)
	if summary.Profiles != 1 {
		t.Fatalf("the import produced %d profiles, want 1", summary.Profiles)
	}
	document := documents[0]
	if document.Model != "deepseek-ai/DeepSeek-R1-0528" {
		t.Fatalf("the profile measures the model %q", document.Model)
	}
	if document.Quantization != "fp8" {
		t.Fatalf("the profile measures the recipe %q", document.Quantization)
	}
	if document.Engine != "sglang" {
		t.Fatalf("the profile measured the engine %q", document.Engine)
	}
	if document.Hardware != "amd-mi325x" {
		t.Fatalf("the profile measured the accelerator %q", document.Hardware)
	}
	if document.Topology.GPUsPerReplica != 8 {
		t.Fatalf("the profile measured %d GPUs per replica", document.Topology.GPUsPerReplica)
	}
	if document.Topology.TensorParallel != 8 {
		t.Fatalf("the profile measured the tensor parallel width %d", document.Topology.TensorParallel)
	}
	if document.Topology.ExpertParallel != 1 {
		t.Fatalf("the profile measured the expert parallel width %d", document.Topology.ExpertParallel)
	}
	if len(document.Points) != 3 {
		t.Fatalf("the profile holds %d points, want 3", len(document.Points))
	}
}

// TestImportInferenceXRowsCountsTheReplica asserts the rate of one card
// becomes the rate of one replica.
//
// IT MATTERS BECAUSE THE CATALOG HOLDS THE CAPACITY OF ONE REPLICA AND NOT THE
// CAPACITY OF ONE CARD.
func TestImportInferenceXRowsCountsTheReplica(t *testing.T) {
	rows := []InferenceXRow{
		row("dsr1", "sglang", "mi325x", "fp8", 4, 8),
		row("dsr1", "sglang", "mi325x", "fp8", 8, 8),
	}
	documents, _ := ImportInferenceXRows(rows)
	point := documents[0].Points[0]
	if point.TotalTokensPerSecond != 4000 {
		t.Fatalf("the point sustained %v tokens per second, want 4000", point.TotalTokensPerSecond)
	}
	if point.InputTokensPerSecond != 3600 {
		t.Fatalf("the point sustained %v prompt tokens per second, want 3600", point.InputTokensPerSecond)
	}
	if point.OutputTokensPerSecond != 400 {
		t.Fatalf("the point generated %v tokens per second, want 400", point.OutputTokensPerSecond)
	}
}

// TestImportInferenceXRowsAlignsTheInterTokenLatency asserts a gap that spans
// more than one token becomes the time of one token.
//
// IT MATTERS BECAUSE A READER THAT TAKES THE GAP FOR A TOKEN LATENCY READS A
// LATENCY THAT IS SEVERAL TIMES TOO LARGE.
func TestImportInferenceXRowsAlignsTheInterTokenLatency(t *testing.T) {
	rows := []InferenceXRow{
		row("dsr1", "sglang", "mi325x", "fp8", 4, 8),
		row("dsr1", "sglang", "mi325x", "fp8", 8, 8),
	}
	rows[0].Metrics.MeanITL = 0.02
	rows[0].Metrics.MeanTPOT = 0.01
	documents, _ := ImportInferenceXRows(rows)
	itl := documents[0].Points[0].ITLMilliseconds
	if itl.P50 == nil {
		t.Fatal("the point holds no inter-token latency")
	}
	if *itl.P50 != 5 {
		t.Fatalf("the inter-token latency is %v milliseconds, want 5", *itl.P50)
	}
}

// TestImportInferenceXRowsPrefersTheLongSweep asserts the sweep of the
// lengths the catalog prefers wins.
//
// IT MATTERS BECAUSE A PROFILE HOLDS ONE MEASUREMENT AND A SWEEP OF ANOTHER
// LENGTH DOES NOT DESCRIBE THE SAME REQUEST.
func TestImportInferenceXRowsPrefersTheLongSweep(t *testing.T) {
	shortA := row("dsr1", "sglang", "mi325x", "fp8", 4, 8)
	shortA.InputLength = 1024
	shortA.OutputLength = 1024
	shortA.Date = "2026-07-01"
	shortB := shortA
	shortB.Concurrency = 8
	longA := row("dsr1", "sglang", "mi325x", "fp8", 16, 8)
	longA.Date = "2026-06-01"
	longB := longA
	longB.Concurrency = 32

	documents, _ := ImportInferenceXRows([]InferenceXRow{shortA, shortB, longA, longB})
	if len(documents) != 1 {
		t.Fatalf("the import produced %d profiles, want 1", len(documents))
	}
	if len(documents[0].Points) != 2 {
		t.Fatalf("the profile holds %d points, want 2", len(documents[0].Points))
	}
	if documents[0].Points[0].Concurrency != 16 {
		t.Fatalf("the profile measured the concurrency %d, want 16", documents[0].Points[0].Concurrency)
	}
}

// TestImportInferenceXRowsRefusesTheRowsItCannotUse asserts a row that
// measured a shape the catalog cannot hold is skipped with a reason.
//
// IT MATTERS BECAUSE A READER HAS TO BE ABLE TO TELL A SOURCE THAT MEASURED
// NOTHING FROM A SOURCE THAT WAS NEVER READ.
func TestImportInferenceXRowsRefusesTheRowsItCannotUse(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*InferenceXRow)
		reason string
	}{
		{name: "an engine the catalog does not name", mutate: func(r *InferenceXRow) { r.Framework = "trt" }, reason: "engine the catalog does not name"},
		{name: "a speculative recipe", mutate: func(r *InferenceXRow) { r.SpecMethod = "mtp" }, reason: "speculative decoding recipe"},
		{name: "more than one node", mutate: func(r *InferenceXRow) { r.IsMultinode = true }, reason: "more than one node"},
		{name: "a disaggregated deployment", mutate: func(r *InferenceXRow) { r.Disagg = true }, reason: "disaggregated deployment"},
		{name: "an accelerator the catalog cannot name", mutate: func(r *InferenceXRow) { r.Hardware = "rtx6000pro" }, reason: "accelerator the catalog cannot name"},
		{name: "a replica the catalog does not hold", mutate: func(r *InferenceXRow) { r.NumPrefillGPU = 64 }, reason: "replica the catalog does not hold"},
		{name: "no throughput", mutate: func(r *InferenceXRow) { r.Metrics.ThroughputPerGPU = 0 }, reason: "no throughput"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rows := []InferenceXRow{
				row("dsr1", "sglang", "mi325x", "fp8", 4, 8),
				row("dsr1", "sglang", "mi325x", "fp8", 8, 8),
			}
			testCase.mutate(&rows[0])
			documents, summary := ImportInferenceXRows(rows)
			if summary.Profiles != 0 {
				t.Fatalf("the import produced %d profiles, want 0", summary.Profiles)
			}
			if len(documents) != 0 {
				t.Fatalf("the import produced %d documents, want 0", len(documents))
			}
			if !skippedFor(summary.Skipped, testCase.reason) {
				t.Fatalf("the import reported %v, want one row skipped for %q", summary.Skipped, testCase.reason)
			}
		})
	}
}

// TestImportInferenceXRowsRefusesASweepOfOneLevel asserts a sweep that
// measured one load level produces no profile.
//
// IT MATTERS BECAUSE A PROFILE WITH ONE LOAD LEVEL CARRIES NO CAPACITY BOUNDARY
// AND CANNOT CARRY A CAPACITY.
func TestImportInferenceXRowsRefusesASweepOfOneLevel(t *testing.T) {
	rows := []InferenceXRow{row("dsr1", "sglang", "mi325x", "fp8", 4, 8)}
	documents, summary := ImportInferenceXRows(rows)
	if summary.Profiles != 0 {
		t.Fatalf("the import produced %d profiles, want 0", summary.Profiles)
	}
	if len(documents) != 0 {
		t.Fatalf("the import produced %d documents, want 0", len(documents))
	}
}

// TestImportInferenceXRowsReadsTheModelOfTheSource asserts the verified
// repository of the source wins over the short name.
//
// IT MATTERS BECAUSE THE SOURCE CHECKED THE REPOSITORY AND THE OPERATOR DID
// NOT, and the catalog has to name the model the deployment names.
func TestImportInferenceXRowsReadsTheModelOfTheSource(t *testing.T) {
	rows := []InferenceXRow{
		row("dsr1", "sglang", "mi325x", "fp8", 4, 8),
		row("dsr1", "sglang", "mi325x", "fp8", 8, 8),
	}
	rows[0].CanonicalModel = ""
	rows[1].CanonicalModel = ""
	documents, _ := ImportInferenceXRows(rows)
	if documents[0].Model != "deepseek-ai/DeepSeek-R1-0528" {
		t.Fatalf("the profile measures the model %q", documents[0].Model)
	}
}

// TestImportedProfilesAreEligible asserts a profile the import produced can
// carry a capacity.
//
// IT MATTERS BECAUSE A PROFILE THAT CANNOT CARRY A CAPACITY IS A PROFILE THAT
// THE OPERATOR SHIPS FOR NOTHING.
func TestImportedProfilesAreEligible(t *testing.T) {
	rows := []InferenceXRow{
		row("dsr1", "sglang", "mi325x", "fp8", 4, 8),
		row("dsr1", "sglang", "mi325x", "fp8", 8, 8),
	}
	documents, _ := ImportInferenceXRows(rows)
	catalog := priors.CatalogDocument{SchemaVersion: priors.SchemaVersion, Profiles: documents}
	encoded, err := priors.MarshalCatalogDocument(catalog)
	if err != nil {
		t.Fatalf("write the catalog: %v", err)
	}
	parsed, err := priors.ParseCatalog(encoded)
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	if len(parsed.Profiles) != 1 {
		t.Fatalf("the catalog holds %d profiles, want 1", len(parsed.Profiles))
	}
	if refusal := priors.CheckEligibility(parsed.Profiles[0]); refusal != nil {
		t.Fatalf("the imported profile cannot carry a capacity: %v", refusal)
	}
}

// skippedFor says whether a reason was reported for the rows.
//
// IT MATCHES A PART OF THE REASON, because the reason names the row and the
// case names the shape of the row.
func skippedFor(skipped map[string]int, reason string) bool {
	for reported, count := range skipped {
		if count > 0 && strings.Contains(reported, reason) {
			return true
		}
	}
	return false
}
