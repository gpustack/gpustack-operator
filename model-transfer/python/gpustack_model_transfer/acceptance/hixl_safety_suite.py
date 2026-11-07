"""The HIXL safety suite: source protection and failure fences.

Every capability runs against disposable owned buffers through the
runtime's own boundary: a wrapper-only refusal is NO-GO, a landed access
is NO-GO, an unobservable boundary is UNRUN with its evidence, and the
recovery capability stays UNRUN — with managed CANN promotion blocked —
until a qualified fence harness substantiates a reclaim rule. Without a
runtime every capability is UNRUN: a missing probe fails the run, never a
skipped pass.

The source window's immutability is UNRUN even when the backend refuses
the write: without a positive WRITE control and the submission/terminal
fence evidence, a refusal is not proof. Recovery rests on that boundary,
so it stays UNRUN with it.
"""

from gpustack_model_transfer.acceptance.report import CheckResult
from gpustack_model_transfer.backends.hixl.runtime import probe_runtime

CAPABILITIES = (
    "raw-write-isolation",
    "raw-read-isolation",
    "atomic-isolation",
    "source-window-write-isolation",
    "stale-replay-after-repair",
    "credential-disclosure",
    "pre-submission-timeout",
    "submitted-nonterminal-quarantine",
    "recovery-capability",
)


def run(context, runtime=None) -> list:
    """Run every safety capability; see the module docstring."""
    if runtime is None:
        try:
            readiness = probe_runtime()
        except Exception as exc:
            return [
                CheckResult(c, "UNRUN", f"runtime probe failed: {exc}")
                for c in CAPABILITIES
            ]
        if readiness.missing:
            return [
                CheckResult(
                    capability,
                    "UNRUN",
                    "missing prerequisites: " + ", ".join(readiness.missing),
                )
                for capability in CAPABILITIES
            ]
        from gpustack_model_transfer.backends.hixl.runtime import load_runtime

        try:
            api = load_runtime()
        except Exception as exc:
            return [
                CheckResult(capability, "UNRUN", str(exc))
                for capability in CAPABILITIES
            ]
        factory = lambda: api  # noqa: E731  (one shared real runtime)
    else:
        factory = runtime

    results = []
    for capability, check in _CHECKS:
        if capability == "recovery-capability":
            continue
        results.append(_run_check(capability, check, context, factory()))
    # The recovery standing is decided from THIS run's own verdicts: the
    # reclaim rule enables only when every boundary it rests on passed,
    # and a NO-GO or UNRUN boundary anywhere keeps it disabled.
    results.append(_recovery_result(results))
    return results


# The boundaries a safe reclaim rule rests on: the retained-UNKNOWN
# quarantine it reclaims, the stale replay it must survive, and the
# isolation probes whose boundary it trusts.
_RECOVERY_PREREQUISITES = (
    "raw-write-isolation",
    "raw-read-isolation",
    "atomic-isolation",
    "source-window-write-isolation",
    "stale-replay-after-repair",
    "submitted-nonterminal-quarantine",
)


def _recovery_result(results: list) -> CheckResult:
    """The recovery capability, assembled from the run's own results.

    The run does NOT record a reclaim rule of its own. The rule needs a
    qualified fence that retains both endpoints until it resolves the
    retained operation, and no such harness exists: the standing reads
    the run's boundaries and stays UNRUN until a real fence substantiates
    the rule.
    """
    from gpustack_model_transfer.backends.hixl.safety import (
        SafetyReport,
        SafetyVerdict,
        check_recovery_standing,
    )

    report = SafetyReport()
    for result in results:
        report.add(
            SafetyVerdict(
                capability=result.capability,
                status=result.status,
                detail=result.detail,
            )
        )
    by_capability = {result.capability: result for result in results}
    unmet = [
        name
        for name in _RECOVERY_PREREQUISITES
        if by_capability.get(name) is None or by_capability[name].status != "PASS"
    ]
    verdict = check_recovery_standing(report)
    detail = verdict.detail
    if unmet:
        detail = detail + "; the reclaim rule rests on: " + ", ".join(unmet)
    if report.recovery.blockers:
        # The report carries one detail field; the disabled standing
        # names its blockers there so the run log says what to fix.
        detail = detail + "; blockers: " + "; ".join(report.recovery.blockers)
    return CheckResult("recovery-capability", verdict.status, detail)


