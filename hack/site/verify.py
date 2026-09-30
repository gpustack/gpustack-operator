#!/usr/bin/env python3
"""Verify that public Pages files match the composed publication snapshot."""

import argparse
import hashlib
import json
import time
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


def verify(pages, ref, root_url):
    manifest = json.loads((pages / "versions.json").read_text())
    files = ["index.html", "versions.json", "llms.txt", "charts/index.yaml"]
    files.extend(record["path"] + "site-version.json" for record in manifest["published"])
    if ref != "main":
        files.append(f"charts/gpustack-operator-{ref.removeprefix('v')}.tgz")
    expected = {name: hashlib.sha256((pages / name).read_bytes()).digest() for name in files}
    deadline = time.monotonic() + 300
    while True:
        failures = []
        for name, digest in expected.items():
            try:
                request = Request(root_url + name, headers={"Cache-Control": "no-cache"})
                with urlopen(request, timeout=20) as response:
                    actual = hashlib.sha256(response.read()).digest()
                if actual != digest:
                    failures.append(f"{name}: public bytes differ")
            except (HTTPError, URLError, TimeoutError) as error:
                failures.append(f"{name}: {error}")
        if not failures:
            print(f"Public publication matches {ref}, its version manifest and chart bytes.")
            return
        if time.monotonic() >= deadline:
            raise ValueError("Public publication did not converge:\n" + "\n".join(failures))
        print("Waiting for Pages propagation: " + "; ".join(failures), flush=True)
        time.sleep(10)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pages", type=Path, required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--root-url", required=True)
    args = parser.parse_args()
    try:
        verify(args.pages, args.ref, args.root_url)
    except (ValueError, KeyError, OSError) as error:
        parser.exit(1, f"{error}\n")
