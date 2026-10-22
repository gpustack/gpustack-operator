package algorithm

import (
	"math"
	"testing"
	"time"
)

func freshObservation(at time.Time) RoleObservation {
	return RoleObservation{
		Role:             "role",
		Kind:             RoleKindServer,
		CurrentReplicas:  2,
		ReadyReplicas:    2,
		ThroughputState:  SignalFresh,
		LatencyState:     SignalFresh,
		GaugeState:       SignalFresh,
		CompleteCoverage: true,
		ObservedAt:       at,
	}
}

func TestProportionalUsesReadyReplicasAndDeadband(t *testing.T) {
	at := time.Unix(0, 0)
	alg := Proportional{Metric: MetricRunningRequests, TargetValue: 4, Tolerance: 0.1}
	obs := freshObservation(at)
	obs.RunningRequests = 12
	got, _ := alg.Recommend(obs, nil)
	if got.DesiredReplicas != 3 || got.Reason != "ScaleUp" {
		t.Fatalf("scale up = %#v", got)
	}
	obs.CurrentReplicas = 3
	obs.ReadyReplicas = 3
	obs.RunningRequests = 12.2
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != 3 || got.Reason != "WithinTolerance" {
		t.Fatalf("deadband = %#v", got)
	}
	obs.RunningRequests = 0
	obs.GaugeState = SignalIdle
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != 1 {
		t.Fatalf("idle = %#v", got)
	}
	obs.ReadyReplicas = 0
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != obs.CurrentReplicas || !got.HoldScaleDown || got.Reason != "NoReadyReplicas" {
		t.Fatalf("no ready = %#v", got)
	}

	// In-progress rollout (CurrentReplicas=4, ReadyReplicas=1) under mild load holds scale-down.
	obs.CurrentReplicas = 4
	obs.ReadyReplicas = 1
	obs.GaugeState = SignalFresh
	obs.RunningRequests = 3
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != 4 || !got.HoldScaleDown {
		t.Fatalf("partial readiness under-target = %#v, want 4 with HoldScaleDown", got)
	}
}

func TestProportionalMissingAndPartialCoverage(t *testing.T) {
	alg := Proportional{Metric: MetricRunningRequests, TargetValue: 1}
	obs := freshObservation(time.Unix(0, 0))
	obs.RunningRequests = 0
	obs.GaugeState = SignalMissing
	got, _ := alg.Recommend(obs, nil)
	if got.Reason != "WaitingForMetrics" || got.DesiredReplicas != obs.CurrentReplicas || !got.HoldScaleDown {
		t.Fatalf("missing = %#v", got)
	}
	obs.GaugeState = SignalFresh
	obs.RunningRequests = 10
	obs.CompleteCoverage = false
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas <= obs.CurrentReplicas || got.HoldScaleDown {
		t.Fatalf("partial scale-up = %#v", got)
	}
	obs.RunningRequests = 0
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != obs.CurrentReplicas || !got.HoldScaleDown {
		t.Fatalf("partial scale-down = %#v", got)
	}

	obs.GaugeState = SignalIdle
	obs.CompleteCoverage = true
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != 1 {
		t.Fatalf("idle scale-down = %#v, want 1", got)
	}
}

// TestProportionalZeroReadingIsNotAToleranceBand holds that a reading of nothing that is not
// idle is no reading at all, and that the recommendation says it waits for metrics rather than
// reporting the count as one that sits within the tolerance of the target.
func TestProportionalZeroReadingIsNotAToleranceBand(t *testing.T) {
	alg := Proportional{Metric: MetricRunningRequests, TargetValue: 4, Tolerance: 0.1}
	cases := []struct {
		name              string
		gaugeState        SignalState
		wantDesired       int32
		wantReason        string
		wantHoldScaleDown bool
	}{
		{
			name:              "a zero reading that is not idle waits for metrics",
			gaugeState:        SignalFresh,
			wantDesired:       2,
			wantReason:        ReasonWaitingForMetrics,
			wantHoldScaleDown: true,
		},
		{
			name:        "an idle zero reading scales down to the floor",
			gaugeState:  SignalIdle,
			wantDesired: 1,
			wantReason:  ReasonScaleDown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := freshObservation(time.Unix(0, 0))
			obs.GaugeState = tc.gaugeState
			obs.RunningRequests = 0
			got, _ := alg.Recommend(obs, nil)
			if got.DesiredReplicas != tc.wantDesired || got.Reason != tc.wantReason ||
				got.HoldScaleDown != tc.wantHoldScaleDown {
				t.Fatalf("recommendation = %#v, want desired %d, reason %s and hold %v",
					got, tc.wantDesired, tc.wantReason, tc.wantHoldScaleDown)
			}
		})
	}
}

