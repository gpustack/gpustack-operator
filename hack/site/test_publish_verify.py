#!/usr/bin/env python3
"""Unit and local-server tests for hack/site/publish.py and hack/site/verify.py."""

import contextlib
import http.server
import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

SITE_DIR = Path(__file__).resolve().parent
if str(SITE_DIR) not in sys.path:
    sys.path.insert(0, str(SITE_DIR))

import publish  # noqa: E402
import rewrite_reference  # noqa: E402
import verify  # noqa: E402

ROOT_URL = "https://docs.gpustack.ai/gpustack-operator/"
COMMIT_A = "a" * 40
COMMIT_B = "b" * 40
COMMIT_C = "c" * 40


def snapshot_tree(root: Path) -> dict[str, tuple[str, object]]:
    """Independent filesystem snapshot recording regular files, dirs, symlinks, and special nodes."""
    snap: dict[str, tuple[str, object]] = {}
    stack = [root]
    while stack:
        current = stack.pop()
        with os.scandir(current) as entries:
            for entry in sorted(entries, key=lambda item: item.name):
                rel = str(Path(entry.path).relative_to(root))
                est = entry.stat(follow_symlinks=False)
                if stat.S_ISLNK(est.st_mode):
                    snap[rel] = ("symlink", os.readlink(entry.path))
                elif stat.S_ISDIR(est.st_mode):
                    snap[rel] = ("dir", None)
                    stack.append(Path(entry.path))
                elif stat.S_ISREG(est.st_mode):
                    snap[rel] = ("file", Path(entry.path).read_bytes())
                else:
                    snap[rel] = ("special", stat.S_IFMT(est.st_mode))
    return snap


def make_pages(pages_dir: Path, chart_versions: tuple[str, ...] = ()) -> Path:
    """Create a Pages fixture directory with charts/index.yaml and optional packages."""
    charts_dir = pages_dir / "charts"
    charts_dir.mkdir(parents=True, exist_ok=True)
    (charts_dir / "index.yaml").write_text("apiVersion: v1\nentries: {}\n", encoding="utf-8")
    for version in chart_versions:
        (charts_dir / f"gpustack-operator-{version}.tgz").write_bytes(
            f"chart-bytes-{version}\n".encode("utf-8")
        )
    return pages_dir


def make_build(
    build_dir: Path,
    ref: str,
    commit: str,
    *,
    root_url: str = ROOT_URL,
    reference_files: dict[str, str] | None = None,
    extra_files: dict[str, bytes] | None = None,
    empty_reference_dir: bool = False,
) -> Path:
    """Create a minimal Hugo build fixture matching publish.py requirements."""
    if build_dir.exists():
        shutil.rmtree(build_dir)
    build_dir.mkdir(parents=True, exist_ok=True)
    (build_dir / "index.html").write_text(f"<html><body>{ref}</body></html>\n", encoding="utf-8")
    (build_dir / "llms.txt").write_text(
        f"# GPUStack Operator\nDocumentation version: {ref}. Source revision: {commit}.\n",
        encoding="utf-8",
    )
    (build_dir / "index.md").write_text(f"# Home {ref}\n", encoding="utf-8")
    (build_dir / "index.json").write_text("[]\n", encoding="utf-8")
    (build_dir / "site-version.json").write_text(
        json.dumps(
            {
                "version": ref,
                "sourceRef": commit,
                "baseURL": f"{root_url}{ref}/",
            }
        )
        + "\n",
        encoding="utf-8",
    )
    if reference_files is not None or empty_reference_dir:
        ref_dir = build_dir / "reference"
        ref_dir.mkdir(parents=True, exist_ok=True)
        for name, content in (reference_files or {}).items():
            (ref_dir / name).write_text(content, encoding="utf-8")
    for rel_path, data in (extra_files or {}).items():
        target = build_dir / rel_path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    return build_dir


class _LocalPagesServer:
    """Real loopback HTTP server serving a Pages directory with optional route overrides."""

    def __init__(
        self,
        root: Path,
        *,
        overrides: dict[str, list[tuple]] | None = None,
    ) -> None:
        self.root = root
        self.overrides = {k.lstrip("/"): list(v) for k, v in (overrides or {}).items()}
        self.requested_paths: list[str] = []
        self._lock = threading.Lock()
        self._server: http.server.ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self.base_url = ""

    def __enter__(self) -> "_LocalPagesServer":
        root_dir = str(self.root)
        overrides = self.overrides
        requested_paths = self.requested_paths
        lock = self._lock

        class Handler(http.server.SimpleHTTPRequestHandler):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, directory=root_dir, **kwargs)

            def do_GET(self):  # noqa: N802
                rel = self.path.split("?", 1)[0].lstrip("/")
                override = None
                with lock:
                    requested_paths.append(self.path)
                    queue = overrides.get(rel)
                    if queue:
                        override = queue.pop(0) if len(queue) > 1 else queue[0]
                if override is not None:
                    status = override[0]
                    payload = override[1]
                    headers = override[2] if len(override) > 2 else {}
                    delay = override[3] if len(override) > 3 else 0.0
                    if delay:
                        time.sleep(delay)
                    self.send_response(status)
                    for header_name, header_val in headers.items():
                        self.send_header(header_name, header_val)
                    self.send_header("Content-Type", "application/octet-stream")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                    return
                super().do_GET()

            def log_message(self, format, *args):  # noqa: A002
                return

        self._server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        port = self._server.server_address[1]
        self.base_url = f"http://127.0.0.1:{port}/"
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
        if self._thread is not None:
            self._thread.join(timeout=5)


