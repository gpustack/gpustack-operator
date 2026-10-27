package worker

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
	"gpustack.ai/gpustack/pkg/worker/scaling/sim"
)

// This deliberately limited passive estimator is evaluated only in tests.
// WindowSeconds is only the shortest window. Counts, exact window alignment and
// request lengths are unavailable, so acceptance here does not identify capacity.
type scalerExperimentEstimator struct {
	Seed        float64
	last        time.Time
	ready       int32
	consecutive int
}

// scalerExperimentPrior measures the base plant before evaluating other shapes.
// Its synthetic profile stays in test memory and never enters a production catalog.
func scalerExperimentPrior(t *testing.T) priors.Resolution {
	t.Helper()
	base := scalerSimulationFixture(t).Scenarios[1]
	base.DurationSeconds, base.Fleet.Initial = 240, 1
	base.Traffic.Phases = []sim.Phase{{UntilSeconds: 180, InputTokens: 100, OutputTokens: 100}, {UntilSeconds: 240}}
	points := make([]priors.MeasuredPoint, 0, 3)
	for _, rate := range []float64{.5, 2, 3} {
		base.Traffic.Phases[0].RequestsPerSecond = rate
		result := runScalerSimulation(t, base, scalerSimulationArm{Shadow: true})
		ttft, itl, throughput, windows, count := 0.0, 0.0, 0.0, 0, 0
		for _, point := range result.Trace {
			if point.Seconds <= 180 && point.ProcessedTPS != nil {
				throughput += *point.ProcessedTPS
				windows++
			}
		}
		for _, request := range result.Requests {
			if request.TTFTSeconds >= 0 {
				ttft, itl = math.Max(ttft, request.TTFTSeconds), math.Max(itl, request.ITLSeconds)
				count++
			}
		}
		if windows == 0 || count == 0 {
			t.Fatal("empty calibration measurements")
		}
		point := priors.MeasuredPoint{
			Load:       priors.LoadLevel{Kind: priors.LoadKindRequestRate, Value: rate, ClientModel: priors.ClientModelOpenLoop, Unit: priors.UnitQueriesPerSecond},
			Throughput: priors.Throughput{Metric: priors.ThroughputMetricCacheMissedTotalTokensPerSecond, Value: throughput / float64(windows), Unit: priors.UnitTokensPerSecond, Denominator: priors.DenominatorPerReplica, TokenCountConvention: priors.TokenCountConventionCacheMissedInputPlusOutput},
			Latencies: []priors.Latency{
				{Metric: priors.LatencyMetricTTFT, Quantile: 1, Value: ttft * 1000, Unit: priors.UnitMilliseconds, SampleCount: count, Censoring: "observed first tokens including failed requests"},
				{Metric: priors.LatencyMetricITL, Quantile: 1, Value: itl * 1000, Unit: priors.UnitMilliseconds, SampleCount: count, Censoring: "maximum of per-request maximum token gaps"},
			},
			Requests: priors.RequestCounts{Successful: result.Summary.Completed, Failed: result.Summary.Failed},
			Duration: priors.MeasurementDuration{MeasuredSeconds: 180},
		}
		points = append(points, point)
		t.Logf("CALIBRATION rate=%g processed_tps=%g max_ttft_ms=%g max_itl_ms=%g failed=%d", rate, point.Throughput.Value, ttft*1000, itl*1000, result.Summary.Failed)
	}
	length, mean := 100, 100.0
	profile := priors.Profile{
		SourceID: "synthetic-base-plant", Role: priors.RoleKindServer, MeasuredReplicas: 1,
		Identity: priors.Identity{
			Model:    priors.ModelIdentity{Name: "synthetic", Revision: "baseline-v1", Tokenizer: "synthetic", Quantization: "none"},
			Engine:   priors.EngineIdentity{Image: "synthetic-vllm-exposition", Build: "baseline-v1"},
			Hardware: priors.HardwareIdentity{Accelerator: "synthetic", MemoryGB: 1, FormFactor: "synthetic"},
		},
		ServingConfig: priors.ServingConfig{Router: "least-active", KVCacheDType: "synthetic", Offload: "none", PrefixCaching: "disabled", SpeculativeDecoding: "none", LaunchParameters: []string{"batch=4"}, EngineEnvComplete: true},
		Topology:      priors.Topology{GPUsPerReplica: 1, TensorParallel: 1, PipelineParallel: 1, DataParallel: 1, LocalDataParallel: 1, ContextParallel: 1, ExpertParallel: 1},
		Workload:      priors.Workload{ID: "synthetic-100-100", Revision: "baseline-v1", Modality: "text", InputLengths: priors.LengthDistribution{Min: &length, Max: &length, Mean: &mean, Count: 990}, OutputLengths: priors.LengthDistribution{Min: &length, Max: &length, Mean: &mean, Count: 990}, CacheBehavior: priors.CacheBehaviorAllMisses, ArrivalModel: priors.ClientModelOpenLoop},
		Points:        points,
	}
	var err error
	profile.Digest, err = priors.ProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	catalog := priors.Catalog{SchemaVersion: priors.SchemaVersion, Profiles: []priors.Profile{profile}, Sources: []priors.Source{{ID: profile.SourceID, Name: "synthetic local experiment", URL: "https://example.invalid/synthetic", Revision: "baseline-v1", CaptureDate: "2026-10-26", License: "Apache-2.0", AdapterVersion: "test-v1", Outcome: priors.SourceOutcomeAccepted}}}
	catalog.Digest, err = priors.CatalogDigest(catalog)
	if err != nil {
		t.Fatal(err)
	}
	// THE PROFILE IS SELECTED BY THE SHAPE IT MEASURED, because a profile is
	// known by its shape and not by a name.
	resolution, err := catalog.Resolve(priors.Request{Role: profile.Role, Identity: profile.Identity, ServingConfig: profile.ServingConfig, Topology: profile.Topology, Constraints: priors.Constraints{TTFTTargetMilliseconds: 2000, ITLTargetMilliseconds: 100, LatencyQuantile: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RESOLUTION %+v", resolution)
	return resolution
}

type scalerExperimentMeasurement struct {
	Scenario                                                  string
	Arm                                                       string
	SeedFactor                                                float64
	InitialCapacity                                           float64
	Summary                                                   sim.Summary
	SLOFraction                                               float64
	RequestedObjectReplicaSeconds                             float64
	PositiveReadyExcessVersusManualSeconds                    float64
	Changes, Retired, Aborted                                 int
	Response                                                  scalerSimulationResponse
	Decisions                                                 map[string]int
	AcceptedMinimum, AcceptedMaximum                          float64
	CostChangeFraction, SLOChangeFraction                     float64
	StaticSeedCostChangeFraction, StaticSeedSLOChangeFraction float64
	WithinPairedTolerances                                    bool
}

func logScalerExperiment(t *testing.T, scenario sim.Scenario, arm string, factor, capacity float64, result, reference, staticSeed scalerSimulationResult, tolerances sim.Tolerances) scalerExperimentMeasurement {
	t.Helper()
	slo := func(r scalerSimulationResult) float64 {
		if r.Summary.Offered == 0 {
			return 1
		}
		return float64(r.Summary.SLOCompliant) / float64(r.Summary.Offered)
	}
	m := scalerExperimentMeasurement{Scenario: scenario.Name, Arm: arm, SeedFactor: factor, InitialCapacity: capacity, Summary: result.Summary, SLOFraction: slo(result), RequestedObjectReplicaSeconds: result.RequestedObjectReplicaSeconds, Changes: result.Changes, Retired: result.Retired, Aborted: result.Aborted, Response: result.Response, Decisions: map[string]int{}}
	for i, point := range result.Trace {
		if point.CapacityDecision != "" {
			m.Decisions[point.CapacityDecision]++
		}
		if point.CapacityDecision == "accepted" && i+1 < len(result.Trace) {
			value := result.Trace[i+1].EffectiveCapacity
			if m.AcceptedMinimum == 0 || value < m.AcceptedMinimum {
				m.AcceptedMinimum = value
			}
			m.AcceptedMaximum = math.Max(m.AcceptedMaximum, value)
		}
		if i+1 < len(result.Trace) {
			m.PositiveReadyExcessVersusManualSeconds += float64(max(0, point.Ready-reference.Trace[i].Ready)) * (result.Trace[i+1].Seconds - point.Seconds)
		}
	}
	m.CostChangeFraction = result.RequestedObjectReplicaSeconds/reference.RequestedObjectReplicaSeconds - 1
	m.SLOChangeFraction = m.SLOFraction - slo(reference)
	m.StaticSeedCostChangeFraction = result.RequestedObjectReplicaSeconds/staticSeed.RequestedObjectReplicaSeconds - 1
	m.StaticSeedSLOChangeFraction = m.SLOFraction - slo(staticSeed)
	m.WithinPairedTolerances = m.StaticSeedCostChangeFraction <= tolerances.MaximumCostIncreaseFraction && m.StaticSeedSLOChangeFraction >= -tolerances.MaximumSLODecreaseFraction
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("EXPERIMENT %s", encoded)
	return m
}

func TestModelDeploymentScalerSimulationExperimentEvaluation(t *testing.T) {
	fixture := scalerSimulationFixture(t)
	data, err := os.ReadFile("../../scaling/sim/testdata/baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if stringx.SumBytesBySHA256(data) != "7366a9ffbc6170646940196ce18e3a8b8958460eb862343a47aab08a3aaaf6ca" {
		t.Fatal("frozen experiment partition changed")
	}
	resolution := scalerExperimentPrior(t)
	for _, scenario := range fixture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			manual := runScalerSimulation(t, scenario, scalerSimulationArm{})
			assertScalerSimulationSafety(t, scenario, scalerSimulationArm{}, manual)
			logScalerExperiment(t, scenario, "manual", 1, 300, manual, manual, manual, fixture.Tolerances)
			factors := []float64{1}
			if scenario.Holdout {
				factors = fixture.CapacityFactors
			}
			for _, factor := range factors {
				staticSeed := manual
				for _, passive := range []bool{false, true} {
					name := "resolved"
					capacity := resolution.EffectiveTokensPerSecond * factor
					arm := scalerSimulationArm{CapacityFactor: capacity / 300}
					if passive {
						name = "passive"
						estimator := &scalerExperimentEstimator{Seed: capacity}
						arm.AfterSample = estimator.Observe
					}
					t.Run(fmt.Sprintf("%s/%g", name, factor), func(t *testing.T) {
						result := runScalerSimulation(t, scenario, arm)
						assertScalerSimulationSafety(t, scenario, arm, result)
						if scenario.Holdout {
							last := result.Trace[len(result.Trace)-1]
							if last.Committed != 1 || last.Ready != 1 || last.Pending || last.Retirement != "" || result.Summary.Outstanding != 0 {
								t.Fatalf("incomplete cooldown: %+v", last)
							}
						}
						if !passive {
							staticSeed = result
						}
						measurement := logScalerExperiment(t, scenario, name, factor, capacity, result, manual, staticSeed, fixture.Tolerances)
						if passive && scenario.Name == "steady_noise" && measurement.Decisions["accepted"] == 0 {
							t.Fatal("steady scenario did not exercise the candidate")
						}
					})
				}
			}
		})
	}
}

