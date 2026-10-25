#!/usr/bin/env bash
#
# CASE 44 — What a grant really enforces (a refusal while the tenant's own objects are held, an
#           eviction of them once they are not) and the ledger gate that decides whether the tenant
#           exists at all
#   (MUTATING, self-recovering)
#
#   case-44.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_MOONCAKE_IMAGE
# Goal:        Two contracts of one multi-tenant master, proven against ONE backend, ONE probe Pod
#              and ONE client:
#                (1) WHAT A GRANT ENFORCES. A write past the grant is refused while every object
#                    holding it carries a live lease, because the master is then allowed to throw
#                    nothing away; once those leases lapse the same write is admitted and paid for
#                    out of this domain's own older objects. The grant is an eviction trigger, not
#                    an admission barrier -- which is NOT what the field names suggest, and the
#                    eviction path is separate from the store's general LRU, so it moves none of the
#                    counters the operator reads and the whole thing is silent.
#                (2) THE LEDGER GATE. A tenant the master holds no policy for is refused -1701
#                    (TENANT_NOT_REGISTERED) while the client itself stays healthy, and a Pool
#                    whose Binding names that domain as domain.name registers it -- after which the
#                    identical put succeeds. This is the contract every injected engine depends on:
#                    an engine writing under a domain nobody registered fails exactly this way, so
#                    the gate is what is guarded here, not any one engine.
#              The two halves share the fixture, so the second one starts from a ledger the first
#              one has already been talking to.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and a converged
#              KV cache chain: the backend below must reach Ready, its election must render, and a
#              Pool and its Bindings must converge before a grant can be filled. No GPU and no RDMA
#              (the member is DRAM over TCP). NEEDS a store image built with the Kubernetes Lease
#              leadership backend -- the default below carries it; override with
#              E2E_MOONCAKE_IMAGE. This case does not auto-skip: it needs the store.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (multi-tenancy on, one DRAM member of
#              2Gi, the leader's lease TTL pinned to 10s through extraArgs so a lease lapse can be
#              timed), one KVCachePool over it, TWO KVCachePoolBindings over that pool (the quota
#              half's domain with a 16Mi ceiling, the ledger half's domain with a 64Mi one -- small
#              on purpose, so the two grants stay inside the master's capacity and neither tenant
#              is scaled down), and one probe Pod on the store image carrying the client script. The
#              client is the leader's own image, which ships it.
#              THE TENANT RIDES THE KEYWORD ARGUMENT tenant_id=: in the python binding the next
#              positional slot after the master address is a TransferEngine pointer, and passing the
#              tenant there raises instead of registering it -- every setup call passes tenant_id=
#              by name.
#
# Expected:    - the backend reaches Ready with the election rendered (the member Role and the Lease
#                with a holder), and the probe Pod runs with the client script on it;
#              - the quota half: the binding reports a grant, which is what proves its tenant
#                reached the ledger; writes succeed up to the grant; with every object in the grant
#                holding a live lease the NEXT write is refused, and refused for being over quota
#                rather than for the tenant being unknown -- two different refusals, and only one
#                of them is this case's subject; the objects that caused that refusal all still read
#                back; the charged bytes are capped AT the grant rather than tracking what was
#                written; once the leases lapse a further write is admitted and older objects of the
#                same domain are gone -- recorded as the behaviour it is, not endorsed; the objects
#                can be removed and the charged bytes come back;
#              - the ledger half: the client's setup returns 0 against the master while the tenant
#                is unregistered, the put is refused -1701 and the miss reads back as an empty
#                result rather than an exception; the Pool and its Binding reach Ready with the
#                domain name being the tenant id; the identical put then returns 0 and the exact
#                payload reads back; the teardown drain removes the key.
#              If the write under lease is NOT refused, this case FAILS: the grant would then be a
#              readout with no enforcement anywhere, which is a finding about the master and belongs
#              in a person's verification notes, never softened into a passing assertion.
#
# Cleanup:     The trap drains both domains FIRST -- a domain that still holds objects makes the
#              master refuse to drop its quota, the operator correctly keeps its finalizer, and the
#              pool's deletion stays open-ended -- then removes the probe Pod, the two bindings, the
#              pool, the backend, the RoleBinding and the tenant namespace, in that order and
#              without waiting. A binding still held after 60s is reported and its finalizers are
#              forced off, so the next run starts clean. Idempotent, runs on pass AND fail, safe to
#              re-run.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-44.sh <NS>}"
# The store image, and it is also the probe Pod's image, so the client library always matches the
# master. The default carries the Kubernetes Lease leadership backend, which the election below
# needs; a published upstream image cannot win an election and the backend never becomes Ready.
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"
# The leader's read-lease TTL for this backend, well inside the drain's 60s deadline.
KV_LEASE_TTL=10s

