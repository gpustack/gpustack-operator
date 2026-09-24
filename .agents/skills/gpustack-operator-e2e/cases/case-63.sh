#!/usr/bin/env bash
#
# CASE 63 — How a KV cache member finds the leader under high availability: reading the Lease
#   (k8s://) versus keeping the plain Service address -- measured as failover convergence
#   intervals over several rounds, every store call bounded   (MUTATING, self-recovering)
#
#   case-63.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. The KVCacheBackend is
# cluster-scoped; its rendered objects live in <NS>, and the two probe Pods live in a namespace this
# case creates and removes.
#
# Goal:        Under leader.highAvailability a member can be handed MOONCAKE_MASTER in one of two
#              forms. Path A is "k8s://<ns>/<lease>": the client reads the Lease itself and follows
#              view changes, which costs the member's image a compiled-in lease backend and so
#              excludes every vendor image. Path B, the default, is the plain leader Service
#              address: the Service's endpoints only ever contain the serving replica (standbys are
#              deliberately not ready), and the client's ping loop re-dials the same DNS name after
#              enough failed pings. Path B pays the endpoint propagation delay: the demoted leader
#              stays in the endpoints until its readiness probe fails (periodSeconds 5 x
#              failureThreshold 3, up to ~15s), during which a reconnect can land on a Pod that
#              answers and refuses to serve.
#
#              This case MEASURES both paths instead of arguing them: delete the serving leader
#              Pod, observe an error window, and record the interval from the delete until the first
#              later store operation completes. A completed put is the endpoint because it requires
#              the whole chain -- client reaches the new master AND a member has re-registered its
#              segment there; "no error in the log" proves neither. The failover is repeated
#              (E2E_C63_ROUNDS, default 3) so the comparison rests on more than one sample, and a
#              sampler records when the old Pod left, when the Lease holder moved and when the
#              Service's ready endpoint changed, so each interval can be split into its parts.
#
#              EVERY STORE CALL IS BOUNDED, and a call that exceeds the bound is a sample. A put
#              that blocks inside the client after a failover is a real reading -- the store is
#              unreachable for that long -- but an unbounded one yields no number at all: it holds
#              the probe past any sampling window. So a put slower than E2E_C63_PUT_BOUND seconds
#              counts as a failed one, a round with no recovery within E2E_C63_ROUND_BOUND seconds
#              is recorded as "still unreachable at the bound", and the probe process is killed at
#              its own deadline whatever it is doing.
#
# Environment: Any cluster; no GPU, no RDMA (the member is DRAM over TCP). NEEDS a store image
#              built with the Kubernetes Lease leadership backend (E2E_MOONCAKE_IMAGE; the default
#              below carries one) and shipping GNU `timeout` -- the Path A probe's preflight put is
#              what proves the probe itself can work before any failover is measured, so a
#              compiled-out backend fails there, loudly, rather than as a zero measurement.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (leader replicas 3, highAvailability,
#              one DRAM member group of 2Gi). Two probe Pods running the store image's python
#              client: probe A set up with "k8s://<ns>/<backend>-leader" (it gets a ServiceAccount
#              bound to the member Role -- `get leases` only -- because that read is exactly what
#              Path A costs), probe B with "<backend>-leader.<ns>.svc:50051" and no API access at
#              all. Each probe then puts once every 0.3s, each put stamped with its start and end,
#              before, during and after each induced failover.
#
# Expected:    - both probes complete a put BEFORE the first failover (preflight; a failure here
#                voids the measurement rather than recording a zero);
#              - in every round, after the serving leader Pod is deleted, each path reports a
#                failed or over-bound put and then completes a put within the bound again, within
#                the round bound -- the delete-to-recovery intervals are REPORTED per round, and
#                Path B failing to converge at all is itself the answer the decision needs (then
#                Path B does not work and the vendor-image work it would avoid is unavoidable);
#              - between rounds the backend settles back to one ready leader that the Lease names,
#                beside a full set of standbys, before the next delete;
#              - the backend is Ready after the last round.
#
# Cleanup:     Trap stops the probe loops and the sampler, deletes the probe namespace (both Pods,
#              the probe ServiceAccount), the probe RoleBinding in <NS>, and the KVCacheBackend
#              (cascading to its Deployment, Service and RBAC; the store-created Lease carries no
#              owner and is deleted by name). Idempotent, runs on pass AND fail, safe to re-run.
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
ROUNDS="${E2E_C63_ROUNDS:-3}"
# A put slower than this is a sample of the store being unreachable; a healthy one takes
# milliseconds.
PUT_BOUND="${E2E_C63_PUT_BOUND:-2}"
# How long one round waits for both paths to recover before recording "still unreachable".
ROUND_BOUND="${E2E_C63_ROUND_BOUND:-180}"
# How long the backend gets to return to one serving leader and a full set of standbys.
SETTLE_BOUND="${E2E_C63_SETTLE_BOUND:-180}"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"

