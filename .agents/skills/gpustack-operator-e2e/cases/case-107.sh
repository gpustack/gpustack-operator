#!/usr/bin/env bash
#
# CASE 107 — A node-delivered model's Pods prefer the nodes holding its weights: they land there when
#            it fits, elsewhere at once when it does not, and a change in which nodes hold the weights
#            rolls nothing (MUTATING, self-recovering; AUTO-SKIPS without two workers running the
#            model-manager plugin)
#
#   case-107.sh <NS>
#
# Goal:        Prove the placement preference end to end through Kueue's topology-aware scheduling:
#              a replica lands on the node holding its digest when that node has room, measured
#              against an exact chance baseline; it is admitted on another node without waiting when
#              the hot node is full; the set of hot nodes changing leaves running replicas alone; and
#              no Pod carrying the preference asks Kueue for a preferred topology level.
# Environment: A kind (or other) cluster installed from this chart with modelManager.enabled, at least
#              two workers whose CSINode lists the plugin's driver (four gives the sharpest baseline),
#              a materialized scheduling chain with an InstanceType (run case-1 first), and the stock
#              python image pullable. NO GPU. Exits 2, NOTHING WAS VERIFIED, with fewer than two such
#              workers, without the plugin, or without an InstanceType. The Kueue gate
#              TASRespectNodeAffinityPreferred is read, not set: the case prints it, and with the gate
#              off the hot-node row is expected to FAIL, which is how the instrument is checked.
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS> whose repositories are generated from their
#              names, one per trial, so every trial has a digest no node holds yet; a warm-up Pod pinned
#              to the trial's hot node, which makes that node download and publish the digest; a
#              placeholder Pod filling the hot node's CPU. Real: the plugin, NodeModelStore status,
#              the worker's preference, Kueue's admission and topology assignment, kube-scheduler.
#              The hot node of each trial is drawn uniformly from the eligible workers with a printed
#              seed (E2E_C107_SEED), so the chance of landing on it without the preference is at most
#              one over their number whatever Kueue's packing order. Override with
#              E2E_MD_INSTANCE_TYPE, E2E_MD_IMAGE, E2E_C107_TRIALS (20), E2E_C107_FULL_TRIALS (10).
# Expected:    - hot node preferred: the consumers land on their trial's hot node often enough to reject
#                the chance baseline with a one-sided exact binomial p below 0.001 (12 of 20 for four
#                workers);
#              - every consumer carries exactly one preferred term, naming its trial's hot node;
#              - hot node full: every consumer is admitted on another node, and no Workload ever reads
#                QuotaReserved=False;
#              - no rollout: a replica of two members carries the same term on both, and after its
#                digest becomes Ready on one more node its Pod UIDs and spec-hash annotations are
#                unchanged across two intervals;
#              - an Instance naming a warmed artifact carries the term naming that artifact's node;
#              - no Pod carrying a preferred term carries kueue.x-k8s.io/podset-preferred-topology.
# Cleanup:     Trap deletes the deployments, the Instance, Pods, artifacts, the placeholder, the hub
#              and the deployments' namespace, and puts back the Settings it changed.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-107.sh <NS>}"
MDNS="${NS}-c107"
P=c107
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
TRIALS="${E2E_C107_TRIALS:-20}"
FULL_TRIALS="${E2E_C107_FULL_TRIALS:-10}"
SEED="${E2E_C107_SEED:-$(date +%s)}"
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
SCRATCH="$(mktemp -d)"

KEYS="model-artifact-huggingface-endpoint model-artifact-delivery-mode"
mkdir -p "$SCRATCH/orig"
for k in $KEYS; do
  kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.$k}" >"$SCRATCH/orig/$k" 2>/dev/null
done
# restore KEY: the value KEY had before the case, or no value when it had none.
restore() {
  if [ -s "$SCRATCH/orig/$1" ]; then setting_set "$1" "$(base64 -d <"$SCRATCH/orig/$1")"; else setting_unset "$1"; fi
}