func TestModelDeploymentScalerSimulationExperimentIdentifiability(t *testing.T) {
	observations := make([][]algorithm.RoleObservation, 0, 2)
	for _, capacity := range []float64{400, 800} {
		scenario := scalerSimulationFixture(t).Scenarios[1]
		scenario.DurationSeconds, scenario.Fleet.Initial = 120, 1
		scenario.Plant.TokensPerSecond = capacity
		scenario.Traffic.Phases = []sim.Phase{{UntilSeconds: 120, RequestsPerSecond: .2, InputTokens: .5, OutputTokens: 2}}
		estimator := scalerExperimentEstimator{Seed: 300}
		var samples []algorithm.RoleObservation
		result := runScalerSimulation(t, scenario, scalerSimulationArm{Shadow: true, AfterSample: func(sample scalerSimulationSample) scalerSimulationCapacityDecision {
			decision := estimator.Observe(sample)
			samples = append(samples, sample.Sample.Observation)
			return decision
		}})
		observations = append(observations, samples)
		last := samples[len(samples)-1]
		encoded, err := json.Marshal(last)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("UNDERLOAD true_capacity=%g observed_tps=%g observation=%s decision=%s", capacity, *result.Trace[len(result.Trace)-1].ProcessedTPS, encoded, result.Trace[len(result.Trace)-1].CapacityDecision)
	}
	if !slices.EqualFunc(observations[0], observations[1], roleObservationsEqual) {
		t.Fatal("chosen underload counterexample no longer has identical observations")
	}
}

