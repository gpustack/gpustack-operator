package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// ModelDeploymentPhaseStarting is reported while replicas are still coming up and none is ready.
	ModelDeploymentPhaseStarting = "Starting"
	// ModelDeploymentPhaseReady is reported when every role's ready count equals its desired count.
	ModelDeploymentPhaseReady = "Ready"
	// ModelDeploymentPhaseDegraded is reported when at least one replica is ready and at least one
	// is not. It is a distinct phase rather than a shade of Starting because it is the state a
	// deployment sits in when it is serving and under-provisioned at the same time.
	ModelDeploymentPhaseDegraded = "Degraded"
	// ModelDeploymentPhaseDeleting is reported while the replicas are being torn down.
	ModelDeploymentPhaseDeleting = "Deleting"
)

// The condition types a ModelDeployment reports, one per axis. They are independent: "quota
// reserved but cache not attached" is a real and actionable state, which is what a single phase
// string cannot carry.
//
// DomainRegistered is declared beside the rule that resolves the Binding, CacheAttached beside the
// reading that judges it, and ReplicasUpToDate beside the convergence that decides it, so that each
// condition's vocabulary sits with the code that can actually observe it.
const (
	ModelDeploymentConditionQuotaReserved kubeapistatus.ConditionType = "QuotaReserved"

	// ModelDeploymentConditionRoleKindsReady reports whether every role kind this deployment
	// declares has at least one ready replica.
	//
	// IT IS NOT REPLICA COMPLETENESS, which is the question it sits closest to and the one it will
	// be read as. "Every role has all the replicas it asked for" is what the phase already answers,
	// by summing every role's counts before judging — and a sum cannot express this one: a
	// deployment missing an entire kind and a deployment merely one replica short produce the same
	// sum, so the same phase.
	//
	// IT IS ALSO NOT AVAILABILITY, and the distance is larger than it looks. Whether a request can
	// be served depends on what the engine does with a role it was told is a producer, and on
	// whether a Service's endpoints reach the replicas that are ready. Neither is observable from
	// this object. So this reports readiness BY KIND and claims nothing past it, which is also what
	// keeps it correct whichever way those two questions are later answered.
	ModelDeploymentConditionRoleKindsReady kubeapistatus.ConditionType = "RoleKindsReady"
)

// The reasons RoleKindsReady carries. Three rather than two, because "no kind is missing" and "no
// role has been accounted for yet" are different answers that a False would merge into one.
const (
	modelDeploymentReasonAllKindsReady = "AllKindsReady"

	modelDeploymentReasonKindsNotReady = "KindsNotReady"

	// modelDeploymentReasonNoRoleStatuses is the Unknown case: the pass accounted for no role at
	// all, so there is nothing to judge. Reporting False here would say a kind is missing, which is
	// a claim this pass cannot make.
	modelDeploymentReasonNoRoleStatuses = "NoRoleStatuses"
)

// modelDeploymentReasonPodGroupIncomplete is QuotaReserved's reason for a group that is short of the
// total it declares.
//
// It exists because that state has NO Workload at all — Kueue declines to compose one for a group it
// has not fully seen — so it is indistinguishable, from the Workload's side, from admission that has
// simply not happened yet. The two need telling apart: one clears in a moment, the other is a
// deployment sitting with gated Pods and an empty `kubectl get workloads` until something creates
// the missing replica.
const modelDeploymentReasonPodGroupIncomplete = "PodGroupIncomplete"

// modelDeploymentReasonPreemptedInPart is QuotaReserved's reason for a deployment a higher-priority
// workload has taken part of.
//
// IT IS A SEPARATE REASON BECAUSE THE OPERATOR ACTION IS SEPARATE. Every other wait this condition
// reports resolves itself or names something in the deployment to fix; this one resolves only when
// capacity elsewhere frees up, and until then the surviving groups hold accelerators for a
// deployment that is short of what it was admitted for, and, when the reclaimed groups were every
// group of some role kind, for one that cannot serve at all. Waiting is right for the others and is
// a decision here, because how long to wait
// depends on what preempted it — which is outside this object, and outside this operator.
//
// A REASON IS THE MACHINE-READABLE CLASSIFICATION AND A MESSAGE IS NOT A CONTRACT. An alert rule or a
// runbook that wants to tell "wait for it" from "go and look at what took the quota" has to branch on
// something stable, and substring-matching a sentence is not that.
const modelDeploymentReasonPreemptedInPart = "PreemptedInPart"

