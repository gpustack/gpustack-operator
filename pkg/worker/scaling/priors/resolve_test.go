package priors

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// refusalOf returns the refusal an error carries.
//
// IT FAILS THE TEST WHEN THE ERROR IS NOT A REFUSAL, because a plain error
// would leave the caller without a reason it can record.
func refusalOf(t *testing.T, err error) *RefusalError {
	t.Helper()

	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a refusal", err)
	}
	return refusal
}

// serverPoint returns a server load point with the given values.
func serverPoint(concurrency, throughput, ttftP95, itlP95 float64) MeasuredPoint {
	return testPoint(concurrency, throughput, ttftP95, itlP95)
}

// ratePoint returns an open loop load point offered at a request rate.
func ratePoint(qps, throughput, ttftP95, itlP95 float64) MeasuredPoint {
	point := testPoint(qps, throughput, ttftP95, itlP95)
	point.Load = LoadLevel{
		Kind:        LoadKindRequestRate,
		Value:       qps,
		ClientModel: ClientModelOpenLoop,
		Unit:        "queries/second",
	}
	return point
}

// medianOnlyPoint returns a load point whose latencies are medians.
//
// IT IS A DIAGNOSTIC, because a median cannot carry the required quantile,
// and a diagnostic cannot carry a capacity either.
func medianOnlyPoint(concurrency, throughput float64) MeasuredPoint {
	point := serverPoint(concurrency, throughput, 500, 20)
	point.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.50, 500),
		testLatency(LatencyMetricITL, 0.50, 20),
	}
	return point
}

// quantilePoint returns a server load point whose latencies are measured at
// one quantile.
//
// IT EXISTS BECAUSE A QUANTILE BELOW THE DEFAULT IS EVIDENCE TOO, and a
// profile that measured at one quantile has to be expressible in a test.
func quantilePoint(concurrency, throughput, quantile, ttft, itl float64) MeasuredPoint {
	point := serverPoint(concurrency, throughput, ttft, itl)
	point.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, quantile, ttft),
		testLatency(LatencyMetricITL, quantile, itl),
	}
	return point
}

// TestResolveOutcomes covers the resolution outcomes.
//
// EACH CASE ASSERTS THE VERDICT, because the verdict is what protects a
// deployment from an invented capacity.
func TestResolveOutcomes(t *testing.T) {
	feasible := serverPoint(4, 1000, 500, 20)
	overLatency := serverPoint(8, 1200, 5000, 20)

	metricMismatch := serverPoint(4, 1000, 500, 20)
	metricMismatch.Throughput.Metric = ThroughputMetricOutputTokensPerSecond
	metricMismatch.Throughput.TokenCountConvention = TokenCountConventionOutput

	conventionMismatch := serverPoint(4, 1000, 500, 20)
	conventionMismatch.Throughput.TokenCountConvention = TokenCountConventionInputIncludingCacheHits

	higherQuantile := serverPoint(4, 1000, 500, 20)
	higherQuantile.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.99, 500),
		testLatency(LatencyMetricITL, 0.99, 20),
	}

	medianOnly := serverPoint(4, 1000, 500, 20)
	medianOnly.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.50, 500),
		testLatency(LatencyMetricITL, 0.50, 20),
	}

	tpotOnly := serverPoint(4, 1000, 500, 20)
	tpotOnly.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.95, 500),
		testLatency(LatencyMetricTPOT, 0.95, 20),
	}

	perGPU := serverPoint(4, 1000, 500, 20)
	perGPU.Throughput.Denominator = DenominatorPerGPU

	wholeSystem := serverPoint(4, 1000, 500, 20)
	wholeSystem.Throughput.Denominator = DenominatorWholeSystem

	cases := []struct {
		name    string
		profile func(t *testing.T) Profile
		request func(t *testing.T) Request
		want    string
		reason  RefusalReason
		seed    float64
	}{
		{
			name: "an exact match resolves the measured capacity",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "resolved",
			seed:    1000,
		},
		{
			name: "the default headroom is applied",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "resolved",
			seed:    1000,
		},
		{
			name: "a headroom above one is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Constraints.Headroom = 1.5
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different model revision is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Identity.Model.Revision = "rev-2"
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different engine image is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Identity.Engine.Image = "registry.example/engine:9.9.9"
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different quantization is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Identity.Model.Quantization = "fp8"
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different kv cache dtype is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.ServingConfig.KVCacheDType = "fp16"
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different launch parameter set is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.ServingConfig.LaunchParameters = []string{"--max-batch-size=16", "--tp=1"}
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different tensor parallel width is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Topology.TensorParallel = 2
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a different expert parallel width is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Topology.ExpertParallel = 2
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a missing latency target is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Constraints.ITLTargetMilliseconds = 0
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
		{
			name: "a numerator the role does not use is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{metricMismatch, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalIncompatibleMetric,
		},
		{
			name: "a token count convention that includes cache hits is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{conventionMismatch, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalIncompatibleMetric,
		},
		{
			name: "a workload with an unknown cache behavior is refused",
			profile: func(t *testing.T) Profile {
				profile := testProfile(t, RoleKindServer, []MeasuredPoint{feasible, overLatency})
				profile.Workload.CacheBehavior = CacheBehaviorUnknown
				return redigest(t, profile)
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalIncompatibleMetric,
		},
		{
			name: "a single measured load level is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalInsufficientLoadCoverage,
		},
		{
			name: "a curve without a boundary is refused",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{feasible, serverPoint(8, 1200, 500, 20)})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalInsufficientLoadCoverage,
		},
		{
			name: "a higher quantile of the same metric is used as a bound",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{higherQuantile, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "resolved",
			seed:    1000,
		},
		{
			name: "a median cannot stand in for the required quantile",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{medianOnly, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalInsufficientLoadCoverage,
		},
		{
			name: "time per output token cannot stand in for inter token latency",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindServer, []MeasuredPoint{tpotOnly, overLatency})
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "refused",
			reason:  RefusalInsufficientLoadCoverage,
		},
		{
			name: "a per gpu rate is reconstructed for one replica",
			profile: func(t *testing.T) Profile {
				profile := testProfile(t, RoleKindServer, []MeasuredPoint{perGPU, overLatency})
				profile.Topology.GPUsPerReplica = 4
				return redigest(t, profile)
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Topology.GPUsPerReplica = 4
				return request
			},
			want: "resolved",
			seed: 4000,
		},
		{
			name: "a whole system rate is divided by the measured replicas",
			profile: func(t *testing.T) Profile {
				profile := testProfile(t, RoleKindServer, []MeasuredPoint{wholeSystem, overLatency})
				profile.MeasuredReplicas = 4
				return redigest(t, profile)
			},
			request: func(t *testing.T) Request { return testRequest() },
			want:    "resolved",
			seed:    250,
		},
		{
			name: "the encode role needs no capacity prior",
			profile: func(t *testing.T) Profile {
				return testProfile(t, RoleKindEncode, []MeasuredPoint{feasible, overLatency})
			},
			request: func(t *testing.T) Request {
				request := testRequest()
				request.Role = RoleKindEncode
				return request
			},
			want:   "refused",
			reason: RefusalCapacityPriorUnresolved,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testCase.profile(t)
			catalog := testCatalog(t, profile)
			request := testCase.request(t)

			resolution, err := catalog.Resolve(request)
			if testCase.want == "refused" {
				if err == nil {
					t.Fatalf("expected a refusal, got the resolution %v", resolution)
				}
				refusal := refusalOf(t, err)
				if refusal.Reason != testCase.reason {
					t.Fatalf("refusal reason %q is not %q", refusal.Reason, testCase.reason)
				}
				if refusal.Message == "" {
					t.Fatal("the refusal carries no message")
				}
				return
			}

			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.SeedTokensPerSecond != testCase.seed {
				t.Fatalf("seed %v is not %v", resolution.SeedTokensPerSecond, testCase.seed)
			}
			if resolution.EffectiveTokensPerSecond != testCase.seed*DefaultHeadroom {
				t.Fatalf("effective %v is not %v", resolution.EffectiveTokensPerSecond, testCase.seed*DefaultHeadroom)
			}
			if resolution.Shape != profile.Shape().String() {
				t.Fatalf("resolution names shape %q, want %q", resolution.Shape, profile.Shape())
			}
			if resolution.Basis.Boundary != BoundaryHigherLoadFailure {
				t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryHigherLoadFailure)
			}
			if resolution.Basis.Headroom != DefaultHeadroom {
				t.Fatalf("basis headroom %v is not %v", resolution.Basis.Headroom, DefaultHeadroom)
			}
		})
	}
}

