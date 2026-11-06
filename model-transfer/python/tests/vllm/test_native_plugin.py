"""Exercise the vLLM native plugin against the architecture-agnostic loader contract."""

import importlib
import sys
import types

import pytest

from gpustack_model_transfer.adapters.engine_reader import TensorDescriptor


@pytest.fixture
def engine(monkeypatch):
    events = []
    registry = {}

    class BaseModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

        def load_model(self, vllm_config, model_config, prefix=""):
            events.append("create")
            model = vllm_config.model
            self.load_weights(model, model_config)
            events.append("finalize")
            events.append("eval")
            return model

    def register_model_loader(name):
        def register(cls):
            assert issubclass(cls, BaseModelLoader)
            registry[name] = cls
            return cls

        return register

    modules = {
        "torch": types.ModuleType("torch"),
        "vllm": types.ModuleType("vllm"),
        "vllm.model_executor": types.ModuleType("vllm.model_executor"),
        "vllm.model_executor.model_loader": types.ModuleType(
            "vllm.model_executor.model_loader"
        ),
        "vllm.model_executor.model_loader.base_loader": types.ModuleType("base_loader"),
    }
    modules[
        "vllm.model_executor.model_loader"
    ].register_model_loader = register_model_loader
    modules[
        "vllm.model_executor.model_loader.base_loader"
    ].BaseModelLoader = BaseModelLoader
    modules["torch"].empty = lambda shape, dtype, device: Tensor(
        shape, dtype, device.type, getattr(device, "index", None)
    )
    modules["torch"].float32 = "torch.float32"
    modules["torch"].float16 = "torch.float16"
    modules["torch"].bfloat16 = "torch.bfloat16"
    modules["torch"].int64 = "torch.int64"
    modules["torch"].uint8 = "torch.uint8"
    modules["torch"].float8_e4m3fn = "torch.float8_e4m3fn"
    for name, module in modules.items():
        monkeypatch.setitem(sys.modules, name, module)
    monkeypatch.delitem(
        sys.modules, "gpustack_model_transfer.adapters.vllm.native", raising=False
    )
    from gpustack_model_transfer.adapters.vllm.plugin import register

    register()
    native = importlib.import_module("gpustack_model_transfer.adapters.vllm.native")
    yield types.SimpleNamespace(events=events, registry=registry, native=native)
    sys.modules.pop("gpustack_model_transfer.adapters.vllm.native", None)


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
    loader = engine.registry["model-transfer"](object())
    parallel = types.SimpleNamespace(
        tensor_parallel_size=1,
        pipeline_parallel_size=1,
        data_parallel_size=1,
        prefill_context_parallel_size=1,
        decode_context_parallel_size=1,
        enable_expert_parallel=False,
        enable_eplb=False,
    )
    config = types.SimpleNamespace(
        parallel_config=parallel, model=Model(engine.events, fail, device)
    )
    return loader, config, types.SimpleNamespace(quantization=None), provider


@pytest.mark.parametrize("device", ["cuda", "hip", "npu", "privateuseone", "cpu"])
def test_native_registration_and_lifecycle(engine, monkeypatch, device):
    loader, config, model_config, _ = inputs(engine, monkeypatch, device=device)
    loader.download_model(model_config)
    assert engine.events == ["metadata"]
    model = loader.load_model(config, model_config)
    assert model is config.model
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
def test_hybrid_accelerator_and_cpu_parameters_supported(engine, monkeypatch, accel):
    """Models with CPU-offloaded parameters (e.g. DeepSeek Engram) use the accelerator buffer."""
    loader, config, model_config, provider = inputs(engine, monkeypatch, device=accel)
    parameters = {
        "model.layers.0.self_attn.qkv_proj.weight": Tensor(
            (6, 2), device=accel, index=0
        ),
        "model.engram.lookup_table.weight": Tensor((16, 2), device="cpu"),
    }
    config.model.named_parameters = lambda remove_duplicate: iter(parameters.items())
    provider.descriptors = (
        TensorDescriptor("model.layers.0.self_attn.qkv_proj.weight", (6, 2), "F16"),
        TensorDescriptor("model.engram.lookup_table.weight", (16, 2), "F16"),
    )
    model = loader.load_model(config, model_config)
    assert model is config.model
    fills = [
        event
        for event in engine.events
        if isinstance(event, tuple) and event[0] == "fill"
    ]
    assert fills == [
        ("fill", "model.layers.0.self_attn.qkv_proj.weight", accel),
        ("fill", "model.engram.lookup_table.weight", accel),
    ]


