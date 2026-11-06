"""Backend capabilities: requested, applied, effective, and rejected options."""

import unittest

from gpustack_model_transfer.core import capabilities
from gpustack_model_transfer.core.capabilities import (
    BackendCapabilities,
    RejectedOption,
    TransportSelection,
)


def valid_caps(**overrides):
    fields = {
        "requested": ("ucx", "tcp"),
        "applied": "tcp",
        "effective": "tcp",
        "effective_observed": True,
        "rejected": (RejectedOption("ucx", "no verbs devices"),),
        "formats": ("safetensors",),
        "memory_types": ("host",),
        "max_request_bytes": 1 << 30,
        "alignment_bytes": 4096,
        "tensor_ownership": "owned",
        "device_visibility": ("host",),
        "transport": TransportSelection(
            control="tcp://10.0.0.1:9100",
            payload=("tcp://10.0.0.1:9101",),
        ),
    }
    fields.update(overrides)
    return BackendCapabilities(**fields)


class TestBackendCapabilities(unittest.TestCase):
    def test_valid_capabilities_pass(self):
        valid_caps().validate()  # must not raise

    def test_rejected_option_must_not_be_applied(self):
        # Reporting an option as rejected while also applying it claims a path
        # the backend itself refused; that combination is invalid.
        caps = valid_caps(applied="ucx")
        with self.assertRaises(capabilities.InvalidCapabilitiesError):
            caps.validate()

    def test_rejected_option_must_have_been_requested(self):
        caps = valid_caps(rejected=(RejectedOption("sm", "not requested"),))
        with self.assertRaises(capabilities.InvalidCapabilitiesError):
            caps.validate()

    def test_applied_must_be_one_of_requested(self):
        caps = valid_caps(applied="sm", requested=("ucx", "tcp"))
        with self.assertRaises(capabilities.InvalidCapabilitiesError):
            caps.validate()

    def test_unobserved_effective_backend_must_be_unknown(self):
        # An effective backend that was not independently observed stays
        # "unknown"; it must not silently claim the requested path.
        caps = valid_caps(effective_observed=False, effective="tcp")
        with self.assertRaises(capabilities.InvalidCapabilitiesError):
            caps.validate()
        valid_caps(effective_observed=False, effective="unknown").validate()

    def test_observed_effective_backend_must_be_named(self):
        caps = valid_caps(effective="unknown", effective_observed=True)
        with self.assertRaises(capabilities.InvalidCapabilitiesError):
            caps.validate()

    def test_alignment_must_be_power_of_two(self):
        for alignment in [0, 3, 1000]:
            with self.subTest(alignment=alignment):
                caps = valid_caps(alignment_bytes=alignment)
                with self.assertRaises(capabilities.InvalidCapabilitiesError):
                    caps.validate()

    def test_max_request_bytes_positive(self):
        for bad in [0, True, False]:
            with self.subTest(bad=bad):
                caps = valid_caps(max_request_bytes=bad)
                with self.assertRaises(capabilities.InvalidCapabilitiesError):
                    caps.validate()

    def test_alignment_rejects_booleans(self):
        for bad in [True, False]:
            with self.subTest(bad=bad):
                caps = valid_caps(alignment_bytes=bad)
                with self.assertRaises(capabilities.InvalidCapabilitiesError):
                    caps.validate()

    def test_empty_enumerations_rejected(self):
        for field in ["formats", "memory_types", "device_visibility"]:
            with self.subTest(field=field):
                caps = valid_caps(**{field: ()})
                with self.assertRaises(capabilities.InvalidCapabilitiesError):
                    caps.validate()

    def test_unknown_ownership_rejected(self):
        caps = valid_caps(tensor_ownership="borrowed-without-fence")
        with self.assertRaises(capabilities.InvalidCapabilitiesError):
            caps.validate()

    def test_control_and_payload_selection_are_separate_fields(self):
        caps = valid_caps()
        self.assertEqual(caps.transport.control, "tcp://10.0.0.1:9100")
        self.assertEqual(caps.transport.payload, ("tcp://10.0.0.1:9101",))


class TestLocalBackendCapabilities(unittest.TestCase):
    def test_local_backend_shape(self):
        caps = capabilities.local_backend_capabilities()
        caps.validate()
        self.assertEqual(caps.requested, ("local",))
        self.assertEqual(caps.applied, "local")
        self.assertEqual(caps.effective, "local")
        self.assertTrue(caps.effective_observed)
        self.assertEqual(caps.rejected, ())
        self.assertEqual(caps.formats, ("safetensors",))
        self.assertEqual(caps.memory_types, ("host",))
        self.assertEqual(caps.tensor_ownership, "owned")
        # The local reference path needs no vendor runtime.
        self.assertEqual(caps.device_visibility, ("host",))
        self.assertGreaterEqual(caps.max_request_bytes, 1)


if __name__ == "__main__":
    unittest.main()
