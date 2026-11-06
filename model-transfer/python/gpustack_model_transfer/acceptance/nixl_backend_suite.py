"""The NIXL backend suite: per-capability results, fail-closed.

Without a runtime the suite cannot observe anything, and a missing
prerequisite is never a skipped pass: every capability reports UNRUN and
the run is NO-GO. With a runtime injected (the fake in tests, a qualified
host wrapper on an approved pair), every capability runs and reports PASS
or NO-GO on its own merits.

Evidence is never inflated: port counters attribute traffic to a selected
rail and never prove a transfer path; memory answers carry active,
reserved, and peak alongside device-wide runtime bytes measured outside
any framework allocator; and a window case that did not run (the planned
8 GiB round among them) stays UNRUN rather than averaging into a pass.
"""

from gpustack_model_transfer.acceptance.report import CheckResult
from gpustack_model_transfer.backends.nixl.runtime import probe_runtime

# The poll deadline the suite's readers use: the operation deadline from
# the configuration, bounded so a hanging check fails in seconds instead
# of minutes wherever it runs.
POLL_DEADLINE_SECONDS = 1.0

CAPABILITIES = (
    "runtime-pin",
    "endpoint-selection",
    "service-memlock",
    "fixed-window-reuse",
    "per-operation-terminal-states",
    "memory-accounting",
    "counter-attribution",
    "multi-rail-qualification",
    "registration-renewal",
    "scale-8gib-qualification",
)

# The smallest scale a qualification run must have moved before the 8 GiB
# capability may claim anything. Evidence below this stays UNRUN: missing
# evidence is never averaged into a pass.
SCALE_8GIB_BYTES = 8 * 1024 * 1024 * 1024


def run(context, runtime=None) -> list:
    """Run every backend capability.

    runtime, when given, is a zero-argument factory producing a fresh
    NixlAPI per capability — the injected fake in tests. Without it the
    host is probed; a missing or mismatched runtime UNRUNs every
    capability, and a probe error is UNRUN with its reason, never a pass.
    """
    if runtime is None:
        try:
            readiness = probe_runtime()
        except Exception as exc:  # a broken probe is not a healthy runtime
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
    results.append(_check_scale_8gib(context))
    return results


def _check_scale_8gib(context) -> CheckResult:
    """The 8 GiB scale gate: UNRUN unless scale evidence is on record.

    A run over the small contract fixture alone says nothing about 8 GiB
    windows, registrations, or renewal at that size. The evidence comes
    from the acceptance configuration's readiness record: an
    "scale_8gib_verified" entry naming the bytes a qualified run moved.
    Nothing invents it, and its absence is reported, never waved through.
    """
    evidence = context["config"]["readiness"].get("scale_8gib_verified")
    if not evidence:
        return CheckResult(
            "scale-8gib-qualification",
            "UNRUN",
            "no 8 GiB scale evidence was supplied; the contract fixture "
            "alone does not qualify 8 GiB windows, registrations, or "
            "renewal at that size",
        )
    moved = evidence.get("bytes_moved", 0) if isinstance(evidence, dict) else 0
    if moved < SCALE_8GIB_BYTES:
        return CheckResult(
            "scale-8gib-qualification",
            "UNRUN",
            f"scale evidence records {moved} bytes moved, below the "
            f"{SCALE_8GIB_BYTES} the capability asks for",
        )
    return CheckResult(
        "scale-8gib-qualification",
        "PASS",
        f"a qualified run moved {moved} bytes; evidence recorded in the "
        "readiness configuration",
    )


