// Package algorithm contains pure scaling recommendation algorithms.
package algorithm

import (
	"time"

	"gpustack.ai/gpustack/pkg/utils/datax"
)

// RoleKind identifies the serving role represented by an observation.
type RoleKind string

const (
	RoleKindServer  RoleKind = "Server"
	RoleKindPrefill RoleKind = "Prefill"
	RoleKindDecode  RoleKind = "Decode"
	RoleKindEncode  RoleKind = "Encode"
	// RoleKindUnknown says the role is one the algorithms do not know.
	//
	// IT IS NOT A DEFAULT TO SERVER, because a role whose kind was not named is not a server:
	// treating it as one would scale it on a target it never declared.
	RoleKindUnknown RoleKind = "Unknown"

	ServerRole  = RoleKindServer
	PrefillRole = RoleKindPrefill
	DecodeRole  = RoleKindDecode
	EncodeRole  = RoleKindEncode
)

// SignalState describes the quality of one sampled signal.
type SignalState string

const (
	SignalFresh   SignalState = "Fresh"
	SignalIdle    SignalState = "Idle"
	SignalMissing SignalState = "Missing"
	SignalStale   SignalState = "Stale"
)

// The reasons an algorithm recommendation carries.
const (
	ReasonWaitingForMetrics           = "WaitingForMetrics"
	ReasonRatioCoupled                = "RatioCoupled"
	ReasonPredictorWarmingUp          = "PredictorWarmingUp"
	ReasonPredictiveConsolidationHold = "PredictiveConsolidationHold"
	ReasonLatencyGuardScaleUp         = "LatencyGuardScaleUp"
	ReasonLatencyGuardHold            = "LatencyGuardHold"
	ReasonTokenThroughput             = "TokenThroughput"
	ReasonNoReadyReplicas             = "NoReadyReplicas"
	ReasonWithinTolerance             = "WithinTolerance"
	ReasonScaleUp                     = "ScaleUp"
	ReasonScaleDown                   = "ScaleDown"
	ReasonPanic                       = "Panic"
)

// RoleObservation contains the values sampled for one role at one instant.
type RoleObservation struct {
	Role                      string
	Kind                      RoleKind
	CurrentReplicas           int32
	ReadyReplicas             int32
	PromptTokensPerSecond     float64
	GenerationTokensPerSecond float64
	RunningRequests           float64
	WaitingRequests           float64
	KVCacheUsage              float64
	TTFTMilliseconds          float64
	ITLMilliseconds           float64
	ThroughputState           SignalState
	LatencyState              SignalState
	GaugeState                SignalState
	CompleteCoverage          bool
	ObservedAt                time.Time
}

// RoleRecommendation is an unconstrained recommendation for one role.
type RoleRecommendation struct {
	DesiredReplicas int32
	Reason          string
	HoldScaleDown   bool
	InPanic         bool
}

// DemandSample is a timestamped role demand sample.
type DemandSample struct {
	Timestamp       time.Time
	TokensPerSecond float64
}

// PredictorState is the state carried between recommendations for one role.
type PredictorState struct {
	Level       float64
	Trend       float64
	Covariance  [4]float64
	SampleCount int32
	// History is the demand history of the role.
	//
	// IT IS A RING BUFFER, because the history grows by one sample on every tick and is
	// read as a window: a slice would be reallocated on every tick to hold the same window.
	History     *datax.RingBuffer[DemandSample]
	PanicExitAt time.Time
}

// NewDemandHistory returns an empty demand history.
//
// IT IS THE HISTORY A PREDICTOR STATE KEEPS, and it holds as many samples as the longest
// window an algorithm may ask for.
func NewDemandHistory() *datax.RingBuffer[DemandSample] {
	return datax.NewRingBuffer[DemandSample](maxDemandHistoryCap)
}

// Algorithm recommends a role replica count from a sampled observation.
type Algorithm interface {
	Name() string
	Recommend(obs RoleObservation, prior *PredictorState) (RoleRecommendation, *PredictorState)
}

// copyDemandHistory copies a demand history into a fresh ring buffer.
func copyDemandHistory(source *datax.RingBuffer[DemandSample]) *datax.RingBuffer[DemandSample] {
	if source == nil {
		return NewDemandHistory()
	}
	return source.Clone()
}

// copyState returns the state a recommendation starts from.
//
// THE DEMAND HISTORY IS COPIED FROM THE PRIOR, because a recommendation appends its sample
// to the history before returning. Copying the history leaves the prior held by the sampler
// untouched if the reconcile fails before the new state is recorded.
func copyState(prior *PredictorState) PredictorState {
	if prior == nil {
		return PredictorState{
			History: NewDemandHistory(),
		}
	}
	state := *prior
	state.History = copyDemandHistory(prior.History)
	return state
}

// signalUsable reports whether a signal is fresh or idle; missing, stale, empty, and unrecognized
// states are treated conservatively as unavailable.
func signalUsable(state SignalState) bool {
	return state == SignalFresh || state == SignalIdle
}

func roleKind(obs RoleObservation) RoleKind {
	if obs.Kind != "" {
		return obs.Kind
	}
	switch obs.Role {
	case "server", "Server":
		return RoleKindServer
	case "prefill", "Prefill":
		return RoleKindPrefill
	case "decode", "Decode":
		return RoleKindDecode
	case "encode", "Encode":
		return RoleKindEncode
	default:
		return RoleKindUnknown
	}
}

func clampPartialCoverage(rec *RoleRecommendation, obs RoleObservation) {
	if obs.CompleteCoverage || rec.DesiredReplicas >= obs.CurrentReplicas {
		return
	}
	rec.DesiredReplicas = obs.CurrentReplicas
	rec.HoldScaleDown = true
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
