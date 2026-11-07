"""A bounded-memory tensor index backed by a private temporary SQLite file."""

import json
import sqlite3
import tempfile
import threading
import weakref
from collections.abc import Mapping, Sequence

from gpustack_model_transfer.core.descriptors import TensorDescriptor
from gpustack_model_transfer.core.errors import InvalidDescriptorError


def _cleanup(connection, directory):
    connection.close()
    directory.cleanup()


def _descriptor(row):
    return TensorDescriptor(
        name=row[0],
        file=row[1],
        dtype=row[2],
        shape=tuple(json.loads(row[3])),
        data_begin=row[4],
        data_end=row[5],
    )


class TensorIndex(Sequence):
    """Verified descriptors on disk, with a 4 MiB SQLite page-cache budget.

    Build before sharing with readers. Iteration and slices remain lazy;
    callers must not convert the complete sequence into a tuple or list.
    """

    def __init__(self):
        directory = tempfile.TemporaryDirectory(prefix="gpustack-tensors-")
        try:
            connection = sqlite3.connect(
                directory.name + "/tensors.db", check_same_thread=False
            )
        except BaseException:
            directory.cleanup()
            raise
        self._connection = connection
        self._cleanup = weakref.finalize(self, _cleanup, connection, directory)
        self._lock = threading.RLock()
        self._counts = {}
        self._count = 0
        connection.execute("PRAGMA cache_size=-4096")
        connection.execute("PRAGMA temp_store=FILE")
        connection.execute("PRAGMA max_page_count=524288")
        connection.execute("""CREATE TABLE tensors (
            name TEXT PRIMARY KEY, file TEXT NOT NULL, dtype TEXT NOT NULL,
            shape TEXT NOT NULL, begin INTEGER NOT NULL, end INTEGER NOT NULL,
            position INTEGER NOT NULL, UNIQUE(file, position))""")
        self.by_name = _Names(self)
        self.by_file = _Files(self)
        self.file_index = _Positions(self)
        self.layouts = _Layouts(self)

    def add(self, tensors):
        """Append a bounded batch in per-file offset order; reject duplicate names."""
        with self._lock:
            counts, count = self._counts.copy(), self._count
            try:
                for tensor in tensors:
                    position = self._counts.get(tensor.file, 0)
                    self._connection.execute(
                        "INSERT INTO tensors VALUES (?,?,?,?,?,?,?)",
                        (
                            tensor.name,
                            tensor.file,
                            tensor.dtype,
                            json.dumps(tensor.shape),
                            tensor.data_begin,
                            tensor.data_end,
                            position,
                        ),
                    )
                    self._counts[tensor.file] = position + 1
                    self._count += 1
                self._connection.commit()
            except BaseException as exc:
                self._connection.rollback()
                self._counts, self._count = counts, count
                if isinstance(exc, sqlite3.IntegrityError) and getattr(
                    exc, "sqlite_errorname", ""
                ) in ("SQLITE_CONSTRAINT_PRIMARYKEY", "SQLITE_CONSTRAINT_UNIQUE"):
                    raise InvalidDescriptorError(
                        "duplicate tensor name in manifest"
                    ) from exc
                if isinstance(exc, sqlite3.DatabaseError):
                    raise InvalidDescriptorError(
                        "tensor index storage limit or IO failure"
                    ) from exc
                raise

    def close(self):
        self._cleanup()

    def __len__(self):
        return self._count

    def __iter__(self):
        return self._rows("ORDER BY file, position", ())

    def __getitem__(self, index):
        return _TensorSlice(self, None, 0, len(self))[index]

    def _rows(self, suffix, arguments):
        with self._lock:
            cursor = self._connection.execute(
                "SELECT name,file,dtype,shape,begin,end FROM tensors " + suffix,
                arguments,
            )
        try:
            while True:
                with self._lock:
                    row = cursor.fetchone()
                if row is None:
                    return
                yield _descriptor(row)
        finally:
            with self._lock:
                cursor.close()


class _TensorSlice(Sequence):
    def __init__(self, table, file, start, stop):
        self.table, self.file, self.start, self.stop = table, file, start, stop

    def __len__(self):
        return max(0, self.stop - self.start)

    def __iter__(self):
        if self.file is None:
            return self.table._rows(
                "ORDER BY file,position LIMIT ? OFFSET ?", (len(self), self.start)
            )
        return self.table._rows(
            "WHERE file=? AND position>=? AND position<? ORDER BY position",
            (self.file, self.start, self.stop),
        )

    def __getitem__(self, index):
        if isinstance(index, slice):
            start, stop, step = index.indices(len(self))
            if step != 1:
                raise ValueError("tensor slices require a unit step")
            return _TensorSlice(
                self.table, self.file, self.start + start, self.start + stop
            )
        if index < 0:
            index += len(self)
        if not 0 <= index < len(self):
            raise IndexError(index)
        return next(
            iter(
                _TensorSlice(
                    self.table, self.file, self.start + index, self.start + index + 1
                )
            )
        )


class _Names(Mapping):
    def __init__(self, table):
        self.table = table

    def __len__(self):
        return len(self.table)

    def __iter__(self):
        return (tensor.name for tensor in self.table)

    def __getitem__(self, name):
        result = next(self.table._rows("WHERE name=?", (name,)), None)
        if result is None:
            raise KeyError(name)
        return result

    def items(self):
        return ((tensor.name, tensor) for tensor in self.table)


class _Layouts(Mapping):
    def __init__(self, table):
        self.table = table

    def __len__(self):
        return len(self.table)

    def __iter__(self):
        return iter(self.table.by_name)

    def __getitem__(self, name):
        return self._layout(self.table.by_name[name])

    def items(self):
        return ((tensor.name, self._layout(tensor)) for tensor in self.table)

    @staticmethod
    def _layout(tensor):
        return (
            tensor.file,
            tensor.dtype,
            tensor.shape,
            tensor.data_begin,
            tensor.data_end,
        )


class _Files(Mapping):
    def __init__(self, table):
        self.table = table

    def __len__(self):
        return len(self.table._counts)

    def __iter__(self):
        return iter(self.table._counts)

    def __getitem__(self, file):
        return _TensorSlice(self.table, file, 0, self.table._counts[file])


class _Positions(Mapping):
    def __init__(self, table):
        self.table = table

    def __len__(self):
        return len(self.table)

    def __iter__(self):
        return ((tensor.file, tensor.name) for tensor in self.table)

    def __getitem__(self, key):
        file, name = key
        with self.table._lock:
            row = self.table._connection.execute(
                "SELECT position FROM tensors WHERE name=? AND file=?", (name, file)
            ).fetchone()
        if row is None:
            raise KeyError(key)
        return row[0]


def tensor_tables(manifest):
    """Return name, file and per-file position lookups without copying disk tables."""
    tensors = manifest.tensors
    if isinstance(tensors, TensorIndex):
        return tensors.by_name, tensors.by_file, tensors.file_index
    by_name = {tensor.name: tensor for tensor in tensors}
    by_file = {}
    for tensor in tensors:
        by_file.setdefault(tensor.file, []).append(tensor)
    for items in by_file.values():
        items.sort(key=lambda item: (item.data_begin, item.data_end, item.name))
    positions = {
        (file, tensor.name): index
        for file, items in by_file.items()
        for index, tensor in enumerate(items)
    }
    return by_name, by_file, positions