func TestTokenThroughputRolesPanicAndLatency(t *testing.T) {
	base := time.Unix(100, 0)
	alg := TokenThroughput{
		PrefillTargetTokensPerSecond: 10,
		DecodeTargetTokensPerSecond:  10,
		ServerTargetTokensPerSecond:  10,
		StableWindowSeconds:          180,
		PanicWindowSeconds:           60,
		PanicThreshold:               2,
		TTFTTargetMilliseconds:       100,
		ITLTargetMilliseconds:        10,
	}
	obs := freshObservation(base)
	obs.Kind = RoleKindPrefill
	obs.PromptTokensPerSecond = 15
	got, state := alg.Recommend(obs, nil)
	if got.DesiredReplicas != 2 || state == nil || state.History.Len() != 1 {
		t.Fatalf("prefill = %#v state=%#v", got, state)
	}
	obs.Kind = RoleKindDecode
	obs.GenerationTokensPerSecond = 10
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != 1 {
		t.Fatalf("decode = %#v", got)
	}
	state = nil
	obs.Kind = RoleKindServer
	obs.PromptTokensPerSecond = 20
	obs.GenerationTokensPerSecond = 20
	obs.ObservedAt = base.Add(10 * time.Second)
	got, state = alg.Recommend(obs, state)
	if got.DesiredReplicas < 2 || !got.InPanic || !got.HoldScaleDown {
		t.Fatalf("panic = %#v state=%#v", got, state)
	}
	obs.PromptTokensPerSecond = 0
	obs.GenerationTokensPerSecond = 0
	obs.ThroughputState = SignalIdle
	obs.ObservedAt = base.Add(30 * time.Second)
	got, state = alg.Recommend(obs, state)
	if !got.InPanic || !got.HoldScaleDown {
		t.Fatalf("sticky panic = %#v state=%#v", got, state)
	}
	obs.LatencyState = SignalFresh
	obs.ThroughputState = SignalFresh
	obs.PromptTokensPerSecond = 10
	obs.GenerationTokensPerSecond = 10
	obs.TTFTMilliseconds = 110
	obs.ObservedAt = base.Add(4 * time.Minute)
	got, _ = alg.Recommend(obs, state)
	if got.Reason != ReasonLatencyGuardHold || got.DesiredReplicas != obs.CurrentReplicas || !got.HoldScaleDown {
		t.Fatalf("latency hold = %#v, want ReasonLatencyGuardHold", got)
	}

	obs.TTFTMilliseconds = 121
	obs.ObservedAt = base.Add(5 * time.Minute)
	got, _ = alg.Recommend(obs, state)
	if got.Reason != "LatencyGuardScaleUp" || got.DesiredReplicas < obs.CurrentReplicas+1 {
		t.Fatalf("latency up = %#v", got)
	}
}

