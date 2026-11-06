"""Source protection and failure fences on the HIXL boundary.

The probes mirror the NIXL safety module's rules and add the two the CANN
route owes: the relaxed-fabric ordering is only as safe as its evidence,
and the stale-replay scenario that timed out before submission proves
nothing about reclamation — so recovery stays disabled, and managed CANN
promotion stays blocked, until a qualified fence harness substantiates
a reclaim rule.

A wrapper-only refusal is NO-GO, a landed access is NO-GO, and an
unobservable boundary is UNRUN with its evidence. A pre-submission
timeout is cleanly recoverable; a submitted nonterminal failure is
UNKNOWN and retained until a qualified fence resolves it.

The source window's immutability has no passing answer yet. A backend
refusal of an in-window write leaves it UNRUN, because a refusal proves
nothing without a working positive WRITE control in the same live session
and without the qualified submission and terminal fence that would show
no write is queued or in flight behind it.

The same rule governs every other bare refusal here. The raw access
probe and the stale replay probe leave a backend refusal UNRUN, because
a refusal and an unchanged snapshot say only that nothing was observed.
Each one needs an operation-specific positive control in the same live
session before its refusal could prove anything.
"""

import time
from dataclasses import dataclass

from gpustack_model_transfer.backends.hixl.reader import HixlReader
from gpustack_model_transfer.backends.hixl.session import RecoveryCapability
from gpustack_model_transfer.core.errors import AmbiguousReadError, OperationStateError
from gpustack_model_transfer.core.operation import OpState

PASS = "PASS"
NO_GO = "NO-GO"
UNRUN = "UNRUN"

ENFORCED_BY_BACKEND = "backend"
ENFORCED_BY_WRAPPER = "wrapper"

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
    """One safety capability's result, with its evidence attached."""

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

    verdicts: list

    def __init__(self):
        self.verdicts = []
        self.recovery = RecoveryCapability()

    def add(self, verdict: SafetyVerdict) -> SafetyVerdict:
        verdict.validate()
        self.verdicts.append(verdict)
        # Every boundary that did not PASS is a blocker by name: an
        # UNRUN boundary leaves the reclaim rule unproven and a NO-GO
        # one disproves the safety it rests on; recovery cannot stand on
        # either.
        if verdict.status in (UNRUN, NO_GO):
            self.recovery.record_blocker(f"{verdict.capability}: {verdict.detail}")
        return verdict

    def by_status(self, status: str) -> list:
        return [v for v in self.verdicts if v.status == status]


