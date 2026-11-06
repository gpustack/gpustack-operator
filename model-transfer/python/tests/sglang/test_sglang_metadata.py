"""The SGLang metadata probe: payload-free, bounded-chunk, cross-checked."""

import unittest

import sglangutil as fixtureutil

from gpustack_model_transfer.core.errors import (
    DeniedContentError,
    InvalidDescriptorError,
)
from gpustack_model_transfer.core.local_reader import LocalFileReader


class CountingReader:
    """A reader that records every span it served."""

    def __init__(self):
        self._inner = LocalFileReader(
            fixtureutil.manifest_path(), expected_sha256=fixtureutil.manifest_digest()
        )
        self.manifest = self._inner.manifest
        self.ranges = []
        self.max_chunk = 0

    def read_bytes(self, file_path, begin, end):
        self.ranges.append((file_path, begin, end))
        self.max_chunk = max(self.max_chunk, end - begin)
        return self._inner.read_bytes(file_path, begin, end)

    def close(self):
        self._inner.close()


class TestProbeCheckpointMetadata(unittest.TestCase):
    def test_probe_answers_from_the_header_alone(self):
        from gpustack_model_transfer.adapters.sglang.metadata import (
            probe_checkpoint_metadata,
        )

        reader = CountingReader()
        try:
            probed = probe_checkpoint_metadata(reader, max_chunk_bytes=16)
        finally:
            reader.close()
        self.assertEqual(
            probed.checkpoint_format,
            "safetensors",
        )
        self.assertFalse(probed.payload_bytes_read)
        self.assertIn("model.norm.weight", probed.parameter_names)
        self.assertIn("metadata.step", probed.engine_metadata_names)
        self.assertNotIn("metadata.step", probed.parameter_names)
        # Every header read stayed inside the header span: none touched a
        # tensor payload.
        header_end = 8 + _header_length()
        for _path, _begin, end in reader.ranges:
            self.assertLessEqual(end, header_end, "the probe read past the header")

    def test_probe_reads_in_bounded_chunks(self):
        from gpustack_model_transfer.adapters.sglang.metadata import (
            probe_checkpoint_metadata,
        )

        reader = CountingReader()
        try:
            probe_checkpoint_metadata(reader, max_chunk_bytes=8)
        finally:
            reader.close()
        self.assertLessEqual(reader.max_chunk, 8)
        self.assertGreater(len(reader.ranges), 1, "the header was read in chunks")

    def test_probe_cross_checks_the_manifest(self):
        from gpustack_model_transfer.adapters.sglang.metadata import (
            probe_checkpoint_metadata,
        )

        reader = CountingReader()
        try:
            probed = probe_checkpoint_metadata(reader)
        finally:
            reader.close()
        by_name = {t.name: t for t in reader.manifest.tensors}
        for name, dtype in probed.effective_dtypes.items():
            self.assertEqual(dtype, by_name[name].dtype)
        self.assertIn("lm_head.weight", probed.parameter_names)

    def test_probe_refuses_a_dtype_mismatch_between_header_and_manifest(self):
        from gpustack_model_transfer.adapters.sglang.metadata import (
            probe_checkpoint_metadata,
        )

        reader = CountingReader()
        try:
            tampered = list(reader.manifest.tensors)
            for index, tensor in enumerate(tampered):
                if tensor.name == "model.norm.weight":
                    object.__setattr__(tampered[index], "dtype", "F16")
            reader.manifest.__dict__["tensors"] = tuple(tampered)
            with self.assertRaises(InvalidDescriptorError):
                probe_checkpoint_metadata(reader)
        finally:
            reader.close()

    def test_probe_refuses_a_quantized_payload(self):
        from gpustack_model_transfer.adapters.sglang.metadata import (
            reject_unsupported_payload,
        )

        with self.assertRaises(DeniedContentError):
            reject_unsupported_payload(["model.layers.0.mlp.qweight"])

    def test_probe_refuses_a_manifest_without_tensors(self):
        from gpustack_model_transfer.adapters.sglang.metadata import (
            probe_checkpoint_metadata,
        )

        reader = CountingReader()
        try:
            reader.manifest.__dict__["tensors"] = ()
            with self.assertRaises(InvalidDescriptorError):
                probe_checkpoint_metadata(reader)
        finally:
            reader.close()


def _header_length() -> int:
    blob = fixtureutil.blob_bytes()
    return int.from_bytes(blob[:8], "little")


if __name__ == "__main__":
    unittest.main()
