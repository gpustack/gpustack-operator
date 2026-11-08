"""Run the pinned upstream chain and plugin registration with CPU doubles."""

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
        load_weights_calls = 0

        def load_weights(self, weights_iter):
            Model.load_weights_calls += 1
            for _ in weights_iter:
                pass
            return

    torch = module("torch", Tensor=object, device=object)
    torch.nn = module("torch.nn", Module=Model)
    mx = module("modelexpress")
    mx.__path__ = [str(package)]
    # The stub envs module reads the process environment like the real
    # one, so the runtime patch's insertion-point normalization is
    # observable through it.
    envs = module(
        "modelexpress.envs",
        MX_PLUGIN_DIR=str(tmp_path),
        MX_LOAD_STRATEGY_CHAIN="INFERENCE",
    )

    def _envs_getattr(name):
        # The real module resolves every variable dynamically from the
        # process environment, and an unknown name is simply unset.
        return os.environ.get(name)

    envs.__getattr__ = _envs_getattr
    module("modelexpress.p2p_pb2")
    module(
        "modelexpress.accelerators",
        AcceleratorBackend=object,
        CudaAcceleratorBackend=object,
    )
    module("modelexpress.client", MxClientBase=object)
    module("modelexpress.nixl_transfer", is_nixl_available=lambda: False)
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
    events = []
    for filename, class_name, name, available in (
        ("rdma_strategy", "RdmaStrategy", "rdma", False),
        ("server_cache_strategy", "ServerCacheStrategy", "server-cache", False),
        ("instant_tensor_strategy", "InstantTensorStrategy", "instant_tensor", False),
        ("model_streamer_strategy", "ModelStreamerStrategy", "model_streamer", False),
        ("gds_strategy", "GdsStrategy", "gds", False),
        ("default_strategy", "DefaultStrategy", "default", True),
    ):

        def make_double(name=name, available=available, class_name=class_name):
            class Double:
                def __init__(self):
                    events.append(f"build:{name}")

                def is_available(self, ctx):
                    if available:
                        events.append(f"available:{name}")
                    return available

                def prepare(self, result, ctx):
                    events.append(f"prepare:{name}")

                def abort(self, result, ctx):
                    pass

                def rollback(self, ctx):
                    pass

                def load(self, result, ctx):
                    events.append(f"load:{name}")
                    return result

                def finalize(self, result, ctx):
                    events.append(f"finalize:{name}")

            Double.__name__ = class_name
            Double.name = name
            return Double

        module(
            "modelexpress.load_strategy." + filename,
            **{class_name: make_double()},
        )
    monkeypatch.setenv("MX_LOAD_PLUGIN", "gpustack")
    monkeypatch.setenv("MX_LOAD_PLUGIN_BEFORE", "server-cache")
    monkeypatch.setenv("MX_LOAD_STRATEGY_CHAIN", "INFERENCE")
    monkeypatch.setenv("MX_SERVER_ADDRESS", "mx:8001")
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Prefer")
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_BACKEND", "nixl")
    monkeypatch.setenv("HF_HUB_OFFLINE", "1")
    monkeypatch.setenv("TRANSFORMERS_OFFLINE", "1")
    monkeypatch.setenv("MX_ARTIFACT_TRANSFER", "0")
    monkeypatch.setenv("MX_VMM_ARENA", "0")
    chain = importlib.import_module("modelexpress.load_strategy")
    plugins = importlib.import_module("modelexpress.plugins")
    registration = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.registration"
    )
    plugin_module = importlib.import_module(
        "gpustack_model_transfer.adapters.modelexpress.plugin"
    )
    yield types.SimpleNamespace(
        chain=chain,
        plugins=plugins,
        registration=registration,
        plugin_module=plugin_module,
        events=events,
        module=module,
        model_class=Model,
        envs=envs,
    )