def test_partial_failure_poisoned_and_aborted(engine, monkeypatch):
    loader, config, model_config, _ = inputs(engine, monkeypatch, fail=True)
    with pytest.raises(RuntimeError, match="partial load"):
        loader.load_model(config, model_config)
    assert engine.events[-1] == "abort"
    assert "finalize" not in engine.events
    with pytest.raises(engine.native.ProcessLoadUnusable):
        loader.load_model(config, model_config)


def test_bloom_style_direct_parameter_layout(engine, monkeypatch):
    loader, config, model_config, provider = inputs(engine, monkeypatch)
    dimensions = {
        "transformer.word_embeddings.weight": (64, 4),
        "transformer.word_embeddings_layernorm.weight": (4,),
        "transformer.word_embeddings_layernorm.bias": (4,),
        "transformer.h.0.input_layernorm.weight": (4,),
        "transformer.h.0.input_layernorm.bias": (4,),
        "transformer.h.0.self_attention.query_key_value.weight": (12, 4),
        "transformer.h.0.self_attention.query_key_value.bias": (12,),
        "transformer.h.0.self_attention.dense.weight": (4, 4),
        "transformer.h.0.self_attention.dense.bias": (4,),
        "transformer.h.0.post_attention_layernorm.weight": (4,),
        "transformer.h.0.post_attention_layernorm.bias": (4,),
        "transformer.h.0.mlp.dense_h_to_4h.weight": (16, 4),
        "transformer.h.0.mlp.dense_h_to_4h.bias": (16,),
        "transformer.h.0.mlp.dense_4h_to_h.weight": (4, 16),
        "transformer.h.0.mlp.dense_4h_to_h.bias": (4,),
        "transformer.ln_f.weight": (4,),
        "transformer.ln_f.bias": (4,),
    }
    parameters = {name: Tensor(shape) for name, shape in dimensions.items()}
    parameters["lm_head.weight"] = parameters["transformer.word_embeddings.weight"]
    config.model.named_parameters = lambda remove_duplicate: iter(parameters.items())
    provider.descriptors = tuple(
        TensorDescriptor(name, shape, "F16") for name, shape in dimensions.items()
    )
    loader.load_model(config, model_config)
    assert engine.events.count("close") == 1
    assert engine.events.index("close") < engine.events.index("finalize")


@pytest.mark.parametrize(
    "reason",
    [
        "topology",
        "prefill_topology",
        "eplb",
        "dtype",
        "unknown_device",
        "multi_accelerator",
    ],
)
def test_reject_before_mutation(engine, monkeypatch, reason):
    loader, config, model_config, provider = inputs(
        engine,
        monkeypatch,
        device="mps" if reason == "unknown_device" else "cuda",
    )
    if reason == "topology":
        config.parallel_config.tensor_parallel_size = 0
    if reason == "prefill_topology":
        config.parallel_config.prefill_context_parallel_size = 0
    if reason == "eplb":
        config.parallel_config.enable_eplb = True
    if reason == "dtype":
        provider.descriptors = (TensorDescriptor("weight", (2,), "F8"),)
    if reason == "multi_accelerator":
        parameters = {
            "w0": Tensor((2,), device="cuda", index=0),
            "w1": Tensor((2,), device="cuda", index=1),
        }
        config.model.named_parameters = lambda remove_duplicate: iter(
            parameters.items()
        )
    with pytest.raises(engine.native.UnsupportedEngineLayout):
        loader.load_model(config, model_config)
    assert "start" not in engine.events
    assert not any(isinstance(e, tuple) and e[0] == "apply" for e in engine.events)


