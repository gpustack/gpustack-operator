#!/usr/bin/env bash
#
# Report which RDMA readings THIS cluster can answer, then run the consolidated RDMA case (case-14).
#
#   run-rdma-block.sh <RAW_DIR> [NS] [CASES...]
#   run-rdma-block.sh --report-only [NS]
#
# Why a runner and not a list in prose: RDMA checks are gated on host SHAPE, and the gates are the
# point:
#
#   - `--report-only` prints the capability matrix WITHOUT touching the cluster, answering "what can
#     this machine tell me" and naming per unmet requirement the host property that is missing;
#   - a run prints the same matrix first, then runs `case-14.sh` (or caller-supplied CASES) and
#     reports its exit code AND whether its output carries `NOTHING WAS VERIFIED`.
#
# Output is written to <RAW_DIR>/case-<N>.txt so the outcome survives context compaction.
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
CASES="${*:-14}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------------------------------
# The capability matrix, printed by sourcing the same _rdma-lib.sh predicates case-14 gates on.
# ---------------------------------------------------------------------------------------------------
report() {
  # shellcheck disable=SC2034 # Consumed by _rdma-lib.sh below.
  CASE_ID=matrix
  # shellcheck source=/dev/null
  . "${HERE}/_rdma-lib.sh"

  echo "== RDMA readings this cluster can answer =="
  echo
  local nodes node req
  nodes=$(rdma_nodes | grep -v '^$' || true)
  if [ -z "$nodes" ]; then
    echo "No node carries a Devices object: no device manager is running, so case 14 Legs 2-4 will"
    echo "record SKIP. Leg 1 (host-fabric KVCacheBackend DaemonSet render) needs no Devices object"
    echo "and still runs on any cluster."
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

  echo "What each leg of case 14 needs, and therefore which rows above gate it:"
  echo "  case 14 leg 1  nothing from this matrix                 (renders RDMA KVCacheBackend DaemonSet;"
  echo "                                                            pulls no store image and starts no member)"
  echo "  case 14 leg 2  Devices object present                   (NIC/RDMA inventory, rdma.* labels,"
  echo "                                                            quiet-pass stability, and DM restart)"
  echo "  case 14 leg 3  endpoint                                 (extended resource sizing; SR-IOV and"
  echo "                                                            failed-link sub-checks skip individually)"
  echo "  case 14 leg 4  endpoint, endpoint-whole                 (container device-cgroup open() isolation,"
  echo "                                                            + E2E_RDMA_IMAGE for ibv_devinfo)"
  echo
  echo "A requirement reported UNMET is a statement about this machine; see references/rdma-host-shapes.md."
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
  out="${RAW}/case-${n}.txt"
  echo
  echo "---------- CASE ${n} start $(date +%H:%M:%S) ----------"
  bash "${HERE}/case-${n}.sh" "$NS" >"$out" 2>&1
  rc=$?
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
echo "   A skipped leg or case answered NOTHING for that hardware shape; see references/rdma-host-shapes.md."
[ -z "$FAILED" ]
