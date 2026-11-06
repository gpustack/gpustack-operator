"""The local CPU contract suite: every contract capability on real fixtures.

Each capability exercises the core contract against the verified fixture the
configuration pins. A capability that cannot run is NO-GO, never a skipped
pass; UNRUN stays reserved for suites whose prerequisites are genuinely
unobservable on this host.
"""

import contextlib
import time
from pathlib import Path

from gpustack_model_transfer.acceptance.config import RefusedError
from gpustack_model_transfer.acceptance.report import CheckResult
from gpustack_model_transfer.core import descriptors, versioning
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import (
    BackendCapabilities,
    InvalidCapabilitiesError,
    RejectedOption,
    TransportSelection,
    local_backend_capabilities,
)
from gpustack_model_transfer.core.errors import (
    AmbiguousReadError,
    AuthenticationError,
    CapacityExhaustedError,
    InvalidDescriptorError,
    OperationStateError,
    RangeOverflowError,
)
from gpustack_model_transfer.core.identity import ArtifactIdentity
from gpustack_model_transfer.core.local_reader import LocalFileReader
from gpustack_model_transfer.core.session import ReaderSession


class _CountingReader:
    """Reader wrapper that counts reads to observe refusal-before-mutation."""

    def __init__(self, inner):
        self.manifest = inner.manifest
        self._inner = inner
        self.reads = 0
        self.ambiguous = False

    def read_bytes(self, file_path, begin, end):
        self.reads += 1
        if self.ambiguous:
            raise AmbiguousReadError("backend state not terminal")
        return self._inner.read_bytes(file_path, begin, end)

    def close(self):
        self._inner.close()


def _budgets(context):
    limits = context["config"]["limits"]
    return ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )


def _open_reader(context):
    artifact = context["config"]["artifact"]
    return LocalFileReader(
        context["manifest_path"], expected_sha256=artifact["manifest_sha256"]
    )


def _open_session(context, reader=None, budgets=None):
    actual_reader = reader or _open_reader(context)
    return ReaderSession(
        identity=actual_reader.manifest.identity(),
        capabilities=local_backend_capabilities(),
        budgets=budgets or _budgets(context),
        reader=actual_reader,
    )


def _check_version_identity(context):
    manifest = context["manifest"]
    versioning.check_contract_version(manifest.contract_version)
    assert versioning.contract_identity() == "gpustack-model-transfer/reader/2"
    return "manifest binds to supported contract version 2"


def _check_identity_binding(context):
    reader = _open_reader(context)
    try:
        manifest = reader.manifest
        ArtifactIdentity(
            artifact_uid=manifest.artifact_uid,
            manifest_sha256=manifest.manifest_sha256,
            source_generation=manifest.source_generation,
        ).validate()
    finally:
        reader.close()
    try:
        LocalFileReader(context["manifest_path"], expected_sha256="0" * 64)
    except AuthenticationError:
        return "artifact UID, manifest digest, and generation bind together"
    raise AssertionError("wrong manifest digest was accepted")


def _check_descriptor_validation(context):
    manifest = context["manifest"]
    file_size = manifest.files[0].size
    descriptor = manifest.tensors[0]
    oversized = descriptors.TensorDescriptor(
        name=descriptor.name,
        file=descriptor.file,
        dtype=descriptor.dtype,
        shape=descriptor.shape,
        data_begin=file_size - 1,
        data_end=file_size + 8,
    )
    try:
        oversized.validate(file_size)
    except InvalidDescriptorError:
        return "dtype, shape, offsets, and overlap validated before exposure"
    raise AssertionError("out-of-bounds descriptor was accepted")


def _check_bounds_overflow(context):
    session = _open_session(context)
    try:
        name = session.descriptors[0].name
        size = session.descriptors[0].byte_size
        for offset, length in [(-1, 1), (0, size + 1), (size, 1), (0, 2**63)]:
            try:
                session.read_range(name, offset, length)
            except RangeOverflowError:
                continue
            raise AssertionError(
                f"range [{offset}, +{length}) was not rejected as overflow"
            )
        return "negative, past-end, and int64-overflow ranges refused"
    finally:
        session.close()


def _check_owned_outputs(context):
    inner = _open_reader(context)
    counting = _CountingReader(inner)
    session = ReaderSession(
        identity=inner.identity(),
        capabilities=local_backend_capabilities(),
        budgets=_budgets(context),
        reader=counting,
    )
    try:
        tensor = max(session.descriptors, key=lambda t: t.byte_size)
        output = session.read_tensor(tensor.name)
        if len(output.payload) != tensor.byte_size:
            raise AssertionError("owned output lost bytes")
    finally:
        session.close()
    # The owned-output budget, not the window, bounds the tensor: shrink it
    # below the tensor size and the read must refuse before any file read.
    tight = ReadBudgets(
        window_bytes=_budgets(context).window_bytes,
        max_inflight=1,
        owned_output_bytes=tensor.byte_size - 1,
    )
    counting2 = _CountingReader(_open_reader(context))
    session2 = ReaderSession(
        identity=counting2.manifest.identity(),
        capabilities=local_backend_capabilities(),
        budgets=tight,
        reader=counting2,
    )
    try:
        try:
            session2.read_tensor(tensor.name)
        except CapacityExhaustedError as exc:
            if counting2.reads != 0:
                raise AssertionError("refusal read bytes before rejecting") from exc
            return (
                f"tensor of {tensor.byte_size} bytes served through "
                f"{_budgets(context).window_bytes}-byte windows; "
                f"oversized tensor refused before any read"
            )
        raise AssertionError("tensor above the owned-output budget was accepted")
    finally:
        session2.close()


