package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const modelDeploymentForgerInitialTerm = int64(1)

func readForgerBinding(md *workercore.ModelDeployment) (workercore.ModelDeploymentForgerBinding, bool, error) {
	if md == nil {
		return workercore.ModelDeploymentForgerBinding{}, false, nil
	}
	value := md.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey]
	if value == "" {
		return workercore.ModelDeploymentForgerBinding{}, false, nil
	}
	var binding workercore.ModelDeploymentForgerBinding
	if err := json.Unmarshal([]byte(value), &binding); err != nil {
		return binding, false, fmt.Errorf("decode target forger binding: %w", err)
	}
	return binding, true, nil
}

type forgerDecisionPayload struct {
	workercore.ModelDeploymentForgerDecision
	RolePromotedAt map[string]int64 `json:"rolePromotedAt,omitempty"`
}

func readForgerDecision(md *workercore.ModelDeployment) (forgerDecisionPayload, bool, error) {
	if md == nil {
		return forgerDecisionPayload{}, false, nil
	}
	value := md.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey]
	if value == "" {
		return forgerDecisionPayload{}, false, nil
	}
	var decision forgerDecisionPayload
	if err := json.Unmarshal([]byte(value), &decision); err != nil {
		return decision, false, fmt.Errorf("decode target forger decision: %w", err)
	}
	return decision, true, nil
}

func formatForgerDecisionID(forgerUID string, term, preUpdateMDGen int64, now time.Time) string {
	return fmt.Sprintf("%s-%d-%d-%d", forgerUID, term, preUpdateMDGen, now.UnixNano())
}

func parseForgerDecisionID(
	decisionID, forgerUID string,
) (owned bool, promotedAt time.Time, hasMeta bool) {
	prefix := forgerUID + "-"
	if forgerUID == "" || !strings.HasPrefix(decisionID, prefix) {
		return false, time.Time{}, false
	}
	rest := strings.TrimPrefix(decisionID, prefix)
	parts := strings.Split(rest, "-")
	if len(parts) < 3 {
		return true, time.Time{}, false
	}
	_, errGen := strconv.ParseInt(parts[1], 10, 64)
	ns, errNS := strconv.ParseInt(parts[2], 10, 64)
	if errGen != nil || errNS != nil || ns <= 0 {
		return true, time.Time{}, false
	}
	return true, time.Unix(0, ns).UTC(), true
}

func forgerTerm(mdf *workercore.ModelDeploymentForger) int64 {
	if mdf == nil || mdf.Status.Binding == nil {
		return 0
	}
	return mdf.Status.Binding.Term
}

