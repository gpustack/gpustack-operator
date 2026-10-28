package sim

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// PlantConfig defines independent service truth in synthetic tokens and seconds.
type PlantConfig struct {
	TokensPerSecond float64
	BatchSize       int
	BatchGain       float64
	Contention      float64
	TimeoutSeconds  float64
	TTFTSLOSeconds  float64
	ITLSLOSeconds   float64
}

// RequestResult records one request outcome, including failures and actual arrival time.
// TTFTSeconds is -1 if no whole output token was delivered; that request is not SLO-compliant.
type RequestResult struct {
	ID          int
	ArrivedAt   float64
	FinishedAt  float64
	TTFTSeconds float64
	ITLSeconds  float64
	Failed      bool
	Compliant   bool
}

type work struct {
	request        Request
	prompt, output float64
	firstTokenAt   float64
	itl            float64
}

type histogram struct {
	buckets    [12]float64
	count, sum float64
}

const serviceCompletionTolerance = 1e-9

var histogramBounds = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 30, math.Inf(1)}

func (h *histogram) observe(value, count float64) {
	h.count += count
	h.sum += value * count
	for i, bound := range histogramBounds {
		if value <= bound {
			h.buckets[i] += count
		}
	}
}

type engine struct {
	active               []*work
	prompt, output, hits float64
	ttft, itl            histogram
}

// Plant owns queues, token completion and request truth; capacity is never learned from observations.
// Calls to Step and Offer are serial. Exposition may be read concurrently between steps.
type Plant struct {
	config        PlantConfig
	queue         []*work
	engines       map[int]*engine
	results       []RequestResult
	offered       int
	readySeconds  float64
	tokenObserver func(requestID, replicaOrdinal, tokenIndex int, at float64)
}

// SetTokenRecorder opts the plant into whole-token event recording: the observer
// receives every generated whole token with its request, serving replica and the
// exact interpolated completion time inside the constant-rate service step.
// The fractional service path and the frozen baseline are unchanged when unset.
func (p *Plant) SetTokenRecorder(observer func(requestID, replicaOrdinal, tokenIndex int, at float64)) {
	p.tokenObserver = observer
}

// NewPlant creates an empty synthetic plant with a fixed service configuration.
func NewPlant(config PlantConfig) *Plant {
	return &Plant{config: config, engines: map[int]*engine{}}
}

// Offer enqueues independent work; cached prompt tokens consume no service budget.
// Requests without output complete after input service but have no first-token latency.
func (p *Plant) Offer(request Request) {
	p.offered++
	p.queue = append(p.queue, &work{
		request: request, prompt: request.InputTokens * (1 - request.CacheFraction),
		output: request.OutputTokens, firstTokenAt: -1,
	})
}

// Outstanding counts both queued and executing requests.
func (p *Plant) Outstanding() int {
	count := len(p.queue)
	for _, e := range p.engines {
		count += len(e.active)
	}
	return count
}

// Offered counts the requests enqueued so far.
func (p *Plant) Offered() int { return p.offered }

