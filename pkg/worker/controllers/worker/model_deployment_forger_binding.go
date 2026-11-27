package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
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

func readForgerDecision(md *workercore.ModelDeployment) (workercore.ModelDeploymentForgerDecision, bool, error) {
	if md == nil {
		return workercore.ModelDeploymentForgerDecision{}, false, nil
	}
	value := md.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey]
	if value == "" {
		return workercore.ModelDeploymentForgerDecision{}, false, nil
	}
	var decision workercore.ModelDeploymentForgerDecision
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
	ownedByPrefix, promotedAt, hasMeta := parseForgerDecisionID(decision.DecisionID, string(mdf.UID))
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
		alreadyPromoted := false
		for _, g := range rs.Generations {
			if g.ArtifactName == committedArtifact &&
				(g.Outcome == workercore.ModelDeploymentForgerGenerationOutcomePromoted ||
					g.Outcome == workercore.ModelDeploymentForgerGenerationOutcomeRolledBack) {
				alreadyPromoted = true
				break
			}
		}
		if alreadyPromoted {
			if rs.ActiveKernelArtifact == "" {
				rs.ActiveKernelArtifact = committedArtifact
			}
			continue
		}

		if rs.Phase == workercore.ModelDeploymentForgerRolePhasePublishing ||
			(rs.Phase == workercore.ModelDeploymentForgerRolePhaseIdle && rs.ActiveKernelArtifact != committedArtifact) {
			rs.ActiveKernelArtifact = committedArtifact
			if parsedGen, genOK := parseForgerArtifactGeneration(mdf.Name, rs.Name, committedArtifact); genOK && parsedGen > rs.ActiveGeneration {
				rs.ActiveGeneration = parsedGen
			}
			if mdf.Spec.Mode == workercore.ModelDeploymentForgerModeActive && !mdf.Spec.Suspend {
				rs.Phase = workercore.ModelDeploymentForgerRolePhasePromoting
			}
		}

		if hasMeta && rs.Phase == workercore.ModelDeploymentForgerRolePhasePromoting {
			r.rememberPromotionIfAbsent(mdf.UID, rs.Name, forgerPromotionAttempt{
				ArtifactName: committedArtifact,
				StartedAt:    promotedAt,
			})
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
	for i := range mdf.Spec.Roles {
		name := mdf.Spec.Roles[i].Name
		managedRefs[name] = currentRefs[name]
	}

	preUpdateGen := fresh.Generation
	decisionID := formatForgerDecisionID(string(mdf.UID), term, preUpdateGen, now)
	decisionBytes, err := json.Marshal(workercore.ModelDeploymentForgerDecision{
		DecisionID:      decisionID,
		Term:            term,
		KernelArtifacts: managedRefs,
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