// TestResolveUsesTheConfiguredHeadroom asserts the headroom a request sets is
// the one that is applied.
func TestResolveUsesTheConfiguredHeadroom(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.Headroom = 0.5

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.EffectiveTokensPerSecond != 500 {
		t.Fatalf("effective %v is not 500", resolution.EffectiveTokensPerSecond)
	}
	if resolution.Basis.Headroom != 0.5 {
		t.Fatalf("basis headroom %v is not 0.5", resolution.Basis.Headroom)
	}
}

// TestResolveLabelsAConservativeBound asserts a higher quantile is used and
// labeled, because a bound taken from a higher quantile is not the requested
// quantile.
func TestResolveLabelsAConservativeBound(t *testing.T) {
	point := serverPoint(4, 1000, 500, 20)
	point.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.99, 500),
		testLatency(LatencyMetricITL, 0.99, 20),
	}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		point,
		serverPoint(8, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolution.Basis.Conservative {
		t.Fatal("the resolution does not label the conservative bound")
	}
	if resolution.Basis.Quantile != "0.99" {
		t.Fatalf("basis quantile %q is not %q", resolution.Basis.Quantile, "0.99")
	}
}

// TestResolveSelectsByShape asserts a shape names the profile it belongs to.
//
// IT MATTERS BECAUSE THE SHAPE IS WHAT A PROFILE IS KNOWN BY, and a deployment
// states its shape before it states anything else about the catalog.
func TestResolveSelectsByShape(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	selected, refusal := catalog.Select(profile.Shape())
	if refusal != nil {
		t.Fatalf("the shape was refused: %v", refusal)
	}
	if selected.Shape().String() != profile.Shape().String() {
		t.Fatalf("the selected profile has the shape %q, want %q", selected.Shape(), profile.Shape())
	}
	if selected.Digest != profile.Digest {
		t.Fatalf("the selected profile has the digest %q, want %q", selected.Digest, profile.Digest)
	}
}

// TestResolveRefusesAnUnknownShape asserts a shape that names nothing is
// refused.
func TestResolveRefusesAnUnknownShape(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	shape := profile.Shape()
	shape.Model = "example/other-model"

	_, refusal := catalog.Select(shape)
	if refusal == nil {
		t.Fatal("expected a refusal")
	}
	if refusal.Reason != RefusalCapacityPriorUnresolved {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalCapacityPriorUnresolved)
	}
	if refusal.Detail["shape"] != shape.String() {
		t.Fatalf("the refusal names the shape %q, want %q", refusal.Detail["shape"], shape.String())
	}
}

// TestResolveRefusesAnAmbiguousMatch asserts two profiles that match the same
// deployment are refused, because the resolver must not choose between them.
func TestResolveRefusesAnAmbiguousMatch(t *testing.T) {
	first := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	second := first
	second.Workload.Revision = "v6.1-compliance"
	second = redigest(t, second)

	catalog := testCatalog(t, first, second)

	_, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	refusal := refusalOf(t, err)
	if refusal.Reason != RefusalCapacityPriorUnresolved {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalCapacityPriorUnresolved)
	}
	if refusal.Detail["shapes"] == "" {
		t.Fatal("the refusal does not name the shapes that matched")
	}
}