def _run_check(capability, check, context, api) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip.

    A runtime that lacks a hook the check needs is UNRUN, and the guards
    inside the checks make that explicit through hasattr. An
    AttributeError from a broken runtime call is a bug in the boundary,
    so it lands as NO-GO here, never as an unexposed-boundary UNRUN.
    """
    had_mode = hasattr(api, "submit_mode")
    previous_mode = getattr(api, "submit_mode", None)
    try:
        verdict = check(context, api)
        return CheckResult(capability, verdict.status, verdict.detail)
    except NotImplementedError as exc:
        return CheckResult(
            capability, "UNRUN", f"the runtime does not expose this hook: {exc}"
        )
    except Exception as exc:
        return CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")

    finally:
        if had_mode:
            api.submit_mode = previous_mode
        elif hasattr(api, "submit_mode"):
            del api.submit_mode


def _register_disposable_buffer(api, size: int = 64):
    agent = api.create_agent("safety-probe", {})
    buffer = bytearray(b"s" * size)
    registration = api.register_memory(agent, {"source": buffer})
    return agent, buffer, registration


def _missing_hook(name: str) -> CheckResult:
    return CheckResult(
        name,
        "UNRUN",
        "the runtime does not expose the fault-injection hook this probe "
        "needs in this environment; the boundary cannot be probed here",
    )


def _check_raw_write_isolation(context, api):
    from gpustack_model_transfer.backends.hixl.safety import probe_raw_access

    if not hasattr(api, "raw_write_out_of_bounds"):
        return _missing_hook("raw-write-isolation")
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_raw_access(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
        operation="raw-write",
    )


def _check_raw_read_isolation(context, api):
    from gpustack_model_transfer.backends.hixl.safety import probe_raw_access

    if not hasattr(api, "raw_read_out_of_bounds"):
        return _missing_hook("raw-read-isolation")
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_raw_access(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_read_out_of_bounds(ag, reg),
        operation="raw-read",
    )


def _check_atomic_isolation(context, api):
    from gpustack_model_transfer.backends.hixl.safety import probe_raw_access

    if not hasattr(api, "raw_atomic_on_source"):
        return _missing_hook("atomic-isolation")
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_raw_access(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_atomic_on_source(ag, reg),
        operation="atomic",
    )


def _check_source_window_write_isolation(context, api):
    """The in-window source WRITE negative.

    The out-of-registration probes ask whether the boundary refuses an
    address outside the registration. This one asks whether a peer can
    change the source weights inside the window the source published. A
    runtime without the hook cannot answer, and that is UNRUN.
    """
    from gpustack_model_transfer.backends.hixl.safety import probe_source_window_write

    if not hasattr(api, "raw_write_in_source_window"):
        return _missing_hook("source-window-write-isolation")
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_source_window_write(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
    )


def _check_stale_replay(context, api):
    from gpustack_model_transfer.backends.hixl.safety import (
        probe_stale_replay_after_repair,
    )

    if not hasattr(api, "raw_write_out_of_bounds"):
        return _missing_hook("stale-replay-after-repair")
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_stale_replay_after_repair(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
        operation="raw-write",
    )


def _check_credential_disclosure(context, api):
    from gpustack_model_transfer.backends.hixl.safety import audit_credential_disclosure

    if not hasattr(api, "describe_connection"):
        return CheckResult(
            "credential-disclosure",
            "UNRUN",
            "the runtime does not expose its connection and rendezvous "
            "records; disclosure cannot be audited here",
        )
    agent, _buffer, _registration = _register_disposable_buffer(api)
    connection = api.connect(agent, {})
    return audit_credential_disclosure(api.describe_connection(connection))


def _open_reader(context, api, poll_deadline_seconds=0.0):
    from gpustack_model_transfer.backends.hixl import HixlReader
    from gpustack_model_transfer.core.budgets import ReadBudgets

    limits = context["config"]["limits"]
    peer = {"endpoint": "hixl://source:9700", "regions": {}}
    if hasattr(api, "publish_remote_region"):
        regions = {}
        import os

        for index, descriptor in enumerate(context["manifest"].files):
            blob_path = os.path.join(
                os.path.dirname(context["manifest_path"]), descriptor.path
            )
            with open(blob_path, "rb") as fh:
                api.publish_remote_region(_region_base(index), fh.read())
            regions[descriptor.path] = {
                "addr": _region_base(index),
                "len": descriptor.size,
            }
        peer = {"endpoint": "hixl://source:9700", "regions": regions}
    return HixlReader(
        context["manifest"],
        api,
        peer,
        ReadBudgets(
            window_bytes=limits["window_bytes"],
            max_inflight=limits["max_inflight"],
            owned_output_bytes=limits["owned_output_bytes"],
        ),
        poll_deadline_seconds=poll_deadline_seconds,
    )


def _region_base(index: int) -> int:
    return 0x500000000000 * (index + 1)


def _first_span(context):
    manifest = context["manifest"]
    tensor = manifest.tensors[0]
    return tensor.file, (
        tensor.data_begin,
        min(tensor.data_end, tensor.data_begin + 8),
    )


def _check_pre_submission_timeout(context, api):
    from gpustack_model_transfer.backends.hixl.safety import (
        check_pre_submission_timeout,
    )
    from gpustack_model_transfer.backends.hixl.testing import SUBMIT_TIMEOUT_PRE

    if not hasattr(api, "submit_mode"):
        return CheckResult(
            "pre-submission-timeout",
            "UNRUN",
            "the runtime exposes no submission fault mode; a pre-submission "
            "timeout cannot be injected here",
        )
    api.submit_mode = SUBMIT_TIMEOUT_PRE
    reader = _open_reader(context, api)
    file_path, span = _first_span(context)
    try:
        return check_pre_submission_timeout(reader, file_path, span)
    finally:
        reader.close()


def _check_submitted_nonterminal(context, api):
    from gpustack_model_transfer.backends.hixl.safety import (
        check_submitted_nonterminal_quarantine,
    )
    from gpustack_model_transfer.backends.hixl.testing import SUBMIT_NONTERMINAL

    if not hasattr(api, "submit_mode"):
        return CheckResult(
            "submitted-nonterminal-quarantine",
            "UNRUN",
            "the runtime exposes no submission fault mode; a nonterminal "
            "submission cannot be injected here",
        )
    api.submit_mode = SUBMIT_NONTERMINAL
    reader = _open_reader(context, api)
    file_path, span = _first_span(context)
    try:
        return check_submitted_nonterminal_quarantine(reader, file_path, span)
    finally:
        reader.close()


_CHECKS = (
    ("raw-write-isolation", _check_raw_write_isolation),
    ("raw-read-isolation", _check_raw_read_isolation),
    ("atomic-isolation", _check_atomic_isolation),
    ("source-window-write-isolation", _check_source_window_write_isolation),
    ("stale-replay-after-repair", _check_stale_replay),
    ("credential-disclosure", _check_credential_disclosure),
    ("pre-submission-timeout", _check_pre_submission_timeout),
    ("submitted-nonterminal-quarantine", _check_submitted_nonterminal),
)


__all__ = ["CAPABILITIES", "run"]
