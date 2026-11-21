#!/usr/bin/env python3
"""Rewrite relative links in flat reference Markdown files and fresh-mirror stage them."""

import argparse
import os
import posixpath
import re
import shutil
from pathlib import Path
from typing import Callable
from urllib.parse import urlsplit

DEFAULT_DOCS_PREFIX = "../docs"
DEFAULT_SOURCE_REF = "main"
DEFAULT_REPO_BLOB_BASE = "https://github.com/gpustack/gpustack-operator/blob"

_FENCE_OPEN_RE = re.compile(r"((?: {0,3}> ?)*) {0,3}(`{3,}|~{3,})([^\r\n]*)(\r?\n|$)")
_REF_DEF_START_RE = re.compile(r" {0,3}\[")


def _split_suffix(dest: str) -> tuple[str, str]:
    """Split path/origin from ?query and/or #fragment verbatim."""
    for idx, ch in enumerate(dest):
        if ch in "?#":
            return dest[:idx], dest[idx:]
    return dest, ""


def _validate_non_empty(value: str, label: str, *, strip_slash: bool = False) -> str:
    cleaned = value.rstrip("/") if strip_slash else value
    if not cleaned:
        raise ValueError(f"{label} must not be empty")
    return cleaned


def rewrite_destination(
    dest: str,
    *,
    docs_prefix: str = DEFAULT_DOCS_PREFIX,
    source_ref: str = DEFAULT_SOURCE_REF,
    repo_blob_base: str = DEFAULT_REPO_BLOB_BASE,
) -> str:
    """Rewrite a single link destination authored inside docs/reference/<file>.md."""
    prefix = _validate_non_empty(docs_prefix, "docs_prefix", strip_slash=True)
    ref = _validate_non_empty(source_ref, "source_ref")
    blob_base = _validate_non_empty(repo_blob_base, "repo_blob_base", strip_slash=True)

    if not dest:
        return dest

    parsed = urlsplit(dest)
    if parsed.scheme or parsed.netloc:
        return dest

    path_part, suffix = _split_suffix(dest)
    if not path_part or path_part.startswith("/"):
        return dest

    norm = posixpath.normpath(posixpath.join("docs/reference", path_part))
    if norm == ".." or norm.startswith("../"):
        raise ValueError(f"link escapes repository root: {dest}")

    if norm == ".":
        return f"{blob_base}/{ref}{suffix}"

    if norm == "docs/reference":
        return f".{suffix}"

    if norm.startswith("docs/reference/"):
        return f"{norm[len('docs/reference/'):]}{suffix}"

    if norm.startswith("docs/assets/"):
        return f"{prefix}/assets/{norm[len('docs/assets/'):]}{suffix}"

    if norm == "docs/README.md" or norm == "docs/contribute" or norm.startswith("docs/contribute/"):
        return f"{blob_base}/{ref}/{norm}{suffix}"

    if norm.startswith("docs/"):
        if norm == "docs/_index.md":
            return f"{prefix}/index.md{suffix}"
        if norm.endswith("/_index.md"):
            rel_dir = norm[len("docs/") : -len("/_index.md")]
            return f"{prefix}/{rel_dir}/index.md{suffix}"
        if norm.endswith(".md"):
            rel_no_ext = norm[len("docs/") : -len(".md")]
            return f"{prefix}/{rel_no_ext}/index.md{suffix}"
        return f"{blob_base}/{ref}/{norm}{suffix}"

    return f"{blob_base}/{ref}/{norm}{suffix}"


def rebase_destination(
    dest: str,
    *,
    from_prefix: str = DEFAULT_DOCS_PREFIX,
    to_prefix: str,
) -> str:
    """Rebase an already-rewritten docs link from one docs prefix to another."""
    src_prefix = _validate_non_empty(from_prefix, "from_prefix", strip_slash=True)
    dst_prefix = _validate_non_empty(to_prefix, "to_prefix", strip_slash=True)

    if not dest:
        return dest

    parsed = urlsplit(dest)
    if parsed.scheme or parsed.netloc:
        return dest

    path_part, suffix = _split_suffix(dest)
    if path_part == src_prefix:
        return f"{dst_prefix}{suffix}"
    if path_part.startswith(src_prefix + "/"):
        return f"{dst_prefix}{path_part[len(src_prefix):]}{suffix}"
    return dest


