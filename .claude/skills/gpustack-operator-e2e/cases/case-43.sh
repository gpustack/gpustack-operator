#!/usr/bin/env bash
#
# CASE 43 — Two namespaces share one KV cache pool: quota falls in proportion, and both backend
#   preconditions fail loudly   (MUTATING, self-recovering)
#
#   case-43.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. The KVCacheBackend and the
# KVCachePool are cluster-scoped; the Bindings live in two namespaces this case creates and removes.
#
# Goal:        A KVCachePool is the quota domain over one KVCacheBackend, and a KVCachePoolBinding is
#              a namespace's authorization to use it under one reuse domain. This proves the four
#              things that only a real master can answer:
#                (1) TWO NAMESPACES, ONE POOL. Two Bindings in different namespaces, each with its
#                    own reuse domain, both reach Ready against the same pool, and the pool lists
#                    both — the list a finalizer later refuses deletion on.
#                (2) A CEILING IS A REQUEST, NOT A GRANT. With the sum of what the Bindings ask for
#                    deliberately above what the master has to give, each one's effective quota falls
# IN PROPORTION to what it asked. The arithmetic is the MASTER'S, not this
#                    operator's — see the note on criterion 2 below before reading a red run.
#                (3) THE PRECONDITIONS FAIL LOUDLY. A master without multi-tenancy holds no tenant
#                    ledger, and a master that cannot persist its quota policy accepts no quota.
# Neither may pass silently: each raises a named Condition and holds the pool away
#                    from Ready.
#                (4) THE EXCLUSIVE REUSE DOMAIN. A second Binding claiming a domain name already
#                    registered is refused at admission, in another namespace, with the holder named.
#
# Two assumptions the unit tests cannot reach are asserted here rather than inherited:
#              that the leader's image can actually run the init container's shell, and that a
#              multi-tenancy master reaches Ready with no pool bound to it yet — the path where the
#              policy ConfigMap does not exist and the seed falls back. The master is asserted Ready
# BEFORE any pool is created; the reverse order never exercises the fallback.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). No GPU, no RDMA
#              and no etcd: the backend runs a DRAM store, and the whole case is sized so that a
#              2 GiB member is enough to oversubscribe. Needs a registry the cluster can pull the
# Mooncake image from — override with E2E_MOONCAKE_IMAGE.
#
# Inputs:      All real, nothing mocked. A KVCacheBackend with multi-tenancy on and three small DRAM
#              members; a KVCachePool over it; two Bindings in two created namespaces. The one
# MOCKED value is criterion 5's usedBy entry, which the case patches onto a Binding's
#              status itself: the kind that will write it in production is the model-deployment
#              spec's, which this repository has not built, so nothing on this cluster fills that
#              list. Waiting for a writer would wait forever; skipping the write would assert a
#              release nothing was holding.
#
# Expected:    - the master reaches Ready with no pool bound, and its init container completed;
#              - both Bindings Ready, the pool's usedBy naming both, sorted;
#              - each Binding's effectiveQuota is its share of the master's allocatable capacity;
#              - the backend's own usedBy carries the pool's claim with an EMPTY namespace, exactly
#                once after a second converging pass;
#              - a third Binding reusing a registered domain name is refused by the API server;
#              - a Binding whose usedBy is non-empty cannot be deleted, and the condition names the
#                holder;
#              - multi-tenancy turned off under the pool raises MultiTenancyDisabled;
#              - a policy source the master cannot write raises QuotaPolicyNotWritable;
#              - a pool over a backend with no mounted member is not Ready and says why.
#
# Cleanup:     Trap removes the two namespaces (taking their Bindings), the pools, the backends, and
#              — before anything else — the directory this case puts in place of the leader's policy
#              file. That rmdir is not optional: the file lives on an emptyDir that outlives the
#              container, so a leader restarted while the directory stands fails to parse it and
#              crash-loops with no way back. The trap also clears the finalizer it deliberately
#              wedged in criterion 5, or the namespace delete would hang on it. Idempotent, runs on
#              pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-43.sh <NS>}"
IMAGE="${E2E_MOONCAKE_IMAGE:-docker.io/kvcacheai/mooncake:0.3.13}"

