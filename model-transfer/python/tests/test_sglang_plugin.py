"""SGLang registration: the ModelExpress load formats and the bridged loader.

The fake engine graph mirrors the SGLang modules the registration
touches, plus the pinned upstream modelexpress modules the bridged
loader hands the model to. The registration must add the formats to the
LoadFormat enum, resolve them to the bridged loader, build the model the
way SGLang's own load path does (quantization config, dtype, device),
and leave every other format on SGLang's own path.
"""

import importlib
import os
import sys
import types
from contextlib import contextmanager
from enum import Enum
from types import SimpleNamespace

import pytest

_STRATEGY_MODULE = "gpustack_model_transfer.adapters.modelexpress.plugin"


class FakeLoadFormat(str, Enum):
    AUTO = "auto"
    SAFETENSORS = "safetensors"


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
def engine(monkeypatch):
    events = []
    dtype_contexts = []
    device_contexts = []

    def _module(name, **attrs):
        value = types.ModuleType(name)
        value.__dict__.update(attrs)
        monkeypatch.setitem(sys.modules, name, value)
        return value

    class StrategyFailed(RuntimeError):
        def __init__(self, message, *, mutated=False):
            super().__init__(message)
            self.mutated = mutated

    class MxModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def load_model(self, *, model, model_config, device_config):
            events.append(("mx_load_model", model, model_config, device_config))
            return model

    def execute_load_strategies(model, ctx, strategies):
        return model

    plugins = {}

    def register_plugin(name, strategy_class):
        if name in plugins:
            raise ValueError(f"Plugin {name!r} is already registered")
        plugins[name] = strategy_class

    def get_model_loader(load_config, *args, **kwargs):
        return types.SimpleNamespace(load_config=load_config, kind="default")

    def _initialize_model(model_config, load_config, quant_config):
        events.append(("initialize_model", model_config, load_config, quant_config))
        return model_config.model

    def _get_quantization_config(model_config, load_config):
        return f"quant-for-{model_config.name}"

    @contextmanager
    def set_default_torch_dtype(dtype):
        dtype_contexts.append(dtype)
        yield

    class FakeTorchDevice:
        def __init__(self, spec):
            self.spec = spec

        def __enter__(self):
            device_contexts.append(self.spec)
            return self

        def __exit__(self, *args):
            return False

    _module("torch", device=FakeTorchDevice)
    _module("modelexpress", __path__=[])
    _module("modelexpress.adapter", StrategyFailed=StrategyFailed)
    _module(
        "modelexpress.load_strategy", execute_load_strategies=execute_load_strategies
    )
    _module("modelexpress.plugins", register_plugin=register_plugin, plugins=plugins)
    _module("modelexpress.engines", __path__=[])
    _module("modelexpress.engines.sglang", __path__=[])
    _module(
        "modelexpress.engines.sglang.loader",
        MxModelLoader=MxModelLoader,
    )

    class NativeSGLangModelTransferLoader:
        def __init__(self, load_config):
            self.load_config = load_config

    sglang_native = types.ModuleType("gpustack_model_transfer.adapters.sglang.native")
    sglang_native.NativeSGLangModelTransferLoader = NativeSGLangModelTransferLoader
    monkeypatch.setitem(
        sys.modules, "gpustack_model_transfer.adapters.sglang.native", sglang_native
    )

    class DefaultModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def load_model(self, *, model_config, device_config):
            events.append("default_create")
            return model_config.model

    _module(
        "sglang.srt.model_loader.loader",
        get_model_loader=get_model_loader,
        DefaultModelLoader=DefaultModelLoader,
        _initialize_model=_initialize_model,
        _get_quantization_config=_get_quantization_config,
    )
    _module(
        "sglang.srt.model_loader",
        get_model_loader=get_model_loader,
    )
    _module(
        "sglang.srt.model_loader.utils",
        set_default_torch_dtype=set_default_torch_dtype,
    )
    _module("sglang", __path__=[])
    _module("sglang.srt", __path__=[])
    _module("sglang.srt.configs", __path__=[])
    load_config_mod = _module(
        "sglang.srt.configs.load_config", LoadFormat=FakeLoadFormat
    )

    monkeypatch.delitem(sys.modules, _STRATEGY_MODULE, raising=False)
    monkeypatch.delitem(
        sys.modules, "gpustack_model_transfer.adapters.sglang.plugin", raising=False
    )
    plugin = importlib.import_module("gpustack_model_transfer.adapters.sglang.plugin")
    return SimpleNamespace(
        events=events,
        dtype_contexts=dtype_contexts,
        device_contexts=device_contexts,
        plugin=plugin,
        loader_mod=sys.modules["sglang.srt.model_loader.loader"],
        load_config_mod=load_config_mod,
        plugins=plugins,
    )


@pytest.mark.parametrize("load_format", ["modelexpress", "mx", "Modelexpress", " mx "])
def test_the_model_express_formats_resolve_to_the_bridged_loader(engine, load_format):
    engine.plugin.register()
    for value in ("modelexpress", "mx"):
        assert any(m.value == value for m in engine.load_config_mod.LoadFormat)
    load_config = SimpleNamespace(load_format=load_format)
    loader = engine.loader_mod.get_model_loader(load_config)
    assert isinstance(loader, engine.plugin.ModelExpressSGLangLoader)
    assert engine.events == []


