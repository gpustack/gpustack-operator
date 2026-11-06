"""Read published file ranges with path and extent checks.

Content verification belongs to cache publication, not the transfer reader.
"""

import contextlib
import os

from gpustack_model_transfer.core.descriptors import MAX_INT64
from gpustack_model_transfer.core.errors import (
    InvalidDescriptorError,
    RangeOverflowError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.manifest import load_manifest


class LocalFileReader:
    """Reads byte ranges from published files with validated extents."""

    def __init__(self, manifest_path, expected_sha256: str | None = None):
        self.manifest = load_manifest(manifest_path, expected_sha256)
        self._root = os.path.dirname(os.fspath(manifest_path))
        self._handles = {}
        try:
            for descriptor in self.manifest.files:
                self._open_file(descriptor)
        except BaseException:
            self.close()
            raise

    def identity(self):
        return self.manifest.identity()

    def _open_file(self, descriptor) -> None:
        from gpustack_model_transfer.source.fill import resolve_tree_file

        fd = resolve_tree_file(self._root, descriptor.path)
        try:
            if os.fstat(fd).st_size != descriptor.size:
                raise InvalidDescriptorError("file extent differs from manifest")
            handle = os.fdopen(fd, "rb")
        except BaseException:
            os.close(fd)
            raise
        self._handles[descriptor.path] = handle

    def read_bytes(self, file_path: str, begin: int, end: int) -> bytes:
        """Return the bytes at [begin, end) of file_path, freshly read."""
        descriptor = self.manifest.file(file_path)
        if begin < 0 or end < begin:
            raise RangeOverflowError(
                f"invalid range [{begin}, {end}) for {file_path!r}"
            )
        if begin > MAX_INT64 or end > MAX_INT64 or end > descriptor.size:
            raise RangeOverflowError(
                f"range [{begin}, {end}) exceeds file size {descriptor.size}"
            )
        handle = self._handles.get(file_path)
        if handle is None:
            raise SourceUnavailableError(f"file not open: {file_path!r}")
        try:
            if os.fstat(handle.fileno()).st_size != descriptor.size:
                raise SourceUnavailableError("file extent differs from manifest")
            handle.seek(begin)
            payload = handle.read(end - begin)
        except OSError as exc:
            raise SourceUnavailableError(
                f"read failed for {file_path!r}: {exc}"
            ) from None
        if len(payload) != end - begin:
            raise SourceUnavailableError(
                f"short read for {file_path!r}: wanted {end - begin} bytes "
                f"at offset {begin}, got {len(payload)}"
            )
        return payload

    def close(self) -> None:
        for handle in self._handles.values():
            with contextlib.suppress(OSError):
                handle.close()
        self._handles = {}


__all__ = ["LocalFileReader"]
