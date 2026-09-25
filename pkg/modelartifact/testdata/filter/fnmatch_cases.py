"""Records what Python's fnmatch.fnmatchcase answers for every pattern and path below, so the Go
port of it is checked against CPython itself rather than against a second reading of its rules.

Usage: python3 fnmatch_cases.py > fnmatch_cases.json
"""
import fnmatch
import json
import sys

PATTERNS = [
    "*", "**", "*.json", "**/*.json", "*/*.json", "original/*", "original/", "original/**",
    "Original/*", "*.SAFETENSORS", "*.safetensors", "model-?????-of-?????.safetensors",
    "tokenizer*", "[abc]*", "[!abc]*", "[a-c]*", "[!a-c]*", "[c-a]*", "[!c-a]*", "[]]*", "[!]]*",
    "[a-]*", "[-a]*", "[!-a]*", "[a-c-e]*", "[z-ab-c]*", "[!z-ab-c]*", "[abc", "a[", "[[]*",
    "[\\]*", "\\*", "a\\b", "[^a]*", "[x-z]", "?", "??", "*?*", "a*b*c", "é*", "[é]*", "*/",
    "*.bin", ".git*", "*/config.json", "config.json",
    # Reversed ranges merged across chunks, the one part of the set parser a review doubted.
    "[b-a-c-b]*", "[z-a-b]*", "[!b-a]*", "[c-b-a]*", "[b-a]*", "[a-c-b-z]*",
]

PATHS = [
    "config.json", "a/config.json", "a/b/config.json", "model.safetensors", "Model.SAFETENSORS",
    "model-00001-of-00004.safetensors", "original/consolidated.00.pth", "original/params.json",
    "Original/params.json", "original", "tokenizer.json", "tokenizer_config.json", "abc", "bcd",
    "cde", "def", "]x", "-x", "!x", "^x", "[x", "\\x", "*", "a\\b", "ab", "a", "x", "é.json",
    ".gitattributes", "pytorch_model.bin", "a/b", "aXbYc", "a/b/c", "b", "-",
    # The low bound of a reversed range, which the merge of its chunks drops.
    "z", "zx",
]

cases = []
for p in PATTERNS:
    for n in PATHS:
        cases.append({"pattern": p, "name": n, "match": fnmatch.fnmatchcase(n, p)})
json.dump({"python": sys.version.split()[0], "cases": cases}, sys.stdout, ensure_ascii=False, indent=0)
sys.stdout.write("\n")
