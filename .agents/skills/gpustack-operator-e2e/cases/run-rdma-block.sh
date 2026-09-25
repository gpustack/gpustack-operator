#!/usr/bin/env bash
#
# Report which RDMA readings THIS cluster can answer, then run the cases that can answer them.
#
#   run-rdma-block.sh <RAW_DIR> [NS] [CASES...]
#   run-rdma-block.sh --report-only [NS]
#
# Why a runner and not a list in prose: every case in this family is gated on a host SHAPE, and the
# gates are the point. A lead who runs the cases and reads only exit codes learns nothing, because a
# case that skipped and a case that passed both exit 0 — by the suite's own convention, which this
# family keeps rather than forks. So:
#
#   - `--report-only` prints the capability matrix WITHOUT touching the cluster, which is what to
#     read before deciding a run is worth the time. It answers "what can this machine tell me", and
#     names, per unmet requirement, the host property that is missing;
#   - a run prints the same matrix first, then each case's exit code AND whether its output carries
#     the marker `NOTHING WAS VERIFIED`. That marker is how a skip is told from a pass, and it is
#     the one thing the summary below asserts about a case that exited 0.
#
# There is NO ordering constraint between these cases: none of them changes a node's hardware state
# and none leaves a baseline for the next one. The numeric order is used because the inventory case
# runs first and its failure explains every later one.
#
# Every case writes to <RAW_DIR>/8<N>-case<N>.txt, so the outcome is readable from the files alone
# after a compaction.
set -uo pipefail

REPORT_ONLY=0
if [ "${1:-}" = "--report-only" ]; then
  REPORT_ONLY=1
  shift
  RAW=""
  NS="${1:-gpustack-system}"
else
  RAW="${1:?usage: run-rdma-block.sh <RAW_DIR> [NS] [CASES...] | --report-only [NS]}"
  NS="${2:-gpustack-system}"
  shift 2 2>/dev/null || shift $#
fi
CASES="${*:-80 81 82 83 84 85}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------------------------------
# The capability matrix.
#
# It is printed by sourcing the same library the cases gate on, so the matrix and the gates cannot
# come to disagree — a second implementation of the predicates would be a second thing to keep in
# step, and the one that drifts is always the report.
# ---------------------------------------------------------------------------------------------------
report() {
  CASE_ID=matrix
  # shellcheck source=/dev/null
  . "${HERE}/_rdma-lib.sh"

  echo "== RDMA readings this cluster can answer =="
  echo
  local nodes node req
  nodes=$(rdma_nodes | grep -v '^$' || true)
  if [ -z "$nodes" ]; then
    echo "No node carries a Devices object: no device manager is running, and none of cases 80-84"
    echo "can answer anything. That is a fact about the cluster, not about the code. Case 85 reads"
    echo "only a rendered DaemonSet and needs nothing from this matrix."
    return 0
  fi

  for node in $nodes; do
    rdma_facts "$node"
    echo "--- ${node}"
    for req in endpoint endpoint-whole sriov failed-link accelerator numa-split topology-enforced partitioned-card; do
      if rdma_requirement_met "$req"; then
        printf '  MET     %-18s %s\n' "$req" "$REQ_READING"
      else
        printf '  UNMET   %-18s %s\n' "$req" "$REQ_READING"
      fi
    done
    echo
  done

  echo "What each case needs, and therefore which of the rows above gates it:"
  echo "  case 80  endpoint                                        (the counts; its SR-IOV and failed-link"
  echo "                                                            checks skip individually)"
  echo "  case 81  endpoint, endpoint-whole                        (+ E2E_RDMA_IMAGE for the verbs check)"
  echo "  case 82  endpoint-whole, accelerator, numa-split,"
  echo "           topology-enforced                               (+ partitioned-card for the observation)"
  echo "  case 83  a device manager and a readable kubelet configz"
  echo "  case 84  endpoint-whole, an InfiniBand link layer,"
  echo "           E2E_RDMA_PERFTEST_IMAGE"
  echo "  case 85  nothing from this matrix                        (renders a backend; pulls no store"
  echo "                                                            image and starts no member)"
  echo
  echo "A requirement reported UNMET is a statement about this machine. references/rdma-host-shapes.md"
  echo "says what a host carrying it looks like, and which reading no host in reach carries at all."
}

report

if [ "$REPORT_ONLY" -eq 1 ]; then
  exit 0
fi

mkdir -p "$RAW"
echo
echo "== rdma block: cases ${CASES} =="
echo "   namespace ${NS}, raw output under ${RAW}"
FAILED=""
SKIPPED=""
for n in $CASES; do
  [ -f "${HERE}/case-${n}.sh" ] || {
    echo "!! no case-${n}.sh — skipping"
    continue
  }
  out="${RAW}/8${n}-case${n}.txt"
  echo
  echo "---------- CASE ${n} start $(date +%H:%M:%S) ----------"
  bash "${HERE}/case-${n}.sh" "$NS" >"$out" 2>&1
  rc=$?
  # Exit 0 alone does not say the case ran. The marker does, and it is the library's own line rather
  # than a heuristic over the prose.
  if grep -q 'NOTHING WAS VERIFIED' "$out"; then
    verdict="SKIPPED (nothing verified)"
    SKIPPED="${SKIPPED}${n} "
  elif [ "$rc" -eq 0 ]; then
    verdict="PASSED"
  else
    verdict="FAILED"
    FAILED="${FAILED}${n} "
  fi
  echo "CASE ${n} exit=${rc} ${verdict} $(date +%H:%M:%S) -> ${out}"
  tail -25 "$out"
done

echo
echo "== rdma block summary =="
echo "   failed:  ${FAILED:-none}"
echo "   skipped: ${SKIPPED:-none}"
echo "   A skipped case answered NOTHING. Read its output for the requirement it named, and"
echo "   references/rdma-host-shapes.md for the host that carries it."
[ -z "$FAILED" ]
