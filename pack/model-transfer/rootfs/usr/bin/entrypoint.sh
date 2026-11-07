#!/usr/bin/env bash
# The source worker's entrypoint: verify this image's runtime packages can be
# located, then hand the process identity to the
# caller's command. The delivery controller (or an operator debugging the
# worker) supplies the command; the image supplies the runtime and the check.
# Native imports and API checks run before the worker offers a source.
#
#   GPUSTACK_MODEL_TRANSFER_BACKEND  nixl | hixl  (optional; an unknown value refuses to
#                          start, because a typo'd backend must not read as a
#                          healthy worker that merely imports nothing)
set -euo pipefail

# The heredoc is quoted, so Python reads its source exactly as written and the
# backend comes from the environment inside Python — never interpolated into
# the source by the shell.
python3 - <<'PYEOF'
import importlib.util
import os
import sys

backend = os.environ.get("GPUSTACK_MODEL_TRANSFER_BACKEND", "")
if backend and backend not in ("nixl", "hixl"):
    print(
        f"GPUSTACK_MODEL_TRANSFER_BACKEND names no known backend: {backend!r} (want nixl or hixl)",
        file=sys.stderr,
    )
    sys.exit(1)

candidates = ["gpustack_model_transfer"]
if backend:
    candidates.append(backend)
missing = [name for name in candidates if importlib.util.find_spec(name) is None]
if missing:
    print(
        "the model-transfer runtime is incomplete; missing: " + ", ".join(missing),
        file=sys.stderr,
    )
    sys.exit(1)
PYEOF

if [[ "$#" -eq 0 ]]; then
  set -- python3 -m gpustack_model_transfer.source
fi
exec "$@"
