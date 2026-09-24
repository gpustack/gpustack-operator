#!/usr/bin/env bash
#
# CASE 73 — An engine under KV turnover writes the shared store; the other replica's replay of the
#            same prefixes is the reuse the chain exists for, and today it does not happen
#   (MUTATING, self-recovering)
#
#   case-73.sh <NS>
#
# Goal:        Proves the two halves of the store-reuse chain separately, because they are in
#              different states and a single verdict would hide the one that is broken.
#
#                The WRITE half is real and this case guards it: drive one engine replica with more
#                distinct prefixes than its local KV cache holds, and the connector sinks evicted
#                blocks to the shared store — the master's key count and the domain's charge both
#                rise above their pre-load baselines. Measured on the cluster this case was written
#                against (vllm 0.29.0, MooncakeStoreConnector kv_both, transfer engine 0.3.13.post1,
#                461,808-token local KV): 690,590 tokens of distinct prefixes grew master_key_count
#                from 1 to hundreds within the load window.
#
#                The READ half does not work, and this case carries it as a KNOWN-FAILURE DETECTOR
#                in the case-67 sense: rows that PASS while the defect persists and go red the day
#                it is fixed. Replay the written prefixes at the OTHER replica — and at the writer
#                itself — and every hit carrier stays at zero: vllm:external_prefix_cache_hits_total
#                on the replaying engine, mem_cache_hit_nums_ on the master. The lookup path is
#                active (its queries counter tracks every prompt token) and the local prefix cache
#                works (a same-prompt repeat hits it fully), so the miss is specific to the store
#                path. The store's content is also transient: it churns during load, drains to the
#                pre-load baseline minutes after, and master_evicted_key_count never moves — the
#                deletions are client-side. Which component deletes the objects, and whether write
#                and lookup even share a key namespace, is not observable from the surfaces this
#                operator renders; that question is recorded, not answered.
#
#              When a detector row FAILS with a nonzero delta, the reuse fix has landed: invert that
#              row and its master-side sibling into positive guards in the same commit, the way
#              case-65's bucket-limit rows were inverted.
#
# Environment: A real accelerator pool with at least TWO free exclusive cards (two server replicas;
#              one card cannot exercise the cross-replica half), an accelerated InstanceType whose
#              observed runtime lets the operator synthesize the engine image, and the model weights
#              pre-staged on every accelerator node at a hostPath. Exits 2 (input required) when
#              E2E_VB_INSTANCE_TYPE is unset rather than guessing a pool. No RDMA. Needs a registry
#              the cluster can pull the Mooncake image from — override with E2E_MOONCAKE_IMAGE.
#
# Inputs:      All real, nothing mocked. This case creates its own KVCacheBackend (one DRAM member),
#              pool, binding and a two-role ModelDeployment bound to that binding; the load comes
#              from throwaway Pods on the Mooncake image driving the engines' own HTTP APIs.
#              E2E_VB_INSTANCE_TYPE (required), E2E_VB_MODEL (default Qwen/Qwen3-0.6B — a small
#              model on purpose: prefill is cheap and every verdict here rides on counters, never on
#              TTFT, which is undiscriminating at this size), E2E_VB_ENGINE_VERSION (default 0.29.0),
#              E2E_VB_WEIGHTS (default /mnt/kvcache-weights), E2E_VB_GPU_MEM_UTIL (default 0.55).
#              The fill volume is computed from the engine's own reported kv_cache_size_tokens, so
#              the case adapts to whatever card and utilization it lands on.
#
#              REUSE MODE, for validating the verdict logic where no two cards are free: when
#              E2E_VB_EXISTING_BACKEND (a Ready backend whose status carries the Admin endpoint),
#              E2E_VB_EXISTING_DOMAIN (the reuse domain those engines share) and
#              E2E_VB_EXISTING_SERVICES (comma-separated host:port of two engine replicas on that
#              binding) are ALL set, the case creates nothing and deletes nothing it did not create:
#              it runs the toolbox, the control, the fill, the replay and every verdict row against
#              the named engines. The one row this mode cannot validate is the creation leg itself;
#              the run reports the mode in its first row so a report never overstates its coverage.
#              The Mooncake image must run on CPU-only nodes too — a CUDA-only tag crashes its
#              member there with "Failed to start store service", which is an image choice, not a
#              backend defect.
#
# Expected:    - both engine replicas reach Ready and answer completions;
#              - a same-prompt repeat on one replica hits that replica's local prefix cache (the
#                control that keeps the store-side misses from reading as a sick engine);
#              - after a fill of distinct prefixes past the local KV capacity, the master's key
#                count and the domain's charged bytes are both above their pre-load baselines;
#              - KNOWN-FAILURE DETECTOR: after replaying every written prefix at the other replica,
#                that replica's vllm:external_prefix_cache_hits_total has not moved, and the
#                master's mem_cache_hit_nums_ has not moved. PASS while the defect persists.
#
# Cleanup:     Trap removes the ModelDeployment, the probe Pods and ConfigMap, then the namespace,
#              pool and backend. A binding whose domain still holds objects is held by its
#              finalizer on purpose; the trap reports what held it and then forces the finalizer
#              off, as case-44 does, so the next run starts clean.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-73.sh <NS>}"
IMAGE="${E2E_MOONCAKE_IMAGE:-docker.io/kvcacheai/mooncake:0.3.13}"
MODEL="${E2E_VB_MODEL:-Qwen/Qwen3-0.6B}"
ENGINE_VERSION="${E2E_VB_ENGINE_VERSION:-0.29.0}"
WEIGHTS="${E2E_VB_WEIGHTS:-/mnt/kvcache-weights}"
GPU_UTIL="${E2E_VB_GPU_MEM_UTIL:-0.55}"

