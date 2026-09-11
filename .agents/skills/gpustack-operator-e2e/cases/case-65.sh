#!/usr/bin/env bash
#
# CASE 65 — A member group's local disk tier takes real bytes: offloaded objects land in the
#   host directory and stay readable, on BOTH write paths the store offers   (MUTATING,
#   self-recovering; AUTO-SKIPS without a prepared host directory on every node)
#
#   case-65.sh <NS>
#
# Goal:        The members[].localDisk + leader.offload pair renders a tier the leader accepts
#              and publishes, but a published capacity is not a written byte — the store has
#              been observed announcing the tier while every offload stayed "deferred" forever.
#              The one figure that answers whether the tier is real is the leader's own
#              master_allocated_file_size_bytes: bytes actually written to disk. TWO THINGS
#              HAVE TO HOLD BEFORE A ZERO THERE IS A VERDICT, and this case establishes both
#              rather than assuming them. The store writes nothing until a BUCKET fills, so a
#              tier offered less than one bucket reads zero while working perfectly — the case
#              reads the rendered limit out of the member container and asserts its write set
#              clears one bucket PER MEMBER, since each member client fills its own while the
#              gauge sums the backend. And the figure follows a bucket being closed rather than
#              a put returning, so
#              it is asked for repeatedly until it carries a number instead of once. This case
#              writes known content, then asserts that figure is non-zero, that files exist in
#              the host directory, and that a read returns the bytes that were written. With
#              offload_on_evict unset the store's contract is write-through at put time, so a
#              zero after successful puts needs no eviction to be a verdict. A second backend
#              with offload.onEvict=true then answers WHICH path is broken: it defers the disk
#              write from put time to eviction time, and this case pushes past aggregate memory
#              so eviction is forced. Bytes landing there means only the write-through path is
#              broken; a zero there too means both paths are broken.
#
# Environment: Any cluster whose nodes share one prepared host directory for the tier.
#              AUTO-SKIPS (exit 0) unless E2E_LOCALDISK_HOST_PATH (default
#              /mnt/kvcache-localdisk) is a writable directory on EVERY Ready node — creating
#              or formatting that directory is the deployer's, deliberately: the operator does
#              not create or chown it either, and a case that silently landed the tier on the
#              root filesystem would be verifying the wrong disk. On an i7ie node the prep is:
#              mkfs.xfs /dev/nvme1n1 && mount it at the path && chmod 0777 — and if the prep
#              runs from a `kubectl debug` pod, the MOUNT must be made in the host's mount
#              namespace (`chroot /host nsenter -t 1 -m -- mount …`): a mount made in the
#              debug container's own namespace dies with the pod, and the hostPath then lands
#              on the node's root filesystem while everything still looks prepared. No GPU, no
#              RDMA (members are DRAM over TCP). Needs the store image with the client bundled
#              (E2E_MOONCAKE_IMAGE; the default carries it).
#
# Inputs:      All real, nothing mocked. Two KVCacheBackends, same shape apart from the leader's
#              offload mode: phase A with leader.offload.enabled=true (write-through), phase B
#              with onEvict=true on top. Both carry one member group with
#              localDisk{path, capacity}; memory per member is sized small (256Mi) so eviction
#              pressure is cheap to induce. Probe Pods (the store image's bundled python client,
#              P2P handshake against the leader Service) write 4MiB objects of known content.
#              Per-node host-directory checks run as nodeName-pinned probe Pods mounting the
#              path read-write.
#
# Expected:    Phase A (write-through, offload.onEvict unset):
#              - the backend reaches Ready with the local disk segment registered;
#              - writes succeed (rc=0) and reads return the bytes written;
#              - the rendered bucket limit is present in the member container and the write set
#                clears it — without this the next assertion could not be read as a verdict;
#              - master_allocated_file_size_bytes on the leader reads > 0 once it has settled —
#                NOT status.capacity, which reports the declared figure and cannot tell an
#                empty tier from a full one;
#              - the host directory on at least one node holds files.
#              Phase B (the discriminator, offload.onEvict=true, writes pushed past aggregate
#              memory so eviction is forced):
#              - every write lands, INCLUDING the ones the store first refuses for want of
#                memory: that refusal is the moment the master flags the eviction this phase
#                exists to force, so the put is retried rather than taken for a stop;
#              - BEFORE any read, the leader reports keys holding a local disk replica and no
#                memory replica — without that, bytes coming back proves only that the store
#                works, not that the TIER served them;
#              - one of THOSE keys, chosen from that answer rather than named in advance, then
#                reads back byte-identical against a payload derived from its own key, and the
#                on-evict leader's metric reads > 0.
#
#              NO LONGER A KNOWN-FAILURE DETECTOR, and the reason is worth keeping because it
#              is the same reason the case exists. This case used to be documented as expected
#              to FAIL: the tier was announced and nothing was ever written. That was never the
#              store refusing to write — it was the write set never closing a BUCKET. The store
#              defaults a bucket to 256 MB, phase A offers less than that per member, and below
#              one bucket a tier legitimately holds nothing. Once the operator began rendering
#              the bucket limit the same write set closes eleven of them. Measured green end to
#              end, both phases, on one member of 256Mi with a rendered 16 MiB bucket. So a FAIL
#              here is now a regression to act on rather than the expected reading — but read
#              the bucket precondition row first, because it is what entitles the rows after it
#              to be read as verdicts at all.
#
# Cleanup:     Trap deletes both KVCacheBackends (owner references cascade) and every probe Pod,
#              then removes this run's subdirectory from every node that is Ready at teardown — or,
#              when the prep check did not pass and so nothing was deployed, only from the nodes
#              whose probe reported the directory writable. The prepared parent directory and other
#              runs' contents are untouched. Removing the subdirectory whole includes dotfiles.
#              Idempotent, runs on pass AND fail, safe to re-run and safe beside another case-65 run
#              using the same parent directory.
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
HOST_PATH="${HOST_PATH%/}"

