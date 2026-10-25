#!/usr/bin/env bash
#
# CASE 98 — ModelArtifact consumers and Instance persistent claims: a deployment and an Instance
#           mount a claim where its PV lives, wait when the claim cannot be placed, and an
#           Instance's own claim puts it on the PV's node every time it starts
#           (MUTATING, self-recovering; AUTO-SKIPS without two workers)
#
#   case-98.sh <NS>
#
# Prerequisites: nodes=2; gpu=none; features=multi-host; cluster=any; inputs=E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_SYSTEM_NS
# Goal:        Prove the consumer side of the ModelArtifact contract and the placement rules an
#              Instance's persistent claims follow, on a real API server, scheduler, kubelet and
#              Kueue: the bound PV's required node affinity reaches every Pod and TAS places it on
#              the PV's node; a claim that cannot be placed creates no Pod and says why; a replica
#              created before its claim binds is not rolled after it; an Engine deployment's Pod
#              carries the pinned argv, the Secret reference, the size-limited cache and the
#              raised ephemeral-storage limit; admission refuses what would override the artifact;
#              and an Instance whose own claim is bound runs on the PV's node on every start, waits
#              without reserving quota while that node is full, and loses the affinity when the
#              instance-persistent-volume-placement Setting is off.
# Environment: A cluster with at least two schedulable workers (else SKIP: with one worker TAS has
#              no wrong node to pick, so NOTHING WOULD BE VERIFIED), a materialized scheduling
#              chain (run case-1 first), an InstanceType, in <NS> the pool's entrance LocalQueue, a
#              default StorageClass whose provisioner creates volumes on the first Pod's node
#              (kind's local-path), and a worker that reaches the Hub. NO GPU and no engine image:
#              managed roles run the pause image, which cannot start an engine, and every asserted
#              value is on an object or a Pod condition.
# Inputs:      All real. HostPath PVs pinned to the second worker, one per bound claim, created by
#              this case; public Qwen/Qwen2.5-0.5B-Instruct for the Engine rows; the
#              model-artifact-delivery-mode Setting pinned to Engine for the Engine rows and the
#              instance-persistent-volume-placement Setting turned off for the last rows. Override
#              with E2E_MD_INSTANCE_TYPE, E2E_MD_IMAGE.
# Expected:    - a claim deployment: its Pod carries the PV's node affinity, runs on the PV's node,
#                loads /var/lib/gpustack/model read-only under --served-model-name, and reports
#                WeightsReady=True Mounted and status.model.delivery PVC;
#              - an Instance on the same claim: the same affinity and node, a read-only mount at the
#                artifact's directory;
#              - two replicas on a ReadWriteOnce claim: AccessModeConflict, no Pod;
#              - a static no-provisioner claim and an immediate-binding claim left pending:
#                ClaimNotBound, no Pod;
#              - a WaitForFirstConsumer claim on a ModelDeployment: the Pod is created, the claim
#                binds, and the Pod is not rolled;
#              - a missing artifact: ArtifactNotFound and no Pod, then an Engine Pod once it exists,
#                with `vllm serve <repo> --revision <commit>`, HF_TOKEN from a secretKeyRef, a cache
#                whose sizeLimit is the manifest size plus its headroom, a raised ephemeral-storage
#                limit over an unchanged request, and no token value in the Pod spec;
#              - admission refuses another --served-model-name, --revision and a volume on the
#                weights' path. An Instance naming a hub artifact is admitted since the node plugin
#                delivers it; case-103 mounts one;
#              - a workspace claim: over three starts, every Pod carries the PV's affinity and runs
#                on the PV's node;
#              - an additional-volume claim: the same;
#              - the PV's node full: the Pod is not scheduled, its Workload has QuotaReserved not
#                True and no admission; once the placeholder goes, the Pod runs on the PV's node;
#              - a WaitForFirstConsumer claim on an Instance: the Pod carries no affinity, the
#                claim binds and the Pod runs;
#              - an unbound claim on an immediate class: no Pod, phase Starting, and the phase
#                message names the claim as Pending;
#              - the Setting off: that Instance's Pod is created, and a bound claim's Pod carries no
#                affinity.
# Cleanup:     Trap deletes every object carrying the case label, then the PVs and the
#              StorageClasses, and puts back both Settings it changed, the
#              model-artifact-delivery-mode it pinned to Engine and the
#              instance-persistent-volume-placement it turned off, the way it found them.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

