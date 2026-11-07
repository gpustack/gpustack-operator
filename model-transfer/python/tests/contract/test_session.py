"""Reader sessions: reads, budgets, completion, cancel, release, UNKNOWN."""

import unittest

import fixtureutil

from gpustack_model_transfer.core import errors
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.identity import ArtifactIdentity
from gpustack_model_transfer.core.local_reader import LocalFileReader
from gpustack_model_transfer.core.session import ReaderSession

FIXTURE_DIR = fixtureutil.fixture_dir()
DIGEST = fixtureutil.manifest_digest()


def identity(**overrides):
    fields = {
        "artifact_uid": "model-fixture-tiny-dense",
        "manifest_sha256": DIGEST,
        "source_generation": 1,
    }
    fields.update(overrides)
    return ArtifactIdentity(**fields)


def budgets(**overrides):
    fields = {
        "window_bytes": 32,
        "max_inflight": 2,
        "owned_output_bytes": 4096,
    }
    fields.update(overrides)
    return ReadBudgets(**fields)


_TRACKED_READERS = []


def _track(reader):
    """Register a reader for closing at the running test's cleanup."""
    _TRACKED_READERS.append(reader)
    return reader


def _close_tracked_readers():
    while _TRACKED_READERS:
        _TRACKED_READERS.pop().close()


def session(reader=None, ident=None, bud=None):
    return ReaderSession(
        identity=ident or identity(),
        capabilities=local_backend_capabilities(),
        budgets=bud or budgets(),
        reader=reader
        or _track(
            LocalFileReader(FIXTURE_DIR / "manifest.json", expected_sha256=DIGEST)
        ),
    )


class FakeReader:
    """In-memory reader over the fixture with injection hooks."""

    def __init__(self, manifest):
        self.manifest = manifest
        self.read_count = 0
        self.ambiguous = False  # raise ambiguous error on next reads
        self.explode = False  # raise an unclassified error on next reads

    def read_bytes(self, file_path, begin, end):
        self.read_count += 1
        if self.explode:
            raise RuntimeError("transport exploded")
        if self.ambiguous:
            raise errors.AmbiguousReadError("backend state not terminal after submit")
        blob = (FIXTURE_DIR / file_path).read_bytes()
        return blob[begin:end]


def fake_session(**budget_overrides):
    # The local reader is opened for its manifest alone; track it so its
    # file handle closes with the test that created it.
    reader = _track(LocalFileReader(FIXTURE_DIR / "manifest.json"))
    fake = FakeReader(reader.manifest)
    return ReaderSession(
        identity=identity(),
        capabilities=local_backend_capabilities(),
        budgets=budgets(**budget_overrides),
        reader=fake,
    ), fake


class TrackedReadersTestCase(unittest.TestCase):
    """Close every reader this file created, at each test's cleanup."""

    def setUp(self):
        self.addCleanup(_close_tracked_readers)


class TestSessionOpen(TrackedReadersTestCase):
    def test_open_binds_identity_to_manifest(self):
        s = session()
        self.assertEqual(s.identity.artifact_uid, "model-fixture-tiny-dense")
        self.assertEqual(len(s.descriptors), 7)

    def test_open_rejects_artifact_uid_mismatch(self):
        with self.assertRaises(errors.AuthenticationError):
            session(ident=identity(artifact_uid="model-other"))

    def test_open_rejects_manifest_digest_mismatch(self):
        with self.assertRaises(errors.AuthenticationError):
            session(ident=identity(manifest_sha256="1" * 64))

    def test_open_rejects_stale_generation(self):
        with self.assertRaises(errors.StaleGenerationError):
            session(ident=identity(source_generation=2))


