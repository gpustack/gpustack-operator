#!/usr/bin/env bash
#
# CASE 77 — The multi-tenant ledger gate: an unregistered tenant's put is refused with -1701 until
#            a Pool+Binding registers its domain, and the registered put round-trips
#   (MUTATING, self-recovering)
#
#   case-77.sh <NS>
#
# Goal:        A multi-tenant master admits writes only from tenants present in its ledger, and the
#              ledger is fed by the operator: a KVCachePoolBinding whose domain.name IS the tenant
#              id is what registers one. This case proves both ends of that contract with the same
#              client, the same key and the same tenant id: before any Pool or Binding exists the
#              put is refused with TENANT_NOT_REGISTERED (-1701) while the client itself is healthy
#              (setup returns 0, and the miss reads back as an empty result, not an exception);
#              after the Pool and its Binding are Ready, the identical put succeeds and the exact
#              payload reads back. This is the admission contract the injection path depends on --
#              an injected engine that writes under a domain nobody registered fails exactly this
#              way, so the case guards the gate rather than any one engine.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). <NS> must be the
#              operator's system namespace. The store image (E2E_MOONCAKE_IMAGE, default pinned to
#              a CPU-capable tag carrying both the lease backend and the mooncake python client) is
#              also the probe pod's image, so the client library always matches the master. The
#              backend MUST be a replicated HA leader: the client reaches the master through
#              k8s://<ns>/<backend>-leader, which resolves through the election's Lease, and the
#              probe's own authorization rides the member Role the operator renders only above one
#              replica -- a single-replica leader renders neither the Lease nor the Role, and the
#              client fails at setup instead of exercising the gate (measured; it is a shape
#              constraint, not a defect).
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (3-replica HA leader, multi-tenancy
#              on, no snapshot, no Pool, no Binding at first); a tenant namespace holding a probe
#              Pod on the store image plus a RoleBinding -- in <NS> -- to the backend's rendered
#              member Role (get leases, the least the k8s:// path needs); the probe runs a small
#              python client via kubectl exec. THE TENANT RIDES THE KEYWORD ARGUMENT tenant_id=:
#              in the python binding the next positional slot after the master address is a
#              TransferEngine pointer, and passing the tenant there raises instead of registering
#              it -- every setup call below passes tenant_id= by name. After the refused half, the
#              Pool and the Binding are created with a retry (admission may reject the first
#              create for a few seconds while the backend's own objects settle), the domain name
#              being exactly the tenant id the client already used.
#
# Expected:    - the backend reaches Ready with the election rendered: the member Role and the
#              Lease both exist (the k8s:// prerequisites);
#              - unregistered: the client's setup returns 0 against the k8s:// master address, and
#                the put returns -1701 (TENANT_NOT_REGISTERED), the get an empty miss (len=-1);
#              - the Pool reaches Ready and the Binding reaches Ready, its domain name being the
#                tenant id;
#              - registered: the same client's put returns 0 and the exact payload reads back;
#              - teardown drain: the client's remove of the key reports removed, retrying past
#                OBJECT_HAS_LEASE (-706) within a deadline and counting OBJECT_NOT_FOUND (-704)
#                as removed -- a domain that still holds objects holds the Pool's deletion
#                open-ended (measured: the Pool sits in Deleting until the domain drains), so the
#                case drains what it wrote. Graded on one call this row failed on a lease that had
#                not expired yet, which is the timing and not the drain.
#
# Cleanup:     Trap deletes the Binding, the Pool, the backend, the RoleBinding in <NS> and the
#              tenant namespace, in that order, all without waiting (a held deletion must not hang
#              the trap; the drain row above is what keeps the ordered deletes flowing). Idempotent,
#              runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-77.sh <NS>}"
CASE_ID=77
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend and the pool are cluster-scoped, and
# the Binding claims its domain per master, so a collision would poison every later run's setup.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-mt-${SFX}"
POOL="kvcp-mt-${SFX}"
TEST_NS="kvc-mt-${SFX}"
BINDING="bind-mt-${SFX}"
DOMAIN="dom-mt-${SFX}"
KEY="mt-${SFX}"
PROBE="kvcb-mt-${SFX}-probe"
ROLEBINDING="kvcb-mt-${SFX}"
MASTER_URI="k8s://${NS}/${BACKEND}-leader"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed"
  return 0
}

