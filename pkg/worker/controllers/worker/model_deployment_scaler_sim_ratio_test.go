package worker

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/sim"
)

func TestModelDeploymentScalerSimulationCoupledRoles(t *testing.T) {
	md := scalerTestDeployment("target", "coupled-simulation", map[string]int32{"prefill": 1, "decode": 1})
	md.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
	md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
	scaler := scalerTestCoupledScaler()
	scaler.Spec.Behavior.ScaleUp.MaxStepReplicas = 1
	scaler.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = 30
	scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
		Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
		TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
			PrefillTargetTokensPerSecond: quantityPointer("100"), DecodeTargetTokensPerSecond: quantityPointer("100"),
			StableWindowSeconds: 30, PanicWindowSeconds: 10,
		},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}, &core.Pod{}, &kueue.Workload{}).
		WithObjects(md, scaler).Build()
	plants := map[string]*sim.Plant{}
	fleets := map[string]*sim.Fleet{}
	traffic := map[string]*sim.Traffic{}
	roles := []string{"prefill", "decode"}
	for _, role := range roles {
		plants[role] = sim.NewPlant(sim.PlantConfig{TokensPerSecond: 400, BatchSize: 4, TimeoutSeconds: 60, TTFTSLOSeconds: 2, ITLSLOSeconds: .1})
		fleets[role] = sim.NewFleet(sim.FleetConfig{Initial: 1, Quota: 4, BootSeconds: 10, RetirementSeconds: 30})
		traffic[role] = sim.NewTraffic(sim.TrafficConfig{Seed: 43, Phases: []sim.Phase{
			{UntilSeconds: 40, RequestsPerSecond: .8, InputTokens: 50, OutputTokens: 50},
			{UntilSeconds: 120, RequestsPerSecond: 5, InputTokens: 50, OutputTokens: 50},
		}})
	}
	now := 0.0
	clock := func() time.Time { return time.Unix(100000, 0).Add(time.Duration(now * float64(time.Second))) }
	r := &ModelDeploymentScalerReconciler{Client: client, clock: clock, sampler: &modelDeploymentScalerSampler{Clock: clock}}
	r.sampler.Scrape = func(_ context.Context, pod *core.Pod) ([]byte, error) {
		role := modelDeploymentPodRole(pod)
		ordinal, err := strconv.Atoi(pod.Labels[modelDeploymentReplicaOrdinalLabel])
		if err != nil {
			return nil, err
		}
		return plants[role].Exposition(ordinal, "vllm", len(fleets[role].Ready()))
	}
	grew := false
	var retirementSamples []float64
	aborted := false
	for tick := 0; tick <= 1200; tick++ {
		now = float64(tick) * .25
		for _, role := range roles {
			fleet, plant := fleets[role], plants[role]
			fleet.Advance(now)
			plant.RetainReplicas(now, fleet.Ordinals())
			plant.Step(now, 0, fleet.Ready(), fleet.Draining()...)
			if tick%40 == 0 {
				syncScalerSimulationRole(t, client, fleet, role, false, &aborted)
			}
		}
		if tick%40 == 0 {
			before := scalerTestGetDeployment(t, client)
			retiring := before.Status.Retirement != nil && before.Status.Retirement.State == workercore.ModelDeploymentRetirementStateDraining
			if retiring {
				retirementSamples = append(retirementSamples, now)
			}
			reconcileScaler(t, r, scaler.Name)
			stored := scalerTestGetDeployment(t, client)
			vector := deploymentReplicaVector(stored)
			if vector["prefill"] != vector["decode"] || vector["prefill"] < 1 || vector["prefill"] > 4 {
				t.Fatalf("ratio or bounds broken at %.0f: %v", now, vector)
			}
			grew = grew || vector["prefill"] > 1
			for _, role := range roles {
				if retiring && vector[role] < deploymentReplicaVector(before)[role] {
					t.Fatalf("downscale during sampled retirement at %.0f: %v", now, vector)
				}
				fleets[role].SetDesired(int(vector[role]), now)
			}
		}
		for _, role := range roles {
			plant, fleet := plants[role], fleets[role]
			for _, request := range traffic[role].Arrivals(now, .25, plant.Outstanding()) {
				plant.Offer(request)
			}
			plant.Step(now, .25, fleet.Ready(), fleet.Draining()...)
		}
	}
	if !grew {
		t.Fatal("coupled controller never grew")
	}
	if len(retirementSamples) == 0 {
		t.Fatal("controller never sampled in-flight retirement")
	}
	t.Logf("sampled in-flight retirement at simulated seconds %v", retirementSamples)
	final := deploymentReplicaVector(scalerTestGetDeployment(t, client))
	if final["prefill"] != 1 || final["decode"] != 1 {
		t.Fatalf("coupled cooldown did not complete: %v", final)
	}
	for _, role := range roles {
		if len(fleets[role].Ready()) != 1 || len(fleets[role].Replicas()) != 1 || fleets[role].Retired == 0 {
			t.Fatalf("role %s did not finish retirement: %+v", role, fleets[role].Replicas())
		}
	}
}

