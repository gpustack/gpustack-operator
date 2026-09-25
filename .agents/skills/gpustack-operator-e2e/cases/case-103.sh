#!/usr/bin/env bash
#
# CASE 103 — The node cache's lifecycle: a plugin roll leaves mounted Pods reading, collection removes
#            the oldest unreferenced digest and never a referenced one, a filtered artifact downloads
#            only its files, and an Instance and a ModelDeployment mount a hub artifact
#            (MUTATING, self-recovering; AUTO-SKIPS the collection rows without docker access to the
#            kind nodes)
#
#   case-103.sh <NS>
#
# Goal:        Prove the parts of node delivery that only a running plugin, a real kubelet and time
#              can show: a rolling restart of the plugin DaemonSet does not disturb a Pod reading its
#              mounted files, and a mount asked for during the roll succeeds after it; the collector
#              removes by age of last use down to the low watermark, keeps a digest a Pod references
#              even when told to go to 5%; allow patterns select the files downloaded, and a filtered
#              artifact cannot be Engine-delivered; an Instance and a ModelDeployment on Node delivery
#              render the plugin's volume and run.
# Environment: A kind cluster installed from this chart with modelManager.enabled, two or more
#              workers, a materialized scheduling chain (run case-1 first) with an InstanceType,
#              docker on this machine reaching the kind node containers, and the stock python image
#              pullable. NO GPU. The collection rows mount a tmpfs over the second worker's cache
#              root; without docker they SKIP. The whole case takes about fifteen minutes: the
#              collector keeps a digest ten minutes after its last use.
# Inputs:      MOCKED: a test hub over HTTP with generated repositories, and a tmpfs as the second
#              worker's cache filesystem. Real: the plugin, kubelet, the collector and the worker.
#              Override with E2E_MD_INSTANCE_TYPE, E2E_MD_IMAGE.
# Expected:    - a Pod on the first worker hashes its files every second through the DaemonSet's
#                roll with no mismatch and no read error, and a Pod created during the roll runs
#                on the manifest's bytes;
#              - a filtered artifact's Pod lists only the allowed files and the hub served no other;
#                under Engine delivery its deployment reports FilterNeedsNodeDelivery and has no Pod,
#                and it starts, unedited, once the Setting is back to Node;
#              - without the CSIDriver a deployment reports NodeDeliveryUnavailable and has no Pod,
#                and it starts, unedited, once the CSIDriver is back;
#              - an Instance naming a hub artifact runs with the plugin's read-only volume;
#              - a ModelDeployment on Node delivery renders the plugin's volume with the artifact's
#                digest, reports WeightsReady Materializing while the node downloads, then Mounted,
#                and status.model.delivery Node;
#              - with a 100 MiB cache holding three 20 MiB digests, two unreferenced past their
#                grace and one referenced: watermarks 50/45 remove only the oldest; 10/5 remove the
#                second and keep the referenced one.
# Cleanup:     Trap deletes the Pods, deployments, Instances, artifacts and the hub, puts the
#              Settings back, restores the CSIDriver if it is missing, unmounts the tmpfs and
#              restarts that node's plugin.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-103.sh <NS>}"
MDNS="${NS}-c103"
P=c103
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
DS=gpustack-operator-model-manager
CACHE_ROOT=/var/lib/gpustack/models
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
SCRATCH="$(mktemp -d)"
TMPFS_NODE=""

KEYS="model-artifact-huggingface-endpoint model-artifact-delivery-mode model-store-high-watermark model-store-low-watermark"
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
  echo "[case-103] cleanup"
  kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai --all --ignore-not-found --wait=false >/dev/null 2>&1
  for ns in "$NS" "$MDNS"; do
    kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
  done
  for _ in $(seq 1 24); do [ -z "$(kubectl -n "$MDNS" get pods -o name 2>/dev/null)" ] && break; sleep 5; done
  for ns in "$NS" "$MDNS"; do
    kubectl -n "$ns" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=103 --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete deploy,svc "${P}-hub" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${P}-hub-script" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$MDNS" --ignore-not-found --wait=false >/dev/null 2>&1
  for k in model-store-high-watermark model-store-low-watermark model-artifact-delivery-mode model-artifact-huggingface-endpoint; do
    restore "$k"
  done
  [ -s "$SCRATCH/csidriver.json" ] && ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1 \
    && kubectl apply -f "$SCRATCH/csidriver.json" >/dev/null 2>&1
  if [ -n "$TMPFS_NODE" ]; then
    docker exec "$TMPFS_NODE" umount -l "$CACHE_ROOT" >/dev/null 2>&1
    kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$TMPFS_NODE")" --wait=false >/dev/null 2>&1
  fi
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