def _starts_blank_line(text: str, newline_pos: int) -> bool:
    """Return True if the line immediately following newline_pos is blank."""
    p = newline_pos + 1
    n = len(text)
    while p < n and text[p] in (" ", "\t"):
        p += 1
    if p < n and text[p] == "\r":
        p += 1
    return p < n and text[p] == "\n"


def _scan_code_span(text: str, start: int) -> int | None:
    """Return end index of an inline code span starting at start, or None if unmatched."""
    n = len(text)
    p = start
    while p < n and text[p] == "`":
        p += 1
    tick_len = p - start
    while p < n:
        ch = text[p]
        if ch == "\n" and _starts_blank_line(text, p):
            return None
        if ch == "`":
            run_start = p
            while p < n and text[p] == "`":
                p += 1
            if p - run_start == tick_len:
                return p
            continue
        p += 1
    return None


def _skip_ws_no_blank(text: str, pos: int) -> int | None:
    """Advance past spaces/tabs/newlines without crossing a blank line."""
    n = len(text)
    p = pos
    while p < n and text[p] in (" ", "\t", "\r", "\n"):
        if text[p] == "\n" and _starts_blank_line(text, p):
            return None
        p += 1
    return p


def _scan_title(text: str, pos: int) -> int | None:
    """Scan a CommonMark link title delimited by quotes or parentheses."""
    n = len(text)
    if pos >= n:
        return None
    opener = text[pos]
    if opener == '"':
        closer = '"'
    elif opener == "'":
        closer = "'"
    elif opener == "(":
        closer = ")"
    else:
        return None

    p = pos + 1
    while p < n:
        ch = text[p]
        if ch == "\\" and p + 1 < n:
            p += 2
            continue
        if ch == "\n" and _starts_blank_line(text, p):
            return None
        if opener == "(" and ch == "(":
            return None
        if ch == closer:
            return p + 1
        p += 1
    return None


def _scan_destination(text: str, pos: int) -> tuple[str, int, bool] | None:
    """Scan an angle-bracketed or unbracketed Markdown link destination."""
    n = len(text)
    if pos >= n:
        return None

    if text[pos] == "<":
        p = pos + 1
        while p < n:
            ch = text[p]
            if ch == "\\" and p + 1 < n:
                p += 2
                continue
            if ch in ("\n", "\r", "<"):
                return None
            if ch == ">":
                return (text[pos + 1 : p], p + 1, True)
            p += 1
        return None

    p = pos
    paren_depth = 0
    while p < n:
        ch = text[p]
        if ch == "\\" and p + 1 < n:
            p += 2
            continue
        if ch in (" ", "\t", "\r", "\n") or ord(ch) < 32:
            break
        if ch == "(":
            paren_depth += 1
        elif ch == ")":
            if paren_depth == 0:
                break
            paren_depth -= 1
        p += 1

    if paren_depth != 0 or p == pos:
        return None
    return (text[pos:p], p, False)


def _consume_fenced_block(text: str, start: int) -> int | None:
    """If a fenced code block starts at line start, return index right after it."""
    match = _FENCE_OPEN_RE.match(text, start)
    if not match:
        return None
    prefix, marker, info = match.group(1), match.group(2), match.group(3)
    fence_char = marker[0]
    fence_len = len(marker)
    # Per CommonMark, a backtick fence info string cannot contain backticks.
    if fence_char == "`" and "`" in info:
        return None

    bq_depth = prefix.count(">")
    close_re = re.compile(rf"^ {{0,3}}{re.escape(fence_char)}{{{fence_len},}}[ \t]*(?:\r?\n|$)")
    bq_re = re.compile(rf"^(?: {{0,3}}> ?){{{bq_depth}}}") if bq_depth else None

    n = len(text)
    pos = match.end()
    while pos < n:
        nl = text.find("\n", pos)
        line_end = n if nl == -1 else nl + 1
        line = text[pos:line_end]
        if bq_re is not None:
            bq_match = bq_re.match(line)
            if not bq_match:
                return pos
            rest = line[bq_match.end() :]
        else:
            rest = line
        pos = line_end
        if close_re.match(rest):
            return pos
    return n


