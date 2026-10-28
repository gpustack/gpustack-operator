package worker

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/sim"
)

// The comparison fixture freezes one deterministic shared setup so the three
// scaling arms differ only in their algorithm. All arms replay identically per
// seed, the plant stays independent of every estimator, and the report labels
// timestamp-derived truth separately from algorithm-input approximations.

func comparisonPlant() sim.PlantConfig {
	return sim.PlantConfig{TokensPerSecond: 300, BatchSize: 4, BatchGain: .05, Contention: .02, TimeoutSeconds: 30, TTFTSLOSeconds: 2, ITLSLOSeconds: .1}
}

func comparisonFleet() sim.FleetConfig {
	return sim.FleetConfig{Initial: 1, Quota: 6, AdmissionSeconds: 10, BootSeconds: 20, RetirementSeconds: 30}
}

// comparisonShortfallFleet caps the fleet below the scaler maximum so the
// overload phase pins admission at the quota instead of the replica cap.
func comparisonShortfallFleet() sim.FleetConfig {
	fleet := comparisonFleet()
	fleet.Quota = 4
	return fleet
}

func comparisonScenarios() []sim.Scenario {
	return []sim.Scenario{
		{
			Name: "idle", Engine: "vllm", DurationSeconds: 120, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261026, Phases: []sim.Phase{{UntilSeconds: 120}}},
			Plant:   comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "step", Engine: "vllm", DurationSeconds: 240, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261027, Phases: []sim.Phase{
				{UntilSeconds: 60, RequestsPerSecond: 1, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 240, RequestsPerSecond: 3, InputTokens: 100, OutputTokens: 100, Jitter: .3},
			}},
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "burst-fault-reset", Engine: "vllm", DurationSeconds: 200, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261028, Phases: []sim.Phase{
				{UntilSeconds: 40, RequestsPerSecond: 3, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 60, RequestsPerSecond: 6, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 200, RequestsPerSecond: 1.5, InputTokens: 100, OutputTokens: 100, Jitter: .3},
			}},
			Faults: []sim.Fault{{FromSeconds: 70, UntilSeconds: 90, Ordinal: -1}}, ResetAtSeconds: 100, RestartAtSeconds: 120,
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "holdout-cache-shift", Engine: "vllm", Holdout: true, DurationSeconds: 240, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261029, Phases: []sim.Phase{
				{UntilSeconds: 120, RequestsPerSecond: 2, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 240, RequestsPerSecond: 2, InputTokens: 100, OutputTokens: 100, CacheFraction: .5, Jitter: .3},
			}},
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "noise", Engine: "vllm", DurationSeconds: 180, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261030, Phases: []sim.Phase{
				{UntilSeconds: 180, RequestsPerSecond: 2, InputTokens: 100, OutputTokens: 100, Jitter: .8},
			}},
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "ramp", Engine: "vllm", DurationSeconds: 240, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261031, Phases: []sim.Phase{
				{UntilSeconds: 45, RequestsPerSecond: .5, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 90, RequestsPerSecond: 1, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 135, RequestsPerSecond: 2, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 180, RequestsPerSecond: 3, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 240, RequestsPerSecond: 4, InputTokens: 100, OutputTokens: 100, Jitter: .3},
			}},
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "quota-ceiling", Engine: "vllm", DurationSeconds: 180, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261032, Phases: []sim.Phase{
				{UntilSeconds: 90, RequestsPerSecond: 10, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 180, RequestsPerSecond: 3, InputTokens: 100, OutputTokens: 100, Jitter: .3},
			}},
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
		{
			Name: "quota-shortfall", Engine: "vllm", DurationSeconds: 200, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261034, Phases: []sim.Phase{
				{UntilSeconds: 100, RequestsPerSecond: 8, InputTokens: 100, OutputTokens: 100, Jitter: .3},
				{UntilSeconds: 200, RequestsPerSecond: .5, InputTokens: 100, OutputTokens: 100, Jitter: .3},
			}},
			Plant: comparisonPlant(), Fleet: comparisonShortfallFleet(),
		},
		{
			Name: "length-shift", Engine: "vllm", DurationSeconds: 240, StepSeconds: 1,
			Traffic: sim.TrafficConfig{Seed: 20261033, Phases: []sim.Phase{
				{UntilSeconds: 120, RequestsPerSecond: 2, InputTokens: 50, OutputTokens: 50, Jitter: .3},
				{UntilSeconds: 240, RequestsPerSecond: 2, InputTokens: 200, OutputTokens: 400, Jitter: .3},
			}},
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
		},
	}
}