get_uid() { kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" -o jsonpath='{.metadata.uid}'; }
# served REPO: the file names the hub served REPO's downloads for, one a line.
served() {
  mh_log "$NS" "${P}-hub" | python3 -c "
import json, sys
print('\n'.join(sorted({json.loads(l)['path'].rsplit('/', 1)[-1] for l in sys.stdin if l.strip()
                        and json.loads(l)['method'] == 'GET' and json.loads(l).get('status') in (200, 206)
                        and json.loads(l)['path'].startswith('/$1/')})))"
}
# files_match NS POD REPO: whether the Pod lists exactly the hub's files of REPO with their hashes.
files_match() {
  local listed want
  listed="$(kubectl -n "$1" logs "$2" 2>/dev/null | sort)"
  want="$(mh_call "$NS" "${P}-hub" GET /_manifest | python3 -c "
import json, sys
for p, f in sorted(json.load(sys.stdin)['$3']['files'].items()): print(f['sha256'], p)" | sort)"
  [ -n "$want" ] && [ "$listed" = "$want" ]
}
# mount_node ARTIFACT_NS NAME NODE POD: resolves the artifact and starts a consumer of it on NODE.
mount_node() {
  local d
  d="$(wait_resolved "$1" "$2" 120)"
  consumer "$1" "$4" "$3" "$2" "$(get_uid "$1" "$2")" "$d"
  printf '%s' "$d"
}
weights() {
  kubectl -n "$MDNS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].status}|{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null
}

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 2 ] || { echo "[case-103] needs two workers; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}" W2="${WORKERS[1]}"
kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1 \
  || { echo "[case-103] the model-manager plugin is not installed; NOTHING WAS VERIFIED"; exit 2; }
[ -n "$IT" ] || IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
[ -n "$IT" ] || { echo "[case-103] no InstanceType; run case-1 first" >&2; exit 2; }
for ns in "$NS" "$MDNS"; do kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; done

REPOS="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({
  'e2e/gc-a': {'files': {'config.json': {'size': 100}, 'w.bin': {'size': 20 * mib, 'lfs': True}}},
  'e2e/gc-b': {'files': {'config.json': {'size': 110}, 'w.bin': {'size': 20 * mib, 'lfs': True}}},
  'e2e/gc-c': {'files': {'config.json': {'size': 120}, 'w.bin': {'size': 20 * mib, 'lfs': True}}},
  'e2e/read': {'files': {'config.json': {'size': 200}, 'w.bin': {'size': mib, 'lfs': True}}},
  'e2e/roll': {'files': {'config.json': {'size': 210}, 'w.bin': {'size': 2 * mib, 'lfs': True}}},
  'e2e/filtered': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 310},
                             'original/w.pth': {'size': mib, 'lfs': True}, 'w.bin': {'size': mib, 'lfs': True}}},
  'e2e/instance': {'files': {'config.json': {'size': 400}}},
  'e2e/md': {'files': {'config.json': {'size': 500}, 'w.bin': {'size': 4 * mib, 'lfs': True}}},
  'e2e/drv': {'files': {'config.json': {'size': 600}}},
}))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
setting_set model-artifact-delivery-mode Node
settings_settle
echo "[case-103] hub at ${HUB_URL}; workers ${W1}, ${W2}; InstanceType ${IT}"

