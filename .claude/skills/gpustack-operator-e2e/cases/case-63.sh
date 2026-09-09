#!/usr/bin/env bash
#
# CASE 63 — How a KV cache member finds the leader under high availability: reading the Lease
#   (k8s://) versus keeping the plain Service address -- measured as two failover convergence
#   intervals   (MUTATING, self-recovering)
#
#   case-63.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. The KVCacheBackend is
# cluster-scoped; its rendered objects live in <NS>, and the two probe Pods live in a namespace this
# case creates and removes.
#
# Goal:        Under leader.highAvailability the operator hands every member MOONCAKE_MASTER as
#              "k8s://<ns>/<lease>" (Path A: the client reads the Lease itself and follows view
#              changes). That choice has a cost nobody chose: the member's image needs the lease
#              backend compiled in, which excludes every vendor image. The unmeasured alternative
#              (Path B) is the member keeping the plain Service address -- the leader Service's
#              endpoints only ever contain the serving replica (standbys are deliberately not
#              ready), and the client's ping loop re-dials the same DNS name after enough failed
#              pings. Path B pays the endpoint propagation delay: the demoted leader stays in the
#              endpoints until its readiness probe fails (periodSeconds 5 x failureThreshold 3, up
#              to ~15s), during which a reconnect can land on a Pod that answers and refuses to
#              serve.
#
#              This case MEASURES both paths instead of arguing them: delete the serving leader
#              Pod, observe an error window, and record the interval from the delete until the first
#              later store operation completes. A completed put is the endpoint because it requires
#              the whole chain -- client reaches the new master AND a member has re-registered its
#              segment there; "no error in the log" proves neither.
#
# Environment: Any cluster; no GPU, no RDMA (the member is DRAM over TCP). NEEDS a store image
#              built with the Kubernetes Lease leadership backend (E2E_MOONCAKE_IMAGE; the default
#              below carries one) -- the Path A probe's preflight put is what proves the probe
#              itself can work before any failover is measured, so a compiled-out backend fails
#              there, loudly, rather than as a zero measurement.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (leader replicas 3, highAvailability,
#              one DRAM member group of 2Gi). Two probe Pods running the store image's python
#              client: probe A set up with "k8s://<ns>/<backend>-leader" (it gets a ServiceAccount
#              bound to the member Role -- `get leases` only -- because that read is exactly what
#              Path A costs), probe B with "<backend>-leader.<ns>.svc:50051" and no API access at
#              all. Each probe then puts once every 0.3s, timestamped, before, during and after one
#              induced failover.
#
# Expected:    - both probes complete a put BEFORE the failover (preflight; a failure here voids
#                the measurement rather than recording a zero);
#              - after the serving leader Pod is deleted, each path reports an error and then
#                completes a put again within the 300s deadline -- the two delete-to-recovery
#                intervals are REPORTED, and Path B failing to converge at all is itself the answer
#                the decision needs (then Path B does not work and the vendor-image work it would
#                avoid is unavoidable);
#              - the leader Service's endpoint set is sampled through the transition, so Path B's
#                interval can be read against the propagation delay it pays;
#              - the backend is Ready after both measurements.
#
# Cleanup:     Trap stops the probe loops and the endpoint sampler, deletes the probe namespace
#              (both Pods, the probe ServiceAccount), the probe RoleBinding in <NS>, and the
#              KVCacheBackend (cascading to its Deployment, Service, Lease and RBAC). Idempotent,
#              runs on pass AND fail, safe to re-run.
#
# One caveat the numbers carry: the delete timestamp comes from this machine's clock and the put
# timestamps from the probe Pods'. Both are NTP-disciplined, so the intervals are good to about a
# second -- enough for a measurement whose expected scale is tens of seconds.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-63.sh <NS>}"
CASE_ID=63
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs apart: the backend's name is cluster-scoped and every rendered name
# derives from it.
#
# LC_ALL=C and the disabled pipefail are both load-bearing: under a UTF-8 locale tr dies on
# /dev/urandom, and with pipefail on the SIGPIPE from head turns a trailing fallback into an append.
# The measurements behind both halves are recorded once, at the same idiom in
# _kvcache-inject-lib.sh.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-cx-${SFX}"
LEADER="${BACKEND}-leader"
TEST_NS="kvc-cx-${SFX}"
PROBE_A="probe-a-${SFX}"
PROBE_B="probe-b-${SFX}"
PROBE_SA="probe-a-${SFX}"
PROBE_RB="probe-a-${SFX}"
DEADLINE=300

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"

