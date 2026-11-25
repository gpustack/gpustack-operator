#!/usr/bin/env bash
#
# CASE 16 — KVCachePool & KVCachePoolBinding multi-tenant proportional quota, eviction enforcement,
#           and tenant ledger gate   (MUTATING, self-recovering)
#
#   case-16.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. KVCacheBackend and KVCachePool
# are cluster-scoped; the Bindings live in namespaces this case creates and removes.
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_MOONCAKE_IMAGE
# Goal:        Prove the multi-tenant KV cache pool and binding contracts end to end across two
#              phases:
#                Phase 1 — Proportional quota & preconditions:
#                  (1) A multi-tenancy master reaches Ready with no pool bound (init container
#                      seed-tenant-quota-policy Completed).
#                  (2) Two namespaces share one pool; oversubscribed ceilings (5:3 summing to 8/6
#                      of master allocatable capacity) set QuotaWithinTotal=False/Oversubscribed
#                      while phase stays Ready, and each Binding's effectiveQuota falls in
#                      proportion (5/8 and 3/8).
#                  (3) Backend usedBy carries the empty-namespace pool claim idempotently; duplicate
#                      reuse domain is refused on the same master and admitted on another master;
#                      a held Binding refuses deletion (HeldByWorkloads) and releases once cleared.
#                  (4) Multi-tenancy toggled off under a bound pool is refused at admission naming
#                      the pool; an unwritable policy source raises QuotaPolicyNotWritable and
#                      recovers to a regular file; a backend with zero mounted members raises
#                      CapacityAllocatable=False.
#                Phase 2 — Grant enforcement & tenant ledger gate:
#                  (5) Under a live read lease, writes past effectiveQuota are refused with -1700
#                      (TENANT_QUOTA_EXCEEDED) and mooncake_tenant_quota_reject_total=1 while all
#                      leased objects remain intact and status.usage is capped at the grant; once
#                      leases lapse, further writes are admitted by evicting older objects of the
#                      same domain; removing all objects reclaims status.usage back to 0.
#                  (6) An unregistered tenant's put is refused -1701 (TENANT_NOT_REGISTERED) while
#                      client setup returns 0 and get misses cleanly; creating a Binding registers
#                      the domain, after which the identical put succeeds and reads back.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). No GPU, no RDMA
#              and no etcd: members are DRAM over TCP. NEEDS a store image built with the Kubernetes
#              Lease leadership backend (default gpustack/mirrored-mooncake:0.3.13.post1-cpu;
#              override with E2E_MOONCAKE_IMAGE).
#
# Inputs:      All real, nothing mocked (except Phase 1 section 6's usedBy status fixture write,
#              which stands in for a ModelDeployment holder). Phase 1 uses a single-leader
#              multi-tenant DRAM backend and two tenant namespaces; Phase 2 uses a 3-replica
#              Kubernetes-elected multi-tenant DRAM backend with -default_kv_lease_ttl=10s, one
#              pool, two Bindings (16Mi and 64Mi), and one probe Pod running the bundled client.
#
# Expected:    - Phase 1: master Ready with no pool bound and init container Completed; both Bindings
#                Ready and sorted in pool usedBy; QuotaWithinTotal=False/Oversubscribed with phase
#                Ready; effectiveQuota split 5:3 within 1Mi tolerance; backend usedBy empty-namespace
#                claim idempotent; duplicate domain refused on same master and admitted on another;
#                held Binding deletion refused with HeldByWorkloads and released after clear;
#                multi-tenancy off refused at admission leaving pool untouched; unwritable policy
#                raises QuotaPolicyNotWritable and recovers; empty backend raises
#                CapacityAllocatable=False.
#              - Phase 2: HA backend Ready with member Role and Lease holder; probe Pod runs;
#                16Mi grant enforced with -1700 and metric=1 under lease while objects stay intact
#                and usage=16Mi; lapsed leases allow eviction-backed writes; drain reclaims usage
#                to 0; unregistered tenant refused -1701; registered Binding admits put/get and
#                drains cleanly.
#
# Cleanup:     Trap removes the policy directory override first, drains both Phase 2 domains, clears
#              any wedged Binding finalizer, and deletes all Pods, Bindings, Pools, Backends,
#              RoleBindings, Leases, temp files, and created namespaces. Idempotent, runs on pass
#              AND fail.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-16.sh <NS>}"
export CASE_ID=16
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"
KV_LEASE_TTL=10s

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"

# Phase 1 names (proportional quota & preconditions).
BACKEND="kvcb-e2e-${SFX}"
EMPTY_BACKEND="kvcb-empty-${SFX}"
POOL="kvcp-e2e-${SFX}"
EMPTY_POOL="kvcp-empty-${SFX}"
NS_A="e2e-c16-a-${SFX}"
NS_B="e2e-c16-b-${SFX}"
DOM_A="dom-a-${SFX}"
DOM_B="dom-b-${SFX}"

# Phase 2 names (grant enforcement & ledger gate).
BACKEND_Q="kvcb-q-${SFX}"
POOL_Q="kvcp-q-${SFX}"
NS_Q="e2e-c16-q-${SFX}"
BIND_Q="bind-q"
BIND_MT="bind-mt-${SFX}"
DOMAIN_Q="dom-q-${SFX}"
DOMAIN_MT="dom-mt-${SFX}"
KEY="mt-${SFX}"
PROBE="kvc-probe-${SFX}"
ROLEBINDING="kvcb-q-${SFX}"
MASTER_URI="k8s://${NS}/${BACKEND_Q}-leader"
METRICS_URL="http://${BACKEND_Q}-leader.${NS}.svc:9003/metrics"

CEILING="16Mi"
CEILING_BYTES=$((16 * 1024 * 1024))
OBJ_BYTES=$((4 * 1024 * 1024))
OBJ_COUNT=4
LEASE_LAPSE=15
LEDGER_CEILING="64Mi"

Q_KEYS=()
for i in $(seq 0 $((OBJ_COUNT - 1))); do Q_KEYS+=("${DOMAIN_Q}-obj-${i}"); done
Q_KEYS+=("${DOMAIN_Q}-over-0")
for i in 0 1 2; do Q_KEYS+=("${DOMAIN_Q}-late-${i}"); done

CLIENT_SCRIPT="$(mktemp)"

leader_pod() {
  kubectl -n "$NS" get pod \
    -l "app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1
}

count_lines() {
  local n
  n="$(grep -c "$1" "$2" 2>/dev/null)" || n=0
  echo "$n"
}

