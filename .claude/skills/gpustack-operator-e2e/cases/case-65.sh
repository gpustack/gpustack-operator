#!/usr/bin/env bash
#
# CASE 65 — A member group's local disk tier takes real bytes: offloaded objects land in the
#   host directory and stay readable   (MUTATING, self-recovering;
#   AUTO-SKIPS without a prepared host directory on every node)
#
#   case-65.sh <NS>
#
# Goal:        The members[].localDisk + leader.offload pair renders a tier the leader accepts
#              and publishes, but a published capacity is not a written byte — the store has
#              been observed announcing the tier while every offload stayed "deferred" forever.
#              The one figure that answers whether the tier is real is the leader's own
#              master_allocated_file_size_bytes: bytes actually written to disk. This case
#              writes known content, then asserts that figure is non-zero, that files exist in
#              the host directory, and that a read returns the bytes that were written. With
#              offload_on_evict unset the store's contract is write-through at put time, so a
#              zero after successful puts needs no eviction to be a verdict.
#
# Environment: Any cluster whose nodes share one prepared host directory for the tier.
#              AUTO-SKIPS (exit 0) unless E2E_LOCALDISK_HOST_PATH (default
#              /mnt/kvcache-localdisk) is a writable directory on EVERY Ready node — creating
#              or formatting that directory is the deployer's, deliberately: the operator does
#              not create or chown it either, and a case that silently landed the tier on the
#              root filesystem would be verifying the wrong disk. On an i7ie node the prep is:
#              mkfs.xfs /dev/nvme1n1 && mount it at the path && chmod 0777. No GPU, no RDMA
#              (members are DRAM over TCP). Needs the store image with the client bundled
#              (E2E_MOONCAKE_IMAGE; the default carries it).
#
# Inputs:      All real, nothing mocked. One KVCacheBackend with leader.offload.enabled=true
#              and one member group carrying localDisk{path, capacity}; memory per member is
#              sized small (256Mi) so eviction pressure is cheap to induce. A probe Pod (the
#              store image's bundled python client, P2P handshake against the leader Service)
#              writes 4MiB objects of known content. Per-node host-directory checks run as
#              nodeName-pinned probe Pods mounting the path read-write.
#
# Expected:    - the backend reaches Ready with the local disk segment registered;
#              - writes succeed (rc=0) and reads return the bytes written;
#              - master_allocated_file_size_bytes on the leader reads > 0 after the writes —
#                NOT status.capacity, which reports the declared figure and cannot tell an
#                empty tier from a full one;
#              - the host directory on at least one node holds files.
#
#              KNOWN-FAILURE DETECTOR: against mooncake 0.3.13 the last two assertions FAIL —
#              the tier is announced (12 GB published, offload RPC servers up) and nothing is
#              ever written. The case is kept failing on purpose: its two FAILs are the
#              regression alarm for the day the tier starts working, and its log carries every
#              intermediate signal.
#
# Cleanup:     Trap deletes the KVCacheBackend (owner references cascade) and every probe Pod,
#              and best-effort removes the files the store wrote under the host directory on
#              each node. The directory itself — mount and all — is the deployer's and is left
#              exactly as found. Idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-65.sh <NS>}"
CASE_ID=65
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"
HOST_PATH="${E2E_LOCALDISK_HOST_PATH:-/mnt/kvcache-localdisk}"

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-ld-${SFX}"

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

NODES="$(kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready" {print $1}')"

