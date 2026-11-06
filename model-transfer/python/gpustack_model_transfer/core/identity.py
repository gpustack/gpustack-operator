"""Artifact identity: artifact UID, manifest digest, source generation.

One load attempt binds all three values together. A manifest that digests to
anything else, or a reader session opened against another generation, is
refused before any tensor is exposed.
"""

import re
from dataclasses import dataclass

from gpustack_model_transfer.core.errors import IdentityError

_UID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_DIGEST_PATTERN = re.compile(r"^[0-9a-f]{64}$")


@dataclass(frozen=True)
class ArtifactIdentity:
    """The immutable identity one reader session is bound to.

    artifact_uid names the artifact (a ModelArtifact UID in the managed
    path), manifest_sha256 is the sha256 of the exact manifest bytes, and
    source_generation is the immutable generation of the prepared source.
    """

    artifact_uid: str
    manifest_sha256: str
    source_generation: int

    def validate(self) -> None:
        if not isinstance(self.artifact_uid, str) or not _UID_PATTERN.match(
            self.artifact_uid
        ):
            raise IdentityError(f"invalid artifact uid: {self.artifact_uid!r}")
        if not isinstance(self.manifest_sha256, str) or not _DIGEST_PATTERN.match(
            self.manifest_sha256
        ):
            raise IdentityError("manifest digest must be 64 lowercase hex characters")
        if (
            isinstance(self.source_generation, bool)
            or not isinstance(self.source_generation, int)
            or self.source_generation < 1
        ):
            raise IdentityError(
                f"source generation must be a positive integer, "
                f"got {self.source_generation!r}"
            )


__all__ = ["ArtifactIdentity", "IdentityError"]
