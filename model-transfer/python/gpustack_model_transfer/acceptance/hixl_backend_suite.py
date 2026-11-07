"""The HIXL backend suite: per-capability results, fail-closed.

Without a runtime — the real acceptance run on a host without the CANN
stack — every capability reports UNRUN and the run is NO-GO: missing
prerequisites fail the run, never a skipped pass. With a runtime injected
(the fake in tests, a qualified host wrapper on the approved CANN pair),
every capability runs and reports PASS or NO-GO on its own merits.
"""

from gpustack_model_transfer.acceptance.report import CheckResult
from gpustack_model_transfer.backends.hixl.runtime import probe_runtime

CAPABILITIES = (
    "runtime-availability",
    "transport-negotiation",
    "session-order",
    "source-context",
    "fixed-window-reuse",
    "per-operation-terminal-states",
    "memory-accounting",
    "registration-in-consuming-process",
    "read-direction-available",
)

# The poll deadline the suite's readers use, bounded so a hanging check
# fails in seconds instead of minutes wherever it runs.
POLL_DEADLINE_SECONDS = 1.0


def run(context, runtime=None) -> list:
    """Run every backend capability; see the module docstring."""
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
        results.append(_run_check(capability, check, context, factory()))
    return results


def _run_check(capability, check, context, api) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip.

    A runtime that lacks a hook the check needs is UNRUN, and the guards
    inside the checks make that explicit through hasattr and the
    NotImplementedError the seam raises for host-injected hooks. An
    AttributeError from a broken runtime call is a bug in the boundary,
    so it lands as NO-GO here, never as an unexposed-boundary UNRUN.
    """
    had_mode = hasattr(api, "submit_mode")
    previous_mode = getattr(api, "submit_mode", None)
    try:
        detail = check(context, api)
        if isinstance(detail, CheckResult):
            detail.capability = capability
            return detail
        return CheckResult(capability, "PASS", detail)
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


def _compat_unrun(name: str, why: str) -> CheckResult:
    """An UNRUN placeholder the run loop retags with the capability."""
    return CheckResult(name, "UNRUN", why)


def _publish_remote_source(context, api) -> dict:
    """Publish the fixture's checkpoint bytes as remote regions.

    The source side registers its files and publishes their remote base
    addresses; the reader submits those addresses. Deterministic bases
    per file keep the map reproducible for the expected-bytes checks.
    """
    import os

    if not hasattr(api, "publish_remote_region"):
        # A runtime that cannot publish test regions is a host wrapper
        # question, not a silent skip: the reader will refuse transfers
        # with the missing-publication reason named.
        return _peer({})
    manifest = context["manifest"]
    regions = {}
    for index, descriptor in enumerate(manifest.files):
        blob_path = os.path.join(
            os.path.dirname(context["manifest_path"]), descriptor.path
        )
        with open(blob_path, "rb") as fh:
            api.publish_remote_region(_region_base(index), fh.read())
        regions[descriptor.path] = {
            "addr": _region_base(index),
            "len": descriptor.size,
        }
    return _peer(regions)


def _region_base(index: int) -> int:
    return 0x500000000000 * (index + 1)


def _file_bytes(context, file_path: str) -> bytes:
    import os

    blob_path = os.path.join(os.path.dirname(context["manifest_path"]), file_path)
    with open(blob_path, "rb") as fh:
        return fh.read()


def _peer(regions: dict) -> dict:
    return {"endpoint": "hixl://source:9700", "regions": regions}


def _open_reader(context, api, ledger=None, transport_options=(), **kwargs):
    from gpustack_model_transfer.backends.hixl import HixlReader
    from gpustack_model_transfer.core.budgets import ReadBudgets

    limits = dict(context["config"]["limits"])
    limits.update(kwargs)
    budgets = ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )
    peer = _publish_remote_source(context, api)
    return HixlReader(
        context["manifest"],
        api,
        peer,
        budgets,
        poll_deadline_seconds=POLL_DEADLINE_SECONDS,
        ledger=ledger,
        transport_options=tuple(transport_options),
    )


def _check_runtime_availability(context, api):
    from gpustack_model_transfer.backends.hixl.runtime import load_runtime

    if load_runtime(api=api) is not api:
        raise AssertionError("the supplied runtime was not retained")
    return "runtime available; version metadata is diagnostic only"


def _check_transport_negotiation(context, api):
    from gpustack_model_transfer.backends.hixl import negotiate_transport

    negotiation = negotiate_transport(
        ("hixl_rdma", "tcp"),
        {"hixl_rdma": "the reviewed Python API refuses transport options", "tcp": True},
    )
    negotiation.validate()
    if negotiation.applied_transport_options != ("tcp",):
        raise AssertionError("the applied options left the negotiation")
    if not negotiation.rejected or negotiation.rejected[0].option != "hixl_rdma":
        raise AssertionError("the rejected option was dropped, not reported")
    if not negotiation.functional_only:
        raise AssertionError(
            "a negotiation with a rejected option claimed the required path"
        )
    # The reader carries its own record: requested options that the
    # binding did not answer are rejections there too, and the reader
    # stays functional-only instead of claiming the required path.
    reader = _open_reader(context, api, transport_options=("hixl_rdma", "tcp"))
    try:
        reader.capabilities.validate()
        record = reader.capabilities.negotiation
        if record.requested_transport_options != ("hixl_rdma", "tcp"):
            raise AssertionError("the reader lost its requested options")
        answered = getattr(reader._session_agent, "applied_transport_options", ())
        if tuple(answered) != record.applied_transport_options:
            raise AssertionError(
                "the reader's applied options are not the binding's answer"
            )
        if not reader.functional_only:
            raise AssertionError("the reader claimed the required transport path")
    finally:
        reader.close()
    return (
        "requested, applied, and effective answers kept apart at the record "
        "and on the reader; the rejected option is reported and the record "
        "stays functional-only until the effective backend is observed"
    )


def _check_session_order(context, api):
    from gpustack_model_transfer.backends.hixl import HixlSession, SessionOrderError

    session = HixlSession(agent=api.create_agent("order-probe", {}))
    registration = api.register_memory(session.agent, {"w": bytearray(64)})
    session.register(registration)
    connection = api.connect(session.agent, {})
    session.connect(connection)
    try:
        session.deregister(registration)
    except SessionOrderError:
        pass
    else:
        raise AssertionError("deregistration before disconnect was accepted")
    session.disconnect(connection)
    session.deregister(registration)
    return "register before connect and disconnect before deregister enforced"


def _check_source_context(context, api):
    from gpustack_model_transfer.backends.hixl import (
        HixlCapabilities,
        negotiate_transport,
    )

    capabilities = HixlCapabilities(
        negotiation=negotiate_transport(("hixl_rdma",), {"hixl_rdma": True})
    )
    capabilities.validate()
    if not capabilities.requires_source_npu_context:
        raise AssertionError(
            "an accelerator-free source is not qualified; the requirement "
            "cannot default to false"
        )
    return (
        "a source NPU context is required by default: the measured source "
        "initialized one, and CPU-only source execution stays unqualified"
    )


def _check_fixed_window_reuse(context, api):
    import hashlib

    reader = _open_reader(context, api)
    try:
        manifest = context["manifest"]
        tensor = max(manifest.tensors, key=lambda t: t.byte_size)
        if tensor.byte_size <= context["config"]["limits"]["window_bytes"]:
            raise AssertionError("the fixture exercises no multi-window tensor")
        first = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        second = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        expected = _file_bytes(context, tensor.file)[
            tensor.data_begin : tensor.data_end
        ]
        if first != expected or second != expected:
            raise AssertionError("the windows did not serve the fixture's bytes")
        if hashlib.sha256(first).hexdigest() != hashlib.sha256(expected).hexdigest():
            raise AssertionError("the served bytes do not digest to the slice's digest")
        short = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_begin + 8)
        if short != expected[:8]:
            raise AssertionError("a short read leaked stale tail bytes")
    finally:
        reader.close()
    return (
        f"a {tensor.byte_size}-byte tensor served byte-exact through "
        f"{context['config']['limits']['window_bytes']}-byte windows, twice, "
        "with no stale tail"
    )


def _check_per_operation_terminal_states(context, api):
    from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError
    from gpustack_model_transfer.backends.hixl.testing import (
        SUBMIT_NONTERMINAL,
        SUBMIT_TIMEOUT_PRE,
    )
    from gpustack_model_transfer.core.errors import (
        AmbiguousReadError,
        OperationStateError,
    )

    if not hasattr(api, "submit_mode"):
        return _compat_unrun(
            "per-operation terminal states",
            "the runtime exposes no submission fault mode; terminal-state "
            "handling can only be qualified where a timeout or a hang is "
            "observable at the boundary",
        )
    manifest = context["manifest"]
    tensor = manifest.tensors[0]
    span = (tensor.data_begin, min(tensor.data_end, tensor.data_begin + 8))

    api.submit_mode = SUBMIT_TIMEOUT_PRE
    reader = _open_reader(context, api)
    try:
        try:
            reader.read_bytes(tensor.file, span[0], span[1])
        except TransferRefusedError as exc:
            flight = list(reader.in_flight.values())[-1]
            if flight.operation.state.value != "failed":
                raise AssertionError(
                    "a pre-submission timeout left no FAILED op"
                ) from exc
            if flight.operation.error_code != "pre-submission":
                raise AssertionError("the error code names the wrong boundary") from exc
            if reader.ledger.active_bytes != 0:
                raise AssertionError("the refused operation kept active bytes") from exc
        else:
            raise AssertionError("the pre-submission timeout was not raised")
    finally:
        reader.close()
    api.submit_mode = SUBMIT_NONTERMINAL
    reader = _open_reader(context, api)
    try:
        try:
            reader.read_bytes(tensor.file, span[0], span[1])
        except AmbiguousReadError as exc:
            retained = [
                f
                for f in reader.in_flight.values()
                if f.operation.state.value == "unknown"
            ]
            if not retained or not retained[-1].operation.retained:
                raise AssertionError(
                    "a nonterminal op was not retained as UNKNOWN"
                ) from exc
            try:
                reader.close()
            except OperationStateError:
                pass
            else:
                raise AssertionError(
                    "the reader closed over a retained UNKNOWN"
                ) from None
            reader.resolve(retained[-1].operation.op_id, "failed", error_code="fence")
            reader.close()
        else:
            raise AssertionError("a nonterminal submission completed")
    finally:
        reader.close()
    return (
        "every operation owns its state: a pre-submission timeout FAILED "
        "and released cleanly; a nonterminal submission stayed UNKNOWN and "
        "retained until a fence resolved it"
    )


def _check_memory_accounting(context, api):
    from gpustack_model_transfer.backends.hixl.memory import MemoryLedger

    try:
        probe_agent = api.create_agent("memory-probe", {})
        api.device_memory_bytes(probe_agent)
    except NotImplementedError:
        return _compat_unrun(
            "memory accounting",
            "the runtime does not expose device-wide NPU memory counters "
            "in this environment",
        )
    ledger = MemoryLedger()
    limits = context["config"]["limits"]
    reader = _open_reader(context, api, ledger=ledger)
    try:
        manifest = context["manifest"]
        tensor = manifest.tensors[0]
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        ledger.observe_device_runtime(api.device_memory_bytes(reader._session_agent))
    finally:
        reader.close()
    if ledger.reserved_bytes != 0:
        raise AssertionError("a closed reader kept its reservation")
    if ledger.peak_bytes <= 0:
        raise AssertionError("no in-flight bytes were ever accounted")
    return (
        f"active/peak accounted ({ledger.peak_bytes} bytes peak against a "
        f"{limits['window_bytes'] * limits['max_inflight']}-byte "
        f"reservation); device-wide NPU runtime bytes "
        f"{ledger.device_runtime_bytes} recorded separately"
    )


def _check_registration_in_consuming_process(context, api):
    reader = _open_reader(context, api)
    try:
        manifest = context["manifest"]
        tensor = manifest.tensors[0]
        payload = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        expected = _file_bytes(context, tensor.file)[
            tensor.data_begin : tensor.data_end
        ]
        if payload != expected:
            raise AssertionError("the consuming-process registration served no bytes")
        # The construction order already proved register-before-connect and
        # close proves disconnect-before-deregister; recorded here as one
        # capability with its evidence.
        registration = reader._registration
        if registration not in reader.session.registrations:
            raise AssertionError("the window registration left the session")
    finally:
        reader.close()
    if reader.session.phase != "deregistered":
        raise AssertionError("close did not complete the documented teardown")
    return (
        "target memory registered in its consuming process, connected "
        "after, and torn down in the documented order"
    )


def _check_read_direction_available(context, api):
    from gpustack_model_transfer.backends.hixl import DIRECTION_READ

    if not hasattr(api, "agents"):
        return _compat_unrun(
            "read direction",
            "the runtime does not record its submissions, so the submitted "
            "direction is not observable here; only the served bytes are",
        )
    reader = _open_reader(context, api)
    try:
        reader._direction = DIRECTION_READ
        manifest = context["manifest"]
        tensor = manifest.tensors[0]
        payload = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        expected = _file_bytes(context, tensor.file)[
            tensor.data_begin : tensor.data_end
        ]
        if payload != expected:
            raise AssertionError("the read direction served no bytes")
        submission = reader._session_agent.submissions[-1]
        if submission["direction"] != "read":
            raise AssertionError("the read direction submitted a write")
        if "addr" not in submission["source"]:
            raise AssertionError("the submission was not address-based")
    finally:
        reader.close()
    return (
        "the target-initiated READ direction is present, submits the "
        "peer's published address, and serves the fixture bytes; its "
        "isolation standing belongs to the safety suite"
    )


_CHECKS = (
    ("runtime-availability", _check_runtime_availability),
    ("transport-negotiation", _check_transport_negotiation),
    ("session-order", _check_session_order),
    ("source-context", _check_source_context),
    ("fixed-window-reuse", _check_fixed_window_reuse),
    ("per-operation-terminal-states", _check_per_operation_terminal_states),
    ("memory-accounting", _check_memory_accounting),
    ("registration-in-consuming-process", _check_registration_in_consuming_process),
    ("read-direction-available", _check_read_direction_available),
)


__all__ = ["CAPABILITIES", "run"]