cleanup_phase1() {
  local pod ns
  pod="$(leader_pod)"
  if [ -n "$pod" ]; then
    kubectl -n "$NS" exec "$pod" -c leader -- sh -c \
      'rmdir /var/lib/mooncake/tenant-quota-policy.yaml 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  for ns in "$NS_A" "$NS_B"; do
    kubectl -n "$ns" get kvcachepoolbindings.worker.gpustack.ai -o name 2>/dev/null \
      | while read -r b; do
      kubectl -n "$ns" patch kvcachepoolbindings.v1alpha1.worker.gpustack.ai "${b##*/}" --subresource=status --type=merge \
        -p '{"status":{"usedBy":null}}' >/dev/null 2>&1 || true
    done
  done
  kubectl delete namespace "$NS_A" "$NS_B" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" "$EMPTY_POOL" "kvcp-other-${SFX}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" "$EMPTY_BACKEND" "kvcb-other-${SFX}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

restore() {
  echo
  echo "[case-16] cleanup"
  cleanup_phase1

  if kubectl -n "$NS_Q" get pod "$PROBE" >/dev/null 2>&1; then
    kubectl -n "$NS_Q" exec "$PROBE" -- env MASTER_URI="$MASTER_URI" TENANT_ID="$DOMAIN_MT" \
      python3 /tmp/client.py remove "$KEY" >/dev/null 2>&1 || true
    kubectl -n "$NS_Q" exec "$PROBE" -- env MASTER_URI="$MASTER_URI" TENANT_ID="$DOMAIN_Q" \
      python3 /tmp/client.py drain "${Q_KEYS[@]}" >/dev/null 2>&1 || true
  fi
  kubectl -n "$NS_Q" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -f "$CLIENT_SCRIPT" "/tmp/kvc-probe-${SFX}.log" "/tmp/kvc-probe-rm-${SFX}.log" 2>/dev/null || true
  kubectl -n "$NS_Q" delete kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" "$BIND_MT" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL_Q" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND_Q" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete leases.coordination.k8s.io "${BACKEND_Q}-leader" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete rolebindings.rbac.authorization.k8s.io "$ROLEBINDING" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete namespace "$NS_Q" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  kvi_release_binding_finalizer "$NS_Q" "$BIND_Q"
  kvi_release_binding_finalizer "$NS_Q" "$BIND_MT"
}
trap restore EXIT

# =============================================================================================
# Phase 1: Proportional quota, domain exclusivity, and backend preconditions
# =============================================================================================
echo "== Phase 1.1: a multi-tenancy master reaches Ready with no pool bound to it =="

PHASE1_OK=1
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
        electionBackend: None
        multiTenancy: true
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if last="$(wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 180)"; then
  record PASS "master ready with no pool bound" \
    "the seed fell back to an empty policy document, the path a first install always takes"
else
  record FAIL "master ready with no pool bound" "phase stuck at '${last:-<none>}' after 180s"
fi

POD="$(leader_pod)"
if [ -z "$POD" ]; then
  record FAIL "leader pod found" "no pod carries the backend's label; Phase 1 cannot continue"
  PHASE1_OK=0
fi

if [ "$PHASE1_OK" = 1 ]; then
  init_state="$(kubectl -n "$NS" get pod "$POD" \
    -o jsonpath='{.status.initContainerStatuses[?(@.name=="seed-tenant-quota-policy")].state.terminated.reason}' 2>/dev/null)"
  if [ "$init_state" = "Completed" ]; then
    record PASS "the init container ran in the real image" "seed-tenant-quota-policy: Completed"
  else
    record FAIL "the init container ran in the real image" \
      "terminated reason is '${init_state:-<none>}', not Completed"
  fi

  echo "== Phase 1.2: two namespaces, one pool =="
  if ! member_pods="$(kubectl -n "$NS" get pod \
    -l "app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=member-0" \
    --field-selector=status.phase=Running \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>&1)"; then
    record FAIL "member pods listed for proportional sizing" "$(echo "$member_pods" | head -3 | tr '\n' ' ')"
    PHASE1_OK=0
  fi
fi

read_allocatable_bytes() {
  kubectl -n "$NS" exec -i "$1" -c leader -- sh -s <<'PROBE' 2>&1
URL=http://127.0.0.1:9003/metrics
KEY=mooncake_tenant_quota_allocatable_capacity_bytes
if command -v python3 >/dev/null 2>&1; then PY=python3
elif command -v python >/dev/null 2>&1; then PY=python
else PY=
fi
if [ -n "$PY" ]; then
  "$PY" - "$URL" "$KEY" <<'PY'
import sys, urllib.request
url, key = sys.argv[1], sys.argv[2]
body = urllib.request.urlopen(url, timeout=10).read().decode()
for line in body.splitlines():
    if line.startswith(key):
        print(line.split()[-1])
        sys.exit(0)
sys.exit(3)
PY
elif command -v curl >/dev/null 2>&1; then
  curl -sS --max-time 10 "$URL" \
    | awk -v k="$KEY" 'index($0,k)==1 {print $NF; f=1; exit} END{exit f?0:3}'
elif command -v wget >/dev/null 2>&1; then
  wget -qO- --timeout=10 "$URL" \
    | awk -v k="$KEY" 'index($0,k)==1 {print $NF; f=1; exit} END{exit f?0:3}'
else
  echo "the leader image carries no python and no http client" >&2
  exit 4
fi
PROBE
}

if [ "$PHASE1_OK" = 1 ]; then
  MEMBERS="$(printf '%s' "$member_pods" | grep -c . || true)"
  if [ "$MEMBERS" -lt 1 ]; then
    record FAIL "member pods running for proportional sizing" "no Running member pod found"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  ALLOC_BYTES=""
  for _ in $(seq 1 30); do
    probe_out="$(read_allocatable_bytes "$POD")"
    candidate="$(printf '%s' "$probe_out" | tr -d '\r' | tail -1)"
    case "$candidate" in
      ''|*[!0-9]*) ;;
      *) [ "$candidate" -gt 0 ] && { ALLOC_BYTES="$candidate"; break; } ;;
    esac
    sleep 4
  done
  if [ -z "$ALLOC_BYTES" ]; then
    record FAIL "master reports allocatable capacity" \
      "no allocatable capacity within 120s; last output: $(printf '%s' "${probe_out:-<none>}" | tr '\n' ' ' | cut -c1-160)"
    PHASE1_OK=0
  else
    ALLOC_MI=$(( ALLOC_BYTES / 1048576 ))
    UNIT_MI=$(( ALLOC_MI / 6 ))
    CEIL_A_MI=$(( UNIT_MI * 5 ))
    CEIL_B_MI=$(( UNIT_MI * 3 ))
    if [ "$UNIT_MI" -lt 1 ]; then
      record FAIL "master reports allocatable capacity" "${ALLOC_BYTES} bytes too little to split 5:3"
      PHASE1_OK=0
    else
      echo "[case-16] ${MEMBERS} member(s); master reports ${ALLOC_MI}Mi allocatable; ceilings ${CEIL_A_MI}Mi + ${CEIL_B_MI}Mi"
    fi
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  kubectl create namespace "$NS_A" >/dev/null 2>&1 || true
  kubectl create namespace "$NS_B" >/dev/null 2>&1 || true

  pool_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL}