// roleObservationsEqual compares two observations of the same role.
// ObservedAt is compared as an instant. The remaining fields are compared by
// value on normalized copies, so fields added later stay covered.
func roleObservationsEqual(a, b algorithm.RoleObservation) bool {
	if !a.ObservedAt.Equal(b.ObservedAt) {
		return false
	}
	a.ObservedAt = time.Time{}
	b.ObservedAt = time.Time{}
	return a == b
}

func TestModelDeploymentScalerSimulationExperimentCoordinatedOmission(t *testing.T) {
	results := make([]scalerSimulationResult, 0, 2)
	for _, concurrency := range []int{0, 2} {
		scenario := scalerSimulationFixture(t).Scenarios[2]
		scenario.Name = fmt.Sprintf("offered_8rps_concurrency_%d", concurrency)
		scenario.Fleet.Initial = 2
		scenario.Traffic.Concurrency = concurrency
		scenario.Traffic.Phases = []sim.Phase{{UntilSeconds: 300, RequestsPerSecond: 8, InputTokens: 100, OutputTokens: 100}, {UntilSeconds: 900}}
		result := runScalerSimulation(t, scenario, scalerSimulationArm{Shadow: true})
		results = append(results, result)
		logScalerExperiment(t, scenario, "fixed-two", 1, 300, result, result, result, scalerSimulationFixture(t).Tolerances)
	}
	if results[1].Summary.Offered >= results[0].Summary.Offered || results[1].Summary.P95LatencySeconds >= results[0].Summary.P95LatencySeconds || results[0].Summary.Failed == 0 {
		t.Fatal("closed-loop traffic did not expose omitted arrivals and apparent latency improvement")
	}
}