WORK="$(mktemp -d)"
LOG_A="$WORK/pathA.log"
LOG_B="$WORK/pathB.log"
LOG_SAMPLE="$WORK/samples.log"

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
  # The store creates the Lease, so nothing owns it and the backend's deletion leaves it behind. It
  # goes after the backend, because a standby still running would campaign it back into existence.
  kubectl wait --for=delete "kvcachebackends.worker.gpustack.ai/${BACKEND}" --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete leases.coordination.k8s.io "$LEADER" --ignore-not-found --wait=false >/dev/null 2>&1 || true
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

lease_holder() {
  kubectl -n "$NS" get leases.coordination.k8s.io "$LEADER" \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null
}

holder_pod_uid() {
  local holder="$1" holder_ip pod_uid pod_ip deleting phase
  case "$holder" in
    \[*\]:*)
      holder_ip="${holder#\[}"
      holder_ip="${holder_ip%%\]*}"
      ;;
    *:*) holder_ip="${holder%:*}" ;;
    *) holder_ip="$holder" ;;
  esac
  while IFS='|' read -r pod_uid pod_ip deleting phase; do
    if [ "$phase" != "Running" ] || [ -n "$deleting" ] || [ -z "$pod_ip" ] \
      || [ "$pod_ip" != "$holder_ip" ]; then
      continue
    fi
    echo "$pod_uid"
    return 0
  done < <(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"|"}{.status.podIP}{"|"}{.metadata.deletionTimestamp}{"|"}{.status.phase}{"\n"}{end}' \
    2>/dev/null)
}

# The probe loop. The master address arrives as argv[1] so the quoting of "k8s://..." never passes
# through the shell twice; argv[2] is the loop's own deadline. One put every 0.3s, and every line
# carries BOTH ends of its put, because a put that blocks inside the client is a sample of the store
# being unreachable and only its duration says so. A START line precedes each put, so a put that
# never returns is still visible as the last thing the probe began.
#
# THE LOOP DOES NOT BOUND THE PUT ITSELF, and cannot: the client call may hold the interpreter lock,
# so no timer in this process is guaranteed to run while it blocks. The bound is the process's --
# the exec wraps it in `timeout -s KILL` -- and the classification of a slow put is the analysis's.
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
    s = time.time()
    print("START s=%.3f" % s, flush=True)
    try:
        rc = store.put("c63probe-%d" % (i % 8), payload)
        print("PUT s=%.3f t=%.3f rc=%d" % (s, time.time(), rc), flush=True)
    except Exception as e:
        print("PUT s=%.3f t=%.3f exc=%r" % (s, time.time(), e), flush=True)
    time.sleep(0.3)
print("END t=%.3f" % time.time(), flush=True)
os._exit(0)
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

# The probes outlive every round they could need, and the case stops them as soon as the last round
# is read, so this deadline is a ceiling rather than the run's length.
PROBE_SECS=$((ROUNDS * (ROUND_BOUND + SETTLE_BOUND) + 60))
kubectl -n "$TEST_NS" exec "$PROBE_A" -c probe -- \
  timeout -s KILL "$((PROBE_SECS + 15))" python3 -c "$PROBE_PY" "$MASTER_A" "$PROBE_SECS" >"$LOG_A" 2>&1 &
PID_A=$!
kubectl -n "$TEST_NS" exec "$PROBE_B" -c probe -- \
  timeout -s KILL "$((PROBE_SECS + 15))" python3 -c "$PROBE_PY" "$MASTER_B" "$PROBE_SECS" >"$LOG_B" 2>&1 &
PID_B=$!

