#!/usr/bin/env bash
#
# CASE 53 — The headline: a plain Deployment reads and writes a KV cache pool with one label
#   (MUTATING, self-recovering)
#
#   case-53.sh <NS>
#
# Goal:        This spec exists so a pool can be used by ANY Pod, not only by workloads this operator
#              renders. That claim is only worth something if a Pod with no queue-name label, no
#              workload CR and no owner reference from this operator moves real bytes in the master.
#              So this case builds a plain Deployment, opts it in with one label and two annotations,
#              and then proves the container talks to the pool: the master's used-bytes figure moves,
#              and a read returns exactly what was written.
#
#              The read/write is done with the CLIENT LIBRARY rather than a running engine. The
#              fixture reads everything from the projected file, the tenant included: it calls setup()
#              with the seven positional arguments and the file's tenant_id as a keyword, which is what
#              a vLLM-family engine that consumes the injected tenant does. Because the tenant comes
#              from the file, where the bytes are charged is a reading of what the webhook injected.
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and one KVCachePoolBinding, so it needs no pool to pre-exist. Never auto-skips.
# Inputs:      a plain Deployment carrying the inject label and two annotations - the REAL object
#              under test, not a mock. The read/write probe is a fixture: it stands in for an engine
#              that consumes the projected file, the injected tenant_id included.
# Expected:    the Deployment's Pod is injected; its container parses the projected file, writes an
#              object and reads it back byte for byte; the Binding carrying the declared domain reports
#              usage above zero, and the Binding whose domain is the literal "default" reports zero.
#
#              SKIPS: none. This case has no conditional half - every check either runs or FAILS, and
#              a precondition it cannot meet is recorded as a failure rather than passed over. The
#              footer counts any SKIP separately from the passes, so a skipped check can never be read
#              off the PASS count.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It changes no shared baseline - every object it touches is one it created.
set -uo pipefail

NS="${1:?usage: case-53.sh <NS>}"
CASE_ID=53
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

PAYLOAD="the-quick-brown-fox-${SFX}"
PROBE_LOG="/tmp/kvc-inject-53-${SFX}.log"

# A plain Deployment. No queue-name label, no InstanceType, no owner reference from this operator -
# which is the whole point of the case.
#
# The container declares the vLLM entry point and runs kvi_setup's stub behind it. That shape is not
# this case's subject; it is what admission accepts, and the reasoning is with the stub in
# _kvcache-inject-lib.sh. What matters here is that the probe below execs the image's OWN python3,
# from PATH, so it reaches the mooncake client rather than the stub.
kubectl apply -f - <<YAML >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: plain
  namespace: ${TEST_NS}
spec:
  replicas: 1
  selector:
    matchLabels: {app: plain}
  template:
    metadata:
      labels:
        app: plain
        kvcache.gpustack.ai/inject: "true"
      annotations:
        kvcache.gpustack.ai/binding: ${BINDING}
        kvcache.gpustack.ai/engine: vllm
    spec:
      volumes:
        - name: ${LAUNCH_VOLUME}
          configMap:
            name: ${LAUNCH_CONFIGMAP}
            defaultMode: 0755
            items:
              - key: launch
                path: vllm
      containers:
        - name: engine
          image: ${CLIENT_IMAGE}
          command: ["${LAUNCH_DIR}/vllm", "serve"]
          volumeMounts:
            - name: ${LAUNCH_VOLUME}
              mountPath: ${LAUNCH_DIR}
YAML

POD=""
for _ in $(seq 1 40); do
  POD="$(kubectl -n "$TEST_NS" get pods -l app=plain -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$POD" ] && break
  sleep 3
done
if [ -z "$POD" ]; then
  record FAIL "the Deployment produced a Pod" "no Pod appeared in 120s; the webhook may have refused it"
  kvi_results "$CASE_ID"; exit 1
fi

# The injection landed, and it landed on a Pod nothing else in this operator owns.
if [ -n "$(kvi_env "$POD" MOONCAKE_CONFIG_PATH)" ]; then
  record PASS "a plain Deployment's Pod is injected" \
    "MOONCAKE_CONFIG_PATH is set on ${POD}, which carries no queue-name label and no owner from this operator"
else
  record FAIL "a plain Deployment's Pod is injected" \
    "MOONCAKE_CONFIG_PATH is absent on ${POD}"
  kvi_results "$CASE_ID"; exit 1
fi

# The wait's outcome is USED rather than discarded. A Pod that is not Running makes the exec below
# fail at once, and its empty log reaches the checks as "the probe printed no SETUP line" - a timing
# artifact wearing the costume of an injection defect. A slow image pull is a documented failure mode
# in this family: case 59 once timed out on a 19.1GB one.
if ! kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/${POD}" --timeout=180s >/dev/null 2>&1; then
  record FAIL "the injected file is one the client accepts" \
    "the Pod never became Ready in 180s, so the probe never ran - this says NOTHING about what was \
injected. Most likely the image is still pulling; pre-pull it or raise the timeout and run again"
  kvi_results "$CASE_ID"; exit 1
