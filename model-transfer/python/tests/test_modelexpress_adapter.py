"""The ModelExpress adapter: chain filter, strategy phases, and publication.

The unit tests run the strategy against a minimal fake of the pinned
upstream modules, so every binding (the strategy base, the expected-miss
error, the chain executor) resolves through the same import surface the
real package presents. Where a test needs the fallback behavior instead,
it uses the module as imported without any fake installed.
"""

import importlib
import os
import sys
import types
from abc import ABC
from contextlib import contextmanager
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.adapters.modelexpress import (
    INSERT_BEFORE,
    LOAD_FORMAT,
    LOAD_FORMAT_ALIASES,
    PLUGIN_NAME,
    DeliveryPolicy,
    filter_chain_strategies,
    install_modelexpress_runtime_patch,
)
from gpustack_model_transfer.adapters.modelexpress.chain import (
    build_chain,
    check_composition,
)
from gpustack_model_transfer.adapters.modelexpress.constants import (
    FALLBACK_STRATEGIES as FALLBACK_NAMES,
)

_STRATEGY_MODULE = "gpustack_model_transfer.adapters.modelexpress.plugin"


@pytest.fixture(autouse=True)
def clean_process_env():
    saved = dict(os.environ)
    for name in (
        "MX_LOAD_PLUGIN",
        "MX_LOAD_PLUGIN_BEFORE",
        "MX_ARTIFACT_TRANSFER",
        "MX_SERVER_ADDRESS",
        "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT",
        "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY",
    ):
        os.environ.pop(name, None)
    yield
    os.environ.clear()
    os.environ.update(saved)


class FakeStrategy:
    """A built-in chain strategy double with one controllable behavior."""

    def __init__(self, name, *, available=True, fail_load=False, events=None):
        self.name = name
        self.available = available
        self.fail_load = fail_load
        self.events = events if events is not None else []

    def is_available(self, ctx):
        return self.available

    def prepare(self, result, ctx):
        return None

    def load(self, result, ctx):
        self.events.append(f"{self.name}:load")
        if self.fail_load:
            raise RuntimeError(f"{self.name} could not load")
        return result

    def finalize(self, result, ctx):
        self.events.append(f"{self.name}:finalize")

    def rollback(self, ctx):
        return None


def run_chain(strategies, ctx, model, events):
    """Mirror the upstream chain loop: eligible filter and mutated reinit."""
    StrategyFailed = sys.modules["modelexpress.adapter"].StrategyFailed
    eligible = [strategy for strategy in strategies if strategy.is_available(ctx)]
    result = SimpleNamespace(
        value=model, model=model, publishable=True, strategy_name=""
    )
    for strategy in eligible:
        try:
            strategy.prepare(result, ctx)
            result = strategy.load(result, ctx)
            strategy.finalize(result, ctx)
            return result
        except StrategyFailed as exc:  # noqa: PERF203 — mirrors the upstream loop
            events.append(f"failed:{strategy.name}:mutated={exc.mutated}")
            strategy.rollback(ctx)
            if exc.mutated:
                result = ctx.adapter.reinit_for_retry(result)
            continue
    raise RuntimeError("no strategy succeeded")


def install_fake_upstream(monkeypatch, events):
    """A minimal fake of the pinned upstream modules the adapter binds to."""

    class StrategyFailed(RuntimeError):
        def __init__(self, message, *, mutated=False):
            super().__init__(message)
            self.mutated = mutated

    class LoadStrategy(ABC):  # noqa: B024 — mirrors the upstream base
        name = ""
        requires = ()

        def prepare(self, result, ctx):
            return None

        def post_process(self, result, ctx):
            return None

        def finalize(self, result, ctx):
            events.append("upstream_finalize")

        def abort(self, result, ctx):
            return None

        def is_available(self, ctx):
            return True

        def rollback(self, ctx):
            return None

    def execute_load_strategies(model, ctx, strategies):
        events.append("execute:" + ",".join(getattr(s, "name", "") for s in strategies))
        return model

    plugins = {}

    def register_plugin(name, strategy_class):
        if name in plugins:
            raise ValueError(f"Plugin {name!r} is already registered")
        plugins[name] = strategy_class

    def mx_register_module(name, **attrs):
        module = types.ModuleType(name)
        module.__dict__.update(attrs)
        monkeypatch.setitem(sys.modules, name, module)
        return module

    mx_register_module("modelexpress", __path__=[])
    mx_register_module("modelexpress.adapter", StrategyFailed=StrategyFailed)
    base_mod = mx_register_module(
        "modelexpress.load_strategy.base", LoadStrategy=LoadStrategy
    )
    chain_mod = mx_register_module(
        "modelexpress.load_strategy",
        execute_load_strategies=execute_load_strategies,
        LoadStrategy=LoadStrategy,
    )
    mx_register_module(
        "modelexpress.plugins", register_plugin=register_plugin, plugins=plugins
    )
    return SimpleNamespace(
        events=events,
        StrategyFailed=StrategyFailed,
        LoadStrategy=LoadStrategy,
        base_mod=base_mod,
        chain_mod=chain_mod,
        plugins=plugins,
    )