CLIENT_SCRIPT="$(mktemp)"
teardown() {
  echo
  echo "[case-77] cleanup"
  # Best-effort drain FIRST and only when the probe still runs: a put that succeeded but was
  # never followed by the drain row (a failure between the two) leaves the domain holding the
  # key, and the Pool below would sit in Deleting indefinitely. A second remove of a missing
  # key is harmless; a probe that is gone skips the attempt, and the deletes flow regardless.
  if kubectl -n "$TEST_NS" get pod "$PROBE" >/dev/null 2>&1; then
    kubectl -n "$TEST_NS" exec "$PROBE" --request-timeout=30s -- env MASTER_URI="$MASTER_URI" TENANT_ID="$DOMAIN" \
      python3 /tmp/client.py remove "$KEY" >/dev/null 2>&1 || true
  fi
  kubectl -n "$TEST_NS" delete kvcachepoolbindings.worker.gpustack.ai "$BINDING" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete rolebindings.rbac.authorization.k8s.io "$ROLEBINDING" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -f "$CLIENT_SCRIPT"
}
trap teardown EXIT

# The probe's client. One script, three actions (put / remove), so the refused half, the admitted
# half and the teardown drain all run byte-identical client code: the only thing that changes
# between the two puts is the ledger, which is the case's whole point. The tenant rides the
# KEYWORD tenant_id= -- see the header for why the positional slot cannot carry it.
cat >"$CLIENT_SCRIPT" <<'PY'
import os, sys, time, uuid
from mooncake.store import MooncakeDistributedStore

action, key = sys.argv[1], sys.argv[2]
s = MooncakeDistributedStore()
rc = s.setup(os.environ.get("POD_IP", "127.0.0.1"), "P2PHANDSHAKE", 0, 134217728, "tcp", "",
             os.environ["MASTER_URI"], tenant_id=os.environ["TENANT_ID"])
print("SETUP rc=%d" % rc, flush=True)
if rc != 0:
    sys.exit(0)
if action == "put":
    val = ("%s-%s" % (key, uuid.uuid4())).encode()
    print("PUTVAL %s" % val.decode(), flush=True)
    print("PUT rc=%d" % s.put(key, val), flush=True)
    got = s.get(key)
    print("GET match=%s len=%d" % (got == val, len(got) if got else -1), flush=True)
elif action == "remove":
    # Retried past OBJECT_HAS_LEASE (-706) rather than graded on one call. The same read that lets
    # the ledger refuse an unregistered tenant also holds the object until its lease expires, and
    # that TTL is a master startup parameter, so one attempt grades the timing instead of the drain.
    # OBJECT_NOT_FOUND (-704) counts as removed: already gone is the outcome this row wants.
    # The deadline is what keeps a master that never releases from hanging the case instead of
    # returning a verdict.
    deadline, attempts, rc = time.time() + 60, 0, -1
    while time.time() < deadline:
        attempts += 1
        rc = s.remove(key)
        if rc != -706:
            break
        time.sleep(3)
    print("REMOVE rc=%d attempts=%d" % (rc, attempts), flush=True)
PY

# wait_for polls one jsonpath until it equals what is wanted, and prints the LAST value seen, so a
# timeout says what the object was doing rather than only that it timed out. The optional 6th
# argument overrides the namespace, for the tenant-side objects.
wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-180}" ns="${6:-$NS}"
  local got="" i
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl -n "$ns" get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

# client_run execs one action in the probe pod and echoes the client's marked lines. The client's
# own logging is noisy (glog to stderr); the marked lines are the contract.
client_run() {
  local action="$1"
  kubectl -n "$TEST_NS" exec "$PROBE" -- env MASTER_URI="$MASTER_URI" TENANT_ID="$DOMAIN" \
    python3 /tmp/client.py "$action" "$KEY" 2>&1 | /usr/bin/grep -E '^(SETUP|PUTVAL|PUT|GET|REMOVE)'
}

