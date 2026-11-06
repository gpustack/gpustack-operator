"""The pinned SGLang route: constants, image metadata, lazy imports."""

import subprocess
import sys
import unittest


class TestPinnedRoute(unittest.TestCase):
    def test_load_format_and_loader_class_are_pinned(self):
        from gpustack_model_transfer.adapters.sglang import (
            LOADER_CLASS_NAME,
            LOADER_LOAD_FORMAT,
        )

        self.assertEqual(LOADER_LOAD_FORMAT, "model-transfer")
        self.assertEqual(LOADER_CLASS_NAME, "ModelTransferSGLangLoader")

    def test_entry_point_names_the_patched_route(self):
        from gpustack_model_transfer.adapters.sglang import (
            INTEGRATION_ROUTE,
            LOADER_ENTRY_POINT,
            LOADER_ENTRY_POINT_GROUP,
        )

        self.assertEqual(LOADER_ENTRY_POINT_GROUP, "sglang.srt.model_loader")
        self.assertEqual(
            LOADER_ENTRY_POINT,
            "gpustack_model_transfer.adapters.sglang:ModelTransferSGLangLoader",
        )
        # The spec requires choosing one route: the versioned patch route
        # against the pinned upstream commit.
        self.assertEqual(INTEGRATION_ROUTE, "versioned-patch")

    def test_supported_format_and_dtypes_are_pinned(self):
        from gpustack_model_transfer.adapters.sglang import (
            SUPPORTED_CHECKPOINT_FORMAT,
            SUPPORTED_DTYPES,
        )

        self.assertEqual(SUPPORTED_CHECKPOINT_FORMAT, "safetensors")
        self.assertGreaterEqual(set(SUPPORTED_DTYPES), {"F32", "F16", "BF16", "I64"})

    def test_image_integration_metadata_round_trips(self):
        import json

        from gpustack_model_transfer.adapters.sglang import image_integration_metadata

        metadata = image_integration_metadata()
        decoded = json.loads(json.dumps(metadata))
        self.assertEqual(decoded["engine"], "sglang")
        self.assertEqual(decoded["load-format"], "model-transfer")
        self.assertIn("contract", decoded)
        self.assertIn("route", decoded)

    def test_importing_the_adapter_needs_neither_torch_nor_sglang(self):
        code = (
            "import sys, json;"
            "sys.path.insert(0, sys.argv[1]);"
            "import gpustack_model_transfer.adapters.sglang as a;"
            "json.dump(sorted(m for m in sys.modules"
            "    if m.split('.')[0] in ('torch', 'sglang')), sys.stdout)"
        )
        done = subprocess.run(
            [sys.executable, "-c", code, str(_python_root())],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(
            done.stdout.strip(), "[]", "a module import pulled torch or sglang in"
        )


def _python_root():
    from pathlib import Path

    return Path(__file__).resolve().parents[2]


if __name__ == "__main__":
    unittest.main()