teardown() {
  echo
  echo "[case-65] cleanup"
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod -l "gpustack-e2e-case=65-${SFX}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # The directory is the deployer's; the files the store wrote into it are this case's.
  for node in $NODES; do
    kubectl -n "$NS" run "case65-wipe-${SFX}-$(echo "$node" | cut -c12-17 | tr -d -)" --restart=Never --rm -i --quiet \
      --image=busybox:1.36 --overrides='{"spec":{"nodeName":"'"$node"'","containers":[{"name":"wipe","image":"busybox:1.36","command":["sh","-c","rm -rf /tier/* 2>/dev/null; true"],"volumeMounts":[{"name":"tier","mountPath":"/tier"}]}],"volumes":[{"name":"tier","hostPath":{"path":"'"$HOST_PATH"'","type":"Directory"}}]}}' \
      >/dev/null 2>&1 || true
  done
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

# ------------------------------------------------- 0. the host directory is really there

if [ -z "$NODES" ]; then
  echo "[case-65] SKIP: no Ready node"
  exit 0
fi

PREP_OK=1
for node in $NODES; do
  short="$(echo "$node" | cut -c12-17 | tr -d -)"
  out="$(kubectl -n "$NS" run "case65-pre-${SFX}-${short}" --restart=Never --rm -i --quiet \
    --image=busybox:1.36 --overrides='{"spec":{"nodeName":"'"$node"'","containers":[{"name":"pre","image":"busybox:1.36","command":["sh","-c","touch /tier/.case65-probe && rm /tier/.case65-probe && echo WRITABLE"],"volumeMounts":[{"name":"tier","mountPath":"/tier"}]}],"volumes":[{"name":"tier","hostPath":{"path":"'"$HOST_PATH"'","type":"Directory"}}]}}' \
    2>/dev/null)"
  if [ "$out" != "WRITABLE" ]; then
    PREP_OK=0
    echo "[case-65] SKIP: node $node has no writable host directory at $HOST_PATH"
    echo "  prep on each node: mkfs.xfs /dev/nvme1n1 && mkdir -p $HOST_PATH && mount /dev/nvme1n1 $HOST_PATH && chmod 0777 $HOST_PATH"
    break
  fi
done
[ "$PREP_OK" = "1" ] || exit 0
record PASS "the host directory is writable on every Ready node" "$(echo $NODES | wc -w | tr -d ' ') node(s) at $HOST_PATH"

# ------------------------------------------------- 1. the backend with a disk tier

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
        offload: {enabled: true}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 256Mi
          localDisk:
            path: ${HOST_PATH}
            capacity: 4Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend with a disk tier reaches Ready" \
    "phase never became Ready in 300s: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  results; exit 1
fi
record PASS "the backend with a disk tier reaches Ready" "phase=Ready for ${BACKEND}"

# The managed workloads live in the OPERATOR's namespace, not necessarily $NS — the only
# trustworthy address is the one the controller published in status.endpoints.
CLIENT_ADDR="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.endpoints[?(@.name=="Client")].address}' 2>/dev/null)"
ADMIN_ADDR="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.endpoints[?(@.name=="Admin")].address}' 2>/dev/null)"
if [ -z "$CLIENT_ADDR" ] || [ -z "$ADMIN_ADDR" ]; then
  record FAIL "status.endpoints publishes Client and Admin addresses" "Client='${CLIENT_ADDR}' Admin='${ADMIN_ADDR}'"
  results; exit 1
fi
record PASS "status.endpoints publishes Client and Admin addresses" "${CLIENT_ADDR} / ${ADMIN_ADDR}"

# ------------------------------------------------- 2. write known content, read it back

# The probe is the store image's own bundled client; the setup call shape (P2P handshake against
# the leader Service, pod IP as the dial-back address, a real staging buffer) is the one the
# suite's quota case measured. Content is a fixed pattern so a read-back can be compared, not
# just sized.
PROBE_LOG="$(mktemp)"
cat <<PY | kubectl -n "$NS" run "case65-probe-${SFX}" --image="$IMAGE" --restart=Never \
  --labels="gpustack-e2e-case=65-${SFX}" \
  --overrides='{"spec":{"containers":[{"name":"probe","image":"'"$IMAGE"'","command":["python3","-"],"stdin":true,"stdinOnce":true}]}}' \
  -i --rm --quiet >"$PROBE_LOG" 2>&1 || true
import sys, socket, hashlib
try:
    from mooncake.store import MooncakeDistributedStore
except Exception as e:
    print("IMPORT-FAIL %s" % e); sys.exit(0)

store = MooncakeDistributedStore()
ip = socket.gethostbyname(socket.gethostname())
try:
    rc = store.setup("%s:0" % ip, "P2PHANDSHAKE", 0, 64 * 1024 * 1024, "tcp", "",
                     "${CLIENT_ADDR}")
except TypeError as e:
    print("SETUP-TYPEERROR %s" % e); sys.exit(0)
print("SETUP-RC %d" % rc)
if rc != 0:
    sys.exit(0)

payload = (b"case65-localdisk!" * (4 * 1024 * 1024 // 17))[:4 * 1024 * 1024]
digest = hashlib.sha256(payload).hexdigest()

# Under the memory segment: 4 x 4MiB against 256Mi per member. These must write cleanly and,
# with offload on store rather than on evict, should already have reached the tier.
for i in range(4):
    print("PUT warm-%d rc=%d" % (i, store.put("warm-%d" % i, payload)))

# Within aggregate memory, past one member's share: another 128 x 4MiB = 512Mi of distinct keys.
# The three members hold 768Mi between them, so this fits in DRAM — with write-through offload
# every put should reach the tier regardless; no eviction is forced or claimed.
for i in range(128):
    rc = store.put("fill-%d" % i, payload)
    if rc != 0:
        print("PUT fill-%d rc=%d (first failure)" % (i, rc))
        break
else:
    print("PUT fill all 128 rc=0")

# The verdict read: warm-0 is the oldest key still expected to be served (from memory or tier).
got = store.get("warm-0")
if got is None:
    print("GET warm-0 rc=NotFound")
else:
    print("GET warm-0 len=%d sha256=%s" % (len(got), hashlib.sha256(got).hexdigest()))
print("WANT sha256=%s len=%d" % (digest, len(payload)))
PY

grep -q 'SETUP-RC 0' "$PROBE_LOG" \
  && record PASS "the probe's client set up against the leader" "$(grep 'SETUP-RC' "$PROBE_LOG")" \
  || record FAIL "the probe's client set up against the leader" "$(grep -E 'SETUP|IMPORT-FAIL|Error|error' "$PROBE_LOG" | head -3 | tr '\n' ' ')"

if grep -q 'PUT fill all 128 rc=0' "$PROBE_LOG" && ! grep -qE 'PUT warm-[0-9] rc=-?[1-9]' "$PROBE_LOG"; then
  record PASS "writes succeed within aggregate memory" "$(grep -c 'rc=0' "$PROBE_LOG") puts rc=0, fill complete"
else
  record FAIL "writes succeed within aggregate memory" "$(grep 'PUT' "$PROBE_LOG" | tail -3 | tr '\n' ' ')"
fi

WANT="$(grep '^WANT' "$PROBE_LOG")"
if grep '^GET warm-0' "$PROBE_LOG" | grep -q "$(echo "$WANT" | awk '{print $2}')"; then
  record PASS "a written object reads back byte-identical" "$(grep '^GET warm-0' "$PROBE_LOG")"
else
  record FAIL "a written object reads back byte-identical" \
    "get: $(grep '^GET warm-0' "$PROBE_LOG") vs $WANT"
fi

# ------------------------------------------------- 3. the verdict figure: bytes actually on disk

# master_allocated_file_size_bytes is the leader's own count of bytes written to the file tier.
# status.capacity cannot stand in for it: that one reports the DECLARED figure, which reads the
# same for an empty tier as for a full one.
METRIC="$(kubectl -n "$NS" run "case65-metrics-${SFX}" --image=busybox:1.36 --restart=Never --rm -i --quiet \
  --labels="gpustack-e2e-case=65-${SFX}" \
  -- sh -c "wget -qO- http://${ADMIN_ADDR}/metrics | grep '^master_allocated_file_size_bytes'" 2>/dev/null)"
METRIC_VAL="$(echo "$METRIC" | awk '{print $2}' | cut -d. -f1)"
if [ -n "$METRIC_VAL" ] && [ "$METRIC_VAL" -gt 0 ] 2>/dev/null; then
  record PASS "the leader reports bytes actually written to the file tier" \
    "master_allocated_file_size_bytes=${METRIC_VAL} (> 0)"
else
  record FAIL "the leader reports bytes actually written to the file tier" \
    "master_allocated_file_size_bytes='${METRIC:-<absent>}' -- a zero here with healthy writes above is the \
known 'tier announced, nothing lands' failure shape: the leader defers offload forever while publishing capacity"
fi

# ------------------------------------------------- 4. files on the host, not just a metric

FOUND=""
for node in $NODES; do
  short="$(echo "$node" | cut -c12-17 | tr -d -)"
  out="$(kubectl -n "$NS" run "case65-ls-${SFX}-${short}" --restart=Never --rm -i --quiet \
    --image=busybox:1.36 --overrides='{"spec":{"nodeName":"'"$node"'","containers":[{"name":"ls","image":"busybox:1.36","command":["sh","-c","ls /tier | head -5; ls /tier | wc -l"],"volumeMounts":[{"name":"tier","mountPath":"/tier"}]}],"volumes":[{"name":"tier","hostPath":{"path":"'"$HOST_PATH"'","type":"Directory"}}]}}' \
    2>/dev/null)"
  n="$(echo "$out" | tail -1)"
  [ "${n:-0}" -gt 0 ] 2>/dev/null && FOUND="$FOUND $node:${n}files"
done
if [ -n "$FOUND" ]; then
  record PASS "offloaded objects exist as files in the host directory" "$(echo $FOUND)"
else
  record FAIL "offloaded objects exist as files in the host directory" \
    "no files under $HOST_PATH on any node -- the metric and the directory disagree with the writes"
fi

rm -f "$PROBE_LOG"
results
