package worker

import (
	"context"
	"testing"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

func TestModelDeploymentScalerRebalanceOpposingTransaction(t *testing.T) {
	r, client, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyOpposing)
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 2},
		"decode":  {DesiredReplicas: 1},
	}
	current := deploymentReplicaVector(md)

	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current,
		map[string]int32{"prefill": 2, "decode": 1}, recommendations, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 1, "decode": 1}) {
		t.Fatalf("donor release = %v, reason=%q", vector, reason)
	}
	if scaler.Status.Rebalance.DonorRole != "decode" || scaler.Status.Rebalance.DonorTargetReplicas != 1 ||
		scaler.Status.Rebalance.ReceiverTargetReplicas != 2 {
		t.Fatalf("rebalance transaction = %#v", scaler.Status.Rebalance)
	}

	current = map[string]int32{"prefill": 1, "decode": 1}
	md.Spec.Roles[1].Replicas = 1
	md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{RoleName: "decode", ReplicaOrdinal: 1}
	vector, reason, err = r.planRebalance(context.Background(), scaler, md, current, current, recommendations, time.Unix(21, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, current) {
		t.Fatalf("active retirement changed vector = %v, reason=%q", vector, reason)
	}

	md.Status.Retirement = nil
	workload := &kueue.Workload{ObjectMeta: meta.ObjectMeta{
		Namespace: md.Namespace, Name: modelDeploymentReplicaGroupName(md, "decode", 1),
	}}
	if err := client.Create(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	vector, reason, err = r.planRebalance(context.Background(), scaler, md, current, current, recommendations, time.Unix(22, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, current) {
		t.Fatalf("old workload changed vector = %v, reason=%q", vector, reason)
	}
	if err := client.Delete(context.Background(), workload); err != nil {
		t.Fatal(err)
	}

	vector, reason, err = r.planRebalance(context.Background(), scaler, md, current, current, recommendations, time.Unix(23, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 2, "decode": 1}) {
		t.Fatalf("receiver acquire = %v, reason=%q", vector, reason)
	}
}

func TestModelDeploymentScalerRebalancePriorityDonationLimit(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyPriority)
	scaler.Spec.Rebalance.Priority = []string{"prefill", "decode"}
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 2},
		"decode":  {DesiredReplicas: 2},
	}
	current := deploymentReplicaVector(md)

	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current,
		map[string]int32{"prefill": 2, "decode": 2}, recommendations, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 1, "decode": 1}) {
		t.Fatalf("priority donor release = %v, reason=%q", vector, reason)
	}

	current = map[string]int32{"prefill": 1, "decode": 1}
	md.Spec.Roles[1].Replicas = 1
	vector, reason, err = r.planRebalance(context.Background(), scaler, md, current, current, recommendations, time.Unix(21, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 2, "decode": 1}) {
		t.Fatalf("priority receiver acquire = %v, reason=%q", vector, reason)
	}
	if !scaler.Status.Rebalance.DonationAttempted {
		t.Fatalf("donation was not recorded: %#v", scaler.Status.Rebalance)
	}

	scaler.Status.Rebalance.Reason = workercore.ModelDeploymentScalerReasonResourceExhausted
	vector, reason, err = r.planRebalance(context.Background(), scaler, md, current, current, recommendations, time.Unix(22, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != workercore.ModelDeploymentScalerReasonRebalanceBlocked || !scalerTestVectorsEqual(vector, current) {
		t.Fatalf("second donation = %v, reason=%q", vector, reason)
	}
}

func TestModelDeploymentScalerRebalanceReportsDistinctQueues(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyOpposing)
	md.Spec.Roles[1].InstanceType = "other-pool"
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 2},
		"decode":  {DesiredReplicas: 1},
	}
	current := deploymentReplicaVector(md)

	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current,
		map[string]int32{"prefill": 2, "decode": 1}, recommendations, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != workercore.ModelDeploymentScalerReasonRebalanceNotApplicable || !scalerTestVectorsEqual(vector, current) {
		t.Fatalf("distinct queues = %v, reason=%q", vector, reason)
	}
}

