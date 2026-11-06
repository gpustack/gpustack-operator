"""Tensor and file descriptors: dtype, shape, offsets, bounds, overflow."""

import unittest

from gpustack_model_transfer.core import descriptors
from gpustack_model_transfer.core.descriptors import (
    FileDescriptor,
    TensorDescriptor,
)

FILE_SIZE = 1000


def tensor(**overrides):
    fields = {
        "name": "w",
        "file": "checkpoint.safetensors",
        "dtype": "F32",
        "shape": (2, 3),
        "data_begin": 100,
        "data_end": 124,
    }
    fields.update(overrides)
    return TensorDescriptor(**fields)


class TestTensorDescriptor(unittest.TestCase):
    def test_valid_descriptor_passes(self):
        tensor().validate(FILE_SIZE)  # must not raise

    def test_known_dtypes_have_sizes(self):
        for dtype, size in descriptors.DTYPE_SIZES.items():
            self.assertGreaterEqual(size, 1, dtype)

    def test_empty_dtype_rejected_and_quantized_dtypes_accepted(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(dtype="").validate(FILE_SIZE)
        tensor(dtype="F8_E4M3", data_begin=100, data_end=106).validate(FILE_SIZE)
        tensor(dtype="F4", data_begin=100, data_end=103).validate(FILE_SIZE)

    def test_known_dtype_span_must_match_shape(self):
        for end in (123, 125):
            with self.subTest(end=end):
                with self.assertRaises(descriptors.InvalidDescriptorError):
                    tensor(data_end=end).validate(FILE_SIZE)

    def test_negative_shape_dimension_rejected(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(shape=(2, -1)).validate(FILE_SIZE)

    def test_shape_must_be_integers(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(shape=(2, 1.5)).validate(FILE_SIZE)

    def test_zero_element_tensor_is_valid(self):
        t = tensor(shape=(0,), data_begin=100, data_end=100)
        t.validate(FILE_SIZE)
        self.assertEqual(t.element_count, 0)
        self.assertEqual(t.byte_size, 0)

    def test_non_empty_tensor_requires_positive_offset_span(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(data_end=100).validate(FILE_SIZE)

    def test_offsets_out_of_file_rejected(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(data_begin=990, data_end=1014).validate(FILE_SIZE)

    def test_negative_offsets_rejected(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(data_begin=-4).validate(FILE_SIZE)

    def test_inverted_offsets_rejected(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(data_begin=124, data_end=100).validate(FILE_SIZE)

    def test_huge_shape_reports_overflow_not_bytes(self):
        # A shape whose element count exceeds the int64 range must be reported
        # as range overflow, never silently accepted or wrapped.
        huge = (2**40, 2**40)
        with self.assertRaises(descriptors.RangeOverflowError):
            tensor(shape=huge, data_begin=0, data_end=24).validate(FILE_SIZE)

    def test_int64_boundary_element_counts(self):
        # 2^62 elements of I64 would overflow int64 once multiplied out.
        with self.assertRaises(descriptors.RangeOverflowError):
            tensor(dtype="I64", shape=(2**62,), data_begin=0, data_end=24).validate(
                FILE_SIZE
            )

    def test_descriptor_is_immutable(self):
        with self.assertRaises(Exception):
            tensor().dtype = "F16"

    def test_rejects_nul_in_tensor_name_and_file(self):
        # NUL separates the derived-digest preimage fields, so it may not
        # appear inside a name or file reference.
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(name="a\x00b").validate(FILE_SIZE)
        with self.assertRaises(descriptors.InvalidDescriptorError):
            tensor(file="a\x00b").validate(FILE_SIZE)


class TestFileDescriptor(unittest.TestCase):
    def test_valid_file_descriptor(self):
        FileDescriptor(
            path="checkpoint.safetensors",
            size=734,
        ).validate()

    def test_rejects_path_escape(self):
        for path in ["../etc/passwd", "/abs/path", "a/../../b", "./x", "a//b"]:
            with self.subTest(path=path):
                with self.assertRaises(descriptors.InvalidDescriptorError):
                    FileDescriptor(
                        path=path,
                        size=1,
                    ).validate()

    def test_rejects_negative_size(self):
        with self.assertRaises(descriptors.InvalidDescriptorError):
            FileDescriptor(
                path="f",
                size=-1,
            ).validate()

    def test_rejects_nul_in_file_path(self):
        # NUL in a path would make the derived-digest preimage ambiguous.
        with self.assertRaises(descriptors.InvalidDescriptorError):
            FileDescriptor(
                path="a\x00b",
                size=5,
            ).validate()


class TestValidateFileTensors(unittest.TestCase):
    def test_overlapping_tensors_rejected(self):
        a = tensor(name="a", data_begin=100, data_end=124)
        b = tensor(name="b", data_begin=120, data_end=140)
        with self.assertRaises(descriptors.InvalidDescriptorError):
            descriptors.validate_file_tensors([a, b], FILE_SIZE)

    def test_adjacent_tensors_valid(self):
        a = tensor(name="a", data_begin=100, data_end=124)
        b = tensor(name="b", data_begin=124, data_end=148)
        descriptors.validate_file_tensors([a, b], FILE_SIZE)

    def test_unsorted_but_disjoint_tensors_valid(self):
        # Order in the manifest list is irrelevant; overlap is what matters.
        a = tensor(name="a", data_begin=124, data_end=148)
        b = tensor(name="b", data_begin=100, data_end=124)
        descriptors.validate_file_tensors([a, b], FILE_SIZE)


if __name__ == "__main__":
    unittest.main()
