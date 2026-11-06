"""The model-load acceptance suite: local reader, per-engine routes.

Every capability the pinned route claims is exercised for real: metadata
probes that read no payload, multi-window assembly against the golden
fixture, budget refusal before mutation, tying and dtypes, exactly-once
finalization, the borrowed-storage contract, resource release before KV
profiling, NIXL connector coexistence, and forward equivalence within
predeclared tolerances. The SGLang route adds the load-group capability:
multi-rank sharding, reassembly, measured traffic amplification, and a
bounded group failure.

Without a driver the suite cannot observe anything, and a missing
prerequisite is never a skipped pass: every capability reports UNRUN and
the run is NO-GO. With a driver injected (the reference runtime in tests),
every capability runs and reports PASS or NO-GO on its own merits; a
driver that cannot answer a capability's question NO-GOs it, never skips.
"""

import copy
import importlib

from gpustack_model_transfer.acceptance.report import CheckResult
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import (
    BackendCapabilities,
    TransportSelection,
    local_backend_capabilities,
)
from gpustack_model_transfer.core.errors import (
    CapacityExhaustedError,
    DeniedContentError,
    InvalidDescriptorError,
    OperationStateError,
)

CAPABILITIES = (
    "extension-route",
    "metadata-probe",
    "multi-window-assembly",
    "owned-budget-refusal",
    "tied-tensors-and-dtype",
    "finalization-once",
    "borrowed-visibility-fence",
    "pre-kv-profile-release",
    "nixl-kv-connector-coexistence",
    "forward-equivalence",
)

# The SGLang route carries its own extra capability: the load group.
ENGINE_EXTRA_CAPABILITIES = {"sglang": ("tp-sharding-load-group",)}

# The entry point group each engine's pinned route registers under.
_ENGINE_ENTRY_POINT_GROUPS = {
    "vllm": "vllm.general_plugins",
    "vllm-ascend": "vllm.general_plugins",
    "sglang": "sglang.srt.model_loader",
}

# The connector configuration each engine preserves untouched through a
# load; the suite loads with it and asserts nothing moved.
_ENGINE_CONNECTOR_CONFIGS = {
    "vllm": {
        "kv_transfer_config": {
            "kv_connector": "NixlConnector",
            "kv_role": "kv_both",
            "kv_buffer_device": "cuda",
        },
        "ec_connector_config": {
            "ec_connector": "NixlConnector",
            "ec_role": "ec_both",
        },
    },
}


def _adapter(engine: str):
    """Load the engine's adapter module; the name is runner-validated."""
    if engine == "vllm":
        return importlib.import_module("gpustack_model_transfer.adapters.vllm")
    if engine == "vllm-ascend":
        return importlib.import_module("gpustack_model_transfer.adapters.vllm_ascend")
    if engine == "sglang":
        return importlib.import_module("gpustack_model_transfer.adapters.sglang")
    raise AssertionError(f"engine {engine!r} has no adapter wired into the suite")


def capabilities_for(engine: str) -> tuple:
    return CAPABILITIES + ENGINE_EXTRA_CAPABILITIES.get(engine, ())


def _probe_of(engine: str):
    """The engine's payload-free checkpoint probe."""
    pkg = engine.replace("-", "_")
    module = importlib.import_module(f"gpustack_model_transfer.adapters.{pkg}.metadata")
    return module.probe_checkpoint_metadata


def _connector_config(engine: str) -> dict:
    adapter = _adapter(engine)
    for attr in ("NIXL_ENGINE_CONFIG", "ASCEND_ENGINE_CONFIG"):
        inline = getattr(adapter, attr, None)
        if inline is not None:
            return inline
    return _ENGINE_CONNECTOR_CONFIGS["vllm"]


