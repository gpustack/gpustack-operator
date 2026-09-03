#!/usr/bin/env bash
#
# CASE 54 — No workload lock-in: a LeaderWorkerSet and a bare Pod get the identical injection
#   (MUTATING, self-recovering, AUTO-SKIPS the LWS half without the LeaderWorkerSet CRD)
#
#   case-54.sh <NS>
#
# Goal:        Case 53 proved a Deployment works. This one proves the mechanism is not shaped for
#              Deployments: the webhook triggers on a LABEL and reads ANNOTATIONS, so anything that
#              can put those on a Pod template - a third-party operator's CR, an LWS, a hand-written
#              Pod - gets the same result. The assertion is the strong form: the two Pods' injected
#              artifacts are compared to each other FIELD BY FIELD, not each checked against a list.
#              A per-workload difference would show up as an inequality without anyone having to
#              predict which field it would be.
#
#              The LWS half SKIPs loudly when the CRD is absent, rather than passing quietly: an
#              absent CRD proves nothing about workload independence, and reporting it as a pass is
#              the failure this suite exists to avoid.
#
# Environment: as case 53, plus the leaderworkerset.x-k8s.io CRD for the second half. Without that
#              CRD the LWS half AUTO-SKIPS and says workload independence is unverified against LWS;
#              the bare-Pod half always runs.
# Inputs:      a bare Pod and an LWS leader Pod, both real objects carrying the same label and
#              annotations. Nothing is mocked: the point is that two unrelated workload kinds reach
#              the same webhook.
# Expected:    the bare Pod is injected; where LWS is installed, its leader Pod's env, args, mounts
#              and stamp are identical to the bare Pod's, modulo the Pod name.
#
#              SKIPS: the LWS half, and only it, when the leaderworkerset.x-k8s.io CRD is absent. It
#              is recorded as a SKIP naming what is left unverified, never as a pass - an absent CRD
#              says nothing about workload independence. The bare-Pod half never skips. The footer
#              counts skips separately, so a run where only the bare Pod was checked cannot be read as
#              a run where both were.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It changes no shared baseline - every object it touches is one it created.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster: the bare-Pod half passes; the LWS half SKIPped
# because that cluster has no leaderworkerset.x-k8s.io CRD, so workload independence remains unverified
# against LWS. The footer counted the skip separately, which is what keeps that from reading as a pass.
set -uo pipefail

NS="${1:?usage: case-54.sh <NS>}"
CASE_ID=54
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

# The volume this webhook injects, by the name it uses (inject.ConfigVolumeName).
CONFIG_VOLUME=gpustack-kvcache-config

# injected_shape prints the parts of a Pod this webhook is allowed to write, sorted so two Pods can be
# compared directly. The Pod name is deliberately absent from it: it is the one thing that must differ.
injected_shape() {
  local pod="$1"
  {
    # Filtered to the variables this webhook writes, for the same reason the volume lines below are:
    # a controller above the Pod adds its own. LWS gives every container LWS_LEADER_ADDRESS,
    # LWS_GROUP_SIZE and LWS_WORKER_INDEX, so an unfiltered list makes the two shapes differ on keys
    # this webhook never touched - and the LWS half would fail on its first real run, having only
    # ever SKIPped. MOONCAKE_ and MC_ are every prefix injectPod can write: the renderers' keys and
    # the two observability defaults.
    # WHAT THIS GIVES UP: a variable this webhook should NOT have written goes unseen here. That is
    # case 56's question - submitted shape versus stored shape - and this case does not ask it.
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].env[*]}ENV {.name}={.value}{"\n"}{end}' 2>/dev/null \
      | grep -E '^ENV (MOONCAKE_|MC_)' || true
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].args[*]}ARG {@}{"\n"}{end}' 2>/dev/null
    # Filtered to the volume this webhook injects, for the same reason case 56 filters: the API server
    # projects a service-account token volume onto every Pod under a RANDOM name, so listing all of
    # them would make two Pods differ on a field neither this webhook nor this case controls. On a
    # cluster where the LWS half actually runs, that would fail the comparison for no real reason.
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.containers[0].volumeMounts[?(@.name=='${CONFIG_VOLUME}')]}MOUNT {.name}@{.mountPath} ro={.readOnly}{'\n'}{end}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.volumes[?(@.name=='${CONFIG_VOLUME}')]}VOL {.name}{'\n'}{end}" 2>/dev/null
    echo "STAMP engine=$(kvi_stamp "$pod" engine) vehicle=$(kvi_stamp "$pod" vehicle) tenantInjected=$(kvi_stamp "$pod" tenantInjected)"
  } | sort
}

# A bare Pod: no controller of any kind above it.
kvi_pod_manifest bare vllm | kubectl apply -f - >/dev/null 2>&1
if ! kvi_wait_for pods bare '{.metadata.name}' bare 60 "$TEST_NS" >/dev/null; then
  record FAIL "a bare Pod is admitted" "the Pod never appeared; the webhook may have refused it"
  kvi_results "$CASE_ID"; exit 1
fi

BARE_SHAPE="$(injected_shape bare)"
if echo "$BARE_SHAPE" | grep -q 'ENV MOONCAKE_CONFIG_PATH='; then
  record PASS "a bare Pod is injected" "no controller above it, and it carries the full artifact set"
else
  record FAIL "a bare Pod is injected" "MOONCAKE_CONFIG_PATH is absent"
  kvi_results "$CASE_ID"; exit 1
fi

# The LWS half. An absent CRD is a SKIP that says so, never a pass.
if ! kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io >/dev/null 2>&1; then
  record SKIP "an LWS Pod gets the identical injection" \
    "leaderworkerset.x-k8s.io is not installed on this cluster; workload independence is UNVERIFIED \
against LWS, and this case asserts it only for the bare Pod"
  kvi_results "$CASE_ID"
  exit $?
fi

kubectl apply -f - <<YAML >/dev/null
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: lws
  namespace: ${TEST_NS}
spec:
  replicas: 1
  leaderWorkerTemplate:
    size: 1
    leaderTemplate:
      metadata:
        labels:
          kvcache.gpustack.ai/inject: "true"
        annotations:
          kvcache.gpustack.ai/binding: ${BINDING}
          kvcache.gpustack.ai/engine: vllm
      spec:
        containers:
          - name: engine
            image: ${CLIENT_IMAGE}
            command: ["python3", "-c", "import time; time.sleep(3600)"]
    workerTemplate:
      spec:
        containers:
          - name: worker
            image: ${CLIENT_IMAGE}
            command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML

LWS_POD=""
for _ in $(seq 1 40); do
  LWS_POD="$(kubectl -n "$TEST_NS" get pods -l leaderworkerset.sigs.k8s.io/name=lws \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$LWS_POD" ] && break
  sleep 3
done
if [ -z "$LWS_POD" ]; then
  record FAIL "an LWS Pod gets the identical injection" "no LWS Pod appeared in 120s"
  kvi_results "$CASE_ID"; exit 1
fi

LWS_SHAPE="$(injected_shape "$LWS_POD")"
if [ "$BARE_SHAPE" = "$LWS_SHAPE" ]; then
  record PASS "an LWS Pod gets the identical injection" \
    "env, args, mounts, volumes and stamp are byte-identical to the bare Pod's"
else
  record FAIL "an LWS Pod gets the identical injection" \
    "the two differ: $(diff <(echo "$BARE_SHAPE") <(echo "$LWS_SHAPE") | tr '\n' ' ' | cut -c1-200)"
fi

kvi_results "$CASE_ID"