// TestResolveRefusesAMissingCatalog asserts a resolver without a catalog
// refuses instead of returning a zero capacity.
func TestResolveRefusesAMissingCatalog(t *testing.T) {
	var catalog *Catalog

	_, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if reason := refusalOf(t, err).Reason; reason != RefusalCapacityPriorUnresolved {
		t.Fatalf("refusal reason %q is not %q", reason, RefusalCapacityPriorUnresolved)
	}
}

// TestResolveUsesAnExplicitSaturationAsTheBoundary asserts an explicit
// saturation measurement establishes the boundary when no higher load point
// failed.
func TestResolveUsesAnExplicitSaturationAsTheBoundary(t *testing.T) {
	saturated := serverPoint(8, 1200, 500, 20)
	saturated.Saturation = &Saturation{Measured: true, Note: "the queue grew without a throughput gain"}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		saturated,
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.Basis.Boundary != BoundaryExplicitSaturation {
		t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryExplicitSaturation)
	}
}

// TestResolveRefusesAProfileThatMeasuresTwice asserts a source that measured
// the same statistic twice with different values is refused, because a source
// that contradicts itself cannot decide a constraint.
func TestResolveRefusesAProfileThatMeasuresTwice(t *testing.T) {
	conflicting := serverPoint(4, 1000, 500, 20)
	conflicting.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.95, 500),
		testLatency(LatencyMetricTTFT, 0.95, 900),
		testLatency(LatencyMetricITL, 0.95, 20),
	}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		conflicting,
		serverPoint(8, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	_, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if reason := refusalOf(t, err).Reason; reason != RefusalIncompatibleMetric {
		t.Fatalf("refusal reason %q is not %q", reason, RefusalIncompatibleMetric)
	}
}

// TestCheckEligibilityReportsEveryGap asserts the eligibility check names
// every piece of evidence a profile is missing.
//
// IT MATTERS BECAUSE A CALLER NEEDS THE WHOLE LIST TO KNOW WHAT TO MEASURE
// NEXT, NOT ONLY THE FIRST GAP.
func TestCheckEligibilityReportsEveryGap(t *testing.T) {
	oneLoadLevel := serverPoint(4, 1000, 500, 20)

	wrongNumerator := serverPoint(4, 1000, 500, 20)
	wrongNumerator.Throughput.Metric = ThroughputMetricTotalTokensPerSecond
	wrongNumerator.Throughput.TokenCountConvention = TokenCountConventionInputPlusOutput

	noITL := serverPoint(4, 1000, 500, 20)
	noITL.Latencies = []Latency{testLatency(LatencyMetricTTFT, 0.95, 500)}

	cases := []struct {
		name   string
		points []MeasuredPoint
		reason RefusalReason
		detail string
	}{
		{
			name:   "a profile with one load level",
			points: []MeasuredPoint{oneLoadLevel},
			reason: RefusalInsufficientLoadCoverage,
			detail: "loadLevels",
		},
		{
			name:   "a profile that does not measure inter token latency",
			points: []MeasuredPoint{noITL},
			reason: RefusalIncompatibleMetric,
			detail: "missingMetric",
		},
		{
			name:   "a profile whose numerator is not the sampler numerator",
			points: []MeasuredPoint{wrongNumerator},
			reason: RefusalIncompatibleMetric,
			detail: "numerator",
		},
		{
			name:   "a profile whose cache behavior is not established",
			points: []MeasuredPoint{serverPoint(4, 1000, 500, 20)},
			reason: RefusalIncompatibleMetric,
			detail: "cacheBehavior",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testProfile(t, RoleKindServer, testCase.points)
			if testCase.detail == "cacheBehavior" {
				profile.Workload.CacheBehavior = CacheBehaviorUnknown
				profile = redigest(t, profile)
			}

			refusal := CheckEligibility(profile)
			if refusal == nil {
				t.Fatal("expected the profile to be refused")
			}
			if refusal.Reason != testCase.reason {
				t.Fatalf("the refusal reason %q is not %q", refusal.Reason, testCase.reason)
			}
			if refusal.Detail[testCase.detail] == "" {
				t.Fatalf("the refusal does not name %q: %v", testCase.detail, refusal.Detail)
			}
		})
	}
}

// TestCheckEligibilityAcceptsACompleteProfile asserts a profile that carries
// the evidence the resolver needs is accepted.
func TestCheckEligibilityAcceptsACompleteProfile(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})

	if refusal := CheckEligibility(profile); refusal != nil {
		t.Fatalf("the profile was refused: %v", refusal)
	}
}

// TestResolveRefusesAMixedLoadAxis asserts a profile whose points vary
// different load models is refused, because a concurrency count and a request
// rate are not comparable numbers and a boundary between them is invented.
func TestResolveRefusesAMixedLoadAxis(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		ratePoint(5, 1000, 500, 20),
		ratePoint(10, 1200, 5000, 20),
		serverPoint(64, 2000, 50000, 20),
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatalf("expected a refusal, got the resolution %v", resolution)
	}
	if reason := refusalOf(t, err).Reason; reason != RefusalIncompatibleMetric {
		t.Fatalf("refusal reason %q is not %q", reason, RefusalIncompatibleMetric)
	}
}

// TestResolveRefusesALoadModelTheWorkloadDoesNotDeclare asserts points whose
// client model differs from the workload's declared arrival model are
// refused, because the envelope the operator selected is not the envelope
// that was measured.
func TestResolveRefusesALoadModelTheWorkloadDoesNotDeclare(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		ratePoint(5, 1000, 500, 20),
		ratePoint(10, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatalf("expected a refusal, got the resolution %v", resolution)
	}
	if reason := refusalOf(t, err).Reason; reason != RefusalIncompatibleMetric {
		t.Fatalf("refusal reason %q is not %q", reason, RefusalIncompatibleMetric)
	}
}

