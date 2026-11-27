package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/forger"
	"gpustack.ai/gpustack/pkg/worker/kernelkey"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	forgerDefaultPollInterval    = 2 * time.Second
	forgerIdleRetryInterval      = 5 * time.Second
	forgerProbeWallClockTimeout  = 20 * time.Second
	forgerSandboxQuotaGrace      = 60 * time.Second
	forgerMaxKubeResourceNameLen = 63
)

type forgerBlockState struct {
	Blocked bool
	Reason  string
	Message string
}

func (b *forgerBlockState) record(reason, message string) {
	if b.Blocked {
		return
	}
	b.Blocked = true
	b.Reason = reason
	b.Message = message
}

func evaluateRoleRuntimeHold(
	md *workercore.ModelDeployment, roleName string,
) (reason, message string, held bool) {
	var specRole *workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == roleName {
			specRole = &md.Spec.Roles[i]
			break
		}
	}
	if specRole == nil {
		return "", "", false
	}

	var statusRole *workercore.ModelDeploymentRoleStatus
	for i := range md.Status.Roles {
		if md.Status.Roles[i].Name == roleName {
			statusRole = &md.Status.Roles[i]
			break
		}
	}

	if specRole.Pause || (statusRole != nil && statusRole.RuntimeControl != nil && statusRole.RuntimeControl.Paused) {
		return workercore.ModelDeploymentForgerReasonRolePaused,
			fmt.Sprintf("target role %q is paused", roleName), true
	}

	if statusRole != nil && statusRole.RuntimeControl != nil {
		rc := statusRole.RuntimeControl
		if !rc.Synced {
			return workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced,
				fmt.Sprintf("target role %q runtimeControl is not synced (%s)", roleName, rc.Reason), true
		}
		if modelDeploymentRoleDeclaresPauseOrRuntimeControl(specRole) &&
			rc.ObservedGeneration > 0 && rc.ObservedGeneration < md.Generation {
			return workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced,
				fmt.Sprintf("target role %q runtimeControl generation %d lags deployment generation %d",
					roleName, rc.ObservedGeneration, md.Generation), true
		}
	} else if modelDeploymentRoleDeclaresPauseOrRuntimeControl(specRole) {
		return workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced,
			fmt.Sprintf("target role %q runtimeControl has not reported convergence yet", roleName), true
	}

	if ModelDeploymentConditionRuntimeControlSynced.Exists(md) &&
		!ModelDeploymentConditionRuntimeControlSynced.IsTrue(md) {
		return workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced,
			ModelDeploymentConditionRuntimeControlSynced.GetMessage(md), true
	}

	if ret := md.Status.Retirement; ret != nil &&
		ret.State != workercore.ModelDeploymentRetirementStateAborted &&
		ret.State != workercore.ModelDeploymentRetirementStateCompleted {
		return workercore.ModelDeploymentForgerReasonRetirementInProgress,
			fmt.Sprintf("target ModelDeployment %q has an active replica retirement in state %q", md.Name, ret.State), true
	}

	return "", "", false
}

func (r *ModelDeploymentForgerReconciler) evaluateClusterBlockSignals(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	block *forgerBlockState,
	prevBlockedReason string,
) error {
	if ModelDeploymentConditionKernelsReady.IsFalse(md) &&
		ModelDeploymentConditionKernelsReady.GetReason(md) == workercore.ModelDeploymentReasonKernelPeerSyncDisabled {
		block.record(
			workercore.ModelDeploymentForgerReasonPeerSyncDisabled,
			ModelDeploymentConditionKernelsReady.GetMessage(md),
		)
	}

	list := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(mdf.Namespace)); err != nil {
		return err
	}
	for i := range list.Items {
		mka := &list.Items[i]
		if mka.DeletionTimestamp != nil || !artifactBelongsToForger(mka, mdf) {
			continue
		}
		if ModelKernelArtifactConditionPinned.IsFalse(mka) &&
			ModelKernelArtifactConditionPinned.GetReason(mka) == workercore.ModelKernelArtifactReasonPinnedCapacityExceeded {
			msg := ModelKernelArtifactConditionPinned.GetMessage(mka)
			if msg == "" {
				msg = fmt.Sprintf("ModelKernelArtifact %q exceeded NodeModelStore pin capacity", mka.Name)
			}
			block.record(workercore.ModelDeploymentForgerReasonPinnedCapacityExceeded, msg)
			if prevBlockedReason != workercore.ModelDeploymentForgerReasonPinnedCapacityExceeded && r.Recorder != nil {
				r.Recorder.Event(mdf, core.EventTypeWarning, workercore.ModelDeploymentForgerReasonPinnedCapacityExceeded, msg)
			}
			for j := range mdf.Spec.Roles {
				_ = r.pruneRetiredKernelArtifacts(ctx, mdf, md, mdf.Spec.Roles[j].Name)
			}
			break
		}
	}
	return nil
}

func artifactBelongsToForger(mka *workercore.ModelKernelArtifact, mdf *workercore.ModelDeploymentForger) bool {
	if mka == nil || mdf == nil {
		return false
	}
	if mka.Spec.Source.NodeStore != nil && mka.Spec.Source.NodeStore.ForgerRef.Name == mdf.Name {
		return true
	}
	for i := range mdf.Status.Roles {
		rs := &mdf.Status.Roles[i]
		if rs.ActiveKernelArtifact == mka.Name || rs.PreviousKernelArtifact == mka.Name {
			return true
		}
	}
	return false
}

func forgerArtifactName(forgerName, roleName string, generation int64) string {
	suffix := fmt.Sprintf("-gen-%d", generation)
	base := forgerName + "-" + roleName
	maxBase := 253 - len(suffix)
	if len(base) > maxBase {
		hash := stringx.SumBySHA256(base)[:8]
		base = strings.TrimRight(base[:maxBase-9], "-.") + "-" + hash
	}
	return base + suffix
}

func parseForgerArtifactGeneration(forgerName, roleName, artifactName string) (int64, bool) {
	prefix := forgerName + "-" + roleName + "-gen-"
	if strings.HasPrefix(artifactName, prefix) {
		n, err := strconv.ParseInt(strings.TrimPrefix(artifactName, prefix), 10, 64)
		return n, err == nil && n >= 1
	}
	idx := strings.LastIndex(artifactName, "-gen-")
	if idx < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(artifactName[idx+5:], 10, 64)
	return n, err == nil && n >= 1
}

func forgerSandboxObjectNames(forgerName, roleName string, generation int64) (podName, cmName string) {
	raw := fmt.Sprintf("%s-%s-gen-%d", forgerName, roleName, generation)
	const maxPrefix = forgerMaxKubeResourceNameLen - len("-sandbox")
	prefix := raw
	if len(prefix) > maxPrefix {
		hash := stringx.SumBySHA256(raw)[:8]
		prefix = strings.TrimRight(raw[:maxPrefix-9], "-.") + "-" + hash
	}
	return prefix + "-sandbox", prefix + "-input"
}

func (r *ModelDeploymentForgerReconciler) nextGenerationNumber(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	roleName string,
	rs *workercore.ModelDeploymentForgerRoleStatus,
) (int64, error) {
	highest := rs.ActiveGeneration
	for i := range rs.Generations {
		if rs.Generations[i].Generation > highest {
			highest = rs.Generations[i].Generation
		}
	}

	list := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(mdf.Namespace)); err != nil {
		return 0, err
	}
	for i := range list.Items {
		mka := &list.Items[i]
		if mka.Spec.Source.NodeStore != nil && mka.Spec.Source.NodeStore.ForgerRef.Name == mdf.Name {
			if gen, ok := parseForgerArtifactGeneration(mdf.Name, roleName, mka.Name); ok && gen > highest {
				highest = gen
			}
		}
	}
	return highest + 1, nil
}

func lastPassTimestamp(rs *workercore.ModelDeploymentForgerRoleStatus) time.Time {
	var latest time.Time
	if rs == nil {
		return latest
	}
	for i := range rs.Generations {
		if fin := rs.Generations[i].FinishedAt; fin != nil && fin.After(latest) {
			latest = fin.Time
		}
	}
	for i := range rs.ObservedBottlenecks {
		if obs := rs.ObservedBottlenecks[i].ObservedAt; obs.After(latest) {
			latest = obs.Time
		}
	}
	return latest
}

func (r *ModelDeploymentForgerReconciler) reconcileTargetRoles(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	now time.Time,
	prevBlockedReason string,
) (ctrl.Result, error) {
	ensureForgerRoleStatuses(mdf)

	block := &forgerBlockState{}
	for i := range mdf.Spec.Roles {
		if reason, msg, held := evaluateRoleRuntimeHold(md, mdf.Spec.Roles[i].Name); held {
			block.record(reason, msg)
		}
	}
	if err := r.evaluateClusterBlockSignals(ctx, mdf, md, block, prevBlockedReason); err != nil {
		return ctrl.Result{}, err
	}

	var combined ctrl.Result
	for i := range mdf.Spec.Roles {
		roleSpec := &mdf.Spec.Roles[i]
		rs := forgerRoleStatusPtr(mdf, roleSpec.Name)
		if rs == nil {
			continue
		}
		mdRole := findModelDeploymentSpecRole(md, roleSpec.Name)
		if mdRole == nil {
			continue
		}

		res, err := r.reconcileSingleRole(ctx, before, mdf, md, roleSpec, mdRole, rs, block, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		combined = earliestResult(combined, res)
	}

	if block.Blocked {
		r.setCondition(
			mdf,
			workercore.ModelDeploymentForgerConditionTypeBlocked,
			meta.ConditionTrue,
			block.Reason,
			block.Message,
			now,
		)
	} else {
		r.setCondition(
			mdf,
			workercore.ModelDeploymentForgerConditionTypeBlocked,
			meta.ConditionFalse,
			workercore.ModelDeploymentForgerReasonNoBlock,
			"no runtime, quota, or pin hold is active",
			now,
		)
	}

	r.updateSummaryReadyCondition(mdf, now)
	mdf.Status.ObservedGeneration = mdf.Generation
	if err := r.updateStatus(ctx, before, mdf); err != nil {
		return ctrl.Result{}, err
	}
	return combined, nil
}

func findModelDeploymentSpecRole(md *workercore.ModelDeployment, roleName string) *workercore.ModelDeploymentRole {
	if md == nil {
		return nil
	}
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == roleName {
			return &md.Spec.Roles[i]
		}
	}
	return nil
}

