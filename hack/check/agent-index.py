#!/usr/bin/env python3
"""Check the shared module map and its generated documentation exports."""

import argparse
import importlib.util
import json
import re
import sys
import xml.etree.ElementTree as ET
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

CHECK_DIR = Path(__file__).resolve().parent
SITE_SCRIPTS_DIR = CHECK_DIR.parent / "site"
if str(SITE_SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SITE_SCRIPTS_DIR))

import rewrite_reference as ref_md  # noqa: E402


def _load_site_links():
    spec = importlib.util.spec_from_file_location("site_links_shared", CHECK_DIR / "site-links.py")
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


site_links_shared = _load_site_links()
RETIRED_MODULE_SLUGS = site_links_shared.RETIRED_MODULE_SLUGS
normalize_origin = site_links_shared.normalize_origin
is_same_origin = site_links_shared.is_same_origin
is_excluded_or_retired_rel_path = site_links_shared.is_excluded_or_retired_rel_path

ATX_HEADING_RE = re.compile(r"^(?: {0,3}> ?)* {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+[ \t]*)?$")
LIST_ITEM_RE = re.compile(r"^(?: {0,3}> ?)* {0,3}(?:[-*+]|\d+[.)])[ \t]")
FOOTNOTE_DEF_RE = re.compile(r"^(?: {0,3}> ?)* {0,3}\[\^[^\]]+\]:")
TABLE_ROW_RE = re.compile(r"^\| \[([^\]]+)\]\(([^)]+)\) \| (.+?) \|$", re.M)
LLMS_BULLET_RE = re.compile(r"^- \[([^\]]+)\]\(([^)]+)\): (.+)$")
GITHUB_REPO_BLOB_RE = re.compile(r"^/gpustack/gpustack-operator/(?:blob|tree)/([^/]+)/(.*)$")


class ProseHTML(HTMLParser):
    """Only attribute-free line breaks and HTML comments may appear as raw HTML in published prose."""

    def __init__(self, source, errors):
        super().__init__()
        self.source = source
        self.errors = errors

    def reject(self, markup):
        self.errors.append(
            f"{self.source}:{self.getpos()[0]}: raw HTML must be an attribute-free <br>: {markup}"
        )

    def handle_starttag(self, tag, attrs):
        if tag != "br" or attrs:
            self.reject(self.get_starttag_text())

    handle_startendtag = handle_starttag

    def handle_endtag(self, tag):
        self.reject(f"</{tag}>")

    def handle_comment(self, data):
        pass

    def handle_decl(self, decl):
        self.reject(f"<!{decl}>")

    def handle_pi(self, data):
        self.reject("processing instruction")

    def unknown_decl(self, data):
        self.reject("HTML declaration")


def check_prose_html(text, source, errors):
    fence = ""
    comment = False
    lines = []
    for lineno, line in enumerate(text.splitlines(), 1):
        marker = re.match(r"^ {0,3}(`{3,}|~{3,})", line)
        if marker and not fence:
            # Backticks in a backtick fence's info string make it ordinary prose.
            if marker[1][0] == "`" and "`" in line[marker.end() :]:
                lines.append(line)
                continue
            fence = marker[1]
            lines.append("")
        elif fence:
            if (
                marker
                and marker[1][0] == fence[0]
                and len(marker[1]) >= len(fence)
                and not line[marker.end() :].strip()
            ):
                fence = ""
            lines.append("")
        else:
            stripped = line.strip()
            if comment:
                if stripped == "-->":
                    comment = False
                elif "-->" in stripped:
                    errors.append(
                        f"{source}:{lineno}: HTML comment body must not contain '-->' (use '->' for flowgraph arrows so HTML/Markdown parsers do not close the comment early)"
                    )
                lines.append("")
                continue
            if stripped.startswith("<!--"):
                body = stripped[4:]
                if body.endswith("-->"):
                    inner = body[:-3]
                    if "-->" in inner:
                        errors.append(
                            f"{source}:{lineno}: HTML comment body must not contain '-->' (use '->' for flowgraph arrows)"
                        )
                else:
                    if "-->" in body:
                        errors.append(
                            f"{source}:{lineno}: HTML comment body must not contain '-->' (use '->' for flowgraph arrows)"
                        )
                    comment = True
                lines.append("")
                continue
            lines.append(line)
    prose = "\n".join(lines)
    # Inline code and Markdown autolinks are examples/text, not raw HTML.
    prose = re.sub(
        r"(?<![\\`])(`+)(?!`)(?:(?!\n[ \t]*\n).)*?(?<![\\`])\1(?!`)",
        lambda match: "\n" * match[0].count("\n"),
        prose,
        flags=re.S,
    )
    prose = re.sub(r"<(?:https?://[^<>\s]+|[^<>\s@]+@[^<>\s@]+)>", "", prose)
    parser = ProseHTML(source, errors)
    parser.feed(prose)
    parser.close()


