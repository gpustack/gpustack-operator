package worker

import (
	"fmt"

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
// A record that can vouch for no replica leaves the condition exactly as it was, for the same reason
// an unobserved domain does: reporting "nothing outdated" from a pass that neither compared nor
// created anything reads as a rollout that completed. Three passes reach the status write that way --
// a teardown, a whole-group rebuild, and the pass between a rollout's delete and its create while the
// names are still taken -- and the last is the worst, because the group is short at exactly that
// moment.
func observeModelDeploymentRollout(holder *workercore.ModelDeployment, rollout *modelDeploymentRollout) {
	// The test is on the record rather than at the call site, because "compared nothing" is a
	// property of what the pass found and every caller would otherwise have to remember it.
	if rollout == nil || rollout.accounted == 0 {
		return
	}

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
		ModelDeploymentConditionReplicasUpToDate.False(holder, modelDeploymentReasonRolloutInProgress, fmt.Sprintf(
			"%d replicas differed from what this pass rendered and were recreated", rollout.outdated))
	default:
		ModelDeploymentConditionReplicasUpToDate.True(holder, modelDeploymentReasonUpToDate,
			"every replica matches what this pass rendered")
	}
}
