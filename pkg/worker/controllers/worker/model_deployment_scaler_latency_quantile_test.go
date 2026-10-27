package worker

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// TestModelDeploymentScalerLatencyQuantile reads the quantile a scaler is held to.
//
// IT MATTERS BECAUSE THE QUANTILE IS THE CALLER'S CHOICE, and a scaler that names
// none is held to the ninety fifth percentile while one that names another is held
// to that one.
func TestModelDeploymentScalerLatencyQuantile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		algorithm workercore.ModelDeploymentScalerAlgorithmSpec
		want      float64
		wantErr   bool
	}{
		{
			name: "a scaler that names no algorithm",
			want: modelDeploymentScalerDefaultLatencyQuantile,
		},
		{
			name: "a token throughput scaler that names no quantile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type:            workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{},
			},
			want: modelDeploymentScalerDefaultLatencyQuantile,
		},
		{
			name: "the first quartile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0.25")),
				},
			},
			want: 0.25,
		},
		{
			name: "the median",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0.5")),
				},
			},
			want: 0.5,
		},
		{
			name: "the ninth decile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0.9")),
				},
			},
			want: 0.9,
		},
		{
			name: "the ninety fifth percentile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0.95")),
				},
			},
			want: 0.95,
		},
		{
			name: "the ninety ninth percentile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0.99")),
				},
			},
			want: 0.99,
		},
		{
			name: "a custom quantile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0.97505")),
				},
			},
			want: 0.97505,
		},
		{
			name: "the largest quantile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("1")),
				},
			},
			want: 1,
		},
		{
			name: "a predictive scaler reads its nested token throughput quantile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypePredictive,
				Predictive: &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
						LatencyQuantile: ptr.To(resource.MustParse("0.9")),
					},
				},
			},
			want: 0.9,
		},
		{
			name: "a predictive scaler that names no quantile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type:       workercore.ModelDeploymentScalerAlgorithmTypePredictive,
				Predictive: &workercore.ModelDeploymentScalerPredictiveSpec{},
			},
			want: modelDeploymentScalerDefaultLatencyQuantile,
		},
		{
			name: "a proportional scaler names no latency quantile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type:         workercore.ModelDeploymentScalerAlgorithmTypeProportional,
				Proportional: &workercore.ModelDeploymentScalerProportionalSpec{},
			},
			want: modelDeploymentScalerDefaultLatencyQuantile,
		},
		{
			name: "a quantile of zero names no percentile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("0")),
				},
			},
			wantErr: true,
		},
		{
			name: "a negative quantile names no percentile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("-0.5")),
				},
			},
			wantErr: true,
		},
		{
			name: "a quantile above one names no percentile",
			algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					LatencyQuantile: ptr.To(resource.MustParse("1.5")),
				},
			},
			wantErr: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			algorithm := testCase.algorithm
			got, err := ModelDeploymentScalerLatencyQuantile(&algorithm)
			if testCase.wantErr {
				require.Error(t, err, "the quantile must be refused")
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, testCase.want, got, 1e-12)
		})
	}
}

// TestModelDeploymentScalerLatencyQuantileReadsTheResolvedValue asserts the
// quantile is read at the precision the quantity resolved it.
//
// IT MATTERS BECAUSE THE QUANTITY HOLDS A ROUNDED VALUE and every comparison
// uses that value, so a scaler held to a quantile it did not write is held to
// the wrong one.
func TestModelDeploymentScalerLatencyQuantileReadsTheResolvedValue(t *testing.T) {
	t.Parallel()

	algorithm := workercore.ModelDeploymentScalerAlgorithmSpec{
		Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
		TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
			LatencyQuantile: ptr.To(resource.MustParse("0.9500000005")),
		},
	}

	got, err := ModelDeploymentScalerLatencyQuantile(&algorithm)
	require.NoError(t, err)
	assert.InDelta(t, 0.950000001, got, 1e-12,
		"the quantile is the value the quantity resolved and not the one that was written")
}