cleanup() {
  echo
  echo "[case-107] cleanup"
  kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai --all --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$MDNS" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
  kubectl -n "$MDNS" delete pod "${P}-placeholder" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1
  for _ in $(seq 1 24); do [ -z "$(kubectl -n "$MDNS" get pods -o name 2>/dev/null)" ] && break; sleep 5; done
  kubectl -n "$MDNS" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=107 --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc "${P}-hub" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${P}-hub-script" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$MDNS" --ignore-not-found --wait=false >/dev/null 2>&1
  for k in $KEYS; do restore "$k"; done
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

get_uid() { kubectl -n "$MDNS" get modelartifacts.worker.gpustack.ai "$1" -o jsonpath='{.metadata.uid}'; }

# mdartifact NAME REPOSITORY: a Hugging Face ModelArtifact in the deployments' namespace.
mdartifact() {
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: $1, namespace: ${MDNS}, labels: {e2e.gpustack.ai/case: "107"}}
spec:
  source:
    huggingFace: {repository: $2, revision: main}
YAML
}

# md NAME ARTIFACT [SIZE]: a deployment of one replica of SIZE members (1) mounting ARTIFACT.
md() {
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${MDNS}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c107, artifactRef: {name: $2}}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, size: ${3:-1}, image: "${IMAGE}", command: ["/pause"]}
YAML
}

# warm ARTIFACT NODE: resolves the artifact, has NODE download and publish it through a Pod pinned
# there, waits until the node lists the digest Ready, and removes the Pod. Prints the digest, or
# nothing when the node did not publish it. The node's status is the signal, not the Pod's phase:
# the plugin publishes in the background of the first mount call, and the Pod runs only at kubelet's
# next mount retry, which can come two minutes later.
warm() {
  local d pod="${P}-warm-$1"
  d="$(wait_resolved "$MDNS" "$1" 120)"
  [ -n "$d" ] || return 0
  consumer "$MDNS" "$pod" "$2" "$1" "$(get_uid "$1")" "$d"
  for _ in $(seq 1 150); do [ "$(store_state "$2" "$d")" = "Ready|" ] && break; sleep 2; done
  kubectl -n "$MDNS" delete pod "$pod" --wait=true --timeout=120s >/dev/null 2>&1
  [ "$(store_state "$2" "$d")" = "Ready|" ] && printf '%s' "$d"
}

# md_pods NAME: the deployment's Pods, one name a line.
md_pods() {
  kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=$1" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

# bound_node POD BOUND: waits for the Pod to be bound, printing its node.
bound_node() {
  local n=""
  for _ in $(seq 1 $(( $2 / 2 ))); do
    n="$(kubectl -n "$MDNS" get pod "$1" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
    [ -n "$n" ] && break
    sleep 2
  done
  printf '%s' "$n"
}

# admission BOUND: polls the namespace's Workloads until one reads QuotaReserved=True, printing
# "<seconds> <whether any read False on the way>".
admission() {
  local start seen_false=no states
  start="$(date +%s)"
  while [ $(( $(date +%s) - start )) -lt "$1" ]; do
    states="$(kubectl -n "$MDNS" get workloads.kueue.x-k8s.io \
      -o jsonpath="{range .items[*]}{.status.conditions[?(@.type=='QuotaReserved')].status}{' '}{end}" 2>/dev/null)"
    case " $states " in *" False "*) seen_false=yes ;; esac
    case " $states " in *" True "*) break ;; esac
    sleep 0.5
  done
  printf '%s %s' "$(( $(date +%s) - start ))" "$seen_false"
}

