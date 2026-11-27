"""Zero-intrusion engine sentinel package for vLLM and SGLang."""

from __future__ import annotations

import importlib.abc
import importlib.machinery
import importlib.metadata
import sys
from typing import Any


def _resolve_package_version() -> str:
    try:
        return importlib.metadata.version("gpustack-engine-sentinel")
    except importlib.metadata.PackageNotFoundError:
        return "0.0.0"


__version__: str = _resolve_package_version()

_BOOTSTRAP_INSTALLED = False

_VLLM_API_SERVER_TARGETS: frozenset[str] = frozenset(
    {
        "vllm.entrypoints.launchers.app",
        "vllm.entrypoints.launchers.api_server.app_state",
        "vllm.entrypoints.launchers.api_server.entry",
        "vllm.entrypoints.openai.api_server",
    }
)

_HOOK_TARGETS: frozenset[str] = _VLLM_API_SERVER_TARGETS | frozenset(
    {
        "vllm_omni.entrypoints.openai.api_server",
        "vllm.v1.engine.core",
        "vllm.model_executor.custom_op",
        "vllm.v1.worker.gpu_model_runner",
        "sglang.srt.managers.scheduler",
        "sglang.srt.managers.scheduler_components.internal_state",
        "sglang.srt.entrypoints.http_server",
        "sglang.srt.model_executor.model_runner",
        "sglang.srt.layers.layernorm",
        "sglang.srt.layers.rotary_embedding",
        "uvicorn.config",
    }
)


def _apply_target_hook(fullname: str, module: Any) -> None:
    if fullname in _VLLM_API_SERVER_TARGETS:
        from gpustack_engine_sentinel.vllm.hooks import patch_vllm_api_server_module

        patch_vllm_api_server_module(module)
    elif fullname == "vllm_omni.entrypoints.openai.api_server":
        from gpustack_engine_sentinel.vllm.hooks import (
            patch_vllm_omni_api_server_module,
        )

        patch_vllm_omni_api_server_module(module)
    elif fullname == "vllm.v1.engine.core":
        engine_core_cls = getattr(module, "EngineCore", None)
        if engine_core_cls is not None:
            from gpustack_engine_sentinel.vllm.utility import (
                attach_engine_core_utility,
            )

            attach_engine_core_utility(engine_core_cls)
    elif fullname == "vllm.model_executor.custom_op":
        from gpustack_engine_sentinel.kernel_loader import patch_vllm_custom_op_module

        patch_vllm_custom_op_module(module)
    elif fullname == "vllm.v1.worker.gpu_model_runner":
        from gpustack_engine_sentinel.kernel_loader import (
            patch_vllm_gpu_model_runner_module,
        )

        patch_vllm_gpu_model_runner_module(module)
    elif fullname == "sglang.srt.managers.scheduler":
        from gpustack_engine_sentinel.sglang.hooks import (
            patch_sglang_scheduler_module,
        )

        patch_sglang_scheduler_module(module)
    elif fullname == "sglang.srt.managers.scheduler_components.internal_state":
        from gpustack_engine_sentinel.sglang.hooks import (
            patch_sglang_internal_state_module,
        )

        patch_sglang_internal_state_module(module)
    elif fullname == "sglang.srt.entrypoints.http_server":
        from gpustack_engine_sentinel.sglang.hooks import (
            patch_sglang_http_server_module,
        )

        patch_sglang_http_server_module(module)
    elif fullname == "sglang.srt.model_executor.model_runner":
        from gpustack_engine_sentinel.kernel_loader import (
            patch_sglang_model_runner_module,
        )

        patch_sglang_model_runner_module(module)
    elif fullname == "sglang.srt.layers.layernorm":
        from gpustack_engine_sentinel.kernel_loader import (
            patch_sglang_layernorm_module,
        )

        patch_sglang_layernorm_module(module)
    elif fullname == "sglang.srt.layers.rotary_embedding":
        from gpustack_engine_sentinel.kernel_loader import (
            patch_sglang_rotary_embedding_module,
        )

        patch_sglang_rotary_embedding_module(module)
    elif fullname == "uvicorn.config":
        from gpustack_engine_sentinel.uvicorn_hook import patch_uvicorn_config_module

        patch_uvicorn_config_module(module)


class _SentinelPostImportFinder(importlib.abc.MetaPathFinder):
    """Lazily patches vLLM and SGLang modules on first import without eager framework imports."""

    def __init__(self) -> None:
        self._active: set[str] = set()

    def find_spec(
        self, fullname: str, path: Any, target: Any = None
    ) -> importlib.machinery.ModuleSpec | None:
        if fullname not in _HOOK_TARGETS or fullname in self._active:
            return None
        self._active.add(fullname)
        try:
            spec = importlib.machinery.PathFinder.find_spec(fullname, path, target)
        finally:
            self._active.discard(fullname)
        if spec is None or spec.loader is None:
            return None
        orig_exec = spec.loader.exec_module

        def _exec_module(module: Any) -> None:
            orig_exec(module)
            _apply_target_hook(fullname, module)

        spec.loader.exec_module = _exec_module  # type: ignore[method-assign]
        return spec


def bootstrap_process() -> None:
    """Install lazy import hooks in the current interpreter (main or spawned child)."""
    global _BOOTSTRAP_INSTALLED
    if _BOOTSTRAP_INSTALLED:
        return
    _BOOTSTRAP_INSTALLED = True
    sys.meta_path.insert(0, _SentinelPostImportFinder())
    for target_name in _HOOK_TARGETS:
        loaded = sys.modules.get(target_name)
        if loaded is not None:
            _apply_target_hook(target_name, loaded)


__all__ = [
    "__version__",
    "bootstrap_process",
]