class TestSessionReads(TrackedReadersTestCase):
    def test_read_tensor_returns_verified_owned_bytes(self):
        s = session()
        out = s.read_tensor("model.embed_tokens.weight")
        self.assertEqual(
            out.payload, fixtureutil.tensor_payload("model.embed_tokens.weight")
        )
        self.assertEqual(out.tensor.name, "model.embed_tokens.weight")
        self.assertEqual(out.offset, 0)
        self.assertTrue(out.op_id)

    def test_read_tensor_larger_than_window_in_chunks(self):
        # 48-byte tensor against a 32-byte window: succeeds because the
        # owned-output budget, not the window, bounds the tensor.
        s, fake = fake_session(window_bytes=32, owned_output_bytes=4096)
        out = s.read_tensor("model.embed_tokens.weight")
        self.assertEqual(len(out.payload), 48)
        self.assertGreaterEqual(fake.read_count, 2)

    def test_read_tensor_above_owned_budget_refused_before_mutation(self):
        s, fake = fake_session(owned_output_bytes=47)
        with self.assertRaises(errors.CapacityExhaustedError):
            s.read_tensor("model.embed_tokens.weight")
        self.assertEqual(fake.read_count, 0)  # refused before any read

    def test_read_zero_element_tensor_returns_empty_output(self):
        s = session()
        out = s.read_tensor("model.layers.0.attn.bias")
        self.assertEqual(out.payload, b"")
        self.assertEqual(out.op.state.value, "succeeded")

    def test_zero_range_is_complete(self):
        out = session().read_range("model.layers.0.attn.bias", 0, 0)
        self.assertTrue(out.complete)

    def test_unknown_tensor_rejected(self):
        s = session()
        with self.assertRaises(errors.InvalidDescriptorError):
            s.read_tensor("nope")

    def test_read_range_validates_bounds(self):
        s = session()
        fixtureutil.manifest_dict()
        size = 48
        cases = [(-1, 4), (0, size + 1), (size, 1), (10, 2**63)]
        for offset, length in cases:
            with self.subTest(offset=offset, length=length):
                with self.assertRaises(errors.RangeOverflowError):
                    s.read_range("model.embed_tokens.weight", offset, length)

    def test_read_range_zero_length_is_valid(self):
        s = session()
        out = s.read_range("model.embed_tokens.weight", 0, 0)
        self.assertEqual(out.payload, b"")

    def test_read_range_returns_subrange(self):
        s = session()
        out = s.read_range("model.embed_tokens.weight", 8, 16)
        self.assertEqual(
            out.payload,
            fixtureutil.tensor_payload("model.embed_tokens.weight")[8:24],
        )

    def test_full_tensor_digest_verified_on_full_range_read(self):
        s, _fake = fake_session()
        out = s.read_range("model.embed_tokens.weight", 0, 48)
        self.assertEqual(len(out.payload), 48)


class TestSessionOperations(TrackedReadersTestCase):
    def test_every_submitted_operation_gets_terminal_state(self):
        s = session()
        s.read_tensor("model.norm.weight")
        s.read_range("model.norm.weight", 0, 4)
        self.assertEqual(len(s.operations), 2)
        for op in s.operations.values():
            self.assertTrue(op.state.terminal, op.op_id)

    def test_cancel_is_idempotent_on_submitted_operations(self):
        s = session()
        out = s.read_tensor("model.norm.weight")
        s.cancel(out.op_id)
        s.cancel(out.op_id)
        self.assertEqual(s.operations[out.op_id].state.value, "succeeded")

    def test_cancel_unknown_operation_rejected(self):
        s = session()
        with self.assertRaises(errors.OperationStateError):
            s.cancel("missing-op")

    def test_release_is_idempotent_for_terminal_operations(self):
        s = session()
        out = s.read_tensor("model.norm.weight")
        s.release(out.op_id)
        s.release(out.op_id)

    def test_ambiguous_read_retains_operation_as_unknown(self):
        s, fake = fake_session()
        fake.ambiguous = True
        with self.assertRaises(errors.AmbiguousReadError):
            s.read_tensor("model.norm.weight")
        unknown = [op for op in s.operations.values() if op.state.value == "unknown"]
        self.assertEqual(len(unknown), 1)
        self.assertTrue(unknown[0].retained)

    def test_unknown_operation_refuses_release(self):
        s, fake = fake_session()
        fake.ambiguous = True
        with self.assertRaises(errors.AmbiguousReadError):
            s.read_tensor("model.norm.weight")
        op_id = next(
            op.op_id for op in s.operations.values() if op.state.value == "unknown"
        )
        with self.assertRaises(errors.OperationStateError):
            s.release(op_id)

    def test_prior_success_never_unlocks_cleanup_of_later_unknown(self):
        s, fake = fake_session()
        s.read_tensor("model.norm.weight")  # earlier success
        fake.ambiguous = True
        with self.assertRaises(errors.AmbiguousReadError):
            s.read_tensor("model.embed_tokens.weight")
        op_id = next(
            op.op_id for op in s.operations.values() if op.state.value == "unknown"
        )
        with self.assertRaises(errors.OperationStateError):
            s.release(op_id)

    def test_resolved_unknown_operation_releases(self):
        s, fake = fake_session()
        fake.ambiguous = True
        with self.assertRaises(errors.AmbiguousReadError):
            s.read_tensor("model.norm.weight")
        op_id = next(
            op.op_id for op in s.operations.values() if op.state.value == "unknown"
        )
        s.resolve(
            op_id,
            "failed",
            error_code="source-unavailable",
            error_message="fence confirmed loss",
        )
        s.release(op_id)  # must not raise

    def test_close_is_idempotent_and_cancels_pending(self):
        s = session()
        s.read_tensor("model.norm.weight")
        s.close()
        s.close()


