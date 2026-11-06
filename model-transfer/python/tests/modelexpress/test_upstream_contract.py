"""Run the pinned upstream chain and plugin wrapper with CPU engine doubles."""

import importlib
import os
import sys
import types
from contextlib import contextmanager
from pathlib import Path

import pytest


@pytest.fixture
def upstream(monkeypatch, tmp_path):
    root = os.environ.get("GPUSTACK_TEST_MODELEXPRESS_ROOT")
    if not root:
        pytest.skip(
            "set GPUSTACK_TEST_MODELEXPRESS_ROOT to the pinned upstream checkout"
        )
    package = Path(root) / "modelexpress_client/python/modelexpress"
    if not (package / "load_strategy/__init__.py").is_file():
        pytest.fail("the configured ModelExpress checkout has no real strategy chain")
    for name in list(sys.modules):
        if name == "modelexpress" or name.startswith(
            ("modelexpress.", "gpustack_model_transfer.adapters.modelexpress")
        ):
            monkeypatch.delitem(sys.modules, name)

    def module(name, **attrs):
        value = types.ModuleType(name)
        value.__dict__.update(attrs)
        monkeypatch.setitem(sys.modules, name, value)
        return value

    class Model:
        pass

    torch = module("torch", Tensor=object, device=object)
    torch.nn = module("torch.nn", Module=Model)
    mx = module("modelexpress")
    mx.__path__ = [str(package)]
    envs = module(
        "modelexpress.envs",
        MX_PLUGIN_DIR=str(tmp_path),
        MX_LOAD_PLUGIN="gpustack",
        MX_LOAD_PLUGIN_BEFORE="rdma",
        MX_LOAD_STRATEGY_CHAIN="INFERENCE",
    )
    module("modelexpress.p2p_pb2")
    module(
        "modelexpress.accelerators",
        AcceleratorBackend=object,
        CudaAcceleratorBackend=object,
    )
    module("modelexpress.client", MxClientBase=object)
    module("modelexpress.nixl_transfer", is_nixl_available=lambda: False)
    readiness = types.SimpleNamespace(ready=True, version="1.4.1")
    runtime = module(
        "gpustack_model_transfer.backends.nixl.runtime",
        probe_runtime=lambda: readiness,
    )
    module("modelexpress.tensor_utils", log_tensor_summary=lambda *a: None)
    module("modelexpress.metadata")
    module(
        "modelexpress.metadata.publish",
        extend_published_tensors=lambda *a, **k: None,
        publish_metadata_and_ready=lambda *a, **k: None,
    )

    class Span:
        def set_attribute(self, *args):
            pass

    @contextmanager
    def span(*args):
        yield Span()

    module(
        "modelexpress.tracing", tracer=types.SimpleNamespace(start_as_current_span=span)
    )
    chain = importlib.import_module("modelexpress.load_strategy")
    plugins = importlib.import_module("modelexpress.plugins")
    events = []
    for filename, class_name, name in (
        ("rdma_strategy", "RdmaStrategy", "rdma"),
        ("server_cache_strategy", "ServerCacheStrategy", "server-cache"),
        ("instant_tensor_strategy", "InstantTensorStrategy", "instant_tensor"),
        ("model_streamer_strategy", "ModelStreamerStrategy", "model_streamer"),
        ("gds_strategy", "GdsStrategy", "gds"),
        ("default_strategy", "DefaultStrategy", "default"),
    ):

        def fallback(self, result, ctx):
            events.append("fallback")
            return result

        cls = type(class_name, (chain.LoadStrategy,), {"name": name, "load": fallback})
        module("modelexpress.load_strategy." + filename, **{class_name: cls})
    (tmp_path / "mx_plugin_gpustack.py").write_text(
        "from gpustack_model_transfer.adapters.modelexpress.entry import register_plugin\n"
    )
    monkeypatch.setenv("MX_LOAD_PLUGIN", "gpustack")
    monkeypatch.setenv("MX_LOAD_PLUGIN_BEFORE", "rdma")
    monkeypatch.setenv("MX_LOAD_STRATEGY_CHAIN", "INFERENCE")
    monkeypatch.setenv("MX_PLUGIN_DIR", str(tmp_path))
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_POLICY", "require")
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_BACKEND", "nixl")
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT", "gpustack-modelexpress")
    monkeypatch.setenv("HF_HUB_OFFLINE", "1")
    monkeypatch.setenv("TRANSFORMERS_OFFLINE", "1")
    monkeypatch.setenv("MX_ARTIFACT_TRANSFER", "0")
    monkeypatch.setenv("MX_VMM_ARENA", "0")
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_PEER_P2P", "0")
    entry = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.entry"
    )
    policy = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.policy"
    )
    yield types.SimpleNamespace(
        chain=chain,
        plugins=plugins,
        entry=entry,
        policy=policy,
        events=events,
        module=module,
        model_class=Model,
        envs=envs,
        readiness=readiness,
        runtime=runtime,
    )


