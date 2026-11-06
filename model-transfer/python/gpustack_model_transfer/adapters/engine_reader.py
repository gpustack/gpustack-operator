"""Contract between native engine loaders and the authenticated broker client.

The production factory is implemented by the client integration. It must never
substitute a local reader. This module deliberately imports no engine runtime.
"""

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Protocol


@dataclass(frozen=True)
class TensorDescriptor:
    name: str
    shape: tuple[int, ...]
    dtype: str


class DescriptorSequence(Sequence):
    """Expose engine descriptors without copying a verified tensor index."""

    def __init__(self, tensors):
        self.tensors = tensors

    def __len__(self):
        return len(self.tensors)

    def __iter__(self):
        return (TensorDescriptor(t.name, tuple(t.shape), t.dtype) for t in self.tensors)

    def __getitem__(self, index):
        item = self.tensors[index]
        if isinstance(index, slice):
            return DescriptorSequence(item)
        return TensorDescriptor(item.name, tuple(item.shape), item.dtype)

    def __eq__(self, other):
        if not isinstance(other, Sequence):
            return NotImplemented
        return len(self) == len(other) and all(
            a == b for a, b in zip(self, other, strict=True)
        )


class ReaderSession(Protocol):
    def fill(self, descriptor: TensorDescriptor, target: object) -> None:
        """Fill an owned device tensor and wait until it is safe to consume.

        Retain each target until close or a proven-safe abort, including when
        this method raises with unknown in-flight work.
        """

    def close(self) -> None:
        """Fence engine consumption, then release load-only resources and session."""

    def abort(self) -> None:
        """Retain targets and registrations with unknown in-flight state.

        Retention must outlive this object when completion remains unknown.
        """


class ReaderProvider(Protocol):
    def probe(self, model_config: object) -> Sequence[TensorDescriptor]:
        """Authenticate and read only configuration, index and tensor headers.

        Descriptors must be verified against the immutable artifact that
        start() opens. Release metadata-only resources and never fetch weight
        payloads or entire checkpoint files during this method.
        The returned sequence may be backed by a bounded disk index.
        """

    def start(self, model_config: object) -> ReaderSession:
        """Open a session for the pinned artifact from probe.

        If opening fails, retain any registrations with unknown in-flight state
        without relying on a session object being returned to the caller.
        """


def create_reader_provider(load_config: object, tp_size: int = 1) -> ReaderProvider:
    """Load the production client factory only when the native loader runs.

    tp_size is the engine TP group's world size; under the "all" TP load
    mode the ranks share one window budget and each reads its own share.
    """
    from gpustack_model_transfer.client import create_engine_reader

    return create_engine_reader(load_config, tp_size=tp_size)
