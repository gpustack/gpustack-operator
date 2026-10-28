package sim

import (
	"math"
	"testing"

	"gpustack.ai/gpustack/pkg/kubemeta"
)

// TestPlantTokenRecorderFinalToken locks the observer contract: every whole
// token is received, including the last one, with generation-order ordinals
// and exact interpolated completion times. It went red against the original
// gap-indexed emission, which dropped the final token of every request.
func TestPlantTokenRecorderFinalToken(t *testing.T) {
	for _, tc := range []struct {
		name          string
		input, output float64
		steps         int
		wantTimes     []float64
		wantFinish    float64
	}{
		{name: "prompt then steps", input: 1, output: 3, steps: 12, wantTimes: []float64{1, 1.5, 2}, wantFinish: 2},
		{name: "promptless uniform steps", output: 5, steps: 12, wantTimes: []float64{.5, 1, 1.5, 2, 2.5}, wantFinish: 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
			type tokenEvent struct {
				replica, ordinal int
				at               float64
			}
			var events []tokenEvent
			p.SetTokenRecorder(func(requestID, replicaOrdinal, tokenIndex int, at float64) {
				if requestID != 7 {
					t.Fatalf("event for request %d, want 7", requestID)
				}
				events = append(events, tokenEvent{replica: replicaOrdinal, ordinal: tokenIndex, at: at})
			})
			p.Offer(Request{ID: 7, InputTokens: tc.input, OutputTokens: tc.output})
			for tick := 0; tick < tc.steps; tick++ {
				p.Step(float64(tick)*.25, .25, []int{0})
			}
			results := p.Results()
			if len(results) != 1 || results[0].Failed || math.Abs(results[0].FinishedAt-tc.wantFinish) > 1e-9 {
				t.Fatalf("request outcome = %+v, want one completion at %.3f", results, tc.wantFinish)
			}
			if len(events) != len(tc.wantTimes) {
				t.Fatalf("observer saw %d token events for %d output tokens: %v", len(events), len(tc.wantTimes), events)
			}
			for i, want := range tc.wantTimes {
				if events[i].replica != 0 || events[i].ordinal != i+1 {
					t.Fatalf("event %d = replica %d ordinal %d, want replica 0 ordinal %d", i, events[i].replica, events[i].ordinal, i+1)
				}
				if diff := events[i].at - want; diff > 1e-9 || diff < -1e-9 {
					t.Fatalf("token %d completed at %.9f, want %.9f", i+1, events[i].at, want)
				}
			}
		})
	}
}

// TestPlantBoundaryFirstTokenMatchesTrace reproduces the boundary case where a
// slow rate accumulates the first whole token exactly onto a quantum boundary:
// at 0.1 tokens/s with 1s steps, the single output token completes at t=10 and
// the request result, the trace and the TTFT histogram must all agree. This is
// the opt-in recorded mode; the frozen default without a recorder keeps the
// first token unset, locked by TestPlantBoundaryFirstTokenFrozenDefault.
func TestPlantBoundaryFirstTokenMatchesTrace(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: .1, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
	rec := NewTokenTruthRecorder()
	p.SetTokenRecorder(rec.Recorder())
	p.Offer(Request{ID: 1, OutputTokens: 1})
	for tick := 0; tick < 10; tick++ {
		p.Step(float64(tick), 1, []int{0})
	}
	results := p.Results()
	traces := rec.Track(results)
	if len(results) != 1 || len(traces) != 1 {
		t.Fatalf("outcomes %+v, traces %+v, want one of each", results, traces)
	}
	tokens := traces[0].Tokens()
	if traces[0].TokenCount() != 1 || math.Abs(tokens[0]-10) > 1e-9 {
		t.Fatalf("trace tokens = %v, want [10]", tokens)
	}
	ttft, ok := traces[0].TTFTSeconds()
	if !ok || math.Abs(ttft-10) > 1e-9 {
		t.Fatalf("trace TTFT = (%.9f, %t), want (10, true)", ttft, ok)
	}
	if results[0].Failed {
		t.Fatalf("boundary completion marked failed: %+v", results[0])
	}
	if results[0].TTFTSeconds < 0 || math.Abs(results[0].TTFTSeconds-tokens[0]) > 1e-9 {
		t.Fatalf("request TTFT %.9f disagrees with trace token %.9f", results[0].TTFTSeconds, tokens[0])
	}
	if p.engines[0].ttft.count != 1 {
		t.Fatalf("ttft histogram count = %g, want 1", p.engines[0].ttft.count)
	}
	if _, ok := traces[0].TPOTSeconds(); ok {
		t.Fatal("single-token request reported TPOT")
	}
}