# Every name carries the same suffix. A reuse domain name is unique CLUSTER-WIDE — the webhook
# enforces it across namespaces — so a fixed name would collide with an interrupted earlier run and
# fail this case for a reason that is not about the operator.
#
# LC_ALL=C and the disabled pipefail are both load-bearing: under a UTF-8 locale tr dies on
# /dev/urandom, and with pipefail on the SIGPIPE from head turns a trailing `|| echo $$` into an
# append, so LC_ALL=C alone yields random characters with the PID glued on. The measurements behind
# both halves are recorded once, at the same idiom in _kvcache-inject-lib.sh.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
# The names below are cluster-scoped, so a bare PID — small and reused across reboots — lets two runs
# adopt each other's leftovers. The epoch makes a collision need the same PID in the same second.
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-e2e-${SFX}"
EMPTY_BACKEND="kvcb-empty-${SFX}"
POOL="kvcp-e2e-${SFX}"
EMPTY_POOL="kvcp-empty-${SFX}"
NS_A="kvc-a-${SFX}"
NS_B="kvc-b-${SFX}"
DOM_A="dom-a-${SFX}"
DOM_B="dom-b-${SFX}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# All three labels, and each one is load-bearing. The instance label alone also selects the members,
# which run the same image and would answer an exec with a different process. The component value for
# a member carries its group ordinal — `member-0`, not `member` — so a selector for members later
# cannot be written by analogy with this one.
#
# `items[*]` rather than `items[0]`: an empty list makes the indexed form fail rather than return
# nothing, and the difference between "no pod" and "the query was malformed" is one this case has to
# keep, since it reports the first as a hard stop.
leader_pod() {
  kubectl -n "$NS" get pod \
    -l "app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1
}