// syncModelDeploymentStatus rebuilds the status from what was observed this pass and writes it only
// if it differs from what is stored.
//
// It is REBUILT rather than patched, so a stale field cannot survive a disagreement with the Pods:
// a role that was Ready and is not any more reports the count that was just measured, not the one
// that was true when it last changed. Anything a later task adds must fold into this one function
// for the same reason — a second writer would be free to leave its own field behind.
// The domain is what THIS pass observed about the referenced Binding, or nil for a pass that did not
// look. Nil leaves the domain projection and its condition exactly as they were, because a teardown
// pass reporting "not observed" would read as the domain having gone away.
//
// THE ROLLOUT RECORD IS NIL ON THE SAME OCCASIONS AND ANSWERS THE OPPOSITE WAY, which is the one
// asymmetry here worth knowing before adding a caller. Nil, or a record that accounted for no
// replica, reports Unknown rather than leaving the stored condition alone. The difference is whose
// object it is: the Binding belongs to somebody else, so a pass that did not read it has no question
// to answer, while the replicas are this controller's own, so a pass that could account for none has
// an answer and that answer is "could not tell". Left alone instead, a steady deployment's True
// survives the very pass that deletes every replica.
func (r *ModelDeploymentReconciler) syncModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, domain *modelDeploymentDomain,
	rollout *modelDeploymentRollout,
) error {
	desired, err := r.computeModelDeploymentStatus(ctx, md, pods, domain, rollout)
	if err != nil {
		return err
	}

	// kubemeta.DeepEqual covers LastTransitionTime too, and that is correct rather than incidental:
	// the condition accessors move it only when the condition's value changes, so an unchanged pass
	// leaves it alone and compares equal.
	if kubemeta.DeepEqual(*desired, md.Status) {
		return nil
	}

	md.Status = *desired

	return r.Client.Status().Update(ctx, md)
}

// computeModelDeploymentStatus derives the whole status from the spec and the observed Pods.
func (r *ModelDeploymentReconciler) computeModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, domain *modelDeploymentDomain,
	rollout *modelDeploymentRollout,
) (*workercore.ModelDeploymentStatus, error) {
	// The condition accessors mutate the object they are given, so they work on a copy of the
	// observed status: conditions carry a LastTransitionTime that must not be reset on every pass,
	// and only the accessors know when it should move.
	holder := &workercore.ModelDeployment{Status: *md.Status.DeepCopy()}

	// EVERY Workload of the deployment, resolved once: a deployment whose roles sit on several
	// instanceTypes is several pod groups and one Workload each, and both the per-role flavor and the
	// group-level quota answer come from this same read. Reading it twice would let the two halves of
	// the status describe two moments.
	wls, err := r.findModelDeploymentGroupWorkloads(ctx, md, pods)
	if err != nil {
		return nil, err
	}
	// EACH GROUP'S OWN WORKLOAD, RESOLVED ONCE. There is no such thing as "the deployment's Workload"
	// once its roles sit on several instanceTypes, and taking whichever sorts first answers for one
	// group while misreporting every other.
	groupOfRole := modelDeploymentGroupOfRole(md)
	wlByGroup := modelDeploymentWorkloadByGroup(md, pods, wls, groupOfRole)

	holder.Status.Roles = modelDeploymentRoleStatuses(md, pods, wlByGroup, groupOfRole)
	holder.Status.Endpoint = modelDeploymentEndpoint(md)

	observeModelDeploymentDomain(holder, domain)

	observeModelDeploymentQuota(md, pods, wls, wlByGroup, groupOfRole, holder)

	r.observeModelDeploymentCache(ctx, md, pods, domain, holder)

	observeModelDeploymentRollout(holder, rollout)

	observeModelDeploymentRoleKinds(holder)

	deriveModelDeploymentPhase(md, holder)

	return &holder.Status, nil
}

// modelDeploymentRoleStatuses counts, per role, how many replicas the spec asks for and how many of
// them are Ready.
//
// A role's replicas are identified by the Pod's resource note rather than by parsing its name,
// because a name is a rendering and a note is what the renderer recorded.
func modelDeploymentRoleStatuses(
	md *workercore.ModelDeployment, pods []core.Pod,
	wlByGroup map[string]*kueue.Workload, groupOfRole map[string]string,
) []workercore.ModelDeploymentRoleStatus {
	ready := make(map[string]int32, len(md.Spec.Roles))
	for i := range pods {
		if pods[i].DeletionTimestamp != nil || !podIsReady(&pods[i]) {
			continue
		}
		ready[modelDeploymentPodRole(&pods[i])]++
	}

	statuses := make([]workercore.ModelDeploymentRoleStatus, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		statuses = append(statuses, workercore.ModelDeploymentRoleStatus{
			Name: role.Name,
			// The kind is ECHOED so an operator reading status alone sees which role is which. It is
			// resolved rather than copied: this field is required and enumerated, so writing an
			// unset spec kind through would be refused, and the API server refuses the status write
			// rather than the one field.
			Kind:    ModelDeploymentEffectiveRoleKind(role),
			Desired: role.Replicas,
			Ready:   ready[role.Name],
			// A role that replaced the whole command line got no synthesized argument and no client
			// environment, so nothing here can claim it is attached to the cache.
			Unmanaged: role.Template != nil && len(role.Template.Command) > 0,
			// THE ROLE'S OWN GROUP'S WORKLOAD, not the deployment's first. A flavor is an answer Kueue
			// gave to one pod group, so a role is told what happened to ITS group or nothing at all.
			// Reading whichever Workload sorts first reports a flavor this role was never assigned for
			// every group but one, which is worse than the nil that means "no answer yet".
			AssignedFlavor: modelDeploymentAssignedFlavor(wlByGroup[groupOfRole[role.Name]], role),
		})
	}

	return statuses
}