func TestTokenThroughputMissingLatencyAndPartialCoverage(t *testing.T) {
	alg := TokenThroughput{ServerTargetTokensPerSecond: 10, TTFTTargetMilliseconds: 100}
	obs := freshObservation(time.Unix(0, 0))
	obs.Kind = RoleKindServer
	obs.PromptTokensPerSecond = 0
	obs.GenerationTokensPerSecond = 0
	obs.ThroughputState = SignalIdle
	obs.LatencyState = SignalMissing
	got, _ := alg.Recommend(obs, nil)
	if got.Reason != "WaitingForMetrics" || got.DesiredReplicas != obs.CurrentReplicas {
		t.Fatalf("missing latency = %#v", got)
	}
	obs.ThroughputState = SignalFresh
	obs.PromptTokensPerSecond = 50
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas <= obs.CurrentReplicas || !got.HoldScaleDown {
		t.Fatalf("missing latency must not suppress throughput scale-up = %#v", got)
	}
	obs.PromptTokensPerSecond = 0
	obs.ThroughputState = SignalIdle
	obs.LatencyState = SignalIdle
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas != 1 {
		t.Fatalf("idle latency = %#v", got)
	}

	obsUnknown := freshObservation(time.Unix(0, 0))
	obsUnknown.Kind = RoleKindUnknown
	gotUnknown, _ := alg.Recommend(obsUnknown, nil)
	if gotUnknown.Reason != ReasonWaitingForMetrics || gotUnknown.DesiredReplicas != obsUnknown.CurrentReplicas || !gotUnknown.HoldScaleDown {
		t.Fatalf("unknown role kind = %#v, want ReasonWaitingForMetrics and HoldScaleDown", gotUnknown)
	}
	obs.LatencyState = SignalFresh
	obs.PromptTokensPerSecond = 100
	obs.CompleteCoverage = false
	got, _ = alg.Recommend(obs, nil)
	if got.DesiredReplicas <= obs.CurrentReplicas {
		t.Fatalf("partial up = %#v", got)
	}

	// NaN latency with Fresh state treats as WaitingForMetrics and holds.
	obsNaN := freshObservation(time.Unix(0, 0))
	obsNaN.Kind = RoleKindServer
	obsNaN.TTFTMilliseconds = math.NaN()
	obsNaN.PromptTokensPerSecond = 50
	obsNaN.LatencyState = SignalFresh
	got, _ = alg.Recommend(obsNaN, nil)
	if !got.HoldScaleDown {
		t.Fatalf("NaN latency must hold scale-down = %#v", got)
	}

	// When history spans the full stable window, missing latency does not block scale down.
	var state *PredictorState
	historyObs := freshObservation(time.Unix(0, 0))
	historyObs.Kind = RoleKindServer
	historyObs.CurrentReplicas = 4
	historyObs.PromptTokensPerSecond = 10
	historyObs.GenerationTokensPerSecond = 0
	historyObs.LatencyState = SignalMissing
	historyObs.ThroughputState = SignalFresh
	got, state = alg.Recommend(historyObs, state)
	if got.Reason != "WaitingForMetrics" || got.DesiredReplicas != 4 {
		t.Fatalf("initial observation with missing latency should hold: %#v", got)
	}

	// Advance time past the stable window with regular ticks and low token demand.
	for ts := int64(15); ts <= 180; ts += 15 {
		historyObs.ObservedAt = time.Unix(ts, 0)
		got, state = alg.Recommend(historyObs, state)
	}
	if got.Reason != "ScaleDown" || got.DesiredReplicas != 1 || got.HoldScaleDown {
		t.Fatalf("matured history should allow scale-down despite missing latency: %#v", got)
	}
}