func (r *ModelDeploymentForgerReconciler) releaseOwnedBinding(
	ctx context.Context, mdf *workercore.ModelDeploymentForger, md *workercore.ModelDeployment,
) error {
	fresh := new(workercore.ModelDeployment)
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(md), fresh); err != nil {
		return ctrlcli.IgnoreNotFound(err)
	}

	binding, ok, err := readForgerBinding(fresh)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "ignoring invalid forger binding annotation during release", "target", fresh.Name)
		return nil //nolint:nilerr // unreadable binding belongs to manual repair rather than blocking cleanup.
	}

	changed := false
	if ok && binding.ForgerUID == string(mdf.UID) {
		delete(fresh.Annotations, workercore.ModelDeploymentForgerBindingAnnotationKey)
		changed = true
	}
	if !mdf.DeletionTimestamp.IsZero() {
		if decision, hasDec, decErr := readForgerDecision(fresh); decErr == nil && hasDec {
			owned, _, _ := parseForgerDecisionID(decision.DecisionID, string(mdf.UID))
			if owned || (ok && binding.ForgerUID == string(mdf.UID)) {
				delete(fresh.Annotations, workercore.ModelDeploymentForgerDecisionAnnotationKey)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	if err := r.Client.Update(ctx, fresh); err != nil {
		return err
	}
	*md = *fresh
	return nil
}

func hasTerminalGenerationOutcome(
	forgerName string, rs *workercore.ModelDeploymentForgerRoleStatus, artifactName string,
) bool {
	if rs == nil || artifactName == "" {
		return false
	}
	for _, g := range rs.Generations {
		if g.ArtifactName == artifactName &&
			(g.Outcome == workercore.ModelDeploymentForgerGenerationOutcomePromoted ||
				g.Outcome == workercore.ModelDeploymentForgerGenerationOutcomeRolledBack) {
			return true
		}
	}
	if len(rs.Generations) >= forgerMaxGenerationHistory {
		if parsedGen, ok := parseForgerArtifactGeneration(forgerName, rs.Name, artifactName); ok &&
			parsedGen < rs.Generations[0].Generation {
			return true
		}
	}
	return false
}

func resolveRoleDecisionPromotedAt(
	decision forgerDecisionPayload, forgerUID, roleName, artifactName string,
) (time.Time, bool) {
	if artifactName == "" {
		return time.Time{}, false
	}
	owned, fallbackTS, hasFallback := parseForgerDecisionID(decision.DecisionID, forgerUID)
	if !owned || decision.KernelArtifacts[roleName] != artifactName {
		return time.Time{}, false
	}
	if tsNS, ok := decision.RolePromotedAt[roleName]; ok && tsNS > 0 {
		return time.Unix(0, tsNS).UTC(), true
	}
	if hasFallback {
		return fallbackTS, true
	}
	return time.Time{}, false
}

func (r *ModelDeploymentForgerReconciler) rollbackUncommittedPromotions(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	now time.Time,
) error {
	if mdf == nil || md == nil {
		return nil
	}
	if binding, hasBinding, bindErr := readForgerBinding(md); bindErr == nil &&
		hasBinding && binding.ForgerUID != string(mdf.UID) {
		return nil
	}

	decision, hasDecision, decErr := readForgerDecision(md)
	ownedDecision := false
	if decErr == nil && hasDecision {
		ownedDecision, _, _ = parseForgerDecisionID(decision.DecisionID, string(mdf.UID))
	}

	for i := range mdf.Status.Roles {
		rs := &mdf.Status.Roles[i]
		mdRole := findModelDeploymentSpecRole(md, rs.Name)
		mdArtifact := ""
		if mdRole != nil && mdRole.KernelArtifactRef != nil {
			mdArtifact = strings.TrimSpace(mdRole.KernelArtifactRef.Name)
		}

		inPromoting := rs.Phase == workercore.ModelDeploymentForgerRolePhasePromoting
		uncommittedActive := rs.ActiveKernelArtifact != "" &&
			mdArtifact == rs.ActiveKernelArtifact &&
			!hasTerminalGenerationOutcome(mdf.Name, rs, rs.ActiveKernelArtifact)
		uncommittedDecision := ownedDecision &&
			mdArtifact != "" &&
			decision.KernelArtifacts[rs.Name] == mdArtifact &&
			!hasTerminalGenerationOutcome(mdf.Name, rs, mdArtifact)

		if !inPromoting && !uncommittedActive && !uncommittedDecision {
			continue
		}

		revertTarget := strings.TrimSpace(rs.PreviousKernelArtifact)
		if mdRole != nil && (mdArtifact != revertTarget || (ownedDecision && decision.KernelArtifacts[rs.Name] != revertTarget)) {
			if err := r.commitRoleKernelArtifact(ctx, mdf, md, rs.Name, revertTarget, now); err != nil {
				return err
			}
		}

		r.forgetPromotion(mdf.UID, rs.Name)
		rs.ActiveKernelArtifact = revertTarget
		rs.PreviousKernelArtifact = ""
		rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
	}
	return nil
}

func (r *ModelDeploymentForgerReconciler) cleanupRemovedRoles(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	now time.Time,
) error {
	if mdf == nil || len(mdf.Status.Roles) == 0 {
		return nil
	}

	specRoles := make(map[string]struct{}, len(mdf.Spec.Roles))
	for i := range mdf.Spec.Roles {
		specRoles[mdf.Spec.Roles[i].Name] = struct{}{}
	}

	hasRemoved := false
	for i := range mdf.Status.Roles {
		if _, kept := specRoles[mdf.Status.Roles[i].Name]; !kept {
			hasRemoved = true
			break
		}
	}
	if !hasRemoved {
		return nil
	}

	canMutateTarget := md != nil
	if md != nil {
		if binding, hasBinding, bindErr := readForgerBinding(md); bindErr == nil &&
			hasBinding && binding.ForgerUID != string(mdf.UID) {
			canMutateTarget = false
		}
	}

	var (
		decision      forgerDecisionPayload
		ownedDecision bool
	)
	if md != nil {
		if dec, hasDec, decErr := readForgerDecision(md); decErr == nil && hasDec {
			decision = dec
			ownedDecision, _, _ = parseForgerDecisionID(dec.DecisionID, string(mdf.UID))
		}
	}

	for i := range mdf.Status.Roles {
		rs := &mdf.Status.Roles[i]
		if _, kept := specRoles[rs.Name]; kept {
			continue
		}

		if attempt, hasAttempt := r.loadProbe(mdf.UID, rs.Name); hasAttempt {
			if attempt.PodName != "" && md != nil {
				if mdRole := findModelDeploymentSpecRole(md, rs.Name); mdRole != nil {
					if pod, _ := r.findPodByName(ctx, md.Namespace, attempt.PodName); pod != nil {
						if target, tErr := r.resolveSentinelTarget(ctx, md, mdRole, pod); tErr == nil {
							_, _ = r.probeClient().DeleteProbe(ctx, target)
						}
					}
				}
			}
			r.forgetProbe(mdf.UID, rs.Name)
		}

		if sb := rs.ActiveSandbox; sb != nil {
			if sb.PodName != "" {
				delPod := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: mdf.Namespace, Name: sb.PodName}}
				if err := ctrlcli.IgnoreNotFound(r.Client.Delete(ctx, delPod)); err != nil {
					return err
				}
			}
			if sb.ConfigMapName != "" {
				delCM := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: mdf.Namespace, Name: sb.ConfigMapName}}
				if err := ctrlcli.IgnoreNotFound(r.Client.Delete(ctx, delCM)); err != nil {
					return err
				}
			}
			rs.ActiveSandbox = nil
		}
		if err := r.sweepOrphanRoleSandboxes(ctx, mdf, rs.Name, nil); err != nil {
			return err
		}

		if canMutateTarget {
			mdRole := findModelDeploymentSpecRole(md, rs.Name)
			mdArtifact := ""
			if mdRole != nil && mdRole.KernelArtifactRef != nil {
				mdArtifact = strings.TrimSpace(mdRole.KernelArtifactRef.Name)
			}

			inPromoting := rs.Phase == workercore.ModelDeploymentForgerRolePhasePromoting
			uncommittedActive := rs.ActiveKernelArtifact != "" &&
				mdArtifact == rs.ActiveKernelArtifact &&
				!hasTerminalGenerationOutcome(mdf.Name, rs, rs.ActiveKernelArtifact)
			uncommittedDecision := ownedDecision &&
				mdArtifact != "" &&
				decision.KernelArtifacts[rs.Name] == mdArtifact &&
				!hasTerminalGenerationOutcome(mdf.Name, rs, mdArtifact)

			if inPromoting || uncommittedActive || uncommittedDecision {
				revertTarget := strings.TrimSpace(rs.PreviousKernelArtifact)
				if revertTarget == "" && rs.ActiveKernelArtifact != "" &&
					rs.ActiveKernelArtifact != mdArtifact &&
					hasTerminalGenerationOutcome(mdf.Name, rs, rs.ActiveKernelArtifact) {
					revertTarget = strings.TrimSpace(rs.ActiveKernelArtifact)
				}
				_, decisionHasRole := decision.KernelArtifacts[rs.Name]
				if mdRole != nil && (mdArtifact != revertTarget || (ownedDecision && decisionHasRole)) {
					if err := r.commitRoleKernelArtifact(ctx, mdf, md, rs.Name, revertTarget, now); err != nil {
						return err
					}
				}
				rs.ActiveKernelArtifact = revertTarget
				rs.PreviousKernelArtifact = ""
				rs.Phase = workercore.ModelDeploymentForgerRolePhaseIdle
			}
		}
		r.forgetPromotion(mdf.UID, rs.Name)
	}
	return nil
}