// modelDeploymentAssignedFlavor is the accelerator model Kueue actually gave this role, or nil.
//
// NIL RATHER THAN EMPTY, and the distinction is the whole reason the field is a pointer: "not
// assigned yet" and "assigned to a flavor whose name is empty" are different facts, and a zero value
// collapses them into one that reads as an assignment.
//
// The flavor is read through the same function the per-accelerator check reads it with, so the
// answer status reports and the answer the gate fits against cannot diverge. That function returns
// empty for a Workload with no admission, a PodSet it does not name, and an assignment that is
// ambiguous — all three of which are "no answer", which is exactly what nil says.
func modelDeploymentAssignedFlavor(
	wl *kueue.Workload, role *workercore.ModelDeploymentRole,
) *string {
	if wl == nil {
		return nil
	}

	flavor := assignedFlavor(wl, kueue.PodSetReference(role.Name))
	if flavor == "" {
		return nil
	}

	return ptr.To(string(flavor))
}

// modelDeploymentPodRole reads which role a replica belongs to.
func modelDeploymentPodRole(pod *core.Pod) string {
	return pod.Labels[modelDeploymentLabelKeyComponent]
}

// observeModelDeploymentQuota reports whether EVERY ONE of the deployment's Workloads holds quota.
//
// Roles on one instanceType are one pod group and one Workload; roles on several are one of each per
// group. So `True` is an answer about the whole set rather than about whichever Workload sorts first,
// and the branches below are what make it one: half a deployment holding quota is not the deployment
// holding quota, and reporting the first Workload's answer for the rest was a defect a cluster found.
//
// It reads those Workloads' OWN conditions rather than asking the admission gate. The gate stops
// evaluating a Workload once it is admitted, so anything derived from the gate would answer for the
// moment of admission and never again — and a Workload that has been preempted since would still
// read as reserved.
//
// THE ORDER OF THE BRANCHES IS THE POINT, and PodGroupIncomplete has to come before "no Workload
// yet". A group short of its declared total has NO Workload by construction — Kueue refuses to
// compose one and says so unretryably on the Pods — so the two states look identical from here and
// mean opposite things: one resolves itself in a moment, the other is the deployment sitting with
// gated Pods and an empty `kubectl get workloads` forever. Reporting the second as the first is the
// failure this reason exists to name.
func observeModelDeploymentQuota(
	md *workercore.ModelDeployment, pods []core.Pod, wls []*kueue.Workload,
	wlByGroup map[string]*kueue.Workload, groupOfRole map[string]string,
	holder *workercore.ModelDeployment,
) {
	// A PARKED DEPLOYMENT IS REPORTED FIRST, because every other answer below would describe it
	// wrongly. Its groups are complete and their Workloads deactivated, which the vocabulary here
	// otherwise reads as "waiting for admission" -- the opposite of the truth once the bound has
	// fired, and the reading that sends an operator to wait for something that is never coming.
	//
	// IT IS OBSERVED HERE RATHER THAN WRITTEN BY THE CONTROLLER THAT PARKED IT. Status is rebuilt from
	// observed state on every pass by this one function, so a second writer would leave its own field
	// behind the moment this one disagreed. The bound is measured on the Workload; this reads what
	// that measurement left there.
	if parked := parkedModelDeploymentWorkloads(wls); len(parked) > 0 {
		ModelDeploymentConditionQuotaReserved.False(holder, "Parked", fmt.Sprintf(
			"the deployment's groups could not all be placed, so %d of its workloads are deactivated "+
				"and no longer ask for quota: %s. An identical re-apply does not clear this — free the "+
				"capacity the deployment needs and reactivate them, or delete the deployment and "+
				"create it again",
			len(parked), strings.Join(parked, ", ")))

		return
	}

	// A DEPLOYMENT PREEMPTED IN PART IS REPORTED BEFORE ANY OTHER WAIT, and it is a different fact
	// from every one of them: part of it is still admitted and serving, holding accelerators for a
	// deployment that is no longer whole, while the rest waits for quota that was taken.
	//
	// EVERY OTHER BRANCH DESCRIBES IT WRONGLY, and one of them says something FALSE. The multi-group
	// wait below ends with "no role is admitted until the whole set can run"; here a role IS admitted,
	// and it is the reason the state matters. The completeness branch would report the preempted
	// group as merely short of its replicas, which sends an operator to look for a scheduling problem
	// rather than at the higher-priority workload that took the quota.
	//
	// THE REASON CARRIES THE CLASSIFICATION RATHER THAN THE MESSAGE. Telling this from an ordinary
	// wait decides what an operator does -- wait, or go and look at what preempted it -- and a
	// consumer that has to substring-match a message to make that decision is reading something that
	// is not a contract.
	//
	// THE WORKLOAD SURVIVES PREEMPTION, WHICH IS WHAT MAKES THIS READABLE AT ALL, and it is read out of
	// Kueue rather than assumed: its preemption path calls workload.Evict with the Preempted reason
	// and a prepare step that sets the Preempted condition, and Evict patches status -- nothing on
	// that path deletes the object. Both conditions are therefore present to be read.
	//
	// WHAT IS STILL ASSUMED IS WHAT HAPPENS TO THE REPLICAS, and this branch is placed before the
	// completeness check so that the answer cannot change what gets reported. If a preempted serving
	// group's Pods are removed, this is what keeps that group from being announced as merely short of
	// its total; if they stay, the ordering is inert. Both ways the report is right, and the
	// assumption decides only whether this ordering is load-bearing.
	lost, kept := modelDeploymentPreemptedInPart(md, wlByGroup)
	// WHAT ELSE IS WRONG STILL GETS REPORTED, and the preemption is carried down with it. A group
	// preempted while no sibling survived is not the state PreemptedInPart names -- nothing is being
	// held -- but "something took a group's quota" is a fact that would otherwise be dropped, and
	// without it the answer sends the reader to investigate the wrong thing.
	//
	// EVERY ANSWER THAT CAN BE GIVEN WHILE A GROUP OF THIS DEPLOYMENT IS PREEMPTED CARRIES IT, and the
	// ones that do not are listed here with the reason, because "deliberately not carried" and
	// "forgotten" read the same in code:
	//
	//   - NoReplicas cannot be reached with a preemption observed. The Workload of a group is resolved
	//     through that group's replicas, so a deployment with no Pods resolves no Workload and there is
	//     nothing on which a preemption could have been seen.
	//   - NoQueueInReservedNamespace is the same in a different way: a reserved namespace carries no
	//     LocalQueue, so a deployment there is never scheduled, never holds quota, and has none to lose.
	//   - Parked supersedes it rather than missing it. Those Workloads are deactivated and no longer ask
	//     for quota at all, so who took some of it earlier does not decide anything the reader does
	//     next. It is also answered before this is computed, which is why it reads as an omission.
	//
	// AllReplicasTerminating DOES carry it, and it is the one that looks unreachable and is not: a
	// group's replicas are resolved to a Workload whether or not they are terminating, so a preempted
	// group whose Pods are on their way out lands exactly there.
	taken := modelDeploymentPreemptionNote(lost)
	if len(lost) > 0 && len(kept) > 0 {
		// WHAT THE SURVIVORS ARE WORTH DEPENDS ON THE SHAPE, so it is computed rather than asserted.
		// A deployment whose every group of some role kind was reclaimed has lost that half and
		// serves nothing; one that still has a group of every kind serves, with the reclaimed
		// groups' capacity gone. Both reach this branch, and announcing the first for both was wrong
		// for the shape the defaults produce: two roles naming no kind are two server roles, and one
		// of their groups surviving is a deployment that is still answering requests.
		// THE SENTENCE MUST NOT IMPLY THE KIND WAS EVER ADMITTED. A kind counts as unserved when no
		// group of it is admitted, and a group that never was -- one still short of its declared
		// total, so Kueue has composed nothing for it -- reaches this branch too, because the
		// preemption of a sibling kind's group is answered before incompleteness is.
		effect := "the deployment still serves, without the capacity those groups provided"
		if unserved := modelDeploymentKindsWithoutAdmittedGroup(md, wlByGroup); len(unserved) > 0 {
			effect = fmt.Sprintf(
				"no group of role kind %s is admitted, so the deployment cannot serve",
				strings.Join(unserved, ", "))
		}

		ModelDeploymentConditionQuotaReserved.False(holder, modelDeploymentReasonPreemptedInPart,
			fmt.Sprintf(
				"a higher-priority workload reclaimed the quota of %d of this deployment's %d groups, "+
					"on instance types %s. The groups on %s are still admitted and hold their "+
					"accelerators, and %s. They are released only by deleting the deployment or by "+
					"the reclaimed groups being admitted again once the capacity returns",
				len(lost), len(modelDeploymentPodGroups(md)), strings.Join(lost, ", "),
				strings.Join(kept, ", "), effect))

		return
	}

	if len(pods) == 0 {
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "NoReplicas",
			"no replica has been created yet")

		return
	}

	var live int
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			live++
		}
	}

	// EVERY REPLICA TERMINATING IS NOT "ALL RESERVED". The early return above guards an empty LIST;
	// this guards an empty RESULT, and they are different emptinesses. A recreate rollout or a group
	// rebuild hands this function a non-empty slice whose every member is skipped, and reporting on
	// what is left would claim a guarantee over a set it just finished emptying.
	//
	// Unknown rather than False: no replica holds quota, but none is being refused any either.
	if live == 0 {
		// THE PREEMPTION IS CARRIED HERE TOO, and this is the branch where it matters most: replicas
		// on their way out is what a reclaimed group looks like from the Pod side, so "all of them are
		// terminating" without the reason why sends the reader to look for who deleted them.
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AllReplicasTerminating", fmt.Sprintf(
			"all %d replicas are terminating, so none holds quota to report on%s", len(pods), taken))

		return
	}

	// THE COUNT IS PER GROUP, and so is the queue it names. Kueue composes one Workload per group and
	// withholds it until THAT group has its own declared total, so a deployment whose roles sit on
	// different instanceTypes can have one group complete and another short. Comparing the
	// deployment-wide sum answers about neither: it reads a complete group as short whenever a
	// sibling is, and reports a queue that is not the one holding anything back.
	//
	// The ClusterQueue is named after the InstanceType, so the queue a refusal points at is read off
	// the spec rather than resolved: the LocalQueue the entrance label names is derived from it.
	// A replica is attributed to a group through its ROLE rather than through the membership label it
	// carries. That is the same source every other figure on this status reads, so a Pod cannot be
	// counted in one place and not another -- and a replica that predates the label, or one still
	// being built, is counted against the group its role puts it in rather than against none.
	groups := modelDeploymentPodGroups(md)
	for _, group := range groups {
		var alive int
		for i := range pods {
			if pods[i].DeletionTimestamp == nil &&
				groupOfRole[modelDeploymentPodRole(&pods[i])] == group.Name {
				alive++
			}
		}

		if want := int(group.TotalCount); alive < want {
			ModelDeploymentConditionQuotaReserved.False(holder, modelDeploymentReasonPodGroupIncomplete,
				fmt.Sprintf(
					"%d of %d of the group's replicas exist, so Kueue composes no workload for it at "+
						"all and there is nothing in cluster queue %q to hold quota%s",
					alive, want, group.InstanceType, taken))

			return
		}
	}

	// Every group is complete, so what is left to report is whether they hold quota.
	//
	// THE ANSWER IS OVER EVERY GROUP, NOT OVER THE FIRST WORKLOAD. A deployment spread over several
	// instanceTypes has one Workload per group, and reporting whichever sorts first says the
	// deployment holds quota while half of it does not. Measured on a cluster: one group reserved,
	// its sibling's queue was held, and this condition read Reserved with a message naming the
	// deployment's whole replica count -- a reading that is wrong in both halves at once.
	// A SINGLE-GROUP DEPLOYMENT HAS ONE QUEUE AND ITS WORDING SAYS SO; a multi-group one has no single
	// queue to name, and naming the first role's is a statement about one group offered as a statement
	// about the deployment. So this is read only where the branch has already established there is one
	// group, and the multi-group branches name the instance types they are actually talking about.
	queue := md.Spec.Roles[0].InstanceType

	if withoutWorkload := modelDeploymentGroupsWithoutWorkload(groups, wlByGroup); len(withoutWorkload) > 0 {
		if kuberess.IsReservedNamespace(md.Namespace) {
			ModelDeploymentConditionQuotaReserved.False(holder, "NoQueueInReservedNamespace", fmt.Sprintf(
				"the deployment is in reserved namespace %q and will never be scheduled", md.Namespace))

			return
		}
		if len(groups) == 1 {
			ModelDeploymentConditionQuotaReserved.Unknown(holder, "AdmissionInFlight", fmt.Sprintf(
				"the group is complete at %d replicas but has no workload yet in cluster queue %q%s",
				live, queue, taken))

			return
		}
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AdmissionInFlight", fmt.Sprintf(
			"%d of this deployment's %d groups are complete but have no workload yet, on instance "+
				"types %s%s",
			len(withoutWorkload), len(groups), strings.Join(withoutWorkload, ", "), taken))

		return
	}

	waiting := modelDeploymentGroupsWithoutQuota(groups, wlByGroup)
	if len(waiting) == 0 {
		// The single-group wording is kept verbatim, because for one group it is exactly right and it
		// is what an operator reading this condition today already recognizes.
		if len(groups) == 1 {
			ModelDeploymentConditionQuotaReserved.True(holder, "Reserved", fmt.Sprintf(
				"the group of %d replicas has quota reserved in cluster queue %q", live, queue))

			return
		}
		ModelDeploymentConditionQuotaReserved.True(holder, "Reserved", fmt.Sprintf(
			"all %d of this deployment's groups have quota reserved", len(groups)))

		return
	}

	if len(groups) == 1 {
		ModelDeploymentConditionQuotaReserved.False(holder, "Pending", fmt.Sprintf(
			"the group of %d replicas is waiting for quota in cluster queue %q%s", live, queue, taken))

		return
	}
	ModelDeploymentConditionQuotaReserved.False(holder, "Pending", fmt.Sprintf(
		"%d of this deployment's %d groups are waiting for quota, on instance types %s. No role is "+
			"admitted until the whole set can run%s",
		len(waiting), len(groups), strings.Join(waiting, ", "), taken))
}

