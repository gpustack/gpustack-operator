#!/usr/bin/env python3
"""Compose versioned documentation into a checkout of the github-pages branch."""

import argparse
import hashlib
import html
import json
import os
import re
import shutil
import stat
import sys
import tempfile
from pathlib import Path
from urllib.parse import urlsplit

_SCRIPT_DIR = str(Path(__file__).resolve().parent)
if _SCRIPT_DIR not in sys.path:
    sys.path.insert(0, _SCRIPT_DIR)

import rewrite_reference  # noqa: E402

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


def _remove_path(path: Path) -> None:
    if path.is_symlink() or path.is_file():
        path.unlink()
    elif path.is_dir():
        shutil.rmtree(path)


def _validate_regular_tree(root: Path, label: str) -> None:
    try:
        root_stat = os.lstat(root)
    except OSError as error:
        raise ValueError(f"{label} does not exist: {root}") from error
    if stat.S_ISLNK(root_stat.st_mode):
        raise ValueError(f"{label} must not be a symlink: {root}")
    if not stat.S_ISDIR(root_stat.st_mode):
        raise ValueError(f"{label} must be a directory: {root}")

    stack = [root]
    while stack:
        current = stack.pop()
        with os.scandir(current) as entries:
            for entry in sorted(entries, key=lambda item: item.name):
                entry_stat = entry.stat(follow_symlinks=False)
                rel = Path(entry.path).relative_to(root)
                if stat.S_ISLNK(entry_stat.st_mode):
                    raise ValueError(f"{label} must not contain symlinks: {rel}")
                if stat.S_ISDIR(entry_stat.st_mode):
                    stack.append(Path(entry.path))
                elif not stat.S_ISREG(entry_stat.st_mode):
                    raise ValueError(
                        f"{label} must contain only regular files and directories: {rel}"
                    )


def _validate_flat_reference(ref_dir: Path) -> None:
    if not ref_dir.is_dir():
        return
    for entry in sorted(ref_dir.iterdir(), key=lambda item: item.name):
        if entry.is_dir():
            raise ValueError(f"reference directory must be flat; found subdirectory: {entry.name}")


def _list_flat_md_names(ref_dir: Path) -> list[str]:
    if ref_dir.is_symlink() or not ref_dir.is_dir():
        return []
    names = []
    with os.scandir(ref_dir) as entries:
        for entry in sorted(entries, key=lambda item: item.name):
            entry_stat = entry.stat(follow_symlinks=False)
            if stat.S_ISREG(entry_stat.st_mode) and entry.name.endswith(".md"):
                names.append(entry.name)
    return names


def _is_matching_regular_dir(target_dir: Path, expected_inventory: dict[str, str]) -> bool:
    try:
        _validate_regular_tree(target_dir, "root reference")
    except ValueError:
        return False
    return inventory(target_dir) == expected_inventory


def _write_bytes_if_changed(path: Path, data: bytes) -> None:
    if path.is_symlink():
        path.unlink()
    elif path.is_file() and path.read_bytes() == data:
        return
    elif path.is_dir():
        shutil.rmtree(path)
    path.write_bytes(data)


def _ensure_empty_file(path: Path) -> None:
    if path.is_symlink():
        path.unlink()
    elif path.is_file() and path.stat().st_size == 0:
        return
    elif path.is_dir():
        shutil.rmtree(path)
    path.write_bytes(b"")


def _stage_root_reference(
    pages: Path,
    latest_dir: Path,
    latest: dict,
    root_url: str,
) -> tuple[Path | None, dict[str, str] | None]:
    source_dir = latest_dir / "reference"
    if not source_dir.is_dir():
        return None, None

    staging_dir = Path(tempfile.mkdtemp(prefix=".reference-staging-", dir=pages))
    try:
        staged = rewrite_reference.stage_reference_dir(
            source_dir,
            staging_dir,
            docs_prefix=f"{root_url}{latest['path']}docs",
            rebase_from=rewrite_reference.DEFAULT_DOCS_PREFIX,
        )
        if not staged:
            shutil.rmtree(staging_dir, ignore_errors=True)
            return None, None
        os.chmod(staging_dir, 0o755)
        return staging_dir, inventory(staging_dir)
    except Exception:
        shutil.rmtree(staging_dir, ignore_errors=True)
        raise


def _swap_root_reference(
    pages: Path,
    staging_dir: Path | None,
    staged_inventory: dict[str, str] | None,
) -> None:
    target_dir = pages / "reference"
    if staging_dir is None or staged_inventory is None:
        if target_dir.exists() or target_dir.is_symlink():
            _remove_path(target_dir)
        return

    if _is_matching_regular_dir(target_dir, staged_inventory):
        return

    if target_dir.exists() or target_dir.is_symlink():
        backup_parent = Path(tempfile.mkdtemp(prefix=".reference-backup-", dir=pages))
        backup_path = backup_parent / "reference"
        moved_to_backup = False
        try:
            os.replace(target_dir, backup_path)
            moved_to_backup = True
            os.replace(staging_dir, target_dir)
        except OSError:
            if moved_to_backup:
                os.replace(backup_path, target_dir)
                shutil.rmtree(backup_parent, ignore_errors=True)
            else:
                shutil.rmtree(backup_parent, ignore_errors=True)
            raise
        else:
            shutil.rmtree(backup_parent, ignore_errors=True)
    else:
        os.replace(staging_dir, target_dir)


