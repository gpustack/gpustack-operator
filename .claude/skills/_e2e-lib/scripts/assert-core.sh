#!/usr/bin/env bash
#
# Shared READ-ONLY assertions common to both E2E skills: the operator rolls out,
# the RUNNING binary is built from HEAD, the aggregated APIs are Available, the
# CRDs are established, and the worker's inlined sub-releases are deployed.
#
#   assert-core.sh <NS>
#
# Prints a STATUS|CHECK|OBJECT table and exits non-zero if any check FAILs.
# Level-based and safe to re-run. Callers (case-1 of each skill) run this first,
# then append their skill-specific assertions.
set -uo pipefail

NS="${1:?usage: assert-core.sh <NS>}"
WORKER=deploy/gpustack-operator-worker

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Operator Deployment becomes Available.
if kubectl -n "$NS" rollout status "$WORKER" --timeout=300s >/dev/null 2>&1; then
  record PASS "worker rollout" "$WORKER Available"
else
  record FAIL "worker rollout" "$WORKER not Available within 300s"
fi

# 2. The RUNNING binary is built from HEAD (not a stale cached image). This asks
#    the binary itself rather than comparing image references, which is what makes
#    it survive a same-tag rebuild: the kubelet matches a cached ":dev" by name,
#    not by digest, so the reference can be right while the bits are old.
want=$(git rev-parse HEAD)
got=$(kubectl -n "$NS" exec "$WORKER" -- gpustack-operator --version 2>/dev/null | grep -oiE '[0-9a-f]{40}')
if [ -n "$got" ] && [ "$want" = "$got" ]; then
  record PASS "binary revision == HEAD" "$got"
else
  # Name the image the kubelet actually resolved. Without the digest a stale-image
  # failure cannot be told apart from "the right tag, served from cache".
  ref=$(kubectl -n "$NS" get pod "$WORKER" -o jsonpath='{.status.containerStatuses[0].image}' 2>/dev/null)
  iid=$(kubectl -n "$NS" get pod "$WORKER" -o jsonpath='{.status.containerStatuses[0].imageID}' 2>/dev/null)
  record FAIL "binary revision == HEAD" \
    "running [${got:-none}] != HEAD [$want] — STALE IMAGE. Running ${ref:-<unknown>} (imageID ${iid:-<unknown>}); rebuild and redeploy with a fresh TAG, or pin the digest if the tag was reused"
fi

# 3. Aggregated extension APIs registered and Available. Poll — the aggregated
#    apiserver registers a few seconds after the worker is Ready.
for api in v1.gpustack.ai v1.worker.gpustack.ai; do
  st=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    st=$(kubectl get apiservice "$api" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)
    [ "$st" = "True" ] && break
    sleep 3
  done
  if [ "$st" = "True" ]; then
    record PASS "apiservice Available" "$api"
  else
    record FAIL "apiservice Available" "$api (Available=${st:-missing})"
  fi
done

# 4. CRDs established.
for crd in instances.worker.gpustack.ai devices.worker.gpustack.ai; do
  if kubectl get crd "$crd" >/dev/null 2>&1; then
    record PASS "crd established" "$crd"
  else
    record FAIL "crd established" "$crd missing"
  fi
done

# 5. The worker self-installs the bundled charts as separate Helm releases. Poll
#    a few seconds — they install shortly after the worker is Ready. The
#    device-manager release exists only with deviceManager.enabled=false, so it
#    is not asserted here (see chart-e2e for that path).
for rel in gpustack-kueue gpustack-node-feature-discovery gpustack-csi-driver-nfs gpustack-csi-driver-s3; do
  ok=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if [ "$(helm status "$rel" -n "$NS" -o json 2>/dev/null | grep -o '"status":"deployed"')" ]; then
      ok=1
      break
    fi
    sleep 3
  done
  if [ -n "$ok" ]; then
    record PASS "sub-release deployed" "$rel"
  else
    record FAIL "sub-release deployed" "$rel not deployed (helm status)"
  fi
done

echo
echo "== assert-core: CPU-only operator core =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} core check(s). Diagnose:"
  echo "  kubectl -n ${NS} logs ${WORKER} --tail=200"
  echo "  kubectl -n ${NS} describe ${WORKER}"
  exit 1
fi
echo "all core checks PASS"