# LC_ALL=C and the disabled pipefail are both load-bearing: under a UTF-8 locale tr dies on
# /dev/urandom, and with pipefail on the SIGPIPE from head turns a trailing `|| echo $$` into an
# append, so LC_ALL=C alone yields random characters with the PID glued on. The measurements behind
# both halves are recorded once, at the same idiom in _kvcache-inject-lib.sh.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
# The names below are cluster-scoped, so a bare PID -- small and reused across reboots -- lets two
# runs adopt each other's leftovers. The epoch makes a collision need the same PID in the same
# second.
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-q-${SFX}"
POOL="kvcp-q-${SFX}"
NS_Q="kvc-q-${SFX}"
BIND_Q="bind-q"
BIND_MT="bind-mt-${SFX}"
DOMAIN_Q="dom-q-${SFX}"
DOMAIN_MT="dom-mt-${SFX}"
KEY="mt-${SFX}"
PROBE="kvc-probe-${SFX}"
ROLEBINDING="kvcb-q-${SFX}"
# The master address the client dials. The k8s:// form resolves the elected leader through the
# Lease, and it needs the member Role the probe's RoleBinding grants.
MASTER_URI="k8s://${NS}/${BACKEND}-leader"
# The master's own metrics, read for the refusal counter the client cannot see.
METRICS_URL="http://${BACKEND}-leader.${NS}.svc:9003/metrics"

# The grant, and the writes that fill it exactly. Four 4 MiB objects come to 16 MiB with nothing left
# over -- the master charges metadata.size and adds no overhead of its own, which a first run
# confirmed by reporting charged_bytes = 16777216 on the nose. Filling it EXACTLY matters: a fill
# that already overshot would have evicted before this case got to the part it means to test.
CEILING="16Mi"
CEILING_BYTES=$((16 * 1024 * 1024))
OBJ_BYTES=$((4 * 1024 * 1024))
OBJ_COUNT=4
# 1.5x the 10s this case pins through the backend's extraArgs above. The operator renders five
# minutes, so the pinned value -- not any default -- is what this sleep is measured against.
LEASE_LAPSE=15
# The ledger half's grant. It is small on purpose: it shares the master with the quota half's
# tenant, and the master's effective grant is lower whenever the sum of every tenant's request
# exceeds the master's capacity. One small grant and one 16Mi grant both fit inside the member's
# 2Gi, so neither tenant is scaled down and the quota half's grant stays exactly what was asked
# for.
LEDGER_CEILING="64Mi"

# The keys the quota half writes, so the trap can drain the same domain if that half died before
# its own removal probe ran.
Q_KEYS=()
for i in $(seq 0 $((OBJ_COUNT - 1))); do Q_KEYS+=("${DOMAIN_Q}-obj-${i}"); done
Q_KEYS+=("${DOMAIN_Q}-over-0")
for i in 0 1 2; do Q_KEYS+=("${DOMAIN_Q}-late-${i}"); done

# count_lines exists because `grep -c … || echo 0` APPENDS a zero rather than substituting one: grep -c
# already prints 0 when it matches nothing, and exits 1 while doing so, so the two together produce
# "0\n0" and every arithmetic test downstream rejects it as "integer expected".
count_lines() {
  local n
  n="$(grep -c "$1" "$2" 2>/dev/null)" || n=0
  echo "$n"
}

CLIENT_SCRIPT="$(mktemp)"

restore() {
  echo
  echo "[case-44] cleanup"
  # The drain comes FIRST, and it covers BOTH domains. A domain that still holds objects makes the
  # master refuse to drop its quota, the operator correctly keeps its finalizer, and the pool's
  # deletion below would then sit in Deleting forever. The quota half may have died before its own
  # removal probe ran, so the trap cannot rely on that probe having drained anything.
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
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete rolebindings.rbac.authorization.k8s.io "$ROLEBINDING" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete namespace "$NS_Q" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  # A binding whose domain still holds objects is held by its finalizer ON PURPOSE -- the master
  # refuses to drop a non-empty tenant's quota, and the operator is right to wait. That is correct
  # behaviour and a broken cleanup at the same time, so the shared helper reports what held it before
  # forcing the issue. Reporting rather than silently forcing: a run that got here left objects
  # behind, and the assertions above have already recorded that as a failure.
  kvi_release_binding_finalizer "$NS_Q" "$BIND_Q"
  kvi_release_binding_finalizer "$NS_Q" "$BIND_MT"
}
trap restore EXIT