def section(text, title):
    match = re.search(rf"^## {re.escape(title)}\n(.*?)(?=^## |\Z)", text, re.M | re.S)
    if not match:
        raise ValueError(f"docs/README.md: missing {title} section")
    return match[1]


def strip_frontmatter(text: str) -> str:
    if text.startswith("---\n"):
        parts = text.split("\n---\n", 1)
        if len(parts) == 2:
            return parts[1]
    return text


def _consume_indented_code_block(text: str, start: int) -> int | None:
    """Consume a standalone CommonMark 4-space or tab indented code block."""
    n = len(text)
    if start >= n or not (text.startswith("    ", start) or text[start] == "\t"):
        return None
    pos = start
    last_code_end = start
    while pos < n:
        nl = text.find("\n", pos)
        line_end = n if nl == -1 else nl + 1
        line = text[pos:line_end]
        if line.startswith("    ") or line.startswith("\t"):
            last_code_end = line_end
            pos = line_end
            continue
        if line.strip() == "":
            pos = line_end
            continue
        break
    return last_code_end


def _walk_markdown(text: str) -> tuple[str, list[tuple[str, str]]]:
    """Normalize non-exempt Markdown link destinations to 'URL' and collect (label, dest) pairs."""
    links: list[tuple[str, str]] = []

    def record(dest: str) -> str:
        if dest:
            links.append(("", dest))
            return "URL"
        return dest

    # Mask standalone 4-space/tab indented code blocks while preserving footnote/list continuations.
    pieces: list[tuple[bool, str]] = []
    n = len(text)
    pos = 0
    chunk_start = 0
    prev_blank = True
    container = ""
    while pos < n:
        nl = text.find("\n", pos)
        line_end = n if nl == -1 else nl + 1
        line = text[pos:line_end]
        if line.strip() == "":
            prev_blank = True
            pos = line_end
            continue
        fence_end = ref_md._consume_fenced_block(text, pos)
        if fence_end is not None:
            prev_blank = False
            pos = fence_end
            continue
        is_indented = line.startswith("    ") or line.startswith("\t")
        if prev_blank and not container and is_indented:
            indent_end = _consume_indented_code_block(text, pos)
            if indent_end is not None:
                if pos > chunk_start:
                    pieces.append((True, text[chunk_start:pos]))
                pieces.append((False, text[pos:indent_end]))
                chunk_start = indent_end
                pos = indent_end
                prev_blank = False
                continue
        if LIST_ITEM_RE.match(line):
            container = "list"
        elif FOOTNOTE_DEF_RE.match(line):
            container = "footnote"
        elif not is_indented:
            container = ""
        prev_blank = False
        pos = line_end
    if chunk_start < n:
        pieces.append((True, text[chunk_start:n]))

    out_parts: list[str] = []
    for rewrite_chunk, chunk in pieces:
        if rewrite_chunk:
            out_parts.append(ref_md._rewrite_text(chunk, record, block_level=True))
        else:
            out_parts.append(chunk)
    return "".join(out_parts), links


def prose_links(text: str):
    _, links = _walk_markdown(strip_frontmatter(text))
    yield from links


def without_link_urls(text: str) -> str:
    normalized, _ = _walk_markdown(strip_frontmatter(text))
    return normalized.strip()


def _strip_heading_links(text: str) -> str:
    def drop_link(s: str) -> str:
        out: list[str] = []
        n = len(s)
        i = 0
        while i < n:
            ch = s[i]
            if ch == "\\" and i + 1 < n:
                out.append(s[i : i + 2])
                i += 2
                continue
            if ch == "`":
                code_end = ref_md._scan_code_span(s, i)
                if code_end is not None:
                    out.append(s[i:code_end])
                    i = code_end
                    continue
            if ch == "!" and i + 1 < n and s[i + 1] == "[":
                res = ref_md._try_rewrite_inline_link(s, i + 1, lambda d: d)
                if res is not None:
                    rebuilt, end = res
                    close_b = rebuilt.rfind("](")
                    out.append(drop_link(rebuilt[1:close_b]))
                    i = end
                    continue
            if ch == "[":
                res = ref_md._try_rewrite_inline_link(s, i, lambda d: d)
                if res is not None:
                    rebuilt, end = res
                    close_b = rebuilt.rfind("](")
                    out.append(drop_link(rebuilt[1:close_b]))
                    i = end
                    continue
            out.append(ch)
            i += 1
        return "".join(out)

    return drop_link(text)


