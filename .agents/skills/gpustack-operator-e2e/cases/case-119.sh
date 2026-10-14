#!/usr/bin/env bash
#
# CASE 119 — The shared claim mounts the same files in the encoder's and the prefiller's Pods
#   (MUTATING, self-recovering)
#   AUTO-SKIPS without an InstanceType that reports an admitted accelerator platform, which this
#   case builds by injecting a CPU vendor label; its runtime row additionally skips, naming the
#   image, when the witness image cannot be pulled.
#
#   case-119.sh <NS>
#
# Goal:        On the platform whose encoder leg runs through a mounted directory rather than the
#              network, the encoder writes its embeddings into a claim and the prefiller reads them
#              out of the same one. Admission holds the contract on the spec side (one claim, one
#              sub-path, one mount path, a writable encoder mount), and this case holds the render
#              and the storage side: both role Pods mount THE SAME claim at the same path, and the
#              claim itself is one filesystem — a file written through one mount is readable
#              through the other.
#
#              THE RENDERED MOUNTS AND THE CLAIM'S OWN SHARING ARE TWO HALVES. Reading only the
#              rendered specs proves the operator points both roles at one claim name; it cannot
#              tell a claim that really shares bytes from two claims sharing a name. Reading only a
#              witness exchange proves the claim shares; it cannot tell whether the deployment
#              mounts it. So the rows assert the volumes and mounts the two role Pods render, and
#              then a pair of case-owned witness Pods — pinned to the one node the pool covers,
#              mounting the same claim — writes through one mount and reads back through the other.
#
#              THE WITNESSES STAND IN FOR THE ROLE PODS AT RUNTIME, on purpose. The role replicas
#              are placeholders on an explicit pause image, which carries no shell: their mounts
#              are the render's contract, and the witness pair answers the question a shell-less
#              replica cannot — whether the claim the renders name is one filesystem. A
#              bytes-through-the-engine-encoder measurement belongs to the accelerator cases.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator: the
#              encoder shape reaches admission through the platform an injected CPU vendor label
#              builds ("Ascend" — the admitted row whose connector is the mounted directory and
#              whose floor, 0.23.0, the pinned version 0.30.0 clears). The gpustack-settings Secret
#              must exist in the system namespace (E2E_SYSTEM_NS, default gpustack-system). The case
#              creates its own PV and PVC (a hostPath volume under /tmp on the labeled node,
#              manually bound), so no StorageClass is needed.
#
#              AUTO-SKIP: the subject needs the platform InstanceType, and the vendor label may not
#              take (NFD owns cpu-model.vendor_id and may decline an injected override). The
#              witness half additionally needs E2E_C119_WITNESS_IMAGE (default busybox:1.36) to be
#              pullable; an image-pull failure is a SKIP naming the image, never a FAIL — a verdict
#              about the operator cannot ride on a container nobody could start.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and
#              one settings key (CPU-manufacturer awareness) flipped on — both restored by the
#              trap. REAL: one PV, one PVC and one ModelDeployment whose encode and prefill roles
#              mount the claim at one path and whose decode role mounts nothing; two one-shot
#              witness Pods mounting the same claim, the writer started first and the reader created
#              only once the writer has finished.
#
# Expected:    - the deployment is admitted and created, and its three role Pods exist (gated
#                counts, they never need to run);
#              - the encoder Pod renders a volume naming the claim, mounted at /ec-cache and NOT
#                read-only;
#              - the prefiller Pod renders the same claim name at the same mount path;
#              - the decoder Pod renders no volume naming the claim;
#              - the PVC reaches Bound against the case's own PV;
#              - a file written through the claim by one witness Pod is read back byte-for-byte by
#                the other, proving one filesystem behind both mounts.
#
# Cleanup:     The trap deletes the deployment, releases any Workload still holding its replicas,
#              deletes the witness Pods, the PVC and the PV, the NodeFeature object, and restores
#              the settings key to its exact original value, then waits for the chain to settle back:
#              the derived type goes only once the setting cache flips and the pool re-derives.
#              Idempotent, runs on pass AND fail, safe to re-run.
#
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-119.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
VENDOR="${E2E_C119_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF="case119-cpu-vendor"
MD=case119-epd
CLAIM=case119-claim
PV=case119-pv
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
WITNESS_IMAGE="${E2E_C119_WITNESS_IMAGE:-busybox:1.36}"
MOUNT=/ec-cache
SETTLE_ROUNDS="${E2E_C119_SETTLE:-60}"   # 60 x 5s for the settings cache and re-derivation
POD_ROUNDS="${E2E_C119_PODS:-40}"        # 40 x 5s for the group to render its Pods

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