# ---------------------------------------------------------------- collection, part one: fill
# Three 20 MiB digests in a 100 MiB tmpfs on the second worker: a and b each mounted once and
# released, a first, c held. The grace starts at each release, so the rows that follow run while it
# passes, all of them on the first worker.
GC=0 D_a="" D_b="" D_c=""
if docker exec "$W2" mount -t tmpfs -o size=100m tmpfs "$CACHE_ROOT" >/dev/null 2>&1; then
  TMPFS_NODE="$W2"
  kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$W2")" --wait=true >/dev/null
  total=""
  for _ in $(seq 1 60); do
    total="$(kubectl get nodemodelstores.worker.gpustack.ai "$W2" -o jsonpath='{.status.capacity.totalBytes}' 2>/dev/null)"
    [ -n "$total" ] && [ "$total" -le $((128 << 20)) ] && break
    sleep 2
  done
  if [ -n "$total" ] && [ "$total" -le $((128 << 20)) ]; then
    GC=1
    for x in a b c; do artifact "$NS" "${P}-gc-${x}" "e2e/gc-${x}" "" 103; done
    # fill X: mounts gc-X on the second worker and keeps its digest in D_X.
    fill() {
      local d
      d="$(mount_node "$NS" "${P}-gc-$1" "$W2" "${P}-gc-$1")"
      printf -v "D_$1" '%s' "$d"
      pod_ready "$NS" "${P}-gc-$1" 240 || record FAIL "the collection fixture $1 mounts" "$(pod_mount_events "$NS" "${P}-gc-$1" | tail -1)"
    }
    fill a
    kubectl -n "$NS" delete pod "${P}-gc-a" --wait=true >/dev/null
    sleep 5
    fill b
    kubectl -n "$NS" delete pod "${P}-gc-b" --wait=true >/dev/null
    fill c
    RELEASED="$(date +%s)"
  else
    record FAIL "the plugin reports the tmpfs cache root" "totalBytes=${total:-<none>}"
  fi
else
  record SKIP "collection: no docker access to the kind node ${W2}; NOTHING WAS VERIFIED on collection" "$W2"
fi

# ---------------------------------------------------------------- a rolling restart of the plugin
artifact "$NS" "${P}-read" e2e/read "" 103
artifact "$NS" "${P}-roll" e2e/roll "" 103
RDIG="$(wait_resolved "$NS" "${P}-read" 120)"
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/consumer: "true"}}
spec:
  nodeName: ${W1}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: c
      image: ${MH_PYTHON}
      command: ["python3", "-c", "import hashlib,os,time\ndef h():\n  out={}\n  for r,_,fs in os.walk('/model'):\n    for f in fs:\n      p=os.path.join(r,f); out[p]=hashlib.sha256(open(p,'rb').read()).hexdigest()\n  return out\nfirst=h()\nwhile True:\n  try: print(int(time.time()), 'ok' if h()==first and first else 'mismatch', flush=True)\n  except Exception as e: print(int(time.time()), 'error', e, flush=True)\n  time.sleep(1)"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: model, mountPath: /model, readOnly: true}]
  volumes:
    - name: model
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes: {artifact: ${P}-read, artifactUID: "$(get_uid "$NS" "${P}-read")", manifestDigest: "${RDIG}"}
YAML
if pod_ready "$NS" "${P}-reader" 240; then
  sleep 5
  ROLL_START="$(date +%s)"
  kubectl -n "$SYSTEM_NS" rollout restart "ds/${DS}" >/dev/null
  sleep 3
  mount_node "$NS" "${P}-roll" "$W1" "${P}-roll" >/dev/null
  kubectl -n "$SYSTEM_NS" rollout status "ds/${DS}" --timeout=600s >/dev/null
  ROLL_END="$(date +%s)"
  sleep 5
  kubectl -n "$NS" logs "${P}-reader" >"$SCRATCH/reader"
  bad="$(awk '$2 != "ok"' "$SCRATCH/reader" | head -1)"
  during="$(awk -v s="$ROLL_START" -v e="$ROLL_END" '$1 >= s && $1 <= e && $2 == "ok"' "$SCRATCH/reader" | wc -l | tr -d ' ')"
  span=$((ROLL_END - ROLL_START))
  if [ -z "$bad" ] && [ "$during" -ge $((span / 2)) ] && [ "$span" -gt 0 ]; then
    record PASS "a mounted Pod read its files ${during} times with no mismatch or error through the ${span}s roll" "${NS}/${P}-reader"
  else
    record FAIL "a mounted Pod reads its files through the roll (${during} ok in ${span}s)" "${bad:-too few readings}"
  fi
  if pod_ready "$NS" "${P}-roll" 300 && files_match "$NS" "${P}-roll" e2e/roll; then
    record PASS "a mount asked for during the roll runs on the manifest's bytes after it" "${NS}/${P}-roll"
  else
    record FAIL "a mount asked for during the roll succeeds after it" "$(pod_mount_events "$NS" "${P}-roll" | tail -1)"
  fi