restore() {
  echo
  echo "[case-43] cleanup"

  # FIRST, and unconditionally. The policy file lives on an emptyDir that survives a container
  # restart, so a directory left in its place turns the next restart into an unrecoverable crash
  # loop — the master cannot parse a directory as a policy document, and the init container only
  # re-seeds on a new Pod.
  local pod
  pod="$(leader_pod)"
  if [ -n "$pod" ]; then
    kubectl -n "$NS" exec "$pod" -c leader -- sh -c \
      'rmdir /var/lib/mooncake/tenant-quota-policy.yaml 2>/dev/null || true' >/dev/null 2>&1 || true
  fi

  # The finalizer criterion 5 wedges on purpose. Left in place the namespace delete below hangs on
  # it forever, and the next run of this case inherits a Terminating namespace.
  for ns in "$NS_A" "$NS_B"; do
    kubectl -n "$ns" get kvcachepoolbindings.worker.gpustack.ai -o name 2>/dev/null \
      | while read -r b; do
      kubectl -n "$ns" patch "$b" --subresource=status --type=merge \
        -p '{"status":{"usedBy":null}}' >/dev/null 2>&1 || true
    done
  done

  kubectl delete namespace "$NS_A" "$NS_B" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" "$EMPTY_POOL" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" "$EMPTY_BACKEND" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap restore EXIT

# wait_for polls a jsonpath until it equals what is wanted. It reports the LAST value seen, so a
# timeout says what the object was doing rather than only that it timed out.
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

echo "== 1. a multi-tenancy master reaches Ready with no pool bound to it =="

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

# Asserted BEFORE the pool exists, and that order is the assertion. With no pool there is no policy
# ConfigMap, so the seed volume mounts empty and the init container falls back to an empty document.
# Creating the pool first would write the ConfigMap and the fallback would never run.
if last="$(wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 180)"; then
  record PASS "master ready with no pool bound" \
    "the seed fell back to an empty policy document, the path a first install always takes"
else
  record FAIL "master ready with no pool bound" "phase stuck at '${last:-<none>}' after 180s"
fi

POD="$(leader_pod)"
if [ -z "$POD" ]; then
  record FAIL "leader pod found" "no pod carries the backend's label; the rest cannot run"
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | tr '|' ' '; done
  exit 1
fi

# A rendered Deployment proves the field; only a real image proves the command. A failure here is a
# CrashLoopBackOff on an init container nobody was watching.
init_state="$(kubectl -n "$NS" get pod "$POD" \
  -o jsonpath='{.status.initContainerStatuses[?(@.name=="seed-tenant-quota-policy")].state.terminated.reason}' 2>/dev/null)"
if [ "$init_state" = "Completed" ]; then
  record PASS "the init container ran in the real image" "seed-tenant-quota-policy: Completed"
else
  record FAIL "the init container ran in the real image" \
    "terminated reason is '${init_state:-<none>}', not Completed"
fi

echo "== 2. two namespaces, one pool =="

# The ceilings are SIZED FROM the capacity the MASTER reports, not written down and not derived
# here. Criterion 2 only tests anything when the ceilings OVERSUBSCRIBE that capacity. Fixed 5Gi + 3Gi
# did not: on a four-node cluster they sum to exactly the 8Gi available, so every grant equals its
# ceiling and the proportional reduction under test never happens while the case still reports PASS;
# on five nodes or more the grants cannot reach capacity at all and the case fails a correct operator.
#
# And the capacity is READ, never computed as members x capacityPerMember. That product is this
# case's own arithmetic, and asserting the master's apportionment against it is asserting a formula
# against itself: nothing here guarantees a DRAM segment's allocatable equals its declared capacity
# verbatim, so a per-segment reservation, an alignment or a rounding by the master would fail the
# exact expectations below against a CORRECT operator and master. The figure comes from
# mooncake_tenant_quota_allocatable_capacity_bytes — the same gauge observeAllocatableCapacity reads
# to decide the pool's own CapacityAllocatable — so the divisor under test is the master's.
#
# Sizing from `unit = allocatable / 6` keeps the ratio at exactly 5:3 and the sum at 8/6 of capacity,
# so oversubscription is guaranteed at ANY capacity. On the three-node shape this case was first
# measured on it reproduces the original numbers exactly: unit=1024Mi, ceilings 5120Mi and 3072Mi
# against 6144Mi allocatable.
#
# Each ceiling stays WITHIN the pool's own total, which is the only legal shape: admission refuses a
# Binding asking for more than the pool declares, so the oversubscription has to come from the SUM. A
# single ceiling above the total fails at apply, for a reason that has nothing to do with criterion 2.
if ! member_pods="$(kubectl -n "$NS" get pod \
  -l "app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=member-0" \
  --field-selector=status.phase=Running \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>&1)"; then
  echo "[case-43] FATAL: could not list member pods, so the ceilings cannot be sized:" >&2
  echo "$member_pods" | head -3 >&2
  exit 1
fi
MEMBERS="$(printf '%s' "$member_pods" | grep -c . || true)"
if [ "$MEMBERS" -lt 1 ]; then
  echo "[case-43] FATAL: the backend is Ready but no member pod is Running; nothing to apportion" >&2
  exit 1
fi

# Fed to the container's shell on stdin rather than assembled into `sh -c "..."`: the probe embeds a
# python program, and every layer of nesting is another round of quoting to get wrong silently. The
# image is whatever the suite was pointed at, so no single http client can be assumed — but a missing
# one is a hard stop, never a fallback to the product above. Falling back would put the assumption
# back in the divisor while the comment above says it was removed.
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

# Retried, because a member Pod that is Running has not necessarily finished mounting its segment:
# the gauge exists and reads 0 until it has. Zero is not a capacity to apportion, so it is retried
# rather than divided by.
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
  echo "[case-43] FATAL: the master reported no allocatable capacity within 120s." >&2
  echo "[case-43] last probe output: $(printf '%s' "${probe_out:-<none>}" | tr '\n' ' ' | cut -c1-200)" >&2
  echo "[case-43] the ceilings are sized from that figure; deriving one here instead would test" >&2
  echo "[case-43] this case's arithmetic against the master's rather than the operator's behaviour." >&2
  exit 1
fi

ALLOC_MI=$(( ALLOC_BYTES / 1048576 ))
UNIT_MI=$(( ALLOC_MI / 6 ))
CEIL_A_MI=$(( UNIT_MI * 5 ))
CEIL_B_MI=$(( UNIT_MI * 3 ))
if [ "$UNIT_MI" -lt 1 ]; then
  echo "[case-43] FATAL: the master reports ${ALLOC_BYTES} bytes allocatable, too little to split 5:3" >&2
  exit 1
fi
echo "[case-43] ${MEMBERS} member(s); master reports ${ALLOC_MI}Mi allocatable; ceilings ${CEIL_A_MI}Mi + ${CEIL_B_MI}Mi"

kubectl create namespace "$NS_A" >/dev/null 2>&1 || true
kubectl create namespace "$NS_B" >/dev/null 2>&1 || true

# The pool is the subject of every check below and the two bindings both point at it, so its apply is
# read back too. Refused with the output discarded, the binding gate a few lines down would still
# pass — a Binding naming an absent pool is accepted — and the convergence wait after it would report
# a binding that did not become Ready, which is true and blames the wrong object.
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
  echo "[case-43] FATAL: no kvcachepool/${POOL} after the apply, so every check below has no" >&2
  echo "                 subject. The apply said:" >&2
  printf '%s\n' "${pool_out:-<no output at all>}" >&2
  exit 1
fi

# TWO DOCUMENTS, AND BOTH BINDINGS ARE READ BACK RATHER THAN THE OUTPUT BEING DISCARDED. `kubectl
# apply` reports both in ONE stream, so a run where bind-a was created and bind-b refused is
# indistinguishable from a clean one with the output thrown away — and the check right below would
# then wait 180s for an object nobody made and report it as a binding that did not converge.
#
# The gate is the objects' EXISTENCE, not the words the apply printed. Counting ` created` lines
# instead makes the gate depend on how they came to be there: a re-run over a surviving namespace, or
# the retrying kubectl shim resending an apply whose response was lost, both report
# `configured`/`unchanged` for objects that are present and correct. The apply output is still
# captured, because a refusal's text is the only diagnosis of why an object is missing.
bind_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-a, namespace: ${NS_A}}
spec:
  poolRef: {name: ${POOL}}
  quotaCeiling: ${CEIL_A_MI}Mi
  domain: {name: ${DOM_A}, blockSize: 16, dtype: bfloat16}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-b, namespace: ${NS_B}}
