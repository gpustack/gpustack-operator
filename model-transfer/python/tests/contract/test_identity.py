"""Artifact identity: artifact UID, manifest digest, and source generation."""

import unittest

from gpustack_model_transfer.core import identity


def valid_identity(**overrides):
    fields = {
        "artifact_uid": "model-fixture-tiny-dense",
        "manifest_sha256": "a" * 64,
        "source_generation": 1,
    }
    fields.update(overrides)
    return identity.ArtifactIdentity(**fields)


class TestArtifactIdentity(unittest.TestCase):
    def test_valid_identity_passes(self):
        valid_identity().validate()  # must not raise

    def test_identity_is_immutable(self):
        with self.assertRaises(Exception):
            valid_identity().artifact_uid = "other"

    def test_rejects_empty_or_malformed_uid(self):
        cases = ["", "has space", "slash/inside", "x" * 129, " leading"]
        for uid in cases:
            with self.subTest(uid=uid):
                with self.assertRaises(identity.IdentityError):
                    valid_identity(artifact_uid=uid).validate()

    def test_rejects_non_hex_manifest_digest(self):
        for digest in ["", "zz" * 32, "a" * 63, "A" * 64]:
            with self.subTest(digest=digest):
                with self.assertRaises(identity.IdentityError):
                    valid_identity(manifest_sha256=digest).validate()

    def test_rejects_non_positive_generation(self):
        for generation in [0, -1]:
            with self.subTest(generation=generation):
                with self.assertRaises(identity.IdentityError):
                    valid_identity(source_generation=generation).validate()


if __name__ == "__main__":
    unittest.main()
