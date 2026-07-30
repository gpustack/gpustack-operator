#!/usr/bin/env bash
#
# CASE 1 — Install + version consistency   (READ-ONLY, mandatory)
#
#   case-1.sh <NS> [EXPECTED_TAG]
#
# Goal:        The chart installs and runs (operator core healthy) and the three version views
#              agree — the running binary is built from HEAD, the chart tgz bundled in the image
#              matches the binary version, and the deployed image tag is the one built. A tgz
#              mismatch is a release/cache bug (the version deviceManagerChartVersion() computes),
#              not cosmetic. See references/version-contract.md.
# Environment: Any reachable cluster with the chart already installed (operator core up). No GPU.
#              Read-only, level-based, safe to re-run.
# Inputs:      None injected — reads live cluster state only (operator-core health delegated to
#              assert-core.sh). Nothing mocked. Optional 2nd arg EXPECTED_TAG (the tag just built)
#              enables the deployed-image-tag assertion.
# Expected:    - assert-core.sh passes (rollout / running revision == HEAD / apiservices / CRDs /
#                the four bundled applications in this release, Kueue's visibility APIs);
#              - the running binary version equals the bundled chart tgz version;
#              - that tgz states an appVersion with no leading "v", and rendering it at its
#                defaults asks for exactly one "v" in the image tag;
#              - when EXPECTED_TAG is passed, the deployed image tag equals it.
# Cleanup:     None — read-only, no trap.
set -uo pipefail

NS="${1:?usage: case-1.sh <NS> [EXPECTED_TAG]}"
EXPECTED_TAG="${2:-}"
WORKER=deploy/gpustack-operator-worker
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# Operator core first (rollout / revision==HEAD / apiservices / CRDs / bundled applications).
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
tgz_path=$(kubectl -n "$NS" exec "$WORKER" -- sh -c 'ls /etc/gpustack/charts/gpustack-operator-*.tgz' 2>/dev/null | head -1)
tgz=$(printf '%s' "$tgz_path" | sed -E 's#.*/gpustack-operator-(.*)\.tgz#\1#')
[ -n "$ver" ] && [ "$ver" = "$tgz" ] && record PASS "binary version == bundled tgz" "$tgz" \
  || record FAIL "binary version == bundled tgz" "binary [$ver] != tgz [${tgz:-none}] — see Dockerfile packaging / build cache"

# The appVersion that same tgz was packaged with. The chart composes its default image tag as
# "v<appVersion>", so an appVersion that kept the binary version's leading "v" asks for
# ":vv<x.y.z>" — a tag no registry serves. Nothing else here would notice: the check above
# compares the tgz FILE NAME, which carries the chart version, not the appVersion.
appver=$(kubectl -n "$NS" exec "$WORKER" -- sh -c "tar -xzOf '${tgz_path}' gpustack-operator/Chart.yaml" 2>/dev/null \
           | awk '/^appVersion:/ {gsub(/["'"'"']/,"",$2); print $2; exit}')
case "$appver" in
  "") record FAIL "bundled chart appVersion" "unreadable from ${tgz_path:-none}" ;;
  v*) record FAIL "bundled chart appVersion" "[$appver] keeps its \"v\" — the default image tag doubles it" ;;
  *)  record PASS "bundled chart appVersion" "$appver" ;;
esac

# And what that appVersion renders to, asked of the PACKAGED chart rather than of the tree: the
# image can carry an older copy of the helper that composes the tag. Every e2e install passes an
# explicit image, so this default is rendered nowhere else. Only the parent's own workloads are
# rendered — the subcharts bring their own images, which this tag never governs.
kubever=$(kubectl version -o json 2>/dev/null \
  | python3 -c 'import json,sys; v=json.load(sys.stdin)["serverVersion"]; print(v["major"]+"."+v["minor"].rstrip("+"))' 2>/dev/null)
tags=$(kubectl -n "$NS" exec "$WORKER" -- sh -c "helm template t '${tgz_path}' -n '${NS}' \
         --kube-version 'v${kubever:-1.33}.0' \
         --set worker.enabled=false --set kueue.enabled=false \
         --set node-feature-discovery.enabled=false \
         --set csi-driver-nfs.enabled=false --set csi-driver-s3.enabled=false" 2>/dev/null \
         | awk '/^[[:space:]]*image:/ {gsub(/"/,"",$2); print $2}' | sort -u)
want="v${appver#v}"
if [ -z "$tags" ]; then
  record FAIL "default image tag" "the packaged chart rendered no image (kube-version v${kubever:-1.33}.0)"
else
  bad=$(printf '%s\n' "$tags" | grep -v ":${want}\$" | tr '\n' ' ')
  [ -z "$bad" ] && record PASS "default image tag" "$(printf '%s' "$tags" | tr '\n' ' ')" \
    || record FAIL "default image tag" "${bad}(expected :${want})"
fi

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
