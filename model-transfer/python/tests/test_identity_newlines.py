"""Artifact identifiers must consume the complete input."""

import pytest

from gpustack_model_transfer.core.identity import ArtifactIdentity, IdentityError


@pytest.mark.parametrize("field", ["artifact_uid", "manifest_sha256"])
@pytest.mark.parametrize("suffix", ["\n", "\r\n"])
def test_identity_rejects_trailing_newline(field, suffix):
    values = {
        "artifact_uid": "artifact-1",
        "manifest_sha256": "a" * 64,
        "source_generation": 1,
    }
    ArtifactIdentity(**values).validate()
    values[field] += suffix
    with pytest.raises(IdentityError):
        ArtifactIdentity(**values).validate()