def compose(build, pages, ref, commit, root_url, *, obsolete_manifest=None):
    kind = version_kind(ref)
    base = urlsplit(root_url)
    if base.scheme != "https" or not base.netloc or base.query or base.fragment or not root_url.endswith("/"):
        raise ValueError("root URL must be an HTTPS directory URL ending in /")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("source commit must be a full Git commit SHA")

    build_raw = Path(build)
    pages_raw = Path(pages)
    _validate_regular_tree(build_raw, "build")
    build, pages = build_raw.resolve(), pages_raw.resolve()
    if build.is_relative_to(pages) or pages.is_relative_to(build):
        raise ValueError("build and Pages checkout must be separate directories")

    obsolete_manifest_path = None
    if obsolete_manifest is not None:
        obsolete_manifest_path = Path(obsolete_manifest).resolve()
        if obsolete_manifest_path.is_relative_to(pages):
            raise ValueError("obsolete manifest must be stored outside the Pages checkout")

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
    if destination.exists() or destination.is_symlink():
        _validate_regular_tree(destination, f"existing snapshot {ref}")

    idempotent_retry = False
    if kind != "development" and previous:
        if not destination.is_dir() or inventory(build) != inventory(destination):
            raise ValueError(
                "published tag documentation is immutable; retry build bytes differ from existing snapshot"
            )
        idempotent_retry = True
    _validate_flat_reference(build / "reference")

    record = {"name": ref, "path": ref + "/", "commit": commit, "kind": kind}
    next_records = [old for old in records if old["name"] != ref] + [record]
    stable = sorted((item for item in next_records if item["kind"] == "stable"), key=order, reverse=True)
    stable_bases = {item["name"] for item in stable}
    previews = sorted((item for item in next_records if item["kind"] == "prerelease"
                       and item["name"].split("-", 1)[0] not in stable_bases), key=order, reverse=True)
    development = [item for item in next_records if item["name"] == "main"]
    visible = stable + development + previews
    latest = (stable or development or previews)[0]

    latest_dir = build if latest["name"] == ref else pages / latest["path"]
    manifest_bytes = (
        json.dumps(
            {
                "latest": latest["name"],
                "versions": visible,
                "published": sorted(next_records, key=lambda item: item["name"]),
            },
            indent=2,
        )
        + "\n"
    ).encode("utf-8")
    url = html.escape(root_url + latest["path"], quote=True)
    index_html_bytes = (
        '<!doctype html><html lang="en"><head><meta charset="utf-8">'
        '<meta name="viewport" content="width=device-width, initial-scale=1">'
        '<title>GPUStack Operator documentation</title>'
        f'<link rel="canonical" href="{url}"><meta http-equiv="refresh" content="0; url={url}">'
        f'</head><body><p><a href="{url}">Read the {html.escape(latest["name"])} documentation</a>.'
        '</p></body></html>\n'
    ).encode("utf-8")
    llms_bytes = (latest_dir / "llms.txt").read_bytes()
    robots_bytes = (
        "User-agent: *\nAllow: /\n"
        + "".join(f'Sitemap: {root_url}{item["path"]}sitemap.xml\n' for item in (stable or [latest]))
    ).encode("utf-8")

    # Capture pre-compose aliases and stage root reference/ before mutating any file in pages.
    pre_root_refs = _list_flat_md_names(pages / "reference")
    pre_main_refs = _list_flat_md_names(pages / "main" / "reference") if ref == "main" else []

    staging_dir, staged_ref_inventory = _stage_root_reference(pages, latest_dir, latest, root_url)
    try:
        if not idempotent_retry:
            if destination.exists():
                shutil.rmtree(destination)
            shutil.copytree(build, destination)
        _write_bytes_if_changed(manifest, manifest_bytes)
        _write_bytes_if_changed(pages / "index.html", index_html_bytes)
        _write_bytes_if_changed(pages / "llms.txt", llms_bytes)
        _swap_root_reference(pages, staging_dir, staged_ref_inventory)
        _ensure_empty_file(pages / ".nojekyll")
        _write_bytes_if_changed(pages / "robots.txt", robots_bytes)
    finally:
        if staging_dir is not None and staging_dir.exists():
            shutil.rmtree(staging_dir, ignore_errors=True)

    if inventory(pages / "charts") != charts:
        raise ValueError("chart files changed while composing documentation")

    if obsolete_manifest_path is not None:
        post_root_refs = set(_list_flat_md_names(pages / "reference"))
        removed = {f"reference/{name}" for name in set(pre_root_refs) - post_root_refs}
        if ref == "main":
            post_main_refs = set(_list_flat_md_names(pages / "main" / "reference"))
            removed.update(f"main/reference/{name}" for name in set(pre_main_refs) - post_main_refs)
        obsolete_manifest_path.parent.mkdir(parents=True, exist_ok=True)
        obsolete_manifest_path.write_text(
            json.dumps({"obsolete_paths": sorted(removed)}, indent=2) + "\n",
            encoding="utf-8",
        )

    print(f"Composed {ref}; default {latest['name']}; {len(visible)} visible version(s); charts unchanged.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--build", type=Path, required=True)
    parser.add_argument("--pages", type=Path, required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--root-url", required=True)
    parser.add_argument(
        "--obsolete-manifest",
        type=Path,
        default=None,
        help="Optional JSON file outside --pages recording removed reference paths",
    )
    args = parser.parse_args()
    try:
        compose(
            args.build,
            args.pages,
            args.ref,
            args.commit,
            args.root_url,
            obsolete_manifest=args.obsolete_manifest,
        )
    except (ValueError, KeyError, OSError) as error:
        parser.exit(1, f"{error}\n")