def install_fake_native_loaders(monkeypatch, events):
    """The native engine loaders as controllable doubles.

    The vLLM double records which entry ran: the bare load_weights entry
    has no topology proof and must never be taken.
    """
    flags = SimpleNamespace(
        fail_download=False, fail_write=False, fail_topology=False, fail_plan=False
    )

    class FakeVllmLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def download_model(self, model_config):
            events.append("vllm:download_model")
            if flags.fail_download:
                raise RuntimeError("no ready source for the digest")

        def load_initialized_model(
            self, model, vllm_config, model_config, *, consume_weights=None
        ):
            events.append("vllm:load_initialized_model")
            if flags.fail_topology:
                raise RuntimeError("engine layout does not match the checkpoint")
            if consume_weights is None:
                raise AssertionError("the topology-checked entry ran bare")
            consume_weights(iter(()))
            if flags.fail_write:
                raise RuntimeError("transfer failed mid-stream")

        def load_weights(self, model, model_config, **kwargs):
            events.append("vllm:load_weights:BARE")

    class FakeSglangLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def download_model(self, model_config):
            events.append("sglang:download_model")
            if flags.fail_download:
                raise RuntimeError("endpoints.json has no sources")

        def load_weights(self, model, model_config):
            events.append("sglang:load_weights")
            if flags.fail_plan:
                raise RuntimeError("engine plan failed before any write")
            if flags.fail_write:
                model.load_weights(iter(()))
                raise RuntimeError("transfer failed mid-stream")
            model.load_weights(iter(()))

    vllm_native = types.ModuleType("gpustack_model_transfer.adapters.vllm.native")
    vllm_native.NativeModelTransferLoader = FakeVllmLoader
    vllm_native._process_unusable = False
    sglang_native = types.ModuleType("gpustack_model_transfer.adapters.sglang.native")
    sglang_native.NativeSGLangModelTransferLoader = FakeSglangLoader
    sglang_native._process_unusable = False
    monkeypatch.setitem(
        sys.modules, "gpustack_model_transfer.adapters.vllm.native", vllm_native
    )
    monkeypatch.setitem(
        sys.modules, "gpustack_model_transfer.adapters.sglang.native", sglang_native
    )
    return SimpleNamespace(vllm=vllm_native, sglang=sglang_native, flags=flags)


def fresh_strategy_module(monkeypatch):
    """Import the strategy module again so its base binds to the fakes."""
    monkeypatch.delitem(sys.modules, _STRATEGY_MODULE, raising=False)
    return importlib.import_module(_STRATEGY_MODULE)


class FakeVllmAdapter:
    engine_name = "vllm"

    def __init__(self, events, *, fail_postprocess=False):
        self.events = events
        self.vllm_config = SimpleNamespace()
        self.load_config = SimpleNamespace(load_format="modelexpress")
        self.model_config = SimpleNamespace(model="tiny-dense")
        self.fail_postprocess = fail_postprocess

    def after_native_load(self, result):
        self.events.append("after_native_load")
        if self.fail_postprocess:
            raise RuntimeError("postprocess failed")
        return result

    def reinit_for_retry(self, result):
        self.events.append("reinit_for_retry")
        return result

    def finalize_as_source(self, result, nixl_manager):
        self.events.append(("finalize_as_source", nixl_manager))


class FakeSglangAdapter:
    engine_name = "sglang"

    def __init__(self, events, *, fail_postprocess=False):
        self.events = events
        self.load_config = SimpleNamespace(load_format="modelexpress")
        self.model_config = SimpleNamespace(model="tiny-dense")
        self.fail_postprocess = fail_postprocess

    def after_weight_iter_load(self, result):
        self.events.append("after_weight_iter_load")
        if self.fail_postprocess:
            raise RuntimeError("postprocess failed")
        return result

    def reinit_for_retry(self, result):
        self.events.append("reinit_for_retry")
        return result

    def finalize_as_source(self, result, nixl_manager):
        self.events.append(("finalize_as_source", nixl_manager))


def make_model(events=None):
    calls = []

    def load_weights(weights_iter):
        calls.append("model.load_weights")

    return SimpleNamespace(load_weights=load_weights), calls


def make_context(
    adapter,
    *,
    model=None,
    p2p_enabled=True,
    nixl_manager="nixl-1",
    is_reload=False,
    p2p_role="main",
):
    return SimpleNamespace(
        adapter=adapter,
        result=SimpleNamespace(model=model if model is not None else object()),
        p2p_enabled=p2p_enabled,
        nixl_manager=nixl_manager,
        accelerator_backend="cuda",
        global_rank=0,
        is_reload=is_reload,
        p2p_role=p2p_role,
    )


def set_env(monkeypatch, **values):
    for name, value in values.items():
        if value is None:
            os.environ.pop(name, None)
        else:
            os.environ[name] = value


# ---------------------------------------------------------------------------
# Constants and delivery policy
# ---------------------------------------------------------------------------