fi

# The probe. It parses the file the webhook projected - not an address this case knows - and calls
# setup() with the seven positional arguments plus the file's tenant_id. The tenant rides the keyword:
# the next positional slot is a TransferEngine pointer (case 77 measured that). A file without a
# tenant_id falls back to the store's own "default", which the usage rows below then catch.
kubectl -n "$TEST_NS" exec "$POD" -c engine -- python3 -c '
import json, os, sys
from mooncake.store import MooncakeDistributedStore

cfg = json.load(open(os.environ["MOONCAKE_CONFIG_PATH"]))
tenant = cfg.get("tenant_id") or "default"
print("TENANT %s" % tenant)
store = MooncakeDistributedStore()
rc = store.setup(
    os.environ.get("POD_IP", "127.0.0.1"),
    cfg["metadata_server"],
    cfg["global_segment_size"],
    cfg["local_buffer_size"],
    cfg["protocol"],
    cfg["device_name"],
    cfg["master_server_address"],
    tenant_id=tenant,
)
print("SETUP rc=%d" % rc)
if rc != 0:
    sys.exit(0)
key = "case53-" + sys.argv[1] if len(sys.argv) > 1 else "case53"
payload = sys.argv[2].encode() if len(sys.argv) > 2 else b"x"
print("PUT rc=%d" % store.put(key, payload))
got = store.get(key)
print("GET len=%d match=%s" % (len(got or b""), (got == payload)))
' "$SFX" "$PAYLOAD" >"$PROBE_LOG" 2>&1 || true

if grep -q '^SETUP rc=0' "$PROBE_LOG"; then
  record PASS "the injected file is one the client accepts" \
    "setup() returned 0 from the projected file ($(grep -m1 '^TENANT' "$PROBE_LOG"))"
else
  record FAIL "the injected file is one the client accepts" \
    "$(grep -m1 '^SETUP' "$PROBE_LOG" || echo 'the probe printed no SETUP line'); see ${PROBE_LOG}"
fi

if grep -q '^GET .*match=True' "$PROBE_LOG"; then
  record PASS "the Pod reads and writes the pool" \
    "a put and a get round-tripped ${#PAYLOAD} bytes through the master at ${ENDPOINT}"
else
  record FAIL "the Pod reads and writes the pool" \
    "$(grep -m1 '^GET' "$PROBE_LOG" || echo 'the probe printed no GET line'); see ${PROBE_LOG}"
fi

# Usage is the master's own figure, not this case's arithmetic: it is what makes "the bytes moved" a
# statement about the store rather than about the probe's return codes. It is read off the Binding
# that carries the declared domain, because the probe wrote under the tenant the file named: usage
# there is a reading of what the webhook injected, and a missing or wrong tenant_id leaves it at zero.
usage=""
for _ in $(seq 1 20); do
  usage="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING" \
    -o jsonpath='{.status.usage}' 2>/dev/null)"
  [ -n "$usage" ] && [ "$usage" != "0" ] && break
  sleep 3
done
if [ -n "$usage" ] && [ "$usage" != "0" ]; then
  record PASS "the master's used-bytes figure moved on the declared domain" \
    "${BINDING} reports usage ${usage}"
else
  record FAIL "the master's used-bytes figure moved on the declared domain" \
    "usage is '${usage:-<absent>}' on ${BINDING} 60s after the write; absent is not the same as zero. \
Zero with the round-trip above passing means the write was charged to another tenant - read the \
probe's TENANT line in ${PROBE_LOG}"
fi

# The paired half: the "default" Binding must report NOTHING. Without it the check above cannot tell
# "the write carried the declared domain" from "usage moves on whichever Binding you ask".
# Polled until the figure EXISTS, and only then required to be zero: status.usage is a pointer with
# omitempty so that "granted zero" and "never observed" do not serialize the same way, and accepting
# absence would pass on a cluster where the reconciler had not run at all.
default_usage=""
for _ in $(seq 1 20); do
  default_usage="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING_DEFAULT" \
    -o jsonpath='{.status.usage}' 2>/dev/null)"
  [ -n "$default_usage" ] && break
  sleep 3
done
if [ -z "$default_usage" ]; then
  record FAIL "the default tenant is charged nothing" \
    "${BINDING_DEFAULT} still reports no usage figure after 60s, so its status never converged; absent \
is not zero, and this run cannot say where the bytes were charged"
elif [ "$default_usage" = "0" ]; then
  record PASS "the default tenant is charged nothing" \
    "${BINDING_DEFAULT} reports 0 while the same bytes moved under the declared domain"
else
  record FAIL "the default tenant is charged nothing" \
    "${BINDING_DEFAULT} reports usage ${default_usage}: the write fell back to the store's default \
tenant, so the injected file carried no usable tenant_id"
fi

kvi_results "$CASE_ID"