class _CountingReader:
    """Counts reads so window shapes and refusal-before-read are visible."""

    def __init__(self, context):
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        artifact = context["config"]["artifact"]
        self._inner = LocalFileReader(
            context["manifest_path"], expected_sha256=artifact["manifest_sha256"]
        )
        self.manifest = self._inner.manifest
        self.ranges = []

    def read_bytes(self, file_path, begin, end):
        self.ranges.append((begin, end))
        return self._inner.read_bytes(file_path, begin, end)

    def close(self):
        self._inner.close()


def _budgets(context, **overrides):
    limits = dict(context["config"]["limits"])
    limits.update(overrides)
    return ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )


def _loader(context, adapter, reader=None, budgets=None, capabilities=None):
    return adapter.ModelTransferModelLoader(
        reader=reader or _CountingReader(context),
        capabilities=capabilities or local_backend_capabilities(),
        budgets=budgets or _budgets(context),
    )


def _load(context, adapter, driver, *, capabilities=None, **option_kwargs):
    """One load with a fresh counting reader; returns (report, reader)."""
    reader = _CountingReader(context)
    loader = _loader(context, adapter, reader=reader, capabilities=capabilities)
    options = adapter.LoadOptions(**option_kwargs)
    return loader.load(driver, options), reader


def _borrowed_capabilities():
    base = local_backend_capabilities()
    return BackendCapabilities(
        requested=base.requested,
        applied=base.applied,
        effective=base.effective,
        effective_observed=base.effective_observed,
        rejected=base.rejected,
        formats=base.formats,
        memory_types=base.memory_types,
        max_request_bytes=base.max_request_bytes,
        alignment_bytes=base.alignment_bytes,
        tensor_ownership="borrowed",
        device_visibility=base.device_visibility,
        transport=TransportSelection(
            control=base.transport.control, payload=base.transport.payload
        ),
    )


def _check_extension_route(context, driver, adapter, engine):
    import json

    metadata = adapter.image_integration_metadata()
    if adapter.LOADER_LOAD_FORMAT != "model-transfer":
        raise AssertionError("the pinned load format moved")
    expected_class = (
        "ModelTransferSGLangLoader"
        if engine == "sglang"
        else "NativeModelTransferLoader"
    )
    if expected_class != adapter.LOADER_CLASS_NAME:
        raise AssertionError("the pinned loader class moved")
    if _ENGINE_ENTRY_POINT_GROUPS[engine] != adapter.LOADER_ENTRY_POINT_GROUP:
        raise AssertionError("the entry point group moved")
    if ":" not in adapter.LOADER_ENTRY_POINT:
        raise AssertionError("the entry point does not name module:attr")
    if adapter.SUPPORTED_CHECKPOINT_FORMAT != "safetensors":
        raise AssertionError("the pinned checkpoint format moved")
    for tensor in context["manifest"].tensors:
        if tensor.dtype not in adapter.SUPPORTED_DTYPES:
            raise AssertionError(
                f"fixture dtype {tensor.dtype!r} is outside the pinned route"
            )
    # The metadata rides image labels, so it must survive a JSON round trip.
    json.loads(json.dumps(metadata))
    return (
        f"declared route metadata {adapter.LOADER_LOAD_FORMAT}/{adapter.LOADER_CLASS_NAME} "
        f"advertises {adapter.SUPPORTED_CHECKPOINT_FORMAT} "
        f"({', '.join(sorted(adapter.SUPPORTED_DTYPES))}); registration not exercised"
    )


def _check_metadata_probe(context, driver, adapter, engine):
    reader = _CountingReader(context)
    try:
        spans = [
            (tensor.data_begin, tensor.data_end)
            for tensor in reader.manifest.tensors
            if tensor.byte_size
        ]
        probed = _probe_of(engine)(reader)
        for begin, end in reader.ranges:
            for payload_begin, payload_end in spans:
                if begin < payload_end and payload_begin < end:
                    raise AssertionError("the probe read inside a tensor payload")
        if probed.payload_bytes_read:
            raise AssertionError("the probe reported payload reads")
        if not probed.parameter_names or not probed.effective_dtypes:
            raise AssertionError("the probe found no parameters")
        return (
            f"probed {len(probed.parameter_names)} parameters and "
            f"{len(probed.engine_metadata_names)} engine-metadata "
            f"tensors from the header alone"
        )
    finally:
        reader.close()


