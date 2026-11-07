"""Verify the mounted metadata-only view before any native allocation."""

import hashlib
import itertools
import json
import os
import re
import stat

from gpustack_model_transfer.core.descriptors import TensorDescriptor
from gpustack_model_transfer.core.errors import (
    AuthenticationError,
    InvalidDescriptorError,
)
from gpustack_model_transfer.core.tensor_index import TensorIndex
from gpustack_model_transfer.source.fill import resolve_tree_file

MAX_VIEW = 64 << 20
MAX_HEADER = 64 << 20
MAX_TOTAL = 256 << 20
MAX_FILES = 1100
_ALLOWED = {
    "manifest.json",
    "config.json",
    "quantize_config.json",
    "quantization_config.json",
    "quant_model_description.json",
    "tokenizer.json",
    "tokenizer_config.json",
    "generation_config.json",
    "special_tokens_map.json",
    "model.safetensors.index.json",
    "tokenizer.model",
    "vocab.json",
    "merges.txt",
    "added_tokens.json",
    "chat_template.jinja",
    "preprocessor_config.json",
    "processor_config.json",
}


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidDescriptorError("duplicate metadata key")
        result[key] = value
    return result


def _json(raw):
    try:
        return json.loads(raw, object_pairs_hook=_object)
    except (ValueError, UnicodeError):
        raise InvalidDescriptorError("invalid metadata JSON") from None


def _read(root, name, limit):
    fd = resolve_tree_file(root, name)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise InvalidDescriptorError("metadata exceeds bound")
        chunks = []
        remaining = info.st_size
        while remaining:
            chunk = os.read(fd, min(1 << 20, remaining))
            if not chunk:
                raise InvalidDescriptorError("metadata was truncated")
            chunks.append(chunk)
            remaining -= len(chunk)
        if os.read(fd, 1):
            raise InvalidDescriptorError("metadata changed during read")
        return b"".join(chunks)
    finally:
        os.close(fd)


def _valid_listing(value, canonical_digest):
    """Authenticate the stored listing's canonical content address."""
    if not isinstance(value, dict) or set(value) != {"digest", "entries"}:
        return False
    entries = value["entries"]
    if (
        value["digest"] != canonical_digest
        or not isinstance(entries, list)
        or len(entries) > 65536
    ):
        raise AuthenticationError("artifact listing identity mismatch")
    parsed = []
    total_size = 0
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {"path", "size", "digest"}:
            raise InvalidDescriptorError("invalid artifact listing")
        name, size, digest = entry["path"], entry["size"], entry["digest"]
        if (
            not isinstance(name, str)
            or not name
            or any(part in ("", ".", "..") for part in name.split("/"))
            or any(ord(ch) < 32 or ord(ch) == 127 for ch in name)
            or isinstance(size, bool)
            or not isinstance(size, int)
            or size < 0
            or size > (1 << 63) - 1
            or not isinstance(digest, str)
            or not (
                re.fullmatch(r"sha256:[0-9a-f]{64}", digest)
                or re.fullmatch(r"gitsha1:[0-9a-f]{40}", digest)
            )
        ):
            raise InvalidDescriptorError("invalid artifact listing entry")
        total_size += size
        if total_size > (1 << 63) - 1:
            raise InvalidDescriptorError("artifact listing size overflow")
        parsed.append((name, size, digest))
    parsed.sort(key=lambda item: item[0].encode("utf-8"))
    if any(left[0] == right[0] for left, right in itertools.pairwise(parsed)):
        raise InvalidDescriptorError("duplicate artifact listing path")
    digest = hashlib.sha256(b"gpustack-manifest v1\n")
    for name, size, file_digest in parsed:
        digest.update(f"{file_digest} {size} {name}\n".encode())
    if "sha256:" + digest.hexdigest() != canonical_digest:
        raise AuthenticationError("artifact listing hash mismatch")
    return True


def load_view(root, canonical_digest):
    """Return verified header layouts and file digests; no payload is read."""
    table = TensorIndex()
    try:
        return _load_view(root, canonical_digest, table)
    except BaseException:
        table.close()
        raise


