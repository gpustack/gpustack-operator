#!/usr/bin/env python3
"""Unit tests for hack/check/agent-index.py and hack/check/site-links.py."""

import importlib.util
import io
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from urllib.parse import urlsplit

CHECK_DIR = Path(__file__).resolve().parent
REPO_ROOT = CHECK_DIR.parent.parent


def _load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


agent_index = _load_module("agent_index", CHECK_DIR / "agent-index.py")
site_links = _load_module("site_links", CHECK_DIR / "site-links.py")
rewrite_reference = _load_module("rewrite_reference", REPO_ROOT / "hack/site/rewrite_reference.py")

MODULE_CAPABILITIES = (
    "devices",
    "rdma",
    "topology",
    "kv-cache",
    "model-delivery",
    "model-deployment",
    "instances",
)


def _write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def _module_map_markdown() -> str:
    blocks = []
    for mod in MODULE_CAPABILITIES:
        title = mod.replace("-", " ").title()
        blocks.append(
            f"### {title}\n\n"
            f"- ID: `{mod}`\n"
            f"- Use for: operate {mod}\n"
            f"- Aliases: `{mod}`\n"
            f"- Guides: [{title}](modules/{mod}/_index.md)\n"
            f"- Specs: [Spec](../specs/example.md)\n"
            f"- Code: [pkg](../pkg/placeholder.go)\n"
            f"- Skills: [docs](../.agents/skills/gpustack-operator-docs/SKILL.md)\n"
        )
    for extra_id, title, guide in (
        ("api", "API Changes", "getting-started/architecture.md"),
        ("installation", "Installation", "getting-started/architecture.md"),
        ("development", "Development", "contribute/development.md"),
    ):
        blocks.append(
            f"### {title}\n\n"
            f"- ID: `{extra_id}`\n"
            f"- Use for: {extra_id} tasks\n"
            f"- Aliases: `{extra_id}`\n"
            f"- Guides: [Guide]({guide})\n"
            f"- Code: [pkg](../pkg/placeholder.go)\n"
            f"- Skills: [docs](../.agents/skills/gpustack-operator-docs/SKILL.md)\n"
        )
    return "\n".join(blocks)


