"""The manual vLLM loader entry: upstream lineage and arm-up behavior.

The fake pinned upstream loader replaces modelexpress.engines.vllm.loader,
so the entry module binds its base class from the fake and no vLLM or
torch import ever runs. The loader must be a subclass of the upstream
base — registry and isinstance compatibility — arm the route (launch
env, runtime patch, strategy registration) before its first load, and
load through the inherited upstream methods.
"""

import importlib
import sys
from types import SimpleNamespace

import pytest

_ENTRY_MODULE = "gpustack_model_transfer.adapters.modelexpress.vllm"


@pytest.fixture(autouse=True)
def clean_process_env(monkeypatch):
    for name in (
        "MX_LOAD_PLUGIN",
        "MX_LOAD_PLUGIN_BEFORE",
        "MX_ARTIFACT_TRANSFER",
        "MX_SERVER_ADDRESS",
        "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT",
        "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY",
    ):
        monkeypatch.delenv(name, raising=False)


@pytest.fixture
def entry(monkeypatch):
    def module(name, **attrs):
        value = SimpleNamespace(**attrs)
        monkeypatch.setitem(sys.modules, name, value)
        return value

    constructed = []

    class FakeMxModelLoader:
        def __init__(self, load_config):
            constructed.append(load_config)
            self.load_config = load_config

        def load_model(self, vllm_config, model_config, prefix=""):
            return model_config.model

        def download_model(self, model_config):
            return model_config

    module("modelexpress", __path__=[])
    module("modelexpress.engines", __path__=[])
    module("modelexpress.engines.vllm", __path__=[])
    module("modelexpress.engines.vllm.loader", MxModelLoader=FakeMxModelLoader)

    class FakeStrategyFailed(RuntimeError):
        def __init__(self, message, *, mutated=False):
            super().__init__(message)
            self.mutated = mutated

    def execute_load_strategies(model, ctx, strategies):
        return model

    plugins = {}

    def register_plugin(name, strategy_class):
        if name in plugins:
            raise ValueError(f"Plugin {name!r} is already registered")
        plugins[name] = strategy_class

    module("modelexpress.adapter", StrategyFailed=FakeStrategyFailed)
    module(
        "modelexpress.load_strategy",
        execute_load_strategies=execute_load_strategies,
    )
    module("modelexpress.plugins", register_plugin=register_plugin, plugins=plugins)

    monkeypatch.delitem(
        sys.modules,
        "gpustack_model_transfer.adapters.modelexpress.plugin",
        raising=False,
    )
    monkeypatch.delitem(sys.modules, _ENTRY_MODULE, raising=False)
    module_value = importlib.import_module(_ENTRY_MODULE)
    sys.modules.pop(_ENTRY_MODULE, None)
    return SimpleNamespace(
        loader_cls=module_value.GpustackMxModelLoader,
        constructed=constructed,
        plugins=plugins,
        FakeMxModelLoader=FakeMxModelLoader,
    )


def test_the_manual_loader_subclasses_the_upstream_base(entry):
    """Registry and isinstance checks against the upstream class pass."""
    assert issubclass(entry.loader_cls, entry.FakeMxModelLoader)


def test_the_manual_loader_arms_the_route_before_constructing_upstream(entry):
    import os

    load_config = SimpleNamespace(format="modelexpress")
    entry.loader_cls(load_config)
    assert entry.constructed == [load_config]
    assert os.environ.get("MX_LOAD_PLUGIN") == "gpustack"
    assert os.environ.get("MX_LOAD_PLUGIN_BEFORE") == "server-cache"
    assert "gpustack" in entry.plugins
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    assert getattr(patched, "_gpustack_model_transfer_filtered", False)


def test_the_manual_loader_loads_through_the_inherited_upstream(entry):
    model = object()
    model_config = SimpleNamespace(model=model)
    loader = entry.loader_cls(SimpleNamespace())
    assert loader.load_model(None, model_config) is model
    assert loader.download_model(model_config) is model_config
