import json
import tracemalloc

import pytest

from gpustack_model_transfer.adapters.engine_reader import TensorDescriptor


def descriptors(count):
    for number in range(count):
        yield TensorDescriptor(f"layer.{number:08}.weight", (2, 3), "F16")


def test_engine_descriptor_storage_has_bounded_python_memory():
    from gpustack_model_transfer.adapters.descriptor_pages import checked_descriptors

    tracemalloc.start()
    try:
        index = checked_descriptors(descriptors(100_000))
        _, peak = tracemalloc.get_traced_memory()
        assert peak < 12 << 20
        assert len(index) == 100_000
        assert index[-1].name == "layer.00099999.weight"
        assert list(index[:2]) == list(descriptors(2))
    finally:
        tracemalloc.stop()
        if "index" in locals():
            index.close()


def test_tp_metadata_broadcast_uses_bounded_pages():
    from gpustack_model_transfer.adapters.descriptor_pages import (
        MAX_DESCRIPTOR_PAGE_BYTES,
        broadcast_descriptors,
        checked_descriptors,
    )

    messages = []
    index = checked_descriptors(descriptors(10_000))

    def send(message):
        messages.append(list(message))

    try:
        assert broadcast_descriptors(index, send, coordinator=True) is index
        pages = [m[1] for m in messages if isinstance(m[1], str)]
        assert len(pages) > 1
        assert all(len(page.encode()) <= MAX_DESCRIPTOR_PAGE_BYTES for page in pages)
        assert sum(len(json.loads(page)) for page in pages) == len(index)

        def receive(message):
            message[:] = messages.pop(0)

        received = broadcast_descriptors(None, receive, coordinator=False)
        try:
            assert received == index
            assert not messages
        finally:
            received.close()
    finally:
        index.close()


def test_duplicate_descriptors_across_pages_are_refused():
    from gpustack_model_transfer.adapters.descriptor_pages import checked_descriptors

    def repeated():
        yield from descriptors(5000)
        yield next(descriptors(1))

    with pytest.raises(ValueError, match="duplicate"):
        checked_descriptors(repeated())


@pytest.mark.parametrize("failure", ["duplicate", "invalid", "generator"])
def test_descriptor_batch_failure_rolls_back_rows_and_positions(failure):
    from gpustack_model_transfer.adapters.descriptor_pages import DescriptorIndex

    index = DescriptorIndex()
    index.add([("first", (1,), "U8")])

    def batch():
        yield ("partial", (1,), "U8")
        if failure == "generator":
            raise RuntimeError("producer failed")
        yield ("first" if failure == "duplicate" else "", (1,), "U8")

    try:
        with pytest.raises((ValueError, RuntimeError)):
            index.add(batch())
        assert len(index) == 1
        assert [row.name for row in index] == ["first"]
        index.add([("partial", (1,), "U8")])
        assert [row.name for row in index] == ["first", "partial"]
    finally:
        index.close()


@pytest.mark.parametrize("table_kind", ["descriptors", "tensors"])
def test_duplicate_first_batch_recovers_without_sqlite_errorname(table_kind):
    import sqlite3

    from gpustack_model_transfer.adapters.descriptor_pages import DescriptorIndex
    from gpustack_model_transfer.core.descriptors import (
        TensorDescriptor as StoredTensor,
    )
    from gpustack_model_transfer.core.errors import InvalidDescriptorError
    from gpustack_model_transfer.core.tensor_index import TensorIndex

    table = DescriptorIndex() if table_kind == "descriptors" else TensorIndex()
    connection = table._connection

    class LegacyConnection:
        def execute(self, *args):
            try:
                return connection.execute(*args)
            except sqlite3.IntegrityError as exc:
                raise sqlite3.IntegrityError(str(exc)) from None

        def __getattr__(self, name):
            return getattr(connection, name)

    table._connection = LegacyConnection()
    row = (
        ("first", (4,), "U8")
        if table_kind == "descriptors"
        else StoredTensor("first", "weights", "U8", (4,), 0, 4)
    )
    error = ValueError if table_kind == "descriptors" else InvalidDescriptorError
    try:
        assert not connection.in_transaction
        with pytest.raises(error, match="duplicate"):
            table.add([row, row])
        assert len(table) == 0
        table.add([row])
        assert len(table) == 1
    finally:
        table.close()
