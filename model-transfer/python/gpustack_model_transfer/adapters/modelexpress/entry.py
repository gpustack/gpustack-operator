"""ModelExpress's public plugin entry over authenticated remote weight loading."""

import logging
import os

from gpustack_model_transfer.adapters.modelexpress.plugin import (
    STRATEGY_NAME,
    GpustackLoadPlugin,
)
from gpustack_model_transfer.adapters.modelexpress.policy import (
    PluginUnavailableError,
    StrategyRecoveryError,
)

# The environment contract the pinned image sets for the entry. The
# manifest path and its verified digest are the source identity; the
# policy mirrors the cluster's remote delivery setting.
ENV_MANIFEST = "GPUSTACK_MODEL_TRANSFER_MANIFEST"
ENV_MANIFEST_SHA256 = "GPUSTACK_MODEL_TRANSFER_MANIFEST_SHA256"
ENV_POLICY = "GPUSTACK_MODEL_TRANSFER_POLICY"
ENV_PEER_P2P = "GPUSTACK_MODEL_TRANSFER_PEER_P2P"
ENV_PEERS_CONFIG = "GPUSTACK_MODEL_TRANSFER_CONFIG"

# Where ModelExpress's own strategy base may live, in the order the
# pinned tree was reviewed against.
_UPSTREAM_MODULES = (
    "modelexpress.load_strategy",
    "modelexpress",
    "modelexpress.strategy",
)


def _upstream_strategy_base():
    """ModelExpress's own LoadStrategy base, when importable."""
    import importlib

    for module_name in _UPSTREAM_MODULES:
        try:
            module = importlib.import_module(module_name)
        except Exception:
            continue
        base = getattr(module, "LoadStrategy", None)
        if isinstance(base, type):
            return base
    return None


_STRATEGY_BASE = _upstream_strategy_base()


def artifact_environment(env=None):
    """Validate the artifact environment without opening any reader.

    Returns (manifest_path, digest, policy). Raises the same errors
    build_plugin raises, so a probe can answer False without touching
    the artifact files: availability is a property of the environment,
    and opening the verified reader is the load's own first act.
    """
    from gpustack_model_transfer.adapters.modelexpress.constants import (
        POLICY_PREFER,
        POLICY_REQUIRE,
    )
    from gpustack_model_transfer.envs import get_envs

    cfg = get_envs(env)
    manifest = cfg.GPUSTACK_MODEL_TRANSFER_MANIFEST or ""
    digest = cfg.GPUSTACK_MODEL_TRANSFER_MANIFEST_SHA256 or ""
    if not manifest or not digest:
        missing = [
            name
            for name, value in ((ENV_MANIFEST, manifest), (ENV_MANIFEST_SHA256, digest))
            if not value
        ]
        raise PluginUnavailableError(
            "the strategy entry needs the verified artifact environment: "
            + ", ".join(missing)
        )
    policy = cfg.GPUSTACK_MODEL_TRANSFER_POLICY
    if policy not in (POLICY_REQUIRE, POLICY_PREFER):
        raise ValueError(f"{ENV_POLICY} must be require or prefer, not {policy!r}")
    return manifest, digest, policy


def peer_replica_endpoints(env=None) -> tuple:
    """Discover ModelDeployment peer endpoints without an external metadata server.

    Reads the mounted GPUSTACK_MODEL_TRANSFER_CONFIG (endpoints.json) and filters
    out the current Pod (by KUBERNETES_POD_UID or KUBERNETES_POD_IP) so ModelExpress
    P2P extensions can copy compiled GPU tensors across peer replicas directly.
    """
    import json
    import os

    from gpustack_model_transfer.envs import get_envs

    cfg = get_envs(env)
    config_path = cfg.GPUSTACK_MODEL_TRANSFER_CONFIG or ""
    if not config_path or not os.path.isabs(config_path):
        return ()
    try:
        with open(config_path, "rb") as handle:
            raw = handle.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            return ()
        parsed = json.loads(raw)
        endpoints = parsed.get("endpoints") if isinstance(parsed, dict) else None
        if not isinstance(endpoints, list):
            return ()
    except (OSError, ValueError, TypeError):
        return ()
    self_uid = cfg.KUBERNETES_POD_UID or ""
    self_ip = cfg.KUBERNETES_POD_IP or ""
    peers = []
    for item in endpoints:
        if not isinstance(item, dict):
            continue
        pod_uid = item.get("podUID")
        address = item.get("address", "")
        if self_uid and pod_uid == self_uid:
            continue
        if self_ip and isinstance(address, str) and address.startswith(self_ip + ":"):
            continue
        peers.append(dict(item))
    return tuple(peers)


