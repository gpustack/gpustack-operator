// Package adapter reads measured sweeps and turns them into catalog profiles.
//
// IT READS WHAT A PUBLIC BENCHMARK PUBLISHED and writes what the catalog
// holds, so the catalog is built from measurements and not from assertions.
package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// chunkBundlingRatio is the ratio between the mean inter-token latency and the
// mean time per output token above which the measured gaps cover more than one
// token.
//
// IT EXISTS BECAUSE A BENCHMARK THAT REPORTS THE GAP BETWEEN CHUNKS reports a
// gap that spans several tokens, and a reader that takes the gap for a token
// latency reads a latency that is several times too large.
const chunkBundlingRatio = 1.05

// maxGPUsPerReplica is the widest replica the catalog holds.
//
// IT EXISTS BECAUSE A REPLICA THAT SPANS MORE GPUs THAN THE MEASUREMENT RUNS ON
// IS A SHAPE THE CLUSTER MAY NOT HAVE, and a profile of a shape no deployment
// can have is a profile no deployment can use.
const maxGPUsPerReplica = 8

// InferenceXRow is one measured row of a public benchmark sweep.
//
// IT NAMES THE FIELDS THE CATALOG USES, because a row carries more than the
// catalog holds and a reader has to say which part it reads.
type InferenceXRow struct {
	// Model is the benchmark's internal name of the model.
	Model string `json:"model"`

	// QueryModel is the name the benchmark was queried with.
	QueryModel string `json:"query_model"`

	// CanonicalModel is the verified repository of the model.
	CanonicalModel string `json:"canonical_model"`

	// Framework is the serving engine the row ran on.
	Framework string `json:"framework"`

	// Hardware is the accelerator the row ran on.
	Hardware string `json:"hardware"`

	// Precision is the quantization recipe the row ran under.
	Precision string `json:"precision"`

	// SpecMethod is the speculative decoding method the row ran under.
	SpecMethod string `json:"spec_method"`

	// IsMultinode says whether the row ran on more than one node.
	IsMultinode bool `json:"is_multinode"`

	// Disagg says whether the row ran a disaggregated deployment.
	Disagg bool `json:"disagg"`

	// Concurrency is how many requests ran at the same time.
	Concurrency int `json:"conc"`

	// InputLength is the prompt length of the sweep.
	InputLength int `json:"isl"`

	// OutputLength is the generated length of the sweep.
	OutputLength int `json:"osl"`

	// Date is the day the sweep ran on.
	Date string `json:"date"`

	// PrefillTensorParallel is the tensor parallel width of the sweep.
	PrefillTensorParallel int `json:"prefill_tp"`

	// PrefillExpertParallel is the expert parallel width of the sweep.
	PrefillExpertParallel int `json:"prefill_ep"`

	// NumPrefillGPU is how many GPUs the sweep ran on.
	NumPrefillGPU int `json:"num_prefill_gpu"`

	// Metrics holds the measured numbers of the row.
	Metrics InferenceXMetrics `json:"metrics"`
}

// InferenceXMetrics holds the measured numbers of one row.
//
// IT NAMES THE FIELDS THE CATALOG USES, and every field is a number the source
// published or a zero it did not.
type InferenceXMetrics struct {
	// ThroughputPerGPU is the prompt and generated token rate of one GPU.
	ThroughputPerGPU float64 `json:"tput_per_gpu"`

	// InputThroughputPerGPU is the prompt token rate of one GPU.
	InputThroughputPerGPU float64 `json:"input_tput_per_gpu"`

	// OutputThroughputPerGPU is the generated token rate of one GPU.
	OutputThroughputPerGPU float64 `json:"output_tput_per_gpu"`

	// MeanITL is the mean gap between the chunks the benchmark reported.
	MeanITL float64 `json:"mean_itl"`

	// MeanTPOT is the mean time per output token.
	MeanTPOT float64 `json:"mean_tpot"`

	// MedianTTFT is the median time to first token, in seconds.
	MedianTTFT float64 `json:"median_ttft"`

	// P90TTFT is the ninetieth percentile of the time to first token, in seconds.
	P90TTFT float64 `json:"p90_ttft"`

	// P99TTFT is the ninety-ninth percentile of the time to first token, in seconds.
	P99TTFT float64 `json:"p99_ttft"`

	// MedianITL is the median gap between the chunks, in seconds.
	MedianITL float64 `json:"median_itl"`

	// P90ITL is the ninetieth percentile of the gap between the chunks, in seconds.
	P90ITL float64 `json:"p90_itl"`

	// P99ITL is the ninety-ninth percentile of the gap between the chunks, in seconds.
	P99ITL float64 `json:"p99_itl"`

	// MedianTPOT is the median time per output token, in seconds.
	MedianTPOT float64 `json:"median_tpot"`

	// P90TPOT is the ninetieth percentile of the time per output token, in seconds.
	P90TPOT float64 `json:"p90_tpot"`

	// P99TPOT is the ninety-ninth percentile of the time per output token, in seconds.
	P99TPOT float64 `json:"p99_tpot"`
}