class PublishAndVerifyScenariosTest(unittest.TestCase):
    """End-to-end fixture tests for Pages composition and public verification."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="test-publish-verify-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def _compose_quiet(
        self,
        build: Path,
        pages: Path,
        ref: str,
        commit: str,
        root_url: str = ROOT_URL,
        *,
        obsolete_manifest: Path | None = None,
    ):
        with contextlib.redirect_stdout(io.StringIO()):
            publish.compose(build, pages, ref, commit, root_url, obsolete_manifest=obsolete_manifest)

    def test_scenario_1_legacy_stable_default_and_new_main(self):
        pages = make_pages(self.tmp / "pages")

        # Legacy stable release v0.8.0 has no reference/ directory.
        v08_build = make_build(self.tmp / "build-v0.8.0", "v0.8.0", COMMIT_A)
        self._compose_quiet(v08_build, pages, "v0.8.0", COMMIT_A)
        (pages / "charts" / "gpustack-operator-0.8.0.tgz").write_bytes(b"chart-bytes-0.8.0\n")
        charts_before = publish.inventory(pages / "charts")

        # Simulate a stale root reference/ directory left behind before publishing main.
        stale_dir = pages / "reference"
        stale_dir.mkdir()
        (stale_dir / "stale.md").write_text("# Stale\n", encoding="utf-8")

        # Publish main with versioned reference/ files and capture obsolete manifest outside pages/.
        obsolete_manifest = self.tmp / "obsolete-paths.json"
        main_refs = {
            "README.md": "# Main Ref\nSee [Preflight](../docs/modules/devices/preflight/index.md#check).\n",
            "commands.md": "# Commands\nSee [README](README.md).\n",
        }
        main_build = make_build(self.tmp / "build-main", "main", COMMIT_B, reference_files=main_refs)
        self._compose_quiet(
            main_build, pages, "main", COMMIT_B, obsolete_manifest=obsolete_manifest
        )

        manifest = json.loads((pages / "versions.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["latest"], "v0.8.0")
        self.assertEqual(
            (pages / "llms.txt").read_bytes(),
            (pages / "v0.8.0" / "llms.txt").read_bytes(),
        )
        self.assertFalse((pages / "reference").exists())
        self.assertEqual(
            (pages / "main" / "reference" / "README.md").read_text(encoding="utf-8"),
            main_refs["README.md"],
        )
        self.assertEqual(publish.inventory(pages / "charts"), charts_before)
        self.assertEqual(
            json.loads(obsolete_manifest.read_text(encoding="utf-8")),
            {"obsolete_paths": ["reference/stale.md"]},
        )

        # Verify against real local HTTP server using obsolete manifest + auto-discovered aliases.
        with _LocalPagesServer(pages) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                verify.verify(
                    pages,
                    "main",
                    server.base_url,
                    obsolete_manifest=obsolete_manifest,
                    timeout=2,
                    poll_interval=0.05,
                )

        # Verify fails if an obsolete root alias (auto-discovered from main/reference/) still serves 200.
        with _LocalPagesServer(
            pages,
            overrides={"reference/commands.md": [(200, b"# Leaked root alias\n")]},
        ) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(ValueError, "reference/commands.md"):
                    verify.verify(
                        pages,
                        "main",
                        server.base_url,
                        obsolete_manifest=obsolete_manifest,
                        timeout=0.1,
                        poll_interval=0.05,
                    )

    def test_scenario_2_new_stable_promotion_atomic_replace_and_stale_alias_404(self):
        # Start with clean chart fixture and add each release chart at the workflow boundary.
        pages = make_pages(self.tmp / "pages")
        charts_before_v09 = publish.inventory(pages / "charts")

        v09_commands = (
            "# Commands\n\n"
            "See [Preflight](../docs/modules/devices/preflight/index.md#step-1).\n"
            "Diagram: ![Arch](../docs/assets/diagrams/arch.svg)\n"
            "Sibling: [Old](old-alias.md#note)\n"
            "Repo: [Dev](https://github.com/gpustack/gpustack-operator/blob/"
            f"{COMMIT_A}/docs/contribute/development.md)\n"
            "Inline code: `[skip](../docs/inline/index.md)`\n"
            "<!-- [comment](../docs/comment/index.md) -->\n"
            "```markdown\n"
            "[fenced](../docs/fenced/index.md)\n"
            "```\n"
        )
        v09_build = make_build(
            self.tmp / "build-v0.9.0",
            "v0.9.0",
            COMMIT_A,
            reference_files={
                "README.md": "# Ref 0.9\n[Old](old-alias.md)\n",
                "old-alias.md": "# Old Alias\n[Guide](../docs/operate/installation-modes/index.md)\n",
                "commands.md": v09_commands,
            },
        )
        self._compose_quiet(v09_build, pages, "v0.9.0", COMMIT_A)
        self.assertEqual(publish.inventory(pages / "charts"), charts_before_v09)
        # Workflow boundary: Helm packages v0.9.0 chart after compose.
        (pages / "charts" / "gpustack-operator-0.9.0.tgz").write_bytes(b"chart-0.9.0\n")

        self.assertEqual(
            sorted(p.name for p in (pages / "reference").iterdir()),
            ["README.md", "commands.md", "old-alias.md"],
        )
        root_v09_cmd = (pages / "reference" / "commands.md").read_text(encoding="utf-8")
        self.assertIn(f"{ROOT_URL}v0.9.0/docs/modules/devices/preflight/index.md#step-1", root_v09_cmd)
        self.assertIn(f"{ROOT_URL}v0.9.0/docs/assets/diagrams/arch.svg", root_v09_cmd)
        self.assertIn("[Old](old-alias.md#note)", root_v09_cmd)
        self.assertIn("`[skip](../docs/inline/index.md)`", root_v09_cmd)
        self.assertIn("<!-- [comment](../docs/comment/index.md) -->", root_v09_cmd)
        self.assertIn("[fenced](../docs/fenced/index.md)", root_v09_cmd)
        self.assertEqual(
            (pages / "v0.9.0" / "reference" / "commands.md").read_text(encoding="utf-8"),
            v09_commands,
        )

        # Promote v0.10.0: drops old-alias.md and adds new-alias.md.
        charts_before_v10 = publish.inventory(pages / "charts")
        obsolete_manifest = self.tmp / "v10-obsolete.json"
        v10_build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_B,
            reference_files={
                "README.md": "# Ref 0.10\n[New](new-alias.md)\n",
                "new-alias.md": "# New Alias\n[Guide](../docs/operate/installation-modes/index.md#modes)\n",
                "commands.md": "# Commands 0.10\n[Preflight](../docs/modules/devices/preflight/index.md)\n",
            },
        )
        self._compose_quiet(
            v10_build, pages, "v0.10.0", COMMIT_B, obsolete_manifest=obsolete_manifest
        )
        self.assertEqual(publish.inventory(pages / "charts"), charts_before_v10)
        self.assertEqual(
            json.loads(obsolete_manifest.read_text(encoding="utf-8")),
            {"obsolete_paths": ["reference/old-alias.md"]},
        )
        # Workflow boundary: Helm packages v0.10.0 chart after compose.
        (pages / "charts" / "gpustack-operator-0.10.0.tgz").write_bytes(b"chart-0.10.0\n")

        manifest = json.loads((pages / "versions.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["latest"], "v0.10.0")
        self.assertEqual(
            (pages / "llms.txt").read_bytes(),
            (pages / "v0.10.0" / "llms.txt").read_bytes(),
        )
        self.assertEqual(
            sorted(p.name for p in (pages / "reference").iterdir()),
            ["README.md", "commands.md", "new-alias.md"],
        )
        self.assertFalse((pages / "reference" / "old-alias.md").exists())
        leftover_temps = [p.name for p in pages.iterdir() if p.name.startswith(".reference-")]
        self.assertEqual(leftover_temps, [])

        root_new_alias = (pages / "reference" / "new-alias.md").read_text(encoding="utf-8")
        self.assertIn(f"{ROOT_URL}v0.10.0/docs/operate/installation-modes/index.md#modes", root_new_alias)
        self.assertTrue((pages / "v0.9.0" / "reference" / "old-alias.md").is_file())

        # Local HTTP verification passes and verifies HTTP 404 on reference/old-alias.md.
        with _LocalPagesServer(pages) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                verify.verify(
                    pages,
                    "v0.10.0",
                    server.base_url,
                    obsolete_manifest=obsolete_manifest,
                    timeout=2,
                    poll_interval=0.05,
                )

        # Negative check: if stale reference/old-alias.md still returns 200, verify MUST fail.
        with _LocalPagesServer(
            pages,
            overrides={"reference/old-alias.md": [(200, b"# Old Alias still served\n")]},
        ) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(ValueError, "reference/old-alias.md"):
                    verify.verify(pages, "v0.10.0", server.base_url, timeout=0.1, poll_interval=0.05)

    def test_scenario_3_idempotent_same_tag_commit_retry_noop_and_repair(self):
        pages = make_pages(self.tmp / "pages")
        v09_build = make_build(
            self.tmp / "build-v0.9.0",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# 0.9\n[G](../docs/a/index.md)\n"},
        )
        self._compose_quiet(v09_build, pages, "v0.9.0", COMMIT_A)
        (pages / "charts" / "gpustack-operator-0.9.0.tgz").write_bytes(b"chart-0.9.0\n")

        v10_build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_B,
            reference_files={"README.md": "# 0.10\n[G](../docs/b/index.md)\n"},
        )
        self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_B)
        (pages / "charts" / "gpustack-operator-0.10.0.tgz").write_bytes(b"chart-0.10.0\n")

        # Set sentinel past mtimes on root outputs to verify a clean retry performs zero writes.
        sentinel_ts = 1_700_000_000
        tracked_root = [
            pages / "versions.json",
            pages / "index.html",
            pages / "llms.txt",
            pages / "robots.txt",
            pages / ".nojekyll",
            pages / "reference" / "README.md",
            pages / "v0.10.0" / "index.html",
        ]
        for path in tracked_root:
            os.utime(path, (sentinel_ts, sentinel_ts))

        full_before = snapshot_tree(pages)
        charts_before = publish.inventory(pages / "charts")
        inodes_before = {path: path.stat().st_ino for path in tracked_root}

        # Clean retry of latest tag (v0.10.0): true zero-write no-op (bytes, mtimes, inodes untouched).
        v10_retry = make_build(
            self.tmp / "build-v0.10.0-retry",
            "v0.10.0",
            COMMIT_B,
            reference_files={"README.md": "# 0.10\n[G](../docs/b/index.md)\n"},
        )
        self._compose_quiet(v10_retry, pages, "v0.10.0", COMMIT_B)
        self.assertEqual(snapshot_tree(pages), full_before)
        self.assertEqual(publish.inventory(pages / "charts"), charts_before)
        for path in tracked_root:
            self.assertEqual(int(path.stat().st_mtime), sentinel_ts, f"{path} was unexpectedly rewritten")
            self.assertEqual(path.stat().st_ino, inodes_before[path], f"{path} inode changed")

        # Clean retry of non-latest tag (v0.9.0): also true zero-write no-op.
        v09_retry = make_build(
            self.tmp / "build-v0.9.0-retry",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# 0.9\n[G](../docs/a/index.md)\n"},
        )
        self._compose_quiet(v09_retry, pages, "v0.9.0", COMMIT_A)
        self.assertEqual(snapshot_tree(pages), full_before)
        for path in tracked_root:
            self.assertEqual(int(path.stat().st_mtime), sentinel_ts)

        # Drift repair retry: stale root files and stale root reference/ are repaired while snapshot stays intact.
        (pages / "index.html").write_text("stale-index\n", encoding="utf-8")
        (pages / ".nojekyll").unlink()
        (pages / "reference" / "stale.md").write_text("# Stale\n", encoding="utf-8")
        self._compose_quiet(v10_retry, pages, "v0.10.0", COMMIT_B)
        self.assertEqual(snapshot_tree(pages), full_before)
        # Existing snapshot file was never rewritten during repair.
        self.assertEqual(int((pages / "v0.10.0" / "index.html").stat().st_mtime), sentinel_ts)

    def test_scenario_4_mutated_same_tag_commit_retry_rejected_without_mutation(self):
        pages = make_pages(self.tmp / "pages")
        base_refs = {
            "README.md": "# 0.10\n[G](../docs/b/index.md)\n",
            "commands.md": "# Commands\n",
        }
        v10_build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_A,
            reference_files=base_refs,
        )
        self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_A)
        (pages / "charts" / "gpustack-operator-0.10.0.tgz").write_bytes(b"chart-0.10.0\n")

        snapshot_before = snapshot_tree(pages)

        cases = [
            (
                "changed byte in root html",
                base_refs,
                {"index.html": b"<html><body>v0.10.0-mutated</body></html>\n"},
            ),
            (
                "changed byte in reference file",
                {"README.md": "# 0.10\n[G](../docs/b/index.md)\n", "commands.md": "# Commands!\n"},
                None,
            ),
            (
                "added extra file",
                base_refs,
                {"extra.txt": b"unexpected\n"},
            ),
            (
                "removed reference file",
                {"README.md": "# 0.10\n[G](../docs/b/index.md)\n"},
                None,
            ),
        ]

        for label, refs, extras in cases:
            with self.subTest(case=label):
                mutated = make_build(
                    self.tmp / f"mutated-{label.replace(' ', '-')}",
                    "v0.10.0",
                    COMMIT_A,
                    reference_files=refs,
                    extra_files=extras,
                )
                with self.assertRaisesRegex(ValueError, "immutable"):
                    self._compose_quiet(mutated, pages, "v0.10.0", COMMIT_A)
                self.assertEqual(snapshot_tree(pages), snapshot_before)

    def test_scenario_4b_missing_snapshot_for_recorded_tag_rejected(self):
        pages = make_pages(self.tmp / "pages")
        v10_build = make_build(self.tmp / "build-v0.10.0", "v0.10.0", COMMIT_A)
        self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_A)
        shutil.rmtree(pages / "v0.10.0")

        snapshot_before = snapshot_tree(pages)
        with self.assertRaisesRegex(ValueError, "immutable"):
            self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_A)
        self.assertEqual(snapshot_tree(pages), snapshot_before)

    def test_scenario_5_different_commit_tag_move_rejected(self):
        pages = make_pages(self.tmp / "pages")
        v10_build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_A,
            reference_files={"README.md": "# Ref\n"},
        )
        self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_A)
        snapshot_before = snapshot_tree(pages)

        moved_build = make_build(
            self.tmp / "build-v0.10.0-moved",
            "v0.10.0",
            COMMIT_B,
            reference_files={"README.md": "# Ref\n"},
        )
        with self.assertRaisesRegex(ValueError, "immutable"):
            self._compose_quiet(moved_build, pages, "v0.10.0", COMMIT_B)
        self.assertEqual(snapshot_tree(pages), snapshot_before)

    def test_mutable_main_obsolete_manifest_end_to_end(self):
        pages = make_pages(self.tmp / "pages")
        obsolete_manifest = self.tmp / "obsolete-paths.json"

        # Publish main as latest with a file unique to main (unique-main.md).
        main_v1 = make_build(
            self.tmp / "build-main-1",
            "main",
            COMMIT_A,
            reference_files={
                "unique-main.md": "# Unique\n[Doc](../docs/x/index.md)\n",
                "beta.md": "# Beta\n",
            },
        )
        self._compose_quiet(main_v1, pages, "main", COMMIT_A, obsolete_manifest=obsolete_manifest)
        self.assertEqual(
            json.loads(obsolete_manifest.read_text(encoding="utf-8")),
            {"obsolete_paths": []},
        )

        # Re-publish main dropping unique-main.md: both root and main/reference paths are recorded.
        main_v2 = make_build(
            self.tmp / "build-main-2",
            "main",
            COMMIT_B,
            reference_files={"beta.md": "# Beta 2\n"},
        )
        self._compose_quiet(main_v2, pages, "main", COMMIT_B, obsolete_manifest=obsolete_manifest)
        self.assertEqual(
            json.loads(obsolete_manifest.read_text(encoding="utf-8")),
            {
                "obsolete_paths": [
                    "main/reference/unique-main.md",
                    "reference/unique-main.md",
                ]
            },
        )
        self.assertEqual(sorted(p.name for p in (pages / "reference").iterdir()), ["beta.md"])

        # End-to-end verify with obsolete manifest passes on direct 404.
        with _LocalPagesServer(pages) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                verify.verify(
                    pages,
                    "main",
                    server.base_url,
                    obsolete_manifest=obsolete_manifest,
                    timeout=2,
                    poll_interval=0.05,
                )

        # Fails if either removed path still serves HTTP 200.
        for leaked in ("reference/unique-main.md", "main/reference/unique-main.md"):
            with self.subTest(leaked=leaked):
                with _LocalPagesServer(
                    pages,
                    overrides={leaked: [(200, b"# Leaked\n")]},
                ) as server:
                    with contextlib.redirect_stdout(io.StringIO()):
                        with self.assertRaisesRegex(ValueError, leaked):
                            verify.verify(
                                pages,
                                "main",
                                server.base_url,
                                obsolete_manifest=obsolete_manifest,
                                timeout=0.1,
                                poll_interval=0.05,
                            )

        # Re-publish main with an empty reference/ directory -> root reference/ removed cleanly.
        main_v3 = make_build(
            self.tmp / "build-main-3",
            "main",
            COMMIT_C,
            empty_reference_dir=True,
        )
        self._compose_quiet(main_v3, pages, "main", COMMIT_C, obsolete_manifest=obsolete_manifest)
        self.assertFalse((pages / "reference").exists())
        self.assertEqual(
            json.loads(obsolete_manifest.read_text(encoding="utf-8")),
            {"obsolete_paths": ["main/reference/beta.md", "reference/beta.md"]},
        )

    def test_stage_before_write_failure_preserves_previous_generation(self):
        pages = make_pages(self.tmp / "pages")
        v09_build = make_build(
            self.tmp / "build-v0.9.0",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# 0.9\n[G](../docs/a/index.md)\n"},
        )
        self._compose_quiet(v09_build, pages, "v0.9.0", COMMIT_A)
        (pages / "charts" / "gpustack-operator-0.9.0.tgz").write_bytes(b"chart-0.9.0\n")

        before = snapshot_tree(pages)

        # Build v0.10.0 with a reference file whose staging fails (invalid UTF-8 bytes).
        v10_bad = make_build(
            self.tmp / "build-v0.10.0-bad",
            "v0.10.0",
            COMMIT_B,
            extra_files={"reference/broken.md": b"\xff\xfeinvalid-utf8"},
        )
        with self.assertRaises(UnicodeDecodeError):
            self._compose_quiet(v10_bad, pages, "v0.10.0", COMMIT_B)

        # Also test a ValueError raised during stage_reference_dir.
        v10_build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_B,
            reference_files={"README.md": "# 0.10\n"},
        )
        with mock.patch.object(
            rewrite_reference, "stage_reference_dir", side_effect=ValueError("simulated staging error")
        ):
            with self.assertRaisesRegex(ValueError, "simulated staging error"):
                self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_B)

        # Previous consistent generation is completely untouched (including zero leftover temp dirs).
        self.assertEqual(snapshot_tree(pages), before)

    def test_swap_backup_cleanup_and_preservation_on_restore_failure(self):
        pages = make_pages(self.tmp / "pages")
        v09_build = make_build(
            self.tmp / "build-v0.9.0",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# 0.9\n"},
        )
        self._compose_quiet(v09_build, pages, "v0.9.0", COMMIT_A)

        v10_build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_B,
            reference_files={"README.md": "# 0.10\n"},
        )

        # Case A: swap fails, restore succeeds -> original root reference/ restored and backup cleaned up.
        real_replace = os.replace
        calls = {"n": 0}

        def fail_second_replace(src, dst):
            calls["n"] += 1
            if calls["n"] == 2:
                raise OSError("simulated swap failure")
            return real_replace(src, dst)

        with mock.patch.object(publish.os, "replace", side_effect=fail_second_replace):
            with self.assertRaisesRegex(OSError, "simulated swap failure"):
                self._compose_quiet(v10_build, pages, "v0.10.0", COMMIT_B)
        self.assertEqual((pages / "reference" / "README.md").read_text(encoding="utf-8"), "# 0.9\n")
        self.assertEqual([p.name for p in pages.iterdir() if p.name.startswith(".reference-")], [])

        # Case B: swap fails AND restore ALSO fails -> backup_parent is preserved so data is never deleted!
        pages_b = make_pages(self.tmp / "pages-b")
        self._compose_quiet(v09_build, pages_b, "v0.9.0", COMMIT_A)
        calls_b = {"n": 0}

        def fail_swap_and_restore(src, dst):
            calls_b["n"] += 1
            if calls_b["n"] == 2:
                raise OSError("swap failed")
            if calls_b["n"] == 3:
                raise OSError("restore failed")
            return real_replace(src, dst)

        with mock.patch.object(publish.os, "replace", side_effect=fail_swap_and_restore):
            with self.assertRaisesRegex(OSError, "restore failed"):
                self._compose_quiet(v10_build, pages_b, "v0.10.0", COMMIT_B)

        backups = [p for p in pages_b.iterdir() if p.name.startswith(".reference-backup-")]
        self.assertEqual(len(backups), 1)
        self.assertEqual(
            (backups[0] / "reference" / "README.md").read_text(encoding="utf-8"),
            "# 0.9\n",
        )

    def test_rejects_symlinks_and_non_regular_files_in_build_and_destination(self):
        pages = make_pages(self.tmp / "pages")
        v09_build = make_build(
            self.tmp / "build-v0.9.0",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# 0.9\n"},
        )
        self._compose_quiet(v09_build, pages, "v0.9.0", COMMIT_A)
        before_pages = snapshot_tree(pages)

        # 1. Symlinked build root.
        build_symlink = self.tmp / "build-link"
        build_symlink.symlink_to(v09_build)
        with self.assertRaisesRegex(ValueError, "symlink"):
            self._compose_quiet(build_symlink, pages, "main", COMMIT_B)
        self.assertEqual(snapshot_tree(pages), before_pages)

        # 2. File symlink, directory symlink, dangling symlink, and FIFO inside build.
        outside_file = self.tmp / "outside.txt"
        outside_file.write_text("secret\n", encoding="utf-8")
        outside_dir = self.tmp / "outside-dir"
        outside_dir.mkdir()

        def add_file_symlink(b: Path):
            (b / "link.md").symlink_to(outside_file)

        def add_dir_symlink(b: Path):
            (b / "dir-link").symlink_to(outside_dir)

        def add_dangling_symlink(b: Path):
            (b / "dangling.md").symlink_to(self.tmp / "does-not-exist")

        def add_fifo(b: Path):
            os.mkfifo(b / "pipe.fifo")

        for label, mutator, pattern in (
            ("file symlink in build", add_file_symlink, "symlink"),
            ("dir symlink in build", add_dir_symlink, "symlink"),
            ("dangling symlink in build", add_dangling_symlink, "symlink"),
            ("fifo in build", add_fifo, "regular files and directories"),
        ):
            with self.subTest(case=label):
                bad_build = make_build(self.tmp / f"bad-{label.replace(' ', '-')}", "main", COMMIT_B)
                mutator(bad_build)
                with self.assertRaisesRegex(ValueError, pattern):
                    self._compose_quiet(bad_build, pages, "main", COMMIT_B)
                self.assertEqual(snapshot_tree(pages), before_pages)

        # 3. Symlink or FIFO inside existing destination snapshot (rejected before comparison/write).
        good_v09_retry = make_build(
            self.tmp / "build-v0.9.0-retry",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# 0.9\n"},
        )
        for label, mutator, pattern in (
            ("dangling symlink in destination", add_dangling_symlink, "symlink"),
            ("fifo in destination", add_fifo, "regular files and directories"),
        ):
            with self.subTest(case=label):
                pages_case = make_pages(self.tmp / f"pages-{label.replace(' ', '-')}")
                self._compose_quiet(v09_build, pages_case, "v0.9.0", COMMIT_A)
                mutator(pages_case / "v0.9.0")
                before_case = snapshot_tree(pages_case)
                with self.assertRaisesRegex(ValueError, pattern):
                    self._compose_quiet(good_v09_retry, pages_case, "v0.9.0", COMMIT_A)
                self.assertEqual(snapshot_tree(pages_case), before_case)

        # 4. Symlinked destination root.
        pages_dst_link = make_pages(self.tmp / "pages-dst-link")
        (pages_dst_link / "main").symlink_to(outside_dir)
        before_dst_link = snapshot_tree(pages_dst_link)
        good_main = make_build(self.tmp / "good-main", "main", COMMIT_B)
        with self.assertRaisesRegex(ValueError, "symlink"):
            self._compose_quiet(good_main, pages_dst_link, "main", COMMIT_B)
        self.assertEqual(snapshot_tree(pages_dst_link), before_dst_link)


class PublishValidationAndOrderingTest(unittest.TestCase):
    """Cover publish.py validation, version ordering, and CLI error paths."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="test-publish-val-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_version_kind_and_order(self):
        self.assertEqual(publish.version_kind("main"), "development")
        self.assertEqual(publish.version_kind("v1.2.3"), "stable")
        self.assertEqual(publish.version_kind("v1.2.3-rc1"), "prerelease")
        with self.assertRaises(ValueError):
            publish.version_kind("release-1")

        self.assertEqual(publish.order({"name": "main"}), (-1, -1, -1, ()))
        self.assertGreater(
            publish.order({"name": "v0.10.0"}),
            publish.order({"name": "v0.9.0"}),
        )
        self.assertGreater(
            publish.order({"name": "v0.10.0-rc10"}),
            publish.order({"name": "v0.10.0-rc2"}),
        )

    def test_prerelease_hidden_after_matching_stable_and_robots(self):
        pages = make_pages(self.tmp / "pages")
        rc_build = make_build(self.tmp / "b-rc", "v0.9.0-rc1", COMMIT_A)
        with contextlib.redirect_stdout(io.StringIO()):
            publish.compose(rc_build, pages, "v0.9.0-rc1", COMMIT_A, ROOT_URL)
        manifest = json.loads((pages / "versions.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["latest"], "v0.9.0-rc1")
        self.assertEqual([v["name"] for v in manifest["versions"]], ["v0.9.0-rc1"])
        self.assertIn("v0.9.0-rc1/sitemap.xml", (pages / "robots.txt").read_text(encoding="utf-8"))

        stable_build = make_build(self.tmp / "b-stable", "v0.9.0", COMMIT_B)
        with contextlib.redirect_stdout(io.StringIO()):
            publish.compose(stable_build, pages, "v0.9.0", COMMIT_B, ROOT_URL)
        manifest = json.loads((pages / "versions.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["latest"], "v0.9.0")
        self.assertEqual([v["name"] for v in manifest["versions"]], ["v0.9.0"])
        self.assertIn("v0.9.0/sitemap.xml", (pages / "robots.txt").read_text(encoding="utf-8"))
        self.assertNotIn("v0.9.0-rc1/sitemap.xml", (pages / "robots.txt").read_text(encoding="utf-8"))

    def test_compose_validation_errors(self):
        pages = make_pages(self.tmp / "pages")
        valid_build = make_build(self.tmp / "build", "main", COMMIT_A)

        with self.assertRaisesRegex(ValueError, "HTTPS"):
            publish.compose(valid_build, pages, "main", COMMIT_A, "http://example.com/")
        with self.assertRaisesRegex(ValueError, "Git commit SHA"):
            publish.compose(valid_build, pages, "main", "short", ROOT_URL)
        with self.assertRaisesRegex(ValueError, "separate directories"):
            nested_build = make_build(pages / "nested", "main", COMMIT_A)
            publish.compose(nested_build, pages, "main", COMMIT_A, ROOT_URL)
        shutil.rmtree(pages / "nested")

        with self.assertRaisesRegex(ValueError, "outside the Pages checkout"):
            publish.compose(
                valid_build, pages, "main", COMMIT_A, ROOT_URL, obsolete_manifest=pages / "obsolete.json"
            )

        missing_file_build = make_build(self.tmp / "build-missing", "main", COMMIT_A)
        (missing_file_build / "index.json").unlink()
        with self.assertRaisesRegex(ValueError, "missing index.json"):
            publish.compose(missing_file_build, pages, "main", COMMIT_A, ROOT_URL)

        bad_identity_build = make_build(self.tmp / "build-bad-id", "main", COMMIT_B)
        with self.assertRaisesRegex(ValueError, "does not match publication"):
            publish.compose(bad_identity_build, pages, "main", COMMIT_A, ROOT_URL)

        no_charts = self.tmp / "no-charts"
        no_charts.mkdir()
        with self.assertRaisesRegex(ValueError, "charts/index.yaml"):
            publish.compose(valid_build, no_charts, "main", COMMIT_A, ROOT_URL)

        bad_manifest = make_pages(self.tmp / "bad-manifest")
        (bad_manifest / "versions.json").write_text('{"published": {}}\n', encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "must be a list"):
            publish.compose(valid_build, bad_manifest, "main", COMMIT_A, ROOT_URL)

        bad_path_manifest = make_pages(self.tmp / "bad-path-manifest")
        (bad_path_manifest / "versions.json").write_text(
            json.dumps({"published": [{"name": "main", "path": "wrong/", "commit": COMMIT_A}]}) + "\n",
            encoding="utf-8",
        )
        with self.assertRaisesRegex(ValueError, "invalid version directory"):
            publish.compose(valid_build, bad_path_manifest, "main", COMMIT_A, ROOT_URL)

        orphan_chart_pages = make_pages(self.tmp / "orphan-chart", chart_versions=("0.9.0",))
        tag_build = make_build(self.tmp / "build-v0.9.0", "v0.9.0", COMMIT_A)
        with self.assertRaisesRegex(ValueError, "no published source revision"):
            publish.compose(tag_build, orphan_chart_pages, "v0.9.0", COMMIT_A, ROOT_URL)

        nested_ref_build = make_build(self.tmp / "build-nested-ref", "main", COMMIT_A)
        (nested_ref_build / "reference" / "sub").mkdir(parents=True)
        with self.assertRaisesRegex(ValueError, "flat"):
            publish.compose(nested_ref_build, pages, "main", COMMIT_A, ROOT_URL)

    def test_publish_cli_success_obsolete_manifest_and_error(self):
        pages = make_pages(self.tmp / "pages")
        obsolete_manifest = self.tmp / "obsolete.json"
        build = make_build(
            self.tmp / "build-v0.9.0",
            "v0.9.0",
            COMMIT_A,
            reference_files={"README.md": "# Ref\n[G](../docs/a/index.md)\n"},
        )
        cmd = [
            sys.executable,
            str(SITE_DIR / "publish.py"),
            "--build",
            str(build),
            "--pages",
            str(pages),
            "--ref",
            "v0.9.0",
            "--commit",
            COMMIT_A,
            "--root-url",
            ROOT_URL,
            "--obsolete-manifest",
            str(obsolete_manifest),
        ]
        ok = subprocess.run(cmd, capture_output=True, text=True, check=False)
        self.assertEqual(ok.returncode, 0, ok.stderr)
        self.assertIn("Composed v0.9.0", ok.stdout)
        self.assertEqual(json.loads(obsolete_manifest.read_text(encoding="utf-8")), {"obsolete_paths": []})

        # Mutate build and verify CLI exits 1 without changing pages.
        (build / "reference" / "README.md").write_text("# Mutated\n", encoding="utf-8")
        fail = subprocess.run(cmd, capture_output=True, text=True, check=False)
        self.assertEqual(fail.returncode, 1)
        self.assertIn("immutable", fail.stderr)


class VerifyBehaviorAndCLITest(unittest.TestCase):
    """Cover verify.py edge cases, no-redirect probes, containment, deadline capping, and CLI."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="test-verify-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.pages = make_pages(self.tmp / "pages")
        build = make_build(
            self.tmp / "build-v0.10.0",
            "v0.10.0",
            COMMIT_A,
            reference_files={
                "README.md": "# Ref\n[Doc](../docs/a/index.md)\n",
                "commands.md": "# Commands\n",
            },
        )
        with contextlib.redirect_stdout(io.StringIO()):
            publish.compose(build, self.pages, "v0.10.0", COMMIT_A, ROOT_URL)
        (self.pages / "charts" / "gpustack-operator-0.10.0.tgz").write_bytes(b"chart-0.10.0\n")

    def test_verify_detects_byte_mismatch_on_root_reference(self):
        with _LocalPagesServer(
            self.pages,
            overrides={"reference/commands.md": [(200, b"# Wrong bytes\n")]},
        ) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(ValueError, "reference/commands.md: public bytes differ"):
                    verify.verify(self.pages, "v0.10.0", server.base_url, timeout=0.1, poll_interval=0.05)

    def test_verify_detects_missing_expected_file(self):
        with _LocalPagesServer(
            self.pages,
            overrides={"v0.10.0/reference/commands.md": [(404, b"not found")]},
        ) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(ValueError, "v0.10.0/reference/commands.md"):
                    verify.verify(self.pages, "v0.10.0", server.base_url, timeout=0.1, poll_interval=0.05)

    def test_verify_converges_after_transient_mismatch_and_stale_alias(self):
        expected_bytes = (self.pages / "reference" / "commands.md").read_bytes()
        with _LocalPagesServer(
            self.pages,
            overrides={
                "reference/commands.md": [(200, b"stale-bytes"), (200, expected_bytes)],
                "reference/removed.md": [(200, b"still-cached"), (404, b"not found")],
            },
        ) as server:
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                verify.verify(
                    self.pages,
                    "v0.10.0",
                    server.base_url,
                    obsolete_paths=["reference/removed.md"],
                    timeout=5,
                    poll_interval=0.05,
                )
            self.assertIn("Waiting for Pages propagation", out.getvalue())
            self.assertIn("Public publication matches v0.10.0", out.getvalue())

    def test_verify_rejects_redirects_on_obsolete_paths(self):
        # Both 301->404 and 301->200 must FAIL; only direct HTTP 404 passes.
        for label, target in (("301 to 404", "/definitely-missing-404"), ("301 to 200", "/index.html")):
            with self.subTest(case=label):
                with _LocalPagesServer(
                    self.pages,
                    overrides={
                        "reference/removed.md": [
                            (301, b"moved", {"Location": target}),
                        ]
                    },
                ) as server:
                    with contextlib.redirect_stdout(io.StringIO()):
                        with self.assertRaisesRegex(ValueError, "expected HTTP 404, got HTTP 301"):
                            verify.verify(
                                self.pages,
                                "v0.10.0",
                                server.base_url,
                                obsolete_paths=["reference/removed.md"],
                                timeout=0.1,
                                poll_interval=0.05,
                            )

    def test_verify_fails_on_non_404_error_for_obsolete_path(self):
        with _LocalPagesServer(
            self.pages,
            overrides={"reference/removed.md": [(500, b"server error")]},
        ) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(ValueError, "expected HTTP 404, got HTTP 500"):
                    verify.verify(
                        self.pages,
                        "v0.10.0",
                        server.base_url,
                        obsolete_paths=["reference/removed.md"],
                        timeout=0.1,
                        poll_interval=0.05,
                    )

    def test_verify_rejects_local_root_reference_drift(self):
        (self.pages / "reference" / "extra-stale.md").write_text("# Drift\n", encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "root reference"):
            verify.verify(self.pages, "v0.10.0", "http://127.0.0.1:9/", timeout=0.1, poll_interval=0.05)

    def test_verify_rejects_invalid_ref_manifest_and_traversal(self):
        # Invalid ref (path traversal).
        with self.assertRaisesRegex(ValueError, "ref must be main"):
            verify.verify(self.pages, "../escape", "http://127.0.0.1:9/", timeout=0.1, poll_interval=0.05)

        # Invalid manifest record path traversal.
        bad_pages = make_pages(self.tmp / "bad-manifest-pages")
        (bad_pages / "versions.json").write_text(
            json.dumps(
                {
                    "latest": "v0.10.0",
                    "published": [{"name": "v0.10.0", "path": "../escape/", "commit": COMMIT_A}],
                }
            )
            + "\n",
            encoding="utf-8",
        )
        with self.assertRaisesRegex(ValueError, "invalid version directory"):
            verify.verify(bad_pages, "v0.10.0", "http://127.0.0.1:9/", timeout=0.1, poll_interval=0.05)

        # Invalid manifest version name.
        bad_name_pages = make_pages(self.tmp / "bad-name-pages")
        (bad_name_pages / "versions.json").write_text(
            json.dumps(
                {
                    "latest": "v0.10.0",
                    "published": [{"name": "../etc", "path": "../etc/", "commit": COMMIT_A}],
                }
            )
            + "\n",
            encoding="utf-8",
        )
        with self.assertRaisesRegex(ValueError, "ref must be main"):
            verify.verify(bad_name_pages, "v0.10.0", "http://127.0.0.1:9/", timeout=0.1, poll_interval=0.05)

        # Symlink escaping Pages directory inside local snapshot.
        outside_secret = self.tmp / "secret.txt"
        outside_secret.write_text("secret\n", encoding="utf-8")
        (self.pages / "reference" / "commands.md").unlink()
        (self.pages / "reference" / "commands.md").symlink_to(outside_secret)
        with self.assertRaisesRegex(ValueError, "symlink"):
            verify.verify(self.pages, "v0.10.0", "http://127.0.0.1:9/", timeout=0.1, poll_interval=0.05)

    def test_verify_canonical_obsolete_path_validation_and_url_encoding(self):
        invalid_paths = (
            "",
            "/",
            "../secret.md",
            "reference/../secret.md",
            "reference/./a.md",
            "reference//a.md",
            "reference/a.md?query=1",
            "reference/a.md#frag",
            "reference/a%20b.md",
            "reference/a b.md",
            "reference/a\tb.md",
            "reference/a\nb.md",
            "reference/a\x00b.md",
            "reference/a\x7fb.md",
            "reference\\a.md",
        )
        for bad in invalid_paths:
            with self.subTest(path=bad):
                with self.assertRaises(ValueError):
                    verify.verify(
                        self.pages,
                        "v0.10.0",
                        "http://127.0.0.1:9/",
                        obsolete_paths=[bad],
                        timeout=0.1,
                        poll_interval=0.05,
                    )

        # Verify percent-encoding is applied consistently on expected and absent URLs.
        pages_enc = make_pages(self.tmp / "pages-enc")
        enc_build = make_build(
            self.tmp / "build-enc",
            "main",
            COMMIT_A,
            reference_files={"my+doc.md": "# Plus\n"},
        )
        with contextlib.redirect_stdout(io.StringIO()):
            publish.compose(enc_build, pages_enc, "main", COMMIT_A, ROOT_URL)
        with _LocalPagesServer(pages_enc) as server:
            with contextlib.redirect_stdout(io.StringIO()):
                verify.verify(
                    pages_enc,
                    "main",
                    server.base_url,
                    obsolete_paths=["reference/old+doc.md"],
                    timeout=2,
                    poll_interval=0.05,
                )
            self.assertIn("/reference/my%2Bdoc.md", server.requested_paths)
            self.assertIn("/main/reference/my%2Bdoc.md", server.requested_paths)
            self.assertIn("/reference/old%2Bdoc.md", server.requested_paths)

    def test_verify_caps_per_request_timeout_to_overall_deadline(self):
        # Slow response sleeps 1.5s, while overall timeout is 0.25s and request_timeout is 20s.
        with _LocalPagesServer(
            self.pages,
            overrides={"index.html": [(200, b"slow", {}, 1.5)]},
        ) as server:
            started = time.monotonic()
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(ValueError, "did not converge"):
                    verify.verify(
                        self.pages,
                        "v0.10.0",
                        server.base_url,
                        timeout=0.25,
                        poll_interval=0.05,
                        request_timeout=20,
                    )
            elapsed = time.monotonic() - started
            self.assertLess(elapsed, 1.0, f"verify exceeded deadline cap: {elapsed:.2f}s")

    def test_verify_cli_with_obsolete_manifest_and_flags(self):
        obsolete_manifest = self.tmp / "obsolete.json"
        obsolete_manifest.write_text(
            json.dumps({"obsolete_paths": ["reference/removed.md"]}) + "\n",
            encoding="utf-8",
        )
        with _LocalPagesServer(self.pages) as server:
            ok = subprocess.run(
                [
                    sys.executable,
                    str(SITE_DIR / "verify.py"),
                    "--pages",
                    str(self.pages),
                    "--ref",
                    "v0.10.0",
                    "--root-url",
                    server.base_url,
                    "--obsolete-manifest",
                    str(obsolete_manifest),
                    "--timeout",
                    "2",
                    "--poll-interval",
                    "0.05",
                ],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(ok.returncode, 0, ok.stderr)

        # CLI exits non-zero when an obsolete path from manifest still serves 200.
        with _LocalPagesServer(
            self.pages,
            overrides={"reference/removed.md": [(200, b"still here")]},
        ) as server:
            failed = subprocess.run(
                [
                    sys.executable,
                    str(SITE_DIR / "verify.py"),
                    "--pages",
                    str(self.pages),
                    "--ref",
                    "v0.10.0",
                    "--root-url",
                    server.base_url,
                    "--obsolete-manifest",
                    str(obsolete_manifest),
                    "--timeout",
                    "0.1",
                    "--poll-interval",
                    "0.05",
                ],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(failed.returncode, 1)
            self.assertIn("reference/removed.md", failed.stderr)


if __name__ == "__main__":
    unittest.main()
