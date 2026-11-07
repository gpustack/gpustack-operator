"""Source protection and failure fences, probed at backend boundaries.

Every probe here uses disposable owned buffers — never real weights — and
observes only what the backend itself does at its own boundary. A
refusal logged by this wrapper proves nothing; enforcement must come from
the backend ("backend"), or the probe is NO-GO. An earlier NIXL raw-key
probe CHANGED the source bytes: an unverified raw key exposure is NO-GO,
never a configuration note.

Faults are injected only at observable boundaries: before submission (the
slot is released cleanly, the operation FAILED) and after an observed
submission that stays nonterminal (the operation is UNKNOWN, its window
and registration retained in quarantine until a qualified fence resolves
it). A process exiting or a wrapper checking first are neither.

Stale access is bounded honestly: a snapshot unchanged after a bounded
window proves neither revocation nor the absence of a late write, so it is
reported UNRUN with its evidence, never PASS.

The source window's immutability has no passing answer yet. A backend
refusal of an in-window write leaves it UNRUN, because a refusal proves
nothing without a working positive WRITE control in the same live session
and without the qualified submission and terminal fence that would show
no write is queued or in flight behind it.

The same rule governs every other bare refusal here. The raw access
probe and the stale access probe leave a backend refusal UNRUN, because
a refusal and an unchanged snapshot say only that nothing was observed.
Each one needs an operation-specific positive control in the same live
session before its refusal could prove anything.
"""

import time
from dataclasses import dataclass, field

from gpustack_model_transfer.backends.nixl.errors import TransferRefusedError
from gpustack_model_transfer.core.operation import OpState

PASS = "PASS"
NO_GO = "NO-GO"
UNRUN = "UNRUN"

# What an out-of-bounds or unauthorized attempt met at the boundary.
ENFORCED_BY_BACKEND = "backend"
ENFORCED_BY_WRAPPER = "wrapper"

# The bounded observation window for a stale access: long enough to watch,
# short enough to stay honest about what a snapshot can prove.
STALE_OBSERVATION_SECONDS = 0.05

# The operation each raw probe drives. The three differ, so the control
# each one is missing differs too: a raw WRITE refusal needs a WRITE
# control, a raw READ refusal needs a READ control, and an atomic
# refusal needs an atomic control. One control does not stand in for
# another, so the caller names the operation it drives.
RAW_WRITE = "raw-write"
RAW_READ = "raw-read"
ATOMIC = "atomic"

_RAW_OPERATION_CONTROLS = {
    RAW_WRITE: "the raw WRITE path",
    RAW_READ: "the raw READ path",
    ATOMIC: "the atomic path",
}


def _positive_control(operation: str) -> str:
    """The control the named operation would need before its refusal
    could prove anything.

    An unknown operation is a caller error: the probe cannot name the
    control it is missing, so it refuses rather than reports a vaguer
    verdict.
    """
    try:
        return _RAW_OPERATION_CONTROLS[operation]
    except KeyError:
        raise ValueError(
            f"unknown raw operation {operation!r}; known operations are "
            + ", ".join(sorted(_RAW_OPERATION_CONTROLS))
        ) from None


@dataclass(frozen=True)
class SafetyVerdict:
    """One safety capability's result, with its evidence attached.

    enforcement names who refused a denied attempt: only the backend's own
    refusal counts as enforcement. evidence carries the raw observations so
    a reviewer can disagree with the verdict from the record.
    """

    capability: str
    status: str
    detail: str
    enforcement: str = ""
    evidence: tuple = ()

    def validate(self) -> None:
        if self.status not in (PASS, NO_GO, UNRUN):
            raise ValueError(f"unknown safety status {self.status!r}")


@dataclass
class SafetyReport:
    """All probes' verdicts, never collapsed into one number."""

    verdicts: list = field(default_factory=list)

    def add(self, verdict: SafetyVerdict) -> SafetyVerdict:
        verdict.validate()
        self.verdicts.append(verdict)
        return verdict

    def by_status(self, status: str) -> list:
        return [v for v in self.verdicts if v.status == status]