@pytest.fixture
def load_context(upstream):
    events = upstream.events

    class VllmAdapter:
        engine_name = "vllm"

        def __init__(self):
            self.vllm_config = types.SimpleNamespace()
            self.model_config = types.SimpleNamespace()
            self.load_config = types.SimpleNamespace()
            self.envelopes = []

        def after_native_load(self, result):
            self.envelopes.append(result)
            events.append("after_native_load")
            return result

        def reinit_for_retry(self, result):
            events.append("reinit_for_retry")
            return result

    class Native:
        fail_download = False
        fail_load = False

        def __init__(self, load_config):
            pass

        def download_model(self, model_config):
            events.append("download_model")
            if self.fail_download:
                raise RuntimeError("no ready source for the digest")

        def load_initialized_model(
            self, model, vllm_config, model_config, *, consume_weights=None
        ):
            events.append("load_initialized_model")
            if consume_weights is None:
                raise AssertionError("the topology-checked entry ran bare")
            consume_weights(iter(()))
            if self.fail_load:
                raise RuntimeError("transfer failed mid-stream")

        def load_weights(self, model, model_config):
            events.append("load_weights:BARE")

    upstream.module("modelexpress.engines.vllm.adapter", VllmAdapter=VllmAdapter)
    upstream.module(
        "gpustack_model_transfer.adapters.vllm.native",
        NativeModelTransferLoader=Native,
    )
    adapter = VllmAdapter()
    ctx = upstream.chain.LoadContext(
        model_config=adapter.model_config,
        load_config=adapter.load_config,
        target_device=types.SimpleNamespace(type="cuda"),
        global_rank=0,
        worker_rank=0,
        local_rank=0,
        device_id=0,
        identity=types.SimpleNamespace(model_name="80b"),
        mx_client=object(),
        worker_id="test",
        adapter=adapter,
        p2p_enabled=True,
    )
    return types.SimpleNamespace(ctx=ctx, native=Native, adapter=adapter)


def test_real_upstream_discovers_strategy_class(upstream):
    upstream.registration.register_strategy_with_modelexpress()
    strategy = upstream.plugins.get_plugin("gpustack")
    assert isinstance(strategy, upstream.chain.LoadStrategy)
    assert strategy.name == "gpustack"


def test_real_registration_is_idempotent_across_callers(upstream):
    """bootstrap → general_plugins → loader hook all land one class."""
    upstream.registration.register_strategy_with_modelexpress()
    first = upstream.plugins.plugins["gpustack"]
    upstream.registration.register_strategy_with_modelexpress()
    upstream.registration.register_strategy_with_modelexpress()
    assert upstream.plugins.plugins["gpustack"] is first


def test_the_general_plugins_entry_gates_on_the_route_request(upstream):
    # Nothing in this process asked for the route: the entry is a no-op
    # and the registry stays empty.
    os.environ.pop("MX_LOAD_PLUGIN", None)
    os.environ.pop("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT", None)
    assert "--load-format" not in " ".join(sys.argv)
    upstream.registration.register()
    assert "gpustack" not in upstream.plugins.plugins
    # With the plugin hook set, registration lands exactly once.
    os.environ["MX_LOAD_PLUGIN"] = "gpustack"
    upstream.registration.register()
    upstream.registration.register()
    assert upstream.plugins.plugins["gpustack"].name == "gpustack"


def test_a_require_registration_failure_stops_the_engine_startup(upstream, monkeypatch):
    """Under Require a failed arm propagates: the chain must not run unenforced."""

    class ForeignStrategy:
        name = "gpustack"

    upstream.plugins.plugins["gpustack"] = ForeignStrategy
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Require")
    with pytest.raises(ValueError):
        upstream.registration.register()


def test_a_prefer_registration_failure_is_swallowed(upstream, monkeypatch, caplog):
    """Under Prefer the advisory entry logs and lets the engine start."""

    class ForeignStrategy:
        name = "gpustack"

    upstream.plugins.plugins["gpustack"] = ForeignStrategy
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Prefer")
    upstream.registration.register()
    assert any(
        "ModelExpress registration unavailable" in record.getMessage()
        for record in caplog.records
    )


def test_a_missing_vllm_keeps_the_require_start_quiet(upstream, monkeypatch):
    """No vLLM in the container is not an enforcement failure: the strategy registered."""
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Require")
    upstream.registration.register()
    assert upstream.plugins.plugins["gpustack"].name == "gpustack"


