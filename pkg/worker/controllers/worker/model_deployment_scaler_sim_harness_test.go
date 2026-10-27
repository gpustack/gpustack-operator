package worker

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/sim"
)

type scalerSimulationArm struct {
	Name           string
	Shadow         bool
	Predictive     bool
	CapacityFactor float64
	// RecordTokens opts this arm's plant into whole-token recording for the
	// opt-in comparison mode; the frozen baseline arms leave it unset.
	RecordTokens bool
	// QuantizeTokens rounds jittered arrivals to whole tokens for the opt-in
	// comparison mode; the frozen baseline arms leave it unset.
	QuantizeTokens bool
	InitialObjects []ctrlcli.Object
	Configure      func(*workercore.ModelDeployment, *workercore.ModelDeploymentScaler)
	AfterSample    func(scalerSimulationSample) scalerSimulationCapacityDecision
}

// scalerSimulationSample exposes observed metrics and lifecycle state, never plant truth.
type scalerSimulationSample struct {
	Now      time.Time
	Sample   modelDeploymentScalerRoleSample
	Pending  bool
	Retiring bool
}

type scalerSimulationCapacityDecision struct {
	Capacity *float64
	Reason   string
}

type scalerSimulationPoint struct {
	Seconds           float64
	Raw               int32
	Committed         int32
	Ready             int
	Coverage          bool
	Signal            algorithm.SignalState
	ProcessedTPS      *float64
	Pending           bool
	BlockedReason     string
	Retirement        string
	Queue             int
	EffectiveCapacity float64
	CapacityDecision  string
	// Observed values are algorithm inputs read from the per-step approximate
	// exposition, never true per-token statistics. An absent reading stays nil
	// instead of a zero.
	ObservedTTFTMilliseconds *float64
	ObservedITLMilliseconds  *float64
	ObservedWaiting          *float64
	LatencyState             algorithm.SignalState
	GaugeState               algorithm.SignalState
	// Offered is the cumulative enqueue count at the sample instant.
	Offered int
	// TokenCapacityConfigured marks a scaler whose configured per-replica token
	// capacity is recorded in EffectiveCapacity; a configured capacity is not
	// applicable to every algorithm.
	TokenCapacityConfigured bool
}

type scalerSimulationResult struct {
	FinalScaler *workercore.ModelDeploymentScaler `json:"-"`
	Summary     sim.Summary
	Requests    []sim.RequestResult
	// Traces carry exact whole-token timestamps only in the opt-in recorded
	// mode; trace count is never the offered denominator.
	Traces  []sim.TokenTrace
	Trace   []scalerSimulationPoint
	Changes int
	// RequestedObjectReplicaSeconds includes pending and retiring objects, not GPU allocation cost.
	RequestedObjectReplicaSeconds float64
	Retired                       int
	Aborted                       int
	Response                      scalerSimulationResponse
}

type scalerSimulationResponse struct {
	LoadEventAtSeconds *float64
	Baseline           *scalerSimulationBaseline
	// Growth times are elapsed seconds from the load event; nil means unobserved.
	FirstRecommendationGrowthSeconds *float64
	FirstCommitGrowthSeconds         *float64
	FirstReadyGrowthSeconds          *float64
	SettledAtSeconds                 *float64
	PeakRaw                          int32
	PeakCommitted                    int32
	PeakReady                        int
	ChangesPerMinute                 float64
}

type scalerSimulationBaseline struct {
	AtSeconds float64
	Raw       int32
	Committed int32
	Ready     int
}

