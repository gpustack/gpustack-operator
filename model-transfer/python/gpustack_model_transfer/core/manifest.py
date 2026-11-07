"""Manifest identity and layout validation.

The manifest binds one artifact UID and source generation to the complete
file and tensor layout. Loading verifies the manifest digest against the
expected bytes, rejects unknown fields and duplicate JSON keys, and validates
every descriptor before the manifest is usable.
"""

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path

from gpustack_model_transfer.core import versioning
from gpustack_model_transfer.core.descriptors import (
    FileDescriptor,
    TensorDescriptor,
    validate_file_tensors,
)
from gpustack_model_transfer.core.errors import (
    AuthenticationError,
    InvalidDescriptorError,
)
from gpustack_model_transfer.core.identity import ArtifactIdentity

MAX_MANIFEST_BYTES = 64 << 20
MAX_PAGED_MANIFEST_BYTES = 4 << 20
MAX_MANIFEST_FILES = 16384
MAX_MANIFEST_TENSORS = 1 << 20
# A raw JSON string can expand sixfold when nested in another JSON document.
MAX_MANIFEST_RESPONSE_BYTES = 6 * MAX_MANIFEST_BYTES + (1 << 20)

_TOP_KEYS = {
    "contract_version",
    "artifact_uid",
    "source_generation",
    "files",
    "tensors",
}
_FILE_KEYS = {"path", "size"}
_TENSOR_KEYS = {
    "name",
    "file",
    "dtype",
    "shape",
    "data_begin",
    "data_end",
}


def _reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidDescriptorError(f"duplicate JSON key: {key!r}")
        result[key] = value
    return result


def _require_exact_keys(obj: dict, keys, where: str) -> None:
    if not isinstance(obj, dict):
        raise InvalidDescriptorError(f"{where} must be a JSON object")
    actual = set(obj)
    expected = set(keys)
    if actual != expected:
        missing = sorted(expected - actual)
        unknown = sorted(actual - expected)
        raise InvalidDescriptorError(
            f"{where} keys mismatch: missing={missing} unknown={unknown}"
        )


@dataclass(frozen=True)
class Manifest:
    """A verified manifest: identity fields plus file and tensor tables."""

    contract_version: int
    artifact_uid: str
    source_generation: int
    files: tuple
    tensors: tuple
    manifest_sha256: str

    def identity(self) -> ArtifactIdentity:
        return ArtifactIdentity(
            artifact_uid=self.artifact_uid,
            manifest_sha256=self.manifest_sha256,
            source_generation=self.source_generation,
        )

    def file(self, path: str) -> FileDescriptor:
        for descriptor in self.files:
            if descriptor.path == path:
                return descriptor
        raise InvalidDescriptorError(f"unknown file: {path!r}")

    def tensor(self, name: str) -> TensorDescriptor:
        from gpustack_model_transfer.core.tensor_index import TensorIndex

        if isinstance(self.tensors, TensorIndex):
            try:
                return self.tensors.by_name[name]
            except KeyError:
                raise InvalidDescriptorError(f"unknown tensor: {name!r}") from None
        for descriptor in self.tensors:
            if descriptor.name == name:
                return descriptor
        raise InvalidDescriptorError(f"unknown tensor: {name!r}")


def load_manifest(path, expected_sha256: str | None = None) -> Manifest:
    """Load and fully verify a manifest file.

    expected_sha256, when given, must equal the sha256 of the manifest bytes;
    a mismatch is an authentication error, not a parse error.
    """
    from gpustack_model_transfer.core.manifest_pages import local_page_reader

    with Path(path).open("rb") as handle:
        return parse_manifest(
            handle.read(MAX_MANIFEST_BYTES + 1),
            expected_sha256,
            page_reader=local_page_reader(Path(path).parent / "pages"),
        )