def test_real_upstream_discovers_strategy_class(upstream):
    upstream.plugins.initialize_plugins()
    strategy = upstream.plugins.get_plugin("gpustack")
    assert isinstance(strategy, upstream.chain.LoadStrategy)


def test_real_terminal_exception_identity(upstream):
    assert issubclass(
        upstream.policy.StrategyRecoveryError, upstream.chain.StrategyRecoveryError
    )


@pytest.fixture
def load_context(upstream):
    events = upstream.events

    class VllmAdapter:
        def __init__(self):
            self.model_config = types.SimpleNamespace()
            self.vllm_config = types.SimpleNamespace(
                load_config=types.SimpleNamespace(),
                speculative_config=None,
                parallel_config=types.SimpleNamespace(
                    tensor_parallel_size=1,
                    pipeline_parallel_size=1,
                    data_parallel_size=1,
                    prefill_context_parallel_size=1,
                    decode_context_parallel_size=1,
                    enable_expert_parallel=False,
                    enable_eplb=False,
                ),
            )
            self.fail_finalize = False
            self.envelopes = []

        def apply_weight_iter(self, result, weights):
            self.envelopes.append(result)
            assert result.publishable is False
            result.model.weights = list(weights)
            events.append("apply")
            return result

        def after_weight_iter_load(self, result):
            self.envelopes.append(result)
            assert result.publishable is False
            events.append("finalize")
            if self.fail_finalize:
                raise RuntimeError("postprocess failed")
            return result

        def reinit_for_retry(self, result):
            events.append("reinit")
            return result

    class Native:
        fail_load = False

        def __init__(self, load_config):
            pass

        def load_initialized_model(
            self, model, config, model_config, *, consume_weights
        ):
            events.append("native")
            consume_weights(iter((("weight", object()),)))
            if self.fail_load:
                raise RuntimeError("partial native load failed")
            events.append("close")

    upstream.module("modelexpress.engines.vllm.adapter", VllmAdapter=VllmAdapter)
    upstream.module(
        "gpustack_model_transfer.adapters.vllm.native", NativeModelTransferLoader=Native
    )
    adapter = VllmAdapter()
    ctx = upstream.chain.LoadContext(
        model_config=adapter.model_config,
        load_config=adapter.vllm_config.load_config,
        target_device=types.SimpleNamespace(type="cuda"),
        global_rank=0,
        worker_rank=0,
        local_rank=0,
        device_id=0,
        identity=types.SimpleNamespace(model_name="80b"),
        mx_client=object(),
        worker_id="test",
        adapter=adapter,
    )
    return types.SimpleNamespace(ctx=ctx, native=Native, adapter=adapter)


def test_real_chain_loads_same_model_and_finalizes_once(upstream, load_context):
    model = upstream.model_class()
    loaded = upstream.chain.LoadStrategyChain.run(model, load_context.ctx)
    assert loaded is model
    assert upstream.events == ["native", "apply", "close", "finalize"]
    assert load_context.adapter.envelopes[0] is load_context.adapter.envelopes[1]
    assert load_context.ctx.p2p_enabled is False
    assert load_context.adapter.envelopes[0].publishable is False


@pytest.fixture
def collective(upstream, load_context, monkeypatch):
    state = types.SimpleNamespace(peer_change=None, fail=False, leave_peer_empty=False)
    device_group = object()
    group = types.SimpleNamespace(world_size=2, device_group=device_group)
    load_context.adapter.vllm_config.parallel_config.tensor_parallel_size = 2
    upstream.module("vllm.distributed", get_tp_group=lambda: group)

    def gather(proposals, proposal, *, group):
        assert group is device_group
        assert upstream.events == []
        upstream.events.append("agreement")
        if state.fail:
            raise RuntimeError("collective failed")
        proposals[:] = [proposal, proposal]
        if state.leave_peer_empty:
            proposals[1] = None
        elif state.peer_change is not None:
            index, value = state.peer_change
            peer = list(proposal)
            peer[index] = value
            proposals[1] = tuple(peer)

    monkeypatch.setattr(
        sys.modules["torch"],
        "distributed",
        types.SimpleNamespace(all_gather_object=gather),
        raising=False,
    )
    return state


