package worker

import (
	"context"
	"slices"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

const (
	forgerMaxGenerationHistory   = 16
	forgerMaxObservedBottlenecks = 32
)

func modelDeploymentForgerStatusChanged(before, after *workercore.ModelDeploymentForgerStatus) bool {
	if before == nil || after == nil {
		return before != after
	}
	if before.ObservedGeneration != after.ObservedGeneration {
		return true
	}
	if !kubemeta.DeepEqual(before.Binding, after.Binding) {
		return true
	}
	if !kubemeta.DeepEqual(before.Roles, after.Roles) {
		return true
	}
	return !equalScalerConditions(before.Conditions, after.Conditions)
}

func modelDeploymentForgerConditionReason(mdf *workercore.ModelDeploymentForger, conditionType string) string {
	for i := range mdf.Status.Conditions {
		if mdf.Status.Conditions[i].Type == conditionType {
			return mdf.Status.Conditions[i].Reason
		}
	}
	return ""
}

func (r *ModelDeploymentForgerReconciler) setCondition(
	mdf *workercore.ModelDeploymentForger,
	conditionType string,
	status meta.ConditionStatus,
	reason, message string,
	now time.Time,
) {
	for i := range mdf.Status.Conditions {
		cond := &mdf.Status.Conditions[i]
		if cond.Type != conditionType {
			continue
		}
		if cond.Status != status {
			cond.LastTransitionTime = meta.NewTime(now)
		}
		cond.Status = status
		cond.Reason = reason
		cond.Message = message
		cond.ObservedGeneration = mdf.Generation
		return
	}
	mdf.Status.Conditions = append(mdf.Status.Conditions, gpustack.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: meta.NewTime(now),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: mdf.Generation,
	})
}

func (r *ModelDeploymentForgerReconciler) updateStatus(
	ctx context.Context, before, mdf *workercore.ModelDeploymentForger,
) error {
	if !modelDeploymentForgerStatusChanged(&before.Status, &mdf.Status) {
		return nil
	}
	return r.Client.Status().Update(ctx, mdf)
}

func ensureForgerRoleStatuses(mdf *workercore.ModelDeploymentForger) {
	existingByName := make(map[string]workercore.ModelDeploymentForgerRoleStatus, len(mdf.Status.Roles))
	for i := range mdf.Status.Roles {
		existingByName[mdf.Status.Roles[i].Name] = mdf.Status.Roles[i]
	}

	out := make([]workercore.ModelDeploymentForgerRoleStatus, 0, len(mdf.Spec.Roles))
	for i := range mdf.Spec.Roles {
		name := mdf.Spec.Roles[i].Name
		st, ok := existingByName[name]
		if !ok {
			st = workercore.ModelDeploymentForgerRoleStatus{
				Name:  name,
				Phase: workercore.ModelDeploymentForgerRolePhaseIdle,
			}
		} else if st.Phase == "" {
			st.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		}
		out = append(out, st)
	}
	mdf.Status.Roles = out
}

func forgerRoleStatusPtr(
	mdf *workercore.ModelDeploymentForger, roleName string,
) *workercore.ModelDeploymentForgerRoleStatus {
	for i := range mdf.Status.Roles {
		if mdf.Status.Roles[i].Name == roleName {
			return &mdf.Status.Roles[i]
		}
	}
	return nil
}

func appendForgerGenerationOutcome(
	roleStatus *workercore.ModelDeploymentForgerRoleStatus,
	entry workercore.ModelDeploymentForgerGenerationStatus,
) {
	if roleStatus == nil {
		return
	}
	filtered := slices.DeleteFunc(roleStatus.Generations, func(g workercore.ModelDeploymentForgerGenerationStatus) bool {
		return g.Generation == entry.Generation
	})
	filtered = append(filtered, entry)
	if len(filtered) > forgerMaxGenerationHistory {
		filtered = filtered[len(filtered)-forgerMaxGenerationHistory:]
	}
	roleStatus.Generations = filtered
}

func (r *ModelDeploymentForgerReconciler) updateSummaryReadyCondition(
	mdf *workercore.ModelDeploymentForger, now time.Time,
) {
	for i := range mdf.Status.Roles {
		rs := &mdf.Status.Roles[i]
		if rs.Phase != "" && rs.Phase != workercore.ModelDeploymentForgerRolePhaseIdle {
			r.setCondition(
				mdf,
				workercore.ModelDeploymentForgerConditionTypeReady,
				meta.ConditionFalse,
				workercore.ModelDeploymentForgerReasonInProgress,
				"role "+rs.Name+" is in phase "+string(rs.Phase),
				now,
			)
			return
		}
	}

	anyFailed := false
	anyRolledBack := false
	var lastMsg string
	for i := range mdf.Status.Roles {
		gens := mdf.Status.Roles[i].Generations
		if len(gens) == 0 {
			continue
		}
		last := gens[len(gens)-1]
		switch last.Outcome {
		case workercore.ModelDeploymentForgerGenerationOutcomeFailed:
			anyFailed = true
			lastMsg = last.Message
		case workercore.ModelDeploymentForgerGenerationOutcomeRolledBack:
			anyRolledBack = true
			if lastMsg == "" {
				lastMsg = last.Message
			}
		}
	}

	switch {
	case anyFailed:
		if lastMsg == "" {
			lastMsg = "the most recent forge generation failed"
		}
		r.setCondition(
			mdf,
			workercore.ModelDeploymentForgerConditionTypeReady,
			meta.ConditionFalse,
			workercore.ModelDeploymentForgerReasonDegraded,
			lastMsg,
			now,
		)
	case anyRolledBack:
		if lastMsg == "" {
			lastMsg = "the most recent promotion failed canary verification and rolled back cleanly"
		}
		r.setCondition(
			mdf,
			workercore.ModelDeploymentForgerConditionTypeReady,
			meta.ConditionTrue,
			workercore.ModelDeploymentForgerReasonRollbackCompleted,
			lastMsg,
			now,
		)
	default:
		r.setCondition(
			mdf,
			workercore.ModelDeploymentForgerConditionTypeReady,
			meta.ConditionTrue,
			workercore.ModelDeploymentForgerReasonIdle,
			"all targeted roles are idle between passes",
			now,
		)
	}
}
