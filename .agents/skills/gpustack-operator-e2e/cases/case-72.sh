#!/usr/bin/env bash
#
# CASE 72 — A pool reports whether its bindings' ceilings still fit its total: True while they do,
#           False with both figures named once they do not, and nobody is blamed either way
#   (MUTATING, self-recovering)
#
#   case-72.sh <NS>
#
# Goal:        The pool's quota.total is a declaration, and each binding's quota.ceiling is a request
#              against it. This case pins the one verdict that compares the two: a condition on the
#              pool that is True while every ceiling can be granted in full, and False the moment the
#              ceilings together exceed the total. False is the half worth proving on a live cluster,
#              because it is a report and NOT a refusal: admission lets the third binding in, the
#              bindings keep reporting Ready, the pool keeps reporting Ready, and the message names
#              the sum and the total so an operator can see by how much the pool is oversubscribed.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). No GPU and no
#              RDMA; the members are DRAM over TCP. Needs a registry the cluster can pull the
#              Mooncake image from — override with E2E_MOONCAKE_IMAGE.
#
# Inputs:      All real, nothing mocked. A KVCacheBackend with one small DRAM member, a KVCachePool
#              over it with quota.total of 1Gi, and bindings whose ceilings are chosen to sit inside
#              the total one at a time: two at 512Mi each (sum exactly the total), then a third at
#              256Mi (sum 1280Mi, past it). No workload ever runs — the verdict reads declarations,
#              and so does this case.
#
# Expected:    - with two bindings at half the total each, the pool's QuotaWithinTotal condition is
#                True;
#              - after the third binding, the condition is False with reason Oversubscribed, and its
#                message names both figures: the sum (1280Mi) and the total (1Gi);
#              - the pool's phase stays Ready throughout — False names a proportional division the
#                store performs by design, not a fault;
#              - all three bindings were admitted (they exist) and none reports an error of its own
#                (each phase is Ready).
#
# Cleanup:     Trap removes the namespace, pool and backend without waiting. The bindings declare no
#              workload and their domains hold no bytes, so nothing is held by a finalizer; a delete
#              that lands mid-pass converges on the next one.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-72.sh <NS>}"
IMAGE="${E2E_MOONCAKE_IMAGE:-docker.io/kvcacheai/mooncake:0.3.13}"

# The suffix exists because the pool and the backend are cluster-scoped: a bare PID is small and
# reused across reboots, which lets two runs adopt each other's leftovers.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-qwt-${SFX}"
POOL="kvcp-qwt-${SFX}"
NS_Q="kvc-qwt-${SFX}"

# 512Mi + 512Mi = 1Gi exactly, so the True half also pins that a sum equal to the total still fits;
# adding 256Mi takes the sum to 1280Mi, which is the False half. Every ceiling is far below the
# total on its own, so admission refuses none of them — that is the point.
TOTAL="1Gi"
HALF="512Mi"
EXTRA="256Mi"
SUM_PAST="1280Mi"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

restore() {
  echo
  echo "[case-72] cleanup"
  kubectl delete namespace "$NS_Q" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap restore EXIT

wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-120}" ns_args=()
  [ -n "${6:-}" ] && ns_args=(-n "$6")
  local got=""
  for _ in $(seq 1 "$secs"); do
    got="$(kubectl "${ns_args[@]}" get "$kind" "$name" -o jsonpath="$path" 2>/dev/null)"
    [ "$got" = "$want" ] && return 0
    sleep 1
  done
  echo "$got"
  return 1
}

# The condition is read as three separate jsonpaths rather than one, because a single
# `{.status.conditions[?(@.type=="...")]}` projection returns the fields in document order as one
# blob and an assertion on the blob is an assertion on the renderer, not on the verdict.
pool_condition() {
  kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
    -o "jsonpath={.status.conditions[?(@.type==\"QuotaWithinTotal\")].$1}" 2>/dev/null
}

echo "== 1. a pool with a declared total, and two bindings splitting it =="

kubectl create namespace "$NS_Q" >/dev/null 2>&1 || true

kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  connection:
    managed:
      leader:
        multiTenancy: true
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 180 >/dev/null; then
  record FAIL "backend ready" "the master did not reach Ready in 180s; nothing below can run"
  print_rows
  exit 1
fi

# The objects' EXISTENCE is the gate, not the words the apply printed: kubectl reports the pool and
# the bindings in one stream, so a run where the pool was created and a binding refused looks
# exactly like a clean one if only the output is counted. A refused binding is a FAIL below for a
# different reason (the case exists to prove none is refused), so the gate names what is missing.
apply_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL}
spec:
  backends: [${BACKEND}]
  quota: {total: ${TOTAL}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-half-a, namespace: ${NS_Q}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: ${HALF}}
  domain: {name: dom-a-${SFX}, blockSize: 16, dtype: bfloat16}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-half-b, namespace: ${NS_Q}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: ${HALF}}
  domain: {name: dom-b-${SFX}, blockSize: 16, dtype: bfloat16}
