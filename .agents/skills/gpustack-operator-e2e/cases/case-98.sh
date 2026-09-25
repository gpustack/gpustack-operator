#!/usr/bin/env bash
#
# CASE 98 — ModelArtifact consumers: a ModelDeployment and an Instance mount a claim where its PV
#           lives, wait when the claim cannot be placed, and render an engine download pinned to
#           the resolved commit       (MUTATING, self-recovering; AUTO-SKIPS without two workers)
#
#   case-98.sh <NS>
#
# Goal:        Prove the consumer side of the ModelArtifact contract on a real API server,
#              scheduler, kubelet and Kueue: the bound PV's required node affinity reaches every Pod
#              and TAS places it on the PV's node; a claim that cannot be placed creates no Pod and
#              says why; a replica created before its claim binds is not rolled after it; an Engine
#              deployment's Pod carries the pinned argv, the Secret reference, the size-limited
#              cache and the raised ephemeral-storage limit; admission refuses what would override
#              the artifact.
# Environment: A cluster with at least two schedulable workers, a materialized scheduling chain
#              (run case-1 first), an InstanceType, in <NS> the pool's entrance LocalQueue, a
#              default StorageClass whose provisioner creates volumes on the first Pod's node
#              (kind's local-path), and a worker that reaches the Hub. NO GPU and no engine image:
#              managed roles run the pause image, which cannot start an engine, and every asserted
#              value is on an object or a Pod condition.
# Inputs:      All real. A hostPath PV pinned to the second worker, created by this case; public
#              Qwen/Qwen2.5-0.5B-Instruct for the Engine rows. Override with E2E_MD_INSTANCE_TYPE,
#              E2E_MD_IMAGE.
# Expected:    - a claim deployment: its Pod carries the PV's node affinity, runs on the PV's node,
#                loads /var/lib/gpustack/model read-only under --served-model-name, and reports
#                WeightsReady=True Mounted and status.model.delivery Pvc;
#              - an Instance on the same claim: the same affinity and node, a read-only mount at the
#                artifact's directory;
#              - two replicas on a ReadWriteOnce claim: AccessModeConflict, no Pod;
#              - a static no-provisioner claim and an immediate-binding claim left pending:
#                ClaimNotBound, no Pod;
#              - a WaitForFirstConsumer claim: the Pod is created, the claim binds, and the Pod is
#                not rolled;
#              - a missing artifact: ArtifactNotFound and no Pod, then an Engine Pod once it exists,
#                with `vllm serve <repo> --revision <commit>`, HF_TOKEN from a secretKeyRef, a cache
#                whose sizeLimit is the manifest size plus its headroom, a raised ephemeral-storage
#                limit over an unchanged request, and no token value in the Pod spec;
#              - admission refuses another --served-model-name, --revision and a volume on the
#                weights' path. An Instance naming a hub artifact is admitted since the node plugin
#                delivers it; case-103 mounts one.
# Cleanup:     Trap deletes every object carrying the case label, then the PV and StorageClasses, and
#              puts back the model-artifact-delivery-mode Setting it pinned to Engine.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-98.sh <NS>}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
P=c98
LABEL="e2e.gpustack.ai/case=98"
FAKE_TOKEN="e2e-c98-not-a-real-token"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"

# This case proves Engine delivery, and the chart seeds Node when it deploys the model-manager plugin,
# so the case pins Engine for its run and puts back what it found. The wait is the worker's
# thirty-second Settings read cache.
ORIG_DELIVERY="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath='{.data.model-artifact-delivery-mode}' 2>/dev/null)"
restore_delivery() {
  local v=null
  [ -n "$ORIG_DELIVERY" ] && v="\"${ORIG_DELIVERY}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"model-artifact-delivery-mode\":${v}}}" >/dev/null 2>&1
}
pin_engine_delivery() {
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"model-artifact-delivery-mode\":\"$(printf Engine | base64)\"}}" >/dev/null
  sleep 35
}

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