def build_plugin(env=None):
    """Build the local reference plugin used by the portable acceptance suite.

    env is the process environment as a mapping; None means os.environ.
    Raises PluginUnavailableError naming what is missing. The caller
    owns the plugin's reader from here: close it through the strategy's
    close(), or the plugin loader's own close on the load paths.
    """
    from gpustack_model_transfer.adapters.vllm import ModelTransferModelLoader
    from gpustack_model_transfer.adapters.vllm.torch_runtime import TorchRuntimeDriver
    from gpustack_model_transfer.core.budgets import (
        MAX_WINDOW_BYTES,
        ReadBudgets,
        transfer_workers,
    )
    from gpustack_model_transfer.core.capabilities import local_backend_capabilities
    from gpustack_model_transfer.core.local_reader import LocalFileReader
    from gpustack_model_transfer.envs import get_envs

    source = get_envs(env)
    manifest, digest, policy = artifact_environment(source)
    workers = transfer_workers(source)
    reader = LocalFileReader(manifest, expected_sha256=digest)
    loader = ModelTransferModelLoader(
        reader=reader,
        capabilities=local_backend_capabilities(),
        budgets=ReadBudgets(
            window_bytes=MAX_WINDOW_BYTES,
            max_inflight=workers,
            owned_output_bytes=4 << 30,
        ),
    )
    peer_p2p = source.GPUSTACK_MODEL_TRANSFER_PEER_P2P
    composition = {
        "enabled": {"p2p": peer_p2p, "rdma": True, "server_cache": False},
        "authorized": {"p2p": True} if peer_p2p else {},
    }
    return GpustackLoadPlugin(
        loader,
        TorchRuntimeDriver(),
        composition,
        policy,
        publishable=peer_p2p,
    )


_BASE = object if _STRATEGY_BASE is None else _STRATEGY_BASE
_LOG = logging.getLogger(__name__)
_process_unusable = False


def validate_environment(env=None):
    """Refuse an entry that could skip GPUStack or publish its loaded weights."""
    source = os.environ if env is None else env
    required = {
        "MX_LOAD_PLUGIN": STRATEGY_NAME,
        "MX_LOAD_PLUGIN_BEFORE": "rdma",
        "MX_LOAD_STRATEGY_CHAIN": "INFERENCE",
        "GPUSTACK_MODEL_TRANSFER_BACKEND": "nixl",
        "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "gpustack-modelexpress",
        "HF_HUB_OFFLINE": "1",
        "TRANSFORMERS_OFFLINE": "1",
        "MX_ARTIFACT_TRANSFER": "0",
        "MX_VMM_ARENA": "0",
    }
    for name, expected in required.items():
        if source.get(name) != expected:
            raise StrategyRecoveryError(f"{name} must be {expected!r}")
    if source.get(ENV_POLICY, "require") != "require":
        raise StrategyRecoveryError("ModelExpress remote loading supports Require only")
    if source.get(ENV_PEER_P2P, "0").lower() not in ("0", "false", ""):
        raise StrategyRecoveryError("ModelExpress peer publication is not supported")
    for name in ("MODEL_EXPRESS_URL", "MX_SERVER_ADDRESS", "MX_MODEL_URI"):
        if source.get(name):
            raise StrategyRecoveryError(
                f"{name} is outside the remote loading contract"
            )
    if source.get("MX_METADATA_BACKEND", "") not in ("", "server"):
        raise StrategyRecoveryError("external ModelExpress metadata is not supported")