def _denied(api_result) -> SafetyVerdict | None:
    """Record one denied attempt, or None when it was not denied.

    The helper says who refused and nothing more. A backend refusal is
    recorded as UNRUN, never PASS: whether a refusal proves anything
    depends on the probe's own evidence rules, and no probe has the
    evidence a bare refusal would need.
    """
    if api_result is None or not getattr(api_result, "denied", False):
        return None
    enforcement = getattr(api_result, "enforcement", "")
    detail = getattr(api_result, "detail", str(api_result))
    if enforcement == ENFORCED_BY_BACKEND:
        return SafetyVerdict(
            capability="",
            status=UNRUN,
            detail=f"the backend refused the attempt: {detail}",
            enforcement=enforcement,
        )
    return SafetyVerdict(
        capability="",
        status=NO_GO,
        detail=(
            f"only the wrapper refused the attempt ({detail}); a "
            "wrapper-only negative is not backend enforcement"
        ),
        enforcement=enforcement or ENFORCED_BY_WRAPPER,
    )


@dataclass(frozen=True)
class _RawAttempt:
    """What one raw attempt did, as the probe could observe it.

    The bytes decide before the refusal does: a refusal that also landed
    is a landed mutation wearing a refusal's name.
    """

    name: str
    changed: bool
    landed: bool
    denied: SafetyVerdict | None


def _observe_raw_attempt(api, agent, registration, buffer, attempt) -> _RawAttempt:
    """Run one raw attempt and record what it changed and what it reported."""
    before = bytes(buffer)
    result = attempt(api, agent, registration)
    after = bytes(buffer)
    return _RawAttempt(
        name=getattr(attempt, "__name__", "attempt"),
        changed=after != before,
        landed=bool(getattr(result, "landed", False)),
        denied=_denied(result),
    )


def probe_raw_access(
    api,
    agent,
    registration,
    buffer,
    attempt,
    *,
    operation: str,
) -> SafetyVerdict:
    """Probe one raw out-of-bounds access into a disposable source buffer.

    attempt is the api's raw escape hatch ("raw_write_out_of_bounds",
    "raw_read_out_of_bounds", "raw_atomic_on_source"): the probe calls it
    and judges only what the boundary did. operation names which raw
    path the attempt drove, so the verdict can say which positive
    control it is missing. A landed write or read is NO-GO, an
    unobservable boundary is UNRUN with its evidence, and a bare backend
    refusal is UNRUN too.

    THE REFUSAL ALONE IS NOT PROOF. A backend refusal and an unchanged
    snapshot say only that nothing was observed, not that the boundary
    would have refused an attempt that reached it: the refusal may have
    come from a path that never touched the source, and a write may still
    land behind it. A passing answer needs a working positive control of
    the named raw path in this same live session, plus a qualified
    submission and terminal fence showing nothing is queued or in flight
    behind the refusal. Neither real adapter supplies both today, so this
    probe has no passing answer yet.

    The bytes decide before the refusal does. A denial is evidence of
    safety only while the source window holds its contents: a refusal
    that also landed is a landed mutation wearing a refusal's name, and
    it is NO-GO.

    This probe asks about an address OUTSIDE the registration. The
    question of the source window itself is
    probe_source_window_write's, and it is judged by different rules.
    """
    control = _positive_control(operation)
    seen = _observe_raw_attempt(api, agent, registration, buffer, attempt)
    if seen.changed or seen.landed:
        evidence = [f"attempt={seen.name}"]
        evidence.append(
            "source-bytes-changed" if seen.changed else "out-of-bounds-read-landed"
        )
        if seen.denied is not None:
            evidence.append("denied-but-landed")
        return SafetyVerdict(
            capability="raw-access",
            status=NO_GO,
            detail=(
                "the attempt was reported refused but it landed: a refusal "
                "that still lands is a landed mutation, not enforcement"
                if seen.denied is not None
                else "the out-of-bounds attempt landed: the boundary does not isolate "
                "the source buffer"
            ),
            enforcement="",
            evidence=tuple(evidence),
        )
    if seen.denied is not None:
        if seen.denied.enforcement == ENFORCED_BY_BACKEND:
            return SafetyVerdict(
                capability="raw-access",
                status=UNRUN,
                detail=(
                    "the backend refused the attempt and the bytes are unchanged, "
                    f"but no working positive control of {control} ran in this same "
                    "session and no qualified submission and terminal fence was "
                    "observed, so the refusal does not prove the boundary refused "
                    "that access"
                ),
                enforcement=seen.denied.enforcement,
                evidence=(
                    f"attempt={seen.name}",
                    f"operation={operation}",
                    "denied-by-backend",
                    "bytes-unchanged",
                    "no-positive-control",
                    "no-terminal-fence",
                ),
            )
        return SafetyVerdict(
            capability="raw-access",
            status=NO_GO,
            detail=seen.denied.detail,
            enforcement=seen.denied.enforcement,
            evidence=(f"attempt={seen.name}", "wrapper-only-refusal"),
        )
    return SafetyVerdict(
        capability="raw-access",
        status=UNRUN,
        detail=(
            "the boundary neither denied the attempt nor observably landed it "
            "within the probe's observation; the boundary is unqualified, not safe"
        ),
        enforcement="",
        evidence=(
            f"attempt={seen.name}",
            f"operation={operation}",
            "snapshot-unchanged",
        ),
    )


