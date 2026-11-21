"""Opt-in sitecustomize hook for host-staged PYTHONPATH gpustack-engine-sentinel.

Remains completely dormant unless GPUSTACK_ENGINE_SENTINEL_ENABLE == "1",
and chains any downstream sitecustomize.py present later on sys.path without
re-entering already visited sitecustomize files.
"""

from __future__ import annotations

import importlib.machinery
import importlib.util
import os
import sys
from pathlib import Path

_VISITED_ATTR = "_gpustack_sitecustomize_visited"


def _visited_set() -> set[str]:
    visited = getattr(sys, _VISITED_ATTR, None)
    if not isinstance(visited, set):
        visited = set()
        setattr(sys, _VISITED_ATTR, visited)
    return visited


def _chain_downstream_sitecustomize(current_file: Path, visited: set[str]) -> None:
    this_dir = current_file.parent
    remaining_paths: list[str] = []
    seen_this_dir = False
    for entry in sys.path:
        try:
            resolved = Path(entry or ".").resolve()
        except Exception:
            continue
        if resolved == this_dir:
            seen_this_dir = True
            continue
        if seen_this_dir:
            remaining_paths.append(entry)

    while remaining_paths:
        spec = importlib.machinery.PathFinder.find_spec(
            "sitecustomize", remaining_paths
        )
        if spec is None or spec.loader is None:
            return
        origin_raw = Path(spec.origin) if spec.origin else None
        origin = origin_raw.resolve() if origin_raw else None
        if origin is None or origin_raw is None:
            return
        origin_key = str(origin)
        if origin_key in visited:
            origin_dir = origin.parent
            raw_parent = origin_raw.parent
            try:
                raw_parent_resolved = raw_parent.resolve()
            except Exception:
                raw_parent_resolved = raw_parent
            next_remaining: list[str] = []
            for entry in remaining_paths:
                entry_path = Path(entry or ".")
                try:
                    entry_resolved = entry_path.resolve()
                except Exception:
                    entry_resolved = entry_path
                if (
                    entry_resolved in (origin_dir, raw_parent_resolved)
                    or entry_path == raw_parent
                ):
                    continue
                next_remaining.append(entry)
            if len(next_remaining) == len(remaining_paths):
                # Guarantee strictly decreasing search list even on custom path aliases.
                next_remaining = remaining_paths[1:]
            remaining_paths = next_remaining
            continue

        chained_mod = importlib.util.module_from_spec(spec)
        try:
            spec.loader.exec_module(chained_mod)
        except Exception as err:
            # Match CPython site.execsitecustomize resilience so a broken downstream
            # sitecustomize never blocks engine startup or sentinel bootstrap.
            sys.stderr.write(
                f"Error in chained sitecustomize ({origin}): {err.__class__.__name__}: {err}\n"
            )
        visited.add(origin_key)
        return


_current_file = Path(__file__).resolve()
_visited = _visited_set()
_current_key = str(_current_file)
if _current_key not in _visited:
    _visited.add(_current_key)
    _chain_downstream_sitecustomize(_current_file, _visited)
    if os.environ.get("GPUSTACK_ENGINE_SENTINEL_ENABLE") == "1":
        import gpustack_engine_sentinel

        gpustack_engine_sentinel.bootstrap_process()
