"""Exercise the SGLang native plugin and autoload hook against the SGLang loader contract."""

import importlib
import sys
import types
from enum import Enum

import pytest

from gpustack_model_transfer.adapters.engine_reader import TensorDescriptor


@pytest.fixture
def engine(monkeypatch):
    events = []

    class LoadFormat(str, Enum):
        AUTO = "auto"
        SAFETENSORS = "safetensors"

    class DefaultModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def download_model(self, model_config):
            events.append("default_download")

        def _get_all_weights(self, model_config, model):
            events.append("default_get_all_weights")
            raise AssertionError("_get_all_weights must not be iterated")
            yield  # pragma: no cover

        @staticmethod
        def postprocess_weights(model, target_device):
            events.append("finalize")

        @staticmethod
        def load_weights_and_postprocess(model, weights, target_device):
            for _ in weights:
                pass
            DefaultModelLoader.postprocess_weights(model, target_device)

        def load_model(self, *, model_config, device_config):
            events.append("create")
            model = model_config.model
            self.load_weights_and_postprocess(
                model,
                self._get_all_weights(model_config, model),
                device_config.device,
            )
            events.append("eval")
            return model

    def get_model_loader(load_config, *args, **kwargs):
        return DefaultModelLoader(load_config)

    modules = {
        "torch": types.ModuleType("torch"),
        "sglang": types.ModuleType("sglang"),
        "sglang.srt": types.ModuleType("sglang.srt"),
        "sglang.srt.configs": types.ModuleType("sglang.srt.configs"),
        "sglang.srt.configs.load_config": types.ModuleType(
            "sglang.srt.configs.load_config"
        ),
        "sglang.srt.model_loader": types.ModuleType("sglang.srt.model_loader"),
        "sglang.srt.model_loader.loader": types.ModuleType(
            "sglang.srt.model_loader.loader"
        ),
    }
    modules["sglang.srt.configs.load_config"].LoadFormat = LoadFormat
    modules["sglang.srt.model_loader.loader"].DefaultModelLoader = DefaultModelLoader
    modules["sglang.srt.model_loader.loader"].get_model_loader = get_model_loader
    modules["sglang.srt.model_loader"].get_model_loader = get_model_loader

    modules["torch"].cuda = types.SimpleNamespace(synchronize=lambda device: None)
    modules["torch"].empty = lambda shape, dtype, device: Tensor(
        shape, dtype, device.type, getattr(device, "index", None)
    )
    modules["torch"].dtype = type("dtype", (), {})
    modules["torch"].float32 = "torch.float32"
    modules["torch"].float16 = "torch.float16"
    modules["torch"].bfloat16 = "torch.bfloat16"
    modules["torch"].int64 = "torch.int64"
    modules["torch"].uint8 = "torch.uint8"
    modules["torch"].float8_e4m3fn = "torch.float8_e4m3fn"
    for name, module in modules.items():
        monkeypatch.setitem(sys.modules, name, module)
    monkeypatch.delitem(
        sys.modules, "gpustack_model_transfer.adapters.sglang.native", raising=False
    )
    from gpustack_model_transfer.adapters.sglang.plugin import register

    register()
    native = importlib.import_module("gpustack_model_transfer.adapters.sglang.native")
    yield types.SimpleNamespace(
        events=events,
        loader_mod=modules["sglang.srt.model_loader.loader"],
        load_config_mod=modules["sglang.srt.configs.load_config"],
        native=native,
    )
    sys.modules.pop("gpustack_model_transfer.adapters.sglang.native", None)


class Tensor:
    def __init__(self, shape=(2,), dtype="torch.float16", device="cuda", index=None):
        self.shape = shape
        self.dtype = dtype
        self.device = types.SimpleNamespace(type=device, index=index)
        self.is_meta = False

    def is_contiguous(self):
        return True

    def view(self, dtype):
        return self


class Model:
    def __init__(self, events, fail=False, device="cuda"):
        self.events = events
        self.weight = Tensor(device=device)
        self.empty = Tensor((0,), device=device)
        self.fail = fail

    def named_parameters(self, remove_duplicate=True):
        assert not remove_duplicate
        return iter(
            (("weight", self.weight), ("tied", self.weight), ("empty", self.empty))
        )

    def load_weights(self, weights):
        loaded = set()
        for name, tensor in weights:
            self.events.append(("apply", name, tensor.device.type))
            loaded.add(name)
            if self.fail:
                raise RuntimeError("partial load")
        return loaded


class Provider:
    def __init__(self, events):
        self.events = events
        self.descriptors = (
            TensorDescriptor("weight", (2,), "F16"),
            TensorDescriptor("empty", (0,), "F16"),
        )

    def probe(self, config):
        self.events.append("metadata")
        return self.descriptors

    def start(self, config):
        self.events.append("start")
        return self

    def fill(self, descriptor, target):
        self.events.append(("fill", descriptor.name, target.device.type))

    def close(self):
        self.events.append("close")

    def abort(self):
        self.events.append("abort")


