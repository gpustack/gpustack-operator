package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	resourcehelper "k8s.io/component-helpers/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	utilpod "sigs.k8s.io/kueue/pkg/util/pod"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// modelDeploymentReplacementSlotsAnnotation carries each role's active replacement slot.
//
// An annotation rather than a status field: it is bookkeeping, not observed state, and it covers
// the window between deleting a replica's members and creating its replacement, in which the
// ordinal is simply empty and no observation can name what is being replaced.
const modelDeploymentReplacementSlotsAnnotation = "modeldeployment.gpustack.ai/replacement-slots"

// modelDeploymentReplacementPhase is how far one slot has been observed to go. Cleanup carries
// the identities the slot captured; WaitingForAdmission carries the render its replacement is
// being built from.
type modelDeploymentReplacementPhase string

const (
	// modelDeploymentReplacementCleanup is a slot whose old members are still leaving.
	modelDeploymentReplacementCleanup modelDeploymentReplacementPhase = "Cleanup"

	// modelDeploymentReplacementWaitingForAdmission holds the role until its replacement is
	// admitted.
	modelDeploymentReplacementWaitingForAdmission modelDeploymentReplacementPhase = "WaitingForAdmission"
)

// modelDeploymentReplacementSlot is one role's active replacement. At most one exists per role:
// a role whose slot this pass did not clear turns over no second healthy old replica.
type modelDeploymentReplacementSlot struct {
	// Role is the role whose replica is being replaced.
	Role string `json:"role"`

	// Ordinal is the slot within that role being replaced.
	Ordinal int `json:"ordinal"`

	// Phase is how far the slot has been observed to go.
	Phase modelDeploymentReplacementPhase `json:"phase"`

	// MemberUIDs are the identities the slot may delete. The derived group name locates
	// candidates; a captured UID is what licenses deleting one.
	MemberUIDs []types.UID `json:"memberUIDs,omitempty"`

	// WorkloadUID is the departing group's Workload. Empty while none has been observed.
	WorkloadUID types.UID `json:"workloadUID,omitempty"`

	// MemberHashes are the fingerprints the replacement is being built from, recorded once the
	// old group has left. A later edit differing from them supersedes it in this same slot.
	MemberHashes []string `json:"memberHashes,omitempty"`

	// CurrentMemberUIDs and CurrentWorkloadUID retain creation observations through member loss.
	CurrentMemberUIDs  []types.UID `json:"currentMemberUIDs,omitempty"`
	CurrentWorkloadUID types.UID   `json:"currentWorkloadUID,omitempty"`
}

// modelDeploymentReplacementSlots is every role's slot, keyed by role name.
type modelDeploymentReplacementSlots map[string]modelDeploymentReplacementSlot

// slotFor returns a role's slot and whether it has one.
func (s modelDeploymentReplacementSlots) slotFor(role string) (modelDeploymentReplacementSlot, bool) {
	slot, held := s[role]

	return slot, held
}

// held reports whether a role has an unresolved replacement.
func (s modelDeploymentReplacementSlots) held(role string) bool {
	_, present := s.slotFor(role)

	return present
}

// modelDeploymentReplacementSlotsOf returns the recorded slots. A missing or unreadable record is
// no slots, which is how a deployment that never held one behaves.
func modelDeploymentReplacementSlotsOf(md *workercore.ModelDeployment) modelDeploymentReplacementSlots {
	raw, present := md.Annotations[modelDeploymentReplacementSlotsAnnotation]
	if !present {
		return nil
	}

	slots := modelDeploymentReplacementSlots{}
	if err := json.Unmarshal([]byte(raw), &slots); err != nil {
		return nil
	}

	return slots
}

// setSlot returns the slots with one role's slot written, leaving every other role's alone.
func (s modelDeploymentReplacementSlots) setSlot(
	slot modelDeploymentReplacementSlot,
) modelDeploymentReplacementSlots {
	next := make(modelDeploymentReplacementSlots, len(s)+1)
	for role, held := range s {
		next[role] = held
	}
	next[slot.Role] = slot

	return next
}

// clearRole returns the slots with one role's slot removed, or nil once none remain.
func (s modelDeploymentReplacementSlots) clearRole(role string) modelDeploymentReplacementSlots {
	next := make(modelDeploymentReplacementSlots, len(s))
	for name, held := range s {
		if name != role {
			next[name] = held
		}
	}
	if len(next) == 0 {
		return nil
	}

	return next
}

// replacementDescription names one slot in words.
func replacementDescription(slot modelDeploymentReplacementSlot) string {
	return fmt.Sprintf("role %s replica %d", slot.Role, slot.Ordinal)
}

// replacementCapturesMember reports whether a Pod is one the slot captured.
func replacementCapturesMember(slot modelDeploymentReplacementSlot, uid types.UID) bool {
	return slices.Contains(slot.MemberUIDs, uid)
}

// replacementCapturesWorkload reports whether a Workload is the one the slot captured. A Workload
// the slot never saw carries no authority, whatever its name reads.
func replacementCapturesWorkload(slot modelDeploymentReplacementSlot, wl *kueue.Workload) bool {
	return slot.WorkloadUID != "" && slot.WorkloadUID == wl.UID
}

// loadReplacementSlots reads the current record from the API server rather than the cache, so
// another role's slot written by a concurrent pass survives this pass's write.
func (r *ModelDeploymentReconciler) loadReplacementSlots(
	ctx context.Context, md *workercore.ModelDeployment,
) (*workercore.ModelDeployment, modelDeploymentReplacementSlots, error) {
	current := new(workercore.ModelDeployment)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(md), current); err != nil {
		return nil, nil, fmt.Errorf("read the deployment's replacement slots: %w", err)
	}

	// The record names a role of this deployment, so a delete and recreate between the read and
	// the write would leave it describing replicas of an object that no longer exists.
	if current.UID != md.UID {
		return nil, nil, fmt.Errorf(
			"the deployment was replaced while this pass ran: read UID %s, expected %s",
			current.UID, md.UID)
	}

	return current, modelDeploymentReplacementSlotsOf(current), nil
}