func (r *ModelDeploymentForgerReconciler) reconcileSingleRole(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	block *forgerBlockState,
	now time.Time,
) (ctrl.Result, error) {
	switch rs.Phase {
	case "", workercore.ModelDeploymentForgerRolePhaseIdle:
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return r.reconcileIdleRole(ctx, mdf, md, roleSpec, mdRole, rs, block, now)
	case workercore.ModelDeploymentForgerRolePhaseProbing:
		return r.reconcileProbingRole(ctx, before, mdf, md, roleSpec, mdRole, rs, block, now)
	case workercore.ModelDeploymentForgerRolePhaseForging, workercore.ModelDeploymentForgerRolePhaseVerifying:
		return r.reconcileSandboxRole(ctx, before, mdf, md, roleSpec, mdRole, rs, block, now)
	case workercore.ModelDeploymentForgerRolePhasePublishing:
		return r.reconcilePublishingRole(ctx, before, mdf, md, roleSpec, mdRole, rs, block, now)
	case workercore.ModelDeploymentForgerRolePhasePromoting:
		return r.reconcilePromotingRole(ctx, before, mdf, md, roleSpec, mdRole, rs, now)
	default:
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{Requeue: true}, nil
	}
}

func (r *ModelDeploymentForgerReconciler) reconcileIdleRole(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	block *forgerBlockState,
	now time.Time,
) (ctrl.Result, error) {
	cooldown := effectiveProbeCooldown(mdf)
	if last := lastPassTimestamp(rs); !last.IsZero() {
		if elapsed := now.Sub(last); elapsed < cooldown {
			return ctrl.Result{RequeueAfter: cooldown - elapsed}, nil
		}
	}

	if reason, msg, held := evaluateRoleRuntimeHold(md, roleSpec.Name); held {
		block.record(reason, msg)
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	leaderPod, err := r.selectHealthyLeaderPod(ctx, md, roleSpec.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if leaderPod == nil {
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	target, err := r.resolveSentinelTarget(ctx, md, mdRole, leaderPod)
	if err != nil {
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // transient pod target readiness retries on schedule.
	}

	readyResp, err := r.probeClient().GetReady(ctx, target)
	if err != nil || readyResp == nil || !readyResp.Ready || strings.TrimSpace(readyResp.BootID) == "" {
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // wait until sentinel reports ready with a valid boot_id.
	}

	minActive := effectiveMinActiveRequests(mdf)
	if minActive > 0 {
		drainTarget := runtimecontrol.DrainTarget{
			Target:       target,
			MetricsPath:  leaderPod.Annotations["prometheus.io/path"],
			Engine:       md.Spec.Engine.Name,
			PodUID:       leaderPod.UID,
			BootID:       readyResp.BootID,
			RestartCount: modelDeploymentScalerRestartCount(leaderPod),
		}
		reading, readErr := r.drainReader().Read(ctx, drainTarget)
		if readErr != nil || reading == nil || reading.Running < float64(minActive) {
			return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // hold probing until active traffic meets minActiveRequests.
		}
	}

	hw, normVer, err := r.resolveTargetCompatibility(ctx, md, mdRole, leaderPod.Spec.NodeName)
	if err != nil {
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // wait for node feature labels and accelerator type to settle.
	}

	steps := effectiveProbeSteps(mdf)
	startResp, err := r.probeClient().StartProbe(ctx, target, runtimecontrol.ProbeRequest{
		ExpectedBootID: readyResp.BootID,
		Steps:          steps,
		EngineVersion:  normVer,
		Manufacturer:   hw.Manufacturer,
		Architecture:   hw.Arch,
	})
	engineLabel := strings.ToLower(md.Spec.Engine.Name)
	if err != nil {
		var cooldownErr *runtimecontrol.ProbeCooldownError
		if errors.As(err, &cooldownErr) {
			recordForgerProbeMetric(engineLabel, forgerProbeResultCooldown, 0)
			wait := cooldownErr.RetryAfter
			if wait <= 0 {
				wait = forgerIdleRetryInterval
			}
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		if errors.Is(err, runtimecontrol.ErrRolePaused) {
			block.record(workercore.ModelDeploymentForgerReasonRolePaused, err.Error())
			return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
		}
		if !errors.Is(err, runtimecontrol.ErrProbeInProgress) {
			recordForgerProbeMetric(engineLabel, forgerProbeResultError, 0)
			if r.Recorder != nil {
				r.Recorder.Eventf(mdf, core.EventTypeWarning, "ProbeFailed", "failed starting sentinel probe on %s: %v", leaderPod.Name, err)
			}
			return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
		}
	}

	if effectiveForgerMode(mdf) != workercore.ModelDeploymentForgerModeObserveOnly {
		nextGen, genErr := r.nextGenerationNumber(ctx, mdf, roleSpec.Name, rs)
		if genErr != nil {
			return ctrl.Result{}, genErr
		}
		rs.ActiveGeneration = nextGen
	}

	r.rememberProbe(mdf.UID, roleSpec.Name, forgerProbeAttempt{
		PodName:   leaderPod.Name,
		PodUID:    string(leaderPod.UID),
		BootID:    readyResp.BootID,
		NodeName:  leaderPod.Spec.NodeName,
		StartedAt: now,
	})
	rs.Phase = workercore.ModelDeploymentForgerRolePhaseProbing

	if startResp != nil && startResp.State == runtimecontrol.ProbeStateCompleted && startResp.Payload != nil {
		return r.finishCompletedProbe(ctx, mdf, md, roleSpec, mdRole, rs, leaderPod.Spec.NodeName, startResp, now, now)
	}
	return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
}

func (r *ModelDeploymentForgerReconciler) reconcileProbingRole(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	block *forgerBlockState,
	now time.Time,
) (ctrl.Result, error) {
	attempt, hasAttempt := r.loadProbe(mdf.UID, roleSpec.Name)

	if reason, msg, held := evaluateRoleRuntimeHold(md, roleSpec.Name); held {
		block.record(reason, msg)
		if hasAttempt {
			if pod, _ := r.findPodByName(ctx, md.Namespace, attempt.PodName); pod != nil {
				if target, tErr := r.resolveSentinelTarget(ctx, md, mdRole, pod); tErr == nil {
					_, _ = r.probeClient().DeleteProbe(ctx, target)
				}
			}
		}
		r.forgetProbe(mdf.UID, roleSpec.Name)
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	var targetPod *core.Pod
	if hasAttempt && attempt.PodName != "" {
		p, err := r.findPodByName(ctx, md.Namespace, attempt.PodName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if p != nil && p.DeletionTimestamp == nil && (attempt.PodUID == "" || string(p.UID) == attempt.PodUID) {
			targetPod = p
		}
	}
	if targetPod == nil {
		var err error
		targetPod, err = r.selectHealthyLeaderPod(ctx, md, roleSpec.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if targetPod == nil {
		r.forgetProbe(mdf.UID, roleSpec.Name)
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	target, err := r.resolveSentinelTarget(ctx, md, mdRole, targetPod)
	if err != nil {
		r.forgetProbe(mdf.UID, roleSpec.Name)
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // reset to Idle when target pod endpoint disappears.
	}

	probeResp, err := r.probeClient().GetProbe(ctx, target)
	engineLabel := strings.ToLower(md.Spec.Engine.Name)
	if err != nil {
		recordForgerProbeMetric(engineLabel, forgerProbeResultError, 0)
		r.forgetProbe(mdf.UID, roleSpec.Name)
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // retry on next probe window after transport failure.
	}

	startedAt := now
	if hasAttempt && !attempt.StartedAt.IsZero() {
		startedAt = attempt.StartedAt
	}

	switch probeResp.State {
	case runtimecontrol.ProbeStateRunning:
		if now.Sub(startedAt) > forgerProbeWallClockTimeout {
			_, _ = r.probeClient().DeleteProbe(ctx, target)
			recordForgerProbeMetric(engineLabel, forgerProbeResultError, now.Sub(startedAt).Seconds())
			r.forgetProbe(mdf.UID, roleSpec.Name)
			r.recordFailedGeneration(mdf, rs, "ProbeTimeout", "sentinel probe exceeded wall-clock timeout", startedAt, now)
			return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
		}
		return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil

	case runtimecontrol.ProbeStateCompleted:
		r.forgetProbe(mdf.UID, roleSpec.Name)
		if probeResp.Payload == nil {
			recordForgerProbeMetric(engineLabel, forgerProbeResultError, 0)
			r.recordFailedGeneration(mdf, rs, "EmptyProbePayload", "completed probe returned nil payload", startedAt, now)
			return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
		}
		res, finishErr := r.finishCompletedProbe(ctx, mdf, md, roleSpec, mdRole, rs, targetPod.Spec.NodeName, probeResp, startedAt, now)
		if finishErr != nil {
			return ctrl.Result{}, finishErr
		}
		if rs.Phase == workercore.ModelDeploymentForgerRolePhaseForging && rs.ActiveSandbox != nil {
			mdf.Status.ObservedGeneration = mdf.Generation
			if err := r.updateStatus(ctx, before, mdf); err != nil {
				return ctrl.Result{}, err
			}
			*before = *mdf.DeepCopy()
		}
		return res, nil

	case runtimecontrol.ProbeStateFailed, runtimecontrol.ProbeStateAborted:
		recordForgerProbeMetric(engineLabel, forgerProbeResultError, 0)
		r.forgetProbe(mdf.UID, roleSpec.Name)
		msg := strings.TrimSpace(probeResp.Error)
		if msg == "" {
			msg = "sentinel probe ended in state " + probeResp.State
		}
		r.recordFailedGeneration(mdf, rs, "ProbeFailed", msg, startedAt, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil

	default:
		r.forgetProbe(mdf.UID, roleSpec.Name)
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}
}

func (r *ModelDeploymentForgerReconciler) finishCompletedProbe(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	probedNodeName string,
	probeResp *runtimecontrol.ProbeResponse,
	startedAt, now time.Time,
) (ctrl.Result, error) {
	engineLabel := strings.ToLower(md.Spec.Engine.Name)
	dur := probeResp.Diagnostics.WallClockSeconds
	if dur <= 0 && now.After(startedAt) {
		dur = now.Sub(startedAt).Seconds()
	}
	recordForgerProbeMetric(engineLabel, forgerProbeResultSucceeded, dur)

	hw, normVer, err := r.resolveTargetCompatibility(ctx, md, mdRole, probedNodeName)
	if err != nil {
		r.recordFailedGeneration(mdf, rs, "HardwareUnresolved", err.Error(), startedAt, now)
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	coveredKeys, quarantinedKeys, err := r.collectCoveredAndQuarantinedKeys(ctx, mdf, mdRole, rs)
	if err != nil {
		return ctrl.Result{}, err
	}

	targetCompat := kernelkey.CompatibilityTarget{
		Engine:        md.Spec.Engine.Name,
		EngineVersion: normVer,
		Manufacturer:  hw.Manufacturer,
		Arch:          hw.Arch,
	}

	sortedBottlenecks := slices.Clone(probeResp.Payload.Bottlenecks)
	slices.SortFunc(sortedBottlenecks, func(a, b runtimecontrol.ProbeBottleneck) int {
		if c := cmp.Compare(b.GPUTimeSharePercent, a.GPUTimeSharePercent); c != 0 {
			return c
		}
		return strings.Compare(a.LocalKernelKey, b.LocalKernelKey)
	})

	maxKernels := effectiveMaxKernelsPerGeneration(roleSpec)
	observedNow := meta.NewTime(now)
	var observed []workercore.ModelDeploymentForgerBottleneck
	var selectedKeys []string
	baselineLatencies := map[string]float64{}
	seenObserved := map[string]struct{}{}

	for _, b := range sortedBottlenecks {
		parsed, parseErr := kernelkey.Parse(b.LocalKernelKey)
		if parseErr != nil {
			continue
		}
		if excluded, _ := kernelkey.IsExcluded(parsed.OpFamily); excluded {
			continue
		}
		if b.OpFamily != "" {
			if excluded, _ := kernelkey.IsExcluded(b.OpFamily); excluded {
				continue
			}
		}
		if matchErr := kernelkey.MatchCompatibility(parsed, targetCompat); matchErr != nil {
			continue
		}
		if _, dup := seenObserved[b.LocalKernelKey]; dup {
			continue
		}
		seenObserved[b.LocalKernelKey] = struct{}{}

		if len(observed) < forgerMaxObservedBottlenecks {
			share := strconv.FormatFloat(b.GPUTimeSharePercent, 'f', -1, 64)
			if share == "" || math.IsNaN(b.GPUTimeSharePercent) || math.IsInf(b.GPUTimeSharePercent, 0) {
				share = "0"
			}
			observed = append(observed, workercore.ModelDeploymentForgerBottleneck{
				LocalKernelKey:      b.LocalKernelKey,
				OpFamily:            parsed.OpFamily,
				GPUTimeSharePercent: share,
				ObservedAt:          observedNow,
			})
		}

		if _, covered := coveredKeys[b.LocalKernelKey]; covered {
			continue
		}
		if _, quarantined := quarantinedKeys[b.LocalKernelKey]; quarantined {
			continue
		}
		if len(selectedKeys) < maxKernels {
			selectedKeys = append(selectedKeys, b.LocalKernelKey)
			if b.GPUTimeUs > 0 && !math.IsNaN(b.GPUTimeUs) && !math.IsInf(b.GPUTimeUs, 0) {
				perCall := b.GPUTimeUs
				if b.CallCount > 1 {
					perCall = b.GPUTimeUs / float64(b.CallCount)
				}
				baselineLatencies[b.LocalKernelKey] = perCall
			}
		}
	}

	rs.ObservedBottlenecks = observed
	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeNormal, "ProbeCompleted",
			"distilled %d eligible non-GEMM bottlenecks on role %s (%d unforged)",
			len(observed), roleSpec.Name, len(selectedKeys))
	}

	mode := effectiveForgerMode(mdf)
	cooldown := effectiveProbeCooldown(mdf)

	if mode == workercore.ModelDeploymentForgerModeObserveOnly {
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: cooldown}, nil
	}

	if rs.ActiveGeneration < 1 {
		nextGen, genErr := r.nextGenerationNumber(ctx, mdf, roleSpec.Name, rs)
		if genErr != nil {
			return ctrl.Result{}, genErr
		}
		rs.ActiveGeneration = nextGen
	}

	if len(selectedKeys) == 0 {
		startMeta := meta.NewTime(startedAt)
		finMeta := meta.NewTime(now)
		appendForgerGenerationOutcome(rs, workercore.ModelDeploymentForgerGenerationStatus{
			Generation: rs.ActiveGeneration,
			Outcome:    workercore.ModelDeploymentForgerGenerationOutcomeSkipped,
			Reason:     workercore.ModelDeploymentForgerReasonNoEligibleBottlenecks,
			Message:    "probing found no unforged unquarantined single-GPU non-GEMM bottlenecks",
			StartedAt:  &startMeta,
			FinishedAt: &finMeta,
		})
		recordForgerPromotionMetric(forgerPromotionOutcomeSkipped)
		if r.Recorder != nil {
			r.Recorder.Eventf(mdf, core.EventTypeNormal, workercore.ModelDeploymentForgerReasonNoEligibleBottlenecks,
				"skipped generation %d for role %s: no eligible unforged bottlenecks", rs.ActiveGeneration, roleSpec.Name)
		}
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		return ctrl.Result{RequeueAfter: cooldown}, nil
	}

	return r.launchForgeSandbox(ctx, mdf, md, roleSpec, mdRole, rs, hw, normVer, selectedKeys, baselineLatencies, probeResp.Payload, now)
}

func (r *ModelDeploymentForgerReconciler) launchForgeSandbox(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	hw kernelkey.NodeHardware,
	normEngineVersion string,
	selectedKeys []string,
	baselineLatencies map[string]float64,
	payload *runtimecontrol.ProbePayload,
	now time.Time,
) (ctrl.Result, error) {
	accType, err := r.getAcceleratorType(ctx, mdRole.AcceleratorType)
	if err != nil {
		return ctrl.Result{}, err
	}
	if accType == nil || strings.TrimSpace(accType.Status.Entrance) == "" {
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	operatorImg, err := r.resolveOperatorImage(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	minSpeedup := effectiveMinSpeedupRatio(roleSpec)
	thresholds := forger.DefaultVerificationThresholds()
	thresholds.MinSpeedupRatio = minSpeedup

	defs, workloads := buildSandboxDefinitionsAndWorkloads(payload, selectedKeys)
	podName, cmName := forgerSandboxObjectNames(mdf.Name, roleSpec.Name, rs.ActiveGeneration)

	cm, items, err := forger.BuildInputConfigMap(forger.InputConfigMapSpec{
		Forger:     mdf,
		Name:       cmName,
		RoleName:   roleSpec.Name,
		Generation: rs.ActiveGeneration,
		Request: forger.ForgeRequest{
			SchemaVersion:       forger.ForgeRequestSchemaVersion,
			Engine:              md.Spec.Engine.Name,
			EngineVersion:       normEngineVersion,
			Manufacturer:        hw.Manufacturer,
			Architecture:        hw.Arch,
			TargetKernelKeys:    selectedKeys,
			BaselineLatenciesUs: baselineLatencies,
			MinSpeedupRatio:     thresholds.MinSpeedupRatio,
			Atol:                thresholds.Atol,
			Rtol:                thresholds.Rtol,
		},
		Definitions: defs,
		Workloads:   workloads,
	})
	if err != nil {
		r.recordFailedGeneration(mdf, rs, "InvalidSandboxInput", err.Error(), now, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	if err := r.Client.Create(ctx, cm); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		existingCM := new(core.ConfigMap)
		if getErr := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(cm), existingCM); getErr != nil {
			return ctrl.Result{}, getErr
		}
		items = forger.ConfigMapVolumeItems(existingCM)
	}

	engineImg := strings.TrimSpace(mdRole.Image)
	if engineImg == "" {
		engineImg = strings.TrimSpace(md.Spec.Engine.Image)
	}

	pod, err := forger.BuildSandboxPod(forger.SandboxPodSpec{
		Forger:              mdf,
		Name:                podName,
		RoleName:            roleSpec.Name,
		Generation:          rs.ActiveGeneration,
		InputConfigMapName:  cmName,
		InputConfigMapItems: items,
		EntranceQueue:       accType.Status.Entrance,
		Manufacturer:        hw.Manufacturer,
		Hardware:            hw,
		OperatorImage:       operatorImg,
		EngineImage:         engineImg,
		SandboxSpec:         mdf.Spec.Sandbox,
		Thresholds:          &thresholds,
		ImagePullSecrets:    mdRole.ImagePullSecrets,
	})
	if err != nil {
		_ = r.Client.Delete(ctx, cm)
		r.recordFailedGeneration(mdf, rs, "InvalidSandboxSpec", err.Error(), now, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	if err := r.Client.Create(ctx, pod); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		existingPod := new(core.Pod)
		if getErr := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(pod), existingPod); getErr != nil {
			return ctrl.Result{}, getErr
		}
		pod = existingPod
	}

	startedMeta := meta.NewTime(now)
	rs.Phase = workercore.ModelDeploymentForgerRolePhaseForging
	rs.ActiveSandbox = &workercore.ModelDeploymentForgerSandboxStatus{
		PodName:       pod.Name,
		PodUID:        string(pod.UID),
		NodeName:      pod.Spec.NodeName,
		ConfigMapName: cmName,
		StartedAt:     &startedMeta,
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeNormal, "SandboxCreated",
			"created Kueue ForgeSandbox Pod %s for role %s generation %d",
			pod.Name, roleSpec.Name, rs.ActiveGeneration)
	}
	return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
}

func buildSandboxDefinitionsAndWorkloads(
	payload *runtimecontrol.ProbePayload, selectedKeys []string,
) ([]forger.Definition, []forger.Workload) {
	if payload == nil {
		return nil, nil
	}
	selectedSet := make(map[string]struct{}, len(selectedKeys))
	for _, k := range selectedKeys {
		selectedSet[k] = struct{}{}
	}

	selectedDefNames := map[string]struct{}{}
	var defs []forger.Definition
	for _, pd := range payload.Definitions {
		if pd.LocalKernelKey != "" {
			if _, ok := selectedSet[pd.LocalKernelKey]; !ok {
				continue
			}
		}
		axes := make(map[string]forger.AxisSpec, len(pd.Axes))
		for k, v := range pd.Axes {
			var valCopy *int64
			if v.Value != nil {
				valCopy = ptr.To(*v.Value)
			}
			axes[k] = forger.AxisSpec{Type: v.Type, Value: valCopy}
		}
		inputs := make(map[string]forger.TensorSpec, len(pd.Inputs))
		for k, v := range pd.Inputs {
			inputs[k] = forger.TensorSpec{Shape: slices.Clone(v.Shape), Dtype: v.Dtype}
		}
		outputs := make(map[string]forger.TensorSpec, len(pd.Outputs))
		for k, v := range pd.Outputs {
			outputs[k] = forger.TensorSpec{Shape: slices.Clone(v.Shape), Dtype: v.Dtype}
		}
		def := forger.Definition{
			Name:           pd.Name,
			OpType:         pd.OpType,
			LocalKernelKey: pd.LocalKernelKey,
			Axes:           axes,
			Inputs:         inputs,
			Outputs:        outputs,
			Reference:      pd.Reference,
			Tags:           slices.Clone(pd.Tags),
		}
		if forger.ValidateDefinition(&def) == nil {
			defs = append(defs, def)
			selectedDefNames[pd.Name] = struct{}{}
		}
	}

	var workloads []forger.Workload
	for _, pw := range payload.Workloads {
		if pw.LocalKernelKey != "" {
			if _, ok := selectedSet[pw.LocalKernelKey]; !ok {
				continue
			}
		} else if pw.Definition != "" && len(selectedDefNames) > 0 {
			if _, ok := selectedDefNames[pw.Definition]; !ok {
				continue
			}
		}
		wlAxes := make(map[string]int64, len(pw.Axes))
		for k, v := range pw.Axes {
			wlAxes[k] = v
		}
		wlInputs := make(map[string]forger.WorkloadInput, len(pw.Inputs))
		for k, v := range pw.Inputs {
			wlInputs[k] = forger.WorkloadInput{Type: v.Type, Value: v.Value}
		}
		wl := forger.Workload{
			UUID:               pw.UUID,
			Definition:         pw.Definition,
			LocalKernelKey:     pw.LocalKernelKey,
			Axes:               wlAxes,
			Inputs:             wlInputs,
			ObservedDurationUs: pw.ObservedDurationUs,
			CallCount:          pw.CallCount,
		}
		if forger.ValidateWorkload(&wl) == nil {
			workloads = append(workloads, wl)
		}
	}
	return defs, workloads
}

func (r *ModelDeploymentForgerReconciler) reconcileSandboxRole(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	block *forgerBlockState,
	now time.Time,
) (ctrl.Result, error) {
	sb := rs.ActiveSandbox
	if sb == nil || sb.PodName == "" {
		r.recordFailedGeneration(mdf, rs, "SandboxMissing", "activeSandbox metadata was missing during sandbox execution", now, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	startedAt := now
	if sb.StartedAt != nil {
		startedAt = sb.StartedAt.Time
	}
	runnerMode := string(effectiveRunnerMode(mdf))

	pod := new(core.Pod)
	err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Namespace: mdf.Namespace, Name: sb.PodName}, pod)
	if err != nil {
		if kerrors.IsNotFound(err) {
			r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
			rs.ActiveSandbox = nil
			recordForgerSandboxMetric(runnerMode, forgerSandboxResultFailed)
			r.recordFailedGeneration(mdf, rs, "SandboxLost", "ForgeSandbox Pod disappeared before completion", startedAt, now)
			return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
		}
		return ctrl.Result{}, err
	}
	if sb.PodUID != "" && string(pod.UID) != sb.PodUID {
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
		rs.ActiveSandbox = nil
		recordForgerSandboxMetric(runnerMode, forgerSandboxResultFailed)
		r.recordFailedGeneration(mdf, rs, "SandboxUIDMismatch", "ForgeSandbox Pod UID changed unexpectedly", startedAt, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	if pod.Spec.NodeName != "" {
		sb.NodeName = pod.Spec.NodeName
	}

	timeout := time.Duration(effectiveSandboxTimeoutSeconds(mdf)) * time.Second
	if now.Sub(startedAt) > timeout {
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
		rs.ActiveSandbox = nil
		recordForgerSandboxMetric(runnerMode, forgerSandboxResultTimeout)
		r.recordFailedGeneration(mdf, rs, "SandboxTimeout", "ForgeSandbox exceeded sandbox.timeoutSeconds", startedAt, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	runnerState, hasRunner := findInitContainerState(pod.Status.InitContainerStatuses, forger.RunnerContainerName)
	verifierState, hasVerifier := findInitContainerState(pod.Status.InitContainerStatuses, forger.VerifierContainerName)

	if hasRunner && runnerState.Terminated != nil && runnerState.Terminated.ExitCode != 0 {
		msg := fmt.Sprintf("forge-runner exited with code %d: %s",
			runnerState.Terminated.ExitCode, strings.TrimSpace(runnerState.Terminated.Message))
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
		rs.ActiveSandbox = nil
		recordForgerSandboxMetric(runnerMode, forgerSandboxResultFailed)
		r.recordFailedGeneration(mdf, rs, "RunnerFailed", msg, startedAt, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	if hasVerifier && verifierState.Terminated != nil && verifierState.Terminated.ExitCode != 0 {
		msg := fmt.Sprintf("forge-verifier exited with code %d: %s",
			verifierState.Terminated.ExitCode, strings.TrimSpace(verifierState.Terminated.Message))
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
		rs.ActiveSandbox = nil
		recordForgerSandboxMetric(runnerMode, forgerSandboxResultVerificationFailed)
		r.recordFailedGeneration(mdf, rs, "VerificationFailed", msg, startedAt, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	runnerDone := hasRunner && runnerState.Terminated != nil && runnerState.Terminated.ExitCode == 0
	verifierDone := hasVerifier && verifierState.Terminated != nil && verifierState.Terminated.ExitCode == 0

	if pod.Status.Phase == core.PodFailed && (!runnerDone || !verifierDone) {
		msg := strings.TrimSpace(pod.Status.Message)
		if msg == "" {
			msg = "ForgeSandbox Pod failed before init containers completed"
		}
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
		rs.ActiveSandbox = nil
		recordForgerSandboxMetric(runnerMode, forgerSandboxResultFailed)
		r.recordFailedGeneration(mdf, rs, "SandboxFailed", msg, startedAt, now)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	if !runnerDone {
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseForging
		if pod.Spec.NodeName == "" && now.Sub(startedAt) >= forgerSandboxQuotaGrace {
			block.record(
				workercore.ModelDeploymentForgerReasonSandboxQuotaPending,
				fmt.Sprintf("ForgeSandbox Pod %s is awaiting Kueue quota admission", pod.Name),
			)
		}
		return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
	}

	if !verifierDone {
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseVerifying
		return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
	}

	if pod.Spec.NodeName == "" {
		return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
	}

	recordForgerSandboxMetric(runnerMode, forgerSandboxResultSucceeded)
	sb.NodeName = pod.Spec.NodeName
	rs.Phase = workercore.ModelDeploymentForgerRolePhasePublishing
	mdf.Status.ObservedGeneration = mdf.Generation
	if err := r.updateStatus(ctx, before, mdf); err != nil {
		return ctrl.Result{}, err
	}
	*before = *mdf.DeepCopy()
	rs = forgerRoleStatusPtr(mdf, roleSpec.Name)
	if rs == nil {
		return ctrl.Result{}, nil
	}

	return r.reconcilePublishingRole(ctx, before, mdf, md, roleSpec, mdRole, rs, block, now)
}

func findInitContainerState(statuses []core.ContainerStatus, name string) (core.ContainerState, bool) {
	for i := range statuses {
		if statuses[i].Name == name {
			return statuses[i].State, true
		}
	}
	return core.ContainerState{}, false
}

func (r *ModelDeploymentForgerReconciler) reconcilePublishingRole(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	block *forgerBlockState,
	now time.Time,
) (ctrl.Result, error) {
	gen := rs.ActiveGeneration
	if gen < 1 {
		gen = 1
		rs.ActiveGeneration = 1
	}
	artifactName := forgerArtifactName(mdf.Name, roleSpec.Name, gen)

	mka := new(workercore.ModelKernelArtifact)
	err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Namespace: mdf.Namespace, Name: artifactName}, mka)
	if err != nil && !kerrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if kerrors.IsNotFound(err) {
		sb := rs.ActiveSandbox
		if sb == nil || sb.PodName == "" || sb.PodUID == "" {
			r.recordFailedGeneration(mdf, rs, "SandboxMissing", "cannot publish kernel artifact without activeSandbox", now, now)
			return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
		}
		startedAt := now
		if sb.StartedAt != nil {
			startedAt = sb.StartedAt.Time
		}

		seedNode := strings.TrimSpace(sb.NodeName)
		if seedNode == "" {
			pod, podErr := r.findPodByName(ctx, mdf.Namespace, sb.PodName)
			if podErr != nil {
				return ctrl.Result{}, podErr
			}
			if pod != nil {
				seedNode = strings.TrimSpace(pod.Spec.NodeName)
				sb.NodeName = seedNode
			}
		}
		if seedNode == "" {
			return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
		}

		compat, compatErr := r.readSandboxInputCompatibility(ctx, mdf.Namespace, sb.ConfigMapName, md, mdRole, seedNode)
		if compatErr != nil {
			r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
			rs.ActiveSandbox = nil
			r.recordFailedGeneration(mdf, rs, "InvalidSandboxInput", compatErr.Error(), startedAt, now)
			return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
		}

		host, port, epErr := r.resolveSeedEndpoint(ctx, seedNode)
		if epErr != nil {
			return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // retry while model-manager pod on seedNode becomes Ready.
		}

		ingester, ingErr := r.ingestClient()
		if ingErr != nil {
			return ctrl.Result{}, ingErr
		}
		ingestResp, callErr := ingester.IngestTree(ctx, host, port, modelstore.IngestTreeRequest{
			Namespace:  mdf.Namespace,
			ForgerName: mdf.Name,
			ForgerUID:  string(mdf.UID),
			PodName:    sb.PodName,
			PodUID:     sb.PodUID,
		})
		if callErr != nil {
			if errors.Is(callErr, forger.ErrIngestInvalidOutput) ||
				errors.Is(callErr, forger.ErrIngestBadRequest) ||
				errors.Is(callErr, forger.ErrIngestForbidden) {
				r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
				rs.ActiveSandbox = nil
				r.recordFailedGeneration(mdf, rs, "IngestRejected", callErr.Error(), startedAt, now)
				return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
			}
			// Retry transient ingest errors while the sandbox hold window remains open.
			return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil //nolint:nilerr // transient seed endpoint errors retry on schedule.
		}

		publishedSeed := strings.TrimSpace(ingestResp.SeedNode)
		if publishedSeed == "" {
			publishedSeed = seedNode
		}

		mka = &workercore.ModelKernelArtifact{
			ObjectMeta: meta.ObjectMeta{
				Namespace: mdf.Namespace,
				Name:      artifactName,
				Labels: map[string]string{
					forger.SandboxLabelName:       "model-deployment-forger",
					forger.SandboxLabelInstance:   mdf.Name,
					forger.SandboxLabelRole:       roleSpec.Name,
					forger.SandboxLabelGeneration: strconv.FormatInt(gen, 10),
				},
			},
			Spec: workercore.ModelKernelArtifactSpec{
				Source: workercore.ModelKernelArtifactSource{
					NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
						ManifestDigest:         ingestResp.ManifestDigest,
						DispatchManifestDigest: ingestResp.DispatchManifestDigest,
						SizeBytes:              ingestResp.SizeBytes,
						FileCount:              ingestResp.FileCount,
						SeedNode:               publishedSeed,
						ForgerRef: workercore.ModelKernelArtifactForgerReference{
							Name: mdf.Name,
							UID:  string(mdf.UID),
						},
					},
				},
				Compatibility: compat,
			},
		}

		if createErr := r.Client.Create(ctx, mka); createErr != nil && !kerrors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, createErr
		}
		if getErr := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(mka), mka); getErr != nil {
			return ctrl.Result{}, getErr
		}

		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, sb)
		rs.ActiveSandbox = nil

		if r.Recorder != nil {
			r.Recorder.Eventf(mdf, core.EventTypeNormal, "KernelArtifactCreated",
				"ingested verified kernel tree %s from node %s and created ModelKernelArtifact %s",
				ingestResp.ManifestDigest, publishedSeed, mka.Name)
		}
	} else if rs.ActiveSandbox != nil {
		r.cleanupRoleSandboxObjects(ctx, mdf.Namespace, rs.ActiveSandbox)
		rs.ActiveSandbox = nil
	}

	if ModelKernelArtifactConditionResolved.IsFalse(mka) {
		reason := ModelKernelArtifactConditionResolved.GetReason(mka)
		if reason == "" {
			reason = workercore.ModelKernelArtifactReasonSourceLost
		}
		msg := ModelKernelArtifactConditionResolved.GetMessage(mka)
		if msg == "" {
			msg = fmt.Sprintf("ModelKernelArtifact %q failed resolution (%s)", mka.Name, reason)
		}
		var digest string
		if mka.Spec.Source.NodeStore != nil {
			digest = mka.Spec.Source.NodeStore.ManifestDigest
		}
		r.recordFailedGenerationWithArtifact(mdf, rs, mka.Name, digest, reason, msg, mka.CreationTimestamp.Time, now)
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	if ModelKernelArtifactConditionPinned.IsFalse(mka) &&
		ModelKernelArtifactConditionPinned.GetReason(mka) == workercore.ModelKernelArtifactReasonPinnedCapacityExceeded {
		block.record(
			workercore.ModelDeploymentForgerReasonPinnedCapacityExceeded,
			ModelKernelArtifactConditionPinned.GetMessage(mka),
		)
		_ = r.pruneRetiredKernelArtifacts(ctx, mdf, md, roleSpec.Name)
	}

	if !ModelKernelArtifactConditionResolved.IsTrue(mka) || mka.Status.Nodes == nil || mka.Status.Nodes.Ready < 1 {
		return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
	}

	manifestDigest := ""
	if mka.Status.Resolved != nil {
		manifestDigest = mka.Status.Resolved.ManifestDigest
	} else if mka.Spec.Source.NodeStore != nil {
		manifestDigest = mka.Spec.Source.NodeStore.ManifestDigest
	}

	if effectiveForgerMode(mdf) == workercore.ModelDeploymentForgerModeShadow {
		startTime := mka.CreationTimestamp.Time
		if startTime.IsZero() {
			startTime = now
		}
		startMeta := meta.NewTime(startTime)
		finMeta := meta.NewTime(now)
		rs.ActiveKernelArtifact = mka.Name
		rs.PreviousKernelArtifact = ""
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		appendForgerGenerationOutcome(rs, workercore.ModelDeploymentForgerGenerationStatus{
			Generation:     gen,
			ArtifactName:   mka.Name,
			ManifestDigest: manifestDigest,
			Outcome:        workercore.ModelDeploymentForgerGenerationOutcomePromoted,
			Reason:         "ShadowPublished",
			Message:        "verified kernel artifact published in Shadow mode without mutating target deployment",
			StartedAt:      &startMeta,
			FinishedAt:     &finMeta,
		})
		recordForgerPromotionMetric(forgerPromotionOutcomePromoted)
		if r.Recorder != nil {
			r.Recorder.Eventf(mdf, core.EventTypeNormal, "ArtifactPublished",
				"published verified ModelKernelArtifact %s in Shadow mode", mka.Name)
		}
		mdf.Status.ObservedGeneration = mdf.Generation
		if err := r.updateStatus(ctx, before, mdf); err != nil {
			return ctrl.Result{}, err
		}
		*before = *mdf.DeepCopy()
		_ = r.pruneRetiredKernelArtifacts(ctx, mdf, md, roleSpec.Name)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	if reason, msg, held := evaluateRoleRuntimeHold(md, roleSpec.Name); held {
		block.record(reason, msg)
		return ctrl.Result{RequeueAfter: forgerIdleRetryInterval}, nil
	}

	prevRef := ""
	if mdRole.KernelArtifactRef != nil && mdRole.KernelArtifactRef.Name != "" && mdRole.KernelArtifactRef.Name != mka.Name {
		prevRef = mdRole.KernelArtifactRef.Name
	}
	rs.PreviousKernelArtifact = prevRef
	rs.ActiveKernelArtifact = mka.Name
	rs.Phase = workercore.ModelDeploymentForgerRolePhasePromoting

	mdf.Status.ObservedGeneration = mdf.Generation
	if err := r.updateStatus(ctx, before, mdf); err != nil {
		return ctrl.Result{}, err
	}
	*before = *mdf.DeepCopy()

	if err := r.commitRoleKernelArtifact(ctx, mdf, md, roleSpec.Name, mka.Name, now); err != nil {
		return ctrl.Result{}, err
	}
	r.rememberPromotion(mdf.UID, roleSpec.Name, forgerPromotionAttempt{
		ArtifactName: mka.Name,
		StartedAt:    now,
	})

	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeNormal, "PromotionStarted",
			"promoted ModelKernelArtifact %s onto %s role %s (previous=%q)",
			mka.Name, md.Name, roleSpec.Name, prevRef)
	}
	return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
}

func (r *ModelDeploymentForgerReconciler) readSandboxInputCompatibility(
	ctx context.Context,
	namespace, cmName string,
	md *workercore.ModelDeployment,
	mdRole *workercore.ModelDeploymentRole,
	seedNode string,
) (workercore.ModelKernelArtifactCompatibility, error) {
	if cmName != "" {
		cm := new(core.ConfigMap)
		if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: cmName}, cm); err == nil {
			if rawReq := cm.Data[forger.ForgeRequestFileName]; rawReq != "" {
				var freq forger.ForgeRequest
				if unmarshalErr := json.Unmarshal([]byte(rawReq), &freq); unmarshalErr == nil && len(freq.TargetKernelKeys) > 0 {
					return workercore.ModelKernelArtifactCompatibility{
						Engine:        md.Spec.Engine.Name,
						EngineVersion: freq.EngineVersion,
						Manufacturer:  freq.Manufacturer,
						Architecture:  freq.Architecture,
						KernelKeys:    freq.TargetKernelKeys,
					}, nil
				}
			}
		}
	}

	hw, normVer, err := r.resolveTargetCompatibility(ctx, md, mdRole, seedNode)
	if err != nil {
		return workercore.ModelKernelArtifactCompatibility{}, err
	}
	return workercore.ModelKernelArtifactCompatibility{}, fmt.Errorf(
		"sandbox input ConfigMap %q for %s/%s had no readable %s",
		cmName, hw.Manufacturer, normVer, forger.ForgeRequestFileName,
	)
}

func (r *ModelDeploymentForgerReconciler) reconcilePromotingRole(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleSpec *workercore.ModelDeploymentForgerRoleSpec,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	now time.Time,
) (ctrl.Result, error) {
	activeName := strings.TrimSpace(rs.ActiveKernelArtifact)
	if activeName == "" {
		activeName = forgerArtifactName(mdf.Name, roleSpec.Name, max(int64(1), rs.ActiveGeneration))
		rs.ActiveKernelArtifact = activeName
	}

	mka := new(workercore.ModelKernelArtifact)
	err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Namespace: mdf.Namespace, Name: activeName}, mka)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return r.rollbackPromotion(ctx, before, mdf, md, roleSpec.Name, rs, nil,
				"ArtifactMissing", fmt.Sprintf("promoted ModelKernelArtifact %q was not found", activeName), now)
		}
		return ctrl.Result{}, err
	}

	if mka.Status.Quarantined {
		qReason := strings.TrimSpace(mka.Status.QuarantineReason)
		if qReason == "" {
			qReason = "Quarantined"
		}
		qMsg := strings.TrimSpace(mka.Status.QuarantineMessage)
		if qMsg == "" {
			qMsg = fmt.Sprintf("ModelKernelArtifact %q is quarantined", mka.Name)
		}
		return r.rollbackPromotion(ctx, before, mdf, md, roleSpec.Name, rs, mka, qReason, qMsg, now)
	}

	if ModelKernelArtifactConditionResolved.IsFalse(mka) &&
		ModelKernelArtifactConditionResolved.GetReason(mka) == workercore.ModelKernelArtifactReasonSourceLost {
		return r.rollbackPromotion(
			ctx, before, mdf, md, roleSpec.Name, rs, mka,
			workercore.ModelKernelArtifactReasonSourceLost,
			fmt.Sprintf("ModelKernelArtifact %q lost all Ready node copies during rollout", mka.Name),
			now,
		)
	}

	if mdRole.KernelArtifactRef == nil || mdRole.KernelArtifactRef.Name != activeName {
		if commitErr := r.commitRoleKernelArtifact(ctx, mdf, md, roleSpec.Name, activeName, now); commitErr != nil {
			return ctrl.Result{}, commitErr
		}
		r.rememberPromotion(mdf.UID, roleSpec.Name, forgerPromotionAttempt{
			ArtifactName: activeName,
			StartedAt:    now,
		})
		mdRole = findModelDeploymentSpecRole(md, roleSpec.Name)
		if mdRole == nil {
			return ctrl.Result{}, nil
		}
	}

	promotedAt := r.resolvePromotionTracking(mdf, md, roleSpec.Name, mka, now)

	converged, loadErrReason, loadErrMsg, checkErr := r.evaluateRoleRolloutConvergence(ctx, md, mdRole, mka)
	if checkErr != nil {
		return ctrl.Result{}, checkErr
	}
	if loadErrReason != "" {
		return r.rollbackPromotion(ctx, before, mdf, md, roleSpec.Name, rs, mka, loadErrReason, loadErrMsg, now)
	}

	if converged {
		manifestDigest := ""
		if mka.Status.Resolved != nil {
			manifestDigest = mka.Status.Resolved.ManifestDigest
		} else if mka.Spec.Source.NodeStore != nil {
			manifestDigest = mka.Spec.Source.NodeStore.ManifestDigest
		}
		startMeta := meta.NewTime(promotedAt)
		finMeta := meta.NewTime(now)
		gen := rs.ActiveGeneration
		if gen < 1 {
			if parsed, ok := parseForgerArtifactGeneration(mdf.Name, roleSpec.Name, mka.Name); ok {
				gen = parsed
				rs.ActiveGeneration = gen
			} else {
				gen = 1
			}
		}

		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		rs.ActiveKernelArtifact = mka.Name
		rs.PreviousKernelArtifact = ""
		appendForgerGenerationOutcome(rs, workercore.ModelDeploymentForgerGenerationStatus{
			Generation:     gen,
			ArtifactName:   mka.Name,
			ManifestDigest: manifestDigest,
			Outcome:        workercore.ModelDeploymentForgerGenerationOutcomePromoted,
			Reason:         "Promoted",
			Message:        "kernel artifact converged across all live role replicas",
			StartedAt:      &startMeta,
			FinishedAt:     &finMeta,
		})
		r.forgetPromotion(mdf.UID, roleSpec.Name)
		recordForgerPromotionMetric(forgerPromotionOutcomePromoted)

		if r.Recorder != nil {
			r.Recorder.Eventf(mdf, core.EventTypeNormal, "GenerationPromoted",
				"ModelKernelArtifact %s converged across role %s replicas", mka.Name, roleSpec.Name)
			r.Recorder.Eventf(mka, core.EventTypeNormal, "Promoted",
				"converged across ModelDeployment %s role %s", md.Name, roleSpec.Name)
		}

		mdf.Status.ObservedGeneration = mdf.Generation
		if err := r.updateStatus(ctx, before, mdf); err != nil {
			return ctrl.Result{}, err
		}
		*before = *mdf.DeepCopy()
		_ = r.pruneRetiredKernelArtifacts(ctx, mdf, md, roleSpec.Name)
		return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
	}

	failureTimeout := time.Duration(effectiveFailureTimeoutSeconds(mdf)) * time.Second
	if now.Sub(promotedAt) >= failureTimeout {
		return r.rollbackPromotion(
			ctx, before, mdf, md, roleSpec.Name, rs, mka,
			"RolloutTimeout",
			fmt.Sprintf("canary rollout of %q did not converge within %s", mka.Name, failureTimeout),
			now,
		)
	}

	return ctrl.Result{RequeueAfter: forgerDefaultPollInterval}, nil
}

func (r *ModelDeploymentForgerReconciler) resolvePromotionTracking(
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleName string,
	mka *workercore.ModelKernelArtifact,
	now time.Time,
) time.Time {
	if tracked, ok := r.loadPromotion(mdf.UID, roleName); ok && tracked.ArtifactName == mka.Name {
		return tracked.StartedAt
	}

	if decision, hasDec, err := readForgerDecision(md); err == nil && hasDec {
		if _, ts, hasMeta := parseForgerDecisionID(decision.DecisionID, string(mdf.UID)); hasMeta {
			r.rememberPromotion(mdf.UID, roleName, forgerPromotionAttempt{
				ArtifactName: mka.Name,
				StartedAt:    ts,
			})
			return ts
		}
	}

	fallbackStart := now
	if mka.Status.Resolved != nil && !mka.Status.Resolved.ResolvedTime.IsZero() {
		fallbackStart = mka.Status.Resolved.ResolvedTime.Time
	} else if !mka.CreationTimestamp.IsZero() {
		fallbackStart = mka.CreationTimestamp.Time
	}
	r.rememberPromotion(mdf.UID, roleName, forgerPromotionAttempt{
		ArtifactName: mka.Name,
		StartedAt:    fallbackStart,
	})
	return fallbackStart
}

func (r *ModelDeploymentForgerReconciler) evaluateRoleRolloutConvergence(
	ctx context.Context,
	md *workercore.ModelDeployment,
	mdRole *workercore.ModelDeploymentRole,
	mka *workercore.ModelKernelArtifact,
) (converged bool, fatalReason, fatalMessage string, err error) {
	rkRender := buildModelDeploymentKernelRender(mka, kernelArtifactDeliveryKind(mka))
	if rkRender == nil {
		return false, "", "", nil
	}

	allPods, err := r.listDeploymentPods(ctx, md)
	if err != nil {
		return false, "", "", err
	}

	var liveRolePods []core.Pod
	maxOrdinal := 0
	for i := range allPods {
		p := &allPods[i]
		if modelDeploymentPodRole(p) != mdRole.Name {
			continue
		}
		if p.DeletionTimestamp != nil || p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
			continue
		}
		liveRolePods = append(liveRolePods, *p)
		if ord, ok := modelDeploymentPodOrdinal(p); ok && ord > maxOrdinal {
			maxOrdinal = ord
		}
	}

	wantDispatchDigest := rkRender.DispatchManifestDigest
	if wantDispatchDigest == "" && mka.Spec.Source.NodeStore != nil {
		wantDispatchDigest = mka.Spec.Source.NodeStore.DispatchManifestDigest
	}

	// First inspect any live replacement Pod mounting mka for immediate fatal kernel_load_error,
	// even while ModelDeployment holds a RequireReadiness canary slot.
	for i := range liveRolePods {
		p := &liveRolePods[i]
		if !podMountsKernelRender(p, rkRender) {
			continue
		}
		if strings.TrimSpace(p.Status.PodIP) == "" && r.ResolveSentinelTarget == nil {
			continue
		}
		target, tErr := r.resolveSentinelTarget(ctx, md, mdRole, p)
		if tErr != nil {
			continue
		}
		rcState, rcErr := r.probeClient().GetRuntimeConfig(ctx, target)
		if rcErr == nil && rcState != nil {
			loadErr, _, _ := summarizeRuntimeConfigKernelState(rcState)
			if loadErr != "" {
				return false, classifyKernelLoadErrorReason(loadErr),
					fmt.Sprintf("Pod %s reported kernel_load_error %q for %s", p.Name, loadErr, mka.Name), nil
			}
		} else {
			readyState, rErr := r.probeClient().GetReady(ctx, target)
			if rErr == nil && readyState != nil && readyState.KernelLoadError != "" {
				return false, classifyKernelLoadErrorReason(readyState.KernelLoadError),
					fmt.Sprintf("Pod %s readiness reported kernel_load_error %q for %s",
						p.Name, readyState.KernelLoadError, mka.Name), nil
			}
		}
	}

	if !roleObservedKernelArtifactMatches(md, mdRole.Name, mka.Name) {
		return false, "", "", nil
	}

	if _, slotHeld := modelDeploymentReplacementSlotsOf(md).slotFor(mdRole.Name); slotHeld {
		return false, "", "", nil
	}

	if mdRole.Replicas > 0 && len(liveRolePods) == 0 {
		return false, "", "", nil
	}
	if mdRole.Replicas == 0 && len(liveRolePods) == 0 {
		return true, "", "", nil
	}

	views := modelDeploymentGroupPodsByReplica(liveRolePods)
	if len(views) != int(mdRole.Replicas) {
		return false, "", "", nil
	}

	desiredByOrdinal, err := r.renderDesiredRolePodsForCurrency(ctx, md, mdRole.Name, allPods, maxOrdinal+1)
	if err != nil {
		return false, "", "", nil //nolint:nilerr // wait for transient render dependencies (e.g. AcceleratorType) to settle.
	}
	minRenderVer := modelDeploymentEffectiveMinRenderVersion(md)

	for _, view := range views {
		if !view.Seated {
			return false, "", "", nil
		}
		shape := modelDeploymentDeployedReplicaShape(view)
		if !shape.modelDeploymentReplicaIsWhole() {
			return false, "", "", nil
		}
		desiredMembers := desiredByOrdinal[view.Ordinal]
		for _, member := range view.Members {
			if !podIsReady(member) || !podMountsKernelRender(member, rkRender) {
				return false, "", "", nil
			}
			seat, seatState := modelDeploymentMemberSeat(member)
			if seatState == modelDeploymentSeatInvalid {
				return false, "", "", nil
			}
			if seat < 0 || seat >= len(desiredMembers) {
				return false, "", "", nil
			}
			if !modelDeploymentControllerPodMatchesCurrency(
				md, mdRole, view.Ordinal, seat, member, desiredMembers[seat], minRenderVer,
			) {
				return false, "", "", nil
			}
		}

		for _, answering := range answeringMembersOfStanding(md, mdRole, view.Members) {
			target, tErr := r.resolveSentinelTarget(ctx, md, mdRole, answering)
			if tErr != nil {
				return false, "", "", nil //nolint:nilerr // answering pod has no reachable sentinel endpoint yet.
			}
			rcState, rcErr := r.probeClient().GetRuntimeConfig(ctx, target)
			if rcErr != nil || rcState == nil {
				return false, "", "", nil //nolint:nilerr // wait until sentinel runtime_config confirms loaded kernels.
			}
			loadErr, dispDigest, loadedKeys := summarizeRuntimeConfigKernelState(rcState)
			if loadErr != "" {
				return false, classifyKernelLoadErrorReason(loadErr),
					fmt.Sprintf("Pod %s reported kernel_load_error %q for %s", answering.Name, loadErr, mka.Name), nil
			}
			if wantDispatchDigest != "" && dispDigest != wantDispatchDigest {
				return false, "", "", nil
			}
			if len(loadedKeys) == 0 {
				return false, "", "", nil
			}
		}
	}

	return true, "", "", nil
}

func roleObservedKernelArtifactMatches(md *workercore.ModelDeployment, roleName, artifactName string) bool {
	if md == nil || artifactName == "" {
		return false
	}
	if !ModelDeploymentConditionKernelsReady.IsTrue(md) {
		return false
	}
	for i := range md.Status.Roles {
		if md.Status.Roles[i].Name != roleName {
			continue
		}
		ka := md.Status.Roles[i].KernelArtifact
		return ka != nil && ka.Artifact == artifactName && !ka.Quarantined
	}
	return false
}

func summarizeRuntimeConfigKernelState(
	rc *runtimecontrol.RuntimeConfigState,
) (loadErr, dispatchDigest string, loadedKeys []string) {
	if rc == nil {
		return "", "", nil
	}
	st := &ModelDeploymentKernelRuntimeState{
		BootID:                 rc.BootID,
		Ready:                  rc.Ready,
		Paused:                 rc.Paused,
		DispatchManifestDigest: rc.DispatchManifestDigest,
		LoadedKernelKeys:       slices.Clone(rc.LoadedKernelKeys),
		KernelLoadError:        rc.KernelLoadError,
	}
	for i := range rc.Ranks {
		rk := &rc.Ranks[i]
		st.Ranks = append(st.Ranks, ModelDeploymentKernelRuntimeRankState{
			RankID:                 rk.RankID,
			DispatchManifestDigest: rk.DispatchManifestDigest,
			LoadedKernelKeys:       slices.Clone(rk.LoadedKernelKeys),
			KernelLoadError:        rk.KernelLoadError,
		})
	}
	st.normalize()
	return st.KernelLoadError, st.DispatchManifestDigest, st.LoadedKernelKeys
}

func classifyKernelLoadErrorReason(loadErr string) string {
	if strings.TrimSpace(loadErr) == "unsupported_kernel_artifact" {
		return "UnsupportedKernelArtifact"
	}
	return "KernelLoadError"
}

func (r *ModelDeploymentForgerReconciler) renderDesiredRolePodsForCurrency(
	ctx context.Context,
	md *workercore.ModelDeployment,
	roleName string,
	actualPods []core.Pod,
	minOrdinals int,
) (map[int][]*core.Pod, error) {
	projectedMD := md.DeepCopy()
	for i := range projectedMD.Spec.Roles {
		if projectedMD.Spec.Roles[i].Name == roleName && int(projectedMD.Spec.Roles[i].Replicas) < minOrdinals {
			projectedMD.Spec.Roles[i].Replicas = int32(minOrdinals)
		}
	}

	helper := &ModelDeploymentReconciler{
		Client:    r.Client,
		APIReader: r.apiReaderOrClient(),
	}
	domain, err := helper.resolveModelDeploymentDomain(ctx, projectedMD)
	if err != nil {
		return nil, err
	}
	connection, err := helper.resolveModelDeploymentConnection(ctx, domain)
	if err != nil {
		return nil, err
	}
	var interfaceProtocols []string
	if connection != nil {
		interfaceProtocols = connection.Protocols
	} else if modelDeploymentRequestsInterfaces(projectedMD) && projectedMD.Spec.KVCache != nil {
		interfaceProtocols, err = helper.resolveModelDeploymentInterfaceProtocols(ctx, projectedMD)
		if err != nil {
			return nil, err
		}
	}
	liveRemote := modelDeploymentModelTransferLiveRemote(actualPods)
	weights, err := helper.resolveModelDeploymentWeights(ctx, projectedMD, liveRemote)
	if err != nil {
		return nil, err
	}
	routerReady, _, err := helper.resolveModelDeploymentRouterIsolation(ctx, projectedMD)
	if err != nil {
		return nil, err
	}
	kernels, err := helper.resolveModelDeploymentKernels(ctx, projectedMD, actualPods)
	if err != nil {
		return nil, err
	}
	awsEFA := settings.AWSEFAEnabled.ShouldValueBool(ctx)
	rendered, err := helper.renderModelDeploymentRolePodsWithKernels(
		ctx, projectedMD, connection, interfaceProtocols, weights, kernels, awsEFA, ptr.To(routerReady), false,
	)
	if err != nil {
		return nil, err
	}
	return rendered[roleName], nil
}

func (r *ModelDeploymentForgerReconciler) rollbackPromotion(
	ctx context.Context,
	before, mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleName string,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	failedMKA *workercore.ModelKernelArtifact,
	reason, message string,
	now time.Time,
) (ctrl.Result, error) {
	failedName := rs.ActiveKernelArtifact
	var failedDigest string
	startedAt := now
	if tracked, ok := r.loadPromotion(mdf.UID, roleName); ok && !tracked.StartedAt.IsZero() {
		startedAt = tracked.StartedAt
	}

	if failedMKA != nil {
		failedName = failedMKA.Name
		if failedMKA.Status.Resolved != nil {
			failedDigest = failedMKA.Status.Resolved.ManifestDigest
		} else if failedMKA.Spec.Source.NodeStore != nil {
			failedDigest = failedMKA.Spec.Source.NodeStore.ManifestDigest
		}
		if !failedMKA.Status.Quarantined || failedMKA.Status.QuarantineReason != reason {
			freshMKA := new(workercore.ModelKernelArtifact)
			if getErr := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(failedMKA), freshMKA); getErr == nil {
				freshMKA.Status.Quarantined = true
				freshMKA.Status.QuarantineReason = reason
				freshMKA.Status.QuarantineMessage = message
				if stErr := r.Client.Status().Update(ctx, freshMKA); stErr != nil {
					return ctrl.Result{}, stErr
				}
				if r.Recorder != nil {
					r.Recorder.Eventf(freshMKA, core.EventTypeWarning, "Quarantined", "%s: %s", reason, message)
				}
			}
		}
	}

	revertTarget := strings.TrimSpace(rs.PreviousKernelArtifact)
	if err := r.commitRoleKernelArtifact(ctx, mdf, md, roleName, revertTarget, now); err != nil {
		return ctrl.Result{}, err
	}

	r.forgetPromotion(mdf.UID, roleName)

	gen := rs.ActiveGeneration
	if gen < 1 {
		if parsed, ok := parseForgerArtifactGeneration(mdf.Name, roleName, failedName); ok {
			gen = parsed
		} else {
			gen = 1
		}
	}
	startMeta := meta.NewTime(startedAt)
	finMeta := meta.NewTime(now)
	appendForgerGenerationOutcome(rs, workercore.ModelDeploymentForgerGenerationStatus{
		Generation:     gen,
		ArtifactName:   failedName,
		ManifestDigest: failedDigest,
		Outcome:        workercore.ModelDeploymentForgerGenerationOutcomeRolledBack,
		Reason:         reason,
		Message:        message,
		StartedAt:      &startMeta,
		FinishedAt:     &finMeta,
	})

	rs.ActiveKernelArtifact = revertTarget
	rs.PreviousKernelArtifact = ""
	rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle

	recordForgerPromotionMetric(forgerPromotionOutcomeRolledBack)
	recordForgerRollbackMetric(reason)

	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeWarning, "GenerationRolledBack",
			"rolled back role %s from %q to %q (%s): %s", roleName, failedName, revertTarget, reason, message)
	}

	mdf.Status.ObservedGeneration = mdf.Generation
	r.updateSummaryReadyCondition(mdf, now)
	if err := r.updateStatus(ctx, before, mdf); err != nil {
		return ctrl.Result{}, err
	}
	*before = *mdf.DeepCopy()

	return ctrl.Result{RequeueAfter: effectiveProbeCooldown(mdf)}, nil
}

func (r *ModelDeploymentForgerReconciler) pruneRetiredKernelArtifacts(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleName string,
) error {
	maxRetained := effectiveMaxRetainedGenerations(mdf)

	mkaList := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, mkaList, ctrlcli.InNamespace(mdf.Namespace)); err != nil {
		return err
	}
	mdList := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mdList, ctrlcli.InNamespace(mdf.Namespace)); err != nil {
		return err
	}
	mdfList := new(workercore.ModelDeploymentForgerList)
	if err := r.Client.List(ctx, mdfList, ctrlcli.InNamespace(mdf.Namespace)); err != nil {
		return err
	}

	protected := map[string]struct{}{}
	addMDRefs := func(d *workercore.ModelDeployment) {
		if d == nil {
			return
		}
		for i := range d.Spec.Roles {
			if ref := d.Spec.Roles[i].KernelArtifactRef; ref != nil && ref.Name != "" {
				protected[ref.Name] = struct{}{}
			}
		}
	}
	addMDRefs(md)
	for i := range mdList.Items {
		addMDRefs(&mdList.Items[i])
	}
	addForgerRefs := func(f *workercore.ModelDeploymentForger) {
		if f == nil || !f.DeletionTimestamp.IsZero() {
			return
		}
		for i := range f.Status.Roles {
			if f.Status.Roles[i].ActiveKernelArtifact != "" {
				protected[f.Status.Roles[i].ActiveKernelArtifact] = struct{}{}
			}
			if f.Status.Roles[i].PreviousKernelArtifact != "" {
				protected[f.Status.Roles[i].PreviousKernelArtifact] = struct{}{}
			}
		}
	}
	addForgerRefs(mdf)
	for i := range mdfList.Items {
		if mdfList.Items[i].Name == mdf.Name {
			continue
		}
		addForgerRefs(&mdfList.Items[i])
	}

	type genArtifact struct {
		gen int64
		mka *workercore.ModelKernelArtifact
	}
	var owned []genArtifact
	for i := range mkaList.Items {
		mka := &mkaList.Items[i]
		if mka.DeletionTimestamp != nil || mka.Spec.Source.NodeStore == nil {
			continue
		}
		if mka.Spec.Source.NodeStore.ForgerRef.Name != mdf.Name {
			continue
		}
		if roleLbl := mka.Labels[forger.SandboxLabelRole]; roleLbl != "" && roleLbl != roleName {
			continue
		}
		gen, ok := parseForgerArtifactGeneration(mdf.Name, roleName, mka.Name)
		if !ok {
			if genStr := mka.Labels[forger.SandboxLabelGeneration]; genStr != "" {
				parsed, parseErr := strconv.ParseInt(genStr, 10, 64)
				if parseErr == nil && parsed >= 1 {
					gen, ok = parsed, true
				}
			}
		}
		if !ok {
			continue
		}
		owned = append(owned, genArtifact{gen: gen, mka: mka})
	}

	slices.SortFunc(owned, func(a, b genArtifact) int {
		if c := cmp.Compare(b.gen, a.gen); c != 0 {
			return c
		}
		return strings.Compare(b.mka.Name, a.mka.Name)
	})

	var errs []error
	for idx, item := range owned {
		if idx < maxRetained {
			continue
		}
		if _, isProtected := protected[item.mka.Name]; isProtected {
			continue
		}
		if item.mka.Status.Quarantined {
			continue
		}
		if delErr := r.Client.Delete(ctx, item.mka); ctrlcli.IgnoreNotFound(delErr) != nil {
			errs = append(errs, delErr)
		}
	}
	return errors.Join(errs...)
}