# The sampler splits each interval into its parts: when the old Pod left, when the Lease named
# someone else, and when the Service's ready endpoint changed. The endpoint is read from the
# EndpointSlice with its conditions, because a Pod being deleted keeps its address in the slice and
# only flips `ready`; a series of bare addresses cannot show that flip. Sampled at about 1s, since
# the readiness gate's own granularity is 5s, and BOUNDED like the probes.
( end=$(( $(date +%s) + PROBE_SECS )); while [ "$(date +%s)" -lt "$end" ]; do
    ts="$(python3 -c 'import time; print("%.3f" % time.time())')"
    holder="$(kubectl -n "$NS" get leases.coordination.k8s.io "$LEADER" \
      -o jsonpath='{.spec.holderIdentity},{.spec.leaseTransitions}' 2>/dev/null)"
    eps="$(kubectl -n "$NS" get endpointslices.discovery.k8s.io -l "kubernetes.io/service-name=${LEADER}" \
      -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]},{.conditions.ready},{.conditions.terminating} {end}' 2>/dev/null)"
    pods="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
      -o jsonpath='{range .items[*]}{.metadata.name},{.status.podIP},{.status.containerStatuses[0].ready},{.metadata.deletionTimestamp} {end}' 2>/dev/null)"
    echo "${ts}|${holder}|${eps}|${pods}"
    sleep 1
  done >"$LOG_SAMPLE" ) &
PID_S=$!

# The analysis, shared by the per-round wait and the final report so both read a round the same way.
#
# A put is GOOD when it returned rc=0 within the put bound. Anything else after the delete -- an
# error, an exception, a put slower than the bound, or a put that started and never returned -- is
# the store being unreachable, and the earliest such put's START -- or the delete, for one already in
# flight -- opens the round's outage. Recovery is the first good put that STARTED after the outage
# opened, which is what keeps a put served by the old leader during its grace period from reading as
# a recovery. A round with an outage and no recovery inside the round bound is DID_NOT_RECOVER: the
# sample is "still unreachable at the bound".
cat >"$WORK/analyze.py" <<'PY'
import re, sys

def puts(path, lo, hi):
    out, open_start = [], None
    for line in open(path, errors="replace"):
        m = re.match(r"START s=([\d.]+)", line)
        if m:
            open_start = float(m.group(1))
            continue
        m = re.match(r"PUT s=([\d.]+) t=([\d.]+) (?:rc=(-?\d+)|exc=.*)", line)
        if m:
            s, t = float(m.group(1)), float(m.group(2))
            rc = int(m.group(3)) if m.group(3) is not None else None
            out.append((s, t, rc))
            open_start = None
    if open_start is not None:
        out.append((open_start, None, None))
    # A put still in flight when the round opens belongs to it too: a put that hung in an earlier
    # round leaves the path unreachable for this one, and filtering on the start alone would read
    # that as a round with no outage at all.
    return [p for p in out if lo < p[0] < hi or (p[0] <= lo and (p[1] is None or p[1] > lo))]

def verdict(path, t0, hi, put_bound, round_bound):
    ps = puts(path, t0, hi)
    good = lambda p: p[1] is not None and p[2] == 0 and p[1] - p[0] <= put_bound
    bad = [p for p in ps if not good(p)]
    if not bad:
        return "NO_ERROR_WINDOW", None, ps
    first_bad = max(bad[0][0], t0)
    rec = [p for p in ps if good(p) and p[0] > first_bad and p[1] - t0 <= round_bound]
    if not rec:
        return "DID_NOT_RECOVER", first_bad - t0, ps
    return "%.3fs" % (rec[0][1] - t0), first_bad - t0, ps

def describe(label, path, t0, hi, put_bound, round_bound):
    v, first_bad, ps = verdict(path, t0, hi, put_bound, round_bound)
    slowest = max([p[1] - p[0] for p in ps if p[1] is not None] or [0.0])
    hung = [p for p in ps if p[1] is None]
    return ("%s: puts=%d first-unreachable=%s slowest-put=%.1fs never-returned=%s convergence=%s"
            % (label, len(ps), ("+%.3f" % first_bad) if first_bad is not None else "none", slowest,
               ("%+.3f" % (hung[0][0] - t0)) if hung else "none",
               v if v != "DID_NOT_RECOVER" else "DID_NOT_RECOVER(>=%ds)" % round_bound))

def samples(path):
    out = []
    for line in open(path, errors="replace"):
        parts = line.rstrip("\n").split("|")
        if len(parts) != 4:
            continue
        holder = parts[1].split(",")[0]
        eps = [e.split(",") for e in parts[2].split()]
        pods = [p.split(",") for p in parts[3].split()]
        out.append((float(parts[0]), holder, eps, pods))
    return out