// modelDeploymentGroupOfRole names, for each role, the pod group its replicas belong to.
//
// A replica is attributed to a group through its ROLE rather than through the membership label it
// carries, so that a replica predating the label, or one still being built, is counted against the
// group its role puts it in rather than against none.
func modelDeploymentGroupOfRole(md *workercore.ModelDeployment) map[string]string {
	groupOfRole := make(map[string]string, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		groupOfRole[role.Name] = modelDeploymentPodGroupFor(md, role.InstanceType).Name
	}

	return groupOfRole
}

// modelDeploymentWorkloadByGroup resolves the Workload Kueue composed for each of this deployment's
// pod groups, keyed by group name, and omits a group that has none yet.
//
// THERE IS NO SUCH THING AS "THE DEPLOYMENT'S WORKLOAD" once its roles sit on several instanceTypes.
// Every question a caller has -- which flavor this role got, whether this group holds quota, whether
// a group has been composed at all -- is a question about ONE group, and the Workload that answers it
// is the one owning that group's replicas. Taking whichever sorts first answers correctly for one
// group and misreports every other, which is a shape this package has now had twice.
//
// The Workloads arrive in name order, so a group with two candidates during a rebuild resolves to the
// same one on every pass rather than flipping while the old one drains.
func modelDeploymentWorkloadByGroup(
	md *workercore.ModelDeployment,
	pods []core.Pod,
	wls []*kueue.Workload,
	groupOfRole map[string]string,
) map[string]*kueue.Workload {
	members := make(map[string]sets.Set[types.UID], len(md.Spec.Roles))
	for i := range pods {
		group := groupOfRole[modelDeploymentPodRole(&pods[i])]
		if group == "" {
			continue
		}
		if members[group] == nil {
			members[group] = sets.New[types.UID]()
		}
		members[group].Insert(pods[i].UID)
	}

	byGroup := make(map[string]*kueue.Workload, len(members))
	for group, own := range members {
		for _, w := range wls {
			if modelDeploymentWorkloadOwnsAny(w, own) {
				byGroup[group] = w

				break
			}
		}
	}

	return byGroup
}