// runScalerSimulation is the shared local experiment seam. It always calls the real reconciler.
// The fake lifecycle observations validate controller reactions, not Kueue or drain behavior.
func runScalerSimulation(t *testing.T, scenario sim.Scenario, arm scalerSimulationArm) scalerSimulationResult {
	t.Helper()
	md := scalerTestDeployment("target", "simulation-target", map[string]int32{"server": int32(scenario.Fleet.Initial)})
	md.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	if scenario.Engine == "sglang" {
		md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
	}
	md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
	scaler := scalerSimulationScaler(arm)
	if arm.Configure != nil {
		arm.Configure(md, scaler)
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}, &core.Pod{}, &kueue.Workload{}).
		WithObjects(md, scaler).WithObjects(arm.InitialObjects...).Build()
	traffic := sim.NewTraffic(scenario.Traffic)
	plant := sim.NewPlant(scenario.Plant)
	recorder := sim.NewTokenTruthRecorder()
	if arm.RecordTokens {
		plant.SetTokenRecorder(recorder.Recorder())
	}
	fleet := sim.NewFleet(scenario.Fleet)
	epoch := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	now := 0.0
	clock := func() time.Time { return epoch.Add(time.Duration(now * float64(time.Second))) }
	newReconciler := func() *ModelDeploymentScalerReconciler {
		sampler := &modelDeploymentScalerSampler{Clock: clock}
		sampler.Scrape = func(_ context.Context, pod *core.Pod) ([]byte, error) {
			ordinal, err := strconv.Atoi(pod.Labels[modelDeploymentReplicaOrdinalLabel])
			if err != nil {
				return nil, err
			}
			for _, fault := range scenario.Faults {
				if now >= fault.FromSeconds && now < fault.UntilSeconds && (fault.Ordinal < 0 || ordinal == fault.Ordinal) {
					return nil, fmt.Errorf("synthetic scrape outage")
				}
			}
			return plant.Exposition(ordinal, scenario.Engine, len(fleet.Ready()))
		}
		return &ModelDeploymentScalerReconciler{Client: client, clock: clock, sampler: sampler}
	}
	r := newReconciler()
	result := scalerSimulationResult{}
	reset, restarted, aborted := false, false, false
	previous := int32(scenario.Fleet.Initial)
	steps := int(math.Round(scenario.DurationSeconds / scenario.StepSeconds))
	sampleTicks := int(math.Round(10 / scenario.StepSeconds))
	for tick := 0; tick <= steps; tick++ {
		now = float64(tick) * scenario.StepSeconds
		fleet.Advance(now)
		plant.RetainReplicas(now, fleet.Ordinals())
		if scenario.ResetAtSeconds > 0 && now >= scenario.ResetAtSeconds && !reset {
			plant.ResetCounters()
			reset = true
		}
		if scenario.RestartAtSeconds > 0 && now >= scenario.RestartAtSeconds && !restarted {
			r = newReconciler()
			restarted = true
		}
		if tick%sampleTicks == 0 {
			plant.Step(now, 0, fleet.Ready(), fleet.Draining()...)
			syncScalerSimulationRole(t, client, fleet, "server", scenario.AbortRetirement, &aborted)
			reconcileScaler(t, r, scaler.Name)
			storedMD := scalerTestGetDeployment(t, client)
			stored := scalerTestGetScaler(t, client, scaler.Name)
			desired := storedMD.Spec.Roles[0].Replicas
			if desired != previous {
				result.Changes++
				storedMD.Generation++
				if err := client.Update(context.Background(), storedMD); err != nil {
					t.Fatal(err)
				}
			}
			fleet.SetDesired(int(desired), now)
			sample := r.sampler.RoleSamples(stored)["server"]
			observation := sample.Observation
			point := scalerSimulationPoint{
				Seconds: now, Committed: desired, Ready: len(fleet.Ready()), Coverage: observation.CompleteCoverage,
				Signal: observation.ThroughputState, Pending: stored.Status.PendingCohort != nil, Queue: plant.Outstanding(),
			}
			if len(stored.Status.Roles) > 0 {
				point.Raw = stored.Status.Roles[0].RawRecommendedReplicas
			}
			if observation.ThroughputState == algorithm.SignalFresh || observation.ThroughputState == algorithm.SignalIdle {
				value := observation.PromptTokensPerSecond + observation.GenerationTokensPerSecond
				point.ProcessedTPS = &value
			}
			if observation.LatencyState == algorithm.SignalFresh || observation.LatencyState == algorithm.SignalIdle {
				point.ObservedTTFTMilliseconds, point.ObservedITLMilliseconds = ptr.To(observation.TTFTMilliseconds), ptr.To(observation.ITLMilliseconds)
			}
			if observation.GaugeState == algorithm.SignalFresh || observation.GaugeState == algorithm.SignalIdle {
				point.ObservedWaiting = ptr.To(observation.WaitingRequests)
			}
			point.LatencyState = observation.LatencyState
			point.GaugeState = observation.GaugeState
			point.Offered = plant.Offered()
			condition := scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeBlocked)
			if condition.Status == meta.ConditionTrue {
				point.BlockedReason = condition.Reason
			}
			if storedMD.Status.Retirement != nil {
				point.Retirement = string(storedMD.Status.Retirement.State)
			}
			parameters := stored.Spec.Algorithm.TokenThroughput
			if stored.Spec.Algorithm.Predictive != nil {
				parameters = &stored.Spec.Algorithm.Predictive.TokenThroughput
			}
			if parameters != nil && parameters.ServerTargetTokensPerSecond != nil {
				point.EffectiveCapacity = parameters.ServerTargetTokensPerSecond.AsApproximateFloat64()
				point.TokenCapacityConfigured = true
			}
			if arm.AfterSample != nil {
				decision := arm.AfterSample(scalerSimulationSample{
					Now: clock(), Sample: sample, Pending: point.Pending, Retiring: point.Retirement != "",
				})
				point.CapacityDecision = decision.Reason
				if decision.Capacity != nil {
					value := *decision.Capacity
					if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
						t.Fatalf("invalid experiment capacity %v", value)
					}
					parameters.ServerTargetTokensPerSecond = quantityPointer(strconv.FormatFloat(value, 'f', 6, 64))
					if stored.Spec.Algorithm.Predictive != nil {
						stored.Spec.Algorithm.Predictive.ServerTokenCapacityPerReplica = parameters.ServerTargetTokensPerSecond
					}
					if err := client.Update(context.Background(), stored); err != nil {
						t.Fatal(err)
					}
				}
			}
			result.Trace = append(result.Trace, point)
			previous = desired
			assertScalerSimulationStateBound(t, r, stored)
			if arm.Shadow && (storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey] != "" || storedMD.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey] != "") {
				t.Fatal("Shadow mutated target annotations")
			}
		}
		if tick == steps {
			break
		}
		offered := traffic.Arrivals(now, scenario.StepSeconds, plant.Outstanding())
		if arm.QuantizeTokens {
			offered = sim.QuantizeArrivals(offered)
		}
		for _, request := range offered {
			plant.Offer(request)
		}
		plant.Step(now, scenario.StepSeconds, fleet.Ready(), fleet.Draining()...)
		result.RequestedObjectReplicaSeconds += float64(len(fleet.Replicas())) * scenario.StepSeconds
	}
	result.Summary = plant.Summary(scenario.DurationSeconds)
	result.Requests = plant.Results()
	if arm.RecordTokens {
		result.Traces = recorder.Track(result.Requests)
	}
	result.Retired = fleet.Retired
	result.Aborted = fleet.Aborted
	result.Response = measureScalerSimulationResponse(scenario, result)
	result.FinalScaler = scalerTestGetScaler(t, client, scaler.Name)
	return result
}