// TestPlantBoundaryFirstTokenFrozenDefault locks the frozen default boundary:
// without token recording, a first token that lands exactly on a quantum
// boundary after fractional accumulation still reports no first token, exactly
// as before the opt-in comparison mode existed.
func TestPlantBoundaryFirstTokenFrozenDefault(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: .1, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
	p.Offer(Request{ID: 1, OutputTokens: 1})
	for tick := 0; tick < 10; tick++ {
		p.Step(float64(tick), 1, []int{0})
	}
	results := p.Results()
	if len(results) != 1 || results[0].Failed {
		t.Fatalf("outcome = %+v, want one completion", results)
	}
	if results[0].TTFTSeconds != -1 || results[0].Compliant {
		t.Fatalf("frozen default boundary changed: %+v", results[0])
	}
	if p.engines[0].ttft.count != 0 {
		t.Fatalf("frozen default ttft histogram count = %g, want 0", p.engines[0].ttft.count)
	}
}

// TestTokenTraceITLSpansRateChangeAcrossQuanta locks true per-token ITL across
// a rate change: batch sharing halves the per-request rate in the second
// quantum, so the gap between tokens 1 and 2 spans both rates. The trace must
// reproduce the exact timestamps; the frozen per-step histogram approximation
// is documented, not fixed.
func TestTokenTraceITLSpansRateChangeAcrossQuanta(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 2, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
	rec := NewTokenTruthRecorder()
	p.SetTokenRecorder(rec.Recorder())
	p.Offer(Request{ID: 0, OutputTokens: 3})
	p.Offer(Request{ID: 1, OutputTokens: 1})
	p.Step(0, 1.5, []int{0})
	p.Step(1.5, .75, []int{0})
	traces := rec.Track(p.Results())
	SortTraces(traces)
	if len(traces) != 2 {
		t.Fatalf("tracked %+v, want two traces", traces)
	}
	a := traces[0]
	tokens := a.Tokens()
	if want := []float64{1, 1.75, 2.25}; a.TokenCount() != 3 || !kubemeta.DeepEqual(tokens, want) {
		t.Fatalf("request A tokens = %v, want %v", tokens, want)
	}
	itls := a.ITLSeconds()
	if len(itls) != 2 || math.Abs(itls[0]-.75) > 1e-9 || math.Abs(itls[1]-.5) > 1e-9 {
		t.Fatalf("true ITLs = %v, want [.75 .5]", itls)
	}
	if got := itls[0] + itls[1]; math.Abs(got-1.25) > 1e-9 {
		t.Fatalf("true ITL sum = %.9f, want 1.25", got)
	}
	tpot, ok := a.TPOTSeconds()
	if !ok || math.Abs(tpot-.625) > 1e-9 {
		t.Fatalf("request A TPOT = (%.9f, %t), want (.625, true)", tpot, ok)
	}
	// The frozen histogram charges whole gaps at the serving step's rate, so
	// the boundary-spanning gap is undercharged. This documents the frozen
	// approximation; true ITL statistics come from the trace.
	if p.engines[0].itl.count != 2 || math.Abs(p.engines[0].itl.sum-1) > 1e-9 {
		t.Fatalf("frozen approximation histogram = count %g sum %g, want count 2 sum 1", p.engines[0].itl.count, p.engines[0].itl.sum)
	}
	if traces[1].TokenCount() != 1 || traces[1].Tokens()[0] != 1 {
		t.Fatalf("request B tokens = %v, want [1]", traces[1].Tokens())
	}
	if _, ok := traces[1].TPOTSeconds(); ok {
		t.Fatal("single-token request reported TPOT")
	}
}