spec:
  backends: [${BACKEND}]
  quota:
    total: ${ALLOC_MI}Mi
YAML
)"
  if ! kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o name >/dev/null 2>&1; then
    record FAIL "pool created" "$(printf '%s' "${pool_out:-<no output>}" | tr '\n' ' ' | cut -c1-180)"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  bind_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-a, namespace: ${NS_A}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: ${CEIL_A_MI}Mi}
  domain: {name: ${DOM_A}, blockSize: 16, dtype: bfloat16}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-b, namespace: ${NS_B}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: ${CEIL_B_MI}Mi}
  domain: {name: ${DOM_B}, blockSize: 16, dtype: bfloat16}
YAML
)"
  bind_missing=""
  for pair in "${NS_A} bind-a" "${NS_B} bind-b"; do
    bind_ns="${pair%% *}"
    bind_name="${pair##* }"
    kubectl -n "$bind_ns" get kvcachepoolbindings.worker.gpustack.ai "$bind_name" -o name >/dev/null 2>&1 \
      || bind_missing="${bind_missing} ${bind_ns}/${bind_name}"
  done
  if [ -n "$bind_missing" ]; then
    record FAIL "both bindings created" "absent:${bind_missing}; $(printf '%s' "${bind_out:-<no output>}" | tr '\n' ' ' | cut -c1-180)"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  ok=true
  for pair in "${NS_A} bind-a" "${NS_B} bind-b"; do
    bind_ns="${pair%% *}"
    bind_name="${pair##* }"
    wait_for kvcachepoolbindings.worker.gpustack.ai "$bind_name" '{.status.phase}' Ready 180 "$bind_ns" >/dev/null || ok=false
  done
  if $ok; then
    record PASS "both bindings ready" "two namespaces converge against one pool"
  else
    record FAIL "both bindings ready" "at least one binding did not reach Ready in 180s"
  fi

  used_by="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
    -o jsonpath='{range .status.usedBy[*]}{.namespace}/{.name} {end}' 2>/dev/null)"
  if [ "$used_by" = "${NS_A}/bind-a ${NS_B}/bind-b " ]; then
    record PASS "the pool lists both bindings" "sorted: ${used_by% }"
  else
    record FAIL "the pool lists both bindings" "usedBy is '${used_by:-<empty>}'"
  fi

  if wait_for kvcachepools.worker.gpustack.ai "$POOL" \
    '{.status.conditions[?(@.type=="QuotaWithinTotal")].status}' False 120 >/dev/null; then
    qwt_reason="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
      -o jsonpath='{.status.conditions[?(@.type=="QuotaWithinTotal")].reason}' 2>/dev/null)"
    if [ "$qwt_reason" = "Oversubscribed" ]; then
      record PASS "the oversubscribed pool reports QuotaWithinTotal False" \
        "reason=Oversubscribed with ceilings ${CEIL_A_MI}Mi + ${CEIL_B_MI}Mi over a ${ALLOC_MI}Mi total"
    else
      record FAIL "the oversubscribed pool reports QuotaWithinTotal False" \
        "status is False but reason is '${qwt_reason:-<absent>}'; wanted Oversubscribed"
    fi
  else
    record FAIL "the oversubscribed pool reports QuotaWithinTotal False" \
      "QuotaWithinTotal did not reach False in 120s"
  fi

  pool_phase="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)"
  if [ "$pool_phase" = "Ready" ]; then
    record PASS "the oversubscribed pool still reads Ready" \
      "phase is Ready: oversubscription is served in proportion, and is not a fault"
  else
    record FAIL "the oversubscribed pool still reads Ready" \
      "phase is '${pool_phase:-<absent>}' while the pool is oversubscribed"
  fi

  echo "== Phase 1.3: the ceiling is a request; the grant falls in proportion =="
  req_a="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a -o jsonpath='{.status.requestedQuota}' 2>/dev/null)"
  eff_a="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"
  eff_b="$(kubectl -n "$NS_B" get kvcachepoolbindings.worker.gpustack.ai bind-b -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"

  if [ -n "$req_a" ] && [ -n "$eff_a" ] && [ -n "$eff_b" ]; then
    record PASS "both figures are published" "requested=${req_a} effective=${eff_a}"
  else
    record FAIL "both figures are published" \
      "requested='${req_a:-<absent>}' effectiveA='${eff_a:-<absent>}' effectiveB='${eff_b:-<absent>}'"
  fi

  mib() {
    case "$1" in
      *Gi) echo $(( ${1%Gi} * 1024 )) ;;
      *Mi) echo "${1%Mi}" ;;
      *Ki) echo $(( ${1%Ki} / 1024 )) ;;
      ''|*[!0-9]*) echo "" ;;
      *) echo $(( $1 / 1048576 )) ;;
    esac
  }

  alloc=$ALLOC_MI
  a_mib="$(mib "${eff_a:-}")"
  b_mib="$(mib "${eff_b:-}")"
  want_a=$(( 5 * alloc / 8 ))
  want_b=$(( 3 * alloc / 8 ))
  if [ -z "$a_mib" ] || [ -z "$b_mib" ]; then
    record FAIL "each grant is its share of what the master has" \
      "a grant is absent or not a quantity: A='${eff_a:-<absent>}' B='${eff_b:-<absent>}'"
  elif [ $(( CEIL_A_MI + CEIL_B_MI )) -le "$alloc" ]; then
    record FAIL "each grant is its share of what the master has" \
      "ceilings ${CEIL_A_MI}+${CEIL_B_MI}Mi do not oversubscribe ${alloc}Mi"
  elif [ "$a_mib" -ge "$CEIL_A_MI" ] || [ "$b_mib" -ge "$CEIL_B_MI" ]; then
    record FAIL "each grant is its share of what the master has" \
      "a grant was not reduced below its ceiling: ${a_mib}/${CEIL_A_MI}Mi and ${b_mib}/${CEIL_B_MI}Mi"
  elif [ $(( a_mib - want_a )) -ge -1 ] && [ $(( a_mib - want_a )) -le 1 ] &&
       [ $(( b_mib - want_b )) -ge -1 ] && [ $(( b_mib - want_b )) -le 1 ] &&
       [ $(( a_mib + b_mib - alloc )) -ge -1 ] && [ $(( a_mib + b_mib - alloc )) -le 1 ]; then
    record PASS "each grant is its share of what the master has" \
      "master reports ${alloc}Mi allocatable over ${MEMBERS} member(s); ${CEIL_A_MI}Mi->${a_mib}Mi and ${CEIL_B_MI}Mi->${b_mib}Mi, summing to ${alloc}Mi"
  else
    record FAIL "each grant is its share of what the master has" \
      "master reports ${alloc}Mi allocatable over ${MEMBERS} member(s); got ${a_mib}Mi and ${b_mib}Mi (sum $(( a_mib + b_mib ))Mi), wanted ${want_a}Mi and ${want_b}Mi"
  fi

  echo "== Phase 1.4: backend usedBy carries an empty-namespace claim, exactly once =="
  claim="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
    -o jsonpath='{range .status.usedBy[*]}{.kind}|{.namespace}|{.name} {end}' 2>/dev/null)"
  if [ "$claim" = "KVCachePool||${POOL} " ]; then
    record PASS "the pool claims its backend" \
      "kind=KVCachePool namespace=<empty> name=${POOL}"
  else
    record FAIL "the pool claims its backend" "usedBy is '${claim:-<empty>}'"
  fi

  sleep 35
  claim_again="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
    -o jsonpath='{range .status.usedBy[*]}{.kind}|{.namespace}|{.name} {end}' 2>/dev/null)"
  if [ "$claim_again" = "$claim" ]; then
    record PASS "a second pass writes no second claim" "still exactly one entry after a resync"
  else
    record FAIL "a second pass writes no second claim" "was '${claim}', now '${claim_again}'"
  fi

  echo "== Phase 1.5: a reuse domain belongs to one binding per master =="
  dup_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-dup, namespace: ${NS_B}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: 1Gi}
  domain: {name: ${DOM_A}, blockSize: 16, dtype: bfloat16}
