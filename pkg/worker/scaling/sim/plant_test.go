package sim

import (
	"math"
	"testing"

	"gpustack.ai/gpustack/pkg/kubemeta"
)

func TestPlantPartialServiceQuantum(t *testing.T) {
	values := []float64{30, 10, 20}
	if got := Quantile(values, 0.5); got != 20 || values[0] != 30 || values[1] != 10 || values[2] != 20 {
		t.Fatalf("Quantile mutated caller slice or returned %v: %v", got, values)
	}
	p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: 3, TTFTSLOSeconds: 1, ITLSLOSeconds: 0.02})
	p.Offer(Request{InputTokens: 10, OutputTokens: 20})
	p.Step(0, 0.25, []int{0})
	p.Step(0.25, 0.25, []int{0})
	got := p.Results()
	if len(got) != 1 || math.Abs(got[0].ITLSeconds-0.01) > 1e-9 || math.Abs(got[0].FinishedAt-0.3) > 1e-9 {
		t.Fatalf("partial quantum inflated latency: %+v", got)
	}
}

func TestPlantDispatchUsesReadyReplicas(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ready       []int
		wantLatency float64
		wantTokens  []float64
	}{
		{name: "one replica", ready: []int{0}, wantLatency: 4, wantTokens: []float64{400}},
		{name: "two replicas", ready: []int{0, 1}, wantLatency: 2, wantTokens: []float64{200, 200}},
		{name: "ordinal tie break", ready: []int{1, 0}, wantLatency: 2, wantTokens: []float64{200, 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 4, TimeoutSeconds: 10, TTFTSLOSeconds: 1, ITLSLOSeconds: 1})
			for i := 0; i < 4; i++ {
				p.Offer(Request{ID: i, InputTokens: 1, OutputTokens: 99})
			}
			for tick := 0; tick < 16; tick++ {
				p.Step(float64(tick)*.25, .25, tc.ready)
				if tick == 0 && p.engines[0].active[0].request.ID != 0 {
					t.Fatal("equal active counts did not select the lowest ordinal")
				}
			}
			summary := p.Summary(4)
			if summary.Completed != 4 || summary.Outstanding != 0 || summary.Failed != 0 || math.Abs(summary.P95LatencySeconds-tc.wantLatency) > 1e-9 {
				t.Fatalf("concurrent request truth = %+v", summary)
			}
			seen := map[int]bool{}
			for _, result := range p.Results() {
				if seen[result.ID] {
					t.Fatalf("duplicate completion: %+v", result)
				}
				seen[result.ID] = true
			}
			for id, want := range tc.wantTokens {
				engine := p.engines[id]
				if math.Abs(engine.prompt-want/100) > 1e-9 || math.Abs(engine.output-want*.99) > 1e-9 {
					t.Fatalf("replica %d processed prompt=%g output=%g, want total %g with 1:99 mix", id, engine.prompt, engine.output, want)
				}
			}
		})
	}
}

func TestPlantFirstWholeOutputToken(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		input                float64
		wantTTFT, wantFinish float64
	}{
		{name: "output spans steps", wantTTFT: .5, wantFinish: 1},
		{name: "prompt shares first step", input: .25, wantTTFT: .625, wantFinish: 1.125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: 2, BatchSize: 1, TimeoutSeconds: 10, TTFTSLOSeconds: 2, ITLSLOSeconds: 1})
			p.Offer(Request{InputTokens: tc.input, OutputTokens: 2})
			p.Step(0, .25, []int{0})
			if p.engines[0].ttft.count != 0 {
				t.Fatal("fractional output established a first token")
			}
			for tick := 1; tick < 5; tick++ {
				p.Step(float64(tick)*.25, .25, []int{0})
			}
			got := p.Results()
			if len(got) != 1 || math.Abs(got[0].TTFTSeconds-tc.wantTTFT) > 1e-9 || math.Abs(got[0].FinishedAt-tc.wantFinish) > 1e-9 {
				t.Fatalf("partial token timing = %+v", got)
			}
		})
	}
}

