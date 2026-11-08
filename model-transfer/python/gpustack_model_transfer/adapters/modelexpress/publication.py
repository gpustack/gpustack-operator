"""The publication gate: only a completed strategy load publishes.

The gate reads the plugin result's own record: a GPUStack strategy that
finished loading marks its result publishable, and the chain then
registers the loaded model's tensors and publishes them for Peer D2D; a
result the strategy never completed stays unpublished, and the gate
refuses it. `WriterRefitFence` is an optional second input the caller
supplies when it wants the publication to also answer to a fence's
refit standing — the strategy's own live path gates on the completed
load alone, and the acceptance suite passes a fence here to exercise
the refusal contract. Nothing here needs the ModelExpress Rust server:
GPUStack source identity and the verified host-memory service are
their own.
"""

from gpustack_model_transfer.adapters.modelexpress.plugin import PluginResult
from gpustack_model_transfer.adapters.modelexpress.policy import WriterRefitFence


class PublicationRefusedError(Exception):
    """Publication of a non-publishable result was attempted."""


def publication_allowed(
    result: PluginResult, fence: WriterRefitFence | None = None
) -> bool:
    """Say whether a result may be published as a Peer-D2D source.

    True only for a result the strategy marked publishable after a
    completed load: the chain registers that model's tensors and
    publishes them so peer replicas can read them device-to-device. A
    result from a failed or fallback load never publishes. The fence is
    optional — the live path passes none and gates on the completed
    load alone; a caller that supplies one adds its refit standing to
    the gate.
    """
    if not result.publishable:
        return False
    return fence is None or bool(fence.publishable())


def refuse_publication(result: PluginResult, fence: WriterRefitFence) -> None:
    """The gate as a refusal: raise when publication is not allowed."""
    if not publication_allowed(result, fence):
        raise PublicationRefusedError(
            "the result is not publishable: only a completed strategy "
            "load publishes, and this result never finished one"
        )


def checkpoint_identity(manifest_digest: str) -> dict:
    """The verified checkpoint identity a publication would need.

    Identity for publication is the raw checkpoint's verified manifest
    digest under its writer's completed fence — never a generation the
    refit has not finished or a writer still holds.
    """
    return {"kind": "verified-checkpoint", "manifest_digest": manifest_digest}


__all__ = [
    "PublicationRefusedError",
    "checkpoint_identity",
    "publication_allowed",
    "refuse_publication",
]