YAML
)"
  if echo "$dup_out" | grep -q "already registered by ${NS_A}/bind-a"; then
    record PASS "a duplicate reuse domain on the same master is refused, naming the holder" \
      "admission names ${NS_A}/bind-a"
  else
    record FAIL "a duplicate reuse domain on the same master is refused, naming the holder" \
      "apply said: $(echo "$dup_out" | tr '\n' ' ' | cut -c1-160)"
  fi

  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: kvcb-other-${SFX}
spec:
  type: Mooncake
  image: ${IMAGE}
  connection:
    managed:
      leader:
        electionBackend: None
        multiTenancy: true
      members:
        - nodeSelector: {gpustack.ai/kvc-e2e-absent: "true"}
          medium: DRAM
          capacityPerMember: 1Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: kvcp-other-${SFX}
spec:
  backends: [kvcb-other-${SFX}]
  quota: {total: 1Gi}
YAML
  other_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-other, namespace: ${NS_B}}
spec:
  poolRef: {name: kvcp-other-${SFX}}
  quota: {ceiling: 1Gi}
  domain: {name: ${DOM_A}, blockSize: 16, dtype: bfloat16}
YAML
)"
  if echo "$other_out" | grep -qE 'created|configured'; then
    record PASS "the same domain on a pool another master serves is admitted" \
      "bind-other took ${DOM_A} against kvcb-other-${SFX} while ${NS_A}/bind-a holds it here"
  else
    record FAIL "the same domain on a pool another master serves is admitted" \
      "apply said: $(echo "$other_out" | tr '\n' ' ' | cut -c1-160)"
  fi
  kubectl -n "$NS_B" delete kvcachepoolbindings.worker.gpustack.ai bind-other \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "kvcp-other-${SFX}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "kvcb-other-${SFX}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true

  echo "== Phase 1.6: a binding a workload holds cannot be deleted =="
  if ! kubectl -n "$NS_A" patch kvcachepoolbindings.v1alpha1.worker.gpustack.ai bind-a --subresource=status --type=merge \
    -p '{"status":{"usedBy":[{"kind":"ModelDeployment","namespace":"","name":"qwen-e2e"}]}}' >/dev/null; then
    record FAIL "the holder fixture is written" "the storage-version status patch failed"
  else
    holder_kind="$(kubectl -n "$NS_A" get kvcachepoolbindings.v1alpha1.worker.gpustack.ai bind-a \
      -o 'jsonpath={.status.usedBy[?(@.name=="qwen-e2e")].kind}')"
    if [ "$holder_kind" != ModelDeployment ]; then
      record FAIL "the holder fixture is read back" "usedBy does not contain ModelDeployment/qwen-e2e"
    else
      kubectl -n "$NS_A" delete kvcachepoolbindings.worker.gpustack.ai bind-a --wait=false >/dev/null 2>&1 || true
      sleep 20

      held_reason="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a \
        -o jsonpath='{.status.conditions[?(@.type=="Releasable")].reason}' 2>/dev/null)"
      held_msg="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a \
        -o jsonpath='{.status.conditions[?(@.type=="Releasable")].message}' 2>/dev/null)"
      if [ "$held_reason" = "HeldByWorkloads" ] && echo "$held_msg" | grep -q "ModelDeployment/qwen-e2e"; then
        record PASS "the deletion is held and names the holder" \
          "Releasable=False HeldByWorkloads, message names ModelDeployment/qwen-e2e"
      else
        record FAIL "the deletion is held and names the holder" \
          "reason='${held_reason:-<none>}' message='$(echo "$held_msg" | cut -c1-90)'"
      fi

      if ! kubectl -n "$NS_A" patch kvcachepoolbindings.v1alpha1.worker.gpustack.ai bind-a --subresource=status --type=merge \
        -p '{"status":{"usedBy":null}}' >/dev/null; then
        record FAIL "the holder fixture is released" "the storage-version status patch failed"
      else
        released=false
        last_out=""
        for _ in $(seq 1 90); do
          if last_out="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a -o name 2>&1)"; then
            sleep 1
            continue
          fi
          case "$last_out" in
            *NotFound*|*"not found"*) released=true; break ;;
            *) sleep 1 ;;
          esac
        done
        if [ "$released" = true ]; then
          record PASS "the binding is released once nothing holds it" \
            "the API server reports it NotFound after the hold clears"
        else
          record FAIL "the binding is released once nothing holds it" \
            "not confirmed gone 90s after usedBy emptied; last read: $(echo "$last_out" | head -1 | cut -c1-70)"
        fi
      fi
    fi
  fi

  echo "== Phase 1.7: a master without a tenant ledger fails loudly =="
  mt_patch_out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type=merge \
    -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":false}}}}}' 2>&1)"
  if echo "$mt_patch_out" | grep -q "multi-tenancy cannot be turned off while KVCachePool/${POOL}"; then
    record PASS "multi-tenancy off under a bound pool is refused at admission, naming the pool" \
      "the refusal names KVCachePool/${POOL}"
  else
    record FAIL "multi-tenancy off under a bound pool is refused at admission, naming the pool" \
      "patch said: $(echo "$mt_patch_out" | tr '\n' ' ' | cut -c1-160)"
  fi

  pool_phase="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)"
  mt_reason="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaLedgerAvailable")].reason}' 2>/dev/null)"
  if [ "$pool_phase" = "Ready" ] && [ "$mt_reason" = "Available" ]; then
    record PASS "the refused toggle leaves the pool untouched" \
      "phase=Ready, QuotaLedgerAvailable=Available"
  else
    record FAIL "the refused toggle leaves the pool untouched" \
      "reason='${mt_reason:-<none>}' phase='${pool_phase:-<none>}'"
  fi

  if ! patch_out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type=merge \
    -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":true}}}}}' 2>&1)"; then
    record FAIL "multi-tenancy is restored for the sections that follow" \
      "re-enabling it failed: $(printf '%s' "$patch_out" | head -1 | cut -c1-80)"
  elif last="$(wait_for kvcachepools.worker.gpustack.ai "$POOL" '{.status.phase}' Ready 180)"; then
    record PASS "multi-tenancy is restored for the sections that follow" \
      "the pool is Ready again"
  else
    record FAIL "multi-tenancy is restored for the sections that follow" \
      "the pool is stuck at '${last:-<none>}' after 180s"
  fi

  echo "== Phase 1.8: a policy source the master cannot write fails loudly =="
  POD="$(leader_pod)"
  kubectl -n "$NS" exec "$POD" -c leader -- sh -c \
    'rm -f /var/lib/mooncake/tenant-quota-policy.yaml && mkdir /var/lib/mooncake/tenant-quota-policy.yaml' \
    >/dev/null 2>&1

  kubectl -n "$NS_B" patch kvcachepoolbindings.worker.gpustack.ai bind-b --type=merge \
    -p '{"spec":{"quota":{"ceiling":"2Gi"}}}' >/dev/null 2>&1

  ro_reason=""
  for _ in $(seq 1 40); do
    ro_reason="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
      -o jsonpath='{.status.conditions[?(@.type=="QuotaPolicyWritable")].reason}' 2>/dev/null)"
    [ "$ro_reason" = "QuotaPolicyNotWritable" ] && break
    sleep 3
  done
  pool_phase="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)"
  if [ "$ro_reason" = "QuotaPolicyNotWritable" ] && [ "$pool_phase" != "Ready" ]; then
    record PASS "an unwritable policy source raises its own reason" \
      "QuotaPolicyWritable=False QuotaPolicyNotWritable, phase=${pool_phase}"
  else
    record FAIL "an unwritable policy source raises its own reason" \
      "reason='${ro_reason:-<none>}' phase='${pool_phase:-<none>}'"
  fi

  kubectl -n "$NS" exec "$POD" -c leader -- sh -c \
    'rmdir /var/lib/mooncake/tenant-quota-policy.yaml 2>/dev/null || true' >/dev/null 2>&1 || true

  policy_back=false
  for _ in $(seq 1 30); do
    if kubectl -n "$NS" exec "$POD" -c leader -- \
        sh -c 'test -f /var/lib/mooncake/tenant-quota-policy.yaml' >/dev/null 2>&1; then
      policy_back=true
      break
    fi
    sleep 3
  done
  if $policy_back; then
    record PASS "the policy path is a readable file again" \
      "the master rewrote it once writes succeeded; a leader restart is safe again"
  else
    record FAIL "the policy path is a readable file again" \
      "no regular file at the policy path after 90s"
  fi
