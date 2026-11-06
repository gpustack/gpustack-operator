"""The NIXL safety suite: source protection and failure fences.

Every capability runs against disposable owned buffers through the
runtime's own boundary. A wrapper-only refusal is NO-GO, a landed
out-of-bounds access is NO-GO (the earlier raw-key probe changed source
bytes — that outcome is a refusal of the boundary, not a footnote), and a
bounded observation that saw nothing is UNRUN with its evidence, because
an unchanged snapshot proves neither revocation nor the absence of a late
write. Without a runtime every capability is UNRUN: a missing probe fails
the run, never a skipped pass.

The source window's immutability is UNRUN even when the backend refuses
the write: without a positive WRITE control and the submission/terminal
fence evidence, a refusal is not proof.
"""

from gpustack_model_transfer.acceptance.report import CheckResult
from gpustack_model_transfer.backends.nixl.runtime import probe_runtime

CAPABILITIES = (
    "raw-write-isolation",
    "raw-read-isolation",
    "atomic-isolation",
    "source-window-write-isolation",
    "stale-access-bounds",
    "credential-disclosure",
    "pre-submission-fault-release",
    "post-submission-unknown-quarantine",
)


def run(context, runtime=None) -> list:
    """Run every safety capability; see the module docstring for the
    verdict rules."""
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
        from gpustack_model_transfer.backends.nixl.runtime import load_runtime

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
        results.append(_run_check(capability, check, context, factory()))
    return results