// modelDeploymentPreemptedInPart names the instance types of the groups a higher-priority workload
// reclaimed, and those of the groups that are still admitted, when BOTH sets are non-empty.
//
// THE SECOND SET IS THE WHOLE PREDICATE. A deployment every one of whose groups was preempted is an
// ordinary state: it holds nothing, serves nothing, and is waiting for capacity exactly as a
// deployment that never started is. What makes this one different is that part of it kept its quota
// and is running — so accelerators are held for a deployment that is no longer whole, and neither
// half of that is visible from the other half alone. Whether the rest still serves is a question
// about the KINDS the reclaimed groups carried, answered separately and never assumed. Reading only "was anything preempted" answers the
// same for both, which is the shape this is written against.
//
// A GROUP WITH NO WORKLOAD COUNTS AS NEITHER, and that asymmetry is deliberate. Kueue composes no
// Workload for a group short of its declared total, so such a group holds nothing and there is
// nothing on it to read: it cannot be a survivor holding accelerators, and it cannot be shown to have
// been preempted. Two consequences follow, and they are not the same.
//
// WITH A SURVIVOR PRESENT THE PREEMPTION WINS THE REPORT, even though another group is still
// assembling. Something took capacity this deployment was admitted for, which is the one fact an
// operator can act on, and the assembling group resolves itself.
//
// WITH NO SURVIVOR THERE IS NOTHING TO HOLD, so this is not the state this reason names and the
// report falls through to the branch that describes what else is wrong. That branch used to say
// nothing about the preemption at all -- it sent the reader to investigate missing replicas when
// something had taken a group's quota -- so the fact is carried down instead of the reason being
// widened to cover a state whose harm it does not describe.
//
// PREEMPTION IS ASKED OF KUEUE RATHER THAN INFERRED. Kueue names it: an evicted Workload carries a
// reason, and Preempted is one value among PodsReadyTimeout, AdmissionCheck and the queue-stopped
// ones. Inferring it from "lost its reservation" would fold every one of those into this answer, and
// they need different actions.
func modelDeploymentPreemptedInPart(
	md *workercore.ModelDeployment, wlByGroup map[string]*kueue.Workload,
) (lost, kept []string) {
	for _, group := range modelDeploymentPodGroups(md) {
		wl := wlByGroup[group.Name]
		if wl == nil {
			continue
		}
		switch {
		case modelDeploymentWorkloadPreempted(wl):
			lost = append(lost, group.InstanceType)
		case kubeapistatus.ConditionType(kueue.WorkloadAdmitted).IsTrue(wl):
			kept = append(kept, group.InstanceType)
		}
	}

	return lost, kept
}