def _check_multi_window_assembly(context, driver, adapter, engine):
    report, reader = _load(context, adapter, driver)
    window = context["config"]["limits"]["window_bytes"]
    multi = [
        tensor.name
        for tensor in context["manifest"].tensors
        if tensor.byte_size > window
    ]
    if not multi:
        raise AssertionError("the fixture exercises no multi-window tensor")
    if not reader.ranges:
        raise AssertionError("the load read nothing")
    # The safetensors header is metadata, not a window; only payload reads
    # are bounded by the transfer window.
    payload_start = min(
        tensor.data_begin for tensor in context["manifest"].tensors if tensor.byte_size
    )
    payload_reads = [
        (begin, end) for begin, end in reader.ranges if begin >= payload_start
    ]
    if not payload_reads:
        raise AssertionError("the load served no payload")
    for begin, end in payload_reads:
        if end - begin > window:
            raise AssertionError("a read exceeded the transfer window")
    native = driver.build_native_model()
    for name in sorted(report.model.parameters):
        if driver.model_parameter_bytes(
            report.model, name
        ) != driver.model_parameter_bytes(native, name):
            raise AssertionError(f"{name} did not assemble byte-exact")
    if report.ledger.load_only_bytes != 0:
        raise AssertionError("staging bytes survived the load")
    return (
        f"assembled {len(multi)} tensors above the {window}-byte "
        f"window through {len(payload_reads)} windows, byte-exact"
    )


def _check_owned_budget_refusal(context, driver, adapter, engine):
    largest = max(context["manifest"].tensors, key=lambda tensor: tensor.byte_size)
    tight = _budgets(context, owned_output_bytes=largest.byte_size - 1)
    reader = _CountingReader(context)
    try:
        loader = _loader(context, adapter, reader=reader, budgets=tight)
        try:
            loader.load(driver, adapter.LoadOptions())
        except CapacityExhaustedError as exc:
            if reader.ranges:
                raise AssertionError("refusal read checkpoint bytes") from exc
            if driver.built:
                raise AssertionError("refusal built or mutated a model") from exc
            return (
                f"{largest.name!r} at {largest.byte_size} bytes refused "
                f"above a {largest.byte_size - 1}-byte owned budget "
                f"before any read"
            )
        raise AssertionError("an over-budget tensor was accepted")
    finally:
        reader.close()


def _check_tied_tensors_and_dtype(context, driver, adapter, engine):
    tied_report, _ = _load(context, adapter, driver, tie_word_embeddings=True)
    if tied_report.tied_aliases != ("lm_head.weight",):
        raise AssertionError("the tied alias was not recorded")
    if not driver.alias_tied(tied_report.model):
        raise AssertionError("tied embeddings did not share one storage")
    untied_report, _ = _load(context, adapter, driver, tie_word_embeddings=False)
    if untied_report.tied_aliases:
        raise AssertionError("an untied load recorded an alias")
    if driver.alias_tied(untied_report.model):
        raise AssertionError("untied embeddings shared storage")
    if (
        driver.model_parameter_bytes(untied_report.model, "model.layers.0.attn.bias")
        != b""
    ):
        raise AssertionError("the zero-element tensor gained bytes")
    native = driver.build_native_model()
    for name in sorted(untied_report.model.parameters):
        if not adapter.DEFAULT_PARAMETER_TOLERANCE.within(
            driver.parameter_values(untied_report.model, name),
            driver.parameter_values(native, name),
        ):
            raise AssertionError(f"{name} left the exact tolerance")
    return (
        "tied embeddings alias one storage; untied stay distinct; "
        "F32, F16, I64, and the zero-element tensor match exactly"
    )


