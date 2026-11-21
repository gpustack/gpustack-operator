#!/usr/bin/env python3
"""Verify that public Pages files match the composed publication snapshot."""

import argparse
import hashlib
import json
import sys
import time
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener, urlopen

_SCRIPT_DIR = str(Path(__file__).resolve().parent)
if _SCRIPT_DIR not in sys.path:
    sys.path.insert(0, _SCRIPT_DIR)

import publish  # noqa: E402


class _NoRedirectHandler(HTTPRedirectHandler):
    """Disable automatic redirect following so absence probes require direct HTTP 404."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _resolve_contained(pages: Path, rel_path: str) -> Path:
    current = pages
    for part in Path(rel_path).parts:
        current = current / part
        if current.is_symlink():
            raise ValueError(f"path must not traverse symlinks: {rel_path}")
    resolved = current.resolve()
    if not resolved.is_relative_to(pages):
        raise ValueError(f"path escapes Pages directory: {rel_path}")
    return resolved


def _flat_reference_names(pages: Path, rel_dir: str) -> list[str]:
    ref_dir = _resolve_contained(pages, rel_dir)
    if not ref_dir.is_dir():
        return []
    names = []
    for entry in sorted(ref_dir.iterdir(), key=lambda item: item.name):
        if entry.is_symlink():
            raise ValueError(f"reference directory must not contain symlinks: {entry.name}")
        if not entry.resolve().is_relative_to(pages):
            raise ValueError(f"reference entry escapes Pages directory: {entry.name}")
        if entry.is_dir():
            raise ValueError(f"reference directory must be flat; found subdirectory: {entry.name}")
        if entry.is_file() and entry.suffix == ".md":
            names.append(entry.name)
    return names


def _normalize_obsolete_path(path: str) -> str:
    if not isinstance(path, str):
        raise ValueError(f"invalid obsolete path: {path!r}")
    if any(ord(ch) < 32 or ord(ch) == 127 or ch.isspace() or ch in "?#%\\" for ch in path):
        raise ValueError(f"invalid obsolete path: {path!r}")
    cleaned = path.lstrip("/")
    if not cleaned or any(part in ("", ".", "..") for part in cleaned.split("/")):
        raise ValueError(f"invalid obsolete path: {path!r}")
    return cleaned


def _load_obsolete_manifest(manifest_path: Path | None) -> list[str]:
    if manifest_path is None:
        return []
    payload = json.loads(Path(manifest_path).read_text(encoding="utf-8"))
    if not isinstance(payload, dict) or not isinstance(payload.get("obsolete_paths"), list):
        raise ValueError("obsolete manifest must be a JSON object with an obsolete_paths list")
    paths = payload["obsolete_paths"]
    if not all(isinstance(item, str) for item in paths):
        raise ValueError("obsolete manifest obsolete_paths entries must be strings")
    return paths


def _collect_expected_and_absent(
    pages: Path,
    ref: str,
    obsolete_paths,
    obsolete_manifest: Path | None,
) -> tuple[dict[str, bytes], list[str]]:
    publish.version_kind(ref)
    pages = Path(pages).resolve()
    manifest_file = _resolve_contained(pages, "versions.json")
    manifest = json.loads(manifest_file.read_text(encoding="utf-8"))
    if not isinstance(manifest, dict):
        raise ValueError("versions.json must be a JSON object")
    published = manifest.get("published")
    if not isinstance(published, list):
        raise ValueError("versions.json published field must be a list")
    latest_name = manifest.get("latest")
    if not isinstance(latest_name, str):
        raise ValueError("versions.json latest field must be a string")
    publish.version_kind(latest_name)

    for record in published:
        if not isinstance(record, dict) or not isinstance(record.get("name"), str) or not isinstance(record.get("path"), str):
            raise ValueError("invalid version record in versions.json")
        publish.version_kind(record["name"])
        if record["path"] != record["name"] + "/":
            raise ValueError("invalid version directory in versions.json")

    latest_record = next((record for record in published if record["name"] == latest_name), None)
    if latest_record is None:
        raise ValueError(f"latest version {latest_name!r} missing from versions.json published list")

    latest_ref_names = _flat_reference_names(pages, f"{latest_record['path']}reference")
    root_ref_candidate = pages / "reference"
    if latest_ref_names:
        if root_ref_candidate.is_symlink():
            raise ValueError("root reference/ must not be a symlink")
        root_ref_dir = _resolve_contained(pages, "reference")
        if (
            not root_ref_dir.is_dir()
            or _flat_reference_names(pages, "reference") != latest_ref_names
            or sorted(entry.name for entry in root_ref_dir.iterdir()) != latest_ref_names
        ):
            raise ValueError("root reference/ directory does not match latest version reference/ files")
    elif root_ref_candidate.exists() or root_ref_candidate.is_symlink():
        raise ValueError("stale root reference/ directory present when latest version has no reference/ files")

    files = ["index.html", "versions.json", "llms.txt", "charts/index.yaml"]
    files.extend(record["path"] + "site-version.json" for record in published)
    if ref != "main":
        files.append(f"charts/gpustack-operator-{ref.removeprefix('v')}.tgz")
    files.extend(f"reference/{name}" for name in latest_ref_names)
    files.extend(f"{latest_record['path']}reference/{name}" for name in latest_ref_names)
    files.extend(f"{ref}/reference/{name}" for name in _flat_reference_names(pages, f"{ref}/reference"))

    deduped = list(dict.fromkeys(files))
    expected = {
        name: hashlib.sha256(_resolve_contained(pages, name).read_bytes()).digest()
        for name in deduped
    }

    root_ref_set = set(latest_ref_names)
    absent_set = set()
    for record in published:
        for name in _flat_reference_names(pages, f"{record['path']}reference"):
            if name not in root_ref_set:
                absent_set.add(f"reference/{name}")
    for raw in _load_obsolete_manifest(obsolete_manifest):
        absent_set.add(_normalize_obsolete_path(raw))
    for raw in obsolete_paths:
        absent_set.add(_normalize_obsolete_path(raw))

    overlap = sorted(absent_set & expected.keys())
    if overlap:
        raise ValueError("paths cannot be both expected and obsolete: " + ", ".join(overlap))
    return expected, sorted(absent_set)


def _encode_url(base_url: str, rel_path: str) -> str:
    return base_url + quote(rel_path, safe="/")


def verify(
    pages,
    ref,
    root_url,
    *,
    obsolete_paths=(),
    obsolete_manifest=None,
    timeout=300,
    poll_interval=10,
    request_timeout=20,
):
    if timeout < 0 or poll_interval < 0 or request_timeout <= 0:
        raise ValueError("timeout and poll_interval must be non-negative and request_timeout must be positive")
    expected, absent = _collect_expected_and_absent(pages, ref, obsolete_paths, obsolete_manifest)
    base_url = root_url if root_url.endswith("/") else root_url + "/"
    is_loopback = urlsplit(base_url).hostname in ("127.0.0.1", "localhost", "::1")
    if is_loopback:
        open_expected = build_opener(ProxyHandler({})).open
        open_absent = build_opener(ProxyHandler({}), _NoRedirectHandler()).open
    else:
        open_expected = urlopen
        open_absent = build_opener(_NoRedirectHandler()).open

    deadline = time.monotonic() + timeout
    last_failures: list[str] = []
    while True:
        failures = []
        deadline_hit = False
        for name, digest in expected.items():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                failures.append(f"{name}: verification deadline exceeded")
                deadline_hit = True
                break
            per_req_timeout = min(request_timeout, remaining)
            try:
                request = Request(_encode_url(base_url, name), headers={"Cache-Control": "no-cache"})
                with open_expected(request, timeout=per_req_timeout) as response:
                    actual = hashlib.sha256(response.read()).digest()
                if actual != digest:
                    failures.append(f"{name}: public bytes differ")
            except (HTTPError, URLError, TimeoutError) as error:
                failures.append(f"{name}: {error}")

        if not deadline_hit:
            for path in absent:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    failures.append(f"{path}: verification deadline exceeded")
                    deadline_hit = True
                    break
                per_req_timeout = min(request_timeout, remaining)
                try:
                    request = Request(_encode_url(base_url, path), headers={"Cache-Control": "no-cache"})
                    with open_absent(request, timeout=per_req_timeout) as response:
                        failures.append(f"{path}: obsolete path still served (HTTP {response.status})")
                except HTTPError as error:
                    try:
                        if error.code != 404:
                            failures.append(f"{path}: expected HTTP 404, got HTTP {error.code}")
                    finally:
                        error.close()
                except (URLError, TimeoutError) as error:
                    failures.append(f"{path}: {error}")

        if not failures:
            print(f"Public publication matches {ref}, its version manifest and chart bytes.")
            return
        remaining = deadline - time.monotonic()
        if deadline_hit or remaining <= 0 or remaining <= poll_interval:
            reported = last_failures if (deadline_hit and last_failures and len(failures) == 1) else failures
            raise ValueError("Public publication did not converge:\n" + "\n".join(reported))
        last_failures = failures
        print("Waiting for Pages propagation: " + "; ".join(failures), flush=True)
        time.sleep(poll_interval)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pages", type=Path, required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--root-url", required=True)
    parser.add_argument(
        "--obsolete-paths",
        nargs="*",
        default=[],
        help="Relative paths that must return HTTP 404",
    )
    parser.add_argument(
        "--obsolete-manifest",
        type=Path,
        default=None,
        help="Optional JSON file containing obsolete_paths produced by publish.py",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=300,
        help="Maximum seconds to wait for Pages propagation",
    )
    parser.add_argument(
        "--poll-interval",
        type=float,
        default=10,
        help="Seconds between verification attempts",
    )
    args = parser.parse_args()
    try:
        verify(
            args.pages,
            args.ref,
            args.root_url,
            obsolete_paths=args.obsolete_paths,
            obsolete_manifest=args.obsolete_manifest,
            timeout=args.timeout,
            poll_interval=args.poll_interval,
        )
    except (ValueError, KeyError, OSError) as error:
        parser.exit(1, f"{error}\n")