// modelDeploymentWorkloadPreempted reports whether Kueue took this Workload's quota back for a
// higher-priority one.
//
// BOTH CONDITIONS ARE READ because they are written at different moments and either can be the one
// still standing: Kueue sets Preempted when it decides, and Evicted with the Preempted reason when it
// acts. A reader of one alone answers differently depending on which write it arrives between.
func modelDeploymentWorkloadPreempted(wl *kueue.Workload) bool {
	if kubeapistatus.ConditionType(kueue.WorkloadPreempted).IsTrue(wl) {
		return true
	}

	for i := range wl.Status.Conditions {
		c := &wl.Status.Conditions[i]
		if c.Type == kueue.WorkloadEvicted && c.Status == meta.ConditionTrue &&
			c.Reason == kueue.WorkloadEvictedByPreemption {
			return true
		}
	}

	return false
}

// modelDeploymentKindsWithoutAdmittedGroup names the role kinds no admitted group carries, in the
// order the roles first mention them.
//
// IT IS WHAT DECIDES WHETHER A PARTIALLY PREEMPTED DEPLOYMENT IS STILL SERVING, and it has to be
// asked about KINDS rather than about groups. Groups are keyed by instanceType, so one kind can have
// several: losing one prefill group out of two leaves the deployment able to prefill, while losing
// the only decode group does not. Counting groups answers neither question.
//
// A ROLE WHOSE GROUP HAS NO WORKLOAD COUNTS AS NOT ADMITTED. Kueue composes none for a group short
// of its declared total, and a group that is not admitted is not serving whatever the reason. That
// is the accurate reading here, unlike in the preemption predicate next door, where the same state
// means "nothing to report about this group" rather than "this group is gone".
func modelDeploymentKindsWithoutAdmittedGroup(
	md *workercore.ModelDeployment, wlByGroup map[string]*kueue.Workload,
) []string {
	groupOfRole := modelDeploymentGroupOfRole(md)

	order := make([]string, 0, len(md.Spec.Roles))
	served := make(map[string]bool, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		kind := string(ModelDeploymentEffectiveRoleKind(role))
		if _, seen := served[kind]; !seen {
			order = append(order, kind)
			served[kind] = false
		}

		wl := wlByGroup[groupOfRole[role.Name]]
		if wl != nil && kubeapistatus.ConditionType(kueue.WorkloadAdmitted).IsTrue(wl) {
			served[kind] = true
		}
	}

	var unserved []string
	for _, kind := range order {
		if !served[kind] {
			unserved = append(unserved, kind)
		}
	}

	return unserved
}