# Two bounds, over two different waits. Keeping them apart is the point of the two names: a change to
# either one must not silently move the other.
#
# PROBE_TIMEOUT is for the probes that mount the tier as hostPath type Directory. The kubelet cannot
# satisfy that mount when the directory is absent: the Pod stays in ContainerCreating forever, so an
# unbounded `kubectl run --rm -i` waits on a Pod that will never start. This bound turns that wait
# into the empty reading the skip branch below is written to read, and `--rm` still deletes the Pod
# it gave up on. It is generous because a cold image pull happens inside the same wait, and a bound
# short enough to fire on a slow pull would report a prepared directory as missing.
PROBE_TIMEOUT=120s

# PULL_TIMEOUT is for the probes that mount nothing, where an absent directory is not among the
# failure modes and the image pull IS what is being waited on. It errs LONG deliberately: a bound
# that fires on a pull that would have succeeded reports a slow registry as a failed assertion about
# the operator, which is a wrong verdict, while one that is too long only costs time and the case
# still reports. By the time any of them runs, this case has already brought its image up on the
# cluster -- the store image as a member, busybox as the probe above -- so the bound covers the
# uncommon path, a probe placed where that image is not cached yet, rather than a first pull.
PULL_TIMEOUT=300s

# REQUIRED, and it REFUSES rather than skipping. The case creates its run directory below this
# prepared parent, and a root or top-level path is not the dedicated tier shape this case claims to
# verify. A SKIP would hide a dangerous value behind the same exit code as an unprepared cluster,
# while that value is a mistake to correct. Trailing slashes are stripped first so "/mnt/" is
# judged as the top-level directory it names.
case "$HOST_PATH" in
  *..*)
    echo "[case-65] REFUSE: E2E_LOCALDISK_HOST_PATH=\"$HOST_PATH\" contains \"..\"; the run directory parent must be unambiguous" >&2
    exit 1
    ;;
  /*/*) ;;
  *)
    echo "[case-65] REFUSE: E2E_LOCALDISK_HOST_PATH=\"$HOST_PATH\" must be an absolute path at least two segments deep, such as /mnt/kvcache-localdisk" >&2
    exit 1
    ;;
esac

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-ld-${SFX}"
BACKEND2="kvcb-oe-${SFX}"
RUN_DIR="case-65-${SFX}"
RUN_HOST_PATH="${HOST_PATH}/${RUN_DIR}"

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

ready_nodes() {
  kubectl get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
    2>/dev/null | awk -F'|' '$2=="True" {print $1}'
}

# ACROSS NAMESPACES, and not in $NS. A KVCacheBackend is cluster-scoped but the workloads it renders
# land in the OPERATOR's namespace, which is not the namespace this case was handed -- the same fact
# the endpoint lookup below already works around. Searched in $NS this returns zero for a healthy
# backend whenever the two differ, and zero members is indistinguishable here from a backend whose
# members never started. The label selector carries the backend's own name, which is unique cluster
# wide, so widening the search cannot pick up another backend's Pods.
running_member_count() {
  local backend="$1"
  kubectl get pod -A \
    -l "app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${backend},app.kubernetes.io/component=member-0" \
    -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | awk 'NF {n++} END {print n+0}'
}

NODES="$(ready_nodes)"

# A probe Pod is named after the node it pins to, so the tag must be UNIQUE PER NODE. A slice of
# the node name is not: chars 12-17 of ip-10-0-1-123.ec2.internal and of ip-10-0-2-123.ec2.internal
# are both "23.ec2", and EKS hands out names of exactly that shape.
#
# The probes run SERIALLY and each is --rm, so a shared name does not normally race. It bites when
# a previous Pod outlives its delete -- a timeout, an interrupt: the next `kubectl run` hits
# AlreadyExists, the error goes to /dev/null, and that node contributes NO READING. A bound that
# expires is the other way to get there, so this is a shape rather than a single cause and an
# enumeration of causes here would go stale without saying so. On the ls probe a node with no
# reading is one missing from the file count this case reads its verdict from, and "no files
# anywhere" is exactly the answer a node that never answered also produces -- which is why that
# loop keeps the two apart per node instead of counting.
node_tag() { printf '%s' "$1" | cksum | cut -d' ' -f1; }

teardown() {
  echo
  echo "[case-65] cleanup"
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" "$BACKEND2" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod -l "gpustack-e2e-case=65-${SFX}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # The parent is the deployer's; this run owns only its named child. Re-read the nodes so one that
  # became Ready during the case is cleaned too. When the prep check did not pass there is nothing to
  # re-read for: no backend was ever created, so the only child directories that exist are the ones
  # the probe itself made, and asking a node that cannot mount the parent would spend the probe bound
  # again per node -- turning the skip this case owes the operator into a long silent wait.
  local wipe_nodes
  if [ "${PREP_OK:-0}" = 1 ]; then wipe_nodes="$(ready_nodes)"; else wipe_nodes="${PREPARED_NODES:-}"; fi
  for node in $wipe_nodes; do
    kubectl -n "$NS" run "case65-wipe-${SFX}-$(node_tag "$node")" --restart=Never --rm -i --quiet \
      --pod-running-timeout="$PROBE_TIMEOUT" \
      --image=busybox:1.36 --overrides='{"spec":{"nodeName":"'"$node"'","containers":[{"name":"wipe","image":"busybox:1.36","command":["sh","-c","rm -rf /tier/'"$RUN_DIR"' 2>/dev/null; true"],"volumeMounts":[{"name":"tier","mountPath":"/tier"}]}],"volumes":[{"name":"tier","hostPath":{"path":"'"$HOST_PATH"'","type":"Directory"}}]}}' \
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
PREPARED_NODES=""
for node in $NODES; do
  short="$(node_tag "$node")"
  out="$(kubectl -n "$NS" run "case65-pre-${SFX}-${short}" --restart=Never --rm -i --quiet \
    --pod-running-timeout="$PROBE_TIMEOUT" \
    --image=busybox:1.36 --overrides='{"spec":{"nodeName":"'"$node"'","containers":[{"name":"pre","image":"busybox:1.36","command":["sh","-c","mkdir -p /tier/'"$RUN_DIR"' && chmod 0777 /tier/'"$RUN_DIR"' && touch /tier/'"$RUN_DIR"'/.hidden && echo WRITABLE"],"volumeMounts":[{"name":"tier","mountPath":"/tier"}]}],"volumes":[{"name":"tier","hostPath":{"path":"'"$HOST_PATH"'","type":"Directory"}}]}}' \
    2>/dev/null)"
  # One LINE of the output, not the whole of it: this container exits before kubectl can attach, so
  # kubectl falls back to streaming the logs and the probe's single line arrives more than once. An
  # equality test reads that repetition as a failed prep and skips a node that is prepared.
  if ! printf '%s\n' "$out" | grep -qx WRITABLE; then
    PREP_OK=0
    echo "[case-65] SKIP: node $node has no writable host directory at $HOST_PATH (probe waited up to $PROBE_TIMEOUT)"
    echo "  prep on each node: mkfs.xfs /dev/nvme1n1 && mkdir -p $HOST_PATH && mount /dev/nvme1n1 $HOST_PATH && chmod 0777 $HOST_PATH"
    break
  fi
  PREPARED_NODES="$PREPARED_NODES $node"
done
[ "$PREP_OK" = "1" ] || exit 0
record PASS "the run directory is prepared on every Ready node" "$(echo $NODES | wc -w | tr -d ' ') node(s) at $RUN_HOST_PATH"

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
            path: ${RUN_HOST_PATH}
            capacity: 4Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend with a disk tier reaches Ready" \
    "phase never became Ready in 300s: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  results; exit 1
fi
record PASS "the backend with a disk tier reaches Ready" "phase=Ready for ${BACKEND}"

MEMBER_COUNT="$(running_member_count "$BACKEND")"
if [ "$MEMBER_COUNT" -le 0 ] 2>/dev/null; then
  record FAIL "running members exist to size the write set" "no Running member Pod belongs to ${BACKEND}"
  results; exit 1
fi
# Preserve the measured three-member set of 132 objects while keeping one member below its 256Mi
# capacity: 44 objects of 4MiB per running member, four warm keys and the rest fill keys.
PHASE_A_TOTAL=$((MEMBER_COUNT * 44))
PHASE_A_FILL=$((PHASE_A_TOTAL - 4))

# ------------------------------------------------- the precondition every figure below rests on
#
# The store writes nothing until a bucket is full, so a tier offered less than one bucket holds
# nothing LEGITIMATELY and the verdict figure reads zero on a tier that is working. This case is
# only entitled to read that figure as a verdict because the write set above clears the bucket
# limit -- so the limit has to be read from the container rather than assumed, and compared.
#
# It is read WITH a companion variable that must be present whenever the tier is rendered at all.
# The two together separate three states a single read cannot: a probe that did not answer, a
# limit that never arrived (leaving the store on its own 256MB default, which this write set would
# NOT clear), and a limit that is there. An empty reading alone is the shape that would otherwise
# arrive as "the tier is empty" -- the failure this whole case exists to report.
MEMBER_REF="$(kubectl get pod -A \
  -l "app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=member-0" \
  -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' 2>/dev/null | head -1)"
MEMBER_NS="${MEMBER_REF%% *}"
MEMBER_POD="${MEMBER_REF##* }"
TIER_ENV="$(kubectl -n "$MEMBER_NS" exec "$MEMBER_POD" -- sh -c \
  'printf "%s|%s" "${MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES-}" "${MOONCAKE_OFFLOAD_FILE_STORAGE_PATH-}"' 2>/dev/null)"
BUCKET_LIMIT="${TIER_ENV%%|*}"
TIER_PATH_ENV="${TIER_ENV##*|}"
# PER MEMBER, and that is the whole point of dividing here. Each member client fills ITS OWN bucket
# from the objects that land on it, so a write set that clears one bucket in aggregate can leave
# every member short of the threshold and close nothing -- while the gauge, which sums the backend,
# still reads zero. Dividing by the member count is the conservative form: it asks that an EVEN
# spread would still close a bucket, and an uneven one only makes some member cross it sooner.
PHASE_A_BYTES_PER_MEMBER=$(((PHASE_A_TOTAL * 4 * 1024 * 1024) / MEMBER_COUNT))
case "$BUCKET_LIMIT" in
  '' | *[!0-9]*)
    if [ -z "$TIER_PATH_ENV" ]; then
      record FAIL "the rendered bucket limit reached the member container" \
        "no reading from ${MEMBER_POD:-<no member pod>}: neither the limit nor the storage path came \
back, so this run did not measure the environment and makes no claim about the limit"
    else
      record FAIL "the rendered bucket limit reached the member container" \
        "MOONCAKE_OFFLOAD_FILE_STORAGE_PATH='${TIER_PATH_ENV}' but \
MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES='${BUCKET_LIMIT}' -- the tier is rendered and the limit is \
not, so the store keeps its own 256MB default and this write set would close no bucket"
    fi
    results; exit 1
    ;;
esac
if [ "$PHASE_A_BYTES_PER_MEMBER" -lt "$BUCKET_LIMIT" ] 2>/dev/null; then
  record FAIL "the write set clears one bucket per member" \
    "phase A offers ${PHASE_A_BYTES_PER_MEMBER} bytes per member across ${MEMBER_COUNT} member(s) \
against a bucket limit of ${BUCKET_LIMIT} -- below one bucket each, the tier holds nothing \
legitimately, so the figures below could not be read as a verdict"
  results; exit 1
fi
record PASS "the write set clears one bucket per member" \
  "phase A offers ${PHASE_A_BYTES_PER_MEMBER} bytes per member across ${MEMBER_COUNT} member(s), \
against MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES=${BUCKET_LIMIT}"

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
  --pod-running-timeout="$PULL_TIMEOUT" \
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

payload = (b"case65-localdisk!" * (4 * 1024 * 1024 // 17 + 1))[:4 * 1024 * 1024]
digest = hashlib.sha256(payload).hexdigest()

# Under the memory segment: 4 x 4MiB against 256Mi per member. These must write cleanly and,
# with offload on store rather than on evict, should already have reached the tier.
for i in range(4):
    print("PUT warm-%d rc=%d" % (i, store.put("warm-%d" % i, payload)))

# Within aggregate memory for the members that are actually running. With write-through offload
# every put should reach the tier regardless; no eviction is forced or claimed.
for i in range(${PHASE_A_FILL}):
    rc = store.put("fill-%d" % i, payload)
    if rc != 0:
        print("PUT fill-%d rc=%d (first failure)" % (i, rc))
        break
else:
    print("PUT fill all ${PHASE_A_FILL} rc=0")

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

if grep -q "PUT fill all ${PHASE_A_FILL} rc=0" "$PROBE_LOG" && ! grep -qE 'PUT warm-[0-9] rc=-?[1-9]' "$PROBE_LOG"; then
  record PASS "writes succeed within aggregate memory" \
    "${PHASE_A_TOTAL} objects for ${MEMBER_COUNT} running member(s), all puts rc=0"
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
# The figure follows a bucket being CLOSED AND WRITTEN, not a put returning, and the master learns
# of it from the member rather than at the same instant. A single read taken here therefore reads
# zero on a tier that is working -- and on a fresh tier that zero is exactly the value this case
# treats as its known failure. So it is asked for repeatedly until it carries a number.
#
# ⛔ This is NOT "wait and it will come": below one bucket's worth the wait would never end, because
# nothing is due. It is legitimate here only because the assertion above proved this write set
# clears the rendered limit. Each attempt gets its own Pod name -- the previous one is still being
# reclaimed, and a name collision returns no reading, which is the shape that must not be read as
# a zero.
METRIC=""
METRIC_VAL=""
SETTLE=0
while [ "$SETTLE" -lt 6 ]; do
  SETTLE=$((SETTLE + 1))
  METRIC="$(kubectl -n "$NS" run "case65-metrics-${SFX}-${SETTLE}" --image=busybox:1.36 --restart=Never --rm -i --quiet \
    --pod-running-timeout="$PULL_TIMEOUT" \
    --labels="gpustack-e2e-case=65-${SFX}" \
    -- sh -c "wget -qO- http://${ADMIN_ADDR}/metrics | grep '^master_allocated_file_size_bytes'" 2>/dev/null)"
  # ONE LINE of the probe's output, for the same reason the prep probe above takes one: this
  # container exits before kubectl can attach, kubectl falls back to streaming the logs, and the
  # single line the probe printed arrives more than once. Two identical lines make awk emit two
  # numbers, and every numeric test below then rejects the pair -- so a tier holding real bytes
  # reads here as a figure that is not a number, which the branch that follows would report as the
  # very failure this case exists to detect.
  METRIC_VAL="$(echo "$METRIC" | head -1 | awk '{print $2}' | cut -d. -f1)"
  case "$METRIC_VAL" in
    '' | *[!0-9]*) ;;
    *) [ "$METRIC_VAL" -gt 0 ] && break ;;
  esac
  [ "$SETTLE" -lt 6 ] && sleep 10
done
if [ -n "$METRIC_VAL" ] && [ "$METRIC_VAL" -gt 0 ] 2>/dev/null; then
  record PASS "the leader reports bytes actually written to the file tier" \
    "master_allocated_file_size_bytes=${METRIC_VAL} (> 0)"
elif [ -z "$METRIC" ]; then
  # NO READING is not a zero, and the branch below would file it as one -- under the store's known
  # failure shape, for the single figure this case exists to report. A probe that never started, a
  # leader that never answered and an absent gauge all arrive here as an empty string, so this says
  # what it knows and stops there.
  record FAIL "the leader reports bytes actually written to the file tier" \
    "the metrics probe returned no reading within ${PULL_TIMEOUT}; an absent reading is not a zero, \
so this run makes no claim about the tier either way"
else
  case "$METRIC_VAL" in
    '' | *[!0-9]*)
      # A reading arrived and is not a number. Kept apart from the zero below because this case's
      # whole verdict is that a zero means something, and a figure this run could not read is not
      # one -- filing it as one would report the known failure shape off a parse.
      record FAIL "the leader reports bytes actually written to the file tier" \
        "master_allocated_file_size_bytes came back unparsable after ${SETTLE} reads: '${METRIC}' \
-- a reading this run could not read is not a zero, so it makes no claim about the tier"
      ;;
    *)
      record FAIL "the leader reports bytes actually written to the file tier" \
        "master_allocated_file_size_bytes=${METRIC_VAL} after ${SETTLE} reads over $(( (SETTLE - 1) * 10 ))s \
-- a zero that does not move, with healthy writes above and a write set proven to clear one bucket, \
is the known 'tier announced, nothing lands' failure shape: the leader defers offload forever while \
publishing capacity"
      ;;
  esac
fi

# ------------------------------------------------- 4. files on the host, not just a metric

FOUND=""
SILENT=""
for node in $NODES; do
  short="$(node_tag "$node")"
  out="$(kubectl -n "$NS" run "case65-ls-${SFX}-${short}" --restart=Never --rm -i --quiet \
    --pod-running-timeout="$PROBE_TIMEOUT" \
    --image=busybox:1.36 --overrides='{"spec":{"nodeName":"'"$node"'","containers":[{"name":"ls","image":"busybox:1.36","command":["sh","-c","ls /tier | head -5; ls /tier | wc -l"],"volumeMounts":[{"name":"tier","mountPath":"/tier"}]}],"volumes":[{"name":"tier","hostPath":{"path":"'"$RUN_HOST_PATH"'","type":"Directory"}}]}}' \
    2>/dev/null)"
  n="$(echo "$out" | tail -1)"
  # Each node contributes a STATE, not a number, and the state that matters is the one with no
  # number in it. Every way this probe fails to answer -- the name still held by a previous Pod, the
  # bound expiring, an unparsable last line -- leaves n empty or non-numeric, and a count that
  # defaulted that to zero would enter the verdict below as a node that looked and found nothing.
  # The distinction has to be kept HERE: after the loop there is only a total, and a total cannot
  # say which nodes it was taken over.
  case "$n" in
    '' | *[!0-9]*) SILENT="$SILENT $node" ;;
    0) ;;
    *) FOUND="$FOUND $node:${n}files" ;;
  esac
done
if [ -n "$FOUND" ]; then
  # Files anywhere settle an existential claim, so a silent node cannot overturn it -- but it is
  # named, because otherwise a partial survey reads as a complete one.
  record PASS "offloaded objects exist as files in the host directory" \
    "$(echo $FOUND)${SILENT:+ (no answer from$SILENT)}"
elif [ -n "$SILENT" ]; then
  record FAIL "offloaded objects exist as files in the host directory" \
    "no file count from$SILENT -- the probe did not answer there, so this run did not measure the \
directory and cannot say whether the writes reached it"
else
  record FAIL "offloaded objects exist as files in the host directory" \
    "no files under $RUN_HOST_PATH on any node -- the metric and the directory disagree with the writes"
fi

# ------------------------------------------------- 5. phase B: the on-evict discriminator

# Phase A's zero cannot say WHICH write path is broken — the store has two: write-through at put
# time (above), and eviction-time (leader.offload.onEvict=true). This phase reruns the same
# backend shape with onEvict set and pushes one member's capacity past the aggregate memory of the
# members actually running, so eviction is forced, not hoped for. The warm keys go in first, so
# they are what eviction must push to the tier; reading one back exercises the disk tier's serve
# path, not just its write path.
kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND2}
spec:
  type: Mooncake
  image: ${IMAGE}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        offload: {enabled: true, onEvict: true}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 256Mi
          localDisk:
            path: ${RUN_HOST_PATH}
            capacity: 4Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND2" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the on-evict backend reaches Ready" \
    "phase never became Ready in 300s: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND2" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  results; exit 1
fi
record PASS "the on-evict backend reaches Ready" "phase=Ready for ${BACKEND2}"

MEMBER_COUNT2="$(running_member_count "$BACKEND2")"
if [ "$MEMBER_COUNT2" -le 0 ] 2>/dev/null; then
  record FAIL "running members exist to force eviction" "no Running member Pod belongs to ${BACKEND2}"
  results; exit 1
fi
# Each member holds 64 objects of 4MiB, so one member's worth beyond the aggregate is what puts the
# set past memory and leaves eviction no choice. There is no separate set of keys held back to be
# the eviction candidates: which objects eviction takes is the store's to decide, and the subject
# of the read-back below is taken from what it actually moved.
PHASE_B_TOTAL=$((MEMBER_COUNT2 * 64 + 64))

CLIENT_ADDR2="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND2" -o jsonpath='{.status.endpoints[?(@.name=="Client")].address}' 2>/dev/null)"
ADMIN_ADDR2="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND2" -o jsonpath='{.status.endpoints[?(@.name=="Admin")].address}' 2>/dev/null)"
if [ -z "$CLIENT_ADDR2" ] || [ -z "$ADMIN_ADDR2" ]; then
  record FAIL "the on-evict backend publishes Client and Admin addresses" "Client='${CLIENT_ADDR2}' Admin='${ADMIN_ADDR2}'"
  results; exit 1
fi
record PASS "the on-evict backend publishes Client and Admin addresses" "${CLIENT_ADDR2} / ${ADMIN_ADDR2}"

PROBE_LOG2="$(mktemp)"
cat <<PY | kubectl -n "$NS" run "case65-probe-oe-${SFX}" --image="$IMAGE" --restart=Never \
  --pod-running-timeout="$PULL_TIMEOUT" \
  --labels="gpustack-e2e-case=65-${SFX}" \
  --overrides='{"spec":{"containers":[{"name":"probe","image":"'"$IMAGE"'","command":["python3","-"],"stdin":true,"stdinOnce":true}]}}' \
  -i --rm --quiet >"$PROBE_LOG2" 2>&1 || true
import sys, socket, hashlib, json, time, urllib.request
try:
    from mooncake.store import MooncakeDistributedStore
except Exception as e:
    print("IMPORT-FAIL %s" % e); sys.exit(0)

store = MooncakeDistributedStore()
ip = socket.gethostbyname(socket.gethostname())
try:
    rc = store.setup("%s:0" % ip, "P2PHANDSHAKE", 0, 64 * 1024 * 1024, "tcp", "",
                     "${CLIENT_ADDR2}")
except TypeError as e:
    print("SETUP-TYPEERROR %s" % e); sys.exit(0)
print("SETUP-RC %d" % rc)
if rc != 0:
    sys.exit(0)

OBJ = 4 * 1024 * 1024

# Content derived FROM THE KEY, so that a read-back's digest says WHICH object came back rather
# than only that something of the right size did. One payload shared by every key makes all the
# digests identical, and then an object served under the wrong key -- the shape a bucket adopted
# from another backend produces -- compares equal and passes.
def payload(key):
    seed = (key + "|case65-onevict|").encode()
    return (seed * (OBJ // len(seed) + 1))[:OBJ]

def query(keys):
    url = "http://${ADMIN_ADDR2}/batch_query_keys?keys=" + ",".join(keys)
    raw = urllib.request.urlopen(url, timeout=60).read().decode()
    return (json.loads(raw).get("data") or {})

# KEYS DISTINCT FROM PHASE A's, and that is a correctness requirement rather than tidiness. Both
# phases point their tier at the same host directory and phase A's backend is still alive here --
# the teardown deletes both at the end -- so this store's startup scan finds phase A's bucket files
# and adopts every key in them. Sharing a key name would let this phase's read be served by phase
# A's bytes: a digest mismatch that looks like a failed read-back but is a different defect.
KEYS = ["oe-%03d" % i for i in range(${PHASE_B_TOTAL})]

# A put returning -200 (NO_AVAILABLE_HANDLE) IS NOT A TERMINAL ERROR HERE, and treating it as one
# is how this phase came to assert a scene that never happened. The master raises its
# need-eviction flag on the very path that returns that code, and its eviction thread then runs
# the eviction -- which, under onEvict, is what writes to the tier. So the first -200 arrives at
# the exact moment the thing this phase exists to force is about to start, and stopping there
# stops just short of it. Retrying is what lets it happen.
#
# The FIRST stall is the slow one: the eviction it asks for was measured taking about 18 seconds,
# while every later one recovered within a couple. The budget below covers the first.
written = []
stalls = 0
hard = []
for k in KEYS:
    body = payload(k)
    rc = -1
    for attempt in range(15):
        rc = store.put(k, body)
        if rc == 0:
            written.append(k)
            break
        if rc == -200:
            stalls += 1
            time.sleep(2)
            continue
        break
    if rc != 0:
        hard.append("%s rc=%d" % (k, rc))
print("FILL written=%d of %d stalls=%d hard=%d" % (len(written), len(KEYS), stalls, len(hard)))
if hard:
    print("FILL-HARD %s" % " ".join(hard[:5]))

# WHERE the objects are, asked BEFORE any read and never after. A read served from the tier can
# promote the object back into memory, so a query taken afterwards would report a memory replica
# for a key that was on disk when it was read -- and telling those two apart is the whole point of
# this phase. Promotion is off in the configuration this operator renders (the store defaults
# promotion_on_hit to false and nothing here turns it on), so today the ordering changes no
# reading; it is kept because it is what makes the assertion survive that being turned on.
#
# The subject is taken FROM this answer rather than assumed to be the oldest key. Which object
# eviction chooses is the store's decision, and a case that names its own subject is asserting the
# eviction policy it happens to have observed.
tier_only = []
for attempt in range(12):
    try:
        d = query(written)
    except Exception as e:
        # Reported, not defaulted to empty: "nothing is on the tier" is exactly the reading this
        # phase acts on, so a failed query that returned it would manufacture the verdict.
        print("PLACEMENT-FAIL %s" % e)
        break
    mem_only, tier_only, both, neither, errored = [], [], [], [], []
    for k, v in d.items():
        if not v.get("ok"):
            errored.append(k)
            continue
        m = len(v.get("values") or [])
        t = len(v.get("local_disk_values") or [])
        if m and t:
            both.append(k)
        elif m:
            mem_only.append(k)
        elif t:
            tier_only.append(k)
        else:
            neither.append(k)
    print("PLACEMENT try=%d mem_only=%d tier_only=%d both=%d neither=%d error=%d" % (
        attempt, len(mem_only), len(tier_only), len(both), len(neither), len(errored)))
    if tier_only:
        break
    # The write lands in a BUCKET and the master learns of it when that bucket closes, so an empty
    # answer this early is not yet a verdict.
    time.sleep(10)

if not tier_only:
    print("SUBJECT none")
else:
    subject = sorted(tier_only)[0]
    print("SUBJECT %s" % subject)
    got = store.get(subject)
    if got is None:
        print("GET %s rc=NotFound" % subject)
    else:
        print("GET %s len=%d sha256=%s" % (subject, len(got), hashlib.sha256(got).hexdigest()))
    print("WANT %s len=%d sha256=%s" % (subject, OBJ, hashlib.sha256(payload(subject)).hexdigest()))
PY

grep -q 'SETUP-RC 0' "$PROBE_LOG2" \
  && record PASS "the on-evict probe's client set up against the leader" "$(grep 'SETUP-RC' "$PROBE_LOG2")" \
  || record FAIL "the on-evict probe's client set up against the leader" "$(grep -E 'SETUP|IMPORT-FAIL|Error|error' "$PROBE_LOG2" | head -3 | tr '\n' ' ')"

FILL="$(grep -E '^FILL written=' "$PROBE_LOG2" | head -1)"
FILL_W="$(echo "$FILL" | sed -n 's/.*written=\([0-9][0-9]*\).*/\1/p')"
FILL_N="$(echo "$FILL" | sed -n 's/.* of \([0-9][0-9]*\).*/\1/p')"
FILL_HARD="$(echo "$FILL" | sed -n 's/.*hard=\([0-9][0-9]*\).*/\1/p')"
if [ -z "$FILL_W" ] || [ -z "$FILL_N" ]; then
  record FAIL "on-evict writes succeed past aggregate memory" \
    "no FILL line from the probe: $(grep -E 'SETUP|IMPORT-FAIL' "$PROBE_LOG2" | head -2 | tr '\n' ' ')"