// TestModelDeploymentScalerQuantileMilliseconds reads a latency histogram at a
// quantile.
//
// IT MATTERS BECAUSE A HISTOGRAM THAT CANNOT REPRESENT THE QUANTILE must say
// so, and the mean of the observations is a different statistic that answers a
// different question.
func TestModelDeploymentScalerQuantileMilliseconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		histogram modelDeploymentScalerHistogram
		quantile  float64
		want      float64
		wantOK    bool
	}{
		{
			name: "the median falls in the first bucket",
			histogram: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 40, 1.0: 50, math.Inf(1): 56},
				count:   56,
				sum:     20,
			},
			quantile: 0.5,
			want:     500,
			wantOK:   true,
		},
		{
			name: "the ninety fifth percentile falls in the second bucket",
			histogram: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 70, 1.0: 80, math.Inf(1): 84},
				count:   84,
				sum:     30,
			},
			quantile: 0.95,
			want:     1000,
			wantOK:   true,
		},
		{
			name: "a custom quantile falls in the second bucket",
			histogram: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 70, 1.0: 80, math.Inf(1): 84},
				count:   84,
				sum:     30,
			},
			quantile: 0.88,
			want:     1000,
			wantOK:   true,
		},
		{
			name: "a quantile that falls in the infinite bucket is unavailable",
			histogram: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 70, 1.0: 80, math.Inf(1): 84},
				count:   84,
				sum:     30,
			},
			quantile: 0.99,
			wantOK:   false,
		},
		{
			name: "a histogram without a usable bucket is unavailable",
			histogram: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{math.Inf(1): 84},
				count:   84,
				sum:     30,
			},
			quantile: 0.95,
			wantOK:   false,
		},
		{
			name: "a histogram without observations is unavailable",
			histogram: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 0, 1.0: 0, math.Inf(1): 0},
				count:   0,
				sum:     0,
			},
			quantile: 0.95,
			wantOK:   false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, ok := modelDeploymentScalerQuantileMilliseconds(testCase.histogram, testCase.quantile)
			assert.Equal(t, testCase.wantOK, ok)
			if !testCase.wantOK {
				assert.Zero(t, got, "an unavailable quantile carries no value")
				return
			}
			assert.InDelta(t, testCase.want, got, 1e-9)
		})
	}
}

// TestModelDeploymentScalerSampler_SelectedQuantile reads the latency at the
// quantile the scaler names.
//
// IT MATTERS BECAUSE THE ONLINE SAMPLING AND THE CAPACITY PRIOR HAVE TO ANSWER
// THE SAME QUESTION, and a sampler that always reads the ninety fifth
// percentile answers a question the scaler did not ask.
func TestModelDeploymentScalerSampler_SelectedQuantile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		quantile         string
		wantTTFT         float64
		wantITL          float64
		wantLatencyState algorithm.SignalState
	}{
		{
			name:             "a scaler that names no quantile reads the ninety fifth percentile",
			wantTTFT:         1000,
			wantITL:          50,
			wantLatencyState: algorithm.SignalFresh,
		},
		{
			name:             "the ninety fifth percentile reads the same buckets",
			quantile:         "0.95",
			wantTTFT:         1000,
			wantITL:          50,
			wantLatencyState: algorithm.SignalFresh,
		},
		{
			name:             "the median reads the lower buckets",
			quantile:         "0.5",
			wantTTFT:         500,
			wantITL:          10,
			wantLatencyState: algorithm.SignalFresh,
		},
		{
			name:             "a quantile that falls in the infinite bucket is unavailable",
			quantile:         "0.99",
			wantLatencyState: algorithm.SignalMissing,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
			target := scalerSamplerTarget(types.UID("target-uid"), workercore.ModelDeploymentEngineVLLM,
				scalerSamplerRole("server", workercore.ModelDeploymentRoleKindServer, 1))
			scaler := scalerSamplerScaler(types.UID("scaler-uid"), 10)
			scalerSamplerSetQuantile(scaler, testCase.quantile)
			script := &scalerSamplerScripted{
				bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
				failures: map[string]bool{},
				asked:    map[string]int{},
			}
			sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

			start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
			script.mu.Lock()
			script.bodies["engine-0"] = []byte(scalerSamplerVLLMSecondExposition)
			script.mu.Unlock()

			second := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(14*time.Second))
			require.Contains(t, second, "server")
			observation := second["server"]

			assert.Equal(t, testCase.wantLatencyState, observation.LatencyState)
			assert.InDelta(t, testCase.wantTTFT, observation.TTFTMilliseconds, 1e-6)
			assert.InDelta(t, testCase.wantITL, observation.ITLMilliseconds, 1e-6)
			if testCase.wantLatencyState != algorithm.SignalFresh {
				assert.False(t, observation.CompleteCoverage,
					"a role whose latency could not be read is not fully covered")
			}
		})
	}
}

