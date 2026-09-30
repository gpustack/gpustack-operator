#!/usr/bin/env python3
"""Check links between pages and assets in a built Hugo site."""

import sys
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit


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


def main(root):
    root = root.resolve()
    pages = {}
    for path in root.rglob("*.html"):
        page = Page()
        page.feed(path.read_text(encoding="utf-8"))
        pages[path] = page

    if not pages:
        print(f"no HTML pages under {root}", file=sys.stderr)
        return 1

    errors = []
    for source, page in pages.items():
        for href in page.links:
            url = urlsplit(href)
            if url.scheme or url.netloc:
                continue
            path = unquote(url.path)
            target = source if not path else (
                root / path.lstrip("/") if path.startswith("/") else source.parent / path
            )
            if path and (target.is_dir() or path.endswith("/")):
                target = target / "index.html"
            target = target.resolve()
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
    if len(sys.argv) != 2:
        sys.exit("usage: site-links.py SITE_PUBLIC_DIR")
    sys.exit(main(Path(sys.argv[1])))