// writeReplacementSlots persists the slots, or writes nothing when they already say what this
// pass reached.
//
// The base is the server's own record and the patch carries the resourceVersion it was read at,
// so two passes writing different roles cannot overwrite one another and a record that moved
// underneath is a Conflict rather than a silent loss. The caller's object carries the new record
// and version forward, so a second write in the same pass is checked against the server.
func (r *ModelDeploymentReconciler) writeReplacementSlots(
	ctx context.Context, current *workercore.ModelDeployment,
	slots modelDeploymentReplacementSlots, carried *workercore.ModelDeployment,
) error {
	previous := modelDeploymentReplacementSlotsOf(current)
	if kubemeta.DeepEqual(previous, slots) {
		return nil
	}

	base := current.DeepCopy()
	candidate := base.DeepCopy()
	if candidate.Annotations == nil {
		candidate.Annotations = map[string]string{}
	}
	if len(slots) == 0 {
		// The key is removed rather than emptied: a merge patch that merely omits it leaves
		// whatever was there, and a finished record must not survive naming a replacement.
		delete(candidate.Annotations, modelDeploymentReplacementSlotsAnnotation)
	} else {
		raw, err := json.Marshal(slots)
		if err != nil {
			return fmt.Errorf("encode the replacement slots: %w", err)
		}
		candidate.Annotations[modelDeploymentReplacementSlotsAnnotation] = string(raw)
	}
	encoded := candidate.Annotations[modelDeploymentReplacementSlotsAnnotation]

	optimistic := ctrlcli.MergeFromWithOptions(base, ctrlcli.MergeFromWithOptimisticLock{})
	if err := r.Client.Patch(ctx, candidate, optimistic); err != nil {
		if kerrors.IsConflict(err) {
			return fmt.Errorf("the replacement slots were written against a stale object: %w", err)
		}

		return fmt.Errorf("write the replacement slots: %w", err)
	}

	if len(slots) == 0 {
		delete(carried.Annotations, modelDeploymentReplacementSlotsAnnotation)
	} else {
		if carried.Annotations == nil {
			carried.Annotations = map[string]string{}
		}
		carried.Annotations[modelDeploymentReplacementSlotsAnnotation] = encoded
	}
	carried.ResourceVersion = candidate.ResourceVersion

	return nil
}

// beginReplacementSlot records a role's replacement before anything is deleted. The captured
// identities are the only record of what the departing replica was; once its members are gone an
// empty ordinal names no replica to recover.
func (r *ModelDeploymentReconciler) beginReplacementSlot(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
) error {
	current, observed, err := r.loadReplacementSlots(ctx, md)
	if err != nil {
		return err
	}
	if held, present := observed.slotFor(slot.Role); present && held.Ordinal == slot.Ordinal {
		return nil
	}

	return r.writeReplacementSlots(ctx, current, observed.setSlot(slot), md)
}

// advanceReplacementSlot persists a change to a slot this pass did not create.
func (r *ModelDeploymentReconciler) advanceReplacementSlot(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
) error {
	current, observed, err := r.loadReplacementSlots(ctx, md)
	if err != nil {
		return err
	}

	return r.writeReplacementSlots(ctx, current, observed.setSlot(slot), md)
}

// endReplacementSlot removes a slot that has finished.
func (r *ModelDeploymentReconciler) endReplacementSlot(
	ctx context.Context, md *workercore.ModelDeployment, role string,
) error {
	current, observed, err := r.loadReplacementSlots(ctx, md)
	if err != nil {
		return err
	}
	if !observed.held(role) {
		return nil
	}

	return r.writeReplacementSlots(ctx, current, observed.clearRole(role), md)
}

// replacementCapturedIdentities builds the slot a selection pass writes: the members it is about
// to delete and the Workload that owns them, captured from the objects as they are now.
func replacementCapturedIdentities(
	view modelDeploymentReplicaView, workloads []kueue.Workload,
) modelDeploymentReplacementSlot {
	memberUIDs := make([]types.UID, 0, len(view.Members))
	for _, member := range view.Members {
		memberUIDs = append(memberUIDs, member.UID)
	}
	slices.Sort(memberUIDs)

	slot := modelDeploymentReplacementSlot{
		Role:       view.Role,
		Ordinal:    view.Ordinal,
		Phase:      modelDeploymentReplacementCleanup,
		MemberUIDs: memberUIDs,
	}
	if wl := replicaOwningWorkload(view.Members, workloadPointers(workloads)); wl != nil {
		slot.WorkloadUID = wl.UID
	}

	return slot
}

// workloadPointers views a listed Workload list as the pointers the ownership helpers take.
func workloadPointers(workloads []kueue.Workload) []*kueue.Workload {
	pointers := make([]*kueue.Workload, 0, len(workloads))
	for i := range workloads {
		pointers = append(pointers, &workloads[i])
	}

	return pointers
}

// replicaOwningWorkload returns the Workload claiming any of a replica's members. A Workload that
// merely carries the group's name is not this replica's.
func replicaOwningWorkload(members []*core.Pod, workloads []*kueue.Workload) *kueue.Workload {
	pods := make([]core.Pod, 0, len(members))
	for _, member := range members {
		pods = append(pods, *member)
	}

	byMember := modelDeploymentReplicaWorkloads(pods, workloads)
	for _, member := range members {
		if wl := byMember[member.UID]; wl != nil {
			return wl
		}
	}

	return nil
}