# Reuse mode: run every verdict against an existing engine pair and create nothing. All three
# variables must be set together; a partial set is an input error rather than a degraded run.
REUSE_BACKEND="${E2E_VB_EXISTING_BACKEND:-}"
REUSE_DOMAIN="${E2E_VB_EXISTING_DOMAIN:-}"
REUSE_SERVICES="${E2E_VB_EXISTING_SERVICES:-}"
REUSE=0
if [ -n "$REUSE_BACKEND" ] || [ -n "$REUSE_DOMAIN" ] || [ -n "$REUSE_SERVICES" ]; then
  if [ -z "$REUSE_BACKEND" ] || [ -z "$REUSE_DOMAIN" ] || [ -z "$REUSE_SERVICES" ]; then
    echo "[case-73] input error: E2E_VB_EXISTING_BACKEND, E2E_VB_EXISTING_DOMAIN and E2E_VB_EXISTING_SERVICES are set together or not at all" >&2
    exit 2
  fi
  REUSE=1
fi

INSTANCE_TYPE="${E2E_VB_INSTANCE_TYPE:-}"
if [ "$REUSE" -eq 0 ] && [ -z "$INSTANCE_TYPE" ]; then
  echo "[case-73] input required: E2E_VB_INSTANCE_TYPE must name an accelerated InstanceType" >&2
  echo "[case-73] backed by a pool with at least two free exclusive cards" >&2
  exit 2
fi

# Same suffix discipline as case-44: cluster-scoped names need more than a PID to be collision-safe.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-73-${SFX}"
POOL="kvcp-73-${SFX}"
DOMAIN="dom-73-${SFX}"
MD="md-73-${SFX}"
CM="cm-73-${SFX}"
TOOLBOX="tb-73-${SFX}"
BINDING="bind-73"

# The fill shoots for 1.5x the engine's reported local KV capacity — enough turnover that eviction
# is forced without spending ten minutes of prefill on a big card. Clamped at both ends: below 8
# requests nothing churns, above 400 the run cost outruns its verdict.
FILL_RATIO_NUM=3
FILL_RATIO_DEN=2
MIN_REQUESTS=8
MAX_REQUESTS=400

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

TMP="$(mktemp -d)"

