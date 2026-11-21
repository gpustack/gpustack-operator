#!/usr/bin/env python3
"""Check links between pages and assets in a built Hugo site."""

import argparse
import posixpath
import re
import sys
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

RETIRED_MODULE_SLUGS = frozenset(
    {
        "docs/modules/devices/discovery",
        "docs/modules/devices/scheduling",
        "docs/modules/devices/admission",
        "docs/modules/rdma/network-topology",
        "docs/modules/topology/scheduling",
        "docs/modules/kv-cache/injection",
        "docs/modules/model-delivery/artifact-api",
        "docs/modules/model-delivery/node-store",
        "docs/modules/model-deployment/parameterize",
        "docs/modules/model-deployment/metrics",
        "docs/modules/model-deployment/status",
        "docs/modules/model-deployment/shutdown",
        "docs/modules/model-deployment/scaling-simulation",
        "docs/modules/instances/metrics",
    }
)
SAME_ORIGIN_EXCLUDED_RE = re.compile(
    r"^(?:gpustack-operator/[^/]+/)?(?:reference|docs/reference|docs/contribute)(?:/|$)"
)
DEFAULT_PORTS = {"http": 80, "https": 443}


class Page(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.ids = set()
        self.links = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if "id" in attrs:
            self.ids.add(attrs["id"])
        if tag == "a" and "href" in attrs:
            self.links.append(attrs["href"])
        if tag == "img" and "src" in attrs:
            self.links.append(attrs["src"])
        if tag == "link" and "href" in attrs:
            self.links.append(attrs["href"])
        if tag == "script" and "src" in attrs:
            self.links.append(attrs["src"])
        if tag == "form" and "action" in attrs:
            self.links.append(attrs["action"])


def normalize_origin(
    scheme: str,
    netloc: str,
    *,
    default_scheme: str | None = None,
) -> tuple[str, str, int | None]:
    """Normalize scheme, host, and non-default port for origin comparisons."""
    eff_scheme = (scheme or (default_scheme or "")).lower()
    if not netloc:
        return ("", "", None)
    parsed = urlsplit(f"{eff_scheme or 'https'}://{netloc}")
    host = (parsed.hostname or "").lower()
    try:
        port = parsed.port
    except ValueError:
        port = None
    if port is not None and (
        (eff_scheme in DEFAULT_PORTS and port == DEFAULT_PORTS[eff_scheme])
        or (not scheme and port in (80, 443))
    ):
        port = None
    return (eff_scheme, host, port)


def is_same_origin(
    url_scheme: str,
    url_netloc: str,
    base_scheme: str,
    base_netloc: str,
) -> bool:
    """Return True when a URL with authority matches base_url's normalized origin."""
    if not base_netloc:
        return False
    base_norm = normalize_origin(base_scheme, base_netloc)
    url_norm = normalize_origin(url_scheme, url_netloc, default_scheme=base_norm[0] or "https")
    return url_norm == base_norm


def is_excluded_zone_rel(rel_path: str) -> bool:
    norm = posixpath.normpath("/" + rel_path.strip("/")).lstrip("/")
    return (
        norm == "reference"
        or norm.startswith("reference/")
        or norm == "docs/reference"
        or norm.startswith("docs/reference/")
        or norm == "docs/contribute"
        or norm.startswith("docs/contribute/")
    )


def is_excluded_or_retired_rel_path(rel_path: str) -> bool:
    norm = posixpath.normpath("/" + rel_path.strip("/")).lstrip("/")
    if not norm or norm == ".":
        return False
    if is_excluded_zone_rel(norm):
        return True
    slug = norm
    for suffix in ("/index.html", "/index.md"):
        if slug.endswith(suffix):
            slug = slug[: -len(suffix)]
            break
    return any(slug == retired or slug.startswith(retired + "/") for retired in RETIRED_MODULE_SLUGS)


def is_excluded_origin_path(decoded_path: str, prefix: str) -> bool:
    """Check decoded URL path against both Pages root and configured site prefix."""
    norm_root = posixpath.normpath("/" + decoded_path.lstrip("/")).lstrip("/")
    if SAME_ORIGIN_EXCLUDED_RE.match(norm_root):
        return True
    norm_abs = "/" + norm_root if norm_root and norm_root != "." else "/"
    if prefix != "/" and norm_abs.startswith(prefix):
        rel = norm_abs[len(prefix) :]
        if is_excluded_zone_rel(rel):
            return True
    return False


def main(root, base_url="/", *, strict_three_zone=False):
    root = Path(root).resolve()
    strict = strict_three_zone or (root / "reference/README.md").is_file()
    base = urlsplit(base_url)
    prefix = base.path.rstrip("/") + "/"
    pages = {}
    for path in sorted(root.rglob("*.html")):
        page = Page()
        page.feed(path.read_text(encoding="utf-8"))
        pages[path] = page

    if not pages:
        print(f"no HTML pages under {root}", file=sys.stderr)
        return 1

    errors = []
    if strict:
        for path in pages:
            rel_html = path.relative_to(root).as_posix()
            if is_excluded_or_retired_rel_path(rel_html):
                errors.append(f"{rel_html}: HTML page forbidden in excluded or retired zone")

    for source, page in pages.items():
        for href in page.links:
            url = urlsplit(href)
            if url.scheme and url.scheme.lower() not in ("http", "https"):
                continue
            if strict and url.netloc:
                _, host, port = normalize_origin(url.scheme, url.netloc)
                decoded_remote = unquote(url.path)
                if host == "docs.gpustack.ai" and port is None and is_excluded_origin_path(
                    decoded_remote, prefix
                ):
                    errors.append(f"{source.relative_to(root)} -> {href} (links to excluded zone)")
                    continue
            if url.scheme or url.netloc:
                if not is_same_origin(url.scheme, url.netloc, base.scheme, base.netloc):
                    continue
            path = unquote(url.path)
            if strict and path.startswith("/") and is_excluded_origin_path(path, prefix):
                errors.append(f"{source.relative_to(root)} -> {href} (links to excluded zone)")
                continue
            if path.startswith("/"):
                if not path.startswith(prefix):
                    errors.append(f"{source.relative_to(root)} -> {href} (outside site base URL)")
                    continue
                path = "/" + path[len(prefix) :]
            target = (
                source
                if not path
                else (root / path.lstrip("/") if path.startswith("/") else source.parent / path)
            )
            if strict and target.resolve().is_relative_to(root):
                rel_raw = target.resolve().relative_to(root).as_posix()
                if is_excluded_zone_rel(rel_raw):
                    errors.append(f"{source.relative_to(root)} -> {href} (links to excluded zone)")
                    continue
            if path and (target.is_dir() or path.endswith("/")):
                target = target / "index.html"
            target = target.resolve()
            if strict and target.is_relative_to(root):
                rel_resolved = target.relative_to(root).as_posix()
                if is_excluded_zone_rel(rel_resolved):
                    errors.append(f"{source.relative_to(root)} -> {href} (links to excluded zone)")
                    continue
            if not target.is_relative_to(root) or not target.is_file():
                errors.append(f"{source.relative_to(root)} -> {href} (missing target)")
            elif url.fragment and target.suffix == ".html":
                target_page = pages.get(target)
                if target_page is None or unquote(url.fragment) not in target_page.ids:
                    errors.append(f"{source.relative_to(root)} -> {href} (missing anchor)")

    for error in errors:
        print(error, file=sys.stderr)
    print(f"Checked {len(pages)} site pages; {len(errors)} broken internal link(s).")
    return bool(errors)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("--base-url", default="/")
    parser.add_argument("--strict-three-zone", action="store_true")
    args = parser.parse_args()
    sys.exit(main(args.root, args.base_url, strict_three_zone=args.strict_three_zone))
