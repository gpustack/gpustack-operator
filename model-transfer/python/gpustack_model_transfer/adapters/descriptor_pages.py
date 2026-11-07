"""Bounded engine metadata storage and TP collective delivery."""

import itertools
import json
import sqlite3
import tempfile
import weakref
from collections.abc import Sequence

from gpustack_model_transfer.adapters.engine_reader import (
    DescriptorSequence,
    TensorDescriptor,
)
from gpustack_model_transfer.core.descriptors import shape_element_count
from gpustack_model_transfer.core.manifest import MAX_MANIFEST_TENSORS
from gpustack_model_transfer.core.tensor_index import TensorIndex

MAX_DESCRIPTOR_PAGE_BYTES = 4 << 20
MAX_DESCRIPTOR_PAGE_COUNT = 4096


def _cleanup(connection, directory):
    connection.close()
    directory.cleanup()


class DescriptorIndex(Sequence):
    """Replayable engine descriptors with a fixed SQLite cache budget."""

    def __init__(self):
        directory = tempfile.TemporaryDirectory(prefix="gpustack-engine-descriptors-")
        try:
            self._connection = sqlite3.connect(directory.name + "/descriptors.db")
        except BaseException:
            directory.cleanup()
            raise
        self._cleanup = weakref.finalize(self, _cleanup, self._connection, directory)
        self._connection.execute("PRAGMA cache_size=-4096")
        self._connection.execute("PRAGMA temp_store=FILE")
        self._connection.execute("PRAGMA max_page_count=524288")
        self._connection.execute(
            "CREATE TABLE descriptors (position INTEGER PRIMARY KEY, name TEXT UNIQUE, shape TEXT, dtype TEXT)"
        )
        self._count = 0

    def add(self, rows):
        count = self._count
        try:
            for name, shape, dtype in rows:
                if self._count >= MAX_MANIFEST_TENSORS:
                    raise ValueError("engine descriptor count exceeds bound")
                if not isinstance(name, str) or not name or "\x00" in name:
                    raise ValueError("invalid engine tensor name")
                if not isinstance(dtype, str) or not dtype or "\x00" in dtype:
                    raise ValueError("invalid engine tensor dtype")
                shape_element_count(shape)
                self._connection.execute(
                    "INSERT INTO descriptors VALUES (?,?,?,?)",
                    (self._count, name, json.dumps(shape), dtype),
                )
                self._count += 1
            self._connection.commit()
        except BaseException as exc:
            self._connection.rollback()
            self._count = count
            if isinstance(exc, sqlite3.IntegrityError) and (
                getattr(exc, "sqlite_errorname", "") == "SQLITE_CONSTRAINT_UNIQUE"
                or str(exc) == "UNIQUE constraint failed: descriptors.name"
            ):
                raise ValueError("duplicate checkpoint tensor names") from exc
            raise

    def close(self):
        self._cleanup()

    def __len__(self):
        return self._count

    def __iter__(self):
        cursor = self._connection.execute(
            "SELECT name,shape,dtype FROM descriptors ORDER BY position"
        )
        try:
            for name, shape, dtype in cursor:
                yield TensorDescriptor(name, tuple(json.loads(shape)), dtype)
        finally:
            cursor.close()

    def __getitem__(self, index):
        if isinstance(index, slice):
            return _Slice(self, range(len(self))[index])
        if index < 0:
            index += len(self)
        if not 0 <= index < len(self):
            raise IndexError(index)
        name, shape, dtype = self._connection.execute(
            "SELECT name,shape,dtype FROM descriptors WHERE position=?", (index,)
        ).fetchone()
        return TensorDescriptor(name, tuple(json.loads(shape)), dtype)

    def __eq__(self, other):
        if not isinstance(other, Sequence):
            return NotImplemented
        return len(self) == len(other) and all(
            a == b for a, b in zip(self, other, strict=True)
        )


class _Slice(Sequence):
    def __init__(self, parent, indices):
        self.parent, self.indices = parent, indices

    def __len__(self):
        return len(self.indices)

    def __getitem__(self, index):
        if isinstance(index, slice):
            return _Slice(self.parent, self.indices[index])
        return self.parent[self.indices[index]]


def checked_descriptors(descriptors):
    """Keep verified disk indices; validate other providers in bounded batches."""
    if isinstance(descriptors, DescriptorIndex) or (
        isinstance(descriptors, DescriptorSequence)
        and isinstance(descriptors.tensors, TensorIndex)
    ):
        return descriptors
    index = DescriptorIndex()
    try:
        iterator = iter(descriptors)
        while batch := tuple(itertools.islice(iterator, MAX_DESCRIPTOR_PAGE_COUNT)):
            index.add((d.name, d.shape, d.dtype) for d in batch)
        return index
    except BaseException:
        index.close()
        raise


def _pages(descriptors):
    rows, size = [], 2
    for descriptor in descriptors:
        raw = json.dumps(
            [descriptor.name, descriptor.shape, descriptor.dtype], separators=(",", ":")
        )
        length = len(raw.encode())
        if length + 2 > MAX_DESCRIPTOR_PAGE_BYTES:
            raise ValueError("engine descriptor exceeds collective page bound")
        if rows and (
            len(rows) >= MAX_DESCRIPTOR_PAGE_COUNT
            or size + length + 1 > MAX_DESCRIPTOR_PAGE_BYTES
        ):
            yield "[" + ",".join(rows) + "]"
            rows, size = [], 2
        rows.append(raw)
        size += length + 1
    if rows:
        yield "[" + ",".join(rows) + "]"


def broadcast_descriptors(descriptors, broadcast, *, coordinator):
    """Send bounded JSON pages; each follower builds a private disk index.

    The caller broadcasts a failure instead of the first header when probing
    or planning fails. Later iteration failures occupy the next page slot.
    A follower validation error aborts its rank; peer collectives rely on
    the engine's process-group timeout and worker teardown. A follower must
    not send a source-rank broadcast as an error signal.
    """
    header = [True, len(descriptors)] if coordinator else [None, None]
    broadcast(header)
    if header[0] is not True:
        raise RuntimeError(header[1])
    count = header[1]
    if (
        isinstance(count, bool)
        or not isinstance(count, int)
        or not 0 <= count <= MAX_MANIFEST_TENSORS
    ):
        raise ValueError("invalid collective descriptor count")
    if coordinator:
        try:
            for page in _pages(descriptors):
                broadcast([True, page])
        except BaseException as exc:
            broadcast([False, str(exc)])
            raise
        broadcast([True, None])
        return descriptors
    index = DescriptorIndex()
    try:
        while True:
            message = [None, None]
            broadcast(message)
            if message[0] is not True:
                raise RuntimeError(message[1])
            raw = message[1]
            if raw is None:
                break
            if (
                not isinstance(raw, str)
                or len(raw.encode()) > MAX_DESCRIPTOR_PAGE_BYTES
            ):
                raise ValueError("collective descriptor page exceeds bound")
            rows = json.loads(raw)
            if (
                not isinstance(rows, list)
                or not 1 <= len(rows) <= MAX_DESCRIPTOR_PAGE_COUNT
            ):
                raise ValueError("invalid collective descriptor page count")
            if any(not isinstance(row, list) or len(row) != 3 for row in rows):
                raise ValueError("invalid collective descriptor row")
            index.add(rows)
            if len(index) > count:
                raise ValueError("collective descriptor count mismatch")
        if len(index) != count:
            raise ValueError("collective descriptor count mismatch")
        return index
    except BaseException:
        index.close()
        raise