// TestResolveUsesTwoQualifiedPointsAndIgnoresADiagnostic asserts a curve with
// two fully qualified points resolves while a median-only point stays in the
// profile as raw evidence without counting toward anything.
func TestResolveUsesTwoQualifiedPointsAndIgnoresADiagnostic(t *testing.T) {
	saturated := serverPoint(8, 1200, 500, 20)
	saturated.Saturation = &Saturation{Measured: true, Note: "the queue grew without a throughput gain"}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		saturated,
		medianOnlyPoint(16, 1400),
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.SeedTokensPerSecond != 1200 {
		t.Fatalf("seed %v is not 1200", resolution.SeedTokensPerSecond)
	}
	if resolution.Basis.FeasiblePoints != 2 {
		t.Fatalf("feasible points %d is not 2", resolution.Basis.FeasiblePoints)
	}
	if resolution.Basis.Boundary != BoundaryExplicitSaturation {
		t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryExplicitSaturation)
	}
}

// TestResolveRefusesOneQualifiedPointWithADiagnostic asserts a profile whose
// qualified evidence covers a single load level is refused even though a
// diagnostic point exists, because one qualified level is not a curve.
func TestResolveRefusesOneQualifiedPointWithADiagnostic(t *testing.T) {
	tpotOnly := serverPoint(8, 1200, 500, 20)
	tpotOnly.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.95, 500),
		testLatency(LatencyMetricTPOT, 0.95, 20),
	}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		tpotOnly,
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatalf("expected a refusal, got the resolution %v", resolution)
	}
	refusal := refusalOf(t, err)
	if refusal.Reason != RefusalInsufficientLoadCoverage {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalInsufficientLoadCoverage)
	}
	if refusal.Detail["loadLevels"] != "1" {
		t.Fatalf("the refusal does not name the qualified load levels: %v", refusal.Detail)
	}
}

// TestResolveRefusesABoundaryEstablishedByADiagnostic asserts a diagnostic
// point above the feasible boundary does not establish it, because a point
// that cannot be judged is neither feasible nor a failed load level.
func TestResolveRefusesABoundaryEstablishedByADiagnostic(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 500, 20),
		medianOnlyPoint(16, 1400),
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatalf("expected a refusal, got the resolution %v", resolution)
	}
	if reason := refusalOf(t, err).Reason; reason != RefusalInsufficientLoadCoverage {
		t.Fatalf("refusal reason %q is not %q", reason, RefusalInsufficientLoadCoverage)
	}
}

// TestResolveTreatsAnUnstatedWidthAsOne asserts a request that leaves a width
// out names the width of one.
//
// IT MATTERS BECAUSE A WIDTH A DEPLOYMENT DID NOT STATE IS THE WIDTH OF ONE, and
// a request that matched no profile would leave the deployment with no prior at
// all.
func TestResolveTreatsAnUnstatedWidthAsOne(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Topology.TensorParallel = 0

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.Shape != profile.Shape().String() {
		t.Fatalf("resolution names shape %q, want %q", resolution.Shape, profile.Shape())
	}
}

func TestResolveLatencyBoundsDescribeSelectedSeed(t *testing.T) {
	tests := []struct {
		name                                     string
		selectedTTFT, selectedITL, otherQuantile float64
	}{
		{name: "mixed seed metrics", selectedTTFT: .99, selectedITL: .95, otherQuantile: .999},
		{name: "other points cannot make seed conservative", selectedTTFT: .95, selectedITL: .95, otherQuantile: .99},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			points := []MeasuredPoint{testPoint(1, 1000, 500, 20), testPoint(2, 2000, 1000, 30), testPoint(3, 2100, 4000, 100)}
			for i := range points {
				for j := range points[i].Latencies {
					points[i].Latencies[j].Quantile = tt.otherQuantile
				}
			}
			points[1].Latencies[0].Quantile = tt.selectedTTFT
			points[1].Latencies[1].Quantile = tt.selectedITL
			// THE SEED POINT RECORDS AN EXPLICIT SATURATION, because the point
			// above it is measured at a higher quantile and a higher quantile
			// that misses the target cannot prove that the requested quantile
			// missed it.
			points[1].Saturation = &Saturation{Measured: true, Note: "the queue grew without a throughput gain"}
			profile := testProfile(t, RoleKindServer, points)
			resolution, err := testCatalog(t, profile).Resolve(testRequest())
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Basis.Boundary != BoundaryExplicitSaturation {
				t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryExplicitSaturation)
			}
			if len(resolution.Basis.LatencyBounds) != 2 {
				t.Fatalf("bounds = %#v", resolution.Basis.LatencyBounds)
			}
			expected := map[LatencyMetric]float64{LatencyMetricTTFT: tt.selectedTTFT, LatencyMetricITL: tt.selectedITL}
			for _, bound := range resolution.Basis.LatencyBounds {
				if bound.Quantile != expected[bound.Metric] || bound.Conservative != (bound.Quantile > DefaultLatencyQuantile) {
					t.Fatalf("seed evidence = %#v, expected %v", bound, expected)
				}
			}
			if resolution.SeedTokensPerSecond != 2000 {
				t.Fatalf("seed changed: %v", resolution.SeedTokensPerSecond)
			}
			quantile := max(tt.selectedTTFT, tt.selectedITL)
			if resolution.Basis.Quantile != FormatLatencyQuantile(quantile) || resolution.Basis.Conservative != (quantile > DefaultLatencyQuantile) {
				t.Fatalf("aggregate evidence does not describe the seed: %#v", resolution.Basis)
			}
		})
	}
}

