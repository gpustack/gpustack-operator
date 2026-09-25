"""An independent implementation of the canonical manifest v1, kept beside the Go one so that the
format is checked against a second reading of its rules rather than against itself.

Usage: python3 canonical_manifest.py <Hugging Face tree listing as a JSON array> [shuffle seed]
       [--allow PATTERN]... [--ignore PATTERN]...
It prints the manifest digest on the first line. The patterns filter the files by rule 5 of the
format, with Python's own fnmatch.fnmatchcase.
"""
import argparse
import fnmatch
import hashlib
import json
import random

parser = argparse.ArgumentParser()
parser.add_argument("tree")
parser.add_argument("seed", nargs="?", type=int, default=0)
parser.add_argument("--allow", action="append", default=[])
parser.add_argument("--ignore", action="append", default=[])
args = parser.parse_args()


def directory_wildcard(pattern):
    return pattern + "*" if pattern.endswith("/") else pattern


allow = [directory_wildcard(p) for p in args.allow]
ignore = [directory_wildcard(p) for p in args.ignore]

tree = json.load(open(args.tree))
random.Random(args.seed).shuffle(tree)
lines = []
for t in tree:
    if t["type"] != "file":
        continue
    if allow and not any(fnmatch.fnmatchcase(t["path"], p) for p in allow):
        continue
    if any(fnmatch.fnmatchcase(t["path"], p) for p in ignore):
        continue
    digest = "sha256:" + t["lfs"]["oid"] if t.get("lfs") else "gitsha1:" + t["oid"]
    path = t["path"].encode("utf-8")
    lines.append((path, f"{digest} {t['size']} ".encode() + path + b"\n"))
body = b"gpustack-manifest v1\n" + b"".join(line for _, line in sorted(lines))
print("sha256:" + hashlib.sha256(body).hexdigest())