def _run_check(capability, check, context, api) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip.

    A runtime that lacks a hook the check needs is UNRUN — an unexposed
    boundary — and the guards above make that explicit; this fallback
    keeps any hook they miss honest too.
    """
    try:
        verdict = check(context, api)
        return CheckResult(capability, verdict.status, verdict.detail)
    except AttributeError as exc:
        return CheckResult(
            capability,
            "UNRUN",
            f"the runtime does not expose this boundary in this environment: {exc}",
        )
    except NotImplementedError as exc:
        return CheckResult(
            capability, "UNRUN", f"the runtime does not expose this hook: {exc}"
        )
    except Exception as exc:
        return CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")


_RAW_HITCHES = (
    ("raw-write-isolation", "raw_write_out_of_bounds"),
    ("raw-read-isolation", "raw_read_out_of_bounds"),
    ("atomic-isolation", "raw_atomic_on_source"),
)


def _missing_hitch_unrun(capability: str, hitch: str):
    from gpustack_model_transfer.acceptance.report import CheckResult

    return CheckResult(
        capability,
        "UNRUN",
        f"the runtime does not expose the {hitch} fault-injection hook in "
        "this environment; the boundary cannot be probed here",
    )


def _register_disposable_buffer(api, size: int = 64):
    agent = api.create_agent("safety-probe", {"UCX_NET_DEVICES": "mlx5_0:1"})
    buffer = bytearray(b"s" * size)
    registration = api.register_memory(agent, {"source": buffer})
    return agent, buffer, registration


def _check_raw_write_isolation(context, api):
    from gpustack_model_transfer.backends.nixl.safety import probe_raw_access

    if not hasattr(api, "raw_write_out_of_bounds"):
        return _missing_hitch_unrun("raw-write-isolation", "raw_write_out_of_bounds")
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
    from gpustack_model_transfer.backends.nixl.safety import probe_raw_access

    if not hasattr(api, "raw_read_out_of_bounds"):
        return _missing_hitch_unrun("raw-read-isolation", "raw_read_out_of_bounds")
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
    from gpustack_model_transfer.backends.nixl.safety import probe_raw_access

    if not hasattr(api, "raw_atomic_on_source"):
        return _missing_hitch_unrun("atomic-isolation", "raw_atomic_on_source")
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
    from gpustack_model_transfer.backends.nixl.safety import probe_source_window_write

    if not hasattr(api, "raw_write_in_source_window"):
        return _missing_hitch_unrun(
            "source-window-write-isolation", "raw_write_in_source_window"
        )
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_source_window_write(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
    )


def _check_stale_access_bounds(context, api):
    from gpustack_model_transfer.backends.nixl.safety import (
        probe_stale_access_after_revoke,
    )

    if not hasattr(api, "raw_write_out_of_bounds"):
        return _missing_hitch_unrun("stale-access-bounds", "raw_write_out_of_bounds")
    agent, buffer, registration = _register_disposable_buffer(api)
    return probe_stale_access_after_revoke(
        api,
        agent,
        registration,
        buffer,
        lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
        operation="raw-write",
    )


def _check_credential_disclosure(context, api):
    from gpustack_model_transfer.backends.nixl.safety import audit_credential_disclosure

    if not hasattr(api, "describe_connection"):
        from gpustack_model_transfer.acceptance.report import CheckResult

        return CheckResult(
            "credential-disclosure",
            "UNRUN",
            "the runtime does not expose its connection and rendezvous "
            "records; disclosure cannot be audited here",
        )
    _agent, _buffer, registration = _register_disposable_buffer(api)
    return audit_credential_disclosure(api.describe_connection(registration))


def _check_pre_submission_fault_release(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.reader import NixlReader
    from gpustack_model_transfer.backends.nixl.safety import check_pre_submission_fault
    from gpustack_model_transfer.backends.nixl.testing import SUBMIT_REFUSE_PRE
    from gpustack_model_transfer.core.budgets import ReadBudgets

    limits = context["config"]["limits"]
    if not hasattr(api, "submit_mode"):
        from gpustack_model_transfer.acceptance.report import CheckResult

        return CheckResult(
            "pre-submission-fault-release",
            "UNRUN",
            "the runtime exposes no submission fault mode; a pre-submission "
            "refusal cannot be injected here",
        )
    api.submit_mode = SUBMIT_REFUSE_PRE
    manifest = context["manifest"]
    tensor = manifest.tensors[0]
    reader = NixlReader(
        manifest,
        api,
        {"source": "peer"},
        ReadBudgets(
            window_bytes=limits["window_bytes"],
            max_inflight=limits["max_inflight"],
            owned_output_bytes=limits["owned_output_bytes"],
        ),
        qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {}),
    )
    span = (tensor.data_begin, min(tensor.data_end, tensor.data_begin + 8))
    return check_pre_submission_fault(reader, tensor.file, span)


def _check_post_submission_unknown_quarantine(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.reader import NixlReader
    from gpustack_model_transfer.backends.nixl.safety import (
        check_post_submission_quarantine,
    )
    from gpustack_model_transfer.backends.nixl.testing import SUBMIT_NONTERMINAL
    from gpustack_model_transfer.core.budgets import ReadBudgets

    limits = context["config"]["limits"]
    if not hasattr(api, "submit_mode"):
        from gpustack_model_transfer.acceptance.report import CheckResult

        return CheckResult(
            "post-submission-unknown-quarantine",
            "UNRUN",
            "the runtime exposes no submission fault mode; a nonterminal "
            "submission cannot be injected here",
        )
    api.submit_mode = SUBMIT_NONTERMINAL
    manifest = context["manifest"]
    tensor = manifest.tensors[0]
    reader = NixlReader(
        manifest,
        api,
        {"source": "peer"},
        ReadBudgets(
            window_bytes=limits["window_bytes"],
            max_inflight=limits["max_inflight"],
            owned_output_bytes=limits["owned_output_bytes"],
        ),
        qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {}),
        poll_deadline_seconds=0.0,
    )
    span = (tensor.data_begin, min(tensor.data_end, tensor.data_begin + 8))
    return check_post_submission_quarantine(reader, tensor.file, span)


_CHECKS = (
    ("raw-write-isolation", _check_raw_write_isolation),
    ("raw-read-isolation", _check_raw_read_isolation),
    ("atomic-isolation", _check_atomic_isolation),
    ("source-window-write-isolation", _check_source_window_write_isolation),
    ("stale-access-bounds", _check_stale_access_bounds),
    ("credential-disclosure", _check_credential_disclosure),
    ("pre-submission-fault-release", _check_pre_submission_fault_release),
    ("post-submission-unknown-quarantine", _check_post_submission_unknown_quarantine),
)


__all__ = ["CAPABILITIES", "run"]