def probe_source_window_write(
    api, agent, registration, buffer, attempt
) -> SafetyVerdict:
    """Probe a peer's WRITE into the registered source window.

    The out-of-bounds probe asks whether the boundary refuses an address
    outside the registration. This one asks the question the spec asks:
    whether a peer can change the source weights inside the window the
    source published.

    A REFUSAL ALONE IS NOT PROOF, so this probe has no passing answer
    yet. A passing answer needs two things neither real adapter supplies
    today:

    - a working positive control of the raw WRITE path, run in this same
      live session, so the refusal is known to come from the path that
      would have written the weights;
    - a qualified submission and terminal fence, so no write is already
      queued or in flight behind the refusal.

    Without them a backend refusal leaves the source window's immutability
    UNPROVEN, and the verdict is UNRUN: the bytes are unchanged, but the
    refusal could have come from a path that never reached the weights,
    and a write could still land behind it. A landed write stays NO-GO, a
    wrapper-only refusal stays NO-GO, and an unobservable boundary stays
    UNRUN.

    A future instrumented run that supplies both may pass this capability.
    That harness is out of scope here, and nothing in this probe treats a
    refusal as a pass on the way to it.
    """
    seen = _observe_raw_attempt(api, agent, registration, buffer, attempt)
    if seen.changed or seen.landed:
        evidence = [f"attempt={seen.name}"]
        evidence.append(
            "source-bytes-changed" if seen.changed else "write-reported-landed"
        )
        if seen.denied is not None:
            evidence.append("denied-but-landed")
        return SafetyVerdict(
            capability="source-window-write",
            status=NO_GO,
            detail=(
                "the attempt was reported refused but it landed: a refusal "
                "that still lands is a landed mutation, not enforcement"
                if seen.denied is not None
                else "the write into the registered source window landed: a peer can "
                "change the source weights"
            ),
            enforcement="",
            evidence=tuple(evidence),
        )
    if seen.denied is not None:
        if seen.denied.enforcement == ENFORCED_BY_BACKEND:
            return SafetyVerdict(
                capability="source-window-write",
                status=UNRUN,
                detail=(
                    "the backend refused the write into the source window and the "
                    "bytes are unchanged, but no working positive control of the raw "
                    "WRITE path ran in this session and no qualified submission and "
                    "terminal fence was observed, so the refusal does not prove the "
                    "source window is immutable"
                ),
                enforcement=seen.denied.enforcement,
                evidence=(
                    f"attempt={seen.name}",
                    "denied-by-backend",
                    "bytes-unchanged",
                    "no-positive-control",
                    "no-terminal-fence",
                ),
            )
        return SafetyVerdict(
            capability="source-window-write",
            status=NO_GO,
            detail=seen.denied.detail,
            enforcement=seen.denied.enforcement,
            evidence=(f"attempt={seen.name}", "wrapper-only-refusal"),
        )
    return SafetyVerdict(
        capability="source-window-write",
        status=UNRUN,
        detail=(
            "the source window's bytes are unchanged, which proves neither "
            "that the backend refused the write nor that a later write will "
            "not land; the source window's immutability is unproven"
        ),
        enforcement="",
        evidence=(f"attempt={seen.name}", "snapshot-unchanged"),
    )