WORK="$(mktemp -d)"
LOG_A="$WORK/pathA.log"
LOG_B="$WORK/pathB.log"
LOG_EP="$WORK/endpoints.log"

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

teardown() {
  echo
  echo "[case-63] cleanup"
  jobs -p | xargs kill 2>/dev/null || true
  kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete rolebinding.rbac.authorization.k8s.io "$PROBE_RB" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "[case-63] probe logs kept at $WORK"
}
trap teardown EXIT

wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-180}"
  local got="" i
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl -n "$NS" get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

ready_leader_pod() {
  kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[?(@.status.containerStatuses[0].ready==true)]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | head -1
}

# The probe loop. The master address arrives as argv[1] so the quoting of "k8s://..." never passes
# through the shell twice; argv[2] is the loop's own deadline. One put every 0.3s, epoch-stamped:
# the timestamps are the measurement, so they are printed whether the put works, errors, or raises.
# A put that BLOCKS inside the client during the failover shows up as a gap between lines, which is
# why the sleep cadence matters.
read -r -d '' PROBE_PY <<'PY' || true
import os, sys, time
from mooncake.store import MooncakeDistributedStore
store = MooncakeDistributedStore()
rc = store.setup(os.environ.get("POD_IP", "127.0.0.1"), "P2PHANDSHAKE", 0, 134217728,
                 "tcp", "", sys.argv[1])
print("SETUP t=%.3f rc=%d" % (time.time(), rc), flush=True)
if rc != 0:
    sys.exit(0)
payload = b"x" * 1024
deadline = time.time() + float(sys.argv[2])
i = 0
while time.time() < deadline:
    i += 1
    try:
        rc = store.put("c63probe-%d" % (i % 8), payload)
        print("PUT t=%.3f rc=%d" % (time.time(), rc), flush=True)
    except Exception as e:
        print("PUT t=%.3f exc=%r" % (time.time(), e), flush=True)
    time.sleep(0.3)
PY

# The one-shot preflight: prove the path serves a store operation BEFORE anything is measured off
# it. A path that cannot do this has no failover interval to measure, only a defect to report.
read -r -d '' PREFLIGHT_PY <<'PY' || true
import os, sys
from mooncake.store import MooncakeDistributedStore
store = MooncakeDistributedStore()
rc = store.setup(os.environ.get("POD_IP", "127.0.0.1"), "P2PHANDSHAKE", 0, 134217728,
                 "tcp", "", sys.argv[1])
print("SETUP rc=%d" % rc, flush=True)
if rc != 0:
    sys.exit(0)
rc = store.put("c63-preflight", b"preflight")
got = store.get("c63-preflight")
print("PUT rc=%d GET match=%s" % (rc, got == b"preflight"), flush=True)
PY

probe_manifest() {
  local name="$1" sa_line="$2"
  cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${TEST_NS}
spec:
  restartPolicy: Never
  ${sa_line}
  containers:
    - name: probe
      image: ${IMAGE}
      command: ["python3", "-c", "import time; time.sleep(3600)"]
      env:
        - name: POD_IP
          valueFrom: {fieldRef: {fieldPath: status.podIP}}
YAML
}

# ---------------------------------------------------------------- setup

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
        highAvailability: {}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend reaches Ready" \
    "phase never became Ready in 300s; nothing below can run. No ready leader at all is the \
signature of a store image without the Kubernetes Lease leadership backend"
  results; exit 1
fi
record PASS "the backend reaches Ready" "phase=Ready for ${BACKEND}"

if ! wait_for deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record FAIL "exactly one leader replica is Ready" \
    "readyReplicas='$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' after 300s"
  results; exit 1
fi

kubectl create namespace "$TEST_NS" >/dev/null 2>&1 || true

# Probe A reads the Lease, which is the whole cost Path A charges a member: a ServiceAccount bound
# to the backend's member Role (get leases, and nothing else). The RoleBinding lives in <NS> because
# that is where the Lease and the Role live; the subject is the probe's account in the probe
# namespace.
kubectl -n "$TEST_NS" create serviceaccount "$PROBE_SA" >/dev/null 2>&1 || true
kubectl apply -f - <<YAML >/dev/null
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${PROBE_RB}
  namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${BACKEND}-member
subjects:
  - kind: ServiceAccount
    name: ${PROBE_SA}
    namespace: ${TEST_NS}
YAML

probe_manifest "$PROBE_A" "serviceAccountName: ${PROBE_SA}" | kubectl apply -f - >/dev/null
probe_manifest "$PROBE_B" "automountServiceAccountToken: false" | kubectl apply -f - >/dev/null

