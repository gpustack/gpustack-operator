package worker

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// The four layers are reported separately because they are four different questions, and the
// failure this design exists to prevent is answering one of them with another. A deployment that
// admits four workers has said nothing about how many Ray members exist, and an engine reporting
// four effective workers has said nothing about how many Pods Kubernetes admitted. Each layer
// carries its own value and its own absence, and a missing layer is never filled in from a
// neighboring one.

// elasticLayer is one layer's independently observed value.
//
// KNOWN IS SEPARATE FROM VALUE because a zero is a real observation and an unobserved layer is not.
// A layer that was not read and a layer that was read as zero differ in exactly the way that
// matters: one may authorize an action and the other must not.
type elasticLayer struct {
	Known  bool
	Value  int
	Reason string
}

// knownLayer is a layer that was read.
func knownLayer(value int) elasticLayer { return elasticLayer{Known: true, Value: value} }

// unknownLayer is a layer that was not read, carrying why.
func unknownLayer(reason string) elasticLayer { return elasticLayer{Reason: reason} }

// elasticFacts is everything the kernel is allowed to decide from. It is a plain value with no
// client, no clock and no network, so the same facts always produce the same decision and a test
// compares the decision rather than the calls that led to it.
type elasticFacts struct {
	// DeploymentUID is the deployment the facts were read for. Facts about a different deployment
	// decide nothing about this operation.
	DeploymentUID types.UID
	// Generation is the spec generation the facts were read at.
	Generation int64
	// Desired, Admitted, Ray and Effective are the four layers.
	Desired   elasticLayer
	Admitted  elasticLayer
	Ray       elasticLayer
	Effective elasticLayer
	// AdmittedAllocation is whether the admitted workers hold a real accelerator allocation. A
	// worker Pod that is admitted without an allocation is a Pod that exists and holds nothing.
	AdmittedAllocation elasticLayer
	// WithdrawalConfirmed is the serving side's own statement that the narrowing has been
	// withdrawn. A boolean from anywhere else is not this fact.
	WithdrawalConfirmed bool
	// NativeComplete is whether the engine reports the operation finished at the target width.
	NativeComplete bool
	// ScalingKnown and Scaling answer whether the engine is mid-resize, read beside a recorded
	// command refusal: a refused command is re-issued only to an engine that is not already
	// working, and an engine that did not answer is not assumed idle.
	ScalingKnown bool
	Scaling      bool
	// ForwardProven is whether a real request has been served since the engine reported the new
	// width. An engine reporting a width is a claim about itself, not evidence it serves.
	ForwardProven bool
	// ActorFree is, per captured worker UID, whether the runtime has shown it holds no actor. A
	// worker with actors may not be removed however narrow the collective is.
	ActorFree map[types.UID]bool
	// ActorMappingKnown is whether the runtime's actor-to-Pod mapping was readable at all. A
	// mapping that could not be read proves nothing about any worker.
	ActorMappingKnown bool
	// ReleaseObserved is whether the ledger and the quota have both been seen released for the
	// captured workers. A delete that was issued is not a release.
	ReleaseObserved bool
}

// elasticAction is what the kernel decided to do next.
type elasticAction string

const (
	// elasticActionHold changes nothing and says why. It is the answer for every case where a fact
	// is missing, and it is the answer a restarted controller most often gives.
	elasticActionHold elasticAction = "Hold"
	// elasticActionRequestScale is the first time a command is sent. A refused command is sent
	// again by elasticActionRetryScale, within the bound the record states.
	elasticActionRequestScale elasticAction = "RequestScale"
	// elasticActionRetryScale re-issues a command the engine refused, on the record's bounded
	// schedule. It is decided only from a recorded refusal and only for an idle engine, and when
	// the attempt is due is the caller's clock question, never this answer.
	elasticActionRetryScale elasticAction = "RetryScale"
	// elasticActionAwaitNative waits for the engine to report the new width.
	elasticActionAwaitNative elasticAction = "AwaitNative"
	// elasticActionAwaitWithdrawal waits for the serving side to confirm the narrowing withdrew.
	elasticActionAwaitWithdrawal elasticAction = "AwaitWithdrawal"
	// elasticActionAwaitForward waits for a real request to be served at the new width.
	elasticActionAwaitForward elasticAction = "AwaitForward"
	// elasticActionRelease removes the captured actor-free workers.
	elasticActionRelease elasticAction = "Release"
	// elasticActionComplete ends the operation.
	elasticActionComplete elasticAction = "Complete"
)

