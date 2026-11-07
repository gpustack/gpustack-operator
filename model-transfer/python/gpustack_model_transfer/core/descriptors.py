"""Immutable file and tensor descriptors with bounds and overflow checks.

A descriptor is validated before any tensor is exposed: non-empty dtype,
non-negative shape, offsets inside the file, and int64 range bounds. Known
dtypes also require a byte span equal to shape times dtype size. Other dtypes
use the declared byte span for opaque transport; engine adapters own dtype
support checks. Values past int64 are range-overflow errors, never truncated.
"""

import itertools
from dataclasses import dataclass

from gpustack_model_transfer.core.errors import (
    InvalidDescriptorError,
    RangeOverflowError,
)

DTYPE_SIZES = {
    "F64": 8,
    "F32": 4,
    "F16": 2,
    "BF16": 2,
    "F8_E4M3": 1,
    "F8_E5M2": 1,
    "F8_E8M0": 1,
    "F8_E4M3FNUZ": 1,
    "F8_E5M2FNUZ": 1,
    "I64": 8,
    "I32": 4,
    "I16": 2,
    "I8": 1,
    "U64": 8,
    "U32": 4,
    "U16": 2,
    "U8": 1,
    "BOOL": 1,
    "C64": 8,
}

MAX_INT64 = 2**63 - 1


def shape_element_count(shape) -> int:
    """Return prod(shape) with an int64 range check."""
    if isinstance(shape, (str, bytes)) or not isinstance(shape, (tuple, list)):
        raise InvalidDescriptorError(f"shape must be a sequence, got {shape!r}")
    count = 1
    for dim in shape:
        if isinstance(dim, bool) or not isinstance(dim, int):
            raise InvalidDescriptorError(
                f"shape dimensions must be integers, got {shape!r}"
            )
        if dim < 0:
            raise InvalidDescriptorError(
                f"shape dimensions must be non-negative, got {shape!r}"
            )
        if dim > MAX_INT64:
            raise RangeOverflowError(f"shape dimension {dim} exceeds the int64 range")
        if count and dim:
            count = count * dim
            if count > MAX_INT64:
                raise RangeOverflowError(
                    f"element count {count} exceeds the int64 range"
                )
        elif dim == 0:
            count = 0
    return count


def _check_relative_path(path: str) -> None:
    if not isinstance(path, str) or not path:
        raise InvalidDescriptorError("file path must be a non-empty string")
    if "\x00" in path:
        raise InvalidDescriptorError("file path contains a NUL byte")
    if path.startswith("/"):
        raise InvalidDescriptorError(f"file path must be relative: {path!r}")
    parts = path.split("/")
    for part in parts:
        if part in ("", ".", ".."):
            raise InvalidDescriptorError(f"file path escapes its root: {path!r}")


@dataclass(frozen=True)
class FileDescriptor:
    """A published file inside the artifact: relative path and size."""

    path: str
    size: int

    def validate(self) -> None:
        _check_relative_path(self.path)
        if isinstance(self.size, bool) or not isinstance(self.size, int):
            raise InvalidDescriptorError("file size must be an integer")
        if self.size < 0:
            raise InvalidDescriptorError(
                f"file size must be non-negative, got {self.size}"
            )


@dataclass(frozen=True)
class TensorDescriptor:
    """A tensor inside one file, with absolute byte offsets in that file.

    data_begin and data_end are byte offsets from the start of the file, not
    from the safetensors data section. Content verification belongs to publication.
    """

    name: str
    file: str
    dtype: str
    shape: tuple
    data_begin: int
    data_end: int

    @property
    def element_count(self) -> int:
        return shape_element_count(self.shape)

    @property
    def byte_size(self) -> int:
        if (
            not isinstance(self.dtype, str)
            or not self.dtype.strip()
            or "\x00" in self.dtype
        ):
            raise InvalidDescriptorError(f"unknown dtype: {self.dtype!r}")
        count = self.element_count
        if self.dtype in DTYPE_SIZES and count > 0:
            size = count * DTYPE_SIZES[self.dtype]
            if size > MAX_INT64:
                raise RangeOverflowError(f"byte size {size} exceeds the int64 range")
            return size
        if count == 0:
            return 0
        if (
            isinstance(self.data_begin, int)
            and not isinstance(self.data_begin, bool)
            and isinstance(self.data_end, int)
            and not isinstance(self.data_end, bool)
            and 0 <= self.data_begin <= self.data_end <= MAX_INT64
        ):
            return self.data_end - self.data_begin
        return 0

    def validate(self, file_size: int) -> None:
        if not isinstance(self.name, str) or not self.name:
            raise InvalidDescriptorError("tensor name must be non-empty")
        if not isinstance(self.file, str) or not self.file:
            raise InvalidDescriptorError("tensor file must be non-empty")
        if "\x00" in self.name or "\x00" in self.file:
            raise InvalidDescriptorError(
                "tensor name and file must not contain a NUL byte"
            )
        if (
            not isinstance(self.dtype, str)
            or not self.dtype.strip()
            or "\x00" in self.dtype
        ):
            raise InvalidDescriptorError(f"unknown dtype: {self.dtype!r}")
        for field in ("data_begin", "data_end"):
            value = getattr(self, field)
            if isinstance(value, bool) or not isinstance(value, int):
                raise InvalidDescriptorError(
                    f"{field} must be an integer, got {value!r}"
                )
            if value > MAX_INT64:
                raise RangeOverflowError(f"{field} {value} exceeds the int64 range")
        # element_count and byte_size raise on overflow first.
        count = self.element_count
        byte_size = self.byte_size
        if self.data_begin < 0 or self.data_end < self.data_begin:
            raise InvalidDescriptorError(
                f"invalid offsets [{self.data_begin}, {self.data_end})"
            )
        if self.data_end > file_size:
            raise InvalidDescriptorError(
                f"tensor {self.name!r} ends at {self.data_end} beyond "
                f"file size {file_size}"
            )
        if (
            (count == 0 and self.data_end != self.data_begin)
            or (count > 0 and self.data_end == self.data_begin)
            or self.data_end - self.data_begin != byte_size
        ):
            raise InvalidDescriptorError(
                f"tensor {self.name!r} span "
                f"{self.data_end - self.data_begin} != byte size {byte_size}"
            )


def validate_file_tensors(tensors, file_size: int) -> None:
    """Validate each tensor and reject overlapping byte ranges in one file."""
    for tensor in tensors:
        tensor.validate(file_size)
    ordered = sorted(tensors, key=lambda t: (t.data_begin, t.data_end))
    for previous, current in itertools.pairwise(ordered):
        if current.data_begin < previous.data_end:
            raise InvalidDescriptorError(
                f"tensors {previous.name!r} and {current.name!r} overlap in "
                f"file {previous.file!r}"
            )


__all__ = [
    "DTYPE_SIZES",
    "MAX_INT64",
    "FileDescriptor",
    "InvalidDescriptorError",
    "RangeOverflowError",
    "TensorDescriptor",
    "shape_element_count",
    "validate_file_tensors",
]