def parse_manifest(
    raw: bytes, expected_sha256: str | None = None, *, page_reader=None
) -> Manifest:
    """Parse and fully verify manifest bytes.

    The bytes are the manifest's EXACT encoded form — a work order carries
    them as a string, and the parser never reserializes: the digest bound
    into the identity is the digest of these bytes. expected_sha256, when
    given, must equal their sha256; a mismatch is an authentication error,
    not a parse error.
    """
    if not isinstance(raw, bytes):
        raise InvalidDescriptorError("manifest bytes must be bytes")
    if len(raw) > MAX_MANIFEST_BYTES:
        raise InvalidDescriptorError("manifest exceeds the byte bound")
    digest = hashlib.sha256(raw).hexdigest()
    if expected_sha256 is not None and digest != expected_sha256:
        raise AuthenticationError(
            f"manifest digest mismatch: expected {expected_sha256}, got {digest}"
        )
    try:
        data = json.loads(raw, object_pairs_hook=_reject_duplicate_keys)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise InvalidDescriptorError(f"manifest is not valid JSON: {exc}") from None
    paged = isinstance(data, dict) and "tensor_pages" in data
    _require_exact_keys(
        data, _TOP_KEYS | {"tensor_pages"} if paged else _TOP_KEYS, "manifest"
    )
    if not isinstance(data["files"], list) or not isinstance(data["tensors"], list):
        raise InvalidDescriptorError("manifest files and tensors must be lists")
    if (
        len(data["files"]) > MAX_MANIFEST_FILES
        or len(data["tensors"]) > MAX_MANIFEST_TENSORS
    ):
        raise InvalidDescriptorError("manifest count exceeds bound")

    versioning.check_contract_version(data["contract_version"])
    identity = ArtifactIdentity(
        artifact_uid=data["artifact_uid"],
        manifest_sha256=digest,
        source_generation=data["source_generation"],
    )
    identity.validate()

    files = []
    seen_paths = {}
    for entry in data["files"]:
        _require_exact_keys(entry, _FILE_KEYS, "file entry")
        descriptor = FileDescriptor(path=entry["path"], size=entry["size"])
        descriptor.validate()
        if descriptor.path in seen_paths:
            raise InvalidDescriptorError(f"duplicate file entry: {descriptor.path!r}")
        seen_paths[descriptor.path] = descriptor.size
        files.append(descriptor)

    if paged:
        if len(raw) > MAX_PAGED_MANIFEST_BYTES:
            raise InvalidDescriptorError("paged root exceeds byte bound")
        if data["tensors"]:
            raise InvalidDescriptorError("mixed inline and paged tensor layouts")
        from gpustack_model_transfer.core.manifest_pages import read_paged_tensors

        tensors = read_paged_tensors(data["tensor_pages"], files, page_reader)
        return Manifest(
            contract_version=data["contract_version"],
            artifact_uid=data["artifact_uid"],
            source_generation=data["source_generation"],
            files=tuple(files),
            tensors=tensors,
            manifest_sha256=digest,
        )

    tensors = []
    for entry in data["tensors"]:
        _require_exact_keys(entry, _TENSOR_KEYS, "tensor entry")
        descriptor = TensorDescriptor(
            name=entry["name"],
            file=entry["file"],
            dtype=entry["dtype"],
            shape=tuple(entry["shape"]),
            data_begin=entry["data_begin"],
            data_end=entry["data_end"],
        )
        tensors.append(descriptor)

    by_path = {}
    for tensor in tensors:
        if tensor.file not in seen_paths:
            raise InvalidDescriptorError(
                f"tensor {tensor.name!r} references unknown file {tensor.file!r}"
            )
        by_path.setdefault(tensor.file, []).append(tensor)
    for path, path_tensors in by_path.items():
        validate_file_tensors(path_tensors, seen_paths[path])

    names = [t.name for t in tensors]
    if len(set(names)) != len(names):
        raise InvalidDescriptorError("duplicate tensor names in manifest")

    return Manifest(
        contract_version=data["contract_version"],
        artifact_uid=data["artifact_uid"],
        source_generation=data["source_generation"],
        files=tuple(files),
        tensors=tuple(tensors),
        manifest_sha256=digest,
    )


__all__ = [
    "AuthenticationError",
    "InvalidDescriptorError",
    "Manifest",
    "load_manifest",
    "parse_manifest",
]