class TestVerifiedFlag(TrackedReadersTestCase):
    """Outputs state whether their bytes were digest-verified."""

    def test_read_tensor_output_is_verified(self):
        s = session()
        out = s.read_tensor("model.embed_tokens.weight")
        self.assertTrue(out.complete)

    def test_full_range_output_is_verified(self):
        s = session()
        out = s.read_range("model.embed_tokens.weight", 0, 48)
        self.assertTrue(out.complete)

    def test_partial_range_output_is_unverified(self):
        # A partial range cannot check the whole-tensor digest; the output
        # says so and a consumer may commit only verified outputs.
        s = session()
        out = s.read_range("model.embed_tokens.weight", 8, 16)
        self.assertFalse(out.complete)

    def test_zero_length_range_output_is_unverified(self):
        s = session()
        out = s.read_range("model.embed_tokens.weight", 0, 0)
        self.assertFalse(out.complete)


class TestOperationKind(TrackedReadersTestCase):
    def test_read_tensor_records_read_tensor_kind(self):
        s = session()
        out = s.read_tensor("model.norm.weight")
        self.assertEqual(s.operations[out.op_id].kind, "read_tensor")

    def test_read_range_records_read_range_kind(self):
        s = session()
        out = s.read_range("model.norm.weight", 0, 4)
        self.assertEqual(s.operations[out.op_id].kind, "read_range")


class TestUnclassifiedFailures(TrackedReadersTestCase):
    """A non-contract exception has an unknown outcome: retain as UNKNOWN."""

    def test_unclassified_exception_marks_unknown_and_reraises(self):
        s, fake = fake_session()
        fake.explode = True
        with self.assertRaises(RuntimeError):
            s.read_tensor("model.norm.weight")
        unknown = [op for op in s.operations.values() if op.state.value == "unknown"]
        self.assertEqual(len(unknown), 1)
        self.assertTrue(unknown[0].retained)
        self.assertIn("RuntimeError", unknown[0].error_message)

    def test_retained_slot_wedges_later_reads_by_design(self):
        # The window slot of an UNKNOWN operation stays held, so with one
        # slot the next read fails with capacity exhaustion instead of
        # reusing a window whose outcome is unknown.
        s, fake = fake_session(max_inflight=1)
        fake.explode = True
        with self.assertRaises(RuntimeError):
            s.read_tensor("model.norm.weight")
        fake.explode = False
        with self.assertRaises(errors.CapacityExhaustedError):
            s.read_tensor("model.norm.weight")

    def test_release_refused_for_unclassified_unknown(self):
        s, fake = fake_session()
        fake.explode = True
        with self.assertRaises(RuntimeError):
            s.read_tensor("model.norm.weight")
        op_id = next(
            op.op_id for op in s.operations.values() if op.state.value == "unknown"
        )
        with self.assertRaises(errors.OperationStateError):
            s.release(op_id)


