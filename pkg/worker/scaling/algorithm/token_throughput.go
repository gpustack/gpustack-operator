package algorithm

import (
	"math"
	"time"

	"gpustack.ai/gpustack/pkg/utils/datax"
)

// TokenThroughput uses stable and panic token demand windows for scaling.
type TokenThroughput struct {
	PrefillTargetTokensPerSecond float64
	DecodeTargetTokensPerSecond  float64
	ServerTargetTokensPerSecond  float64
	StableWindowSeconds          float64
	PanicWindowSeconds           float64
	PanicThreshold               float64
	TTFTTargetMilliseconds       float64
	ITLTargetMilliseconds        float64
}

// NewTokenThroughput constructs a token throughput algorithm.
func NewTokenThroughput() TokenThroughput { return TokenThroughput{} }

// Name returns the algorithm name.
func (t TokenThroughput) Name() string { return "TokenThroughput" }

// Recommend returns a replica recommendation based on role token demand.
func (t TokenThroughput) Recommend(obs RoleObservation, prior *PredictorState) (RoleRecommendation, *PredictorState) {
	state := copyState(prior)
	rec := RoleRecommendation{DesiredReplicas: obs.CurrentReplicas}
	if roleKind(obs) == RoleKindEncode {
		rec.Reason = ReasonRatioCoupled
		return rec, &state
	}
	if obs.ReadyReplicas <= 0 {
		rec.HoldScaleDown = true
		rec.Reason = ReasonNoReadyReplicas
		return rec, &state
	}
	target := t.target(roleKind(obs))
	if target <= 0 || math.IsNaN(target) || math.IsInf(target, 0) {
		rec.HoldScaleDown = true
		rec.Reason = ReasonWaitingForMetrics
		return rec, &state
	}
	if !signalUsable(obs.ThroughputState) {
		rec.HoldScaleDown = true
		rec.Reason = ReasonWaitingForMetrics
		return rec, &state
	}

	demand := t.demand(obs)
	appendDemand(&state, DemandSample{Timestamp: obs.ObservedAt, TokensPerSecond: demand}, t.maxWindow())
	stableWindow := t.stableWindow()
	panicWindow := t.panicWindow()
	stableTPS := windowMean(state.History, obs.ObservedAt, stableWindow, demand)
	panicTPS := windowMean(state.History, obs.ObservedAt, panicWindow, demand)
	stableDesired := desiredFromDemand(stableTPS, target)
	panicDesired := desiredFromDemand(panicTPS, target)
	ready := math.Max(1, float64(obs.ReadyReplicas))
	threshold := t.PanicThreshold
	if threshold <= 0 {
		threshold = 2
	}
	panicRatio := panicTPS / (ready * target)
	panicActive := false
	switch {
	case panicRatio >= threshold:
		panicActive = true
		state.PanicExitAt = obs.ObservedAt.Add(stableWindow)
	case !state.PanicExitAt.IsZero() && !obs.ObservedAt.After(state.PanicExitAt):
		panicActive = true
	default:
		state.PanicExitAt = time.Time{}
	}

	desired := stableDesired
	if panicActive {
		desired = maxInt32(stableDesired, panicDesired)
		rec.InPanic = true
		rec.HoldScaleDown = true
		rec.Reason = ReasonPanic
	} else {
		rec.Reason = ReasonTokenThroughput
	}
	rec.DesiredReplicas = desired
	if rec.DesiredReplicas > obs.CurrentReplicas {
		rec.Reason = ReasonScaleUp
	} else if rec.DesiredReplicas < obs.CurrentReplicas {
		rec.Reason = ReasonScaleDown
	}
	if panicActive {
		rec.Reason = ReasonPanic
	}

	latencyRec, latencyReason, latencyHold := t.latencyGuard(obs)
	if latencyReason == ReasonWaitingForMetrics {
		hasMaturedHistory := false
		if state.History != nil {
			if first, ok := state.History.First(); ok && !first.Timestamp.IsZero() {
				hasMaturedHistory = obs.ObservedAt.Sub(first.Timestamp) >= stableWindow/2
			}
		}
		if !hasMaturedHistory {
			rec.HoldScaleDown = true
			if rec.DesiredReplicas <= obs.CurrentReplicas {
				rec.DesiredReplicas = obs.CurrentReplicas
				if !panicActive {
					rec.Reason = latencyReason
				}
			}
		}
	} else if latencyReason != "" {
		rec.DesiredReplicas = maxInt32(rec.DesiredReplicas, latencyRec)
		rec.HoldScaleDown = rec.HoldScaleDown || latencyHold
		rec.Reason = latencyReason
	}
	if rec.DesiredReplicas < 1 {
		rec.DesiredReplicas = 1
	}
	clampPartialCoverage(&rec, obs)
	return rec, &state
}