# terms POD: the Pod's preferred node-affinity terms as "weight:key=v1,v2" separated by ";", and
# "|annotated" when it asks for a preferred topology level.
terms() {
  kubectl -n "$MDNS" get pod "$1" -o json 2>/dev/null | python3 -c "
import json, sys
p = json.load(sys.stdin)
na = ((p['spec'].get('affinity') or {}).get('nodeAffinity') or {})
out = []
for t in na.get('preferredDuringSchedulingIgnoredDuringExecution') or []:
    for e in t['preference'].get('matchExpressions') or []:
        out.append('%d:%s=%s' % (t['weight'], e['key'], ','.join(e.get('values') or [])))
ann = 'kueue.x-k8s.io/podset-preferred-topology' in (p['metadata'].get('annotations') or {})
print(';'.join(out) + ('|annotated' if ann else ''))"
}

# md_gone NAME: deletes the deployment and waits until none of its Pods or Workloads is left.
md_gone() {
  kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai "$1" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 90); do
    [ -z "$(md_pods "$1")" ] && [ -z "$(kubectl -n "$MDNS" get workloads.kueue.x-k8s.io -o name 2>/dev/null)" ] && return 0
    sleep 2
  done
  return 1
}

# requested_cpu_m NODE: the CPU the node's running Pods request, in millicores.
requested_cpu_m() {
  kubectl get pods -A --field-selector "spec.nodeName=$1" -o json 2>/dev/null | python3 -c "
import json, sys
def m(q):
    q = str(q)
    return int(float(q[:-1])) if q.endswith('m') else int(float(q) * 1000)
total = 0
for p in json.load(sys.stdin)['items']:
    if p['status'].get('phase') in ('Succeeded', 'Failed'):
        continue
    for c in p['spec'].get('containers', []):
        total += m(((c.get('resources') or {}).get('requests') or {}).get('cpu', '0'))
print(total)"
}

# cpu_m QUANTITY: a CPU quantity in millicores.
cpu_m() { python3 -c "q='$1'; print(int(float(q[:-1])) if q.endswith('m') else int(float(q) * 1000))"; }

# ---------------------------------------------------------------- preconditions
kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1 \
  || { echo "[case-107] the model-manager plugin is not installed; NOTHING WAS VERIFIED"; exit 2; }
POOL=()
for n in $(model_workers); do
  kubectl get csinode "$n" -o jsonpath='{.spec.drivers[*].name}' 2>/dev/null | tr ' ' '\n' \
    | grep -qx model.csi.gpustack.ai && POOL+=("$n")
done
[ "${#POOL[@]}" -ge 2 ] || { echo "[case-107] needs two workers running the plugin; NOTHING WAS VERIFIED"; exit 2; }
K="${#POOL[@]}"
[ -n "$IT" ] || IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
[ -n "$IT" ] || { echo "[case-107] no InstanceType; run case-1 first; NOTHING WAS VERIFIED" >&2; exit 2; }
kubectl create namespace "$MDNS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
for _ in $(seq 1 30); do [ -n "$(kubectl -n "$MDNS" get localqueues -o name 2>/dev/null)" ] && break; sleep 2; done

GATE_CONFIG="$(kubectl -n "$SYSTEM_NS" get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}' 2>/dev/null \
  | awk '$1 == "TASRespectNodeAffinityPreferred:" {print $2}')"
GATE_LOADED="$(kubectl -n "$SYSTEM_NS" logs deploy/kueue-controller-manager 2>/dev/null \
  | grep -o 'TASRespectNodeAffinityPreferred[^,}]*' | tail -1 | grep -o 'true\|false')"
GATE="config=${GATE_CONFIG:-unset} loaded=${GATE_LOADED:-unread}"
NEED="$(python3 -c "
from math import comb
n, p = ${TRIALS}, 1 / ${K}
for h in range(n + 1):
    if sum(comb(n, i) * p**i * (1 - p)**(n - i) for i in range(h, n + 1)) < 0.001:
        print(h); break
else:
    print(n + 1)")"

REPOS="$(python3 -c "
import json
mib = 1 << 20
repos = {}
for i in range(1, ${TRIALS} + 1):
    repos['e2e/hot-%d' % i] = {'files': {'config.json': {'size': 100 + i}, 'w.bin': {'size': mib, 'lfs': True}}}