cleanup() {
  echo
  echo "[case-98] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 24); do
    [ -z "$(kubectl -n "$NS" get pods -l "$LABEL" -o name 2>/dev/null)$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "${P}-")" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai,pvc,secret -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete pv "${P}-pv" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete storageclass "${P}-static" "${P}-immediate" --ignore-not-found >/dev/null 2>&1
  restore_delivery
}
trap cleanup EXIT

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
[ -n "$IT" ] || { echo "[case-98] no InstanceType; run case-1 first" >&2; exit 2; }
WORKERS="$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.labels.kubernetes\.io/hostname}{"\n"}{end}' | sort)"
PV_NODE="$(printf '%s\n' "$WORKERS" | sed -n 2p)"
if [ -z "$PV_NODE" ]; then
  record SKIP "the claim rows" "fewer than two workers; NOTHING WAS VERIFIED"
  print_rows
  exit 0
fi

md() { # name artifact replicas [extra role yaml]
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c98, artifactRef: {name: $2}}
  roles:
    - name: server
      instanceType: "${IT}"
      replicas: $3
      image: "${IMAGE}"${4:-}
YAML
}

claim_artifact() { # name claim
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec: {source: {persistentVolumeClaim: {claimName: $2, path: qwen}}}
YAML
}

pvc() { # name class access [volumeName]; an empty class takes the cluster's default
  local vn="" sc=""
  [ -n "${4:-}" ] && vn="
  volumeName: $4"
  [ -n "$2" ] && sc="
  storageClassName: $2"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec:${sc}
  accessModes: [$3]
  resources: {requests: {storage: 1Gi}}${vn}
YAML
}

weights() { # md -> status|reason
  kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].status}|{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null
}

wait_weights() { # md want-reason bound
  local reading=""
  for _ in $(seq 1 $(( $3 / 3 ))); do
    reading="$(weights "$1")"
    [ "${reading#*|}" = "$2" ] && break
    sleep 3
  done
  printf '%s' "$reading"
}

pods_of() { kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$1" -o jsonpath="$2" 2>/dev/null; }

refused() { # check manifest-on-stdin
  local out
  if out="$(kubectl apply --dry-run=server -f - 2>&1)"; then
    record FAIL "$1" "admitted: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
  else
    record PASS "$1" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
  fi
}

echo "== fixtures: a PV pinned to ${PV_NODE}, a static and an immediate class =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ${P}-static}
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: WaitForFirstConsumer
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ${P}-immediate}
provisioner: e2e.gpustack.ai/provisions-nothing
volumeBindingMode: Immediate
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: ${P}-pv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  storageClassName: ${P}-static
  hostPath: {path: /tmp/${P}-models, type: DirectoryOrCreate}
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions: [{key: kubernetes.io/hostname, operator: In, values: ["${PV_NODE}"]}]
YAML
pvc "${P}-bound" "${P}-static" ReadWriteOnce "${P}-pv"
claim_artifact "${P}-bound" "${P}-bound"

echo "== 1. a claim deployment runs on the PV's node =="
md "${P}-claim" "${P}-bound" 1
reading="$(wait_weights "${P}-claim" Mounted 120)"
node="$(pods_of "${P}-claim" '{.items[0].spec.nodeName}')"
aff="$(pods_of "${P}-claim" '{.items[0].spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')"
if [ "$node" = "$PV_NODE" ] && [ "$aff" = "$PV_NODE" ]; then
  record PASS "the replica carries the PV's node affinity and runs there" "node=${node}"
else
  record FAIL "the replica carries the PV's node affinity and runs there" "node=${node:-<unscheduled>} affinity=${aff:-<none>} pv=${PV_NODE}"
fi
cmd="$(pods_of "${P}-claim" '{.items[0].spec.containers[0].command}')"
ro="$(pods_of "${P}-claim" "{.items[0].spec.volumes[?(@.name=='gpustack-model')].persistentVolumeClaim.readOnly}")"
sub="$(pods_of "${P}-claim" "{.items[0].spec.containers[0].volumeMounts[?(@.name=='gpustack-model')].subPath}")"
case "$cmd" in
  *'"vllm","serve","/var/lib/gpustack/model"'*'"--served-model-name","e2e/c98"'*)
    [ "$ro" = true ] && [ "$sub" = qwen ] \
      && record PASS "the engine loads the read-only claim under the served name" "command=${cmd}" \
      || record FAIL "the engine loads the read-only claim under the served name" "readOnly=${ro} subPath=${sub}" ;;
  *) record FAIL "the engine loads the read-only claim under the served name" "command=${cmd:-<none>}" ;;