def first(ss, t0, hi, pred):
    for s in ss:
        if t0 < s[0] < hi and pred(s):
            return "+%.1f" % (s[0] - t0)
    return "never"

mode = sys.argv[1]
if mode == "recovered":
    t0, put_bound, round_bound = float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])
    ok = all(verdict(p, t0, float("inf"), put_bound, round_bound)[0].endswith("s")
             for p in sys.argv[5:])
    sys.exit(0 if ok else 1)

# report <put_bound> <round_bound> <logA> <logB> <samples> <t0,pod,ip>...
put_bound, round_bound = float(sys.argv[2]), float(sys.argv[3])
log_a, log_b, ss = sys.argv[4], sys.argv[5], samples(sys.argv[6])
rounds = [r.split(",") for r in sys.argv[7:]]
for i, (t0, pod, ip) in enumerate(rounds):
    t0 = float(t0)
    hi = float(rounds[i + 1][0]) if i + 1 < len(rounds) else float("inf")
    print("round %d: delete %s (%s) at t0=%.3f" % (i + 1, pod, ip, t0))
    print("round %d: %s" % (i + 1, describe("Path A (k8s://lease)", log_a, t0, hi, put_bound, round_bound)))
    print("round %d: %s" % (i + 1, describe("Path B (service dns)", log_b, t0, hi, put_bound, round_bound)))
    print("round %d: timeline old-pod-gone=%s lease-holder-moved=%s old-endpoint-unready=%s new-endpoint-ready=%s"
          % (i + 1,
             first(ss, t0, hi, lambda s: all(p[0] != pod for p in s[3])),
             first(ss, t0, hi, lambda s: s[1] != "" and not s[1].startswith(ip + ":")),
             first(ss, t0, hi, lambda s: not any(e[0] == ip and e[1] == "true" for e in s[2])),
             first(ss, t0, hi, lambda s: any(e[0] != ip and e[1] == "true" for e in s[2]))))
PY

# A baseline of successful puts on both paths before anything is deleted -- the loop prints one PUT
# line per attempt, so four seconds is a dozen samples per path.
sleep 4
BASE_A="$(grep -c '^PUT s=.* rc=0$' "$LOG_A" 2>/dev/null || true)"
BASE_B="$(grep -c '^PUT s=.* rc=0$' "$LOG_B" 2>/dev/null || true)"

# The serving Pod, confirmed as the Lease holder: the Pod deleted is the one the election names, not
# merely one that happens to be ready. Waits up to its argument in seconds.
serving_holder() {
  local secs="$1" i
  for ((i = 0; i < secs; i++)); do
    OLD_READY="$(ready_leader_pod)"
    OLD_READY_UID=""
    OLD_READY_IP=""
    if [ -n "$OLD_READY" ]; then
      OLD_READY_UID="$(kubectl -n "$NS" get pod "$OLD_READY" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
      OLD_READY_IP="$(kubectl -n "$NS" get pod "$OLD_READY" -o jsonpath='{.status.podIP}' 2>/dev/null)"
    fi
    OLD_HOLDER="$(lease_holder)"
    OLD_HOLDER_POD_UID="$(holder_pod_uid "$OLD_HOLDER")"
    [ -n "$OLD_READY_UID" ] && [ "$OLD_HOLDER_POD_UID" = "$OLD_READY_UID" ] && return 0
    sleep 1
  done
  return 1
}

# A round is only comparable to the one before it if it starts from the same state: the deleted Pod
# gone, every leader replica back and not terminating, and the serving one named by the Lease.
settled() {
  local secs="$1" gone="$2" i n
  for ((i = 0; i < secs; i += 2)); do
    n="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
      -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name},{.metadata.deletionTimestamp}{"\n"}{end}' \
      2>/dev/null | awk -F, -v gone="$gone" '$1 != gone && $2 == ""' | wc -l | tr -d ' ')"
    if [ "$n" = 3 ] && ! kubectl -n "$NS" get pod "$gone" >/dev/null 2>&1 && serving_holder 1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