func comparisonArms() []scalerSimulationArm {
	proportional := scalerSimulationArm{Name: "proportional", RecordTokens: true, QuantizeTokens: true}
	proportional.Configure = func(_ *workercore.ModelDeployment, scaler *workercore.ModelDeploymentScaler) {
		scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
			Type: workercore.ModelDeploymentScalerAlgorithmTypeProportional,
			Proportional: &workercore.ModelDeploymentScalerProportionalSpec{
				Metric:      workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
				RoleTargets: []workercore.ModelDeploymentScalerRoleTarget{{Name: "server", Target: *quantityPointer("4")}},
				Tolerance:   quantityPointer("0.1"),
			},
		}
	}
	return []scalerSimulationArm{
		proportional,
		{Name: "token-throughput", RecordTokens: true, QuantizeTokens: true},
		{Name: "predictive", RecordTokens: true, QuantizeTokens: true, Predictive: true},
		{Name: "shadow-control", Shadow: true, RecordTokens: true, QuantizeTokens: true},
	}
}

// TestScalerComparisonQuantizesArrivals verifies the quantized comparison
// fixture conserves its raw arrivals request-for-request, and that the frozen
// traffic really is fractional before quantization.
func TestScalerComparisonQuantizesArrivals(t *testing.T) {
	for _, scenario := range comparisonScenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			traffic := sim.NewTraffic(scenario.Traffic)
			var raw []sim.Request
			for tick := 0; tick < int(scenario.DurationSeconds/scenario.StepSeconds); tick++ {
				raw = append(raw, traffic.Arrivals(float64(tick)*scenario.StepSeconds, scenario.StepSeconds, 0)...)
			}
			if err := sim.ConservationCheck(raw, sim.QuantizeArrivals(raw)); err != nil {
				t.Fatalf("scenario %s conservation: %v", scenario.Name, err)
			}
			fractional := 0
			for _, request := range raw {
				if request.InputTokens != math.Round(request.InputTokens) || request.OutputTokens != math.Round(request.OutputTokens) {
					fractional++
				}
			}
			if len(raw) > 0 && fractional == 0 {
				t.Fatalf("scenario %s has no fractional arrivals; quantization is untested", scenario.Name)
			}
		})
	}
}

// TestScalerComparisonArmsReplayIdentically proves every comparison arm
// replays identically under the same seed, plant and fleet configuration.
func TestScalerComparisonArmsReplayIdentically(t *testing.T) {
	scenario := comparisonScenarios()[1]
	for _, arm := range comparisonArms() {
		t.Run(arm.Name, func(t *testing.T) {
			first := runScalerSimulation(t, scenario, arm)
			second := runScalerSimulation(t, scenario, arm)
			if !kubemeta.DeepEqual(first.Requests, second.Requests) || !kubemeta.DeepEqual(first.Trace, second.Trace) || first.Summary != second.Summary || first.Changes != second.Changes {
				t.Fatalf("arm %s replayed differently", arm.Name)
			}
			if len(first.Traces) != len(second.Traces) {
				t.Fatalf("arm %s trace counts differ: %d vs %d", arm.Name, len(first.Traces), len(second.Traces))
			}
			for i := range first.Traces {
				a, b := first.Traces[i], second.Traces[i]
				if a.RequestID != b.RequestID || a.ArrivedAt != b.ArrivedAt || a.FinishedAt != b.FinishedAt || a.Failed != b.Failed || a.TokenCount() != b.TokenCount() || !kubemeta.DeepEqual(a.Tokens(), b.Tokens()) {
					t.Fatalf("arm %s trace %d diverged: %+v vs %+v", arm.Name, i, a, b)
				}
			}
			events := 0
			for _, trace := range first.Traces {
				events += trace.TokenCount()
			}
			if events == 0 {
				t.Fatalf("arm %s recorded no token events; the replay check is vacuous", arm.Name)
			}
		})
	}
}

type comparisonReport struct {
	GeneratedBy string                     `json:"generatedBy"`
	Provenance  comparisonProvenance       `json:"provenance"`
	Scenarios   []comparisonScenarioResult `json:"scenarios"`
}