class TestCloseLifecycle(TrackedReadersTestCase):
    def test_close_refuses_while_unknown_retained(self):
        s, fake = fake_session()
        fake.ambiguous = True
        with self.assertRaises(errors.AmbiguousReadError):
            s.read_tensor("model.norm.weight")
        op_id = next(
            op.op_id for op in s.operations.values() if op.state.value == "unknown"
        )
        with self.assertRaises(errors.OperationStateError) as ctx:
            s.close()
        self.assertIn(op_id, str(ctx.exception))
        # The session and reader stay usable after the refused close.
        fake.ambiguous = False
        self.assertTrue(s.read_tensor("model.norm.weight").complete)

    def test_close_succeeds_after_retained_operations_resolve(self):
        s, fake = fake_session()
        fake.ambiguous = True
        with self.assertRaises(errors.AmbiguousReadError):
            s.read_tensor("model.norm.weight")
        op_id = next(
            op.op_id for op in s.operations.values() if op.state.value == "unknown"
        )
        s.resolve(
            op_id,
            "failed",
            error_code="source-unavailable",
            error_message="fence confirmed loss",
        )
        s.close()
        s.close()  # idempotent

    def test_reads_after_close_fail_with_typed_error(self):
        s = session()
        s.close()
        with self.assertRaises(errors.OperationStateError):
            s.read_tensor("model.norm.weight")
        with self.assertRaises(errors.OperationStateError):
            s.read_range("model.norm.weight", 0, 4)


class TestSessionAlignmentAndMaxRequest(TrackedReadersTestCase):
    """Capabilities alignment_bytes and max_request_bytes are enforced on reads."""

    def test_misaligned_range_offset_or_length_rejected_before_read(self):
        from dataclasses import replace

        reader = _track(LocalFileReader(FIXTURE_DIR / "manifest.json"))
        fake = FakeReader(reader.manifest)
        caps = replace(local_backend_capabilities(), alignment_bytes=16)
        s = ReaderSession(
            identity=identity(), capabilities=caps, budgets=budgets(), reader=fake
        )
        with self.assertRaises(errors.InvalidDescriptorError):
            s.read_range("model.embed_tokens.weight", 4, 16)
        with self.assertRaises(errors.InvalidDescriptorError):
            s.read_range("model.embed_tokens.weight", 0, 12)
        self.assertEqual(fake.read_count, 0)
        out = s.read_range("model.embed_tokens.weight", 16, 16)
        self.assertEqual(len(out.payload), 16)
        self.assertGreater(fake.read_count, 0)

    def test_full_tensor_enforces_the_same_alignment_as_range(self):
        from dataclasses import replace

        reader = _track(LocalFileReader(FIXTURE_DIR / "manifest.json"))
        fake = FakeReader(reader.manifest)
        tensor = next(t for t in reader.manifest.tensors if t.byte_size % 16)
        s = ReaderSession(
            identity=identity(),
            capabilities=replace(local_backend_capabilities(), alignment_bytes=16),
            budgets=budgets(),
            reader=fake,
        )
        for read in (
            lambda: s.read_tensor(tensor.name),
            lambda: s.read_range(tensor.name, 0, tensor.byte_size),
        ):
            with self.assertRaises(errors.InvalidDescriptorError):
                read()
        self.assertEqual(fake.read_count, 0)

    def test_request_above_max_request_bytes_rejected_before_read(self):
        from dataclasses import replace

        reader = _track(LocalFileReader(FIXTURE_DIR / "manifest.json"))
        fake = FakeReader(reader.manifest)
        caps = replace(local_backend_capabilities(), max_request_bytes=16)
        s = ReaderSession(
            identity=identity(),
            capabilities=caps,
            budgets=budgets(window_bytes=32),
            reader=fake,
        )
        with self.assertRaises(errors.CapacityExhaustedError):
            s.read_tensor("model.embed_tokens.weight")
        self.assertEqual(fake.read_count, 0)


if __name__ == "__main__":
    unittest.main()
