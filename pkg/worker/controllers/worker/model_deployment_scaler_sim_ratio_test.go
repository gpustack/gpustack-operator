package worker

import (
	"context"
	"strconv"
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