// TestResolveDoesNotFailOnAQuantileAboveTheRequestedOne asserts a
// measurement at a quantile above the requested one decides nothing, however
// close the two are.
//
// IT MATTERS BECAUSE A QUANTILE ABOVE THE REQUESTED ONE BOUNDS IT FROM ABOVE
// AND CANNOT SAY WHETHER THE REQUESTED ONE MISSED THE TARGET, and a tolerance
// WOULD LET A QUANTILE THAT IS NOT THE REQUESTED ONE PROVE THE FAILURE.
func TestResolveDoesNotFailOnAQuantileAboveTheRequestedOne(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		quantilePoint(4, 1000, 0.95, 500, 20),
		quantilePoint(8, 1200, 0.9500000005, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.LatencyQuantile = 0.95

	_, err := catalog.Resolve(request)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	refusal := refusalOf(t, err)
	if refusal.Reason != RefusalInsufficientLoadCoverage {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalInsufficientLoadCoverage)
	}
}

// TestResolveAcceptsTwoQuantilesThatAreCloseButDifferent asserts two
// measurements at two quantiles that are close but different do not
// contradict each other.
//
// IT MATTERS BECAUSE A SOURCE THAT MEASURED TWO QUANTILES HAS NOT CONTRADICTED
// ITSELF, and a tolerance WOULD CALL TWO DIFFERENT QUANTILES THE SAME ONE.
func TestResolveAcceptsTwoQuantilesThatAreCloseButDifferent(t *testing.T) {
	seed := quantilePoint(4, 1000, 0.95, 500, 20)
	seed.Latencies = append(seed.Latencies, testLatency(LatencyMetricTTFT, 0.9500000005, 5000))

	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		seed,
		quantilePoint(8, 1200, 0.95, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	resolution, err := catalog.Resolve(testRequest())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.SeedTokensPerSecond != 1000 {
		t.Fatalf("seed %v is not 1000", resolution.SeedTokensPerSecond)
	}
	if resolution.Basis.Boundary != BoundaryHigherLoadFailure {
		t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryHigherLoadFailure)
	}
	for _, bound := range resolution.Basis.LatencyBounds {
		if bound.Metric != LatencyMetricTTFT {
			continue
		}
		if bound.Quantile != 0.95 {
			t.Fatalf("the bound quantile %v is not 0.95", bound.Quantile)
		}
	}
}

// TestResolveReportsTheQuantileFaithfully asserts the reported quantile keeps
// every digit the value carries.
//
// IT MATTERS BECAUSE A STATUS THAT WRITES 0.00001 AS 0.0000 REPORTS A
// QUANTILE NOBODY ASKED FOR.
func TestResolveReportsTheQuantileFaithfully(t *testing.T) {
	cases := []struct {
		name     string
		quantile float64
	}{
		{name: "the smallest quantile", quantile: 0.00001},
		{name: "a quantile with five digits", quantile: 0.97505},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testProfile(t, RoleKindServer, []MeasuredPoint{
				quantilePoint(4, 1000, testCase.quantile, 500, 20),
				quantilePoint(8, 1200, testCase.quantile, 5000, 20),
			})
			catalog := testCatalog(t, profile)

			request := testRequest()
			request.Constraints.LatencyQuantile = testCase.quantile

			resolution, err := catalog.Resolve(request)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.Basis.Quantile != FormatLatencyQuantile(testCase.quantile) {
				t.Fatalf("reported quantile %q is not %q",
					resolution.Basis.Quantile, FormatLatencyQuantile(testCase.quantile))
			}
			parsed, err := strconv.ParseFloat(resolution.Basis.Quantile, 64)
			if err != nil {
				t.Fatalf("parse the reported quantile: %v", err)
			}
			if parsed != testCase.quantile {
				t.Fatalf("the reported quantile reads back as %v, want %v", parsed, testCase.quantile)
			}
		})
	}
}

// TestResolveUsesTheRequestedQuantile asserts a request is held to the
// quantile it names.
//
// IT MATTERS BECAUSE THE QUANTILE IS THE CALLER'S CHOICE, and a resolver that
// answered every request at one fixed quantile would answer a question nobody
// asked.
func TestResolveUsesTheRequestedQuantile(t *testing.T) {
	cases := []struct {
		name     string
		quantile float64
	}{
		{name: "the first quartile", quantile: 0.25},
		{name: "the median", quantile: 0.50},
		{name: "the ninth decile", quantile: 0.90},
		{name: "the default", quantile: 0.95},
		{name: "the ninety ninth percentile", quantile: 0.99},
		{name: "a custom quantile", quantile: 0.975},
		{name: "the maximum", quantile: 1.0},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testProfile(t, RoleKindServer, []MeasuredPoint{
				quantilePoint(4, 1000, testCase.quantile, 500, 20),
				quantilePoint(8, 1200, testCase.quantile, 5000, 20),
			})
			catalog := testCatalog(t, profile)

			request := testRequest()
			request.Constraints.LatencyQuantile = testCase.quantile

			resolution, err := catalog.Resolve(request)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.Basis.RequestedQuantile != testCase.quantile {
				t.Fatalf("requested quantile %v is not %v", resolution.Basis.RequestedQuantile, testCase.quantile)
			}
			if resolution.Basis.Quantile != FormatLatencyQuantile(testCase.quantile) {
				t.Fatalf("measured quantile %q is not %q", resolution.Basis.Quantile, FormatLatencyQuantile(testCase.quantile))
			}
			if resolution.Basis.Conservative {
				t.Fatal("the resolution labels the requested quantile conservative")
			}
			for _, bound := range resolution.Basis.LatencyBounds {
				if bound.Quantile != testCase.quantile {
					t.Fatalf("bound %#v does not carry the measured quantile", bound)
				}
				if bound.Conservative {
					t.Fatalf("bound %#v labels the requested quantile conservative", bound)
				}
			}
			if resolution.SeedTokensPerSecond != 1000 {
				t.Fatalf("seed %v is not 1000", resolution.SeedTokensPerSecond)
			}
			if resolution.Basis.Boundary != BoundaryHigherLoadFailure {
				t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryHigherLoadFailure)
			}
		})
	}
}