def inputs(engine, monkeypatch, *, fail=False, device="cuda"):
    provider = Provider(engine.events)
    monkeypatch.setattr(
        engine.native.engine_reader,
        "create_reader_provider",
        lambda _, tp_size=1: provider,
    )
    load_config = types.SimpleNamespace(load_format="model-transfer")
    loader = engine.loader_mod.get_model_loader(load_config)
    model_config = types.SimpleNamespace(
        quantization=None,
        model=Model(engine.events, fail, device),
    )
    device_config = types.SimpleNamespace(device=device)
    return loader, model_config, device_config, provider


@pytest.mark.parametrize("device", ["cuda", "hip", "npu", "privateuseone", "cpu"])
def test_sglang_native_registration_and_lifecycle(engine, monkeypatch, device):
    assert any(m.value == "model-transfer" for m in engine.load_config_mod.LoadFormat)
    loader, model_config, device_config, _ = inputs(engine, monkeypatch, device=device)
    loader.download_model(model_config)
    assert engine.events == ["metadata"]
    model = loader.load_model(model_config=model_config, device_config=device_config)
    assert model is model_config.model
    assert engine.events == [
        "metadata",
        "create",
        "metadata",
        "start",
        ("fill", "weight", device),
        ("apply", "weight", device),
        ("fill", "empty", device),
        ("apply", "empty", device),
        "close",
        "finalize",
        "eval",
    ]


@pytest.mark.parametrize("accel", ["cuda", "hip", "npu"])
def test_sglang_hybrid_accelerator_and_cpu_parameters(engine, monkeypatch, accel):
    loader, model_config, device_config, provider = inputs(
        engine, monkeypatch, device=accel
    )
    parameters = {
        "model.layers.0.self_attn.qkv_proj.weight": Tensor(
            (6, 2), device=accel, index=0
        ),
        "model.engram.lookup_table.weight": Tensor((16, 2), device="cpu"),
    }
    model_config.model.named_parameters = lambda remove_duplicate: iter(
        parameters.items()
    )
    provider.descriptors = (
        TensorDescriptor("model.layers.0.self_attn.qkv_proj.weight", (6, 2), "F16"),
        TensorDescriptor("model.engram.lookup_table.weight", (16, 2), "F16"),
    )
    model = loader.load_model(model_config=model_config, device_config=device_config)
    assert model is model_config.model
    fills = [
        event
        for event in engine.events
        if isinstance(event, tuple) and event[0] == "fill"
    ]
    assert fills == [
        ("fill", "model.layers.0.self_attn.qkv_proj.weight", accel),
        ("fill", "model.engram.lookup_table.weight", accel),
    ]


def test_sglang_partial_failure_poisoned_and_aborted(engine, monkeypatch):
    loader, model_config, device_config, _ = inputs(engine, monkeypatch, fail=True)
    with pytest.raises(RuntimeError, match="partial load"):
        loader.load_model(model_config=model_config, device_config=device_config)
    assert engine.events[-1] == "abort"
    assert "finalize" not in engine.events
    with pytest.raises(engine.native.ProcessLoadUnusable):
        loader.load_model(model_config=model_config, device_config=device_config)


@pytest.mark.parametrize(
    "reason",
    ["dtype", "unknown_device", "multi_accelerator"],
)
def test_sglang_reject_before_mutation(engine, monkeypatch, reason):
    loader, model_config, device_config, provider = inputs(
        engine,
        monkeypatch,
        device="mps" if reason == "unknown_device" else "cuda",
    )
    if reason == "dtype":
        provider.descriptors = (TensorDescriptor("weight", (2,), "F8"),)
    if reason == "multi_accelerator":
        parameters = {
            "w0": Tensor((2,), device="cuda", index=0),
            "w1": Tensor((2,), device="cuda", index=1),
        }
        model_config.model.named_parameters = lambda remove_duplicate: iter(
            parameters.items()
        )
    with pytest.raises(engine.native.UnsupportedEngineLayout):
        loader.load_model(model_config=model_config, device_config=device_config)
    assert "start" not in engine.events
    assert not any(isinstance(e, tuple) and e[0] == "apply" for e in engine.events)


def test_sglang_quantized_model_supported(engine, monkeypatch):
    loader, model_config, device_config, provider = inputs(engine, monkeypatch)
    model_config.quantization = "fp8"
    model_config.quantization_config = {"quant_method": "fp8"}
    provider.descriptors = (
        TensorDescriptor("weight", (2, 2), "F8_E4M3"),
        TensorDescriptor("weight_scale", (1,), "F32"),
    )
    model = loader.load_model(model_config=model_config, device_config=device_config)
    assert model is model_config.model
    assert "finalize" in engine.events


