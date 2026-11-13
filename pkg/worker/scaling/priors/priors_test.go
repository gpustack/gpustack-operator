package priors

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// testIdentity returns an identity a profile and a request can agree on.
func testIdentity() Identity {
	return Identity{
		Model: ModelIdentity{
			Name:         "example/model",
			Revision:     "rev-1",
			Tokenizer:    "example/tokenizer",
			Quantization: "mxfp4",
		},
		Engine: EngineIdentity{
			Image: "registry.example/engine:1.2.3",
			Build: "1.2.3",
		},
		Hardware: HardwareIdentity{
			Accelerator: "Example Accelerator",
			MemoryGB:    270,
			FormFactor:  "SXM6",
		},
	}
}

// testServingConfig returns a serving configuration a profile and a request
// can agree on.
func testServingConfig() ServingConfig {
	return ServingConfig{
		Router:              "none",
		KVCacheDType:        "fp8",
		Offload:             "none",
		PrefixCaching:       "disabled",
		SpeculativeDecoding: "none",
		LaunchParameters:    []string{"--max-batch-size=8", "--tp=1"},
		EngineEnvComplete:   true,
	}
}

// testTopology returns a topology a profile and a request can agree on.
func testTopology() Topology {
	return Topology{
		GPUsPerReplica:    1,
		TensorParallel:    1,
		PipelineParallel:  1,
		DataParallel:      1,
		LocalDataParallel: 1,
		ContextParallel:   1,
		ExpertParallel:    1,
	}
}

// testWorkload returns a workload a profile can carry.
//
// THE ARRIVAL MODEL MATCHES THE FIXED CONCURRENCY POINTS testPoint BUILDS,
// because a profile whose points disagree with the workload's declared
// arrival model is refused.
func testWorkload() Workload {
	return Workload{
		ID:       "example-fixed-sequence-performance",
		Revision: "v6.1",
		Modality: "text",
		InputLengths: LengthDistribution{
			Min:   intPointer(1),
			Max:   intPointer(15330),
			Mean:  floatPointer(2000),
			Count: 4395,
		},
		OutputLengths: LengthDistribution{
			Min:   intPointer(1),
			Max:   intPointer(10240),
			Mean:  floatPointer(500),
			Count: 4395,
		},
		CacheBehavior: CacheBehaviorAllMisses,
		ArrivalModel:  ClientModelFixedConcurrency,
	}
}

// testLatency returns one latency statistic.
func testLatency(metric LatencyMetric, quantile, value float64) Latency {
	return Latency{
		Metric:      metric,
		Quantile:    quantile,
		Value:       value,
		Unit:        UnitMilliseconds,
		SampleCount: 4395,
		Censoring:   "none",
	}
}

// testPoint returns one measured load point.
func testPoint(concurrency, throughput, ttftP95, itlP95 float64) MeasuredPoint {
	return MeasuredPoint{
		Load: LoadLevel{
			Kind:        LoadKindConcurrency,
			Value:       concurrency,
			ClientModel: ClientModelFixedConcurrency,
			Unit:        "requests",
		},
		Throughput: Throughput{
			Metric:               ThroughputMetricCacheMissedTotalTokensPerSecond,
			Value:                throughput,
			Unit:                 UnitTokensPerSecond,
			Denominator:          DenominatorPerReplica,
			TokenCountConvention: TokenCountConventionCacheMissedInputPlusOutput,
		},
		Latencies: []Latency{
			testLatency(LatencyMetricTTFT, 0.95, ttftP95),
			testLatency(LatencyMetricITL, 0.95, itlP95),
		},
		Requests: RequestCounts{Successful: 4395},
		Duration: MeasurementDuration{MeasuredSeconds: 600},
	}
}

// testProfile returns a profile that carries the given points.
//
// IT COMPUTES THE DIGEST, because a profile the validator accepts has to carry
// one.
func testProfile(t *testing.T, role RoleKind, points []MeasuredPoint) Profile {
	t.Helper()

	profile := Profile{
		SourceID:         "source-1",
		Role:             role,
		Identity:         testIdentity(),
		ServingConfig:    testServingConfig(),
		Topology:         testTopology(),
		MeasuredReplicas: 1,
		Workload:         testWorkload(),
		Points:           points,
	}

	return redigest(t, profile)
}