// TestResolveHandlesAnInvalidQuantile asserts a quantile that names no
// quantile is refused, and that an omitted quantile takes the default.
//
// IT MATTERS BECAUSE A QUANTILE OUTSIDE THE UNIT INTERVAL, OR ONE THAT IS NOT
// A NUMBER AT ALL, WOULD SILENTLY BECOME A DIFFERENT CONSTRAINT.
func TestResolveHandlesAnInvalidQuantile(t *testing.T) {
	cases := []struct {
		name     string
		quantile float64
		want     string
		reason   RefusalReason
	}{
		{name: "an omitted quantile takes the default", quantile: 0, want: "resolved"},
		{name: "a negative quantile is refused", quantile: -0.1, want: "refused", reason: RefusalCapacityPriorUnresolved},
		{name: "a quantile above one is refused", quantile: 1.5, want: "refused", reason: RefusalCapacityPriorUnresolved},
		{name: "a quantile that is not a number is refused", quantile: math.NaN(), want: "refused", reason: RefusalCapacityPriorUnresolved},
		{name: "an infinite quantile is refused", quantile: math.Inf(1), want: "refused", reason: RefusalCapacityPriorUnresolved},
		{name: "a negative infinite quantile is refused", quantile: math.Inf(-1), want: "refused", reason: RefusalCapacityPriorUnresolved},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testProfile(t, RoleKindServer, []MeasuredPoint{
				quantilePoint(4, 1000, 0.95, 500, 20),
				quantilePoint(8, 1200, 0.95, 5000, 20),
			})
			catalog := testCatalog(t, profile)

			request := testRequest()
			request.Constraints.LatencyQuantile = testCase.quantile

			resolution, err := catalog.Resolve(request)
			if testCase.want == "refused" {
				if err == nil {
					t.Fatalf("expected a refusal, got the resolution %v", resolution)
				}
				refusal := refusalOf(t, err)
				if refusal.Reason != testCase.reason {
					t.Fatalf("refusal reason %q is not %q", refusal.Reason, testCase.reason)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.Basis.RequestedQuantile != DefaultLatencyQuantile {
				t.Fatalf("requested quantile %v is not %v", resolution.Basis.RequestedQuantile, DefaultLatencyQuantile)
			}
		})
	}
}

// TestResolveUsesAHigherQuantileConservatively asserts a higher quantile of
// the same metric is used as a bound and labeled, because a measurement taken
// at a higher quantile is not the measurement the request asked for.
//
// IT MATTERS BECAUSE A REQUEST AT THE NINTH DECILE IS ANSWERED BY A SOURCE
// THAT MEASURED THE NINETY NINTH PERCENTILE, AND THE ANSWER HAS TO SAY SO.
func TestResolveUsesAHigherQuantileConservatively(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		quantilePoint(4, 1000, 0.99, 500, 20),
		quantilePoint(8, 1200, 0.90, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.LatencyQuantile = 0.90

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.Basis.RequestedQuantile != 0.90 {
		t.Fatalf("requested quantile %v is not 0.90", resolution.Basis.RequestedQuantile)
	}
	if resolution.Basis.Quantile != "0.99" {
		t.Fatalf("measured quantile %q is not %q", resolution.Basis.Quantile, "0.99")
	}
	if !resolution.Basis.Conservative {
		t.Fatal("the resolution does not label the conservative bound")
	}
	for _, bound := range resolution.Basis.LatencyBounds {
		if bound.Quantile != 0.99 {
			t.Fatalf("bound %#v does not carry the measured quantile", bound)
		}
		if !bound.Conservative {
			t.Fatalf("bound %#v does not label the conservative bound", bound)
		}
	}
	if resolution.SeedTokensPerSecond != 1000 {
		t.Fatalf("seed %v is not 1000", resolution.SeedTokensPerSecond)
	}
}

// TestResolveDoesNotFailOnAHigherQuantile asserts a higher quantile that
// missed the target decides nothing, while the requested quantile that missed
// the target proves the failure.
//
// IT MATTERS BECAUSE A MEASUREMENT AT A HIGHER QUANTILE BOUNDS THE REQUESTED
// ONE FROM ABOVE AND CANNOT SAY WHETHER THE REQUESTED ONE MISSED THE TARGET,
// SO IT MUST NOT ESTABLISH A FAILURE BOUNDARY.
func TestResolveDoesNotFailOnAHigherQuantile(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		quantilePoint(4, 1000, 0.99, 500, 20),
		quantilePoint(8, 1200, 0.99, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	// THE REQUEST AT THE NINTH DECILE CANNOT DECIDE THE POINT ABOVE THE
	// FEASIBLE BOUNDARY, so no boundary is established.
	lower := testRequest()
	lower.Constraints.LatencyQuantile = 0.90

	_, err := catalog.Resolve(lower)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	refusal := refusalOf(t, err)
	if refusal.Reason != RefusalInsufficientLoadCoverage {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalInsufficientLoadCoverage)
	}

	// THE SAME PROFILE AT THE QUANTILE IT MEASURED PROVES THE FAILURE.
	higher := testRequest()
	higher.Constraints.LatencyQuantile = 0.99

	resolution, err := catalog.Resolve(higher)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.Basis.Boundary != BoundaryHigherLoadFailure {
		t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryHigherLoadFailure)
	}
	if resolution.SeedTokensPerSecond != 1000 {
		t.Fatalf("seed %v is not 1000", resolution.SeedTokensPerSecond)
	}
}