func (r *ModelDeploymentForgerReconciler) collectCoveredAndQuarantinedKeys(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	mdRole *workercore.ModelDeploymentRole,
	rs *workercore.ModelDeploymentForgerRoleStatus,
) (covered, quarantined map[string]struct{}, err error) {
	covered = map[string]struct{}{}
	quarantined = map[string]struct{}{}

	list := new(workercore.ModelKernelArtifactList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(mdf.Namespace)); err != nil {
		return nil, nil, err
	}

	activeRefName := ""
	if mdRole != nil && mdRole.KernelArtifactRef != nil {
		activeRefName = mdRole.KernelArtifactRef.Name
	}
	shadowActiveName := ""
	if effectiveForgerMode(mdf) == workercore.ModelDeploymentForgerModeShadow {
		shadowActiveName = rs.ActiveKernelArtifact
	}

	for i := range list.Items {
		mka := &list.Items[i]
		if mka.DeletionTimestamp != nil {
			continue
		}
		if mka.Status.Quarantined {
			for _, k := range mka.Spec.Compatibility.KernelKeys {
				quarantined[k] = struct{}{}
			}
			continue
		}
		if (activeRefName != "" && mka.Name == activeRefName) ||
			(shadowActiveName != "" && mka.Name == shadowActiveName) {
			for _, k := range mka.Spec.Compatibility.KernelKeys {
				covered[k] = struct{}{}
			}
		}
	}
	return covered, quarantined, nil
}