# The client's own logging is noisy (glog to stderr); the marked lines are the contract. One filter
# for every action, so a line a row greps for is never dropped by the reader.
CLIENT_TAGS='^(SETUP|IMPORT-FAIL|SETUP-TYPEERROR|SETUP-FAIL|FILL|LEASED|OVER|INTACT|REJECTMETRIC|LAPSE|LATE|SURVIVED|PENDING|REMOVE|LEFT|PUTVAL|PUT|GET)'

# The probe's client. ONE script, four actions, so the quota half, the ledger half and the teardown
# drain all run byte-identical client code. The tenant rides the KEYWORD tenant_id= -- see the
# header for why the positional slot cannot carry it.
cat >"$CLIENT_SCRIPT" <<'PY'
import os, sys, time, uuid, socket, urllib.request

action = sys.argv[1]
args = sys.argv[2:]

try:
    from mooncake.store import MooncakeDistributedStore
except Exception as e:
    print("IMPORT-FAIL %s" % e); sys.exit(0)

store = MooncakeDistributedStore()

# The local hostname is what the peer dials BACK on under a peer-to-peer handshake, so it has to be
# an address reachable from inside the cluster; a loopback name leaves the master unable to answer.
# The local buffer is what the client stages data through, and a zero there is not "use the
# default" -- a client with no buffer cannot move a multi-megabyte object.
ip = os.environ.get("POD_IP") or socket.gethostbyname(socket.gethostname())
try:
    rc = store.setup(ip, "P2PHANDSHAKE", 0, 134217728, "tcp", "",
                     os.environ["MASTER_URI"], tenant_id=os.environ["TENANT_ID"])
except TypeError as e:
    # A probe that cannot fail is worth more than a probe that explains.
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
    # Retried past OBJECT_HAS_LEASE (-706) rather than graded on one call. The same read that lets
    # the ledger refuse an unregistered tenant also holds the object until its lease expires, and
    # that TTL is a master startup parameter, so one attempt grades the timing instead of the drain.
    # OBJECT_NOT_FOUND (-704) counts as removed: already gone is the outcome this row wants. The
    # deadline is what keeps a master that never releases from hanging the case instead of
    # returning a verdict.
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

    # Exactly to the grant, never over it. An overshoot here would evict before the lease is taken
    # and leave the refusal below with nothing to prove.
    for i in range(count):
        print("FILL %s-obj-%d rc=%d" % (domain, i, store.put("%s-obj-%d" % (domain, i), payload)))

    # The lease is the whole mechanism. A GET makes the object un-evictable for the 10s this case
    # pinned, so for that window the grant has nothing it is allowed to throw away and has to
    # refuse instead.
    for i in range(count):
        got = store.get("%s-obj-%d" % (domain, i))
        print("LEASED %s-obj-%d len=%d" % (domain, i, len(got) if got else -1))

    print("OVER %s-over-0 rc=%d" % (domain, store.put("%s-over-0" % domain, payload)))

    # Asserted per object, and asserted AFTER the refusal rather than inferred from it: the claim is
    # that the refusal cost nothing, and a store that had quietly dropped one to try to make room
    # would still have refused.
    for i in range(count):
        got = store.get("%s-obj-%d" % (domain, i))
        print("INTACT %s-obj-%d len=%d" % (domain, i, len(got) if got else -1))

    # The master's own account of that refusal, read here rather than inferred from the client's
    # return code, so the two are independent witnesses to the same event. This counter is
    # incremented ONLY on the branch that gives up after the last eviction retry -- it counts real
    # refusals, not overshoots -- which is what makes "exactly one" a meaningful reading. The
    # store's four general eviction counters stay at zero throughout, because the quota eviction is
    # not on that path; a case that watched them would conclude nothing had happened.
    try:
        body = urllib.request.urlopen(os.environ["METRICS_URL"], timeout=10).read().decode()
        hit = [l.strip() for l in body.splitlines()
               if l.startswith("mooncake_tenant_quota_reject_total") and domain in l]
        print("REJECTMETRIC %s" % (hit[0] if hit else "<no sample for this tenant>"))
    except Exception as e:
        print("REJECTMETRIC-FAIL %s" % e)

    # The other half. With every lease lapsed the same write that was just refused goes through,
    # and it is paid for out of this domain's own older objects. Printed as facts rather than
    # judged here.
    print("LAPSE sleeping %ss for the leases to expire" % os.environ["LEASE_LAPSE"])
    time.sleep(int(os.environ["LEASE_LAPSE"]))
    for i in range(3):
        print("LATE %s-late-%d rc=%d" % (domain, i, store.put("%s-late-%d" % (domain, i), payload)))
    for i in range(count):
        print("SURVIVED %s-obj-%d exists=%d" % (domain, i, 1 if store.is_exist("%s-obj-%d" % (domain, i)) else 0))