for i in range(1, ${FULL_TRIALS} + 1):
    repos['e2e/full-%d' % i] = {'files': {'config.json': {'size': 300 + i}, 'w.bin': {'size': mib, 'lfs': True}}}
repos['e2e/roll'] = {'files': {'config.json': {'size': 500}, 'w.bin': {'size': mib, 'lfs': True}}}
repos['e2e/inst'] = {'files': {'config.json': {'size': 600}, 'w.bin': {'size': mib, 'lfs': True}}}
print(json.dumps(repos))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
setting_set model-artifact-delivery-mode Node
settings_settle
echo "[case-107] hub at ${HUB_URL}; eligible workers ${POOL[*]}; InstanceType ${IT}; seed ${SEED}"
echo "[case-107] Kueue TASRespectNodeAffinityPreferred: ${GATE}; ${NEED} of ${TRIALS} hits reject the 1/${K} baseline"

WITH_TERM=0 ANNOTATED=0 REQ_M="" T=""
# note_terms POD: reads the Pod's terms into T and counts a Pod carrying a preferred term, and one
# that also asks for a preferred level. It sets globals, so it is never called in a subshell.
note_terms() {
  T=""
  [ -n "$1" ] || return 0
  T="$(terms "$1")"
  case "$T" in "") ;; *"|annotated") WITH_TERM=$((WITH_TERM + 1)); ANNOTATED=$((ANNOTATED + 1)) ;; *) WITH_TERM=$((WITH_TERM + 1)) ;; esac
}

# ---------------------------------------------------------------- hot node preferred
read -r -a HOTS <<<"$(python3 -c "
import random
r = random.Random(${SEED})
print(' '.join(str(r.randrange(${K})) for _ in range(${TRIALS})))")"
HITS=0 WRONG_TERM=0 LOST=0
for i in $(seq 1 "$TRIALS"); do
  hot="${POOL[${HOTS[$((i - 1))]}]}"
  mdartifact "${P}-hot-${i}" "e2e/hot-${i}"
  d="$(warm "${P}-hot-${i}" "$hot")"
  if [ -z "$d" ]; then
    LOST=$((LOST + 1)); echo "[case-107] trial ${i}: ${hot} did not publish its digest"; continue
  fi
  md "${P}-hot-${i}" "${P}-hot-${i}"
  pod=""
  for _ in $(seq 1 60); do pod="$(md_pods "${P}-hot-${i}" | head -1)"; [ -n "$pod" ] && break; sleep 1; done
  node="$( [ -n "$pod" ] && bound_node "$pod" 120)"
  note_terms "$pod"; t="$T"
  [ -z "$REQ_M" ] && [ -n "$pod" ] && REQ_M="$(cpu_m "$(kubectl -n "$MDNS" get pod "$pod" -o jsonpath='{.spec.containers[0].resources.requests.cpu}')")"
  [ "$node" = "$hot" ] && HITS=$((HITS + 1))
  [ "${t%|annotated}" = "100:kubernetes.io/hostname=${hot}" ] || WRONG_TERM=$((WRONG_TERM + 1))
  [ -z "$node" ] && LOST=$((LOST + 1))
  echo "[case-107] trial ${i}: hot ${hot}, landed ${node:-<unbound>}, terms ${t:-<none>}"
  md_gone "${P}-hot-${i}" || echo "[case-107] trial ${i}: the deployment did not go away in time"
done
if [ "$LOST" -eq 0 ] && [ "$HITS" -ge "$NEED" ]; then
  record PASS "consumers landed on their trial's hot node ${HITS} of ${TRIALS} times (baseline 1/${K}, ${NEED} needed)" "${GATE}"
else
  record FAIL "consumers land on their trial's hot node at least ${NEED} of ${TRIALS} times (baseline 1/${K})" "hits=${HITS} lost=${LOST} ${GATE}"
