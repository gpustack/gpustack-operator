"""The publication gate: a live loaded model is never a Stage-2 source.

Finalize and publication cannot expose the loaded model as an external
Stage-2 P2P source implicitly, so the gate answers from the plugin
result's own record — publishable is False for every GPUStack-loaded
model — and from the fence's standing between verified checkpoint
identity and live processed-tensor generations. Nothing here needs the
ModelExpress Rust server: GPUStack source identity and the verified
host-memory service are their own.
"""

from gpustack_model_transfer.adapters.modelexpress.plugin import PluginResult
from gpustack_model_transfer.adapters.modelexpress.policy import WriterRefitFence


class PublicationRefusedError(Exception):
    """Publication of a non-publishable result was attempted."""


def publication_allowed(result: PluginResult, fence: WriterRefitFence) -> bool:
    """Say whether a result may be published as a Stage-2 source.

    False for every GPUStack plugin result: the loaded model is a live
    processed-tensor generation, not verified checkpoint content, and
    the fence keeps the two identity spaces apart.
    """
    return bool(result.publishable and fence.publishable())


def refuse_publication(result: PluginResult, fence: WriterRefitFence) -> None:
    """The gate as a refusal: raise when publication is not allowed."""
    if not publication_allowed(result, fence):
        raise PublicationRefusedError(
            "the result is not publishable: a live processed-tensor "
            "generation is never an external Stage-2 source"
        )


def checkpoint_identity(manifest_digest: str) -> dict:
    """The verified checkpoint identity a publication would need.

    Identity for publication is the raw checkpoint's verified manifest
    digest under its writer's completed fence — never a live generation
    the refit has not finished or a writer still holds.
    """
    return {"kind": "verified-checkpoint", "manifest_digest": manifest_digest}


__all__ = [
    "PublicationRefusedError",
    "checkpoint_identity",
    "publication_allowed",
    "refuse_publication",
]