// TestTokenTraceWithholdsLatencyStatistics locks who gets latency statistics:
// failed requests keep their recorded tokens but expose no statistics, and
// outstanding requests have no outcome and therefore no trace, so trace count
// never serves as the offered denominator.
func TestTokenTraceWithholdsLatencyStatistics(t *testing.T) {
	t.Run("retained deletion fails with tokens kept", func(t *testing.T) {
		p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
		rec := NewTokenTruthRecorder()
		p.SetTokenRecorder(rec.Recorder())
		p.Offer(Request{ID: 2, OutputTokens: 4})
		for tick := 0; tick < 4; tick++ {
			p.Step(float64(tick)*.25, .25, []int{0})
		}
		p.RetainReplicas(1, nil)
		results := p.Results()
		if len(results) != 1 || !results[0].Failed {
			t.Fatalf("deletion outcome = %+v, want one failure", results)
		}
		traces := rec.Track(results)
		if len(traces) != 1 || traces[0].TokenCount() != 2 {
			t.Fatalf("trace kept %d tokens, want the two delivered before deletion", len(traces[0].Tokens()))
		}
		if _, ok := traces[0].TTFTSeconds(); ok {
			t.Fatal("failed request reported TTFT")
		}
		if _, ok := traces[0].TPOTSeconds(); ok {
			t.Fatal("failed request reported TPOT")
		}
		if len(traces[0].ITLSeconds()) != 0 {
			t.Fatal("failed request reported ITLs")
		}
	})
	t.Run("timeout before any token", func(t *testing.T) {
		p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: .6, TTFTSLOSeconds: 1, ITLSLOSeconds: 1})
		rec := NewTokenTruthRecorder()
		p.SetTokenRecorder(rec.Recorder())
		p.Offer(Request{ID: 9, InputTokens: 1000, OutputTokens: 5})
		for tick := 0; tick < 4; tick++ {
			p.Step(float64(tick)*.25, .25, []int{0})
		}
		results := p.Results()
		if len(results) != 1 || !results[0].Failed {
			t.Fatalf("timeout outcome = %+v, want one failure", results)
		}
		traces := rec.Track(results)
		if len(traces) != 1 || traces[0].TokenCount() != 0 {
			t.Fatalf("timeout trace = %+v, want zero tokens", traces)
		}
		if _, ok := traces[0].TTFTSeconds(); ok {
			t.Fatal("timed out request reported TTFT")
		}
	})
	t.Run("outstanding request has no trace and is recorded separately", func(t *testing.T) {
		p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
		rec := NewTokenTruthRecorder()
		p.SetTokenRecorder(rec.Recorder())
		p.Offer(Request{ID: 4, OutputTokens: 4})
		for tick := 0; tick < 4; tick++ {
			p.Step(float64(tick)*.25, .25, []int{0})
		}
		if len(rec.Track(p.Results())) != 0 {
			t.Fatal("outstanding request produced a trace without an outcome")
		}
		if p.Outstanding() != 1 {
			t.Fatalf("outstanding = %d, want 1", p.Outstanding())
		}
		summary := p.Summary(1)
		if summary.Offered != 1 || summary.Outstanding != 1 || summary.Completed+summary.Failed != 0 {
			t.Fatalf("summary = %+v, want offered 1 outstanding 1 and no outcomes", summary)
		}
	})
}

// TestPlantTokenRecorderSharedBatchKeepsPerRequestTruth checks that batch
// sharing interleaves events from two requests while each trace keeps its own
// generation order and exact per-request rate.
func TestPlantTokenRecorderSharedBatchKeepsPerRequestTruth(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 2, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
	type tokenEvent struct {
		requestID, replica, ordinal int
		at                          float64
	}
	var events []tokenEvent
	p.SetTokenRecorder(func(requestID, replicaOrdinal, tokenIndex int, at float64) {
		events = append(events, tokenEvent{requestID: requestID, replica: replicaOrdinal, ordinal: tokenIndex, at: at})
	})
	p.Offer(Request{ID: 0, OutputTokens: 2})
	p.Offer(Request{ID: 1, OutputTokens: 2})
	for tick := 0; tick < 8; tick++ {
		p.Step(float64(tick)*.25, .25, []int{0})
	}
	results := p.Results()
	if len(results) != 2 || results[0].Failed || results[1].Failed {
		t.Fatalf("request outcomes = %+v", results)
	}
	// Two active requests halve each share: one token per second per request.
	wantTimes := []float64{1, 1, 2, 2}
	wantRequests := []int{0, 1, 0, 1}
	if len(events) != len(wantTimes) {
		t.Fatalf("observer saw %d token events, want 4: %v", len(events), events)
	}
	for i, want := range wantTimes {
		if events[i].requestID != wantRequests[i] || events[i].ordinal != i/2+1 {
			t.Fatalf("event %d = %+v, want request %d token ordinal %d", i, events[i], wantRequests[i], i/2+1)
		}
		if diff := events[i].at - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("token of request %d completed at %.9f, want %.9f", events[i].requestID, events[i].at, want)
		}
	}
}

