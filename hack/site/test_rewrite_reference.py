#!/usr/bin/env python3
"""Unit tests for hack/site/rewrite_reference.py."""

import contextlib
import io
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import rewrite_reference  # noqa: E402


REPO_ROOT = Path(__file__).resolve().parents[2]
COMMIT_SHA = "30f3c1a32bb1bf7df0869d318df98e04aafa3fca"
ROOT_PREFIX = "https://docs.gpustack.ai/gpustack-operator/v0.9.0/docs"


class RewriteDestinationTest(unittest.TestCase):
    """Verify all six link rewrite classes and destination edge cases."""

    def test_six_classes_version_local(self):
        cases = [
            (
                "public regular page with anchor",
                "../modules/devices/preflight.md#step-1",
                "../docs/modules/devices/preflight/index.md#step-1",
            ),
            (
                "public regular page with query and anchor",
                "../operate/installation-modes.md?tab=helm#modes",
                "../docs/operate/installation-modes/index.md?tab=helm#modes",
            ),
            (
                "public getting-started page",
                "../getting-started/architecture.md",
                "../docs/getting-started/architecture/index.md",
            ),
            (
                "public walkthrough page",
                "../walkthroughs/devices/scheduling.md#step-4",
                "../docs/walkthroughs/devices/scheduling/index.md#step-4",
            ),
            (
                "public section index",
                "../modules/instances/_index.md#network-isolation",
                "../docs/modules/instances/index.md#network-isolation",
            ),
            (
                "public root section index",
                "../_index.md#top",
                "../docs/index.md#top",
            ),
            (
                "sibling reference bare",
                "settings.md#value-ownership",
                "settings.md#value-ownership",
            ),
            (
                "sibling reference dot-slash",
                "./settings.md#value-ownership",
                "settings.md#value-ownership",
            ),
            (
                "sibling reference via parent",
                "../reference/commands.md#worker",
                "commands.md#worker",
            ),
            (
                "sibling reference directory self",
                ".",
                ".",
            ),
            (
                "shared diagram asset",
                "../assets/diagrams/device-resource.svg",
                "../docs/assets/diagrams/device-resource.svg",
            ),
            (
                "shared asset with query",
                "../assets/diagrams/chart.svg?v=2#layer",
                "../docs/assets/diagrams/chart.svg?v=2#layer",
            ),
            (
                "repo source pkg file",
                "../../pkg/nodefeature/unit_resources_preset.yaml#L10",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/pkg/nodefeature/unit_resources_preset.yaml#L10",
            ),
            (
                "repo source spec file",
                "../../specs/2026-11-21-three-zone-docs-governance.md#proposal",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/specs/2026-11-21-three-zone-docs-governance.md#proposal",
            ),
            (
                "repo source deploy directory",
                "../../deploy/gpustack-operator/chart/",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/deploy/gpustack-operator/chart",
            ),
            (
                "repo root directory",
                "../../",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}",
            ),
            (
                "contribute doc",
                "../contribute/development.md#runtime-log-verbosity",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/docs/contribute/development.md#runtime-log-verbosity",
            ),
            (
                "contribute directory",
                "../contribute",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/docs/contribute",
            ),
            (
                "docs README",
                "../README.md#module-map",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/docs/README.md#module-map",
            ),
            (
                "non-markdown helper under docs",
                "../operate/migration/cleanup-v0.5-orphans.sh",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/docs/operate/migration/cleanup-v0.5-orphans.sh",
            ),
            (
                "external https URL",
                "https://kubernetes.io/docs/concepts/?q=1#pods",
                "https://kubernetes.io/docs/concepts/?q=1#pods",
            ),
            (
                "external http URL",
                "http://example.com/status",
                "http://example.com/status",
            ),
            (
                "protocol-relative URL",
                "//cdn.example.com/diagram.svg",
                "//cdn.example.com/diagram.svg",
            ),
            (
                "mailto link",
                "mailto:security@gpustack.ai",
                "mailto:security@gpustack.ai",
            ),
            (
                "pure anchor",
                "#device-manager-preflight",
                "#device-manager-preflight",
            ),
            (
                "query only",
                "?view=raw",
                "?view=raw",
            ),
            (
                "empty destination",
                "",
                "",
            ),
            (
                "root-relative path passthrough",
                "/reference/settings.md#a",
                "/reference/settings.md#a",
            ),
        ]
        for name, raw, expected in cases:
            with self.subTest(name=name):
                got = rewrite_reference.rewrite_destination(
                    raw,
                    docs_prefix="../docs",
                    source_ref=COMMIT_SHA,
                )
                self.assertEqual(got, expected)

    def test_pages_root_prefixes(self):
        cases = [
            (
                "absolute root_url latest prefix on page",
                ROOT_PREFIX,
                "../modules/model-deployment/configuration.md#a",
                f"{ROOT_PREFIX}/modules/model-deployment/configuration/index.md#a",
            ),
            (
                "absolute root_url latest prefix with trailing slash on asset",
                ROOT_PREFIX + "/",
                "../assets/diagrams/flow.svg",
                f"{ROOT_PREFIX}/assets/diagrams/flow.svg",
            ),
            (
                "relative latest/docs prefix on section index",
                "v0.9.0/docs",
                "../modules/instances/_index.md#a",
                "v0.9.0/docs/modules/instances/index.md#a",
            ),
            (
                "sibling stays relative under Pages-root prefix",
                ROOT_PREFIX,
                "./settings.md#top",
                "settings.md#top",
            ),
        ]
        for name, prefix, raw, expected in cases:
            with self.subTest(name=name):
                got = rewrite_reference.rewrite_destination(
                    raw,
                    docs_prefix=prefix,
                    source_ref=COMMIT_SHA,
                )
                self.assertEqual(got, expected)

    def test_destination_validation_errors(self):
        with self.assertRaises(ValueError):
            rewrite_reference.rewrite_destination("../../../escape.md")
        with self.assertRaises(ValueError):
            rewrite_reference.rewrite_destination("../modules/a.md", docs_prefix="/")
        with self.assertRaises(ValueError):
            rewrite_reference.rewrite_destination("../modules/a.md", source_ref="")
        with self.assertRaises(ValueError):
            rewrite_reference.rewrite_destination("../modules/a.md", repo_blob_base="///")

    def test_rebase_destination(self):
        cases = [
            (
                "rewrites version-local doc link",
                "../docs/modules/devices/preflight/index.md#a",
                f"{ROOT_PREFIX}/modules/devices/preflight/index.md#a",
            ),
            (
                "rewrites version-local asset link",
                "../docs/assets/diagrams/x.svg?v=1",
                f"{ROOT_PREFIX}/assets/diagrams/x.svg?v=1",
            ),
            (
                "rewrites bare prefix match",
                "../docs#top",
                f"{ROOT_PREFIX}#top",
            ),
            (
                "preserves sibling reference",
                "settings.md#a",
                "settings.md#a",
            ),
            (
                "preserves GitHub blob URL",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/pkg/foo.go",
                f"https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/pkg/foo.go",
            ),
            (
                "preserves pure anchor",
                "#summary",
                "#summary",
            ),
            (
                "preserves mailto",
                "mailto:ops@example.com",
                "mailto:ops@example.com",
            ),
            (
                "preserves unrelated relative path",
                "../other/file.md",
                "../other/file.md",
            ),
            (
                "preserves empty destination",
                "",
                "",
            ),
        ]
        for name, raw, expected in cases:
            with self.subTest(name=name):
                got = rewrite_reference.rebase_destination(
                    raw,
                    from_prefix="../docs",
                    to_prefix=ROOT_PREFIX,
                )
                self.assertEqual(got, expected)

        with self.assertRaises(ValueError):
            rewrite_reference.rebase_destination("../docs/a.md", from_prefix="", to_prefix=ROOT_PREFIX)
        with self.assertRaises(ValueError):
            rewrite_reference.rebase_destination("../docs/a.md", from_prefix="../docs", to_prefix="")