NS="${1:?usage: case-98.sh <NS>}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
# shellcheck disable=SC2034 # Read by the sourced library, which shellcheck does not follow.
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
P=c98
LABEL="e2e.gpustack.ai/case=98"
FAKE_TOKEN="e2e-c98-not-a-real-token"
SETTING=instance-persistent-volume-placement

# Two Settings are the cluster baseline this case changes, and both go back the way it found them.
# The worker reads them through its thirty-second Settings cache, so every change waits it out.
settings_save model-artifact-delivery-mode "$SETTING"
# This case proves Engine delivery, and the chart seeds Node when it deploys the model-manager
# plugin, so the case pins Engine for its run and puts back what it found.
pin_engine_delivery() {
  setting_set model-artifact-delivery-mode Engine
  settings_settle
}
set_placement() { # true|false
  setting_set "$SETTING" "$1"
  settings_settle
}

cleanup() {
  echo
  echo "[case-98] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 24); do
    [ -z "$(kubectl -n "$NS" get pods -l "$LABEL" -o name 2>/dev/null)$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "/${P}-")" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete pods,modelartifacts.worker.gpustack.ai,pvc,secret -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete pv "${P}-pv" "${P}-pv-ws" "${P}-pv-add" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete storageclass "${P}-static" "${P}-immediate" --ignore-not-found >/dev/null 2>&1
  # Both Settings go back the way the case found them. The wait for the worker's read cache is
  # spent only when the case actually changed one of them.
  settings_restore
}
trap cleanup EXIT

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
[ -n "$IT" ] || { echo "[case-98] no InstanceType; run case-1 first" >&2; exit 2; }
WORKERS="$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.labels.kubernetes\.io/hostname}{"\n"}{end}' | sort)"
if [ "$(printf '%s\n' "$WORKERS" | command grep -c .)" -lt 2 ]; then
  record SKIP "the claim rows" "fewer than two workers; NOTHING WAS VERIFIED"
  record SKIP "the placement rows" "fewer than two workers: TAS has no wrong node to pick; NOTHING WAS VERIFIED"
  print_rows
  exit 0
fi
PV_NODE="$(printf '%s\n' "$WORKERS" | sed -n 2p)"
PV_NODE_NAME="$(kubectl get nodes -l "kubernetes.io/hostname=${PV_NODE}" -o jsonpath='{.items[0].metadata.name}')"

md() { # name artifact replicas [extra role yaml]
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
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

inst() { # name volume-yaml [additional-volumes-yaml]
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec:
  type: "${IT}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: $2${3:+
  additionalVolumes: $3}
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

pv() { # name; a hostPath volume pinned to the PV node
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $1, labels: {e2e.gpustack.ai/case: "98"}}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  storageClassName: ${P}-static
  hostPath: {path: /tmp/$1, type: DirectoryOrCreate}
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions: [{key: kubernetes.io/hostname, operator: In, values: ["${PV_NODE}"]}]
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

pod_field() { kubectl -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null; }

affinity_of() { pod_field "$1" '{.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[?(@.key=="kubernetes.io/hostname")].values[0]}'; }

wait_running() { # pod bound -> node, or ""
  local node=""
  for _ in $(seq 1 $(( $2 / 3 ))); do
    if [ "$(pod_field "$1" '{.status.phase}')" = Running ]; then
      node="$(pod_field "$1" '{.spec.nodeName}')"
      break
    fi
    sleep 3
  done
  printf '%s' "$node"
}

wait_gone() { # pod
  for _ in $(seq 1 40); do
    kubectl -n "$NS" get pod "$1" >/dev/null 2>&1 || return 0
    sleep 3
  done
  return 1
}

workload_of() { # pod -> quotaReserved|admitted
  kubectl -n "$NS" get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c '
import json,sys
pod=sys.argv[1]
for w in json.load(sys.stdin)["items"]:
    if any(o.get("kind")=="Pod" and o.get("name")==pod for o in w["metadata"].get("ownerReferences",[])):
        c={x["type"]:x["status"] for x in w.get("status",{}).get("conditions",[])}
        print("%s|%s" % (c.get("QuotaReserved","<unset>"), "admission" if w.get("status",{}).get("admission") else "none"))
        break
' "$1"
}

placed() { # check pod
  local node aff
  node="$(wait_running "$2" 120)"
  aff="$(affinity_of "$2")"
  if [ "$node" = "$PV_NODE_NAME" ] && [ "$aff" = "$PV_NODE" ]; then
    record PASS "$1" "node=${node} affinity=${aff}"
  else
    record FAIL "$1" "node=${node:-<not running>} affinity=${aff:-<none>} pv=${PV_NODE}"
  fi
}

# The manifest travels on stdin; md_dryrun_rejects reads it there and prints the row's object.
refused() { # check manifest-on-stdin
  local check="$1" text
  if text="$(md_dryrun_rejects 200)"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

echo "== fixtures: a static and an immediate class, PVs pinned to ${PV_NODE} =="
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
YAML
pv "${P}-pv"
pv "${P}-pv-ws"
pv "${P}-pv-add"
pvc "${P}-bound" "${P}-static" ReadWriteOnce "${P}-pv"
claim_artifact "${P}-bound" "${P}-bound"

echo "== 1. a claim deployment runs on the PV's node =="
md "${P}-claim" "${P}-bound" 1
reading="$(wait_weights "${P}-claim" Mounted 120)"
node="$(pods_of "${P}-claim" '{.items[0].spec.nodeName}')"
aff="$(pods_of "${P}-claim" '{.items[0].spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')"
if [ "$node" = "$PV_NODE_NAME" ] && [ "$aff" = "$PV_NODE" ]; then
  record PASS "the replica carries the PV's node affinity and runs there" "node=${node}"
else
  record FAIL "the replica carries the PV's node affinity and runs there" "node=${node:-<unscheduled>} affinity=${aff:-<none>} pv=${PV_NODE_NAME}"
fi
cmd="$(pods_of "${P}-claim" '{.items[0].spec.containers[0].command}')"
ro="$(pods_of "${P}-claim" "{.items[0].spec.volumes[?(@.name=='gpustack-model')].persistentVolumeClaim.readOnly}")"
sub="$(pods_of "${P}-claim" "{.items[0].spec.containers[0].volumeMounts[?(@.name=='gpustack-model')].subPath}")"
case "$cmd" in
  *'"vllm","serve","/var/lib/gpustack/model"'*'"--served-model-name","e2e/c98"'*)
    if [ "$ro" = true ] && [ "$sub" = qwen ]; then
      record PASS "the engine loads the read-only claim under the served name" "command=${cmd}"
    else
      record FAIL "the engine loads the read-only claim under the served name" "readOnly=${ro} subPath=${sub}"
    fi ;;
  *) record FAIL "the engine loads the read-only claim under the served name" "command=${cmd:-<none>}" ;;
esac
delivery="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-claim" -o jsonpath='{.status.model.delivery}')"
if [ "$reading" = "True|Mounted" ] && [ "$delivery" = PVC ]; then
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
if [ "$inode" = "$PV_NODE_NAME" ] && [ "$imount" = "true/qwen" ]; then
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
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c98, artifactRef: {name: ${P}-later}}
  roles: [{name: server, instanceType: "${IT}", image: "${IMAGE}", extraArgs: ["--served-model-name", "other"]}]