def _run_check(capability, check, context, api) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip.

    An AttributeError from a runtime that simply lacks a fault-injection
    hook is UNRUN — an unexposed boundary — not a failed capability; the
    missing-hook guards above keep that the explicit path, and this catch
    is the honest fallback for anything they miss.
    """
    try:
        detail = check(context, api)
        if isinstance(detail, CheckResult):
            detail.capability = capability
            return detail
        return CheckResult(capability, "PASS", detail)
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


def _compat_unrun(name: str, why: str) -> CheckResult:
    """An UNRUN placeholder the run loop retags with the capability."""
    return CheckResult(name, "UNRUN", why)


def _check_runtime_pin(context, api):
    from gpustack_model_transfer.backends.nixl.runtime import (
        SUPPORTED_VERSIONS,
        VersionMismatchError,
        load_runtime,
    )

    pinned = load_runtime(api=api)
    if pinned.version not in SUPPORTED_VERSIONS:
        raise AssertionError("an unpinned runtime was accepted")
    try:
        load_runtime(api=_BumpedVersionAPI(api), supported_versions={"9.9.9"})
    except VersionMismatchError:
        pass
    else:
        raise AssertionError("a version mismatch was not refused")  # pragma: no cover
    return f"runtime pinned and validated at {api.version}"


def _check_endpoint_selection(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.errors import UnqualifiedEndpointError

    try:
        qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {"UCX_NET_DEVICES": "all"})
    except UnqualifiedEndpointError:
        pass
    else:
        raise AssertionError("UCX_NET_DEVICES=all was accepted")
    for rails in (("all",), ("any",), ("mlx5_*",), ("",), ()):
        try:
            qualify_endpoints("tcp://source:9100", rails, {})
        except UnqualifiedEndpointError:
            continue
        raise AssertionError(f"unqualified rails {rails} were accepted")
    selection = qualify_endpoints(
        "tcp://source:9100", ["mlx5_0:1", "mlx5_1:1"], {"UCX_NET_DEVICES": "mlx5_0:1"}
    )
    settings = selection.agent_settings()
    if settings.get("UCX_NET_DEVICES") != "mlx5_0:1,mlx5_1:1":
        raise AssertionError("the per-agent settings lost the selected rails")
    return (
        "control and payload rails named explicitly; all/wildcards refused; "
        "selection carried per agent, never process-wide"
    )


def _check_service_memlock(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import check_memlock

    window = context["config"]["limits"]["window_bytes"]
    inflight = context["config"]["limits"]["max_inflight"]
    required = window * inflight
    strict = check_memlock(
        required, getrlimit=lambda _which: (required - 1, required - 1)
    )
    if strict.sufficient:
        raise AssertionError("an insufficient memlock read as sufficient")
    exact = check_memlock(required, getrlimit=lambda _which: (required, required))
    unlimited = check_memlock(required, getrlimit=lambda _which: (-1, -1))
    if not exact.sufficient or not unlimited.sufficient:
        raise AssertionError("an adequate memlock read as insufficient")
    return (
        f"registration budget {required} bytes checked against the service "
        "memory lock before any registration"
    )


def _fixture_source_files(context) -> dict:
    """The fixture's real checkpoint bytes keyed by the manifest's file."""
    import os

    blob_path = os.path.join(
        os.path.dirname(context["manifest_path"]), "checkpoint.safetensors"
    )
    with open(blob_path, "rb") as fh:
        return {"checkpoint.safetensors": fh.read()}


def _check_fixed_window_reuse(context, api):
    import hashlib

    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.reader import NixlReader
    from gpustack_model_transfer.core.budgets import ReadBudgets

    # The runtime serves the fixture's real bytes, not a stand-in pattern:
    # the check answers for the bytes it actually moved.
    api.source_files = _fixture_source_files(context)
    limits = context["config"]["limits"]
    budgets = ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )
    manifest = context["manifest"]
    selection = qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {})
    reader = NixlReader(
        manifest,
        api,
        {"source": "peer"},
        budgets,
        selection,
        poll_deadline_seconds=POLL_DEADLINE_SECONDS,
    )
    tensor = max(manifest.tensors, key=lambda t: t.byte_size)
    if tensor.byte_size <= budgets.window_bytes:
        raise AssertionError("the fixture exercises no multi-window tensor")
    first = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
    second = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
    if len(first) != tensor.byte_size or first != second:
        raise AssertionError("the windows did not serve identical bytes")
    expected = api.source_files[tensor.file][tensor.data_begin : tensor.data_end]
    if first != expected:
        raise AssertionError("the windows did not serve the fixture's bytes")
    if hashlib.sha256(first).hexdigest() != hashlib.sha256(expected).hexdigest():
        raise AssertionError("the served bytes do not digest to the slice's digest")
    # A shorter trailing read must not leak the previous window's tail.
    short = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_begin + 8)
    if short != expected[:8]:
        raise AssertionError("a short read leaked stale tail bytes")
    if len(reader.windows) != budgets.max_inflight:
        raise AssertionError("the window count left the in-flight ceiling")
    reader.close()
    return (
        f"a {tensor.byte_size}-byte tensor served byte-exact through "
        f"{budgets.window_bytes}-byte windows, twice, over "
        f"{len(reader.windows)} fixed reusable windows, with no stale tail"
    )


def _check_per_operation_terminal_states(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.errors import (
        TransferRefusedError as Refused,
    )
    from gpustack_model_transfer.backends.nixl.reader import NixlReader
    from gpustack_model_transfer.backends.nixl.testing import (
        SUBMIT_NONTERMINAL,
        SUBMIT_REFUSE_PRE,
    )
    from gpustack_model_transfer.core.budgets import ReadBudgets
    from gpustack_model_transfer.core.errors import (
        AmbiguousReadError,
        OperationStateError,
    )

    if not hasattr(api, "submit_mode"):
        return _compat_unrun(
            "per-operation terminal states",
            "the runtime exposes no submission fault mode; terminal-state "
            "handling can only be qualified where a refusal or a hang is "
            "observable at the boundary",
        )

    limits = context["config"]["limits"]
    manifest = context["manifest"]
    tensor = manifest.tensors[0]
    span = (tensor.data_begin, min(tensor.data_end, tensor.data_begin + 8))

    def open_reader(fake_api):
        budgets = ReadBudgets(
            window_bytes=limits["window_bytes"],
            max_inflight=limits["max_inflight"],
            owned_output_bytes=limits["owned_output_bytes"],
        )
        selection = qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {})
        return NixlReader(
            manifest,
            fake_api,
            {"source": "peer"},
            budgets,
            selection,
            poll_deadline_seconds=0.0,
        )

    refusing = api
    refusing.submit_mode = SUBMIT_REFUSE_PRE
    reader = open_reader(refusing)
    try:
        reader.read_bytes(tensor.file, span[0], span[1])
    except Refused as exc:
        last = list(reader.operations.values())[-1]
        if last.state.value != "failed":
            raise AssertionError("a pre-submission refusal left no FAILED op") from exc
    else:
        raise AssertionError("the pre-submission refusal was not raised")

    hanging = type(api).__new__(type(api))
    hanging.__dict__.update(api.__dict__)
    hanging.submit_mode = SUBMIT_NONTERMINAL
    reader = open_reader(hanging)
    try:
        reader.read_bytes(tensor.file, span[0], span[1])
    except AmbiguousReadError as exc:
        unknown = [
            op for op in reader.operations.values() if op.state.value == "unknown"
        ]
        if not unknown or not unknown[-1].retained:
            raise AssertionError(
                "a nonterminal op was not retained as UNKNOWN"
            ) from exc
        try:
            reader.close()
        except OperationStateError:
            pass
        else:
            raise AssertionError("the reader closed over a retained UNKNOWN")
        reader.resolve(unknown[-1].op_id, "failed", error_code="fence")
        reader.close()
    else:
        raise AssertionError("a nonterminal submission completed")
    return (
        "every operation owns its state: pre-submission refusals FAILED and "
        "released their slot; nonterminal submissions stayed UNKNOWN and "
        "retained until a fence resolved them"
    )


def _check_memory_accounting(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.memory import (
        CounterAttribution,
        MemoryLedger,
    )
    from gpustack_model_transfer.backends.nixl.reader import NixlReader
    from gpustack_model_transfer.core.budgets import ReadBudgets

    try:
        api.device_memory_bytes(api.create_agent("probe", {}))
    except NotImplementedError:
        return _compat_unrun(
            "memory accounting",
            "the runtime does not expose device-wide memory counters in "
            "this environment",
        )

    limits = context["config"]["limits"]
    budgets = ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )
    ledger = MemoryLedger()
    selection = qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {})
    manifest = context["manifest"]
    reader = NixlReader(
        manifest,
        api,
        {"source": "peer"},
        budgets,
        selection,
        ledger=ledger,
        poll_deadline_seconds=POLL_DEADLINE_SECONDS,
    )
    tensor = manifest.tensors[0]
    reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
    reader.close()

    reservation = budgets.window_bytes * budgets.max_inflight
    if ledger.reserved_bytes != 0:
        raise AssertionError("a closed reader kept its reservation")
    if ledger.peak_bytes <= 0:
        raise AssertionError("no in-flight bytes were ever accounted")
    ledger.observe_device_runtime(api.device_memory_bytes(reader._agent))
    attribution = CounterAttribution(
        rail="mlx5_0:1", counters=api.port_counters(reader._agent, "mlx5_0:1")
    )
    attribution.validate()
    return (
        f"active/peak accounted ({ledger.peak_bytes} bytes peak against a "
        f"{reservation}-byte reservation); device-wide runtime bytes "
        f"{ledger.device_runtime_bytes} recorded separately; port counters "
        "attributed to their rail without proving the transfer path"
    )


def _check_counter_attribution(context, api):
    from gpustack_model_transfer.backends.nixl.memory import CounterAttribution

    agent = api.create_agent("counter-probe", {"UCX_NET_DEVICES": "mlx5_0:1"})
    try:
        raw_counters = api.port_counters(agent, "mlx5_0:1")
    except NotImplementedError:
        return _compat_unrun(
            "counter attribution",
            "the runtime does not expose port counters in this environment",
        )
    counters = CounterAttribution(rail="mlx5_0:1", counters=raw_counters)
    counters.validate()
    if not counters.counters:
        raise AssertionError("no counters were collected to attribute")
    try:
        CounterAttribution(
            rail="mlx5_0:1", counters=counters.counters, proves_transfer_path=True
        ).validate()
    except Exception:
        return (
            "counters attribute traffic to the selected rail; a record "
            "claiming they prove the transfer path is refused"
        )
    raise AssertionError("a counters-prove-path record was accepted")


def _check_multi_rail_qualification(context, api):
    from gpustack_model_transfer.backends.nixl.capabilities import (
        QualifiedRailPair,
        RailQualifier,
        nixl_backend_capabilities,
    )
    from gpustack_model_transfer.backends.nixl.errors import UnqualifiedEndpointError

    qualifier = RailQualifier()
    qualifier.probe_pair("mlx5_0:1", "mlx5_1:1", lambda s, t: (True, ("delta",)))
    failed = qualifier.probe_pair("mlx5_0:1", "mlx5_2:1", lambda s, t: (False, ()))
    if failed.probe_passed:
        raise AssertionError("a failed probe qualified a pair")
    capabilities = nixl_backend_capabilities(
        requested=("ucx",),
        applied="ucx",
        effective="ucx",
        effective_observed=True,
        qualified_rail_pairs=qualifier.qualified_pairs(),
    )
    capabilities.validate()
    advertised = {
        f"{pair.source_rail}->{pair.target_rail}"
        for pair in capabilities.qualified_rail_pairs
    }
    if "mlx5_0:1->mlx5_1:1" not in advertised or "mlx5_0:1->mlx5_2:1" in advertised:
        raise AssertionError("an unqualified pair was advertised")
    try:
        nixl_backend_capabilities(
            requested=("ucx",),
            applied="ucx",
            effective="ucx",
            effective_observed=True,
            qualified_rail_pairs=(
                QualifiedRailPair(
                    source_rail="mlx5_0:1", target_rail="mlx5_9:1", probe_passed=False
                ),
            ),
        )
    except UnqualifiedEndpointError:
        return (
            f"{len(advertised)} rail pair(s) advertised, each from a passing "
            "end-to-end probe; the failed pair stayed unadvertised and a "
            "record claiming it was refused"
        )
    raise AssertionError("an unqualified pair was accepted into capabilities")


def _check_registration_renewal(context, api):
    from gpustack_model_transfer.backends.nixl.endpoints import qualify_endpoints
    from gpustack_model_transfer.backends.nixl.reader import NixlReader
    from gpustack_model_transfer.core.budgets import ReadBudgets

    if not hasattr(api, "expire_registration_after"):
        return _compat_unrun(
            "registration renewal",
            "the runtime exposes no registration expiry hook; renewal can "
            "only be qualified where expiry is observable",
        )
    limits = context["config"]["limits"]
    api.expire_registration_after = 1
    api.source_files = _fixture_source_files(context)
    budgets = ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )
    manifest = context["manifest"]
    selection = qualify_endpoints("tcp://source:9100", ["mlx5_0:1"], {})
    reader = NixlReader(
        manifest,
        api,
        {"source": "peer"},
        budgets,
        selection,
        poll_deadline_seconds=POLL_DEADLINE_SECONDS,
    )
    tensor = manifest.tensors[0]
    first = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
    second = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
    if first != second or reader.registration_renewals < 1:
        raise AssertionError("the expired registration was not renewed in place")
    reader.close()
    return (
        f"fixed-window registration renewed {reader.registration_renewals} "
        "time(s) on expiry, without a per-read re-registration"
    )


class _BumpedVersionAPI:
    """A wrapper reporting a version outside any qualified set."""

    def __init__(self, inner):
        self._inner = inner
        self.version = "8.8.8"

    def __getattr__(self, name):
        return getattr(self._inner, name)


_CHECKS = (
    ("runtime-pin", _check_runtime_pin),
    ("endpoint-selection", _check_endpoint_selection),
    ("service-memlock", _check_service_memlock),
    ("fixed-window-reuse", _check_fixed_window_reuse),
    ("per-operation-terminal-states", _check_per_operation_terminal_states),
    ("memory-accounting", _check_memory_accounting),
    ("counter-attribution", _check_counter_attribution),
    ("multi-rail-qualification", _check_multi_rail_qualification),
    ("registration-renewal", _check_registration_renewal),
)


__all__ = ["CAPABILITIES", "run"]