def test_sglang_single_tp_rank_under_distributed_stays_stage_local(engine, monkeypatch):
    """A lone TP rank never falls through to WORLD, which spans PP stages."""
    distributed = types.SimpleNamespace(
        is_available=lambda: True,
        is_initialized=lambda: True,
        get_world_size=lambda: 2,
        get_rank=lambda: 0,
        group=types.SimpleNamespace(WORLD=object()),
    )
    monkeypatch.setattr(engine.native.torch, "distributed", distributed, raising=False)
    tp_group = types.SimpleNamespace(
        device_group=object(), world_size=1, rank_in_group=0, ranks=[0]
    )
    state = types.ModuleType("sglang.srt.distributed")
    state.get_tp_group = lambda: tp_group
    monkeypatch.setitem(sys.modules, "sglang.srt.distributed", state)
    assert distributed.get_world_size() > 1
    assert engine.native._tp_coordination() == (False, True, None, 0, 1)


def test_sglang_pp_stage_skips_other_stages_layers(engine):
    """A stage holding only layer 1 skips layer 0 tensors and loads its own."""
    model = types.SimpleNamespace(
        named_parameters=lambda remove_duplicate: iter(
            (("model.layers.1.weight", object()),)
        )
    )
    layer0 = TensorDescriptor("model.layers.0.weight", (2,), "F16")
    layer1 = TensorDescriptor("model.layers.1.weight", (2,), "F16")
    norm = TensorDescriptor("model.norm.weight", (2,), "F16")
    assert tuple(engine.native._active_descriptors(model, (layer0, layer1, norm))) == (
        layer1,
        norm,
    )


@pytest.mark.parametrize("device", ["cuda", "hip", "npu", "privateuseone"])
def test_device_synchronization_failure_propagates(engine, monkeypatch, device):
    def fail(device):
        raise RuntimeError("device fence failed")

    for namespace in ("cuda", "hip", "npu"):
        monkeypatch.setattr(
            engine.native.torch,
            namespace,
            types.SimpleNamespace(synchronize=fail),
            raising=False,
        )
    with pytest.raises(RuntimeError, match="device fence failed"):
        engine.native._synchronize_device(types.SimpleNamespace(type=device))


def test_missing_device_fence_is_refused(engine):
    with pytest.raises(engine.native.UnsupportedEngineLayout, match="completion fence"):
        engine.native._synchronize_device(types.SimpleNamespace(type="unsupported"))


def test_failed_collective_fence_aborts_and_poison_load(engine, monkeypatch):
    loader, model_config, device_config, _ = inputs(engine, monkeypatch)

    def fail(device):
        raise RuntimeError("collective failed")

    monkeypatch.setattr(
        engine.native, "_tp_coordination", lambda: (True, True, object(), 0, 2)
    )
    monkeypatch.setattr(
        engine.native.torch,
        "distributed",
        types.SimpleNamespace(
            broadcast=lambda *args, **kwargs: None,
            broadcast_object_list=lambda *args, **kwargs: None,
        ),
        raising=False,
    )
    monkeypatch.setattr(engine.native.torch.cuda, "synchronize", fail)
    with pytest.raises(RuntimeError, match="collective failed"):
        loader.load_model(model_config=model_config, device_config=device_config)
    assert engine.events[-1] == "abort"
    assert "finalize" not in engine.events
    assert engine.native._process_unusable


def test_cached_provider_refuses_changed_tp_size(engine, monkeypatch):
    loader, _, _, provider = inputs(engine, monkeypatch)
    assert loader._reader(tp_size=1) is provider
    with pytest.raises(engine.native.UnsupportedEngineLayout, match="size changed"):
        loader._reader(tp_size=2)
    assert loader._reader(tp_size=1) is provider


@pytest.mark.parametrize("name", ["tensor", "device"])
def test_non_dtype_torch_attributes_are_refused(engine, monkeypatch, name):
    class Dtype:
        pass

    monkeypatch.setattr(engine.native.torch, "dtype", Dtype, raising=False)
    monkeypatch.setattr(engine.native.torch, name, object(), raising=False)
    with pytest.raises(
        engine.native.UnsupportedEngineLayout, match="unsupported dtype"
    ):
        engine.native._torch_dtype(name)


def test_loader_registration_failure_is_observable(engine, monkeypatch, caplog):
    from gpustack_model_transfer.adapters.sglang.plugin import register

    monkeypatch.setattr(engine.loader_mod, "get_model_loader", None)
    with caplog.at_level("WARNING"):
        register()
    assert "SGLang loader registration unavailable (TypeError)" in caplog.text
