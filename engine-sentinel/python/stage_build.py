"""Deterministic pure-stdlib staging builder for gpustack-engine-sentinel.

Installs an unpacked layout (gpustack_engine_sentinel/, sitecustomize.py, and
PEP 376 .dist-info metadata locked to the operator release version) directly
into <output-dir> without requiring pip, wheel, or third-party build packages
inside Docker builds.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

PACKAGE_NAME = "gpustack-engine-sentinel"
DIST_NAME = "gpustack_engine_sentinel"
FALLBACK_VERSION = "0.0.0"

WHEEL_TXT = """\
Wheel-Version: 1.0
Generator: gpustack-stage-build
Root-Is-Purelib: true
Tag: py3-none-any
"""

_PEP440_FALLBACK_RE = re.compile(
    r"^v?(\d+(?:\.\d+)*(?:(?:a|b|rc)\d+)?(?:\.post\d+)?(?:\.dev\d+)?)$"
)


def normalize_package_version(raw: str | None) -> str:
    """Normalize an operator release string (e.g. GPUSTACK_GIT_VERSION) to PEP 440."""
    text = (raw or "").strip()
    if not text:
        return FALLBACK_VERSION
    try:
        from packaging.version import InvalidVersion, Version

        try:
            return str(Version(text))
        except InvalidVersion:
            print(
                f"warning: version {text!r} is not a Python package version; "
                f"using {FALLBACK_VERSION} for {PACKAGE_NAME}",
                file=sys.stderr,
            )
            return FALLBACK_VERSION
    except ImportError:
        match = _PEP440_FALLBACK_RE.match(text)
        if match:
            return match.group(1)
        print(
            f"warning: version {text!r} is not a Python package version; "
            f"using {FALLBACK_VERSION} for {PACKAGE_NAME}",
            file=sys.stderr,
        )
        return FALLBACK_VERSION


def _render_metadata(version: str) -> str:
    return (
        "Metadata-Version: 2.1\n"
        f"Name: {PACKAGE_NAME}\n"
        f"Version: {version}\n"
        "Summary: Zero-intrusion host-staged online runtime control, ASGI pause gate, "
        "readiness latch, and PreStop drain for vLLM and SGLang.\n"
        "Requires-Python: >=3.10\n"
        "License: Apache-2.0\n"
    )


def _record_digest_and_size(data: bytes) -> tuple[str, int]:
    b64 = (
        base64.urlsafe_b64encode(hashlib.sha256(data).digest())
        .rstrip(b"=")
        .decode("ascii")
    )
    return f"sha256={b64}", len(data)


def stage_unpacked(
    src_root: Path, output_dir: Path, version: str | None = None
) -> Path:
    resolved_version = normalize_package_version(version)
    resolved_src = src_root.resolve()
    resolved_out = output_dir.resolve()
    resolved_cwd = Path.cwd().resolve()
    if (
        resolved_out in (resolved_out.parent, resolved_src, resolved_cwd)
        or resolved_out in resolved_src.parents
        or resolved_src in resolved_out.parents
    ):
        raise ValueError(
            f"Refusing to clear unsafe output_dir {resolved_out} relative to src_root {resolved_src}"
        )
    if output_dir.exists():
        shutil.rmtree(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    # Copy sitecustomize.py.
    shutil.copy2(src_root / "sitecustomize.py", output_dir / "sitecustomize.py")

    # Copy pure-Python package sources without bytecode caches.
    pkg_src = src_root / DIST_NAME
    for py_file in sorted(pkg_src.rglob("*.py")):
        rel = py_file.relative_to(src_root)
        dst = output_dir / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(py_file, dst)

    # Write PEP 376 .dist-info directory (with zero vllm.* entry points).
    dist_info = output_dir / f"{DIST_NAME}-{resolved_version}.dist-info"
    dist_info.mkdir(parents=True, exist_ok=True)
    (dist_info / "METADATA").write_text(
        _render_metadata(resolved_version), encoding="utf-8"
    )
    (dist_info / "WHEEL").write_text(WHEEL_TXT, encoding="utf-8")
    (dist_info / "INSTALLER").write_text("gpustack-stage-build\n", encoding="utf-8")

    record_lines: list[str] = []
    for item in sorted(
        (p for p in output_dir.rglob("*") if p.is_file()),
        key=lambda p: p.relative_to(output_dir).as_posix(),
    ):
        rel_posix = item.relative_to(output_dir).as_posix()
        digest, size = _record_digest_and_size(item.read_bytes())
        record_lines.append(f"{rel_posix},{digest},{size}")
    record_rel = (dist_info / "RECORD").relative_to(output_dir).as_posix()
    record_lines.append(f"{record_rel},,")
    (dist_info / "RECORD").write_text("\n".join(record_lines) + "\n", encoding="utf-8")

    # Verify in a clean interpreter with -B (no .pyc writes) that metadata and dormant sitecustomize work.
    verify_env = {
        k: v
        for k, v in os.environ.items()
        if not k.startswith("GPUSTACK_ENGINE_SENTINEL") and k != "VLLM_PLUGINS"
    }
    verify_env["PYTHONDONTWRITEBYTECODE"] = "1"
    verify_env["PYTHONPATH"] = str(output_dir)
    verify_code = (
        "import importlib.metadata as m, os, sys; "
        "assert 'gpustack_engine_sentinel' not in sys.modules; "
        "assert os.environ.get('VLLM_PLUGINS') is None; "
        f"assert m.version({PACKAGE_NAME!r}) == {resolved_version!r}; "
        "assert not [e for e in m.entry_points(group='vllm.endpoint_plugins') if 'gpustack' in e.name]; "
        "assert not [e for e in m.entry_points(group='vllm.general_plugins') if 'gpustack' in e.name]; "
        "import gpustack_engine_sentinel as es; "
        f"assert es.__version__ == {resolved_version!r}, es.__version__"
    )
    subprocess.run(
        [sys.executable, "-B", "-c", verify_code],
        env=verify_env,
        check=True,
    )
    return output_dir


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--src-root",
        type=Path,
        default=Path(__file__).resolve().parent,
        help="Path to engine-sentinel/python source directory.",
    )
    parser.add_argument(
        "--version",
        default=os.environ.get("GPUSTACK_GIT_VERSION", ""),
        help="Operator version string stamped into .dist-info (defaults to GPUSTACK_GIT_VERSION or 0.0.0).",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        required=True,
        help="Target directory receiving the unpacked flat layout (gpustack_engine_sentinel/, sitecustomize.py, .dist-info).",
    )
    args = parser.parse_args(argv)
    src_root = args.src_root.resolve()
    staged = stage_unpacked(src_root, args.output_dir.resolve(), version=args.version)
    print(staged)


if __name__ == "__main__":
    main()