def _strip_inline_html_comments(line: str) -> str:
    out: list[str] = []
    n = len(line)
    i = 0
    while i < n:
        ch = line[i]
        if ch == "\\" and i + 1 < n:
            out.append(line[i : i + 2])
            i += 2
            continue
        if ch == "`":
            code_end = ref_md._scan_code_span(line, i)
            if code_end is not None:
                out.append(line[i:code_end])
                i = code_end
                continue
        if line.startswith("<!--", i):
            end = line.find("-->", i + 4)
            if end == -1:
                break
            i = end + 3
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def _github_slug(heading_text: str) -> str:
    cleaned = _strip_heading_links(heading_text).lower()
    cleaned = cleaned.replace("`", "").replace("*", "")
    cleaned = re.sub(r"[^a-z0-9 _-]", "", cleaned).strip(" ")
    return cleaned.replace(" ", "-")


def markdown_anchors(text: str) -> set[str]:
    """Extract GitHub heading anchor slugs (collision-free -1/-2 suffixes) outside code/comments."""
    body = strip_frontmatter(text)
    used: set[str] = set()
    n = len(body)
    i = 0
    prev_blank = True
    container = ""

    while i < n:
        nl = body.find("\n", i)
        line_end = n if nl == -1 else nl + 1
        line = body[i:line_end]

        if line.strip() == "":
            i = line_end
            prev_blank = True
            continue

        fence_end = ref_md._consume_fenced_block(body, i)
        if fence_end is not None:
            i = fence_end
            prev_blank = False
            continue

        is_indented = line.startswith("    ") or line.startswith("\t")
        if prev_blank and not container and is_indented:
            indent_end = _consume_indented_code_block(body, i)
            if indent_end is not None:
                i = indent_end
                prev_blank = False
                continue

        if line.lstrip().startswith("<!--"):
            end_comment = body.find("-->", i + line.find("<!--") + 4)
            if end_comment == -1:
                break
            next_nl = body.find("\n", end_comment + 3)
            i = n if next_nl == -1 else next_nl + 1
            prev_blank = False
            continue

        if LIST_ITEM_RE.match(line):
            container = "list"
        elif FOOTNOTE_DEF_RE.match(line):
            container = "footnote"
        elif not is_indented:
            container = ""
        prev_blank = False

        uncommented = _strip_inline_html_comments(line.rstrip("\r\n"))
        m = ATX_HEADING_RE.match(uncommented)
        if m:
            slug = _github_slug(m.group(2))
            candidate = slug
            suffix = 1
            while candidate in used:
                candidate = f"{slug}-{suffix}"
                suffix += 1
            used.add(candidate)
        i = line_end

    return used


def h1_of(path: Path) -> str:
    body = strip_frontmatter(path.read_text(encoding="utf-8"))
    for line in body.splitlines():
        if line.startswith("# "):
            return line[2:].strip()
    return ""


def markdown_path(source: Path) -> Path:
    if source.name == "_index.md":
        return source.with_name("index.md")
    return source.with_suffix("") / "index.md"


def html_path(source: Path) -> Path:
    if source.name == "_index.md":
        return source.with_name("index.html")
    return source.with_suffix("") / "index.html"


def html_url_path(source: Path, prefix: str) -> str:
    rel = html_path(source).parent.as_posix()
    return prefix + ("" if rel == "." else rel.rstrip("/") + "/")


def _strip_site_prefix(raw_url: str, base_url: str) -> tuple[str | None, str | None]:
    """Validate optional URL authority against base_url and return site-relative path."""
    base = urlsplit(base_url)
    prefix = base.path.rstrip("/") + "/"
    parsed = urlsplit(raw_url)
    if parsed.scheme or parsed.netloc:
        if not is_same_origin(parsed.scheme, parsed.netloc, base.scheme, base.netloc):
            return None, f"foreign origin against base URL {base_url}"
    path = unquote(parsed.path)
    if not path.startswith(prefix):
        return None, f"outside site base URL {base_url}"
    return path[len(prefix) :], None