func measureScalerSimulationResponse(scenario sim.Scenario, result scalerSimulationResult) scalerSimulationResponse {
	response := scalerSimulationResponse{}
	if scenario.DurationSeconds > 0 {
		response.ChangesPerMinute = float64(result.Changes) * 60 / scenario.DurationSeconds
	}
	offeredWork := func(phase sim.Phase) float64 {
		return phase.RequestsPerSecond * (phase.InputTokens*(1-phase.CacheFraction) + phase.OutputTokens)
	}
	for i := 1; i < len(scenario.Traffic.Phases); i++ {
		previous, next := scenario.Traffic.Phases[i-1], scenario.Traffic.Phases[i]
		if offeredWork(next) > offeredWork(previous) {
			seconds := previous.UntilSeconds
			response.LoadEventAtSeconds = &seconds
			break
		}
	}
	if response.LoadEventAtSeconds != nil {
		for _, point := range result.Trace {
			if point.Seconds >= *response.LoadEventAtSeconds {
				break
			}
			response.Baseline = &scalerSimulationBaseline{AtSeconds: point.Seconds, Raw: point.Raw, Committed: point.Committed, Ready: point.Ready}
		}
	}
	quietStart := 0.0
	if len(scenario.Traffic.Phases) > 1 {
		quietStart = scenario.Traffic.Phases[len(scenario.Traffic.Phases)-2].UntilSeconds
	}
	for _, point := range result.Trace {
		seconds := point.Seconds
		if response.Baseline != nil && point.Seconds >= *response.LoadEventAtSeconds {
			elapsed := point.Seconds - *response.LoadEventAtSeconds
			if response.FirstRecommendationGrowthSeconds == nil && point.Raw > response.Baseline.Raw {
				response.FirstRecommendationGrowthSeconds = &elapsed
			}
			if response.FirstCommitGrowthSeconds == nil && point.Committed > response.Baseline.Committed {
				response.FirstCommitGrowthSeconds = &elapsed
			}
			if response.FirstReadyGrowthSeconds == nil && point.Ready > response.Baseline.Ready {
				response.FirstReadyGrowthSeconds = &elapsed
			}
		}
		if point.Seconds >= quietStart {
			if point.Committed == 1 && point.Ready == 1 && point.Queue == 0 && point.Retirement == "" && !point.Pending {
				if response.SettledAtSeconds == nil {
					response.SettledAtSeconds = &seconds
				}
			} else {
				response.SettledAtSeconds = nil
			}
		}
		response.PeakRaw = max(response.PeakRaw, point.Raw)
		response.PeakCommitted = max(response.PeakCommitted, point.Committed)
		response.PeakReady = max(response.PeakReady, point.Ready)
	}
	return response
}

