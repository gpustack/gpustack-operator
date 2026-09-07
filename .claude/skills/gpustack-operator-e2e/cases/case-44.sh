#!/usr/bin/env bash
#
# CASE 44 — What an effective quota actually is: a refusal when nothing can be evicted, and an
#           eviction of this domain's own objects when something can be
#   (MUTATING, self-recovering)
#
#   case-44.sh <NS>
#
# Goal:        Asserts what the master really does when a reuse domain is driven past its grant, which
#              is NOT what a reader of the field name would assume. A first run of this case wrote
#              eight objects into a 16 MiB grant, every write returned success, and four of the eight
#              were simply gone afterwards. Tracing it into the store's own source settled the
#              mechanism:
#
#                master_service.cpp:4492-4506 — PutStart does not return TENANT_QUOTA_EXCEEDED on the
#                first refusal. It calls EvictTenantMemoryForQuota(tenant, deficit) to throw away THIS
#                tenant's own objects and retries, up to kMaxTenantQuotaEvictionRetries = 2 (:69).
# Only when three attempts in a row cannot free enough does the write actually fail.
#
#                master_service.cpp:9856 — that eviction skips any object whose lease is still live
#                (`!member_metadata.IsLeaseExpired(now)`), and a GET grants a lease of
# DEFAULT_DEFAULT_KV_LEASE_TTL = 10s (types.h:84).
#
# So the grant is a barrier ONLY while the objects holding it cannot be evicted. This case
#              pins BOTH halves, because either one alone reads as a different product:
#                - hold a lease on everything, then write past the grant: the write is refused, and the
#                  objects that caused the refusal are all still there;
#                - let the lease lapse and write again: the write is admitted, and this domain's own
#                  older objects are gone to pay for it.
#
# The second is the one nobody would have written down from the field names, and it is
#              silent: EvictTenantMemoryForQuota is a separate path from the store's general LRU, so
#              master_evicted_key_count and its three siblings all stay at zero while it happens.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), and CASE 43
#              passing, so a pool and its bindings are known to converge before this pushes one over.
# No GPU and no RDMA. Needs a registry the cluster can pull the Mooncake image from —
#              override with E2E_MOONCAKE_IMAGE.
#
# Inputs:      All real, nothing mocked. A KVCacheBackend with one small DRAM member, a KVCachePool
#              over it, and ONE binding whose ceiling is deliberately tiny so the grant can be filled
#              in seconds rather than by writing terabytes. The writes come from the leader's own
#              image, which ships the store client; the case runs it in throwaway Pods.
#
# Expected:    - the binding reports a grant, which is what proves its tenant reached the ledger;
#              - writes succeed up to the grant;
#              - with every object in the grant holding a live lease, the NEXT write is refused, and
#                refused for being over quota rather than for the tenant being unknown — the two are
#                different refusals and only one of them is this case's subject;
#              - the objects that caused that refusal all still read back;
#              - the charged bytes are capped AT the grant rather than tracking what was written;
#              - once the leases lapse, a further write is admitted and older objects of the same
#                domain are gone — recorded as the behaviour it is, not endorsed;
#              - the objects can be removed and the charged bytes come back.
# If the write under lease is NOT refused, this case FAILS: the grant would then be a
#              readout with no enforcement anywhere, which is a finding about the master and belongs in
#              the spec's Verification written by a person, never softened into a passing assertion.
#
# Cleanup:     Trap removes both probe Pods, then the namespace, pool and backend. Removal is retried
#              past OBJECT_HAS_LEASE (-706) rather than attempted once: a domain that still holds
#              objects makes the master refuse to drop its quota, the operator correctly keeps its
#              finalizer, and the namespace is left Terminating forever. The first version of this case
#              did exactly that on every run.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-44.sh <NS>}"
IMAGE="${E2E_MOONCAKE_IMAGE:-docker.io/kvcacheai/mooncake:0.3.13}"