ROUND_SPECS=()
for ((round = 1; round <= ROUNDS; round++)); do
  if ! serving_holder 10; then
    record FAIL "round ${round}: the Lease holder is the serving Pod immediately before the delete" \
      "holderIdentity='${OLD_HOLDER:-<empty>}' maps to '${OLD_HOLDER_POD_UID:-<none>}', ready Pod '${OLD_READY:-<none>}' has UID '${OLD_READY_UID:-<none>}'"
    break
  fi
  T0="$(python3 -c 'import time; print("%.3f" % time.time())')"
  if ! DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --wait=false 2>&1)"; then
    record FAIL "round ${round}: the serving Pod deletion is accepted" \
      "delete of ${OLD_READY} (${OLD_READY_UID}) failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
    break
  fi
  ROUND_SPECS+=("${T0},${OLD_READY},${OLD_READY_IP}")
  echo "[case-63] round ${round}: deleted ${OLD_READY} (${OLD_READY_IP}) at ${T0}"

  # Both paths recovered, or the round bound ran out; the verdict for either is read below.
  for ((i = 0; i < ROUND_BOUND; i += 2)); do
    python3 "$WORK/analyze.py" recovered "$T0" "$PUT_BOUND" "$ROUND_BOUND" "$LOG_A" "$LOG_B" && break
    sleep 2
  done

  if [ "$round" -lt "$ROUNDS" ] && ! settled "$SETTLE_BOUND" "$OLD_READY"; then
    record FAIL "round ${round}: the backend settles back to one serving leader and a full set of standbys" \
      "not settled ${SETTLE_BOUND}s after the round; the remaining rounds would not start from the same state"
    break
  fi
done

# The probes and the sampler are stopped here rather than waited for: every round has been read, and
# what they would print past this point belongs to no round.
kill "$PID_A" "$PID_B" "$PID_S" 2>/dev/null || true
wait 2>/dev/null || true

python3 "$WORK/analyze.py" report "$PUT_BOUND" "$ROUND_BOUND" "$LOG_A" "$LOG_B" "$LOG_SAMPLE" \
  ${ROUND_SPECS[@]+"${ROUND_SPECS[@]}"} >"$WORK/summary.txt" 2>&1 || true
cat "$WORK/summary.txt"

for ((round = 1; round <= ${#ROUND_SPECS[@]}; round++)); do
  TIMELINE="$(grep "^round ${round}: timeline" "$WORK/summary.txt")"
  for path in A B; do
    if [ "$path" = A ]; then
      title="Path A (member reads the Lease) converges after failover"
      base="${BASE_A:-0}"
      log="$LOG_A"
    else
      title="Path B (member keeps the Service address) converges after failover"
      base="${BASE_B:-0}"
      log="$LOG_B"
    fi
    detail="$(grep "^round ${round}: Path ${path}" "$WORK/summary.txt")"
    conv="$(echo "$detail" | awk '{for (i=1; i<=NF; i++) if ($i ~ /^convergence=/) print $i}' | cut -d= -f2)"
    case "$conv" in
      *s)
        record PASS "round ${round}: ${title}" \
          "put succeeds again ${conv} after the leader delete; baseline ${base} puts ok before the first round; \
detail: ${detail}; ${TIMELINE}" ;;
      NO_ERROR_WINDOW)
        record FAIL "round ${round}: ${title}" \
          "no failed or over-bound put was observed after the delete, so recovery was not measured; detail: ${detail}; log: ${log}" ;;
      DID_NOT_RECOVER*)
        # For Path B this is the answer the measurement exists for, and it is a FAIL of the check,
        # not of the run: Path B not converging means the member's Lease read is load-bearing and
        # the vendor-image rebuild work cannot be avoided.
        record FAIL "round ${round}: ${title}" \
          "no put within ${PUT_BOUND}s followed the first unreachable one within ${ROUND_BOUND}s of the delete -- the sample \
is 'still unreachable at the bound'; detail: ${detail}; ${TIMELINE}; log: ${log}" ;;
      *)
        record FAIL "round ${round}: ${title}" \
          "the measurement produced no convergence verdict; detail: ${detail:-<none>}; log: ${log}" ;;
    esac
  done
done
if [ "${#ROUND_SPECS[@]}" -lt "$ROUNDS" ]; then
  record FAIL "every requested failover round ran" "${#ROUND_SPECS[@]} of ${ROUNDS} rounds ran"
fi

FINAL_PHASE="$(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$FINAL_PHASE" = "Ready" ]; then
  record PASS "the backend is Ready after the measurement" "phase=Ready"
else
  record FAIL "the backend is Ready after the measurement" "phase='${FINAL_PHASE:-<absent>}'"
fi

results
