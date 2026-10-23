package algorithm

import "math"

// ProportionalMetric selects the observation used by Proportional.
type ProportionalMetric string

const (
	MetricRunningRequests           ProportionalMetric = "RunningRequests"
	MetricWaitingRequests           ProportionalMetric = "WaitingRequests"
	MetricKVCacheUsage              ProportionalMetric = "KVCacheUsage"
	MetricPromptTokensPerSecond     ProportionalMetric = "PromptTokensPerSecond"
	MetricGenerationTokensPerSecond ProportionalMetric = "GenerationTokensPerSecond"
	MetricQueueDepth                ProportionalMetric = "QueueDepth"
	MetricTotalTokensPerSecond      ProportionalMetric = "TotalTokensPerSecond"
)

// Metric is a short alias for ProportionalMetric.
type Metric = ProportionalMetric

// Proportional scales from a metric target with a tolerance deadband.
type Proportional struct {
	Metric      ProportionalMetric
	TargetValue float64
	Tolerance   float64
}

// NewProportional constructs a proportional algorithm.
func NewProportional(metric ProportionalMetric, targetValue, tolerance float64) Proportional {
	return Proportional{Metric: metric, TargetValue: targetValue, Tolerance: tolerance}
}

// Name returns the algorithm name.
func (p Proportional) Name() string { return "Proportional" }

// Recommend returns a replica recommendation based on the configured metric.
func (p Proportional) Recommend(obs RoleObservation, _ *PredictorState) (RoleRecommendation, *PredictorState) {
	rec := RoleRecommendation{DesiredReplicas: obs.CurrentReplicas}
	if obs.ReadyReplicas <= 0 {
		rec.HoldScaleDown = true
		rec.Reason = ReasonNoReadyReplicas
		return rec, nil
	}
	target := p.TargetValue
	if target <= 0 || math.IsNaN(target) || math.IsInf(target, 0) {
		rec.HoldScaleDown = true
		rec.Reason = ReasonWaitingForMetrics
		return rec, nil
	}
	value, state, perReplica := p.value(obs)
	if !signalUsable(state) {
		rec.HoldScaleDown = true
		rec.Reason = ReasonWaitingForMetrics
		return rec, nil
	}
	if !perReplica {
		value /= float64(obs.ReadyReplicas)
	}
	ratio := value / target
	tolerance := p.Tolerance
	if tolerance <= 0 {
		tolerance = 0.10
	}
	if math.Abs(ratio-1) <= tolerance {
		rec.Reason = ReasonWithinTolerance
		return rec, nil
	}
	if ratio <= 0 {
		if obs.GaugeState == SignalIdle {
			rec.DesiredReplicas = 1
		} else {
			// A READING OF NOTHING THAT IS NOT IDLE IS NO READING AT ALL, so the
			// recommendation holds the count and says it waits for metrics.
			rec.DesiredReplicas = obs.CurrentReplicas
			rec.HoldScaleDown = true
			rec.Reason = ReasonWaitingForMetrics
			clampPartialCoverage(&rec, obs)
			return rec, nil
		}
	} else {
		rec.DesiredReplicas = int32(math.Ceil(float64(obs.ReadyReplicas) * ratio))
		if ratio > 1 && rec.DesiredReplicas < obs.CurrentReplicas {
			rec.DesiredReplicas = obs.CurrentReplicas
		}
		if ratio < 1 && rec.DesiredReplicas < obs.CurrentReplicas && obs.ReadyReplicas < obs.CurrentReplicas {
			rec.DesiredReplicas = obs.CurrentReplicas
			rec.HoldScaleDown = true
		}
		if rec.DesiredReplicas < 1 {
			rec.DesiredReplicas = 1
		}
	}
	switch {
	case rec.DesiredReplicas > obs.CurrentReplicas:
		rec.Reason = ReasonScaleUp
	case rec.DesiredReplicas < obs.CurrentReplicas:
		rec.Reason = ReasonScaleDown
	default:
		rec.Reason = ReasonWithinTolerance
	}
	clampPartialCoverage(&rec, obs)
	return rec, nil
}

func (p Proportional) value(obs RoleObservation) (float64, SignalState, bool) {
	state := obs.GaugeState
	switch p.Metric {
	case MetricWaitingRequests:
		return obs.WaitingRequests, state, false
	case MetricQueueDepth:
		return obs.RunningRequests + obs.WaitingRequests, state, false
	case MetricKVCacheUsage:
		return obs.KVCacheUsage, state, true
	case MetricPromptTokensPerSecond:
		return obs.PromptTokensPerSecond, obs.ThroughputState, false
	case MetricGenerationTokensPerSecond:
		return obs.GenerationTokensPerSecond, obs.ThroughputState, false
	case MetricTotalTokensPerSecond:
		return obs.PromptTokensPerSecond + obs.GenerationTokensPerSecond, obs.ThroughputState, false
	case MetricRunningRequests, "":
		return obs.RunningRequests, state, false
	default:
		return 0, SignalMissing, false
	}
}