def test_quantized_model_supported(engine, monkeypatch):
    loader, config, model_config, provider = inputs(engine, monkeypatch)
    model_config.quantization = "fp8"
    model_config.quantization_config = {"quant_method": "fp8"}
    config.quant_config = object()
    provider.descriptors = (
        TensorDescriptor("weight", (2, 2), "F8_E4M3"),
        TensorDescriptor("weight_scale", (1,), "F32"),
    )
    model = loader.load_model(config, model_config)
    assert model is config.model
    assert "finalize" in engine.events


def test_engine_owns_the_name_mapping(engine, monkeypatch):
    """Any model architecture maps checkpoint tensor names inside model.load_weights."""
    loader, config, model_config, provider = inputs(engine, monkeypatch)
    parameters = {
        "model.layers.0.self_attn.qkv_proj.weight": Tensor((6, 2)),
        "model.layers.0.linear_attn.conv1d.ascend_conv1d_weight": Tensor((4, 2)),
        "model.norm.weight": Tensor((2,)),
    }
    config.model.named_parameters = lambda remove_duplicate: iter(parameters.items())
    provider.descriptors = (
        TensorDescriptor("model.layers.0.self_attn.q_proj.weight", (2, 2), "F16"),
        TensorDescriptor("model.layers.0.self_attn.k_proj.weight", (2, 2), "F16"),
        TensorDescriptor("model.layers.0.self_attn.v_proj.weight", (2, 2), "F16"),
        TensorDescriptor("model.layers.0.linear_attn.conv1d.weight", (4, 2), "F16"),
        TensorDescriptor("model.norm.weight", (2,), "F16"),
    )
    loader.load_model(config, model_config)
    applied = [
        event[1]
        for event in engine.events
        if isinstance(event, tuple) and event[0] == "apply"
    ]
    assert applied == [item.name for item in provider.descriptors]
    assert engine.events.count("close") == 1
    assert engine.events.index("close") < engine.events.index("finalize")


def test_engine_rejection_aborts_before_finalization(engine, monkeypatch):
    """An engine that refuses a weight stops the load and aborts the session."""
    loader, config, model_config, _provider = inputs(engine, monkeypatch)

    def refuses_a_weight(weights):
        for name, tensor in weights:
            config.model.events.append(("apply", name, tensor.device.type))
            if name == "empty":
                raise ValueError(f"unexpected weight {name!r}")
        return set()

    config.model.load_weights = refuses_a_weight
    with pytest.raises(ValueError):
        loader.load_model(config, model_config)
    assert engine.events[-1] == "abort"
    assert "finalize" not in engine.events


def test_missing_client_fails_closed(engine, monkeypatch):
    monkeypatch.setitem(sys.modules, "gpustack_model_transfer.client", None)
    loader = engine.registry["model-transfer"](object())
    with pytest.raises(ModuleNotFoundError, match=r"gpustack_model_transfer\.client"):
        loader.download_model(types.SimpleNamespace())
    assert engine.events == []


