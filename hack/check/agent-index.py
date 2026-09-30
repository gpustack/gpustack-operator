#!/usr/bin/env python3
"""Check the shared module map and its generated documentation exports."""

import argparse
import re
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

LINK = re.compile(r"!?\[([^\]\n]*)\]\(([^)\n]+)\)")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")


class ProseHTML(HTMLParser):
    """Only attribute-free line breaks may appear as raw HTML in published prose."""

    def __init__(self, source, errors):
        super().__init__()
        self.source = source
        self.errors = errors

    def reject(self, markup):
        self.errors.append(f"{self.source}:{self.getpos()[0]}: raw HTML must be an attribute-free <br>: {markup}")

    def handle_starttag(self, tag, attrs):
        if tag != "br" or attrs:
            self.reject(self.get_starttag_text())

    handle_startendtag = handle_starttag

    def handle_endtag(self, tag):
        self.reject(f"</{tag}>")

    def handle_comment(self, data):
        self.reject("HTML comment")

    def handle_decl(self, decl):
        self.reject(f"<!{decl}>")

    def handle_pi(self, data):
        self.reject("processing instruction")

    def unknown_decl(self, data):
        self.reject("HTML declaration")


def check_prose_html(text, source, errors):
    fence = ""
    lines = []
    for line in text.splitlines():
        marker = FENCE.match(line)
        if marker and not fence:
            # Backticks in a backtick fence's info string make it ordinary prose.
            if marker[1][0] == "`" and "`" in line[marker.end():]:
                lines.append(line)
                continue
            fence = marker[1]
            lines.append("")
        elif fence:
            if (marker and marker[1][0] == fence[0] and len(marker[1]) >= len(fence)
                    and not line[marker.end():].strip()):
                fence = ""
            lines.append("")
        else:
            lines.append(line)
    prose = "\n".join(lines)
    # Inline code and Markdown autolinks are examples/text, not raw HTML.
    prose = re.sub(r"(?<![\\`])(`+)(?!`)(?:(?!\n[ \t]*\n).)*?(?<![\\`])\1(?!`)",
                   lambda match: "\n" * match[0].count("\n"), prose, flags=re.S)
    prose = re.sub(r"<(?:https?://[^<>\s]+|[^<>\s@]+@[^<>\s@]+)>", "", prose)
    parser = ProseHTML(source, errors)
    parser.feed(prose)
    parser.close()


def section(text, title):
    match = re.search(rf"^## {re.escape(title)}\n(.*?)(?=^## |\Z)", text, re.M | re.S)
    if not match:
        raise ValueError(f"docs/README.md: missing {title} section")
    return match[1]


def prose_links(text):
    fence = ""
    for line in text.splitlines():
        marker = FENCE.match(line)
        if marker:
            marker = marker[1]
            if not fence:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = ""
        elif not fence:
            yield from LINK.findall(line)


def without_link_urls(text):
    fence = ""
    lines = []
    for line in text.splitlines():
        marker = FENCE.match(line)
        if marker:
            marker = marker[1]
            if not fence:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = ""
        elif not fence:
            line = LINK.sub(lambda match: match[0].replace(f"({match[2]})", "(URL)"), line)
        lines.append(line)
    return "\n".join(lines).strip()


def markdown_path(source):
    if source.name == "_index.md":
        return source.with_name("index.md")
    return source.with_suffix("") / "index.md"