elif action == "drain":
    keys = list(args)

    # Retried on -706 rather than slept through blindly: the lease TTL is a master startup
    # parameter, so a fixed sleep here would be a number that silently stops being right.
    #
    # -704 OBJECT_NOT_FOUND counts as done, not as a failure. Half these keys are EXPECTED to be
    # gone by now -- the overflow write was refused and never existed, and the originals were
    # evicted to pay for the late writes. Treating "already absent" as a removal failure would make
    # this probe red on exactly the run where the case worked.
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
                # leaving it out of `still` dropped the key from the loop for good.
                print("REMOVE-FAIL %s rc=%d" % (key, rc))
                still.append(key)
        pending = still
        if pending:
            time.sleep(3)
    for key in pending:
        print("REMOVE %s rc=-706-gave-up" % key)
    print("LEFT %d" % len([k for k in keys if store.is_exist(k)]))
PY

# client_run execs one action in the probe Pod and echoes the client's marked lines. The tenant and
# the master's address ride the environment, so the same action runs against either domain.
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

echo "== 1. the backend, its election and the probe =="

kubectl create namespace "$NS_Q" >/dev/null 2>&1 || true

# One backend for both halves. Multi-tenancy is on, the election runs through a Kubernetes Lease
# (the member Role and the Lease are what the ledger half's k8s:// client path needs), and the
# lease TTL is pinned through extraArgs: the operator renders five minutes, and this case times a
# lease lapse.
kubectl apply -f - >/dev/null 2>&1 <<YAML
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
        replicas: 3  # retains case-77's HA election shape beside the tenant gate
        electionBackend: Kubernetes
        multiTenancy: true
        # This case times a lease lapse, so it pins the lease rather than inheriting whatever
        # default the operator renders. Pinning it through extraArgs is also the only place on a
        # real cluster where the override path itself runs: the operator renders this same flag and
        # deliberately leaves the key off its derived list, so a value here has to reach the master
        # and win. If a later change reserves the key, admission refuses this object and the case
        # fails at creation rather than in an assertion -- which is the right place for that
        # breakage to appear.
        extraArgs:
          - -default_kv_lease_ttl=${KV_LEASE_TTL}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

# ONE wait for ONE backend, asserted under BOTH absorbed labels: the quota half and the ledger half
# each used to bring up a backend of their own, and each asserted that it was Ready.
backend_ready=0
if wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  backend_ready=1
fi
if [ "$backend_ready" = 1 ]; then
  record PASS "backend ready" "the master reached Ready in 300s; both halves have a subject"
else
  record FAIL "backend ready" "the master did not reach Ready in 300s; nothing below can run"
fi
if [ "$backend_ready" = 1 ]; then
  record PASS "the backend reaches Ready" \
    "phase Ready in 300s; the one backend both halves talk to"
else
  record FAIL "the backend reaches Ready" \
    "phase never became Ready in 300s; nothing below can run: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  print_rows
  exit 1
fi

# The k8s:// prerequisites, asserted rather than assumed: the member Role exists (the probe's
# authorization) and the Lease has a holder (the master address resolves through it). A missing
# grant or holder prevents the client from reaching the elected master.
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
    "role or lease holder missing; the k8s:// client cannot run"
  print_rows
  exit 1
fi