// elasticDecision is the kernel's answer: what to do, and what the state becomes.
type elasticDecision struct {
	Action elasticAction
	State  elasticOperationState
	Reason string
}

// hold is the single place a refusal is built, so every hold carries a state and a reason and none
// of them can be returned as a bare boolean.
func elasticHold(state elasticOperationState, format string, args ...any) elasticDecision {
	return elasticDecision{Action: elasticActionHold, State: state, Reason: fmt.Sprintf(format, args...)}
}

// The refused scale command is retried on a bounded schedule, and the bound is part of the record's
// contract, so it is stated here: at most five attempts, and attempt n+1 waits
// min(15s*2^(n-1), 4m) after refusal n — one reconcile interval, then doubling to a cap. The fifth
// refusal abandons the operation terminally instead of leaving it awaiting a width no command is
// coming to report.
const (
	elasticScaleMaxAttempts = 5
	elasticScaleRetryBase   = 15 * time.Second
	elasticScaleRetryCap    = 4 * time.Minute
)

// decideElasticOperation is the whole kernel.
//
// IT IS A PURE FUNCTION OF A RECORD AND A SET OF FACTS. Nothing here observes, retries, sleeps or
// dispatches; a caller acts on the decision and brings the result back as facts. That is what makes
// a restart reconstructable: the record plus fresh facts produce the same decision they would have
// produced before the process died.
//
// THE ORDER OF THE CHECKS IS THE ORDER OF WHAT MUST BE PROVEN, and each gap is a hold. Facts about
// the wrong deployment or a generation that has moved on, a record that already asked, a capacity
// that was not admitted or not allocated, a Ray world that does not corroborate it, a worker whose
// actor state is unreadable, and a release that was issued but not observed are all states in which
// this controller may not remove anything.
func decideElasticOperation(op *elasticOperation, facts elasticFacts) elasticDecision {
	if err := op.validate(); err != nil {
		return elasticDecision{
			Action: elasticActionHold,
			State:  op.State,
			Reason: err.Error(),
		}
	}

	// FACTS ABOUT SOMETHING ELSE DECIDE NOTHING. A record read for one deployment with facts read
	// for another is not a partially known answer, it is an answer about a different object.
	if facts.DeploymentUID != op.DeploymentUID {
		return elasticHold(op.State,
			"the facts are about deployment %s rather than %s", facts.DeploymentUID, op.DeploymentUID)
	}

	// A SPEC THAT HAS MOVED ON IS A DIFFERENT INTENT. The operation stays unresolved and is
	// reconstructed, never carried forward against the new generation.
	if facts.Generation != op.Generation {
		return elasticHold(op.State,
			"the deployment is at generation %d while this operation is bound to %d",
			facts.Generation, op.Generation)
	}

	// A COMMAND THAT HAS LEFT IS NOT SENT AGAIN — except on the one answer the record itself
	// carries. This is the whole point of the durable intent: a timeout, a 408, a 500 and an
	// unreadable answer all look alike to an observer, and none of them is permission to try
	// again on a guess. A refusal the ENGINE ANSWERED WITH is different only because the record
	// keeps its text, and the after-command path below is where that recorded refusal is turned
	// into a bounded retry or a terminal abandon.
	//
	// THE STATE GOVERNS, NOT THE BOOLEAN ALONGSIDE IT. The boolean is a copy of a fact the state
	// already carries, and a copy that disagrees with its original is not evidence in the copy's
	// favor. A record whose state says a command left is one that may not ask again whatever the
	// other field says.
	if op.SentState() {
		return decideAfterCommand(op, facts)
	}

	return decideBeforeCommand(op, facts)
}

