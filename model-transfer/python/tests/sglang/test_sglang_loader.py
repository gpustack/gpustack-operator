"""The SGLang load path: preflight order, tying, borrowed, finalization."""

import unittest

import sglangutil as fixtureutil

from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import (
    BackendCapabilities,
    TransportSelection,
    local_backend_capabilities,
)
from gpustack_model_transfer.core.errors import (
    CapacityExhaustedError,
    DeniedContentError,
    InvalidDescriptorError,
)
from gpustack_model_transfer.core.local_reader import LocalFileReader


class CountingReader:
    def __init__(self, path=None, digest=None):
        self._inner = LocalFileReader(
            path or fixtureutil.manifest_path(),
            expected_sha256=digest if digest is not None else None,
        )
        self.manifest = self._inner.manifest
        self.reads = []
        self.close_calls = 0

    def read_bytes(self, file_path, begin, end):
        self.reads.append(end - begin)
        return self._inner.read_bytes(file_path, begin, end)

    def close(self):
        self.close_calls += 1
        self._inner.close()


def default_budgets():
    return ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096)


def borrowed_capabilities():
    base = local_backend_capabilities()
    return BackendCapabilities(
        requested=base.requested,
        applied=base.applied,
        effective=base.effective,
        effective_observed=base.effective_observed,
        rejected=base.rejected,
        formats=base.formats,
        memory_types=base.memory_types,
        max_request_bytes=base.max_request_bytes,
        alignment_bytes=base.alignment_bytes,
        tensor_ownership="borrowed",
        device_visibility=base.device_visibility,
        transport=TransportSelection(
            control=base.transport.control, payload=base.transport.payload
        ),
    )


def make_loader(reader=None, budgets=None, capabilities=None):
    from gpustack_model_transfer.adapters.sglang import ModelTransferSGLangLoader

    counting = reader if reader is not None else CountingReader()
    return ModelTransferSGLangLoader(
        reader=counting,
        capabilities=capabilities or local_backend_capabilities(),
        budgets=budgets or default_budgets(),
    ), counting


def make_driver(**kwargs):
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

    return ReferenceRuntime(fixtureutil.manifest_dir(), **kwargs)