fi
if [ "$LOST" -eq 0 ] && [ "$WRONG_TERM" -eq 0 ]; then
  record PASS "every consumer carried exactly one preferred term, naming its trial's hot node" "${TRIALS} trials"
else
  record FAIL "every consumer carries exactly one preferred term naming its trial's hot node" "wrong=${WRONG_TERM} lost=${LOST}"
fi

# ---------------------------------------------------------------- hot node full
hot="${POOL[0]}"
alloc="$(cpu_m "$(kubectl get node "$hot" -o jsonpath='{.status.allocatable.cpu}')")"
fill=$(( alloc - $(requested_cpu_m "$hot") - ${REQ_M:-0} / 2 ))
if [ -z "$REQ_M" ] || [ "$REQ_M" -le 0 ] || [ "$fill" -le 0 ]; then
  record FAIL "the hot node can be filled below one consumer" "alloc=${alloc}m request=${REQ_M:-?}m fill=${fill}m"
else
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-placeholder, namespace: ${MDNS}}
spec:
  nodeName: ${hot}
  terminationGracePeriodSeconds: 1
  containers:
    - {name: c, image: "${IMAGE}", resources: {requests: {cpu: "${fill}m"}, limits: {cpu: "${fill}m"}}}
YAML
  pod_ready "$MDNS" "${P}-placeholder" 120 || record FAIL "the placeholder runs on the hot node" "$hot"
  ELSEWHERE=0 WAITED=0 NAMED=0 SLOWEST=0
  for j in $(seq 1 "$FULL_TRIALS"); do
    mdartifact "${P}-full-${j}" "e2e/full-${j}"
    d="$(warm "${P}-full-${j}" "$hot")"
    [ -n "$d" ] || { echo "[case-107] full trial ${j}: ${hot} did not publish its digest"; continue; }
    md "${P}-full-${j}" "${P}-full-${j}"
    read -r secs waited <<<"$(admission 120)"
    [ "$waited" = yes ] && WAITED=$((WAITED + 1))
    [ "$secs" -gt "$SLOWEST" ] && SLOWEST="$secs"
    pod="$(md_pods "${P}-full-${j}" | head -1)"
    node="$( [ -n "$pod" ] && bound_node "$pod" 120)"
    note_terms "$pod"; t="$T"
    [ -n "$node" ] && [ "$node" != "$hot" ] && ELSEWHERE=$((ELSEWHERE + 1))
    [ "${t%|annotated}" = "100:kubernetes.io/hostname=${hot}" ] && NAMED=$((NAMED + 1))
    echo "[case-107] full trial ${j}: hot ${hot} full, landed ${node:-<unbound>} after ${secs}s, QuotaReserved=False seen: ${waited}"
    md_gone "${P}-full-${j}" || echo "[case-107] full trial ${j}: the deployment did not go away in time"
  done
  if [ "$ELSEWHERE" -eq "$FULL_TRIALS" ] && [ "$WAITED" -eq 0 ] && [ "$NAMED" -eq "$FULL_TRIALS" ]; then
    record PASS "with the hot node full, every consumer was admitted elsewhere and none read QuotaReserved=False" "${FULL_TRIALS} trials, slowest ${SLOWEST}s"
  else
    record FAIL "with the hot node full, every consumer is admitted elsewhere without waiting" "elsewhere=${ELSEWHERE} waited=${WAITED} named-hot=${NAMED} of ${FULL_TRIALS}"
  fi
  kubectl -n "$MDNS" delete pod "${P}-placeholder" --wait=true --timeout=60s >/dev/null 2>&1
fi

# ---------------------------------------------------------------- no rollout
mdartifact "${P}-roll" e2e/roll
d="$(warm "${P}-roll" "${POOL[0]}")"
md "${P}-roll" "${P}-roll" 2
members=()
for _ in $(seq 1 60); do
  read -r -a members <<<"$(md_pods "${P}-roll" | tr '\n' ' ')"
  [ "${#members[@]}" -eq 2 ] && break
  sleep 2