class _FailingStoreDriver:
    """A driver whose second store fails, simulating a mid-load refusal."""

    def __init__(self, inner):
        self._inner = inner
        self._stores = 0

    def __getattr__(self, name):
        return getattr(self._inner, name)

    def build_model(self, plan, options):
        return self._inner.build_model(plan, options)

    def store(self, model, entry, payload, ownership=None):
        self._stores += 1
        if self._stores > 1:
            raise DeniedContentError("the engine refused mid-load")
        return self._inner.store(model, entry, payload, ownership)


def _check_finalization_once(context, driver, adapter, engine):
    report, _ = _load(context, adapter, driver)
    if report.finalization_count != 1:
        raise AssertionError(f"finalization ran {report.finalization_count} times")
    failing = _FailingStoreDriver(driver)
    try:
        _load(context, adapter, failing)
    except DeniedContentError as exc:
        model = failing.last_model
        if not model.mutated:
            raise AssertionError("a partial load left the model untainted") from exc
        if model.finalization_count != 0:
            raise AssertionError("a failed load finalized the model") from exc
        return "finalization ran exactly once; a partial load is tainted"
    raise AssertionError("a mid-load failure was accepted")


def _check_borrowed_visibility_fence(context, driver, adapter, engine):
    # The stream a borrowed tensor becomes visible on is the engine's own:
    # CUDA for the vLLM and SGLang routes, the NPU stream for vLLM-Ascend.
    stream = getattr(adapter, "DEFAULT_VISIBILITY_STREAM", "cuda:0")
    try:
        _load(
            context,
            adapter,
            driver,
            ownership=adapter.TensorOwnership.BORROWED,
            borrowed=adapter.BorrowedContract(
                stream=stream,
                visibility_fence=lambda name: None,
                release=lambda name: True,
            ),
        )
    except InvalidDescriptorError:
        pass
    else:
        raise AssertionError("borrowed storage was accepted without backend support")
    events = []
    contract = adapter.BorrowedContract(
        stream=stream,
        visibility_fence=lambda name: events.append(f"fence:{name}"),
        release=lambda name: events.append(f"release:{name}") or True,
    )
    report, _ = _load(
        context,
        adapter,
        driver,
        capabilities=_borrowed_capabilities(),
        ownership=adapter.TensorOwnership.BORROWED,
        borrowed=contract,
    )
    if report.borrowed_fallbacks:
        raise AssertionError("a released borrowed tensor fell back")
    for name in report.loaded_parameters:
        if f"fence:{name}" not in events or f"release:{name}" not in events:
            raise AssertionError(f"fence/release missing for {name}")
        if events.index(f"fence:{name}") > events.index(f"release:{name}"):
            raise AssertionError(f"release ran before the fence for {name}")
    retained, _ = _load(
        context,
        adapter,
        driver,
        capabilities=_borrowed_capabilities(),
        ownership=adapter.TensorOwnership.BORROWED,
        borrowed=adapter.BorrowedContract(
            stream=stream,
            visibility_fence=lambda name: None,
            release=lambda name: False,
        ),
    )
    if not retained.borrowed_fallbacks:
        raise AssertionError(
            "a retained borrowed tensor was not copied to owned storage"
        )
    return (
        "borrowed refused without support or contract; fenced and "
        "released in order; a retaining consumer is copied to owned"
    )


def _check_pre_kv_profile_release(context, driver, adapter, engine):
    report, _ = _load(context, adapter, driver)
    if report.ledger.load_only_bytes != 0:
        raise AssertionError("load-only bytes survived the load")
    if not report.session_released:
        raise AssertionError("the reader session survived the load")
    try:
        adapter.profile_kv_capacity(
            driver, report.model, adapter.ResourceLedger(load_only_bytes=1024)
        )
    except OperationStateError:
        pass
    else:
        raise AssertionError("profiling ran over held staging bytes")
    capacity = adapter.profile_kv_capacity(driver, report.model, report.ledger)
    native = driver.build_native_model()
    baseline = driver.profile_kv_capacity(native)
    if capacity != baseline:
        raise AssertionError(
            f"usable KV capacity {capacity} != native baseline {baseline}"
        )
    return (
        f"load-only resources released first; usable KV capacity "
        f"{capacity} matches the native baseline"
    )