def check(root, public, base_url):
    index = (root / "docs/README.md").read_text()
    errors = []
    for directory in (root / "docs", root / "site/content"):
        for source in directory.rglob("*.md"):
            if source != root / "docs/README.md":
                check_prose_html(source.read_text(), source.relative_to(root), errors)
    records = {}
    for block in re.split(r"(?m)^### ", section(index, "Module map"))[1:]:
        title, _, body = block.partition("\n")
        fields = re.findall(r"^- ([A-Za-z ]+): (.+)$", body, re.M)
        values = dict(fields)
        if len(fields) != len(values):
            errors.append(f"{title}: duplicate field")
        for name in values.keys() - {"ID", "Use for", "Aliases", "Guides", "Specs", "Code", "Related", "Skills"}:
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
                if url.scheme or url.netloc or not resolved.is_relative_to(root) or not resolved.exists():
                    errors.append(f"{title}: {name} target must exist in this checkout: {target}")
                elif name == "Guides" and not resolved.is_relative_to(root / "docs"):
                    errors.append(f"{title}: guide outside docs/: {target}")
                elif name == "Specs" and not resolved.is_relative_to(root / "specs"):
                    errors.append(f"{title}: spec outside specs/: {target}")
                elif name == "Skills" and (
                    resolved.name != "SKILL.md" or not resolved.is_relative_to(root / ".agents/skills")
                ):
                    errors.append(f"{title}: skill must use its canonical SKILL.md: {target}")

    modules = {path.name for path in (root / "docs/modules").iterdir() if path.is_dir()}
    required = modules | {"api", "installation", "development"}
    if set(records) != required:
        errors.append(f"module IDs differ from capability directories and task routes: {set(records) ^ required}")
    for identity in modules & records.keys():
        guides = [target for _, target in prose_links(records[identity].get("Guides", ""))]
        if not any(target.startswith(f"modules/{identity}/") for target in guides):
            errors.append(f"{identity}: missing a guide in its own module")

    consumers = set()
    for skill in (root / ".agents/skills").iterdir():
        if not (skill / "SKILL.md").is_file():
            continue
        if any("docs/" in path.read_text() for path in skill.rglob("*") if path.suffix in (".md", ".sh")):
            consumers.add(skill.name)
    routed = {
        Path(urlsplit(target).path).parent.name
        for record in records.values()
        for _, target in prose_links(record.get("Skills", ""))
    }
    for name in sorted(consumers - routed):
        errors.append(f"documentation consumer missing from module map: {name}")

    if public:
        public = public.resolve()
        base = urlsplit(base_url)
        prefix = base.path.rstrip("/") + "/"
        llms = public / "llms.txt"
        if not llms.is_file():
            errors.append("site: missing llms.txt")
        else:
            rows = re.findall(r"^\| \[([^]]+)\]\(([^)]+)\) \| (.+?) \|$", section(index, "All pages"), re.M)
            expected = {(label, prefix + str(markdown_path(Path("docs") / target)), description) for label, target, description in rows}
            actual = set(re.findall(r"^- \[([^]]+)\]\(([^)]+)\): (.+)$", llms.read_text(), re.M))
            actual = {(label, unquote(urlsplit(target).path), description) for label, target, description in actual if urlsplit(target).path.startswith(prefix + "docs/")}
            if not expected or actual != expected:
                errors.append(f"llms.txt differs from All pages: missing {expected - actual}; extra {actual - expected}")
        for source in sorted((root / "docs").rglob("*.md")):
            if source.name == "README.md":
                continue
            exported = public / markdown_path(source.relative_to(root))
            if not exported.is_file():
                errors.append(f"missing Markdown export: {source.relative_to(root)}")
                continue
            content = source.read_text()
            if content.startswith("---\n"):
                content = content.split("\n---\n", 1)[1]
            output = exported.read_text()
            if without_link_urls(content) != without_link_urls(output):
                errors.append(f"Markdown export changed body or code blocks: {source.relative_to(root)}")
            for _, target in prose_links(output):
                url = urlsplit(target)
                if url.scheme and url.scheme not in ("http", "https"):
                    continue
                if (url.scheme or url.netloc) and url.netloc != base.netloc:
                    continue
                path = unquote(url.path)
                if path.startswith("/") and not path.startswith(prefix):
                    errors.append(f"{exported.relative_to(public)}: target outside site base URL {target}")
                    continue
                resolved = (public / path[len(prefix):]) if path.startswith("/") else exported.parent / path
                if not url.path:
                    resolved = exported
                resolved = resolved.resolve()
                if not resolved.is_relative_to(public) or not resolved.exists():
                    errors.append(f"{exported.relative_to(public)}: missing target {target}")

    for error in errors:
        print(error)
    print(f"Checked {len(records)} module routes and {len(consumers)} documentation-consuming skills; {len(errors)} error(s).")
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