# LC_ALL=C and the disabled pipefail are both load-bearing: under a UTF-8 locale tr dies on
# /dev/urandom, and with pipefail on the SIGPIPE from head turns a trailing `|| echo $$` into an
# append, so LC_ALL=C alone yields random characters with the PID glued on. The measurements behind
# both halves are recorded once, at the same idiom in _kvcache-inject-lib.sh.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
# The names below are cluster-scoped, so a bare PID — small and reused across reboots — lets two runs
# adopt each other's leftovers. The epoch makes a collision need the same PID in the same second.
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-q-${SFX}"
POOL="kvcp-q-${SFX}"
NS_Q="kvc-q-${SFX}"
DOMAIN="dom-q-${SFX}"
PROBE="kvc-probe-${SFX}"

# The grant, and the writes that fill it exactly. Four 4 MiB objects come to 16 MiB with nothing left
# over — the master charges metadata.size and adds no overhead of its own, which a first run confirmed
# by reporting charged_bytes = 16777216 on the nose. Filling it EXACTLY matters: a fill that already
# overshot would have evicted before this case got to the part it means to test.
CEILING="16Mi"
CEILING_BYTES=$((16 * 1024 * 1024))
OBJ_BYTES=$((4 * 1024 * 1024))
OBJ_COUNT=4
# 1.5x the master's default lease TTL of 10s (types.h:84). The operator renders no lease flag, so the
# default is what runs.
LEASE_LAPSE=15

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# count_lines exists because `grep -c … || echo 0` APPENDS a zero rather than substituting one: grep -c
# already prints 0 when it matches nothing, and exits 1 while doing so, so the two together produce
# "0\n0" and every arithmetic test downstream rejects it as "integer expected".
count_lines() {
  local n
  n="$(grep -c "$1" "$2" 2>/dev/null)" || n=0
  echo "$n"
}

restore() {
  echo
  echo "[case-44] cleanup"
  kubectl -n "$NS_Q" delete pod "$PROBE" "${PROBE}-rm" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -f "/tmp/kvc-probe-${SFX}.log" "/tmp/kvc-probe-rm-${SFX}.log" 2>/dev/null || true
  kubectl delete namespace "$NS_Q" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  # A binding whose domain still holds objects is held by its finalizer ON PURPOSE — the master refuses
  # to drop a non-empty tenant's quota, and the operator is right to wait. That is correct behaviour
  # and a broken cleanup at the same time, so this reports what held it before forcing the issue.
  # Reporting rather than silently forcing: a run that got here left objects behind, and the assertions
  # above have already recorded that as a failure.
  local left
  for _ in $(seq 1 20); do
    left="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai -o name 2>/dev/null)"
    [ -z "$left" ] && break
    sleep 3
  done
  if [ -n "${left:-}" ]; then
    echo "[case-44] a binding is still held after 60s; what the operator says about it:"
    kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai \
      -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[*]}{.type}={.status}({.reason}) {.message}{"\n"}{end}{end}' 2>/dev/null \
      | sed 's/^/    /'
    echo "[case-44] forcing the finalizers off so the next run starts clean"
    kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai -o name 2>/dev/null | while read -r b; do
      kubectl -n "$NS_Q" patch "$b" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    done
  fi
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

echo "== 1. a pool, and one binding with a small grant =="

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
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | tr '|' ' '; done
  exit 1
fi

# TWO DOCUMENTS, AND BOTH OBJECTS ARE READ BACK RATHER THAN THE OUTPUT BEING DISCARDED. `kubectl
# apply` reports the pool and the binding in ONE stream, so with the output thrown away a run that
# created the pool and had the binding refused looks exactly like a clean one — and the grant check
# below would then report effectiveQuota as absent, blaming the ledger for an object nobody made.
#
# The gate is the objects' EXISTENCE, not the words the apply printed. Counting ` created` lines
# instead makes the gate depend on how the objects came to be there: a re-run over a surviving
# namespace, or the retrying kubectl shim resending an apply whose response was lost, both report
# `configured`/`unchanged` for objects that are present and correct. The apply output is still
# captured, because a refusal's text is the only diagnosis of why an object is missing.
apply_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL}
spec:
  backends: [${BACKEND}]
  quota:
    total: 1Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-q, namespace: ${NS_Q}}