restore() {
  echo
  echo "[case-73] cleanup"
  # Everything this case names by ITS OWN suffix or kind, never by namespace-wide listing: the run's
  # namespace can carry objects no case owns (this one was written against a namespace that already
  # held another lane's binding), and a cleanup that sweeps by kind touches them. Reuse mode names
  # no objects of its own beyond the toolbox and ConfigMap, and touches nothing else.
  kubectl -n "$NS" delete pod "$TOOLBOX" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete configmap "$CM" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "$TMP" 2>/dev/null || true
  if [ "$REUSE" -eq 1 ]; then
    return 0
  fi
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete kvcachepoolbindings.worker.gpustack.ai "$BINDING" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # Same held-binding dance as case-44, for the same reason: the master refuses to drop a non-empty
  # tenant's quota and the operator is right to wait. Scoped to THIS case's binding by name, for the
  # reason at the top of this function.
  local left=""
  for _ in $(seq 1 20); do
    kubectl -n "$NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING" -o name >/dev/null 2>&1 || break
    left="held"
    sleep 3
  done
  if [ -n "$left" ]; then
    echo "[case-73] the binding is still held after 60s; what the operator says about it:"
    kubectl -n "$NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING" \
      -o 'jsonpath={range .status.conditions[*]}{.type}={.status}({.reason}) {.message}{"\n"}{end}' 2>/dev/null \
      | sed 's/^/    /'
    echo "[case-73] forcing the finalizer off so the next run starts clean"
    kubectl -n "$NS" patch kvcachepoolbindings.worker.gpustack.ai "$BINDING" \
      --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  fi
}
trap restore EXIT

wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-120}" ns_args=()
  [ -n "${6:-}" ] && ns_args=(-n "$6")
  local got=""
  for _ in $(seq 1 "$secs"); do
    got="$(kubectl "${ns_args[@]}" get "$kind" "$name" -o jsonpath="$path" 2>/dev/null)"
    [ "$got" = "$want" ] && return 0
    sleep 1
  done
  echo "$got"
  return 1
}

fail_out() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-73] ${FAILS} check(s) FAILED"; exit 1; }
  exit 1
}

# One metric value off one Prometheus exposition, fetched through the toolbox Pod because the
# engines and the leader answer only inside the cluster. The tail -1 collapses the _created sibling
# of a counter family onto its _total line's value by filtering it upstream; the caller's pattern
# is expected to be specific enough that exactly one line survives.
engval() { # <service-dns> <pattern>
  kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/snapm.py \
    --url "http://${1}/metrics" --filter "$2" 2>/dev/null \
    | grep -v _created | head -1 | awk '{print $NF}'
}

mval() { # <master-admin-dns> <pattern>
  kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/snapm.py \
    --url "http://${1}/metrics" --filter "$2" 2>/dev/null \
    | grep -v '^#' | head -1 | awk '{print $NF}'
}

echo "== 1. a store, a binding, and a two-replica engine =="

if [ "$REUSE" -eq 1 ]; then
  # Nothing is created; the variables the rest of the case reads are pointed at the existing pair.
  # Reachability is not asserted here on purpose: the baselines in section 2 read every engine and
  # the master through the toolbox, and a dead endpoint fails there with a named object.
  BACKEND="$REUSE_BACKEND"
  DOMAIN="$REUSE_DOMAIN"
  SVC_A="${REUSE_SERVICES%%,*}"
  SVC_B="${REUSE_SERVICES##*,}"
  record PASS "reuse mode: verdicts run against an existing engine pair" \
    "backend ${BACKEND}, domain ${DOMAIN}, engines ${SVC_A} and ${SVC_B}; this run does not exercise the case's own creation leg"
else
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  connection:
    managed:
      leader:
        multiTenancy: true
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 4Gi
YAML

  if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 240 >/dev/null; then
    record FAIL "backend ready" "the master did not reach Ready in 240s; nothing below can run"
    fail_out
  fi

  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL}
spec:
  backends: [${BACKEND}]
  quota:
    total: 4Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: ${BINDING}, namespace: ${NS}}
spec:
  poolRef: {name: ${POOL}}
  quota: {ceiling: 4Gi}
  domain: {name: ${DOMAIN}, blockSize: 16, dtype: bfloat16}