elif [ "$FILL_W" = "$FILL_N" ] && [ "${FILL_HARD:-1}" = 0 ]; then
  # The stall count is printed rather than asserted on. A run with zero stalls wrote the whole set
  # without ever exhausting memory, which is a smaller write set than intended rather than a
  # failure; the placement reading below is what says whether eviction happened.
  record PASS "on-evict writes succeed past aggregate memory" \
    "${FILL} for ${MEMBER_COUNT2} running member(s) (eviction forced)"
else
  record FAIL "on-evict writes succeed past aggregate memory" \
    "${FILL} $(grep '^FILL-HARD' "$PROBE_LOG2") -- puts that no retry recovered, so the set that \
reached the store is not the one the assertions below are sized for"
fi

# WHERE the read comes FROM, established before the read itself is judged. A successful GET of the
# right bytes is compatible with the object never having left memory, and this phase's whole claim
# is that it did -- so the leader's own placement answer, taken before any read, is what makes the
# next assertion a statement about the TIER rather than about the store in general.
PLACE="$(grep -E '^PLACEMENT try=' "$PROBE_LOG2" | tail -1)"
PLACE_T="$(echo "$PLACE" | sed -n 's/.*tier_only=\([0-9][0-9]*\).*/\1/p')"
if [ -z "$PLACE_T" ]; then
  # An unanswered query is NOT "nothing is on the tier". That reading is the one this phase acts
  # on, so defaulting it to zero would manufacture the verdict out of a failed probe.
  record FAIL "the leader holds keys on the tier that memory no longer has" \
    "no placement reading: ${PLACE:-<no PLACEMENT line>} $(grep '^PLACEMENT-FAIL' "$PROBE_LOG2") \
-- this run cannot say where the read below was served from"
elif [ "$PLACE_T" -gt 0 ] 2>/dev/null; then
  record PASS "the leader holds keys on the tier that memory no longer has" "$PLACE"