def _check_nixl_coexistence(context, driver, adapter, engine):
    import os

    engine_config = copy.deepcopy(_connector_config(engine))
    saved_ucx = os.environ.get("UCX_NET_DEVICES")
    saved_nixl = os.environ.get("NIXL_BACKEND")
    os.environ["UCX_NET_DEVICES"] = "mlx5_0:1"
    os.environ["NIXL_BACKEND"] = "UCX"
    try:
        env_before = {
            key: os.environ[key]
            for key in sorted(os.environ)
            if key.startswith(("UCX_", "NIXL_"))
        }
        report, _ = _load(context, adapter, driver, engine_config=engine_config)
        if copy.deepcopy(engine_config) != _connector_config(engine):
            raise AssertionError("the loader rewrote the engine configuration")
        env_after = {
            key: os.environ[key]
            for key in sorted(os.environ)
            if key.startswith(("UCX_", "NIXL_"))
        }
        if env_after != env_before:
            raise AssertionError("the loader touched UCX/NIXL process settings")
    finally:
        if saved_ucx is None:
            os.environ.pop("UCX_NET_DEVICES", None)
        else:
            os.environ["UCX_NET_DEVICES"] = saved_ucx
        if saved_nixl is None:
            os.environ.pop("NIXL_BACKEND", None)
        else:
            os.environ["NIXL_BACKEND"] = saved_nixl
    connector = report.engine_config.get("kv_connector")
    if connector is None:
        transfer = report.engine_config.get("kv_transfer_config")
        connector = transfer and transfer.get("kv_connector")
    if connector not in ("NixlConnector", "HixlConnector"):
        raise AssertionError("the KV connector did not survive the load")
    if not driver.forward(report.model, [1, 2]):
        raise AssertionError("inference is broken after the load")
    return (
        "NixlConnector KV/EC configuration preserved; UCX/NIXL "
        "environment untouched; inference works after the load"
    )


def _check_forward_equivalence(context, driver, adapter, engine):
    report, _ = _load(context, adapter, driver)
    inputs = [1, 2]
    loaded = driver.forward(report.model, inputs)
    native = driver.build_native_model()
    expected = driver.forward(native, inputs)
    if not adapter.DEFAULT_FORWARD_TOLERANCE.within(loaded, expected):
        raise AssertionError("forward output left the declared tolerance")
    perturb = getattr(driver, "perturb_parameter", None)
    if perturb is None:
        raise AssertionError("the driver cannot demonstrate a falsifiable comparison")
    perturb(native, "model.norm.weight")
    if adapter.DEFAULT_FORWARD_TOLERANCE.within(loaded, driver.forward(native, inputs)):
        raise AssertionError("the equivalence comparison cannot fail")
    return (
        "forward outputs match the native reference within "
        f"rtol={adapter.DEFAULT_FORWARD_TOLERANCE.rtol}, "
        f"atol={adapter.DEFAULT_FORWARD_TOLERANCE.atol}"
    )