spec:
  poolRef: {name: ${POOL}}
  quotaCeiling: ${CEIL_B_MI}Mi
  domain: {name: ${DOM_B}, blockSize: 16, dtype: bfloat16}
YAML
)"
# Split with parameter expansion rather than `set --`, which would clobber the script's own
# positional parameters. Nothing reads them after this point today, so the difference is a footgun
# rather than a bug -- but it is one a later wrapper passing arguments would step on silently.
bind_missing=""
for pair in "${NS_A} bind-a" "${NS_B} bind-b"; do
  bind_ns="${pair%% *}"
  bind_name="${pair##* }"
  kubectl -n "$bind_ns" get kvcachepoolbindings.worker.gpustack.ai "$bind_name" -o name >/dev/null 2>&1 \
    || bind_missing="${bind_missing} ${bind_ns}/${bind_name}"
done
if [ -n "$bind_missing" ]; then
  echo "[case-43] FATAL: absent after the apply:${bind_missing} — the convergence check below has" >&2
  echo "                 no subject and would blame the operator for their absence. The apply said:" >&2
  printf '%s\n' "${bind_out:-<no output at all>}" >&2
  exit 1
fi

ok=true
for pair in "${NS_A} bind-a" "${NS_B} bind-b"; do
  set -- $pair
  wait_for kvcachepoolbindings.worker.gpustack.ai "$2" '{.status.phase}' Ready 180 "$1" >/dev/null || ok=false
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

