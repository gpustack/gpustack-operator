"""The native vLLM-Ascend load path: budgets, NPU fences, tying, finalization."""

import tempfile
import unittest

import ascendutil as fixtureutil

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
    from gpustack_model_transfer.adapters.vllm_ascend import ModelTransferAscendLoader

    counting = reader if reader is not None else _CountingReader()
    return ModelTransferAscendLoader(
        reader=counting,
        capabilities=capabilities or local_backend_capabilities(),
        budgets=budgets or default_budgets(),
    ), counting


def make_driver(**kwargs):
    from gpustack_model_transfer.adapters.vllm_ascend.runtime import ReferenceRuntime

    return ReferenceRuntime(fixtureutil.manifest_dir(), **kwargs)


def ascend_contract(events=None, release_result=True):
    from gpustack_model_transfer.adapters.vllm_ascend import BorrowedContract
    from gpustack_model_transfer.adapters.vllm_ascend.constants import (
        DEFAULT_VISIBILITY_STREAM,
    )

    def fence(name):
        if events is not None:
            events.append(f"fence:{name}")

    def release(name):
        if events is not None:
            events.append(f"release:{name}")

        return release_result

    return BorrowedContract(
        stream=DEFAULT_VISIBILITY_STREAM, visibility_fence=fence, release=release
    )