func (e *scalerExperimentEstimator) Observe(sample scalerSimulationSample) scalerSimulationCapacityDecision {
	obs := sample.Sample.Observation
	reason := ""
	switch {
	case obs.ObservedAt.IsZero() || sample.Now.Sub(obs.ObservedAt) > 20*time.Second || obs.ObservedAt.After(sample.Now):
		reason = "stale"
	case !e.last.IsZero() && !obs.ObservedAt.After(e.last):
		reason = "duplicate"
	case !e.last.IsZero() && obs.ObservedAt.Sub(e.last) > 60*time.Second:
		reason = "ttl"
	case !obs.CompleteCoverage:
		reason = "coverage"
	case sample.Pending || sample.Retiring || obs.ReadyReplicas != obs.CurrentReplicas || (e.ready != 0 && e.ready != obs.ReadyReplicas):
		reason = "transition"
	case obs.ThroughputState == algorithm.SignalIdle:
		reason = "idle"
	case obs.ThroughputState != algorithm.SignalFresh || obs.LatencyState != algorithm.SignalFresh || obs.GaugeState != algorithm.SignalFresh:
		reason = "missing"
	case sample.Sample.WindowSeconds != 10 || obs.ReadyReplicas <= 0 || obs.TTFTMilliseconds <= 0 || obs.ITLMilliseconds <= 0:
		reason = "window"
	}
	if obs.ObservedAt.After(e.last) {
		e.last, e.ready = obs.ObservedAt, obs.ReadyReplicas
	}
	capacity := e.Seed
	if reason == "" {
		capacity = (obs.PromptTokensPerSecond + obs.GenerationTokensPerSecond) / float64(obs.ReadyReplicas) * math.Sqrt(math.Min(2000/obs.TTFTMilliseconds, 100/obs.ITLMilliseconds))
		if math.IsNaN(capacity) || math.IsInf(capacity, 0) || capacity < .25*e.Seed || capacity > 4*e.Seed {
			reason = "outlier"
		} else {
			e.consecutive++
			if e.consecutive < 3 {
				reason = "warming"
			} else {
				reason = "accepted"
			}
		}
	}
	if reason != "accepted" {
		capacity = e.Seed
		if reason != "warming" {
			e.consecutive = 0
		}
	}
	return scalerSimulationCapacityDecision{Capacity: &capacity, Reason: reason}
}