def validate_plugin():
    """Require the exact GPUStack strategy in the pinned upstream registry."""
    validate_environment()
    try:
        from modelexpress.plugins import initialize_plugins, plugins

        initialize_plugins()
        if plugins.get(STRATEGY_NAME) is not GpustackLoadStrategy:
            raise StrategyRecoveryError(
                "the registered ModelExpress strategy is not the GPUStack loader"
            )
    except StrategyRecoveryError:
        raise
    except Exception as exc:
        raise StrategyRecoveryError(
            "the GPUStack ModelExpress plugin could not be registered"
        ) from exc


def _vllm_context(ctx):
    from modelexpress.engines.vllm.adapter import VllmAdapter

    if ctx is None or not isinstance(getattr(ctx, "adapter", None), VllmAdapter):
        raise StrategyRecoveryError("a vLLM ModelExpress load context is required")
    if ctx.is_reload or ctx.p2p_role != "main":
        raise StrategyRecoveryError(
            "ModelExpress remote loading supports cold main loads only"
        )
    adapter = ctx.adapter
    config = adapter.vllm_config
    if (
        ctx.model_config is not adapter.model_config
        or ctx.load_config is not config.load_config
    ):
        raise StrategyRecoveryError("ModelExpress and vLLM configuration disagree")
    if getattr(ctx.target_device, "type", None) != "cuda":
        raise StrategyRecoveryError("ModelExpress remote loading requires CUDA")
    if getattr(config, "speculative_config", None) is not None:
        raise StrategyRecoveryError("speculative loading is not supported")
    parallel = config.parallel_config
    for name in (
        "pipeline_parallel_size",
        "data_parallel_size",
        "prefill_context_parallel_size",
        "decode_context_parallel_size",
    ):
        if getattr(parallel, name, None) != 1:
            raise StrategyRecoveryError(
                "ModelExpress remote loading supports tensor parallelism only"
            )
    if getattr(parallel, "enable_expert_parallel", None) is not False:
        raise StrategyRecoveryError("ModelExpress expert parallelism is not qualified")
    return config


def _agree_loading_group(config):
    """Agree on the remote path before any rank opens a loading session."""
    from gpustack_model_transfer.backends.nixl.runtime import probe_runtime
    from gpustack_model_transfer.core.budgets import tp_load_mode

    try:
        readiness = probe_runtime()
        ready, version = readiness.ready, readiness.version
    except Exception:
        ready, version = False, ""
    proposal = (STRATEGY_NAME, "nixl", version, ready, tp_load_mode())
    expected = config.parallel_config.tensor_parallel_size
    if not isinstance(expected, int) or isinstance(expected, bool) or expected < 1:
        raise StrategyRecoveryError("tensor parallelism is unknown")
    proposals = [proposal]
    if expected > 1:
        import torch
        from vllm.distributed import get_tp_group

        group = get_tp_group()
        if group.world_size != expected or group.device_group is None:
            raise StrategyRecoveryError("the loading group differs from the topology")
        proposals = [None] * expected
        torch.distributed.all_gather_object(
            proposals, proposal, group=group.device_group
        )
    if not ready or any(item != proposal for item in proposals):
        raise StrategyRecoveryError("the loading group disagrees on the remote path")