fi

echo "== Phase 1.9: a backend with nothing mounted is not a pool with an empty cache =="
empty_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${EMPTY_BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  connection:
    managed:
      leader:
        electionBackend: None
        multiTenancy: true
      members:
        - nodeSelector: {gpustack.ai/kvc-e2e-absent: "true"}
          medium: DRAM
          capacityPerMember: 1Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${EMPTY_POOL}
spec:
  backends: [${EMPTY_BACKEND}]
  quota:
    total: 1Gi
YAML
)"
empty_missing=""
kubectl get kvcachebackends.worker.gpustack.ai "$EMPTY_BACKEND" -o name >/dev/null 2>&1 \
  || empty_missing="${empty_missing} kvcachebackend/${EMPTY_BACKEND}"
kubectl get kvcachepools.worker.gpustack.ai "$EMPTY_POOL" -o name >/dev/null 2>&1 \
  || empty_missing="${empty_missing} kvcachepool/${EMPTY_POOL}"
if [ -n "$empty_missing" ]; then
  record FAIL "zero mounted members is a condition, not a silence" \
    "absent after apply:${empty_missing}; $(printf '%s' "${empty_out:-<no output>}" | tr '\n' ' ' | cut -c1-160)"
else
  cap_reason=""
  for _ in $(seq 1 60); do
    cap_reason="$(kubectl get kvcachepools.worker.gpustack.ai "$EMPTY_POOL" \
      -o jsonpath='{.status.conditions[?(@.type=="CapacityAllocatable")].status}' 2>/dev/null)"
    [ "$cap_reason" = "False" ] && break
    sleep 3
  done
  empty_phase="$(kubectl get kvcachepools.worker.gpustack.ai "$EMPTY_POOL" -o jsonpath='{.status.phase}' 2>/dev/null)"
  if [ "$cap_reason" = "False" ] && [ "$empty_phase" != "Ready" ]; then
    record PASS "zero mounted members is a condition, not a silence" \
      "CapacityAllocatable=False, phase=${empty_phase}"
  else
    record FAIL "zero mounted members is a condition, not a silence" \
      "CapacityAllocatable='${cap_reason:-<none>}' phase='${empty_phase:-<none>}'"
  fi
fi

# Release Phase 1 backends before Phase 2 starts so member DRAM does not stack.
cleanup_phase1

# =============================================================================================
# Phase 2: Grant enforcement (lease refusal + lapsed eviction) and tenant ledger gate
# =============================================================================================
CLIENT_TAGS='^(SETUP|IMPORT-FAIL|SETUP-TYPEERROR|SETUP-FAIL|FILL|LEASED|OVER|INTACT|REJECTMETRIC|LAPSE|LATE|SURVIVED|PENDING|REMOVE|LEFT|PUTVAL|PUT|GET)'

cat >"$CLIENT_SCRIPT" <<'PY'
import os, sys, time, uuid, socket, urllib.request

action = sys.argv[1]
args = sys.argv[2:]

try:
    from mooncake.store import MooncakeDistributedStore
except Exception as e:
    print("IMPORT-FAIL %s" % e); sys.exit(0)

store = MooncakeDistributedStore()

ip = os.environ.get("POD_IP") or socket.gethostbyname(socket.gethostname())
try:
    rc = store.setup(ip, "P2PHANDSHAKE", 0, 134217728, "tcp", "",
                     os.environ["MASTER_URI"], tenant_id=os.environ["TENANT_ID"])
