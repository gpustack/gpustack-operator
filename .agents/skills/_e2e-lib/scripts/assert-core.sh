#!/usr/bin/env bash
#
# Shared READ-ONLY assertions common to both E2E skills: the operator rolls out,
# the RUNNING binary is built from HEAD, the aggregated APIs are Available, the
# CRDs are established, the four bundled applications run as workloads of the
# operator's own release, and the NodeFeatureRule the chain starts from is applied.
#
#   assert-core.sh <NS> [RELEASE]
#
# Prints a STATUS|CHECK|OBJECT table and exits non-zero if any check FAILs.
# Level-based and safe to re-run. Callers (case-1 of each skill) run this first,
# then append their skill-specific assertions.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: assert-core.sh <NS> [RELEASE]}"
RELEASE="${2:-gpustack-operator}"
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

# 4. CRDs established. A query that FAILED is not an answer of "absent": against a remote endpoint
#    the read can die on transport alone, and swallowing stderr turns that into a verdict blaming
#    the operator for the network. Retry the transport, stop at once on a real NotFound, and report
#    whatever actually came back.
for crd in instances.worker.gpustack.ai devices.worker.gpustack.ai; do
  err="" ok=no
  for _ in 1 2 3 4 5; do
    err=$(kubectl get crd "$crd" -o name 2>&1 >/dev/null) && { ok=yes; break; }
    case "$err" in *NotFound*) break ;; esac
    sleep 3
  done
  if [ "$ok" = yes ]; then
    record PASS "crd established" "$crd"
  else
    record FAIL "crd established" "$crd unreadable: ${err:-missing}"
  fi
done

# 5. Kueue, NFD and the two CSI drivers run as workloads of the operator's OWN release.
#    They used to be four Helm releases the worker installed at runtime; as subcharts what
#    proves it is Helm's own ownership annotation, which is exactly the field an ownership
#    transfer rewrites. It is read instead of app.kubernetes.io/instance because Helm stamps it
#    on everything it manages while the label is up to each chart's templates — the
#    csi-driver-s3 chart labels its controller Deployment `managed-by` only, with no `instance`.
#    Kueue and the NFD pair are required — the scheduling chain cannot start without them —
#    while a CSI driver an install switched off is SKIPped. `rollout status` covers Deployments
#    and DaemonSets alike.
for entry in \
  "deploy/kueue-controller-manager|required" \
  "deploy/node-feature-discovery-master|required" \
  "daemonset/node-feature-discovery-worker|required" \
  "deploy/csi-nfs-controller|optional" \
  "daemonset/csi-nfs-node|optional" \
  "deploy/csi-s3-controller|optional" \
  "daemonset/csi-s3-node|optional"; do
  obj="${entry%|*}"
  posture="${entry#*|}"
  if ! kubectl -n "$NS" get "$obj" >/dev/null 2>&1; then
    if [ "$posture" = required ]; then
      record FAIL "application in release" "$obj missing — the scheduling chain needs it"
    else
      record SKIP "application in release" "$obj not installed (switched off)"
    fi
    continue
  fi
  owner=$(kubectl -n "$NS" get "$obj" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null)
  if [ "$owner" != "$RELEASE" ]; then
    record FAIL "application in release" \
      "$obj belongs to [${owner:-none}], not ${RELEASE} — installed as a separate release?"
  elif kubectl -n "$NS" rollout status "$obj" --timeout=180s >/dev/null 2>&1; then
    record PASS "application in release" "$obj"
  else
    record FAIL "application in release" "$obj not rolled out within 180s"
  fi
done

# The four per-application releases must NOT be back. Their reappearance means the worker
# started installing applications at runtime again — the regression this layout removed.
legacy=$(helm list -n "$NS" -q 2>/dev/null \
  | grep -E '^gpustack-(kueue|node-feature-discovery|csi-driver-nfs|csi-driver-s3)$' | tr '\n' ' ')
if [ -z "$legacy" ]; then
  record PASS "no per-application releases" "none"
else
  record FAIL "no per-application releases" "$legacy — the worker is installing at runtime again"
fi

# 6. The gpustack-cpu-info NodeFeatureRule, where the scheduling chain starts. It is asserted
#    outside the release above on purpose: no chart can carry it, because its CRD belongs to NFD
#    and the rule is needed even by an install that deploys no NFD, so the worker applies it
#    itself at startup. Its vendor matcher is the payload — a rule matching no PCI vendor
#    classifies no node.
RULE=gpustack-cpu-info
if ! kubectl get nodefeaturerule "$RULE" >/dev/null 2>&1; then
  record FAIL "chain rule applied" "nodefeaturerule/$RULE missing — the chain classifies no node"
else
  vendors=$(kubectl get nodefeaturerule "$RULE" \
    -o jsonpath='{range .spec.rules[*]}{range .matchFeatures[*]}{.matchExpressions.vendor.value}{end}{end}' 2>/dev/null)
  if [ -n "$vendors" ]; then
    record PASS "chain rule applied" "nodefeaturerule/$RULE matches vendors $vendors"
  else
    record FAIL "chain rule applied" "nodefeaturerule/$RULE matches no PCI vendor"
  fi
fi

# 7. Kueue's visibility APIServices. These are aggregated APIs, so an unavailable one costs
#    every client in the cluster — not just Kueue's own callers — because each discovery round
#    trip waits on it. Assert the condition, then time a full discovery as the evidence.
if kubectl -n "$NS" get deploy/kueue-controller-manager >/dev/null 2>&1; then
  for api in v1beta1.visibility.kueue.x-k8s.io v1beta2.visibility.kueue.x-k8s.io; do
    st=""
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      st=$(kubectl get apiservice "$api" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)
      [ "$st" = "True" ] && break
      sleep 3
    done
    if [ "$st" = "True" ]; then
      record PASS "visibility apiservice Available" "$api"
    else
      record FAIL "visibility apiservice Available" "$api (Available=${st:-missing})"
    fi
  done
  started=$SECONDS
  if kubectl api-resources >/dev/null 2>&1; then
    record PASS "discovery round trip" "$((SECONDS - started))s for kubectl api-resources"
  else
    record FAIL "discovery round trip" \
      "kubectl api-resources errored after $((SECONDS - started))s — an aggregated API is wedged"
  fi
else
  record SKIP "visibility apiservice Available" "kueue not installed (switched off)"
fi

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