client_field() {
  printf '%s\n' "$1" | sed -n "s/^$2 //p" | head -1
}

# ------------------------------------------------------------- the backend and its election

kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        replicas: 3
        multiTenancy: true
        highAvailability: {}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend reaches Ready" \
    "phase never became Ready in 300s; nothing below can run: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  results; exit 1
fi

# The k8s:// prerequisites, asserted rather than assumed: the member Role exists (the probe's
# authorization) and the Lease has a holder (the master address resolves through it). A
# single-replica leader renders neither, and the failure mode is a client that cannot even setup.
HOLDER=""
for ((i = 0; i < 120; i += 3)); do
  HOLDER="$(kubectl -n "$NS" get leases.coordination.k8s.io "${BACKEND}-leader" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)"
  [ -n "$HOLDER" ] && break
  sleep 3
done
if wait_for roles.rbac.authorization.k8s.io "${BACKEND}-member" '{.metadata.name}' "${BACKEND}-member" 120 >/dev/null \
  && [ -n "$HOLDER" ]; then
  record PASS "the election is rendered (member Role + Lease with a holder)" \
    "role ${BACKEND}-member exists, lease ${BACKEND}-leader holder='${HOLDER}'"
else
  record FAIL "the election is rendered (member Role + Lease with a holder)" \
    "role or lease holder missing; a single-replica leader renders neither and the k8s:// client cannot run"
  results; exit 1
fi

# ------------------------------------------------------------- the probe and the refused half

kubectl create namespace "$TEST_NS" >/dev/null 2>&1 || true
kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${ROLEBINDING}
  namespace: ${TEST_NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${ROLEBINDING}
  namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${BACKEND}-member
subjects:
  - kind: ServiceAccount
    name: ${ROLEBINDING}
    namespace: ${TEST_NS}
---
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${TEST_NS}
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

if ! wait_for pod "$PROBE" '{.status.phase}' Running 180 "$TEST_NS" >/dev/null; then
  record FAIL "the probe pod runs" \
    "phase='$(kubectl -n "$TEST_NS" get pod "$PROBE" -o jsonpath='{.status.phase}' 2>/dev/null)'; the client below cannot run"
  results; exit 1
fi
if ! kubectl -n "$TEST_NS" cp "$CLIENT_SCRIPT" "$PROBE:/tmp/client.py" >/dev/null 2>&1; then
  record FAIL "the client script reaches the probe" "kubectl cp into ${PROBE} failed"
  results; exit 1
fi

# THE REFUSED HALF. No Pool, no Binding: the tenant the client presents is in no ledger, so the
# master must refuse the put by code (-1701) while the client itself stays healthy (setup=0) and
# the miss reads back empty (len=-1), not as an exception.
REFUSED_OUT="$(client_run put)"
REFUSED_SETUP="$(client_field "$REFUSED_OUT" "SETUP")"
REFUSED_PUT="$(client_field "$REFUSED_OUT" "PUT")"
REFUSED_GET="$(client_field "$REFUSED_OUT" "GET")"
if [ "$REFUSED_SETUP" = "rc=0" ] && [ "$REFUSED_PUT" = "rc=-1701" ] && [[ "$REFUSED_GET" == *"len=-1"* ]]; then
  record PASS "an unregistered tenant's put is refused with -1701" \
    "setup=${REFUSED_SETUP} put=${REFUSED_PUT} get='${REFUSED_GET}' -- the client is healthy and the miss is an empty result, so the refusal is the ledger's"
elif [ "$REFUSED_SETUP" != "rc=0" ]; then
  record FAIL "an unregistered tenant's put is refused with -1701" \
    "the client did not reach the master at all (setup='${REFUSED_SETUP}'), so nothing was asserted about the gate: $(echo "$REFUSED_OUT" | tr '\n|' '  ')"
else
  record FAIL "an unregistered tenant's put is refused with -1701" \
    "setup=${REFUSED_SETUP} put='${REFUSED_PUT}' get='${REFUSED_GET}'"
fi

# ------------------------------------------------------------- register the domain, retry the put

# Admission may reject the first create for a few seconds while the backend's rendered objects
# settle; a single rejected apply would read as a fixture defect. The retry is bounded and the
# last error, if all attempts fail, is the row's message. The if tests the apply's own status
# through the assignment, so the two cannot drift apart.
REGISTERED=0
REGISTER_ERR=""
for attempt in 1 2 3; do
  if REGISTER_ERR="$(kubectl apply -f - <<YAML 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL}
spec:
  backends:
    - ${BACKEND}
  quota:
    total: 2Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: ${DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quota:
    ceiling: 2Gi
YAML
)"; then
    REGISTERED=1
    break
  fi
  sleep 5
done

if [ "$REGISTERED" != "1" ]; then
  record FAIL "the Pool and its Binding register the domain" \
    "apply failed on all ${attempt} attempt(s): $(echo "$REGISTER_ERR" | tr '\n|' '  ' | cut -c1-200)"
  results; exit 1
fi
if wait_for kvcachepools.worker.gpustack.ai "$POOL" '{.status.phase}' Ready 180 >/dev/null \
  && wait_for kvcachepoolbindings.worker.gpustack.ai "$BINDING" '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
  record PASS "the Pool and its Binding register the domain" \
    "pool ${POOL} Ready, binding ${BINDING} Ready, domain name '${DOMAIN}' is the tenant the client presents"
else
  record FAIL "the Pool and its Binding register the domain" \
    "pool phase='$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)', binding phase='$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING" -o jsonpath='{.status.phase}' 2>/dev/null)'"
  results; exit 1