// TestPlantTokenRecorderFollowsVaryingReplicaSet checks that events carry the
// serving replica while the fleet scales out mid-run: the existing request
// keeps its rate on replica 0 and the next arrival lands on replica 1.
func TestPlantTokenRecorderFollowsVaryingReplicaSet(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
	type tokenEvent struct {
		requestID, replica, ordinal int
		at                          float64
	}
	var events []tokenEvent
	p.SetTokenRecorder(func(requestID, replicaOrdinal, tokenIndex int, at float64) {
		events = append(events, tokenEvent{requestID: requestID, replica: replicaOrdinal, ordinal: tokenIndex, at: at})
	})
	p.Offer(Request{ID: 0, OutputTokens: 8})
	ready := []int{0}
	for tick := 0; tick < 20; tick++ {
		now := float64(tick) * .25
		if now == 2 {
			// Request 0 still holds replica 0 busy, so the new arrival takes replica 1.
			ready = []int{0, 1}
			p.Offer(Request{ID: 1, OutputTokens: 2})
		}
		p.Step(now, .25, ready)
	}
	want := []tokenEvent{
		{requestID: 0, replica: 0, ordinal: 1, at: .5},
		{requestID: 0, replica: 0, ordinal: 2, at: 1},
		{requestID: 0, replica: 0, ordinal: 3, at: 1.5},
		{requestID: 0, replica: 0, ordinal: 4, at: 2},
		{requestID: 0, replica: 0, ordinal: 5, at: 2.5},
		{requestID: 1, replica: 1, ordinal: 1, at: 2.5},
		{requestID: 0, replica: 0, ordinal: 6, at: 3},
		{requestID: 1, replica: 1, ordinal: 2, at: 3},
		{requestID: 0, replica: 0, ordinal: 7, at: 3.5},
		{requestID: 0, replica: 0, ordinal: 8, at: 4},
	}
	if len(events) != len(want) {
		t.Fatalf("observer saw %d token events, want %d: %v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("event %d = %+v, want %+v", i, events[i], w)
		}
	}
	results := p.Results()
	finish := map[int]float64{}
	for _, r := range results {
		if r.Failed {
			t.Fatalf("request %d failed: %+v", r.ID, r)
		}
		finish[r.ID] = r.FinishedAt
	}
	if len(finish) != 2 || math.Abs(finish[0]-4) > 1e-9 || math.Abs(finish[1]-3) > 1e-9 {
		t.Fatalf("finish times = %v, want request 0 at 4 and request 1 at 3", finish)
	}
}

