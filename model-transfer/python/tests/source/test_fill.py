"""Streaming source verification, cancellation, and bounded shard workers."""

import errno
import os
import threading
from dataclasses import replace

import pytest

from gpustack_model_transfer.core.descriptors import FileDescriptor, TensorDescriptor
from gpustack_model_transfer.core.errors import (
    AuthenticationError,
    InvalidDescriptorError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.manifest import Manifest
from gpustack_model_transfer.source import fill


class MemoryBlock:
    """Owned CPU storage with the same writable view contract as HostBlock."""

    def __init__(self, size):
        self.byte_size = size
        self.data = bytearray(size)
        self.views = []

    def view(self, offset, length):
        self.views.append((offset, length))
        return memoryview(self.data)[offset : offset + length]

    def read_bytes(self, offset, length):
        raise AssertionError("verification copied pinned memory")


def make_tree(tmp_path, count=1, size=80):
    manifest = Manifest(2, "artifact-uid-1", 3, (), (), "0" * 64)
    files, tensors, blocks, payloads = [], [], {}, {}
    ranges = [(47, size), (16, 16), (8, 16), (16, 33), (40, 40), (0, 0), (size, size)]
    for index in range(count):
        path = f"{index}.safetensors"
        payload = bytes((i + index) % 256 for i in range(size))
        (tmp_path / path).write_bytes(payload)
        files.append(FileDescriptor(path, size))
        payloads[path] = payload
        blocks[path] = MemoryBlock(size)
        for ordinal, (begin, end) in enumerate(ranges):
            name = f"file{index}.tensor{ordinal}"
            tensors.append(
                TensorDescriptor(
                    name,
                    path,
                    "U8",
                    (end - begin,),
                    begin,
                    end,
                )
            )
    return (
        replace(manifest, files=tuple(files), tensors=tuple(tensors)),
        blocks,
        payloads,
    )


def test_fill_copies_published_payload_without_rehashing(tmp_path):
    manifest, blocks, payloads = make_tree(tmp_path)
    path = "0.safetensors"
    changed = bytearray(payloads[path])
    changed[-1] ^= 255
    (tmp_path / path).write_bytes(changed)
    fill.fill_from_tree(manifest, str(tmp_path), blocks)
    assert blocks[path].data == changed


@pytest.mark.parametrize("chunk_bytes", [1, 7, 16, fill.FILL_CHUNK_BYTES])
@pytest.mark.parametrize("short_reads", [False, True])
def test_fill_verifies_each_landed_byte_without_copying(
    tmp_path, monkeypatch, chunk_bytes, short_reads
):
    manifest, blocks, payloads = make_tree(tmp_path)
    readv = os.readv
    read_sizes = []

    def read_limited(handle, views):
        assert len(views) == 1
        assert isinstance(views[0], memoryview)
        assert views[0].obj is blocks["0.safetensors"].data
        count = readv(handle, [views[0][:3]] if short_reads else views)
        read_sizes.append(count)
        return count

    monkeypatch.setattr(fill.os, "readv", read_limited)
    report = fill.fill_from_tree(manifest, str(tmp_path), blocks, chunk_bytes)
    coverage = sum(t.data_end - t.data_begin for t in manifest.tensors)
    assert report.as_dict() == {
        "filesFilled": 1,
        "bytesFilled": 80,
        "tensorsFilled": 7,
        "headerAndGapBytes": 80 - coverage,
    }
    assert blocks["0.safetensors"].data == payloads["0.safetensors"]
    assert sum(read_sizes) == 80
    assert len(blocks["0.safetensors"].views) == len(read_sizes)
    position = 0
    for (offset, length), count in zip(
        blocks["0.safetensors"].views, read_sizes, strict=True
    ):
        assert offset == position
        assert length <= chunk_bytes
        position += count


@pytest.mark.parametrize(
    "chunk_bytes", [True, 0, -1, fill.FILL_CHUNK_BYTES + 1, "8", None]
)
def test_invalid_chunk_size_is_refused(tmp_path, chunk_bytes):
    manifest, blocks, _ = make_tree(tmp_path)
    with pytest.raises(InvalidDescriptorError, match="chunk size"):
        fill.fill_from_tree(manifest, str(tmp_path), blocks, chunk_bytes)


@pytest.mark.parametrize("block_size", [None, 79, 81])
def test_missing_or_wrong_block_is_refused_before_open(tmp_path, block_size):
    manifest, _, _ = make_tree(tmp_path)
    (tmp_path / "0.safetensors").unlink()
    blocks = {} if block_size is None else {"0.safetensors": MemoryBlock(block_size)}
    with pytest.raises(InvalidDescriptorError, match="no pinned block"):
        fill.fill_from_tree(manifest, str(tmp_path), blocks)


@pytest.mark.parametrize(
    "change,error,message",
    [
        ("missing", SourceUnavailableError, "cannot safely open"),
        ("short", AuthenticationError, "file extent"),
        ("long", AuthenticationError, "file extent"),
        ("symlink", SourceUnavailableError, "cannot safely open"),
    ],
)
def test_tree_refusals_keep_their_error_types(tmp_path, change, error, message):
    manifest, blocks, payloads = make_tree(tmp_path)
    path = tmp_path / "0.safetensors"
    data = payloads[path.name]
    path.unlink()
    if change == "symlink":
        target = tmp_path / "outside"
        target.write_bytes(data)
        path.symlink_to(target)
    elif change != "missing":
        content = {"short": data[:-1], "long": data + b"x", "corrupt": b"x" + data[1:]}
        path.write_bytes(content[change])
    with pytest.raises(error, match=message):
        fill.fill_from_tree(manifest, str(tmp_path), blocks, 7)


@pytest.mark.parametrize(
    "change,error,message",
    [
        ("read error", SourceUnavailableError, "reading a manifest file failed"),
        ("truncate", SourceUnavailableError, "shorter than declared"),
        ("grow", AuthenticationError, "exceeds manifest extent"),
        ("abort", fill.FillAborted, "fill aborted"),
    ],
)
def test_midstream_failure_closes_file_and_retains_block(
    tmp_path, monkeypatch, change, error, message
):
    manifest, blocks, payloads = make_tree(tmp_path)
    readv = os.readv
    handles = []
    abort = threading.Event()

    def changed_read(handle, views):
        handles.append(handle)
        if change == "read error":
            raise OSError(errno.EIO, "injected read failure")
        count = readv(handle, views)
        if len(handles) == 1:
            path = tmp_path / "0.safetensors"
            if change == "truncate":
                os.truncate(path, count)
            elif change == "grow":
                path.write_bytes(payloads[path.name] + b"extra")
            elif change == "abort":
                abort.set()
        return count

    monkeypatch.setattr(fill.os, "readv", changed_read)
    with pytest.raises(error, match=message):
        fill.fill_from_tree(manifest, str(tmp_path), blocks, 7, abort.is_set)
    assert handles
    for handle in set(handles):
        with pytest.raises(OSError):
            os.fstat(handle)
    assert len(blocks["0.safetensors"].data) == 80
    if change == "abort":
        assert len(handles) == 1


def test_abort_before_open_preserves_cancellation_precedence(tmp_path):
    manifest, _, _ = make_tree(tmp_path)
    with pytest.raises(fill.FillAborted):
        fill.fill_from_tree(manifest, str(tmp_path), {}, should_abort=lambda: True)


@pytest.mark.parametrize("cpu_count,workers", [(1, 1), (2, 2), (32, 8), (None, 1)])
def test_multifile_fill_bounds_concurrency_and_combines_reports(
    tmp_path, monkeypatch, cpu_count, workers
):
    manifest, blocks, payloads = make_tree(tmp_path, count=10)
    monkeypatch.setattr(fill.os, "cpu_count", lambda: cpu_count)
    original = fill._fill_file
    barrier = threading.Barrier(workers)
    lock = threading.Lock()
    active, maximum, started = 0, 0, 0

    def concurrent_fill(*args):
        nonlocal active, maximum, started
        with lock:
            active += 1
            maximum = max(maximum, active)
            started += 1
            initial = started <= workers
        try:
            if initial:
                barrier.wait(timeout=5)
            return original(*args)
        finally:
            with lock:
                active -= 1

    monkeypatch.setattr(fill, "_fill_file", concurrent_fill)
    report = fill.fill_from_tree(manifest, str(tmp_path), blocks, 7)
    assert maximum == workers
    assert active == 0
    assert report.as_dict() == {
        "filesFilled": 10,
        "bytesFilled": 800,
        "tensorsFilled": 70,
        "headerAndGapBytes": 220,
    }
    for path, block in blocks.items():
        assert block.data == payloads[path]


@pytest.mark.parametrize(
    "error_type",
    [
        fill.FillAborted,
        AuthenticationError,
        InvalidDescriptorError,
        SourceUnavailableError,
    ],
)
def test_parallel_failure_stops_and_joins_other_writers(
    tmp_path, monkeypatch, error_type
):
    manifest, blocks, _ = make_tree(tmp_path, count=6)
    monkeypatch.setattr(fill.os, "cpu_count", lambda: 2)
    barrier = threading.Barrier(2)
    failed = threading.Event()
    stopped = threading.Event()
    original_fail = fill._FillCancellation.fail

    def record_failure(self, index):
        original_fail(self, index)
        if index == 0:
            failed.set()

    def controlled_fill(descriptor, tensors, tree, block, chunk, check_abort):
        check_abort()
        if descriptor.path == "0.safetensors":
            barrier.wait(timeout=5)
            raise error_type("first failure")
        if descriptor.path == "1.safetensors":
            barrier.wait(timeout=5)
            assert failed.wait(timeout=5)
            try:
                check_abort()
            finally:
                stopped.set()
        pytest.fail("a later writer ran after cancellation")

    monkeypatch.setattr(fill._FillCancellation, "fail", record_failure)
    monkeypatch.setattr(fill, "_fill_file", controlled_fill)
    with pytest.raises(error_type, match="first failure"):
        fill.fill_from_tree(manifest, str(tmp_path), blocks)
    assert stopped.is_set()


def test_parallel_refusals_follow_descriptor_order(tmp_path, monkeypatch):
    manifest, blocks, _ = make_tree(tmp_path, count=3)
    monkeypatch.setattr(fill.os, "cpu_count", lambda: 3)
    barrier = threading.Barrier(3)
    later_failed, earlier_failed, stopped = (
        threading.Event(),
        threading.Event(),
        threading.Event(),
    )
    original_fail = fill._FillCancellation.fail

    def record_failure(self, index):
        original_fail(self, index)
        if index == 1:
            later_failed.set()
        if index == 0:
            earlier_failed.set()

    def controlled_fill(descriptor, tensors, tree, block, chunk, check_abort):
        check_abort()
        barrier.wait(timeout=5)
        if descriptor.path == "1.safetensors":
            raise InvalidDescriptorError("later descriptor")
        if descriptor.path == "0.safetensors":
            assert later_failed.wait(timeout=5)
            check_abort()
            raise AuthenticationError("earlier descriptor")
        assert earlier_failed.wait(timeout=5)
        try:
            check_abort()
        finally:
            stopped.set()
        pytest.fail("later writer was not cancelled")

    monkeypatch.setattr(fill._FillCancellation, "fail", record_failure)
    monkeypatch.setattr(fill, "_fill_file", controlled_fill)
    with pytest.raises(AuthenticationError, match="earlier descriptor"):
        fill.fill_from_tree(manifest, str(tmp_path), blocks)
    assert stopped.is_set()


def test_no_registered_files_returns_empty_report(tmp_path):
    manifest = Manifest(2, "artifact-uid-1", 3, (), (), "0" * 64)
    assert fill.fill_from_tree(manifest, str(tmp_path), {}).as_dict() == {
        "filesFilled": 0,
        "bytesFilled": 0,
        "tensorsFilled": 0,
        "headerAndGapBytes": 0,
    }


@pytest.mark.parametrize("begin,end", [(-1, 4), (8, 81), (9, 8), (0, -1)])
def test_invalid_ranges_refused_before_copy(tmp_path, begin, end):
    manifest, blocks, _ = make_tree(tmp_path)
    tensor = replace(manifest.tensors[0], data_begin=begin, data_end=end)
    manifest = replace(manifest, tensors=(tensor,))
    with pytest.raises(InvalidDescriptorError):
        fill.fill_from_tree(manifest, str(tmp_path), blocks, 7)
    assert blocks["0.safetensors"].data == bytes(80)