func TestModelDeploymentScalerSimulationOpposingRebalance(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyOpposing)
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}, &core.Pod{}, &kueue.Workload{}).
		WithObjects(md, scaler).Build()
	r.Client = client
	fleet := sim.NewFleet(sim.FleetConfig{Initial: 2, Quota: 2, RetirementSeconds: 30})
	aborted := false
	syncScalerSimulationRole(t, client, fleet, "decode", false, &aborted)
	recommendations := map[string]algorithm.RoleRecommendation{"prefill": {DesiredReplicas: 2}, "decode": {DesiredReplicas: 1}}
	current := deploymentReplicaVector(md)
	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current, map[string]int32{"prefill": 2, "decode": 1}, recommendations, time.Unix(20, 0), true)
	if err != nil || reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 1, "decode": 1}) {
		t.Fatalf("opposing donor release = %v, reason=%q, err=%v", vector, reason, err)
	}
	md = scalerTestGetDeployment(t, client)
	for i := range md.Spec.Roles {
		md.Spec.Roles[i].Replicas = vector[md.Spec.Roles[i].Name]
	}
	if err = client.Update(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	fleet.SetDesired(1, 20)
	for _, tc := range []struct {
		name            string
		now             float64
		clearRetirement bool
		wantReceiver    int32
		wantWorkload    bool
	}{
		{name: "in-flight drain", now: 30, wantReceiver: 1, wantWorkload: true},
		{name: "reservation outlives status", now: 49, clearRetirement: true, wantReceiver: 1, wantWorkload: true},
		{name: "receiver after release", now: 50, wantReceiver: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fleet.Advance(tc.now)
			syncScalerSimulationRole(t, client, fleet, "decode", false, &aborted)
			stored := scalerTestGetDeployment(t, client)
			if tc.clearRetirement {
				stored.Status.Retirement = nil
			}
			current := deploymentReplicaVector(stored)
			got, why, planErr := r.planRebalance(context.Background(), scaler, stored, current, current, recommendations, time.Unix(int64(tc.now), 0), true)
			if planErr != nil || why != "" || got["prefill"] != tc.wantReceiver || got["decode"] != 1 {
				t.Fatalf("rebalance = %v, reason=%q, err=%v", got, why, planErr)
			}
			group := modelDeploymentReplicaGroupName(stored, "decode", 1)
			wl := new(kueue.Workload)
			getErr := client.Get(context.Background(), ctrlcli.ObjectKey{Namespace: stored.Namespace, Name: group}, wl)
			if tc.wantWorkload {
				if getErr != nil {
					t.Fatal(getErr)
				}
				pods := new(core.PodList)
				if err := client.List(context.Background(), pods, ctrlcli.MatchingLabels{kueuepodconst.GroupNameLabel: group}); err != nil {
					t.Fatal(err)
				}
				if len(pods.Items) != 1 || wl.Labels[kueuepodconst.GroupNameLabel] != group || wl.Labels[modelDeploymentReplicaOrdinalLabel] != "1" {
					t.Fatalf("reservation labels differ from replica: workload=%+v pods=%d", wl.Labels, len(pods.Items))
				}
			} else if !apierrors.IsNotFound(getErr) {
				t.Fatalf("released workload remains: %v", getErr)
			}
		})
	}
	if fleet.Retired != 1 || scaler.Status.Rebalance.Reason != modelDeploymentScalerRebalanceReceiverAcquire {
		t.Fatalf("transaction did not finish: retired=%d state=%+v", fleet.Retired, scaler.Status.Rebalance)
	}
}