// redigest recomputes the digest of a changed profile.
//
// IT IS NEEDED BECAUSE A PROFILE THAT CHANGED ITS CONTENT NO LONGER HAS THE
// DIGEST ITS CONTENT DERIVES.
func redigest(t *testing.T, profile Profile) Profile {
	t.Helper()

	digest, err := ProfileDigest(profile)
	if err != nil {
		t.Fatalf("compute profile digest: %v", err)
	}
	profile.Digest = digest

	return profile
}

// reseal recomputes the digest of a changed catalog.
//
// IT IS NEEDED BECAUSE A CATALOG THAT CHANGED ITS CONTENT NO LONGER HAS THE
// DIGEST ITS CONTENT DERIVES.
func reseal(t *testing.T, catalog *Catalog) *Catalog {
	t.Helper()

	digest, err := CatalogDigest(*catalog)
	if err != nil {
		t.Fatalf("compute catalog digest: %v", err)
	}
	catalog.Digest = digest

	return catalog
}

// testCatalog returns a catalog that carries one profile.
func testCatalog(t *testing.T, profiles ...Profile) *Catalog {
	t.Helper()

	catalog := Catalog{
		SchemaVersion: SchemaVersion,
		Sources: []Source{
			{
				ID:             "source-1",
				Name:           "test source",
				URL:            "https://example.invalid/results",
				Revision:       "rev-1",
				CaptureDate:    "2026-10-26",
				License:        "Apache-2.0",
				AdapterVersion: "test",
				Outcome:        SourceOutcomeAccepted,
			},
		},
		Profiles: profiles,
	}

	digest, err := CatalogDigest(catalog)
	if err != nil {
		t.Fatalf("compute catalog digest: %v", err)
	}
	catalog.Digest = digest

	return &catalog
}

// testRequest returns a request that matches the test profile.
func testRequest() Request {
	return Request{
		Role:          RoleKindServer,
		Identity:      testIdentity(),
		ServingConfig: testServingConfig(),
		Topology:      testTopology(),
		Constraints: Constraints{
			TTFTTargetMilliseconds: 3000,
			ITLTargetMilliseconds:  80,
		},
	}
}

func TestRoleKindIsTheAlgorithmRoleKind(t *testing.T) {
	// THE CATALOG AND THE ALGORITHMS MUST AGREE ON THE ROLE NAMES, because a
	// second set of names would let a profile and a deployment drift apart.
	role := algorithm.RoleKindServer
	if role != RoleKindServer {
		t.Fatalf("role %q is not the algorithm role %q", role, RoleKindServer)
	}
}

// testSource returns an accepted source a catalog can carry.
func testSource() Source {
	return Source{
		ID:             "source-1",
		Name:           "test source",
		URL:            "https://example.invalid/results",
		Revision:       "rev-1",
		CaptureDate:    "2026-10-26",
		License:        "Apache-2.0",
		AdapterVersion: "test",
		Outcome:        SourceOutcomeAccepted,
	}
}

// intPointer returns a pointer to an int.
func intPointer(value int) *int { return &value }

// floatPointer returns a pointer to a float64.
func floatPointer(value float64) *float64 { return &value }