func TestModelDeploymentScalerRebalanceRejectsRatioViolation(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyOpposing)
	md.Spec.Roles[0].Replicas = 2
	md.Spec.Roles[1].Replicas = 2
	scaler.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
		NumeratorRole: "prefill", DenominatorRole: "decode",
		Min: quantityPointer("1"), Max: quantityPointer("1"), Target: quantityPointer("1"),
	}}
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 3},
		"decode":  {DesiredReplicas: 1},
	}
	current := deploymentReplicaVector(md)

	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current,
		map[string]int32{"prefill": 3, "decode": 1}, recommendations, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != workercore.ModelDeploymentScalerReasonRebalanceBlocked || !scalerTestVectorsEqual(vector, current) {
		t.Fatalf("ratio-invalid donation = %v, reason=%q", vector, reason)
	}
}

func TestModelDeploymentScalerRebalanceDisabled(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyDisabled)
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 2},
		"decode":  {DesiredReplicas: 1},
	}
	target := map[string]int32{"prefill": 2, "decode": 1}
	vector, reason, err := r.planRebalance(context.Background(), scaler, md, deploymentReplicaVector(md),
		target, recommendations, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, target) || scaler.Status.Rebalance != nil {
		t.Fatalf("disabled policy = %v, reason=%q, status=%#v", vector, reason, scaler.Status.Rebalance)
	}
}

func TestModelDeploymentScalerRebalanceKeepsExhaustionWhileMetricsWait(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyPriority)
	scaler.Spec.Rebalance.Priority = []string{"prefill", "decode"}
	scaler.Status.Roles = []workercore.ModelDeploymentScalerRoleStatus{{
		Name: "prefill", RawRecommendedReplicas: 2,
	}}
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 1, Reason: "WaitingForMetrics"},
		"decode":  {DesiredReplicas: 2},
	}
	current := deploymentReplicaVector(md)

	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current, current,
		recommendations, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 1, "decode": 1}) {
		t.Fatalf("stale exhausted receiver = %v, reason=%q", vector, reason)
	}
}

func TestModelDeploymentScalerRebalanceRecordsOnlyUnadmittedReceiver(t *testing.T) {
	_, _, scaler, _ := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyOpposing)
	scaler.Status.Rebalance = nil
	cohort := &workercore.ModelDeploymentScalerPendingCohortStatus{
		BaselineVector: map[string]int32{"prefill": 1, "decode": 1},
		TargetVector:   map[string]int32{"prefill": 2, "decode": 2},
	}
	(&ModelDeploymentScalerReconciler{}).recordRebalanceExhaustion(scaler, cohort,
		map[string]map[int32]bool{"prefill": {1: true}})

	state := scaler.Status.Rebalance
	if state.ReceiverRole != "decode" || state.Reason != workercore.ModelDeploymentScalerReasonResourceExhausted ||
		state.ExhaustionEpisodeGeneration != 1 {
		t.Fatalf("exhaustion = %#v", state)
	}
}