YAML
refused "a revision beside an artifact is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c98, artifactRef: {name: ${P}-later}}
  roles: [{name: server, instanceType: "${IT}", image: "${IMAGE}", extraArgs: ["--revision", "v2"]}]
YAML
refused "a volume on the weights' path is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c98, artifactRef: {name: ${P}-later}}
  roles:
    - name: server
      instanceType: "${IT}"
      image: "${IMAGE}"
      additionalVolumes: [{mountPath: /var/lib/gpustack, configMap: {name: x}}]
YAML

echo "== 7. a workspace claim runs on the PV's node on every start =="
pvc "${P}-ws" "${P}-static" ReadWriteOnce "${P}-pv-ws"
pvc "${P}-add" "${P}-static" ReadWriteOnce "${P}-pv-add"
for c in "${P}-ws" "${P}-add"; do
  for _ in $(seq 1 20); do
    [ "$(kubectl -n "$NS" get pvc "$c" -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
    sleep 3
  done
done
inst "${P}-ws" "{persistent: {name: ${P}-ws}}"
for round in 1 2 3; do
  placed "start ${round}: the workspace claim's Pod carries the PV's affinity and runs there" "${P}-ws"
  [ "$round" = 3 ] && break
  kubectl -n "$NS" patch instances.worker.gpustack.ai "${P}-ws" --type=merge -p '{"spec":{"stop":true}}' >/dev/null
  wait_gone "${P}-ws" || record FAIL "start ${round}: the Pod goes on stop" "still present after 120s"
  # Admission starts only a Stopped Instance, which the phase says a pass after the Pod is gone.
  for _ in $(seq 1 20); do
    [ "$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-ws" -o jsonpath='{.status.phase}')" = Stopped ] && break
    sleep 3
  done
  kubectl -n "$NS" patch instances.worker.gpustack.ai "${P}-ws" --type=merge -p '{"spec":{"stop":false}}' >/dev/null
done
REQ="$(pod_field "${P}-ws" '{.spec.containers[0].resources.requests.cpu}')"

echo "== 8. an additional-volume claim runs on the PV's node =="
inst "${P}-add" "{ephemeral: {capacity: 1Gi}}" "[{mountPath: /mnt/data, persistent: {name: ${P}-add}}]"
placed "the additional claim's Pod carries the PV's affinity and runs there" "${P}-add"

echo "== 9. with the PV's node full, the Workload waits without quota =="
# The rows above leave their Pods on the PV's node. This row measures that node's free CPU, so
# the deployments and the Instances this case made go first.
kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
for _ in $(seq 1 40); do
  [ -z "$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "/${P}-")" ] && break
  sleep 3
done
wait_gone "${P}-ws"
wait_gone "${P}-add"
FREE="$(kubectl get pods -A --field-selector "spec.nodeName=${PV_NODE_NAME}" -o json | python3 -c '
import json,sys
def milli(q):
    q=str(q)
    return int(q[:-1]) if q.endswith("m") else int(float(q)*1000)
alloc=milli(sys.argv[1]); used=0
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase") in ("Succeeded","Failed"): continue
    for c in p["spec"]["containers"]:
        used+=milli(c.get("resources",{}).get("requests",{}).get("cpu","0"))
print(alloc-used)
' "$(kubectl get node "$PV_NODE_NAME" -o jsonpath='{.status.allocatable.cpu}')")"
REQ_M="$(printf '%s' "${REQ:-0}" | python3 -c 'import sys; q=sys.stdin.read().strip() or "0"; print(int(q[:-1]) if q.endswith("m") else int(float(q)*1000))')"
FILL=$(( FREE - REQ_M / 2 ))
if [ "$REQ_M" -le 0 ] || [ "$FILL" -le 0 ]; then
  record FAIL "the PV's node can be filled" "free=${FREE}m instance request=${REQ:-<none>}"
else
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-fill, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "98"}}
spec:
  nodeName: ${PV_NODE_NAME}
  containers: [{name: pause, image: "${IMAGE}", resources: {requests: {cpu: "${FILL}m"}}}]