class TestPreflightOrderAndReaderLifetime(unittest.TestCase):
    def test_over_budget_refusal_reads_nothing_and_closes_the_reader(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions

        probe = CountingReader()
        try:
            largest = max(
                (t for t in probe.manifest.tensors),
                key=lambda t: t.byte_size,
            )
        finally:
            probe.close()
        tight = ReadBudgets(
            window_bytes=32, max_inflight=2, owned_output_bytes=largest.byte_size - 1
        )
        loader, counting = make_loader(budgets=tight)
        driver = make_driver()
        with self.assertRaises(CapacityExhaustedError):
            loader.load(driver, LoadOptions())
        self.assertEqual(counting.reads, [], "the plan preflight must read nothing")
        self.assertFalse(driver.built)
        self.assertEqual(counting.close_calls, 1, "refusal must close the reader")

    def test_probe_failure_closes_the_reader(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang import loader as loader_module

        loader, counting = make_loader()
        original = loader_module.probe_checkpoint_metadata

        def broken(*args, **kwargs):
            raise InvalidDescriptorError("header refused")

        loader_module.probe_checkpoint_metadata = broken
        try:
            with self.assertRaises(InvalidDescriptorError):
                loader.load(make_driver(), LoadOptions())
        finally:
            loader_module.probe_checkpoint_metadata = original
        self.assertEqual(counting.close_calls, 1)
        self.assertEqual(counting.reads, [])

    def test_invalid_options_close_the_reader_before_anything(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions

        loader, counting = make_loader()
        with self.assertRaises(ValueError):
            loader.load(make_driver(), LoadOptions(tie_word_embeddings="yes"))
        self.assertEqual(counting.close_calls, 1)
        self.assertEqual(counting.reads, [])

    def test_borrowed_without_support_or_contract_closes_the_reader(self):
        from gpustack_model_transfer.adapters.sglang import (
            BorrowedContract,
            LoadOptions,
            TensorOwnership,
        )

        contract = BorrowedContract(
            stream="cuda:0",
            visibility_fence=lambda name: None,
            release=lambda name: True,
        )
        loader, counting = make_loader()
        with self.assertRaises(InvalidDescriptorError):
            loader.load(
                make_driver(),
                LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract),
            )
        self.assertEqual(counting.close_calls, 1)


class TestLoadPath(unittest.TestCase):
    def test_single_rank_load_is_byte_exact_and_verified(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions

        loader, counting = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertTrue(report.manifest_consumed)
        self.assertEqual(report.finalization_count, 1)
        self.assertTrue(report.session_released)
        self.assertEqual(report.partial_shard_reads, ())
        for length in counting.reads:
            self.assertLessEqual(length, 32)
        for name in fixtureutil.parameter_names():
            self.assertEqual(
                driver.model_parameter_bytes(report.model, name),
                fixtureutil.tensor_payload(name),
                name,
            )

    def test_tied_embeddings_without_double_counting(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions

        loader, _ = make_loader()
        driver = make_driver(tie_word_embeddings=True)
        report = loader.load(driver, LoadOptions(tie_word_embeddings=True))
        self.assertEqual(report.tied_aliases, ("lm_head.weight",))
        self.assertTrue(driver.alias_tied(report.model))
        expected = sum(
            len(fixtureutil.tensor_payload(name))
            for name in fixtureutil.parameter_names()
            if name != "lm_head.weight"
        )
        self.assertEqual(report.ledger.retained_parameter_bytes, expected)
        self.assertEqual(report.plan.total_parameter_bytes, expected)

    def test_tied_checkpoint_omitting_lm_head_is_supported(self):
        import tempfile

        from gpustack_model_transfer.adapters.sglang import (
            LoadOptions,
            ModelTransferSGLangLoader,
        )
        from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

        with tempfile.TemporaryDirectory() as tmp:
            # A tied checkpoint may legitimately omit lm_head.weight: the
            # alias points at the embedding and nothing is missing.
            root = fixtureutil.tied_manifest_dir(tmp, drop_lm_head=True)
            reader = CountingReader(path=str(root / "manifest.json"))
            driver = ReferenceRuntime(str(root), tie_word_embeddings=True)
            loader = ModelTransferSGLangLoader(
                reader=reader,
                capabilities=local_backend_capabilities(),
                budgets=default_budgets(),
            )
            report = loader.load(driver, LoadOptions(tie_word_embeddings=True))
            self.assertEqual(report.tied_aliases, ("lm_head.weight",))
            self.assertTrue(driver.alias_tied(report.model))
            self.assertNotIn("lm_head.weight", report.loaded_parameters)
            self.assertEqual(
                driver.model_parameter_bytes(report.model, "lm_head.weight"),
                fixtureutil.tensor_payload("model.embed_tokens.weight"),
            )

    def test_engine_metadata_and_zero_element_tensor(self):
        import struct

        from gpustack_model_transfer.adapters.sglang import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "model.layers.0.attn.bias"),
            b"",
        )
        (step,) = struct.unpack("<q", fixtureutil.tensor_payload("metadata.step"))
        self.assertEqual(report.engine_metadata["metadata.step"], step)
        self.assertNotIn("metadata.step", report.loaded_parameters)

    def test_mid_load_failure_taints_and_does_not_finalize(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions

        loader, counting = make_loader()
        driver = make_driver(
            fail_store_after=0, store_error=DeniedContentError("engine refused")
        )
        with self.assertRaises(DeniedContentError):
            loader.load(driver, LoadOptions())
        self.assertTrue(driver.model_mutated(driver.last_model))
        self.assertEqual(driver.model_finalization_count(driver.last_model), 0)
        self.assertEqual(counting.close_calls, 1)


class TestBorrowedStorage(unittest.TestCase):
    def test_borrowed_fence_release_order_and_fallback(self):
        from gpustack_model_transfer.adapters.sglang import (
            BorrowedContract,
            LoadOptions,
            TensorOwnership,
        )

        events = []
        contract = BorrowedContract(
            stream="cuda:0",
            visibility_fence=lambda name: events.append(f"fence:{name}"),
            release=lambda name: events.append(f"release:{name}") or True,
        )
        loader, _ = make_loader(capabilities=borrowed_capabilities())
        driver = make_driver(record_events=events)
        report = loader.load(
            driver, LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract)
        )
        self.assertEqual(report.borrowed_fallbacks, ())
        stores = [e for e in events if e.startswith("store:")]
        self.assertTrue(stores)
        for event in stores:
            name = event.split(":", 1)[1]
            self.assertLess(events.index(event), events.index(f"fence:{name}"))
            self.assertLess(
                events.index(f"fence:{name}"), events.index(f"release:{name}")
            )

    def test_retaining_consumer_is_copied_to_owned(self):
        from gpustack_model_transfer.adapters.sglang import (
            BorrowedContract,
            LoadOptions,
            TensorOwnership,
        )

        contract = BorrowedContract(
            stream="cuda:0",
            visibility_fence=lambda name: None,
            release=lambda name: False,
        )
        loader, _ = make_loader(capabilities=borrowed_capabilities())
        driver = make_driver(consumer_retains=True)
        report = loader.load(
            driver, LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract)
        )
        self.assertEqual(
            set(report.borrowed_fallbacks), set(fixtureutil.parameter_names())
        )
        for name in fixtureutil.parameter_names():
            self.assertEqual(
                driver.model_parameter_bytes(report.model, name),
                fixtureutil.tensor_payload(name),
            )


if __name__ == "__main__":
    unittest.main()


class TestIncompleteWholeTensorRefused(unittest.TestCase):
    def test_incomplete_whole_tensor_output_is_refused_before_mutation(self):
        """Only complete outputs may be committed; whole-tensor reads that
        come back incomplete refuse the load before any parameter moves."""
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang import loader as loader_module

        loader, _counting = make_loader()
        driver = make_driver()
        original = loader_module.ReaderSession

        class IncompleteSession:
            def __init__(self, *args, **kwargs):
                self._real = original(*args, **kwargs)

            def read_tensor(self, name):
                output = self._real.read_tensor(name)
                # Strip the verification the session earned, the way a
                # backend that skipped the digest check would.
                object.__setattr__(output, "complete", False)
                return output

            def read_range(self, name, offset, length):
                return self._real.read_range(name, offset, length)

            def close(self):
                self._real.close()

        loader_module.ReaderSession = IncompleteSession
        try:
            with self.assertRaises(InvalidDescriptorError):
                loader.load(driver, LoadOptions())
        finally:
            loader_module.ReaderSession = original
        # No parameter store ran: the refusal preceded every mutation. The
        # built model is tainted by the failure, which is the contract.
        self.assertEqual(driver.stores_done, 0)
        self.assertIsNotNone(driver.last_model)
        self.assertTrue(
            driver.last_model.mutated, "a failed load leaves the model tainted"
        )
        self.assertEqual(driver.last_model.finalization_count, 0)

    def test_tied_alias_entry_reads_no_head_bytes(self):
        """The tied alias is the embedding's storage: its bytes are never
        read, staged, or counted."""
        from gpustack_model_transfer.adapters.sglang import LoadOptions

        loader, counting = make_loader()
        driver = make_driver(tie_word_embeddings=True)
        report = loader.load(driver, LoadOptions(tie_word_embeddings=True))
        self.assertEqual(report.tied_aliases, ("lm_head.weight",))
        # The head's bytes never crossed the reader: every read is
        # accounted for by non-head tensors, asserted by total size since
        # window boundaries vary.
        total_read = sum(counting.reads)
        expected = sum(
            len(fixtureutil.tensor_payload(name))
            for name in fixtureutil.parameter_names()
            if name != "lm_head.weight"
        )
        # Header probe bytes and the engine-metadata scalars are also
        # read; the payload share must exclude the head's 48 bytes exactly.
        engine_metadata_bytes = sum(
            len(fixtureutil.tensor_payload(name)) for name in ("metadata.step",)
        )
        self.assertEqual(
            total_read - expected - engine_metadata_bytes,
            _header_bytes_read(),
            "the tied head's payload bytes were read anyway",
        )
        self.assertEqual(
            report.ledger.retained_parameter_bytes,
            expected,
        )


def _header_bytes_read() -> int:
    """The fixture's safetensors header span: 8-byte prefix plus JSON."""
    blob = fixtureutil.blob_bytes()
    return 8 + int.from_bytes(blob[:8], "little")