pick_worker_node() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("items",[]):
    cs=n.get("status",{}).get("conditions",[])
    if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in cs):
        continue
    if n.get("spec",{}).get("unschedulable"):
        continue
    ts=n.get("spec",{}).get("taints",[])
    if any(str(t.get("key","")).startswith("node-role.kubernetes.io/") for t in ts):
        continue
    print(n["metadata"]["name"])
    break
'
}

epd_platform_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ "$manufacturer" = "$PLATFORM" ]; then
          echo "$name"
          break
        fi
      done
}

ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
set_awareness() { # true|false
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":\"$(printf '%s' "$1" | base64)\"}}" >/dev/null 2>&1
}
restore_setting() {
  local v=null
  [ -n "$ORIG_SETTING" ] && v="\"${ORIG_SETTING}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":${v}}}" >/dev/null 2>&1
}

# Restoring the value is not the end of the restore: the worker reads it through a thirty-second
# cache, and the type derived from it outlives the patch by that cache plus one reconcile. Wait for
# the derived type to go, so the next case in a serial run does not list an aware type this run left
# behind (case-18's wait_generic, on this same fixture). A cluster that started with the setting
# already on keeps its derived type, so there is nothing to wait for and the wait is skipped.
settle_back() {
  [ "$(printf '%s' "$ORIG_SETTING" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    [ -z "$(epd_platform_type)" ] && return 0
    sleep 5
  done
  return 0
}

# One list call, because this runs from the EXIT trap: Kueue holds a finalizer on every Pod of a
# serving group and releases it only when the Workload is deleted (case-50's mechanic).
force_release() {
  local row wl uids owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in *" $u "*)
        kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
          --ignore-not-found --wait=false >/dev/null 2>&1
        break
        ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF

  return 0
}

cleanup() {
  echo
  echo "[case-119] cleanup"
  kubectl -n "$NS" delete pod case119-witness-write case119-witness-read \
    --ignore-not-found --force --grace-period=0 >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 12); do
    sleep 5
    force_release
    [ -z "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null)" ] && break
  done
  kubectl -n "$NS" delete pvc "$CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete pv "$PV" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
  restore_setting
  settle_back
}
trap cleanup EXIT

# ---------------------------------------------------------------- the fixture

NODE="$(pick_worker_node)"
if [ -z "$NODE" ]; then
  echo "[case-119] no Ready schedulable worker node; this case cannot place its vendor label" >&2
  exit 2
fi

echo "[case-119] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"

kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
spec:
  labels:
    feature.node.kubernetes.io/cpu-model.vendor_id: "${VENDOR}"
  features: {}
YAML
set_awareness true

IT=""
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  IT="$(epd_platform_type)"
  [ -n "$IT" ] && break
  sleep 5
done

if [ -z "$IT" ]; then
  record SKIP "the shared-claim encoder shape exists" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  print_rows
  echo "[case-119] skipped: no platform InstanceType"
  exit 0
fi

ENTRANCE="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  [ -n "$(kubectl -n "$NS" get localqueue "$ENTRANCE" -o name 2>/dev/null)" ] && break
  sleep 5
done

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}' from the injected vendor label; entrance ${ENTRANCE}"

# The claim the two roles mount: a hostPath volume under /tmp on the labeled node, manually bound.
# The pool the labeled node feeds is the only pool the deployment names, so every Pod that mounts
# this claim lands on that node and ReadWriteOnce is enough; DirectoryOrCreate spares the case any
# need to reach the node's shell. A storage class is deliberately not used: binding by volumeName
# needs none, and the default class of an arbitrary cluster is not this case's to depend on.
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${PV}
  labels:
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath:
    path: /tmp/gpustack-case-119
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${CLAIM}
  namespace: ${NS}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: ${PV}
YAML

# ---------------------------------------------------------------- the subject

