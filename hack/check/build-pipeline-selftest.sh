#!/usr/bin/env bash
# Use local stubs and small compiled fixtures; no image builds, pushes or cluster calls.
set -euo pipefail
ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
python3 "${ROOT}/hack/check/build_pipeline_test.py" "${ROOT}"
