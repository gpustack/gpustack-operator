#!/usr/bin/env bash
# Hardware qualification for the ModelDeployment scaling chain on the 8x NVIDIA H200 SXM node.
# Modes: --prepare, or --suite {sweep,capacity-arms,sim-sensitivity,builtin-catalog,pd-epd};
# every case is bounded and dumps failure_summary.json when it fails or overruns.
set -euo pipefail
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"
export BENCH_INCLUSTER=1
exec python3 -B "${SKILL_DIR}/scripts/autoscaling_qualification.py" "$@"