# The encode and prefill roles mount the same claim at the same path; the decode role mounts
# nothing, because the mounted-directory leg ends at the prefiller.
APPLY_OUT="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    version: "0.30.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
      additionalVolumes:
        - mountPath: ${MOUNT}
          persistentVolumeClaim:
            claimName: ${CLAIM}
    - name: prefill
      kind: Prefill
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
      additionalVolumes:
        - mountPath: ${MOUNT}
          persistentVolumeClaim:
            claimName: ${CLAIM}
    - name: decode
      kind: Decode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"

if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o name >/dev/null 2>&1; then
  record PASS "the shared-claim encoder deployment is admitted and created" \
    "encode and prefill mount ${CLAIM} at ${MOUNT} on ${IT}"
else
  record FAIL "the shared-claim encoder deployment is admitted and created" \
    "no modeldeployment/${MD} after the apply: $(printf '%s' "${APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  print_rows
  exit 1
fi

PODS=""
for _ in $(seq 1 "$POD_ROUNDS"); do
  PODS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null | grep -c . || true)"
  [ "${PODS:-0}" -ge 3 ] && break
  sleep 5
done
if [ "${PODS:-0}" -ge 3 ]; then
  record PASS "all three role Pods exist" "${PODS} Pods rendered; the mount rows below read specs, not running containers"
else
  record FAIL "all three role Pods exist" \
    "only ${PODS:-0} Pod(s) of the group appeared within $((POD_ROUNDS * 5))s, so the mount rows below have nothing to read"
fi

# ---------------------------------------------------------------- the rendered mounts

# The claim one role's FIRST Pod mounts, as "claimName|mountPath|readOnly", or "" when it mounts no
# claim. Pending Pods count: the render is what these rows pin, and a gated Pod carries its full
# spec (case-89's reading posture).
#
# EVERY CONTAINER IS SEARCHED, not just the first. A direct decode renders the routing sidecar as
# the FIRST regular container, and reading that one would let the decoder's row pass whatever the
# engine mounts: the sidecar carries no mounts of its own, so a claim the decode role grew would
# still read as "no claim" there. Looking through all of them keeps every row a question about the
# whole Pod.
role_pod_claim() { # <component>
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${MD},app.kubernetes.io/component=$1" \
    -o json 2>/dev/null | python3 -c '
import json,sys
items=json.load(sys.stdin).get("items",[])
if not items:
    print("")
    raise SystemExit
pod=items[0]
claims={}
for v in pod["spec"].get("volumes",[]):
    p=v.get("persistentVolumeClaim")
    if p and p.get("claimName"):
        claims[v["name"]]=p["claimName"]
for ctr in pod["spec"].get("containers",[]):
    for m in ctr.get("volumeMounts",[]):
        claim=claims.get(m.get("name",""))
        if claim:
            print(claim+"|"+m.get("mountPath","")+"|"+str(m.get("readOnly",False)).lower())
            raise SystemExit
print("")'
}

ENCODE_CLAIM="$(role_pod_claim encode)"
PREFILL_CLAIM="$(role_pod_claim prefill)"
DECODE_CLAIM="$(role_pod_claim decode)"

case "$ENCODE_CLAIM" in
  "${CLAIM}|${MOUNT}|false")
    record PASS "the encoder Pod mounts the claim at the shared path, writable" \
    "rendered ${ENCODE_CLAIM}"
    ;;
  *)
    record FAIL "the encoder Pod mounts the claim at the shared path, writable" \
      "rendered '${ENCODE_CLAIM:-no claim mount}', expected '${CLAIM}|${MOUNT}|false'"
    ;;
esac

case "$PREFILL_CLAIM" in
  "${CLAIM}|${MOUNT}|"*)

    # Read-only is the PREFILLER's own choice — it only reads — so the row pins the claim name and
    # the path and leaves the flag open, unlike the encoder's row above.
    record PASS "the prefiller Pod mounts the same claim at the same path" \
      "rendered ${PREFILL_CLAIM}, matching the encoder's claim and path"
    ;;
  *)
    record FAIL "the prefiller Pod mounts the same claim at the same path" \
      "rendered '${PREFILL_CLAIM:-no claim mount}', expected '${CLAIM}|${MOUNT}|<any readOnly>'"
    ;;
esac

if [ -z "$DECODE_CLAIM" ]; then
  record PASS "the decoder Pod mounts no claim" \
    "the mounted-directory leg ends at the prefiller, and the decode role declares no volume"