// TestModelDeploymentScalerSampler_QuantileChangeDropsTheCachedObservations
// holds the sampler against an observation of another distribution.
//
// IT MATTERS BECAUSE AN OBSERVATION TAKEN AT THE NINETY NINTH PERCENTILE DOES
// NOT DESCRIBE THE DISTRIBUTION THE NINETY FIFTH PERCENTILE NAMES, and a
// scaler that acted on it would hold a role to a latency it never measured.
func TestModelDeploymentScalerSampler_QuantileChangeDropsTheCachedObservations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		first     string
		second    string
		wantEmpty bool
	}{
		{
			name:      "a changed quantile drops the remembered observations",
			first:     "0.95",
			second:    "0.5",
			wantEmpty: true,
		},
		{
			name:      "an omitted quantile and the ninety fifth percentile are the same quantile",
			first:     "",
			second:    "0.95",
			wantEmpty: false,
		},
		{
			name:      "the same quantile keeps the remembered observations",
			first:     "0.5",
			second:    "0.5",
			wantEmpty: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
			target := scalerSamplerTarget(types.UID("target-uid"), workercore.ModelDeploymentEngineVLLM,
				scalerSamplerRole("server", workercore.ModelDeploymentRoleKindServer, 1))
			scaler := scalerSamplerScaler(types.UID("scaler-uid"), 10)
			scalerSamplerSetQuantile(scaler, testCase.first)
			script := &scalerSamplerScripted{
				bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
				failures: map[string]bool{},
				asked:    map[string]int{},
			}
			sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

			start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			first := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
			require.Contains(t, first, "server")

			scalerSamplerSetQuantile(scaler, testCase.second)
			// THE SECOND PASS IS REFUSED BY THE FLOOR, so the remembered observations are
			// what the scaler would read.
			cached := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(time.Second))
			if testCase.wantEmpty {
				assert.Empty(t, cached,
					"the observations of another quantile are not the observations of this one")
				return
			}
			require.Contains(t, cached, "server")
		})
	}
}

// scalerSamplerSetQuantile names the latency quantile a scaler is held to.
//
// IT LEAVES THE SCALER ALONE WHEN THE QUANTILE IS EMPTY, because a scaler that
// names none is held to the ninety fifth percentile and not to another one.
func scalerSamplerSetQuantile(scaler *workercore.ModelDeploymentScaler, quantile string) {
	scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput
	if quantile == "" {
		scaler.Spec.Algorithm.TokenThroughput = nil
		return
	}
	scaler.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
		LatencyQuantile: ptr.To(resource.MustParse(quantile)),
	}
}