func scalerSimulationScaler(arm scalerSimulationArm) *workercore.ModelDeploymentScaler {
	mode := workercore.ModelDeploymentScalerModeActive
	if arm.Shadow {
		mode = workercore.ModelDeploymentScalerModeShadow
	}
	scaler := scalerTestScaler("scaler", "simulation-scaler", mode)
	scaler.Spec.Roles[0].MaxReplicas = 6
	scaler.Spec.Sampling = workercore.ModelDeploymentScalerSamplingSpec{IntervalSeconds: 10, PendingTimeoutSeconds: 60}
	scaler.Spec.Behavior.ScaleUp.MaxStepReplicas = 1
	scaler.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = 60
	scaler.Spec.Behavior.ScaleDown.CooldownAfterAbortSeconds = 90
	factor := arm.CapacityFactor
	if factor == 0 {
		factor = 1
	}
	capacity := quantityPointer(strconv.FormatFloat(300*factor, 'f', 4, 64))
	parameters := workercore.ModelDeploymentScalerTokenThroughputSpec{
		ServerTargetTokensPerSecond: capacity, StableWindowSeconds: 30, PanicWindowSeconds: 10,
		TTFTTargetMilliseconds: quantityPointer("2000"), ITLTargetMilliseconds: quantityPointer("100"),
	}
	scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
		Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput, TokenThroughput: &parameters,
	}
	if arm.Predictive {
		scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
			Type:       workercore.ModelDeploymentScalerAlgorithmTypePredictive,
			Predictive: &workercore.ModelDeploymentScalerPredictiveSpec{TokenThroughput: parameters, ServerTokenCapacityPerReplica: capacity, HorizonSeconds: 30, MinPoints: 4},
		}
	}
	return scaler
}

func syncScalerSimulationRole(t *testing.T, client ctrlcli.Client, fleet *sim.Fleet, role string, abort bool, aborted *bool) {
	t.Helper()
	ctx := context.Background()
	md := scalerTestGetDeployment(t, client)
	controller := true
	keep := map[string]bool{}
	var retirement *workercore.ModelDeploymentRetirementStatus
	for _, replica := range fleet.Replicas() {
		name := fmt.Sprintf("sim-%s-%d", role, replica.Ordinal)
		keep[name] = true
		pod := scalerSamplerPod(name, role, types.UID(fmt.Sprintf("%s-%.0f", name, replica.CreatedAt)), replica.Ordinal)
		pod.Namespace = md.Namespace
		pod.Labels[modelDeploymentLabelKeyName] = modelDeploymentLabelValueName
		pod.Labels[modelDeploymentLabelKeyInstance] = md.Name
		group := modelDeploymentReplicaGroupName(md, role, replica.Ordinal)
		pod.Labels[kueuepodconst.GroupNameLabel] = group
		pod.OwnerReferences = []meta.OwnerReference{{APIVersion: workercore.SchemeGroupVersion.String(), Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: &controller}}
		if !replica.Ready || replica.Retiring {
			pod.Status.Conditions[0].Status = core.ConditionFalse
			pod.Status.ContainerStatuses[0].Ready = false
		}
		scalerSimulationUpsertPod(t, client, &pod)
		wl := &kueue.Workload{ObjectMeta: meta.ObjectMeta{Namespace: md.Namespace, Name: group, Labels: pod.Labels, OwnerReferences: []meta.OwnerReference{{Kind: "Pod", Name: name, UID: pod.UID}}}}
		if replica.Admitted {
			wl.Status.Conditions = []meta.Condition{{Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue, Reason: "SyntheticAdmission"}, {Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue, Reason: "SyntheticAdmission"}}
		}
		scalerSimulationUpsertWorkload(t, client, wl)
		if replica.Retiring {
			retirement = &workercore.ModelDeploymentRetirementStatus{RoleName: role, ReplicaOrdinal: int32(replica.Ordinal), State: workercore.ModelDeploymentRetirementStateDraining}
			if abort && !*aborted {
				retirement.State = workercore.ModelDeploymentRetirementStateAborted
				retirement.Reason = "SyntheticDrainTimeout"
				*aborted = true
			}
		}
	}
	pods := new(core.PodList)
	if err := client.List(ctx, pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if modelDeploymentPodRole(pod) != role || keep[pod.Name] {
			continue
		}
		if err := client.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
		wl := &kueue.Workload{ObjectMeta: meta.ObjectMeta{Namespace: md.Namespace, Name: pod.Labels[kueuepodconst.GroupNameLabel]}}
		if err := client.Delete(ctx, wl); err != nil {
			t.Fatal(err)
		}
	}
	if retirement != nil || md.Status.Retirement == nil || md.Status.Retirement.RoleName == role {
		md.Status.Retirement = retirement
	}
	if err := client.Status().Update(ctx, md); err != nil {
		t.Fatal(err)
	}
}