func (r *ModelDeploymentForgerReconciler) recordFailedGeneration(
	mdf *workercore.ModelDeploymentForger,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	reason, message string,
	startedAt, now time.Time,
) {
	r.recordFailedGenerationWithArtifact(mdf, rs, "", "", reason, message, startedAt, now)
}

func (r *ModelDeploymentForgerReconciler) recordFailedGenerationWithArtifact(
	mdf *workercore.ModelDeploymentForger,
	rs *workercore.ModelDeploymentForgerRoleStatus,
	artifactName, manifestDigest, reason, message string,
	startedAt, now time.Time,
) {
	gen := rs.ActiveGeneration
	if gen < 1 {
		gen = 1
		rs.ActiveGeneration = 1
	}
	if startedAt.IsZero() {
		startedAt = now
	}
	startMeta := meta.NewTime(startedAt)
	finMeta := meta.NewTime(now)
	appendForgerGenerationOutcome(rs, workercore.ModelDeploymentForgerGenerationStatus{
		Generation:     gen,
		ArtifactName:   artifactName,
		ManifestDigest: manifestDigest,
		Outcome:        workercore.ModelDeploymentForgerGenerationOutcomeFailed,
		Reason:         reason,
		Message:        message,
		StartedAt:      &startMeta,
		FinishedAt:     &finMeta,
	})
	rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
	recordForgerPromotionMetric(forgerPromotionOutcomeFailed)
	if r.Recorder != nil {
		r.Recorder.Eventf(mdf, core.EventTypeWarning, reason, "role %s generation %d failed: %s", rs.Name, gen, message)
	}
}