var causalDisaggHistogramBounds = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 30, math.Inf(1)}

type causalStageCounters struct {
	promptTokens int64
	outputTokens int64
	running      float64
	waiting      float64
	cacheUsage   float64
	ttftBuckets  [12]int64
	ttftCount    int64
	ttftSum      float64
	itlBuckets   [12]int64
	itlCount     int64
	itlSum       float64
}

func (c *causalStageCounters) observeTTFT(value float64, weight int64) {
	if weight <= 0 {
		return
	}
	c.ttftCount += weight
	c.ttftSum += value * float64(weight)
	for i, bound := range causalDisaggHistogramBounds {
		if value <= bound {
			c.ttftBuckets[i] += weight
		}
	}
}

func (c *causalStageCounters) observeITL(value float64, weight int64) {
	if weight <= 0 {
		return
	}
	c.itlCount += weight
	c.itlSum += value * float64(weight)
	for i, bound := range causalDisaggHistogramBounds {
		if value <= bound {
			c.itlBuckets[i] += weight
		}
	}
}

type causalDisaggPhase struct {
	untilSeconds      float64
	requestsPerSecond float64
}

type causalDisaggConfig struct {
	hasEncode              bool
	encodeReqsPerSecPerPod float64
	prefillTokPerSecPerPod float64
	decodeTokPerSecPerPod  float64
	promptTokensPerRequest float64
	outputTokensPerRequest float64
	baseTTFTSeconds        float64
	overloadTTFTSeconds    float64
	baseITLSeconds         float64
	overloadITLSeconds     float64
	phases                 []causalDisaggPhase
}

// causalDisaggPlant simulates a causally conserved Encode -> Prefill -> Decode pipeline
// with independent stage capacities so upstream backpressure bounds downstream throughput.
type causalDisaggPlant struct {
	cfg          causalDisaggConfig
	queueEncode  float64
	queuePrefill float64
	queueDecode  float64
	remPTokens   float64
	remPReqs     float64
	remDTokens   float64
	prefill      map[int]*causalStageCounters
	decode       map[int]*causalStageCounters
}

func newCausalDisaggPlant(cfg causalDisaggConfig) *causalDisaggPlant {
	return &causalDisaggPlant{
		cfg:     cfg,
		prefill: make(map[int]*causalStageCounters),
		decode:  make(map[int]*causalStageCounters),
	}
}

func (p *causalDisaggPlant) arrivalRate(now float64) float64 {
	for _, phase := range p.cfg.phases {
		if now <= phase.untilSeconds {
			return phase.requestsPerSecond
		}
	}
	return 0
}

func (p *causalDisaggPlant) retain(role string, ordinals []int) {
	var target map[int]*causalStageCounters
	switch role {
	case "prefill":
		target = p.prefill
	case "decode":
		target = p.decode
	default:
		return
	}
	present := make(map[int]bool, len(ordinals))
	for _, ord := range ordinals {
		present[ord] = true
		if target[ord] == nil {
			target[ord] = &causalStageCounters{}
		}
	}
	for ord := range target {
		if !present[ord] {
			delete(target, ord)
		}
	}
}

