#!/usr/bin/env python3
"""Parse single-line Dockerfile ARG <NAME>=<VALUE> pins."""

from __future__ import annotations

import re
import shlex
import sys


def has_dockerfile_arg(txt: str, name: str) -> bool:
    pat = re.compile(rf"^[ \t]*ARG[ \t]+{re.escape(name)}=")
    return any(pat.match(line) for line in txt.splitlines())


def read_dockerfile_arg(txt: str, name: str, label: str = "Dockerfile") -> str:
    pat = re.compile(rf"^[ \t]*ARG[ \t]+{re.escape(name)}=(.*)$")
    raw_rhs: str | None = None
    for line in txt.splitlines():
        m = pat.match(line)
        if m:
            raw_rhs = m.group(1)
            break
    if raw_rhs is None:
        raise SystemExit(f"FAIL: missing ARG {name} in {label}")

    trimmed_rhs = raw_rhs.rstrip(" \t")
    in_sq = False
    in_dq = False
    escaped = False
    end = len(trimmed_rhs)
    for i, ch in enumerate(trimmed_rhs):
        if escaped:
            escaped = False
            continue
        if in_sq:
            if ch == "'":
                in_sq = False
            continue
        if in_dq:
            if ch == "\\":
                escaped = True
            elif ch == '"':
                in_dq = False
            continue
        if ch == "\\":
            escaped = True
        elif ch == "'":
            in_sq = True
        elif ch == '"':
            in_dq = True
        elif ch == "#":
            if i == 0 or trimmed_rhs[i - 1] in (" ", "\t"):
                end = i
                break
            raise SystemExit(
                f"FAIL: malformed ARG {name} in {label} (unquoted '#' in value)"
            )

    if escaped:
        raise SystemExit(
            f"FAIL: multi-line line continuation in ARG {name} in {label} is not supported"
        )
    if in_sq or in_dq:
        raise SystemExit(f"FAIL: malformed ARG {name} in {label}")

    code = trimmed_rhs[:end].rstrip(" \t")
    if (len(code) - len(code.rstrip("\\"))) % 2 == 1:
        raise SystemExit(
            f"FAIL: multi-line line continuation in ARG {name} in {label} is not supported"
        )

    lex = shlex.shlex(code, posix=True)
    lex.whitespace_split = True
    lex.commenters = ""
    try:
        tokens = list(lex)
    except ValueError as exc:
        raise SystemExit(f"FAIL: malformed ARG {name} in {label}: {exc}") from exc
    if len(tokens) != 1 or not tokens[0]:
        raise SystemExit(f"FAIL: malformed ARG {name} in {label}")
    return tokens[0]


def main(argv: list[str]) -> int:
    optional = False
    args = argv[1:]
    if args and args[0] == "--optional":
        optional = True
        args = args[1:]
    if len(args) not in (2, 3):
        raise SystemExit(
            "usage: dockerfile_arg.py [--optional] <dockerfile_path> <arg_name> [label]"
        )
    path, name = args[0], args[1]
    label = args[2] if len(args) == 3 else path
    try:
        with open(path, "r", encoding="utf-8") as f:
            txt = f.read()
    except (OSError, UnicodeDecodeError) as exc:
        raise SystemExit(f"FAIL: cannot read {path}: {exc}") from exc
    if optional and not has_dockerfile_arg(txt, name):
        return 0
    print(read_dockerfile_arg(txt, name, label))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