fi

# THE ADMITTED HALF. Same client, same key, same tenant id: only the ledger changed.
ADMITTED_OUT="$(client_run put)"
ADMITTED_PUT="$(client_field "$ADMITTED_OUT" "PUT")"
ADMITTED_GET="$(client_field "$ADMITTED_OUT" "GET")"
if [ "$ADMITTED_PUT" = "rc=0" ] && [[ "$ADMITTED_GET" == *"match=True"* ]]; then
  record PASS "the registered tenant's put succeeds and reads back" \
    "put=${ADMITTED_PUT} get='${ADMITTED_GET}'"
else
  record FAIL "the registered tenant's put succeeds and reads back" \
    "put='${ADMITTED_PUT}' get='${ADMITTED_GET}': $(echo "$ADMITTED_OUT" | tr '\n|' '  ')"
fi

# THE TEARDOWN DRAIN. A domain that still holds objects holds the Pool's deletion open-ended (the
# Pool sits in Deleting until the master's ledger entry goes), so the case removes what it wrote
# BEFORE the trap's deletes. The remove being the same client's call, this row also proves the
# registered tenant can manage its objects, not only create them.
DRAIN_OUT="$(client_run remove)"
DRAIN_REMOVE="$(client_field "$DRAIN_OUT" "REMOVE")"
# The client reports "rc=<code> attempts=<n>", so the verdict reads the code and the message keeps
# the count: a drain that took several passes is a pass, and one that took many says so.
# -704 is removed, not failed -- see the client for why both codes end the loop.
DRAIN_RC="${DRAIN_REMOVE%% *}"
if [ "$DRAIN_RC" = "rc=0" ] || [ "$DRAIN_RC" = "rc=-704" ]; then
  record PASS "the teardown drain removes the key (an held domain blocks pool deletion)" \
    "remove=${DRAIN_REMOVE} for key ${KEY}; the trap's ordered deletes can now flow"
else
  record FAIL "the teardown drain removes the key (an held domain blocks pool deletion)" \
    "remove='${DRAIN_REMOVE}': $(echo "$DRAIN_OUT" | tr '\n|' '  '); expect the Pool to sit in Deleting until the domain drains"
fi

results