else
  record FAIL "the leader holds keys on the tier that memory no longer has" \
    "$PLACE -- every key the store still knows about has a memory replica, so eviction either did \
not run or dropped what it took instead of offloading it"
fi

SUBJECT="$(grep -E '^SUBJECT ' "$PROBE_LOG2" | head -1 | awk '{print $2}')"
GET2_SHA="$(grep -E '^GET ' "$PROBE_LOG2" | head -1 | awk '{print $4}')"
WANT2_SHA="$(grep -E '^WANT ' "$PROBE_LOG2" | head -1 | awk '{print $4}')"
if [ -n "$WANT2_SHA" ] && [ "$GET2_SHA" = "$WANT2_SHA" ]; then
  record PASS "an evicted object reads back byte-identical from the tier" \
    "$(grep -E '^GET ' "$PROBE_LOG2" | head -1) [${PLACE}]"
else
  # Three outcomes reach here and they are different defects, so the message names all three rather
  # than the one that was expected first. The payload is derived from the key, so a digest that
  # matches nothing is bytes stored under some other key rather than a corrupted read.
  record FAIL "an evicted object reads back byte-identical from the tier" \
    "subject=${SUBJECT:-<none: no key was on the tier>} get: $(grep -E '^GET ' "$PROBE_LOG2" | head -1) \
vs $(grep -E '^WANT ' "$PROBE_LOG2" | head -1) -- NotFound means eviction dropped the object instead \
of offloading it; a digest that matches neither means the bytes belong to a different key"
fi