// replacementMemberDeletes returns the captured members still standing. The caller deletes each
// under its captured UID, so a Pod that took the name after the capture is not removed.
func replacementMemberDeletes(
	slot modelDeploymentReplacementSlot, members []*core.Pod,
) []*core.Pod {
	deletes := make([]*core.Pod, 0, len(slot.MemberUIDs))
	for _, member := range members {
		if replacementCapturesMember(slot, member.UID) {
			deletes = append(deletes, member)
		}
	}
	slices.SortFunc(deletes, func(a, b *core.Pod) int { return strings.Compare(a.Name, b.Name) })

	return deletes
}

// replacementWorkloadDeletes returns the Workloads the slot may delete: the one it captured, and
// any uncaptured Workload owning nothing this operator still counts as live, which is the one
// holding Kueue's finalizer on the departing group. A Workload claiming a member outside the
// slot's set is another replica's, and a matching name is not a claim.
func replacementWorkloadDeletes(
	slot modelDeploymentReplacementSlot, workloads []*kueue.Workload, live sets.Set[types.UID],
) []*kueue.Workload {
	var deletes []*kueue.Workload
	for _, wl := range workloads {
		if replacementCapturesWorkload(slot, wl) {
			deletes = append(deletes, wl)

			continue
		}
		if modelDeploymentWorkloadOwnsAny(wl, live) {
			continue
		}
		if modelDeploymentWorkloadOwnsAny(wl, replacementSlotMemberSet(slot)) {
			continue
		}
		if wl.DeletionTimestamp != nil {
			continue
		}
		deletes = append(deletes, wl)
	}

	return deletes
}

// replacementSlotMemberSet is the member UID set a slot captured.
func replacementSlotMemberSet(slot modelDeploymentReplacementSlot) sets.Set[types.UID] {
	return sets.New[types.UID](slot.MemberUIDs...)
}

// workloadOwnsGroupName reports whether a Workload stands on a group's derived name. It locates
// an object; it never authorizes deleting or adopting one.
func workloadOwnsGroupName(wl *kueue.Workload, group string) bool {
	if wl.Name == group {
		return true
	}
	for _, ps := range wl.Spec.PodSets {
		if ps.Name == kueue.PodSetReference(group) {
			return true
		}
	}

	return false
}

// replacementOccupancyOnServer lists the API server's members of a role at one ordinal,
// terminating ones included: a departing member still on the server holds the slot.
func (r *ModelDeploymentReconciler) replacementOccupancyOnServer(
	ctx context.Context, md *workercore.ModelDeployment, role string, ordinal int,
) ([]core.Pod, error) {
	podList := new(core.PodList)
	if err := r.APIReader.List(ctx, podList,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance:  md.Name,
			modelDeploymentLabelKeyComponent: role,
		}); err != nil {
		return nil, fmt.Errorf("list the members of role %q on the api server: %w", role, err)
	}

	held := make([]core.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !modelDeploymentOwns(pod, md) || modelDeploymentPodRole(pod) != role {
			continue
		}
		if pod.Labels[modelDeploymentReplicaOrdinalLabel] == strconvx.Itoa(ordinal) {
			held = append(held, *pod)
		}
	}

	return held, nil
}

// modelDeploymentPartialGroupMembers reports which members a replacement slot's group is still
// missing, and whether filling them is this operator's to do.
//
// A PARTIAL CREATE LEAVES THE REPLICA'S GROUP SHORT OF ITS TOTAL, and Kueue composes no Workload for
// an incomplete group, so a slot waiting on one waits forever. Filling it is safe only under three
// conditions, and each of them is checked rather than assumed:
//
//   - the group is declared at the size this pass renders it, so the member to add agrees with the
//     members already there about what the group is;
//   - every member standing carries this pass's own render, so the group is the configuration the
//     spec states now rather than a survivor of an earlier one;
//   - the group holds no Workload, so nothing has been composed against it and nothing admitted is
//     being extended.
//
// A group failing any of them is not filled here. A replica that lost a member under an earlier
// render waits behind the slot and is replaced whole, because adding a member from the current spec
// beside one from the previous spec is a group whose members disagree about what they are.
//
// The occupancy and the Workloads are read on the API server, because the state this question lives
// in is exactly the window the cache lags in: the members were created moments ago.
func (r *ModelDeploymentReconciler) modelDeploymentPartialGroupMembers(
	ctx context.Context, md *workercore.ModelDeployment,
	slot modelDeploymentReplacementSlot, want []*core.Pod,
) ([]*core.Pod, bool, error) {
	members, err := r.replacementOccupancyOnServer(ctx, md, slot.Role, slot.Ordinal)
	if err != nil {
		return nil, false, err
	}

	standing := replacementPodPointers(members)
	for _, member := range standing {
		if member.DeletionTimestamp != nil {
			return nil, false, nil
		}
	}

	// Nothing standing is not a partial create: an empty ordinal is the create gate's own case, and
	// it waits for the vacancy to be proved before anything is built there.
	if len(standing) == 0 || len(standing) >= len(want) {
		return nil, false, nil
	}

	// Every member standing must be seated on a seat this render has, and must carry this render's
	// own fingerprint for it. The group being short is expected here; the group being made of
	// something else is not.
	seated := make(map[int]string, len(standing))
	for _, member := range standing {
		seat := modelDeploymentPodMemberIndex(member)
		if seat >= len(want) {
			return nil, false, nil
		}
		if _, duplicated := seated[seat]; duplicated {
			return nil, false, nil
		}
		seated[seat] = member.Annotations[modelDeploymentPodSpecHashAnnotation]
	}
	for index, rendered := range want {
		if carried, present := seated[index]; present &&
			carried != rendered.Annotations[modelDeploymentPodSpecHashAnnotation] {
			return nil, false, nil
		}
	}

	total, totalReason := modelDeploymentReplicaDeclaredTotal(standing)
	if totalReason != "" || total != len(want) {
		return nil, false, nil
	}

	workloads, err := r.findModelDeploymentGroupWorkloads(ctx, md, members)
	if err != nil {
		return nil, false, err
	}
	if len(workloads) > 0 {
		return nil, false, nil
	}

	missing := make([]*core.Pod, 0, len(want)-len(standing))
	for index, rendered := range want {
		if _, present := seated[index]; !present {
			missing = append(missing, rendered)
		}
	}

	return missing, len(missing) > 0, nil
}