@pytest.mark.parametrize(
    "peer_change",
    [(0, "default"), (1, "hixl"), (2, "1.5.2"), (3, False), (4, "different-mode")],
)
def test_group_disagreement_prevents_native_loading(
    upstream, load_context, collective, peer_change
):
    collective.peer_change = peer_change
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    assert upstream.events == ["agreement"]


@pytest.mark.parametrize(
    "failure", ["collective", "empty-peer", "local-unavailable", "probe-error"]
)
def test_failed_group_readiness_prevents_native_loading(
    upstream, load_context, collective, monkeypatch, failure
):
    if failure == "collective":
        collective.fail = True
    elif failure == "empty-peer":
        collective.leave_peer_empty = True
    elif failure == "local-unavailable":
        upstream.readiness.ready = False
    else:

        def unavailable():
            raise RuntimeError("backend cannot import")

        monkeypatch.setattr(upstream.runtime, "probe_runtime", unavailable)
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    assert upstream.events == ["agreement"]


def test_matching_loading_group_agrees_before_native_loading(
    upstream, load_context, collective
):
    model = upstream.model_class()
    assert upstream.chain.LoadStrategyChain.run(model, load_context.ctx) is model
    assert upstream.events == ["agreement", "native", "apply", "close", "finalize"]


def test_single_rank_missing_backend_cannot_start_native_loading(
    upstream, load_context
):
    upstream.readiness.ready = False
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    assert upstream.events == []


@pytest.mark.parametrize(
    "failure",
    [
        "missing-hook",
        "prefer",
        "wrong-backend",
        "reload",
        "wrong-engine",
        "load",
        "finalize",
    ],
)
def test_real_chain_terminal_errors_never_fall_back(
    upstream, load_context, monkeypatch, failure
):
    if failure == "missing-hook":
        monkeypatch.delenv("MX_LOAD_PLUGIN")
    elif failure == "prefer":
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_POLICY", "prefer")
    elif failure == "wrong-backend":
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_BACKEND", "hixl")
    elif failure == "reload":
        load_context.ctx.is_reload = True
    elif failure == "wrong-engine":
        load_context.ctx.adapter = object()
    elif failure == "load":
        load_context.native.fail_load = True
    elif failure == "finalize":
        load_context.adapter.fail_finalize = True
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    assert "fallback" not in upstream.events
    assert "reinit" not in upstream.events
    assert upstream.events.count("finalize") <= 1


@pytest.mark.parametrize(
    "args",
    [
        [],
        ["--load-format", "model-transfer"],
        ["--load-format", "gpustack-modelexpress", "--load-format=auto"],
    ],
)
def test_launcher_refuses_missing_or_competing_loader(upstream, args):
    launcher = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.launch"
    )
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        launcher.validate_launch(args)


def test_launcher_accepts_explicit_real_plugin(upstream):
    launcher = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.launch"
    )
    launcher.validate_launch(["--load-format", "gpustack-modelexpress"])


@pytest.mark.parametrize(
    "plugin_contents", [None, "raise RuntimeError('broken plugin')\n"]
)
def test_launcher_refuses_absent_or_broken_plugin_file(upstream, plugin_contents):
    plugin_file = Path(upstream.envs.MX_PLUGIN_DIR) / "mx_plugin_gpustack.py"
    if plugin_contents is None:
        plugin_file.unlink()
    else:
        plugin_file.write_text(plugin_contents)
    launcher = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.launch"
    )
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        launcher.validate_launch(["--load-format", "gpustack-modelexpress"])
    assert upstream.events == []


@pytest.mark.parametrize("entry_point", ["launcher", "loader"])
def test_same_name_foreign_plugin_cannot_replace_remote_loader(
    upstream, derived_loader, entry_point
):
    class ForeignStrategy(upstream.chain.LoadStrategy):
        name = "gpustack"

        def load(self, result, ctx):
            upstream.events.append("foreign-plugin")
            return result

    upstream.plugins.initialize_plugins()
    upstream.plugins.plugins["gpustack"] = ForeignStrategy
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        if entry_point == "launcher":
            launcher = importlib.import_module(
                "gpustack_model_transfer.adapters.modelexpress.launch"
            )
            launcher.validate_launch(["--load-format", "gpustack-modelexpress"])
        else:
            derived_loader.load_model(
                types.SimpleNamespace(model=object(), speculative_config=None),
                object(),
            )
    assert upstream.events == []


