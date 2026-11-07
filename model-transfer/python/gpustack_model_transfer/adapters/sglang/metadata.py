"""Metadata-first probes: configuration decisions before any weight moves.

Everything the model configuration needs — checkpoint format, dtypes, the
tied-embedding flag, the parameter vs engine-metadata split — is answered
from the verified manifest and the safetensors header alone. The probe
never reads tensor payload bytes, so a refusal costs nothing and a load
plan is preflighted before any parameter exists to mutate.
"""

import json
from dataclasses import dataclass

from gpustack_model_transfer.adapters.sglang.constants import (
    SUPPORTED_CHECKPOINT_FORMAT,
    SUPPORTED_DTYPES,
)
from gpustack_model_transfer.core import versioning
from gpustack_model_transfer.core.errors import (
    DeniedContentError,
    InvalidDescriptorError,
)

# The safetensors header is bounded: a real header is kilobytes, and a
# "header" of gigabytes is a corrupt file, not a big model.
MAX_HEADER_BYTES = 64 * 1024 * 1024

# Tensor names with these suffixes carry a quantized payload. The pinned
# route serves dense checkpoints; anything else is refused, not approximated.
_QUANTIZATION_SUFFIXES = (
    ".weight_scale",
    ".weight_packed",
    ".weight_quant",
    ".qweight",
    ".qscale",
    ".scales",
    ".zeros",
    ".qzeros",
    ".g_idx",
    ".quant_state",
)

_ENGINE_METADATA_PREFIX = "metadata."


def check_supported_dtypes(dtypes) -> None:
    """Refuse a dtype the pinned route does not assemble accurately."""
    for dtype in dtypes:
        if dtype not in SUPPORTED_DTYPES:
            raise InvalidDescriptorError(
                f"unsupported dtype {dtype!r}: the pinned SGLang route "
                f"assembles {sorted(SUPPORTED_DTYPES)}"
            )


def reject_unsupported_payload(names) -> None:
    """Refuse a quantized payload; dense safetensors is the pinned route."""
    for name in names:
        for suffix in _QUANTIZATION_SUFFIXES:
            if name.endswith(suffix):
                raise DeniedContentError(
                    f"tensor {name!r} carries a quantized payload: the "
                    f"pinned route serves dense {SUPPORTED_CHECKPOINT_FORMAT} "
                    f"only"
                )


def _parse_safetensors_header_with_base(
    reader, file_path: str, max_chunk_bytes: int = 32
) -> tuple:
    """Parse the safetensors JSON header through the reader, payload-free.

    Returns (header_dict, data_base_offset) where data_base_offset is
    8 + header_length. Reads in chunks of at most max_chunk_bytes so header
    probes never exceed small transfer-window budgets in counting readers.
    """
    if max_chunk_bytes < 8:
        max_chunk_bytes = 8

    def _read_span(begin: int, end: int) -> bytes:
        chunks = []
        cursor = begin
        while cursor < end:
            step = min(max_chunk_bytes, end - cursor)
            chunks.append(reader.read_bytes(file_path, cursor, cursor + step))
            cursor += step
        return b"".join(chunks)

    prefix = _read_span(0, 8)
    length = int.from_bytes(prefix, "little")
    if length < 2 or length > MAX_HEADER_BYTES:
        raise InvalidDescriptorError(
            f"{file_path!r} has an implausible safetensors header length {length}"
        )
    raw = _read_span(8, 8 + length)

    def no_duplicates(pairs):
        seen = {}
        for key, value in pairs:
            if key in seen:
                raise InvalidDescriptorError(
                    f"{file_path!r} header has duplicate key {key!r}"
                )
            seen[key] = value
        return seen

    try:
        header = json.loads(raw, object_pairs_hook=no_duplicates)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise InvalidDescriptorError(
            f"{file_path!r} header is not valid JSON: {exc}"
        ) from None
    if not isinstance(header, dict):
        raise InvalidDescriptorError(f"{file_path!r} header must be a JSON object")
    for name, entry in header.items():
        if name == "__metadata__":
            continue
        if not isinstance(entry, dict):
            raise InvalidDescriptorError(
                f"{file_path!r} header entry {name!r} is not an object"
            )
        if not isinstance(entry.get("dtype"), str) or not isinstance(
            entry.get("shape"), list
        ):
            raise InvalidDescriptorError(
                f"{file_path!r} header entry {name!r} lacks dtype or shape"
            )
        for dim in entry["shape"]:
            if isinstance(dim, bool) or not isinstance(dim, int) or dim < 0:
                raise InvalidDescriptorError(
                    f"{file_path!r} header entry {name!r} has invalid shape"
                )
        offsets = entry.get("data_offsets")
        if (
            not isinstance(offsets, list)
            or len(offsets) != 2
            or any(
                isinstance(v, bool) or not isinstance(v, int) or v < 0 for v in offsets
            )
            or offsets[0] > offsets[1]
        ):
            raise InvalidDescriptorError(
                f"{file_path!r} header entry {name!r} has invalid data_offsets"
            )
    return header, 8 + length