YAML

  if ! wait_for kvcachepoolbindings.worker.gpustack.ai "$BINDING" '{.status.phase}' Ready 240 "$NS" >/dev/null; then
    record FAIL "binding ready" "the binding did not reach Ready in 240s; the engine below would run against a domain the ledger does not know"
    fail_out
  fi

  # The engine command is the thing being exercised, so the deployment mirrors the shape this case was
  # measured on: two equal server roles, one card each, the utilization lowered (a store member or a
  # sibling replica may hold device memory the engine's free-memory probe cannot see), weights from
  # the node's own disk, and the kv cache bound to this case's binding. The engine version is an
  # input because the synthesized image tag hangs off it.
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
  engine:
    name: vllm
    version: "${ENGINE_VERSION}"
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
    - name: server-a
      kind: server
      replicas: 1
      instanceType: ${INSTANCE_TYPE}
      resources: {accelerator: 1}
      extraArgs: ["--gpu-memory-utilization", "${GPU_UTIL}"]
      additionalVolumes:
        - {mountPath: /weights, readOnly: true, hostPath: {path: ${WEIGHTS}, type: Directory}}
      env:
        - {name: HF_HOME, value: /weights}
        - {name: HF_HUB_OFFLINE, value: "1"}
    - name: server-b
      kind: server
      replicas: 1
      instanceType: ${INSTANCE_TYPE}
      resources: {accelerator: 1}
      extraArgs: ["--gpu-memory-utilization", "${GPU_UTIL}"]
      additionalVolumes:
        - {mountPath: /weights, readOnly: true, hostPath: {path: ${WEIGHTS}, type: Directory}}
      env:
        - {name: HF_HOME, value: /weights}
        - {name: HF_HUB_OFFLINE, value: "1"}
YAML

  # Both replicas Ready, or the case reports it rather than deriving anything from a half-started
  # engine. Discovery and readiness are separate waits: the pods can take a while to be created at
  # all (queue admission), and then a while to become Ready (image pull, CUDA init, weights mount).
  POD_A=""; POD_B=""
  for _ in $(seq 1 30); do
    POD_A="$(kubectl -n "$NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep "^${MD}-server-a-" | head -1)"
    POD_B="$(kubectl -n "$NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep "^${MD}-server-b-" | head -1)"
    [ -n "$POD_A" ] && [ -n "$POD_B" ] && break
    sleep 6
  done
  SVC_A="${MD}-server-a.${NS}.svc:8000"
  SVC_B="${MD}-server-b.${NS}.svc:8000"
  if [ -z "$POD_A" ] || [ -z "$POD_B" ] \
    || ! kubectl -n "$NS" wait --for=condition=Ready "pod/$POD_A" "pod/$POD_B" --timeout=540s >/dev/null 2>&1; then
    record FAIL "both engine replicas become Ready" \
      "server-a='${POD_A:-absent}' server-b='${POD_B:-absent}' — check image synthesis, weights at ${WEIGHTS}, and that the pool really has two free cards"
    fail_out
  fi
  record PASS "both engine replicas become Ready" "${POD_A} and ${POD_B}"
fi

echo "== 2. toolbox, baselines, and the local-cache control =="

# The load scripts ride a ConfigMap into a long-lived toolbox Pod. fill.py's determinism contract
# is the case's replay mechanism: prompt(seed) is a pure function of the embedded word list and the
# seed, so replaying a seed range reproduces the prompts byte for byte.
cat >"$TMP/scripts.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: CMNAME
  namespace: NSNAME