def _load_view(root, canonical_digest, table):
    if not isinstance(root, str) or not os.path.isabs(root):
        raise InvalidDescriptorError("metadata path must be absolute")
    view = _json(_read(root, "view.json", MAX_VIEW))
    if (
        not isinstance(view, dict)
        or view.get("kind") != "gpustack-model-transfer-metadata-view/v1"
        or view.get("incomplete") is not True
        or view.get("artifactDigest") != canonical_digest
    ):
        raise AuthenticationError("metadata view identity mismatch")
    entries = view.get("files")
    if not isinstance(entries, list) or len(entries) > MAX_FILES:
        raise InvalidDescriptorError("invalid metadata file count")
    layouts, hashes, expected_files = table.layouts, {}, {"view.json"}
    total = 0
    listing = None
    metadata_digests = {}
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) - {
            "name",
            "size",
            "sha256",
            "truncated",
        }:
            raise InvalidDescriptorError("invalid metadata record")
        name, size, digest = entry.get("name"), entry.get("size"), entry.get("sha256")
        if (
            not isinstance(name, str)
            or name in expected_files
            or isinstance(size, bool)
            or not isinstance(size, int)
            or size < 0
        ):
            raise InvalidDescriptorError("invalid metadata file")
        is_header = name.endswith(".safetensors")
        if is_header:
            if entry.get("truncated") is not True or size > MAX_HEADER + 8:
                raise InvalidDescriptorError("checkpoint payload is not metadata")
        elif name not in _ALLOWED or entry.get("truncated", False) or size > MAX_VIEW:
            raise InvalidDescriptorError("unrecognized metadata file")
        total += size
        if total > MAX_TOTAL:
            raise InvalidDescriptorError("metadata view exceeds bound")
        raw = _read(root, name, MAX_HEADER + 8 if is_header else MAX_VIEW)
        actual = hashlib.sha256(raw).hexdigest()
        if len(raw) != size or actual != digest:
            raise AuthenticationError("metadata file hash mismatch")
        hashes[name] = actual
        if not is_header and name != "manifest.json":
            metadata_digests[name] = (
                len(raw),
                "sha256:" + actual,
                "gitsha1:"
                + hashlib.sha1(f"blob {len(raw)}\x00".encode() + raw).hexdigest(),
            )
        expected_files.add(name)
        if is_header:
            if len(raw) < 10:
                raise InvalidDescriptorError("invalid header")
            header_length = int.from_bytes(raw[:8], "little")
            if (
                header_length < 2
                or header_length > MAX_HEADER
                or len(raw) != 8 + header_length
            ):
                raise InvalidDescriptorError(
                    "header stub contains payload or is incomplete"
                )
            header = _json(raw[8:])
            if not isinstance(header, dict):
                raise InvalidDescriptorError("invalid header")
            tensors = []
            for tensor_name, item in header.items():
                if tensor_name == "__metadata__":
                    continue
                if (
                    not isinstance(tensor_name, str)
                    or not tensor_name
                    or not isinstance(item, dict)
                    or set(item) != {"dtype", "shape", "data_offsets"}
                ):
                    raise InvalidDescriptorError("invalid tensor header")
                offsets = item["data_offsets"]
                if (
                    not isinstance(offsets, list)
                    or len(offsets) != 2
                    or any(
                        isinstance(n, bool) or not isinstance(n, int) for n in offsets
                    )
                    or offsets[0] < 0
                    or offsets[1] < offsets[0]
                ):
                    raise InvalidDescriptorError("invalid header offsets")
                if not isinstance(item["shape"], list) or not isinstance(
                    item["dtype"], str
                ):
                    raise InvalidDescriptorError("invalid tensor shape or dtype")
                tensors.append(
                    TensorDescriptor(
                        name=tensor_name,
                        file=name,
                        dtype=item["dtype"],
                        shape=tuple(item["shape"]),
                        data_begin=8 + header_length + offsets[0],
                        data_end=8 + header_length + offsets[1],
                    )
                )
            table.add(tensors)
        elif name.endswith(".json"):
            parsed = _json(raw)
            if not isinstance(parsed, dict):
                raise InvalidDescriptorError("invalid metadata document")
            if name == "manifest.json":
                if not _valid_listing(parsed, canonical_digest):
                    raise AuthenticationError("artifact listing is required")
                listing = {entry["path"]: entry for entry in parsed["entries"]}
            if name == "model.safetensors.index.json":
                index = parsed.get("weight_map")
                if not isinstance(index, dict):
                    raise InvalidDescriptorError("invalid checkpoint index")
    if not {"config.json", "manifest.json"}.issubset(expected_files) or not layouts:
        raise InvalidDescriptorError("metadata view incomplete")
    if "model.safetensors.index.json" in expected_files and (
        len(index) != len(layouts)
        or any(index.get(name) != layout[0] for name, layout in layouts.items())
    ):
        raise AuthenticationError("checkpoint index does not match headers")
    if listing is None:
        raise AuthenticationError("artifact listing is required")
    for name, (size, sha, git) in metadata_digests.items():
        entry = listing.get(name)
        if entry is None or entry["size"] != size or entry["digest"] not in (sha, git):
            raise AuthenticationError("metadata file differs from published listing")
    recorded_dirs = set()
    for name in expected_files:
        parent = os.path.dirname(name)
        while parent:
            recorded_dirs.add(parent)
            parent = os.path.dirname(parent)

    def walk_error(_error):
        raise InvalidDescriptorError("metadata directory unavailable")

    for directory, subdirs, files in os.walk(
        root, followlinks=False, onerror=walk_error
    ):
        for name in subdirs:
            path = os.path.join(directory, name)
            relative = os.path.relpath(path, root).replace(os.sep, "/")
            if os.path.islink(path) or relative not in recorded_dirs:
                raise InvalidDescriptorError("unlisted metadata directory")
        for name in files:
            path = os.path.join(directory, name)
            if (
                os.path.islink(path)
                or os.path.relpath(path, root).replace(os.sep, "/")
                not in expected_files
            ):
                raise InvalidDescriptorError("unlisted metadata file")
    return layouts, hashes, listing


def verify_layout(manifest, layouts, hashes, listing_verified):
    if not isinstance(listing_verified, dict):
        raise AuthenticationError("authenticated file listing is required")
    for file in manifest.files:
        entry = listing_verified.get(file.path)
        if entry is None or entry["size"] != file.size:
            raise AuthenticationError("manifest file differs from published listing")
    if len(manifest.tensors) != len(layouts):
        raise AuthenticationError("manifest and metadata tensor counts differ")
    for tensor in manifest.tensors:
        expected = (
            tensor.file,
            tensor.dtype,
            tuple(tensor.shape),
            tensor.data_begin,
            tensor.data_end,
        )
        if layouts.get(tensor.name) != expected:
            raise AuthenticationError("manifest and metadata layouts differ")
    files = {file.path: file for file in manifest.files}
    for name in hashes:
        if name.endswith(".safetensors"):
            continue
        if name == "manifest.json" and name not in files and listing_verified:
            continue
        if name not in files:
            raise AuthenticationError("metadata file differs from manifest")
