"""An independent implementation of the canonical manifest v1, kept beside the Go one so that the
format is checked against a second reading of its rules rather than against itself.

Usage: python3 canonical_manifest.py <Hugging Face tree listing as a JSON array> [shuffle seed]
It prints the manifest digest on the first line.
"""
import hashlib
import json
import random
import sys

tree = json.load(open(sys.argv[1]))
random.Random(int(sys.argv[2]) if len(sys.argv) > 2 else 0).shuffle(tree)
lines = []
for t in tree:
    if t["type"] != "file":
        continue
    digest = "sha256:" + t["lfs"]["oid"] if t.get("lfs") else "gitsha1:" + t["oid"]
    path = t["path"].encode("utf-8")
    lines.append((path, f"{digest} {t['size']} ".encode() + path + b"\n"))
body = b"gpustack-manifest v1\n" + b"".join(line for _, line in sorted(lines))
print("sha256:" + hashlib.sha256(body).hexdigest())
