"""The native vLLM load path: windows, budgets, tying, finalization, borrowed."""

import unittest

import fixtureutil

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


class _CountingReader:
    """Counts reads so refusal-before-read and window shapes are observable."""

    def __init__(self):
        self._inner = LocalFileReader(
            fixtureutil.manifest_path(), expected_sha256=fixtureutil.manifest_digest()
        )
        self.manifest = self._inner.manifest
        self.reads = []
        self.close_calls = 0

    def read_bytes(self, file_path, begin, end):
        self.reads.append(end - begin)
        return self._inner.read_bytes(file_path, begin, end)

    def window_count(self) -> int:
        return len(self.reads)

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
    from gpustack_model_transfer.adapters.vllm import ModelTransferModelLoader

    counting = reader if reader is not None else _CountingReader()
    return ModelTransferModelLoader(
        reader=counting,
        capabilities=capabilities or local_backend_capabilities(),
        budgets=budgets or default_budgets(),
    ), counting


def make_driver(**kwargs):
    from gpustack_model_transfer.adapters.vllm.runtime import ReferenceRuntime

    return ReferenceRuntime(fixtureutil.manifest_dir(), **kwargs)


class TestMultiWindowAssembly(unittest.TestCase):
    def test_tensors_above_one_window_assemble_byte_exact(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, counting = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())

        # lm_head.weight is 48 bytes and q_proj is 36: both span more than
        # one 32-byte window, and every window is bounded by the budget.
        windowed = [
            name
            for name in (
                "lm_head.weight",
                "model.layers.0.attn.q_proj.weight",
                "model.embed_tokens.weight",
            )
            if fixtureutil.tensor_entry(name)["data_end"]
            - fixtureutil.tensor_entry(name)["data_begin"]
            > 32
        ]
        self.assertTrue(windowed)
        self.assertGreater(counting.window_count(), 1)
        for length in counting.reads:
            self.assertLessEqual(length, 32, "a read exceeded the window")
        for name in fixtureutil.parameter_names():
            self.assertEqual(
                driver.model_parameter_bytes(report.model, name),
                fixtureutil.tensor_payload(name),
                name,
            )

    def test_every_loaded_output_was_digest_verified(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertTrue(report.manifest_consumed)
        self.assertEqual(
            len(report.loaded_parameters), len(fixtureutil.parameter_names())
        )

    def test_loader_closes_the_reader_after_the_load(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, counting = make_loader()
        loader.load(make_driver(), LoadOptions())
        self.assertEqual(
            counting.close_calls, 1, "the loader owns the reader for the load duration"
        )


class TestOwnedBudgetRefusal(unittest.TestCase):
    def test_tensor_above_the_owned_budget_refused_before_any_read(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        tight = ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=47)
        loader, counting = make_loader(budgets=tight)
        driver = make_driver()
        with self.assertRaises(CapacityExhaustedError):
            loader.load(driver, LoadOptions())
        self.assertEqual(counting.reads, [], "refusal read checkpoint bytes")
        self.assertFalse(driver.built, "refusal built or mutated a model")
        self.assertEqual(counting.close_calls, 1, "refusal must still close the reader")

    def test_refusal_names_the_offender(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        tight = ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=47)
        loader, _ = make_loader(budgets=tight)
        with self.assertRaises(CapacityExhaustedError) as caught:
            loader.load(make_driver(), LoadOptions())
        self.assertIn("lm_head.weight", str(caught.exception))


class TestDtypesAndTying(unittest.TestCase):
    def test_zero_element_tensor_and_engine_metadata_handled(self):
        import struct

        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        bias = fixtureutil.tensor_payload("model.layers.0.attn.bias")
        self.assertEqual(bias, b"")
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "model.layers.0.attn.bias"), b""
        )
        self.assertIn("metadata.step", report.engine_metadata)
        (expected,) = struct.unpack("<q", fixtureutil.tensor_payload("metadata.step"))
        self.assertEqual(report.engine_metadata["metadata.step"], expected)
        self.assertNotIn("metadata.step", report.loaded_parameters)

    def test_tied_embeddings_share_one_storage(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions, TensorOwnership

        loader, _ = make_loader()
        driver = make_driver(tie_word_embeddings=True)
        report = loader.load(
            driver,
            LoadOptions(ownership=TensorOwnership.OWNED, tie_word_embeddings=True),
        )
        self.assertEqual(report.tied_aliases, ("lm_head.weight",))
        self.assertTrue(driver.alias_tied(report.model))
        # Manifest order is deterministic: embed_tokens is loaded after
        # lm_head, so the shared storage ends with the embed bytes.
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "model.embed_tokens.weight"),
            fixtureutil.tensor_payload("model.embed_tokens.weight"),
        )
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "lm_head.weight"),
            fixtureutil.tensor_payload("model.embed_tokens.weight"),
        )
        expected_retained = sum(
            len(fixtureutil.tensor_payload(name))
            for name in fixtureutil.parameter_names()
            if name != "lm_head.weight"
        )
        self.assertEqual(report.ledger.retained_parameter_bytes, expected_retained)
        self.assertEqual(report.plan.total_parameter_bytes, expected_retained)

    def test_untied_embeddings_are_distinct_without_leakage(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, _ = make_loader()
        driver = make_driver(tie_word_embeddings=False)
        report = loader.load(driver, LoadOptions())
        self.assertEqual(report.tied_aliases, ())
        self.assertFalse(driver.alias_tied(report.model))
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "lm_head.weight"),
            fixtureutil.tensor_payload("lm_head.weight"),
        )
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "model.embed_tokens.weight"),
            fixtureutil.tensor_payload("model.embed_tokens.weight"),
        )