YAML
)"
missing=""
kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o name >/dev/null 2>&1 \
  || missing="${missing} kvcachepool/${POOL}"
for b in bind-half-a bind-half-b; do
  kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$b" -o name >/dev/null 2>&1 \
    || missing="${missing} ${NS_Q}/kvcachepoolbinding/${b}"
done
if [ -n "$missing" ]; then
  record FAIL "the pool and its two bindings exist" \
    "absent after the apply:${missing} — so nothing below has a subject. The apply said: \
$(printf '%s' "${apply_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  print_rows
  exit 1
fi

# Two halves of one total: the sum equals the total and still fits, which is the boundary of the
# True side. Polled rather than read once, because the verdict is written by the pool's next pass
# after the bindings land, not by the apply.
if wait_for kvcachepools.worker.gpustack.ai "$POOL" \
  '{.status.conditions[?(@.type=="QuotaWithinTotal")].status}' True 120 >/dev/null; then
  record PASS "two bindings at half the total each leave the verdict True" \
    "QuotaWithinTotal=True with two ${HALF} ceilings against a ${TOTAL} total, reason \
$(pool_condition reason)"
else
  record FAIL "two bindings at half the total each leave the verdict True" \
    "QuotaWithinTotal is '$(pool_condition status)' after 120s; wanted True"
fi

echo "== 2. a third binding takes the sum past the total =="

third_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-past, namespace: ${NS_Q}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: ${EXTRA}}
  domain: {name: dom-c-${SFX}, blockSize: 16, dtype: bfloat16}
YAML
)"
if kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai bind-past -o name >/dev/null 2>&1; then
  record PASS "the third binding is admitted" \
    "a ${EXTRA} ceiling fits the ${TOTAL} total on its own, so admission refuses nothing; the \
oversubscription is the pool's to report"
else
  record FAIL "the third binding is admitted" \
    "the apply left no binding behind, so the False half below has no subject. The apply said: \
$(printf '%s' "${third_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  print_rows
  exit 1
fi

if wait_for kvcachepools.worker.gpustack.ai "$POOL" \
  '{.status.conditions[?(@.type=="QuotaWithinTotal")].status}' False 120 >/dev/null; then
  reason="$(pool_condition reason)"
  message="$(pool_condition message)"
  if [ "$reason" = "Oversubscribed" ] &&
    printf '%s' "$message" | grep -q "$SUM_PAST" &&
    printf '%s' "$message" | grep -q "$TOTAL"; then
    record PASS "the verdict turns False, naming the sum and the total" \
      "reason=${reason}, message names ${SUM_PAST} against ${TOTAL}"
  else
    record FAIL "the verdict turns False, naming the sum and the total" \
      "status is False but reason='${reason:-<absent>}' and message='${message:-<absent>}'; wanted \
reason=Oversubscribed naming ${SUM_PAST} and ${TOTAL}"
  fi
else
  record FAIL "the verdict turns False, naming the sum and the total" \
    "QuotaWithinTotal did not reach False in 120s after the third binding landed"
fi

# The False verdict is a report, and the phase is where that shows: a pool whose tenants are being
# served in proportion is working, and reading as Degraded would tell an operator to fix it.
pool_phase="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
  -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$pool_phase" = "Ready" ]; then
  record PASS "the oversubscribed pool still reads Ready" \
    "phase is Ready while QuotaWithinTotal is False; the store serves every tenant in proportion"
else
  record FAIL "the oversubscribed pool still reads Ready" \
    "phase is '${pool_phase:-<absent>}' while the verdict is False; False is not a fault and the \
phase is where that shows"
fi

# None of the three carries an error of its own: each phase is Ready, which the binding derives
# from its own conditions — an unknown tenant, an unreadable ledger or a refused policy write would
# each have held it away from Ready.
blamed=""
for b in bind-half-a bind-half-b bind-past; do
  phase="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$b" \
    -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase" = "Ready" ] || blamed="${blamed} ${b}=${phase:-<absent>}"
done
if [ -z "$blamed" ]; then
  record PASS "no binding is refused and none reports an error of its own" \
    "all three bindings report Ready; the oversubscription lives on the pool and nowhere else"
else
  record FAIL "no binding is refused and none reports an error of its own" \
    "blamed:${blamed} — a binding reporting other than Ready while the pool is oversubscribed is \
the reading the verdict's spelling exists to avoid"
fi

print_rows

if [ "$FAILS" -gt 0 ]; then
  echo
  echo "[case-72] ${FAILS} FAIL row(s)"
  exit 1
fi
