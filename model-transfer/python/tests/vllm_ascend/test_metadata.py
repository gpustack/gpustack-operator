"""Metadata-first probes: bounded header parsing and manifest cross-checks."""

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import ascendutil as fixtureutil

from gpustack_model_transfer.core.errors import (
    AuthenticationError,
    DeniedContentError,
    InvalidDescriptorError,
    SourceUnavailableError,
)

MAX_PROBE_CHUNK = 16


class _CountingReader:
    """Counts reads and records every range it served."""

    def __init__(self, manifest_path, expected_sha256=None):
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        self._inner = LocalFileReader(manifest_path, expected_sha256=expected_sha256)
        self.manifest = self._inner.manifest
        self.reads = []

    def read_bytes(self, file_path, begin, end):
        self.reads.append((file_path, begin, end))
        return self._inner.read_bytes(file_path, begin, end)

    def close(self):
        self._inner.close()


def open_counting():
    return _CountingReader(
        fixtureutil.manifest_path(), expected_sha256=fixtureutil.manifest_digest()
    )


class TestHeaderProbe(unittest.TestCase):
    def test_probe_parses_the_safetensors_header(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            parse_safetensors_header,
        )

        reader = open_counting()
        try:
            header = parse_safetensors_header(reader, "checkpoint.safetensors")
        finally:
            reader.close()
        self.assertIn("lm_head.weight", header)
        self.assertEqual(header["lm_head.weight"]["dtype"], "F32")
        self.assertEqual(header["lm_head.weight"]["shape"], [4, 3])

    def test_header_parse_reads_only_in_bounded_chunks(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            parse_safetensors_header,
        )

        reader = open_counting()
        try:
            parse_safetensors_header(
                reader, "checkpoint.safetensors", max_chunk_bytes=MAX_PROBE_CHUNK
            )
        finally:
            reader.close()
        self.assertGreater(len(reader.reads), 0, "the header was read")
        for _, begin, end in reader.reads:
            self.assertLessEqual(
                end - begin,
                MAX_PROBE_CHUNK,
                f"the probe read {end - begin} bytes past the chunk bound",
            )
        # The pieces still reassemble into the real header: every read
        # continues the previous one's offset on the same file.
        cursor = 0
        for _, begin, end in reader.reads:
            self.assertEqual(begin, cursor, "the chunked reads are contiguous")
            cursor = end

    def test_probe_reports_configuration_before_any_tensor_read(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            metadata = probe_checkpoint_metadata(
                reader, max_chunk_bytes=MAX_PROBE_CHUNK
            )
            self.assertGreater(len(reader.reads), 0, "the probe parsed the header")
            for _, begin, _ in reader.reads:
                for tensor in reader.manifest.tensors:
                    if not tensor.byte_size:
                        continue
                    self.assertFalse(
                        begin >= tensor.data_begin and begin < tensor.data_end,
                        f"the probe read inside tensor {tensor.name!r}",
                    )
            self.assertFalse(
                metadata.payload_bytes_read, "the probe served tensor payload bytes"
            )
            self.assertEqual(metadata.checkpoint_format, "safetensors")
            self.assertEqual(metadata.dtypes, ("F16", "F32", "I64"))
            self.assertIn("model.embed_tokens.weight", metadata.parameter_names)
            self.assertEqual(metadata.engine_metadata_names, ("metadata.step",))
        finally:
            reader.close()

    def test_probe_cross_checks_header_against_the_manifest(self):
        # The golden fixture agrees with itself, so the probe accepts it:
        # the same probe is what refuses a disagreement below.
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            metadata = probe_checkpoint_metadata(reader)
        finally:
            reader.close()
        self.assertEqual(len(metadata.parameter_names), 6)

    def test_probe_rejects_a_header_dtype_the_manifest_disputes(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            orig = reader.manifest.tensors
            bad = type(orig[0])(
                name=orig[0].name,
                file=orig[0].file,
                dtype="F16",
                shape=orig[0].shape,
                data_begin=orig[0].data_begin,
                data_end=orig[0].data_end,
            )
            with self.assertRaises(InvalidDescriptorError):
                probe_checkpoint_metadata(_ReaderLike(reader, (bad, *orig[1:])))
        finally:
            reader.close()

    def test_probe_rejects_a_header_offset_the_manifest_disputes(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            orig = reader.manifest.tensors
            shifted = type(orig[0])(
                name=orig[0].name,
                file=orig[0].file,
                dtype=orig[0].dtype,
                shape=orig[0].shape,
                data_begin=orig[0].data_begin + 4,
                data_end=orig[0].data_end,
            )
            with self.assertRaises(InvalidDescriptorError):
                probe_checkpoint_metadata(_ReaderLike(reader, (shifted, *orig[1:])))
        finally:
            reader.close()

    def test_tie_flag_is_an_explicit_probe_input(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            untied = probe_checkpoint_metadata(reader)
            tied = probe_checkpoint_metadata(reader, tie_word_embeddings=True)
        finally:
            reader.close()
        self.assertFalse(untied.tie_word_embeddings)
        self.assertTrue(tied.tie_word_embeddings)

    def test_effective_dtypes_cover_every_parameter(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            metadata = probe_checkpoint_metadata(reader)
        finally:
            reader.close()
        self.assertEqual(
            metadata.effective_dtypes,
            {
                "lm_head.weight": "F32",
                "model.embed_tokens.weight": "F32",
                "model.layers.0.attn.q_proj.weight": "F32",
                "model.layers.0.attn.bias": "F32",
                "model.layers.0.mlp.up_proj.weight": "F16",
                "model.norm.weight": "F32",
            },
        )
        self.assertNotIn("metadata.step", metadata.effective_dtypes)


class TestProbeRefusals(unittest.TestCase):
    """Refusals happen before any model is built or any weight is read."""

    def probe(self, manifest_path=None, expected=None):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = _CountingReader(
            manifest_path or fixtureutil.manifest_path(), expected_sha256=expected
        )
        try:
            return probe_checkpoint_metadata(reader)
        finally:
            reader.close()

    def test_unverified_manifest_refused(self):
        with self.assertRaises(AuthenticationError):
            self.probe(expected="0" * 64)

    def test_missing_checkpoint_file_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copy(fixtureutil.manifest_path(), root / "manifest.json")
            with self.assertRaises(SourceUnavailableError):
                self.probe(
                    manifest_path=root / "manifest.json",
                    expected=fixtureutil.manifest_digest(),
                )

    def test_non_safetensors_checkpoint_refused(self):
        # The bytes are a valid safetensors file, but the manifest points
        # at them under a name the pinned route does not read, so the
        # refusal comes from format policy, not from a missing file.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copy(fixtureutil.manifest_path(), root / "manifest.json")
            shutil.copy(
                fixtureutil.fixture_dir() / "checkpoint.safetensors",
                root / "checkpoint.bin",
            )
            manifest = fixtureutil.manifest_dict()
            manifest["files"][0]["path"] = "checkpoint.bin"
            for tensor in manifest["tensors"]:
                tensor["file"] = "checkpoint.bin"
            (root / "manifest.json").write_bytes(json.dumps(manifest).encode())
            with self.assertRaises(InvalidDescriptorError):
                self.probe(manifest_path=root / "manifest.json", expected=None)

    def test_empty_tensor_table_refused(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            probe_checkpoint_metadata,
        )

        reader = open_counting()
        try:
            with self.assertRaises(InvalidDescriptorError):
                probe_checkpoint_metadata(_ReaderLike(reader, ()))
        finally:
            reader.close()


class TestPayloadPolicy(unittest.TestCase):
    """The dense-route policy, checked on names and dtypes directly."""

    def test_quantization_names_refused(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            reject_unsupported_payload,
        )

        for name in (
            "model.layers.0.attn.q_proj.weight_scale",
            "model.layers.0.mlp.down_proj.qweight",
            "model.layers.0.mlp.gate_proj.scales",
            "model.layers.0.mlp.up_proj.g_idx",
        ):
            with self.assertRaises(DeniedContentError, msg=name):
                reject_unsupported_payload([name])

    def test_dense_names_pass_the_policy(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            reject_unsupported_payload,
        )

        reject_unsupported_payload(fixtureutil.parameter_names())

    def test_unsupported_dtype_refused(self):
        from gpustack_model_transfer.adapters.vllm_ascend.metadata import (
            check_supported_dtypes,
        )

        with self.assertRaises(InvalidDescriptorError):
            check_supported_dtypes(("F32", "F8"))
        check_supported_dtypes(("F32", "F16", "BF16", "I64"))


class _ReaderLike:
    """A reader whose manifest carries the given tensor table."""

    def __init__(self, inner, tensors):
        self._inner = inner
        self.manifest = _ManifestLike(inner.manifest, tensors)

    def read_bytes(self, file_path, begin, end):
        return self._inner.read_bytes(file_path, begin, end)

    def close(self):
        self._inner.close()


class _ManifestLike:
    def __init__(self, inner, tensors):
        self._inner = inner
        self.tensors = tensors

    def __getattr__(self, name):
        return getattr(self._inner, name)


if __name__ == "__main__":
    unittest.main()