func (p *causalDisaggPlant) step(now, dt float64, readyE, readyP, readyD []int) {
	arrived := p.arrivalRate(now) * dt

	// Encode transfers admitted requests to Prefill with strict request conservation.
	if p.cfg.hasEncode {
		p.queueEncode += arrived
		capE := float64(len(readyE)) * p.cfg.encodeReqsPerSecPerPod * dt
		servedE := math.Min(p.queueEncode, capE)
		p.queueEncode -= servedE
		p.queuePrefill += servedE
	} else {
		p.queuePrefill += arrived
	}

	// Prefill processes prompt tokens up to stage capacity and forwards completed prompts to Decode.
	capP := float64(len(readyP)) * p.cfg.prefillTokPerSecPerPod * dt
	demandP := p.queuePrefill * p.cfg.promptTokensPerRequest
	servedPTokens := math.Min(demandP, capP)
	servedPReqs := servedPTokens / p.cfg.promptTokensPerRequest
	p.queuePrefill = math.Max(0, p.queuePrefill-servedPReqs)
	p.queueDecode += servedPReqs * p.cfg.outputTokensPerRequest

	utilP := 0.0
	if capP > 0 {
		utilP = demandP / capP
	}
	ttft := p.cfg.baseTTFTSeconds
	if utilP > 1.05 {
		ttft = p.cfg.overloadTTFTSeconds
	}
	if len(readyP) > 0 {
		totalP := p.remPTokens + servedPTokens
		intP := int64(math.Floor(totalP))
		p.remPTokens = totalP - float64(intP)
		baseP, extraP := intP/int64(len(readyP)), intP%int64(len(readyP))

		totalReqsP := p.remPReqs + servedPReqs
		intReqsP := int64(math.Floor(totalReqsP))
		p.remPReqs = totalReqsP - float64(intReqsP)
		baseReqs, extraReqs := intReqsP/int64(len(readyP)), intReqsP%int64(len(readyP))

		for idx, ord := range readyP {
			c := p.prefill[ord]
			if c == nil {
				c = &causalStageCounters{}
				p.prefill[ord] = c
			}
			shareP := baseP
			if int64(idx) < extraP {
				shareP++
			}
			shareReq := baseReqs
			if int64(idx) < extraReqs {
				shareReq++
			}
			c.promptTokens += shareP
			c.running = math.Min(4, utilP*4)
			c.waiting = p.queuePrefill / float64(len(readyP))
			c.cacheUsage = math.Min(1, utilP*0.5)
			c.observeTTFT(ttft, shareReq)
		}
	}

	// Decode processes generation tokens forwarded from Prefill up to Decode capacity.
	capD := float64(len(readyD)) * p.cfg.decodeTokPerSecPerPod * dt
	demandD := p.queueDecode
	servedDTokens := math.Min(demandD, capD)
	p.queueDecode = math.Max(0, p.queueDecode-servedDTokens)

	utilD := 0.0
	if capD > 0 {
		utilD = demandD / capD
	}
	itl := p.cfg.baseITLSeconds
	if utilD > 1.05 {
		itl = p.cfg.overloadITLSeconds
	}
	if len(readyD) > 0 {
		totalD := p.remDTokens + servedDTokens
		intD := int64(math.Floor(totalD))
		p.remDTokens = totalD - float64(intD)
		baseD, extraD := intD/int64(len(readyD)), intD%int64(len(readyD))
		for idx, ord := range readyD {
			c := p.decode[ord]
			if c == nil {
				c = &causalStageCounters{}
				p.decode[ord] = c
			}
			shareD := baseD
			if int64(idx) < extraD {
				shareD++
			}
			c.outputTokens += shareD
			c.running = math.Min(4, utilD*4)
			c.waiting = (p.queueDecode / p.cfg.outputTokensPerRequest) / float64(len(readyD))
			c.cacheUsage = math.Min(1, utilD*0.5)
			c.observeITL(itl, shareD)
		}
	}
}