class TestFinalization(unittest.TestCase):
    def test_finalization_runs_exactly_once(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertEqual(report.finalization_count, 1)
        self.assertEqual(driver.model_finalization_count(report.model), 1)

    def test_cleanup_failure_preserves_original_load_failure(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        class CloseFailure(_CountingReader):
            fail_close = True

            def close(self):
                super().close()
                if self.fail_close:
                    raise RuntimeError("cleanup failed")

        counting = CloseFailure()
        loader, _ = make_loader(reader=counting)
        driver = make_driver(
            fail_store_after=0, store_error=DeniedContentError("engine refused")
        )
        with self.assertRaisesRegex(DeniedContentError, "engine refused"):
            loader.load(driver, LoadOptions())
        self.assertEqual(counting.close_calls, 1)
        self.assertIsNotNone(loader.retained_session)
        from gpustack_model_transfer.core.errors import OperationStateError

        with self.assertRaises(OperationStateError):
            loader.load(driver, LoadOptions())
        retained = loader.retained_session
        with self.assertRaisesRegex(RuntimeError, "cleanup failed"):
            loader.close()
        self.assertIs(loader.retained_session, retained)
        counting.fail_close = False
        loader.close()
        self.assertIsNone(loader.retained_session)

    def test_failure_midway_taints_the_model(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions

        loader, counting = make_loader()
        driver = make_driver(
            fail_store_after=0, store_error=DeniedContentError("engine refused")
        )
        with self.assertRaises(DeniedContentError):
            loader.load(driver, LoadOptions())
        self.assertTrue(
            driver.model_mutated(driver.last_model),
            "a partially mutated model must be marked unsafe",
        )
        self.assertEqual(driver.model_finalization_count(driver.last_model), 0)
        # The read completed and failed cleanly, so the loader could close.
        self.assertEqual(counting.close_calls, 1)


class TestBorrowedStorage(unittest.TestCase):
    def test_borrowed_refused_without_backend_support(self):
        from gpustack_model_transfer.adapters.vllm import (
            BorrowedContract,
            LoadOptions,
            TensorOwnership,
        )

        loader, _ = make_loader()
        contract = BorrowedContract(
            stream="cuda:0",
            visibility_fence=lambda name: None,
            release=lambda name: True,
        )
        with self.assertRaises(InvalidDescriptorError):
            loader.load(
                make_driver(),
                LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract),
            )

    def test_borrowed_refused_without_an_explicit_contract(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions, TensorOwnership

        loader, _ = make_loader(capabilities=borrowed_capabilities())
        with self.assertRaises(InvalidDescriptorError):
            loader.load(
                make_driver(),
                LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=None),
            )

    def test_borrowed_window_visible_and_released_in_order(self):
        from gpustack_model_transfer.adapters.vllm import (
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
        loader, _counting = make_loader(capabilities=borrowed_capabilities())
        driver = make_driver(record_events=events)
        report = loader.load(
            driver, LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract)
        )
        self.assertEqual(report.borrowed_fallbacks, ())
        stores = [e for e in events if e.startswith("store:")]
        self.assertTrue(stores)
        for event in stores:
            name = event.split(":", 1)[1]
            self.assertIn(f"fence:{name}", events)
            self.assertIn(f"release:{name}", events)
            self.assertLess(
                events.index(f"store:{name}"),
                events.index(f"fence:{name}"),
                "fence before the bytes are stored is meaningless",
            )
            self.assertLess(
                events.index(f"fence:{name}"), events.index(f"release:{name}")
            )

    def test_retaining_consumer_copied_to_owned(self):
        from gpustack_model_transfer.adapters.vllm import (
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
        driver = make_driver()
        report = loader.load(
            driver, LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract)
        )
        self.assertEqual(
            set(report.borrowed_fallbacks),
            set(fixtureutil.parameter_names()),
            "every retained borrowed tensor was copied to owned",
        )
        for name in fixtureutil.parameter_names():
            self.assertEqual(
                driver.model_parameter_bytes(report.model, name),
                fixtureutil.tensor_payload(name),
            )

        retaining_driver = make_driver(consumer_retains=True)
        releasing_contract = BorrowedContract(
            stream="cuda:0",
            visibility_fence=lambda name: None,
            release=lambda name: True,
        )
        loader2, _ = make_loader(capabilities=borrowed_capabilities())
        report2 = loader2.load(
            retaining_driver,
            LoadOptions(
                ownership=TensorOwnership.BORROWED, borrowed=releasing_contract
            ),
        )
        self.assertEqual(
            set(report2.borrowed_fallbacks), set(fixtureutil.parameter_names())
        )
        for name in fixtureutil.parameter_names():
            self.assertEqual(
                retaining_driver.model_parameter_bytes(report2.model, name),
                fixtureutil.tensor_payload(name),
            )


class TestForwardEquivalence(unittest.TestCase):
    def test_forward_output_matches_the_native_reference(self):
        from gpustack_model_transfer.adapters.vllm import LoadOptions
        from gpustack_model_transfer.adapters.vllm.tolerances import (
            DEFAULT_FORWARD_TOLERANCE,
            DEFAULT_PARAMETER_TOLERANCE,
        )

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        native = make_driver().build_native_model()
        for name in fixtureutil.parameter_names():
            self.assertTrue(
                DEFAULT_PARAMETER_TOLERANCE.within(
                    driver.parameter_values(report.model, name),
                    driver.parameter_values(native, name),
                ),
                name,
            )
        inputs = [1, 2]
        loaded = driver.forward(report.model, inputs)
        expected = driver.forward(native, inputs)
        self.assertTrue(DEFAULT_FORWARD_TOLERANCE.within(loaded, expected))
        self.assertEqual(len(loaded), len(expected))
        self.assertTrue(
            all(a == b for a, b in zip(loaded, expected, strict=False)),
            "identical arithmetic on identical bytes must agree",
        )

    def test_a_perturbed_reference_is_not_within_tolerance(self):
        from gpustack_model_transfer.adapters.vllm.tolerances import (
            DEFAULT_FORWARD_TOLERANCE,
        )

        driver = make_driver()
        native = driver.build_native_model()
        driver.perturb_parameter(native, "model.norm.weight")
        perturbed = driver.forward(native, [1, 2])
        plain = driver.forward(driver.build_native_model(), [1, 2])
        self.assertFalse(
            DEFAULT_FORWARD_TOLERANCE.within(perturbed, plain),
            "the comparison must be able to fail",
        )


if __name__ == "__main__":
    unittest.main()


class TestTiedAliasBudget(unittest.TestCase):
    def test_unused_head_bytes_do_not_refuse_the_plan(self):
        import importlib
        from dataclasses import replace
        from types import SimpleNamespace

        reader = _CountingReader()
        self.addCleanup(reader.close)
        head = next(t for t in reader.manifest.tensors if t.name == "lm_head.weight")
        tensors = tuple(
            replace(t, data_end=t.data_begin + 8192, shape=(2048,)) if t is head else t
            for t in reader.manifest.tensors
        )
        manifest = SimpleNamespace(tensors=tensors)
        for engine in ("vllm", "sglang", "vllm_ascend"):
            with self.subTest(engine=engine):
                build = importlib.import_module(
                    f"gpustack_model_transfer.adapters.{engine}.loader"
                ).build_load_plan
                plan = build(
                    manifest,
                    default_budgets(),
                    local_backend_capabilities(),
                    tie_word_embeddings=True,
                )
                self.assertTrue(plan.tied_aliases)