// LoadInferenceXRows reads the measured rows of a public benchmark.
//
// IT READS A JSON ARRAY OF ROWS, because that is what the benchmark publishes.
func LoadInferenceXRows(path string) ([]InferenceXRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var rows []InferenceXRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return rows, nil
}

// Summary says what an import produced and what it left out.
//
// IT COUNTS THE ROWS THAT PRODUCED NO MEASUREMENT BY REASON, because a reader
// has to be able to tell a source that measured nothing from a source that was
// never read.
type Summary struct {
	// Rows is how many rows were read.
	Rows int

	// Profiles is how many profiles the import produced.
	Profiles int

	// Skipped counts the rows that produced no measurement, by reason.
	Skipped map[string]int
}

// ImportInferenceXRows turns measured rows into catalog profiles.
//
// IT GROUPS THE ROWS BY THE SHAPE THEY MEASURED, because one shape measured
// twice is one profile and not two.
//
// IT PICKS ONE SWEEP PER SHAPE, because a profile holds one measurement and a
// sweep of another workload length does not describe this one.
//
// IT SKIPS A ROW THAT MEASURED A SHAPE THE CATALOG CANNOT PLACE, because a
// measurement of an accelerator the catalog cannot name is a measurement the
// catalog cannot use.
func ImportInferenceXRows(rows []InferenceXRow) ([]priors.ProfileDocument, Summary) {
	summary := Summary{Rows: len(rows), Skipped: map[string]int{}}
	groups := map[string][]InferenceXRow{}
	for _, row := range rows {
		if reason, refused := row.skipReason(); refused {
			summary.Skipped[reason]++
			continue
		}
		key := row.groupKey()
		groups[key] = append(groups[key], row)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	documents := make([]priors.ProfileDocument, 0, len(keys))
	for _, key := range keys {
		document, selected, reason := selectSweep(groups[key])
		if !selected {
			summary.Skipped[reason]++
			continue
		}
		documents = append(documents, document)
	}
	summary.Profiles = len(documents)
	return documents, summary
}

// skipReason says why a row measured a shape the catalog cannot hold.
//
// IT REFUSES THE SHAPES THAT ARE NOT ONE REPLICA OF ONE ENGINE: a
// disaggregated deployment runs the prefill and the decode apart, a
// multinode run spans more than one node, a speculative run measures a
// different recipe, and an engine the catalog does not name is not one the
// catalog can match.
func (r InferenceXRow) skipReason() (string, bool) {
	if r.Framework != "vllm" && r.Framework != "sglang" {
		return "the row ran an engine the catalog does not name", true
	}
	if r.SpecMethod != "" && r.SpecMethod != "none" {
		return "the row ran a speculative decoding recipe", true
	}
	if r.IsMultinode {
		return "the row ran on more than one node", true
	}
	if r.Disagg {
		return "the row ran a disaggregated deployment", true
	}
	if nodefeature.ResolveAcceleratorFamily(r.Hardware) == "" {
		return "the row ran an accelerator the catalog cannot name", true
	}
	if gpus := r.gpusPerReplica(); gpus <= 0 || gpus > maxGPUsPerReplica {
		return "the row ran a replica the catalog does not hold", true
	}
	if r.Metrics.ThroughputPerGPU <= 0 {
		return "the row published no throughput", true
	}
	return "", false
}

// gpusPerReplica returns how many GPUs the row's replica used.
//
// IT FALLS BACK TO THE PARALLEL WIDTHS, because a source that does not count
// the GPUs still states the widths the replica ran with.
func (r InferenceXRow) gpusPerReplica() int {
	if r.NumPrefillGPU > 0 {
		return r.NumPrefillGPU
	}
	return r.tensorParallel() * r.expertParallel()
}

// tensorParallel returns the tensor parallel width of the row.
//
// IT IS ONE WHEN THE SOURCE STATES NONE, because a width of one is how a
// deployment states that it does not split the model, and the catalog has to
// speak the width the deployment speaks.
func (r InferenceXRow) tensorParallel() int {
	if r.PrefillTensorParallel <= 0 {
		return 1
	}
	return r.PrefillTensorParallel
}

// expertParallel returns the expert parallel width of the row.
//
// IT IS ONE WHEN THE SOURCE STATES NONE, because a width of one is how a
// deployment states that it does not split the experts.
func (r InferenceXRow) expertParallel() int {
	if r.PrefillExpertParallel <= 0 {
		return 1
	}
	return r.PrefillExpertParallel
}

// groupKey names the deployment shape the row measured.
func (r InferenceXRow) groupKey() string {
	return strings.Join([]string{
		r.modelName(),
		priors.CanonicalQuantization(r.Precision),
		r.Framework,
		nodefeature.ResolveAcceleratorFamily(r.Hardware),
		fmt.Sprintf("tp%d", r.tensorParallel()),
		fmt.Sprintf("ep%d", r.expertParallel()),
		fmt.Sprintf("gpu%d", r.gpusPerReplica()),
	}, "|")
}

// modelName returns the canonical name of the model the row measured.
//
// IT PREFERS THE VERIFIED REPOSITORY THE SOURCE PUBLISHED, because the source
// checked it and the operator did not. IT FALLS BACK TO THE MAPPING OF THE
// INTERNAL NAME, because a row that published no repository still names a model
// the operator knows.
func (r InferenceXRow) modelName() string {
	if canonical := strings.TrimSpace(r.CanonicalModel); canonical != "" {
		return canonical
	}
	if canonical := priors.CanonicalModelName(r.Model); canonical != "" && canonical != strings.TrimSpace(r.Model) {
		return canonical
	}
	return priors.CanonicalModelName(r.QueryModel)
}

// sweepKey names the sweep the row belongs to.
//
// IT NAMES THE DAY AND THE TWO LENGTHS, because a benchmark sweeps one length
// pair on one day and every other row of the same shape is a different
// measurement.
func (r InferenceXRow) sweepKey() string {
	return strings.Join([]string{r.Date, fmt.Sprintf("isl%d", r.InputLength), fmt.Sprintf("osl%d", r.OutputLength)}, "|")
}

// selectSweep picks the sweep that describes the shape and returns the profile
// it produced.
//
// IT PREFERS THE SWEEP WITH THE LONGEST PROMPT AND THE SHORTEST ANSWER, then
// the one with the shortest of both, then the one that measured the most load
// levels, then the one that ran last. The order is fixed, so the same rows
// always produce the same profile.
func selectSweep(rows []InferenceXRow) (priors.ProfileDocument, bool, string) {
	sweeps := map[string][]InferenceXRow{}
	for _, row := range rows {
		key := row.sweepKey()
		sweeps[key] = append(sweeps[key], row)
	}
	keys := make([]string, 0, len(sweeps))
	for key := range sweeps {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	best := ""
	bestPoints := map[int]InferenceXRow{}
	bestRank := sweepRank{}
	for _, key := range keys {
		points := pointsByConcurrency(sweeps[key])
		if len(points) < 2 {
			continue
		}
		rank := sweepRank{lengths: lengthPreference(sweeps[key][0]), levels: len(points), date: sweeps[key][0].Date}
		if best == "" || rank.betterThan(bestRank) {
			best, bestPoints, bestRank = key, points, rank
		}
	}
	if best == "" {
		return priors.ProfileDocument{}, false, "the shape measured fewer than two load levels"
	}
	return buildProfile(rows[0], bestPoints), true, ""
}

// sweepRank orders the sweeps of one shape.
type sweepRank struct {
	// lengths says how close the sweep is to the lengths the catalog prefers.
	lengths int

	// levels is how many load levels the sweep measured.
	levels int

	// date is the day the sweep ran on.
	date string
}

// betterThan says whether a sweep is a better measurement than another.
//
// IT PREFERS THE LENGTHS FIRST, because a profile that measured the length a
// deployment sends is worth more than one that measured many load levels of
// another length.
func (r sweepRank) betterThan(other sweepRank) bool {
	if r.lengths != other.lengths {
		return r.lengths < other.lengths
	}
	if r.levels != other.levels {
		return r.levels > other.levels
	}
	return r.date > other.date
}

// lengthPreference says how close a sweep is to the lengths the catalog
// prefers.
//
// IT PREFERS A PROMPT OF EIGHT THOUSAND TOKENS AND AN ANSWER OF ONE THOUSAND,
// because that is the length pair a served request has.
func lengthPreference(row InferenceXRow) int {
	switch {
	case row.InputLength == 8192 && row.OutputLength == 1024:
		return 0
	case row.InputLength == 1024 && row.OutputLength == 1024:
		return 1
	default:
		return 2
	}
}

// pointsByConcurrency returns the row of every load level of a sweep.
//
// IT KEEPS THE FIRST ROW OF A LEVEL, because a sweep that published a level
// twice is one measurement and not two, and the first is the one the file
// holds.
func pointsByConcurrency(rows []InferenceXRow) map[int]InferenceXRow {
	points := make(map[int]InferenceXRow, len(rows))
	for _, row := range rows {
		if row.Concurrency <= 0 {
			continue
		}
		if _, seen := points[row.Concurrency]; seen {
			continue
		}
		points[row.Concurrency] = row
	}
	return points
}

// buildProfile turns the measured load levels of one sweep into a profile.
func buildProfile(row InferenceXRow, points map[int]InferenceXRow) priors.ProfileDocument {
	levels := make([]int, 0, len(points))
	for level := range points {
		levels = append(levels, level)
	}
	sort.Ints(levels)

	document := priors.ProfileDocument{
		Role:         string(priors.RoleKindServer),
		Model:        row.modelName(),
		Quantization: priors.CanonicalQuantization(row.Precision),
		Engine:       row.Framework,
		Hardware:     nodefeature.ResolveAcceleratorFamily(row.Hardware),
		Topology: priors.TopologyDocument{
			TensorParallel: row.tensorParallel(),
			ExpertParallel: row.expertParallel(),
			GPUsPerReplica: row.gpusPerReplica(),
		},
	}
	for _, level := range levels {
		document.Points = append(document.Points, buildPoint(points[level]))
	}
	return document
}

// buildPoint turns one measured row into a measured load point.
//
// IT MULTIPLIES THE RATE OF ONE GPU BY THE WIDTH OF THE REPLICA, because the
// catalog holds the capacity of one replica and not the capacity of one card.
func buildPoint(row InferenceXRow) priors.PointDocument {
	gpus := float64(row.gpusPerReplica())
	ratio := interTokenRatio(row.Metrics)
	point := priors.PointDocument{
		Concurrency:           row.Concurrency,
		TotalTokensPerSecond:  round(row.Metrics.ThroughputPerGPU*gpus, 2),
		InputTokensPerSecond:  round(row.Metrics.InputThroughputPerGPU*gpus, 2),
		OutputTokensPerSecond: round(row.Metrics.OutputThroughputPerGPU*gpus, 2),
		TTFTMilliseconds: priors.LatencyQuantiles{
			P50: seconds(row.Metrics.MedianTTFT),
			P90: seconds(row.Metrics.P90TTFT),
			P99: seconds(row.Metrics.P99TTFT),
		},
	}
	point.ITLMilliseconds = interTokenLatency(row.Metrics, ratio)
	return point
}

// interTokenRatio returns the number of tokens the measured gap covered.
//
// IT IS ONE UNLESS THE MEASURED GAP IS LONGER THAN THE TIME PER TOKEN BY MORE
// THAN THE TOLERANCE, because a gap that is a little longer is a measurement
// and a gap that spans tokens is a different quantity.
func interTokenRatio(metrics InferenceXMetrics) float64 {
	if metrics.MeanTPOT <= 0 || metrics.MeanITL <= 0 {
		return 1
	}
	ratio := metrics.MeanITL / metrics.MeanTPOT
	if ratio <= chunkBundlingRatio {
		return 1
	}
	return ratio
}

// interTokenLatency returns the per-token latency readings of a row.
//
// IT FALLS BACK TO THE TIME PER OUTPUT TOKEN, because a source that published
// no gap published the per-token latency under another name.
func interTokenLatency(metrics InferenceXMetrics, ratio float64) priors.LatencyQuantiles {
	if metrics.MedianITL > 0 {
		return priors.LatencyQuantiles{
			P50: seconds(metrics.MedianITL / ratio),
			P90: seconds(metrics.P90ITL / ratio),
			P99: seconds(metrics.P99ITL / ratio),
		}
	}
	return priors.LatencyQuantiles{
		P50: seconds(metrics.MedianTPOT),
		P90: seconds(metrics.P90TPOT),
		P99: seconds(metrics.P99TPOT),
	}
}

// seconds returns a duration in seconds as milliseconds.
//
// IT RETURNS NOTHING FOR A DURATION THE SOURCE DID NOT PUBLISH, because a zero
// latency is a measurement and a missing one is not.
func seconds(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	milliseconds := round(value*1000, 4)
	return &milliseconds
}

// round rounds a number to the given number of decimal places.
//
// IT EXISTS SO THE CATALOG READS THE SAME WHATEVER THE MEASUREMENT PUBLISHED,
// because a file that changes its last digit on every run is a file no one can
// diff.
func round(value float64, places int) float64 {
	factor := 1.0
	for range places {
		factor *= 10
	}
	return float64(int64(value*factor+0.5)) / factor
}