// decideBeforeCommand is the only path that may ask the engine to do anything, and it requires the
// capacity to exist before the ask rather than after it.
//
// UPWARD, THE TARGET WIDTH MUST BE ADMITTED AND ALLOCATED AND CORROBORATED BY RAY. A resize asked
// for ahead of its own capacity is a request the cluster cannot satisfy, and a Ray world that does
// not already carry the target width is evidence against the admission rather than a thing to
// discover by trying.
//
// DOWNWARD, THE COMMAND IS PERMITTED WITHOUT ADMISSION because the workers are leaving. What it
// does need is that the target width is not a number the cluster has been asked for, which is
// carried by the record rather than by a layer.
func decideBeforeCommand(op *elasticOperation, facts elasticFacts) elasticDecision {
	// THE DESIRED LAYER GATES EVERY ENGINE ACTION, because it is the spec's own statement of what
	// width this deployment wants. An unobserved desired width is not a yes, and a desired width
	// that disagrees with this record's target is a different intent that this record may not carry
	// out. Agreement between the record and the spec is what makes the record current.
	if !facts.Desired.Known {
		return elasticHold(op.State, "the desired width is unknown: %s", facts.Desired.Reason)
	}
	if facts.Desired.Value != op.Width.Target {
		return elasticHold(op.State, "the spec now wants width %d rather than %d",
			facts.Desired.Value, op.Width.Target)
	}

	// A NARROWING IS WITHDRAWN BEFORE THE ENGINE IS TOLD. Asking an engine to drop workers while
	// the serving side is still routing to them removes capacity that is in use, so the withdrawal
	// is confirmed first and the command is a consequence of it rather than a parallel step.
	if !op.Width.Upward() {
		if !facts.WithdrawalConfirmed {
			return elasticDecision{
				Action: elasticActionAwaitWithdrawal,
				State:  op.State,
				Reason: "the serving side has not confirmed the narrowing withdrew",
			}
		}

		return elasticDecision{
			Action: elasticActionRequestScale,
			State:  elasticStateCommandSent,
			Reason: fmt.Sprintf("the engine was not yet asked to move from %d to %d",
				op.Width.Old, op.Width.Target),
		}
	}

	if !facts.Admitted.Known {
		return elasticHold(op.State, "admitted width is unknown: %s", facts.Admitted.Reason)
	}
	if facts.Admitted.Value < op.Width.Target {
		return elasticHold(op.State, "only %d workers are admitted and %d are wanted",
			facts.Admitted.Value, op.Width.Target)
	}
	if !facts.AdmittedAllocation.Known {
		return elasticHold(op.State, "admitted allocation is unknown: %s", facts.AdmittedAllocation.Reason)
	}
	if facts.AdmittedAllocation.Value < op.Width.Target {
		return elasticHold(op.State, "only %d workers hold an allocation and %d are wanted",
			facts.AdmittedAllocation.Value, op.Width.Target)
	}
	if !facts.Ray.Known {
		return elasticHold(op.State, "the ray world is unknown: %s", facts.Ray.Reason)
	}
	if facts.Ray.Value < op.Width.Target {
		return elasticHold(op.State, "the ray world holds %d members and %d are wanted",
			facts.Ray.Value, op.Width.Target)
	}

	return elasticDecision{
		Action: elasticActionRequestScale,
		State:  elasticStateCommandSent,
		Reason: fmt.Sprintf("the engine was not yet asked to move from %d to %d",
			op.Width.Old, op.Width.Target),
	}
}