def test_finalization_failure_requires_process_replacement(upstream, load_context):
    load_context.adapter.fail_finalize = True
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    before = list(upstream.events)
    load_context.adapter.fail_finalize = False
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    assert upstream.events == before


@pytest.fixture
def derived_loader(upstream):
    events = upstream.events

    class MxModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def download_model(self, model_config):
            pytest.fail("the default MX checkpoint download must not run")

        def load_model(self, vllm_config, model_config, prefix):
            events.append("mx-load-model")
            return vllm_config.model

        def _load_model(
            self, vllm_config, model_config, prefix, ctx, is_speculative_draft, model
        ):
            assert ctx.p2p_enabled is False
            events.append("mx-model-setup-without-publication")
            return vllm_config.model

        def _run_load_strategy_chain(self, model, ctx):
            assert ctx.p2p_enabled is False
            events.append("mx-chain-without-publication")
            return model

    class Native:
        def __init__(self, load_config):
            pass

        def download_model(self, model_config):
            events.append("authenticated-metadata-only")

    upstream.module("modelexpress.engines.vllm.loader", MxModelLoader=MxModelLoader)
    upstream.module(
        "gpustack_model_transfer.adapters.vllm.native", NativeModelTransferLoader=Native
    )
    module = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.vllm"
    )
    config = types.SimpleNamespace(load_format="gpustack-modelexpress")
    return module.GpustackModelExpressLoader(config)


def test_derived_loader_probes_only_metadata(upstream, derived_loader):
    derived_loader.download_model(object())
    assert upstream.events == ["authenticated-metadata-only"]


def test_derived_loader_runs_real_mx_parent_entry(upstream, derived_loader):
    config = types.SimpleNamespace(model=object(), speculative_config=None)
    assert derived_loader.load_model(config, object()) is config.model
    assert upstream.events == ["mx-load-model"]


def test_derived_loader_disables_publication_before_parent_model_setup(
    upstream, derived_loader, load_context
):
    config = types.SimpleNamespace(model=object())
    ctx = load_context.ctx
    ctx.p2p_enabled = True
    assert derived_loader._load_model(config, object(), "", ctx, False) is config.model
    assert upstream.events == ["mx-model-setup-without-publication"]


@pytest.mark.parametrize("draft,model", [(True, None), (False, object())])
def test_derived_loader_blocks_reload_or_draft_before_parent_model_setup(
    upstream, derived_loader, load_context, draft, model
):
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        derived_loader._load_model(
            object(), object(), "", load_context.ctx, draft, model
        )
    assert upstream.events == []


@pytest.mark.parametrize("method", ["standalone", "reload"])
def test_derived_loader_refuses_other_mutation_routes(upstream, derived_loader, method):
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        if method == "standalone":
            derived_loader.load_weights(object(), object())
        else:
            derived_loader.reload_model(object(), object(), object())
    assert upstream.events == []


def test_derived_loader_rechecks_hook_before_model_work(
    upstream, derived_loader, monkeypatch
):
    monkeypatch.delenv("MX_LOAD_PLUGIN")
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        derived_loader.download_model(object())
    with pytest.raises(upstream.chain.StrategyRecoveryError):
        derived_loader.load_model(object(), object())
    assert upstream.events == []


def test_optional_registration_does_not_import_mx_for_native(upstream, monkeypatch):
    registration = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.registration"
    )
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT", "model-transfer")
    monkeypatch.setitem(
        sys.modules, "gpustack_model_transfer.adapters.modelexpress.vllm", None
    )
    registration.register()


def test_explicit_registration_rejects_missing_mx_loader(upstream, monkeypatch):
    registration = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.registration"
    )
    upstream.module(
        "vllm.model_executor.model_loader",
        register_model_loader=lambda name: lambda cls: cls,
    )
    monkeypatch.setitem(
        sys.modules, "gpustack_model_transfer.adapters.modelexpress.vllm", None
    )
    with pytest.raises(ModuleNotFoundError):
        registration.register()