data:
  fill.py: |
    import argparse
    import json
    import random
    import sys
    import threading
    import time
    import urllib.request
    import urllib.error

    WORDS = (
        "time year people way day man thing woman life child world school state family "
        "student group country problem hand part place case week company system program "
        "question work government number night point home water room mother area money "
        "story fact month lot right study book eye job word business issue side kind "
        "head house service friend father power hour game line end member law car city "
        "community name president team minute idea kid body information back parent face "
        "others level office door health person art war history party result change "
        "morning reason research girl guy moment air teacher force education foot boy "
        "age policy process music market sense nation plan college interest death "
        "experience effect use class control care field development role effort rate "
        "heart drug show leader light voice wife police mind price report decision "
        "son view relationship town road arm difference value building action model "
        "season society tax director position player record paper space ground form "
        "event official matter center couple site project activity star table need "
        "court produce eat teach oil situation cost industry figure street image "
        "phone data picture practice piece land product doctor wall patient worker "
        "news test movie north love support technology step baby computer type "
        "attention film tree source truth performance radio recognition board "
        "influence leg ocean spread horse return natural shape cause answer machine "
        "township texture plasma lattice manifold gradient tensor kernel epoch beam "
        "quantum photon ion vertex matrix suffix prefix token cache buffer queue "
        "shard replica ledger tenant quota eviction socket stream"
    ).split()

    def prompt_for(seed, words):
        rng = random.Random(seed * 7919 + 13)
        body = " ".join(rng.choice(WORDS) for _ in range(words))
        return "document %d: %s" % (seed, body)

    def one_request(url, seed, words, max_tokens, results, idx):
        payload = json.dumps({
            "model": "MODELNAME",
            "prompt": prompt_for(seed, words),
            "max_tokens": max_tokens,
            "temperature": 0.0,
        }).encode()
        req = urllib.request.Request(
            url.rstrip("/") + "/v1/completions", data=payload,
            headers={"Content-Type": "application/json"})
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=600) as resp:
                body = json.loads(resp.read().decode())
            usage = body.get("usage", {})
            results[idx] = (resp.status, usage.get("prompt_tokens", -1))
            print("req seed=%d status=%d ptoks=%d lat=%.3f" % (
                seed, resp.status, usage.get("prompt_tokens", -1), time.time() - t0), flush=True)
        except Exception as err:
            results[idx] = (-1, -1)
            print("req seed=%d status=-1 ERR %r" % (seed, err), flush=True)

    def main():
        ap = argparse.ArgumentParser()
        ap.add_argument("--url", required=True)
        ap.add_argument("--seed-start", type=int, required=True)
        ap.add_argument("--count", type=int, required=True)
        ap.add_argument("--conc", type=int, default=8)
        ap.add_argument("--words", type=int, default=6000)
        ap.add_argument("--max-tokens", type=int, default=4)
        args = ap.parse_args()

        seeds = list(range(args.seed_start, args.seed_start + args.count))
        results = [None] * len(seeds)
        next_idx = [0]
        lock = threading.Lock()

        def worker():
            while True:
                with lock:
                    if next_idx[0] >= len(seeds):
                        return
                    idx = next_idx[0]
                    next_idx[0] += 1
                one_request(args.url, seeds[idx], args.words, args.max_tokens, results, idx)

        threads = [threading.Thread(target=worker) for _ in range(args.conc)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        ok = [r for r in results if r and r[0] == 200]
        ptoks = sum(r[1] for r in ok if r[1] > 0)
        print("summary total=%d ok=%d fail=%d prompt_tokens_sum=%d" % (
            len(seeds), len(ok), len(seeds) - len(ok), ptoks), flush=True)
        if len(ok) < len(seeds):
            sys.exit(1)

    if __name__ == "__main__":
        main()
  snapm.py: |
    import argparse
    import re
    import urllib.request

    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--filter", default="cache|hit|mooncake|kv")
    args = ap.parse_args()

    body = urllib.request.urlopen(args.url, timeout=10).read().decode()
    pat = re.compile(args.filter)
    for line in body.splitlines():
        if line.startswith("#"):
            continue
        if pat.search(line):
            print(line)
YAML
sed -i.bak "s/CMNAME/$CM/; s/NSNAME/$NS/; s/MODELNAME/$(printf '%s' "$MODEL" | sed 's/[\/&]/\\&/g')/" "$TMP/scripts.yaml" && rm -f "$TMP/scripts.yaml.bak"
kubectl apply -f "$TMP/scripts.yaml" >/dev/null 2>&1

kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${TOOLBOX}
  namespace: ${NS}
spec:
  restartPolicy: Never
  containers:
    - name: toolbox
      image: ${IMAGE}
      command: ["python3", "-c", "import time; time.sleep(14400)"]
      volumeMounts:
        - {name: scripts, mountPath: /scripts}
  volumes:
    - name: scripts
      configMap: {name: ${CM}}
YAML
if ! kubectl -n "$NS" wait --for=condition=Ready "pod/$TOOLBOX" --timeout=300s >/dev/null 2>&1; then
  record FAIL "the toolbox pod becomes Ready" "the load generator could not start; nothing below can run"
  fail_out
fi

# The master's admin endpoint off the backend's own status, because that is the address the
# operator itself scrapes and the one whose figures a Binding reports.
ADMIN="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o json 2>/dev/null \
  | python3 -c 'import json,sys; print(next(e["address"] for e in json.load(sys.stdin)["status"]["endpoints"] if e["name"]=="Admin"))' 2>/dev/null || true)"
if [ -z "$ADMIN" ]; then
  record FAIL "the backend publishes an admin endpoint" "no Admin address on the backend's status"
  fail_out
fi

KEYS0="$(mval "$ADMIN" '^master_key_count ')"
HITS0="$(mval "$ADMIN" '^mem_cache_hit_nums_ ')"
LOCAL_A0="$(engval "$SVC_A" 'prompt_tokens_by_source_total.*local_cache_hit')"
EXT_B0="$(engval "$SVC_B" 'external_prefix_cache_hits_total')"
KV_TOKENS="$(kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/snapm.py --url "http://${SVC_A}/metrics" --filter 'cache_config_info' 2>/dev/null \
  | sed -n 's/.*kv_cache_size_tokens="\([0-9]*\)".*/\1/p' | head -1)"