else
  record FAIL "the reader Pod mounts" "$(pod_mount_events "$NS" "${P}-reader" | tail -1)"
fi
kubectl -n "$NS" delete pod "${P}-reader" "${P}-roll" --wait=false >/dev/null 2>&1

# ---------------------------------------------------------------- a filtered artifact
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-filtered, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "103"}}
spec:
  source: {huggingFace: {repository: e2e/filtered, revision: main}}
  allowPatterns: ["*.json", "original/"]
  ignorePatterns: ["tokenizer.json"]
YAML
mount_node "$NS" "${P}-filtered" "$W1" "${P}-filtered" >/dev/null
if pod_ready "$NS" "${P}-filtered" 240; then
  sleep 3
  listed="$(kubectl -n "$NS" logs "${P}-filtered" | awk '{print $2}' | sort | tr '\n' ' ')"
  got="$(served e2e/filtered | sort | tr '\n' ' ')"
  if [ "$listed" = "config.json original/w.pth " ] && [ "$got" = "config.json w.pth " ]; then
    record PASS "a filtered artifact holds and downloads only its files: ${listed}" "${NS}/${P}-filtered"
  else
    record FAIL "a filtered artifact holds and downloads only its files" "listed=${listed} served=${got}"
  fi
else
  record FAIL "a filtered artifact mounts" "$(pod_mount_events "$NS" "${P}-filtered" | tail -1)"
fi
kubectl -n "$NS" delete pod "${P}-filtered" --wait=false >/dev/null 2>&1

# The deployment rows need a namespace without the restricted profile, since the operator renders
# its own Pods, and the pool's entrance LocalQueue, which the operator creates in every namespace.
for _ in $(seq 1 30); do [ -n "$(kubectl -n "$MDNS" get localqueues -o name 2>/dev/null)" ] && break; sleep 2; done
md() { # name artifact
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${MDNS}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c103, artifactRef: {name: $2}}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, image: "${IMAGE}", command: ["/pause"]}
YAML
}
mdartifact() { # name repository [patterns yaml]
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: $1, namespace: ${MDNS}, labels: {e2e.gpustack.ai/case: "103"}}
spec:
  source: {huggingFace: {repository: $2, revision: main}}${3:-}
YAML
}

setting_set model-artifact-delivery-mode Engine
settings_settle
mdartifact "${P}-mdf" e2e/filtered '
  allowPatterns: ["*.json"]'
wait_resolved "$MDNS" "${P}-mdf" 120 >/dev/null
md "${P}-engine" "${P}-mdf"
reading=""
for _ in $(seq 1 30); do reading="$(weights "${P}-engine")"; [ "$reading" = "False|FilterNeedsNodeDelivery" ] && break; sleep 2; done
pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-engine" -o name 2>/dev/null)"
if [ "$reading" = "False|FilterNeedsNodeDelivery" ] && [ -z "$pods" ]; then
  record PASS "under Engine delivery a filtered artifact's deployment reports FilterNeedsNodeDelivery and has no Pod" "${MDNS}/${P}-engine"
else
  record FAIL "under Engine delivery a filtered artifact's deployment is held" "${reading:-<none>} pods=${pods}"