def _validate_markdown_links(
    exported: Path,
    output: str,
    *,
    root: Path,
    public: Path,
    base_url: str,
    source_ref: str | None,
    anchor_cache: dict[Path, set[str]],
    errors: list[str],
) -> None:
    base = urlsplit(base_url)
    prefix = base.path.rstrip("/") + "/"

    def get_anchors(md_file: Path) -> set[str]:
        resolved_md = md_file.resolve()
        if resolved_md not in anchor_cache:
            anchor_cache[resolved_md] = markdown_anchors(resolved_md.read_text(encoding="utf-8"))
        return anchor_cache[resolved_md]

    rel_source = exported.relative_to(public)
    for _, target in prose_links(output):
        url = urlsplit(target)
        if url.scheme and url.scheme.lower() not in ("http", "https"):
            continue
        fragment = unquote(url.fragment)

        if url.scheme.lower() in ("http", "https") and (url.hostname or "").lower() == "github.com":
            gh_match = GITHUB_REPO_BLOB_RE.match(unquote(url.path))
            if gh_match:
                ref_in_url, repo_rel = gh_match.group(1), gh_match.group(2)
                if source_ref is not None and ref_in_url != source_ref:
                    errors.append(
                        f"{rel_source}: GitHub link ref {ref_in_url!r} differs from site sourceRef {source_ref!r}: {target}"
                    )
                    continue
                repo_target = (root / repo_rel).resolve() if repo_rel else root
                if not repo_target.is_relative_to(root) or not repo_target.exists():
                    errors.append(f"{rel_source}: missing repository target {target}")
                    continue
                if fragment and repo_target.is_file() and repo_target.suffix == ".md":
                    if fragment not in get_anchors(repo_target):
                        errors.append(f"{rel_source}: missing anchor #{fragment} in {target}")
            continue

        if url.scheme or url.netloc:
            if not is_same_origin(url.scheme, url.netloc, base.scheme, base.netloc):
                continue

        path = unquote(url.path)
        if path.startswith("/") and not path.startswith(prefix):
            errors.append(f"{rel_source}: target outside site base URL {target}")
            continue

        if not path:
            resolved = exported
        elif path.startswith("/"):
            resolved = public / path[len(prefix) :]
        else:
            resolved = exported.parent / path
        resolved = resolved.resolve()

        if not resolved.is_relative_to(public) or not resolved.exists():
            errors.append(f"{rel_source}: missing target {target}")
            continue

        if fragment:
            anchor_file: Path | None = None
            if resolved.is_file() and resolved.suffix == ".md":
                anchor_file = resolved
            elif resolved.is_dir() and (resolved / "index.md").is_file():
                anchor_file = resolved / "index.md"
            if anchor_file is not None and fragment not in get_anchors(anchor_file):
                errors.append(f"{rel_source}: missing anchor #{fragment} in {target}")