def test_the_route_constants_pin_the_standard_formats_and_placement():
    assert LOAD_FORMAT == "modelexpress"
    assert LOAD_FORMAT_ALIASES == ("modelexpress", "mx")
    assert PLUGIN_NAME == "gpustack"
    assert INSERT_BEFORE == "server-cache"
    assert "gpustack-modelexpress" not in LOAD_FORMAT_ALIASES


def test_the_delivery_policy_defaults_to_prefer():
    from gpustack_model_transfer.adapters.modelexpress import delivery_policy

    assert delivery_policy({}) == DeliveryPolicy(allow_fallback=True)
    assert delivery_policy({"GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY": ""}) == (
        DeliveryPolicy(allow_fallback=True)
    )


def test_the_delivery_policy_reads_the_environment():
    from gpustack_model_transfer.adapters.modelexpress import delivery_policy

    assert delivery_policy(
        {"GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY": "Require"}
    ) == DeliveryPolicy(allow_fallback=False)
    assert delivery_policy(
        {"GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY": "PREFER"}
    ) == DeliveryPolicy(allow_fallback=True)


# ---------------------------------------------------------------------------
# Chain composition and the delivery-policy filter
# ---------------------------------------------------------------------------


def test_the_composed_chain_places_the_plugin_before_the_local_strategies():
    chain = build_chain({"rdma": True, "server_cache": True, "default": True})
    assert chain == ("rdma", "gpustack", "server-cache", "default")


def test_the_composed_chain_accepts_the_upstream_cache_spelling():
    chain = build_chain({"rdma": True, "server-cache": True, "default": True})
    assert chain == ("rdma", "gpustack", "server-cache", "default")


def test_the_composition_guard_holds_for_every_enabled_combination():
    for enabled in (
        {"rdma": True},
        {"rdma": True, "server_cache": True},
        {"rdma": True, "server-cache": True},
        {"server_cache": True, "default": True},
        {},
    ):
        composed = check_composition(build_chain(enabled))
        plugin_index = composed.index(PLUGIN_NAME)
        for index, name in enumerate(composed):
            if name == "rdma":
                assert index < plugin_index
            if name in FALLBACK_NAMES:
                assert index > plugin_index


def test_the_strategy_rebase_names_instances_made_against_the_stand_in(
    monkeypatch, caplog
):
    """A rebase over live stand-in instances must name the mixed state."""
    import logging

    from gpustack_model_transfer.adapters.modelexpress.registration import (
        register_strategy_with_modelexpress,
    )

    # Import the strategy module before the upstream base exists, so its
    # class binds the local stand-in — the bootstrap's real sequence.
    plugin = fresh_strategy_module(monkeypatch)
    strategy = plugin.GpustackLoadStrategy()
    assert plugin.GpustackLoadStrategy._instantiations == 1
    install_fake_upstream(monkeypatch, [])
    with caplog.at_level(logging.WARNING):
        registered = register_strategy_with_modelexpress()
    assert registered is plugin.GpustackLoadStrategy
    assert any(
        "instances keep the stand-in behavior" in record.getMessage()
        for record in caplog.records
    )
    # The live instance was built on the stand-in; the class it belongs
    # to is the one the rebase rebinding changed.
    assert isinstance(strategy, registered)
    assert registered.__bases__ == (
        sys.modules["modelexpress.load_strategy.base"].LoadStrategy,
    )
    # Rebind the shared class to its stand-in base: other tests hold the
    # same class object, and the fake base must not leak into them.
    plugin.GpustackLoadStrategy.__bases__ = (plugin._FallbackLoadStrategy,)
    plugin._STRATEGY_BASE = plugin._FallbackLoadStrategy


def builtins(events):
    return [
        FakeStrategy("rdma", available=False, events=events),
        FakeStrategy("server-cache", events=events),
        FakeStrategy("instant_tensor", available=False, events=events),
        FakeStrategy("model_streamer", available=False, events=events),
        FakeStrategy("gds", available=False, events=events),
        FakeStrategy("default", events=events),
    ]


def test_prefer_keeps_the_full_chain_with_the_plugin_between_rdma_and_server_cache(
    monkeypatch,
):
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    strategies = builtins([])
    filtered = filter_chain_strategies(strategies)
    assert [s.name for s in filtered] == [
        "rdma",
        "gpustack",
        "server-cache",
        "instant_tensor",
        "model_streamer",
        "gds",
        "default",
    ]


def test_require_strips_every_local_fallback_strategy(monkeypatch):
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    filtered = filter_chain_strategies(builtins([]))
    assert [s.name for s in filtered] == ["rdma", "gpustack"]


def test_a_chain_that_already_carries_the_plugin_is_not_modified_twice(monkeypatch):
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    strategies = [
        FakeStrategy("rdma", available=False, events=[]),
        FakeStrategy(PLUGIN_NAME, events=[]),
        FakeStrategy("server_cache", events=[]),
        FakeStrategy("default", events=[]),
    ]
    filtered = filter_chain_strategies(strategies)
    assert [s.name for s in filtered] == [
        "rdma",
        "gpustack",
        "server_cache",
        "default",
    ]