@pytest.mark.parametrize("failure", ["fill", "close", "not_consumed", "not_confirmed"])
def test_failure_aborts_before_finalization(engine, monkeypatch, failure):
    loader, config, model_config, provider = inputs(engine, monkeypatch)

    def fail_fill(descriptor, target):
        raise RuntimeError("fill failed")

    def fail_close():
        raise RuntimeError("close failed")

    def incomplete(weights):
        next(weights)
        return {"weight"}

    def unconfirmed(weights):
        list(weights)
        return set()

    if failure == "fill":
        provider.fill = fail_fill
    if failure == "close":
        provider.close = fail_close
    if failure == "not_consumed":
        config.model.load_weights = incomplete
    if failure == "not_confirmed":
        config.model.load_weights = unconfirmed
    with pytest.raises(RuntimeError):
        loader.load_model(config, model_config)
    assert engine.events[-1] == "abort"
    assert "finalize" not in engine.events
    with pytest.raises(engine.native.ProcessLoadUnusable):
        loader.load_model(config, model_config)


def test_root_import_does_not_load_engine():
    import subprocess

    subprocess.run(
        [
            sys.executable,
            "-c",
            "import sys, gpustack_model_transfer; "
            "assert 'torch' not in sys.modules and 'vllm' not in sys.modules",
        ],
        check=True,
        timeout=30,
    )


def test_entrypoint_is_packaged():
    from importlib.metadata import entry_points

    matches = tuple(entry_points(group="vllm.general_plugins", name="model_transfer"))
    assert len(matches) == 1
    assert matches[0].value == "gpustack_model_transfer.adapters.vllm.plugin:register"


@pytest.mark.parametrize("tp", [1, 2, 8])
def test_positive_tensor_parallel_topology(engine, monkeypatch, tp):
    _, config, _, _ = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = tp
    engine.native._check_topology(config)


@pytest.mark.parametrize("tp", [0, -1, True, 1.5, None])
def test_invalid_tensor_parallel_topology(engine, monkeypatch, tp):
    _, config, _, _ = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = tp
    with pytest.raises(engine.native.UnsupportedEngineLayout):
        engine.native._check_topology(config)


def test_pipeline_and_context_parallel_topology(engine, monkeypatch):
    """PP and the prefill/decode context dimensions admit any size of at least one."""
    _, config, _, _ = inputs(engine, monkeypatch)
    config.parallel_config.pipeline_parallel_size = 2
    config.parallel_config.prefill_context_parallel_size = 2
    config.parallel_config.decode_context_parallel_size = 2
    engine.native._check_topology(config)


@pytest.fixture
def tp_distributed(engine, monkeypatch):
    rank = [0]
    objects = []
    broadcasts = []
    group = object()

    def broadcast_object_list(items, *, src, group):
        assert src == 0
        if rank[0] == 0:
            objects.append(list(items))
        else:
            items[:] = objects.pop(0)

    def broadcast(target, *, src, group):
        broadcasts.append((rank[0], src, group, target.shape))

    distributed = types.SimpleNamespace(
        is_available=lambda: True,
        is_initialized=lambda: True,
        get_world_size=lambda: 2,
        get_rank=lambda: rank[0],
        group=types.SimpleNamespace(WORLD=group),
        broadcast_object_list=broadcast_object_list,
        broadcast=broadcast,
    )
    monkeypatch.setattr(engine.native.torch, "distributed", distributed, raising=False)
    return types.SimpleNamespace(
        rank=rank, objects=objects, broadcasts=broadcasts, group=group
    )


def test_tp_coordinator_is_only_broker_reader(engine, monkeypatch, tp_distributed):
    loader, config, model_config, provider = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = 2
    loader.download_model(model_config)
    loader.load_model(config, model_config)
    assert engine.events.count("metadata") == 2
    assert engine.events.count("start") == 1
    assert engine.events.count("close") == 1

    tp_distributed.rank[0] = 1
    follower = engine.registry["model-transfer"](object())
    follower._reader = lambda: pytest.fail("non-coordinator accessed the Broker")
    follower.download_model(model_config)
    assert follower._cached_descriptors == provider.descriptors
    follower.load_model(config, model_config)
    assert engine.events.count("start") == 1
    assert engine.events.count("close") == 1
    assert [item[0] for item in tp_distributed.broadcasts] == [0, 0, 1, 1]
    assert all(item[2] is tp_distributed.group for item in tp_distributed.broadcasts)
    assert tp_distributed.objects == []