METRIC2="$(kubectl -n "$NS" run "case65-metrics-oe-${SFX}" --image=busybox:1.36 --restart=Never --rm -i --quiet \
  --pod-running-timeout="$PULL_TIMEOUT" \
  --labels="gpustack-e2e-case=65-${SFX}" \
  -- sh -c "wget -qO- http://${ADMIN_ADDR2}/metrics | grep '^master_allocated_file_size_bytes'" 2>/dev/null)"
# One line, as above: the probe's single line is replayed by kubectl's log fallback, and the pair
# of identical numbers that produces is not something any numeric test accepts.
METRIC2_VAL="$(echo "$METRIC2" | head -1 | awk '{print $2}' | cut -d. -f1)"
if [ -n "$METRIC2_VAL" ] && [ "$METRIC2_VAL" -gt 0 ] 2>/dev/null; then
  record PASS "the on-evict leader reports bytes written under forced eviction" \
    "master_allocated_file_size_bytes=${METRIC2_VAL} (> 0) -- the eviction path writes; only the write-through path is broken"
elif [ -z "$METRIC2" ]; then
  # The discriminator's verdict is the harshest this case can reach -- both write paths broken -- and
  # a probe that never returned would reach it without a reading.
  record FAIL "the on-evict leader reports bytes written under forced eviction" \
    "the metrics probe returned no reading within ${PULL_TIMEOUT}; an absent reading is not a zero, \
so this run cannot say which write path the tier uses"
else
  case "$METRIC2_VAL" in
    '' | *[!0-9]*)
      # A reading arrived and is not a number. Named separately from the zero below because the
      # harshest verdict this case can reach must not be reachable by a parse: a run that read a
      # healthy figure it could not parse would otherwise be reported as a tier that never wrote.
      record FAIL "the on-evict leader reports bytes written under forced eviction" \
        "master_allocated_file_size_bytes came back unparsable: '${METRIC2}' -- a reading this run \
could not read is not a zero, so it makes no claim about either write path"
      ;;
    *)
      record FAIL "the on-evict leader reports bytes written under forced eviction" \
        "master_allocated_file_size_bytes=${METRIC2_VAL} -- a zero here, with eviction forced and writes healthy, \
means BOTH write paths are broken: the tier is unusable in every configuration this API renders"
      ;;
  esac
fi

rm -f "$PROBE_LOG" "$PROBE_LOG2"
results
