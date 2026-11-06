"""Work-order parsing: the broker's assignment, validated before anything.

A work order carries one generation's identity, the EXACT portable
manifest bytes, the admission bounds, and the published tree path. Every
field is validated here — bounds included — before any native context,
allocation, or registration exists, so a malformed or oversized order
can never leak a native owner. Tenant-shaped fields (secret, tenant
tensor and file names) never reach a log, a status view, or an argv.
"""

import hashlib
import json
import os
import re

from gpustack_model_transfer.core.budgets import align_down_2m
from gpustack_model_transfer.core.errors import (
    InvalidDescriptorError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.manifest import MAX_MANIFEST_BYTES, parse_manifest
from gpustack_model_transfer.envs import get_envs
from gpustack_model_transfer.source.fill import (
    MAX_REGISTRATIONS,
    MAX_TOTAL_BYTES,
    check_admission,
    registered_files,
    resident_file_bytes,
)

_SECRET_PATTERN = re.compile(r"^[A-Za-z0-9._~+/=-]{1,512}$")
_DIGEST_PREFIX = "sha256:"

# Wire bounds enforced before any vendor call.
MAX_MANIFEST_JSON_CHARS = MAX_MANIFEST_BYTES
MIN_RESIDENT_BYTES = 1 << 30


def resident_budget(total_bytes, admitted_bytes, backend):
    """Resolve a source ceiling without enlarging the broker's admission."""
    envs = get_envs()
    raw = envs.GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES_RAW
    configured = envs.GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES
    if raw not in (None, "") and (configured is None or configured < 0):
        raise InvalidDescriptorError("invalid source resident budget")
    budget = min(total_bytes, admitted_bytes)
    if configured:
        budget = min(budget, max(align_down_2m(configured), MIN_RESIDENT_BYTES))
    if backend == "hixl":
        legacy = envs.GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES or 16 << 30
        budget = min(budget, max(align_down_2m(legacy), MIN_RESIDENT_BYTES))
    return budget


class Assignment:
    """One admitted work order: identity, manifest, admission, tree."""

    def __init__(
        self,
        generation,
        worker_secret,
        digest,
        namespace,
        artifact_uid,
        source_generation,
        manifest_sha256,
        manifest_json,
        tree_path,
        total_bytes,
        registrations,
        backend,
        resident_budget_bytes=None,
    ):
        self.generation = generation
        self.worker_secret = worker_secret
        self.digest = digest
        self.namespace = namespace
        self.artifact_uid = artifact_uid
        self.source_generation = source_generation
        self.manifest_sha256 = manifest_sha256
        self.manifest_json = manifest_json
        self.tree_path = tree_path
        self.total_bytes = total_bytes
        self.registrations = registrations
        self.backend = backend
        self.resident_budget_bytes = (
            total_bytes if resident_budget_bytes is None else resident_budget_bytes
        )
        self.paged = total_bytes > self.resident_budget_bytes

    @property
    def manifest_bytes(self) -> bytes:
        """The manifest's EXACT encoded bytes, from the order's string.

        The string is decoded as strict UTF-8 and never reserialized:
        the manifest identity is the digest of exactly these bytes.
        """
        try:
            return self.manifest_json.encode("utf-8")
        except (UnicodeEncodeError, AttributeError) as exc:
            raise InvalidDescriptorError(
                "the work order's manifest is not valid UTF-8 text"
            ) from exc

    def revision(self) -> tuple:
        """The assignment revision: re-delivery of the same order is a
        no-op, a changed order for the same generation is a conflict."""
        return (
            self.generation,
            self.worker_secret,
            self.digest,
            self.namespace,
            self.artifact_uid,
            self.source_generation,
            self.manifest_sha256,
            self.tree_path,
            self.total_bytes,
            self.registrations,
            self.resident_budget_bytes,
        )

    def redacted(self) -> dict:
        """A log/status view: kind-level facts, no secret, no tenant paths."""
        return {
            "generation": self.generation,
            "namespace": self.namespace,
            "artifactUID": self.artifact_uid,
            "sourceGeneration": self.source_generation,
            "backend": self.backend,
            "registrations": self.registrations,
            "totalBytes": self.total_bytes,
        }

    def __repr__(self):
        return f"Assignment(generation={self.generation!r}, secret=<redacted>)"


def parse_assignment(payload, *, backend: str, page_reader=None) -> Assignment:
    """Validate one broker work order into an Assignment.

    Every required field is checked with its type, shape, and bound; the
    manifest bytes are parsed and digest-verified here, so an order that
    cannot be honored is refused before any native owner exists.
    """
    if not isinstance(payload, dict):
        raise InvalidDescriptorError("a work order must be a JSON object")
    required = (
        "generation",
        "workerSecret",
        "digest",
        "namespace",
        "artifactUID",
        "sourceGeneration",
        "manifestSHA256",
        "manifestJSON",
        "treePath",
        "totalBytes",
        "registrations",
    )
    missing = [key for key in required if key not in payload]
    if missing:
        raise InvalidDescriptorError(
            f"work order is missing required fields: {sorted(missing)}"
        )
    generation = payload["generation"]
    if not isinstance(generation, str) or not re.fullmatch(
        r"[A-Za-z0-9_-]{1,128}", generation
    ):
        raise InvalidDescriptorError(
            "work order generation must be an opaque bounded identifier"
        )
    if backend not in ("nixl", "hixl"):
        raise InvalidDescriptorError(
            f"work order backend must be nixl or hixl, got {backend!r}"
        )
    secret = payload["workerSecret"]
    if not isinstance(secret, str) or not _SECRET_PATTERN.match(secret):
        raise InvalidDescriptorError("work order secret has an unusable shape")
    digest = payload["digest"]
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise InvalidDescriptorError("work order digest must be a canonical sha256")
    manifest_sha256 = payload["manifestSHA256"]
    if not isinstance(manifest_sha256, str) or not re.fullmatch(
        r"[0-9a-f]{64}", manifest_sha256
    ):
        raise InvalidDescriptorError(
            "work order manifest digest must be 64 lowercase hex characters"
        )
    manifest_json = payload["manifestJSON"]
    if not isinstance(manifest_json, str):
        raise InvalidDescriptorError("work order manifest must be a string")
    if len(manifest_json) > MAX_MANIFEST_JSON_CHARS:
        raise InvalidDescriptorError("work order manifest exceeds the wire bound")
    tree_path = payload["treePath"]
    if (
        not isinstance(tree_path, str)
        or not os.path.isabs(tree_path)
        or len(tree_path) > 4096
        or "\x00" in tree_path
    ):
        raise InvalidDescriptorError(
            "work order tree path must be bounded and absolute"
        )
    total_bytes = payload["totalBytes"]
    registrations = payload["registrations"]
    for name, value in (("totalBytes", total_bytes), ("registrations", registrations)):
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            raise InvalidDescriptorError(
                f"work order {name} must be a non-negative integer"
            )

    admitted_bytes = payload.get("residentBudgetBytes", total_bytes)
    if (
        isinstance(admitted_bytes, bool)
        or not isinstance(admitted_bytes, int)
        or admitted_bytes <= 0
        or admitted_bytes > MAX_TOTAL_BYTES + MAX_REGISTRATIONS * (4 << 20)
    ):
        raise InvalidDescriptorError("invalid admitted source resident budget")

    assignment = Assignment(
        generation=generation,
        worker_secret=secret,
        digest=digest,
        namespace=payload.get("namespace", ""),
        artifact_uid=payload["artifactUID"],
        source_generation=payload["sourceGeneration"],
        manifest_sha256=manifest_sha256,
        manifest_json=manifest_json,
        tree_path=tree_path,
        total_bytes=total_bytes,
        registrations=registrations,
        backend=backend,
        resident_budget_bytes=admitted_bytes,
    )
    manifest_bytes = assignment.manifest_bytes
    if len(manifest_bytes) > MAX_MANIFEST_BYTES:
        raise InvalidDescriptorError("work order manifest exceeds the byte bound")
    manifest = parse_manifest(
        manifest_bytes, expected_sha256=manifest_sha256, page_reader=page_reader
    )
    if manifest.artifact_uid != assignment.artifact_uid:
        raise InvalidDescriptorError(
            "the manifest's artifact UID does not match the work order"
        )
    if manifest.source_generation != assignment.source_generation:
        raise InvalidDescriptorError(
            "the manifest's source generation does not match the work order"
        )
    # The manifest's own extent and nonempty-file count must match the
    # admission, inside the process bounds, before any native owner.
    check_admission(manifest, total_bytes, registrations)
    if registrations == 0:
        raise InvalidDescriptorError("work order has no registered files")
    files = registered_files(manifest)
    required_bytes = sum(resident_file_bytes(f.size, backend) for f in files)
    if admitted_bytes > required_bytes:
        raise InvalidDescriptorError("invalid admitted source resident budget")
    assignment.resident_budget_bytes = resident_budget(
        required_bytes, admitted_bytes, backend
    )
    assignment.paged = required_bytes > assignment.resident_budget_bytes
    if assignment.paged and assignment.resident_budget_bytes < max(
        MIN_RESIDENT_BYTES, max(resident_file_bytes(f.size, backend) for f in files)
    ):
        raise InvalidDescriptorError(
            "source resident budget cannot hold the largest shard or the source floor"
        )
    descriptors = [
        {
            "name": f.path,
            "addr": (1 << 64) - 1,
            "length": f.size,
            "memoryType": "HOST",
            "backend": backend,
            "workerUID": "x" * 128,
        }
        for f in registered_files(manifest)
    ]
    if len(json.dumps(descriptors).encode()) > 768 * 1024:
        raise InvalidDescriptorError("registration descriptors exceed wire bound")
    assignment.manifest = manifest
    return assignment


def canonical_digest(manifest_sha256: str) -> str:
    """The canonical digest form the work order carries: sha256:<rawhex>."""
    return f"{_DIGEST_PREFIX}{manifest_sha256}"


def sha256_of(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def require_pod_identity(environ) -> str:
    """The worker's Pod UID, from the environment, never fabricated.

    A source holder without a Pod UID has no worker identity to register
    under; the refusal is visible instead of a made-up default.
    """
    pod_uid = environ.get("KUBERNETES_POD_UID", "")
    if not isinstance(pod_uid, str) or not re.fullmatch(
        r"[A-Za-z0-9._-]{1,128}", pod_uid
    ):
        raise SourceUnavailableError(
            "KUBERNETES_POD_UID is not set: the source holder refuses to "
            "start without its Pod identity"
        )
    return pod_uid


__all__ = [
    "MAX_MANIFEST_BYTES",
    "Assignment",
    "canonical_digest",
    "parse_assignment",
    "require_pod_identity",
    "sha256_of",
]