func TestPlantZeroServiceIsFinite(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		capacity, step, input float64
	}{
		{name: "zero quantum empty request", capacity: 100},
		{name: "zero capacity empty request", step: .25},
		{name: "zero quantum input only", capacity: 100, input: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: tc.capacity, BatchSize: 1, TimeoutSeconds: 10, TTFTSLOSeconds: 1, ITLSLOSeconds: 1})
			p.Offer(Request{ID: 1, InputTokens: tc.input})
			p.Step(0, tc.step, []int{0})
			if _, err := p.Exposition(0, "vllm", 1); err != nil {
				t.Fatal(err)
			}
			if len(p.Results()) != 0 || p.Outstanding() != 1 {
				t.Fatalf("zero service completed work: %+v", p.Results())
			}
			p.Step(10, .25, []int{0})
			got := p.Results()
			if len(got) != 1 || !got[0].Failed || got[0].Compliant || math.IsNaN(got[0].FinishedAt) || math.IsInf(got[0].FinishedAt, 0) {
				t.Fatalf("nonfinite timeout: %+v", got)
			}
		})
	}
}

func TestPlantNoOutputHasNoFirstToken(t *testing.T) {
	for _, input := range []float64{0, 10} {
		p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: 10, TTFTSLOSeconds: 1, ITLSLOSeconds: 1})
		p.Offer(Request{ArrivedAt: 2, InputTokens: input})
		p.Step(2, .25, []int{0})
		got := p.Results()
		if len(got) != 1 || got[0].TTFTSeconds != -1 || got[0].Compliant || got[0].Failed || math.Abs(got[0].FinishedAt-(2+input/100)) > 1e-9 {
			t.Fatalf("input-only outcome: %+v", got)
		}
	}
}

func TestFleetRetractsUnadmittedReplica(t *testing.T) {
	for _, tc := range []struct {
		name      string
		quota     int
		admission float64
	}{
		{name: "quota blocked", quota: 1},
		{name: "admission delayed", quota: 2, admission: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFleet(FleetConfig{Initial: 1, Quota: tc.quota, AdmissionSeconds: tc.admission, RetirementSeconds: 30})
			f.SetDesired(2, 0)
			f.Advance(1)
			f.SetDesired(1, 2)
			if len(f.Replicas()) != 1 || len(f.Draining()) != 0 || f.Retired != 0 {
				t.Fatalf("pending cancellation became retirement: %+v", f)
			}
			f.Advance(100)
			if len(f.Replicas()) != 1 || f.Retired != 0 {
				t.Fatalf("cancelled capacity resurrected or retired: %+v", f)
			}
			f.SetDesired(2, 101)
			if got := f.Replicas()[1]; got.CreatedAt != 101 || got.Admitted || got.Ready || f.Aborted != 0 {
				t.Fatalf("new request reused cancelled replica: %+v", got)
			}
			f.Advance(101)
			if len(f.Ready()) != 1 {
				t.Fatal("new request bypassed admission or quota")
			}
		})
	}
}

func TestPlantRetirementFinishesOnlyInflightWork(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: 10, TTFTSLOSeconds: 2, ITLSLOSeconds: .1})
	for i := 0; i < 2; i++ {
		p.Offer(Request{ID: i, InputTokens: 50, OutputTokens: 50})
	}
	p.Step(0, .5, []int{0})
	p.Step(.5, .5, nil, 0)
	p.Step(1, .5, nil, 0)
	if got := p.Summary(1.5); got.Completed != 1 || got.Outstanding != 1 {
		t.Fatalf("draining admitted new work or stopped inflight work: %+v", got)
	}
}

func TestPlantReplacementCannotResurrectRequests(t *testing.T) {
	p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: 10})
	p.Offer(Request{InputTokens: 100, OutputTokens: 100})
	p.Step(0, .25, []int{0})
	p.RetainReplicas(.25, nil)
	p.Step(.25, 3, []int{0})
	if got := p.Summary(3.25); got.Failed != 1 || got.Completed != 0 || got.Outstanding != 0 {
		t.Fatalf("replacement resurrected old work: %+v", got)
	}
}