echo "== 3. the ceiling is a request; the grant falls in proportion =="

# The arithmetic is the MASTER'S. The operator writes each requested figure verbatim into the
# ledger and echoes back what the ledger returns; nothing here divides anything. A red run means the
# master changed how it apportions, and the first thing to check is its version — not this operator.
req_a="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a -o jsonpath='{.status.requestedQuota}' 2>/dev/null)"
eff_a="$(kubectl -n "$NS_A" get kvcachepoolbindings.worker.gpustack.ai bind-a -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"
eff_b="$(kubectl -n "$NS_B" get kvcachepoolbindings.worker.gpustack.ai bind-b -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"

if [ -n "$req_a" ] && [ -n "$eff_a" ] && [ -n "$eff_b" ]; then
  record PASS "both figures are published" "requested=${req_a} effective=${eff_a}"
else
  record FAIL "both figures are published" \
    "requested='${req_a:-<absent>}' effectiveA='${eff_a:-<absent>}' effectiveB='${eff_b:-<absent>}'"
fi

# The expectation follows from the ceilings section 2 SIZED, so it holds at whatever capacity the
# master reported. Those ceilings are 5:3 by construction and sum to 8/6 of allocatable, so the master
# divides capacity in that same 5:3 — 5/8 and 3/8 of it. The 1Mi tolerance covers BOTH roundings that
# are not this operator's to pin: the master's own apportionment, and this case truncating the
# master's byte figure to whole MiB. The sum is checked to the same tolerance and not for equality,
# because a capacity that is not a whole number of MiB cannot add back up to one exactly.
#
# Both the ratio AND the sum are asserted, and the sum is actually computed: a pass on the ratio
# alone would survive an apportionment that handed out more than the master has. The reduction is
# also asserted to have HAPPENED — a grant equal to its ceiling means nothing was oversubscribed and
# criterion 2 tested nothing, which is precisely how the fixed 5Gi+3Gi shape passed vacuously on a
# four-node cluster.
mib() { # Quantity -> integer MiB; empty/unparseable -> empty, never 0
  case "$1" in
    *Gi) echo $(( ${1%Gi} * 1024 )) ;;
    *Mi) echo "${1%Mi}" ;;
    *Ki) echo $(( ${1%Ki} / 1024 )) ;;
    ''|*[!0-9]*) echo "" ;;
    *) echo $(( $1 / 1048576 )) ;;
  esac
}

{
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
      "ceilings ${CEIL_A_MI}+${CEIL_B_MI}Mi do not oversubscribe ${alloc}Mi, so nothing is under test"
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
      "master reports ${alloc}Mi allocatable over ${MEMBERS} member(s); got ${a_mib}Mi and ${b_mib}Mi (sum $(( a_mib + b_mib ))Mi), wanted ${want_a}Mi and ${want_b}Mi summing to ${alloc}Mi"
  fi
}

echo "== 4. the backend's own usedBy carries an empty-namespace claim, exactly once =="

claim="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{range .status.usedBy[*]}{.kind}|{.namespace}|{.name} {end}' 2>/dev/null)"
if [ "$claim" = "KVCachePool||${POOL} " ]; then
  record PASS "the pool claims its backend" \
    "kind=KVCachePool namespace=<empty> name=${POOL}; the empty namespace is a value, not an absence"
else
  record FAIL "the pool claims its backend" "usedBy is '${claim:-<empty>}'"
fi

# A converging pass must not append a second identical entry. The list is keyed on all three fields
# and a duplicate would be a schema violation as well as an accounting one, so this is read back
# after another reconcile rather than only once.
sleep 35
claim_again="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{range .status.usedBy[*]}{.kind}|{.namespace}|{.name} {end}' 2>/dev/null)"
if [ "$claim_again" = "$claim" ]; then
  record PASS "a second pass writes no second claim" "still exactly one entry after a resync"