def _try_rewrite_ref_def(
    text: str,
    start: int,
    transform: Callable[[str], str],
) -> tuple[str, int] | None:
    """If a reference-style link definition starts at start, rewrite and return (out, end)."""
    match = _REF_DEF_START_RE.match(text, start)
    if not match:
        return None

    n = len(text)
    p = match.end()
    # GFM footnote definitions begin with [^label]: and contain normal prose.
    if p < n and text[p] == "^":
        return None

    has_non_ws = False
    while p < n:
        ch = text[p]
        if ch == "\\" and p + 1 < n:
            has_non_ws = True
            p += 2
            continue
        if ch == "\n" and _starts_blank_line(text, p):
            return None
        if ch == "[":
            return None
        if ch == "]":
            break
        if ch not in (" ", "\t", "\r", "\n"):
            has_non_ws = True
        p += 1

    if not has_non_ws or p + 1 >= n or text[p] != "]" or text[p + 1] != ":":
        return None

    p += 2
    # Skip spaces/tabs and at most one newline before the destination.
    while p < n and text[p] in (" ", "\t"):
        p += 1
    if p < n and text[p] in ("\r", "\n"):
        if text[p] == "\r" and p + 1 < n and text[p + 1] == "\n":
            p += 2
        else:
            p += 1
        while p < n and text[p] in (" ", "\t"):
            p += 1

    dest_start = p
    scanned = _scan_destination(text, dest_start)
    if scanned is None:
        return None
    dest, dest_end, is_angled = scanned

    # Verify the remainder of the definition line is empty or a valid optional title.
    q = dest_end
    while q < n and text[q] in (" ", "\t"):
        q += 1
    if q == n or text[q] in ("\r", "\n"):
        if q < n:
            q = q + 2 if text[q] == "\r" and q + 1 < n and text[q + 1] == "\n" else q + 1
        end = q
    elif q > dest_end and text[q] in ('"', "'", "("):
        title_end = _scan_title(text, q)
        if title_end is None:
            return None
        r = title_end
        while r < n and text[r] in (" ", "\t"):
            r += 1
        if r < n and text[r] not in ("\r", "\n"):
            return None
        if r < n:
            r = r + 2 if text[r] == "\r" and r + 1 < n and text[r + 1] == "\n" else r + 1
        end = r
    else:
        return None

    new_dest = transform(dest)
    formatted = f"<{new_dest}>" if is_angled else new_dest
    rebuilt = text[start:dest_start] + formatted + text[dest_end:end]
    return rebuilt, end