def _check_tp_sharding_load_group(context, driver, adapter, engine):
    """The SGLang-only capability: one load group, every rank accounted."""
    import os

    from gpustack_model_transfer.adapters.sglang.sharding import load_group
    from gpustack_model_transfer.core.local_reader import LocalFileReader

    root = os.path.dirname(context["manifest_path"])
    artifact = context["config"]["artifact"]
    window = context["config"]["limits"]["window_bytes"]

    def reader_factory():
        return LocalFileReader(
            context["manifest_path"], expected_sha256=artifact["manifest_sha256"]
        )

    def fresh_driver(rank):
        return driver.__class__(root)

    group = load_group(
        reader_factory=reader_factory,
        driver_factory=fresh_driver,
        options=_sglang_load_options(),
        world_size=2,
        capabilities=local_backend_capabilities(),
        budgets=ReadBudgets(
            window_bytes=window,
            max_inflight=context["config"]["limits"]["max_inflight"],
            owned_output_bytes=context["config"]["limits"]["owned_output_bytes"],
        ),
        raise_on_failure=False,
    )
    if group.group_state != "loaded":
        raise AssertionError(f"the two-rank group failed: {group.failure}")
    if len(group.rank_reports) != 2:
        raise AssertionError("a rank went missing")
    for report in group.rank_reports:
        if report.finalization_count != 1:
            raise AssertionError("a rank was not finalized exactly once")
        if report.ledger.load_only_bytes != 0:
            raise AssertionError("a rank kept staging bytes")
    # Column shards reassemble byte-exact, in rank order.
    for name in group.plan.sharded_names:
        if group.plan.strategies[name] != "column":
            continue
        whole = driver.model_parameter_bytes(driver.build_native_model(), name)
        joined = b"".join(
            group.rank_payload(rank, name) for rank in range(group.world_size)
        )
        if joined != whole:
            raise AssertionError(f"{name} did not reassemble byte-exact")
        for report in group.rank_reports:
            if name not in report.partial_shard_reads:
                raise AssertionError(f"{name} shard read was not recorded as partial")
    # Replicated parameters are identical on every rank.
    native = driver.build_native_model()
    for name in group.plan.replicated_names:
        if name.startswith("metadata."):
            continue
        for rank in range(group.world_size):
            if group.rank_payload(rank, name) != driver.model_parameter_bytes(
                native, name
            ):
                raise AssertionError(f"replicated {name} differs on rank {rank}")
    if group.traffic_amplification < 1.0:
        raise AssertionError("traffic amplification below one is unmeasurable")

    # A rank that fails after partial initialization bounds the group:
    # tainted ranks, nothing finalized, no retry.
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

    def failing_driver_factory(rank):
        runtime = ReferenceRuntime(root)
        if rank != 1:
            return runtime
        runtime.store_error = DeniedContentError("rank 1 refused mid-load")

        def store(model, entry, payload, ownership=None):
            if entry.tensor.name.startswith("model.norm"):
                raise runtime.store_error
            return ReferenceRuntime.store(runtime, model, entry, payload)

        runtime.store = store
        return runtime

    failed = load_group(
        reader_factory=reader_factory,
        driver_factory=failing_driver_factory,
        options=_sglang_load_options(),
        world_size=2,
        capabilities=local_backend_capabilities(),
        budgets=ReadBudgets(
            window_bytes=window,
            max_inflight=context["config"]["limits"]["max_inflight"],
            owned_output_bytes=context["config"]["limits"]["owned_output_bytes"],
        ),
        raise_on_failure=False,
    )
    if failed.group_state != "failed" or not failed.tainted:
        raise AssertionError("a failed group was not bounded and tainted")
    for report in failed.rank_reports:
        if report.model is not None and report.finalization_count != 0:
            raise AssertionError("a failed group finalized a rank")
    return (
        f"two ranks reassembled {len(group.plan.sharded_names)} sharded "
        f"and {len(group.plan.replicated_names)} replicated tensors; "
        f"traffic amplification {group.traffic_amplification:.2f}; a "
        f"failed rank taints the group and finalizes nothing"
    )


def _sglang_load_options():
    from gpustack_model_transfer.adapters.sglang import LoadOptions

    return LoadOptions()