except TypeError as e:
    print("SETUP-TYPEERROR %s" % e); sys.exit(0)
except Exception as e:
    print("SETUP-FAIL %s" % e); sys.exit(0)
print("SETUP rc=%d" % rc, flush=True)
if rc != 0:
    sys.exit(0)

if action == "put":
    key = args[0]
    val = ("%s-%s" % (key, uuid.uuid4())).encode()
    print("PUTVAL %s" % val.decode(), flush=True)
    print("PUT rc=%d" % store.put(key, val), flush=True)
    got = store.get(key)
    print("GET match=%s len=%d" % (got == val, len(got) if got else -1), flush=True)
elif action == "remove":
    key = args[0]
    deadline, attempts, rc = time.time() + 60, 0, -1
    while time.time() < deadline:
        attempts += 1
        rc = store.remove(key)
        if rc != -706:
            break
        time.sleep(3)
    print("REMOVE rc=%d attempts=%d" % (rc, attempts), flush=True)
elif action == "quota":
    domain = os.environ["TENANT_ID"]
    count = int(os.environ["OBJ_COUNT"])
    payload = b"x" * int(os.environ["OBJ_BYTES"])

    for i in range(count):
        print("FILL %s-obj-%d rc=%d" % (domain, i, store.put("%s-obj-%d" % (domain, i), payload)))

    for i in range(count):
        got = store.get("%s-obj-%d" % (domain, i))
        print("LEASED %s-obj-%d len=%d" % (domain, i, len(got) if got else -1))

    print("OVER %s-over-0 rc=%d" % (domain, store.put("%s-over-0" % domain, payload)))

    for i in range(count):
        got = store.get("%s-obj-%d" % (domain, i))
        print("INTACT %s-obj-%d len=%d" % (domain, i, len(got) if got else -1))

    try:
        body = urllib.request.urlopen(os.environ["METRICS_URL"], timeout=10).read().decode()
        hit = [l.strip() for l in body.splitlines()
               if l.startswith("mooncake_tenant_quota_reject_total") and domain in l]
        print("REJECTMETRIC %s" % (hit[0] if hit else "<no sample for this tenant>"))
    except Exception as e:
        print("REJECTMETRIC-FAIL %s" % e)

    print("LAPSE sleeping %ss for the leases to expire" % os.environ["LEASE_LAPSE"])
    time.sleep(int(os.environ["LEASE_LAPSE"]))
    for i in range(3):
        print("LATE %s-late-%d rc=%d" % (domain, i, store.put("%s-late-%d" % (domain, i), payload)))
    for i in range(count):
        print("SURVIVED %s-obj-%d exists=%d" % (domain, i, 1 if store.is_exist("%s-obj-%d" % (domain, i)) else 0))
elif action == "drain":
    keys = list(args)
    pending = [k for k in keys if store.is_exist(k)]
    print("PENDING %d" % len(pending))
    deadline = time.time() + 60
    while pending and time.time() < deadline:
        still = []
        for key in pending:
            rc = store.remove(key)
            if rc == 0 or rc == -704:
                print("REMOVE %s rc=%d" % (key, rc))
            elif rc == -706:
                still.append(key)
            else:
                print("REMOVE-FAIL %s rc=%d" % (key, rc))
                still.append(key)
        pending = still
        if pending:
            time.sleep(3)
    for key in pending:
        print("REMOVE %s rc=-706-gave-up" % key)
    print("LEFT %d" % len([k for k in keys if store.is_exist(k)]))
PY

client_run() {
  local action="$1" tenant="$2"
  shift 2
  kubectl -n "$NS_Q" exec "$PROBE" -- env \
    MASTER_URI="$MASTER_URI" TENANT_ID="$tenant" \
    OBJ_COUNT="$OBJ_COUNT" OBJ_BYTES="$OBJ_BYTES" LEASE_LAPSE="$LEASE_LAPSE" \
    METRICS_URL="$METRICS_URL" \
    python3 /tmp/client.py "$action" ${@+"$@"} 2>&1 | grep -E "$CLIENT_TAGS"
}

client_field() {
  printf '%s\n' "$1" | sed -n "s/^$2 //p" | head -1
}

echo "== Phase 2.1: the HA multi-tenant backend, its election and the probe =="
PHASE2_OK=1
kubectl create namespace "$NS_Q" >/dev/null 2>&1 || true

kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND_Q}
spec:
  type: Mooncake
  image: ${IMAGE}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        replicas: 3
        electionBackend: Kubernetes
        multiTenancy: true
        extraArgs:
          - -default_kv_lease_ttl=${KV_LEASE_TTL}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if wait_for kvcachebackends.worker.gpustack.ai "$BACKEND_Q" '{.status.phase}' Ready 300 >/dev/null; then
  record PASS "the HA multi-tenant backend reaches Ready" "phase Ready in 300s"
else
  record FAIL "the HA multi-tenant backend reaches Ready" \
    "phase never became Ready in 300s: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_Q" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  PHASE2_OK=0
fi

if [ "$PHASE2_OK" = 1 ]; then
  HOLDER=""
  for ((i = 0; i < 120; i += 3)); do
    HOLDER="$(kubectl -n "$NS" get leases.coordination.k8s.io "${BACKEND_Q}-leader" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)"
    [ -n "$HOLDER" ] && break
    sleep 3
  done
  if wait_for roles.rbac.authorization.k8s.io "${BACKEND_Q}-member" '{.metadata.name}' "${BACKEND_Q}-member" 120 "$NS" >/dev/null \
    && [ -n "$HOLDER" ]; then
    record PASS "the election is rendered (member Role + Lease with a holder)" \
      "role ${BACKEND_Q}-member exists, lease ${BACKEND_Q}-leader holder='${HOLDER}'"
  else
    record FAIL "the election is rendered (member Role + Lease with a holder)" \
      "role or lease holder missing; the k8s:// client cannot run"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${ROLEBINDING}
  namespace: ${NS_Q}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${ROLEBINDING}
  namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${BACKEND_Q}-member
subjects:
  - kind: ServiceAccount
    name: ${ROLEBINDING}
    namespace: ${NS_Q}
---
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${NS_Q}
spec:
  restartPolicy: Never
  serviceAccountName: ${ROLEBINDING}
  containers:
    - name: probe
      image: ${IMAGE}
      command: ["python3", "-c", "import time; time.sleep(7200)"]
      env:
        - name: POD_IP
          valueFrom: {fieldRef: {fieldPath: status.podIP}}