else
  record FAIL "the decoder Pod mounts no claim" "rendered ${DECODE_CLAIM}"
fi

# ---------------------------------------------------------------- the claim is one filesystem

PVC_PHASE="$(kubectl -n "$NS" get pvc "$CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$PVC_PHASE" = Bound ]; then
  record PASS "the claim is Bound" "pvc/${CLAIM} bound to pv/${PV}"
else
  record FAIL "the claim is Bound" "pvc/${CLAIM} phase='${PVC_PHASE:-<none>}', so no witness can mount it"
fi

# Two one-shot witnesses pinned to the labeled node: the pool's only node, which is also where the
# hostPath lives. The writer records a token through its mount; the reader cats the same path. That
# round-trip is the reading that separates one shared filesystem from two claims sharing a name.
#
# THE WRITER IS CREATED FIRST AND THE READER ONLY AFTER IT HAS RUN, because the reader cats a file
# that does not exist until the writer has written it: created together, whichever the kubelet starts
# first decides the verdict, and on a cluster that already holds the image the reader usually loses,
# failing a claim that shares perfectly well. Nothing in the manifest, the node or the loop orders
# them, so the case orders them.
if [ "$PVC_PHASE" = Bound ]; then
  TOKEN="case119-$(date +%s)"
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: case119-witness-write
  namespace: ${NS}
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  containers:
    - name: write
      image: ${WITNESS_IMAGE}
      command: ["sh", "-c", "printf '%s' '${TOKEN}' > /shared/written-by-the-other-mount"]
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: ${CLAIM}
YAML

  # An image that cannot be pulled is a SKIP naming the image (case-81's rule): a container nobody
  # could start is a reading nobody took, not a verdict about the claim.
  pull_reason() { # <pod>
    kubectl -n "$NS" get pod "$1" \
      -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null
  }

  W_ROUNDS=$((POD_ROUNDS))
  W_OK=no
  for _ in $(seq 1 "$W_ROUNDS"); do
    case "$(kubectl -n "$NS" get pod case119-witness-write -o jsonpath='{.status.phase}' 2>/dev/null)" in
      Succeeded) W_OK=yes; break ;;
      Failed) break ;;
    esac
    case "$(pull_reason case119-witness-write)" in ImagePullBackOff | ErrImagePull) break ;; esac
    sleep 5
  done

  if [ "$W_OK" != yes ]; then
    case "$(pull_reason case119-witness-write)" in
      ImagePullBackOff | ErrImagePull)
        record SKIP "a file written through one mount is read back through the other" \
          "the witness image '${WITNESS_IMAGE}' could not be pulled ($(pull_reason case119-witness-write)), so no reading was taken; the render rows above stand on their own"
        ;;
      *)
        record FAIL "a file written through one mount is read back through the other" \
          "the writer witness never completed (phase '$(kubectl -n "$NS" get pod case119-witness-write -o jsonpath='{.status.phase}' 2>/dev/null)')"
        ;;
    esac
  else
    # The file exists now, so the reader is safe to start.
    kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: case119-witness-read
  namespace: ${NS}
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  containers:
    - name: read
      image: ${WITNESS_IMAGE}
      command: ["sh", "-c", "cat /shared/written-by-the-other-mount"]
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: ${CLAIM}
YAML

    R_OK=no
    for _ in $(seq 1 "$W_ROUNDS"); do
      case "$(kubectl -n "$NS" get pod case119-witness-read -o jsonpath='{.status.phase}' 2>/dev/null)" in
        Succeeded | Failed) R_OK=yes; break ;;
      esac
      case "$(pull_reason case119-witness-read)" in ImagePullBackOff | ErrImagePull) break ;; esac
      sleep 5
    done

    READ_BACK=""
    if [ "$R_OK" = yes ]; then
      READ_BACK="$(kubectl -n "$NS" logs case119-witness-read -c read 2>/dev/null)"
    fi
    if [ "$READ_BACK" = "$TOKEN" ]; then
      record PASS "a file written through one mount is read back through the other" \
        "witness pair exchanged '${READ_BACK}' through pvc/${CLAIM}: one filesystem behind both mounts"
    else
      record FAIL "a file written through one mount is read back through the other" \
        "reader witness returned '${READ_BACK:-<nothing>}', expected '${TOKEN}'"
    fi
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-119] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-119] all checks passed"