esac
delivery="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-claim" -o jsonpath='{.status.model.delivery}')"
if [ "$reading" = "True|Mounted" ] && [ "$delivery" = Pvc ]; then
  record PASS "WeightsReady reports the claim mounted" "${reading} delivery=${delivery}"
else
  record FAIL "WeightsReady reports the claim mounted" "${reading:-<none>} delivery=${delivery:-<none>}"
fi

echo "== 2. an Instance on the same claim =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: ${P}-inst, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec:
  type: "${IT}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: {ephemeral: {capacity: 1Gi}}
  additionalVolumes:
    - {mountPath: /models/qwen, model: {artifactRef: {name: ${P}-bound}}}
YAML
inode=""
for _ in $(seq 1 40); do
  inode="$(kubectl -n "$NS" get pod "${P}-inst" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
  [ -n "$inode" ] && break
  sleep 3
done
imount="$(kubectl -n "$NS" get pod "${P}-inst" -o jsonpath="{.spec.containers[0].volumeMounts[?(@.mountPath=='/models/qwen')].readOnly}/{.spec.containers[0].volumeMounts[?(@.mountPath=='/models/qwen')].subPath}" 2>/dev/null)"
if [ "$inode" = "$PV_NODE" ] && [ "$imount" = "true/qwen" ]; then
  record PASS "the Instance mounts the claim read-only on the PV's node" "node=${inode} mount=${imount}"
else
  record FAIL "the Instance mounts the claim read-only on the PV's node" "node=${inode:-<no pod>} mount=${imount:-<none>}"
fi

echo "== 3. claims that cannot be placed create nothing =="
md "${P}-rwo" "${P}-bound" 2
pvc "${P}-static-pending" "${P}-static" ReadWriteOnce
claim_artifact "${P}-static-pending" "${P}-static-pending"
md "${P}-static" "${P}-static-pending" 1
pvc "${P}-imm" "${P}-immediate" ReadWriteMany
claim_artifact "${P}-imm" "${P}-imm"
md "${P}-imm" "${P}-imm" 1
for pair in "${P}-rwo:AccessModeConflict" "${P}-static:ClaimNotBound" "${P}-imm:ClaimNotBound"; do
  name="${pair%%:*}"
  reading="$(wait_weights "$name" "${pair#*:}" 60)"
  count="$(pods_of "$name" '{range .items[*]}x{end}')"
  if [ "$reading" = "False|${pair#*:}" ] && [ -z "$count" ]; then
    record PASS "an unplaceable claim creates no Pod" "${name}: ${reading}"
  else
    record FAIL "an unplaceable claim creates no Pod" "${name}: ${reading:-<none>} pods=${#count}"
  fi
done

echo "== 4. a claim bound by its first Pod does not roll that Pod =="
pvc "${P}-dyn" "" ReadWriteOnce
claim_artifact "${P}-dyn" "${P}-dyn"
md "${P}-dyn" "${P}-dyn" 1
phase=""
for _ in $(seq 1 60); do
  phase="$(kubectl -n "$NS" get pvc "${P}-dyn" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase" = Bound ] && break
  sleep 3
done
before="$(pods_of "${P}-dyn" '{.items[*].metadata.uid}')"
sleep 30
after="$(pods_of "${P}-dyn" '{.items[*].metadata.uid}')"
if [ "$phase" = Bound ] && [ -n "$before" ] && [ "$before" = "$after" ]; then
  record PASS "the replica that bound the claim is kept" "uid=${after}"
else
  record FAIL "the replica that bound the claim is kept" "claim=${phase:-<none>} before=${before:-<none>} after=${after:-<none>}"
fi

echo "== 5. a missing artifact, then an Engine download =="
pin_engine_delivery
md "${P}-engine" "${P}-later" 1
reading="$(wait_weights "${P}-engine" ArtifactNotFound 60)"
count="$(pods_of "${P}-engine" '{range .items[*]}x{end}')"
if [ "$reading" = "False|ArtifactNotFound" ] && [ -z "$count" ]; then
  record PASS "a missing artifact creates no Pod" "$reading"
