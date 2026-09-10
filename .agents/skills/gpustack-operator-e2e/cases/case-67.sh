#!/usr/bin/env bash
#
# CASE 67 — A drain between two store members pinned to TWO different nodes moves the bytes its
#   report claims, and a disk-resident key is neither moved nor counted anywhere   (MUTATING,
#   self-recovering; AUTO-SKIPS without two schedulable nodes)
#
#   case-67.sh <NS>
#
# Goal:        The store's drain job API promises to move a member's data to a named peer before
#              the member goes away, and a shipped API doc comment rests on two claims about it:
#              the memory replica really moves, and a disk-resident key is skipped without being
#              counted, so the job reports success over a tier it left untouched. Both claims had
#              only ever been measured with both members in one docker network on one host -- the
#              assertion the feature exists for is about two machines, and a single host carries
#              zero information about it. This case pins the two members to two different nodes,
#              asserts that placement from the Pod objects (never assumed from the pinning),
#              drains, and reads the data back AT THE TARGET with the source member deleted and
#              forgotten by the master. The job report alone is never the verdict; the verdict is
#              where the bytes read back from.
#
# Environment: Any cluster with >= 2 Ready schedulable nodes, no GPU and no RDMA needed (the
#              members are DRAM over TCP). AUTO-SKIPS (exit 0) with fewer than two. No operator
#              objects are created: the drain API this case exercises is the store's own HTTP
#              surface, nothing in the operator calls it, and the operator-rendered member
#              configuration cannot exercise the disk half (it sets neither the allocation
#              strategy that makes a write land on a named member nor the bucket thresholds the
#              tier stores anything under), so the fixture is three pinned Pods carrying the
#              shipped member environment key for key plus the two deviations the single-host
#              measurement already recorded (the bucket thresholds, and the tier on a writable
#              volume). Needs a registry the nodes can pull the store image from
#              (E2E_MOONCAKE_IMAGE; the default is the published upstream image this behavior
#              was measured on).
#
# Inputs:      All real, nothing mocked. One master and two store members as bare Pods, each
#              member pinned to its own node by spec.nodeName; member A additionally carries a
#              disk tier on an emptyDir. Probe Pods (the store image's python3, plain HTTP)
#              write 4MiB objects of known content through member A, drive the master's drain
#              job API, and read keys back through member B.
#
# Expected:    Placement:
#              - member A and member B run on DIFFERENT nodes, read from their Pod objects.
#              Phase A, memory-resident keys:
#              - the drain job reaches SUCCEEDED with failed=0, succeeded_units equal to the
#                number of written objects, and migrated_bytes equal to their total size;
#              - the source segment reads used=0 afterwards and the target grew by exactly the
#                migrated bytes;
#              - with member A's Pod deleted and its segment dropped by the master, every
#                written key still reads back through B, byte-identical.
#              Phase B, disk-resident keys (KNOWN-FAILURE DETECTOR, green today by design):
#              - after writes past the source's memory force eviction, at least 4 keys are
#                confirmed served from the disk tier (per-read file-cache-hit delta, read
#                through member A itself: served from the tier file without leaving a
#                migratable memory replica, which a read through B can leave behind);
#              - the drain job again reaches SUCCEEDED with failed=0 and blocked=0, migrates at
#                least the two witnesses (they read back through B with the source gone,
#                byte-identical; extras are memory keys the removal could not name, and the
#                byte counter must agree with the unit counter), and its counters do not sum to
#                the keys the source held: succeeded+failed+blocked+active is strictly less
#                than tier-resident plus the memory keys measured on the source's own segment
#                before the drain -- never from the report under test;
#              - every tier-resident key reads 404 through B with the source gone. A 200 there
#                means the drain has started reaching the tier -- the day this detector goes
#                red, the skipped-tier premise (and the scale-in policy resting on it) has to
#                be revisited.
#
# Cleanup:     Trap deletes every Pod this case created (store and probes) by label, and the
#              namespace too if and only if this run created it. Idempotent, runs on pass AND
#              fail, safe to re-run; concurrent runs carry distinct suffixes.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the store.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-67.sh <NS>}"
CASE_ID=67
IMAGE="${E2E_MOONCAKE_IMAGE:-kvcacheai/mooncake:0.3.13}"

OBJ_BYTES=4194304
A_SEGMENT=67108864
B_SEGMENT=268435456
BUFFER_BYTES=67108864
TIER_LIMIT=268435456
BUCKET_KEYS=1
BUCKET_BYTES=16777216

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
P_MASTER="case67-master-${SFX}"
P_A="case67-a-${SFX}"
P_B="case67-b-${SFX}"
LABEL_KEY="gpustack-e2e-case"
LABEL_VAL="67-${SFX}"

FAILS=0
SKIPS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); [ "$1" = SKIP ] && SKIPS=$((SKIPS + 1)); return 0; }

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed (${SKIPS} skipped)"
  return 0
}

ready_workers() {
  kubectl get nodes -l '!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master' \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"|"}{.spec.unschedulable}{"\n"}{end}' \
    2>/dev/null | awk -F'|' '$2=="True" && $3!="true" {print $1}' | sort
}

CREATED_NS=0
teardown() {
  echo
  echo "[case-67] cleanup"
  kubectl -n "$NS" delete pod -l "${LABEL_KEY}=${LABEL_VAL}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [ "$CREATED_NS" = "1" ]; then
    kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  rm -f "${LOGA1:-}" "${LOGA2:-}" "${LOGB1:-}" "${LOGB2:-}"
}
trap teardown EXIT

wait_ready() { # wait_ready <pod> <seconds>
  kubectl -n "$NS" wait --for=condition=Ready "pod/$1" --timeout="$2s" >/dev/null 2>&1
}

pod_state() { # pod_state <pod> -- a one-line triage hint for a pod that never became Ready
  kubectl -n "$NS" get pod "$1" \
    -o jsonpath='{.status.phase} {.status.containerStatuses[0].state} {.status.containerStatuses[0].lastState}' \
    2>/dev/null | cut -c1-200
}