// modelDeploymentLiveRoleSelectors reads the selector each role's Service is published with, so a
// deployment whose eligibility was enabled keeps routing by it on the passes that did not decide it.
//
// The read is uncached. A selector published by the previous pass is the fact being asked about, and
// the informer lags the writer on exactly the pass that published it.
func (r *ModelDeploymentReconciler) modelDeploymentLiveRoleSelectors(
	ctx context.Context, md *workercore.ModelDeployment,
) (map[string]map[string]string, error) {
	selectors := make(map[string]map[string]string, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		svc := new(core.Service)
		err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{
			Namespace: md.Namespace, Name: md.Name + "-" + role.Name,
		}, svc)
		if err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("read the service of role %q: %w", role.Name, err)
		}
		selectors[role.Name] = maps.Clone(svc.Spec.Selector)
		if svc.Annotations[modelDeploymentEligibilityActiveAnnotation] == modelDeploymentEndpointEligibleValue {
			selectors[role.Name][modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		}
	}

	return selectors, nil
}

// replacementVacant reports whether the slot's ordinal is provably free on the server.
//
// The reads are uncached because the informer lags the writer in exactly this window: a departing
// member the cache still lists, a create whose response was lost, an old Workload already gone.
// A lost response is not absence.
func (r *ModelDeploymentReconciler) replacementVacant(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
) (bool, string) {
	occupied, err := r.replacementOccupancyOnServer(ctx, md, slot.Role, slot.Ordinal)
	if err != nil {
		return false, err.Error()
	}
	if len(occupied) > 0 {
		names := make([]string, 0, len(occupied))
		for i := range occupied {
			names = append(names, occupied[i].Name)
		}
		slices.Sort(names)

		return false, fmt.Sprintf("the server still holds %s", strings.Join(names, ", "))
	}

	// An occupied name holds the slot even by a Workload this operator cannot prove it composed.
	// Such a Workload is never deleted and never adopted, but a replacement composed beside it
	// would collide, so the wait stands until it goes.
	blocking, err := r.replacementGroupWorkload(ctx, md, slot)
	if err != nil {
		return false, err.Error()
	}
	if blocking != nil {
		if !blocking.DeletionTimestamp.IsZero() {
			return false, fmt.Sprintf("workload %s is still terminating", blocking.Name)
		}

		return false, fmt.Sprintf("workload %s still occupies the group", blocking.Name)
	}

	return true, ""
}

// replacementGroupWorkload returns the Workload still standing on a slot's derived group name,
// and nil when the group holds none.
func (r *ModelDeploymentReconciler) replacementGroupWorkload(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
) (*kueue.Workload, error) {
	group := modelDeploymentReplicaGroupName(md, slot.Role, slot.Ordinal)
	wlList := new(kueue.WorkloadList)
	if err := r.APIReader.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	for i := range wlList.Items {
		if workloadOwnsGroupName(&wlList.Items[i], group) {
			return &wlList.Items[i], nil
		}
	}

	return nil, nil
}

// replacementAdmitted reports whether the slot's ordinal now holds a complete current group whose
// Workload is admitted. Quota reservation is not admission: a reserved group is a request Kueue
// agreed to consider, and releasing the role on it would release it while the replacement holds
// nothing.
func (r *ModelDeploymentReconciler) replacementAdmitted(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
	want []*core.Pod,
) (bool, string) {
	members, err := r.replacementOccupancyOnServer(ctx, md, slot.Role, slot.Ordinal)
	if err != nil {
		return false, err.Error()
	}

	standing := make([]*core.Pod, 0, len(members))
	for i := range members {
		if members[i].DeletionTimestamp != nil {
			return false, fmt.Sprintf("member %s is on its way out", members[i].Name)
		}
		standing = append(standing, &members[i])
	}

	if reason := replacementMembersMatch(want, standing); reason != "" {
		return false, reason
	}

	wl, err := r.replacementGroupWorkload(ctx, md, slot)
	if err != nil {
		return false, err.Error()
	}
	if wl == nil {
		return false, "the group holds no workload yet"
	}

	if reason := replacementAdmissionHolds(wl, standing, want); reason != "" {
		return false, reason
	}

	return true, ""
}