// TestPlantTokenRecorderTokenCountBoundaries locks token-count boundaries:
// zero output has no tokens, one token has TTFT but no TPOT or ITL, and a
// fractional final token is never emitted as a whole token.
func TestPlantTokenRecorderTokenCountBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name            string
		output          float64
		wantEvents      int
		wantTTFTDefined bool
		wantTPOTDefined bool
		wantLastIsEnd   bool
	}{
		{name: "no output tokens", output: 0},
		{name: "single output token", output: 1, wantEvents: 1, wantTTFTDefined: true, wantLastIsEnd: true},
		{name: "fractional final token", output: 2.5, wantEvents: 2, wantTTFTDefined: true, wantTPOTDefined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 1, TimeoutSeconds: 30, TTFTSLOSeconds: 30, ITLSLOSeconds: 30})
			rec := NewTokenTruthRecorder()
			p.SetTokenRecorder(rec.Recorder())
			p.Offer(Request{ID: 5, OutputTokens: tc.output})
			for tick := 0; tick < 12; tick++ {
				p.Step(float64(tick)*.25, .25, []int{0})
			}
			results := p.Results()
			traces := rec.Track(results)
			if len(results) != 1 || len(traces) != 1 {
				t.Fatalf("outcome %+v, trace %+v, want one of each", results, traces)
			}
			trace := traces[0]
			if trace.TokenCount() != tc.wantEvents {
				t.Fatalf("output %g produced %d token events, want %d", tc.output, trace.TokenCount(), tc.wantEvents)
			}
			ttft, ttftOK := trace.TTFTSeconds()
			_, tpotOK := trace.TPOTSeconds()
			if ttftOK != tc.wantTTFTDefined {
				t.Fatalf("output %g TTFT defined = %t, want %t (ttft %.3f)", tc.output, ttftOK, tc.wantTTFTDefined, ttft)
			}
			if tpotOK != tc.wantTPOTDefined {
				t.Fatalf("output %g TPOT defined = %t, want %t", tc.output, tpotOK, tc.wantTPOTDefined)
			}
			if got, want := len(trace.ITLSeconds()), max(0, tc.wantEvents-1); got != want {
				t.Fatalf("output %g produced %d ITLs, want %d", tc.output, got, want)
			}
			if tc.wantEvents > 0 {
				last := trace.Tokens()[tc.wantEvents-1]
				if tc.wantLastIsEnd {
					if diff := last - results[0].FinishedAt; diff > 1e-9 || diff < -1e-9 {
						t.Fatalf("last token %.9f != finish %.9f", last, results[0].FinishedAt)
					}
				} else if last >= results[0].FinishedAt {
					t.Fatalf("last token %.9f is not before finish %.9f", last, results[0].FinishedAt)
				}
			}
		})
	}
}

// TestPlantTokenRecorderReplaysIdentically proves determinism: the same
// identical-seed traffic driving two fresh plants produces identical traces.
func TestPlantTokenRecorderReplaysIdentically(t *testing.T) {
	run := func() []TokenTrace {
		p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 2, BatchGain: .1, Contention: .05, TimeoutSeconds: 3, TTFTSLOSeconds: 1, ITLSLOSeconds: .05})
		rec := NewTokenTruthRecorder()
		p.SetTokenRecorder(rec.Recorder())
		traffic := NewTraffic(TrafficConfig{Seed: 17, Phases: []Phase{{UntilSeconds: 2, RequestsPerSecond: 8, InputTokens: 40, OutputTokens: 15, Jitter: .5}}})
		for tick := 0; tick < 24; tick++ {
			now := float64(tick) * .1
			for _, request := range traffic.Arrivals(now, .1, p.Outstanding()) {
				p.Offer(request)
			}
			p.Step(now, .1, []int{0, 1})
		}
		traces := rec.Track(p.Results())
		SortTraces(traces)
		return traces
	}
	first, second := run(), run()
	if len(first) != len(second) {
		t.Fatalf("replay lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.RequestID != b.RequestID || a.ArrivedAt != b.ArrivedAt || a.FinishedAt != b.FinishedAt || a.Failed != b.Failed || a.TokenCount() != b.TokenCount() || !kubemeta.DeepEqual(a.Tokens(), b.Tokens()) {
			t.Fatalf("replay diverged at trace %d: %+v vs %+v", i, a, b)
		}
	}
	events := 0
	for _, trace := range first {
		events += trace.TokenCount()
	}
	if len(first) == 0 || events == 0 {
		t.Fatalf("replay fixture captured nothing: %d traces, %d token events", len(first), events)
	}
}