func TestIsProfileNotFound(t *testing.T) {
	prefillPoints := []MeasuredPoint{
		testPoint(4, 100, 1000, 20),
		testPoint(8, 200, 4000, 20),
	}
	for i := range prefillPoints {
		prefillPoints[i].Throughput.Metric = ThroughputMetricCacheMissedInputTokensPerSecond
		prefillPoints[i].Throughput.TokenCountConvention = TokenCountConventionCacheMissedInput
	}
	base := testProfile(t, RoleKindPrefill, prefillPoints)
	catalog := testCatalog(t, base)

	var nilRefusal *RefusalError
	missingReq := testRequest()
	missingReq.Role = RoleKindDecode
	_, zeroMatchResolveErr := catalog.Resolve(missingReq)
	_, zeroMatchSelectErr := catalog.Select(missingReq.Shape())

	serverPoints := []MeasuredPoint{
		testPoint(4, 100, 1000, 20),
		testPoint(8, 200, 4000, 20),
	}
	ambig1 := testProfile(t, RoleKindServer, serverPoints)
	ambig2 := ambig1
	ambig2.Workload.Revision = "rev-2"
	ambig2 = redigest(t, ambig2)
	ambigCatalog := testCatalog(t, ambig1, ambig2)
	_, ambigErr := ambigCatalog.Resolve(testRequest())

	mismatchReq := testRequest()
	mismatchReq.Role = RoleKindPrefill
	mismatchReq.Identity.Model.Revision = "rev-other"
	_, mismatchErr := catalog.Resolve(mismatchReq)

	sloReq := testRequest()
	sloReq.Role = RoleKindPrefill
	sloReq.Constraints.TTFTTargetMilliseconds = 1
	_, sloErr := catalog.Resolve(sloReq)

	var nilCatalog *Catalog
	_, nilCatalogErr := nilCatalog.Resolve(testRequest())

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "typed nil refusal", err: nilRefusal, want: false},
		{name: "non-refusal error", err: errors.New("boom"), want: false},
		{name: "resolve zero match", err: zeroMatchResolveErr, want: true},
		{name: "select zero match", err: zeroMatchSelectErr, want: true},
		{name: "wrapped zero match", err: fmt.Errorf("probe prefill: %w", zeroMatchResolveErr), want: true},
		{name: "ambiguous match", err: ambigErr, want: false},
		{name: "identity mismatch", err: mismatchErr, want: false},
		{name: "slo infeasible", err: sloErr, want: false},
		{name: "nil catalog", err: nilCatalogErr, want: false},
		{name: "insufficient load coverage", err: newRefusal(RefusalInsufficientLoadCoverage, "missing coverage", nil), want: false},
		{name: "incompatible metric", err: newRefusal(RefusalIncompatibleMetric, "wrong metric", nil), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProfileNotFound(tc.err); got != tc.want {
				t.Fatalf("IsProfileNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDenominatorReconstructionAndSingleHeadroom(t *testing.T) {
	cases := []struct {
		name        string
		denominator Denominator
		throughput  float64
		wantSeed    float64
		wantEff     float64
	}{
		{
			name:        "PerGPU multiplies by GPUsPerReplica once",
			denominator: DenominatorPerGPU,
			throughput:  100,
			wantSeed:    400,
			wantEff:     340,
		},
		{
			name:        "PerReplica preserves per-replica throughput",
			denominator: DenominatorPerReplica,
			throughput:  400,
			wantSeed:    400,
			wantEff:     340,
		},
		{
			name:        "WholeSystem divides by MeasuredReplicas once",
			denominator: DenominatorWholeSystem,
			throughput:  800,
			wantSeed:    400,
			wantEff:     340,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			points := []MeasuredPoint{
				testPoint(4, tc.throughput, 1000, 20),
				testPoint(8, tc.throughput*1.5, 4000, 20),
			}
			for i := range points {
				points[i].Throughput.Denominator = tc.denominator
			}
			profile := testProfile(t, RoleKindServer, points)
			profile.Topology.GPUsPerReplica = 4
			profile.MeasuredReplicas = 2
			profile = redigest(t, profile)
			catalog := testCatalog(t, profile)

			req := testRequest()
			req.Topology.GPUsPerReplica = 4
			req.Constraints.Headroom = 0.85

			res, err := catalog.Resolve(req)
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			if math.Abs(res.SeedTokensPerSecond-tc.wantSeed) > 1e-9 {
				t.Fatalf("SeedTokensPerSecond = %v, want %v", res.SeedTokensPerSecond, tc.wantSeed)
			}
			if math.Abs(res.EffectiveTokensPerSecond-tc.wantEff) > 1e-9 {
				t.Fatalf("EffectiveTokensPerSecond = %v, want %v", res.EffectiveTokensPerSecond, tc.wantEff)
			}
		})
	}
}
