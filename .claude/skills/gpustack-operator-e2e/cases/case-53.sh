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
#              The read/write is done with the CLIENT LIBRARY rather than a running engine, and the
#              fixture calls setup() the way vLLM does - POSITIONALLY, with seven arguments, mirroring
#              worker.py:1040-1048. Never the dict overload: that one forwards every key including a
#              tenant, so a fixture using it would be MORE CAPABLE than the engine it stands in for,
#              and case 55's stamp would then describe a deployment nothing matches.
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and one KVCachePoolBinding, so it needs no pool to pre-exist. Never auto-skips.
# Inputs:      a plain Deployment carrying the inject label and two annotations - the REAL object
#              under test, not a mock. The read/write probe is a fixture: it stands in for an engine
#              by calling the client the way vLLM's connector does, positionally with seven
#              arguments. It is deliberately NOT the dict overload, which would forward a tenant the
#              real engine truncates.
# Expected:    the Deployment's Pod is injected; its container parses the projected file, writes an
#              object and reads it back byte for byte; the Binding's reported usage rises above zero.
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
#
# NOT RE-RUN since the readiness wait stopped being discarded. That run reached the probe, so it says
# nothing about the new failure path - a Pod that never becomes Ready now FAILS here instead of
# reaching the probe and reporting an empty log as an injection defect.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster: all five checks passed. Two amendments have landed
# since that run, so it is evidence for the shape but not for the current assertions: the usage check
# now waits for the figure to EXIST before requiring zero (status.usage is a pointer, so absent means
# unobserved rather than zero), and the comment below now scopes the tenant claim to vLLM.
#
# The first run failed twice - the read/write half with TENANT_NOT_REGISTERED, which is what
# established the "default" Binding prerequisite, and then the usage half because it was reading the
# domain Binding rather than the default one.
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
      containers:
        - name: engine
          image: ${CLIENT_IMAGE}
          command: ["python3", "-c", "import time; time.sleep(3600)"]
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
# setup() positionally with seven arguments, exactly as vLLM's connector does.
kubectl -n "$TEST_NS" exec "$POD" -c engine -- python3 -c '
import json, os, sys
from mooncake.store import MooncakeDistributedStore

cfg = json.load(open(os.environ["MOONCAKE_CONFIG_PATH"]))
store = MooncakeDistributedStore()
rc = store.setup(
    os.environ.get("POD_IP", "127.0.0.1"),
    cfg["metadata_server"],
    cfg["global_segment_size"],
    cfg["local_buffer_size"],
    cfg["protocol"],
    cfg["device_name"],
    cfg["master_server_address"],
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
    "setup() returned 0 from the projected file, called positionally with seven arguments"
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
# statement about the store rather than about the probe's return codes.
#
# It is read off the DEFAULT Binding, not off the domain-carrying one, and the difference is the point.
# THIS ENGINE - vLLM - forwards no tenant, so its writes land under the literal "default", which means
# the per-namespace Binding's ceiling does not govern them at all. That is per engine, not universal:
# an SGLang Pod is given its own domain and would charge the domain Binding instead, which is why this
# case pins vllm in its manifest rather than taking whatever engine happens to be handy.
#
# Reading the domain Binding here was this case's own bug: it reported zero while the round-trip above
# plainly succeeded, and zero was the correct answer to the wrong question.
usage=""
for _ in $(seq 1 20); do
  usage="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING_DEFAULT" \
    -o jsonpath='{.status.usage}' 2>/dev/null)"
  [ -n "$usage" ] && [ "$usage" != "0" ] && break
  sleep 3
done
if [ -n "$usage" ] && [ "$usage" != "0" ]; then
  record PASS "the master's used-bytes figure moved" \
    "the default-tenant Binding reports usage ${usage}"
else
  record FAIL "the master's used-bytes figure moved" \
    "usage is '${usage:-<absent>}' on ${BINDING_DEFAULT} 60s after the write; absent is not the same \
as zero"
fi

# The paired half: the domain Binding must report NOTHING. Without this, the check above cannot tell
# "the writes went to the default tenant" from "usage moves on whichever Binding you ask", and the
# documented consequence - that a namespace's quotaCeiling does not bound its injected Pods - would
# rest on the reference page alone.
# Polled until the figure EXISTS, and only then required to be zero. status.usage is a pointer with
# omitempty precisely so that "granted zero" and "never observed" do not serialize the same way, so an
# absent value here means the Binding's status has not converged - which proves nothing about where
# the write was charged. Accepting absence would have made this check pass on a cluster where the
# reconciler had not run at all.
domain_usage=""
for _ in $(seq 1 20); do
  domain_usage="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING" \
    -o jsonpath='{.status.usage}' 2>/dev/null)"
  [ -n "$domain_usage" ] && break
  sleep 3
done
if [ -z "$domain_usage" ]; then
  record FAIL "the declared domain is charged nothing" \
    "${BINDING} still reports no usage figure after 60s, so its status never converged; absent is not \
zero, and this run cannot say where the bytes were charged"
elif [ "$domain_usage" = "0" ]; then
  record PASS "the declared domain is charged nothing" \
    "${BINDING} reports '${domain_usage:-<absent>}' while the same bytes moved under 'default' - so a \
namespace's quotaCeiling does not bound the traffic its injected Pods generate"
else
  record FAIL "the declared domain is charged nothing" \
    "${BINDING} reports usage ${domain_usage}, so the writes did carry the declared domain after all - \
the F4a facts table and every stamp built on it would be wrong"
fi

kvi_results "$CASE_ID"