// replacementAdmissionHolds reports whether an admitted Workload is fresh admission for the group
// its current members make, and why it is not when it is not.
//
// Ownership alone is not enough: a recreated Pod takes the same name and the same group, so a
// Workload composed for the departing replica can own the members that replaced it. An admission is
// fresh when its scheduling inputs describe the group standing now -- same members, count,
// template, reservation and queue.
func replacementAdmissionHolds(wl *kueue.Workload, standing, want []*core.Pod) string {
	if !kubeapistatus.ConditionType(kueue.WorkloadAdmitted).IsTrue(wl) {
		return fmt.Sprintf("workload %s is not admitted yet", wl.Name)
	}

	ours := sets.New[types.UID]()
	for _, member := range standing {
		ours.Insert(member.UID)
	}
	if !modelDeploymentWorkloadOwnsAny(wl, ours) {
		return fmt.Sprintf("workload %s owns none of the replica's current members", wl.Name)
	}
	for _, member := range standing {
		if !modelDeploymentWorkloadOwnsAny(wl, sets.New(member.UID)) {
			return fmt.Sprintf("workload %s does not own member %s", wl.Name, member.Name)
		}
	}

	if reason := replacementCompositionHolds(wl, standing); reason != "" {
		return reason
	}

	// The queue is compared unconditionally: a Workload naming none has not proved it submitted
	// where its members were sent.
	if queue := replacementQueueName(want); queue != "" && string(wl.Spec.QueueName) != queue {
		return fmt.Sprintf("workload %s is queued on %q rather than %q", wl.Name, wl.Spec.QueueName, queue)
	}

	return ""
}

// replacementCompositionHolds compares a Workload's scheduling inputs against the composition its
// own current members produce, and names the first difference.
//
// The composition is rebuilt from the members standing now rather than copied from the Workload:
// grouped by role hash as Kueue groups them, each count read from the members carrying it.
func replacementCompositionHolds(wl *kueue.Workload, members []*core.Pod) string {
	pinned := replacementPinnedPodSets(members)
	// Members sharing a role hash compose one podset carrying that count, so the number of
	// podsets is the number of distinct hashes among them, not the number of members.
	if len(wl.Spec.PodSets) != len(pinned) {
		return fmt.Sprintf("workload %s describes %d podsets for members composing %d",
			wl.Name, len(wl.Spec.PodSets), len(pinned))
	}
	live := make(map[kueue.PodSetReference]kueue.PodSet, len(wl.Spec.PodSets))
	for _, ps := range wl.Spec.PodSets {
		live[ps.Name] = ps
	}
	grouped := map[kueue.PodSetReference][]*core.Pod{}
	for _, member := range members {
		hash, err := replacementRoleHash(member)
		if err != nil {
			continue
		}
		grouped[hash] = append(grouped[hash], member)
	}

	for _, want := range pinned {
		got, present := live[want.Name]
		if !present {
			return fmt.Sprintf("workload %s carries no podset %s", wl.Name, want.Name)
		}
		if got.Count != want.Count {
			return fmt.Sprintf("workload %s asks for %d pods in podset %s, not the %d its members compose to",
				wl.Name, got.Count, want.Name, want.Count)
		}
		if reason := replacementTemplateMatches(wl, got, grouped[want.Name]); reason != "" {
			return reason
		}
	}

	return ""
}

// replacementTemplateMatches reports whether a Workload's PodSet describes one of a group's members.
//
// Any member matches, not only the one Kueue listed first: its List carries no order and rank-specific
// members share a role hash, so a template composed from m1 is correct even when the reader sees m0
// first.
func replacementTemplateMatches(
	wl *kueue.Workload, got kueue.PodSet, members []*core.Pod,
) string {
	for _, member := range members {
		template := core.PodTemplateSpec{Spec: *member.Spec.DeepCopy()}
		if replacementPodShapesMatch(got.Template.Spec, template.Spec) {
			// Kueue states no separate request on a PodSet: what it reserves on is the request it
			// derives from the template's containers.
			if resourceQuantityListEqual(
				replacementPodRequests(&core.Pod{Spec: got.Template.Spec}),
				replacementPodRequests(&core.Pod{Spec: template.Spec})) {
				return ""
			}

			return fmt.Sprintf("workload %s reserves resources in podset %s that its members do not declare",
				wl.Name, got.Name)
		}
	}

	return fmt.Sprintf("workload %s describes a pod in podset %s that its members do not match",
		wl.Name, got.Name)
}

// replacementPinnedPodSets rebuilds the PodSets a set of members composes to, following Kueue's
// own rule: grouped by the role hash read verbatim, each count taken from the members carrying it.
func replacementPinnedPodSets(members []*core.Pod) []kueue.PodSet {
	order := make([]kueue.PodSetReference, 0, len(members))
	grouped := make(map[kueue.PodSetReference][]*core.Pod, len(members))
	for _, member := range members {
		hash, err := replacementRoleHash(member)
		if err != nil {
			continue
		}
		if _, present := grouped[hash]; !present {
			order = append(order, hash)
		}
		grouped[hash] = append(grouped[hash], member)
	}

	pinned := make([]kueue.PodSet, 0, len(order))
	for _, hash := range order {
		group := grouped[hash]
		pinned = append(pinned, replacementPodSetFor(hash, group))
	}

	return pinned
}

// replacementPodSetFor is the PodSet one role hash composes to: the count of the members carrying
// it, and the template of whichever of them Kueue listed first.
//
// Which member that is does not matter, and the pinned set keeps all of them because Kueue's List
// carries no order: rank-specific members share one role hash by design, and a template taken from
// whichever member happened to come first would read a valid placement as a stale one.
func replacementPodSetFor(hash kueue.PodSetReference, members []*core.Pod) kueue.PodSet {
	return kueue.PodSet{
		Name:     hash,
		Count:    int32(len(members)),
		Template: core.PodTemplateSpec{Spec: *members[0].Spec.DeepCopy()},
	}
}

// replacementRoleHash is the group name Kueue gives a PodSet: the annotation this operator writes,
// read verbatim when present, and otherwise the digest Kueue derives from the Pod's shape.
func replacementRoleHash(pod *core.Pod) (kueue.PodSetReference, error) {
	if hash, present := pod.Annotations[kueuepodconst.RoleHashAnnotation]; present && hash != "" {
		return kueue.NewPodSetReference(hash), nil
	}

	hash, err := utilpod.GenerateRoleHash(&pod.Spec)
	if err != nil {
		return "", err
	}

	return kueue.NewPodSetReference(hash), nil
}