func (r *ModelDeploymentForgerReconciler) selectHealthyLeaderPod(
	ctx context.Context, md *workercore.ModelDeployment, roleName string,
) (*core.Pod, error) {
	pods, err := r.listDeploymentPods(ctx, md)
	if err != nil {
		return nil, err
	}
	var candidates []*core.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || modelDeploymentPodRole(p) != roleName {
			continue
		}
		if !podIsReady(p) || p.Spec.NodeName == "" {
			continue
		}
		if p.Labels[modelDeploymentLabelKeyEndpointEligible] != modelDeploymentEndpointEligibleValue {
			continue
		}
		seat, seatState := modelDeploymentMemberSeat(p)
		if seatState == modelDeploymentSeatInvalid || (seatState == modelDeploymentSeatValid && seat != 0) {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	slices.SortFunc(candidates, func(a, b *core.Pod) int {
		ordA, okA := modelDeploymentPodOrdinal(a)
		ordB, okB := modelDeploymentPodOrdinal(b)
		if okA && okB && ordA != ordB {
			return cmp.Compare(ordA, ordB)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return candidates[0], nil
}

func (r *ModelDeploymentForgerReconciler) resolveTargetCompatibility(
	ctx context.Context,
	md *workercore.ModelDeployment,
	mdRole *workercore.ModelDeploymentRole,
	nodeName string,
) (kernelkey.NodeHardware, string, error) {
	rawVer := ModelDeploymentEffectiveEngineVersion(md, mdRole)
	normVer, err := kernelkey.NormalizeEngineVersion(rawVer)
	if err != nil {
		return kernelkey.NodeHardware{}, "", fmt.Errorf("normalize engineVersion %q: %w", rawVer, err)
	}

	var wantManu string
	if mdRole.AcceleratorType != "" {
		accType, atErr := r.getAcceleratorType(ctx, mdRole.AcceleratorType)
		if atErr != nil {
			return kernelkey.NodeHardware{}, "", atErr
		}
		if accType != nil {
			wantManu = strings.ToLower(strings.TrimSpace(accType.Status.Detail.Manufacturer))
		}
	}

	node := new(core.Node)
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, node); err != nil {
		return kernelkey.NodeHardware{}, "", fmt.Errorf("read node %q: %w", nodeName, err)
	}
	hws := kernelkey.ExtractNodeHardwareFromNode(node)
	for _, hw := range hws {
		if wantManu != "" && hw.Manufacturer != wantManu {
			continue
		}
		if hw.Arch == "" {
			continue
		}
		return hw, normVer, nil
	}
	return kernelkey.NodeHardware{}, "", fmt.Errorf("node %q exposes no accelerator architecture labels", nodeName)
}

func (r *ModelDeploymentForgerReconciler) getAcceleratorType(
	ctx context.Context, name string,
) (*worker.AcceleratorType, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}
	v1AT := new(worker.AcceleratorType)
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Name: name}, v1AT); err == nil {
		return v1AT, nil
	} else if !kerrors.IsNotFound(err) {
		return nil, err
	}

	at := new(workercore.AcceleratorType)
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKey{Name: name}, at); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return (*worker.AcceleratorType)(at), nil
}