YAML

  if ! wait_for pod "$PROBE" '{.status.phase}' Running 180 "$NS_Q" >/dev/null; then
    record FAIL "the probe pod runs" \
      "phase='$(kubectl -n "$NS_Q" get pod "$PROBE" -o jsonpath='{.status.phase}' 2>/dev/null)'"
    PHASE2_OK=0
  else
    record PASS "the probe pod runs" "phase=Running"
    if ! kubectl -n "$NS_Q" cp "$CLIENT_SCRIPT" "$PROBE:/tmp/client.py" >/dev/null 2>&1; then
      record FAIL "the client script reaches the probe" "kubectl cp into ${PROBE} failed"
      PHASE2_OK=0
    else
      record PASS "the client script reaches the probe" "client script copied to ${PROBE}"
    fi
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  echo "== Phase 2.2: what an effective quota enforces =="
  apply_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL_Q}
spec:
  backends: [${BACKEND_Q}]
  quota:
    total: 2Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: ${BIND_Q}, namespace: ${NS_Q}}
spec:
  poolRef: {name: ${POOL_Q}}
  quota: {ceiling: ${CEILING}}
  domain: {name: ${DOMAIN_Q}, blockSize: 16, dtype: bfloat16}
YAML
)"
  missing=""
  kubectl get kvcachepools.worker.gpustack.ai "$POOL_Q" -o name >/dev/null 2>&1 \
    || missing="${missing} kvcachepool/${POOL_Q}"
  kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" -o name >/dev/null 2>&1 \
    || missing="${missing} ${NS_Q}/kvcachepoolbinding/${BIND_Q}"
  if [ -n "$missing" ]; then
    record FAIL "the pool and its binding exist" \
      "absent after apply:${missing}; $(printf '%s' "${apply_out:-<no output>}" | tr '\n' ' ' | cut -c1-200)"
  else
    record PASS "the pool and its binding exist" \
      "pool ${POOL_Q} and binding ${BIND_Q} exist"

    wait_for kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" '{.status.phase}' Ready 180 "$NS_Q" >/dev/null || true

    grant="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" \
      -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"
    if [ "$grant" = "$CEILING" ]; then
      record PASS "the tenant reached the ledger" \
        "effectiveQuota=${grant}"
    else
      record FAIL "the tenant reached the ledger" \
        "effectiveQuota is '${grant:-<absent>}', wanted ${CEILING}"
    fi

    QUOTA_LOG="/tmp/kvc-probe-${SFX}.log"
    client_run quota "$DOMAIN_Q" >"$QUOTA_LOG"
    sed -n '1,80p' "$QUOTA_LOG" 2>/dev/null | sed 's/^/    /'

    if grep -q "IMPORT-FAIL\|SETUP-TYPEERROR\|SETUP-FAIL" "$QUOTA_LOG" 2>/dev/null; then
      record FAIL "the store client ran" \
        "$(grep -m1 'IMPORT-FAIL\|SETUP-TYPEERROR\|SETUP-FAIL' "$QUOTA_LOG" | cut -c1-140)"
    elif grep -q "SETUP rc=0" "$QUOTA_LOG" 2>/dev/null; then
      record PASS "the store client ran" "setup accepted the tenant id and connected to the leader"
    else
      record FAIL "the store client ran" "setup did not return 0; see the probe log above"
    fi

    filled="$(count_lines 'FILL .* rc=0' "$QUOTA_LOG")"
    if [ "$filled" -eq "$OBJ_COUNT" ]; then
      record PASS "the domain fills its grant" \
        "${filled} write(s) of ${OBJ_BYTES}B accepted, coming to exactly ${CEILING}"
    else
      record FAIL "the domain fills its grant" \
        "${filled} of ${OBJ_COUNT} writes accepted"
    fi

    leased_ok="$(count_lines 'LEASED .* len=[0-9]' "$QUOTA_LOG")"
    if [ "$leased_ok" -eq "$OBJ_COUNT" ]; then
      record PASS "every object in the grant takes a lease" \
        "${leased_ok} read back, un-evictable for ~10s"
    else
      record FAIL "every object in the grant takes a lease" \
        "${leased_ok} of ${OBJ_COUNT} read back"
    fi

    if grep -q 'OVER .* rc=-1700' "$QUOTA_LOG" 2>/dev/null; then
      record PASS "a write past the grant is refused when nothing can be evicted" \
        "rc=-1700 TENANT_QUOTA_EXCEEDED: the grant is a barrier while its objects are leased"
    elif grep -q 'OVER .* rc=0' "$QUOTA_LOG" 2>/dev/null; then
      record FAIL "a write past the grant is refused when nothing can be evicted" \
        "the write was ACCEPTED past a ${CEILING} grant whose every object held a live lease"
    else
      record FAIL "a write past the grant is refused when nothing can be evicted" \
        "$(grep -m1 'OVER ' "$QUOTA_LOG" | cut -c1-140 || echo 'no OVER line in the log')"
    fi

    if grep -q "rc=-1701" "$QUOTA_LOG" 2>/dev/null; then
      record FAIL "the refusal is about the quota, not the tenant" \
        "a write returned -1701 TENANT_NOT_REGISTERED"
    else
      record PASS "the refusal is about the quota, not the tenant" "no TENANT_NOT_REGISTERED in the run"
    fi

    rejects="$(grep -m1 '^REJECTMETRIC mooncake_tenant_quota_reject_total' "$QUOTA_LOG" 2>/dev/null \
      | awk '{print int($NF)}')"
    if [ "${rejects:-x}" = "1" ]; then
      record PASS "the master recorded exactly one refusal" \
        "mooncake_tenant_quota_reject_total{reason=\"quota_exceeded\"}=1"
    else
      record FAIL "the master recorded exactly one refusal" \
        "$(grep -m1 '^REJECTMETRIC' "$QUOTA_LOG" | cut -c1-160 || echo 'no REJECTMETRIC line') -- wanted 1"
    fi

    intact_ok="$(count_lines 'INTACT .* len=[0-9]' "$QUOTA_LOG")"
    if [ "$intact_ok" -eq "$OBJ_COUNT" ]; then
      record PASS "the objects that caused the refusal are all still there" \
        "${intact_ok} of ${OBJ_COUNT} read back after the refusal"
    else
      record FAIL "the objects that caused the refusal are all still there" \
        "${intact_ok} of ${OBJ_COUNT} read back after the refusal"
    fi

    usage_held=""
    for _ in $(seq 1 20); do
      usage_held="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" \
        -o jsonpath='{.status.usage}' 2>/dev/null)"
      [ "$usage_held" = "$CEILING" ] && break
      sleep 3
    done
    if [ "$usage_held" = "$CEILING" ]; then
      record PASS "the charged bytes are capped at the grant" \
        "usage=${usage_held} after ${OBJ_COUNT}+3 writes of ${OBJ_BYTES}B"
    else
      record FAIL "the charged bytes are capped at the grant" \
        "usage is '${usage_held:-<absent>}', wanted ${CEILING} (${CEILING_BYTES}B)"
    fi

    late_ok="$(count_lines 'LATE .* rc=0' "$QUOTA_LOG")"
    survived="$(count_lines 'SURVIVED .* exists=1' "$QUOTA_LOG")"
    if [ "$late_ok" -eq 3 ] && [ "$survived" -lt "$OBJ_COUNT" ]; then
      record PASS "the lapsed grant admits by evicting this domain's own objects" \
        "${late_ok} further write(s) admitted and ${survived} of ${OBJ_COUNT} originals survive"
    elif [ "$late_ok" -eq 3 ]; then
      record FAIL "the lapsed grant admits by evicting this domain's own objects" \
        "${late_ok} write(s) admitted yet all ${OBJ_COUNT} originals survive"
    else
      record FAIL "the lapsed grant admits by evicting this domain's own objects" \
        "only ${late_ok} of 3 writes admitted after the leases lapsed"
    fi

    RMLOG="/tmp/kvc-probe-rm-${SFX}.log"
    client_run drain "$DOMAIN_Q" "${Q_KEYS[@]}" >"$RMLOG"
    sed -n '1,40p' "$RMLOG" 2>/dev/null | sed 's/^/    /'

    if grep -q '^LEFT 0' "$RMLOG" 2>/dev/null; then
      record PASS "every object can be removed" \
        "$(count_lines '^REMOVE ' "$RMLOG") removal(s) issued, none left"
    else
      record FAIL "every object can be removed" \
        "$(grep -m1 '^LEFT ' "$RMLOG" | cut -c1-60 || echo 'no LEFT line')"
    fi

    usage_after=""
    for _ in $(seq 1 30); do
      usage_after="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" \
        -o jsonpath='{.status.usage}' 2>/dev/null)"
      [ "$usage_after" = "0" ] && break
      sleep 3
    done
    if [ "$usage_after" = "0" ]; then
      record PASS "the bytes come back once the objects go" \
        "usage fell from ${CEILING} to 0"
    else
      record FAIL "the bytes come back once the objects go" \
        "usage is '${usage_after:-<absent>}' 90s after every object was removed"
    fi
  fi

  echo "== Phase 2.3: the multi-tenant ledger gate =="
  REFUSED_OUT="$(client_run put "$DOMAIN_MT" "$KEY")"
  REFUSED_SETUP="$(client_field "$REFUSED_OUT" "SETUP")"
  REFUSED_PUT="$(client_field "$REFUSED_OUT" "PUT")"
  REFUSED_GET="$(client_field "$REFUSED_OUT" "GET")"
  if [ "$REFUSED_SETUP" = "rc=0" ] && [ "$REFUSED_PUT" = "rc=-1701" ] && [[ "$REFUSED_GET" == *"len=-1"* ]]; then
    record PASS "an unregistered tenant's put is refused with -1701" \
      "setup=${REFUSED_SETUP} put=${REFUSED_PUT} get='${REFUSED_GET}'"
  elif [ "$REFUSED_SETUP" != "rc=0" ]; then
    record FAIL "an unregistered tenant's put is refused with -1701" \
      "client did not reach the master (setup='${REFUSED_SETUP}'): $(echo "$REFUSED_OUT" | tr '\n|' '  ')"
  else
    record FAIL "an unregistered tenant's put is refused with -1701" \
      "setup=${REFUSED_SETUP} put='${REFUSED_PUT}' get='${REFUSED_GET}'"
  fi

  REGISTERED=0
  REGISTER_ERR=""
  for attempt in 1 2 3; do
    if REGISTER_ERR="$(kubectl apply -f - <<YAML 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${BIND_MT}
  namespace: ${NS_Q}
