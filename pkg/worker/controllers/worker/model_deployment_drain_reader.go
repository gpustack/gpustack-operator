// The drain seam: the one question the retirement protocol asks a replica's engine before it is
// allowed to commit a delete.
//
// The transport belongs to a later task, so this file defines the question and refuses to answer
// it. Production wiring returns Unsupported until a collector lands behind it, and that is the
// safe direction: the protocol holds at Draining and deletes nothing. A seam that answered Idle
// while unwired would turn "nobody is watching" into "the queue is empty", which is the exact
// inversion this type exists to make impossible.
//
// IT IS AN INTERFACE RATHER THAN A DIAL, for the same reason the serving view and the cache
// scraper are: the states this has to get right are the failures, and a real collector cannot be
// made to produce a missing series, an unbindable collection or a transport error on demand.
//
// THE READ IS PER MEMBER, and the type is what makes that enforceable rather than merely stated.
// A group aggregate may stand for the group and never for an individual member's zero, so no
// single call here can ever answer "this replica is idle" by reporting a number summed across
// members. Getting that wrong would let a busy member ride an idle sibling out of selection.
package worker

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// modelDeploymentDrainState is what one member's engine can say about the work it still holds.
//
// THE FOUR STATES ARE FOUR INSTRUCTIONS. Idle is a claim the protocol is allowed to act on.
// Busy is a claim that it must not. Unknown is an absence of an answer and holds. Unsupported is
// a statement that no such measurement exists for this engine, and it holds for a different
// reason: the shape is unmeasured rather than unobserved, and a future transport is expected to
// change it while a flaky collector is not.
type modelDeploymentDrainState string

const (
	modelDeploymentDrainIdle        modelDeploymentDrainState = "Idle"
	modelDeploymentDrainBusy        modelDeploymentDrainState = "Busy"
	modelDeploymentDrainUnknown     modelDeploymentDrainState = "Unknown"
	modelDeploymentDrainUnsupported modelDeploymentDrainState = "Unsupported"
)

// modelDeploymentDrainTarget names the one member a read is about. The container and the engine
// are carried rather than looked up at the call site, so a reader cannot be handed a member and
// quietly answer for a different container of it.
type modelDeploymentDrainTarget struct {
	PodUID    types.UID
	Container string
	Role      string
	Engine    string
	// DeploymentUID is the deployment the member is read on behalf of. A member is only this
	// member if the live object still belongs to the operation that named it, and a Pod whose owner
	// has been recreated under the same name is a different owner.
	DeploymentUID types.UID
}

// modelDeploymentDrainAnswer is three-valued on purpose and carries its own completeness, because
// "the numbers I read were zero" and "I read every number that counts" are different facts and
// only the conjunction of them is a zero the protocol may act on.
type modelDeploymentDrainAnswer struct {
	State  modelDeploymentDrainState
	Reason string
	// Complete is whether every expected series was present in a well-formed envelope matched to
	// this member. An incomplete read is Unknown whatever the numbers say, so a reader that scraped
	// two of the four SGLang gauges cannot report a zero by summing what it did find.
	Complete bool
	// Series is what the read observed, kept for the status reason so a reader asking "why is this
	// held" gets the numbers rather than only the verdict.
	Series map[string]float64
}

// modelDeploymentDrainReader reads ONE member's engine in-flight activity.
type modelDeploymentDrainReader interface {
	// Drain reports what this member still has in flight. It is per-member by construction: a
	// group aggregate may stand for the group and never for an individual member's zero.
	Drain(ctx context.Context, target modelDeploymentDrainTarget) (modelDeploymentDrainAnswer, error)
}

// unwiredDrainReader is the production reader until a collector lands behind the seam. It refuses
// with the engine named, so a status read during the interval says which engine went unmeasured
// rather than that a queue was empty.
type unwiredDrainReader struct{}

// Drain reports Unsupported. It never reports an error: an unwired seam is a known state of the
// tree, not a failure, and returning an error would make every reader treat it as one to retry.
func (unwiredDrainReader) Drain(
	_ context.Context, target modelDeploymentDrainTarget,
) (modelDeploymentDrainAnswer, error) {
	return modelDeploymentDrainAnswer{
		State:  modelDeploymentDrainUnsupported,
		Reason: fmt.Sprintf("no engine in-flight collector is wired for %s yet", target.Engine),
	}, nil
}

// modelDeploymentDrainReaderOf resolves the reader for a pass. A nil field is the production
// transport, which today is the refusal above.
func (r *ModelDeploymentReconciler) modelDeploymentDrainReaderOf() modelDeploymentDrainReader {
	if r.drainReader == nil {
		return unwiredDrainReader{}
	}

	return r.drainReader
}