if [ -z "$KV_TOKENS" ] || [ "$KV_TOKENS" = "0" ]; then
  record FAIL "the engine reports its local KV capacity" \
    "kv_cache_size_tokens read empty from server-a's cache_config_info"
  fail_out
fi

# The local-cache control, run BEFORE any fill so its blocks are the only thing resident: the same
# prompt twice, and the second request must hit. This is what keeps the store-side zero below from
# reading as an engine whose prefix caching is off.
CONTROL_SEED=900000
kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/fill.py \
  --url "http://${SVC_A}" --seed-start "$CONTROL_SEED" --count 1 --conc 1 --words 6000 \
  >"$TMP/control1.txt" 2>&1
kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/fill.py \
  --url "http://${SVC_A}" --seed-start "$CONTROL_SEED" --count 1 --conc 1 --words 6000 \
  >"$TMP/control2.txt" 2>&1
PTOKS="$(sed -n 's/.*ptoks=\([0-9]*\).*/\1/p' "$TMP/control1.txt" | head -1)"
LOCAL_A1="$(engval "$SVC_A" 'prompt_tokens_by_source_total.*local_cache_hit')"
if [ -n "$PTOKS" ] && [ -n "$LOCAL_A0" ] && [ -n "$LOCAL_A1" ] \
  && awk -v a="$LOCAL_A0" -v b="$LOCAL_A1" 'BEGIN{exit !(b>a)}'; then
  record PASS "a same-prompt repeat hits the engine's local prefix cache" \
    "local_cache_hit ${LOCAL_A0} -> ${LOCAL_A1} for a ${PTOKS}-token prompt: the engine's own reuse works, so the store-side misses below are not a sick engine"
else
  record FAIL "a same-prompt repeat hits the engine's local prefix cache" \
    "local_cache_hit '${LOCAL_A0:-absent}' -> '${LOCAL_A1:-absent}'; with local reuse dead the store-side rows cannot attribute their misses"
fi

echo "== 3. fill past the local KV capacity, then read the store's verdict =="

N=$(( KV_TOKENS * FILL_RATIO_NUM / FILL_RATIO_DEN / PTOKS + 1 ))
[ "$N" -lt "$MIN_REQUESTS" ] && N=$MIN_REQUESTS
[ "$N" -gt "$MAX_REQUESTS" ] && N=$MAX_REQUESTS
echo "[case-73] local KV ${KV_TOKENS} tokens, ${PTOKS} tokens/prompt -> ${N} distinct requests"

kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/fill.py \
  --url "http://${SVC_A}" --seed-start 0 --count "$N" --conc 8 --words 6000 \
  >"$TMP/fill.txt" 2>&1
FILL_OK="$(sed -n 's/summary total=[0-9]* ok=\([0-9]*\).*/\1/p' "$TMP/fill.txt" | head -1)"
FILL_TOKENS="$(sed -n 's/.*prompt_tokens_sum=\([0-9]*\).*/\1/p' "$TMP/fill.txt" | head -1)"
if [ "$FILL_OK" != "$N" ]; then
  record FAIL "the fill completes" "${FILL_OK:-0} of ${N} requests succeeded; the turnover premise did not hold"
  fail_out
fi

# The store's key count and charge both CHURN during and after load (write-then-delete, master
# eviction counters unmoved), so a single read can land on an oscillation low. The verdicts take the
# maximum over a one-minute window, which is the honest reading of a spiky signal rather than a
# lucky one.
KEYS_MAX="${KEYS0:-0}"
CHARGE_MAX=0
for _ in $(seq 1 15); do
  K="$(mval "$ADMIN" '^master_key_count ')"
  [ -n "$K" ] && awk -v a="$KEYS_MAX" -v b="$K" 'BEGIN{exit !(b>a)}' && KEYS_MAX=$K
  C="$(kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/snapm.py --url "http://${ADMIN}/metrics" --filter 'charged_bytes' 2>/dev/null \
    | grep "$DOMAIN" | awk '{print $NF}' | head -1)"
  [ -n "$C" ] && awk -v a="$CHARGE_MAX" -v b="$C" 'BEGIN{exit !(b>a)}' && CHARGE_MAX=$C
  sleep 4
