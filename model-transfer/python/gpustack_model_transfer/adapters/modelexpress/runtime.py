"""The runtime probe and the accelerator claims this entry may make.

ModelExpress is optional: the adapter imports without it, a host learns
what is missing through the probe, and the reviewed entry's accelerator
claims are CUDA alone. An accelerator claim is a fact this module
refuses to overstate: the reviewed ModelExpress entry provides no CANN
adapter, and no configuration here may say it does.
"""

import importlib.util
from dataclasses import dataclass

from gpustack_model_transfer.adapters.modelexpress.constants import (
    LOAD_PLUGIN_VARIABLE,
    PINNED_COMMIT,
    QUALIFIED_ACCELERATORS,
)


@dataclass(frozen=True)
class EntryReadiness:
    """What the host has for the ModelExpress entry path."""

    modelexpress_available: bool
    plugin_hook_exposed: bool
    torch_available: bool
    cuda_available: bool
    missing: tuple

    @property
    def ready(self) -> bool:
        return not self.missing


def probe_entry(
    env: dict | None = None, cuda_available: bool | None = None
) -> EntryReadiness:
    """Probe the entry path without importing ModelExpress.

    env is the process environment as a mapping (tests inject); the
    plugin hook is exposed when the pinned variable is set. cuda_available,
    when given, stands in for the device probe.
    """
    import os

    source = os.environ if env is None else env
    missing = []
    modelexpress_available = _importable("modelexpress")
    torch_available = _importable("torch")
    if cuda_available is None:
        cuda_available = torch_available and _cuda_available()
    if not modelexpress_available:
        missing.append("modelexpress")
    if not torch_available:
        missing.append("torch")
    if not cuda_available:
        missing.append("cuda")
    plugin_hook_exposed = bool(source.get(LOAD_PLUGIN_VARIABLE))
    if not plugin_hook_exposed:
        missing.append(LOAD_PLUGIN_VARIABLE)
    return EntryReadiness(
        modelexpress_available=modelexpress_available,
        plugin_hook_exposed=plugin_hook_exposed,
        torch_available=torch_available,
        cuda_available=bool(cuda_available),
        missing=tuple(missing),
    )


def accelerator_claimed(accelerator: str) -> bool:
    """Say whether this entry may claim an accelerator family.

    CUDA only: the reviewed entry provides no CANN accelerator adapter,
    so a CANN claim is refused no matter what image it runs in.
    """
    return accelerator in QUALIFIED_ACCELERATORS


def refuse_cann() -> None:
    """The CANN refusal, as the standing rule this entry keeps."""
    raise ValueError(
        "the reviewed ModelExpress entry is a CUDA qualification target "
        f"({PINNED_COMMIT}); it provides no CANN accelerator adapter, and "
        "an Ascend claim must come from its own qualified adapter"
    )


def _importable(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def _cuda_available() -> bool:
    try:
        import torch

        return bool(torch.cuda.is_available())
    except Exception:
        return False


__all__ = [
    "EntryReadiness",
    "accelerator_claimed",
    "probe_entry",
    "refuse_cann",
]