// decideAfterCommand is every state a command that has left can be in. None of them sends a
// command, because the record says one already did.
func decideAfterCommand(op *elasticOperation, facts elasticFacts) elasticDecision {
	if op.Width.Upward() {
		return decideUpwardAfterCommand(op, facts)
	}

	// A RELEASED OPERATION IS ALREADY PAST THE PROOFS, so it is finished or it is not, and the only
	// thing left to observe is the release itself. Asking for it again would re-enter the proof
	// chain above, which is a different operation.
	if op.State == elasticStateReleased {
		return elasticReleaseComplete(op, facts)
	}

	return decideDownwardAfterCommand(op, facts)
}

// elasticScaleRefusalOutcome turns a recorded command refusal into the decision that follows it:
// another attempt while the bound holds, and a terminal abandon once it is spent. It decides
// nothing for a record whose command was never refused, and it is reached only after the
// completion checks, so an engine that applied a command whose answer was lost still completes by
// observation. Whether the next attempt is DUE yet is the caller's clock question; this decision
// says only that one is owed, and only to an engine that answered and is not already scaling.
func elasticScaleRefusalOutcome(op *elasticOperation, facts elasticFacts) (elasticDecision, bool) {
	if op.ScaleLastError == "" {
		return elasticDecision{}, false
	}
	if !facts.ScalingKnown {
		return elasticHold(op.State,
			"the engine did not answer whether it is scaling, so the refused command is not re-issued on a guess; the recorded refusal: %s",
			op.ScaleLastError), true
	}
	if facts.Scaling {
		return elasticHold(op.State,
			"the engine is still scaling, so the refused command waits for it to settle; the recorded refusal: %s",
			op.ScaleLastError), true
	}
	if max(op.ScaleAttempts, 1) < elasticScaleMaxAttempts {
		return elasticDecision{
			Action: elasticActionRetryScale,
			State:  op.State,
			Reason: fmt.Sprintf("the engine refused the scale command to width %d; the recorded refusal: %s",
				op.Width.Target, op.ScaleLastError),
		}, true
	}
	return elasticHold(elasticStateAbandoned, "%s", elasticScaleRefusalMessage(op)), true
}

// elasticScaleRefusalMessage is the terminal refusal: the widths, the spent bound and the engine's
// own last refusal text, which the record kept for exactly this reading.
func elasticScaleRefusalMessage(op *elasticOperation) string {
	return fmt.Sprintf(
		"the scale command from width %d to %d was refused %d times and its retry bound is spent; the last refusal: %s",
		op.Width.Old, op.Width.Target, max(op.ScaleAttempts, 1), op.ScaleLastError)
}

// decideUpwardAfterCommand waits for the engine to report the new width. There is nothing to remove
// in this direction, so the operation ends when the engine agrees.
func decideUpwardAfterCommand(op *elasticOperation, facts elasticFacts) elasticDecision {
	if !facts.NativeComplete {
		if decision, refused := elasticScaleRefusalOutcome(op, facts); refused {
			return decision
		}
		return elasticDecision{
			Action: elasticActionAwaitNative,
			State:  elasticStateAwaitingWidth,
			Reason: "the engine has not reported the new width yet",
		}
	}
	if !facts.Effective.Known {
		return elasticHold(elasticStateNativeDone, "the effective width is unknown: %s", facts.Effective.Reason)
	}
	if facts.Effective.Value != op.Width.Target {
		return elasticHold(elasticStateNativeDone, "the engine reports width %d rather than %d",
			facts.Effective.Value, op.Width.Target)
	}
	// A WIDTH IS A CLAIM AND A FORWARD IS EVIDENCE. An engine that reports the new width has said
	// what it believes about itself; only a request actually served at that width has shown it can
	// serve there, and the same rule governs the narrowing direction.
	if !facts.ForwardProven {
		return elasticDecision{
			Action: elasticActionAwaitForward,
			State:  elasticStateNativeDone,
			Reason: "no request has been served at the new width yet",
		}
	}

	return elasticDecision{
		Action: elasticActionComplete,
		State:  elasticStateCompleted,
		Reason: fmt.Sprintf("the engine serves at width %d", op.Width.Target),
	}
}