// modelDeploymentGroupsWithoutWorkload names the instance types of the groups Kueue has composed no
// Workload for. A complete group in that state is mid-admission; an incomplete one is reported by the
// branch above this one, which is why the two are not the same answer.
func modelDeploymentGroupsWithoutWorkload(
	groups []modelDeploymentPodGroupSpec, wlByGroup map[string]*kueue.Workload,
) []string {
	var missing []string
	for _, group := range groups {
		if wlByGroup[group.Name] == nil {
			missing = append(missing, group.InstanceType)
		}
	}

	return missing
}

// modelDeploymentGroupsWithoutQuota names the instance types of the groups that hold no quota
// reservation.
//
// A group is answered by the Workload owning ITS replicas. Reading one Workload for the whole
// deployment cannot distinguish a set that is fully reserved from one where a sibling is held, and
// those two states are the difference between a deployment that is about to run and one that never
// will.
func modelDeploymentGroupsWithoutQuota(
	groups []modelDeploymentPodGroupSpec, wlByGroup map[string]*kueue.Workload,
) []string {
	var waiting []string
	for _, group := range groups {
		w := wlByGroup[group.Name]
		if w == nil || !kubeapistatus.ConditionType(kueue.WorkloadQuotaReserved).IsTrue(w) {
			waiting = append(waiting, group.InstanceType)
		}
	}

	return waiting
}

// findModelDeploymentGroupWorkloads returns EVERY Workload owning any of these Pods, in name order.
//
// THERE IS NO SINGULAR FORM OF THIS, deliberately. One that returned the first was here and had no
// production caller left: every question is about one group, and the group is chosen by the caller
// through modelDeploymentWorkloadByGroup rather than by sort order. A correctly written accessor that
// only serves a world model this package has abandoned is not dead weight, it is an invitation to
// reintroduce the defect it enables -- and it draws no review comment, because it is not wrong.
//
// IT MATCHES ON A PLAIN OWNER REFERENCE, NOT A CONTROLLER REFERENCE, and that is not a relaxation —
// it is the difference between finding the Workload and never finding one. Kueue sets a CONTROLLER
// reference when it builds a Workload from a single Pod, and plain owner references to EVERY member
// when it builds one from a pod group (`SetOwnerReference` per Pod, not `SetControllerReference`).
// So the moment the replicas became a group, a controller-reference filter started matching nothing,
// and every deployment would have reported "no workload yet" forever while being admitted normally.
//
// The API version is checked with the kind: "Pod" is not a reserved word, and a resource of that
// kind in another group would match on the kind alone.
//
// A rebuild can briefly leave the OLD group's Workload owning Pods that are on their way out. The
// scan is over the Pods this pass observed, and the result is taken in name order so that two
// candidates cannot make the reported state flip between passes while the old one drains. A caller
// that takes only the first leaves the rest in place -- and each of those still holds Kueue's
// finalizer on its own replicas, which is the one thing that keeps them from ever leaving. The
// symptom is a deployment stuck in Deleting with nothing erroring.
//
// Callers that scope the Pods scope the result: passing one group's replicas returns that group's
// Workload and no other, which is how a rebuild reaches exactly the group it is rebuilding.
func (r *ModelDeploymentReconciler) findModelDeploymentGroupWorkloads(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) ([]*kueue.Workload, error) {
	if len(pods) == 0 {
		return nil, nil
	}

	ours := sets.New[types.UID]()
	for i := range pods {
		ours.Insert(pods[i].UID)
	}

	wlList := new(kueue.WorkloadList)
	err := r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}

	var found []*kueue.Workload
	for i := range wlList.Items {
		wl := &wlList.Items[i]
		if modelDeploymentWorkloadOwnsAny(wl, ours) {
			found = append(found, wl)
		}
	}
	slices.SortFunc(found, func(a, b *kueue.Workload) int { return strings.Compare(a.Name, b.Name) })

	return found, nil
}

// modelDeploymentWorkloadOwnsAny reports whether the Workload names any of these Pods as an owner.
func modelDeploymentWorkloadOwnsAny(wl *kueue.Workload, pods sets.Set[types.UID]) bool {
	for _, ref := range wl.OwnerReferences {
		if ref.Kind == "Pod" && ref.APIVersion == "v1" && pods.Has(ref.UID) {
			return true
		}
	}

	return false
}

// observeModelDeploymentRoleKinds reports whether every role kind has a ready replica.
//
// IT READS THE ROLE STATUSES THIS PASS JUST COMPUTED rather than the spec, so its answer cannot
// disagree with the counts published beside it. Reading the spec instead would let the condition
// speak about a role whose replicas this pass failed to account for.
func observeModelDeploymentRoleKinds(holder *workercore.ModelDeployment) {
	roles := holder.Status.Roles
	if len(roles) == 0 {
		ModelDeploymentConditionRoleKindsReady.Unknown(holder, modelDeploymentReasonNoRoleStatuses,
			"no role has been accounted for yet")

		return
	}

	missing := modelDeploymentKindsWithoutReady(roles)
	if len(missing) == 0 {
		ModelDeploymentConditionRoleKindsReady.True(holder, modelDeploymentReasonAllKindsReady,
			"every role kind of this deployment has at least one ready replica")

		return
	}

	ModelDeploymentConditionRoleKindsReady.False(holder, modelDeploymentReasonKindsNotReady,
		fmt.Sprintf("no replica is ready for role kinds %s", strings.Join(missing, ", ")))
}