func TestPredictiveWarmupForecastAndHistory(t *testing.T) {
	alg := Predictive{
		TokenThroughput:               TokenThroughput{ServerTargetTokensPerSecond: 100},
		ServerTokenCapacityPerReplica: 100,
		HorizonSeconds:                300,
	}
	var state *PredictorState
	var rec RoleRecommendation
	for i := 0; i < 6; i++ {
		obs := freshObservation(time.Unix(int64(i*15), 0))
		obs.Kind = RoleKindServer
		obs.PromptTokensPerSecond = float64(100 + i*40)
		rec, state = alg.Recommend(obs, state)
		if i < 5 && rec.Reason != "PredictorWarmingUp" {
			t.Fatalf("warmup %d = %#v", i, rec)
		}
	}
	if state.SampleCount != 6 || state.History.Len() != 6 {
		t.Fatalf("state = %#v", state)
	}
	if state.Level <= 0 || math.IsNaN(state.Level) || math.IsNaN(state.Trend) {
		t.Fatalf("filter state = %#v", state)
	}
	obs := freshObservation(time.Unix(90, 0))
	obs.Kind = RoleKindServer
	obs.PromptTokensPerSecond = 340
	obs.CurrentReplicas = 1
	obs.ReadyReplicas = 1
	rec, state = alg.Recommend(obs, state)
	if rec.DesiredReplicas < 2 || state.SampleCount != 7 {
		t.Fatalf("forecast = %#v state=%#v", rec, state)
	}
	for i := 0; i < 40; i++ {
		obs.ObservedAt = time.Unix(int64(105+i*15), 0)
		_, state = alg.Recommend(obs, state)
	}
	if state.History.Len() > 32 {
		t.Fatalf("history len = %d", state.History.Len())
	}
}

func TestPredictiveConsolidationAndLatencyCorrection(t *testing.T) {
	alg := Predictive{
		TokenThroughput:               TokenThroughput{ServerTargetTokensPerSecond: 200, TTFTTargetMilliseconds: 100},
		ServerTokenCapacityPerReplica: 100,
		HorizonSeconds:                1,
	}
	state := &PredictorState{SampleCount: 6, Level: 100, Trend: 0, Covariance: [4]float64{1, 0, 0, 1}}
	obs := freshObservation(time.Unix(0, 0))
	obs.Kind = RoleKindServer
	obs.CurrentReplicas = 3
	obs.ReadyReplicas = 3
	obs.PromptTokensPerSecond = 170
	obs.LatencyState = SignalFresh
	obs.TTFTMilliseconds = 100
	rec, _ := alg.Recommend(obs, state)
	if rec.DesiredReplicas != obs.CurrentReplicas {
		t.Fatalf("consolidation hold = %#v", rec)
	}
	obs.TTFTMilliseconds = 121
	rec, _ = alg.Recommend(obs, state)
	if rec.DesiredReplicas < obs.CurrentReplicas+1 || rec.Reason != "LatencyGuardScaleUp" {
		t.Fatalf("latency correction = %#v", rec)
	}
}

// TestPredictiveWarmupKeepsAnUpwardReason holds that the warmup of the predictor does not
// overwrite the reason of a recommendation that asks for more, because that reason says more
// about the load than the state of the predictor does.
func TestPredictiveWarmupKeepsAnUpwardReason(t *testing.T) {
	cases := []struct {
		name             string
		throughput       float64
		ttftMilliseconds float64
		latencyTarget    float64
		wantDesired      int32
		wantReason       string
	}{
		{
			name:        "a scale-up keeps its reason through the warmup",
			throughput:  250,
			wantDesired: 3,
			wantReason:  ReasonScaleUp,
		},
		{
			name:             "a latency guard scale-up keeps its reason through the warmup",
			throughput:       100,
			ttftMilliseconds: 121,
			latencyTarget:    100,
			wantDesired:      3,
			wantReason:       ReasonLatencyGuardScaleUp,
		},
		{
			name:        "a recommendation that asks for no more is marked as warming up",
			throughput:  100,
			wantDesired: 1,
			wantReason:  ReasonPredictorWarmingUp,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alg := Predictive{
				TokenThroughput: TokenThroughput{
					ServerTargetTokensPerSecond: 100,
					TTFTTargetMilliseconds:      tc.latencyTarget,
				},
				ServerTokenCapacityPerReplica: 100,
				HorizonSeconds:                300,
			}
			obs := freshObservation(time.Unix(0, 0))
			obs.Kind = RoleKindServer
			obs.PromptTokensPerSecond = tc.throughput
			obs.TTFTMilliseconds = tc.ttftMilliseconds
			got, state := alg.Recommend(obs, nil)
			if state.SampleCount >= 6 {
				t.Fatalf("the predictor warmed up before the case asked it to: %#v", state)
			}
			if got.DesiredReplicas != tc.wantDesired || got.Reason != tc.wantReason {
				t.Fatalf("recommendation = %#v, want desired %d and reason %s",
					got, tc.wantDesired, tc.wantReason)
			}
		})
	}
}