def parse_safetensors_header(reader, file_path: str, max_chunk_bytes: int = 32) -> dict:
    """Parse the safetensors JSON header through the reader, payload-free."""
    header, _ = _parse_safetensors_header_with_base(
        reader, file_path, max_chunk_bytes=max_chunk_bytes
    )
    return header


@dataclass(frozen=True)
class CheckpointMetadata:
    """What the engine configuration needs, read before any weight moves.

    payload_bytes_read is always False on a probe result: the probe reads
    the safetensors header only, and the field exists so a caller cannot
    confuse a probe with a partial read.
    """

    checkpoint_format: str
    dtypes: tuple
    effective_dtypes: dict
    parameter_names: tuple
    engine_metadata_names: tuple
    tie_word_embeddings: bool
    architecture: str
    payload_bytes_read: bool


def probe_checkpoint_metadata(
    reader,
    *,
    tie_word_embeddings: bool = False,
    architecture: str = "",
    max_chunk_bytes: int = 32,
) -> CheckpointMetadata:
    """Answer the model-configuration questions from metadata only.

    Refuses an unverified or structurally wrong manifest, a checkpoint in
    any format but safetensors, an unsupported dtype, and a quantized
    payload — all before any tensor payload is read and before any model
    parameter exists to mutate. Parses each safetensors file header and
    cross-checks tensor dtypes, shapes, and byte offsets against the
    manifest.
    """
    manifest = reader.manifest
    versioning.check_contract_version(manifest.contract_version)
    if not manifest.tensors:
        raise InvalidDescriptorError("the manifest carries no tensors: nothing to load")
    for descriptor in manifest.files:
        if not descriptor.path.endswith("." + SUPPORTED_CHECKPOINT_FORMAT):
            raise InvalidDescriptorError(
                f"unsupported checkpoint file {descriptor.path!r}: the "
                f"pinned route reads {SUPPORTED_CHECKPOINT_FORMAT}"
            )

    names = [tensor.name for tensor in manifest.tensors]
    check_supported_dtypes(sorted({tensor.dtype for tensor in manifest.tensors}))
    reject_unsupported_payload(names)

    headers_by_file = {}
    for descriptor in manifest.files:
        headers_by_file[descriptor.path] = _parse_safetensors_header_with_base(
            reader, descriptor.path, max_chunk_bytes=max_chunk_bytes
        )

    declared = {descriptor.path: set() for descriptor in manifest.files}
    for tensor in manifest.tensors:
        declared[tensor.file].add(tensor.name)
    ignored = {"__metadata__"}
    if tie_word_embeddings and "model.embed_tokens.weight" in names:
        ignored.add("lm_head.weight")
    for file_path, (header, _data_base) in headers_by_file.items():
        if set(header) - ignored - declared[file_path]:
            raise InvalidDescriptorError(
                f"safetensors header for {file_path!r} contains undeclared tensors"
            )

    for tensor in manifest.tensors:
        parsed = headers_by_file.get(tensor.file)
        if parsed is None:
            raise InvalidDescriptorError(
                f"tensor {tensor.name!r} references undeclared file {tensor.file!r}"
            )
        header, data_base = parsed
        entry = header.get(tensor.name)
        if entry is None:
            raise InvalidDescriptorError(
                f"safetensors header for {tensor.file!r} is missing tensor "
                f"{tensor.name!r}"
            )
        if entry["dtype"] != tensor.dtype:
            raise InvalidDescriptorError(
                f"tensor {tensor.name!r} header dtype {entry['dtype']!r} "
                f"mismatches manifest dtype {tensor.dtype!r}"
            )
        if tuple(entry["shape"]) != tuple(tensor.shape):
            raise InvalidDescriptorError(
                f"tensor {tensor.name!r} header shape {entry['shape']} "
                f"mismatches manifest shape {list(tensor.shape)}"
            )
        offsets = entry["data_offsets"]
        if (
            data_base + offsets[0] != tensor.data_begin
            or data_base + offsets[1] != tensor.data_end
        ):
            raise InvalidDescriptorError(
                f"tensor {tensor.name!r} header data_offsets {offsets} "
                f"(base {data_base}) mismatch manifest "
                f"[{tensor.data_begin}, {tensor.data_end})"
            )

    parameter_names = tuple(
        sorted(name for name in names if not name.startswith(_ENGINE_METADATA_PREFIX))
    )
    engine_metadata_names = tuple(
        sorted(name for name in names if name.startswith(_ENGINE_METADATA_PREFIX))
    )
    effective_dtypes = {
        tensor.name: tensor.dtype
        for tensor in manifest.tensors
        if not tensor.name.startswith(_ENGINE_METADATA_PREFIX)
    }

    return CheckpointMetadata(
        checkpoint_format=SUPPORTED_CHECKPOINT_FORMAT,
        dtypes=tuple(sorted({tensor.dtype for tensor in manifest.tensors})),
        effective_dtypes=effective_dtypes,
        parameter_names=parameter_names,
        engine_metadata_names=engine_metadata_names,
        tie_word_embeddings=bool(tie_word_embeddings),
        architecture=architecture or "unknown",
        payload_bytes_read=False,
    )


__all__ = [
    "CheckpointMetadata",
    "check_supported_dtypes",
    "parse_safetensors_header",
    "probe_checkpoint_metadata",
    "reject_unsupported_payload",
]