done
# snapshot: every member's "name uid hash", sorted.
snapshot() {
  kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-roll" -o jsonpath=\
'{range .items[*]}{.metadata.name} {.metadata.uid} {.metadata.annotations.modeldeployment\.gpustack\.ai/pod-spec-hash}{"\n"}{end}' 2>/dev/null | sort
}
if [ -z "$d" ] || [ "${#members[@]}" -ne 2 ]; then
  record FAIL "a two-member replica of a warmed digest is created" "digest=${d:-<none>} members=${#members[@]}"
else
  note_terms "${members[0]}"; t0="$T"
  note_terms "${members[1]}"; t1="$T"
  used=" $(bound_node "${members[0]}" 120) $(bound_node "${members[1]}" 120) ${POOL[0]} "
  if [ -n "$t0" ] && [ "$t0" = "$t1" ]; then
    record PASS "both members of a replica carry the same preferred term" "$t0"
  else
    record FAIL "both members of a replica carry the same preferred term" "${members[0]}=${t0:-<none>} ${members[1]}=${t1:-<none>}"
  fi
  other=""
  for n in "${POOL[@]}"; do case "$used" in *" $n "*) ;; *) other="$n"; break ;; esac; done
  before="$(snapshot)"
  if [ -z "$other" ]; then
    record SKIP "no rollout: no eligible worker is left to warm; NOTHING WAS VERIFIED on rollout" "${POOL[*]}"
  elif [ -z "$(warm "${P}-roll" "$other")" ]; then
    record FAIL "the digest becomes Ready on one more node" "$other"
  else
    sleep 30; mid="$(snapshot)"
    sleep 30; after="$(snapshot)"
    if [ -n "$before" ] && [ "$before" = "$mid" ] && [ "$before" = "$after" ]; then
      record PASS "after the digest became Ready on ${other}, the replica's Pod UIDs and spec hashes stayed the same over two intervals" "${MDNS}/${P}-roll"
    else
      record FAIL "a change in which nodes hold the digest rolls nothing" "before=[${before}] after=[${after}]"
    fi
  fi
fi
md_gone "${P}-roll" >/dev/null

# ---------------------------------------------------------------- an Instance
mdartifact "${P}-inst" e2e/inst
hot="${POOL[1]}"
d="$(warm "${P}-inst" "$hot")"
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: ${P}-inst, namespace: ${MDNS}}
spec:
  type: "${IT}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: {ephemeral: {capacity: 1Gi}}
  additionalVolumes: [{mountPath: /models/hub, model: {artifactRef: {name: ${P}-inst}}}]
YAML
for _ in $(seq 1 60); do kubectl -n "$MDNS" get pod "${P}-inst" >/dev/null 2>&1 && break; sleep 2; done
note_terms "$(kubectl -n "$MDNS" get pod "${P}-inst" -o name 2>/dev/null | cut -d/ -f2)"
if [ -n "$d" ] && [ "${T%|annotated}" = "100:kubernetes.io/hostname=${hot}" ]; then
  record PASS "an Instance naming a warmed artifact carries the preferred term naming its node" "${MDNS}/${P}-inst ${T}"
else
  record FAIL "an Instance naming a warmed artifact carries the preferred term naming its node" "digest=${d:-<none>} terms=${T:-<none>} want ${hot}"
fi
kubectl -n "$MDNS" delete instances.worker.gpustack.ai "${P}-inst" --wait=false >/dev/null 2>&1

# ---------------------------------------------------------------- no preferred topology level
if [ "$WITH_TERM" -gt 0 ] && [ "$ANNOTATED" -eq 0 ]; then
  record PASS "no Pod carrying a preferred term asks for a preferred topology level" "${WITH_TERM} Pods"
else
  record FAIL "no Pod carrying a preferred term asks for a preferred topology level" "with-term=${WITH_TERM} annotated=${ANNOTATED}"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-107] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-107] PASS"