def _check_operation_completion(context):
    counting = _CountingReader(_open_reader(context))
    session = ReaderSession(
        identity=counting.manifest.identity(),
        capabilities=local_backend_capabilities(),
        budgets=_budgets(context),
        reader=counting,
    )
    try:
        session.read_tensor(session.descriptors[0].name)
        counting.ambiguous = True
        with contextlib.suppress(AmbiguousReadError):
            session.read_tensor(session.descriptors[-1].name)
        unknown = [
            op for op in session.operations.values() if op.state.value == "unknown"
        ]
        if len(unknown) != 1 or not unknown[0].retained:
            raise AssertionError("ambiguous read did not retain an UNKNOWN op")
        try:
            session.release(unknown[0].op_id)
        except OperationStateError:
            session.resolve(
                unknown[0].op_id,
                "failed",
                error_code="source-unavailable",
                error_message="fence confirmed loss",
            )
            return (
                "every submitted operation holds its own state; UNKNOWN "
                "stays retained and unreleasable"
            )
        raise AssertionError("UNKNOWN operation was releasable")
    finally:
        counting.close()


def _check_cancel_release(context):
    session = _open_session(context)
    try:
        output = session.read_tensor(session.descriptors[0].name)
        session.cancel(output.op_id)
        session.cancel(output.op_id)
        session.release(output.op_id)
        session.release(output.op_id)
        try:
            session.cancel("missing-op")
        except OperationStateError:
            return "cancel and release idempotent; unknown operations rejected"
        raise AssertionError("cancelling an unknown operation was accepted")
    finally:
        session.close()


def _check_capabilities(context):
    caps = local_backend_capabilities()
    caps.validate()
    if caps.effective != "local" or not caps.effective_observed:
        raise AssertionError("local backend must report an observed backend")
    broken = BackendCapabilities(
        requested=("ucx", "tcp"),
        applied="ucx",
        effective="ucx",
        effective_observed=True,
        rejected=(RejectedOption("ucx", "no verbs devices"),),
        formats=caps.formats,
        memory_types=caps.memory_types,
        max_request_bytes=caps.max_request_bytes,
        alignment_bytes=caps.alignment_bytes,
        tensor_ownership=caps.tensor_ownership,
        device_visibility=caps.device_visibility,
        transport=TransportSelection(control="tcp://ctl", payload=("tcp://data",)),
    )
    try:
        broken.validate()
    except InvalidCapabilitiesError:
        return (
            "requested/applied/effective distinguished; rejected options "
            "reported and never applied"
        )
    raise AssertionError("a rejected-then-applied option passed validation")


def _check_fixture_verification(context):
    reader = _open_reader(context)
    try:
        manifest = reader.manifest
        for tensor in manifest.tensors:
            payload = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
            with open(
                Path(context["manifest_path"]).parent / tensor.file, "rb"
            ) as source:
                source.seek(tensor.data_begin)
                expected = source.read(tensor.data_end - tensor.data_begin)
            if payload != expected:
                raise AssertionError("fixture bytes differ")
        return (
            f"all {len(manifest.tensors)} fixture tensors verified against "
            f"expected fixture bytes"
        )
    finally:
        reader.close()


_CHECKS = [
    ("version-identity", _check_version_identity),
    ("identity-binding", _check_identity_binding),
    ("descriptor-validation", _check_descriptor_validation),
    ("bounds-overflow", _check_bounds_overflow),
    ("owned-outputs", _check_owned_outputs),
    ("operation-completion", _check_operation_completion),
    ("cancel-release", _check_cancel_release),
    ("capabilities", _check_capabilities),
    ("fixture-verification", _check_fixture_verification),
]


def run(context):
    """Run every capability; failures are NO-GO, never silent skips."""
    results = []
    deadline = context["operation_seconds"]
    for capability, check in _CHECKS:
        started = time.monotonic()
        try:
            detail = check(context)
            elapsed = time.monotonic() - started
            if elapsed > deadline:
                results.append(
                    CheckResult(
                        capability,
                        "NO-GO",
                        f"deadline exceeded: {elapsed:.2f}s > {deadline}s",
                    )
                )
                continue
            results.append(CheckResult(capability, "PASS", detail))
        except Exception as exc:  # a failed check is a verdict, not a crash
            results.append(
                CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")
            )
    return results


__all__ = ["RefusedError", "run"]