# check_reads <log> <csv-keys> <want-status> [want-sha]
# Prints "BAD TOTAL" on stdout; per-key mismatches on stderr. The per-key READ-KEY lines the probe
# printed are the instrument; this only counts them against the expectation it is handed.
check_reads() {
  local log="$1" keys="$2" want_st="$3" want_sha="${4:-}"
  local k line bad=0 total=0
  while IFS= read -r k; do
    [ -n "$k" ] || continue
    total=$((total + 1))
    line="$(grep -E "^READ-KEY key=${k} " "$log" | tail -1)"
    if [ -z "$line" ]; then
      echo "  MISSING no read line for $k" >&2
      bad=$((bad + 1))
      continue
    fi
    if ! grep -qE "status=${want_st}( |$)" <<<"$line"; then
      echo "  UNEXPECTED $line (want status=${want_st})" >&2
      bad=$((bad + 1))
      continue
    fi
    if [ -n "$want_sha" ] && ! grep -q "sha256=${want_sha}" <<<"$line"; then
      echo "  DIGEST-MISMATCH $line" >&2
      bad=$((bad + 1))
      continue
    fi
  done < <(printf '%s\n' "$keys" | tr ',' '\n')
  printf '%d %d\n' "$bad" "$total"
}

# run_probe <name-suffix> <log-file> -- the python program arrives on stdin
run_probe() {
  kubectl -n "$NS" run "case67-probe-$1-${SFX}" --restart=Never --rm -i --quiet \
    --image="$IMAGE" --labels="${LABEL_KEY}=${LABEL_VAL}" \
    --overrides="{\"spec\":{\"nodeName\":\"${NODE_A}\",\"containers\":[{\"name\":\"probe\",\"image\":\"${IMAGE}\",\"command\":[\"python3\",\"-\"],\"stdin\":true,\"stdinOnce\":true}]}}" \
    >"$2" 2>&1 || true
}

# ------------------------------------------------- 0. two nodes, or this case says nothing

NODES="$(ready_workers)"
NNODES="$(printf '%s\n' "$NODES" | sed '/^$/d' | wc -l | tr -d ' ')"
if [ "$NNODES" -lt 2 ]; then
  echo "[case-67] SKIP: needs >= 2 Ready schedulable nodes, found ${NNODES}"
  exit 0
fi
NODE_A="$(printf '%s\n' "$NODES" | sed -n 1p)"
NODE_B="$(printf '%s\n' "$NODES" | sed -n 2p)"
echo "[case-67] member A node: ${NODE_A}"
echo "[case-67] member B node: ${NODE_B}"

if ! kubectl get namespace "$NS" >/dev/null 2>&1; then
  # Only a create this run succeeded at makes the namespace this run's own: claiming it on a
  # failed create (quota, RBAC, a race with a concurrent case that won the create) would have
  # teardown delete a namespace another run is using.
  if kubectl create namespace "$NS" >/dev/null; then
    CREATED_NS=1
  fi
fi

# ------------------------------------------------- 1. the store: one master, two pinned members

kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${P_MASTER}
  namespace: ${NS}
  labels:
    ${LABEL_KEY}: ${LABEL_VAL}
    case67-role: master
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
    - name: master
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["mooncake_master"]
      args:
        - "-rpc_port=50051"
        - "-metrics_port=9003"
        - "-enable_offload=true"
        - "-offload_on_evict=true"
        - "-allocation_strategy=local_first"
      env:
        - {name: GLOG_logtostderr, value: "1"}
      resources:
        requests: {memory: 256Mi}
        limits: {memory: 1Gi}
      readinessProbe:
        httpGet: {path: /health, port: 9003}
        initialDelaySeconds: 2
        periodSeconds: 3
YAML

# The first pull of the store image on a node can take minutes; later pods on the same node reuse it.
if ! wait_ready "$P_MASTER" 900; then
  record FAIL "the master pod becomes Ready" "$(pod_state "$P_MASTER")"
  results; exit 1
fi
MASTER_IP="$(kubectl -n "$NS" get pod "$P_MASTER" -o jsonpath='{.status.podIP}')"
record PASS "the master pod becomes Ready" "${P_MASTER} at ${MASTER_IP}"

