# Spec: Debt Closeout — What the Delivered Specs Left Undone

Status: Planned
Blocked on: its own tasks, of which three can be started today and four cannot. The three startable
ones are code, and they are listed here rather than left in their issues because nothing else states
which set they belong to or when the set is finished. The fourth code task waits on a design decision
and is not a coding task until that decision lands. The three verification tasks need clusters this
repository does not have, and each of those says what it needs. This moves to Building when the first
task is in progress, and to Shipped when the Acceptance section below is satisfied in full — that
section, not this line, is where the finish line is written.
Type: Bug fix

## Summary

Seven items were left behind by specifications that are no longer being written — six by
specifications that have shipped, and one by a specification that is Building and blocked on hardware
it will not get soon. Each one is real, each one is small, and none of them has a home: a delivered
specification does not reopen to absorb its own leftovers, and an issue on its own says what to do
without saying what set it completes.

This document is that home. It does two things and nothing else. It fixes the membership of the set,
so that a later reader can tell whether an item belongs; and it states, per item, what counts as
done, so that the set has an end rather than a direction.

The items divide by the only distinction that changes how they are worked: whether finishing one
needs a cluster. Four are code. Three need hardware or a fabric this project does not own, and for
those the deliverable is chiefly a recorded reading rather than a merged change — chiefly, because
one of the three also has two stale pointers behind it that no reading will move.

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

It is also a source that expires, and it expired on several entries here while this document was
being drafted. Each was caught the same way, by reading the cited coordinate instead of the note
about it. The general form: a provenance does not expire, but the present tense inside it does, and
the two are usually written in the same sentence.

One consequence is built into the tables below. Where an item's obligation was written in a
specification but has since been restated on its issue, the issue is named as the authority and the
specification is not. A delivered specification is a record of what was decided, and it is not
updated when a later reading contradicts it — so on exactly the items this document exists to close,
the shipped text is the least current source available.

## Verification tasks

These need a cluster, a fabric or an accelerator that this repository does not have. For each of them
the issue named in the third column is the authoritative statement of what closes it, and a reader
scheduling one of these must read that issue rather than this table: the rows below are an index, and
two of the three carry conditions that do not fit in a cell.

| | What has to be observed | Authority, and what closes it |
| --- | --- | --- |
| V1 | A client built against one accelerator vendor and a client built against another share a single master and each reads the other's segments | Obligation written in `2026-08-28-kv-cache-backend.md`; issue 216 is the carrier |
| V2 | An engine on the second accelerator vendor starts, serves requests with the configuration this operator injects, and the cache traffic demonstrably goes through the store. Nothing has yet shown it serving at all | Issue 333. It states all three conditions together, and refuses several readings that would otherwise look like an answer |
| V3 | The packaged image carries a host-fabric member all the way onto the fabric, and the upgrade path runs | Issue 295, which restates its own acceptance after two live readings. That restatement, not the specification that deferred the work, is the criterion |

**V2 is not a reading alone.** Its issue records two pointers that still have to move and says neither
is done: a parked end-to-end case still names a closed issue as its carrier, and a note in the
injection table omits one of the two spellings by which its row can be reached, so a reader following
it concludes a route does not exist. Those two are ordinary edits, blocked on nothing, and V2 is not
closed while they stand.

**V3 is partly done, and a list like this hides that.** The identity half has been observed on a real
cluster: two co-located host-network members produced distinct segment and client identifiers that
the controller declined to attribute to individual Pods, which is the designed behaviour. What remains
unread is the transport half on the packaged image, and the upgrade step.

The criterion that half was originally written against has since been withdrawn, and the withdrawal
is the reason V3's authority is the issue rather than the specification. The specification asks for
segment names that are identical and **portless**. They are not portless on either transport: the
leader emits a dynamic port, and within one row the port in the name and the port in the transfer
endpoint are independently assigned and differ. The portless shape came from a fixture, not from a
live store. A verifier working from the shipped text would assert something already known to be
false.

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
| C3 | Correct the two status words in the header of the one specification here that is Building rather than shipped | `specs/2026-08-28-model-deployment.md:6-7` says the document moves to `Completed`, and to `Abandoned` if the decision goes the other way. Neither word is in the vocabulary the spec checker accepts, which is `Shipped`, `Building`, `Planned` and `Built` | None. No issue carries this: opening one costs more than the fix |
| C4 | Close the half of the reuse-domain separation that admission cannot currently see | Issue 379, narrowed after its store half landed. Its own text states what closes it and what does not | **A decision.** The engine consuming a backend is recorded on no object a webhook reads, so this is a change to the object model, not another check in an existing webhook |