def _build_strict_fixture(
    root: Path,
    *,
    base_url: str = "/",
    source_ref: str = "abc1234",
) -> Path:
    """Create a complete miniature three-zone repository + built Hugo public output."""
    _write(root / "pkg/placeholder.go", "package pkg\n")
    _write(root / "specs/example.md", "# Spec\n")
    _write(
        root / ".agents/skills/gpustack-operator-docs/SKILL.md",
        "# Docs Skill\n\nReads docs/README.md.\n",
    )
    _write(root / "docs/assets/diagrams/arch.svg", "<svg></svg>\n")

    _write(
        root / "docs/getting-started/architecture.md",
        "# Architecture\n\n"
        "Intro with footnote.[^1]\n\n"
        "## Overview\n\n"
        "See [Devices](../modules/devices/_index.md#overview) and "
        "![Diagram](../assets/diagrams/arch.svg).\n\n"
        "### Retry\n\n"
        "First retry.\n\n"
        "### Retry-1\n\n"
        "Explicit retry-1.\n\n"
        "### Retry\n\n"
        "Third retry allocates retry-2.\n\n"
        "[^1]: Footnote citing [Devices](../modules/devices/_index.md#overview).\n\n"
        "    Footnote continuation citing [Devices](../modules/devices/_index.md#overview).\n",
    )

    all_pages_rows = [
        "| [Architecture](getting-started/architecture.md) | Core architecture |",
    ]
    for mod in MODULE_CAPABILITIES:
        title = mod.replace("-", " ").title()
        _write(
            root / f"docs/modules/{mod}/_index.md",
            f"# {title}\n\n## Overview\n\nModule overview for {mod}.\n",
        )
        all_pages_rows.append(f"| [{title}](modules/{mod}/_index.md) | {title} overview |")

    _write(
        root / "docs/reference/README.md",
        "# Reference Hub\n\n"
        "## Task routing\n\n"
        "| Task | Reference | Public |\n"
        "| --- | --- | --- |\n"
        "| Settings | [Settings](settings.md#flags) | [Architecture](../getting-started/architecture.md#overview) |\n",
    )
    _write(
        root / "docs/reference/settings.md",
        "# Settings\n\n"
        "Evidence: contract.[^1]\n\n"
        "## Flags\n\n"
        "Link to [first retry](../getting-started/architecture.md#retry) and "
        "[collision-free\nthird retry](../getting-started/architecture.md#retry-2) and "
        "[contrib](../contribute/development.md#commands) and "
        "[asset](../assets/diagrams/arch.svg) and [ref-style][hub-link].\n\n"
        "[hub-link]: README.md#task-routing\n\n"
        "```markdown\n"
        "# Fake Fence Heading\n"
        "[Broken in fence](missing.md#nope)\n"
        "```\n\n"
        "    # Fake Indented Heading\n"
        "    [Broken in indent](missing.md#nope)\n\n"
        "Inline `[Broken in code](missing.md#nope)` and `` `# Not Heading` ``.\n\n"
        "<!--\n"
        "# Fake Comment Heading\n"
        "[Broken in comment](missing.md#nope)\n"
        "-->\n\n"
        "[^1]: Footnote link to [Architecture](../getting-started/architecture.md#overview).\n\n"
        "    Continuation link to [Architecture](../getting-started/architecture.md#retry-2).\n",
    )

    _write(
        root / "docs/contribute/development.md",
        "# Development\n\n## Commands\n\nRun make lint docs.\n",
    )

    _write(root / "site/content/_index.md", "---\ntitle: GPUStack Operator\n---\n\nHome.\n")
    _write(root / "site/content/docs/_index.md", "---\ntitle: Documentation\n---\n\n# Documentation\n")
    _write(
        root / "site/content/docs/getting-started/_index.md",
        "---\ntitle: Getting Started\n---\n\n# Getting Started\n",
    )

    _write(
        root / "docs/README.md",
        "# Operator Docs\n\n"
        "## Module map\n\n"
        + _module_map_markdown()
        + "\n## All pages\n\n"
        "| Page | Description |\n"
        "| --- | --- |\n"
        + "\n".join(all_pages_rows)
        + "\n\n## Reference\n\n"
        "| Page | Description |\n"
        "| --- | --- |\n"
        "| [Reference Hub](reference/README.md) | Flat LLM navigation hub |\n"
        "| [Settings](reference/settings.md) | Runtime settings contract |\n\n"
        "## Contribute\n\n"
        "| Page | Description |\n"
        "| --- | --- |\n"
        "| [Development](contribute/development.md) | Build and lint workflow |\n",
    )

    public = root / "public"
    parsed = urlsplit(base_url)
    prefix = parsed.path.rstrip("/") + "/"
    origin = f"{parsed.scheme}://{parsed.netloc}" if parsed.scheme and parsed.netloc else ""

    _write(
        public / "site-version.json",
        json.dumps({"version": "main", "sourceRef": source_ref, "baseURL": base_url}),
    )
    _write(public / "docs/assets/diagrams/arch.svg", "<svg></svg>\n")
    _write(
        public / "index.html",
        f'<html><body><a href="{prefix}docs/">Docs</a></body></html>\n',
    )
    _write(
        public / "docs/index.html",
        f'<html><body><h1 id="documentation">Documentation</h1>'
        f'<a href="{prefix}docs/getting-started/architecture/#overview">Arch</a></body></html>\n',
    )
    _write(public / "docs/index.md", "# Documentation\n")
    _write(
        public / "docs/getting-started/index.html",
        f'<html><body><h1 id="getting-started">Getting Started</h1>'
        f'<a href="{prefix}docs/getting-started/architecture/">Arch</a></body></html>\n',
    )
    _write(public / "docs/getting-started/index.md", "# Getting Started\n")
    _write(
        public / "docs/getting-started/architecture/index.html",
        f'<html><body><h1 id="architecture">Architecture</h1>'
        f'<h2 id="overview">Overview</h2>'
        f'<h3 id="retry">Retry</h3>'
        f'<h3 id="retry-1">Retry-1</h3>'
        f'<h3 id="retry-2">Retry</h3>'
        f'<img src="{prefix}docs/assets/diagrams/arch.svg">'
        f'<a href="{prefix}docs/modules/devices/#overview">Devices</a></body></html>\n',
    )
    _write(
        public / "docs/getting-started/architecture/index.md",
        "# Architecture\n\n"
        "Intro with footnote.[^1]\n\n"
        "## Overview\n\n"
        f"See [Devices]({prefix}docs/modules/devices/index.md#overview) and "
        f"![Diagram]({prefix}docs/assets/diagrams/arch.svg).\n\n"
        "### Retry\n\n"
        "First retry.\n\n"
        "### Retry-1\n\n"
        "Explicit retry-1.\n\n"
        "### Retry\n\n"
        "Third retry allocates retry-2.\n\n"
        f"[^1]: Footnote citing [Devices]({prefix}docs/modules/devices/index.md#overview).\n\n"
        f"    Footnote continuation citing [Devices]({prefix}docs/modules/devices/index.md#overview).\n",
    )

    search_entries = [
        {"title": "Documentation", "url": f"{prefix}docs/", "content": "Documentation"},
        {"title": "Getting Started", "url": f"{prefix}docs/getting-started/", "content": "Getting Started"},
        {
            "title": "Architecture",
            "url": f"{prefix}docs/getting-started/architecture/",
            "content": "Architecture",
        },
    ]
    sitemap_urls = [
        f"{origin}{prefix}",
        f"{origin}{prefix}docs/",
        f"{origin}{prefix}docs/getting-started/",
        f"{origin}{prefix}docs/getting-started/architecture/",
    ]

    for mod in MODULE_CAPABILITIES:
        title = mod.replace("-", " ").title()
        _write(
            public / f"docs/modules/{mod}/index.html",
            f'<html><body><h1 id="{mod}">{title}</h1><h2 id="overview">Overview</h2></body></html>\n',
        )
        _write(
            public / f"docs/modules/{mod}/index.md",
            f"# {title}\n\n## Overview\n\nModule overview for {mod}.\n",
        )
        search_entries.append(
            {"title": title, "url": f"{prefix}docs/modules/{mod}/", "content": title}
        )
        sitemap_urls.append(f"{origin}{prefix}docs/modules/{mod}/")

    _write(public / "index.json", json.dumps(search_entries))
    sitemap_body = "".join(f"<url><loc>{u}</loc></url>" for u in sitemap_urls)
    _write(
        public / "sitemap.xml",
        '<?xml version="1.0" encoding="utf-8"?>'
        f'<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">{sitemap_body}</urlset>\n',
    )

    rewrite_reference.stage_reference_dir(
        root / "docs/reference",
        public / "reference",
        docs_prefix="../docs",
        source_ref=source_ref,
    )

    llms_lines = [
        "# GPUStack Operator",
        "",
        "> Summary.",
        "",
        "## Getting Started",
        "",
        f"- [Architecture]({origin}{prefix}docs/getting-started/architecture/index.md): Core architecture",
    ]
    for mod in MODULE_CAPABILITIES:
        title = mod.replace("-", " ").title()
        llms_lines.append(
            f"- [{title}]({origin}{prefix}docs/modules/{mod}/index.md): {title} overview"
        )
    llms_lines.extend(
        [
            "",
            "## Reference",
            "",
            f"- [Reference Hub]({origin}{prefix}reference/README.md): Flat LLM navigation hub",
            f"- [Settings]({origin}{prefix}reference/settings.md): Runtime settings contract",
            "",
            "## Optional",
            "",
            f"- [Repository navigation](https://github.com/gpustack/gpustack-operator/blob/{source_ref}/docs/README.md): Module relationships, design records, code entry points and applicable skills for contributors.",
            f"- [Development](https://github.com/gpustack/gpustack-operator/blob/{source_ref}/docs/contribute/development.md): Build and lint workflow",
        ]
    )
    _write(public / "llms.txt", "\n".join(llms_lines) + "\n")
    return public