def _checks_for(engine):
    checks = [
        ("extension-route", _check_extension_route),
        ("metadata-probe", _check_metadata_probe),
        ("multi-window-assembly", _check_multi_window_assembly),
        ("owned-budget-refusal", _check_owned_budget_refusal),
        ("tied-tensors-and-dtype", _check_tied_tensors_and_dtype),
        ("finalization-once", _check_finalization_once),
        ("borrowed-visibility-fence", _check_borrowed_visibility_fence),
        ("pre-kv-profile-release", _check_pre_kv_profile_release),
        ("nixl-kv-connector-coexistence", _check_nixl_coexistence),
        ("forward-equivalence", _check_forward_equivalence),
    ]
    if engine == "sglang":
        checks.append(("tp-sharding-load-group", _check_tp_sharding_load_group))
    return checks


# The (backend, engine) pairs this suite is qualified to carry. A
# requested pair outside this set UNRUNs rather than silently running
# the local route under a remote backend's name.
_QUALIFIED_BACKEND_ENGINES = {
    ("nixl", "vllm"),
    ("nixl", "sglang"),
    ("hixl", "vllm-ascend"),
}


def _remote_backend_reason(backend, engine) -> str:
    """'' when the requested remote backend may carry this engine.

    Otherwise the UNRUN reason: an unqualified (backend, engine) pair,
    or a backend whose runtime prerequisites are missing on this host.
    The backend transport itself is exercised by the backend and safety
    suites; here its presence is a gate, never a silent LocalFileReader
    substitution under a remote backend's name.
    """
    if (backend, engine) not in _QUALIFIED_BACKEND_ENGINES:
        pairs = ", ".join(f"{b}/{e}" for b, e in sorted(_QUALIFIED_BACKEND_ENGINES))
        return (
            f"the engine {engine!r} is not qualified for backend {backend!r}; "
            f"qualified model-load pairs are {pairs}"
        )
    try:
        module = importlib.import_module(
            f"gpustack_model_transfer.backends.{backend}.runtime"
        )
        readiness = module.probe_runtime()
    except Exception as exc:
        return f"the {backend} runtime probe failed: {exc}"
    if readiness.missing:
        return f"missing backend prerequisites ({backend}): " + ", ".join(
            readiness.missing
        )
    return ""


def run(context, runtime=None, engine="vllm", backend=None) -> list:
    """Run every model-load capability for the route of one engine.

    runtime, when given, is a zero-argument factory producing a fresh
    driver per capability — the reference runtime in tests; an injected
    runtime always keeps the existing behavior. Without it the host is
    probed: under a requested remote backend (nixl or hixl) the backend
    runtime is probed too and an unqualified (backend, engine) pair or a
    missing backend prerequisite UNRUNs every capability instead of
    silently reading local files under a remote backend's name. Under
    the local backend, missing torch, the engine, or CUDA UNRUNs every
    capability as before: a missing prerequisite fails the run, never a
    skipped pass.
    """
    adapter = _adapter(engine)
    if backend is None:
        backend = context["config"]["backend"]
    if runtime is None and backend != "local":
        reason = _remote_backend_reason(backend, engine)
        if reason:
            return [
                CheckResult(capability, "UNRUN", reason)
                for capability in capabilities_for(engine)
            ]
    if runtime is None:
        pkg = engine.replace("-", "_")
        runtime_mod = "npu_runtime" if engine == "vllm-ascend" else "torch_runtime"
        torch_runtime = importlib.import_module(
            f"gpustack_model_transfer.adapters.{pkg}.{runtime_mod}"
        )
        driver, reason = torch_runtime.probe_host_driver()
        if driver is None:
            return [
                CheckResult(capability, "UNRUN", reason)
                for capability in capabilities_for(engine)
            ]
    else:
        driver, reason = None, ""

    results = []
    for capability, check in _checks_for(engine):
        active = runtime() if runtime is not None else driver
        try:
            detail = check(context, active, adapter, engine)
            if runtime is not None:
                detail = f"reference fixture only: {detail}"
            results.append(CheckResult(capability, "PASS", detail))
        except Exception as exc:
            results.append(
                CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")
            )
    return results


__all__ = ["CAPABILITIES", "capabilities_for", "run"]