// replacementPodRequests is what a member contributes to its PodSet's reservation.
//
// IT IS KUBERNETES' OWN HELPER rather than a rule written here, because the rule is not this
// operator's to state: requests win over limits, several containers sum, and init containers carry
// the pod maximum. A hand-rolled aggregation agreeing with it on one shape is not agreement, and
// the reservation being compared is the one Kueue will charge.
func replacementPodRequests(pod *core.Pod) core.ResourceList {
	return resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
}

// replacementPodShapesMatch compares the Pod specs Kueue reserved quota against, ignoring what
// admission is expected to have changed between the two.
func replacementPodShapesMatch(got, want core.PodSpec) bool {
	actual, member := replacementComparablePod(got), replacementComparablePod(want)
	actual.Affinity = replacementWorkloadAffinity(actual.Affinity, member.Affinity)

	return kubemeta.DeepEqual(actual, member)
}

// replacementWorkloadAffinity removes only Workload webhook pins absent from the member.
// Fit and model-manager pins constrain Kueue placement but are not copied to the Pod.
// Required terms and all other affinity must still describe the member's placement.
func replacementWorkloadAffinity(got, want *core.Affinity) *core.Affinity {
	if got == nil || got.NodeAffinity == nil || got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return got
	}
	var wanted []core.NodeSelectorTerm
	if want != nil && want.NodeAffinity != nil && want.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		wanted = want.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	}
	required := got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(required.NodeSelectorTerms) != max(1, len(wanted)) {
		return got
	}
	for i := range required.NodeSelectorTerms {
		term := &required.NodeSelectorTerms[i]
		var original []core.NodeSelectorRequirement
		if len(wanted) > 0 {
			original = wanted[i].MatchExpressions
		}
		before := len(term.MatchExpressions)
		term.MatchExpressions = slices.DeleteFunc(term.MatchExpressions, func(e core.NodeSelectorRequirement) bool {
			if !nodefeature.IsFitLabelKey(e.Key) && e.Key != ModelManagerRegisteredLabel {
				return false
			}
			return !slices.ContainsFunc(original, func(existing core.NodeSelectorRequirement) bool {
				return kubemeta.DeepEqual(existing, e)
			})
		})
		if len(wanted) == 0 && before > 0 && len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		}
	}
	if kubemeta.DeepEqual(got.NodeAffinity, &core.NodeAffinity{}) {
		got.NodeAffinity = nil
	}
	if kubemeta.DeepEqual(got, &core.Affinity{}) {
		return nil
	}

	return got
}

// replacementComparablePod reduces a Pod spec to what the operator rendered, which is what Kueue
// reserved on.
//
// The PodSet holds the pre-admission template, so the live Pod carries the placement the cluster made
// after Kueue read it: a node selector and tolerations the admission chain added, a scheduling gate
// it consumed, and the node the scheduler bound. Those are one configuration seen before and after,
// so they are compared out. Everything the operator renders is still compared, so a template
// describing a different configuration is still refused.
func replacementComparablePod(spec core.PodSpec) core.PodSpec {
	reduced := *spec.DeepCopy()
	reduced.Volumes = nil
	reduced.InitContainers = nil
	reduced.EphemeralContainers = nil
	reduced.RestartPolicy = ""
	reduced.TerminationGracePeriodSeconds = nil
	reduced.ActiveDeadlineSeconds = nil
	reduced.DNSPolicy = ""
	reduced.NodeSelector = nil
	reduced.Tolerations = nil
	reduced.SchedulingGates = nil
	reduced.NodeName = ""
	for i := range reduced.Containers {
		reduced.Containers[i].TerminationMessagePath = ""
		reduced.Containers[i].TerminationMessagePolicy = ""
		reduced.Containers[i].ImagePullPolicy = ""
		// Resources are compared on their own, since that is what Kueue reserves on.
		reduced.Containers[i].Resources = core.ResourceRequirements{}
	}

	return reduced
}

// resourceQuantityListEqual compares two resource lists by value rather than by representation.
func resourceQuantityListEqual(got, want core.ResourceList) bool {
	if len(got) != len(want) {
		return false
	}
	for name, quantity := range want {
		other, present := got[name]
		if !present {
			return false
		}
		if quantity.Cmp(other) != 0 {
			return false
		}
	}

	return true
}

// replacementMembersMatch reports whether a standing group is the render this pass produced, and
// names the first difference. The rendered fingerprint covers the template and the request
// together, so a replica rebuilt from any other configuration does not match.
func replacementMembersMatch(want, standing []*core.Pod) string {
	if len(standing) != len(want) {
		return fmt.Sprintf("the replica holds %d of the %d members it is rendered at",
			len(standing), len(want))
	}

	seats := make(map[int]*core.Pod, len(standing))
	for _, member := range standing {
		seat := modelDeploymentPodMemberIndex(member)
		if _, duplicated := seats[seat]; duplicated {
			return fmt.Sprintf("two members sit at index %d", seat)
		}
		seats[seat] = member
	}

	for index, rendered := range want {
		member, seated := seats[index]
		if !seated {
			return fmt.Sprintf("no member sits at index %d", index)
		}
		if member.Annotations[modelDeploymentPodSpecHashAnnotation] !=
			rendered.Annotations[modelDeploymentPodSpecHashAnnotation] {
			return fmt.Sprintf("member %s does not carry the render this pass produced", member.Name)
		}
	}

	return ""
}

