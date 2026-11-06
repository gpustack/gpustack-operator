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