class GpustackLoadStrategy(_BASE):
    """Require-only cold vLLM load using ModelExpress's actual strategy ABI."""

    name = STRATEGY_NAME

    def __init__(self):
        self._native = None
        self._loaded_result = None
        self._prepared_result = None
        self._finalized = False
        self._failed = False

    def peer_endpoints(self, env=None) -> tuple:
        return peer_replica_endpoints(env)

    def is_available(self, ctx=None) -> bool:
        # Returning False would let the real chain try a forbidden fallback.
        try:
            if _process_unusable:
                raise StrategyRecoveryError(
                    "a prior remote load failed; restart the process"
                )
            validate_environment()
            _vllm_context(ctx)
            ctx.p2p_enabled = False
        except Exception as exc:
            raise StrategyRecoveryError(
                "ModelExpress remote entry is unavailable"
            ) from exc
        return True

    def prepare(self, result, ctx):
        try:
            self.is_available(ctx)
            if self._failed or self._prepared_result is not None:
                raise StrategyRecoveryError("a ModelExpress strategy cannot be reused")
            if result.model is None or result.value is not result.model:
                raise StrategyRecoveryError(
                    "ModelExpress has no initialized model to load"
                )
            result.publishable = False
            _agree_loading_group(_vllm_context(ctx))
            self._prepared_result = result
        except Exception as exc:
            raise StrategyRecoveryError(
                "ModelExpress remote preparation failed"
            ) from exc

    def load(self, result, ctx):
        global _process_unusable
        try:
            if (
                self._failed
                or self._prepared_result is None
                or result is not self._prepared_result
                or self._loaded_result is not None
            ):
                raise StrategyRecoveryError(
                    "ModelExpress load has no fresh prepared model"
                )
            config = _vllm_context(ctx)
            validate_environment()
            from gpustack_model_transfer.adapters.vllm.native import (
                NativeModelTransferLoader,
            )

            native = NativeModelTransferLoader(ctx.load_config)
            self._native = native
            _LOG.info(
                "GPUStack ModelExpress remote load starting rank=%s", ctx.global_rank
            )

            def consume(weights):
                loaded = ctx.adapter.apply_weight_iter(result, weights)
                if loaded is not result:
                    raise StrategyRecoveryError(
                        "ModelExpress replaced the load result envelope"
                    )
                return

            native.load_initialized_model(
                result.model, config, ctx.model_config, consume_weights=consume
            )
            self._loaded_result = result
            result.metadata["gpustack_remote_loaded"] = True
            _LOG.info(
                "GPUStack ModelExpress remote load completed rank=%s", ctx.global_rank
            )
            return result
        except BaseException as exc:
            _process_unusable = True
            self._failed = True
            self._native = None
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            raise StrategyRecoveryError(
                "ModelExpress remote load failed; fallback is forbidden"
            ) from exc

    def finalize(self, result, ctx):
        global _process_unusable
        try:
            if (
                self._failed
                or self._loaded_result is None
                or result is not self._loaded_result
                or self._finalized
            ):
                raise StrategyRecoveryError(
                    "nothing was loaded through this strategy to finalize"
                )
            result.publishable = False
            ctx.p2p_enabled = False
            loaded = ctx.adapter.after_weight_iter_load(result)
            if loaded is not result:
                raise StrategyRecoveryError(
                    "ModelExpress replaced the finalized model envelope"
                )
            self._finalized = True
        except BaseException as exc:
            _process_unusable = True
            self._failed = True
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            raise StrategyRecoveryError(
                "ModelExpress finalization failed; fallback is forbidden"
            ) from exc
        finally:
            self._native = None

    def abort(self, result, ctx):
        # Native loading already aborted its session and retained unknown DMA.
        self._failed = True
        self._native = None
        result.publishable = False
        ctx.p2p_enabled = False

    def rollback(self, ctx):
        self._failed = True
        self._native = None

    def close(self):
        self._native = None


def register_plugin():
    """Return the concrete strategy class for MX_PLUGIN_DIR discovery."""
    if _STRATEGY_BASE is None:
        raise PluginUnavailableError("ModelExpress LoadStrategy is unavailable")
    return GpustackLoadStrategy


__all__ = [
    "ENV_MANIFEST",
    "ENV_MANIFEST_SHA256",
    "ENV_PEERS_CONFIG",
    "ENV_PEER_P2P",
    "ENV_POLICY",
    "GpustackLoadStrategy",
    "artifact_environment",
    "build_plugin",
    "peer_replica_endpoints",
    "register_plugin",
    "validate_environment",
    "validate_plugin",
]