fi
# The deployment is left as it is while the Setting changes back: nothing edits it, so only the
# worker's watch on the Settings can wake it.
setting_set model-artifact-delivery-mode Node
woke="" pods=""
for _ in $(seq 1 60); do
  reading="$(weights "${P}-engine")"
  pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-engine" -o name 2>/dev/null)"
  [ -n "$pods" ] && [ "${reading#*|}" != FilterNeedsNodeDelivery ] && { woke=1; break; }
  sleep 3
done
if [ -n "$woke" ]; then
  record PASS "switched back to Node, the held deployment starts without an edit" "${MDNS}/${P}-engine ${reading}"
else
  record FAIL "switched back to Node, the held deployment starts without an edit" "${reading:-<none>} pods=${pods:-<none>}"
fi
kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai "${P}-engine" --wait=false >/dev/null
settings_settle

# ---------------------------------------------------------------- an Instance
mdartifact "${P}-inst" e2e/instance
wait_resolved "$MDNS" "${P}-inst" 120 >/dev/null
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
phase=""
for _ in $(seq 1 100); do
  phase="$(kubectl -n "$MDNS" get pod "${P}-inst" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase" = Running ] && break
  sleep 3
done
vol="$(kubectl -n "$MDNS" get pod "${P}-inst" -o jsonpath="{.spec.volumes[?(@.csi.driver=='model.csi.gpustack.ai')].csi.volumeAttributes.artifact}|{.spec.containers[0].volumeMounts[?(@.mountPath=='/models/hub')].readOnly}" 2>/dev/null)"
if [ "$phase" = Running ] && [ "$vol" = "${P}-inst|true" ]; then
  record PASS "an Instance naming a hub artifact runs with the plugin's read-only volume" "${MDNS}/${P}-inst"
else
  record FAIL "an Instance naming a hub artifact runs with the plugin's volume" "phase=${phase:-<none>} volume=${vol}"
fi
kubectl -n "$MDNS" delete instances.worker.gpustack.ai "${P}-inst" --wait=false >/dev/null

# ---------------------------------------------------------------- a ModelDeployment on Node delivery
mh_call "$NS" "${P}-hub" POST "/_control?throttle=65536" >/dev/null
mdartifact "${P}-md" e2e/md
MDIG="$(wait_resolved "$MDNS" "${P}-md" 120)"
md "${P}-node" "${P}-md"
seen="" reading=""
for _ in $(seq 1 120); do
  reading="$(weights "${P}-node")"
  # The reasons seen, each once, matched as whole words: WeightsNotMounted contains Mounted.
  [ -n "$reading" ] && case "${seen} " in *" ${reading#*|} "*) ;; *) seen="${seen} ${reading#*|}" ;; esac
  [ "$reading" = "True|Mounted" ] && break
  sleep 3
done
mh_call "$NS" "${P}-hub" POST "/_control?throttle=0" >/dev/null
pod="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
digest="$(kubectl -n "$MDNS" get pod "$pod" -o jsonpath="{.spec.volumes[?(@.csi.driver=='model.csi.gpustack.ai')].csi.volumeAttributes.manifestDigest}" 2>/dev/null)"
delivery="$(kubectl -n "$MDNS" get modeldeployments.worker.gpustack.ai "${P}-node" -o jsonpath='{.status.model.delivery}')"
if [ -n "$MDIG" ] && [ "$digest" = "$MDIG" ]; then
  record PASS "the deployment's Pod carries the plugin's volume with the artifact's digest" "${MDNS}/${pod}"
else
  record FAIL "the deployment's Pod carries the plugin's volume with the artifact's digest" "pod=${pod:-<none>} digest=${digest:-<none>} want=${MDIG}"
fi
case "${seen} " in
  *" Materializing "*)
    if [ "$reading" = "True|Mounted" ] && [ "$delivery" = Node ]; then
      record PASS "WeightsReady went through Materializing to Mounted, delivery Node" "reasons:${seen}"
    else
      record FAIL "WeightsReady reaches Mounted with delivery Node" "last=${reading} delivery=${delivery} reasons:${seen}"
    fi ;;
  *) record FAIL "WeightsReady reports Materializing while the node downloads" "reasons:${seen}" ;;
esac

