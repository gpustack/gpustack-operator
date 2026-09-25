#!/usr/bin/env bash
#
# CASE 100 — The weight identity in the KV store key: one artifact's deployments share blocks
#            through the store, and a deployment of another commit on the same Binding reads none
#            (MUTATING, cluster-scoped store objects; needs real accelerators)
#
#   case-100.sh <NS>
#
# Goal:        Prove at runtime that cache_prefix isolates weights: two commits of
#              HuggingFaceTB/SmolLM-135M-Instruct share a tokenizer and a served name and differ in
#              their weights, which without the prefix was measured to put both on the same store
#              keys. A writer of commit A fills the store; a second deployment of the same artifact
#              must take the replayed prompts from the store; a deployment of commit B must take
#              none of them.
# Environment: A cluster with an accelerator InstanceType (E2E_C100_IT) hosting two single-card
#              replicas at once, a node for the store member (E2E_C100_MEMBER_NODE, a hostname with
#              4 GiB of memory to spare), a KV store image the chart's default names, nodes that
#              reach the Hub, and in <NS> the pool's entrance LocalQueue. This case creates a
#              cluster-scoped KVCacheBackend and KVCachePool: hold the cluster lock while it runs.
# Inputs:      All real. Commit A 0a0a7c2a1b1dc8f75f1d5a6ac86d38e3e7bab014 and commit B
#              fcc320f490e08fdb4b99d935b2c58d40bf35b0d0; deterministic prompts of about 1000 words.
#              The instrument is vllm:external_prefix_cache_hits_total on the replaying engine; every
#              send must also move vllm:request_success_total by exactly its prompt count, or the
#              row it serves FAILs. E2E_C100_ENGINE_URL=http://127.0.0.1:9 shows the case failing.
# Expected:    - the writer and the same-artifact deployment render one cache_prefix, the other
#                commit's deployment another;
#              - must-miss control: prompts nobody sent take no token from the store;
#              - must-hit: the same artifact's deployment takes the writer's prompts from the store;
#              - the other commit's deployment takes none of them.
# Cleanup:     Trap deletes the deployments, the artifacts, the Binding, the pool and the backend.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-100.sh <NS>}"
IT="${E2E_C100_IT:?set E2E_C100_IT to an accelerator InstanceType}"
MEMBER_NODE="${E2E_C100_MEMBER_NODE:?set E2E_C100_MEMBER_NODE to the hostname the store member runs on}"
READY_BOUND="${E2E_C100_READY_BOUND:-1200}"
# Where the prompts are sent inside the engine container. Pointing it at a port nothing listens on is
# how this case is shown to fail: every send then fails and every served count stays put.
ENGINE_URL="${E2E_C100_ENGINE_URL:-http://127.0.0.1:8000}"
COMMIT_A=0a0a7c2a1b1dc8f75f1d5a6ac86d38e3e7bab014
COMMIT_B=fcc320f490e08fdb4b99d935b2c58d40bf35b0d0
P=c100
LABEL="e2e.gpustack.ai/case=100"
SERVED=e2e/c100

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

cleanup() {
  echo
  echo "[case-100] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 36); do
    [ -z "$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "${P}-")" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai,kvcachepoolbindings.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete kvcachepools.worker.gpustack.ai "${P}-kvcp" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete kvcachebackends.worker.gpustack.ai "${P}-kvcb" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

# The prompts are a pure function of the seed, generated inside the engine container, so the
# writer and every replaying deployment send byte-identical text.
PROMPTS_PY='
import json, random, sys, urllib.request
words = ("time year people way day man thing woman life child world school state family student "
         "group country problem hand part place case week company system program question work").split()
start, count, served, url = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
for seed in range(start, start + count):
    rng = random.Random(seed * 7919 + 13)
    prompt = "%d " % seed + " ".join(rng.choice(words) for _ in range(1000))
    body = json.dumps({"model": served, "prompt": prompt, "max_tokens": 1, "temperature": 0}).encode()
    req = urllib.request.Request(url + "/v1/completions", data=body, headers={"Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=600).read()
print("sent", count)
'
# Prints a metric family's sum, read from the engine itself on its own port whatever ENGINE_URL
# says, so a broken send cannot also blind the reading.
METRIC_PY='
import sys, urllib.request
total = 0.0
for line in urllib.request.urlopen("http://127.0.0.1:8000/metrics").read().decode().splitlines():
    if line.startswith(sys.argv[1]):
        total += float(line.rsplit(" ", 1)[1])
print(int(total))
'

leader() { kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$1,app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }
metric() { kubectl -n "$NS" exec "$(leader "$1")" -c main -- python3 -c "$METRIC_PY" "$2" 2>/dev/null; }
hits() { metric "$1" vllm:external_prefix_cache_hits_total; }

# send sends a seed range and proves it arrived: the command must succeed and the engine's own count
# of finished requests must grow by exactly the range. A reading of hits after a send that did not
# arrive would be a reading of nothing, which every negative row here would pass.
SEND_ERR=""
send() { # md start count
  local before after
  before="$(metric "$1" vllm:request_success_total)"
  if ! kubectl -n "$NS" exec "$(leader "$1")" -c main -- python3 -c "$PROMPTS_PY" "$2" "$3" "$SERVED" "$ENGINE_URL" >/dev/null 2>&1; then
    SEND_ERR="${1}: sending seeds ${2}+${3} failed"
    return 1
  fi
  after="$(metric "$1" vllm:request_success_total)"
  if [ -z "$before" ] || [ -z "$after" ] || [ $(( after - before )) -ne "$3" ]; then
    SEND_ERR="${1}: finished requests ${before:-?} -> ${after:-?}, not +${3}"
    return 1
  fi
}

# sent records a failed send as its own FAIL row, so a row reading hits after it is never a pass.
sent() { # check md start count
  if send "$2" "$3" "$4"; then
    return 0
  fi
  record FAIL "$1" "$SEND_ERR"
  return 1
}
prefix() {
  kubectl -n "$NS" get pod "$(leader "$1")" -o jsonpath='{.spec.containers[0].command}' 2>/dev/null \
    | command grep -o 'cache_prefix[^,]*' | head -1
}

wait_ready() {
  local phase=""
  for _ in $(seq 1 $(( READY_BOUND / 10 ))); do
    phase="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" -o jsonpath='{.status.phase}' 2>/dev/null)"
    [ "$phase" = Ready ] && break
    sleep 10
  done
  printf '%s' "$phase"
}

deployment() { # name artifact
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "100"}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: ${SERVED}, artifactRef: {name: $2}}
  kvCache: {poolRef: {name: ${P}-shared}}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--max-model-len", "2048", "--gpu-memory-utilization", "0.3"]}
YAML
}

echo "== fixtures: one store, one Binding, two artifacts =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata: {name: ${P}-kvcb}
spec:
  type: Mooncake
  connection:
    managed:
      leader: {multiTenancy: true}
      members:
        - {nodeSelector: {kubernetes.io/hostname: "${MEMBER_NODE}"}, medium: DRAM, capacityPerMember: 4Gi}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata: {name: ${P}-kvcp}
spec: {backends: [${P}-kvcb], quota: {total: 4Gi}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: ${P}-shared, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "100"}}
spec:
  poolRef: {name: ${P}-kvcp}
  quota: {ceiling: 4Gi}
  domain: {name: ${P}-shared, blockSize: 16, dtype: bfloat16}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-a, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "100"}}
