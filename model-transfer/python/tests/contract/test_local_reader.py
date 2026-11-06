"""The local verified-file reader over the shared golden fixture."""

import shutil
import tempfile
import unittest
from pathlib import Path

import fixtureutil

from gpustack_model_transfer.core import errors
from gpustack_model_transfer.core.local_reader import LocalFileReader


class TestLocalFileReader(unittest.TestCase):
    def test_open_verifies_manifest_and_file_digests(self):
        reader = LocalFileReader(
            fixtureutil.fixture_dir() / "manifest.json",
            expected_sha256=fixtureutil.manifest_digest(),
        )
        self.addCleanup(reader.close)
        self.assertEqual(reader.identity().artifact_uid, "model-fixture-tiny-dense")
        self.assertEqual(
            reader.identity().manifest_sha256, fixtureutil.manifest_digest()
        )

    def test_read_returns_exact_tensor_bytes(self):
        reader = LocalFileReader(fixtureutil.fixture_dir() / "manifest.json")
        self.addCleanup(reader.close)
        t = reader.manifest.tensor("model.embed_tokens.weight")
        payload = reader.read_bytes(t.file, t.data_begin, t.data_end)
        self.assertEqual(payload, fixtureutil.tensor_payload(t.name))
        # Each read returns independent storage.
        again = reader.read_bytes(t.file, t.data_begin, t.data_end)
        self.assertEqual(payload, again)
        self.assertIsNot(payload, again)

    def test_manifest_digest_mismatch_refused(self):
        with self.assertRaises(errors.AuthenticationError):
            LocalFileReader(
                fixtureutil.fixture_dir() / "manifest.json", expected_sha256="0" * 64
            )

    def test_published_payload_is_not_rehashed_at_open(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copytree(fixtureutil.fixture_dir(), root, dirs_exist_ok=True)
            blob = bytearray((root / "checkpoint.safetensors").read_bytes())
            blob[-1] ^= 0xFF
            (root / "checkpoint.safetensors").write_bytes(bytes(blob))
            reader = LocalFileReader(root / "manifest.json")
            self.addCleanup(reader.close)
            self.assertEqual(
                reader.read_bytes("checkpoint.safetensors", len(blob) - 1, len(blob)),
                bytes(blob[-1:]),
            )

    def test_read_refuses_changed_extent_even_when_range_still_exists(self):
        for delta in (-1, 1):
            with self.subTest(delta=delta), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                shutil.copytree(fixtureutil.fixture_dir(), root, dirs_exist_ok=True)
                reader = LocalFileReader(root / "manifest.json")
                self.addCleanup(reader.close)
                file = reader.manifest.file("checkpoint.safetensors")
                with open(root / file.path, "r+b") as handle:
                    handle.truncate(file.size + delta)
                with self.assertRaises(errors.SourceUnavailableError):
                    reader.read_bytes(file.path, 0, 1)

    def test_missing_file_is_source_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copytree(fixtureutil.fixture_dir(), root, dirs_exist_ok=True)
            (root / "checkpoint.safetensors").unlink()
            with self.assertRaises(errors.SourceUnavailableError):
                LocalFileReader(root / "manifest.json")

    def test_read_beyond_declared_size_is_range_overflow(self):
        reader = LocalFileReader(fixtureutil.fixture_dir() / "manifest.json")
        self.addCleanup(reader.close)
        f = reader.manifest.file("checkpoint.safetensors")
        with self.assertRaises(errors.RangeOverflowError):
            reader.read_bytes(f.path, 0, f.size + 1)

    def test_negative_range_rejected(self):
        reader = LocalFileReader(fixtureutil.fixture_dir() / "manifest.json")
        self.addCleanup(reader.close)
        with self.assertRaises(errors.RangeOverflowError):
            reader.read_bytes("checkpoint.safetensors", -1, 4)

    def test_truncated_file_detected_at_read(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copytree(fixtureutil.fixture_dir(), root, dirs_exist_ok=True)
            reader = LocalFileReader(root / "manifest.json")
            self.addCleanup(reader.close)
            f = reader.manifest.file("checkpoint.safetensors")
            with open(root / f.path, "r+b") as fh:
                fh.truncate(f.size // 2)
            with self.assertRaises(errors.SourceUnavailableError):
                reader.read_bytes(f.path, f.size - 10, f.size)


class TestSingleHandleVerification(unittest.TestCase):
    """The digest check and the served bytes come from one file handle."""

    def test_verify_and_serve_use_one_open(self):
        import os
        from unittest.mock import patch

        opens = []
        real_open = os.open

        def counting_open(file, *args, **kwargs):
            if str(file).endswith("checkpoint.safetensors"):
                opens.append(str(file))
            return real_open(file, *args, **kwargs)

        with patch("os.open", side_effect=counting_open):
            reader = LocalFileReader(fixtureutil.fixture_dir() / "manifest.json")
        self.addCleanup(reader.close)
        self.assertEqual(len(opens), 1)
        # Serving must still work through that single handle.
        t = reader.manifest.tensor("model.norm.weight")
        self.assertEqual(
            reader.read_bytes(t.file, t.data_begin, t.data_end),
            fixtureutil.tensor_payload(t.name),
        )

    def test_serves_verified_bytes_after_file_swap(self):
        with tempfile.TemporaryDirectory() as tmp:
            import os

            root = Path(tmp)
            shutil.copytree(fixtureutil.fixture_dir(), root, dirs_exist_ok=True)
            reader = LocalFileReader(root / "manifest.json")
            self.addCleanup(reader.close)
            t = reader.manifest.tensor("model.norm.weight")
            original = fixtureutil.tensor_payload(t.name)
            # Replace the file behind the manifest with a new inode holding
            # different bytes; the open handle keeps serving verified bytes.
            tampered = bytearray((root / t.file).read_bytes())
            tampered[t.data_begin] ^= 0xFF
            swap = root / "swap.bin"
            swap.write_bytes(bytes(tampered))
            os.replace(swap, root / t.file)
            self.assertEqual(
                reader.read_bytes(t.file, t.data_begin, t.data_end), original
            )


class TestBoundedVerify(unittest.TestCase):
    """File verification reads in bounded chunks, never the whole file."""

    CHUNK_BOUND = 1024 * 1024
    FILE_SIZE = int(2.5 * 1024 * 1024)

    def _build_large_artifact(self, root: Path) -> Path:
        import json

        pattern = bytes(range(256))
        blob = (pattern * (self.FILE_SIZE // 256 + 1))[: self.FILE_SIZE]
        (root / "big.bin").write_bytes(blob)
        manifest = {
            "contract_version": 2,
            "artifact_uid": "big-fixture",
            "source_generation": 1,
            "files": [
                {
                    "path": "big.bin",
                    "size": self.FILE_SIZE,
                }
            ],
            "tensors": [
                {
                    "name": "big.weight",
                    "file": "big.bin",
                    "dtype": "F32",
                    "shape": [(self.FILE_SIZE - 64) // 4],
                    "data_begin": 64,
                    "data_end": self.FILE_SIZE,
                }
            ],
        }
        raw = (
            json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode() + b"\n"
        )
        (root / "manifest.json").write_bytes(raw)
        return root / "manifest.json"

    def test_open_does_not_read_payload(self):
        from unittest.mock import patch

        with tempfile.TemporaryDirectory() as tmp:
            manifest_path = self._build_large_artifact(Path(tmp))
            with patch("os.fdopen") as opened:
                reader = LocalFileReader(manifest_path)
                opened.return_value.read.assert_not_called()
                self.assertEqual(opened.call_count, 1)
                reader.close()


if __name__ == "__main__":
    unittest.main()