# The probe. It carries the client script for every action below, and it reaches the master through
# the member Role, which the RoleBinding in the operator's namespace grants.
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
  name: ${BACKEND}-member
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
    "phase='$(kubectl -n "$NS_Q" get pod "$PROBE" -o jsonpath='{.status.phase}' 2>/dev/null)'; the client below cannot run"
  print_rows
  exit 1
fi
record PASS "the probe pod runs" "phase=Running; the client script is copied onto it next"

if ! kubectl -n "$NS_Q" cp "$CLIENT_SCRIPT" "$PROBE:/tmp/client.py" >/dev/null 2>&1; then
  record FAIL "the client script reaches the probe" "kubectl cp into ${PROBE} failed"
  print_rows
  exit 1
fi
record PASS "the client script reaches the probe" "the one client script is on ${PROBE} and serves both halves"

echo "== 2. what an effective quota enforces =="

# TWO DOCUMENTS, AND BOTH OBJECTS ARE READ BACK RATHER THAN THE OUTPUT BEING DISCARDED. `kubectl
# apply` reports the pool and the binding in ONE stream, so with the output thrown away a run that
# created the pool and had the binding refused looks exactly like a clean one -- and the grant check
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
    total: 2Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: ${BIND_Q}, namespace: ${NS_Q}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: ${CEILING}}
  domain: {name: ${DOMAIN_Q}, blockSize: 16, dtype: bfloat16}
YAML
)"
missing=""
kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o name >/dev/null 2>&1 \
  || missing="${missing} kvcachepool/${POOL}"
kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" -o name >/dev/null 2>&1 \
  || missing="${missing} ${NS_Q}/kvcachepoolbinding/${BIND_Q}"