spec: {source: {huggingFace: {repository: HuggingFaceTB/SmolLM-135M-Instruct, revision: "${COMMIT_A}"}}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-b, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "100"}}
spec: {source: {huggingFace: {repository: HuggingFaceTB/SmolLM-135M-Instruct, revision: "${COMMIT_B}"}}}
YAML

echo "== 1. the writer fills the store with commit A's blocks =="
deployment "${P}-writer" "${P}-a"
deployment "${P}-same" "${P}-a"
w="$(wait_ready "${P}-writer")"; s="$(wait_ready "${P}-same")"
if [ "$w" != Ready ] || [ "$s" != Ready ]; then
  record FAIL "the writer and the same-artifact deployment go Ready" "writer=${w:-<none>} same=${s:-<none>}"
  print_rows
  exit 1
fi
sent "the writer's prompts reach it" "${P}-writer" 1000 8 && record PASS "the writer's prompts reach it" "8 requests finished"
sleep 15

echo "== 2. the same artifact: a must-miss control, then the replay =="
p_writer="$(prefix "${P}-writer")"; p_same="$(prefix "${P}-same")"
if [ -n "$p_writer" ] && [ "$p_writer" = "$p_same" ]; then
  record PASS "one artifact renders one prefix" "$p_writer"
else
  record FAIL "one artifact renders one prefix" "writer=${p_writer:-<none>} same=${p_same:-<none>}"
fi
before="$(hits "${P}-same")"
if sent "must-miss: prompts nobody sent take nothing from the store" "${P}-same" 9000 4; then
  control="$(hits "${P}-same")"
  if [ -n "$control" ] && [ "$control" = "$before" ]; then
    record PASS "must-miss: prompts nobody sent take nothing from the store" "4 requests finished, hits ${before} -> ${control}"
  else
    record FAIL "must-miss: prompts nobody sent take nothing from the store" "hits ${before:-?} -> ${control:-?}"
  fi
  if sent "must-hit: the same artifact takes the writer's blocks from the store" "${P}-same" 1000 8; then
    after="$(hits "${P}-same")"
    if [ -n "$after" ] && [ "$after" -gt "$control" ]; then
      record PASS "must-hit: the same artifact takes the writer's blocks from the store" "8 requests finished, hits ${control} -> ${after}"
    else
      record FAIL "must-hit: the same artifact takes the writer's blocks from the store" "hits ${control} -> ${after:-<none>}"
    fi
  fi
fi

echo "== 3. the other commit, on the same Binding =="
kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-same" --wait=false >/dev/null 2>&1
deployment "${P}-rev" "${P}-b"
r="$(wait_ready "${P}-rev")"
if [ "$r" != Ready ]; then
  record FAIL "the other commit's deployment goes Ready" "phase=${r:-<none>}"
else
  p_rev="$(prefix "${P}-rev")"
  before="$(hits "${P}-rev")"
  if sent "the other commit takes none of the writer's blocks" "${P}-rev" 1000 8; then
    after="$(hits "${P}-rev")"
    if [ -n "$p_rev" ] && [ "$p_rev" != "$p_writer" ] && [ -n "$after" ] && [ "$after" = "$before" ]; then
      record PASS "the other commit takes none of the writer's blocks" "prefix=${p_rev}, 8 requests finished, hits ${before} -> ${after}"
    else
      record FAIL "the other commit takes none of the writer's blocks" "prefix=${p_rev:-<none>} (writer ${p_writer}) hits ${before:-?} -> ${after:-<none>}"
    fi
  fi
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-100] ${FAILS} check(s) FAILED"; exit 1; }