def test_real_chain_places_the_plugin_between_rdma_and_the_cache(upstream):
    upstream.registration.register_strategy_with_modelexpress()
    upstream.registration.install_modelexpress_runtime_patch()
    model = upstream.model_class()
    ctx = types.SimpleNamespace(
        adapter=None,
        global_rank=0,
        identity=types.SimpleNamespace(model_name="80b"),
        is_reload=False,
        p2p_enabled=False,
        nixl_manager=None,
    )
    # The launch env names the built-in strategy exactly as upstream
    # does, so the chain accepts its own insertion point.
    assert os.environ["MX_LOAD_PLUGIN_BEFORE"] == "server-cache"
    loaded = upstream.chain.LoadStrategyChain.run(model, ctx)
    assert loaded is model
    assert os.environ["MX_LOAD_PLUGIN_BEFORE"] == "server-cache"
    built = [event for event in upstream.events if event.startswith("build:")]
    assert built == [
        "build:rdma",
        "build:server-cache",
        "build:instant_tensor",
        "build:model_streamer",
        "build:gds",
        "build:default",
    ]
    # The default double is the only available one, and it ran last.
    assert upstream.events.count("available:default") == 1
    assert upstream.events.index("build:default") < upstream.events.index(
        "available:default"
    )


_BUILDS = [
    "build:rdma",
    "build:server-cache",
    "build:instant_tensor",
    "build:model_streamer",
    "build:gds",
    "build:default",
]


def test_prefer_chain_falls_through_to_default_without_a_ready_source(
    upstream, load_context
):
    upstream.registration.register_strategy_with_modelexpress()
    upstream.registration.install_modelexpress_runtime_patch()
    load_context.native.fail_download = True
    model = upstream.model_class()
    loaded = upstream.chain.LoadStrategyChain.run(model, load_context.ctx)
    assert loaded is model
    assert "available:server-cache" not in upstream.events
    assert "available:default" in upstream.events
    assert upstream.events.count("download_model") == 1
    assert "load:default" in upstream.events
    assert "reinit_for_retry" not in upstream.events
    assert upstream.events == [
        *_BUILDS,
        "available:default",
        "download_model",
        "prepare:default",
        "load:default",
        "finalize:default",
    ]


def test_require_chain_terminates_without_a_ready_source(
    upstream, load_context, monkeypatch
):
    upstream.registration.register_strategy_with_modelexpress()
    upstream.registration.install_modelexpress_runtime_patch()
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Require")
    load_context.native.fail_download = True
    with pytest.raises(RuntimeError):
        upstream.chain.LoadStrategyChain.run(upstream.model_class(), load_context.ctx)
    assert upstream.events.count("download_model") == 1
    assert "available:server-cache" not in upstream.events
    assert "available:default" not in upstream.events
    assert "load:default" not in upstream.events


def test_successful_gpustack_load_publishes(upstream, load_context):
    upstream.registration.register_strategy_with_modelexpress()
    upstream.registration.install_modelexpress_runtime_patch()
    model = upstream.model_class()
    loaded = upstream.chain.LoadStrategyChain.run(model, load_context.ctx)
    assert loaded is model
    assert upstream.events == [
        *_BUILDS,
        "available:default",
        "download_model",
        "load_initialized_model",
        "after_native_load",
    ]
    assert "load_weights:BARE" not in upstream.events
    assert load_context.ctx.p2p_enabled is True
    assert load_context.ctx.adapter is load_context.adapter
    # The strategy's result envelope is the publishable one the chain
    # publishes through publish_source_if_supported.
    assert load_context.adapter.envelopes[0].publishable is True
    assert load_context.adapter.envelopes[0].strategy_name == "gpustack"


def test_a_post_mutation_failure_reinits_and_falls_back(upstream, load_context):
    upstream.registration.register_strategy_with_modelexpress()
    upstream.registration.install_modelexpress_runtime_patch()
    load_context.native.fail_load = True
    model = upstream.model_class()
    loaded = upstream.chain.LoadStrategyChain.run(model, load_context.ctx)
    assert loaded is model
    assert upstream.events == [
        *_BUILDS,
        "available:default",
        "download_model",
        "load_initialized_model",
        "reinit_for_retry",
        "prepare:default",
        "load:default",
        "finalize:default",
    ]


def test_the_runtime_patch_is_idempotent(upstream):
    upstream.registration.install_modelexpress_runtime_patch()
    patched = upstream.chain.execute_load_strategies
    assert getattr(patched, "_gpustack_model_transfer_filtered", False)
    upstream.registration.install_modelexpress_runtime_patch()
    assert upstream.chain.execute_load_strategies is patched