def _try_rewrite_inline_link(
    text: str,
    start: int,
    transform: Callable[[str], str],
) -> tuple[str, int] | None:
    """If an inline link starts at start ('['), rewrite its label and destination."""
    n = len(text)
    p = start + 1
    depth = 1
    close_bracket = -1

    while p < n:
        ch = text[p]
        if ch == "\\" and p + 1 < n:
            p += 2
            continue
        if text.startswith("<!--", p):
            end_comment = text.find("-->", p + 4)
            if end_comment == -1:
                return None
            p = end_comment + 3
            continue
        if ch == "`":
            end_code = _scan_code_span(text, p)
            if end_code is not None:
                p = end_code
                continue
            while p < n and text[p] == "`":
                p += 1
            continue
        if ch == "\n" and _starts_blank_line(text, p):
            return None
        if ch == "[":
            depth += 1
            p += 1
            continue
        if ch == "]":
            depth -= 1
            if depth == 0:
                close_bracket = p
                break
            p += 1
            continue
        p += 1

    if close_bracket == -1 or close_bracket + 1 >= n or text[close_bracket + 1] != "(":
        return None

    dest_start = _skip_ws_no_blank(text, close_bracket + 2)
    if dest_start is None:
        return None

    if dest_start < n and text[dest_start] == ")":
        inner = _rewrite_text(text[start + 1 : close_bracket], transform, block_level=False)
        return f"[{inner}]({text[close_bracket + 2 : dest_start + 1]}", dest_start + 1

    scanned = _scan_destination(text, dest_start)
    if scanned is None:
        return None
    dest, dest_end, is_angled = scanned

    after_ws = _skip_ws_no_blank(text, dest_end)
    if after_ws is None:
        return None

    q = after_ws
    if q < n and q > dest_end and text[q] in ('"', "'", "("):
        title_end = _scan_title(text, q)
        if title_end is None:
            return None
        after_title = _skip_ws_no_blank(text, title_end)
        if after_title is None:
            return None
        q = after_title

    if q >= n or text[q] != ")":
        return None

    inner = _rewrite_text(text[start + 1 : close_bracket], transform, block_level=False)
    new_dest = transform(dest)
    formatted = f"<{new_dest}>" if is_angled else new_dest
    rebuilt = (
        f"[{inner}]("
        + text[close_bracket + 2 : dest_start]
        + formatted
        + text[dest_end : q + 1]
    )
    return rebuilt, q + 1


def _rewrite_text(
    text: str,
    transform: Callable[[str], str],
    *,
    block_level: bool,
) -> str:
    """Single-pass Markdown scanner applying transform outside exempt regions."""
    out: list[str] = []
    n = len(text)
    i = 0

    while i < n:
        if block_level and (i == 0 or text[i - 1] == "\n"):
            fence_end = _consume_fenced_block(text, i)
            if fence_end is not None:
                out.append(text[i:fence_end])
                i = fence_end
                continue

            ref_result = _try_rewrite_ref_def(text, i, transform)
            if ref_result is not None:
                rebuilt, ref_end = ref_result
                out.append(rebuilt)
                i = ref_end
                continue

        if text.startswith("<!--", i):
            end_comment = text.find("-->", i + 4)
            if end_comment == -1:
                out.append(text[i:])
                break
            out.append(text[i : end_comment + 3])
            i = end_comment + 3
            continue

        ch = text[i]
        if ch == "\\" and i + 1 < n:
            out.append(text[i : i + 2])
            i += 2
            continue

        if ch == "`":
            code_end = _scan_code_span(text, i)
            if code_end is not None:
                out.append(text[i:code_end])
                i = code_end
                continue
            run_start = i
            while i < n and text[i] == "`":
                i += 1
            out.append(text[run_start:i])
            continue

        if ch == "[":
            link_result = _try_rewrite_inline_link(text, i, transform)
            if link_result is not None:
                rebuilt, link_end = link_result
                out.append(rebuilt)
                i = link_end
                continue

        out.append(ch)
        i += 1

    return "".join(out)


def rewrite_markdown(
    text: str,
    *,
    docs_prefix: str = DEFAULT_DOCS_PREFIX,
    source_ref: str = DEFAULT_SOURCE_REF,
    repo_blob_base: str = DEFAULT_REPO_BLOB_BASE,
) -> str:
    """Rewrite all non-exempt Markdown links in a docs/reference/*.md document."""
    _validate_non_empty(docs_prefix, "docs_prefix", strip_slash=True)
    _validate_non_empty(source_ref, "source_ref")
    _validate_non_empty(repo_blob_base, "repo_blob_base", strip_slash=True)
    return _rewrite_text(
        text,
        lambda dest: rewrite_destination(
            dest,
            docs_prefix=docs_prefix,
            source_ref=source_ref,
            repo_blob_base=repo_blob_base,
        ),
        block_level=True,
    )