// TestPredictiveForecastScaleUpReason holds that a recommendation the forecast raises above the
// current count says it scales up, because the reason it carried was about the count the
// recommendation no longer has.
func TestPredictiveForecastScaleUpReason(t *testing.T) {
	alg := Predictive{
		TokenThroughput:               TokenThroughput{ServerTargetTokensPerSecond: 1000},
		ServerTokenCapacityPerReplica: 100,
		HorizonSeconds:                300,
	}
	state := &PredictorState{SampleCount: 6, Level: 1000, Trend: 0, Covariance: [4]float64{1, 0, 0, 1}}
	obs := freshObservation(time.Unix(0, 0))
	obs.Kind = RoleKindServer
	obs.CurrentReplicas = 2
	obs.ReadyReplicas = 2
	obs.PromptTokensPerSecond = 1000
	got, _ := alg.Recommend(obs, state)
	if got.DesiredReplicas != 10 || got.Reason != ReasonScaleUp {
		t.Fatalf("recommendation = %#v, want 10 replicas and reason %s", got, ReasonScaleUp)
	}
}

func TestRatioCoupledRolesAndConsolidationFloor(t *testing.T) {
	obs := freshObservation(time.Unix(10, 0))
	obs.Kind = RoleKindEncode
	obs.CurrentReplicas = 3
	obs.ReadyReplicas = 3

	tt := TokenThroughput{ServerTargetTokensPerSecond: 100}
	if rec, _ := tt.Recommend(obs, nil); rec.DesiredReplicas != 3 || rec.Reason != "RatioCoupled" {
		t.Fatalf("tokenThroughput encode = %#v", rec)
	}
	pred := Predictive{TokenThroughput: tt, ServerTokenCapacityPerReplica: 100}
	if rec, _ := pred.Recommend(obs, nil); rec.DesiredReplicas != 3 || rec.Reason != "RatioCoupled" {
		t.Fatalf("predictive encode = %#v", rec)
	}

	propPartial := NewProportional(MetricRunningRequests, 10, 0.05)
	obsPartial := freshObservation(time.Unix(10, 0))
	obsPartial.Role = "server"
	obsPartial.Kind = RoleKindServer
	obsPartial.CurrentReplicas = 4
	obsPartial.ReadyReplicas = 1
	obsPartial.RunningRequests = 15
	if rec, _ := propPartial.Recommend(obsPartial, nil); rec.DesiredReplicas != 4 {
		t.Fatalf("proportional partial readiness clamp = %#v, want 4", rec)
	}
}

func TestPredictorStateCopyIsolationAndSignalUsability(t *testing.T) {
	if signalUsable("") {
		t.Fatalf("empty SignalState must not be usable")
	}
	if !signalUsable(SignalFresh) || !signalUsable(SignalIdle) {
		t.Fatalf("fresh and idle states must be usable")
	}

	tt := TokenThroughput{ServerTargetTokensPerSecond: 100}
	priorHistory := NewDemandHistory()
	priorHistory.Push(DemandSample{Timestamp: time.Unix(10, 0), TokensPerSecond: 50})
	prior := &PredictorState{SampleCount: 1, History: priorHistory}

	obs := freshObservation(time.Unix(20, 0))
	obs.Kind = RoleKindServer
	obs.CurrentReplicas = 2
	obs.ReadyReplicas = 2
	obs.GenerationTokensPerSecond = 80
	obs.ThroughputState = SignalFresh

	_, next := tt.Recommend(obs, prior)
	if prior.History.Len() != 1 {
		t.Fatalf("prior history mutated in place: len = %d, want 1", prior.History.Len())
	}
	if next == nil || next.History.Len() != 2 {
		t.Fatalf("next history len = %v, want 2", next)
	}
}