if [ -n "$missing" ]; then
  record FAIL "the pool and its binding exist" \
    "absent after the apply:${missing} -- so nothing below has a subject. The apply said: \
$(printf '%s' "${apply_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
else
  record PASS "the pool and its binding exist" \
    "pool ${POOL} and binding ${BIND_Q} are both there, so the grant below has a subject"
fi

# A failed precondition ends THIS SECTION, not the case: the ledger half below still has a subject
# of its own, and its rows are not this half's misconfiguration. The case still exits non-zero.
if [ -z "$missing" ]; then
  wait_for kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" '{.status.phase}' Ready 180 "$NS_Q" >/dev/null || true

  # The grant's PRESENCE is the precondition for everything below, and asserting it is not a
  # formality. A write to a tenant the master holds no policy for is refused TENANT_NOT_REGISTERED
  # -- the very same refusal a domain that was never declared gets, and NOT a quota verdict.
  # Without this check a case whose policy never reached the ledger would read its own
  # misconfiguration as the enforcement it set out to prove.
  grant="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" \
    -o jsonpath='{.status.effectiveQuota}' 2>/dev/null)"
  if [ "$grant" = "$CEILING" ]; then
    record PASS "the tenant reached the ledger" \
      "effectiveQuota=${grant}; a write refused below is therefore a quota verdict, not an unknown tenant"
  else
    record FAIL "the tenant reached the ledger" \
      "effectiveQuota is '${grant:-<absent>}', wanted ${CEILING}; the byte arithmetic below assumes the \
grant is exactly what was asked for"
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
      "${filled} of ${OBJ_COUNT} writes accepted; the grant was never filled, so a refusal below would \
not be a quota verdict"
  fi

  leased_ok="$(count_lines 'LEASED .* len=[0-9]' "$QUOTA_LOG")"
  if [ "$leased_ok" -eq "$OBJ_COUNT" ]; then
    record PASS "every object in the grant takes a lease" \
      "${leased_ok} read back, so none of them can be evicted for the next ~10s"
  else
    record FAIL "every object in the grant takes a lease" \
      "${leased_ok} of ${OBJ_COUNT} read back; an object that is not leased is one the master may evict, \
and the refusal below would then never come"
  fi

  # THE SUBJECT OF THIS HALF. Under lease the master has nothing it is allowed to throw away, so its
  # three eviction attempts all come up empty and the write finally fails with
  # TENANT_QUOTA_EXCEEDED = -1700. A success here means the grant is not enforced ANYWHERE, which is
  # a finding to write up, never an assertion to soften.
  if grep -q 'OVER .* rc=-1700' "$QUOTA_LOG" 2>/dev/null; then
    record PASS "a write past the grant is refused when nothing can be evicted" \
      "rc=-1700 TENANT_QUOTA_EXCEEDED: the grant is a barrier while its objects are leased"
  elif grep -q 'OVER .* rc=0' "$QUOTA_LOG" 2>/dev/null; then
    record FAIL "a write past the grant is refused when nothing can be evicted" \
      "the write was ACCEPTED past a ${CEILING} grant whose every object held a live lease. The master \
had nothing it was allowed to evict and admitted the write anyway: the grant is not enforced at all. \
Record it in the verification notes, do not soften it"
  else
    record FAIL "a write past the grant is refused when nothing can be evicted" \
      "$(grep -m1 'OVER ' "$QUOTA_LOG" | cut -c1-140 || echo 'no OVER line in the log')"
  fi

  # A quota refusal and an unregistered tenant are different failures with different fixes, and only
  # the first is this half's subject. -1701 here would mean the policy never reached the ledger.
  if grep -q "rc=-1701" "$QUOTA_LOG" 2>/dev/null; then
    record FAIL "the refusal is about the quota, not the tenant" \
      "a write returned -1701 TENANT_NOT_REGISTERED -- the tenant has no policy, so nothing here \
tested a quota"
  else
    record PASS "the refusal is about the quota, not the tenant" "no TENANT_NOT_REGISTERED in the run"
  fi

  # A SECOND, independent witness to the same refusal. The client's -1700 is what the caller saw;
  # this is what the master recorded. They can disagree -- a transport that lost the reply, a
  # client that mapped the code itself -- and a case that only ever asks one side cannot tell.
  rejects="$(grep -m1 '^REJECTMETRIC mooncake_tenant_quota_reject_total' "$QUOTA_LOG" 2>/dev/null \
    | awk '{print int($NF)}')"
  if [ "${rejects:-x}" = "1" ]; then
    record PASS "the master recorded exactly one refusal" \
      "mooncake_tenant_quota_reject_total{reason=\"quota_exceeded\"}=1, which is the write above and \
nothing else: no earlier write was quietly refused and retried"
  else
    record FAIL "the master recorded exactly one refusal" \
      "$(grep -m1 '^REJECTMETRIC' "$QUOTA_LOG" | cut -c1-160 || echo 'the probe printed no REJECTMETRIC line') \
-- wanted exactly 1"
  fi

  intact_ok="$(count_lines 'INTACT .* len=[0-9]' "$QUOTA_LOG")"
  if [ "$intact_ok" -eq "$OBJ_COUNT" ]; then
    record PASS "the objects that caused the refusal are all still there" \
      "${intact_ok} of ${OBJ_COUNT} read back after the refusal: refusing cost nothing"
  else
    record FAIL "the objects that caused the refusal are all still there" \
      "${intact_ok} of ${OBJ_COUNT} read back after the refusal; the master gave something up while \
refusing anyway"
  fi

  echo "== 2b. the charge is capped at the grant, not at what was written =="

  # Seven objects of 4 MiB each went in over the course of the probe, and the domain is granted four
  # of them. Reading exactly the grant back is what shows the charge tracks what is HELD, not what
  # was offered -- and it is also the "before" that gives the reclaim assertion at the end its
  # meaning: a charge returning to zero proves nothing on a run where nothing was ever charged.
  usage_held=""
  for _ in $(seq 1 20); do
    usage_held="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" \
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

  echo "== 2c. once the leases lapse, the grant admits by evicting this domain's own objects =="

  # NOT an endorsement. This records the behaviour the field names would never suggest, so that the
  # day the master changes it this case says so instead of quietly passing. A FAIL here is as likely
  # to mean "upstream fixed it" as "upstream broke it" -- either way the verification notes need
  # rewriting, and that is exactly what an assertion is for.
  late_ok="$(count_lines 'LATE .* rc=0' "$QUOTA_LOG")"
  survived="$(count_lines 'SURVIVED .* exists=1' "$QUOTA_LOG")"
  if [ "$late_ok" -eq 3 ] && [ "$survived" -lt "$OBJ_COUNT" ]; then
    record PASS "the lapsed grant admits by evicting this domain's own objects" \
      "${late_ok} further write(s) admitted and only ${survived} of the original ${OBJ_COUNT} survive: \
the grant is an eviction trigger once its objects are evictable, not an admission barrier"
  elif [ "$late_ok" -eq 3 ]; then
    record FAIL "the lapsed grant admits by evicting this domain's own objects" \
      "${late_ok} write(s) admitted yet all ${OBJ_COUNT} originals survive -- the domain now holds more \
than its grant, which no path in the master should allow"
  else
    record FAIL "the lapsed grant admits by evicting this domain's own objects" \
      "only ${late_ok} of 3 writes admitted after the leases lapsed; if the master now refuses instead \
of evicting, the behaviour recorded in the verification notes is out of date and must be rewritten"
  fi

  echo "== 2d. removing the objects gives the bytes back =="

  # A SECOND action, on purpose -- see the charge reading above. Removal is retried rather than
  # attempted once: an object read within the last lease window is refused OBJECT_HAS_LEASE = -706,
  # and the first version of this case took that single refusal as final. The domain then still held
  # objects, the master refused to drop its quota, the operator correctly kept its finalizer, and
  # the namespace was left Terminating on every single run.
  RMLOG="/tmp/kvc-probe-rm-${SFX}.log"
  client_run drain "$DOMAIN_Q" "${Q_KEYS[@]}" >"$RMLOG"
  sed -n '1,40p' "$RMLOG" 2>/dev/null | sed 's/^/    /'

  if grep -q '^LEFT 0' "$RMLOG" 2>/dev/null; then
    record PASS "every object can be removed" \
      "$(count_lines '^REMOVE ' "$RMLOG") removal(s) issued, none left holding the domain open"
  else
    record FAIL "every object can be removed" \
      "$(grep -m1 '^LEFT ' "$RMLOG" | cut -c1-60 || echo 'the removal probe printed no LEFT line') \
-- a domain that still holds objects makes the master refuse to drop its quota, and the binding's \
finalizer will hold correctly and forever"
  fi

  # Reclaimable, not merely spent. The charge falling is what separates a quota from a lifetime
  # allowance, and the capped reading above is what lets this one say so.
  usage_after=""
  for _ in $(seq 1 30); do
    usage_after="$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_Q" \
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
fi

