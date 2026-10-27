package sim

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

// TokenTrace is one request's exact whole-token truth from the opt-in
// integer-token recording mode.
//
// TTFT is the first generated token's completion minus arrival. Request TPOT is
// (last-first)/(N-1) for completed requests with more than one generated token
// and is otherwise undefined. Each true ITL is the difference of adjacent
// token timestamps, so the count is one fewer than the generated tokens.
// Failed requests keep their recorded tokens but expose no latency statistics.
type TokenTrace struct {
	RequestID  int
	ArrivedAt  float64
	FinishedAt float64
	Failed     bool
	tokens     []float64
}

// Tokens returns the exact completion times of the request's generated whole
// tokens in generation order.
func (t TokenTrace) Tokens() []float64 { return append([]float64(nil), t.tokens...) }

// TokenCount returns how many whole tokens the request generated.
func (t TokenTrace) TokenCount() int { return len(t.tokens) }

// TTFTSeconds returns the first generated token's latency; ok is false when
// the request failed or generated no whole token.
func (t TokenTrace) TTFTSeconds() (float64, bool) {
	if t.Failed || len(t.tokens) == 0 {
		return 0, false
	}
	return t.tokens[0] - t.ArrivedAt, true
}

// TPOTSeconds returns (last-first)/(N-1); ok is false for failed requests and
// requests with fewer than two generated tokens.
func (t TokenTrace) TPOTSeconds() (float64, bool) {
	if t.Failed || len(t.tokens) < 2 {
		return 0, false
	}
	return (t.tokens[len(t.tokens)-1] - t.tokens[0]) / float64(len(t.tokens)-1), true
}

// ITLSeconds returns the adjacent token timestamp differences in generation
// order (the true inter-token latencies). Failed requests report none.
func (t TokenTrace) ITLSeconds() []float64 {
	if t.Failed {
		return nil
	}
	gaps := make([]float64, 0, max(0, len(t.tokens)-1))
	for i := 1; i < len(t.tokens); i++ {
		gaps = append(gaps, t.tokens[i]-t.tokens[i-1])
	}
	return gaps
}

// TokenTruthRecorder collects whole-token events per request while the plant
// serves, and reconciles them with request outcomes afterwards.
type TokenTruthRecorder struct {
	events map[int][]float64
}

// NewTokenTruthRecorder returns an empty recorder.
func NewTokenTruthRecorder() *TokenTruthRecorder {
	return &TokenTruthRecorder{events: map[int][]float64{}}
}

// Recorder is the plant token observer.
func (r *TokenTruthRecorder) Recorder() func(requestID, replicaOrdinal, tokenIndex int, at float64) {
	return func(requestID, replicaOrdinal, tokenIndex int, at float64) {
		r.events[requestID] = append(r.events[requestID], at)
	}
}

// Track reconciles the recorded token events with the plant's request
// outcomes, in outcome order. Requests still outstanding at Track time have
// no outcome and no trace, so trace count is not an offered denominator;
// Summary reports Offered and Outstanding for that.
func (r *TokenTruthRecorder) Track(results []RequestResult) []TokenTrace {
	traces := make([]TokenTrace, 0, len(results))
	for _, result := range results {
		trace := TokenTrace{
			RequestID: result.ID, ArrivedAt: result.ArrivedAt,
			FinishedAt: result.FinishedAt, Failed: result.Failed,
			tokens: slices.Clone(r.events[result.ID]),
		}
		traces = append(traces, trace)
	}
	return traces
}

// QuantizeRequest rounds a synthetic request's token counts to whole tokens,
// keeping the arrival and cache fraction unchanged.
//
// This is the opt-in fixture's quantization: the jittered traffic otherwise
// offers fractional token work that no integer-token comparison can
// reproduce deterministically.
func QuantizeRequest(request Request) Request {
	request.InputTokens = math.Round(request.InputTokens)
	request.OutputTokens = math.Round(request.OutputTokens)
	return request
}

// QuantizeArrivals rounds every arrival's token counts to whole tokens.
func QuantizeArrivals(arrivals []Request) []Request {
	quantized := make([]Request, 0, len(arrivals))
	for _, request := range arrivals {
		quantized = append(quantized, QuantizeRequest(request))
	}
	return quantized
}

// ConservationCheck verifies a quantized arrival set preserves the raw set
// request-for-request: same count and order, identical IDs, arrival times and
// cache fractions, and token counts equal to the per-request rounded values.
// Per-request rounding does not conserve raw fractional totals, so totals are
// deliberately not compared against the raw set.
func ConservationCheck(raw, quantized []Request) error {
	if len(raw) != len(quantized) {
		return fmt.Errorf("quantized set has %d arrivals, raw has %d", len(quantized), len(raw))
	}
	for i := range raw {
		want := QuantizeRequest(raw[i])
		if quantized[i] != want {
			return fmt.Errorf("arrival %d (request %d) drifted under quantization: got %+v, want %+v", i, raw[i].ID, quantized[i], want)
		}
	}
	return nil
}

// SortTraces orders traces by request ID for deterministic comparisons.
func SortTraces(traces []TokenTrace) {
	sort.Slice(traces, func(i, j int) bool { return traces[i].RequestID < traces[j].RequestID })
}