def _denied(result) -> SafetyVerdict | None:
    """Record one denied attempt, or None when it was not denied.

    The helper says who refused and nothing more. A backend refusal is
    recorded as UNRUN, never PASS: whether a refusal proves anything
    depends on the probe's own evidence rules, and no probe has the
    evidence a bare refusal would need.
    """
    if result is None or not getattr(result, "denied", False):
        return None
    enforcement = getattr(result, "enforcement", "")
    detail = getattr(result, "detail", str(result))
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
    """Probe one raw out-of-bounds access into a disposable buffer.

    attempt is the api's raw escape hatch; the probe judges only what the
    boundary did. operation names which raw path the attempt drove, so
    the verdict can say which positive control it is missing. A landed
    write or an exfiltrated read is NO-GO, an unobservable boundary is
    UNRUN, and a bare backend refusal is UNRUN too.

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
                else "the out-of-bounds attempt landed: the boundary does not "
                "isolate the source window"
            ),
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
            "within the probe's observation; unqualified, not safe"
        ),
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
        evidence=(
            f"attempt={seen.name}",
            "snapshot-unchanged",
        ),
    )


def probe_stale_replay_after_repair(
    api,
    agent,
    registration,
    buffer,
    attempt,
    *,
    operation: str,
    observe_seconds: float = STALE_OBSERVATION_SECONDS,
) -> SafetyVerdict:
    """Probe a stale registration handle after a full repair cycle.

    The repaired cleanup order the PoC missed: the old generation is
    deregistered, a NEW generation buffer takes its place through a
    fresh registration, and only then does a replay of the STALE handle
    run. operation names the raw path the replay drove, so the verdict
    can say which positive control it is missing. A landed access in
    either generation is NO-GO (the new generation's bytes changing proves
    the stale handle reached memory the repair said was replaced); an
    unchanged window proves neither revocation nor the absence of a late
    write, so it is UNRUN with its evidence.

    A bare backend refusal is UNRUN too, for the same reason the raw
    probe's is: the refusal and the unchanged bytes say only that
    nothing was observed. A passing answer needs a working positive
    control of the named raw path in this same session and a qualified
    fence showing the deregistration and the replay are both terminal.

    THE REGISTRATION AND THE BUFFER BELOW ARE LOCAL. The deregistration
    here is not a qualified fence, and a native hook that submits or
    ambiguously fails could still have a transfer in flight when this
    function returns. Heap lifetime, process exit and a Python exception
    are not fences. Nothing here proves the ownership hazard away.

    The bytes decide before the refusal does: a denial that also landed
    is a landed mutation wearing a refusal's name, and it is NO-GO.
    """
    control = _positive_control(operation)
    old_before = bytes(buffer)
    api.deregister(agent, registration)
    new_buffer = bytearray(old_before)
    api.register_memory(agent, {"source": new_buffer})
    new_before = bytes(new_buffer)
    result = attempt(api, agent, registration)
    time.sleep(observe_seconds)
    old_after = bytes(buffer)
    new_after = bytes(new_buffer)
    denied = _denied(result)
    landed_evidence = []
    if old_after != old_before:
        landed_evidence.append("old-generation-bytes-changed")
    if new_after != new_before:
        landed_evidence.append("new-generation-bytes-changed")
    if landed_evidence or getattr(result, "landed", False):
        evidence = ["stale-handle-replayed-after-repair", *landed_evidence]
        if denied is not None:
            evidence.append("denied-but-landed")
        return SafetyVerdict(
            capability="stale-replay",
            status=NO_GO,
            detail=(
                "the stale handle was reported refused but the replay "
                "landed: a refusal that still lands is a landed mutation, "
                "not enforcement"
                if denied is not None
                else "a stale handle admitted an access after the repair "
                "replaced the registration"
            ),
            evidence=tuple(evidence),
        )
    if denied is not None:
        if denied.enforcement == ENFORCED_BY_BACKEND:
            return SafetyVerdict(
                capability="stale-replay",
                status=UNRUN,
                detail=(
                    "the backend refused the stale handle and both generations are "
                    f"unchanged, but no working positive control of {control} ran in "
                    "this same session and no qualified fence showed the "
                    "deregistration and the replay are terminal, so the refusal "
                    "does not prove the stale handle was revoked"
                ),
                enforcement=denied.enforcement,
                evidence=(
                    "stale-handle-replayed-after-repair",
                    f"operation={operation}",
                    "denied-by-backend",
                    "both-generations-unchanged",
                    "no-positive-control",
                    "no-terminal-fence",
                ),
            )
        return SafetyVerdict(
            capability="stale-replay",
            status=NO_GO,
            detail=denied.detail,
            enforcement=denied.enforcement,
            evidence=("stale-handle-replayed-after-repair", "wrapper-only-refusal"),
        )
    return SafetyVerdict(
        capability="stale-replay",
        status=UNRUN,
        detail="the bounded window observed no change in either "
        "generation, which proves neither revocation nor the absence "
        "of a late write; the replay state is repaired but the "
        "reclamation rule is still unproven",
        evidence=(
            "stale-handle-replayed-after-repair",
            f"observed-{observe_seconds}s",
            "both-generations-unchanged",
        ),
    )


def audit_credential_disclosure(records) -> SafetyVerdict:
    """Audit what connection setup and rendezvous would emit.

    A raw key or pointer in a wire-scoped record is NO-GO; sensitive
    fields that stay internal to the trusted process are noted, not
    hidden.

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
            if getattr(record, "is_sensitive", lambda _key: True)(key)
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
        evidence=tuple(f"internal:{name}" for name, _keys in notes),
    )


def check_pre_submission_timeout(
    reader: HixlReader, file_path: str, span: tuple
) -> SafetyVerdict:
    """A timeout BEFORE submission must fail cleanly and release.

    The pre-submission timeout tests a different boundary from a
    submitted operation with an observed nonterminal state; this probe is
    the former, and it must leave the reader serving.
    """
    try:
        reader.read_bytes(file_path, span[0], span[1])
    except Exception as exc:
        flights = list(reader.in_flight.values())
        if not flights:
            return SafetyVerdict(
                capability="pre-submission-timeout",
                status=NO_GO,
                detail="the refusal produced no tracked operation",
                evidence=(f"exception={type(exc).__name__}",),
            )
        last = flights[-1].operation
        if last.state != OpState.FAILED:
            return SafetyVerdict(
                capability="pre-submission-timeout",
                status=NO_GO,
                detail=f"the refused operation ended {last.state.value}, not "
                "FAILED with its slot released",
                evidence=(f"state={last.state.value}",),
            )
        return SafetyVerdict(
            capability="pre-submission-timeout",
            status=PASS,
            detail=f"the pre-submission timeout failed cleanly: {exc}",
            evidence=(
                f"state={last.state.value}",
                "slot-released",
                "bytes-deactivated",
            ),
        )
    return SafetyVerdict(
        capability="pre-submission-timeout",
        status=NO_GO,
        detail="a configured pre-submission timeout completed instead of being refused",
        evidence=("reader-returned",),
    )