# Member A carries the disk tier. The store runs as uid 65532 and an emptyDir is root-owned, so the
# pod sets fsGroup to the store's gid -- the same deviation from a docker volume the single-host
# fixture recorded for its tmpfs. Phase B re-creates the identical pod after the phase A drain, so
# the manifest lives in one function: two copies would drift.
apply_member_a() {
  kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${P_A}
  namespace: ${NS}
  labels:
    ${LABEL_KEY}: ${LABEL_VAL}
    case67-role: member
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  securityContext:
    fsGroup: 65532
  containers:
    - name: member
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["mc_store_rest_server"]
      env:
        - {name: MOONCAKE_TE_META_DATA_SERVER, value: "P2PHANDSHAKE"}
        - {name: MOONCAKE_MASTER, value: "${MASTER_IP}:50051"}
        - {name: MOONCAKE_PROTOCOL, value: "tcp"}
        - {name: MOONCAKE_LOCAL_HOSTNAME, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
        - {name: MOONCAKE_GLOBAL_SEGMENT_SIZE, value: "${A_SEGMENT}"}
        - {name: MOONCAKE_LOCAL_BUFFER_SIZE, value: "${BUFFER_BYTES}"}
        - {name: MOONCAKE_OFFLOAD_ENABLED, value: "true"}
        - {name: MOONCAKE_OFFLOAD_FILE_STORAGE_PATH, value: "/tier"}
        - {name: MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES, value: "${TIER_LIMIT}"}
        - {name: MOONCAKE_OFFLOAD_BUCKET_KEYS_LIMIT, value: "${BUCKET_KEYS}"}
        - {name: MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES, value: "${BUCKET_BYTES}"}
        - {name: GLOG_logtostderr, value: "1"}
      volumeMounts:
        - {name: tier, mountPath: /tier}
      resources:
        requests: {memory: 512Mi}
        limits: {memory: 2Gi}
      readinessProbe:
        httpGet: {path: /api/exist/never-written, port: 8080}
        initialDelaySeconds: 2
        periodSeconds: 3
  volumes:
    - name: tier
      emptyDir: {sizeLimit: 1Gi}
YAML
}

apply_member_a

kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${P_B}
  namespace: ${NS}
  labels:
    ${LABEL_KEY}: ${LABEL_VAL}
    case67-role: member
spec:
  nodeName: ${NODE_B}
  restartPolicy: Never
  containers:
    - name: member
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["mc_store_rest_server"]
      env:
        - {name: MOONCAKE_TE_META_DATA_SERVER, value: "P2PHANDSHAKE"}
        - {name: MOONCAKE_MASTER, value: "${MASTER_IP}:50051"}
        - {name: MOONCAKE_PROTOCOL, value: "tcp"}
        - {name: MOONCAKE_LOCAL_HOSTNAME, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
        - {name: MOONCAKE_GLOBAL_SEGMENT_SIZE, value: "${B_SEGMENT}"}
        - {name: MOONCAKE_LOCAL_BUFFER_SIZE, value: "${BUFFER_BYTES}"}
        - {name: GLOG_logtostderr, value: "1"}
      resources:
        requests: {memory: 512Mi}
        limits: {memory: 2Gi}
      readinessProbe:
        httpGet: {path: /api/exist/never-written, port: 8080}
        initialDelaySeconds: 2
        periodSeconds: 3
YAML

if ! wait_ready "$P_A" 300; then
  record FAIL "member A becomes Ready" "$(pod_state "$P_A")"
  results; exit 1
fi
if ! wait_ready "$P_B" 900; then
  record FAIL "member B becomes Ready" "$(pod_state "$P_B")"
  results; exit 1
fi
A_IP="$(kubectl -n "$NS" get pod "$P_A" -o jsonpath='{.status.podIP}')"
B_IP="$(kubectl -n "$NS" get pod "$P_B" -o jsonpath='{.status.podIP}')"
record PASS "both store members become Ready" "${P_A} at ${A_IP}, ${P_B} at ${B_IP}"

# THE core placement assertion: read from the Pod objects, never assumed from the pinning. A
# same-node run of this case carries zero information and must not be allowed to read as a pass.
ACT_NODE_A="$(kubectl -n "$NS" get pod "$P_A" -o jsonpath='{.spec.nodeName}')"
ACT_NODE_B="$(kubectl -n "$NS" get pod "$P_B" -o jsonpath='{.spec.nodeName}')"
kubectl -n "$NS" get pod "$P_A" "$P_B" -o custom-columns='POD:.metadata.name,NODE:.spec.nodeName' --no-headers
if [ -n "$ACT_NODE_A" ] && [ -n "$ACT_NODE_B" ] && [ "$ACT_NODE_A" != "$ACT_NODE_B" ]; then
  record PASS "the two store members run on two different nodes" "${P_A} on ${ACT_NODE_A}, ${P_B} on ${ACT_NODE_B}"
else
  record FAIL "the two store members run on two different nodes" \
    "${P_A} on '${ACT_NODE_A}', ${P_B} on '${ACT_NODE_B}' -- a same-node run says nothing about data crossing machines"
  results; exit 1
fi

# ------------------------------------------------- 2. phase A: memory-resident keys cross the nodes

LOGA1="$(mktemp)"; LOGA2="$(mktemp)"; LOGB1="$(mktemp)"; LOGB2="$(mktemp)"

run_probe a-fill "$LOGA1" <<PY
import json, sys, time, hashlib, urllib.error, urllib.request

MASTER = "${MASTER_IP}:9003"
A = "${A_IP}:8080"
B = "${B_IP}:8080"
A_IP = "${A_IP}"
B_IP = "${B_IP}"
OBJ = ${OBJ_BYTES}

def http(method, url, body=None, timeout=60):
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return -1, str(e).encode()

def segments():
    st, body = http("GET", "http://%s/get_segments_detail" % MASTER)
    if st != 200:
        raise RuntimeError("segment listing failed: %d %r" % (st, body[:200]))
    return json.loads(body).get("segments", [])

def seg_of(ip):
    for s in segments():
        if s.get("segment_name", "").startswith(ip + ":"):
            return s
    return None

def free_bytes(seg):
    return seg["allocator_capacity_bytes"] - seg["allocator_used_bytes"]

payload = "x" * OBJ
digest = hashlib.sha256(payload.encode()).hexdigest()

# Fill the source segment and stop there, so nothing spills onto the target and the target keeps
# the room the drain needs. With the master on local_first, a put through member A lands in A.
# The a- prefix keeps phase A's keys out of phase B's d-/p- namespace: after the drain these
# keys live in B's memory, and a same-named phase B key would make the disk phase's 404 verdict
# answerable by a stale phase A replica instead of by the tier's fate.
keys = []
for i in range(60):
    key = "a-%03d" % i
    st, _ = http("PUT", "http://%s/api/put" % A, json.dumps({"key": key, "value": payload}).encode())
    if st != 200:
        print("WRITE refused at i=%d status=%d" % (i, st))
        break
    keys.append(key)
    src = seg_of(A_IP)
    if src and free_bytes(src) < OBJ:
        print("WRITE stopping: source free=%d, under one object" % free_bytes(src))
        break
print("WRITE accepted=%d" % len(keys))
if len(keys) < 8:
    print("SETUP-FAILED only %d objects fit the source segment" % len(keys))
    sys.exit(1)
subject = keys
print("SUBJECT %s" % ",".join(subject))

a_seg, b_seg = seg_of(A_IP), seg_of(B_IP)
if not a_seg or not b_seg:
    print("SETUP-FAILED both segments have to be listed")
    sys.exit(1)
print("SEGMENTS source=%s used=%d free=%d" % (a_seg["segment_name"], a_seg["allocator_used_bytes"], free_bytes(a_seg)))
print("SEGMENTS target=%s used=%d free=%d" % (b_seg["segment_name"], b_seg["allocator_used_bytes"], free_bytes(b_seg)))
print("TARGET used_before=%d" % b_seg["allocator_used_bytes"])
if free_bytes(b_seg) < len(subject) * OBJ:
    print("SETUP-FAILED target free=%d, under the %d the subject needs" % (free_bytes(b_seg), len(subject) * OBJ))
    sys.exit(1)

body = json.dumps({"segments": [a_seg["segment_name"]],
                   "target_segments": [b_seg["segment_name"]],
                   "max_concurrency": 4}).encode()
st, resp = http("POST", "http://%s/api/v1/drain_jobs" % MASTER, body, timeout=60)
text = resp.decode("utf-8", "replace")
print("DRAIN-CREATE status=%d body=%s" % (st, text[:400]))
if st != 200:
    print("DRAIN-CREATE-REJECTED nothing below carries information")
    sys.exit(1)
job_id = (json.loads(text) or {}).get("job_id")
print("DRAIN-JOB-ID %s" % job_id)
if not job_id:
    print("SETUP-FAILED the create response carried no job_id")
    sys.exit(1)

# The terminal state is read from the PARSED status_name; a substring match over the body matched
# the field name "failed_units" in an earlier instrument and broke on the first poll.
terminal = {"DRAINED", "COMPLETED", "FAILED", "CANCELLED", "CANCELED", "SUCCEEDED"}
doc, state = {}, None
for i in range(90):
    st, q = http("GET", "http://%s/api/v1/drain_jobs/query?job_id=%s" % (MASTER, job_id), timeout=30)
    last = q.decode("utf-8", "replace")
    try:
        doc = json.loads(last)
    except Exception:
        doc = {}
    state = doc.get("status_name")
    print("DRAIN-QUERY t=%ds http=%d state=%s succeeded=%s failed=%s blocked=%s active=%s migrated_bytes=%s" % (
        i * 2, st, state, doc.get("succeeded_units"), doc.get("failed_units"),
        doc.get("blocked_units"), doc.get("active_units"), doc.get("migrated_bytes")), flush=True)
    if st == 200 and state in terminal:
        break
    time.sleep(2)
print("DRAIN-TERMINAL-STATE %s" % state)

def num(d, k):
    v = d.get(k)
    return v if isinstance(v, int) else -1

print("FINAL succeeded=%d failed=%d blocked=%d active=%d migrated_bytes=%d" % (
    num(doc, "succeeded_units"), num(doc, "failed_units"), num(doc, "blocked_units"),
    num(doc, "active_units"), num(doc, "migrated_bytes")))

a_after, b_after = seg_of(A_IP), seg_of(B_IP)
print("AFTER source_used=%d source_status=%s" % (
    a_after["allocator_used_bytes"] if a_after else -1, a_after.get("status") if a_after else "MISSING"))
print("AFTER target_used=%d" % (b_after["allocator_used_bytes"] if b_after else -1))

# The control reads: with the source up they would pass whether the bytes moved or not, since the
# source can still serve them. They only prove the keys existed.
for key in subject[:5]:
    st, rbody = http("GET", "http://%s/api/get/%s" % (B, key), timeout=30)
    print("READ-SOURCE-UP key=%s status=%d bytes=%d" % (key, st, len(rbody) if st == 200 else -1))
print("WANT sha256=%s bytes=%d" % (digest, OBJ))
print("PROBE-A-FILL-DONE")
PY

echo "----- phase A fill/drain probe log -----"
cat "$LOGA1"
echo "----------------------------------------"

if grep -q "SETUP-FAILED" "$LOGA1"; then
  record FAIL "phase A preconditions hold" "$(grep 'SETUP-FAILED' "$LOGA1" | head -1)"
  results; exit 1
fi

ACC="$(sed -n 's/^WRITE accepted=\([0-9]*\).*/\1/p' "$LOGA1" | tail -1)"
SUBJECT_A="$(sed -n 's/^SUBJECT //p' "$LOGA1" | tail -1)"
WANT_SHA="$(sed -n 's/^WANT sha256=\([0-9a-f]*\).*/\1/p' "$LOGA1" | tail -1)"
TERM_A="$(sed -n 's/^DRAIN-TERMINAL-STATE //p' "$LOGA1" | tail -1)"
FINAL_A="$(sed -n 's/^FINAL //p' "$LOGA1" | tail -1)"
F_SUCC="$(printf '%s' "$FINAL_A" | sed -n 's/^succeeded=\([0-9-]*\).*/\1/p')"
F_FAIL="$(printf '%s' "$FINAL_A" | sed -n 's/.*failed=\([0-9-]*\).*/\1/p')"
F_BLOCK="$(printf '%s' "$FINAL_A" | sed -n 's/.*blocked=\([0-9-]*\).*/\1/p')"
F_MIG="$(printf '%s' "$FINAL_A" | sed -n 's/.*migrated_bytes=\([0-9-]*\).*/\1/p')"
SRC_USED="$(sed -n 's/^AFTER source_used=\([0-9-]*\).*/\1/p' "$LOGA1" | tail -1)"
TGT_BEFORE="$(sed -n 's/^TARGET used_before=\([0-9-]*\).*/\1/p' "$LOGA1" | tail -1)"
TGT_AFTER="$(sed -n 's/^AFTER target_used=\([0-9-]*\).*/\1/p' "$LOGA1" | tail -1)"

if grep -q "DRAIN-CREATE status=200" "$LOGA1"; then
  record PASS "the drain job is accepted" "$(grep '^DRAIN-CREATE' "$LOGA1" | head -1 | cut -c1-120)"
else
  record FAIL "the drain job is accepted" "$(grep -E '^DRAIN-CREATE' "$LOGA1" | head -1 | cut -c1-160)"
  results; exit 1
fi

if [ "$TERM_A" = "SUCCEEDED" ]; then
  record PASS "the drain job reaches SUCCEEDED" "terminal=${TERM_A}"
else
  record FAIL "the drain job reaches SUCCEEDED" "terminal='${TERM_A}' ($FINAL_A)"
  results; exit 1
fi

if [ "$F_FAIL" = "0" ] && [ "$F_BLOCK" = "0" ]; then
  record PASS "no unit failed or stayed blocked" "$FINAL_A"
else
  record FAIL "no unit failed or stayed blocked" "$FINAL_A"
fi

if [ -n "$ACC" ] && [ "$F_SUCC" = "$ACC" ]; then
  record PASS "succeeded_units equals the number of written objects" "succeeded=${F_SUCC} written=${ACC}"
else
  record FAIL "succeeded_units equals the number of written objects" "succeeded=${F_SUCC} written=${ACC}"
fi

if [ -n "$ACC" ] && [ -n "$F_MIG" ] && [ "$F_MIG" = "$((ACC * OBJ_BYTES))" ]; then
  record PASS "migrated_bytes equals the total size of the written objects" "migrated_bytes=${F_MIG} for ${ACC} objects of ${OBJ_BYTES}"
else
  record FAIL "migrated_bytes equals the total size of the written objects" "migrated_bytes=${F_MIG}, want ${ACC} objects * ${OBJ_BYTES}"
fi

if [ "$SRC_USED" = "0" ]; then
  record PASS "the source segment is empty afterwards" "$(grep '^AFTER source_used' "$LOGA1")"
else
  record FAIL "the source segment is empty afterwards" "$(grep '^AFTER source_used' "$LOGA1")"
fi

if [ -n "$TGT_BEFORE" ] && [ -n "$TGT_AFTER" ] && [ -n "$F_MIG" ] && [ "$((TGT_AFTER - TGT_BEFORE))" = "$F_MIG" ]; then
  record PASS "the target segment grew by exactly the migrated bytes" "used ${TGT_BEFORE} -> ${TGT_AFTER}"
else
  record FAIL "the target segment grew by exactly the migrated bytes" "used ${TGT_BEFORE} -> ${TGT_AFTER}, migrated_bytes=${F_MIG}"
fi

# The load-bearing read: the source pod goes away, the master forgets its segment, and only then
# are the keys read -- from the peer, on the other node.
kubectl -n "$NS" delete pod "$P_A" --wait=true --timeout=180s >/dev/null 2>&1 || true

run_probe a-read "$LOGA2" <<PY
import json, sys, time, hashlib, urllib.error, urllib.request

MASTER = "${MASTER_IP}:9003"
B = "${B_IP}:8080"
A_IP = "${A_IP}"
EXPECT_200 = "${SUBJECT_A}".split(",")

def http(method, url, body=None, timeout=60):
    req = urllib.request.Request(url, data=body, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return -1, str(e).encode()

def segments():
    st, body = http("GET", "http://%s/get_segments_detail" % MASTER)
    if st != 200:
        raise RuntimeError("segment listing failed: %d" % st)
    return json.loads(body).get("segments", [])

def seg_of(ip):
    for s in segments():
        if s.get("segment_name", "").startswith(ip + ":"):
            return s
    return None

gone = -1
for t in range(0, 150, 5):
    try:
        if seg_of(A_IP) is None:
            gone = t
            break
    except Exception:
        pass
    time.sleep(5)
if gone >= 0:
    print("SOURCE-SEGMENT-GONE after=%ds" % gone)
else:
    print("SOURCE-SEGMENT-STILL-LISTED after=150s")

for key in [k for k in EXPECT_200 if k]:
    st, body = http("GET", "http://%s/api/get/%s" % (B, key), timeout=30)
    if st == 200:
        print("READ-KEY key=%s status=%d bytes=%d sha256=%s" % (key, st, len(body), hashlib.sha256(body).hexdigest()))
    else:
        print("READ-KEY key=%s status=%d body=%r" % (key, st, body[:100]))
print("READ-PROBE-DONE")
PY

echo "----- phase A read-back probe log (source pod deleted) -----"
cat "$LOGA2"
echo "-------------------------------------------------------------"

if grep -q "SOURCE-SEGMENT-GONE" "$LOGA2"; then
  record PASS "the master drops the source segment after the pod is gone" "$(grep 'SOURCE-SEGMENT-GONE' "$LOGA2")"
else
  record FAIL "the master drops the source segment after the pod is gone" \
    "$(grep -E 'SOURCE-SEGMENT|Error|error' "$LOGA2" | head -2 | tr '\n' ' ')"
fi

READ_RES="$(check_reads "$LOGA2" "$SUBJECT_A" 200 "$WANT_SHA")"
RBAD="$(printf '%s' "$READ_RES" | awk '{print $1}')"
RTOT="$(printf '%s' "$READ_RES" | awk '{print $2}')"
if [ "$RTOT" -gt 0 ] && [ "$RBAD" = "0" ]; then
  record PASS "every written key reads back byte-identical through the peer with the source gone" \
    "${RTOT} keys, status=200 and sha256 match, source pod deleted and forgotten"
else
  record FAIL "every written key reads back byte-identical through the peer with the source gone" \
    "${RBAD} of ${RTOT} keys did not read back as written"
fi

# ------------------------------------------------- 3. phase B: the disk tier is neither moved nor counted

# The replacement carries the same pod name, so the phase A delete has to have finished: an
# apply over a still-terminating pod is a no-op and the Ready wait below would time out on the
# old pod, reporting a store failure that is really a slow delete.
if ! kubectl -n "$NS" wait --for=delete "pod/$P_A" --timeout=60s >/dev/null 2>&1; then
  record FAIL "the phase A source pod finishes deleting before its replacement is created" "$(pod_state "$P_A")"
  results; exit 1
fi

apply_member_a

if ! wait_ready "$P_A" 300; then
  record FAIL "member A comes back for the disk phase" "$(pod_state "$P_A")"
  results; exit 1
fi
A_IP="$(kubectl -n "$NS" get pod "$P_A" -o jsonpath='{.status.podIP}')"
record PASS "member A comes back for the disk phase" "${P_A} at ${A_IP} on ${NODE_A}"

run_probe b-fill "$LOGB1" <<PY
import json, sys, time, hashlib, urllib.error, urllib.request

MASTER = "${MASTER_IP}:9003"
A = "${A_IP}:8080"
B = "${B_IP}:8080"
A_IP = "${A_IP}"
B_IP = "${B_IP}"
OBJ = ${OBJ_BYTES}

def http(method, url, body=None, timeout=60):
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return -1, str(e).encode()

def metrics():
    st, body = http("GET", "http://%s/metrics" % MASTER)
    out = {}
    if st != 200:
        return out
    for line in body.decode("utf-8", "replace").splitlines():
        if line.startswith("#") or " " not in line:
            continue
        name, _, value = line.partition(" ")
        try:
            out[name] = float(value)
        except ValueError:
            pass
    return out

def g(name, snap=None):
    return (snap or metrics()).get(name, 0.0)

def segments():
    st, body = http("GET", "http://%s/get_segments_detail" % MASTER)
    if st != 200:
        raise RuntimeError("segment listing failed: %d" % st)
    return json.loads(body).get("segments", [])

def seg_of(ip):
    for s in segments():
        if s.get("segment_name", "").startswith(ip + ":"):
            return s
    return None

def free_bytes(seg):
    return seg["allocator_capacity_bytes"] - seg["allocator_used_bytes"]

payload = "x" * OBJ
digest = hashlib.sha256(payload.encode()).hexdigest()

# Writes through A past A's memory: once the segment fills, eviction pushes the oldest keys onto
# A's disk tier.
dkeys = []
for i in range(40):
    key = "d-%03d" % i
    st, _ = http("PUT", "http://%s/api/put" % A, json.dumps({"key": key, "value": payload}).encode())
    if st == 200:
        dkeys.append(key)
    else:
        print("WRITE d refused at i=%d status=%d" % (i, st))
        break
print("WRITE d accepted=%d" % len(dkeys))

filler = []
for i in range(40):
    key = "p-%03d" % i
    st, _ = http("PUT", "http://%s/api/put" % A, json.dumps({"key": key, "value": payload}).encode())
    if st == 200:
        filler.append(key)
    else:
        print("WRITE p refused at i=%d status=%d" % (i, st))
        break
print("WRITE p accepted=%d" % len(filler))
for _ in range(12):
    if g("master_evicted_key_count_mem") > 0:
        break
    time.sleep(5)
print("TIER evicted_key_count_mem=%d" % g("master_evicted_key_count_mem"))

# A key is disk-resident when reading it moves the master's file-cache-hit counter. Everything
# else about the tier's bookkeeping is inference; this one is a served read. The reads go to
# member A itself: B never touches a subject key here, so no replica of one can be cached on B
# and later mistake the verdict reads.
subject = []
# The metrics page is large; read it once per key and carry the previous reading forward as the
# next key's baseline instead of reading it twice per key.
hits = g("file_cache_hit_nums_")
for key in dkeys:
    st, _ = http("GET", "http://%s/api/get/%s" % (A, key), timeout=30)
    after = g("file_cache_hit_nums_")
    if st == 200 and after > hits:
        subject.append(key)
    hits = after
print("TIER subject_count=%d subject=%s" % (len(subject), ",".join(subject)))
if len(subject) < 4:
    print("SETUP-FAILED only %d keys are disk-resident, the disk half cannot be exercised" % len(subject))
    sys.exit(1)

# A remove reaches only the keys the addressed member can name: filler that spilled to B is
# removed through B cleanly, while a key homed on A (memory or tier) answers the same call
# through B with a 500. Try the peer before calling a key unremovable.
def remove(key):
    st, _ = http("DELETE", "http://%s/api/remove/%s" % (B, key), timeout=30)
    if st != 200:
        st, _ = http("DELETE", "http://%s/api/remove/%s" % (A, key), timeout=30)
    return st

# Reading a tier key through the member that owns the tier serves it from the tier file without
# pulling a replica into that member's memory (measured: every subject key 404s after the source
# goes away, with no flush in between). Reading it through B instead can leave a migratable
# replica behind, which is why the detection above stays on A and no memory flush is needed.
#
# The filler goes first: the store sits at the edge of its effective capacity here (measured:
# writes past the 80th object in this shape are refused), and the witnesses must land. Removing
# the filler opens the room; the measured keys are untouched. A remove can fail on a key
# eviction already lost (eviction past the bucket limits drops it without a tier write); those
# keys are not measured, but the failure is printed, not assumed.
removed_filler = 0
for key in filler:
    st = remove(key)
    if st == 200:
        removed_filler += 1
    else:
        print("REMOVE %s status=%d" % (key, st))
print("TIER removed_filler_keys=%d" % removed_filler)

# The witnesses go in after the detection so no later write evicts them before the drain, and
# they must exist: a witness that never landed makes the drain's succeeded_units measure
# whatever else the memory held.
mkeys = ["m-000", "m-001"]
for key in mkeys:
    st, _ = http("PUT", "http://%s/api/put" % A, json.dumps({"key": key, "value": payload}).encode())
    print("PUT %s status=%d" % (key, st))
    if st != 200:
        print("SETUP-FAILED witness %s refused status=%d" % (key, st))
        sys.exit(1)

# Free everything that is written but not measured, so the drain has somewhere to put what it
# does move.
removed = 0
for key in dkeys:
    if key in subject:
        continue
    st = remove(key)
    if st == 200:
        removed += 1
    else:
        print("REMOVE %s status=%d" % (key, st))
print("TIER removed_non_subject_keys=%d" % removed)
time.sleep(3)

a_seg, b_seg = seg_of(A_IP), seg_of(B_IP)
if not a_seg or not b_seg:
    print("SETUP-FAILED both segments have to be listed")
    sys.exit(1)
print("SEGMENTS source=%s used=%d free=%d" % (a_seg["segment_name"], a_seg["allocator_used_bytes"], free_bytes(a_seg)))
print("SEGMENTS target=%s used=%d free=%d" % (b_seg["segment_name"], b_seg["allocator_used_bytes"], free_bytes(b_seg)))
# The pre-drain baselines the verdict compares against: the source's memory keys (used/OBJ, an
# exact count here -- every object is OBJ bytes) and the tier's size. Both come from the store's
# own segment and metrics reads, never from the job report under test.
print("BEFORE source_used=%d" % a_seg["allocator_used_bytes"])
print("BEFORE tier_bytes=%d" % g("master_allocated_file_size_bytes"))
if free_bytes(b_seg) < 3 * OBJ:
    print("SETUP-FAILED target free=%d, the memory witnesses need %d" % (free_bytes(b_seg), 3 * OBJ))
    sys.exit(1)

body = json.dumps({"segments": [a_seg["segment_name"]],
                   "target_segments": [b_seg["segment_name"]],
                   "max_concurrency": 4}).encode()
st, resp = http("POST", "http://%s/api/v1/drain_jobs" % MASTER, body, timeout=60)
text = resp.decode("utf-8", "replace")
print("DRAIN-CREATE status=%d body=%s" % (st, text[:400]))
if st != 200:
    print("DRAIN-CREATE-REJECTED nothing below carries information")
    sys.exit(1)
job_id = (json.loads(text) or {}).get("job_id")
print("DRAIN-JOB-ID %s" % job_id)
if not job_id:
    print("SETUP-FAILED the create response carried no job_id")
    sys.exit(1)

terminal = {"DRAINED", "COMPLETED", "FAILED", "CANCELLED", "CANCELED", "SUCCEEDED"}
doc, state = {}, None
for i in range(90):
    st, q = http("GET", "http://%s/api/v1/drain_jobs/query?job_id=%s" % (MASTER, job_id), timeout=30)
    last = q.decode("utf-8", "replace")
    try:
        doc = json.loads(last)
    except Exception:
        doc = {}
    state = doc.get("status_name")
    print("DRAIN-QUERY t=%ds http=%d state=%s succeeded=%s failed=%s blocked=%s active=%s migrated_bytes=%s" % (
        i * 2, st, state, doc.get("succeeded_units"), doc.get("failed_units"),
        doc.get("blocked_units"), doc.get("active_units"), doc.get("migrated_bytes")), flush=True)
    if st == 200 and state in terminal:
        break
    time.sleep(2)
print("DRAIN-TERMINAL-STATE %s" % state)

def num(d, k):
    v = d.get(k)
    return v if isinstance(v, int) else -1

print("FINAL succeeded=%d failed=%d blocked=%d active=%d migrated_bytes=%d" % (
    num(doc, "succeeded_units"), num(doc, "failed_units"), num(doc, "blocked_units"),
    num(doc, "active_units"), num(doc, "migrated_bytes")))

a_after, b_after = seg_of(A_IP), seg_of(B_IP)
print("AFTER source_used=%d source_status=%s" % (
    a_after["allocator_used_bytes"] if a_after else -1, a_after.get("status") if a_after else "MISSING"))
print("AFTER target_used=%d" % (b_after["allocator_used_bytes"] if b_after else -1))
print("AFTER tier_bytes=%d" % g("master_allocated_file_size_bytes"))
print("MEM subject=%s" % ",".join(mkeys))
print("WANT sha256=%s bytes=%d" % (digest, OBJ))
print("PROBE-B-FILL-DONE")
PY

echo "----- phase B fill/drain probe log (disk tier) -----"
cat "$LOGB1"
echo "-----------------------------------------------------"

if grep -q "SETUP-FAILED" "$LOGB1"; then
  record FAIL "phase B preconditions hold (a disk-resident subject exists)" "$(grep 'SETUP-FAILED' "$LOGB1" | head -1)"
  results; exit 1
fi

TERM_B="$(sed -n 's/^DRAIN-TERMINAL-STATE //p' "$LOGB1" | tail -1)"
FINAL_B="$(sed -n 's/^FINAL //p' "$LOGB1" | tail -1)"
B_SUCC="$(printf '%s' "$FINAL_B" | sed -n 's/^succeeded=\([0-9-]*\).*/\1/p')"
B_FAIL="$(printf '%s' "$FINAL_B" | sed -n 's/.*failed=\([0-9-]*\).*/\1/p')"
B_BLOCK="$(printf '%s' "$FINAL_B" | sed -n 's/.*blocked=\([0-9-]*\).*/\1/p')"
B_ACTIVE="$(printf '%s' "$FINAL_B" | sed -n 's/.*active=\([0-9-]*\).*/\1/p')"
B_MIG="$(printf '%s' "$FINAL_B" | sed -n 's/.*migrated_bytes=\([0-9-]*\).*/\1/p')"
TIER_LINE="$(sed -n 's/^TIER subject_count=\([0-9]*\) subject=\(.*\)/\1 \2/p' "$LOGB1" | tail -1)"
TIER_COUNT="$(printf '%s' "$TIER_LINE" | awk '{print $1}')"
TIER_SUBJECT="$(printf '%s' "$TIER_LINE" | cut -d' ' -f2-)"
MEM_SUBJECT="$(sed -n 's/^MEM subject=//p' "$LOGB1" | tail -1)"
B_TIER_BEFORE="$(sed -n 's/^BEFORE tier_bytes=\([0-9-]*\).*/\1/p' "$LOGB1" | tail -1)"
B_TIER_BYTES="$(sed -n 's/^AFTER tier_bytes=\([0-9-]*\).*/\1/p' "$LOGB1" | tail -1)"
B_SRC_USED_PRE="$(sed -n 's/^BEFORE source_used=\([0-9-]*\).*/\1/p' "$LOGB1" | tail -1)"
B_SRC_USED="$(sed -n 's/^AFTER source_used=\([0-9-]*\).*/\1/p' "$LOGB1" | tail -1)"

record PASS "a disk-resident subject exists" "${TIER_COUNT} keys confirmed served from the tier"

if [ "$TERM_B" = "SUCCEEDED" ]; then
  record PASS "the drain job over a tiered source still reports SUCCEEDED" "terminal=${TERM_B}"
else
  record FAIL "the drain job over a tiered source still reports SUCCEEDED" "terminal='${TERM_B}' ($FINAL_B)"
fi

# The drain moves every memory key the source still holds: the witnesses plus whatever the
# removal above could not name (measured: a remove can fail through both members on a key the
# drain then moves). That the witnesses are among the moved is proven by the read-back below;
# the counters here must show the job moved at least the witnesses, lost nothing, and that its
# byte counter agrees with its unit counter.
if [ -n "$B_SUCC" ] && [ "$B_SUCC" -ge 2 ] 2>/dev/null && [ "$B_FAIL" = "0" ] && [ "$B_BLOCK" = "0" ] && [ -n "$B_MIG" ] && [ "$B_MIG" = "$((B_SUCC * OBJ_BYTES))" ]; then
  record PASS "the drain migrated the memory witnesses, lost nothing, and its counters agree" "$FINAL_B"
else
  record FAIL "the drain migrated the memory witnesses, lost nothing, and its counters agree" \
    "$FINAL_B -- want succeeded>=2 (the witnesses), failed=0, blocked=0, migrated_bytes=succeeded*${OBJ_BYTES}; a miss means the fixture did not converge or the drain lost units"
fi

# The counter gap, today: the source held TIER_COUNT + MEM_KEYS keys at drain time -- the tier's,
# plus the memory keys measured on its own segment before the drain, never from the report under
# test -- and the job's counters sum to the memory keys alone. The tier's keys are in nobody's
# report. The day a fix makes the job account for the tier -- migrating it or counting the skip --
# the covered sum reaches the held one and the case must go red.
if [ -n "$B_SUCC" ] && [ -n "$B_FAIL" ] && [ -n "$B_BLOCK" ] && [ -n "$B_ACTIVE" ] && [ -n "$TIER_COUNT" ] && [ -n "$B_SRC_USED_PRE" ]; then
  GAP_COVERED=$((B_SUCC + B_FAIL + B_BLOCK + B_ACTIVE))
  GAP_HELD=$((TIER_COUNT + B_SRC_USED_PRE / OBJ_BYTES))
else
  GAP_COVERED=0
  GAP_HELD=0
fi
if [ "$GAP_HELD" -gt 0 ] && [ "$GAP_COVERED" -lt "$GAP_HELD" ]; then
  record PASS "the job's counters do not sum to the keys the source held" \
    "succeeded+failed+blocked+active=${GAP_COVERED} < held=${GAP_HELD} (${TIER_COUNT} tier-resident + $((B_SRC_USED_PRE / OBJ_BYTES)) memory keys measured pre-drain)"
else
  record FAIL "the job's counters do not sum to the keys the source held" \
    "succeeded+failed+blocked+active=${GAP_COVERED} vs held=${GAP_HELD} (${TIER_COUNT} tier-resident + pre-drain source_used=${B_SRC_USED_PRE}) -- the drain's accounting of the tier changed, or the probe log did not parse; revisit the skipped-tier premise"
fi

if [ -n "$B_TIER_BEFORE" ] && [ -n "$B_TIER_BYTES" ] && [ "$B_TIER_BYTES" = "$B_TIER_BEFORE" ]; then
  record PASS "the drain leaves the tier's bytes untouched" "tier_bytes ${B_TIER_BEFORE} -> ${B_TIER_BYTES}"
else
  record FAIL "the drain leaves the tier's bytes untouched" "tier_bytes ${B_TIER_BEFORE} -> ${B_TIER_BYTES}"
fi

if [ "$B_SRC_USED" = "0" ]; then
  record PASS "the tiered source's memory segment is empty afterwards" "$(grep '^AFTER source_used' "$LOGB1")"
else
  record FAIL "the tiered source's memory segment is empty afterwards" "$(grep '^AFTER source_used' "$LOGB1")"
fi

kubectl -n "$NS" delete pod "$P_A" --wait=true --timeout=180s >/dev/null 2>&1 || true

WANT_SHA="$(sed -n 's/^WANT sha256=\([0-9a-f]*\).*/\1/p' "$LOGB1" | tail -1)"

run_probe b-read "$LOGB2" <<PY
import json, sys, time, hashlib, urllib.error, urllib.request

MASTER = "${MASTER_IP}:9003"
B = "${B_IP}:8080"
A_IP = "${A_IP}"
EXPECT_200 = "${MEM_SUBJECT}".split(",")
EXPECT_404 = "${TIER_SUBJECT}".split(",")

def http(method, url, body=None, timeout=60):
    req = urllib.request.Request(url, data=body, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return -1, str(e).encode()

def segments():
    st, body = http("GET", "http://%s/get_segments_detail" % MASTER)
    if st != 200:
        raise RuntimeError("segment listing failed: %d" % st)
    return json.loads(body).get("segments", [])

def seg_of(ip):
    for s in segments():
        if s.get("segment_name", "").startswith(ip + ":"):
            return s
    return None

gone = -1
for t in range(0, 150, 5):
    try:
        if seg_of(A_IP) is None:
            gone = t
            break
    except Exception:
        pass
    time.sleep(5)
if gone >= 0:
    print("SOURCE-SEGMENT-GONE after=%ds" % gone)
else:
    print("SOURCE-SEGMENT-STILL-LISTED after=150s")
# The segment drop and the key-directory cleanup are not one atomic step on this build; give the
# master a beat so a key whose only location was the tier resolves to "not found" rather than to
# a dead address.
time.sleep(10)

for key in [k for k in EXPECT_200 if k]:
    st, body = http("GET", "http://%s/api/get/%s" % (B, key), timeout=30)
    if st == 200:
        print("READ-KEY key=%s status=%d bytes=%d sha256=%s" % (key, st, len(body), hashlib.sha256(body).hexdigest()))
    else:
        print("READ-KEY key=%s status=%d body=%r" % (key, st, body[:100]))
for key in [k for k in EXPECT_404 if k]:
    st, body = http("GET", "http://%s/api/get/%s" % (B, key), timeout=15)
    if st == 200:
        print("READ-KEY key=%s status=%d bytes=%d sha256=%s" % (key, st, len(body), hashlib.sha256(body).hexdigest()))
    else:
        print("READ-KEY key=%s status=%d body=%r" % (key, st, body[:100]))
print("READ-PROBE-DONE")
PY

echo "----- phase B read-back probe log (source pod deleted) -----"
cat "$LOGB2"
echo "-------------------------------------------------------------"

if grep -q "SOURCE-SEGMENT-GONE" "$LOGB2"; then
  record PASS "the master drops the tiered source segment after the pod is gone" "$(grep 'SOURCE-SEGMENT-GONE' "$LOGB2")"
else
  record FAIL "the master drops the tiered source segment after the pod is gone" \
    "$(grep -E 'SOURCE-SEGMENT|Error|error' "$LOGB2" | head -2 | tr '\n' ' ')"
fi

READ_RES="$(check_reads "$LOGB2" "$MEM_SUBJECT" 200 "$WANT_SHA")"
MBAD="$(printf '%s' "$READ_RES" | awk '{print $1}')"
MTOT="$(printf '%s' "$READ_RES" | awk '{print $2}')"
if [ "$MTOT" -gt 0 ] && [ "$MBAD" = "0" ]; then
  record PASS "the memory witnesses read back byte-identical through the peer with the source gone" \
    "${MTOT} keys, status=200 and sha256 match"
else
  record FAIL "the memory witnesses read back byte-identical through the peer with the source gone" \
    "${MBAD} of ${MTOT} keys did not read back as written"
fi

# THE known-failure detector. On the store version this was measured on, the drain cannot name the
# tier's segments, so these keys vanished with their member and read 404. A 200 here means the
# drain has started reaching the tier: this case MUST go red that day, and the skipped-tier
# premise in the shipped API has to be revisited.
READ_RES="$(check_reads "$LOGB2" "$TIER_SUBJECT" 404)"
TBAD="$(printf '%s' "$READ_RES" | awk '{print $1}')"
TTOT="$(printf '%s' "$READ_RES" | awk '{print $2}')"
if [ "$TTOT" -gt 0 ] && [ "$TBAD" = "0" ]; then
  record PASS "every tier-resident key is gone with its member (404), though the job reported success" \
    "${TTOT} keys, all 404 through the peer"
else
  record FAIL "every tier-resident key is gone with its member (404), though the job reported success" \
    "${TBAD} of ${TTOT} keys still read back -- the drain now reaches the tier; the skipped-tier premise is fixed upstream and this detector is doing its job"
fi

results
