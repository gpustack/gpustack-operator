# Spec: Debt Closeout — What the Delivered Specs Left Undone

Status: Planned
Blocked on: its own tasks, and for four of the seven nothing else. The four code tasks can each be
started today; they are listed here rather than left in their issues because nothing else states
which set they belong to or when the set is finished. The three verification tasks additionally need
clusters this repository does not have, and each of those says what it needs. This moves to Building
when the first code task is in progress, and to Shipped when all four have merged and each
verification task has either recorded a reading or been withdrawn with the reason written down.
Type: Bug fix

## Summary

Seven items were left behind by specifications that have already shipped. Each one is real, each one
is small, and none of them has a home: a shipped specification does not reopen to absorb its own
leftovers, and an issue on its own says what to do without saying what set it completes.

This document is that home. It does two things and nothing else. It fixes the membership of the set,
so that a later reader can tell whether an item belongs; and it states, per item, what counts as
done, so that the set has an end rather than a direction.

The items divide by the only distinction that changes how they are worked: whether finishing one
needs a cluster. Four need nothing but the work. Three need hardware or a fabric this project does
not own, and for those the deliverable is a recorded reading, not a merged change.

## Goals

- Name every task a delivered specification left undone, with the coordinate that proves it is still
  undone.
- Give each task a completion criterion that a reader can evaluate without asking the author.
- Separate the tasks that need an environment from the tasks that do not, because only the first kind
  can be blocked by something nobody in this repository controls.
- Record, for the items deliberately excluded, why they are excluded — an absent item and a rejected
  item look identical in a list that only carries what it accepts.

## Non-Goals

- Reopening any shipped specification. Where an item corrects the text of one, the correction is a
  task here and the shipped document keeps its own status.
- Deciding anything that has an open question of its own. Two items below are named and then handed
  back for exactly that reason.
- Absorbing the remainder of `2026-08-28-model-deployment.md`. That document is Building because two
  of its tasks wait on an accelerator, and no code change lifts them. Its hardware remainder is not
  debt this set closes. One defect in its header text is, and that one is a task below.
- Speaking for the prefill and decode topology work. The router and pairing design is a separate,
  undrafted document, and one item below waits on a question that belongs to it.

## Where the debt came from

Every item here was found the same way: a delivered specification says, in its own text or in the
code it shipped, that something was left. That is a stronger source than a reading of the code,
because the author recorded the gap deliberately and said why.

It is also a source that expires. Two of the entries that went into this inventory had already moved
by the time they were written down, and both were caught by reading the cited coordinate rather than
the note about it. The general form: a provenance does not expire, but the present tense inside it
does, and both are usually written in the same sentence.

## Verification tasks

These need a cluster, a fabric or an accelerator that this repository does not have. None of them has
a code change waiting behind it. The deliverable for each is a reading recorded on its issue.

| | What has to be observed | Where the obligation was written |
| --- | --- | --- |
| V1 | A client built against one accelerator vendor and a client built against another share a single master and each reads the other's segments | `2026-08-28-kv-cache-backend.md`, tracked as issue 216 |
| V2 | An engine on the second accelerator vendor actually serves with the configuration this operator injects. Nothing has yet shown it serving at all | `2026-08-28-kv-cache-injection.md`, tracked as issue 333 |
| V3 | The packaged image carries a host-fabric member all the way onto the fabric, and the upgrade path runs | `2026-09-09-kv-cache-segment-identity-status.md` names this as its own non-goal and says it is tracked separately; issue 295 is where it is tracked |

V3 is the one to read carefully before scheduling it, because it is partly done and a list like this
one hides that. The identity half has been observed: two co-located host-network members reported
segment rows sharing one portless name with distinct identifiers, and the controller declined to
attribute either row to a Pod, which is the designed behaviour. What remains unread is the transport
half on the packaged image, and the upgrade step. Scheduling V3 as though nothing had been measured
would re-run the half that already passed.

**These three are not blocked on each other, and V1 and V2 are not blocked on the topology work that
was removed from the plan.** That removal took one document out of scope; it did not take the
accelerator out of scope, and these two obligations were written by backend and injection
specifications that remain shipped.

## Code tasks

Nothing external blocks these. Three are small and complete as stated; the fourth is small to state
and is not a coding task at all until a decision lands, which is why its prerequisite is a column
rather than a footnote.