def probe_stale_access_after_revoke(
    api,
    agent,
    registration,
    buffer,
    attempt,
    *,
    operation: str,
    observe_seconds=STALE_OBSERVATION_SECONDS,
) -> SafetyVerdict:
    """Probe access after the registration was revoked.

    operation names the raw path the access drove, so the verdict can say
    which positive control it is missing. A snapshot that stays unchanged
    for the bounded window proves neither revocation nor the absence of a
    late write, so it is UNRUN with its evidence. A bare backend refusal
    is UNRUN too, for the same reason the raw probe's is: the refusal and
    the unchanged bytes say only that nothing was observed. A passing
    answer needs a working positive control of the named raw path in this
    same session and a qualified fence showing the revocation and the
    replay are both terminal.

    THE REGISTRATION AND THE BUFFER BELOW ARE LOCAL. The revocation here
    is not a qualified fence, and a native hook that submits or
    ambiguously fails could still have a transfer in flight when this
    function returns. Heap lifetime, process exit and a Python exception
    are not fences. Nothing here proves the ownership hazard away.

    The bytes decide before the refusal does: a denial that also landed
    is a landed mutation wearing a refusal's name, and it is NO-GO.
    """
    control = _positive_control(operation)
    before = bytes(buffer)
    api.revoke_registration(agent, registration)
    result = attempt(api, agent, registration)
    time.sleep(observe_seconds)
    after = bytes(buffer)
    denied = _denied(result)
    changed = after != before
    if changed or getattr(result, "landed", False):
        evidence = ["registration-revoked"]
        evidence.append("source-bytes-changed" if changed else "access-landed")
        if denied is not None:
            evidence.append("denied-but-landed")
        return SafetyVerdict(
            capability="stale-access",
            status=NO_GO,
            detail=(
                "the revoked registration was reported refused but the "
                "access landed: a refusal that still lands is a landed "
                "mutation, not enforcement"
                if denied is not None
                else "a revoked registration still admitted an access that "
                "landed on the source"
            ),
            enforcement="",
            evidence=tuple(evidence),
        )
    if denied is not None:
        if denied.enforcement == ENFORCED_BY_BACKEND:
            return SafetyVerdict(
                capability="stale-access",
                status=UNRUN,
                detail=(
                    "the backend refused the revoked registration and the bytes "
                    f"are unchanged, but no working positive control of {control} "
                    "ran in this same session and no qualified fence showed the "
                    "revocation and the replay are terminal, so the refusal does "
                    "not prove the stale handle was revoked"
                ),
                enforcement=denied.enforcement,
                evidence=(
                    "registration-revoked",
                    f"operation={operation}",
                    "denied-by-backend",
                    "bytes-unchanged",
                    "no-positive-control",
                    "no-terminal-fence",
                ),
            )
        return SafetyVerdict(
            capability="stale-access",
            status=NO_GO,
            detail=denied.detail,
            enforcement=denied.enforcement,
            evidence=("registration-revoked", "wrapper-only-refusal"),
        )
    return SafetyVerdict(
        capability="stale-access",
        status=UNRUN,
        detail="the bounded window observed no change, which proves neither "
        "revocation nor the absence of a late write",
        enforcement="",
        evidence=(
            "registration-revoked",
            f"observed-{observe_seconds}s",
            "snapshot-unchanged",
        ),
    )


def audit_credential_disclosure(records) -> SafetyVerdict:
    """Audit what connection setup and rendezvous disclose.

    records are the api's own disclosure records: each carries the fields
    it would put on the wire (scope "wire") or keep inside the trusted
    process (scope "internal"). A raw key or a raw pointer in a
    wire-scoped record is NO-GO: an earlier raw-key probe changed source
    bytes with exactly such a disclosure. Records that stay internal are
    noted as evidence, not violations.

    An empty set of records is UNRUN, not PASS: there was no record to
    check, so nothing was audited. An empty audit is not a clean audit.
    """
    records = list(records)
    if not records:
        return SafetyVerdict(
            capability="credential-disclosure",
            status=UNRUN,
            detail="no disclosure records were returned, so nothing was "
            "audited; an empty audit is not a clean audit",
            evidence=("no-records-returned",),
        )
    violations = []
    notes = []
    for record in records:
        scope = getattr(record, "scope", "wire")
        content = getattr(record, "content", {})
        sensitive = sorted(
            key
            for key in content
            if getattr(record, "is_sensitive", lambda _k: True)(key)
        )
        if scope == "wire" and sensitive:
            violations.append((getattr(record, "name", "record"), tuple(sensitive)))
        elif sensitive:
            notes.append((getattr(record, "name", "record"), tuple(sensitive)))
    if violations:
        return SafetyVerdict(
            capability="credential-disclosure",
            status=NO_GO,
            detail="connection setup or rendezvous places raw keys or "
            "pointers on the wire: "
            + "; ".join(f"{name}: {keys}" for name, keys in violations),
            enforcement="",
            evidence=tuple(
                f"violation:{name}:{','.join(keys)}" for name, keys in violations
            ),
        )
    return SafetyVerdict(
        capability="credential-disclosure",
        status=PASS,
        detail="no raw key or pointer crosses the wire in connection setup "
        "or rendezvous"
        + (
            "; sensitive fields kept internal: "
            + "; ".join(f"{name}: {keys}" for name, keys in notes)
            if notes
            else ""
        ),
        enforcement="",
        evidence=tuple(f"internal:{name}:{','.join(keys)}" for name, keys in notes),
    )