class AgentIndexAndSiteLinksTests(unittest.TestCase):
    def _run_agent_index(self, root: Path, public: Path | None, base_url: str = "/") -> tuple[bool, str]:
        buf = io.StringIO()
        with redirect_stdout(buf):
            failed = agent_index.check(root, public, base_url)
        return failed, buf.getvalue()

    def _run_site_links(
        self, public: Path, base_url: str = "/", *, strict: bool = False
    ) -> tuple[bool, str]:
        out = io.StringIO()
        err = io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            failed = site_links.main(public, base_url, strict_three_zone=strict)
        return bool(failed), err.getvalue() + out.getvalue()

    def test_shared_retired_module_slugs_single_source(self) -> None:
        self.assertEqual(
            Path(agent_index.site_links_shared.__file__).resolve(),
            (CHECK_DIR / "site-links.py").resolve(),
        )
        self.assertEqual(agent_index.RETIRED_MODULE_SLUGS, site_links.RETIRED_MODULE_SLUGS)
        self.assertEqual(len(site_links.RETIRED_MODULE_SLUGS), 14)

    def test_strict_happy_path_root_and_versioned_base_url(self) -> None:
        for base_url in ("/", "https://docs.gpustack.ai/gpustack-operator/main/"):
            with self.subTest(base_url=base_url), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root, base_url=base_url)
                failed, output = self._run_agent_index(root, public, base_url)
                self.assertFalse(failed, output)
                link_failed, link_out = self._run_site_links(public, base_url)
                self.assertFalse(link_failed, link_out)

    def test_legacy_pre_cutover_mode_remains_compatible(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url="/")
            (root / "docs/reference/README.md").unlink()
            (public / "reference/README.md").unlink()
            (public / "reference/settings.md").unlink()
            readme = (root / "docs/README.md").read_text(encoding="utf-8")
            readme = readme.split("\n## Reference\n")[0]
            readme += (
                "\n| [Settings](reference/settings.md) | Runtime settings contract |\n"
                "| [Development](contribute/development.md) | Build and lint workflow |\n"
            )
            _write(root / "docs/README.md", readme)
            _write(root / "docs/reference/settings.md", "# Settings\n\n## Flags\n\nSimple flags.\n")
            _write(
                public / "docs/reference/settings/index.md",
                "# Settings\n\n## Flags\n\nSimple flags.\n",
            )
            _write(
                public / "docs/contribute/development/index.md",
                "# Development\n\n## Commands\n\nRun make lint docs.\n",
            )
            llms = (public / "llms.txt").read_text(encoding="utf-8").split("\n## Reference\n")[0]
            llms += (
                "\n- [Settings](/docs/reference/settings/index.md): Runtime settings contract\n"
                "- [Development](/docs/contribute/development/index.md): Build and lint workflow\n"
            )
            _write(public / "llms.txt", llms)

            failed, output = self._run_agent_index(root, public, "/")
            self.assertFalse(failed, output)
            link_failed, link_out = self._run_site_links(public, "/")
            self.assertFalse(link_failed, link_out)

    def test_zone_html_leaks_and_retired_urls_rejected(self) -> None:
        leaks = (
            "docs/reference/index.html",
            "docs/reference/settings/index.html",
            "docs/contribute/index.html",
            "docs/contribute/development/index.html",
            "reference/index.html",
            "docs/modules/devices/discovery/index.html",
            "docs/modules/model-deployment/parameterize/index.html",
        )
        for rel in leaks:
            with self.subTest(leak=rel), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root)
                _write(public / rel, "<html><body>leaked</body></html>\n")
                failed, output = self._run_agent_index(root, public, "/")
                self.assertTrue(failed)
                self.assertIn("excluded or retired HTML output", output)
                link_failed, link_out = self._run_site_links(public, "/")
                self.assertTrue(link_failed)
                self.assertIn("HTML page forbidden in excluded or retired zone", link_out)

    def test_site_links_rejects_public_html_links_into_excluded_zones_and_normalized_origins(self) -> None:
        bad_hrefs = (
            "/reference/README.md",
            "/gpustack-operator/reference/settings.md",
            "../../../reference/settings.md",
            "/docs/reference/settings/",
            "/docs/contribute/development/",
            "/docs/%72eference/settings/",
            "https://docs.gpustack.ai/gpustack-operator/reference/settings.md",
            "HTTPS://DOCS.GPUSTACK.AI:443/gpustack-operator/reference/settings.md",
            "https://docs.gpustack.ai/gpustack-operator/main/reference/settings.md",
            "HTTPS://DOCS.GPUSTACK.AI:443/reference/README.md",
            "http://docs.gpustack.ai:80/docs/contribute/development/",
            "//docs.gpustack.ai/gpustack-operator/reference/settings.md",
            "//docs.gpustack.ai/gpustack-operator/main/reference/settings.md",
            "//DOCS.GPUSTACK.AI:443/docs/reference/settings/",
        )
        for href in bad_hrefs:
            with self.subTest(href=href), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root)
                arch_html = public / "docs/getting-started/architecture/index.html"
                arch_html.write_text(
                    f'<html><body><h1 id="architecture">Architecture</h1>'
                    f'<a href="{href}">Bad</a></body></html>\n',
                    encoding="utf-8",
                )
                link_failed, link_out = self._run_site_links(public, "/")
                self.assertTrue(link_failed, f"site-links allowed {href}: {link_out}")
                self.assertIn("links to excluded zone", link_out)

    def test_index_json_and_sitemap_zone_isolation_count_and_authority(self) -> None:
        # 1. Isolated leak check: replace an existing entry so total count stays unchanged
        for bad_url in (
            "/docs/reference/settings/",
            "/docs/contribute/development/",
            "/reference/README.md",
            "/docs/modules/devices/discovery/",
        ):
            with self.subTest(index_leak=bad_url), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root)
                entries = json.loads((public / "index.json").read_text(encoding="utf-8"))
                entries[-1] = {"title": "Leak", "url": bad_url, "content": "leak"}
                _write(public / "index.json", json.dumps(entries))
                failed, output = self._run_agent_index(root, public, "/")
                self.assertTrue(failed)
                self.assertIn("index.json: leaked excluded or retired entry", output)

        # 2. Foreign authority in index.json and sitemap.xml
        for base_url, foreign_url in (
            ("/", "https://evil.example/docs/getting-started/architecture/"),
            (
                "https://docs.gpustack.ai/gpustack-operator/main/",
                "https://evil.example/gpustack-operator/main/docs/getting-started/architecture/",
            ),
        ):
            with self.subTest(foreign_url=foreign_url), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root, base_url=base_url)
                entries = json.loads((public / "index.json").read_text(encoding="utf-8"))
                entries[0]["url"] = foreign_url
                _write(public / "index.json", json.dumps(entries))
                failed, output = self._run_agent_index(root, public, base_url)
                self.assertTrue(failed)
                self.assertIn("index.json: entry foreign origin", output)

            with self.subTest(sitemap_foreign=foreign_url), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root, base_url=base_url)
                xml = (public / "sitemap.xml").read_text(encoding="utf-8")
                xml = xml.replace("</urlset>", f"<url><loc>{foreign_url}</loc></url></urlset>")
                _write(public / "sitemap.xml", xml)
                failed, output = self._run_agent_index(root, public, base_url)
                self.assertTrue(failed)
                self.assertIn("sitemap.xml: loc foreign origin", output)

        # 3. Missing wrapper / count mismatch in index.json
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            entries = json.loads((public / "index.json").read_text(encoding="utf-8"))
            entries = [e for e in entries if e["url"] != "/docs/getting-started/"]
            _write(public / "index.json", json.dumps(entries))
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("index.json docs entries", output)

        # 4. Duplicate entry in index.json
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            entries = json.loads((public / "index.json").read_text(encoding="utf-8"))
            entries.append(entries[0])
            _write(public / "index.json", json.dumps(entries))
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("index.json: duplicate search entry URL", output)

        # 5. Excluded URLs in sitemap.xml
        for bad_loc in (
            "/docs/reference/settings/",
            "/docs/contribute/development/",
            "/reference/settings.md",
            "/docs/modules/model-deployment/status/",
        ):
            with self.subTest(sitemap_leak=bad_loc), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root)
                xml = (public / "sitemap.xml").read_text(encoding="utf-8")
                xml = xml.replace("</urlset>", f"<url><loc>{bad_loc}</loc></url></urlset>")
                _write(public / "sitemap.xml", xml)
                failed, output = self._run_agent_index(root, public, "/")
                self.assertTrue(failed)
                self.assertIn("sitemap.xml: leaked excluded or retired URL", output)

    def test_complete_markdown_inventory_and_reference_rewrite_parity(self) -> None:
        # 1. Unlisted docs/orphan/index.md caught in public/docs/
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            _write(public / "docs/orphan/index.md", "# Orphan\n")
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("unexpected docs Markdown export not in All pages or wrappers: docs/orphan/index.md", output)

        # 2. Reference file present in docs/reference/ and public/reference/ but missing from ## Reference
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            _write(root / "docs/reference/unlisted.md", "# Unlisted\n\nBody.\n")
            _write(public / "reference/unlisted.md", "# Unlisted\n\nBody.\n")
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("docs/reference/unlisted.md: missing from docs/README.md Reference table", output)

        # 3. Tampered rewritten URL in public/reference/settings.md pointing to a DIFFERENT valid file
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            exported = public / "reference/settings.md"
            tampered = exported.read_text(encoding="utf-8").replace(
                "../docs/getting-started/architecture/index.md#retry)",
                "../docs/modules/devices/index.md#overview)",
            )
            exported.write_text(tampered, encoding="utf-8")
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("Markdown export link rewriting differs from expected reference output", output)

    def test_llms_txt_bidirectional_origin_multiplicity_reference_and_optional(self) -> None:
        base_url = "https://docs.gpustack.ai/gpustack-operator/main/"

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url=base_url)
            llms = (public / "llms.txt").read_text(encoding="utf-8")
            llms = llms.replace(
                "https://docs.gpustack.ai/gpustack-operator/main/reference/settings.md",
                "/gpustack-operator/main/reference/settings.md",
            )
            _write(public / "llms.txt", llms)
            failed, output = self._run_agent_index(root, public, base_url)
            self.assertTrue(failed)
            self.assertIn("entry origin mismatch", output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url=base_url)
            llms = (public / "llms.txt").read_text(encoding="utf-8")
            dup_line = "- [Settings](https://docs.gpustack.ai/gpustack-operator/main/reference/settings.md): Runtime settings contract"
            llms = llms.replace(dup_line, dup_line + "\n" + dup_line)
            _write(public / "llms.txt", llms)
            failed, output = self._run_agent_index(root, public, base_url)
            self.assertTrue(failed)
            self.assertIn("duplicate entry", output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url=base_url)
            llms = (public / "llms.txt").read_text(encoding="utf-8")
            llms = llms.replace("Runtime settings contract", "Stale summary")
            _write(public / "llms.txt", llms)
            failed, output = self._run_agent_index(root, public, base_url)
            self.assertTrue(failed)
            self.assertIn("llms.txt Reference section differs", output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url=base_url, source_ref="abc1234")
            llms = (public / "llms.txt").read_text(encoding="utf-8")
            llms = llms.replace(
                "blob/abc1234/docs/contribute/development.md",
                "blob/wrongref/docs/contribute/development.md",
            )
            _write(public / "llms.txt", llms)
            failed, output = self._run_agent_index(root, public, base_url)
            self.assertTrue(failed)
            self.assertIn("llms.txt Optional section differs", output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url=base_url)
            llms = (public / "llms.txt").read_text(encoding="utf-8")
            llms += "- [Rogue](https://example.com/rogue.md): Unexpected\n"
            _write(public / "llms.txt", llms)
            failed, output = self._run_agent_index(root, public, base_url)
            self.assertTrue(failed)
            self.assertIn("llms.txt Optional section differs", output)

    def test_reference_exports_integrity_and_anchor_validation(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            _write(public / "reference/obsolete.md", "# Obsolete\n")
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("unexpected or stale file in published reference/", output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            (public / "reference/settings.md").unlink()
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("missing reference Markdown export: docs/reference/settings.md", output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            exported = public / "reference/settings.md"
            exported.write_text(
                exported.read_text(encoding="utf-8") + "\nMutated prose.\n",
                encoding="utf-8",
            )
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("Markdown export changed body or code blocks", output)

        # Broken duplicate slug (#retry-3 when only retry, retry-1, retry-2 exist)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            src = root / "docs/reference/settings.md"
            src.write_text(
                src.read_text(encoding="utf-8").replace("#retry-2", "#retry-3"),
                encoding="utf-8",
            )
            rewrite_reference.stage_reference_dir(
                root / "docs/reference", public / "reference", source_ref="abc1234"
            )
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            self.assertIn("missing anchor #retry-3", output)

        bad_targets = (
            "[multi\nline](README.md#no-such-anchor)",
            "[ref-call][bad-ref]\n\n[bad-ref]: ../getting-started/architecture.md#missing",
            "Text[^2]\n\n[^2]: First line.\n\n    Continuation [broken](README.md#missing-in-footnote).",
            "[fence heading](settings.md#fake-fence-heading)",
            "[indent heading](settings.md#fake-indented-heading)",
            "[comment heading](settings.md#fake-comment-heading)",
            "[contrib anchor](../contribute/development.md#missing-anchor)",
        )
        for snippet in bad_targets:
            with self.subTest(snippet=snippet), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                public = _build_strict_fixture(root)
                src = root / "docs/reference/settings.md"
                src.write_text(src.read_text(encoding="utf-8") + f"\n{snippet}\n", encoding="utf-8")
                rewrite_reference.stage_reference_dir(
                    root / "docs/reference", public / "reference", source_ref="abc1234"
                )
                failed, output = self._run_agent_index(root, public, "/")
                self.assertTrue(failed, f"expected anchor failure for {snippet!r}, got: {output}")
                self.assertIn("missing anchor", output)

    def test_additional_strict_and_cli_error_branches(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url="https://docs.gpustack.ai/gpustack-operator/main/")
            res_idx = subprocess.run(
                [
                    sys.executable,
                    str(CHECK_DIR / "agent-index.py"),
                    str(root),
                    "--site",
                    str(public),
                    "--base-url",
                    "https://docs.gpustack.ai/gpustack-operator/main/",
                ],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(res_idx.returncode, 0, res_idx.stdout + res_idx.stderr)

            res_links = subprocess.run(
                [
                    sys.executable,
                    str(CHECK_DIR / "site-links.py"),
                    str(public),
                    "--base-url",
                    "https://docs.gpustack.ai/gpustack-operator/main/",
                    "--strict-three-zone",
                ],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(res_links.returncode, 0, res_links.stdout + res_links.stderr)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            (public / "index.json").unlink()
            (public / "sitemap.xml").unlink()
            (public / "llms.txt").unlink()
            shutil.rmtree(public / "reference")
            (root / "docs/reference/sub").mkdir()
            _write(public / "docs/reference/settings/index.md", "# Leak\n")
            (public / "docs/getting-started/architecture/index.md").unlink()
            (public / "docs/getting-started/architecture/index.html").unlink()
            _write(public / "docs/unexpected/index.html", "<html></html>\n")
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            for expected_msg in (
                "missing index.json",
                "missing sitemap.xml",
                "missing llms.txt",
                "missing static reference/ directory",
                "subdirectory forbidden under flat docs/reference/",
                "excluded or retired Markdown export in site",
                "missing Markdown export",
                "missing public HTML output",
                "unexpected docs HTML output",
            ):
                self.assertIn(expected_msg, output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root)
            (public / "reference/sub").mkdir()
            _write(
                public / "docs/getting-started/architecture/index.md",
                "# Architecture — [Link](self.md)\n\nMutated body [self](#missing-self) and [out](/outside) and [ext](mailto:test@example.com).\n",
            )
            _write(root / "docs/getting-started/architecture.md", "# Wrong H1\n\nBody.\n")
            _write(root / "docs/reference/settings.md", "# Wrong Ref H1\n\nBody.\n")
            _write(root / "docs/contribute/development.md", "# Wrong Contrib H1\n\nBody.\n")
            llms = (
                "- [Outside](/docs/outside/index.md): Before any section\n"
                "## Getting Started\n"
                "- malformed bullet without colon\n"
            )
            _write(public / "llms.txt", llms)
            failed, output = self._run_agent_index(root, public, "/")
            self.assertTrue(failed)
            for expected_msg in (
                "bullet outside ## section",
                "malformed bullet",
                "All pages label",
                "Reference label",
                "Contribute label",
                "subdirectory forbidden in published reference/",
                "Markdown export changed body or code blocks: docs/getting-started/architecture.md",
            ):
                self.assertIn(expected_msg, output)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            public = _build_strict_fixture(root, base_url="https://docs.gpustack.ai/sub/")
            _write(
                root / "docs/contribute/development.md",
                "# Development\n\n<div>bad</div>\n<!-- a --> b -->\n<!--\na --> b\n-->\n<br>\n",
            )
            failed, output = self._run_agent_index(root, None, "/")
            self.assertTrue(failed)
            self.assertIn("raw HTML must be an attribute-free <br>", output)
            self.assertIn("HTML comment body must not contain '-->'", output)

            _write(
                public / "docs/index.html",
                '<html><head><link href="/outside.css"><script src="missing.js"></script></head>'
                '<body><form action="mailto:x@y.z"></form>'
                '<a href="#missing-id">Bad anchor</a></body></html>\n',
            )
            link_failed, link_out = self._run_site_links(public, "https://docs.gpustack.ai/sub/")
            self.assertTrue(link_failed)
            self.assertIn("outside site base URL", link_out)
            self.assertIn("missing target", link_out)
            self.assertIn("missing anchor", link_out)

        with tempfile.TemporaryDirectory() as empty_tmp:
            link_failed, link_out = self._run_site_links(Path(empty_tmp), "/")
            self.assertTrue(link_failed)
            self.assertIn("no HTML pages", link_out)


if __name__ == "__main__":
    unittest.main()