func TestModelDeploymentScalerRebalanceShadowDoesNotCreateTransaction(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyPriority)
	scaler.Spec.Rebalance.Priority = []string{"prefill", "decode"}
	recommendations := map[string]algorithm.RoleRecommendation{
		"prefill": {DesiredReplicas: 2},
		"decode":  {DesiredReplicas: 2},
	}
	current := deploymentReplicaVector(md)

	vector, reason, err := r.planRebalance(context.Background(), scaler, md, current,
		map[string]int32{"prefill": 2, "decode": 2}, recommendations, time.Unix(20, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || !scalerTestVectorsEqual(vector, map[string]int32{"prefill": 2, "decode": 2}) ||
		scaler.Status.Rebalance.DonorRole != "" {
		t.Fatalf("shadow rebalance = %v, reason=%q, status=%#v", vector, reason, scaler.Status.Rebalance)
	}
}

func TestModelDeploymentScalerRebalanceGenerationChangeResetsDonation(t *testing.T) {
	r, _, scaler, md := scalerRebalanceFixture(t, workercore.ModelDeploymentScalerRebalancePolicyPriority)
	scaler.Generation++
	scaler.Status.ObservedGeneration = scaler.Generation - 1
	scaler.Status.Rebalance.DonationAttempted = true
	scaler.Status.Rebalance.ExhaustionEpisodeGeneration = 2
	scaler.Status.Rebalance.Reason = workercore.ModelDeploymentScalerReasonRebalanceBlocked
	r.resetRebalanceForGenerationChange(scaler, md)

	state := scaler.Status.Rebalance
	if state.ExhaustionEpisodeGeneration != 3 || state.DonationAttempted ||
		state.Reason != workercore.ModelDeploymentScalerReasonResourceExhausted {
		t.Fatalf("generation reset = %#v", state)
	}
	r.resetRebalanceForGenerationChange(scaler, md)
	if state.ExhaustionEpisodeGeneration != 3 {
		t.Fatalf("generation reset repeated without another donation: %#v", state)
	}

	state.DonationAttempted = true
	state.DonorRole = "decode"
	state.DonorTargetReplicas = 1
	state.Reason = modelDeploymentScalerRebalanceDonorRelease
	r.resetRebalanceForGenerationChange(scaler, md)
	if state.DonorRole != "decode" || state.DonorTargetReplicas != 1 ||
		state.Reason != modelDeploymentScalerRebalanceDonorRelease || state.DonationAttempted {
		t.Fatalf("generation reset changed in-flight transaction: %#v", state)
	}

	state.Reason = modelDeploymentScalerRebalanceReceiverAcquire
	state.ReceiverRole = "prefill"
	state.ReceiverTargetReplicas = 2
	current := map[string]int32{"prefill": 2, "decode": 1}
	_, _, err := r.planRebalance(context.Background(), scaler, md, current, current,
		map[string]algorithm.RoleRecommendation{"prefill": {DesiredReplicas: 3}}, time.Unix(20, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if !state.DonationAttempted {
		t.Fatalf("completed receiver acquire did not count its donation: %#v", state)
	}
}

func TestModelDeploymentScalerRebalanceAbortedDonorRestoresAndCancels(t *testing.T) {
	now := time.Unix(30, 0)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"prefill": 1, "decode": 1})
	md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
		RoleName: "decode", ReplicaOrdinal: 1, State: workercore.ModelDeploymentRetirementStateAborted,
	}
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
		{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
		{Name: "decode", MinReplicas: 1, MaxReplicas: 4},
	}
	scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: 4}
	scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
		Role: "decode", Ordinal: 1, Outcome: "Committed",
		PreScaleDownVector: map[string]int32{"prefill": 1, "decode": 2},
	}
	scaler.Status.Rebalance = &workercore.ModelDeploymentScalerRebalanceStatus{
		ReceiverRole: "prefill", DonorRole: "decode", DonorTargetReplicas: 1,
		ReceiverTargetReplicas: 2, ExhaustionEpisodeGeneration: 1,
	}
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	reconcileScaler(t, r, scaler.Name)

	if got := scalerTestVector(scalerTestGetDeployment(t, client)); !scalerTestVectorsEqual(got, map[string]int32{"prefill": 1, "decode": 2}) {
		t.Fatalf("aborted donor vector = %v", got)
	}
	if got := scalerTestGetScaler(t, client, scaler.Name).Status.Rebalance; got != nil {
		t.Fatalf("aborted donor transaction remains: %#v", got)
	}
}

func scalerRebalanceFixture(
	t *testing.T, policy workercore.ModelDeploymentScalerRebalancePolicy,
) (*ModelDeploymentScalerReconciler, ctrlcli.Client, *workercore.ModelDeploymentScaler, *workercore.ModelDeployment) {
	t.Helper()
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"prefill": 1, "decode": 2})
	for i := range md.Spec.Roles {
		md.Spec.Roles[i].InstanceType = "shared-pool"
	}
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
		{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
		{Name: "decode", MinReplicas: 1, MaxReplicas: 4},
	}
	scaler.Spec.Rebalance.Policy = policy
	scaler.Status.Rebalance = &workercore.ModelDeploymentScalerRebalanceStatus{
		ReceiverRole: "prefill", ExhaustionEpisodeGeneration: 1,
		Reason: workercore.ModelDeploymentScalerReasonResourceExhausted,
	}
	client := scalerTestClient(t, md, scaler)
	return scalerTestReconciler(client, time.Unix(20, 0)), client, scaler, md
}