// Step advances a bounded service quantum. Ready replicas accept new work.
// Optional draining replicas finish existing work without accepting requests.
// A nonpositive quantum only initializes engines for metrics exposition.
func (p *Plant) Step(now, step float64, ready []int, draining ...int) {
	serving := append(append([]int(nil), ready...), draining...)
	sort.Ints(serving)
	for _, id := range serving {
		if p.engines[id] == nil {
			p.engines[id] = &engine{}
		}
	}
	if step <= 0 {
		return
	}
	p.readySeconds += float64(len(ready)) * step
	p.queue = p.expire(p.queue, now)
	ordinals := make([]int, 0, len(p.engines))
	for id := range p.engines {
		ordinals = append(ordinals, id)
	}
	sort.Ints(ordinals)
	for _, id := range ordinals {
		p.engines[id].active = p.expire(p.engines[id].active, now)
	}
	accepting := append([]int(nil), ready...)
	sort.Ints(accepting)
	for len(p.queue) > 0 {
		var selected *engine
		for _, id := range accepting {
			e := p.engines[id]
			if len(e.active) < max(1, p.config.BatchSize) && (selected == nil || len(e.active) < len(selected.active)) {
				selected = e
			}
		}
		if selected == nil {
			break
		}
		cached := p.queue[0].request.InputTokens * p.queue[0].request.CacheFraction
		selected.prompt += cached
		selected.hits += cached
		selected.active = append(selected.active, p.queue[0])
		p.queue = p.queue[1:]
	}
	for _, id := range serving {
		e := p.engines[id]
		batch := len(e.active)
		if batch == 0 {
			continue
		}
		capacity := p.config.TokensPerSecond * (1 + p.config.BatchGain*float64(batch-1)) / (1 + p.config.Contention*float64(batch-1))
		share := capacity * step / float64(batch)
		if share <= 0 {
			continue
		}
		remaining := e.active[:0]
		for _, w := range e.active {
			budget := share
			prompt := math.Min(w.prompt, budget)
			w.prompt -= prompt
			budget -= prompt
			if prompt > 0 {
				e.prompt += prompt
			}
			output := math.Min(w.output, budget)
			if output > 0 {
				secondsPerToken := step / share
				untilFirstToken := 1 - (w.request.OutputTokens - w.output)
				// With token recording, a first token can land exactly on a quantum
				// boundary after fractional accumulation, so detection shares the
				// completion tolerance. The frozen default keeps the strict
				// comparison: such a boundary case reports no first token.
				firstTokenTolerance := 0.0
				if p.tokenObserver != nil {
					firstTokenTolerance = serviceCompletionTolerance
				}
				if w.firstTokenAt < 0 && output+firstTokenTolerance >= untilFirstToken {
					w.firstTokenAt = now + (prompt+untilFirstToken)*secondsPerToken
					e.ttft.observe(w.firstTokenAt-w.request.ArrivedAt, 1)
				}
				itl := secondsPerToken
				w.itl = math.Max(w.itl, itl)
				// Histogram counts are whole token gaps; fractional work stays in counters.
				completed := w.request.OutputTokens - w.output
				doneBefore := math.Floor(completed + serviceCompletionTolerance)
				doneAfter := math.Floor(completed + output + serviceCompletionTolerance)
				e.itl.observe(itl, math.Max(0, doneAfter-1)-math.Max(0, doneBefore-1))
				// Whole-token events carry the exact interpolated completion
				// time of every whole token completed in this constant-rate step.
				if p.tokenObserver != nil {
					for m := int(doneBefore) + 1; m <= int(doneAfter); m++ {
						p.tokenObserver(w.request.ID, id, m, now+(prompt+float64(m)-completed)*secondsPerToken)
					}
				}
				w.output -= output
				e.output += output
			}
			if w.prompt <= serviceCompletionTolerance && w.output <= serviceCompletionTolerance {
				p.finish(w, now+(prompt+output)*step/share, false)
			} else {
				remaining = append(remaining, w)
			}
		}
		e.active = remaining
	}
}

// RetainReplicas fails unfinished work on deleted replicas and clears their counters.
// A replacement at the same ordinal starts with independent engine state.
func (p *Plant) RetainReplicas(now float64, ordinals []int) {
	live := make(map[int]bool, len(ordinals))
	for _, id := range ordinals {
		live[id] = true
	}
	var deleted []int
	for id := range p.engines {
		if !live[id] {
			deleted = append(deleted, id)
		}
	}
	sort.Ints(deleted)
	for _, id := range deleted {
		for _, w := range p.engines[id].active {
			p.finish(w, now, true)
		}
		delete(p.engines, id)
	}
}

func (p *Plant) expire(items []*work, now float64) []*work {
	remaining := items[:0]
	for _, w := range items {
		if now-w.request.ArrivedAt >= p.config.TimeoutSeconds {
			p.finish(w, now, true)
		} else {
			remaining = append(remaining, w)
		}
	}
	return remaining
}

func (p *Plant) finish(w *work, now float64, failed bool) {
	ttft := -1.0
	if w.firstTokenAt >= 0 {
		ttft = w.firstTokenAt - w.request.ArrivedAt
	}
	compliant := !failed && ttft >= 0 && ttft <= p.config.TTFTSLOSeconds && w.itl <= p.config.ITLSLOSeconds
	p.results = append(p.results, RequestResult{
		ID: w.request.ID, ArrivedAt: w.request.ArrivedAt, FinishedAt: now,
		TTFTSeconds: ttft, ITLSeconds: w.itl, Failed: failed, Compliant: compliant,
	})
}