// TestResolveRefusesWhenNoPointCanBeDecided asserts a profile whose every
// measurement above the target comes from a higher quantile is refused,
// because nothing in it decides the target.
func TestResolveRefusesWhenNoPointCanBeDecided(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		quantilePoint(4, 1000, 0.99, 5000, 100),
		quantilePoint(8, 1200, 0.99, 5000, 100),
	})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.LatencyQuantile = 0.90

	_, err := catalog.Resolve(request)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	refusal := refusalOf(t, err)
	if refusal.Reason != RefusalCapacityPriorUnresolved {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalCapacityPriorUnresolved)
	}
	if !strings.Contains(refusal.Message, "cannot decide") {
		t.Fatalf("the refusal does not say that the measurement cannot decide the target: %q", refusal.Message)
	}
	if refusal.Detail["requestedQuantile"] != "0.9" {
		t.Fatalf("the refusal does not name the requested quantile: %v", refusal.Detail)
	}
}

// TestResolveChangesTheSeedWithTheQuantile asserts the requested quantile
// changes the answer and leaves the profile alone.
//
// IT MATTERS BECAUSE THE SAME MEASUREMENTS ANSWER DIFFERENT QUESTIONS AT
// DIFFERENT QUANTILES, AND THE ANSWER MAY NOT CHANGE THE PROFILE IT READ.
func TestResolveChangesTheSeedWithTheQuantile(t *testing.T) {
	low := quantilePoint(4, 1000, 0.95, 500, 20)
	low.Latencies = append(low.Latencies,
		testLatency(LatencyMetricTTFT, 0.99, 800),
		testLatency(LatencyMetricITL, 0.99, 30),
	)

	middle := quantilePoint(8, 1200, 0.50, 500, 40)
	middle.Latencies = append(middle.Latencies,
		testLatency(LatencyMetricTTFT, 0.99, 5000),
		testLatency(LatencyMetricITL, 0.99, 50),
	)

	// THE HIGHEST POINT IS MEASURED AT THE MEDIAN ONLY, so it cannot answer a
	// request at the ninety ninth percentile.
	highest := quantilePoint(16, 1400, 0.50, 6000, 40)

	profile := testProfile(t, RoleKindServer, []MeasuredPoint{low, middle, highest})
	catalog := testCatalog(t, profile)

	median := testRequest()
	median.Constraints.LatencyQuantile = 0.50

	medianResolution, err := catalog.Resolve(median)
	if err != nil {
		t.Fatalf("resolve at the median: %v", err)
	}
	if medianResolution.SeedTokensPerSecond != 1200 {
		t.Fatalf("seed %v is not 1200", medianResolution.SeedTokensPerSecond)
	}
	if medianResolution.Basis.Boundary != BoundaryHigherLoadFailure {
		t.Fatalf("boundary %q is not %q", medianResolution.Basis.Boundary, BoundaryHigherLoadFailure)
	}

	highestRequest := testRequest()
	highestRequest.Constraints.LatencyQuantile = 0.99

	highestResolution, err := catalog.Resolve(highestRequest)
	if err != nil {
		t.Fatalf("resolve at the ninety ninth percentile: %v", err)
	}
	if highestResolution.SeedTokensPerSecond != 1000 {
		t.Fatalf("seed %v is not 1000", highestResolution.SeedTokensPerSecond)
	}
	if highestResolution.Basis.Boundary != BoundaryHigherLoadFailure {
		t.Fatalf("boundary %q is not %q", highestResolution.Basis.Boundary, BoundaryHigherLoadFailure)
	}

	// THE PROFILE IS THE SAME PROFILE, because the quantile selects an answer
	// inside a profile and never forges a new one.
	for _, resolution := range []Resolution{medianResolution, highestResolution} {
		if resolution.Shape != profile.Shape().String() {
			t.Fatalf("resolution names shape %q, want %q", resolution.Shape, profile.Shape())
		}
		if resolution.Basis.ProfileDigest != profile.Digest {
			t.Fatalf("resolution names digest %q, want %q", resolution.Basis.ProfileDigest, profile.Digest)
		}
	}
}

// TestResolveNeverReportsTheRequestedQuantileAsTheMeasuredOne asserts the
// basis keeps the two apart.
//
// IT MATTERS BECAUSE A STATUS FIELD THAT SHOWS THE MEASURED QUANTILE AS THE
// REQUESTED ONE REPORTS A MEASUREMENT THAT WAS NOT TAKEN.
func TestResolveNeverReportsTheRequestedQuantileAsTheMeasuredOne(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		quantilePoint(4, 1000, 0.99, 500, 20),
		quantilePoint(8, 1200, 0.90, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.LatencyQuantile = 0.90

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.Basis.RequestedQuantile == 0.99 {
		t.Fatal("the basis reports the measured quantile as the requested one")
	}
	for _, bound := range resolution.Basis.LatencyBounds {
		if bound.Quantile == resolution.Basis.RequestedQuantile {
			t.Fatalf("bound %#v reports the requested quantile as the measured one", bound)
		}
	}
}

// TestCheckEligibilityAcceptsAProfileMeasuredBelowTheDefaultQuantile asserts
// a profile that measured below the default quantile still carries the
// evidence.
//
// IT MATTERS BECAUSE THE GENERATOR MUST NOT FILTER SUCH A PROFILE, because the
// quantile a request needs is not known when the catalog is written.
func TestCheckEligibilityAcceptsAProfileMeasuredBelowTheDefaultQuantile(t *testing.T) {
	cases := []struct {
		name     string
		quantile float64
	}{
		{name: "the first quartile", quantile: 0.25},
		{name: "the median", quantile: 0.50},
		{name: "the ninth decile", quantile: 0.90},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testProfile(t, RoleKindServer, []MeasuredPoint{
				quantilePoint(4, 1000, testCase.quantile, 500, 20),
				quantilePoint(8, 1200, testCase.quantile, 5000, 20),
			})

			if refusal := CheckEligibility(profile); refusal != nil {
				t.Fatalf("the profile was refused: %v", refusal)
			}
		})
	}
}

