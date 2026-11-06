"""Copy published files into owned pinned memory with bounded reads.

Cache publication owns content verification. Transfer retains path, extent,
range, cancellation and native ownership checks without hashing payloads.
"""

import os
import stat
import threading
from concurrent.futures import ThreadPoolExecutor

from gpustack_model_transfer.core.budgets import hixl_allocation_bytes
from gpustack_model_transfer.core.errors import (
    AuthenticationError,
    InvalidDescriptorError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.manifest import (
    MAX_MANIFEST_FILES,
    MAX_MANIFEST_TENSORS,
)

FILL_CHUNK_BYTES = 1 << 20

# Bound what one work order may admit before any native allocation.
MAX_FILE_BYTES = 1 << 40
MAX_TOTAL_BYTES = 4 << 40
MAX_REGISTRATIONS = 4096


class FillAborted(SourceUnavailableError):
    """Fill was cancelled between bounded file reads."""


class FillReport:
    """What one fill did: extents, counts, and the header/gap accounting."""

    def __init__(self):
        self.files_filled = 0
        self.bytes_filled = 0
        self.tensors_filled = 0
        # Bytes of each file NOT covered by a tensor descriptor: the
        # safetensors header and any alignment gaps, counted per file.
        self.header_and_gap_bytes = 0

    def as_dict(self) -> dict:
        return {
            "filesFilled": self.files_filled,
            "bytesFilled": self.bytes_filled,
            "tensorsFilled": self.tensors_filled,
            "headerAndGapBytes": self.header_and_gap_bytes,
        }


def resolve_tree_file(tree_path: str, relative: str) -> int:
    """Open one manifest file inside the verified tree, safely.

    The path must be relative, must not climb out by name, must not
    escape by symlink at any component (O_NOFOLLOW on the leaf, and a
    realpath containment check on the whole path), and must be a regular
    file. The caller owns the returned descriptor.
    """
    if not tree_path or not relative:
        raise InvalidDescriptorError("tree path and file path are required")
    if os.path.isabs(relative) or ".." in relative.split(os.sep) or "\x00" in relative:
        raise InvalidDescriptorError("unsafe manifest path: refused, not opened")
    flags = os.O_RDONLY | os.O_NOFOLLOW
    directory_flags = flags | os.O_DIRECTORY
    try:
        directory = os.open(tree_path, directory_flags)
        try:
            parts = relative.split("/")
            for part in parts[:-1]:
                child = os.open(part, directory_flags, dir_fd=directory)
                os.close(directory)
                directory = child
            handle = os.open(parts[-1], flags, dir_fd=directory)
        finally:
            os.close(directory)
    except OSError:
        raise SourceUnavailableError("cannot safely open a manifest file") from None
    try:
        if not stat.S_ISREG(os.fstat(handle).st_mode):
            raise InvalidDescriptorError("a manifest path must be a regular file")
    except BaseException:
        os.close(handle)
        raise
    return handle


def registered_files(manifest):
    """The broker admits only safetensors files referenced by tensors."""
    paths = {tensor.file for tensor in manifest.tensors}
    if any(not path.endswith(".safetensors") for path in paths):
        raise InvalidDescriptorError("tensor file is not safetensors")
    return tuple(f for f in manifest.files if f.path in paths and f.size > 0)


def resident_file_bytes(size, backend):
    """Charge the complete allocation while keeping the file extent immutable."""
    return hixl_allocation_bytes(size) if backend == "hixl" else size


def resident_files(manifest, budget_bytes, start_name=None, *, backend=None):
    """Select a contiguous batch in path order, bounded by the resident budget."""
    files = sorted(registered_files(manifest), key=lambda f: f.path)
    if start_name is not None:
        index = next((i for i, f in enumerate(files) if f.path == start_name), None)
        if index is None:
            raise InvalidDescriptorError("source file is not in the manifest")
        files = files[index:]
    selected, size = [], 0
    for descriptor in files:
        charge = resident_file_bytes(descriptor.size, backend)
        if size + charge > budget_bytes:
            break
        selected.append(descriptor)
        size += charge
    if not selected:
        raise InvalidDescriptorError("resident budget cannot hold a source file")
    return tuple(selected)


def check_admission(manifest, total_bytes: int, registrations: int) -> None:
    """Check the manifest's own extent against the work order's admission
    and the process bounds — before any native allocation or start."""
    if isinstance(total_bytes, bool) or not isinstance(total_bytes, int):
        raise InvalidDescriptorError("totalBytes admission must be an integer")
    if isinstance(registrations, bool) or not isinstance(registrations, int):
        raise InvalidDescriptorError("registrations admission must be an integer")
    nonempty = registered_files(manifest)
    if (
        len(manifest.files) > MAX_MANIFEST_FILES
        or len(manifest.tensors) > MAX_MANIFEST_TENSORS
    ):
        raise InvalidDescriptorError("manifest count exceeds bound")
    for descriptor in nonempty:
        if descriptor.size < 0 or descriptor.size > MAX_FILE_BYTES:
            raise InvalidDescriptorError(
                "a manifest file declares an out-of-bounds size"
            )
    extent = sum(f.size for f in nonempty)
    if extent != total_bytes:
        raise InvalidDescriptorError(
            "the manifest's file extent does not match the work order's "
            "registered-byte admission"
        )
    if len(nonempty) != registrations:
        raise InvalidDescriptorError(
            "the manifest's nonempty file count does not match the work "
            "order's registration admission"
        )
    if registrations > MAX_REGISTRATIONS or extent > MAX_TOTAL_BYTES:
        raise InvalidDescriptorError(
            "the work order exceeds this source's bounded registrations "
            "or byte admission"
        )


def fill_from_tree(
    manifest,
    tree_path: str,
    blocks: dict,
    chunk_bytes: int = FILL_CHUNK_BYTES,
    should_abort=None,
    *,
    resident_only=False,
) -> FillReport:
    """Copy the selected files into owned blocks, without payload hashes.

    Every block must have the declared size. Path and extent checks precede
    bounded reads. Files run concurrently; errors retain descriptor order.
    All writers stop before return. The caller owns cleanup on failure.
    resident_only limits reads to the allocated batch, including initial fill.
    """
    if (
        isinstance(chunk_bytes, bool)
        or not isinstance(chunk_bytes, int)
        or not 0 < chunk_bytes <= FILL_CHUNK_BYTES
    ):
        raise InvalidDescriptorError("invalid fill chunk size")
    report = FillReport()
    descriptors = registered_files(manifest)
    if resident_only:
        descriptors = tuple(f for f in descriptors if f.path in blocks)
    from gpustack_model_transfer.core.tensor_index import tensor_tables

    _, path_tensors, _ = tensor_tables(manifest)

    cancellation = _FillCancellation(len(descriptors), should_abort)

    def fill_one(index):
        descriptor = descriptors[index]
        try:
            cancellation.check(index)
            if descriptor.path not in blocks:
                raise InvalidDescriptorError(
                    "no pinned block exists for a manifest file"
                )
            return _fill_file(
                descriptor,
                path_tensors[descriptor.path],
                tree_path,
                blocks.get(descriptor.path),
                chunk_bytes,
                lambda: cancellation.check(index),
            )
        except BaseException:
            cancellation.fail(index)
            raise

    if len(descriptors) <= 1:
        return fill_one(0) if descriptors else report

    workers = min(len(descriptors), os.cpu_count() or 1, 8)
    with ThreadPoolExecutor(max_workers=workers) as executor:
        futures = [executor.submit(fill_one, i) for i in range(len(descriptors))]
        try:
            for future in futures:
                filled = future.result()
                report.files_filled += filled.files_filled
                report.bytes_filled += filled.bytes_filled
                report.tensors_filled += filled.tensors_filled
                report.header_and_gap_bytes += filled.header_and_gap_bytes
        finally:
            cancellation.stop.set()
            for future in futures:
                future.cancel()
    return report


class _FillCancellation:
    def __init__(self, count, should_abort):
        self.failed_index = count
        self.should_abort = should_abort
        self.lock = threading.Lock()
        self.stop = threading.Event()

    def fail(self, index):
        with self.lock:
            self.failed_index = min(self.failed_index, index)

    def check(self, index):
        with self.lock:
            cancelled = index > self.failed_index
        if (
            cancelled
            or self.stop.is_set()
            or (self.should_abort is not None and self.should_abort())
        ):
            raise FillAborted("fill aborted")


def _fill_file(descriptor, path_tensors, tree_path, block, chunk_bytes, check_abort):
    check_abort()
    if block is None or block.byte_size != descriptor.size:
        raise InvalidDescriptorError(
            "no pinned block of the declared size exists for a manifest file"
        )
    handle = resolve_tree_file(tree_path, descriptor.path)
    total = 0
    try:
        if os.fstat(handle).st_size != descriptor.size:
            raise AuthenticationError("file extent does not match manifest")
        for tensor in path_tensors:
            tensor.validate(descriptor.size)
        while total < descriptor.size:
            check_abort()
            length = min(chunk_bytes, descriptor.size - total)
            view = block.view(total, length)
            count = os.readv(handle, [view])
            if not count:
                break
            total += count
        check_abort()
        if os.read(handle, 1):
            raise AuthenticationError("file exceeds manifest extent")
    except OSError as exc:
        raise SourceUnavailableError(
            f"reading a manifest file failed: {exc.strerror or 'read failed'}"
        ) from None
    finally:
        os.close(handle)
    if total != descriptor.size:
        raise SourceUnavailableError("a manifest file is shorter than declared")
    report = FillReport()
    report.files_filled = 1
    report.bytes_filled = total
    report.tensors_filled = len(path_tensors)
    report.header_and_gap_bytes = _uncovered_bytes(
        ((t.data_begin, t.data_end) for t in path_tensors), descriptor.size
    )
    return report


def _uncovered_bytes(covered, file_size: int) -> int:
    """Bytes of the file no tensor descriptor covers: header plus gaps."""
    covered_total = sum(end - begin for begin, end in covered)
    return file_size - covered_total


__all__ = [
    "FILL_CHUNK_BYTES",
    "MAX_FILE_BYTES",
    "MAX_REGISTRATIONS",
    "MAX_TOTAL_BYTES",
    "FillReport",
    "check_admission",
    "fill_from_tree",
    "resolve_tree_file",
]
