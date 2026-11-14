package algorithm

import (
	"math"
	"time"

	"gpustack.ai/gpustack/pkg/utils/datax"
)

// Predictive combines TokenThroughput with a local linear trend predictor.
type Predictive struct {
	TokenThroughput
	HorizonSeconds                 float64
	PrefillTokenCapacityPerReplica float64
	DecodeTokenCapacityPerReplica  float64
	ServerTokenCapacityPerReplica  float64
	QLevel                         float64
	QTrend                         float64
	R                              float64
	MinPoints                      int32
}

// NewPredictive constructs a predictive algorithm.
func NewPredictive(throughput TokenThroughput) Predictive {
	return Predictive{TokenThroughput: throughput}
}

// Name returns the algorithm name.
func (p Predictive) Name() string { return "Predictive" }

// Recommend updates the predictor and returns a demand and latency recommendation.
func (p Predictive) Recommend(obs RoleObservation, prior *PredictorState) (RoleRecommendation, *PredictorState) {
	rec, statePtr := p.TokenThroughput.Recommend(obs, prior)
	if statePtr == nil {
		statePtr = &PredictorState{}
	}
	state := *statePtr
	if roleKind(obs) == RoleKindEncode || obs.ReadyReplicas <= 0 {
		return rec, &state
	}
	if target := p.target(roleKind(obs)); target <= 0 || !isFiniteFloat64(target) {
		return rec, &state
	}
	if !signalUsable(obs.ThroughputState) || !p.demandFinite(obs) {
		if rec.Reason != ReasonLatencyGuardScaleUp && rec.Reason != ReasonLatencyGuardHold {
			switch latencyRec, latencyReason, _ := p.latencyGuard(obs); latencyReason {
			case ReasonLatencyGuardScaleUp:
				rec.DesiredReplicas = maxInt32(1, latencyRec)
				rec.Reason = ReasonLatencyGuardScaleUp
				rec.HoldScaleDown = true
			case ReasonLatencyGuardHold:
				rec.DesiredReplicas = obs.CurrentReplicas
				rec.Reason = ReasonLatencyGuardHold
				rec.HoldScaleDown = true
			}
		}
		return rec, &state
	}
	state = p.updateFilter(state, obs)
	minimum := p.MinPoints
	if minimum <= 0 {
		minimum = 6
	}
	if state.SampleCount < minimum {
		// A RECOMMENDATION THAT ASKS FOR MORE IS NOT OVERWRITTEN BY THE WARMUP, because the
		// reason of a scale-up says more about the load than the state of the predictor does.
		if !rec.InPanic && rec.Reason != ReasonPanic && rec.DesiredReplicas <= obs.CurrentReplicas {
			rec.Reason = ReasonPredictorWarmingUp
			rec.HoldScaleDown = true
		}
		return rec, &state
	}
	capacity := p.capacity(roleKind(obs))
	if capacity <= 0 || math.IsNaN(capacity) || math.IsInf(capacity, 0) {
		return rec, &state
	}
	forecast := p.forecast(state)
	if math.IsNaN(forecast) || math.IsInf(forecast, 0) {
		return rec, &state
	}
	slowFloor := clampPositiveCeilInt32(forecast / capacity)
	rec.DesiredReplicas = maxInt32(rec.DesiredReplicas, slowFloor)

	latencyFast := p.latencyBreached(obs)
	if latencyFast || rec.InPanic {
		rec.DesiredReplicas = maxInt32(rec.DesiredReplicas, saturatingIncInt32(obs.CurrentReplicas))
		rec.HoldScaleDown = true
		if latencyFast {
			rec.Reason = ReasonLatencyGuardScaleUp
		}
	}
	// A RECOMMENDATION THAT ASKS FOR MORE SAYS SO, because the reason of the recommendation
	// below is about a count that is no longer the one the recommendation carries.
	if rec.DesiredReplicas > obs.CurrentReplicas &&
		rec.Reason != ReasonPanic && rec.Reason != ReasonLatencyGuardScaleUp {
		rec.Reason = ReasonScaleUp
	}
	if rec.DesiredReplicas < obs.CurrentReplicas {
		currentDemand := p.demand(obs)
		canConsolidate := slowFloor < obs.CurrentReplicas && !rec.HoldScaleDown && !rec.InPanic && obs.CurrentReplicas > 1
		targetReplicas := maxInt32(obs.CurrentReplicas-1, slowFloor)
		if targetReplicas < 1 {
			targetReplicas = 1
		}
		if canConsolidate {
			perReplicaAfter := currentDemand / float64(targetReplicas)
			canConsolidate = perReplicaAfter <= 0.8*capacity
		}
		if canConsolidate {
			rec.DesiredReplicas = targetReplicas
		} else {
			rec.DesiredReplicas = obs.CurrentReplicas
			rec.HoldScaleDown = true
			rec.Reason = ReasonPredictiveConsolidationHold
		}
	}
	clampPartialCoverage(&rec, obs)
	return rec, &state
}