@pytest.mark.parametrize("load_format", ["modelexpress", "mx"])
def test_the_bridged_loader_builds_the_model_like_sglang_does(engine, load_format):
    """Quantization config — not the device config — is the third argument,
    and the model is built under the configured dtype on the target device."""
    engine.plugin.register()
    load_config = SimpleNamespace(load_format=load_format)
    model_config = SimpleNamespace(model=object(), name="tiny", dtype="bfloat16")
    device_config = SimpleNamespace(device="cuda:0")
    loader = engine.loader_mod.get_model_loader(load_config)
    model = loader.load_model(model_config=model_config, device_config=device_config)
    assert model is model_config.model
    assert engine.events == [
        ("initialize_model", model_config, load_config, "quant-for-tiny"),
        ("mx_load_model", model, model_config, device_config),
    ]
    assert engine.dtype_contexts == ["bfloat16"]
    assert engine.device_contexts == ["cuda:0"]


def test_the_bridged_loader_installs_the_patch_and_registers_the_strategy(engine):
    engine.plugin.register()
    load_config = SimpleNamespace(load_format="modelexpress")
    engine.loader_mod.get_model_loader(load_config)
    assert os.environ.get("MX_LOAD_PLUGIN") == "gpustack"
    assert os.environ.get("MX_LOAD_PLUGIN_BEFORE") == "server-cache"
    assert "gpustack" in engine.plugins
    patched = sys.modules["modelexpress.load_strategy"].execute_load_strategies
    assert getattr(patched, "_gpustack_model_transfer_filtered", False)


def test_other_formats_stay_on_sglang_path(engine):
    engine.plugin.register()
    loader = engine.loader_mod.get_model_loader(SimpleNamespace(load_format="auto"))
    assert getattr(loader, "kind", "") == "default"
    assert engine.events == []


def test_the_member_table_derives_from_the_route_aliases(engine):
    """One source of truth: the member table mirrors the route's aliases."""
    from gpustack_model_transfer.modelexpress_route import LOAD_FORMAT_ALIASES

    assert set(engine.plugin._ENUM_MEMBERS) == {
        "model-transfer",
        *LOAD_FORMAT_ALIASES,
    }
    assert tuple(LOAD_FORMAT_ALIASES) == engine.plugin._MODELEXPRESS_FORMATS


def test_registering_twice_keeps_one_strategy(engine):
    engine.plugin.register()
    loader = engine.loader_mod.get_model_loader(
        SimpleNamespace(load_format="modelexpress")
    )
    assert isinstance(loader, engine.plugin.ModelExpressSGLangLoader)
    assert engine.plugins["gpustack"].name == "gpustack"
    engine.plugin.register()
    assert engine.plugins["gpustack"].name == "gpustack"


def test_one_rejected_choice_value_does_not_abort_the_rest(engine, monkeypatch):
    """A mid-list refusal leaves the other values registered, not a mix."""
    attempts = []

    def add_choices(value):
        for item in value if isinstance(value, list) else [value]:
            attempts.append(item)
            if item == "modelexpress":
                raise TypeError("modelexpress refused")

    arg_groups = types.ModuleType("sglang.srt.arg_groups")
    arg_groups.__path__ = []
    monkeypatch.setitem(sys.modules, "sglang.srt.arg_groups", arg_groups)
    monkeypatch.setitem(
        sys.modules,
        "sglang.srt.arg_groups.choices",
        SimpleNamespace(add_load_format_choices=add_choices),
    )
    engine.plugin.register()
    assert attempts == [
        # The batch attempt, refused partway through.
        "model-transfer",
        "modelexpress",
        # The per-value retry reaches past the refusal.
        "model-transfer",
        "modelexpress",
        "mx",
    ]


def test_the_loader_construction_resolves_failures_by_the_policy(
    engine, monkeypatch, caplog
):
    """A missing upstream fails by the policy, not an opaque import error."""
    import logging
    import types as types_module

    stub = types_module.ModuleType("modelexpress")
    stub.__path__ = []
    monkeypatch.setitem(sys.modules, "modelexpress", stub)
    monkeypatch.setitem(sys.modules, "modelexpress.plugins", None)
    loader_cls = engine.plugin.ModelExpressSGLangLoader
    with caplog.at_level(logging.ERROR):
        # Prefer: construction logs the failed arm and stays advisory.
        loader = loader_cls(SimpleNamespace(load_config=SimpleNamespace()))
        assert loader is not None
    assert any(
        "ModelExpress registration unavailable" in record.getMessage()
        for record in caplog.records
    )
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Require")
    # Under Require the raw failure propagates out of loader construction.
    with pytest.raises(ModuleNotFoundError):
        loader_cls(SimpleNamespace(load_config=SimpleNamespace()))


def test_the_enum_extension_fallback_rebuilds_the_member(engine, monkeypatch):
    """Without aenum, the member is rebuilt so lookup and value checks work."""
    import enum as enum_module

    monkeypatch.setitem(sys.modules, "aenum", None)

    class LoadFormat(str, enum_module.Enum):
        AUTO = "auto"

    engine.plugin._extend_load_format_enum(LoadFormat, "MODELEXPRESS", "modelexpress")
    assert LoadFormat["MODELEXPRESS"].value == "modelexpress"
    assert LoadFormat("modelexpress") is LoadFormat["MODELEXPRESS"]
    # Idempotent: a second extension is a no-op.
    engine.plugin._extend_load_format_enum(LoadFormat, "MODELEXPRESS", "modelexpress")
    assert len(LoadFormat.__members__) == 2