# ---------------------------------------------------------------- a deployment waiting for the plugin
# The CSIDriver goes and comes back while a deployment waits on it; nothing edits the deployment, so
# only the worker's watch on the CSIDriver can wake it.
kubectl get csidriver model.csi.gpustack.ai -o json | python3 -c "
import json, sys
o = json.load(sys.stdin)
m = o['metadata']
print(json.dumps({'apiVersion': o['apiVersion'], 'kind': o['kind'], 'spec': o['spec'],
                  'metadata': {k: m[k] for k in ('name', 'labels', 'annotations') if k in m}}))" >"$SCRATCH/csidriver.json"
mdartifact "${P}-drv" e2e/drv
wait_resolved "$MDNS" "${P}-drv" 120 >/dev/null
kubectl delete csidriver model.csi.gpustack.ai >/dev/null
md "${P}-drv" "${P}-drv"
reading=""
for _ in $(seq 1 30); do reading="$(weights "${P}-drv")"; [ "$reading" = "False|NodeDeliveryUnavailable" ] && break; sleep 2; done
pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-drv" -o name 2>/dev/null)"
if [ "$reading" = "False|NodeDeliveryUnavailable" ] && [ -z "$pods" ]; then
  record PASS "without the CSIDriver a deployment reports NodeDeliveryUnavailable and has no Pod" "${MDNS}/${P}-drv"
else
  record FAIL "without the CSIDriver a deployment is held" "${reading:-<none>} pods=${pods}"
fi
kubectl apply -f "$SCRATCH/csidriver.json" >/dev/null
woke=""
for _ in $(seq 1 40); do
  reading="$(weights "${P}-drv")"
  pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-drv" -o name 2>/dev/null)"
  [ -n "$pods" ] && [ "${reading#*|}" != NodeDeliveryUnavailable ] && { woke=1; break; }
  sleep 3
done
if [ -n "$woke" ]; then
  record PASS "with the CSIDriver back, the held deployment starts without an edit" "${MDNS}/${P}-drv ${reading}"
else
  record FAIL "with the CSIDriver back, the held deployment starts without an edit" "${reading:-<none>} pods=${pods:-<none>}"
fi
kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai "${P}-drv" --wait=false >/dev/null

# ---------------------------------------------------------------- collection, part two: collect
if [ "$GC" = 1 ]; then
  left=$(( RELEASED + 660 - $(date +%s) ))
  [ "$left" -gt 0 ] && { echo "[case-103] waiting ${left}s for the collection grace"; sleep "$left"; }
  # The CSIDriver row recreated every NodeModelStore; the node lists its content again first.
  for _ in $(seq 1 30); do [ -n "$(store_state "$W2" "$D_c")" ] && break; sleep 2; done
  # gone DIGEST BOUND: waits for DIGEST to leave the second worker's status.
  gone() {
    for _ in $(seq 1 $(( $2 / 3 ))); do [ -z "$(store_state "$W2" "$1")" ] && return 0; sleep 3; done
    return 1
  }
  held() { [ -n "$(store_state "$W2" "$1")" ]; }
  # Low first, so every intermediate pair keeps low below high.
  setting_set model-store-low-watermark 45
  setting_set model-store-high-watermark 50
  if gone "$D_a" 120 && held "$D_b" && held "$D_c"; then
    record PASS "at 50/45 the collector removed only the oldest unreferenced digest" "$W2"
  else
    record FAIL "at 50/45 the collector removes only the oldest" "a=$(store_state "$W2" "$D_a") b=$(store_state "$W2" "$D_b") c=$(store_state "$W2" "$D_c")"
  fi
  setting_set model-store-low-watermark 5
  setting_set model-store-high-watermark 10
  if gone "$D_b" 120 && held "$D_c" && [ -n "$(kubectl -n "$NS" logs "${P}-gc-c" | head -1)" ]; then
    record PASS "at 10/5 the collector removed the other unreferenced digest and kept the referenced one" "$W2"
  else
    record FAIL "at 10/5 the referenced digest stays" "b=$(store_state "$W2" "$D_b") c=$(store_state "$W2" "$D_c")"
  fi
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-103] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-103] PASS"