echo "== 3. the multi-tenant ledger gate =="

# THE REFUSED HALF. This tenant is in no ledger: the quota half above registered its own domain,
# and this one is a different tenant. The master must refuse the put by code (-1701) while the
# client itself stays healthy (setup=0) and the miss reads back empty (len=-1), not as an exception.
REFUSED_OUT="$(client_run put "$DOMAIN_MT" "$KEY")"
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

# Register the domain: a Pool already exists, and the Binding's domain name IS the tenant id the
# client presents. Admission may reject the first create for a few seconds while the backend's own
# objects settle; a single rejected apply would read as a fixture defect. The retry is bounded and
# the last error, if all attempts fail, is the row's message. The if tests the apply's own status
# through the assignment, so the two cannot drift apart.
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
    name: ${POOL}
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
elif wait_for kvcachepools.worker.gpustack.ai "$POOL" '{.status.phase}' Ready 180 >/dev/null \
  && wait_for kvcachepoolbindings.worker.gpustack.ai "$BIND_MT" '{.status.phase}' Ready 180 "$NS_Q" >/dev/null; then
  record PASS "the Pool and its Binding register the domain" \
    "pool ${POOL} Ready, binding ${BIND_MT} Ready, domain name '${DOMAIN_MT}' is the tenant the client presents"
  LEDGER_OK=1
else
  record FAIL "the Pool and its Binding register the domain" \
    "pool phase='$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)', binding phase='$(kubectl -n "$NS_Q" get kvcachepoolbindings.worker.gpustack.ai "$BIND_MT" -o jsonpath='{.status.phase}' 2>/dev/null)'"
fi

# A failed registration ends THIS SECTION, not the case: there is no registered tenant to write
# with, so the two rows below have no subject and are not emitted. The case still exits non-zero.
if [ "$LEDGER_OK" = 1 ]; then
  # THE ADMITTED HALF. Same client, same key, same tenant id: only the ledger changed.
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

  # THE TEARDOWN DRAIN. A domain that still holds objects holds the Pool's deletion open-ended (the
  # Pool sits in Deleting until the master's ledger entry goes), so the case removes what it wrote
  # BEFORE the trap's deletes. The remove being the same client's call, this row also proves the
  # registered tenant can manage its objects, not only create them.
  DRAIN_OUT="$(client_run remove "$DOMAIN_MT" "$KEY")"
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
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-44] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-44] all checks passed"
