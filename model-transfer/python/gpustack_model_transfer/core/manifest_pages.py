"""Authenticate bounded tensor pages and index them without retaining all layouts."""

import hashlib
import json
import os
import re
import stat
from pathlib import Path

from gpustack_model_transfer.core.descriptors import TensorDescriptor
from gpustack_model_transfer.core.errors import (
    AuthenticationError,
    InvalidDescriptorError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.tensor_index import TensorIndex

MAX_PAGE_BYTES = 4 << 20
MAX_PAGE_TENSORS = 4096
MAX_PAGES = 16384
MAX_TOTAL_PAGE_BYTES = 512 << 20
_PAGE_KEYS = {"sha256", "size", "count", "file", "begin", "end"}


def local_page_reader(directory):
    def read(page):
        path = Path(directory) / (page["sha256"] + ".json")
        try:
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        except OSError as exc:
            raise SourceUnavailableError("cannot open a manifest page") from exc
        try:
            handle = os.fdopen(fd, "rb")
        except BaseException:
            os.close(fd)
            raise
        with handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size != page["size"]:
                raise InvalidDescriptorError(
                    "manifest page is not the declared regular file"
                )
            return handle.read(page["size"] + 1)

    return read


def read_paged_tensors(pages, files, reader):
    from gpustack_model_transfer.core.manifest import (
        _TENSOR_KEYS,
        MAX_MANIFEST_TENSORS,
        _reject_duplicate_keys,
        _require_exact_keys,
    )

    if (
        not isinstance(pages, list)
        or not pages
        or len(pages) > MAX_PAGES
        or reader is None
    ):
        raise InvalidDescriptorError("invalid page references or missing page reader")
    sizes = {file.path: file.size for file in files}
    ends, digests = {}, set()
    count, total = 0, 0
    for page in pages:
        _require_exact_keys(page, _PAGE_KEYS, "tensor page reference")
        if (
            any(
                isinstance(page[key], bool) or not isinstance(page[key], int)
                for key in ("size", "count", "begin", "end")
            )
            or not isinstance(page["sha256"], str)
            or not re.fullmatch(r"[0-9a-f]{64}", page["sha256"])
            or not isinstance(page["file"], str)
            or page["file"] not in sizes
            or not 2 <= page["size"] <= MAX_PAGE_BYTES
            or not 1 <= page["count"] <= MAX_PAGE_TENSORS
            or not ends.get(page["file"], 0)
            <= page["begin"]
            <= page["end"]
            <= sizes[page["file"]]
            or page["sha256"] in digests
        ):
            raise InvalidDescriptorError("invalid page identity, bounds or order")
        ends[page["file"]] = page["end"]
        digests.add(page["sha256"])
        count += page["count"]
        total += page["size"]
        if count > MAX_MANIFEST_TENSORS or total > MAX_TOTAL_PAGE_BYTES:
            raise InvalidDescriptorError("page count or total bytes exceed bound")
    table = TensorIndex()
    try:
        for page in pages:
            raw = reader(page)
            if (
                not isinstance(raw, bytes)
                or len(raw) != page["size"]
                or hashlib.sha256(raw).hexdigest() != page["sha256"]
            ):
                raise AuthenticationError("manifest page size or digest mismatch")
            try:
                entries = json.loads(raw, object_pairs_hook=_reject_duplicate_keys)
            except (ValueError, UnicodeError):
                raise InvalidDescriptorError("invalid tensor page JSON") from None
            if not isinstance(entries, list) or len(entries) != page["count"]:
                raise InvalidDescriptorError("page count mismatch")
            tensors = []
            end = page["begin"]
            for number, entry in enumerate(entries):
                _require_exact_keys(entry, _TENSOR_KEYS, "tensor entry")
                if not isinstance(entry["shape"], list):
                    raise InvalidDescriptorError("tensor shape must be a list")
                tensor = TensorDescriptor(
                    name=entry["name"],
                    file=entry["file"],
                    dtype=entry["dtype"],
                    shape=tuple(entry["shape"]),
                    data_begin=entry["data_begin"],
                    data_end=entry["data_end"],
                )
                if (
                    tensor.file != page["file"]
                    or tensor.data_begin < end
                    or (number == 0 and tensor.data_begin != page["begin"])
                ):
                    raise InvalidDescriptorError(
                        "page file, order or beginning mismatch"
                    )
                tensor.validate(sizes[page["file"]])
                end = tensor.data_end
                tensors.append(tensor)
            if end != page["end"]:
                raise InvalidDescriptorError("page end mismatch")
            table.add(tensors)
        return table
    except BaseException:
        table.close()
        raise