def test_a_chain_from_a_process_that_never_asked_for_the_plugin_is_unchanged(
    monkeypatch,
):
    set_env(monkeypatch, MX_LOAD_PLUGIN=None)
    strategies = builtins([])
    filtered = filter_chain_strategies(strategies)
    assert filtered == strategies


def test_a_command_line_request_places_the_plugin_without_the_env(monkeypatch):
    """Placement answers to the same gate the runtime patch installs on."""
    set_env(monkeypatch, MX_LOAD_PLUGIN=None)
    monkeypatch.setattr(
        sys, "argv", ["vllm", "serve", "model", "--load-format", "modelexpress"]
    )
    filtered = filter_chain_strategies(builtins([]))
    assert [s.name for s in filtered] == [
        "rdma",
        "gpustack",
        "server-cache",
        "instant_tensor",
        "model_streamer",
        "gds",
        "default",
    ]


def test_a_command_line_request_keeps_only_peer_d2d_and_gpustack_under_require(
    monkeypatch,
):
    """An argv-only process under Require cannot be left with rdma alone."""
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN=None,
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    monkeypatch.setattr(sys, "argv", ["vllm", "serve", "model", "--load-format=mx"])
    filtered = filter_chain_strategies(builtins([]))
    assert [s.name for s in filtered] == ["rdma", "gpustack"]


def test_the_plugin_is_placed_after_rdma_when_no_fallback_precedes_it(monkeypatch):
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    strategies = [FakeStrategy("rdma", events=[])]
    filtered = filter_chain_strategies(strategies)
    assert [s.name for s in filtered] == ["rdma", "gpustack"]


# ---------------------------------------------------------------------------
# The strategy on the chain
# ---------------------------------------------------------------------------


def test_probe_failure_is_a_clean_miss_and_default_loads_without_a_reinit(
    monkeypatch,
):
    upstream = install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events).flags.fail_download = True
    plugin = fresh_strategy_module(monkeypatch)
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Prefer",
    )

    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter)
    strategy = plugin.GpustackLoadStrategy()
    strategies = [
        FakeStrategy("rdma", available=False, events=events),
        strategy,
        FakeStrategy("server-cache", available=False, events=events),
        FakeStrategy("default", events=events),
    ]
    result = run_chain(strategies, ctx, model=object(), events=events)
    assert result is not None
    assert events == [
        "vllm:download_model",
        "failed:gpustack:mutated=False",
        "default:load",
        "default:finalize",
    ]
    assert "reinit_for_retry" not in events
    assert upstream.StrategyFailed is sys.modules["modelexpress.adapter"].StrategyFailed


def test_probe_failure_leaves_the_native_process_unpoisoned(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    native.flags.fail_download = True
    plugin = fresh_strategy_module(monkeypatch)
    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter)
    strategy = plugin.GpustackLoadStrategy()
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert native.vllm._process_unusable is False
    assert native.sglang._process_unusable is False


def test_a_successful_vllm_load_runs_the_topology_checked_entry(monkeypatch):
    """The bare load_weights entry has no topology proof and is never taken."""
    upstream = install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")

    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    strategy = plugin.GpustackLoadStrategy()
    result = strategy.load(ctx.result, ctx)
    assert events == [
        "vllm:download_model",
        "vllm:load_initialized_model",
        "after_native_load",
    ]
    assert "vllm:load_weights:BARE" not in events
    assert result.publishable is True
    assert result.strategy_name == "gpustack"
    strategy.finalize(result, ctx)
    assert "upstream_finalize" in events
    assert ctx.p2p_enabled is True
    assert upstream.events is events