type comparisonProvenance struct {
	FixtureVersion          int             `json:"fixtureVersion"`
	Engine                  string          `json:"engine"`
	StepSeconds             float64         `json:"stepSeconds"`
	SamplingIntervalSeconds int             `json:"samplingIntervalSeconds"`
	Plant                   sim.PlantConfig `json:"plant"`
	Fleet                   sim.FleetConfig `json:"fleet"`
	TokenMode               string          `json:"tokenMode"`
	ProportionalTarget      string          `json:"proportionalTarget"`
	TPOTSLOTarget           string          `json:"tpotSloTarget"`
	Notes                   []string        `json:"notes"`
}

type comparisonScenarioResult struct {
	Name    string                `json:"name"`
	Holdout bool                  `json:"holdout"`
	Seed    uint64                `json:"seed"`
	Arms    []comparisonArmResult `json:"arms"`
}

type comparisonArmResult struct {
	Arm     string      `json:"arm"`
	Shadow  bool        `json:"shadow"`
	Summary sim.Summary `json:"summary"`
	// ApproximateSLOFraction comes from the frozen plant approximation
	// (RequestResult TTFT and maximum instantaneous quantum); it is labeled
	// and never mixed with the timestamp-derived SLO below.
	ApproximateSLOFraction *float64                 `json:"approximateSloFraction"`
	TrueSLO                *comparisonTrueSLO       `json:"trueTimestampSlo"`
	ReplicaSeconds         float64                  `json:"requestedObjectReplicaSeconds"`
	Changes                int                      `json:"committedChanges"`
	ChangesPerMin          float64                  `json:"changesPerMinute"`
	Retired                int                      `json:"retired"`
	Aborted                int                      `json:"aborted"`
	Response               scalerSimulationResponse `json:"response"`
	Decisions              map[string]int           `json:"capacityDecisions"`
	True                   *comparisonTrueLatency   `json:"trueTimestampLatency"`
	Observed               *comparisonObserved      `json:"observedApproximateLatency"`
	TimelinePoints         int                      `json:"timelinePoints"`
}

// comparisonTimelinePoint is one sampled instant of one arm's adjustment
// dynamics, plot-ready: what the algorithm read (null means the signal was
// missing), what it recommended, what was committed and why.
type comparisonTimelinePoint struct {
	Scenario                 string   `json:"scenario"`
	Arm                      string   `json:"arm"`
	Shadow                   bool     `json:"shadow"`
	Seconds                  float64  `json:"seconds"`
	OfferedCumulative        int      `json:"offeredCumulative"`
	RawRecommended           int32    `json:"rawRecommendedReplicas"`
	Committed                int32    `json:"committedReplicas"`
	Ready                    int      `json:"readyReplicas"`
	Coverage                 bool     `json:"coverageComplete"`
	ThroughputState          string   `json:"throughputSignal"`
	LatencyState             string   `json:"latencySignal"`
	GaugeState               string   `json:"gaugeSignal"`
	ProcessedTPS             *float64 `json:"processedTokensPerSecond"`
	ObservedTTFTMilliseconds *float64 `json:"observedTTFTMilliseconds"`
	ObservedITLMilliseconds  *float64 `json:"observedITLMilliseconds"`
	ObservedWaiting          *float64 `json:"observedWaitingRequests"`
	PlantQueue               int      `json:"plantOutstanding"`
	Pending                  bool     `json:"pendingCohort"`
	BlockedReason            string   `json:"blockedReason,omitempty"`
	Retirement               string   `json:"retirementState,omitempty"`
	EffectiveCapacity        *float64 `json:"effectiveCapacityTokensPerSecond"`
	Change                   string   `json:"committedChange,omitempty"`
}

// comparisonTrueLatency holds statistics derived from recorded whole-token
// timestamps, never from the per-step approximations the algorithms observe.
type comparisonTrueLatency struct {
	TTFTSamples int      `json:"ttftSamples"`
	TTFTP50     *float64 `json:"ttftP50Milliseconds"`
	TTFTP95     *float64 `json:"ttftP95Milliseconds"`
	TTFTMax     *float64 `json:"ttftMaxMilliseconds"`
	TPOTSamples int      `json:"tpotSamples"`
	TPOTP50     *float64 `json:"tpotP50Milliseconds"`
	TPOTP95     *float64 `json:"tpotP95Milliseconds"`
	TPOTMax     *float64 `json:"tpotMaxMilliseconds"`
	ITLSamples  int      `json:"itlSamples"`
	ITLP50      *float64 `json:"itlP50Milliseconds"`
	ITLP95      *float64 `json:"itlP95Milliseconds"`
	ITLMax      *float64 `json:"itlMaxMilliseconds"`
}