func (r *ModelDeploymentForgerReconciler) recoverDecision(
	ctx context.Context, mdf *workercore.ModelDeploymentForger, md *workercore.ModelDeployment,
) {
	decision, ok, err := readForgerDecision(md)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "ignoring invalid forger decision annotation", "target", md.Name)
		return
	}
	if !ok {
		return
	}

	binding, hasBinding, bindErr := readForgerBinding(md)
	if bindErr != nil {
		return
	}
	ownedByPrefix, _, _ := parseForgerDecisionID(decision.DecisionID, string(mdf.UID))
	if hasBinding {
		if binding.ForgerUID != string(mdf.UID) || decision.Term != binding.Term {
			return
		}
	} else if !ownedByPrefix {
		return
	}
	if mdf.Status.Binding != nil && mdf.Status.Binding.TargetUID != "" && mdf.Status.Binding.TargetUID != string(md.UID) {
		return
	}

	ensureForgerRoleStatuses(mdf)
	roleRefsOnMD := modelDeploymentKernelArtifactRefs(md)

	for i := range mdf.Status.Roles {
		rs := &mdf.Status.Roles[i]
		committedArtifact, managed := decision.KernelArtifacts[rs.Name]
		if !managed {
			continue
		}
		if roleRefsOnMD[rs.Name] != committedArtifact {
			continue
		}
		if committedArtifact == "" {
			continue
		}
		if hasTerminalGenerationOutcome(mdf.Name, rs, committedArtifact) {
			if rs.ActiveKernelArtifact == "" {
				rs.ActiveKernelArtifact = committedArtifact
			}
			continue
		}

		if rs.ActiveKernelArtifact != "" && rs.ActiveKernelArtifact != committedArtifact && rs.PreviousKernelArtifact == "" {
			rs.PreviousKernelArtifact = rs.ActiveKernelArtifact
		}
		if rs.Phase == workercore.ModelDeploymentForgerRolePhasePublishing ||
			rs.Phase == workercore.ModelDeploymentForgerRolePhaseIdle ||
			rs.Phase == "" {
			rs.ActiveKernelArtifact = committedArtifact
			if parsedGen, genOK := parseForgerArtifactGeneration(mdf.Name, rs.Name, committedArtifact); genOK && parsedGen > rs.ActiveGeneration {
				rs.ActiveGeneration = parsedGen
			}
		}
		if mdf.Spec.Mode == workercore.ModelDeploymentForgerModeActive && !mdf.Spec.Suspend {
			rs.ActiveKernelArtifact = committedArtifact
			if parsedGen, genOK := parseForgerArtifactGeneration(mdf.Name, rs.Name, committedArtifact); genOK && parsedGen > rs.ActiveGeneration {
				rs.ActiveGeneration = parsedGen
			}
			rs.Phase = workercore.ModelDeploymentForgerRolePhasePromoting
		}

		if rs.Phase == workercore.ModelDeploymentForgerRolePhasePromoting {
			if promotedAt, hasMeta := resolveRoleDecisionPromotedAt(decision, string(mdf.UID), rs.Name, committedArtifact); hasMeta {
				r.rememberPromotionIfAbsent(mdf.UID, rs.Name, forgerPromotionAttempt{
					ArtifactName: committedArtifact,
					StartedAt:    promotedAt,
				})
			}
		}
	}

	if (mdf.Status.Binding == nil || mdf.Status.Binding.TargetUID == "") && hasBinding {
		mdf.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
			TargetUID:  string(md.UID),
			TargetName: md.Name,
			Term:       decision.Term,
		}
	}
}