// TestResolveRefusesAProfileMeasuredBelowTheRequestedQuantile asserts a
// profile that measured below the requested quantile cannot answer the
// request.
//
// IT MATTERS BECAUSE THE EVIDENCE CHECK AND THE RESOLUTION ASK DIFFERENT
// QUESTIONS, AND A PROFILE THAT CARRIES EVIDENCE CAN STILL BE UNFIT FOR ONE
// REQUEST.
func TestResolveRefusesAProfileMeasuredBelowTheRequestedQuantile(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		quantilePoint(4, 1000, 0.90, 500, 20),
		quantilePoint(8, 1200, 0.90, 5000, 20),
	})
	catalog := testCatalog(t, profile)

	_, err := catalog.Resolve(testRequest())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	refusal := refusalOf(t, err)
	if refusal.Reason != RefusalIncompatibleMetric {
		t.Fatalf("refusal reason %q is not %q", refusal.Reason, RefusalIncompatibleMetric)
	}
	if refusal.Detail["requestedQuantile"] != "0.95" {
		t.Fatalf("the refusal does not name the requested quantile: %v", refusal.Detail)
	}
}

// TestResolveInterpolatesBetweenTwoMeasuredQuantiles asserts a request for a
// quantile the source did not measure is decided between the two it did.
//
// IT MATTERS BECAUSE A SOURCE THAT PUBLISHED A CURVE AT SEVERAL QUANTILES
// STATES THE VALUE BETWEEN THEM AS WELL, and a catalog that keeps one of them
// cannot answer a request for another.
func TestResolveInterpolatesBetweenTwoMeasuredQuantiles(t *testing.T) {
	feasible := serverPoint(4, 1000, 0, 0)
	feasible.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.90, 500),
		testLatency(LatencyMetricITL, 0.90, 20),
		testLatency(LatencyMetricTTFT, 0.99, 1000),
		testLatency(LatencyMetricITL, 0.99, 40),
	}
	failing := serverPoint(8, 1200, 0, 0)
	failing.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.90, 4000),
		testLatency(LatencyMetricITL, 0.90, 100),
		testLatency(LatencyMetricTTFT, 0.99, 5000),
		testLatency(LatencyMetricITL, 0.99, 120),
	}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{feasible, failing})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.LatencyQuantile = 0.95

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution.Basis.Quantile != FormatLatencyQuantile(0.95) {
		t.Fatalf("measured quantile %q is not %q", resolution.Basis.Quantile, FormatLatencyQuantile(0.95))
	}
	if resolution.Basis.Conservative {
		t.Fatal("the resolution labels the interpolated quantile conservative")
	}
	for _, bound := range resolution.Basis.LatencyBounds {
		if bound.Quantile != 0.95 {
			t.Fatalf("bound %#v does not carry the requested quantile", bound)
		}
		if bound.Conservative {
			t.Fatalf("bound %#v labels the interpolated quantile conservative", bound)
		}
	}
	if resolution.SeedTokensPerSecond != 1000 {
		t.Fatalf("seed %v is not 1000", resolution.SeedTokensPerSecond)
	}
	if resolution.Basis.Boundary != BoundaryHigherLoadFailure {
		t.Fatalf("boundary %q is not %q", resolution.Basis.Boundary, BoundaryHigherLoadFailure)
	}
}

// TestResolveDoesNotInterpolateAnOutOfOrderCurve asserts a curve that falls
// between two of its own measurements is not interpolated.
//
// IT MATTERS BECAUSE A SOURCE WHOSE CURVE FALLS CANNOT STATE THE VALUE IN
// BETWEEN, and an invented value would decide a target nobody measured.
func TestResolveDoesNotInterpolateAnOutOfOrderCurve(t *testing.T) {
	feasible := serverPoint(4, 1000, 0, 0)
	feasible.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.90, 1000),
		testLatency(LatencyMetricITL, 0.90, 20),
		testLatency(LatencyMetricTTFT, 0.99, 500),
		testLatency(LatencyMetricITL, 0.99, 40),
	}
	failing := serverPoint(8, 1200, 0, 0)
	failing.Latencies = []Latency{
		testLatency(LatencyMetricTTFT, 0.90, 4000),
		testLatency(LatencyMetricITL, 0.90, 100),
		testLatency(LatencyMetricTTFT, 0.99, 5000),
		testLatency(LatencyMetricITL, 0.99, 120),
	}
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{feasible, failing})
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Constraints.LatencyQuantile = 0.95

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolution.Basis.Conservative {
		t.Fatal("the resolution does not label the out of order curve conservative")
	}
	if resolution.Basis.Quantile != FormatLatencyQuantile(0.99) {
		t.Fatalf("measured quantile %q is not %q", resolution.Basis.Quantile, FormatLatencyQuantile(0.99))
	}
}

// TestResolveAcceptsUnstatedDeploymentQuantization asserts a deployment that
// leaves quantization empty resolves against a unique matching profile whose
// quantization is stated, and a revision mismatch reports model.revision rather
// than model.name.
func TestResolveAcceptsUnstatedDeploymentQuantization(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	profile.Identity.Model.Quantization = "fp8"
	profile.Identity.Model.Revision = "rev-a"
	profile = redigest(t, profile)
	catalog := testCatalog(t, profile)

	request := testRequest()
	request.Identity.Model.Quantization = ""
	request.Identity.Model.Revision = ""

	resolution, err := catalog.Resolve(request)
	if err != nil {
		t.Fatalf("resolve with unstated deployment quantization: %v", err)
	}
	if resolution.SeedTokensPerSecond != 1000 {
		t.Fatalf("seed %v is not 1000", resolution.SeedTokensPerSecond)
	}

	request.Identity.Model.Revision = "rev-b"
	_, err = catalog.Resolve(request)
	if err == nil {
		t.Fatal("expected a refusal on mismatched model.revision")
	}
	refusal := refusalOf(t, err)
	if refusal.Detail["mismatch"] != "model.revision" {
		t.Fatalf("mismatch = %q, want model.revision", refusal.Detail["mismatch"])
	}
}
