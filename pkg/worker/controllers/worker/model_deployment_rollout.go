package worker

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// ModelDeploymentConditionReplicasUpToDate reports whether the running replicas match what the
// convergence renders for them now.
//
// IT IS NOT A STATEMENT ABOUT THE OBJECT'S SPEC ALONE, and defining it that way would make it
// unreadable for consumers. The hash it compares covers the KV cache connector synthesized from the
// Binding as well, so a replica still carrying the current spec differs from the render as soon as
// that connector stops resolving -- which is the very state this condition exists to report.
//
// It exists because the rollout has one state that every other condition describes correctly while
// saying nothing about it. A replica whose hash differs from the desired one is left in place when
// the connection cannot be resolved, because recreating it would rebuild every replica of every
// deployment on a store that merely blinked -- and that guard delays an unrelated edit for as long
// as the store is away.
//
// What the object said while it delayed one was accurate and about the wrong subject: the only false
// condition named a reuse domain whose figures could not be read, and there is no path from that
// sentence to "the edit you are watching for is queued behind it". This condition is that path, and
// it is the whole remedy for the diagnosability half of the cost -- the delayed change is not lost,
// so nothing here needs to recover it.
const ModelDeploymentConditionReplicasUpToDate kubeapistatus.ConditionType = "ReplicasUpToDate"

const (
	modelDeploymentReasonUpToDate           = "UpToDate"
	modelDeploymentReasonRolloutInProgress  = "RolloutInProgress"
	modelDeploymentReasonRolloutHeldByCache = "RolloutHeldByCache"

	// modelDeploymentReasonReplacementInProgress is the state of a pass whose every surviving
	// replica matches the render while the declared count is short: a replica left on its own --
	// a node drained, Kueue reclaiming quota, the kubelet evicting, or a hand deleting it, which
	// this pass cannot tell apart -- and the pass is replacing it.
	//
	// IT IS A SEPARATE REASON BECAUSE THE QUESTION A READER BRINGS IS THE OPPOSITE OF A ROLLOUT'S.
	// A rollout answers "did my edit land"; a replacement answers "why is capacity moving when I
	// changed nothing". Reporting the first for the second sends the operator to diff a spec that
	// did not change, and reporting nothing -- the answer this reason replaces, an UpToDate that
	// read exactly like the steady state -- hides the one window in which the deployment is
	// serving below the count it declared for a cause outside itself.
	modelDeploymentReasonReplacementInProgress = "ReplacementInProgress"

	// modelDeploymentReasonRolloutNotObserved is the answer of a pass that could account for no
	// replica. It is Unknown rather than an absent condition, because the alternative -- leaving the
	// stored value in place -- keeps an authoritative True through exactly the passes that delete
	// every replica.
	modelDeploymentReasonRolloutNotObserved = "RolloutNotObserved"
)

// modelDeploymentRollout is what one convergence pass decided about replicas carrying an earlier
// spec.
//
// RECORDED AS THE PASS DECIDES, not re-derived from the Pods afterwards. The desired hashes belong
// to the render this pass performed and are not kept, so reading the answer back would mean
// rendering a second time and comparing against that -- which answers a different question the one
// moment it matters, a spec edited again while the pass was running.
type modelDeploymentRollout struct {
	// accounted counts the replicas this pass can vouch for: it read the hash of an existing one, or
	// it created one from the render it just performed.
	//
	// It is what separates "nothing was outdated" from "nothing was looked at", and those are the
	// same zero everywhere else in this record. A pass reaches the status write able to vouch for
	// nothing more often than it looks: a teardown, and the pass between a rollout's delete and its
	// create, where the names are still held by terminating replicas so the creates do not land
	// either.
	//
	// A create counts because the Pod it just issued was rendered from this pass's own desired state,
	// which is a stronger claim than a hash comparison rather than a weaker one. Leaving it out would
	// make a deployment's first pass unable to answer and its second pass write a status for a spec
	// nobody changed, which this controller does not do.
	accounted int
	// outdated counts the existing replicas whose rendered spec hash differs from the desired one.
	outdated int
	// held counts how many of those were left in place because the KV cache connection could not be
	// resolved. It is never greater than outdated.
	held int
}

