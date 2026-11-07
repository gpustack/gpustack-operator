"""Manifest loading and verification against the shared golden fixture."""

import json
import unittest

import fixtureutil
import pytest

from gpustack_model_transfer.core import errors, manifest


@pytest.mark.parametrize("excess", [0, 1])
def test_manifest_raw_byte_boundary(tmp_path, excess):
    raw = fixtureutil.manifest_bytes()
    size = manifest.MAX_MANIFEST_BYTES + excess
    path = tmp_path / "manifest.json"
    path.write_bytes(raw + b" " * (size - len(raw)))
    if excess:
        with pytest.raises(errors.InvalidDescriptorError, match="byte bound"):
            manifest.load_manifest(path)
    else:
        assert len(manifest.load_manifest(path).tensors) == 7


@pytest.mark.parametrize(
    ("field", "value", "message"),
    [
        ("files", None, "must be lists"),
        ("tensors", {}, "must be lists"),
        ("files", [None] * 3, "count exceeds"),
        ("tensors", [None] * 3, "count exceeds"),
    ],
)
def test_manifest_counts_rejected_before_descriptor_construction(
    monkeypatch, field, value, message
):
    monkeypatch.setattr(manifest, "MAX_MANIFEST_FILES", 2)
    monkeypatch.setattr(manifest, "MAX_MANIFEST_TENSORS", 2)
    document = fixtureutil.manifest_dict()
    document["files"] = []
    document["tensors"] = []
    document[field] = value
    with pytest.raises(errors.InvalidDescriptorError, match=message):
        manifest.parse_manifest(json.dumps(document).encode())


def test_manifest_count_boundary_is_inclusive(monkeypatch):
    document = fixtureutil.manifest_dict()
    monkeypatch.setattr(manifest, "MAX_MANIFEST_FILES", len(document["files"]))
    monkeypatch.setattr(manifest, "MAX_MANIFEST_TENSORS", len(document["tensors"]))
    assert len(manifest.parse_manifest(json.dumps(document).encode()).tensors) == 7


def test_manifest_capacity_and_nested_response_contract():
    from gpustack_model_transfer.client_transport import MAX_RESPONSE
    from gpustack_model_transfer.source.control import MAX_RESPONSE_BYTES
    from gpustack_model_transfer.source.transfer_service import MAX_JOBS
    from gpustack_model_transfer.source.workorder import MAX_MANIFEST_BYTES

    assert MAX_MANIFEST_BYTES == manifest.MAX_MANIFEST_BYTES == 64 << 20
    assert MAX_JOBS == manifest.MAX_MANIFEST_TENSORS == 1 << 20
    assert MAX_RESPONSE_BYTES == MAX_RESPONSE == manifest.MAX_MANIFEST_RESPONSE_BYTES
    assert MAX_RESPONSE >= 6 * MAX_MANIFEST_BYTES + (1 << 20)


class TestGoldenManifest(unittest.TestCase):
    def test_sidecar_digest_matches_manifest_bytes(self):
        sidecar = (fixtureutil.fixture_dir() / "manifest.sha256").read_text().strip()
        self.assertEqual(sidecar, fixtureutil.manifest_digest())

    def test_load_manifest_with_expected_digest(self):
        m = manifest.load_manifest(
            fixtureutil.fixture_dir() / "manifest.json",
            expected_sha256=fixtureutil.manifest_digest(),
        )
        self.assertEqual(m.artifact_uid, "model-fixture-tiny-dense")
        self.assertEqual(m.source_generation, 1)
        self.assertEqual(len(m.files), 1)
        self.assertEqual(len(m.tensors), 7)

    def test_tensors_and_files_are_indexable(self):
        m = manifest.load_manifest(fixtureutil.fixture_dir() / "manifest.json")
        t = m.tensor("model.norm.weight")
        self.assertEqual(t.dtype, "F32")
        self.assertEqual(t.shape, (3,))
        f = m.file("checkpoint.safetensors")
        self.assertEqual(f.size, 734)

    def test_unknown_tensor_and_file_raise(self):
        m = manifest.load_manifest(fixtureutil.fixture_dir() / "manifest.json")
        with self.assertRaises(errors.InvalidDescriptorError):
            m.tensor("nope")
        with self.assertRaises(errors.InvalidDescriptorError):
            m.file("nope.bin")


def load_mutated(testcase, mutate):
    import shutil
    import tempfile

    data = fixtureutil.manifest_dict()
    mutate(data)
    tmp = tempfile.mkdtemp()
    testcase.addCleanup(shutil.rmtree, tmp)
    path = f"{tmp}/manifest.json"
    with open(path, "w") as fh:
        json.dump(data, fh)
    return path


class TestManifestRejections(unittest.TestCase):
    def test_digest_mismatch_is_authentication_error(self):
        with self.assertRaises(errors.AuthenticationError):
            manifest.load_manifest(
                fixtureutil.fixture_dir() / "manifest.json",
                expected_sha256="0" * 64,
            )

    def test_wrong_contract_version_rejected(self):
        path = load_mutated(self, lambda d: d.update(contract_version=99))
        with self.assertRaises(errors.ContractVersionError):
            manifest.load_manifest(path)

    def test_unknown_top_level_field_rejected(self):
        path = load_mutated(self, lambda d: d.update(extra=1))
        with self.assertRaises(errors.InvalidDescriptorError):
            manifest.load_manifest(path)

    def test_unknown_tensor_field_rejected(self):
        def mutate(d):
            d["tensors"][0]["extra"] = 1

        path = load_mutated(self, mutate)
        with self.assertRaises(errors.InvalidDescriptorError):
            manifest.load_manifest(path)

    def test_tensor_referencing_unknown_file_rejected(self):
        def mutate(d):
            d["tensors"][0]["file"] = "ghost.bin"

        path = load_mutated(self, mutate)
        with self.assertRaises(errors.InvalidDescriptorError):
            manifest.load_manifest(path)

    def test_tensor_beyond_declared_file_size_rejected(self):
        def mutate(d):
            d["files"][0]["size"] = 600

        path = load_mutated(self, mutate)
        with self.assertRaises(errors.InvalidDescriptorError):
            manifest.load_manifest(path)

    def test_file_digest_length_rejected(self):
        def mutate(d):
            d["files"][0]["sha256"] = "abcd"

        path = load_mutated(self, mutate)
        with self.assertRaises(errors.InvalidDescriptorError):
            manifest.load_manifest(path)

    def test_duplicate_json_keys_rejected(self):
        raw = fixtureutil.manifest_bytes().decode()
        raw = raw.replace('{"artifact_uid"', '{"artifact_uid":"x","artifact_uid"', 1)
        import tempfile

        tmp = tempfile.mkdtemp()
        self.addCleanup(__import__("shutil").rmtree, tmp)
        path = f"{tmp}/manifest.json"
        with open(path, "w") as fh:
            fh.write(raw)
        with self.assertRaises(errors.InvalidDescriptorError):
            manifest.load_manifest(path)


if __name__ == "__main__":
    unittest.main()
