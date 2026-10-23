package worker

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

const (
	modelDeploymentScalerRebalanceDonorRelease    = "DonorRelease"
	modelDeploymentScalerRebalanceReceiverAcquire = "ReceiverAcquire"
	modelDeploymentScalerRebalanceDonationAttempt = "DonationAttempted"
)

func (r *ModelDeploymentScalerReconciler) planRebalance(
	ctx context.Context,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	current, target map[string]int32,
	recommendations map[string]algorithm.RoleRecommendation,
	now time.Time,
	active bool,
) (map[string]int32, string, error) {
	if !active {
		return target, "", nil
	}
	state := scaler.Status.Rebalance
	if state == nil || state.ReceiverRole == "" {
		return target, "", nil
	}
	if !rebalanceReceiverWantsScaleUp(scaler, recommendations[state.ReceiverRole], state.ReceiverRole, current) {
		if state.Reason == modelDeploymentScalerRebalanceDonorRelease && md.Status.Retirement != nil {
			return cloneReplicaVector(current), "", nil
		}
		scaler.Status.Rebalance = nil
		return target, "", nil
	}

	if state.Reason == modelDeploymentScalerRebalanceDonorRelease {
		return r.finishRebalanceDonorRelease(ctx, scaler, md, current)
	}
	if state.Reason == modelDeploymentScalerRebalanceReceiverAcquire {
		if scaler.Status.PendingCohort != nil {
			return cloneReplicaVector(current), "", nil
		}
		if current[state.ReceiverRole] >= state.ReceiverTargetReplicas {
			state.DonationAttempted = true
			state.Reason = modelDeploymentScalerRebalanceDonationAttempt
			return target, "", nil
		}
		return cloneReplicaVector(current), workercore.ModelDeploymentScalerReasonRebalanceBlocked, nil
	}
	if md.Status.Retirement != nil {
		return cloneReplicaVector(current), "", nil
	}

	policy := scaler.Spec.Rebalance.Policy
	if policy == "" {
		policy = workercore.ModelDeploymentScalerRebalancePolicyOpposing
	}
	if policy == workercore.ModelDeploymentScalerRebalancePolicyDisabled {
		scaler.Status.Rebalance = nil
		return target, "", nil
	}
	if policy == workercore.ModelDeploymentScalerRebalancePolicyPriority &&
		state.DonationAttempted && state.Reason == workercore.ModelDeploymentScalerReasonResourceExhausted {
		state.Reason = workercore.ModelDeploymentScalerReasonRebalanceBlocked
		return cloneReplicaVector(current), workercore.ModelDeploymentScalerReasonRebalanceBlocked, nil
	}

	roles := make(map[string]workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for _, role := range md.Spec.Roles {
		roles[role.Name] = role
	}
	receiver, ok := roles[state.ReceiverRole]
	if !ok {
		scaler.Status.Rebalance = nil
		return target, "", nil
	}
	minimums := scalerRoleMinimums(scaler.Spec.Roles)
	donors := rebalanceDonorCandidates(scaler, md.Spec.Roles, receiver.Name, policy)
	for _, donor := range donors {
		minimum := minimums[donor.Name]
		if minimum <= 0 {
			minimum = 1
		}
		if current[donor.Name] <= minimum || receiver.InstanceType == "" || donor.InstanceType == "" ||
			receiver.InstanceType != donor.InstanceType {
			continue
		}
		if policy == workercore.ModelDeploymentScalerRebalancePolicyOpposing &&
			recommendations[donor.Name].DesiredReplicas >= current[donor.Name] {
			continue
		}

		donorTarget := current[donor.Name] - 1
		donorVector := cloneReplicaVector(current)
		donorVector[donor.Name] = donorTarget
		receiverVector := cloneReplicaVector(donorVector)
		receiverTarget := current[receiver.Name] + 1
		receiverVector[receiver.Name] = receiverTarget
		if currentVectorConflict(scaler.Spec, md.Spec.Roles, donorVector) != "" ||
			currentVectorConflict(scaler.Spec, md.Spec.Roles, receiverVector) != "" {
			state.Reason = workercore.ModelDeploymentScalerReasonRebalanceBlocked
			return cloneReplicaVector(current), workercore.ModelDeploymentScalerReasonRebalanceBlocked, nil
		}

		startedAt := meta.NewTime(now)
		state.DonorRole = donor.Name
		state.DonorTargetReplicas = donorTarget
		state.ReceiverTargetReplicas = receiverTarget
		state.StartedAt = &startedAt
		state.Reason = modelDeploymentScalerRebalanceDonorRelease
		return donorVector, "", nil
	}

	state.Reason = workercore.ModelDeploymentScalerReasonRebalanceNotApplicable
	return cloneReplicaVector(current), workercore.ModelDeploymentScalerReasonRebalanceNotApplicable, nil
}

func rebalanceReceiverWantsScaleUp(
	scaler *workercore.ModelDeploymentScaler,
	recommendation algorithm.RoleRecommendation,
	role string,
	current map[string]int32,
) bool {
	if recommendation.DesiredReplicas > current[role] {
		return true
	}
	if recommendation.Reason != algorithm.ReasonWaitingForMetrics && recommendation.Reason != algorithm.ReasonPredictorWarmingUp {
		return false
	}
	for _, status := range scaler.Status.Roles {
		if status.Name == role && status.RawRecommendedReplicas > current[role] {
			return true
		}
	}
	return false
}

func (r *ModelDeploymentScalerReconciler) finishRebalanceDonorRelease(
	ctx context.Context,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	current map[string]int32,
) (map[string]int32, string, error) {
	state := scaler.Status.Rebalance
	if md.Status.Retirement != nil || current[state.DonorRole] != state.DonorTargetReplicas {
		return cloneReplicaVector(current), "", nil
	}
	workload := new(kueue.Workload)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: md.Namespace,
		Name:      modelDeploymentReplicaGroupName(md, state.DonorRole, int(state.DonorTargetReplicas)),
	}, workload)
	if err == nil {
		return cloneReplicaVector(current), "", nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, "", err
	}

	receiverVector := cloneReplicaVector(current)
	receiverVector[state.ReceiverRole] = state.ReceiverTargetReplicas
	if currentVectorConflict(scaler.Spec, md.Spec.Roles, receiverVector) != "" {
		state.Reason = workercore.ModelDeploymentScalerReasonRebalanceBlocked
		return cloneReplicaVector(current), workercore.ModelDeploymentScalerReasonRebalanceBlocked, nil
	}
	state.DonationAttempted = true
	state.Reason = modelDeploymentScalerRebalanceReceiverAcquire
	return receiverVector, "", nil
}