func (t TokenThroughput) target(kind RoleKind) float64 {
	switch kind {
	case RoleKindPrefill:
		return t.PrefillTargetTokensPerSecond
	case RoleKindDecode:
		return t.DecodeTargetTokensPerSecond
	case RoleKindUnknown:
		return 0
	default:
		return t.ServerTargetTokensPerSecond
	}
}

func (t TokenThroughput) demand(obs RoleObservation) float64 {
	switch roleKind(obs) {
	case RoleKindPrefill:
		return nonNegative(obs.PromptTokensPerSecond)
	case RoleKindDecode:
		return nonNegative(obs.GenerationTokensPerSecond)
	case RoleKindServer:
		return nonNegative(obs.PromptTokensPerSecond) + nonNegative(obs.GenerationTokensPerSecond)
	default:
		return 0
	}
}

func nonNegative(value float64) float64 {
	if value < 0 || math.IsNaN(value) {
		return 0
	}
	return value
}

func desiredFromDemand(demand, target float64) int32 {
	if demand <= 0 {
		return 1
	}
	return int32(math.Max(1, math.Ceil(demand/target)))
}

func (t TokenThroughput) stableWindow() time.Duration {
	if t.StableWindowSeconds > 0 {
		return time.Duration(t.StableWindowSeconds * float64(time.Second))
	}
	return 180 * time.Second
}

func (t TokenThroughput) panicWindow() time.Duration {
	if t.PanicWindowSeconds > 0 {
		return time.Duration(t.PanicWindowSeconds * float64(time.Second))
	}
	return 60 * time.Second
}

func (t TokenThroughput) maxWindow() time.Duration {
	stable, panicWindow := t.stableWindow(), t.panicWindow()
	if stable > panicWindow {
		return stable
	}
	return panicWindow
}

const (
	defaultDemandHistoryCap = 32
	maxDemandHistoryCap     = 256
	minSamplingInterval     = 5 * time.Second
)

func demandHistoryCap(maxWindow time.Duration) int {
	limit := defaultDemandHistoryCap
	if maxWindow > 0 {
		windowSamples := int(math.Ceil(float64(maxWindow)/float64(minSamplingInterval))) + 1
		if windowSamples > limit {
			limit = windowSamples
		}
	}
	if limit > maxDemandHistoryCap {
		limit = maxDemandHistoryCap
	}
	return limit
}

func appendDemand(state *PredictorState, sample DemandSample, maxWindow time.Duration) {
	if sample.Timestamp.IsZero() {
		return
	}
	if state.History == nil {
		state.History = NewDemandHistory()
	}
	state.History.Push(sample)
	if maxWindow > 0 {
		cutoff := sample.Timestamp.Add(-maxWindow)
		state.History.DropWhile(func(entry DemandSample) bool {
			return entry.Timestamp.IsZero() || entry.Timestamp.Before(cutoff)
		})
	}
	trimDemandHistory(state.History, demandHistoryCap(maxWindow))
}