else
  record FAIL "a missing artifact creates no Pod" "${reading:-<none>} pods=${#count}"
fi
kubectl -n "$NS" create secret generic "${P}-token" --from-literal=token="$FAKE_TOKEN" --dry-run=client -o yaml \
  | kubectl label --local -f - "$LABEL" -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-later, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec: {source: {huggingFace: {repository: Qwen/Qwen2.5-0.5B-Instruct, revision: main, secretRef: {name: ${P}-token}}}}
YAML
pod=""
for _ in $(seq 1 60); do
  pod="$(pods_of "${P}-engine" '{.items[0].metadata.name}')"
  [ -n "$pod" ] && break
  sleep 3
done
commit="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-later" -o jsonpath='{.status.resolved.revision}')"
size="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-later" -o jsonpath='{.status.resolved.sizeBytes}')"
if [ -z "$pod" ]; then
  record FAIL "an Engine Pod is created once the artifact resolves" "no Pod within 180s"
else
  json="$(kubectl -n "$NS" get pod "$pod" -o json)"
  read -r got_cmd got_ref got_limit got_eph_limit got_eph_req <<EOF
$(printf '%s' "$json" | python3 -c '
import json,sys
p=json.load(sys.stdin); c=p["spec"]["containers"][0]
env={e["name"]:e for e in c.get("env",[])}
ref=env.get("HF_TOKEN",{}).get("valueFrom",{}).get("secretKeyRef",{})
vol=[v for v in p["spec"]["volumes"] if v["name"]=="gpustack-model-cache"]
lim=vol[0]["emptyDir"].get("sizeLimit","") if vol else ""
print(" ".join(c["command"][:5]).replace(" ","+"), ref.get("name","")+"/"+ref.get("key",""), lim,
      c["resources"].get("limits",{}).get("ephemeral-storage",""), c["resources"].get("requests",{}).get("ephemeral-storage",""))
')
EOF
  want_limit=$(( size + (size / 10 > 1073741824 ? size / 10 : 1073741824) ))
  if [ "$got_cmd" = "vllm+serve+Qwen/Qwen2.5-0.5B-Instruct+--revision+${commit}" ]; then
    record PASS "the engine downloads the pinned commit" "${got_cmd//+/ }"
  else
    record FAIL "the engine downloads the pinned commit" "command=${got_cmd//+/ } commit=${commit}"
  fi
  if [ "$got_ref" = "${P}-token/token" ] && ! printf '%s' "$json" | command grep -qF -- "$FAKE_TOKEN"; then
    record PASS "the token arrives by secretKeyRef and never as a value" "HF_TOKEN <- ${got_ref}"
  else
    record FAIL "the token arrives by secretKeyRef and never as a value" "ref=${got_ref:-<none>}"
  fi
  if [ "$got_limit" = "$want_limit" ] && [ "$got_eph_limit" != "$got_eph_req" ]; then
    record PASS "the cache is sized from the manifest and the limit raised over the request" \
      "sizeLimit=${got_limit} ephemeral limit=${got_eph_limit} request=${got_eph_req}"
  else
    record FAIL "the cache is sized from the manifest and the limit raised over the request" \
      "sizeLimit=${got_limit:-<none>} want=${want_limit} limit=${got_eph_limit} request=${got_eph_req}"
  fi
fi

echo "== 6. admission =="
refused "another served name is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c98, artifactRef: {name: ${P}-later}}
  roles: [{name: server, instanceType: "${IT}", image: "${IMAGE}", extraArgs: ["--served-model-name", "other"]}]
YAML
refused "a revision beside an artifact is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c98, artifactRef: {name: ${P}-later}}
  roles: [{name: server, instanceType: "${IT}", image: "${IMAGE}", extraArgs: ["--revision", "v2"]}]
YAML
refused "a volume on the weights' path is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c98, artifactRef: {name: ${P}-later}}
  roles:
    - name: server
      instanceType: "${IT}"
      image: "${IMAGE}"
      additionalVolumes: [{mountPath: /var/lib/gpustack, configMap: {name: x}}]
YAML

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-98] ${FAILS} check(s) FAILED"; exit 1; }
