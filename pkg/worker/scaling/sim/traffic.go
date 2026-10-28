// Package sim supplies deterministic synthetic traffic and service models for local tests.
// It has no dependency on the scaling algorithms or Kubernetes controllers.
package sim

import (
	"cmp"
	rand "math/rand/v2"
	"slices"
)

// Phase describes offered traffic until an exclusive simulated time boundary.
type Phase struct {
	UntilSeconds      float64
	RequestsPerSecond float64
	InputTokens       float64
	OutputTokens      float64
	CacheFraction     float64
	Jitter            float64
}

// TrafficConfig fixes the random seed and distinguishes open from closed loop load.
type TrafficConfig struct {
	Seed        uint64
	Concurrency int
	Phases      []Phase
}

// Request is independently generated work; token counts are native synthetic tokens.
type Request struct {
	ID            int
	ArrivedAt     float64
	InputTokens   float64
	OutputTokens  float64
	CacheFraction float64
}

// Traffic generates arrivals without access to a controller's capacity setting.
type Traffic struct {
	config TrafficConfig
	random *rand.Rand
	credit float64
	nextID int
}

// NewTraffic returns a replayable traffic source. Positive Concurrency selects closed loop.
func NewTraffic(config TrafficConfig) *Traffic {
	config.Phases = slices.Clone(config.Phases)
	slices.SortFunc(config.Phases, func(a, b Phase) int { return cmp.Compare(a.UntilSeconds, b.UntilSeconds) })
	return &Traffic{config: config, random: rand.New(rand.NewPCG(config.Seed, config.Seed+1))}
}

// Arrivals returns requests offered during this step, limited by closed loop clients if set.
// Closed loop clients discard blocked arrival opportunities rather than catching up later.
func (t *Traffic) Arrivals(now, step float64, outstanding int) []Request {
	var phase *Phase
	for i := range t.config.Phases {
		if now < t.config.Phases[i].UntilSeconds {
			phase = &t.config.Phases[i]
			break
		}
	}
	if phase == nil {
		return nil
	}
	t.credit += phase.RequestsPerSecond * step
	count := int(t.credit)
	t.credit -= float64(count)
	if t.config.Concurrency > 0 {
		count = min(count, max(0, t.config.Concurrency-outstanding))
	}
	arrivals := make([]Request, 0, count)
	for i := 0; i < count; i++ {
		factor := 1 + phase.Jitter*(2*t.random.Float64()-1)
		arrivals = append(arrivals, Request{
			ID: t.nextID, ArrivedAt: now, InputTokens: phase.InputTokens * factor,
			OutputTokens: phase.OutputTokens * factor, CacheFraction: phase.CacheFraction,
		})
		t.nextID++
	}
	return arrivals
}