func (p Predictive) updateFilter(state PredictorState, obs RoleObservation) PredictorState {
	demand := p.demand(obs)
	if !isFiniteFloat64(demand) {
		return state
	}
	if state.SampleCount == 0 {
		state.Level = demand
		state.Trend = 0
		if state.Covariance == [4]float64{} || !isFiniteCovariance(state.Covariance) {
			state.Covariance = [4]float64{1, 0, 0, 1}
		}
		state.SampleCount = 1
		return state
	}
	if !isFiniteFloat64(state.Level) || !isFiniteFloat64(state.Trend) || !isFiniteCovariance(state.Covariance) {
		return state
	}
	dt := 15.0
	if previous, ok := previousDemandSample(state.History); ok {
		if !previous.Timestamp.IsZero() && !obs.ObservedAt.IsZero() {
			if delta := obs.ObservedAt.Sub(previous.Timestamp).Seconds(); delta > 0 {
				dt = delta
			}
		}
	}
	p00, p01, p10, p11 := state.Covariance[0], state.Covariance[1], state.Covariance[2], state.Covariance[3]
	if p00 == 0 && p01 == 0 && p10 == 0 && p11 == 0 {
		p00, p11 = 1, 1
	}
	predictedLevel := state.Level + dt*state.Trend
	predictedTrend := state.Trend
	qLevel, qTrend, r := p.QLevel, p.QTrend, p.R
	if qLevel <= 0 {
		qLevel = 1
	}
	if qTrend <= 0 {
		qTrend = 0.1
	}
	if r <= 0 {
		r = 10
	}
	pred00 := p00 + dt*(p10+p01) + dt*dt*p11 + qLevel
	pred01 := p01 + dt*p11
	pred10 := p10 + dt*p11
	pred11 := p11 + qTrend
	innovation := demand - predictedLevel
	innovationVariance := pred00 + r
	if innovationVariance <= 0 {
		innovationVariance = r
	}
	gain0, gain1 := pred00/innovationVariance, pred10/innovationVariance
	nextLevel := predictedLevel + gain0*innovation
	nextTrend := predictedTrend + gain1*innovation
	nextCov := [4]float64{
		(1 - gain0) * pred00,
		(1 - gain0) * pred01,
		pred10 - gain1*pred00,
		pred11 - gain1*pred01,
	}
	if !isFiniteFloat64(nextLevel) || !isFiniteFloat64(nextTrend) || !isFiniteCovariance(nextCov) {
		return state
	}
	state.Level = nextLevel
	state.Trend = nextTrend
	state.Covariance = nextCov
	state.SampleCount++
	return state
}

func isFiniteCovariance(cov [4]float64) bool {
	return isFiniteFloat64(cov[0]) && isFiniteFloat64(cov[1]) && isFiniteFloat64(cov[2]) && isFiniteFloat64(cov[3])
}

// previousDemandSample returns the sample the newest one was measured against.
//
// IT IS THE SECOND NEWEST SAMPLE OF THE HISTORY, because the newest one is the sample of
// the tick being filtered and the one before it says how much time passed since the last
// measurement.
func previousDemandSample(history *datax.RingBuffer[DemandSample]) (DemandSample, bool) {
	if history == nil || history.Len() < 2 {
		return DemandSample{}, false
	}
	return history.At(history.Len() - 2)
}

func (p Predictive) forecast(state PredictorState) float64 {
	horizon := time.Duration(p.HorizonSeconds * float64(time.Second))
	if horizon <= 0 {
		horizon = 300 * time.Second
	}
	return math.Max(0, state.Level+state.Trend*horizon.Seconds())
}

func (p Predictive) capacity(kind RoleKind) float64 {
	switch kind {
	case RoleKindPrefill:
		return p.PrefillTokenCapacityPerReplica
	case RoleKindDecode:
		return p.DecodeTokenCapacityPerReplica
	case RoleKindServer:
		return p.ServerTokenCapacityPerReplica
	default:
		return 0
	}
}

// latencyBreached reports whether a latency the role is held to is beyond its target.
//
// IT HOLDS EVERY TARGET ON ITS OWN, because a target whose latency could not be read does not
// make the target that could be read unreadable.
func (p Predictive) latencyBreached(obs RoleObservation) bool {
	if obs.LatencyState == SignalIdle || !signalUsable(obs.LatencyState) {
		return false
	}
	ratio, _ := p.latencyAssessment(obs)
	return ratio > 1.2
}