// TestPlantTokenRecorderOptInLeavesServiceTruthUnchanged proves the recorder
// is opt-in: service results, summary and exposition are identical with and
// without recording, and the recorded run really captured events.
func TestPlantTokenRecorderOptInLeavesServiceTruthUnchanged(t *testing.T) {
	run := func(record bool) ([]RequestResult, Summary, []byte, int) {
		p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 2, TimeoutSeconds: 3, TTFTSLOSeconds: 1, ITLSLOSeconds: .05})
		rec := NewTokenTruthRecorder()
		if record {
			p.SetTokenRecorder(rec.Recorder())
		}
		for i := 0; i < 6; i++ {
			p.Offer(Request{ID: i, InputTokens: 20, OutputTokens: 15, CacheFraction: .25})
		}
		var exposition []byte
		for tick := 0; tick < 24; tick++ {
			p.Step(float64(tick)*.1, .1, []int{0})
			if tick == 7 {
				var err error
				exposition, err = p.Exposition(0, "vllm", 1)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		events := 0
		for _, trace := range rec.Track(p.Results()) {
			events += trace.TokenCount()
		}
		return p.Results(), p.Summary(2.4), exposition, events
	}
	plainResults, plainSummary, plainExposition, _ := run(false)
	seenResults, seenSummary, seenExposition, seenEvents := run(true)
	if !kubemeta.DeepEqual(plainResults, seenResults) {
		t.Fatalf("recording changed request results:\nplain %+v\nseen  %+v", plainResults, seenResults)
	}
	if plainSummary != seenSummary {
		t.Fatalf("recording changed summary: plain %+v, seen %+v", plainSummary, seenSummary)
	}
	if !kubemeta.DeepEqual(plainExposition, seenExposition) {
		t.Fatalf("recording changed exposition:\nplain %s\nseen  %s", plainExposition, seenExposition)
	}
	if seenEvents == 0 {
		t.Fatal("recorded run captured no token events; the comparison is vacuous")
	}
}

// TestQuantizeArrivalsConservesRequests locks the self-consistent quantization
// rule: per-request rounding, verified request-for-request. Per-request
// rounding deliberately does not conserve raw fractional totals.
func TestQuantizeArrivalsConservesRequests(t *testing.T) {
	t.Run("jittered traffic quantizes per request", func(t *testing.T) {
		traffic := NewTraffic(TrafficConfig{Seed: 17, Phases: []Phase{{UntilSeconds: 10, RequestsPerSecond: 4, InputTokens: 40, OutputTokens: 20, CacheFraction: .3, Jitter: .5}}})
		var raw []Request
		for tick := 0; tick < 40; tick++ {
			raw = append(raw, traffic.Arrivals(float64(tick)*.25, .25, 0)...)
		}
		if len(raw) == 0 {
			t.Fatal("traffic fixture offered nothing")
		}
		quantized := QuantizeArrivals(raw)
		if err := ConservationCheck(raw, quantized); err != nil {
			t.Fatalf("quantized arrivals drifted: %v", err)
		}
		for i, request := range quantized {
			if request.InputTokens != math.Round(request.InputTokens) || request.OutputTokens != math.Round(request.OutputTokens) {
				t.Fatalf("arrival %d kept fractional tokens: %+v", i, request)
			}
		}
	})
	t.Run("per-request rounding is self-consistent", func(t *testing.T) {
		raw := []Request{
			{ID: 0, InputTokens: 1.4, OutputTokens: 1.4},
			{ID: 1, InputTokens: 1.4, OutputTokens: 1.4},
		}
		quantized := QuantizeArrivals(raw)
		if quantized[0].OutputTokens != 1 || quantized[1].OutputTokens != 1 {
			t.Fatalf("quantized outputs = %g, %g, want 1, 1", quantized[0].OutputTokens, quantized[1].OutputTokens)
		}
		if err := ConservationCheck(raw, quantized); err != nil {
			t.Fatalf("self-consistent quantization rejected: %v", err)
		}
	})
	t.Run("tampered and truncated sets are rejected", func(t *testing.T) {
		raw := []Request{{ID: 0, InputTokens: 2.5, OutputTokens: 3.5}}
		quantized := QuantizeArrivals(raw)
		tampered := append([]Request(nil), quantized...)
		tampered[0].OutputTokens++
		if err := ConservationCheck(raw, tampered); err == nil {
			t.Fatal("tampered quantization passed conservation")
		}
		if err := ConservationCheck(raw, quantized[:len(quantized)-1]); err == nil {
			t.Fatal("truncated quantization passed conservation")
		}
	})
}