for p in "$PROBE_A" "$PROBE_B"; do
  if ! kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/$p" --timeout=300s >/dev/null 2>&1; then
    record FAIL "the probe Pods run" "$p never became Ready in 300s (most likely still pulling ${IMAGE})"
    results; exit 1
  fi
done
record PASS "the probe Pods run" "${PROBE_A} (k8s://) and ${PROBE_B} (Service address) are Ready"

MASTER_A="k8s://${NS}/${LEADER}"
MASTER_B="${LEADER}.${NS}.svc:50051"

OUT_A="$(kubectl -n "$TEST_NS" exec "$PROBE_A" -c probe -- python3 -c "$PREFLIGHT_PY" "$MASTER_A" 2>&1)"
if echo "$OUT_A" | grep -q '^PUT rc=0 GET match=True'; then
  record PASS "Path A serves a store operation before the failover" "$(echo "$OUT_A" | tr '\n' ' ')"
else
  record FAIL "Path A serves a store operation before the failover" \
    "preflight: $(echo "$OUT_A" | tr '\n' ' ' | cut -c1-200) -- a path that cannot put once has no failover interval to measure"
fi

OUT_B="$(kubectl -n "$TEST_NS" exec "$PROBE_B" -c probe -- python3 -c "$PREFLIGHT_PY" "$MASTER_B" 2>&1)"
if echo "$OUT_B" | grep -q '^PUT rc=0 GET match=True'; then
  record PASS "Path B serves a store operation before the failover" "$(echo "$OUT_B" | tr '\n' ' ')"
else
  record FAIL "Path B serves a store operation before the failover" \
    "preflight: $(echo "$OUT_B" | tr '\n' ' ' | cut -c1-200)"
fi
if [ "$FAILS" -gt 0 ]; then results; exit 1; fi

# ---------------------------------------------------------------- measure

kubectl -n "$TEST_NS" exec "$PROBE_A" -c probe -- python3 -c "$PROBE_PY" "$MASTER_A" "$DEADLINE" >"$LOG_A" 2>&1 &
kubectl -n "$TEST_NS" exec "$PROBE_B" -c probe -- python3 -c "$PROBE_PY" "$MASTER_B" "$DEADLINE" >"$LOG_B" 2>&1 &

# The endpoint sampler explains Path B's number: when the old address left the Service's endpoints
# and when the new one arrived. Sampled at 1s because the readiness gate's own granularity is 5s.
# The loop is BOUNDED: an unbounded sampler is one more background job, and the `wait` below would
# then block on it forever after both probes have long finished.
( for _ in $(seq 1 $((DEADLINE + 60))); do
    echo "$(python3 -c 'import time; print("%.3f" % time.time())') $(kubectl -n "$NS" get endpoints "$LEADER" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null)"
    sleep 1
  done >"$LOG_EP" ) &

# A baseline of successful puts on both paths before anything is deleted -- the loop prints one PUT
# line per attempt, so four seconds is a dozen samples per path.
sleep 4
BASE_A="$(grep -c '^PUT t=.* rc=0$' "$LOG_A" 2>/dev/null || true)"
BASE_B="$(grep -c '^PUT t=.* rc=0$' "$LOG_B" 2>/dev/null || true)"

OLD_READY="$(ready_leader_pod)"
T0="$(python3 -c 'import time; print("%.3f" % time.time())')"
kubectl -n "$NS" delete pod "$OLD_READY" --wait=false >/dev/null 2>&1

# Wait for both probes to finish on their own deadline; a converged path ends at ${DEADLINE}s, an
# unconverged one at the same deadline with no post-T0 success. The logs are the record either way.
wait

python3 - "$T0" "$LOG_A" "$LOG_B" "$LOG_EP" >"$WORK/summary.txt" <<'PY' || true
import re, sys
t0 = float(sys.argv[1])
print("delete at t0=%.3f" % t0)
for label, path in (("Path A (k8s://lease)", sys.argv[2]), ("Path B (service dns)", sys.argv[3])):
    puts = []
    for line in open(path):
        m = re.match(r"PUT t=([\d.]+) (?:rc=(-?\d+)|exc=.*)", line)
        if m:
            rc = int(m.group(2)) if m.group(2) is not None else None
            puts.append((float(m.group(1)), rc))
    ok_after = [t for t, rc in puts if rc == 0 and t > t0]
    ok_before = [t for t, rc in puts if rc == 0 and t <= t0]
    bad_after = [t for t, rc in puts if rc != 0 and t > t0]
    recovered_after = [t for t, rc in puts if rc == 0 and bad_after and t > bad_after[0]]
    if recovered_after:
        convergence = "%.3fs" % (recovered_after[0] - t0)
    elif bad_after:
        convergence = "DID_NOT_RECOVER"
    else:
        convergence = "NO_ERROR_WINDOW"
    print("%s: puts total=%d ok-before-t0=%d first-error-after-t0=%s first-ok-after-t0=%s convergence=%s"
          % (label, len(puts), len(ok_before),
             ("%.3f" % (bad_after[0] - t0)) if bad_after else "none",
             ("%.3f" % (ok_after[0] - t0)) if ok_after else "NEVER",
             convergence))
