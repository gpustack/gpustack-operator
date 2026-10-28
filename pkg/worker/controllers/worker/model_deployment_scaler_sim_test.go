package worker

import (
	"encoding/json"
	"os"
	"testing"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/scaling/sim"
)

func scalerSimulationFixture(t *testing.T) sim.Fixture {
	t.Helper()
	data, err := os.ReadFile("../../scaling/sim/testdata/baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture sim.Fixture
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !fixture.Synthetic || fixture.Version != 1 {
		t.Fatal("expected versioned synthetic fixture")
	}
	return fixture
}

func TestModelDeploymentScalerSimulationBaseline(t *testing.T) {
	fixture := scalerSimulationFixture(t)
	for _, scenario := range fixture.Scenarios {
		if scenario.Holdout {
			continue
		}
		for _, arm := range []scalerSimulationArm{{Name: "fixed", Shadow: true}, {Name: "token"}, {Name: "predictive", Predictive: true}} {
			t.Run(scenario.Name+"/"+arm.Name, func(t *testing.T) {
				first := runScalerSimulation(t, scenario, arm)
				second := runScalerSimulation(t, scenario, arm)
				if !kubemeta.DeepEqual(first, second) {
					t.Fatal("simulation did not replay exactly")
				}
				assertScalerSimulationSafety(t, scenario, arm, first)
				if !arm.Shadow && scenario.Name == "step_and_cooldown" && first.Changes == 0 {
					t.Fatalf("step traffic never exercised a commit: %+v", first.Trace[:min(20, len(first.Trace))])
				}
				assertScalerSimulationContract(t, scenario, arm, first)
				if scenario.Name == "steady_noise" && first.Changes > fixture.Tolerances.MaximumSteadyChanges {
					t.Fatalf("steady replica chatter: %d changes", first.Changes)
				}
				data, err := json.Marshal(first.Summary)
				if err != nil {
					t.Fatal(err)
				}
				response, err := json.Marshal(first.Response)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("synthetic seconds=%.0f changes=%d requested_object_replica_seconds=%.0f retired=%d aborted=%d summary=%s response=%s", scenario.DurationSeconds, first.Changes, first.RequestedObjectReplicaSeconds, first.Retired, first.Aborted, data, response)
			})
		}
	}
}

func assertScalerSimulationContract(t *testing.T, scenario sim.Scenario, arm scalerSimulationArm, result scalerSimulationResult) {
	t.Helper()
	if arm.Shadow {
		return
	}
	flags := map[string]bool{}
	for _, point := range result.Trace {
		flags["growth"] = flags["growth"] || point.Committed > int32(scenario.Fleet.Initial)
		flags["boot gap"] = flags["boot gap"] || point.Committed > int32(point.Ready)
		flags["rollback"] = flags["rollback"] || point.BlockedReason == workercore.ModelDeploymentScalerReasonResourceExhausted
		flags["abort restore"] = flags["abort restore"] || point.BlockedReason == workercore.ModelDeploymentScalerReasonScaleDownAborted
		if scenario.Name == "coverage_and_counter_reset" && point.Seconds >= 130 && point.Seconds < 250 {
			if point.Coverage || point.ProcessedTPS != nil {
				t.Fatalf("outage was represented as observed throughput: %+v", point)
			}
			flags["outage"] = true
		}
		if scenario.ResetAtSeconds > 0 && point.Seconds == scenario.ResetAtSeconds {
			flags["counter reset"] = !point.Coverage && point.ProcessedTPS == nil
		}
		if scenario.RestartAtSeconds > 0 && point.Seconds == scenario.RestartAtSeconds {
			flags["restart"] = !point.Coverage && point.ProcessedTPS == nil
		}
	}
	required := map[string][]string{
		"step_and_cooldown":          {"growth", "boot gap"},
		"quota_exhaustion":           {"rollback", "boot gap"},
		"slow_boot":                  {"growth", "boot gap"},
		"coverage_and_counter_reset": {"outage", "counter reset"},
		"retirement_abort":           {"abort restore", "growth"},
		"restart":                    {"restart", "growth"},
	}
	for _, flag := range required[scenario.Name] {
		if !flags[flag] {
			t.Fatalf("scenario never exercised %s", flag)
		}
	}
	if scenario.AbortRetirement && result.Aborted != 1 {
		t.Fatalf("aborted retirements = %d", result.Aborted)
	}
	last := result.Trace[len(result.Trace)-1]
	if last.Committed != 1 || last.Ready != 1 || result.Retired == 0 {
		t.Fatalf("did not settle to baseline after full cooldown: %+v retired=%d", last, result.Retired)
	}
}

func TestModelDeploymentScalerSimulationEngineCacheNumerator(t *testing.T) {
	scenario := scalerSimulationFixture(t).Scenarios[1]
	scenario.DurationSeconds = 120
	scenario.Traffic.Phases[0].CacheFraction = 0.5
	scenario.Traffic.Phases[0].Jitter = 0
	var reference scalerSimulationResult
	for _, engine := range []string{"vllm", "sglang"} {
		t.Run(engine, func(t *testing.T) {
			scenario.Engine = engine
			result := runScalerSimulation(t, scenario, scalerSimulationArm{Name: "fixed", Shadow: true})
			if engine == "vllm" {
				reference = result
			} else if !kubemeta.DeepEqual(reference, result) {
				for i, point := range result.Trace {
					if !kubemeta.DeepEqual(reference.Trace[i], point) {
						t.Logf("vllm=%+v sglang=%+v", reference.Trace[i], point)
						break
					}
				}
				t.Fatal("engine exposition changed independent truth or sampled cache-missed numerator")
			}
			last := result.Trace[len(result.Trace)-1]
			if last.ProcessedTPS == nil || *last.ProcessedTPS != 120 {
				t.Fatalf("cache-missed input plus output TPS = %+v, want 120", last)
			}
		})
	}
}

func TestModelDeploymentScalerSimulationResponseBaseline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phases []sim.Phase
		trace  []scalerSimulationPoint
		want   []float64
	}{
		{
			name:   "growth after pre-event downscale",
			phases: []sim.Phase{{UntilSeconds: 120, RequestsPerSecond: 1, OutputTokens: 100}, {UntilSeconds: 200, RequestsPerSecond: 4, OutputTokens: 100}},
			trace:  []scalerSimulationPoint{{Seconds: 0, Raw: 2, Committed: 2, Ready: 2}, {Seconds: 110, Raw: 1, Committed: 1, Ready: 1}, {Seconds: 120, Raw: 1, Committed: 1, Ready: 1}, {Seconds: 130, Raw: 3, Committed: 2, Ready: 1}, {Seconds: 140, Raw: 3, Committed: 2, Ready: 2}},
			want:   []float64{10, 10, 20},
		},
		{
			name:   "independent raw committed and ready baselines",
			phases: []sim.Phase{{UntilSeconds: 120, RequestsPerSecond: 1, InputTokens: 100, CacheFraction: .9}, {UntilSeconds: 200, RequestsPerSecond: 1, InputTokens: 100}},
			trace:  []scalerSimulationPoint{{Seconds: 110, Raw: 3, Committed: 1, Ready: 2}, {Seconds: 120, Raw: 3, Committed: 2, Ready: 2}, {Seconds: 130, Raw: 4, Committed: 3, Ready: 2}, {Seconds: 150, Raw: 4, Committed: 3, Ready: 3}},
			want:   []float64{10, 0, 30},
		},
		{
			name:   "no growth observed",
			phases: []sim.Phase{{UntilSeconds: 120, RequestsPerSecond: 1, OutputTokens: 100}, {UntilSeconds: 200, RequestsPerSecond: 4, OutputTokens: 100}},
			trace:  []scalerSimulationPoint{{Seconds: 110, Raw: 1, Committed: 1, Ready: 1}, {Seconds: 200, Raw: 1, Committed: 1, Ready: 1}},
			want:   []float64{-1, -1, -1},
		},
		{
			name:   "no load increase event",
			phases: []sim.Phase{{UntilSeconds: 200, RequestsPerSecond: 1, OutputTokens: 100}},
			trace:  []scalerSimulationPoint{{Seconds: 0, Raw: 2, Committed: 2, Ready: 2}, {Seconds: 100, Raw: 3, Committed: 3, Ready: 3}},
			want:   []float64{-1, -1, -1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := sim.Scenario{DurationSeconds: 200, Fleet: sim.FleetConfig{Initial: 2}, Traffic: sim.TrafficConfig{Phases: tc.phases}}
			response := measureScalerSimulationResponse(scenario, scalerSimulationResult{Trace: tc.trace})
			got := []*float64{response.FirstRecommendationGrowthSeconds, response.FirstCommitGrowthSeconds, response.FirstReadyGrowthSeconds}
			for i, want := range tc.want {
				if (want < 0 && got[i] != nil) || (want >= 0 && (got[i] == nil || *got[i] != want)) {
					t.Fatalf("response %d = %v, want %g; full response %+v", i, got[i], want, response)
				}
			}
			if _, err := json.Marshal(response); err != nil {
				t.Fatal(err)
			}
		})
	}
}