func (p *causalDisaggPlant) exposition(role string, ordinal int) ([]byte, error) {
	var c *causalStageCounters
	switch role {
	case "prefill":
		c = p.prefill[ordinal]
	case "decode":
		c = p.decode[ordinal]
	default:
		return nil, fmt.Errorf("role %q does not expose metrics", role)
	}
	if c == nil {
		return nil, fmt.Errorf("replica %s-%d has no metrics", role, ordinal)
	}
	var b strings.Builder
	metric := func(key, kind string, value float64) {
		fmt.Fprintf(&b, "# TYPE vllm:%s %s\nvllm:%s %.6f\n", key, kind, key, value)
	}
	counter := func(key string, value int64) {
		fmt.Fprintf(&b, "# TYPE vllm:%s counter\nvllm:%s %d\n", key, key, value)
	}
	metric("num_requests_running", "gauge", c.running)
	metric("num_requests_waiting", "gauge", c.waiting)
	metric("kv_cache_usage_perc", "gauge", c.cacheUsage)
	counter("prompt_tokens_total", c.promptTokens)
	counter("generation_tokens_total", c.outputTokens)
	counter("prefix_cache_queries_total", c.promptTokens)
	counter("prefix_cache_hits_total", 0)
	writeHist := func(name string, buckets [12]int64, count int64, sum float64) {
		fmt.Fprintf(&b, "# TYPE %s histogram\n", name)
		for i, bound := range causalDisaggHistogramBounds {
			fmt.Fprintf(&b, "%s_bucket{le=\"%g\"} %d\n", name, bound, buckets[i])
		}
		fmt.Fprintf(&b, "%s_count %d\n%s_sum %.6f\n", name, count, name, sum)
	}
	writeHist("vllm:time_to_first_token_seconds", c.ttftBuckets, c.ttftCount, c.ttftSum)
	writeHist("vllm:inter_token_latency_seconds", c.itlBuckets, c.itlCount, c.itlSum)
	return []byte(b.String()), nil
}