def test_a_vllm_write_time_failure_reports_mutation(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    native.flags.fail_write = True
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is True


def test_require_holds_the_offline_guards_in_the_process_env(monkeypatch):
    """Under Require the process env gains the hub-offline second boundary."""
    import os

    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    result = plugin.GpustackLoadStrategy().load(ctx.result, ctx)
    assert result.model is not None
    assert os.environ["HF_HUB_OFFLINE"] == "1"
    assert os.environ["TRANSFORMERS_OFFLINE"] == "1"


def test_prefer_does_not_touch_the_offline_guards(monkeypatch):
    """Prefer is advisory: the guards are none of its business."""
    import os

    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Prefer")
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    plugin.GpustackLoadStrategy().load(ctx.result, ctx)
    assert "HF_HUB_OFFLINE" not in os.environ


def test_plugin_discovery_adopts_an_upstream_base_that_arrived_late(monkeypatch):
    """The MX_PLUGIN_DIR route must not refuse when the base arrives later."""
    # Force the stand-in binding even when a real modelexpress is on the
    # path from an earlier test: blind every probe target first.
    for probe_target in (
        "modelexpress.load_strategy.base",
        "modelexpress.load_strategy",
        "modelexpress",
    ):
        monkeypatch.setitem(sys.modules, probe_target, None)
    plugin = fresh_strategy_module(monkeypatch)
    assert plugin._STRATEGY_BASE is None
    install_fake_upstream(monkeypatch, [])
    registered = plugin.register_plugin()
    assert registered is plugin.GpustackLoadStrategy
    assert plugin._STRATEGY_BASE is not None
    assert plugin.GpustackLoadStrategy.__bases__ == (plugin._STRATEGY_BASE,)
    # Rebind the shared class back to its stand-in: other tests hold it.
    plugin.GpustackLoadStrategy.__bases__ = (plugin._FallbackLoadStrategy,)
    plugin._STRATEGY_BASE = plugin._FallbackLoadStrategy


def test_plugin_discovery_refuses_an_incompatible_base_and_keeps_the_stand_in(
    monkeypatch,
):
    """A failed rebase must not leave the snapshot on the incompatible base."""
    from gpustack_model_transfer.adapters.modelexpress.policy import (
        PluginUnavailableError,
    )

    for probe_target in (
        "modelexpress.load_strategy.base",
        "modelexpress.load_strategy",
        "modelexpress",
    ):
        monkeypatch.setitem(sys.modules, probe_target, None)
    plugin = fresh_strategy_module(monkeypatch)
    assert plugin._STRATEGY_BASE is None
    install_fake_upstream(monkeypatch, [])
    incompatible = type("IncompatibleBase", (), {})
    monkeypatch.setitem(
        sys.modules,
        "modelexpress.load_strategy.base",
        SimpleNamespace(LoadStrategy=incompatible),
    )
    with pytest.raises(PluginUnavailableError):
        plugin.register_plugin()
    # The snapshot stays on the stand-in so a second discovery call
    # still probes instead of silently skipping.
    assert plugin._STRATEGY_BASE is None
    assert plugin.GpustackLoadStrategy.__bases__ == (plugin._FallbackLoadStrategy,)


def test_a_base_exception_still_restores_the_sglang_load_weights(monkeypatch):
    """Even a BaseException between patch and load must restore the model."""
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    adapter = FakeSglangAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    strategy = plugin.GpustackLoadStrategy()
    original = model.load_weights

    def exploding_factory(backend=None):
        raise KeyboardInterrupt

    monkeypatch.setattr(plugin, "_capture_tensor_attrs_factory", exploding_factory)
    with pytest.raises(KeyboardInterrupt):
        strategy.load(ctx.result, ctx)
    assert model.load_weights is original


def test_a_vllm_pre_write_failure_is_a_clean_miss(monkeypatch):
    """Topology refusal happens before the first write: no mutation, no poison."""
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Prefer")
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    native.flags.fail_topology = True
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert native.vllm._process_unusable is False


def test_a_pre_write_failure_never_lifts_an_earlier_attempt_poison(monkeypatch):
    """A restore puts the flag back, it does not clear a taint it did not earn."""
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Prefer")
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    native.flags.fail_topology = True
    # An earlier load failed mid-write in this process and poisoned it.
    native.vllm._process_unusable = True
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert native.vllm._process_unusable is True


def test_a_vllm_pre_write_failure_stays_clean_under_require(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require")
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    native.flags.fail_topology = True
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert native.vllm._process_unusable is False


def test_a_successful_sglang_load_wraps_the_weights_in_the_tensor_capture(
    monkeypatch,
):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)

    capture_events = []

    @contextmanager
    def capture_tensor_attrs(backend):
        capture_events.append(("enter", backend))
        yield
        capture_events.append("exit")

    module = types.ModuleType("modelexpress.tensor_utils")
    module.capture_tensor_attrs = capture_tensor_attrs
    monkeypatch.setitem(sys.modules, "modelexpress.tensor_utils", module)

    model, _ = make_model(events)
    adapter = FakeSglangAdapter(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    strategy = plugin.GpustackLoadStrategy()
    result = strategy.load(ctx.result, ctx)
    assert events == [
        "sglang:download_model",
        "sglang:load_weights",
        "after_weight_iter_load",
    ]
    assert capture_events == [("enter", "cuda"), "exit"]
    assert result.publishable is True
    assert result.strategy_name == "gpustack"


def test_an_sglang_load_without_the_capture_module_still_loads(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    model, _ = make_model(events)
    adapter = FakeSglangAdapter(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    strategy = plugin.GpustackLoadStrategy()
    result = strategy.load(ctx.result, ctx)
    assert events == [
        "sglang:download_model",
        "sglang:load_weights",
        "after_weight_iter_load",
    ]
    assert result.publishable is True


def test_an_sglang_pre_write_failure_is_a_clean_miss(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Prefer")
    model, model_calls = make_model(events)
    adapter = FakeSglangAdapter(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    native.flags.fail_plan = True
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    # The write boundary never fired: no tensor reached the model.
    assert model_calls == []
    assert native.sglang._process_unusable is False


def test_an_sglang_write_time_failure_reports_mutation(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    model, model_calls = make_model(events)
    adapter = FakeSglangAdapter(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    # The double's write-time failure fires the model callback first.
    native.flags.fail_write = True
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed):
        strategy.load(ctx.result, ctx)
    assert model_calls == ["model.load_weights"]
    # The engine's own callback is restored after the load.
    assert model.load_weights.__name__ == "load_weights"


def test_an_sglang_load_restores_class_method_lookup(monkeypatch):
    """A load_weights resolved from the class leaves no instance attribute."""
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)

    class EngineModel:
        def __init__(self):
            self.calls = []

        def load_weights(self, weights_iter):
            self.calls.append("class.load_weights")

    model = EngineModel()
    adapter = FakeSglangAdapter(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    result = strategy.load(ctx.result, ctx)
    assert model.calls == ["class.load_weights"]
    # The temporary wrapper is gone from the instance dict, so the
    # class method is what lookup resolves again.
    assert "load_weights" not in model.__dict__
    assert model.load_weights.__func__ is EngineModel.load_weights
    assert result.publishable is True


def test_the_reload_guard_refuses_a_streaming_reload(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter, is_reload=True)
    strategy = plugin.GpustackLoadStrategy()
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert events == []


def test_the_draft_guard_refuses_a_speculative_draft_pass(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter, p2p_role="draft")
    strategy = plugin.GpustackLoadStrategy()
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert events == []


def test_require_keeps_only_peer_d2d_and_gpustack_on_the_chain(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events).flags.fail_download = True
    plugin = fresh_strategy_module(monkeypatch)
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )

    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter)
    strategy = plugin.GpustackLoadStrategy()
    strategies = [
        FakeStrategy("rdma", available=False, events=events),
        strategy,
    ]
    with pytest.raises(RuntimeError, match="no strategy succeeded"):
        run_chain(strategies, ctx, model=object(), events=events)
    assert events == [
        "vllm:download_model",
        "failed:gpustack:mutated=False",
    ]


def test_a_mid_load_failure_falls_back_to_default_through_a_reinit(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    native = install_fake_native_loaders(monkeypatch, events)
    native.flags.fail_write = True
    plugin = fresh_strategy_module(monkeypatch)
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")

    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model)
    strategy = plugin.GpustackLoadStrategy()
    strategies = [
        FakeStrategy("rdma", available=False, events=events),
        strategy,
        FakeStrategy("server-cache", available=False, events=events),
        FakeStrategy("default", events=events),
    ]
    result = run_chain(strategies, ctx, model=object(), events=events)
    assert events == [
        "vllm:download_model",
        "vllm:load_initialized_model",
        "failed:gpustack:mutated=True",
        "reinit_for_retry",
        "default:load",
        "default:finalize",
    ]
    assert result is not None


def test_finalize_registers_the_loaded_model_as_a_publishable_source(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    adapter = FakeVllmAdapter(events)
    model, _ = make_model(events)
    ctx = make_context(adapter, model=model, p2p_enabled=True)
    strategy = plugin.GpustackLoadStrategy()
    result = strategy.load(ctx.result, ctx)
    result.publishable = True
    strategy.finalize(result, ctx)
    assert events[-1] == "upstream_finalize"
    assert result.publishable is True


def test_the_fallback_finalizer_hands_the_model_to_finalize_as_source(monkeypatch):
    from gpustack_model_transfer.adapters.modelexpress import GpustackLoadStrategy

    events = []
    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter, p2p_enabled=True, nixl_manager="nixl-agent")
    strategy = GpustackLoadStrategy()
    result = SimpleNamespace(model=object(), publishable=True)
    strategy.finalize(result, ctx)
    assert adapter.events == [("finalize_as_source", "nixl-agent")]


def test_the_fallback_finalizer_skips_unpublishable_models(monkeypatch):
    from gpustack_model_transfer.adapters.modelexpress import GpustackLoadStrategy

    events = []
    adapter = FakeVllmAdapter(events)
    ctx = make_context(adapter, p2p_enabled=False)
    strategy = GpustackLoadStrategy()
    result = SimpleNamespace(model=object(), publishable=False)
    strategy.finalize(result, ctx)
    assert adapter.events == []


# ---------------------------------------------------------------------------
# Availability and engine inference
# ---------------------------------------------------------------------------


def test_availability_requires_a_supported_engine_adapter(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    strategy = plugin.GpustackLoadStrategy()
    assert strategy.is_available(make_context(FakeVllmAdapter(events))) is True
    assert strategy.is_available(make_context(FakeSglangAdapter(events))) is True
    assert strategy.is_available(make_context(object())) is False
    assert strategy.is_available(SimpleNamespace(adapter=None)) is False


def test_availability_reads_the_result_envelope_or_the_configuration_pair(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)
    strategy = plugin.GpustackLoadStrategy()
    adapter = FakeVllmAdapter([])
    empty = SimpleNamespace(adapter=adapter, result=SimpleNamespace(model=None))
    assert strategy.is_available(empty) is False
    configured = SimpleNamespace(adapter=adapter)
    assert strategy.is_available(configured) is True


def test_the_engine_is_inferred_from_the_adapter_class_and_module(monkeypatch):
    install_fake_upstream(monkeypatch, [])
    plugin = fresh_strategy_module(monkeypatch)

    class UpstreamVllmAdapter(FakeVllmAdapter):
        pass

    UpstreamVllmAdapter.engine_name = None
    UpstreamVllmAdapter.__module__ = "modelexpress.engines.vllm.adapter"

    class UpstreamSglangAdapter(FakeSglangAdapter):
        pass

    UpstreamSglangAdapter.__module__ = "modelexpress.engines.sglang.adapter"
    # The module-sniff fallback: the real upstream classes are not
    # importable in this fake, so the defining module decides.
    assert plugin.adapter_engine_name(UpstreamVllmAdapter([])) == "vllm"
    assert plugin.adapter_engine_name(UpstreamSglangAdapter([])) == "sglang"

    class ForeignAdapter(FakeVllmAdapter):
        pass

    ForeignAdapter.engine_name = None
    ForeignAdapter.__module__ = "some.other.module"
    assert plugin.adapter_engine_name(ForeignAdapter([])) == ""


def test_an_unclassified_adapter_is_a_clean_miss_not_a_crash(monkeypatch):
    """A load into an unknown adapter carries the chain's mutated=False miss."""
    install_fake_upstream(monkeypatch, events := [])
    install_fake_native_loaders(monkeypatch, events)
    plugin = fresh_strategy_module(monkeypatch)

    class ForeignAdapter(FakeVllmAdapter):
        pass

    ForeignAdapter.engine_name = None
    ForeignAdapter.__module__ = "some.other.module"
    adapter = ForeignAdapter(events)
    ctx = make_context(adapter)
    strategy = plugin.GpustackLoadStrategy()
    with pytest.raises(sys.modules["modelexpress.adapter"].StrategyFailed) as caught:
        strategy.load(ctx.result, ctx)
    assert caught.value.mutated is False
    assert events == []


# ---------------------------------------------------------------------------
# The runtime patch
# ---------------------------------------------------------------------------


def test_the_runtime_patch_filters_the_chain_executor(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    install_modelexpress_runtime_patch()
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    assert getattr(patched, "_gpustack_model_transfer_filtered", False)
    ctx = SimpleNamespace(adapter=None)
    patched(None, ctx, builtins([]))
    assert events[-1] == (
        "execute:rdma,gpustack,server-cache,instant_tensor,model_streamer,gds,default"
    )
    install_modelexpress_runtime_patch()
    assert sys.modules["modelexpress.load_strategy"].execute_load_strategies is patched


def test_the_runtime_patch_leaves_unrequested_chains_alone(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    set_env(monkeypatch, MX_LOAD_PLUGIN=None)
    install_modelexpress_runtime_patch()
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    patched(None, None, builtins([]))
    assert events[-1] == (
        "execute:rdma,server-cache,instant_tensor,model_streamer,gds,default"
    )


def test_the_runtime_patch_filters_a_keyword_strategies_call(monkeypatch):
    install_fake_upstream(monkeypatch, events := [])
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    install_modelexpress_runtime_patch()
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    ctx = SimpleNamespace(adapter=None)
    patched(None, ctx, strategies=builtins([]))
    assert events[-1] == (
        "execute:rdma,gpustack,server-cache,instant_tensor,model_streamer,gds,default"
    )


def test_the_runtime_patch_follows_a_widened_executor_signature(monkeypatch):
    """Extra keyword-only parameters forward untouched; strategies filters."""
    install_fake_upstream(monkeypatch, events := [])
    chain_mod = sys.modules["modelexpress.load_strategy"]

    def widened_execute_load_strategies(model, ctx, strategies, *, span=None):
        events.append("execute:" + ",".join(s.name for s in strategies))
        return f"span={span}"

    chain_mod.execute_load_strategies = widened_execute_load_strategies
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    install_modelexpress_runtime_patch()
    patched = chain_mod.execute_load_strategies
    assert getattr(patched, "_gpustack_model_transfer_filtered", False)
    assert patched(None, None, builtins([]), span="s1") == "span=s1"
    assert events[-1] == (
        "execute:rdma,gpustack,server-cache,instant_tensor,model_streamer,gds,default"
    )


def test_the_runtime_patch_forwards_extra_positional_arguments(monkeypatch):
    """A caller beyond the pinned arity must not become a load failure."""
    install_fake_upstream(monkeypatch, events := [])
    chain_mod = sys.modules["modelexpress.load_strategy"]

    def widened_execute_load_strategies(model, ctx, strategies, trace=None):
        events.append("execute:" + ",".join(s.name for s in strategies))
        return trace

    chain_mod.execute_load_strategies = widened_execute_load_strategies
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    install_modelexpress_runtime_patch()
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    assert patched(None, None, builtins([]), "t9") == "t9"
    assert events[-1] == (
        "execute:rdma,gpustack,server-cache,instant_tensor,model_streamer,gds,default"
    )


def test_the_runtime_patch_follows_the_named_strategies_parameter_only(monkeypatch):
    """A renamed build must not filter an unrelated trailing argument."""
    install_fake_upstream(monkeypatch, events := [])
    chain_mod = sys.modules["modelexpress.load_strategy"]

    def renamed_execute_load_strategies(engine, context, chain):
        events.append("execute:" + ",".join(getattr(s, "name", "") for s in chain))
        return "loaded"

    chain_mod.execute_load_strategies = renamed_execute_load_strategies
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    install_modelexpress_runtime_patch()
    patched = chain_mod.execute_load_strategies
    assert getattr(patched, "_gpustack_model_transfer_filtered", False)
    # Prefer: the unfiltered chain passes through untouched rather than
    # filter the unrelated third argument.
    assert patched(None, None, builtins([])) == "loaded"
    assert events[-1] == (
        "execute:rdma,server-cache,instant_tensor,model_streamer,gds,default"
    )


def test_the_runtime_patch_fails_the_load_under_require_when_renamed(monkeypatch):
    """Under Require an unfiltered chain is the forbidden fallback itself."""
    from gpustack_model_transfer.adapters.modelexpress import StrategyRecoveryError

    install_fake_upstream(monkeypatch, events := [])
    chain_mod = sys.modules["modelexpress.load_strategy"]

    def renamed_execute_load_strategies(engine, context, chain):
        events.append("execute")
        return "loaded"

    chain_mod.execute_load_strategies = renamed_execute_load_strategies
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    install_modelexpress_runtime_patch()
    with pytest.raises(StrategyRecoveryError):
        chain_mod.execute_load_strategies(None, None, builtins([]))
    assert events == []


def test_the_runtime_patch_does_not_silently_load_a_default_chain(monkeypatch):
    """An omitted strategies argument resolves through the policy too."""
    from gpustack_model_transfer.adapters.modelexpress import StrategyRecoveryError

    install_fake_upstream(monkeypatch, events := [])
    chain_mod = sys.modules["modelexpress.load_strategy"]

    def defaulted_execute_load_strategies(model, ctx, strategies=None):
        events.append("execute:" + ",".join(s.name for s in strategies or []))
        return "loaded"

    chain_mod.execute_load_strategies = defaulted_execute_load_strategies
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    install_modelexpress_runtime_patch()
    with pytest.raises(StrategyRecoveryError):
        chain_mod.execute_load_strategies(None, None)
    assert events == []


def test_the_runtime_patch_warns_and_passes_when_the_signature_is_blind(
    monkeypatch, caplog
):
    """Prefer stays transparent: an uninspectable executor is not filtered."""
    import inspect
    import logging

    install_fake_upstream(monkeypatch, events := [])
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")

    def blind(*args, **kwargs):
        raise ValueError

    # The position is probed once at install time: blind the inspection
    # before installing so the cached position is None.
    monkeypatch.setattr(inspect, "signature", blind)
    install_modelexpress_runtime_patch()
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    with caplog.at_level(logging.WARNING):
        patched(None, None, builtins([]))
    assert events[-1] == (
        "execute:rdma,server-cache,instant_tensor,model_streamer,gds,default"
    )
    assert any(
        "delivery policy was not applied" in record.getMessage()
        for record in caplog.records
    )


def test_the_runtime_patch_fails_the_load_under_require_when_blind(monkeypatch):
    """An uninspectable signature must not bypass the Require policy."""
    import inspect

    from gpustack_model_transfer.adapters.modelexpress import StrategyRecoveryError

    install_fake_upstream(monkeypatch, events := [])
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )

    def blind(*args, **kwargs):
        raise ValueError

    monkeypatch.setattr(inspect, "signature", blind)
    install_modelexpress_runtime_patch()
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    with pytest.raises(StrategyRecoveryError):
        patched(None, None, builtins([]))
    assert events == []


def test_the_runtime_patch_fails_closed_under_require_when_the_executor_is_missing(
    monkeypatch,
):
    """An unwrapped executor under Require must not arm silently."""
    from gpustack_model_transfer.adapters.modelexpress import StrategyRecoveryError

    install_fake_upstream(monkeypatch, [])
    chain_mod = sys.modules["modelexpress.load_strategy"]
    del chain_mod.execute_load_strategies
    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    with pytest.raises(StrategyRecoveryError):
        install_modelexpress_runtime_patch()


def test_the_runtime_patch_warns_under_prefer_when_the_executor_is_missing(
    monkeypatch, caplog
):
    """Prefer stays advisory: the skip is a warning, not a crash."""
    import logging

    install_fake_upstream(monkeypatch, [])
    chain_mod = sys.modules["modelexpress.load_strategy"]
    del chain_mod.execute_load_strategies
    set_env(monkeypatch, MX_LOAD_PLUGIN="gpustack")
    with caplog.at_level(logging.WARNING):
        install_modelexpress_runtime_patch()
    assert any(
        "delivery policy was not applied" in record.getMessage()
        for record in caplog.records
    )
    # The skip left the module unpatched: no executor appeared.
    assert getattr(chain_mod, "execute_load_strategies", None) is None


def test_the_runtime_patch_fails_closed_when_the_chain_module_is_missing(monkeypatch):
    from gpustack_model_transfer.adapters.modelexpress import StrategyRecoveryError

    set_env(
        monkeypatch,
        MX_LOAD_PLUGIN="gpustack",
        GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY="Require",
    )
    monkeypatch.setitem(sys.modules, "modelexpress.load_strategy", None)
    with pytest.raises(StrategyRecoveryError):
        install_modelexpress_runtime_patch()


def test_the_runtime_patch_survives_a_missing_upstream(monkeypatch):
    monkeypatch.setitem(sys.modules, "modelexpress.load_strategy", None)
    install_modelexpress_runtime_patch()