class TestMultiWindowAssembly(unittest.TestCase):
    def test_tensors_above_one_window_assemble_byte_exact(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

        loader, counting = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())

        # lm_head.weight is 48 bytes and q_proj is 36: both span more than
        # one 32-byte window, and every window is bounded by the budget.
        self.assertGreater(counting.window_count(), 1)
        for length in counting.reads:
            self.assertLessEqual(length, 32, "a read exceeded the window")
        for name in fixtureutil.parameter_names():
            self.assertEqual(
                driver.model_parameter_bytes(report.model, name),
                fixtureutil.tensor_payload(name),
                name,
            )

    def test_preflight_refusal_reads_nothing_and_closes_the_reader(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

        tight = ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=47)
        loader, counting = make_loader(budgets=tight)
        driver = make_driver()
        with self.assertRaises(CapacityExhaustedError):
            loader.load(driver, LoadOptions())
        self.assertEqual(counting.reads, [], "the over-budget refusal made zero reads")
        self.assertFalse(driver.built, "refusal built or mutated a model")
        self.assertEqual(
            counting.close_calls, 1, "a pre-session refusal still closes the reader"
        )

    def test_refusal_names_the_offender(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

        tight = ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=47)
        loader, _ = make_loader(budgets=tight)
        with self.assertRaises(CapacityExhaustedError) as caught:
            loader.load(make_driver(), LoadOptions())
        self.assertIn("lm_head.weight", str(caught.exception))

    def test_every_loaded_output_was_digest_verified(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertTrue(report.manifest_consumed)
        self.assertEqual(
            len(report.loaded_parameters), len(fixtureutil.parameter_names())
        )


class TestDtypesAndTying(unittest.TestCase):
    def test_zero_element_tensor_and_engine_metadata_handled(self):
        import struct

        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertEqual(fixtureutil.tensor_payload("model.layers.0.attn.bias"), b"")
        self.assertEqual(
            driver.model_parameter_bytes(report.model, "model.layers.0.attn.bias"), b""
        )
        self.assertIn("metadata.step", report.engine_metadata)
        (expected,) = struct.unpack("<q", fixtureutil.tensor_payload("metadata.step"))
        self.assertEqual(report.engine_metadata["metadata.step"], expected)
        self.assertNotIn("metadata.step", report.loaded_parameters)

    def test_tied_embeddings_share_one_storage_without_double_counting(self):
        from gpustack_model_transfer.adapters.vllm_ascend import (
            LoadOptions,
            TensorOwnership,
        )

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
        # The head is an alias of the embedding: the ledger counts the
        # shared storage once, not once per name.
        expected_retained = sum(
            len(fixtureutil.tensor_payload(name))
            for name in fixtureutil.parameter_names()
            if name != "lm_head.weight"
        )
        self.assertEqual(report.ledger.retained_parameter_bytes, expected_retained)
        self.assertEqual(report.plan.total_parameter_bytes, expected_retained)

    def test_tied_checkpoint_may_omit_lm_head_entirely(self):
        import hashlib

        from gpustack_model_transfer.adapters.vllm_ascend import (
            LoadOptions,
            ModelTransferAscendLoader,
            TensorOwnership,
        )

        with tempfile.TemporaryDirectory() as tmp:
            root = fixtureutil.write_tied_fixture_without_lm_head(tmp)
            manifest = root / "manifest.json"
            reader = LocalFileReader(
                manifest,
                expected_sha256=hashlib.sha256(manifest.read_bytes()).hexdigest(),
            )
            loader = ModelTransferAscendLoader(
                reader=reader,
                capabilities=local_backend_capabilities(),
                budgets=default_budgets(),
            )
            driver = make_driver(tie_word_embeddings=True)
            report = loader.load(
                driver,
                LoadOptions(ownership=TensorOwnership.OWNED, tie_word_embeddings=True),
            )

            plan_names = [entry.tensor.name for entry in report.plan.entries]
            self.assertNotIn("lm_head.weight", plan_names)
            self.assertEqual(report.tied_aliases, ("lm_head.weight",))
            self.assertTrue(driver.alias_tied(report.model))
            self.assertEqual(
                driver.model_parameter_bytes(report.model, "model.embed_tokens.weight"),
                fixtureutil.tensor_payload("model.embed_tokens.weight"),
            )
            self.assertEqual(
                driver.model_parameter_bytes(report.model, "lm_head.weight"),
                fixtureutil.tensor_payload("model.embed_tokens.weight"),
            )
            self.assertTrue(report.manifest_consumed)

    def test_untied_embeddings_are_distinct_without_leakage(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

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
    def test_ascend_finalization_runs_exactly_once(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

        loader, _ = make_loader()
        driver = make_driver()
        report = loader.load(driver, LoadOptions())
        self.assertEqual(report.finalization_count, 1)
        self.assertEqual(driver.model_finalization_count(report.model), 1)
        # The Ascend finalization hooks ran, and only once.
        self.assertEqual(driver.finalization_hooks, ["contiguous", "cast", "format"])

    def test_failure_midway_taints_the_model(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions

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
        self.assertEqual(counting.close_calls, 1)


class TestBorrowedStorage(unittest.TestCase):
    def test_borrowed_refused_without_backend_support(self):
        from gpustack_model_transfer.adapters.vllm_ascend import (
            BorrowedContract,
            LoadOptions,
            TensorOwnership,
        )

        loader, _ = make_loader()
        contract = BorrowedContract(
            stream="npu:0",
            visibility_fence=lambda name: None,
            release=lambda name: True,
        )
        with self.assertRaises(InvalidDescriptorError):
            loader.load(
                make_driver(),
                LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=contract),
            )

    def test_borrowed_refused_without_an_explicit_contract(self):
        from gpustack_model_transfer.adapters.vllm_ascend import (
            LoadOptions,
            TensorOwnership,
        )

        loader, _ = make_loader(capabilities=borrowed_capabilities())
        with self.assertRaises(InvalidDescriptorError):
            loader.load(
                make_driver(),
                LoadOptions(ownership=TensorOwnership.BORROWED, borrowed=None),
            )

    def test_borrowed_bytes_become_visible_on_the_npu_stream_before_release(self):
        from gpustack_model_transfer.adapters.vllm_ascend import (
            LoadOptions,
            TensorOwnership,
        )

        events = []
        contract = ascend_contract(events=events)
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
            self.assertIn(f"fence:{name}", events)
            self.assertIn(f"release:{name}", events)
            self.assertLess(
                events.index(f"store:{name}"),
                events.index(f"fence:{name}"),
                "the stream fence must follow the store it makes visible",
            )
            self.assertLess(
                events.index(f"fence:{name}"), events.index(f"release:{name}")
            )

    def test_retaining_consumer_copied_to_owned(self):
        from gpustack_model_transfer.adapters.vllm_ascend import (
            LoadOptions,
            TensorOwnership,
        )

        contract = ascend_contract(release_result=False)
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


class TestForwardEquivalence(unittest.TestCase):
    def test_forward_output_matches_the_native_reference(self):
        from gpustack_model_transfer.adapters.vllm_ascend import LoadOptions
        from gpustack_model_transfer.adapters.vllm_ascend.tolerances import (
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
        self.assertTrue(
            all(a == b for a, b in zip(loaded, expected, strict=False)),
            "identical arithmetic on identical bytes must agree",
        )

    def test_a_perturbed_reference_is_not_within_tolerance(self):
        from gpustack_model_transfer.adapters.vllm_ascend.tolerances import (
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


class TestReferenceProbeAndNpuFixture(unittest.TestCase):
    def test_a_device_byte_probe_is_not_a_model_load(self):
        # A torch_npu byte probe answers "is there device memory"; it has
        # never loaded a model. The reference fixture gate has its own
        # bookkeeping; neither outcome proves native engine loading.
        from gpustack_model_transfer.adapters.vllm_ascend import ResourceLedger
        from gpustack_model_transfer.adapters.vllm_ascend.npu_runtime import (
            HostReadiness,
        )
        from gpustack_model_transfer.adapters.vllm_ascend.runtime import ModelHandle
        from gpustack_model_transfer.core.errors import OperationStateError

        readiness = HostReadiness(
            torch_available=True,
            torch_npu_available=True,
            vllm_ascend_available=True,
            npu_available=True,
            missing=(),
        )
        self.assertTrue(readiness.ready, "the probe answers a readiness question")

        probed_only = ModelHandle(built_by="torch_npu-byte-probe")
        ledger = ResourceLedger()
        with self.assertRaises(OperationStateError):
            loader_profile(probed_only, ledger)

        # The separate reference fixture has the bookkeeping this fixture
        # gate expects, without establishing native engine finalization.
        driver = make_driver()
        native = driver.build_native_model()
        self.assertEqual(native.finalization_count, 1)
        loader_profile(native, ledger)

    def test_npu_runtime_stays_lazy_and_refuses_without_its_stack(self):
        from gpustack_model_transfer.adapters.vllm_ascend.npu_runtime import (
            NPURuntimeDriver,
            probe_host_readiness,
        )

        readiness = probe_host_readiness()
        self.assertFalse(
            readiness.ready,
            "this CPU checkout has no torch_npu stack, and the probe says so",
        )
        driver = NPURuntimeDriver()
        with self.assertRaises(Exception) as caught:
            driver.build_model(plan=None, options=None)
        self.assertIn(
            "required on an Ascend host",
            str(caught.exception),
            "the driver refuses and names the missing piece of the stack",
        )


def loader_profile(model, ledger):
    from gpustack_model_transfer.adapters.vllm_ascend import profile_kv_capacity

    class _Passthrough:
        def profile_kv_capacity(self, model):
            return 1

    return profile_kv_capacity(_Passthrough(), model, ledger)


if __name__ == "__main__":
    unittest.main()