func TestPlantConservesRequests(t *testing.T) {
	cases := []struct {
		name          string
		capacity      float64
		wantCompleted int
	}{
		{name: "service completes", capacity: 200, wantCompleted: 4},
		{name: "no capacity times out", capacity: 0, wantCompleted: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: tc.capacity, BatchSize: 2, TimeoutSeconds: 3, TTFTSLOSeconds: 2, ITLSLOSeconds: 1})
			for i := 0; i < 4; i++ {
				p.Offer(Request{ID: i, InputTokens: 20, OutputTokens: 10})
			}
			for tick := 0; tick < 40; tick++ {
				p.Step(float64(tick)*0.1, 0.1, []int{0})
			}
			got := p.Summary(4)
			if got.Completed != tc.wantCompleted || got.Completed+got.Failed+got.Outstanding != 4 {
				t.Fatalf("summary = %+v", got)
			}
		})
	}
}

func TestTrafficReplaysSeed(t *testing.T) {
	config := TrafficConfig{Seed: 17, Phases: []Phase{{UntilSeconds: 20, RequestsPerSecond: 3, InputTokens: 40, OutputTokens: 20, Jitter: 0.3}}}
	a, b := NewTraffic(config), NewTraffic(config)
	for tick := 0; tick < 80; tick++ {
		x, y := a.Arrivals(float64(tick)*0.25, 0.25, 0), b.Arrivals(float64(tick)*0.25, 0.25, 0)
		if !kubemeta.DeepEqual(x, y) {
			t.Fatalf("traffic differs at %d", tick)
		}
	}
}

func TestFleetRespectsBootAndQuota(t *testing.T) {
	f := NewFleet(FleetConfig{Initial: 1, Quota: 2, AdmissionSeconds: 10, BootSeconds: 20, RetirementSeconds: 10})
	f.SetDesired(3, 0)
	for _, tc := range []struct {
		now             float64
		admitted, ready int
	}{{0, 1, 1}, {10, 2, 1}, {29, 2, 1}, {30, 2, 2}} {
		f.Advance(tc.now)
		admitted := 0
		for _, replica := range f.Replicas() {
			if replica.Admitted {
				admitted++
			}
		}
		if admitted != tc.admitted || len(f.Ready()) != tc.ready {
			t.Fatalf("at %.0f admitted=%d ready=%v", tc.now, admitted, f.Ready())
		}
	}
}

func TestPlantOpenAndClosedLoop(t *testing.T) {
	results := make(map[string]Summary)
	for _, tc := range []struct {
		name        string
		concurrency int
	}{{name: "open"}, {name: "closed", concurrency: 1}} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: 10, TTFTSLOSeconds: 1, ITLSLOSeconds: 0.1})
			traffic := NewTraffic(TrafficConfig{Seed: 17, Concurrency: tc.concurrency, Phases: []Phase{{UntilSeconds: 30, RequestsPerSecond: 10, InputTokens: 50, OutputTokens: 50}}})
			for tick := 0; tick < 400; tick++ {
				now := float64(tick) * 0.1
				for _, request := range traffic.Arrivals(now, 0.1, p.Outstanding()) {
					p.Offer(request)
				}
				p.Step(now, 0.1, []int{0})
			}
			results[tc.name] = p.Summary(40)
		})
	}
	open, closed := results["open"], results["closed"]
	if open.Offered <= closed.Offered || open.Failed == 0 || closed.Failed != 0 {
		t.Fatalf("offered load suppression missing: open=%+v closed=%+v", open, closed)
	}
	if float64(open.SLOCompliant)/float64(open.Offered) >= float64(closed.SLOCompliant)/float64(closed.Offered) {
		t.Fatalf("closed loop concealed no overload: open=%+v closed=%+v", open, closed)
	}
}

func TestPlantCacheChangesEffectiveWork(t *testing.T) {
	completed := make([]int, 0, 2)
	for _, fraction := range []float64{0, 0.8} {
		p := NewPlant(PlantConfig{TokensPerSecond: 100, BatchSize: 1, TimeoutSeconds: 60, TTFTSLOSeconds: 60, ITLSLOSeconds: 1})
		for i := 0; i < 10; i++ {
			p.Offer(Request{ID: i, InputTokens: 100, OutputTokens: 100, CacheFraction: fraction})
		}
		for tick := 0; tick < 100; tick++ {
			p.Step(float64(tick)*0.1, 0.1, []int{0})
		}
		completed = append(completed, p.Summary(10).Completed)
	}
	if completed[1] <= completed[0] {
		t.Fatalf("cache did not change work: %v", completed)
	}
}