def rebase_markdown(
    text: str,
    *,
    from_prefix: str = DEFAULT_DOCS_PREFIX,
    to_prefix: str,
) -> str:
    """Rebase already-rewritten docs links outside exempt Markdown regions."""
    _validate_non_empty(from_prefix, "from_prefix", strip_slash=True)
    _validate_non_empty(to_prefix, "to_prefix", strip_slash=True)
    return _rewrite_text(
        text,
        lambda dest: rebase_destination(
            dest,
            from_prefix=from_prefix,
            to_prefix=to_prefix,
        ),
        block_level=True,
    )


def stage_reference_dir(
    source_dir: Path,
    destination_dir: Path,
    *,
    docs_prefix: str = DEFAULT_DOCS_PREFIX,
    source_ref: str = DEFAULT_SOURCE_REF,
    repo_blob_base: str = DEFAULT_REPO_BLOB_BASE,
    rebase_from: str | None = None,
) -> list[str]:
    """Fresh-mirror flat *.md files from source_dir to destination_dir with link rewriting."""
    src = Path(source_dir)
    dst = Path(destination_dir)
    if not src.is_dir():
        raise ValueError(f"source directory does not exist: {src}")
    if src.resolve() == dst.resolve():
        raise ValueError("source and destination directories must differ")
    if dst.exists() and not dst.is_dir():
        raise ValueError(f"destination path is not a directory: {dst}")

    if rebase_from is not None:
        _validate_non_empty(rebase_from, "rebase_from", strip_slash=True)
        _validate_non_empty(docs_prefix, "docs_prefix", strip_slash=True)
    else:
        _validate_non_empty(docs_prefix, "docs_prefix", strip_slash=True)
        _validate_non_empty(source_ref, "source_ref")
        _validate_non_empty(repo_blob_base, "repo_blob_base", strip_slash=True)

    staged: list[tuple[str, bytes]] = []
    for entry in sorted(src.iterdir(), key=lambda p: p.name):
        if entry.is_dir():
            raise ValueError(f"reference directory must be flat; found subdirectory: {entry.name}")
        if not entry.is_file() or entry.suffix != ".md":
            continue
        raw = entry.read_text(encoding="utf-8")
        if rebase_from is not None:
            rewritten = rebase_markdown(raw, from_prefix=rebase_from, to_prefix=docs_prefix)
        else:
            rewritten = rewrite_markdown(
                raw,
                docs_prefix=docs_prefix,
                source_ref=source_ref,
                repo_blob_base=repo_blob_base,
            )
        staged.append((entry.name, rewritten.encode("utf-8")))

    dst.mkdir(parents=True, exist_ok=True)
    expected = {name for name, _ in staged}
    for existing in list(dst.iterdir()):
        if existing.is_dir() and not existing.is_symlink():
            shutil.rmtree(existing)
        elif existing.is_symlink() or existing.name not in expected:
            existing.unlink()

    for name, payload in staged:
        (dst / name).write_bytes(payload)

    return [name for name, _ in staged]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True, help="Flat source reference directory")
    parser.add_argument("--destination", type=Path, required=True, help="Target staging directory")
    parser.add_argument(
        "--docs-prefix",
        default=DEFAULT_DOCS_PREFIX,
        help="Prefix for public docs and asset links (default: ../docs)",
    )
    parser.add_argument(
        "--source-ref",
        default=os.environ.get("HUGO_PARAMS_SOURCEREF") or DEFAULT_SOURCE_REF,
        help="Git ref or commit SHA for repository and contribute links",
    )
    parser.add_argument(
        "--repo-blob-base",
        default=DEFAULT_REPO_BLOB_BASE,
        help="Base GitHub blob URL for repository links",
    )
    parser.add_argument(
        "--rebase-from",
        default=None,
        help="Rebase already-rewritten links from this prefix to --docs-prefix",
    )
    args = parser.parse_args(argv)
    try:
        stage_reference_dir(
            args.source,
            args.destination,
            docs_prefix=args.docs_prefix,
            source_ref=args.source_ref,
            repo_blob_base=args.repo_blob_base,
            rebase_from=args.rebase_from,
        )
    except (ValueError, OSError) as error:
        parser.exit(1, f"{error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