func TestModelDeploymentScalerSimulationWholeUnitCausal(t *testing.T) {
	type scenarioSpec struct {
		name               string
		cfg                causalDisaggConfig
		expectLatencyGuard bool
	}

	buildScenarios := func(includeEncode bool) []scenarioSpec {
		balancedRate := 23.0 // 23 * 300 = 6900 tok/s > 5250 C_unit (1P:3D)
		if includeEncode {
			balancedRate = 17.0 // 17 * 300 = 5100 tok/s > 3750 C_unit (1E:1P:2D)
		}
		return []scenarioSpec{
			{
				name: "balanced_throughput",
				cfg: causalDisaggConfig{
					hasEncode:              includeEncode,
					encodeReqsPerSecPerPod: 60,
					prefillTokPerSecPerPod: 4000,
					decodeTokPerSecPerPod:  3000,
					promptTokensPerRequest: 50,
					outputTokensPerRequest: 250,
					baseTTFTSeconds:        0.08,
					overloadTTFTSeconds:    0.85,
					baseITLSeconds:         0.02,
					overloadITLSeconds:     0.085,
					phases: []causalDisaggPhase{
						{untilSeconds: 30, requestsPerSecond: 3},
						{untilSeconds: 140, requestsPerSecond: balancedRate},
						{untilSeconds: 500, requestsPerSecond: 1},
					},
				},
			},
			{
				name:               "prefill_bottlenecked_latency_guard",
				expectLatencyGuard: true,
				cfg: causalDisaggConfig{
					hasEncode:              includeEncode,
					encodeReqsPerSecPerPod: 60,
					prefillTokPerSecPerPod: 1100, // Small independent P capacity -> TTFT spikes even below C_unit.
					decodeTokPerSecPerPod:  5000,
					promptTokensPerRequest: 200,
					outputTokensPerRequest: 100,
					baseTTFTSeconds:        0.08,
					overloadTTFTSeconds:    0.85, // > 1.2 * 500ms
					baseITLSeconds:         0.02,
					overloadITLSeconds:     0.085,
					phases: []causalDisaggPhase{
						{untilSeconds: 30, requestsPerSecond: 2},
						{untilSeconds: 140, requestsPerSecond: 6.5}, // 6.5*300 = 1950 tok/s << C_unit, but P demand 1300 > 1100.
						{untilSeconds: 500, requestsPerSecond: 1},
					},
				},
			},
			{
				name:               "decode_bottlenecked_latency_guard",
				expectLatencyGuard: true,
				cfg: causalDisaggConfig{
					hasEncode:              includeEncode,
					encodeReqsPerSecPerPod: 60,
					prefillTokPerSecPerPod: 5000,
					decodeTokPerSecPerPod:  450, // Small independent D capacity -> ITL spikes even below C_unit.
					promptTokensPerRequest: 50,
					outputTokensPerRequest: 250,
					baseTTFTSeconds:        0.08,
					overloadTTFTSeconds:    0.85,
					baseITLSeconds:         0.02,
					overloadITLSeconds:     0.085, // > 1.2 * 50ms
					phases: []causalDisaggPhase{
						{untilSeconds: 30, requestsPerSecond: 2},
						{untilSeconds: 140, requestsPerSecond: 6.0}, // 6.0*300 = 1800 tok/s << C_unit, but D demand 1500 > 3*450.
						{untilSeconds: 500, requestsPerSecond: 1},
					},
				},
			},
		}
	}

	topologies := []struct {
		name          string
		includeEncode bool
		unitVector    map[string]int32
		unitTargetTPS float64
	}{
		{
			name:          "1P3D",
			includeEncode: false,
			unitVector:    map[string]int32{"prefill": 1, "decode": 3},
			unitTargetTPS: 5250,
		},
		{
			name:          "1E1P2D",
			includeEncode: true,
			unitVector:    map[string]int32{"encode": 1, "prefill": 1, "decode": 2},
			unitTargetTPS: 3750,
		},
	}

	algorithms := []struct {
		name       string
		predictive bool
	}{
		{name: "token_throughput", predictive: false},
		{name: "predictive", predictive: true},
	}

	for _, topo := range topologies {
		for _, alg := range algorithms {
			for _, sc := range buildScenarios(topo.includeEncode) {
				t.Run(fmt.Sprintf("%s/%s/%s", topo.name, alg.name, sc.name), func(t *testing.T) {
					scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, topo.includeEncode)
					scaler.Spec.Sampling = workercore.ModelDeploymentScalerSamplingSpec{
						IntervalSeconds:       10,
						PendingTimeoutSeconds: 60,
					}
					scaler.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = 30
					scaler.Spec.Behavior.ScaleDown.DecodePostScaleUpGraceSeconds = 10
					scaler.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = 0
					scaler.Spec.Algorithm.TokenThroughput.StableWindowSeconds = 20
					scaler.Spec.Algorithm.TokenThroughput.PanicWindowSeconds = 10
					if alg.predictive {
						scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
						scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
							TokenThroughput: *scaler.Spec.Algorithm.TokenThroughput.DeepCopy(),
							HorizonSeconds:  20,
							MinPoints:       2,
						}
						scaler.Spec.Algorithm.TokenThroughput = nil
					}

					objs := []ctrlcli.Object{md, scaler, cm}
					for _, inst := range instances {
						objs = append(objs, inst)
					}
					client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
						WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}, &core.Pod{}, &kueue.Workload{}).
						WithObjects(objs...).Build()

					fleets := make(map[string]*sim.Fleet, len(topo.unitVector))
					roles := make([]string, 0, len(topo.unitVector))
					for role, unit := range topo.unitVector {
						roles = append(roles, role)
						fleets[role] = sim.NewFleet(sim.FleetConfig{
							Initial:           int(unit),
							Quota:             int(unit * 4),
							BootSeconds:       5,
							RetirementSeconds: 20,
						})
					}
					sort.Strings(roles)
					plant := newCausalDisaggPlant(sc.cfg)

					now := 0.0
					clock := func() time.Time { return time.Unix(200000, 0).Add(time.Duration(now * float64(time.Second))) }
					r := &ModelDeploymentScalerReconciler{
						Client:  client,
						clock:   clock,
						sampler: &modelDeploymentScalerSampler{Clock: clock},
					}
					r.sampler.Scrape = func(_ context.Context, pod *core.Pod) ([]byte, error) {
						role := modelDeploymentPodRole(pod)
						ordinal, err := strconv.Atoi(pod.Labels[modelDeploymentReplicaOrdinalLabel])
						if err != nil {
							return nil, err
						}
						return plant.exposition(role, ordinal)
					}

					aborted := false
					grew := false
					sawLatencyGuard := false
					maxObservedTPS := 0.0
					var sawRetirementHold bool
					var trace []string

					for tick := 0; tick <= 480; tick++ {
						now = float64(tick)
						for _, role := range roles {
							fleet := fleets[role]
							fleet.Advance(now)
							plant.retain(role, fleet.Ordinals())
						}
						var readyE []int
						if topo.includeEncode {
							readyE = fleets["encode"].Ready()
						}
						plant.step(now, 1.0, readyE, fleets["prefill"].Ready(), fleets["decode"].Ready())

						if tick%10 == 0 {
							for _, role := range roles {
								syncScalerSimulationRole(t, client, fleets[role], role, false, &aborted)
							}
							before := scalerTestGetDeployment(t, client)
							retiringBefore := before.Status.Retirement != nil &&
								before.Status.Retirement.State == workercore.ModelDeploymentRetirementStateDraining

							reconcileScaler(t, r, scaler.Name)

							after := scalerTestGetDeployment(t, client)
							afterVec := deploymentReplicaVector(after)
							kAfter, ok := wholeUnitMultiplier(after.Spec.Roles, topo.unitVector)
							if !ok || kAfter < 1 {
								t.Fatalf("whole-unit invariant violated at t=%.0fs: vector=%v unit=%v", now, afterVec, topo.unitVector)
							}
							if kAfter >= 2 {
								grew = true
							}
							beforeVec := deploymentReplicaVector(before)
							if retiringBefore {
								sawRetirementHold = true
								for _, role := range roles {
									if afterVec[role] < beforeVec[role] {
										t.Fatalf("scale-down happened during active retirement at t=%.0fs: %v -> %v", now, beforeVec, afterVec)
									}
								}
							}

							curScaler := scalerTestGetScaler(t, client, scaler.Name)
							var pTPS, dTPS float64
							var dReason string
							for _, rs := range curScaler.Status.Roles {
								if rs.Name == "prefill" && rs.PromptTokensPerSecond != nil {
									pTPS = rs.PromptTokensPerSecond.AsApproximateFloat64()
								}
								if rs.Name == "decode" {
									dReason = rs.Reason
									if rs.GenerationTokensPerSecond != nil {
										dTPS = rs.GenerationTokensPerSecond.AsApproximateFloat64()
									}
								}
								if rs.Reason == algorithm.ReasonLatencyGuardScaleUp {
									sawLatencyGuard = true
								}
							}
							if !scalerTestVectorsEqual(beforeVec, afterVec) {
								trace = append(trace, fmt.Sprintf("t=%.0f:%v->%v(%s)", now, beforeVec, afterVec, dReason))
							}
							if tick%60 == 0 {
								trace = append(trace, fmt.Sprintf("t=%.0f:%v[ret=%v,reason=%s,tps=%.0f]", now, afterVec, retiringBefore, dReason, pTPS+dTPS))
							}
							if totalTPS := pTPS + dTPS; totalTPS > maxObservedTPS {
								maxObservedTPS = totalTPS
							}

							for _, role := range roles {
								fleets[role].SetDesired(int(afterVec[role]), now)
							}
						}
					}

					if !grew {
						t.Fatalf("whole-unit simulation never scaled up to >=2x; trace=%v", trace)
					}
					if !sawRetirementHold {
						t.Fatalf("expected retirement hold window during 2x -> 1x scale-down; trace=%v", trace)
					}
					if sc.expectLatencyGuard {
						if !sawLatencyGuard {
							t.Fatalf("expected LatencyGuardScaleUp reason under %s bottleneck", sc.name)
						}
						if maxObservedTPS >= topo.unitTargetTPS {
							t.Fatalf("expected bottlenecked total TPS (%.1f) to remain below C_unit (%.1f)", maxObservedTPS, topo.unitTargetTPS)
						}
					}
					finalMD := scalerTestGetDeployment(t, client)
					finalVec := deploymentReplicaVector(finalMD)
					for role, unit := range topo.unitVector {
						if finalVec[role] != unit {
							t.Fatalf("final %s replicas = %d, want %d (full vector %v)", role, finalVec[role], unit, finalVec)
						}
						if len(fleets[role].Ready()) != int(unit) || len(fleets[role].Replicas()) != int(unit) {
							t.Fatalf("role %s fleet did not settle to %d: ready=%v all=%+v", role, unit, fleets[role].Ready(), fleets[role].Replicas())
						}
					}
					finalScaler := scalerTestGetScaler(t, client, scaler.Name)
					if finalScaler.Status.LastScaleDown != nil {
						t.Fatalf("expected Committed LastScaleDown to clear after retirement completion, got %+v", finalScaler.Status.LastScaleDown)
					}
				})
			}
		}
	}
}