spec:
  poolRef: {name: ${POOL}}
  quotaCeiling: ${CEILING}
  domain: {name: ${DOMAIN}, blockSize: 16, dtype: bfloat16}
YAML
)"
missing=""
kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o name >/dev/null 2>&1 \
  || missing="${missing} kvcachepool/${POOL}"
kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai bind-q -o name >/dev/null 2>&1 \
  || missing="${missing} ${NS_Q}/kvcachepoolbinding/bind-q"
if [ -n "$missing" ]; then
  record FAIL "the pool and its binding exist" \
    "absent after the apply:${missing} — so nothing below has a subject. The apply said: \
$(printf '%s' "${apply_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | tr '|' ' '; done
  exit 1
fi

wait_for kvcachepoolbindings.worker.gpustack.ai bind-q '{.status.phase}' Ready 180 "$NS_Q" >/dev/null || true

# The grant's PRESENCE is the precondition for everything below, and asserting it is not a formality.
# A write to a tenant the master holds no policy for is refused TENANT_NOT_REGISTERED — the very same
# refusal a domain that was never declared gets, and NOT a quota verdict. Without this check a case
# whose policy never reached the ledger would read its own misconfiguration as the enforcement it set
# out to prove.
grant="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai bind-q \
  -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"
if [ "$grant" = "$CEILING" ]; then
  record PASS "the tenant reached the ledger" \
    "effectiveQuota=${grant}; a write refused below is therefore a quota verdict, not an unknown tenant"
else
  record FAIL "the tenant reached the ledger" \
    "effectiveQuota is '${grant:-<absent>}', wanted ${CEILING}; the byte arithmetic below assumes the \
grant is exactly what was asked for"
fi

echo "== 2. fill the grant, hold it under lease, then write past it =="

# The client is the leader's own image, which ships it; nothing else in this suite can speak to the
# store. Every call's return code is printed with a tag, so the log distinguishes a quota refusal
# from an unknown tenant from a transport failure — three outcomes that a bare "write failed" would
# collapse into one.
cat <<PY | kubectl -n "$NS_Q" run "$PROBE" --image="$IMAGE" --restart=Never \
  --overrides='{"spec":{"containers":[{"name":"probe","image":"'"$IMAGE"'","command":["python3","-"],"stdin":true,"stdinOnce":true}]}}' \
  -i --rm --quiet >/tmp/kvc-probe-${SFX}.log 2>&1 || true
import sys, time, socket, urllib.request
try:
    from mooncake.store import MooncakeDistributedStore
except Exception as e:
    print("IMPORT-FAIL %s" % e); sys.exit(0)

store = MooncakeDistributedStore()

# The local hostname is what the peer dials BACK on under a peer-to-peer handshake, so it has to be
# an address reachable from inside the cluster; a loopback name leaves the master unable to answer.
# The local buffer is what the client stages data through, and a zero there is not "use the
# default" — a client with no buffer cannot move a multi-megabyte object.
ip = socket.gethostbyname(socket.gethostname())
try:
    rc = store.setup("%s:0" % ip, "P2PHANDSHAKE", 0, 64 * 1024 * 1024, "tcp", "",
                     "${BACKEND}-leader.${NS}.svc:50051", tenant_id="${DOMAIN}")
except TypeError as e:
    # Kept, unlike the inspect.signature() call that used to sit above it. That one raised
    # ValueError on this method — it is a pybind11 builtin and carries no introspectable
    # signature — so the diagnostic killed the script BEFORE setup was ever reached, and every
    # assertion below then failed while reporting something about setup's return value. A probe
    # that cannot fail is worth more than a probe that explains.
    print("SETUP-TYPEERROR %s" % e); sys.exit(0)
print("SETUP-RC %d" % rc)
if rc != 0:
    sys.exit(0)

payload = b"x" * ${OBJ_BYTES}

# Exactly to the grant, never over it. An overshoot here would evict before the lease is taken and
# leave the refusal below with nothing to prove.
for i in range(${OBJ_COUNT}):
    print("FILL ${DOMAIN}-obj-%d rc=%d" % (i, store.put("${DOMAIN}-obj-%d" % i, payload)))

# The lease is the whole mechanism. A GET makes the object un-evictable for the master's default
# 10s (master_service.cpp:9856 skips replicas whose lease has not expired), so for that window the
# grant has nothing it is allowed to throw away and has to refuse instead.
for i in range(${OBJ_COUNT}):
    got = store.get("${DOMAIN}-obj-%d" % i)
    print("LEASED ${DOMAIN}-obj-%d len=%d" % (i, len(got) if got else -1))

print("OVER ${DOMAIN}-over-0 rc=%d" % store.put("${DOMAIN}-over-0", payload))

# Asserted per object, and asserted AFTER the refusal rather than inferred from it: the claim is that
# the refusal cost nothing, and a store that had quietly dropped one to try to make room would still
# have refused.
for i in range(${OBJ_COUNT}):
    got = store.get("${DOMAIN}-obj-%d" % i)
    print("INTACT ${DOMAIN}-obj-%d len=%d" % (i, len(got) if got else -1))

# The master's own account of that refusal, read here rather than inferred from the client's return
# code, so the two are independent witnesses to the same event. This counter is incremented ONLY on
# the branch that gives up after the last eviction retry (master_service.cpp:4499) — it counts real
# refusals, not overshoots — which is what makes "exactly one" a meaningful reading. The store's four
# general eviction counters stay at zero throughout, because EvictTenantMemoryForQuota is not on that
# path; a case that watched them would conclude nothing had happened.
#
# This series is absent from the catalogue in pkg/worker/kvcache/mooncake/tenant_metrics.go, and
# that absence is not evidence the master does not export it: that block lists the series this
# OPERATOR reads, not the ones the master publishes. Nothing in the operator acts on a refusal count,
# so it has no reader there. Declared at master_metric_manager.cpp:410 and serialized at :1988, read
# at tag v0.3.13-rc1. The same catalogue names two 0.3.12-era series a 0.3.13 master no longer
# exports, which is the same distinction seen from the other side.
# No curl in this image, and none needed.
try:
    body = urllib.request.urlopen(
        "http://${BACKEND}-leader.${NS}.svc:9003/metrics", timeout=10).read().decode()
    hit = [l.strip() for l in body.splitlines()
           if l.startswith("mooncake_tenant_quota_reject_total") and "${DOMAIN}" in l]
    print("REJECTMETRIC %s" % (hit[0] if hit else "<no sample for this tenant>"))
except Exception as e:
    print("REJECTMETRIC-FAIL %s" % e)

# The other half. With every lease lapsed the same write that was just refused goes through, and it is
# paid for out of this domain's own older objects. Printed as facts rather than judged here.
print("LAPSE sleeping ${LEASE_LAPSE}s for the leases to expire")
time.sleep(${LEASE_LAPSE})
for i in range(3):
    print("LATE ${DOMAIN}-late-%d rc=%d" % (i, store.put("${DOMAIN}-late-%d" % i, payload)))
for i in range(${OBJ_COUNT}):
    print("SURVIVED ${DOMAIN}-obj-%d exists=%d" % (i, 1 if store.is_exist("${DOMAIN}-obj-%d" % i) else 0))

# NOT removed here. Removal is a second probe, so this case can read the charged figure WHILE the
# objects still exist. Without that reading the "the bytes come back" assertion below would pass on a
# run where nothing was ever written — the charge is already 0 — which is precisely the run where it
# ought to shout.
PY

LOG="/tmp/kvc-probe-${SFX}.log"
sed -n '1,80p' "$LOG" 2>/dev/null | sed 's/^/    /'

if grep -q "IMPORT-FAIL\|SETUP-TYPEERROR" "$LOG" 2>/dev/null; then
  record FAIL "the store client ran" \
    "$(grep -m1 'IMPORT-FAIL\|SETUP-TYPEERROR' "$LOG" | cut -c1-140)"
elif grep -q "SETUP-RC 0" "$LOG" 2>/dev/null; then
  record PASS "the store client ran" "setup accepted the tenant id and connected to the leader"
else
  record FAIL "the store client ran" "setup did not return 0; see the probe log above"
fi

filled="$(count_lines 'FILL .* rc=0' "$LOG")"
if [ "$filled" -eq "$OBJ_COUNT" ]; then
  record PASS "the domain fills its grant" \
    "${filled} write(s) of ${OBJ_BYTES}B accepted, coming to exactly ${CEILING}"
else
  record FAIL "the domain fills its grant" \
    "${filled} of ${OBJ_COUNT} writes accepted; the grant was never filled, so a refusal below would \
not be a quota verdict"
fi

leased_ok="$(count_lines 'LEASED .* len=[0-9]' "$LOG")"
if [ "$leased_ok" -eq "$OBJ_COUNT" ]; then
  record PASS "every object in the grant takes a lease" \
    "${leased_ok} read back, so none of them can be evicted for the next ~10s"
else
  record FAIL "every object in the grant takes a lease" \
    "${leased_ok} of ${OBJ_COUNT} read back; an object that is not leased is one the master may evict, \
and the refusal below would then never come"
fi

# THE SUBJECT OF THIS CASE. Under lease the master has nothing it is allowed to throw away, so its
# three eviction attempts (master_service.cpp:4492) all come up empty and the write finally fails with
# TENANT_QUOTA_EXCEEDED = -1700 (types.h:392). A success here means the grant is not enforced ANYWHERE,
# which is a finding to write up, never an assertion to soften.
if grep -q 'OVER .* rc=-1700' "$LOG" 2>/dev/null; then
  record PASS "a write past the grant is refused when nothing can be evicted" \
    "rc=-1700 TENANT_QUOTA_EXCEEDED: the grant is a barrier while its objects are leased"
elif grep -q 'OVER .* rc=0' "$LOG" 2>/dev/null; then
  record FAIL "a write past the grant is refused when nothing can be evicted" \
    "the write was ACCEPTED past a ${CEILING} grant whose every object held a live lease. The master \
had nothing it was allowed to evict and admitted the write anyway: the grant is not enforced at all. \
Record it in the spec's Verification, do not soften it"
else
  record FAIL "a write past the grant is refused when nothing can be evicted" \
    "$(grep -m1 'OVER ' "$LOG" | cut -c1-140 || echo 'no OVER line in the log')"
fi

# A quota refusal and an unregistered tenant are different failures with different fixes, and only
# the first is this case's subject. -1701 here would mean the policy never reached the ledger.
if grep -q "rc=-1701" "$LOG" 2>/dev/null; then
  record FAIL "the refusal is about the quota, not the tenant" \
    "a write returned -1701 TENANT_NOT_REGISTERED — the tenant has no policy, so nothing here \
tested a quota"
else
  record PASS "the refusal is about the quota, not the tenant" "no TENANT_NOT_REGISTERED in the run"
fi

# A SECOND, independent witness to the same refusal. The client's -1700 is what the caller saw; this
# is what the master recorded. They can disagree — a transport that lost the reply, a client that
# mapped the code itself — and a case that only ever asks one side cannot tell.
rejects="$(grep -m1 '^REJECTMETRIC mooncake_tenant_quota_reject_total' "$LOG" 2>/dev/null \
  | awk '{print int($NF)}')"
if [ "${rejects:-x}" = "1" ]; then
  record PASS "the master recorded exactly one refusal" \
    "mooncake_tenant_quota_reject_total{reason=\"quota_exceeded\"}=1, which is the write above and \
nothing else: no earlier write was quietly refused and retried"
else
  record FAIL "the master recorded exactly one refusal" \
    "$(grep -m1 '^REJECTMETRIC' "$LOG" | cut -c1-160 || echo 'the probe printed no REJECTMETRIC line') \
— wanted exactly 1"
fi

intact_ok="$(count_lines 'INTACT .* len=[0-9]' "$LOG")"
if [ "$intact_ok" -eq "$OBJ_COUNT" ]; then
  record PASS "the objects that caused the refusal are all still there" \
    "${intact_ok} of ${OBJ_COUNT} read back after the refusal: refusing cost nothing"
else
  record FAIL "the objects that caused the refusal are all still there" \
    "${intact_ok} of ${OBJ_COUNT} read back after the refusal; the master gave something up while \
refusing anyway"
fi

echo "== 3. the charge is capped at the grant, not at what was written =="

# Seven objects of 4 MiB each went in over the course of the probe, and the domain is granted four of
# them. Reading exactly the grant back is what shows the charge tracks what is HELD, not what was
# offered — and it is also the "before" that gives the reclaim assertion at the end its meaning: a
# charge returning to zero proves nothing on a run where nothing was ever charged.
usage_held=""
for _ in $(seq 1 20); do
  usage_held="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai bind-q \
    -o jsonpath='{.status.usage}' 2>/dev/null)"
  [ "$usage_held" = "$CEILING" ] && break
  sleep 3
done
if [ "$usage_held" = "$CEILING" ]; then
  record PASS "the charged bytes are capped at the grant" \
    "usage=${usage_held} after ${OBJ_COUNT}+3 writes of ${OBJ_BYTES}B, which is the grant exactly"
else
  record FAIL "the charged bytes are capped at the grant" \
    "usage is '${usage_held:-<absent>}', wanted ${CEILING} (${CEILING_BYTES}B)"
fi

echo "== 4. once the leases lapse, the grant admits by evicting this domain's own objects =="

# NOT an endorsement. This records the behaviour the field names would never suggest, so that the day
# the master changes it this case says so instead of quietly passing. A FAIL here is as likely to mean
# "upstream fixed it" as "upstream broke it" — either way the spec's Verification needs rewriting, and
# that is exactly what an assertion is for.
late_ok="$(count_lines 'LATE .* rc=0' "$LOG")"
survived="$(count_lines 'SURVIVED .* exists=1' "$LOG")"
if [ "$late_ok" -eq 3 ] && [ "$survived" -lt "$OBJ_COUNT" ]; then
  record PASS "the lapsed grant admits by evicting this domain's own objects" \
    "${late_ok} further write(s) admitted and only ${survived} of the original ${OBJ_COUNT} survive: \
the grant is an eviction trigger once its objects are evictable, not an admission barrier"
elif [ "$late_ok" -eq 3 ]; then
  record FAIL "the lapsed grant admits by evicting this domain's own objects" \
    "${late_ok} write(s) admitted yet all ${OBJ_COUNT} originals survive — the domain now holds more \
than its grant, which no path in the master should allow"
else
  record FAIL "the lapsed grant admits by evicting this domain's own objects" \
    "only ${late_ok} of 3 writes admitted after the leases lapsed; if the master now refuses instead \
of evicting, the behaviour recorded in the spec's Verification is out of date and must be rewritten"
fi

echo "== 5. removing the objects gives the bytes back =="

# A SECOND probe, on purpose — see the charge reading above. Removal is retried rather than attempted
# once: an object read within the last lease window is refused OBJECT_HAS_LEASE = -706 (types.h:318),
# and the first version of this case took that single refusal as final. The domain then still held
# objects, the master refused to drop its quota, the operator correctly kept its finalizer, and the
# namespace was left Terminating on every single run.
cat <<PY | kubectl -n "$NS_Q" run "${PROBE}-rm" --image="$IMAGE" --restart=Never \
  --overrides='{"spec":{"containers":[{"name":"probe","image":"'"$IMAGE"'","command":["python3","-"],"stdin":true,"stdinOnce":true}]}}' \
  -i --rm --quiet >/tmp/kvc-probe-rm-${SFX}.log 2>&1 || true
import sys, time, socket
try:
    from mooncake.store import MooncakeDistributedStore
except Exception as e:
    print("IMPORT-FAIL %s" % e); sys.exit(0)

store = MooncakeDistributedStore()
ip = socket.gethostbyname(socket.gethostname())
try:
    rc = store.setup("%s:0" % ip, "P2PHANDSHAKE", 0, 64 * 1024 * 1024, "tcp", "",
                     "${BACKEND}-leader.${NS}.svc:50051", tenant_id="${DOMAIN}")
except TypeError as e:
    print("SETUP-TYPEERROR %s" % e); sys.exit(0)
if rc != 0:
    print("SETUP-RC %d" % rc); sys.exit(0)

keys = (["${DOMAIN}-obj-%d" % i for i in range(${OBJ_COUNT})]
        + ["${DOMAIN}-over-0"]
        + ["${DOMAIN}-late-%d" % i for i in range(3)])

# Retried on -706 rather than slept through blindly: the lease TTL is a master startup parameter, so a
# fixed sleep here would be a number that silently stops being right.
#
# -704 OBJECT_NOT_FOUND counts as done, not as a failure. Half these keys are EXPECTED to be gone by
# now — the overflow write was refused and never existed, and the originals were evicted to pay for
# the late writes. Treating "already absent" as a removal failure would make this probe red on exactly
# the run where the case worked.
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
            # Retried like -706, and tagged apart from the successes. Printed as a plain REMOVE
            # line an unexpected code was indistinguishable from a removal that worked, and
            # leaving it out of `still` dropped the key from the loop for good: LEFT below would
            # still catch it, but only after a log that read as though every removal had landed.
            print("REMOVE-FAIL %s rc=%d" % (key, rc))
            still.append(key)
    pending = still
    if pending:
        time.sleep(3)
for key in pending:
    print("REMOVE %s rc=-706-gave-up" % key)
print("LEFT %d" % len([k for k in keys if store.is_exist(k)]))
PY

RMLOG="/tmp/kvc-probe-rm-${SFX}.log"
sed -n '1,40p' "$RMLOG" 2>/dev/null | sed 's/^/    /'

if grep -q '^LEFT 0' "$RMLOG" 2>/dev/null; then
  record PASS "every object can be removed" \
    "$(count_lines '^REMOVE ' "$RMLOG") removal(s) issued, none left holding the domain open"
else
  record FAIL "every object can be removed" \
    "$(grep -m1 '^LEFT ' "$RMLOG" | cut -c1-60 || echo 'the removal probe printed no LEFT line') \
— a domain that still holds objects makes the master refuse to drop its quota, and the binding's \
finalizer will hold correctly and forever"
fi

# Reclaimable, not merely spent. The charge falling is what separates a quota from a lifetime
# allowance, and the capped reading above is what lets this one say so.
usage_after=""
for _ in $(seq 1 30); do
  usage_after="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai bind-q \
    -o jsonpath='{.status.usage}' 2>/dev/null)"
  [ "$usage_after" = "0" ] && break
  sleep 3
done
if [ "$usage_after" = "0" ]; then
  record PASS "the bytes come back once the objects go" \
    "usage fell from ${CEILING} to 0: the grant is reclaimable, not a lifetime allowance"
else
  record FAIL "the bytes come back once the objects go" \
    "usage is '${usage_after:-<absent>}' 90s after every object was removed"
fi

# Results.
echo
echo "STATUS | CHECK | OBJECT"
# Split on the delimiter `record` actually wrote, not on whitespace — see case-43 for what the
# whitespace split did to multi-word CHECK names.
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-44] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-44] all checks passed"
