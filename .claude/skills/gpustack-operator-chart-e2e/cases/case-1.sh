#!/usr/bin/env bash
#
# CASE 1 — Install + version consistency   (READ-ONLY, mandatory)
#
#   case-1.sh <NS> [EXPECTED_TAG]
#
# Asserts the chart installs and runs (operator core via assert-core.sh) AND that
# the three version views agree: the running binary is built from HEAD, the chart
# tgz bundled in the image matches the binary version, and the deployed image tag
# is the one built. A tgz mismatch is the version it computes via
# deviceManagerChartVersion() — a release/cache bug, not cosmetic. See
# references/version-contract.md. Level-based and safe to re-run.
set -uo pipefail

NS="${1:?usage: case-1.sh <NS> [EXPECTED_TAG]}"
EXPECTED_TAG="${2:-}"
WORKER=deploy/gpustack-operator-worker
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# Operator core first (rollout / revision==HEAD / apiservices / CRDs / sub-releases).
bash "$LIB/assert-core.sh" "$NS" || exit 1

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# Chart tgz bundled in the image matches the binary version. The Dockerfile derives
# the tgz version from `gpustack-operator --version` (strip "v", else 0.0.0).
ver=$(kubectl -n "$NS" exec "$WORKER" -- gpustack-operator --version 2>/dev/null \
        | awk '{for (i=1;i<NF;i++) if ($i=="version") print $(i+1)}')
ver=${ver#v}
[[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || ver=0.0.0
tgz=$(kubectl -n "$NS" exec "$WORKER" -- sh -c 'ls /etc/gpustack/charts/gpustack-operator-*.tgz' 2>/dev/null \
        | sed -E 's#.*/gpustack-operator-(.*)\.tgz#\1#')
[ -n "$ver" ] && [ "$ver" = "$tgz" ] && record PASS "binary version == bundled tgz" "$tgz" \
  || record FAIL "binary version == bundled tgz" "binary [$ver] != tgz [${tgz:-none}] — see Dockerfile packaging / build cache"

# Deployed image tag is the one built (asserted only when EXPECTED_TAG is passed).
img=$(kubectl -n "$NS" get "$WORKER" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
if [ -n "$EXPECTED_TAG" ]; then
  case "$img" in
    *":${EXPECTED_TAG}") record PASS "deployed image tag == built" "$img" ;;
    *) record FAIL "deployed image tag == built" "$img (expected tag ${EXPECTED_TAG})" ;;
  esac
else
  record PASS "deployed image" "$img"
fi

echo
echo "== CASE 1 — Install + version consistency =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). A version mismatch is a release/cache bug — see references/version-contract.md"
  echo "(run CASE 3 to reproduce against a mock tag). Diagnose: kubectl -n ${NS} logs ${WORKER} --tail=200"
  exit 1
fi
echo "CASE 1 PASS"