func (r *ModelDeploymentScalerReconciler) recordRebalanceExhaustion(
	scaler *workercore.ModelDeploymentScaler,
	cohort *workercore.ModelDeploymentScalerPendingCohortStatus,
	admitted map[string]map[int32]bool,
) {
	roles := make([]string, 0, len(cohort.TargetVector))
	for role, target := range cohort.TargetVector {
		for ordinal := cohort.BaselineVector[role]; ordinal < target; ordinal++ {
			if !admitted[role][ordinal] {
				roles = append(roles, role)
				break
			}
		}
	}
	if len(roles) == 0 {
		return
	}
	sort.Strings(roles)
	receiver := roles[0]
	state := scaler.Status.Rebalance
	if state == nil {
		state = &workercore.ModelDeploymentScalerRebalanceStatus{ExhaustionEpisodeGeneration: 1}
	} else if state.ReceiverRole != receiver {
		state.ExhaustionEpisodeGeneration++
	}
	state.ReceiverRole = receiver
	state.ReceiverTargetReplicas = cohort.TargetVector[receiver]
	state.Reason = workercore.ModelDeploymentScalerReasonResourceExhausted
	scaler.Status.Rebalance = state
}

func (r *ModelDeploymentScalerReconciler) resetRebalanceForGenerationChange(
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
) {
	state := scaler.Status.Rebalance
	if state == nil || !state.DonationAttempted {
		return
	}
	scalerGenerationChanged := scaler.Status.ObservedGeneration > 0 &&
		scaler.Status.ObservedGeneration != scaler.Generation
	mdGenerationChanged := false
	if commit := scaler.Status.LastCommit; commit != nil && commit.TargetGeneration > 0 {
		mdGenerationChanged = md.Generation > commit.TargetGeneration
	}
	if !scalerGenerationChanged && !mdGenerationChanged {
		return
	}
	state.ExhaustionEpisodeGeneration++
	state.DonationAttempted = false
	switch state.Reason {
	case modelDeploymentScalerRebalanceDonorRelease, modelDeploymentScalerRebalanceReceiverAcquire:
		return
	default:
		state.DonorRole = ""
		state.DonorTargetReplicas = 0
		state.ReceiverTargetReplicas = 0
	}
	state.Reason = workercore.ModelDeploymentScalerReasonResourceExhausted
}

func rebalanceDonorCandidates(
	scaler *workercore.ModelDeploymentScaler,
	roles []workercore.ModelDeploymentRole,
	receiver string,
	policy workercore.ModelDeploymentScalerRebalancePolicy,
) []workercore.ModelDeploymentRole {
	if policy == workercore.ModelDeploymentScalerRebalancePolicyPriority {
		priority := scaler.Spec.Rebalance.Priority
		receiverIndex := -1
		for i, role := range priority {
			if role == receiver {
				receiverIndex = i
				break
			}
		}
		if receiverIndex < 0 {
			return nil
		}
		byName := make(map[string]workercore.ModelDeploymentRole, len(roles))
		for _, role := range roles {
			byName[role.Name] = role
		}
		candidates := make([]workercore.ModelDeploymentRole, 0, len(priority)-receiverIndex-1)
		for _, name := range priority[receiverIndex+1:] {
			if role, ok := byName[name]; ok {
				candidates = append(candidates, role)
			}
		}
		return candidates
	}
	candidates := make([]workercore.ModelDeploymentRole, 0, len(roles))
	for _, role := range roles {
		if role.Name != receiver {
			candidates = append(candidates, role)
		}
	}
	return candidates
}

func scalerRoleMinimums(bounds []workercore.ModelDeploymentScalerRoleBounds) map[string]int32 {
	minimums := make(map[string]int32, len(bounds))
	for _, bound := range bounds {
		minimums[bound.Name] = bound.MinReplicas
	}
	return minimums
}