YAML
  wait_running "${P}-fill" 60 >/dev/null
  inst "${P}-full" "{persistent: {name: ${P}-ws}}"
  sleep 45
  node="$(pod_field "${P}-full" '{.spec.nodeName}')"
  wl="$(workload_of "${P}-full")"
  if [ -n "$(pod_field "${P}-full" '{.metadata.uid}')" ] && [ -z "$node" ] && [ "${wl%%|*}" != True ] && [ "${wl#*|}" = none ]; then
    record PASS "the full node's Workload waits without reserving quota" "fill=${FILL}m workload=${wl}"
  else
    record FAIL "the full node's Workload waits without reserving quota" "node=${node:-<none>} workload=${wl:-<none>}"
  fi
  kubectl -n "$NS" delete pod "${P}-fill" --wait=false >/dev/null 2>&1
  placed "once room appears the Pod runs on the PV's node" "${P}-full"
fi

echo "== 10. a WaitForFirstConsumer claim binds where its first Pod lands =="
default_class="$(kubectl get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}|{.volumeBindingMode}|{.provisioner}{"\n"}{end}' | head -1)"
case "$default_class" in
  *'|WaitForFirstConsumer|kubernetes.io/no-provisioner'|'') record SKIP "a WaitForFirstConsumer claim binds on the first Pod's node" "no default class provisioning on the first Pod's node: '${default_class}'" ;;
  *'|WaitForFirstConsumer|'*)
    pvc "${P}-wffc" "" ReadWriteOnce
    inst "${P}-wffc" "{persistent: {name: ${P}-wffc}}"
    node="$(wait_running "${P}-wffc" 180)"
    aff="$(affinity_of "${P}-wffc")"
    phase="$(kubectl -n "$NS" get pvc "${P}-wffc" -o jsonpath='{.status.phase}' 2>/dev/null)"
    if [ -n "$node" ] && [ -z "$aff" ] && [ "$phase" = Bound ]; then
      record PASS "a WaitForFirstConsumer claim binds on the first Pod's node" "node=${node} claim=${phase}"
    else
      record FAIL "a WaitForFirstConsumer claim binds on the first Pod's node" "node=${node:-<not running>} affinity=${aff:-<none>} claim=${phase:-<none>}"
    fi ;;
  *) record SKIP "a WaitForFirstConsumer claim binds on the first Pod's node" "the default class binds immediately: '${default_class}'" ;;