func scalerSimulationUpsertPod(t *testing.T, client ctrlcli.Client, pod *core.Pod) {
	t.Helper()
	ctx := context.Background()
	existing := new(core.Pod)
	err := client.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), existing)
	if apierrors.IsNotFound(err) {
		if err = client.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if existing.UID != pod.UID {
		if err = client.Delete(ctx, existing); err != nil {
			t.Fatal(err)
		}
		if err = client.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		return
	}
	existing.Status = pod.Status
	if err = client.Status().Update(ctx, existing); err != nil {
		t.Fatal(err)
	}
}

func scalerSimulationUpsertWorkload(t *testing.T, client ctrlcli.Client, workload *kueue.Workload) {
	t.Helper()
	ctx := context.Background()
	existing := new(kueue.Workload)
	err := client.Get(ctx, ctrlcli.ObjectKeyFromObject(workload), existing)
	if apierrors.IsNotFound(err) {
		if err = client.Create(ctx, workload); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	existing.OwnerReferences = workload.OwnerReferences
	existing.Labels = workload.Labels
	if err = client.Update(ctx, existing); err != nil {
		t.Fatal(err)
	}
	existing.Status = workload.Status
	if err = client.Status().Update(ctx, existing); err != nil {
		t.Fatal(err)
	}
}

func assertScalerSimulationStateBound(t *testing.T, r *ModelDeploymentScalerReconciler, scaler *workercore.ModelDeploymentScaler) {
	t.Helper()
	if len(r.sampler.rings) > 1 {
		t.Fatal("sampler state leaked between targets")
	}
	for _, role := range scaler.Spec.Roles {
		if len(r.sampler.StabilizationHistory(scaler, role.Name)) > modelDeploymentScalerMaxStabilizationSamples {
			t.Fatal("unbounded stabilization state")
		}
	}
}

func assertScalerSimulationSafety(t *testing.T, scenario sim.Scenario, arm scalerSimulationArm, result scalerSimulationResult) {
	t.Helper()
	s := result.Summary
	if s.Offered != s.Completed+s.Failed+s.Outstanding {
		t.Fatalf("request accounting: %+v", s)
	}
	for _, request := range result.Requests {
		for _, value := range []float64{request.ArrivedAt, request.FinishedAt, request.TTFTSeconds, request.ITLSeconds} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				t.Fatalf("nonfinite request truth: %+v", request)
			}
		}
		if request.FinishedAt < request.ArrivedAt || (request.TTFTSeconds < 0 && request.Compliant) {
			t.Fatalf("invalid request outcome: %+v", request)
		}
	}
	previous := int32(scenario.Fleet.Initial)
	for _, point := range result.Trace {
		if point.Committed < 1 || point.Committed > 6 || point.Raw < 0 {
			t.Fatalf("invalid replica count: %+v", point)
		}
		if point.Ready > scenario.Fleet.Quota {
			t.Fatalf("impossible readiness: %+v", point)
		}
		if point.Committed > previous+1 {
			t.Fatalf("growth pacing violated: %+v", point)
		}
		if arm.Shadow && point.Committed != int32(scenario.Fleet.Initial) {
			t.Fatalf("Shadow wrote desired replicas: %+v", point)
		}
		if !point.Coverage && point.Committed < previous && point.BlockedReason != workercore.ModelDeploymentScalerReasonResourceExhausted {
			t.Fatalf("incomplete coverage downscale: %+v", point)
		}
		if point.ProcessedTPS != nil && (math.IsNaN(*point.ProcessedTPS) || math.IsInf(*point.ProcessedTPS, 0) || *point.ProcessedTPS < 0) {
			t.Fatalf("invalid token rate: %+v", point)
		}
		previous = point.Committed
	}
}