// trimDemandHistory keeps the newest samples of a demand history.
//
// IT IS THE COUNT BOUND OF THE HISTORY, because the window prunes by time and this prunes
// by count: a sampler that ticks faster than the window expects still holds a bounded
// number of samples.
func trimDemandHistory(history *datax.RingBuffer[DemandSample], limit int) {
	excess := history.Len() - limit
	if excess <= 0 {
		return
	}
	dropped := 0
	history.DropWhile(func(DemandSample) bool {
		if dropped >= excess {
			return false
		}
		dropped++
		return true
	})
}

func windowMean(history *datax.RingBuffer[DemandSample], now time.Time, window time.Duration, fallback float64) float64 {
	if history == nil || history.Len() == 0 || now.IsZero() {
		return fallback
	}
	cutoff := now.Add(-window)
	var sum float64
	var count int
	for i := 0; i < history.Len(); i++ {
		sample, ok := history.At(i)
		if !ok {
			break
		}
		if !sample.Timestamp.IsZero() && !sample.Timestamp.Before(cutoff) && !sample.Timestamp.After(now) {
			sum += sample.TokensPerSecond
			count++
		}
	}
	if count == 0 {
		return fallback
	}
	return sum / float64(count)
}

func (t TokenThroughput) latencyGuard(obs RoleObservation) (int32, string, bool) {
	if obs.LatencyState == SignalIdle || !t.latencyConfigured(obs) {
		return obs.CurrentReplicas, "", false
	}
	if !signalUsable(obs.LatencyState) {
		return obs.CurrentReplicas, ReasonWaitingForMetrics, true
	}
	target, latency := t.latencyTargetAndValue(obs)
	if math.IsNaN(latency) || math.IsInf(latency, 0) {
		return obs.CurrentReplicas, ReasonWaitingForMetrics, true
	}
	if target <= 0 {
		return obs.CurrentReplicas, "", false
	}
	ratio := latency / target
	if ratio > 1.2 {
		return obs.CurrentReplicas + 1, ReasonLatencyGuardScaleUp, true
	}
	if ratio > 1 {
		return obs.CurrentReplicas, ReasonLatencyGuardHold, true
	}
	return obs.CurrentReplicas, "", false
}

func (t TokenThroughput) latencyConfigured(obs RoleObservation) bool {
	switch roleKind(obs) {
	case RoleKindPrefill:
		return t.TTFTTargetMilliseconds > 0
	case RoleKindDecode:
		return t.ITLTargetMilliseconds > 0
	default:
		return t.TTFTTargetMilliseconds > 0 || t.ITLTargetMilliseconds > 0
	}
}

func (t TokenThroughput) latencyTargetAndValue(obs RoleObservation) (float64, float64) {
	kind := roleKind(obs)
	if kind == RoleKindPrefill {
		if math.IsNaN(obs.TTFTMilliseconds) || math.IsInf(obs.TTFTMilliseconds, 0) {
			return 0, math.NaN()
		}
		return t.TTFTTargetMilliseconds, obs.TTFTMilliseconds
	}
	if kind == RoleKindDecode {
		if math.IsNaN(obs.ITLMilliseconds) || math.IsInf(obs.ITLMilliseconds, 0) {
			return 0, math.NaN()
		}
		return t.ITLTargetMilliseconds, obs.ITLMilliseconds
	}
	var targets []float64
	var values []float64
	if t.TTFTTargetMilliseconds > 0 {
		targets = append(targets, t.TTFTTargetMilliseconds)
		values = append(values, obs.TTFTMilliseconds)
	}
	if t.ITLTargetMilliseconds > 0 {
		targets = append(targets, t.ITLTargetMilliseconds)
		values = append(values, obs.ITLMilliseconds)
	}
	if len(targets) == 0 {
		return 0, 0
	}
	maxRatio := 0.0
	for i := range targets {
		if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
			return 0, math.NaN()
		}
		maxRatio = math.Max(maxRatio, values[i]/targets[i])
	}
	return 1, maxRatio
}