// observeModelDeploymentRollout folds one pass's rollout decision into the status.
//
// A record that can vouch for no replica reports Unknown. It does NOT leave the stored value alone,
// and the difference is the whole point: leaving it alone keeps whatever the last answering pass
// wrote, which after a steady deployment is an authoritative True. A teardown then deletes every
// replica without vouching for one, and the object goes on saying every replica matches the render
// while none exists.
//
// Two passes arrive here -- a teardown, and the pass between a rollout's delete and its create
// while the names are still taken -- and both are moments when the replicas are least current, so
// the stale answer is wrong in exactly the state it is read in. Unknown is the same shape
// CacheAttached already uses for a reading it could not take.
//
// THE PASS'S DECISION AND THE OBSERVED COUNT ANSWER TOGETHER, and the order they are asked in is
// the ranking of causes. A held rollout outranks everything, because nothing else can be acted on
// until the store returns. A rollout outranks a replacement, because when both are in flight the
// rollout is the umbrella cause -- it is why a replica is away, and its message is the one the
// reader who caused it needs. The replacement is what remains when every surviving replica is
// current and the count is still short, which no spec change explains.
func observeModelDeploymentRollout(
	holder, md *workercore.ModelDeployment, pods []core.Pod,
	wlByReplica map[types.UID]*kueue.Workload, rollout *modelDeploymentRollout,
) {
	// The test is on the record rather than at the call site, because "vouched for nothing" is a
	// property of what the pass found and every caller would otherwise have to remember it.
	if rollout == nil || rollout.accounted == 0 {
		ModelDeploymentConditionReplicasUpToDate.Unknown(holder, modelDeploymentReasonRolloutNotObserved,
			"this pass accounted for no replica: it compared no hash and created none, so whether the "+
				"replicas match the current render was not established either way")

		return
	}

	missing, missingWhere := modelDeploymentReplicasMissing(md, pods, wlByReplica)

	switch {
	case rollout.held > 0:
		// IT DOES NOT SAY A CHANGE IS WAITING, because it cannot know. During an outage the desired
		// render carries no connector, so every attached replica's hash differs whether or not
		// anyone edited the deployment -- an ordinary store blink puts every deployment on the pool
		// here. Naming a change the user may not have made would be wrong far more often than right,
		// so the consequence is stated conditionally and the reader is pointed at the store.
		//
		// AND IT NAMES THE CLASS RATHER THAN "a spec edit", which is wider than this guard. What the
		// guard holds is a difference on a replica that EXISTS: the ordinals a scale-up adds are
		// created during the outage too, without a connector, because a replica that does not exist
		// yet cannot be given an address that does not exist yet either. What waits is an edit that
		// changes a running replica's rendered Pod -- and it waits whole, because there is no
		// group-shape edit left that could carry it past the guard.
		ModelDeploymentConditionReplicasUpToDate.False(holder, modelDeploymentReasonRolloutHeldByCache, fmt.Sprintf(
			"%d of %d replicas differ from what this pass rendered and were left in place: the KV "+
				"cache connection could not be resolved, and recreating them on that alone would "+
				"rebuild every replica whenever the store blinks. Until it resolves, an edit that "+
				"changes a running replica's rendered Pod waits with them -- withheld rather than "+
				"dropped, and it rolls out once the connection returns",
			rollout.held, rollout.outdated))
	case rollout.outdated > 0:
		// DELETED, NOT ALL OF THEM AT ONCE. The rollout turns over one replica per role per pass,
		// never beside a deficit and never beside a surplus this pass already shed, so a count of
		// differing replicas is not a count of deletions this pass issued -- a message that read it
		// that way claimed deletions that had not happened yet. What this pass did is delete at
		// most one per role; the pass that observes a deleted one gone creates its replacement, and
		// a reader told the replacements were already made would stop watching for them.
		ModelDeploymentConditionReplicasUpToDate.False(holder, modelDeploymentReasonRolloutInProgress, fmt.Sprintf(
			"%d replicas differ from what this pass rendered and turn over one per role per pass: "+
				"this pass deleted at most one replica per role, and the pass that finds it gone "+
				"creates the replacement", rollout.outdated))
	case missing > 0 && modelDeploymentRolloutWasInProgress(holder):
		// THE GAP A ROLLOUT OPENS IS STILL THE ROLLOUT'S. The pass that deletes the last outdated
		// replica leaves nothing outdated behind it and the replacement not yet created, so the pass
		// after it sees a shortfall and no difference -- which is indistinguishable, from the state
		// alone, from a replica that left on its own. Read as a departure it tells the operator that
		// nothing changed the spec, in the middle of the rollout they started, and sends them to
		// diff a spec against itself.
		//
		// THE PREVIOUS VERDICT IS THE ONLY CARRIER. A reconcile has no memory, and the replica whose
		// hash would have proved the provenance is precisely the one that is gone. It clears itself:
		// once the replacement exists the pass falls through to the branch below, which writes a
		// verdict that is not this one.
		ModelDeploymentConditionReplicasUpToDate.False(holder, modelDeploymentReasonRolloutInProgress, fmt.Sprintf(
			"%d of the declared replicas are being replaced by the rollout in flight: %s. The last "+
				"replica built from the earlier spec has gone, and the pass that finds it gone "+
				"creates its replacement",
			missing, strings.Join(missingWhere, "; ")))
	case missing > 0:
		// NOTHING THIS PASS COMPARED DIFFERED, so the shortfall is not a rollout this pass can see,
		// and the message has to say that much, because the alternative readings both misdirect:
		// RolloutInProgress sends the reader to diff a spec that may not have changed, and UpToDate
		// reads exactly like the steady state while the deployment is serving below its declared
		// count.
		//
		// THE CLAIM RESTS ON WHAT WAS COMPARED, NOT ON THE SPEC. An earlier wording said "nothing
		// changed the spec" as though this axis could know that; it cannot -- a scale-up whose
		// create has not landed arrives here looking exactly like a departure, because the replica
		// that would tell the two apart is the one that is absent. The honest statement is the
		// observable one, and the branch below modelDeploymentReplicasMissing records the boundary
		// in full.
		//
		// IT STOPS THERE AND DOES NOT SAY WHY THEY LEFT, which an earlier wording did. "They left on
		// their own" is a claim about a cause this axis never observed, and it is wrong in a case
		// that is neither rare nor the operator's doing: a Kueue preemption evicts the replicas
		// while the Workload survives with its reservation withdrawn, so the shortfall arrives here
		// looking exactly like a departure. Telling an operator that nothing acted on their
		// deployment, in the middle of a preemption, sends them to look for a cause on the wrong
		// side. The quota condition reports that one, and this message points at it instead of
		// competing with it.
		ModelDeploymentConditionReplicasUpToDate.False(holder, modelDeploymentReasonReplacementInProgress, fmt.Sprintf(
			"%d of the declared replicas are missing: %s. Every replica this pass could compare "+
				"matched what it rendered, so no rollout is in flight. What removed them is not "+
				"this condition's to say -- a preemption reports itself on the quota condition -- "+
				"and the pass creates each replacement as Kueue asks for it",
			missing, strings.Join(missingWhere, "; ")))
	default:
		ModelDeploymentConditionReplicasUpToDate.True(holder, modelDeploymentReasonUpToDate,
			"every replica matches what this pass rendered")
	}
}