// decideDownwardAfterCommand is the direction that removes things, so it is where every proof is
// required. The order is the order the proofs can be obtained in: the engine finishes, the engine's
// new width is observed, a real request is served at it, the serving side confirms the narrowing
// withdrew, the workers are proven actor-free, and only then is anything released.
func decideDownwardAfterCommand(op *elasticOperation, facts elasticFacts) elasticDecision {
	if !facts.NativeComplete {
		if decision, refused := elasticScaleRefusalOutcome(op, facts); refused {
			return decision
		}
		return elasticDecision{
			Action: elasticActionAwaitNative,
			State:  elasticStateAwaitingWidth,
			Reason: "the engine has not reported the new width yet",
		}
	}
	if !facts.Effective.Known {
		return elasticHold(elasticStateNativeDone, "the effective width is unknown: %s", facts.Effective.Reason)
	}
	if facts.Effective.Value != op.Width.Target {
		return elasticHold(elasticStateNativeDone, "the engine reports width %d rather than %d",
			facts.Effective.Value, op.Width.Target)
	}
	// AN ENGINE REPORTING A WIDTH IS A CLAIM ABOUT ITSELF. Nothing has been shown to serve at the
	// new width until a request has been served there.
	if !facts.ForwardProven {
		return elasticDecision{
			Action: elasticActionAwaitForward,
			State:  elasticStateNativeDone,
			Reason: "no request has been served at the new width yet",
		}
	}
	if !facts.WithdrawalConfirmed {
		return elasticDecision{
			Action: elasticActionAwaitWithdrawal,
			State:  elasticStateWithdrawn,
			Reason: "the serving side has not confirmed the narrowing withdrew",
		}
	}

	if reason := elasticRetirementRefusal(op, facts); reason != "" {
		return elasticHold(elasticStateWithdrawn, "%s", reason)
	}

	return elasticDecision{
		Action: elasticActionRelease,
		State:  elasticStateReleased,
		Reason: fmt.Sprintf("%d captured workers are proven actor-free and may be released",
			len(op.retractableWorkers())),
	}
}

// elasticRetirementRefusal is every way a worker may not be removed, as one reason.
//
// THE MAPPING MUST HAVE BEEN READ AT ALL. A mapping that could not be read proves nothing about
// any worker, and reading a missing entry as "no actors" is how a worker holding the collective's
// state is deleted.
func elasticRetirementRefusal(op *elasticOperation, facts elasticFacts) string {
	if !facts.ActorMappingKnown {
		return "the runtime's actor mapping could not be read, so no worker is proven actor-free"
	}

	for _, worker := range op.retractableWorkers() {
		if worker.UID == op.Master.UID {
			return fmt.Sprintf("worker %s is the master and is never retirable", worker.Name)
		}
		actorFree, known := facts.ActorFree[worker.UID]
		if !known {
			return fmt.Sprintf("worker %s has no actor evidence, because nothing mapped it", worker.Name)
		}
		if !actorFree {
			return fmt.Sprintf("worker %s still holds an actor", worker.Name)
		}
	}

	return ""
}

// elasticReleaseComplete is the last step, and it needs the release observed rather than issued.
//
// A DELETE THAT WAS SENT IS NOT A RELEASE. The ledger and the quota are what say a worker's
// accelerator came back, and a Pod that no longer exists is equally consistent with one that was
// deleted while still holding its allocation.
func elasticReleaseComplete(_ *elasticOperation, facts elasticFacts) elasticDecision {
	if !facts.ReleaseObserved {
		return elasticHold(elasticStateReleased,
			"the captured workers have not been seen released from the ledger and the quota")
	}

	return elasticDecision{
		Action: elasticActionComplete,
		State:  elasticStateCompleted,
		Reason: "the narrowing is released, observed and complete",
	}
}