@pytest.mark.parametrize("stage", ["download", "probe", "plan", "start"])
def test_tp_initial_failure_is_broadcast(engine, monkeypatch, tp_distributed, stage):
    loader, config, model_config, provider = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = 2

    def fail(*args):
        raise RuntimeError("coordinator failed")

    if stage in ("download", "probe"):
        provider.probe = fail
    elif stage == "plan":
        monkeypatch.setattr(engine.native, "_plan", fail)
    else:
        provider.start = fail
    operation = (
        loader.download_model
        if stage == "download"
        else lambda mc: loader.load_model(config, mc)
    )
    with pytest.raises(RuntimeError, match="coordinator failed"):
        operation(model_config)
    assert tp_distributed.objects == [[False, "coordinator failed"]]
    engine.native._process_unusable = False
    tp_distributed.rank[0] = 1
    follower = engine.registry["model-transfer"](object())
    follower._reader = lambda: pytest.fail("non-coordinator accessed the Broker")
    operation = (
        follower.download_model
        if stage == "download"
        else lambda mc: follower.load_model(config, mc)
    )
    with pytest.raises(RuntimeError, match="coordinator failed"):
        operation(model_config)
    assert not tp_distributed.objects


def test_tp_uses_vllm_device_group_and_global_source(
    engine, monkeypatch, tp_distributed
):
    group = object()
    tp_group = types.SimpleNamespace(
        device_group=group, world_size=2, rank_in_group=0, ranks=[4, 5]
    )
    state = types.ModuleType("vllm.distributed.parallel_state")
    state.get_tp_group = lambda: tp_group
    monkeypatch.setitem(sys.modules, "vllm.distributed.parallel_state", state)
    assert engine.native._tp_coordination() == (True, True, group, 4, 2)
    tp_group.rank_in_group = 1
    assert engine.native._tp_coordination() == (True, False, group, 4, 2)


def test_single_tp_rank_under_distributed_stays_stage_local(
    engine, monkeypatch, tp_distributed
):
    """A lone TP rank never falls through to WORLD, which spans PP stages."""
    tp_group = types.SimpleNamespace(
        device_group=object(), world_size=1, rank_in_group=0, ranks=[0]
    )
    state = types.ModuleType("vllm.distributed.parallel_state")
    state.get_tp_group = lambda: tp_group
    monkeypatch.setitem(sys.modules, "vllm.distributed.parallel_state", state)
    assert engine.native.torch.distributed.get_world_size() > 1
    assert engine.native._tp_coordination() == (False, True, None, 0, 1)


def test_uninitialized_distributed_is_single_process(
    engine, monkeypatch, tp_distributed
):
    monkeypatch.setattr(
        engine.native.torch.distributed, "is_initialized", lambda: False
    )
    assert engine.native._tp_coordination() == (False, True, None, 0, 1)


@pytest.mark.parametrize(("dp", "ep"), [(1, True), (2, True), (4, False)])
def test_expert_and_data_parallel_topology(engine, monkeypatch, dp, ep):
    _, config, _, _ = inputs(engine, monkeypatch)
    config.parallel_config.data_parallel_size = dp
    config.parallel_config.enable_expert_parallel = ep
    engine.native._check_topology(config)


@pytest.mark.parametrize(("dp", "ep"), [(0, False), (True, False), (1, None)])
def test_invalid_expert_and_data_parallel_topology(engine, monkeypatch, dp, ep):
    _, config, _, _ = inputs(engine, monkeypatch)
    config.parallel_config.data_parallel_size = dp
    config.parallel_config.enable_expert_parallel = ep
    with pytest.raises(engine.native.UnsupportedEngineLayout):
        engine.native._check_topology(config)