C3 is worth one sentence of justification, because a one-line text fix in a document nobody is
editing looks like the item to drop. The words are not wrong today; they are wrong on the day
somebody follows them, and on that day the gate turns red on a change that did exactly what the
document said. The inventory that fed this set counted one such word. There are two.

C4 belongs in this set by origin, and it is the one item whose finish line is not "merged". Its issue
already states the two shapes that would close the defect — an upstream change that makes every
engine forward the identity, or somewhere for the intent to live that admission can read — and states
four things that would not. Restating them here would create a second copy that drifts.

What this document adds is the one thing the issue cannot say about itself: **C4 closes for the
purposes of this set either by merging or by a written deferral**, and the deferral is a real end
state rather than a way of not finishing. The reason is that C4's prerequisite is a decision nobody
in this set is authorised to take, and a set whose completion waits on an unowned decision has no
finish line at all. A deferral that counts names the decision, leaves the issue open, and is written
where the next reader of this document meets it. A silently dropped C4 does not count, and neither
does a closed issue with the defect still live.

## Acceptance

The set is closed when all four of the following hold. They are written as observations, not as
intentions, because the reason this document exists is that seven items had no shared finish line.

- C1, C2 and C3 have merged, and each of C1 and C2 carries a case that fails without the change.
  C1's case comes from the writer's value set and not from the enum's, for the reason its issue
  gives: the failure this change can cause is a value the controller still emits that the enum now
  refuses, which fails every later status write on the object. A case that offers an illegal value
  and sees it rejected proves the marker took effect and says nothing about that failure.
- C4 has merged, or has been deferred in writing as described above with its issue left open.
- V1, V2 and V3 each carry a recorded reading on their issue, or a written withdrawal, and V2's two
  stale pointers have moved. A reading that says the behaviour is wrong closes the verification task
  and opens a defect; it does not reopen this set.
- The two excluded items below are still excluded, or have been moved with a reason. An item that
  drifts back in silently is how a closed set reopens without anybody deciding to reopen it.

**What does not count as closing an item.** A test asserting the new behaviour with no case showing
the behaviour it replaced was reachable. A document updated to describe the gap more precisely.
A verification task marked done because the code that would be verified has merged — the whole point
of the split above is that merging is not a reading.

## Verification

The vehicle differs per task, so it is stated per task rather than once. This document adds no new
harness and names no command to re-run, because a command naming a test that does not exist yet is a
permanently green line.

- **C1** — a package test, and not the obvious one. The case enumerates every value the controller
  can write into the field and asserts the API server accepts each, because the failure this change
  introduces is a value the writer still emits that the enum now refuses, and that failure freezes
  every later status write on the object. Offering an illegal value and watching it be rejected
  proves only that the marker took effect.
- **C2** — a package test in the admission handler's own suite: an object the current handler accepts
  and the changed handler refuses, one case per rule.
- **C3** — no package test. The vehicle is the spec checker that already reads the status vocabulary
  on every documentation run, and which accepts the corrected words and rejects the present ones.
- **C4** — none, and deliberately none until its decision lands. Whatever closes it will change the
  object model, and a test written against a shape nobody has chosen would have to be discarded with
  the shape. This is an absence with a reason, not an oversight.
- **V1, V2, V3** — no case here. What each needs is recorded on its issue, which is the authority for
  all three. For V3 in particular the shipped specification must not be used: it still carries the
  criterion that its own issue has since withdrawn.

C1 has one further trap, because the obvious fix does not work either. The marker belongs on the
field, not on the named string type: that type already carries a Go-level enum marker, and that
marker does not become schema validation.

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
