#!/usr/bin/env python3
"""Compose versioned documentation into a checkout of the github-pages branch."""

import argparse
import hashlib
import html
import json
import re
import shutil
from pathlib import Path
from urllib.parse import urlsplit

TAG = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?")


def version_kind(ref):
    if ref == "main":
        return "development"
    match = TAG.fullmatch(ref)
    if not match:
        raise ValueError("ref must be main or a version tag such as v0.9.0 or v0.9.0-rc2")
    return "prerelease" if match[4] else "stable"


def order(record):
    match = TAG.fullmatch(record["name"])
    if not match:
        return (-1, -1, -1, ())
    suffix = tuple((1, int(part)) if part.isdigit() else (0, part)
                   for part in re.findall(r"\d+|\D+", match[4] or ""))
    return (*map(int, match.group(1, 2, 3)), suffix)


def inventory(directory):
    return {str(path.relative_to(directory)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in directory.rglob("*") if path.is_file()}


def compose(build, pages, ref, commit, root_url):
    kind = version_kind(ref)
    base = urlsplit(root_url)
    if base.scheme != "https" or not base.netloc or base.query or base.fragment or not root_url.endswith("/"):
        raise ValueError("root URL must be an HTTPS directory URL ending in /")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("source commit must be a full Git commit SHA")
    build, pages = build.resolve(), pages.resolve()
    if build.is_relative_to(pages) or pages.is_relative_to(build):
        raise ValueError("build and Pages checkout must be separate directories")
    for name in ("index.html", "llms.txt", "index.md", "index.json", "site-version.json"):
        if not (build / name).is_file():
            raise ValueError(f"build is missing {name}")
    identity = json.loads((build / "site-version.json").read_text())
    if identity != {"version": ref, "sourceRef": commit, "baseURL": root_url + ref + "/"}:
        raise ValueError("build version, source revision or base URL does not match publication")
    if not (pages / "charts/index.yaml").is_file():
        raise ValueError("Pages checkout is missing charts/index.yaml; restore charts before publishing documentation")
    charts = inventory(pages / "charts")
    manifest = pages / "versions.json"
    records = json.loads(manifest.read_text())["published"] if manifest.exists() else []
    if not isinstance(records, list):
        raise ValueError("versions.json published field must be a list")
    for record in records:
        version_kind(record["name"])
        if record["path"] != record["name"] + "/":
            raise ValueError("invalid version directory in existing manifest")
    previous = next((record for record in records if record["name"] == ref), None)
    if kind != "development" and previous and previous["commit"] != commit:
        raise ValueError("published tag documentation is immutable; do not move an existing tag")
    package = pages / "charts" / f"gpustack-operator-{ref.removeprefix('v')}.tgz"
    if kind != "development" and package.exists() and not previous:
        raise ValueError("existing chart has no published source revision; verify its provenance before publishing documentation")
    destination = pages / ref
    if destination.exists():
        shutil.rmtree(destination)
    shutil.copytree(build, destination)
    record = {"name": ref, "path": ref + "/", "commit": commit, "kind": kind}
    records = [old for old in records if old["name"] != ref] + [record]
    stable = sorted((item for item in records if item["kind"] == "stable"), key=order, reverse=True)
    stable_bases = {item["name"] for item in stable}
    previews = sorted((item for item in records if item["kind"] == "prerelease"
                       and item["name"].split("-", 1)[0] not in stable_bases), key=order, reverse=True)
    development = [item for item in records if item["name"] == "main"]
    visible = stable + development + previews
    latest = (stable or development or previews)[0]
    manifest.write_text(json.dumps({"latest": latest["name"], "versions": visible,
                                    "published": sorted(records, key=lambda item: item["name"])}, indent=2) + "\n")
    url = html.escape(root_url + latest["path"], quote=True)
    (pages / "index.html").write_text(
        '<!doctype html><html lang="en"><head><meta charset="utf-8">'
        '<meta name="viewport" content="width=device-width, initial-scale=1">'
        '<title>GPUStack Operator documentation</title>'
        f'<link rel="canonical" href="{url}"><meta http-equiv="refresh" content="0; url={url}">'
        f'</head><body><p><a href="{url}">Read the {html.escape(latest["name"])} documentation</a>.'
        '</p></body></html>\n')
    shutil.copyfile(pages / latest["path"] / "llms.txt", pages / "llms.txt")
    (pages / ".nojekyll").touch()
    (pages / "robots.txt").write_text("User-agent: *\nAllow: /\n" + "".join(
        f'Sitemap: {root_url}{item["path"]}sitemap.xml\n' for item in (stable or [latest])))
    if inventory(pages / "charts") != charts:
        raise ValueError("chart files changed while composing documentation")
    print(f"Composed {ref}; default {latest['name']}; {len(visible)} visible version(s); charts unchanged.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--build", type=Path, required=True)
    parser.add_argument("--pages", type=Path, required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--root-url", required=True)
    args = parser.parse_args()
    try:
        compose(args.build, args.pages, args.ref, args.commit, args.root_url)
    except (ValueError, KeyError, OSError) as error:
        parser.exit(1, f"{error}\n")