def test_hierarchical_moe_layout_with_mtp_drop(engine, monkeypatch):
    """Unused prefixes dropped by hf_to_vllm_mapper._map_name are skipped before fill."""
    loader, config, model_config, provider = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = 8
    config.parallel_config.enable_expert_parallel = True
    parameters = {
        "language_model.model.layers.0.self_attn.in_proj_qkvz.weight": Tensor((4, 2)),
        "language_model.model.layers.0.mlp.experts.routed_experts.w13_weight": Tensor(
            (8, 2, 2)
        ),
    }
    config.model.named_parameters = lambda remove_duplicate: iter(parameters.items())
    config.model.hf_to_vllm_mapper = types.SimpleNamespace(
        _map_name=lambda name: None if name.startswith("mtp.") else name,
    )
    provider.descriptors = (
        TensorDescriptor(
            "model.language_model.layers.0.self_attn.in_proj_qkv.weight",
            (3, 2),
            "BF16",
        ),
        TensorDescriptor(
            "model.language_model.layers.0.mlp.experts.gate_up_proj",
            (8, 2, 2),
            "BF16",
        ),
        TensorDescriptor("mtp.fc.weight", (2, 2), "BF16"),
    )

    def load_moe_weights(weights):
        consumed_names = [name for name, _ in weights]
        assert "mtp.fc.weight" not in consumed_names
        return {"language_model.model.layers.0.mlp.experts.w13_weight"}

    config.model.load_weights = load_moe_weights
    model = loader.load_model(config, model_config)
    assert model is config.model
    assert "finalize" in engine.events


def test_pp_stage_skips_other_stages_layers(engine):
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


def test_initialized_model_load_preserves_engine_lifecycle(engine, monkeypatch):
    loader, config, model_config, _ = inputs(engine, monkeypatch)
    loader.load_initialized_model(config.model, config, model_config)
    assert "create" not in engine.events
    assert "finalize" not in engine.events
    assert engine.events[-1] == "close"
    assert any(
        isinstance(event, tuple) and event[0] == "fill" for event in engine.events
    )
    with pytest.raises(engine.native.UnsupportedEngineLayout, match="topology proof"):
        loader.load_weights(config.model, model_config)


def test_initialized_model_requires_real_tp_group(engine, monkeypatch):
    loader, config, model_config, _ = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = 4
    with pytest.raises(engine.native.UnsupportedEngineLayout, match="not initialized"):
        loader.load_initialized_model(config.model, config, model_config)
    assert engine.events == []


@pytest.mark.parametrize(
    "group_size,device_group,reason",
    [(4, object(), "differs from topology"), (2, None, "device group is unavailable")],
)
def test_initialized_model_rejects_unproven_collective_before_session(
    engine, monkeypatch, tp_distributed, group_size, device_group, reason
):
    loader, config, model_config, _ = inputs(engine, monkeypatch)
    config.parallel_config.tensor_parallel_size = 2
    tp_group = types.SimpleNamespace(
        device_group=device_group,
        world_size=group_size,
        rank_in_group=0,
        ranks=list(range(group_size)),
    )
    state = types.ModuleType("vllm.distributed.parallel_state")
    state.get_tp_group = lambda: tp_group
    monkeypatch.setitem(sys.modules, "vllm.distributed.parallel_state", state)
    with pytest.raises(engine.native.UnsupportedEngineLayout, match=reason):
        loader.load_initialized_model(config.model, config, model_config)
    assert engine.events == []
    assert tp_distributed.objects == []
    assert tp_distributed.broadcasts == []


def test_initialized_model_consumer_must_exhaust_iterator(engine, monkeypatch):
    loader, config, model_config, _ = inputs(engine, monkeypatch)
    with pytest.raises(RuntimeError, match="complete checkpoint"):
        loader.load_initialized_model(
            config.model,
            config,
            model_config,
            consume_weights=lambda weights: next(weights),
        )
    assert engine.events[-1] == "abort"