esac

echo "== 11. an unbound claim on an immediate class creates no Pod =="
pvc "${P}-imm-inst" "${P}-immediate" ReadWriteOnce
inst "${P}-imm-inst" "{persistent: {name: ${P}-imm-inst}}"
sleep 40
phase="$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-imm-inst" -o jsonpath='{.status.phase}')"
msg="$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-imm-inst" -o jsonpath='{.status.phaseMessage}')"
if ! kubectl -n "$NS" get pod "${P}-imm-inst" >/dev/null 2>&1 && [ "$phase" = Starting ] &&
  printf '%s' "$msg" | command grep -qF "PersistentVolumeClaim \"${P}-imm-inst\" is Pending"; then
  record PASS "an unbound immediate claim creates no Pod and says why" "${phase}: ${msg}"
else
  record FAIL "an unbound immediate claim creates no Pod and says why" "pod=$(pod_field "${P}-imm-inst" '{.metadata.name}') phase=${phase:-<none>} message=${msg:-<none>}"
fi

echo "== 12. the escape Setting restores the render from claim names alone =="
set_placement false
pod=""
for _ in $(seq 1 20); do
  pod="$(pod_field "${P}-imm-inst" '{.metadata.name}')"
  [ -n "$pod" ] && break
  sleep 3
done
if [ -n "$pod" ] && [ -z "$(affinity_of "${P}-imm-inst")" ]; then
  record PASS "with the Setting off the waiting Instance's Pod is created" "pod=${pod}"
else
  record FAIL "with the Setting off the waiting Instance's Pod is created" "pod=${pod:-<none>} within 60s"
fi
inst "${P}-off" "{ephemeral: {capacity: 1Gi}}" "[{mountPath: /mnt/data, persistent: {name: ${P}-add}}]"
pod=""
for _ in $(seq 1 20); do
  pod="$(pod_field "${P}-off" '{.metadata.name}')"
  [ -n "$pod" ] && break
  sleep 3
done
aff="$(affinity_of "${P}-off")"
if [ -n "$pod" ] && [ -z "$aff" ]; then
  record PASS "with the Setting off a bound claim's Pod carries no affinity" "pod=${pod}"
else
  record FAIL "with the Setting off a bound claim's Pod carries no affinity" "pod=${pod:-<none>} affinity=${aff:-<none>}"
fi
# Both Settings go back the way the case found them, so the trap's own restore finds them unchanged.
settings_restore

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-98] ${FAILS} check(s) FAILED"; exit 1; }