func (r *ModelDeploymentForgerReconciler) commitRoleKernelArtifact(
	ctx context.Context,
	mdf *workercore.ModelDeploymentForger,
	md *workercore.ModelDeployment,
	roleName string,
	artifactName string,
	now time.Time,
) error {
	fresh := new(workercore.ModelDeployment)
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(md), fresh); err != nil {
		return err
	}

	term := forgerTerm(mdf)
	if term < modelDeploymentForgerInitialTerm {
		term = modelDeploymentForgerInitialTerm
	}

	binding, hasBinding, err := readForgerBinding(fresh)
	if err != nil {
		return err
	}
	if !hasBinding || binding.ForgerUID != string(mdf.UID) || binding.Term != term {
		if hasBinding && binding.ForgerUID != string(mdf.UID) {
			return fmt.Errorf("target %s is now bound to forger %s", fresh.Name, binding.ForgerName)
		}
		if fresh.Annotations == nil {
			fresh.Annotations = make(map[string]string)
		}
		rawBind, marshalErr := json.Marshal(workercore.ModelDeploymentForgerBinding{
			ForgerName: mdf.Name,
			ForgerUID:  string(mdf.UID),
			Term:       term,
		})
		if marshalErr != nil {
			return marshalErr
		}
		fresh.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] = string(rawBind)
	}

	roleFound := false
	for i := range fresh.Spec.Roles {
		if fresh.Spec.Roles[i].Name != roleName {
			continue
		}
		roleFound = true
		if artifactName == "" {
			fresh.Spec.Roles[i].KernelArtifactRef = nil
		} else {
			fresh.Spec.Roles[i].KernelArtifactRef = &core.LocalObjectReference{Name: artifactName}
		}
	}
	if !roleFound {
		return fmt.Errorf("target ModelDeployment %q has no role %q", fresh.Name, roleName)
	}

	managedRefs := make(map[string]string, len(mdf.Spec.Roles))
	currentRefs := modelDeploymentKernelArtifactRefs(fresh)
	isTargetedRole := false
	for i := range mdf.Spec.Roles {
		name := mdf.Spec.Roles[i].Name
		managedRefs[name] = currentRefs[name]
		if name == roleName {
			isTargetedRole = true
		}
	}

	rolePromotedAt := make(map[string]int64, len(mdf.Spec.Roles))
	if prevDec, hasPrevDec, decErr := readForgerDecision(fresh); decErr == nil && hasPrevDec && prevDec.Term == term {
		if prevOwned, prevFallbackTS, prevHasFallback := parseForgerDecisionID(prevDec.DecisionID, string(mdf.UID)); prevOwned {
			for i := range mdf.Spec.Roles {
				name := mdf.Spec.Roles[i].Name
				if name == roleName || managedRefs[name] == "" {
					continue
				}
				if tsNS, ok := prevDec.RolePromotedAt[name]; ok && tsNS > 0 && prevDec.KernelArtifacts[name] == managedRefs[name] {
					rolePromotedAt[name] = tsNS
				} else if len(prevDec.RolePromotedAt) == 0 && prevHasFallback && prevDec.KernelArtifacts[name] == managedRefs[name] {
					rolePromotedAt[name] = prevFallbackTS.UnixNano()
				}
			}
		}
	}
	if artifactName == "" || !isTargetedRole {
		delete(rolePromotedAt, roleName)
	} else {
		rolePromotedAt[roleName] = now.UnixNano()
	}
	if len(rolePromotedAt) == 0 {
		rolePromotedAt = nil
	}

	preUpdateGen := fresh.Generation
	decisionID := formatForgerDecisionID(string(mdf.UID), term, preUpdateGen, now)
	decisionBytes, err := json.Marshal(forgerDecisionPayload{
		ModelDeploymentForgerDecision: workercore.ModelDeploymentForgerDecision{
			DecisionID:      decisionID,
			Term:            term,
			KernelArtifacts: managedRefs,
		},
		RolePromotedAt: rolePromotedAt,
	})
	if err != nil {
		return err
	}
	if fresh.Annotations == nil {
		fresh.Annotations = make(map[string]string)
	}
	fresh.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey] = string(decisionBytes)

	if err := r.Client.Update(ctx, fresh); err != nil {
		return err
	}
	if err := r.apiReaderOrClient().Get(ctx, ctrlcli.ObjectKeyFromObject(fresh), fresh); err != nil {
		return err
	}
	*md = *fresh
	return nil
}