done

if awk -v a="${KEYS0:-0}" -v b="$KEYS_MAX" 'BEGIN{exit !(b>a+10)}'; then
  record PASS "turnover writes reach the shared store" \
    "master_key_count ${KEYS0:-?} -> ${KEYS_MAX} (max over 60s) after ${FILL_TOKENS} tokens of distinct prefixes through one replica"
else
  record FAIL "turnover writes reach the shared store" \
    "master_key_count ${KEYS0:-?} -> ${KEYS_MAX} (max over 60s) after ${FILL_TOKENS} tokens; the connector's write path did not fire under eviction"
fi
if [ -n "$CHARGE_MAX" ] && [ "$CHARGE_MAX" -gt 1835008 ]; then
  record PASS "the domain's charge rises with the writes" \
    "charged_bytes peak ${CHARGE_MAX} for domain ${DOMAIN}, above the single-object floor this store idles at"
else
  record FAIL "the domain's charge rises with the writes" \
    "charged_bytes peak '${CHARGE_MAX:-absent}' for domain ${DOMAIN}"
fi

echo "== 4. the reuse half: replay every written prefix at the other replica =="

kubectl -n "$NS" exec "$TOOLBOX" -- python3 /scripts/fill.py \
  --url "http://${SVC_B}" --seed-start 0 --count "$N" --conc 4 --words 6000 \
  >"$TMP/replay.txt" 2>&1
REPLAY_OK="$(sed -n 's/summary total=[0-9]* ok=\([0-9]*\).*/\1/p' "$TMP/replay.txt" | head -1)"
if [ "$REPLAY_OK" != "$N" ]; then
  record FAIL "the replay completes" "${REPLAY_OK:-0} of ${N} replay requests succeeded"
  fail_out
fi

EXT_B1="$(engval "$SVC_B" 'external_prefix_cache_hits_total')"
HITS1="$(mval "$ADMIN" '^mem_cache_hit_nums_ ')"

# KNOWN-FAILURE DETECTORS, case-67 polarity: these rows PASS while the defect persists. The day one
# FAILS with a nonzero delta the reuse fix has landed, and that row plus its master-side sibling get
# inverted into positive guards in the same commit. Measured defect: every replayed token computes
# locally (prompt_tokens_by_source local_compute rises by the full replay volume) while the store
# demonstrably holds keys, and the same zero holds for the writer's own replica replaying itself.
DELTA_EXT="$(awk -v a="${EXT_B0:-0}" -v b="${EXT_B1:-0}" 'BEGIN{print b-a}')"
if [ "$DELTA_EXT" = "0" ]; then
  record PASS "the other replica's store-path hits stay at zero (KNOWN-FAILURE DETECTOR)" \
    "external_prefix_cache_hits_total ${EXT_B0:-?} -> ${EXT_B1:-?} across a ${REPLAY_OK}-prompt, ${FILL_TOKENS}-token replay of prefixes that replica never served: content the store holds is not visible to the lookup path. When this row FAILS with a nonzero delta the fix has landed — invert it"
else
  record FAIL "the other replica's store-path hits stay at zero (KNOWN-FAILURE DETECTOR)" \
    "external_prefix_cache_hits_total moved by ${DELTA_EXT} tokens: cross-replica reuse WORKS now — invert this row and the master-side one into positive guards"
fi
if [ "${HITS1:-0}" = "0" ]; then
  record PASS "the master counts no served hits (KNOWN-FAILURE DETECTOR)" \
    "mem_cache_hit_nums_ ${HITS0:-?} -> ${HITS1:-?}: the master's own read counters agree with the engine-side zero. When this row FAILS the fix has landed — invert it"
else
  record FAIL "the master counts no served hits (KNOWN-FAILURE DETECTOR)" \
    "mem_cache_hit_nums_ moved ${HITS0:-?} -> ${HITS1:-?}: the master is serving reads — invert this row and the engine-side one into positive guards"
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-73] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-73] all checks passed"