def check_submitted_nonterminal_quarantine(
    reader: HixlReader, file_path: str, span: tuple
) -> SafetyVerdict:
    """A submitted operation stuck nonterminal must be UNKNOWN, retained,
    and released only by a qualified fence.

    The stale-replay rule: every operation tracks its own terminal state,
    and a prior success never makes this timeout eligible for normal
    cleanup — the reader keeps serving only through other slots, and
    close() refuses until the fence resolves.
    """
    try:
        reader.read_bytes(file_path, span[0], span[1])
    except AmbiguousReadError:
        pass
    else:
        return SafetyVerdict(
            capability="submitted-nonterminal-quarantine",
            status=NO_GO,
            detail="a configured nonterminal submission completed instead "
            "of being retained",
            evidence=("reader-returned",),
        )
    retained = [
        flight
        for flight in reader.in_flight.values()
        if flight.operation.state == OpState.UNKNOWN
    ]
    if not retained:
        return SafetyVerdict(
            capability="submitted-nonterminal-quarantine",
            status=NO_GO,
            detail="no operation was retained as UNKNOWN",
            evidence=(),
        )
    flight = retained[-1]
    # Its bytes are still active: the quarantine holds them.
    if flight._resolved:
        return SafetyVerdict(
            capability="submitted-nonterminal-quarantine",
            status=NO_GO,
            detail="the retained operation's bytes were released before "
            "any fence resolved it",
            evidence=("bytes-released-early",),
        )
    try:
        reader.close()
    except OperationStateError:
        pass
    else:
        return SafetyVerdict(
            capability="submitted-nonterminal-quarantine",
            status=NO_GO,
            detail="the reader closed over a retained UNKNOWN operation",
            evidence=("close-succeeded",),
        )
    reader.resolve(flight.operation.op_id, "failed", error_code="fence-confirmed")
    reader.close()
    return SafetyVerdict(
        capability="submitted-nonterminal-quarantine",
        status=PASS,
        detail="the nonterminal operation was retained as UNKNOWN with its "
        "window and bytes held, close refused, and the fence resolved and "
        "released it",
        evidence=(
            "state=unknown",
            "bytes-held",
            "close-refused",
            "fence-released",
        ),
    )


def check_recovery_standing(report: SafetyReport) -> SafetyVerdict:
    """The recovery capability's standing, from its own record.

    A NO-GO boundary anywhere blocks recovery outright: no reclaim rule
    can stand on a boundary that failed. An UNRUN boundary leaves the
    rule unproven — UNRUN, with the blockers named.

    There is no PASS standing yet. A reclaim rule needs a qualified fence
    that retains both endpoints until it resolves the retained operation,
    and no such harness exists. A recorded proof and a verdict's marker
    are not the observation, so neither can enable recovery: the standing
    stays UNRUN, and managed CANN promotion stays blocked, until a real
    fence harness substantiates the rule.
    """
    no_go = [v for v in report.verdicts if v.status == NO_GO]
    if no_go:
        return SafetyVerdict(
            capability="recovery-capability",
            status=NO_GO,
            detail="a NO-GO boundary blocks recovery: "
            + "; ".join(f"{v.capability}: {v.detail}" for v in no_go),
            enforcement="",
            evidence=tuple(f"no-go:{v.capability}" for v in no_go),
        )
    unobserved = [v for v in report.verdicts if v.status == UNRUN]
    for verdict in unobserved:
        report.recovery.record_blocker(f"{verdict.capability}: {verdict.detail}")
    if report.recovery.enabled:
        # A recorded rule is not a qualified fence. The standing refuses to
        # inherit a rule that no fence harness substantiated.
        report.recovery.record_blocker(
            "the reclaim rule has no qualified fence harness behind it"
        )
        report.recovery.enabled = False
    return SafetyVerdict(
        capability="recovery-capability",
        status=UNRUN,
        detail="no qualified reclaim rule is proven; the fence harness that "
        "would substantiate it does not exist, so recovery stays disabled and "
        "managed CANN promotion stays blocked",
        evidence=tuple(f"blocker:{b}" for b in report.recovery.blockers)
        or ("no-proof",),
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
    "check_pre_submission_timeout",
    "check_recovery_standing",
    "check_submitted_nonterminal_quarantine",
    "probe_raw_access",
    "probe_source_window_write",
    "probe_stale_replay_after_repair",
]