def _check_strict_site(
    root: Path,
    public: Path,
    index: str,
    base_url: str,
    errors: list[str],
) -> None:
    base = urlsplit(base_url)
    prefix = base.path.rstrip("/") + "/"
    base_norm = normalize_origin(base.scheme, base.netloc)
    origin = f"{base.scheme}://{base.netloc}" if (base.scheme or base.netloc) else ""

    source_ref = "main"
    version_file = public / "site-version.json"
    if version_file.is_file():
        version_data = json.loads(version_file.read_text(encoding="utf-8"))
        source_ref = version_data.get("sourceRef") or "main"

    all_pages_rows = TABLE_ROW_RE.findall(section(index, "All pages"))
    reference_rows = TABLE_ROW_RE.findall(section(index, "Reference"))
    contribute_rows = TABLE_ROW_RE.findall(section(index, "Contribute"))

    expected_docs_html: set[Path] = set()
    expected_docs_md: set[Path] = set()
    expected_docs_urls: set[str] = set()
    for _, target, _ in all_pages_rows:
        rel_doc = Path("docs") / target
        expected_docs_html.add(html_path(rel_doc))
        expected_docs_md.add(markdown_path(rel_doc))
        expected_docs_urls.add(html_url_path(rel_doc, prefix))

    wrappers_dir = root / "site/content/docs"
    if wrappers_dir.is_dir():
        for wrapper in sorted(wrappers_dir.rglob("*.md")):
            rel_wrapper = wrapper.relative_to(root / "site/content")
            expected_docs_html.add(html_path(rel_wrapper))
            expected_docs_md.add(markdown_path(rel_wrapper))
            expected_docs_urls.add(html_url_path(rel_wrapper, prefix))

    for html_file in sorted(public.rglob("*.html")):
        rel_html = html_file.relative_to(public)
        rel_str = rel_html.as_posix()
        if is_excluded_or_retired_rel_path(rel_str):
            errors.append(f"excluded or retired HTML output in site: {rel_str}")
        elif rel_str.startswith("docs/") and rel_html not in expected_docs_html:
            errors.append(f"unexpected docs HTML output not in All pages or wrappers: {rel_str}")

    for expected_html in sorted(expected_docs_html):
        if not (public / expected_html).is_file():
            errors.append(f"missing public HTML output: {expected_html.as_posix()}")

    index_json_path = public / "index.json"
    if not index_json_path.is_file():
        errors.append("site: missing index.json")
    else:
        entries = json.loads(index_json_path.read_text(encoding="utf-8"))
        seen_urls: set[str] = set()
        actual_docs_urls: set[str] = set()
        for entry in entries:
            raw_url = entry.get("url", "")
            if raw_url in seen_urls:
                errors.append(f"index.json: duplicate search entry URL {raw_url}")
            seen_urls.add(raw_url)
            rel_url, err_reason = _strip_site_prefix(raw_url, base_url)
            if rel_url is None:
                errors.append(f"index.json: entry {err_reason}: {raw_url}")
                continue
            if is_excluded_or_retired_rel_path(rel_url):
                errors.append(f"index.json: leaked excluded or retired entry {raw_url}")
                continue
            if rel_url == "docs" or rel_url.startswith("docs/"):
                actual_docs_urls.add(unquote(urlsplit(raw_url).path))
        if actual_docs_urls != expected_docs_urls or len(entries) != len(expected_docs_urls):
            errors.append(
                f"index.json docs entries ({len(entries)}) differ from expected ({len(expected_docs_urls)}): "
                f"missing {sorted(expected_docs_urls - actual_docs_urls)}; "
                f"extra {sorted(actual_docs_urls - expected_docs_urls)}"
            )

    sitemap_path = public / "sitemap.xml"
    if not sitemap_path.is_file():
        errors.append("site: missing sitemap.xml")
    else:
        xml_root = ET.fromstring(sitemap_path.read_text(encoding="utf-8"))
        for elem in xml_root.iter():
            if elem.tag.endswith("loc") and elem.text:
                loc = elem.text.strip()
                rel_loc, err_reason = _strip_site_prefix(loc, base_url)
                if rel_loc is None:
                    errors.append(f"sitemap.xml: loc {err_reason}: {loc}")
                    continue
                if is_excluded_or_retired_rel_path(rel_loc):
                    errors.append(f"sitemap.xml: leaked excluded or retired URL {loc}")
                elif (rel_loc == "docs" or rel_loc.startswith("docs/")) and (
                    unquote(urlsplit(loc).path) not in expected_docs_urls
                ):
                    errors.append(f"sitemap.xml: unexpected docs URL {loc}")

    llms_path = public / "llms.txt"
    if not llms_path.is_file():
        errors.append("site: missing llms.txt")
    else:
        current_sec = ""
        seen_bullets: set[str] = set()
        seen_llms_urls: set[str] = set()
        actual_public: list[tuple[str, str, str]] = []
        actual_reference: list[tuple[str, str, str]] = []
        actual_optional: list[tuple[str, str, str]] = []

        for lineno, line in enumerate(llms_path.read_text(encoding="utf-8").splitlines(), 1):
            if line.startswith("## "):
                current_sec = line[3:].strip()
                continue
            if not line.startswith("- "):
                continue
            bullet_match = LLMS_BULLET_RE.match(line)
            if not bullet_match:
                errors.append(f"llms.txt:{lineno}: malformed bullet {line!r}")
                continue
            label, target, desc = bullet_match.groups()
            if line in seen_bullets or target in seen_llms_urls:
                errors.append(f"llms.txt:{lineno}: duplicate entry {target}")
            seen_bullets.add(line)
            seen_llms_urls.add(target)

            if not current_sec:
                errors.append(f"llms.txt:{lineno}: bullet outside ## section: {target}")
            elif current_sec == "Reference":
                actual_reference.append((label, target, desc))
            elif current_sec == "Optional":
                actual_optional.append((label, target, desc))
            else:
                actual_public.append((label, target, desc))

        expected_public_llms: set[tuple[str, str, str]] = set()
        for label, target, desc in all_pages_rows:
            src_file = root / "docs" / target
            if src_file.is_file() and h1_of(src_file) != label:
                errors.append(
                    f"docs/README.md All pages label {label!r} != H1 {h1_of(src_file)!r} for {target}"
                )
            expected_url = f"{origin}{prefix}{markdown_path(Path('docs') / target).as_posix()}"
            expected_public_llms.add((label, expected_url, desc))

        expected_reference_llms: set[tuple[str, str, str]] = set()
        for label, target, desc in reference_rows:
            src_file = root / "docs" / target
            if src_file.is_file() and h1_of(src_file) != label:
                errors.append(
                    f"docs/README.md Reference label {label!r} != H1 {h1_of(src_file)!r} for {target}"
                )
            expected_url = f"{origin}{prefix}reference/{Path(target).name}"
            expected_reference_llms.add((label, expected_url, desc))

        expected_contribute_llms: set[tuple[str, str, str]] = set()
        for label, target, desc in contribute_rows:
            src_file = root / "docs" / target
            if src_file.is_file() and h1_of(src_file) != label:
                errors.append(
                    f"docs/README.md Contribute label {label!r} != H1 {h1_of(src_file)!r} for {target}"
                )
            expected_url = f"https://github.com/gpustack/gpustack-operator/blob/{source_ref}/docs/{target}"
            expected_contribute_llms.add((label, expected_url, desc))

        for _, target, _ in actual_public + actual_reference:
            parsed_u = urlsplit(target)
            if normalize_origin(parsed_u.scheme, parsed_u.netloc) != base_norm:
                errors.append(f"llms.txt: entry origin mismatch against base URL {base_url}: {target}")

        if set(actual_public) != expected_public_llms:
            errors.append(
                f"llms.txt public pages differ from All pages: "
                f"missing {expected_public_llms - set(actual_public)}; "
                f"extra {set(actual_public) - expected_public_llms}"
            )
        if set(actual_reference) != expected_reference_llms:
            errors.append(
                f"llms.txt Reference section differs from docs/README.md Reference: "
                f"missing {expected_reference_llms - set(actual_reference)}; "
                f"extra {set(actual_reference) - expected_reference_llms}"
            )

        repo_nav_url = f"https://github.com/gpustack/gpustack-operator/blob/{source_ref}/docs/README.md"
        filtered_optional = {
            item
            for item in actual_optional
            if not (item[0] == "Repository navigation" and item[1] == repo_nav_url)
        }
        if filtered_optional != expected_contribute_llms:
            errors.append(
                f"llms.txt Optional section differs from Contribute: "
                f"missing {expected_contribute_llms - filtered_optional}; "
                f"extra {filtered_optional - expected_contribute_llms}"
            )

    anchor_cache: dict[Path, set[str]] = {}

    if (public / "docs").is_dir():
        for pub_md in sorted((public / "docs").rglob("*.md")):
            rel_md_path = pub_md.relative_to(public)
            rel_md = rel_md_path.as_posix()
            if is_excluded_or_retired_rel_path(rel_md):
                errors.append(f"excluded or retired Markdown export in site: {rel_md}")
            elif rel_md_path not in expected_docs_md:
                errors.append(
                    f"unexpected docs Markdown export not in All pages or wrappers: {rel_md}"
                )

    for _, target, _ in all_pages_rows:
        source = root / "docs" / target
        if not source.is_file():
            errors.append(f"missing source file in All pages: docs/{target}")
            continue
        exported = public / markdown_path(Path("docs") / target)
        if not exported.is_file():
            errors.append(f"missing Markdown export: docs/{target}")
            continue
        content = source.read_text(encoding="utf-8")
        output = exported.read_text(encoding="utf-8")
        if without_link_urls(content) != without_link_urls(output):
            errors.append(f"Markdown export changed body or code blocks: docs/{target}")
        _validate_markdown_links(
            exported,
            output,
            root=root,
            public=public,
            base_url=base_url,
            source_ref=source_ref,
            anchor_cache=anchor_cache,
            errors=errors,
        )

    ref_src_dir = root / "docs/reference"
    ref_pub_dir = public / "reference"
    src_ref_names: set[str] = set()
    for entry in sorted(ref_src_dir.iterdir(), key=lambda p: p.name):
        if entry.is_dir():
            errors.append(f"docs/reference/{entry.name}: subdirectory forbidden under flat docs/reference/")
            continue
        if entry.is_file() and entry.suffix == ".md":
            src_ref_names.add(entry.name)

    index_ref_names: set[str] = set()
    for _, target, _ in reference_rows:
        rel_p = Path(target)
        if rel_p.parent != Path("reference") or rel_p.suffix != ".md":
            errors.append(f"docs/README.md Reference: entry must be flat reference/*.md: {target}")
            continue
        if rel_p.name in index_ref_names:
            errors.append(f"docs/README.md Reference: duplicate entry {target}")
        index_ref_names.add(rel_p.name)

    for missing_from_index in sorted(src_ref_names - index_ref_names):
        errors.append(
            f"docs/reference/{missing_from_index}: missing from docs/README.md Reference table"
        )
    for missing_src in sorted(index_ref_names - src_ref_names):
        errors.append(
            f"docs/README.md Reference: indexed source file missing: docs/reference/{missing_src}"
        )

    expected_ref_names = src_ref_names & index_ref_names
    if not ref_pub_dir.is_dir():
        errors.append("site: missing static reference/ directory")
    else:
        for pub_entry in sorted(ref_pub_dir.iterdir(), key=lambda p: p.name):
            if pub_entry.is_dir():
                errors.append(f"reference/{pub_entry.name}: subdirectory forbidden in published reference/")
            elif pub_entry.suffix != ".md" or pub_entry.name not in expected_ref_names:
                errors.append(f"reference/{pub_entry.name}: unexpected or stale file in published reference/")

        for name in sorted(src_ref_names):
            source = ref_src_dir / name
            exported = ref_pub_dir / name
            if not exported.is_file():
                errors.append(f"missing reference Markdown export: docs/reference/{name}")
                continue
            content = source.read_text(encoding="utf-8")
            output = exported.read_text(encoding="utf-8")
            expected_output = ref_md.rewrite_markdown(
                content,
                docs_prefix="../docs",
                source_ref=source_ref,
            )
            if without_link_urls(content) != without_link_urls(output):
                errors.append(f"Markdown export changed body or code blocks: docs/reference/{name}")
            elif output != expected_output:
                errors.append(
                    f"Markdown export link rewriting differs from expected reference output: docs/reference/{name}"
                )
            _validate_markdown_links(
                exported,
                output,
                root=root,
                public=public,
                base_url=base_url,
                source_ref=source_ref,
                anchor_cache=anchor_cache,
                errors=errors,
            )