def check_pre_submission_fault(reader, file_path: str, span: tuple) -> SafetyVerdict:
    """A failure BEFORE submission must be cleanly recoverable.

    The operation reaches FAILED, its own terminal state; the window slot
    is released; the reader keeps serving. Nothing is quarantined by a
    refusal that never reached the transport.
    """
    try:
        reader.read_bytes(file_path, span[0], span[1])
    except TransferRefusedError as exc:
        detail = str(exc)
    else:
        return SafetyVerdict(
            capability="pre-submission-fault",
            status=NO_GO,
            detail="a configured pre-submission refusal was not raised",
            enforcement="",
            evidence=("reader-continued",),
        )
    operations = list(reader.operations.values())
    if not operations:
        return SafetyVerdict(
            capability="pre-submission-fault",
            status=NO_GO,
            detail="the refusal produced no tracked operation",
            enforcement="",
            evidence=(),
        )
    last = operations[-1]
    if last.state != OpState.FAILED:
        return SafetyVerdict(
            capability="pre-submission-fault",
            status=NO_GO,
            detail=f"the refused operation ended {last.state.value}, not "
            "FAILED with its slot released",
            enforcement="",
            evidence=(f"state={last.state.value}",),
        )
    return SafetyVerdict(
        capability="pre-submission-fault",
        status=PASS,
        detail=f"the pre-submission refusal failed cleanly: {detail}",
        enforcement="",
        evidence=(f"state={last.state.value}", "slot-released"),
    )


def check_post_submission_quarantine(
    reader, file_path: str, span: tuple
) -> SafetyVerdict:
    """A submitted operation that stays nonterminal must be UNKNOWN and
    retained: its window slot held, close refused, and only a qualified
    fence releasing it."""
    from gpustack_model_transfer.core.errors import (
        AmbiguousReadError,
        OperationStateError,
    )

    try:
        reader.read_bytes(file_path, span[0], span[1])
    except AmbiguousReadError:
        pass
    else:
        return SafetyVerdict(
            capability="post-submission-quarantine",
            status=NO_GO,
            detail="a configured nonterminal submission completed instead "
            "of being retained",
            enforcement="",
            evidence=("reader-returned",),
        )
    retained = [op for op in reader.operations.values() if op.state == OpState.UNKNOWN]
    if not retained:
        return SafetyVerdict(
            capability="post-submission-quarantine",
            status=NO_GO,
            detail="no operation was retained as UNKNOWN",
            enforcement="",
            evidence=(),
        )
    try:
        reader.close()
    except OperationStateError:
        pass
    else:
        return SafetyVerdict(
            capability="post-submission-quarantine",
            status=NO_GO,
            detail="the reader closed over a retained UNKNOWN operation",
            enforcement="",
            evidence=("close-succeeded",),
        )
    op = retained[-1]
    # The qualified fence: the backend resolved the operation out of band.
    reader.resolve(
        op.op_id,
        "failed",
        error_code="fence-confirmed",
        external_fence="injected-runtime test fence",
    )
    reader.close()
    return SafetyVerdict(
        capability="post-submission-quarantine",
        status=PASS,
        detail="the nonterminal operation was retained as UNKNOWN with its "
        "window held, close refused, and the fence released it",
        enforcement="",
        evidence=("state=unknown", "close-refused", "fence-released"),
    )


__all__ = [
    "ATOMIC",
    "ENFORCED_BY_BACKEND",
    "ENFORCED_BY_WRAPPER",
    "NO_GO",
    "PASS",
    "RAW_READ",
    "RAW_WRITE",
    "UNRUN",
    "SafetyReport",
    "SafetyVerdict",
    "audit_credential_disclosure",
    "check_post_submission_quarantine",
    "check_pre_submission_fault",
    "probe_raw_access",
    "probe_source_window_write",
    "probe_stale_access_after_revoke",
]