// replacementQueueName is the LocalQueue a rendered replica submits to, read off the render so
// this check and the label Kueue admits the group by cannot disagree.
func replacementQueueName(want []*core.Pod) string {
	for _, rendered := range want {
		if entrance := rendered.Labels[kueuectrlconst.QueueLabel]; entrance != "" {
			return entrance
		}
	}

	return ""
}

// replacementRenderHashes are the fingerprints a render produced for one replica, in member order.
func replacementRenderHashes(want []*core.Pod) []string {
	hashes := make([]string, 0, len(want))
	for _, rendered := range want {
		hashes = append(hashes, rendered.Annotations[modelDeploymentPodSpecHashAnnotation])
	}

	return hashes
}

// replacementSuperseded reports whether the slot's replacement was rendered from a configuration
// the spec has since replaced.
//
// The comparison is on rendered fingerprints, not on the generation: a replica-count edit moves
// the generation without moving any member's render, and treating that as a configuration change
// would tear down a healthy replacement and rebuild it identically.
func replacementSuperseded(slot modelDeploymentReplacementSlot, want []*core.Pod) bool {
	if len(slot.MemberHashes) == 0 {
		// The slot has not recorded its target render yet, so nothing is known to be obsolete.
		return false
	}
	if len(slot.MemberHashes) != len(want) {
		return true
	}

	return !slices.Equal(slot.MemberHashes, replacementRenderHashes(want))
}

// modelDeploymentReplacementProgress is what one pass did about a role's active slot.
type modelDeploymentReplacementProgress struct {
	// slot is the record to carry into the rest of the pass.
	slot modelDeploymentReplacementSlot

	// cleared is that the replacement holds an admission and the role no longer holds a slot.
	cleared bool

	// requeue is that nothing else will wake this deployment before the slot can move.
	requeue bool
}

// resolveReplacementSlot advances one role's active slot by one step.
//
// The order of the questions is the lifecycle: an ordinal the spec no longer declares is finished
// by cleanup alone; then the captured members are removed; then vacancy is proved on the server,
// because a lost delete response is not absence; then a superseded replacement is rebuilt in this
// same slot; then an admitted replacement releases the role.
func (r *ModelDeploymentReconciler) resolveReplacementSlot(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
	role *workercore.ModelDeploymentRole, want []*core.Pod,
) (modelDeploymentReplacementProgress, error) {
	logger := ctrllog.FromContext(ctx)
	progress := modelDeploymentReplacementProgress{slot: slot}

	// A REPLICA COUNT THAT NO LONGER DECLARES THIS ORDINAL CANCELS THE SLOT, and the cancellation
	// runs through the same cleanup: the members and the Workload the slot captured still have to
	// leave before the record can be dropped, or the finalizer and the reservation outlive it.
	if slot.Ordinal >= int(role.Replicas) {
		members, err := r.replacementOccupancyOnServer(ctx, md, slot.Role, slot.Ordinal)
		if err != nil {
			return progress, err
		}
		if err = r.cleanReplacementSlot(ctx, md, slot, replacementPodPointers(members), members); err != nil {
			return progress, err
		}
		if vacant, why := r.replacementVacant(ctx, md, slot); !vacant {
			progress.requeue = true
			logger.V(3).Info("waiting for a cancelled replacement to leave",
				"slot", replacementDescription(slot), "because", why)

			return progress, nil
		}
		progress.cleared = true

		return progress, nil
	}

	// Whatever the slot captured is removed first, on its own authority. Members that arrived
	// afterwards belong to a replacement this slot is waiting on and are left standing.
	captured, err := r.replacementOccupancyOnServer(ctx, md, slot.Role, slot.Ordinal)
	if err != nil {
		return progress, err
	}
	// A CAPTURED MEMBER ALREADY ON ITS WAY OUT IS LEFT TO ITS FINALIZER. Re-issuing the delete
	// would say nothing and would keep this pass reporting that it issued a cleanup it had not,
	// which is the one state the vacancy question below is asked from.
	mine := replacementStandingMemberDeletes(slot, replacementPodPointers(captured))
	if slot.Phase == modelDeploymentReplacementCleanup {
		if err = r.cleanReplacementSlot(ctx, md, slot, mine, captured); err != nil {
			return progress, err
		}
		if len(mine) > 0 {
			progress.requeue = true
			return progress, nil
		}
	}

	standing := replacementUncapturedMembers(slot, captured)
	wl, wlErr := r.replacementGroupWorkload(ctx, md, slot)
	if wlErr != nil {
		return progress, wlErr
	}
	if slot.Phase == modelDeploymentReplacementWaitingForAdmission {
		present := sets.New[types.UID]()
		broken := false
		for _, member := range standing {
			present.Insert(member.UID)
			broken = broken || member.DeletionTimestamp != nil
		}
		for _, uid := range slot.CurrentMemberUIDs {
			broken = broken || !present.Has(uid)
		}
		// A composed group cannot be filled beside its existing reservation.
		ownsCurrent := wl != nil && (wl.UID == slot.CurrentWorkloadUID ||
			modelDeploymentWorkloadOwnsAny(wl, present.Union(sets.New(slot.CurrentMemberUIDs...))))
		if ownsCurrent && len(standing) < len(want) {
			broken = true
		}
		if broken || replacementSuperseded(slot, want) {
			next := modelDeploymentReplacementSlot{
				Role: slot.Role, Ordinal: slot.Ordinal, Phase: modelDeploymentReplacementCleanup,
				MemberUIDs:  sets.List(present.Union(sets.New(slot.CurrentMemberUIDs...))),
				WorkloadUID: slot.CurrentWorkloadUID,
			}
			if ownsCurrent {
				next.WorkloadUID = wl.UID
			}
			progress.slot = next
			if err = r.advanceReplacementSlot(ctx, md, next); err != nil {
				return progress, err
			}
			if err = r.cleanReplacementSlot(ctx, md, next, standing, captured); err != nil {
				return progress, err
			}
			progress.requeue = true
			return progress, nil
		}
		// Persist identities before admission or cleanup can remove their observable evidence.
		slot.CurrentMemberUIDs = sets.List(present)
		if ownsCurrent {
			slot.CurrentWorkloadUID = wl.UID
		}
		if err = r.advanceReplacementSlot(ctx, md, slot); err != nil {
			return progress, err
		}
		progress.slot = slot
	}

	// The replacement exists once a member the slot did not capture is standing. From there the
	// slot waits on admission rather than on vacancy, and the two questions are different: vacancy
	// is about the old group leaving, admission about the new one being granted quota.
	if len(standing) == 0 {
		vacant, why := r.replacementVacant(ctx, md, slot)
		if !vacant {
			// Something still holds the ordinal. That is the ordinary waiting state -- a member
			// draining behind its finalizer, a Workload not yet gone -- and it ends through the Pod
			// and Workload events rather than through a timer.
			logger.V(3).Info("waiting for the replaced group to leave",
				"slot", replacementDescription(slot), "because", why)

			return progress, nil
		}

		// The target render is recorded once the old group has left, which is what a later edit is
		// compared against.
		if slot.Phase == modelDeploymentReplacementCleanup || len(slot.MemberHashes) == 0 {
			progress.slot = modelDeploymentReplacementSlot{
				Role:         slot.Role,
				Ordinal:      slot.Ordinal,
				Phase:        modelDeploymentReplacementWaitingForAdmission,
				MemberHashes: replacementRenderHashes(want),
			}
			if err = r.advanceReplacementSlot(ctx, md, progress.slot); err != nil {
				return progress, err
			}
		}

		return progress, nil
	}

	if admitted, reason := r.replacementAdmitted(ctx, md, slot, want); admitted {
		progress.cleared = true

		return progress, nil
	} else {
		logger.V(3).Info("waiting for the replacement to be admitted",
			"slot", replacementDescription(slot), "because", reason)
	}

	return progress, nil
}