def check(root, public, base_url):
    root = Path(root).resolve()
    index = (root / "docs/README.md").read_text(encoding="utf-8")
    errors = []
    for directory in (root / "docs", root / "site/content"):
        if not directory.is_dir():
            continue
        for source in directory.rglob("*.md"):
            if source != root / "docs/README.md":
                check_prose_html(source.read_text(encoding="utf-8"), source.relative_to(root), errors)
    records = {}
    for block in re.split(r"(?m)^### ", section(index, "Module map"))[1:]:
        title, _, body = block.partition("\n")
        fields = re.findall(r"^- ([A-Za-z ]+): (.+)$", body, re.M)
        values = dict(fields)
        if len(fields) != len(values):
            errors.append(f"{title}: duplicate field")
        for name in values.keys() - {
            "ID",
            "Use for",
            "Aliases",
            "Guides",
            "Specs",
            "Code",
            "Related",
            "Skills",
        }:
            errors.append(f"{title}: unknown field {name}")
        for name in ("ID", "Use for", "Aliases", "Guides", "Code", "Skills"):
            if not values.get(name):
                errors.append(f"{title}: missing {name}")
        identity = values.get("ID", "").strip("`")
        if not re.fullmatch(r"[a-z][a-z0-9-]*", identity) or identity in records:
            errors.append(f"{title}: invalid or duplicate ID {identity!r}")
        records[identity] = values
        for name in ("Guides", "Specs", "Code", "Skills"):
            links = list(prose_links(values.get(name, "")))
            if name in values and not links:
                errors.append(f"{title}: {name} has no source links")
            for _, target in links:
                url = urlsplit(target)
                resolved = (root / "docs" / unquote(url.path)).resolve()
                if (
                    url.scheme
                    or url.netloc
                    or not resolved.is_relative_to(root)
                    or not resolved.exists()
                ):
                    errors.append(f"{title}: {name} target must exist in this checkout: {target}")
                elif name == "Guides" and not resolved.is_relative_to(root / "docs"):
                    errors.append(f"{title}: guide outside docs/: {target}")
                elif name == "Specs" and not resolved.is_relative_to(root / "specs"):
                    errors.append(f"{title}: spec outside specs/: {target}")
                elif name == "Skills" and (
                    resolved.name != "SKILL.md"
                    or not resolved.is_relative_to(root / ".agents/skills")
                ):
                    errors.append(f"{title}: skill must use its canonical SKILL.md: {target}")

    modules = {path.name for path in (root / "docs/modules").iterdir() if path.is_dir()}
    required = modules | {"api", "installation", "development"}
    if set(records) != required:
        errors.append(
            f"module IDs differ from capability directories and task routes: {set(records) ^ required}"
        )
    for identity in modules & records.keys():
        guides = [target for _, target in prose_links(records[identity].get("Guides", ""))]
        if not any(target.startswith(f"modules/{identity}/") for target in guides):
            errors.append(f"{identity}: missing a guide in its own module")

    consumers = set()
    skills_root = root / ".agents/skills"
    if skills_root.is_dir():
        for skill in skills_root.iterdir():
            if not (skill / "SKILL.md").is_file():
                continue
            if any(
                "docs/" in path.read_text(encoding="utf-8")
                for path in skill.rglob("*")
                if path.suffix in (".md", ".sh")
            ):
                consumers.add(skill.name)
    routed = {
        Path(urlsplit(target).path).parent.name
        for record in records.values()
        for _, target in prose_links(record.get("Skills", ""))
    }
    for name in sorted(consumers - routed):
        errors.append(f"documentation consumer missing from module map: {name}")

    if public:
        public = Path(public).resolve()
        strict_three_zone = (root / "docs/reference/README.md").is_file()
        if strict_three_zone:
            _check_strict_site(root, public, index, base_url, errors)
        else:
            base = urlsplit(base_url)
            prefix = base.path.rstrip("/") + "/"
            llms = public / "llms.txt"
            if not llms.is_file():
                errors.append("site: missing llms.txt")
            else:
                rows = TABLE_ROW_RE.findall(section(index, "All pages"))
                expected = {
                    (label, prefix + str(markdown_path(Path("docs") / target)), description)
                    for label, target, description in rows
                }
                actual = set(
                    re.findall(r"^- \[([^]]+)\]\(([^)]+)\): (.+)$", llms.read_text(encoding="utf-8"), re.M)
                )
                actual = {
                    (label, unquote(urlsplit(target).path), description)
                    for label, target, description in actual
                    if urlsplit(target).path.startswith(prefix + "docs/")
                }
                if not expected or actual != expected:
                    errors.append(
                        f"llms.txt differs from All pages: missing {expected - actual}; extra {actual - expected}"
                    )
            anchor_cache: dict[Path, set[str]] = {}
            for source in sorted((root / "docs").rglob("*.md")):
                if source.name == "README.md":
                    continue
                exported = public / markdown_path(source.relative_to(root))
                if not exported.is_file():
                    errors.append(f"missing Markdown export: {source.relative_to(root)}")
                    continue
                content = source.read_text(encoding="utf-8")
                output = exported.read_text(encoding="utf-8")
                if without_link_urls(content) != without_link_urls(output):
                    errors.append(f"Markdown export changed body or code blocks: {source.relative_to(root)}")
                _validate_markdown_links(
                    exported,
                    output,
                    root=root,
                    public=public,
                    base_url=base_url,
                    source_ref=None,
                    anchor_cache=anchor_cache,
                    errors=errors,
                )

    for error in errors:
        print(error)
    print(
        f"Checked {len(records)} module routes and {len(consumers)} documentation-consuming skills; {len(errors)} error(s)."
    )
    return bool(errors)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("--site", type=Path)
    parser.add_argument("--base-url", default="/")
    args = parser.parse_args()
    try:
        raise SystemExit(check(args.root.resolve(), args.site, args.base_url))
    except ValueError as error:
        parser.exit(1, f"{error}\n")