class RewriteMarkdownTest(unittest.TestCase):
    """Verify Markdown parser rewrites and exemption rules."""

    def test_inline_links_images_titles_parens_angles(self):
        source = (
            "See [Preflight](../modules/devices/preflight.md#a \"Guide Title\"),\n"
            "[Single](../modules/devices/requests.md 'Single Title'),\n"
            "[ParenTitle](../modules/devices/nvidia-mig.md (Paren Title)),\n"
            "[Balanced](../modules/foo_(bar).md#x(1)),\n"
            "[Angled](<../modules/devices/preflight.md#a> \"Angled\"),\n"
            "![Diagram](../assets/diagrams/x.svg),\n"
            "[![Linked](../assets/diagrams/y.svg)](../modules/instances/_index.md#s),\n"
            "[`code[0]`](../operate/installation-modes.md),\n"
            "[Empty]() and [Space](\n  ../modules/rdma/operations.md#q\n).\n"
        )
        expected = (
            "See [Preflight](../docs/modules/devices/preflight/index.md#a \"Guide Title\"),\n"
            "[Single](../docs/modules/devices/requests/index.md 'Single Title'),\n"
            "[ParenTitle](../docs/modules/devices/nvidia-mig/index.md (Paren Title)),\n"
            "[Balanced](../docs/modules/foo_(bar)/index.md#x(1)),\n"
            "[Angled](<../docs/modules/devices/preflight/index.md#a> \"Angled\"),\n"
            "![Diagram](../docs/assets/diagrams/x.svg),\n"
            "[![Linked](../docs/assets/diagrams/y.svg)](../docs/modules/instances/index.md#s),\n"
            "[`code[0]`](../docs/operate/installation-modes/index.md),\n"
            "[Empty]() and [Space](\n  ../docs/modules/rdma/operations/index.md#q\n).\n"
        )
        self.assertEqual(rewrite_reference.rewrite_markdown(source, source_ref=COMMIT_SHA), expected)

    def test_multiline_and_blockquote_links(self):
        source = (
            "See [Instance\n"
            "Metrics](../modules/instances/_index.md#metrics).\n"
            "\n"
            "> Read [Runtime log\n"
            "> verbosity](../contribute/development.md#runtime-log-verbosity).\n"
        )
        expected = (
            "See [Instance\n"
            "Metrics](../docs/modules/instances/index.md#metrics).\n"
            "\n"
            "> Read [Runtime log\n"
            "> verbosity](https://github.com/gpustack/gpustack-operator/blob/"
            f"{COMMIT_SHA}/docs/contribute/development.md#runtime-log-verbosity).\n"
        )
        self.assertEqual(rewrite_reference.rewrite_markdown(source, source_ref=COMMIT_SHA), expected)

    def test_reference_definitions_and_gfm_footnotes(self):
        source = (
            "See [Config][cfg], [Idx][idx], [Repo][repo], and note[^1].\n"
            "\n"
            "   [cfg]: ../modules/model-deployment/configuration.md#a \"Config\"\n"
            "[idx]: <../modules/instances/_index.md#net>\n"
            "[repo]:\n"
            "  ../../pkg/worker/main.go\n"
            "[bad]: ../modules/a.md extra non-title tokens [Inline](../modules/devices/preflight.md)\n"
            "\n"
            "[^1]: Footnote with [Preflight](../modules/devices/preflight.md#a) and `[skip](../modules/x.md)`.\n"
            "    Continuation with [Settings](./settings.md#s) and [Asset](../assets/diagrams/a.svg).\n"
        )
        expected = (
            "See [Config][cfg], [Idx][idx], [Repo][repo], and note[^1].\n"
            "\n"
            "   [cfg]: ../docs/modules/model-deployment/configuration/index.md#a \"Config\"\n"
            "[idx]: <../docs/modules/instances/index.md#net>\n"
            "[repo]:\n"
            f"  https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/pkg/worker/main.go\n"
            "[bad]: ../modules/a.md extra non-title tokens [Inline](../docs/modules/devices/preflight/index.md)\n"
            "\n"
            "[^1]: Footnote with [Preflight](../docs/modules/devices/preflight/index.md#a) and `[skip](../modules/x.md)`.\n"
            "    Continuation with [Settings](settings.md#s) and [Asset](../docs/assets/diagrams/a.svg).\n"
        )
        self.assertEqual(rewrite_reference.rewrite_markdown(source, source_ref=COMMIT_SHA), expected)

    def test_fenced_code_blocks_exempt(self):
        source = (
            "Before [Real](../modules/devices/preflight.md).\n"
            "```markdown\n"
            "[Skip1](../modules/devices/preflight.md)\n"
            "<!-- not a comment opener\n"
            "```\n"
            "   ~~~~md\n"
            "[Skip2](../modules/devices/preflight.md)\n"
            "~~~\n"
            "[StillInsideTilde](../modules/devices/preflight.md)\n"
            "~~~~~\n"
            "> ```yaml\n"
            "> [SkipBlockquote](../modules/devices/preflight.md)\n"
            "> ```\n"
            "> ```\n"
            "> [SkipEndedByPlainLine](../modules/devices/preflight.md)\n"
            "AfterBlockquote [Real2](../modules/devices/preflight.md).\n"
            "```not a fence because `backtick` in info string``` and [Real3](../modules/devices/preflight.md).\n"
            "```\n"
            "[UnclosedFence](../modules/devices/preflight.md)\n"
        )
        expected = (
            "Before [Real](../docs/modules/devices/preflight/index.md).\n"
            "```markdown\n"
            "[Skip1](../modules/devices/preflight.md)\n"
            "<!-- not a comment opener\n"
            "```\n"
            "   ~~~~md\n"
            "[Skip2](../modules/devices/preflight.md)\n"
            "~~~\n"
            "[StillInsideTilde](../modules/devices/preflight.md)\n"
            "~~~~~\n"
            "> ```yaml\n"
            "> [SkipBlockquote](../modules/devices/preflight.md)\n"
            "> ```\n"
            "> ```\n"
            "> [SkipEndedByPlainLine](../modules/devices/preflight.md)\n"
            "AfterBlockquote [Real2](../docs/modules/devices/preflight/index.md).\n"
            "```not a fence because `backtick` in info string``` and [Real3](../docs/modules/devices/preflight/index.md).\n"
            "```\n"
            "[UnclosedFence](../modules/devices/preflight.md)\n"
        )
        self.assertEqual(rewrite_reference.rewrite_markdown(source, source_ref=COMMIT_SHA), expected)

    def test_inline_code_comments_escapes_and_malformed_brackets(self):
        source = (
            "Inline `[skip1](../modules/a.md)` and `` `[skip2](../modules/b.md)` `` "
            "and ``` ``[skip3](../modules/c.md)`` ```.\n"
            "Unmatched ` backtick then [Real1](../modules/a.md).\n\n"
            "Escaped \\`[Real2](../modules/b.md)\\` and \\[Escaped](../modules/c.md).\n"
            "Inline <!-- [CommentSkip](../modules/d.md) --> and [Real3](../modules/e.md).\n"
            "Code with comment opener `<!--` then [Real4](../modules/f.md).\n"
            "<!--\n"
            "flowchart LR\n"
            "  A[Node] -> B[../modules/g.md]\n"
            "  ```\n"
            "  [MermaidSkip](../modules/h.md)\n"
            "-->\n"
            "After comment [Real5](../modules/i.md).\n"
            "array[0] and [unclosed bracket then [Real6](../modules/j.md).\n"
            "[broken dest](../modules/k.md \"unclosed title and [Real7](../modules/l.md)\n"
            "<!-- unclosed comment [SkipEOF](../modules/m.md)\n"
        )
        expected = (
            "Inline `[skip1](../modules/a.md)` and `` `[skip2](../modules/b.md)` `` "
            "and ``` ``[skip3](../modules/c.md)`` ```.\n"
            "Unmatched ` backtick then [Real1](../docs/modules/a/index.md).\n\n"
            "Escaped \\`[Real2](../docs/modules/b/index.md)\\` and \\[Escaped](../modules/c.md).\n"
            "Inline <!-- [CommentSkip](../modules/d.md) --> and [Real3](../docs/modules/e/index.md).\n"
            "Code with comment opener `<!--` then [Real4](../docs/modules/f/index.md).\n"
            "<!--\n"
            "flowchart LR\n"
            "  A[Node] -> B[../modules/g.md]\n"
            "  ```\n"
            "  [MermaidSkip](../modules/h.md)\n"
            "-->\n"
            "After comment [Real5](../docs/modules/i/index.md).\n"
            "array[0] and [unclosed bracket then [Real6](../docs/modules/j/index.md).\n"
            "[broken dest](../modules/k.md \"unclosed title and [Real7](../docs/modules/l/index.md)\n"
            "<!-- unclosed comment [SkipEOF](../modules/m.md)\n"
        )
        self.assertEqual(rewrite_reference.rewrite_markdown(source, source_ref=COMMIT_SHA), expected)

    def test_rebase_markdown_preserves_exempt_blocks(self):
        staged = (
            "See [Guide](../docs/modules/devices/preflight/index.md#a), "
            "[Asset](../docs/assets/diagrams/x.svg), [Sibling](settings.md#b), "
            f"[Code](https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/pkg/x.go).\n"
            "Inline `../docs/modules/a/index.md` and `[c](../docs/modules/a/index.md)`.\n"
            "```md\n"
            "[Fence](../docs/modules/a/index.md)\n"
            "```\n"
            "<!-- [Comment](../docs/modules/a/index.md) -->\n"
            "[ref]: ../docs/modules/instances/index.md#net\n"
        )
        expected = (
            f"See [Guide]({ROOT_PREFIX}/modules/devices/preflight/index.md#a), "
            f"[Asset]({ROOT_PREFIX}/assets/diagrams/x.svg), [Sibling](settings.md#b), "
            f"[Code](https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/pkg/x.go).\n"
            "Inline `../docs/modules/a/index.md` and `[c](../docs/modules/a/index.md)`.\n"
            "```md\n"
            "[Fence](../docs/modules/a/index.md)\n"
            "```\n"
            "<!-- [Comment](../docs/modules/a/index.md) -->\n"
            f"[ref]: {ROOT_PREFIX}/modules/instances/index.md#net\n"
        )
        got = rewrite_reference.rebase_markdown(
            staged,
            from_prefix="../docs",
            to_prefix=ROOT_PREFIX,
        )
        self.assertEqual(got, expected)

    def test_parser_edge_cases_and_crlf(self):
        cases = [
            (
                "escaped bracket and comment and unmatched backtick inside link label",
                "[a \\] <!-- ] --> ` b](../modules/devices/preflight.md \"t \\\"q\\\"\")\n",
                "[a \\] <!-- ] --> ` b](../docs/modules/devices/preflight/index.md \"t \\\"q\\\"\")\n",
            ),
            (
                "escaped parens in unbracketed dest and escaped angle in angled dest",
                "[p](../modules/a\\(1\\).md) [q](<../modules/b\\>.md>)\n",
                "[p](../docs/modules/a\\(1\\)/index.md) [q](<../docs/modules/b\\>/index.md>)\n",
            ),
            (
                "ref def with escaped label, CRLF, and trailing spaces after title",
                "[a\\]b]:\r\n  ../modules/devices/preflight.md \"T\"   \r\n\r\n[ok](../modules/a.md)\r\n",
                "[a\\]b]:\r\n  ../docs/modules/devices/preflight/index.md \"T\"   \r\n\r\n[ok](../docs/modules/a/index.md)\r\n",
            ),
            (
                "malformed blank line in label",
                "[blank\n\nlabel](../modules/a.md)\n",
                "[blank\n\nlabel](../modules/a.md)\n",
            ),
            (
                "malformed unclosed comment in label",
                "[unclosed comment <!--](../modules/a.md)\n",
                "[unclosed comment <!--](../modules/a.md)\n",
            ),
            (
                "malformed blank line before dest",
                "[blank dest](\n\n../modules/a.md)\n",
                "[blank dest](\n\n../modules/a.md)\n",
            ),
            (
                "malformed unclosed angle dest",
                "[unclosed angle](<../modules/a.md\n)\n",
                "[unclosed angle](<../modules/a.md\n)\n",
            ),
            (
                "malformed angle dest at eof",
                "[angle eof](<../modules/a.md",
                "[angle eof](<../modules/a.md",
            ),
            (
                "malformed nested angle dest",
                "[nested angle](<../modules/<a.md>)\n",
                "[nested angle](<../modules/<a.md>)\n",
            ),
            (
                "malformed unbalanced paren dest",
                "[unbalanced paren](../modules/a(b.md)\n",
                "[unbalanced paren](../modules/a(b.md)\n",
            ),
            (
                "malformed blank line after dest",
                "[blank after dest](../modules/a.md\n\n)\n",
                "[blank after dest](../modules/a.md\n\n)\n",
            ),
            (
                "malformed nested paren title",
                "[nested paren title](../modules/a.md (a (b)))\n",
                "[nested paren title](../modules/a.md (a (b)))\n",
            ),
            (
                "malformed blank line in title",
                "[blank in title](../modules/a.md \"a\n\nb\")\n",
                "[blank in title](../modules/a.md \"a\n\nb\")\n",
            ),
            (
                "malformed blank line after title",
                "[blank after title](../modules/a.md \"t\"\n\n)\n",
                "[blank after title](../modules/a.md \"t\"\n\n)\n",
            ),
            (
                "malformed unclosed dest at eof",
                "[eof dest](../modules/a.md",
                "[eof dest](../modules/a.md",
            ),
            (
                "malformed ref def blank label",
                "[bad ref\n\nlabel]: ../modules/a.md\n",
                "[bad ref\n\nlabel]: ../modules/a.md\n",
            ),
            (
                "malformed ref def empty dest",
                "[bad ref empty]: \n",
                "[bad ref empty]: \n",
            ),
            (
                "malformed ref def unclosed title",
                "[bad ref title]: ../modules/a.md \"unclosed\n",
                "[bad ref title]: ../modules/a.md \"unclosed\n",
            ),
            (
                "malformed ref def extra tokens after title",
                "[bad ref extra]: ../modules/a.md \"t\" extra\n",
                "[bad ref extra]: ../modules/a.md \"t\" extra\n",
            ),
            (
                "malformed ref def eof after colon",
                "[eof]:",
                "[eof]:",
            ),
            (
                "unmatched backtick at eof",
                "unmatched ` at eof",
                "unmatched ` at eof",
            ),
        ]
        for name, source, expected in cases:
            with self.subTest(name=name):
                self.assertEqual(rewrite_reference.rewrite_markdown(source, source_ref=COMMIT_SHA), expected)

    def test_real_corpus_smoke(self):
        ref_dir = REPO_ROOT / "docs/reference"
        files = sorted(p for p in ref_dir.iterdir() if p.is_file() and p.suffix == ".md")
        self.assertTrue(files)
        for path in files:
            with self.subTest(file=path.name):
                raw = path.read_text(encoding="utf-8")
                once = rewrite_reference.rewrite_markdown(raw, source_ref=COMMIT_SHA)
                twice = rewrite_reference.rewrite_markdown(raw, source_ref=COMMIT_SHA)
                self.assertEqual(once, twice)
                self.assertNotIn("](../modules/", once)
                self.assertNotIn("](../operate/", once)
                self.assertNotIn("](../contribute/", once)
                self.assertNotIn("](../getting-started/", once)
                self.assertNotIn("](../walkthroughs/", once)


