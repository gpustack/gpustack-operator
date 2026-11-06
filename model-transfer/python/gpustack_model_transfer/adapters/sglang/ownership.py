"""Tensor ownership and the borrowed-storage contract.

Owned storage is the default and needs no ceremony: the model keeps the
bytes. Borrowed storage is a favor the backend does the engine, and it is
only safe when the consumer declares, up front, the device stream the bytes
become visible on, the fence that makes them visible, and the release that
says the consumer is done — and only when the backend actually supports it.
"""

from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum


class TensorOwnership(Enum):
    """Who keeps the bytes a loaded tensor lives in."""

    OWNED = "owned"
    BORROWED = "borrowed"


@dataclass(frozen=True)
class BorrowedContract:
    """The explicit borrowed-storage contract a consumer must provide.

    visibility_fence is called once per borrowed tensor after its transfer
    completes, to make the bytes visible on the consumer stream. release is
    called after the fence; it returns True when the consumer is done with
    the bytes (the borrowed storage may be recycled) and False when the
    consumer retains them beyond the load scope, in which case the loader
    copies the tensor into owned storage instead.
    """

    stream: str
    visibility_fence: Callable[[str], None]
    release: Callable[[str], bool]

    def validate(self) -> None:
        if not isinstance(self.stream, str) or not self.stream:
            raise ValueError("borrowed storage needs a consumer stream name")
        if not callable(self.visibility_fence):
            raise ValueError("borrowed storage needs a visibility fence")
        if not callable(self.release):
            raise ValueError("borrowed storage needs a release callback")


@dataclass(frozen=True)
class LoadOptions:
    """How one load binds into the engine model.

    tie_word_embeddings mirrors the engine model configuration: when true,
    a checkpoint's lm_head.weight lands in the same storage as
    model.embed_tokens.weight, matching native tied-embedding semantics,
    and a checkpoint that omits lm_head.weight entirely is complete: the
    alias supplies the head from the embedding. engine_config is the
    engine's own configuration dict (including any KV or EC connector
    configuration); the loader records it untouched.
    """

    ownership: TensorOwnership = TensorOwnership.OWNED
    tie_word_embeddings: bool = False
    borrowed: BorrowedContract = None
    engine_config: dict = None

    def validate(self) -> None:
        if not isinstance(self.ownership, TensorOwnership):
            raise ValueError(
                f"ownership must be a TensorOwnership, got {self.ownership!r}"
            )
        if not isinstance(self.tie_word_embeddings, bool):
            raise ValueError("tie_word_embeddings must be a bool")
        if self.borrowed is not None:
            self.borrowed.validate()
        if self.engine_config is not None and not isinstance(self.engine_config, dict):
            raise ValueError("engine_config must be a dict when given")


__all__ = [
    "BorrowedContract",
    "LoadOptions",
    "TensorOwnership",
]