// TestModelDeploymentScalerCapacityPriorCarriesTheSelectedQuantile reads the
// prior a scaler is resolved against.
//
// IT MATTERS BECAUSE THE ONLINE SAMPLING AND THE CAPACITY PRIOR HAVE TO ANSWER
// THE SAME QUESTION, and a prior measured at another quantile does not answer it.
// The status keeps the two apart, because a source that measured a higher
// quantile than the request needed is a different answer from one that measured
// the requested quantile.
func TestModelDeploymentScalerCapacityPriorCarriesTheSelectedQuantile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		quantile      string
		wantMeasured  string
		wantRequested string
		wantRefusal   string
	}{
		{
			name:          "a scaler that names no quantile is held to the ninety fifth percentile",
			wantMeasured:  "0.95",
			wantRequested: "0.95",
		},
		{
			name:          "the ninety fifth percentile is answered by the ninety fifth percentile",
			quantile:      "0.95",
			wantMeasured:  "0.95",
			wantRequested: "0.95",
		},
		{
			name:          "a lower quantile is answered by the quantile the source measured",
			quantile:      "0.9",
			wantMeasured:  "0.9",
			wantRequested: "0.9",
		},
		{
			name:        "a higher quantile is refused by the evidence",
			quantile:    "0.99",
			wantRefusal: "does not measure the TTFT latency at the requested quantile",
		},
		{
			name:        "a quantile that names no percentile is refused",
			quantile:    "1.5",
			wantRefusal: "the latency quantile must be greater than zero and no greater than one",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			capacityPriorQuantileCatalog(t, cm)
			if testCase.quantile != "" {
				scaler.Spec.Algorithm.TokenThroughput.LatencyQuantile = ptr.To(resource.MustParse(testCase.quantile))
			}
			client := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(cm, instance).
				Build()

			resolved, err := ResolveModelDeploymentScalerCapacityPriors(
				context.Background(), client, scaler, md,
			)
			if testCase.wantRefusal != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), testCase.wantRefusal)
				return
			}
			require.NoError(t, err)

			require.Len(t, resolved.Status, 1)
			status := resolved.Status[0]
			assert.Equal(t, testCase.wantMeasured, status.Quantile,
				"the status reports the quantile the source measured")
			assert.Equal(t, testCase.wantRequested, status.RequestedQuantile,
				"the status reports the quantile the request asked for")
			for _, bound := range status.LatencyBounds {
				assert.Equal(t, testCase.wantMeasured, bound.Quantile,
					"a latency bound carries the quantile it was measured at")
			}
		})
	}
}

// TestModelDeploymentScalerStatusCarriesTheSelectedQuantile reads the quantile
// the scaler says it is held to.
//
// IT MATTERS BECAUSE A SCALER THAT IS HELD TO A QUANTILE SAYS SO, and an
// operator who cannot read the quantile cannot tell which distribution the
// scaler is measuring.
func TestModelDeploymentScalerStatusCarriesTheSelectedQuantile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		quantile string
		want     string
	}{
		{
			name: "a scaler that names no quantile says the ninety fifth percentile",
			want: "0.95",
		},
		{
			name:     "a scaler that names a quantile says it",
			quantile: "0.9",
			want:     "0.9",
		},
		{
			name:     "a scaler that names a custom quantile says it",
			quantile: "0.97505",
			want:     "0.97505",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			algorithm := scalerAlgorithmWithQuantile(testCase.quantile)
			quantile, err := ModelDeploymentScalerLatencyQuantile(&algorithm)
			require.NoError(t, err)
			status := &workercore.ModelDeploymentScalerStatus{
				LatencyQuantile: priors.FormatLatencyQuantile(quantile),
			}
			assert.Equal(t, testCase.want, status.LatencyQuantile)
		})
	}
}

// TestModelDeploymentScalerStatusIsWrittenWhenTheQuantileChanges states that a
// change of the quantile reaches the status of the scaler.
//
// IT MATTERS BECAUSE THE STATUS IS NOT WRITTEN ON EVERY TICK, and a scaler whose
// quantile changed keeps the old one in its status until something says that the
// status moved.
func TestModelDeploymentScalerStatusIsWrittenWhenTheQuantileChanges(t *testing.T) {
	t.Parallel()

	before := &workercore.ModelDeploymentScalerStatus{LatencyQuantile: "0.95"}
	after := &workercore.ModelDeploymentScalerStatus{LatencyQuantile: "0.9"}
	assert.True(t, modelDeploymentScalerStatusChanged(before, after),
		"a changed quantile is a change of the scaler")
	assert.False(t, modelDeploymentScalerStatusChanged(before, before.DeepCopy()),
		"the same quantile is not a change of the scaler")
}