spec:
  poolRef:
    name: ${POOL_Q}
  domain:
    name: ${DOMAIN_MT}
    blockSize: 16
    dtype: bfloat16
  quota:
    ceiling: ${LEDGER_CEILING}
YAML
)"; then
      REGISTERED=1
      break
    fi
    sleep 5
  done

  LEDGER_OK=0
  if [ "$REGISTERED" != "1" ]; then
    record FAIL "the Pool and its Binding register the domain" \
      "apply failed on all ${attempt} attempt(s): $(echo "$REGISTER_ERR" | tr '\n|' '  ' | cut -c1-200)"
  elif wait_for kvcachepools.worker.gpustack.ai "$POOL_Q" '{.status.phase}' Ready 180 >/dev/null \
    && wait_for kvcachepoolbindings.worker.gpustack.ai "$BIND_MT" '{.status.phase}' Ready 180 "$NS_Q" >/dev/null; then
    record PASS "the Pool and its Binding register the domain" \
      "pool ${POOL_Q} Ready, binding ${BIND_MT} Ready, domain='${DOMAIN_MT}'"
    LEDGER_OK=1
  else
    record FAIL "the Pool and its Binding register the domain" \
      "pool phase='$(kubectl get kvcachepools.worker.gpustack.ai "$POOL_Q" -o jsonpath='{.status.phase}' 2>/dev/null)', binding phase='$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_MT" -o jsonpath='{.status.phase}' 2>/dev/null)'"
  fi

  if [ "$LEDGER_OK" = 1 ]; then
    ADMITTED_OUT="$(client_run put "$DOMAIN_MT" "$KEY")"
    ADMITTED_PUT="$(client_field "$ADMITTED_OUT" "PUT")"
    ADMITTED_GET="$(client_field "$ADMITTED_OUT" "GET")"
    if [ "$ADMITTED_PUT" = "rc=0" ] && [[ "$ADMITTED_GET" == *"match=True"* ]]; then
      record PASS "the registered tenant's put succeeds and reads back" \
        "put=${ADMITTED_PUT} get='${ADMITTED_GET}'"
    else
      record FAIL "the registered tenant's put succeeds and reads back" \
        "put='${ADMITTED_PUT}' get='${ADMITTED_GET}': $(echo "$ADMITTED_OUT" | tr '\n|' '  ')"
    fi

    DRAIN_OUT="$(client_run remove "$DOMAIN_MT" "$KEY")"
    DRAIN_REMOVE="$(client_field "$DRAIN_OUT" "REMOVE")"
    DRAIN_RC="${DRAIN_REMOVE%% *}"
    if [ "$DRAIN_RC" = "rc=0" ] || [ "$DRAIN_RC" = "rc=-704" ]; then
      record PASS "the teardown drain removes the key (a held domain blocks pool deletion)" \
        "remove=${DRAIN_REMOVE} for key ${KEY}"
    else
      record FAIL "the teardown drain removes the key (a held domain blocks pool deletion)" \
        "remove='${DRAIN_REMOVE}': $(echo "$DRAIN_OUT" | tr '\n|' '  ')"
    fi
  fi
fi

results