func TestModelDeploymentScalerSimulationExperimentSeam(t *testing.T) {
	scenario := scalerSimulationFixture(t).Scenarios[1]
	scenario.DurationSeconds = 30
	seen := 0
	result := runScalerSimulation(t, scenario, scalerSimulationArm{
		Shadow: true,
		Configure: func(_ *workercore.ModelDeployment, scaler *workercore.ModelDeploymentScaler) {
			scaler.Spec.Algorithm.TokenThroughput.ServerTargetTokensPerSecond = quantityPointer("450")
		},
		AfterSample: func(sample scalerSimulationSample) scalerSimulationCapacityDecision {
			seen++
			if sample.Sample.Observation.Role != "server" || sample.Now.IsZero() {
				t.Fatalf("missing real observation: %+v", sample)
			}
			if sample.Sample.Observation.LatencyState == algorithm.SignalFresh && sample.Sample.Observation.ITLMilliseconds <= 0 {
				t.Fatalf("active token generation lost its ITL histogram in the real sampler: %+v", sample)
			}
			capacity := 350.0
			return scalerSimulationCapacityDecision{Capacity: &capacity, Reason: "seam-check"}
		},
	})
	if seen != 4 || result.Trace[0].EffectiveCapacity != 450 || result.Trace[1].EffectiveCapacity != 350 || result.Trace[1].CapacityDecision != "seam-check" {
		t.Fatalf("callback or next-tick capacity missing: seen=%d trace=%+v", seen, result.Trace)
	}
}