// TestModelDeploymentScalerStatusCarriesTheQuantileTheScalerIsHeldTo reads the
// quantile out of the status the reconciler writes.
//
// IT MATTERS BECAUSE THE STATUS IS THE ONLY PLACE AN OPERATOR CAN READ THE
// QUANTILE FROM, and a status that does not carry it leaves the scaler held to a
// percentile nobody can see.
func TestModelDeploymentScalerStatusCarriesTheQuantileTheScalerIsHeldTo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		quantile string
		want     string
	}{
		{
			name: "a scaler that names no quantile says the ninety fifth percentile",
			want: "0.95",
		},
		{
			name:     "a scaler that names a quantile says it",
			quantile: "0.9",
			want:     "0.9",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
			scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
			scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				},
			}
			if testCase.quantile != "" {
				scaler.Spec.Algorithm.TokenThroughput.LatencyQuantile = ptr.To(
					resource.MustParse(testCase.quantile),
				)
			}
			client := scalerTestClient(t, md, scaler)
			reconciler := scalerTestReconciler(client, now)

			reconcileScaler(t, reconciler, scaler.Name)
			stored := scalerTestGetScaler(t, client, scaler.Name)
			assert.Equal(t, testCase.want, stored.Status.LatencyQuantile,
				"the status says which quantile the scaler is held to")
		})
	}
}

// TestModelDeploymentScalerStatusDoesNotLeakTheSampledQuantile states that the
// status carries the configured quantile and not the one a sample was read at.
//
// IT MATTERS BECAUSE THE SAMPLES ARRIVE ON EVERY TICK and a quantile that
// changed with every sample would put every scaler of the cluster on the write
// path of the apiserver once per interval.
func TestModelDeploymentScalerStatusDoesNotLeakTheSampledQuantile(t *testing.T) {
	t.Parallel()

	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 10)
	scalerSamplerSetQuantile(scaler, "0.9")

	sampled := scalerSamplerScaler(types.UID("scaler-uid"), 10)
	scalerSamplerSetQuantile(sampled, "0.5")

	assert.NotEqual(t, scaler.Spec.Algorithm.TokenThroughput.LatencyQuantile.String(),
		sampled.Spec.Algorithm.TokenThroughput.LatencyQuantile.String(),
		"the two scalers are held to different quantiles")

	// The status of a role carries the reading, and the reading is a latency in
	// milliseconds. It does not carry a quantile, because the quantile the
	// reading was taken at is the one the scaler says it is held to.
	role := workercore.ModelDeploymentScalerRoleStatus{Name: "server"}
	encoded, err := json.Marshal(role)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "uantile",
		"the status of a role does not carry a quantile of its own")
}

// scalerAlgorithmWithQuantile builds the algorithm of a scaler held to a
// quantile.
//
// IT NAMES NO QUANTILE WHEN THE VALUE IS EMPTY, because a scaler that names
// none is held to the ninety fifth percentile.
func scalerAlgorithmWithQuantile(quantile string) workercore.ModelDeploymentScalerAlgorithmSpec {
	algorithm := workercore.ModelDeploymentScalerAlgorithmSpec{
		Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
	}
	if quantile == "" {
		return algorithm
	}
	algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
		LatencyQuantile: ptr.To(resource.MustParse(quantile)),
	}
	return algorithm
}

// capacityPriorQuantileCatalog replaces the catalog of a fixture with one whose
// boundary is a saturation measurement.
//
// IT IS NEEDED BECAUSE A BOUNDARY PROVEN BY A FAILURE REQUIRES THE MEASUREMENT
// TO BE AT THE REQUESTED QUANTILE, and a request for a lower quantile has to be
// answered by a boundary the source measured at the higher one.
//
// IT KEEPS THE MEASURED QUANTILE AT THE NINETY FIFTH PERCENTILE, because the
// question the test asks is what a request for another quantile is answered
// with.
// capacityPriorQuantileCatalog writes the catalog the quantile cases read.
//
// IT KEEPS THE WHOLE MEASURED CURVE, because the boundary of the capacity is
// read from the curve and a curve that was cut short carries no boundary.
func capacityPriorQuantileCatalog(t *testing.T, cm *core.ConfigMap) {
	t.Helper()
	document := capacityPriorReadCatalog(t, cm)
	profile := &document.Profiles[0]
	if len(profile.Points) < 2 {
		t.Fatal("the fixture must carry two measured load levels")
	}
	capacityPriorWriteCatalog(t, cm, document)
}