// Results returns request-level truth, never an average of run quantiles.
func (p *Plant) Results() []RequestResult { return append([]RequestResult(nil), p.results...) }

// ResetCounters simulates a process counter reset without deleting request truth.
func (p *Plant) ResetCounters() {
	for _, e := range p.engines {
		e.prompt = 0
		e.output = 0
		e.hits = 0
		e.ttft = histogram{}
		e.itl = histogram{}
	}
}

// Exposition renders engine-style cumulative counters and latency histograms.
// Latency histograms are per-step approximations charged at the serving step's
// rate; exact per-token truth lives in TokenTrace.
// An absent replica returns an error rather than an observed idle sample.
func (p *Plant) Exposition(ordinal int, name string, readyCount int) ([]byte, error) {
	e := p.engines[ordinal]
	if e == nil {
		return nil, fmt.Errorf("replica %d has no metrics", ordinal)
	}
	var b strings.Builder
	metric := func(key, kind string, value float64) {
		fmt.Fprintf(&b, "# TYPE %s:%s %s\n%s:%s %g\n", name, key, kind, name, key, value)
	}
	running, waiting, cache := "num_requests_running", "num_requests_waiting", "kv_cache_usage_perc"
	if name == "sglang" {
		running, waiting, cache = "num_running_reqs", "num_queue_reqs", "token_usage"
	}
	metric(running, "gauge", float64(len(e.active)))
	metric(waiting, "gauge", float64(len(p.queue))/float64(max(1, readyCount)))
	metric(cache, "gauge", math.Min(1, float64(len(e.active))/float64(max(1, p.config.BatchSize))))
	metric("prompt_tokens_total", "counter", e.prompt)
	metric("generation_tokens_total", "counter", e.output)
	if name == "sglang" {
		fmt.Fprintln(&b, "# TYPE sglang:prefill_effective_tokens_total counter")
		fmt.Fprintf(&b, "sglang:prefill_effective_tokens_total{mode=\"input\"} %g\n", e.prompt-e.hits)
		fmt.Fprintf(&b, "sglang:prefill_effective_tokens_total{mode=\"device_hit\"} %g\n", e.hits)
	} else {
		metric("prefix_cache_queries_total", "counter", e.prompt)
		metric("prefix_cache_hits_total", "counter", e.hits)
	}
	writeHistogram(&b, name+":time_to_first_token_seconds", e.ttft)
	writeHistogram(&b, name+":inter_token_latency_seconds", e.itl)
	return []byte(b.String()), nil
}

func writeHistogram(b *strings.Builder, name string, h histogram) {
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)
	for i, bound := range histogramBounds {
		fmt.Fprintf(b, "%s_bucket{le=\"%g\"} %g\n", name, bound, h.buckets[i])
	}
	fmt.Fprintf(b, "%s_count %g\n%s_sum %g\n", name, h.count, name, h.sum)
}

// Summary reports synthetic request outcomes and resource cost over the supplied duration.
type Summary struct {
	Offered             int
	Completed           int
	Failed              int
	Outstanding         int
	SLOCompliant        int
	Goodput             float64
	P95LatencySeconds   float64
	P95TTFTSeconds      float64
	ReadyReplicaSeconds float64
}

// Summary computes quantiles from individual successful requests and reports failures separately.
func (p *Plant) Summary(seconds float64) Summary {
	s := Summary{Offered: p.offered, Outstanding: p.Outstanding(), ReadyReplicaSeconds: p.readySeconds}
	var latency, ttft []float64
	for _, r := range p.results {
		if r.Failed {
			s.Failed++
			continue
		}
		s.Completed++
		if r.Compliant {
			s.SLOCompliant++
		}
		latency = append(latency, r.FinishedAt-r.ArrivedAt)
		if r.TTFTSeconds >= 0 {
			ttft = append(ttft, r.TTFTSeconds)
		}
	}
	if seconds > 0 {
		s.Goodput = float64(s.SLOCompliant) / seconds
	}
	s.P95LatencySeconds = Quantile(latency, 0.95)
	s.P95TTFTSeconds = Quantile(ttft, 0.95)
	return s
}

// Quantile returns the q-quantile of the values in nearest-rank order.
// An empty population returns zero.
func Quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(float64(len(sorted))*q)) - 1
	idx = max(0, min(len(sorted)-1, idx))
	return sorted[idx]
}