| | What to do | Proof it is still undone | Prerequisite |
| --- | --- | --- | --- |
| C1 | Give the status-side role kind the enum its spec-side twin already carries | `api/worker/v1alpha1/model_deployment.go:497` declares the field with no validation marker, while the same Go type at `:270` carries one on the line above it. Tracked as issue 381 | None. The constraint was ordering, and the specification that recorded the debt states it is now satisfied |
| C2 | Write the two admission rules the handler says are missing: that the InstanceType offers the mode the request asks for, and that the request fits its per-unit ceiling | `pkg/worker/webhooks/worker/model_deployment.go:673-677` states both in the source, and states that what prevented them is gone. Tracked as issue 380 | None |
| C3 | Correct the two status words in a shipped specification's own header | `specs/2026-08-28-model-deployment.md:6-7` says the document moves to `Completed`, and to `Abandoned` if the decision goes the other way. Neither word is in the vocabulary the spec checker accepts, which is `Shipped`, `Building`, `Planned` and `Built` | None. No issue carries this: opening one costs more than the fix |
| C4 | Close the half of the reuse-domain separation that admission cannot currently see | Issue 379, narrowed after its store half landed. Its own text states what closes it and what does not | **A decision.** The engine consuming a backend is recorded on no object a webhook reads, so this is a change to the object model, not another check in an existing webhook |

C3 is worth one sentence of justification, because a one-line text fix in a document nobody is
editing looks like the item to drop. The words are not wrong today; they are wrong on the day
somebody follows them, and on that day the gate turns red on a change that did exactly what the
document said. The inventory that fed this set counted one such word. There are two.

C4 belongs in this set by origin but will not be finished inside it unless the decision it waits on
arrives. Its issue already states the two shapes that would close it — an upstream change that makes
every engine forward the identity, or somewhere for the intent to live that admission can read — and
states four things that would not. Restating them here would create a second copy that drifts. What
this document adds is only that C4 is part of the same debt, and that the set is not finished while
it is open.

## Acceptance

The set is closed when all four of the following hold. They are written as observations, not as
intentions, because the reason this document exists is that seven items had no shared finish line.

- C1, C2 and C3 have merged, and each of C1 and C2 carries a case that fails without the change.
- C4 is either merged or recorded as deferred with the decision it waits on named and its issue left
  open. An open issue with a written reason is an acceptable end state for this set; a closed issue
  with the defect still live is not.
- V1, V2 and V3 each carry a recorded reading on their issue, or a written withdrawal. A reading that
  says the behaviour is wrong closes the verification task and opens a defect; it does not reopen
  this set.
- The two excluded items below are still excluded, or have been moved with a reason. An item that
  drifts back in silently is how a closed set reopens without anybody deciding to reopen it.

**What does not count as closing an item.** A test asserting the new behaviour with no case showing
the behaviour it replaced was reachable. A document updated to describe the gap more precisely.
A verification task marked done because the code that would be verified has merged — the whole point
of the split above is that merging is not a reading.

## Verification

Each code task carries its own case, and the three cases are ordinary package tests; this document
adds no new harness and names no command to re-run, because a command naming a test that does not
exist yet is a permanently green line.

For C1 and C2 the case is a rejection: an object that the current code accepts and the changed code
refuses. For C3 the case is the spec checker itself, which already reads the status vocabulary and
will accept the corrected words.

C1 has one trap worth naming, because the obvious fix does not work. The marker belongs on the
field, not on the named string type: that type already carries the Go-level enum marker, and that
marker does not become schema validation. C1 also changes what the API server accepts on a status
write, which is why the specification that recorded it asked for verification of its own rather than
letting it ride along with something else.

For V1, V2 and V3 there is no case to write here. What each needs is recorded on its issue, and V3's
acceptance is written out in full in the specification that deferred it, including the control that
must stay non-ambiguous.

## Open Questions

**Does a member drain its memory segment on scale-in, or keep dropping it?** Two shipped
specifications say it drops, and both call the drain a non-goal. One of the reasons they gave has
since changed: the leader's listing does expose the identifiers, and the decoder that reads them has
shipped. What has not changed is that a member has no supported way to read its own identifier, so a
drain would have to be driven from what the leader reports rather than from inside the member. That
is a design choice with a cost, not an oversight, and it is not scheduled here.

**Where does the engine that cannot declare prefill or decode get answered?** The refusal it hits
today is the behaviour an open question deliberately asked for, so it is not a defect. Answering it
needs two things this set does not have: a version-pinned fact about the engine's own flag, and the
router question that belongs to the undrafted pairing specification. Named here so that the next
reader does not re-triage it as debt; tracked as issue 383.