else
  record FAIL "a second pass writes no second claim" "was '${claim}', now '${claim_again}'"
fi

echo "== 5. a reuse domain belongs to one binding, cluster-wide =="

# Applied into a THIRD namespace, so this proves the check is cluster-wide rather than per-namespace.
dup_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: bind-dup, namespace: ${NS_B}}
spec:
  poolRef: {name: ${POOL}}
  quotaCeiling: 1Gi
  domain: {name: ${DOM_A}, blockSize: 16, dtype: bfloat16}
YAML
)"
if echo "$dup_out" | grep -q "already registered by ${NS_A}/bind-a"; then
  record PASS "a duplicate reuse domain is refused, naming the holder" \
    "admission names ${NS_A}/bind-a, which is what an operator needs to go and look at"
else
  record FAIL "a duplicate reuse domain is refused, naming the holder" \
    "apply said: $(echo "$dup_out" | tr '\n' ' ' | cut -c1-160)"
fi

echo "== 6. a binding a workload holds cannot be deleted =="

# WRITTEN BY THIS CASE. No controller in this repository fills a Binding's usedBy: the kind that
# will is the model-deployment spec's, which has not been built. A case that waited for a writer
# would wait forever, and one that skipped the write would assert a release nothing was holding.
kubectl -n "$NS_A" patch kvcachepoolbindings.worker.gpustack.ai bind-a --subresource=status --type=merge \
  -p '{"status":{"usedBy":[{"kind":"ModelDeployment","namespace":"","name":"qwen-e2e"}]}}' >/dev/null 2>&1

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

# Released by hand, since nothing else will: this is the same patch a real consumer's controller
# would make when it stops using the pool.
kubectl -n "$NS_A" patch kvcachepoolbindings.worker.gpustack.ai bind-a --subresource=status --type=merge \
  -p '{"status":{"usedBy":null}}' >/dev/null 2>&1
# Deliberately NOT wait_for with an empty expected value. That helper reads kubectl under
# 2>/dev/null and compares the output, so a transport error, an RBAC denial or a webhook intercepting
# the GET yields the same empty string a deleted object does — and with an empty `want` the FIRST
# iteration matches, meaning a failed query reports "gone" instantly rather than even waiting. This is
# the one check in the case whose PASS is an ABSENCE, so absence has to be the API server's own word:
# a non-zero exit carrying NotFound. Any other failure keeps waiting and is reported with its message.
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

echo "== 7. a master without a tenant ledger fails loudly =="

# Turned off UNDER the bound pool, never started that way: admission refuses a pool whose backend
# runs without multi-tenancy, so a backend started that way would never acquire the pool that
# reports the Condition. F5 calls this a runtime observation for exactly this reason.
kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type=merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":false}}}}}' >/dev/null 2>&1

mt_reason=""
for _ in $(seq 1 40); do
  mt_reason="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaLedgerAvailable")].reason}' 2>/dev/null)"
  [ "$mt_reason" = "MultiTenancyDisabled" ] && break
  sleep 3
done
pool_phase="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$mt_reason" = "MultiTenancyDisabled" ] && [ "$pool_phase" != "Ready" ]; then
  record PASS "multi-tenancy off raises its own reason and holds the pool back" \
    "QuotaLedgerAvailable=False MultiTenancyDisabled, phase=${pool_phase}"
else
  record FAIL "multi-tenancy off raises its own reason and holds the pool back" \
    "reason='${mt_reason:-<none>}' phase='${pool_phase:-<none>}'"
fi

