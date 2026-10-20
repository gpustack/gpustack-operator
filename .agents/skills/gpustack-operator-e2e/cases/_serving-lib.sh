#!/usr/bin/env bash
#
# _serving-lib.sh — compatibility shim in gpustack-operator-e2e/cases/ that sources the shared
# serving evidence library at ../../_e2e-lib/scripts/serving-lib.sh (shared between
# gpustack-operator-e2e and gpustack-operator-benchmark).
#
# NOT A CASE. Never run it directly.

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../../_e2e-lib/scripts/serving-lib.sh"