func TestModelDeploymentScalerSimulationExperimentHistogramCounts(t *testing.T) {
	for _, tc := range []struct {
		output, firstTokens, gaps, rate, step float64
		steps                                 int
	}{
		{.5, 0, 0, 4, .125, 8},
		{1, 1, 0, 4, .125, 8},
		{1.5, 1, 0, 4, .125, 8},
		{2.5, 1, 1, 4, .125, 8},
		{3.5, 1, 2, 4, .125, 8},
		{2, 1, 1, .8, .25, 10},
	} {
		for _, engine := range []string{"vllm", "sglang"} {
			t.Run(fmt.Sprintf("%s/%g@%g", engine, tc.output, tc.rate), func(t *testing.T) {
				plant := sim.NewPlant(sim.PlantConfig{TokensPerSecond: tc.rate, BatchSize: 1, TimeoutSeconds: 10})
				plant.Offer(sim.Request{OutputTokens: tc.output})
				for tick := 0; tick < tc.steps; tick++ {
					plant.Step(float64(tick)*tc.step, tc.step, []int{0})
				}
				body, err := plant.Exposition(0, engine, 1)
				if err != nil {
					t.Fatal(err)
				}
				pod := scalerSamplerPod("engine", "server", "engine", 0)
				sample, err := modelDeploymentScalerParseSample(&pod, body, engine, time.Unix(1, 0))
				if err != nil {
					t.Fatal(err)
				}
				if math.Abs(sample.generationTokens-tc.output) > 1e-9 || sample.ttft.count != tc.firstTokens || sample.itl.count != tc.gaps {
					t.Fatalf("parsed generation=%g firstTokens=%g gaps=%g; want %+v", sample.generationTokens, sample.ttft.count, sample.itl.count, tc)
				}
				if tc.gaps > 0 {
					latency, ok := modelDeploymentScalerQuantileMilliseconds(sample.itl, .95)
					if !ok || latency <= 0 {
						t.Fatalf("parsed ITL=%g valid=%v", latency, ok)
					}
				}
			})
		}
	}
}

func TestModelDeploymentScalerSimulationExperimentReplay(t *testing.T) {
	estimator := scalerExperimentEstimator{Seed: 300}
	epoch := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		now, observed int
		want          string
	}{
		{10, 10, "warming"},
		{20, 20, "warming"},
		{30, 30, "accepted"},
		{40, 40, "accepted"},
		{50, 50, "accepted"},
		{60, 60, "accepted"},
		{60, 30, "stale"},
		{60, 40, "duplicate"},
		{60, 50, "duplicate"},
		{60, 60, "duplicate"},
		{70, 70, "warming"},
		{80, 80, "warming"},
		{90, 90, "accepted"},
	} {
		t.Run(fmt.Sprintf("now%d-observed%d", tc.now, tc.observed), func(t *testing.T) {
			previous := estimator.last
			decision := estimator.Observe(scalerSimulationSample{Now: epoch.Add(time.Duration(tc.now) * time.Second), Sample: modelDeploymentScalerRoleSample{WindowSeconds: 10, Observation: algorithm.RoleObservation{Role: "server", ReadyReplicas: 1, CurrentReplicas: 1, ObservedAt: epoch.Add(time.Duration(tc.observed) * time.Second), CompleteCoverage: true, ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh, GaugeState: algorithm.SignalFresh, PromptTokensPerSecond: 100, GenerationTokensPerSecond: 100, TTFTMilliseconds: 2000, ITLMilliseconds: 100}}})
			wantCapacity := 300.0
			if tc.want == "accepted" {
				wantCapacity = 200
			}
			if estimator.last.Before(previous) || decision.Reason != tc.want || decision.Capacity == nil || *decision.Capacity != wantCapacity {
				t.Fatalf("replay guard: last=%v previous=%v decision=%+v want=%s capacity=%g", estimator.last, previous, decision, tc.want, wantCapacity)
			}
		})
	}
}