// replacementStandingMemberDeletes returns the captured members still standing, skipping those
// already terminating. A member past its delete is on its way out whatever this pass does.
func replacementStandingMemberDeletes(
	slot modelDeploymentReplacementSlot, members []*core.Pod,
) []*core.Pod {
	deletes := make([]*core.Pod, 0, len(members))
	for _, member := range replacementMemberDeletes(slot, members) {
		if member.DeletionTimestamp == nil {
			deletes = append(deletes, member)
		}
	}

	return deletes
}

// replacementUncapturedMembers returns the members on a slot's ordinal that it did not capture:
// the replacement, wherever an earlier pass managed to create it.
func replacementUncapturedMembers(
	slot modelDeploymentReplacementSlot, live []core.Pod,
) []*core.Pod {
	pointers := replacementPodPointers(live)
	uncaptured := make([]*core.Pod, 0, len(pointers))
	for _, member := range pointers {
		if !replacementCapturesMember(slot, member.UID) {
			uncaptured = append(uncaptured, member)
		}
	}
	slices.SortFunc(uncaptured, func(a, b *core.Pod) int { return strings.Compare(a.Name, b.Name) })

	return uncaptured
}

// replacementPodPointers views a list of Pod values as the pointers the cleanup helpers take.
func replacementPodPointers(pods []core.Pod) []*core.Pod {
	pointers := make([]*core.Pod, 0, len(pods))
	for i := range pods {
		pointers = append(pointers, &pods[i])
	}

	return pointers
}

// cleanReplacementSlot removes what a slot captured: its members under their own UIDs, then the
// Workload holding them.
//
// A member and its Workload are deleted only under captured UIDs. The group name locates
// vacancy blockers and never authorizes cleanup, so a Workload this operator
// cannot prove it composed is left standing and the vacancy proof waits on it.
func (r *ModelDeploymentReconciler) cleanReplacementSlot(
	ctx context.Context, md *workercore.ModelDeployment, slot modelDeploymentReplacementSlot,
	members []*core.Pod, live []core.Pod,
) error {
	for _, member := range replacementMemberDeletes(slot, members) {
		uid := member.UID
		if err := r.Client.Delete(ctx, member, ctrlcli.Preconditions{UID: &uid}); err != nil &&
			!kerrors.IsNotFound(err) {
			return fmt.Errorf("delete replaced member %s: %w", member.Name, err)
		}
	}

	// A SLOT WHOSE MEMBERS ARE ALL GONE STILL OWES ITS WORKLOAD A DELETE. That Workload is what
	// releases Kueue's finalizer on the Pods and gives the reservation back, and reading it as
	// already done because the members left is what strands a group holding quota forever.
	standing := sets.New[types.UID]()
	for i := range live {
		if live[i].DeletionTimestamp == nil {
			standing.Insert(live[i].UID)
		}
	}

	listed := new(kueue.WorkloadList)
	if err := r.APIReader.List(ctx, listed, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return fmt.Errorf("list replaced workloads: %w", err)
	}
	workloads := make([]*kueue.Workload, 0, 1)
	for i := range listed.Items {
		wl := &listed.Items[i]
		if replacementCapturesWorkload(slot, wl) {
			workloads = append(workloads, wl)
		}
	}
	for _, wl := range replacementWorkloadDeletes(slot, workloads, standing) {
		uid := wl.UID
		if err := r.Client.Delete(ctx, wl, ctrlcli.Preconditions{UID: &uid}); err != nil &&
			!kerrors.IsNotFound(err) {
			return fmt.Errorf("delete replaced replica's workload %s: %w", wl.Name, err)
		}
	}

	return nil
}
