#!/usr/bin/env python3
"""Shared OCI reference validation and summary formatting helpers for charts preflight."""

import re
import shlex

DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
VERSION_RE = re.compile(r"^[0-9A-Za-z._+-]+$")
OCI_REF_RE = re.compile(r"^[A-Za-z0-9._/@:-]+$")
OCI_SPEC_RE = re.compile(r"^[A-Za-z0-9._/@:>-]+$")


def format_sync_image_command(specs: list[str]) -> str:
    """Format a shell-safe gh workflow run sync-image.yml command for valid OCI specs."""
    valid_specs = [s for s in specs if s and OCI_SPEC_RE.fullmatch(s)]
    if not valid_specs:
        return ""
    return f"gh workflow run sync-image.yml -f images={shlex.quote(' '.join(valid_specs))}"


def sanitize_md_inline_code(val: str) -> str:
    """Sanitize a string for safe embedding inside a Markdown table inline-code span.

    Strips angle brackets rather than HTML-escaping them because GitHub renders
    &lt;/&gt; literally inside backtick spans.
    """
    return (
        str(val)
        .replace("\r", " ")
        .replace("\n", " ")
        .replace("`", "'")
        .replace("|", "\\|")
        .replace("<", "")
        .replace(">", "")
    )