// modelDeploymentKindsWithoutReady names the role kinds no ready replica was counted for, in the
// order the roles first mention them.
//
// It is PURE: same statuses, same result, no client and no clock.
//
// THE THRESHOLD IS ONE READY REPLICA, NEVER ALL OF THEM. "All of them" is replica completeness,
// which the phase already derives; asking it here would put one question in two fields, and the two
// would be free to disagree the moment either is edited. One is what makes this a question about
// the deployment's SHAPE: whether each half of a disaggregated deployment is represented at all.
//
// THE ORDER IS THE ROLES' ORDER, first mention winning, so two passes over unchanged statuses
// produce the same message. A map's iteration order would not, and this list is rendered into a
// condition message that is compared against the stored one to decide whether to write at all.
//
// AN UNMANAGED ROLE'S READY REPLICAS COUNT, and that is a decision rather than an oversight. Such a
// role replaced the whole command line, so the renderer attached no probes to it and its Pods are
// Ready once their containers start — a weaker statement than a probed role's Ready. It still
// counts, because the operator did not build that command line and has nothing better to judge it
// by, and because discounting it would leave a deployment whose roles all took over reporting a
// missing kind forever. What it costs is that this condition's True rests, for such a kind, on the
// kubelet's container-start signal alone; status.roles[].Unmanaged is published per role, so a
// reader needing the stronger reading can apply it without this field folding it in.
func modelDeploymentKindsWithoutReady(roles []workercore.ModelDeploymentRoleStatus) []string {
	order := make([]string, 0, len(roles))
	ready := make(map[string]bool, len(roles))

	for i := range roles {
		kind := string(roles[i].Kind)
		if _, seen := ready[kind]; !seen {
			order = append(order, kind)
			ready[kind] = false
		}
		if roles[i].Ready > 0 {
			ready[kind] = true
		}
	}

	var missing []string
	for _, kind := range order {
		if !ready[kind] {
			missing = append(missing, kind)
		}
	}

	return missing
}

// deriveModelDeploymentPhase summarizes the counts into the one field a human reads first.
//
// Degraded means SOME replicas are ready and some are not, which is the state worth telling apart:
// the deployment is serving, at less than the capacity that was asked for. A deployment with nothing
// ready is Starting whatever the reason — to a reader, a replica that has not been admitted and one
// that has not finished loading its weights are the same state, and phaseMessage is what
// distinguishes them.
func deriveModelDeploymentPhase(md, holder *workercore.ModelDeployment) {
	if md.DeletionTimestamp != nil {
		holder.Status.Phase = ModelDeploymentPhaseDeleting
		holder.Status.PhaseMessage = "the replicas are being torn down"

		return
	}

	var desired, ready int32
	for _, rs := range holder.Status.Roles {
		desired += rs.Desired
		ready += rs.Ready
	}

	switch {
	case ready == desired:
		holder.Status.Phase = ModelDeploymentPhaseReady
		holder.Status.PhaseMessage = ""
	case ready > 0:
		holder.Status.Phase = ModelDeploymentPhaseDegraded
		holder.Status.PhaseMessage = fmt.Sprintf("%d of %d replicas are ready", ready, desired)
	default:
		holder.Status.Phase = ModelDeploymentPhaseStarting
		holder.Status.PhaseMessage = ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	}
}

// parkedModelDeploymentWorkloads names the deployment's Workloads the joint barrier has deactivated.
//
// spec.active is what the barrier writes when it gives up holding a set, and it is durable state the
// scheduler reads: while it is false the Workload is not considered and the group is not rebuilt
// behind it.
//
// BUT THAT FLAG ALONE DOES NOT SAY WHO WROTE IT. Kueue deactivates a Workload of its own accord, and
// so can an operator pausing one group by hand. Reporting either as "parked for infeasibility" tells
// the reader the deployment's set could not be placed and instructs them to free capacity or delete
// and recreate -- an answer to a question nobody asked, about a state somebody chose. So the barrier's
// OWN check has to be carrying its verdict on the same Workload for this to be the barrier's doing.
func parkedModelDeploymentWorkloads(wls []*kueue.Workload) []string {
	var parked []string
	for _, wl := range wls {
		if !kueueworkload.IsActive(wl) && jointBarrierParked(wl) {
			parked = append(parked, wl.Name)
		}
	}

	return parked
}

// jointBarrierParked reports whether the joint-admission check is the reason this Workload is
// inactive, by reading the verdict that controller left on it.
func jointBarrierParked(wl *kueue.Workload) bool {
	acs := jointCheckEntry(wl)

	return acs != nil && strings.Contains(acs.Message, _JointAdmissionParkedMarker)
}

// modelDeploymentPreemptionNote is the clause the other quota answers carry when a group of this
// deployment was preempted but the state is not the one PreemptedInPart names.
//
// IT IS A SUFFIX RATHER THAN A REASON OF ITS OWN, because it is not what the condition is about: the
// branch it lands on has already said what is wrong, and this adds who took a group's quota. Giving
// it a reason would mean two classifications of one state, and the one that resolves the reader's
// next action is the branch's own.
func modelDeploymentPreemptionNote(lost []string) string {
	if len(lost) == 0 {
		return ""
	}

	return fmt.Sprintf(
		". A higher-priority workload has also reclaimed the quota of the groups on instance types %s",
		strings.Join(lost, ", "))
}