// comparisonObserved holds the latency-shaped values the algorithms actually
// read from the per-step approximate exposition.
type comparisonObserved struct {
	Points        int     `json:"pointsWithFreshLatency"`
	TTFTMax       float64 `json:"observedTTFTMaxMilliseconds"`
	ITLMax        float64 `json:"observedITLMaxMilliseconds"`
	WaitingPoints int     `json:"pointsWithFreshGauge"`
	WaitingMax    float64 `json:"observedWaitingMax"`
}

// TestScalerComparisonAcrossArms runs the three scaling arms plus a separate
// fixed Shadow control over identical deterministic scenarios and writes a
// local report for docs. It records measurements only and never declares a
// winning arm.
func TestScalerComparisonAcrossArms(t *testing.T) {
	report := comparisonReport{
		GeneratedBy: "TestScalerComparisonAcrossArms",
		Provenance: comparisonProvenance{
			FixtureVersion: 1, Engine: "vllm", StepSeconds: 1, SamplingIntervalSeconds: 10,
			Plant: comparisonPlant(), Fleet: comparisonFleet(),
			TokenMode:          "opt-in whole-token recording with quantized arrivals; conservation verified per scenario",
			ProportionalTarget: "QueueDepth target 4 tolerance 0.1",
			TPOTSLOTarget:      "not-configured",
			Notes: []string{
				"True latency statistics derive from recorded whole-token timestamps: TTFT is the first token minus arrival, TPOT is (last-first)/(N-1) and each ITL is an adjacent timestamp difference.",
				"Observed latency values are algorithm inputs read from the per-step approximate exposition; they are not true per-token statistics.",
				"The frozen exposition histograms and RequestResult.ITLSeconds stay per-step approximations; reported true ITL quantiles are computed from timestamps, never relabeled from the approximation.",
				"True SLO semantics: per completed request, the recorded TTFT is within the TTFT target and every adjacent token ITL is within the ITL target; single-token requests satisfy the ITL constraint vacuously; failed and outstanding requests count as non-compliant; the fraction denominator is Offered.",
				"approximateSloFraction is computed from the frozen plant approximation and is never mixed with the timestamp-derived SLO.",
				"No TPOT SLO target is configured; TPOT is reported as a distribution only and is never scored.",
				"Holdout scenarios are reported unmodified and no arm is tuned on them within this fixture; that demonstrates independence from the current configuration only and does not verify historical tuning.",
				"Replica seconds count requested objects including pending and retiring replicas, not billed GPU cost.",
				"capacityDecisions counts AfterSample override decisions; the timeline JSONL carries the per-instant series: offered traffic, observed inputs with null for missing, raw recommendation, committed, ready, signals and reasons.",
				"The quota-shortfall scenario clears its pending cohort through the pendingTimeoutSeconds (60 s) timeout rollback (ResourceExhausted) back to the last committed count while recommendations still exceed the quota; the later demand drop only prevents re-attempts. The fixture contains no quota-raise or capacity-release event, so recovery from an enlarged quota stays unverified.",
				"Synthetic local fixture only; nothing here is verified on real hardware.",
			},
		},
	}
	scenarios, arms := comparisonScenarios(), comparisonArms()
	timelineCapacity := 0
	for _, scenario := range scenarios {
		samples := int(math.Round(10 / scenario.StepSeconds))
		timelineCapacity += len(arms) * (int(math.Round(scenario.DurationSeconds/scenario.StepSeconds))/samples + 1)
	}
	timelinePoints := make([]comparisonTimelinePoint, 0, timelineCapacity)
	for _, scenario := range scenarios {
		result := comparisonScenarioResult{Name: scenario.Name, Holdout: scenario.Holdout, Seed: scenario.Traffic.Seed}
		for _, arm := range arms {
			run := runScalerSimulation(t, scenario, arm)
			assertScalerSimulationSafety(t, scenario, arm, run)
			assertComparisonArmsObservedAndAdjusted(t, scenario, arm, run)
			if scenario.Name == "quota-shortfall" {
				assertComparisonQuotaShortfall(t, arm, run)
			}
			decisions := map[string]int{}
			for _, point := range run.Trace {
				if point.CapacityDecision != "" {
					decisions[point.CapacityDecision]++
				}
			}
			timelinePoints = append(timelinePoints, comparisonTimeline(scenario, arm, run)...)
			var approximateSLO *float64
			if run.Summary.Offered > 0 {
				approximateSLO = ptr.To(comparisonSLO(run.Summary))
			}
			result.Arms = append(result.Arms, comparisonArmResult{
				Arm: arm.Name, Shadow: arm.Shadow,
				Summary:                run.Summary,
				ApproximateSLOFraction: approximateSLO,
				TrueSLO:                newComparisonTrueSLO(run.Traces, run.Summary.Offered, scenario.DurationSeconds, scenario.Plant.TTFTSLOSeconds, scenario.Plant.ITLSLOSeconds),
				ReplicaSeconds:         run.RequestedObjectReplicaSeconds,
				Changes:                run.Changes, ChangesPerMin: run.Response.ChangesPerMinute,
				Retired: run.Retired, Aborted: run.Aborted,
				Response:       run.Response,
				Decisions:      decisions,
				True:           newComparisonTrueLatency(run.Traces),
				Observed:       comparisonObservedLatency(run.Trace),
				TimelinePoints: len(run.Trace),
			})
		}
		report.Scenarios = append(report.Scenarios, result)
	}
	assertComparisonInvariants(t, report)
	path, err := writeComparisonReport(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("comparison report written to %s", path)
	timelinePath := writeComparisonTimeline(t, timelinePoints)
	t.Logf("comparison timeline written to %s", timelinePath)
}

func comparisonTimeline(scenario sim.Scenario, arm scalerSimulationArm, run scalerSimulationResult) []comparisonTimelinePoint {
	points := make([]comparisonTimelinePoint, 0, len(run.Trace))
	for i, point := range run.Trace {
		change := ""
		if i > 0 {
			switch {
			case point.Committed > run.Trace[i-1].Committed:
				change = "scale-up"
			case point.Committed < run.Trace[i-1].Committed:
				change = "scale-down"
			}
		}
		var capacity *float64
		if point.TokenCapacityConfigured {
			capacity = ptr.To(point.EffectiveCapacity)
		}
		points = append(points, comparisonTimelinePoint{
			Scenario: scenario.Name, Arm: arm.Name, Shadow: arm.Shadow,
			Seconds: point.Seconds, OfferedCumulative: point.Offered,
			RawRecommended: point.Raw, Committed: point.Committed, Ready: point.Ready,
			Coverage:        point.Coverage,
			ThroughputState: string(point.Signal), LatencyState: string(point.LatencyState), GaugeState: string(point.GaugeState),
			ProcessedTPS: point.ProcessedTPS, ObservedTTFTMilliseconds: point.ObservedTTFTMilliseconds,
			ObservedITLMilliseconds: point.ObservedITLMilliseconds, ObservedWaiting: point.ObservedWaiting,
			PlantQueue: point.Queue, Pending: point.Pending,
			BlockedReason: point.BlockedReason, Retirement: point.Retirement,
			EffectiveCapacity: capacity, Change: change,
		})
	}
	return points
}

func writeComparisonTimeline(t *testing.T, points []comparisonTimelinePoint) string {
	t.Helper()
	var b strings.Builder
	for _, point := range points {
		payload, err := json.Marshal(point)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(payload)
		b.WriteByte('\n')
	}
	path := filepath.Join(os.TempDir(), "gpustack-three-algorithm-timeline.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func comparisonSLO(summary sim.Summary) float64 {
	if summary.Offered == 0 {
		return 1
	}
	return float64(summary.SLOCompliant) / float64(summary.Offered)
}

// assertComparisonArmsObservedAndAdjusted proves the comparison is not vacuous:
// in every loaded scenario each arm receives fresh throughput and latency
// signals and its algorithm recommends growth; only the Shadow control never
// commits a change.
func assertComparisonArmsObservedAndAdjusted(t *testing.T, scenario sim.Scenario, arm scalerSimulationArm, run scalerSimulationResult) {
	t.Helper()
	if scenario.Name == "idle" {
		return
	}
	freshThroughput, freshLatency, freshGauge, growth := 0, 0, 0, 0
	for _, point := range run.Trace {
		if point.Signal == algorithm.SignalFresh {
			freshThroughput++
		}
		if point.LatencyState == algorithm.SignalFresh {
			freshLatency++
		}
		if point.GaugeState == algorithm.SignalFresh {
			freshGauge++
		}
		if point.Raw > 1 {
			growth++
		}
	}
	if freshThroughput == 0 || freshLatency == 0 || freshGauge == 0 {
		t.Fatalf("scenario %s arm %s never received fresh signals: throughput %d, latency %d, gauge %d", scenario.Name, arm.Name, freshThroughput, freshLatency, freshGauge)
	}
	if growth == 0 {
		t.Fatalf("scenario %s arm %s never recommended growth above one replica", scenario.Name, arm.Name)
	}
	if !arm.Shadow && run.Changes == 0 {
		t.Fatalf("scenario %s arm %s never committed a replica change", scenario.Name, arm.Name)
	}
}

// assertComparisonQuotaShortfall pins the quota-blocked dynamics: the fleet
// quota sits below the scaler maximum, so the overload phase must strand
// desired replicas in the pending cohort with admission pinned at the quota
// and a ResourceExhausted block, and the pending cohort must clear through the
// pendingTimeoutSeconds timeout rollback back to four replicas while the raw
// recommendation still exceeds the quota. The later demand drop only keeps the
// recommendation inside the quota; the fixture contains no quota raise and
// therefore no capacity release.
func assertComparisonQuotaShortfall(t *testing.T, arm scalerSimulationArm, run scalerSimulationResult) {
	t.Helper()
	if arm.Shadow {
		return
	}
	pendingSeen, pinned, exhausted, cleared := false, 0, 0, false
	for _, point := range run.Trace {
		if point.BlockedReason == "ResourceExhausted" {
			exhausted++
		}
		if point.Pending {
			pendingSeen = true
			if point.Ready == 4 {
				pinned++
			}
		} else if pendingSeen && point.Raw > 4 {
			cleared = true
		}
	}
	if !pendingSeen || pinned == 0 || exhausted == 0 {
		t.Fatalf("scenario quota-shortfall arm %s never pended at the quota: pending %t, pinned %d, ResourceExhausted %d", arm.Name, pendingSeen, pinned, exhausted)
	}
	if !cleared {
		t.Fatalf("scenario quota-shortfall arm %s pending cohort never cleared while the raw recommendation still exceeded the quota", arm.Name)
	}
	if last := run.Trace[len(run.Trace)-1]; last.Raw > 4 {
		t.Fatalf("scenario quota-shortfall arm %s ends with recommendation %d above the quota", arm.Name, last.Raw)
	}
}

// comparisonTrueSLO holds compliance derived from recorded whole-token
// timestamps under the semantics declared in the report notes.
type comparisonTrueSLO struct {
	Semantics string   `json:"semantics"`
	Compliant int      `json:"compliantRequests"`
	Fraction  *float64 `json:"fractionOfOffered"`
	Goodput   *float64 `json:"goodputPerSecond"`
}

func newComparisonTrueSLO(traces []sim.TokenTrace, offered int, duration, ttftSLO, itlSLO float64) *comparisonTrueSLO {
	out := &comparisonTrueSLO{
		Semantics: "per completed request: recorded TTFT within the TTFT target and every adjacent token ITL within the ITL target; single-token requests satisfy the ITL constraint vacuously; failed and outstanding requests count as non-compliant; the fraction denominator is Offered",
	}
	for _, trace := range traces {
		if comparisonTraceSLO(trace, ttftSLO, itlSLO) {
			out.Compliant++
		}
	}
	if offered > 0 {
		fraction := float64(out.Compliant) / float64(offered)
		goodput := float64(out.Compliant) / duration
		out.Fraction, out.Goodput = &fraction, &goodput
	}
	return out
}

func comparisonTraceSLO(trace sim.TokenTrace, ttftSLO, itlSLO float64) bool {
	if trace.Failed {
		return false
	}
	ttft, ok := trace.TTFTSeconds()
	if !ok || ttft > ttftSLO {
		return false
	}
	for _, gap := range trace.ITLSeconds() {
		if gap > itlSLO {
			return false
		}
	}
	return true
}

func newComparisonTrueLatency(traces []sim.TokenTrace) *comparisonTrueLatency {
	out := &comparisonTrueLatency{}
	var ttft, tpot, itl []float64
	for _, trace := range traces {
		if value, ok := trace.TTFTSeconds(); ok {
			out.TTFTSamples++
			ttft = append(ttft, value*1000)
		}
		if value, ok := trace.TPOTSeconds(); ok {
			out.TPOTSamples++
			tpot = append(tpot, value*1000)
		}
		for _, value := range trace.ITLSeconds() {
			itl = append(itl, value*1000)
		}
	}
	out.ITLSamples = len(itl)
	out.TTFTP50, out.TTFTP95, out.TTFTMax = comparisonQuantilePointer(ttft, .5), comparisonQuantilePointer(ttft, .95), comparisonQuantilePointer(ttft, 1)
	out.TPOTP50, out.TPOTP95, out.TPOTMax = comparisonQuantilePointer(tpot, .5), comparisonQuantilePointer(tpot, .95), comparisonQuantilePointer(tpot, 1)
	out.ITLP50, out.ITLP95, out.ITLMax = comparisonQuantilePointer(itl, .5), comparisonQuantilePointer(itl, .95), comparisonQuantilePointer(itl, 1)
	return out
}

func comparisonObservedLatency(points []scalerSimulationPoint) *comparisonObserved {
	out := &comparisonObserved{}
	for _, point := range points {
		if point.ObservedTTFTMilliseconds != nil {
			out.Points++
			out.TTFTMax = math.Max(out.TTFTMax, *point.ObservedTTFTMilliseconds)
			out.ITLMax = math.Max(out.ITLMax, *point.ObservedITLMilliseconds)
		}
		if point.ObservedWaiting != nil {
			out.WaitingPoints++
			out.WaitingMax = math.Max(out.WaitingMax, *point.ObservedWaiting)
		}
	}
	return out
}

func comparisonQuantile(values []float64, q float64) float64 {
	return sim.Quantile(values, q)
}

// comparisonQuantilePointer returns nil instead of a zero when the population
// is empty, so an unobserved quantile never serializes as a measurement.
func comparisonQuantilePointer(values []float64, q float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	value := comparisonQuantile(values, q)
	return &value
}

func assertComparisonInvariants(t *testing.T, report comparisonReport) {
	t.Helper()
	if len(report.Scenarios) != len(comparisonScenarios()) {
		t.Fatalf("report has %d scenarios, want %d", len(report.Scenarios), len(comparisonScenarios()))
	}
	for _, scenario := range report.Scenarios {
		if len(scenario.Arms) != len(comparisonArms()) {
			t.Fatalf("scenario %s has %d arms, want %d", scenario.Name, len(scenario.Arms), len(comparisonArms()))
		}
		offered := scenario.Arms[0].Summary.Offered
		for _, arm := range scenario.Arms {
			if arm.Summary.Offered != offered {
				t.Fatalf("scenario %s arm %s offered %d, shared traffic offered %d", scenario.Name, arm.Arm, arm.Summary.Offered, offered)
			}
			if arm.Summary.Offered != arm.Summary.Completed+arm.Summary.Failed+arm.Summary.Outstanding {
				t.Fatalf("scenario %s arm %s request accounting: %+v", scenario.Name, arm.Arm, arm.Summary)
			}
			if arm.Shadow && arm.Changes != 0 {
				t.Fatalf("scenario %s shadow control committed changes: %+v", scenario.Name, arm)
			}
			if arm.TrueSLO == nil || arm.True == nil || arm.Observed == nil || arm.TimelinePoints == 0 {
				t.Fatalf("scenario %s arm %s is missing labeled measurements: %+v", scenario.Name, arm.Arm, arm)
			}
			if scenario.Name != "idle" && (arm.True.TTFTSamples == 0 || arm.True.TPOTSamples == 0 || arm.True.ITLSamples == 0) {
				t.Fatalf("scenario %s arm %s recorded incomplete true latency samples: ttft %d, tpot %d, itl %d", scenario.Name, arm.Arm, arm.True.TTFTSamples, arm.True.TPOTSamples, arm.True.ITLSamples)
			}
		}
	}
}

func writeComparisonReport(report comparisonReport) (string, error) {
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(os.TempDir(), "gpustack-three-algorithm-comparison-report.json")
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
