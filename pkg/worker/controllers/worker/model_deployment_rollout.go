package worker

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
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
	// nothing more often than it looks: a rebuild that deletes every member without comparing, a
	// teardown, and the pass between a rollout's delete and its create, where the names are still
	// held by terminating replicas so the creates do not land either.
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
// wrote, which after a steady deployment is an authoritative True. A group-shape edit then deletes
// every replica without vouching for one, and the object goes on saying every replica matches the
// render while none exists.
//
// Three passes arrive here -- a teardown, a whole-group rebuild, and the pass between a rollout's
// delete and its create while the names are still taken -- and all three are moments when the
// replicas are least current, so the stale answer is wrong in exactly the state it is read in.
// Unknown is the same shape CacheAttached already uses for a reading it could not take.
//
// THE PASS'S DECISION AND THE OBSERVED COUNT ANSWER TOGETHER, and the order they are asked in is
// the ranking of causes. A held rollout outranks everything, because nothing else can be acted on
// until the store returns. A rollout outranks a replacement, because when both are in flight the
// rollout is the umbrella cause -- it is why a replica is away, and its message is the one the
// reader who caused it needs. The replacement is what remains when every surviving replica is
// current and the count is still short, which no spec change explains.
func observeModelDeploymentRollout(
	holder, md *workercore.ModelDeployment, pods []core.Pod,
	wlByGroup map[string]*kueue.Workload, groupOfRole map[string]string, rollout *modelDeploymentRollout,
) {
	// The test is on the record rather than at the call site, because "vouched for nothing" is a
	// property of what the pass found and every caller would otherwise have to remember it.
	if rollout == nil || rollout.accounted == 0 {
		ModelDeploymentConditionReplicasUpToDate.Unknown(holder, modelDeploymentReasonRolloutNotObserved,
			"this pass accounted for no replica: it compared no hash and created none, so whether the "+
				"replicas match the current render was not established either way")

		return
	}

	missing, missingRoles := modelDeploymentReplicasMissing(md, pods, wlByGroup, groupOfRole)

	switch {
	case rollout.held > 0:
		// IT DOES NOT SAY A CHANGE IS WAITING, because it cannot know. During an outage the desired
		// render carries no connector, so every attached replica's hash differs whether or not
		// anyone edited the deployment -- an ordinary store blink puts every deployment on the pool
		// here. Naming a change the user may not have made would be wrong far more often than right,
		// so the consequence is stated conditionally and the reader is pointed at the store.
		//
		// AND IT NAMES THE CLASS RATHER THAN "a spec edit", which is wider than this guard. An edit
		// to the replica counts or the role set moves the group annotations, so the group resizes
		// and takes the rebuild branch, which deletes every replica before this guard is reached:
		// that edit proceeds during an outage. What waits is an edit that changes a replica's
		// rendered Pod while leaving the group's shape alone.
		ModelDeploymentConditionReplicasUpToDate.False(holder, modelDeploymentReasonRolloutHeldByCache, fmt.Sprintf(
			"%d of %d replicas differ from what this pass rendered and were left in place: the KV "+
				"cache connection could not be resolved, and recreating them on that alone would "+
				"rebuild every replica whenever the store blinks. Until it resolves, an edit that "+
				"changes a replica's rendered Pod without changing the group's shape waits with "+
				"them -- withheld rather than dropped, and it rolls out once the connection returns",
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
			"%d of the declared replicas are being replaced by the rollout in flight, from the "+
				"groups of roles %s: the last replica built from the earlier spec has gone, and the "+
				"pass that finds it gone creates its replacement",
			missing, strings.Join(missingRoles, ", ")))
	case missing > 0:
		// NOBODY CHANGED THE SPEC. Every replica this pass could compare matched the render, so the
		// shortfall is not a rollout's doing, and the message has to say that much, because the
		// alternative readings both misdirect: RolloutInProgress sends the reader to diff a spec
		// that did not change, and UpToDate reads exactly like the steady state while the deployment
		// is serving below its declared count.
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
			"%d of the declared replicas are missing, from the groups of roles %s, while every "+
				"surviving replica matches what this pass rendered: no rollout is in flight, because "+
				"nothing changed the spec. What removed them is not this condition's to say -- a "+
				"preemption reports itself on the quota condition -- and the pass creates each "+
				"replacement as Kueue asks for it",
			missing, strings.Join(missingRoles, ", ")))
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
// declare, and names the roles that are short.
//
// ONLY A GROUP WHOSE WORKLOAD KUEUE HAS COMPOSED COUNTS, and that test is what separates a
// replacement from a beginning. Kueue composes a Workload for a group once it has seen its declared
// total, so a group with a Workload and a missing replica has LOST one, while a group with no
// Workload is still assembling the first set it ever had -- initial creation is not the replacement
// of anything. Counting both would report every deployment's first passes as replacements, and
// counting neither is the old answer, which read a lost replica exactly like the steady state.
//
// A REBUILD EXCLUDES ITSELF. A resizing group's Workload is deleted by the pass that tears it down,
// so the roles of a scale change fall out of this count on their own and come back as the new
// groups' first creation rather than as replacements.
func modelDeploymentReplicasMissing(
	md *workercore.ModelDeployment, pods []core.Pod,
	wlByGroup map[string]*kueue.Workload, groupOfRole map[string]string,
) (int, []string) {
	live := make(map[string]int, len(md.Spec.Roles))
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			live[modelDeploymentPodRole(&pods[i])]++
		}
	}

	missing := 0
	var roles []string
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		short := int(role.Replicas) - live[role.Name]
		if short <= 0 || wlByGroup[groupOfRole[role.Name]] == nil {
			continue
		}
		missing += short
		roles = append(roles, role.Name)
	}

	return missing, roles
}