// modelDeploymentRolloutWasInProgress reads the verdict the PREVIOUS pass wrote, which is still on
// the holder because the status under construction starts as a copy of the observed one and this
// axis has not been written yet.
//
// It exists because a rollout's last step and an unasked-for departure produce the same observable:
// replicas short, none differing. Nothing on the objects tells them apart -- the evidence left with
// the replica that was deleted -- so the question is answered from what this controller said one
// pass ago rather than from the cluster.
func modelDeploymentRolloutWasInProgress(holder *workercore.ModelDeployment) bool {
	for i := range holder.Status.Conditions {
		if holder.Status.Conditions[i].Type == string(ModelDeploymentConditionReplicasUpToDate) {
			return holder.Status.Conditions[i].Reason == modelDeploymentReasonRolloutInProgress
		}
	}

	return false
}

// modelDeploymentReplicasMissing measures how far the deployment sits below the counts its roles
// declare, and names each empty slot so the verdict it feeds speaks of replicas rather than of a
// role-level shortfall count.
//
// WHAT IT DISTINGUISHES: which ordinals of which role are empty. A slot is named by the ordinal
// label, the same identity the converger creates and removes by, so "role prefill replica 1" names
// the exact slot that is short rather than a count over the role.
//
// WHAT IT CANNOT DISTINGUISH, and no single pass can: whether an empty slot was ever filled. The
// evidence left with the replica -- its Pod, and with it the ownership every workload resolution
// reads -- so a slot whose replica departed and a slot the spec grew into yesterday's count produce
// the same observation. The shapes that meet there: a scale-up whose create has not landed, a
// top-slot replica that left on its own, and a workload deleted by hand all read as one empty slot
// beside replicas that still have workloads.
//
// HOW THE INDISTINGUISHABLE IS CLASSIFIED: by the role-level proxy "any replica of this role,
// departing ones included, still has a workload behind it". While that holds, an empty slot reads
// as a departure -- so the scale-up window above reports replacement rather than assembly, and only
// the previous pass's verdict (modelDeploymentRolloutWasInProgress) can upgrade the reading to a
// rollout. When it does not hold, the role is still assembling the first set it ever had and its
// shortfall is excluded entirely: initial creation is not the replacement of anything, and counting
// it would report every deployment's first passes as replacements.
//
// THE EMPTY SLOTS ARE FILLED LOWEST FIRST AND ONLY AS MANY AS THE COUNT IS SHORT, which is the
// same arithmetic the converger creates them by: a role carrying a replica with no ordinal still
// counts that replica against the shortfall without claiming any slot for it.
//
// A ROLE SCALED AWAY EXCLUDES ITSELF. Its workloads are deleted by the pass that sweeps its Pods,
// so it falls out of this count on its own and never reads as a replacement.
func modelDeploymentReplicasMissing(
	md *workercore.ModelDeployment, pods []core.Pod,
	wlByReplica map[types.UID]*kueue.Workload,
) (int, []string) {
	live := make(map[string]int, len(md.Spec.Roles))
	occupied := make(map[string]map[int]bool, len(md.Spec.Roles))
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			continue
		}
		role := modelDeploymentPodRole(&pods[i])
		live[role]++
		if ordinal, ok := modelDeploymentPodOrdinal(&pods[i]); ok {
			if occupied[role] == nil {
				occupied[role] = make(map[int]bool)
			}
			occupied[role][ordinal] = true
		}
	}

	missing := 0
	var where []string
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		declared := int(role.Replicas)
		short := declared - live[role.Name]
		if short <= 0 || !modelDeploymentRoleEverAssembled(role.Name, pods, wlByReplica) {
			continue
		}

		var ordinals []int
		for ordinal := 0; ordinal < declared && len(ordinals) < short; ordinal++ {
			if !occupied[role.Name][ordinal] {
				ordinals = append(ordinals, ordinal)
			}
		}
		missing += short
		where = append(where, modelDeploymentNameReplicas(role.Name, ordinals))
	}

	return missing, where
}