func TestModelDeploymentScalerSimulationExperimentRejections(t *testing.T) {
	for _, tc := range []struct {
		name                                                              string
		state                                                             algorithm.SignalState
		partial, pending, retiring, duplicate, stale, transition, outlier bool
		want                                                              string
	}{
		{name: "idle", state: algorithm.SignalIdle, want: "idle"},
		{name: "missing", state: algorithm.SignalMissing, want: "missing"},
		{name: "partial", state: algorithm.SignalFresh, partial: true, want: "coverage"},
		{name: "pending", state: algorithm.SignalFresh, pending: true, want: "transition"},
		{name: "retirement", state: algorithm.SignalFresh, retiring: true, want: "transition"},
		{name: "duplicate", state: algorithm.SignalFresh, duplicate: true, want: "duplicate"},
		{name: "stale", state: algorithm.SignalFresh, stale: true, want: "stale"},
		{name: "membership", state: algorithm.SignalFresh, transition: true, want: "transition"},
		{name: "outlier", state: algorithm.SignalFresh, outlier: true, want: "outlier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			estimator := scalerExperimentEstimator{Seed: 300}
			epoch := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
			sample := scalerSimulationSample{Sample: modelDeploymentScalerRoleSample{WindowSeconds: 10, Observation: algorithm.RoleObservation{
				Role: "server", ReadyReplicas: 2, CurrentReplicas: 2, CompleteCoverage: true,
				PromptTokensPerSecond: 200, GenerationTokensPerSecond: 200, TTFTMilliseconds: 1000, ITLMilliseconds: 50,
				ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh, GaugeState: algorithm.SignalFresh,
			}}}
			for tick := 1; tick <= 4; tick++ {
				sample.Now = epoch.Add(time.Duration(tick*10) * time.Second)
				sample.Sample.Observation.ObservedAt = sample.Now
				estimator.Observe(sample)
			}
			sample.Now = epoch.Add(50 * time.Second)
			sample.Sample.Observation.ObservedAt = sample.Now
			sample.Sample.Observation.ThroughputState = tc.state
			sample.Sample.Observation.CompleteCoverage = !tc.partial
			sample.Pending, sample.Retiring = tc.pending, tc.retiring
			if tc.duplicate {
				sample.Sample.Observation.ObservedAt = epoch.Add(40 * time.Second)
			}
			if tc.stale {
				sample.Now = epoch.Add(200 * time.Second)
			}
			if tc.transition {
				sample.Sample.Observation.ReadyReplicas = 3
			}
			if tc.outlier {
				sample.Sample.Observation.PromptTokensPerSecond = 100000
			}
			decision := estimator.Observe(sample)
			if decision.Reason != tc.want || decision.Capacity == nil || *decision.Capacity != 300 {
				t.Fatalf("invalid sample did not restore seed: %+v", decision)
			}
		})
	}
}

func TestModelDeploymentScalerSimulationExperimentMinimumEvidence(t *testing.T) {
	estimator := scalerExperimentEstimator{Seed: 300}
	epoch := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		seconds int
		state   algorithm.SignalState
		want    string
	}{
		{10, algorithm.SignalFresh, "warming"},
		{20, algorithm.SignalFresh, "warming"},
		{30, algorithm.SignalFresh, "accepted"},
		{40, algorithm.SignalIdle, "idle"},
		{50, algorithm.SignalFresh, "warming"},
		{60, algorithm.SignalFresh, "warming"},
		{70, algorithm.SignalFresh, "accepted"},
		{140, algorithm.SignalFresh, "ttl"},
		{150, algorithm.SignalFresh, "warming"},
		{160, algorithm.SignalFresh, "warming"},
		{170, algorithm.SignalFresh, "accepted"},
	} {
		t.Run(fmt.Sprint(tc.seconds), func(t *testing.T) {
			now := epoch.Add(time.Duration(tc.seconds) * time.Second)
			decision := estimator.Observe(scalerSimulationSample{Now: now, Sample: modelDeploymentScalerRoleSample{WindowSeconds: 10, Observation: algorithm.RoleObservation{Role: "server", ReadyReplicas: 1, CurrentReplicas: 1, ObservedAt: now, CompleteCoverage: true, ThroughputState: tc.state, LatencyState: algorithm.SignalFresh, GaugeState: algorithm.SignalFresh, PromptTokensPerSecond: 100, GenerationTokensPerSecond: 100, TTFTMilliseconds: 2000, ITLMilliseconds: 100}}})
			wantCapacity := 300.0
			if tc.want == "accepted" {
				wantCapacity = 200
			}
			if decision.Reason != tc.want || decision.Capacity == nil || *decision.Capacity != wantCapacity {
				t.Fatalf("decision=%+v expected reason=%s capacity=%g", decision, tc.want, wantCapacity)
			}
		})
	}
}
