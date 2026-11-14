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
	p50, ok := itl["p50"]
	if !ok {
		t.Fatal("the point holds no inter-token latency")
	}
	if p50 != 5 {
		t.Fatalf("the inter-token latency is %v milliseconds, want 5", p50)
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
		{name: "an engine the catalog does not name", mutate: func(r *InferenceXRow) { r.Framework = "unknown-engine" }, reason: "engine the catalog does not name"},
		{name: "a speculative recipe", mutate: func(r *InferenceXRow) { r.SpecMethod = "mtp" }, reason: "speculative decoding recipe"},
		{name: "an accelerator the catalog cannot name", mutate: func(r *InferenceXRow) { r.Hardware = "rtx6000pro" }, reason: "accelerator the catalog cannot name"},
		{name: "non-divisible worker GPUs", mutate: func(r *InferenceXRow) { r.NumPrefillGPU = 8; r.PrefillNumWorkers = 3 }, reason: "replica the catalog does not hold"},
		{name: "non-conserving throughput", mutate: func(r *InferenceXRow) { r.Metrics.ThroughputPerGPU = 600 }, reason: "violated throughput conservation"},
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
	catalog := priors.NewCatalogDocument(documents...)
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

func TestImportInferenceXRowsImportsLargeAggregatedAndDisaggregated(t *testing.T) {
	// Multi-node aggregated (>8 GPUs per replica) with DP attention.
	agg1 := row("dsr1", "sglang", "b200", "fp4", 32, 16)
	agg1.IsMultinode = true
	agg1.PrefillTensorParallel = 16
	agg1.PrefillExpertParallel = 16
	agg1.PrefillDPAttention = true
	agg1.PrefillNumWorkers = 1
	agg2 := agg1
	agg2.Concurrency = 64

	// Disaggregated wide-EP (dynamo-sglang -> sglang, 4P / 2D workers, gcd=2 -> 2P / 1D).
	disagg1 := InferenceXRow{
		Model:                 "minimaxm2.5",
		CanonicalModel:        "",
		Framework:             "dynamo-sglang",
		Hardware:              "b200",
		Precision:             "fp4",
		Concurrency:           16,
		InputLength:           8192,
		OutputLength:          1024,
		Date:                  "2026-08-01",
		Disagg:                true,
		IsMultinode:           true,
		NumPrefillGPU:         16,
		PrefillNumWorkers:     4,
		PrefillTensorParallel: 4,
		PrefillExpertParallel: 4,
		NumDecodeGPU:          16,
		DecodeNumWorkers:      2,
		DecodeTensorParallel:  8,
		DecodeExpertParallel:  8,
		Metrics: InferenceXMetrics{
			ThroughputPerGPU:       100,
			InputThroughputPerGPU:  160,
			OutputThroughputPerGPU: 40,
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
	disagg2 := disagg1
	disagg2.Concurrency = 32
	disagg2.Metrics.ThroughputPerGPU = 150
	disagg2.Metrics.InputThroughputPerGPU = 240
	disagg2.Metrics.OutputThroughputPerGPU = 60

	documents, summary := ImportInferenceXRows([]InferenceXRow{agg1, agg2, disagg1, disagg2})
	if summary.Profiles != 2 || len(documents) != 2 {
		t.Fatalf("got %d profiles (%d docs), want 2; skipped=%v", summary.Profiles, len(documents), summary.Skipped)
	}

	var aggDoc, disaggDoc *priors.ProfileDocument
	for i := range documents {
		switch documents[i].Mode {
		case string(priors.ModeAggregated):
			aggDoc = &documents[i]
		case string(priors.ModeDisaggregated):
			disaggDoc = &documents[i]
		}
	}
	if aggDoc == nil || disaggDoc == nil {
		t.Fatalf("missing aggregated (%v) or disaggregated (%v) doc", aggDoc, disaggDoc)
	}
	if aggDoc.Topology.GPUsPerReplica != 16 || aggDoc.Topology.ExpertParallel != 16 {
		t.Fatalf("unexpected aggregated topology: %+v", aggDoc.Topology)
	}
	if disaggDoc.Model != "MiniMaxAI/MiniMax-M2.5" || disaggDoc.Engine != "sglang" {
		t.Fatalf("unexpected disaggregated identity: model=%q engine=%q", disaggDoc.Model, disaggDoc.Engine)
	}
	if disaggDoc.Prefill == nil || disaggDoc.Decode == nil {
		t.Fatalf("missing prefill/decode stages: %+v", disaggDoc)
	}
	if disaggDoc.Prefill.Replicas != 2 || disaggDoc.Prefill.Topology.GPUsPerReplica != 4 {
		t.Fatalf("unexpected prefill stage after gcd reduction: %+v", disaggDoc.Prefill)
	}
	if disaggDoc.Decode.Replicas != 1 || disaggDoc.Decode.Topology.GPUsPerReplica != 8 {
		t.Fatalf("unexpected decode stage after gcd reduction: %+v", disaggDoc.Decode)
	}
	// Unit GPUs = (16 + 16) / gcd(4, 2) = 16 -> Total = 100 * 16 = 1600.
	if disaggDoc.Points[0].TotalTokensPerSecond != 1600 {
		t.Fatalf("PerUnit throughput = %v, want 1600", disaggDoc.Points[0].TotalTokensPerSecond)
	}

	catalog := priors.NewCatalogDocument(documents...)
	encoded, err := priors.MarshalCatalogDocument(catalog)
	if err != nil {
		t.Fatalf("MarshalCatalogDocument: %v", err)
	}
	parsed, err := priors.ParseCatalog(encoded)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	for _, p := range parsed.Profiles {
		if refusal := priors.CheckEligibility(p); refusal != nil {
			t.Fatalf("CheckEligibility(%s) failed: %v", p.Shape(), refusal)
		}
	}
}

func TestImportInferenceXRowsSeparatesSweepCohorts(t *testing.T) {
	// Same date/isl/osl but different framework or worker count or dp_attention must stay distinct sweeps.
	base := InferenceXRow{
		Model: "dsr1", CanonicalModel: "deepseek-ai/DeepSeek-R1-0528",
		Framework: "sglang", Hardware: "h200", Precision: "fp8",
		Concurrency: 8, InputLength: 8192, OutputLength: 1024, Date: "2026-08-01",
		Disagg: true, NumPrefillGPU: 8, PrefillNumWorkers: 1, PrefillTensorParallel: 8,
		NumDecodeGPU: 8, DecodeNumWorkers: 1, DecodeTensorParallel: 8,
		Metrics: InferenceXMetrics{
			ThroughputPerGPU: 200, InputThroughputPerGPU: 360, OutputThroughputPerGPU: 40,
			MeanITL: 0.01, MeanTPOT: 0.01, P90TTFT: 0.2, P99TTFT: 0.3, P90ITL: 0.012, P99ITL: 0.015,
		},
	}
	p2 := base
	p2.Concurrency = 16
	dpOn1 := base
	dpOn1.DecodeDPAttention = true
	dpOn2 := dpOn1
	dpOn2.Concurrency = 16

	documents, summary := ImportInferenceXRows([]InferenceXRow{base, p2, dpOn1, dpOn2})
	if summary.Profiles != 1 || len(documents) != 1 || len(documents[0].Points) != 2 {
		t.Fatalf("expected 1 profile with 2 points from winning sweep cohort, got %d profiles, %v", summary.Profiles, documents)
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
