#!/usr/bin/env python3
"""Check links between pages and assets in a built Hugo site."""

import argparse
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
        if tag == "link" and "href" in attrs:
            self.links.append(attrs["href"])
        if tag == "script" and "src" in attrs:
            self.links.append(attrs["src"])
        if tag == "form" and "action" in attrs:
            self.links.append(attrs["action"])


def main(root, base_url):
    root = root.resolve()
    base = urlsplit(base_url)
    prefix = base.path.rstrip("/") + "/"
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
            if url.scheme and url.scheme not in ("http", "https"):
                continue
            if (url.scheme or url.netloc) and url.netloc != base.netloc:
                continue
            path = unquote(url.path)
            if path.startswith("/"):
                if not path.startswith(prefix):
                    errors.append(f"{source.relative_to(root)} -> {href} (outside site base URL)")
                    continue
                path = "/" + path[len(prefix):]
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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("--base-url", default="/")
    args = parser.parse_args()
    sys.exit(main(args.root, args.base_url))