class StageReferenceDirAndCLITest(unittest.TestCase):
    """Verify fresh-mirror staging, flat enforcement, fail-before-write, and CLI."""

    def test_fresh_mirror_removes_stale_files_and_dirs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            src = root / "src"
            dst = root / "dst"
            src.mkdir()
            (src / "commands.md").write_text("[Modes](../operate/installation-modes.md)\n", encoding="utf-8")
            (src / "settings.md").write_text("[Cmds](./commands.md#worker)\n", encoding="utf-8")
            (src / ".DS_Store").write_bytes(b"ignored")

            dst.mkdir()
            (dst / "deleted.md").write_text("stale\n", encoding="utf-8")
            stale_dir = dst / "nested"
            stale_dir.mkdir()
            (stale_dir / "old.md").write_text("stale\n", encoding="utf-8")

            staged = rewrite_reference.stage_reference_dir(src, dst, source_ref=COMMIT_SHA)
            self.assertEqual(staged, ["commands.md", "settings.md"])
            self.assertEqual(sorted(p.name for p in dst.iterdir()), ["commands.md", "settings.md"])
            self.assertEqual(
                (dst / "commands.md").read_text(encoding="utf-8"),
                "[Modes](../docs/operate/installation-modes/index.md)\n",
            )
            self.assertEqual(
                (dst / "settings.md").read_text(encoding="utf-8"),
                "[Cmds](commands.md#worker)\n",
            )

            # Remove one source file and re-stage: deleted file must disappear.
            (src / "settings.md").unlink()
            staged_again = rewrite_reference.stage_reference_dir(src, dst, source_ref=COMMIT_SHA)
            self.assertEqual(staged_again, ["commands.md"])
            self.assertEqual(sorted(p.name for p in dst.iterdir()), ["commands.md"])

    def test_rejects_subdirectories_and_same_dir_and_fail_before_write(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            src = root / "src"
            dst = root / "dst"
            src.mkdir()
            dst.mkdir()
            (dst / "keep.md").write_text("untouched\n", encoding="utf-8")

            # Missing source dir
            with self.assertRaises(ValueError):
                rewrite_reference.stage_reference_dir(root / "missing", dst)

            # Same source and destination
            with self.assertRaises(ValueError):
                rewrite_reference.stage_reference_dir(src, src)

            # Destination exists as a regular file
            not_a_dir = root / "file.txt"
            not_a_dir.write_text("x", encoding="utf-8")
            with self.assertRaises(ValueError):
                rewrite_reference.stage_reference_dir(src, not_a_dir)

            # Nested subdirectory rejected before mutating dst
            (src / "nested").mkdir()
            with self.assertRaises(ValueError):
                rewrite_reference.stage_reference_dir(src, dst)
            self.assertEqual((dst / "keep.md").read_text(encoding="utf-8"), "untouched\n")
            (src / "nested").rmdir()

            # Bad escaping link rejected before mutating dst
            (src / "bad.md").write_text("[Bad](../../../outside.md)\n", encoding="utf-8")
            with self.assertRaises(ValueError):
                rewrite_reference.stage_reference_dir(src, dst)
            self.assertEqual((dst / "keep.md").read_text(encoding="utf-8"), "untouched\n")

    def test_rebase_staging_and_cli(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            src = root / "docs-ref"
            ver_dst = root / "site-ref"
            pages_dst = root / "pages-ref"
            src.mkdir()
            (src / "commands.md").write_text(
                "[Preflight](../modules/devices/preflight.md#a) "
                "[Dev](../contribute/development.md)\n",
                encoding="utf-8",
            )

            # Run CLI in rewrite mode with HUGO_PARAMS_SOURCEREF env default.
            with mock.patch.dict(os.environ, {"HUGO_PARAMS_SOURCEREF": COMMIT_SHA}, clear=False):
                rc = rewrite_reference.main(["--source", str(src), "--destination", str(ver_dst)])
            self.assertEqual(rc, 0)
            self.assertEqual(
                (ver_dst / "commands.md").read_text(encoding="utf-8"),
                "[Preflight](../docs/modules/devices/preflight/index.md#a) "
                f"[Dev](https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/docs/contribute/development.md)\n",
            )

            # Run CLI in rebase mode for Pages root /reference/.
            rc = rewrite_reference.main([
                "--source", str(ver_dst),
                "--destination", str(pages_dst),
                "--rebase-from", "../docs",
                "--docs-prefix", ROOT_PREFIX,
            ])
            self.assertEqual(rc, 0)
            self.assertEqual(
                (pages_dst / "commands.md").read_text(encoding="utf-8"),
                f"[Preflight]({ROOT_PREFIX}/modules/devices/preflight/index.md#a) "
                f"[Dev](https://github.com/gpustack/gpustack-operator/blob/{COMMIT_SHA}/docs/contribute/development.md)\n",
            )

            # CLI error path exits non-zero cleanly via SystemExit from parser.exit.
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as ctx:
                rewrite_reference.main(["--source", str(root / "nope"), "--destination", str(pages_dst)])
            self.assertEqual(ctx.exception.code, 1)
            self.assertIn("source directory does not exist", stderr.getvalue())

            # Subprocess execution smoke test.
            proc = subprocess.run(
                [
                    sys.executable,
                    str(Path(__file__).resolve().parent / "rewrite_reference.py"),
                    "--source", str(src),
                    "--destination", str(root / "sub-dst"),
                    "--source-ref", COMMIT_SHA,
                ],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertTrue((root / "sub-dst/commands.md").is_file())


if __name__ == "__main__":
    unittest.main()