eps = []
for line in open(sys.argv[4]):
    parts = line.split()
    if len(parts) >= 2:
        eps.append((float(parts[0]), " ".join(parts[1:])))
moves = [e for e in eps if e[0] > t0]
if moves:
    first = moves[0][1]
    changed = [e for e in moves if e[1] != first]
    print("endpoints: first change %.3fs after delete" % (changed[0][0] - t0) if changed
          else "endpoints: no change observed after delete")
PY
cat "$WORK/summary.txt"

CONV_A="$(awk '/^Path A/ {for (i=1; i<=NF; i++) if ($i ~ /^convergence=/) print $i}' "$WORK/summary.txt" | cut -d= -f2)"
CONV_B="$(awk '/^Path B/ {for (i=1; i<=NF; i++) if ($i ~ /^convergence=/) print $i}' "$WORK/summary.txt" | cut -d= -f2)"

case "$CONV_A" in
  *s)
    record PASS "Path A (member reads the Lease) converges after failover" \
      "put succeeds again ${CONV_A} after the leader delete; baseline ${BASE_A:-0} puts ok before it; \
detail: $(grep '^Path A' "$WORK/summary.txt")" ;;
  NO_ERROR_WINDOW)
    record FAIL "Path A (member reads the Lease) converges after failover" \
      "no failed put was observed after the delete, so recovery was not measured; detail: $(grep '^Path A' "$WORK/summary.txt"); log: $LOG_A" ;;
  DID_NOT_RECOVER)
    record FAIL "Path A (member reads the Lease) converges after failover" \
      "no successful put followed the first failure within ${DEADLINE}s of the delete; detail: $(grep '^Path A' "$WORK/summary.txt"); log: $LOG_A" ;;
  *)
    record FAIL "Path A (member reads the Lease) converges after failover" \
      "the measurement produced no convergence verdict; detail: $(grep '^Path A' "$WORK/summary.txt"); log: $LOG_A" ;;
esac

case "$CONV_B" in
  *s)
    record PASS "Path B (member keeps the Service address) converges after failover" \
      "put succeeds again ${CONV_B} after the leader delete; baseline ${BASE_B:-0} puts ok before it; \
detail: $(grep '^Path B' "$WORK/summary.txt"); $(grep '^endpoints' "$WORK/summary.txt")" ;;
  NO_ERROR_WINDOW)
    record FAIL "Path B (member keeps the Service address) converges after failover" \
      "no failed put was observed after the delete, so recovery was not measured; detail: $(grep '^Path B' "$WORK/summary.txt"); $(grep '^endpoints' "$WORK/summary.txt"); log: $LOG_B" ;;
  DID_NOT_RECOVER)
    # This is the answer the issue exists for, and it is a FAIL of the check, not of the run: Path B
    # not converging means the member's Lease read is load-bearing and the vendor-image rebuild work
    # cannot be avoided.
    record FAIL "Path B (member keeps the Service address) converges after failover" \
      "no successful put followed the first failure within ${DEADLINE}s of the delete -- Path B DOES NOT WORK as read from \
upstream's source; detail: $(grep '^Path B' "$WORK/summary.txt"); $(grep '^endpoints' "$WORK/summary.txt"); log: $LOG_B" ;;
  *)
    record FAIL "Path B (member keeps the Service address) converges after failover" \
      "the measurement produced no convergence verdict; detail: $(grep '^Path B' "$WORK/summary.txt"); $(grep '^endpoints' "$WORK/summary.txt"); log: $LOG_B" ;;
esac

FINAL_PHASE="$(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$FINAL_PHASE" = "Ready" ]; then
  record PASS "the backend is Ready after the measurement" "phase=Ready"
else
  record FAIL "the backend is Ready after the measurement" "phase='${FINAL_PHASE:-<absent>}'"
fi

results