# The recovery is a RECORDED outcome, not a silenced one. Section 8 asks whether an unwritable
# policy source is reported as such, and it can only answer that from a working ledger: left as
# `|| true`, a pool that never came back would make section 8 fail for a reason that has nothing to
# do with a policy source, and the run would be red at the wrong line. Recorded here, the two rows
# are read together.
if ! patch_out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type=merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":true}}}}}' 2>&1)"; then
  record FAIL "multi-tenancy is restored for the sections that follow" \
    "re-enabling it failed: $(printf '%s' "$patch_out" | head -1 | cut -c1-80)"
elif last="$(wait_for kvcachepools.worker.gpustack.ai "$POOL" '{.status.phase}' Ready 180)"; then
  record PASS "multi-tenancy is restored for the sections that follow" \
    "the pool is Ready again, so section 8 starts from a working ledger"
else
  record FAIL "multi-tenancy is restored for the sections that follow" \
    "the pool is stuck at '${last:-<none>}' after 180s — whatever section 8 reports below is not \
about a policy source"
fi

echo "== 8. a policy source the master cannot write fails loudly =="

# A directory in place of the file. A chmod does not hold: the master writes through a temp file and
# a rename, and it runs as a non-root user that cannot chmod the directory the file sits in. The
# directory blocks both the truncating write and the rename.
POD="$(leader_pod)"
kubectl -n "$NS" exec "$POD" -c leader -- sh -c \
  'rm -f /var/lib/mooncake/tenant-quota-policy.yaml && mkdir /var/lib/mooncake/tenant-quota-policy.yaml' \
  >/dev/null 2>&1

# Forces a write: a changed ceiling is a PUT the master must persist.
kubectl -n "$NS_B" patch kvcachepoolbindings.worker.gpustack.ai bind-b --type=merge \
  -p '{"spec":{"quotaCeiling":"2Gi"}}' >/dev/null 2>&1

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

# Undone here as well as in the trap. The leader must not restart while the directory stands — the
# emptyDir outlives the container, so the master would fail to parse it and crash-loop with no way
# back — and leaving it in place for the rest of the run widens that window for nothing.
kubectl -n "$NS" exec "$POD" -c leader -- sh -c \
  'rmdir /var/lib/mooncake/tenant-quota-policy.yaml 2>/dev/null || true' >/dev/null 2>&1 || true

# ASSERTED, not assumed, and this is the one repair in the case whose failure would otherwise be
# invisible: nothing restarts the leader afterwards and the backend is deleted soon after, so a
# botched repair costs whoever runs next rather than this run.
#
# The check is `test -f`, which rejects BOTH states that break a restart — the directory still
# standing, and nothing at the path at all. The second is the real trap: this section removed the
# file before making the directory, so an rmdir on its own leaves the path empty, and a leader
# restarted onto an empty path fails to load exactly as it would onto a directory. What puts the
# file back is the master's own next successful write, through the same rename it always uses, so a
# readable file proves the directory is gone AND that writes work again.
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
    "no regular file at the policy path after 90s — a leader restart from here crash-loops"
fi

echo "== 9. a backend with nothing mounted is not a pool with an empty cache =="

# The startup-ordering trap: every effective quota is zero and no write can succeed, while every
# object still looks correctly configured. The member selector matches no node on purpose.
# Read back for the same reason as the bindings above: the backend and the pool arrive in one stream,
# and the only check in this section reads the POOL. Created the backend and refused the pool, and the
# poll below would spend its whole window on an absent object and then report a missing condition as
# the operator's silence — which is the exact substitution this section exists to rule out.
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
  echo "[case-43] FATAL: absent after the apply:${empty_missing} — the check below has no subject" >&2
  echo "                 and would read an absence as a missing condition. The apply said:" >&2
  printf '%s\n' "${empty_out:-<no output at all>}" >&2
  exit 1
fi

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

# Results.
echo
echo "STATUS | CHECK | OBJECT"
# Split on the delimiter `record` actually wrote, not on whitespace. Nearly every CHECK name here is
# multi-word, and flattening the `|` first made awk tokenize on spaces: the CHECK column collapsed to
# its first word and the rest of the name leaked into OBJECT.
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-43] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-43] all checks passed"