// modelDeploymentRoleEverAssembled reports whether any replica of the role, departing ones
// included, still has a workload behind it.
//
// IT IS A PROXY RATHER THAN A RECORD, and its limit is the boundary the missing-count comment
// states: it says the role assembled before, not that the empty slot ever existed. Departing
// replicas count, because a workload holding one is still observable ownership and its deletion --
// the converger's, during a replacement -- is exactly the moment the proxy must not flip.
func modelDeploymentRoleEverAssembled(
	role string, pods []core.Pod, wlByReplica map[types.UID]*kueue.Workload,
) bool {
	for i := range pods {
		if modelDeploymentPodRole(&pods[i]) == role && wlByReplica[pods[i].UID] != nil {
			return true
		}
	}

	return false
}

// modelDeploymentNameReplicas renders one role's empty slots as a verdict fragment:
// "role prefill replica 1", "role prefill replicas 1, 2".
func modelDeploymentNameReplicas(role string, ordinals []int) string {
	digits := make([]string, 0, len(ordinals))
	for _, ordinal := range ordinals {
		digits = append(digits, strconvx.Itoa(ordinal))
	}
	noun := "replica"
	if len(ordinals) > 1 {
		noun = "replicas"
	}

	return fmt.Sprintf("role %s %s %s", role, noun, strings.Join(digits, ", "))
}
