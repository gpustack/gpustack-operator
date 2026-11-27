#!/usr/bin/env bash
#
# CASE 38 — ModelDeploymentForger & ModelKernelArtifact end-to-end closed loop: admission and
#           content-class isolation refusals, deterministic 2-node mock-Forge pipeline
#           (probe -> Kueue ForgeSandbox -> emptyDir ingest -> pin GC immunity -> Peer Sync ->
#           read-only CSI mount -> readiness-gated canary rollout -> gen-2 promotion & gen-1
#           unpin/GC -> simulated canary fault auto-rollback & persistent quarantine), and
#           live-GPU probe + Triton kernel promotion qualification
#           (MUTATING, self-cleaning; Leg A Part 2 AUTO-SKIPS without 2 workers, CSI plugin,
#            LocalQueue, and >= 3 accelerator units; Leg B AUTO-SKIPS without real GPU + images)
#
#   case-38.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C38_ACCELERATOR_TYPE,E2E_C38_ARTIFACT,E2E_C38_BOUND,E2E_C38_ENGINE_IMAGE,E2E_C38_MODEL,E2E_C38_MOCK_IMAGE,E2E_C38_PROBE_IMAGE,E2E_C38_READY_BOUND,E2E_C38_RUNNER_IMAGE,E2E_C38_SGLANG_IMAGE,E2E_C38_VLLM_IMAGE
# Goal:        Verify the full kernel specialization and delivery pipeline end to end:
#              - Leg A Part 1 (Admission & security refusals on every cluster):
#                1. ModelKernelArtifact with spec.source.configMap fails strict decoding.
#                2. ModelDeployment additionalVolumes[].configMap overlapping
#                   /var/lib/gpustack/kernels fails admission (ConfigMapKernelMountForbidden).
#                3. Tenant ModelKernelArtifact with spec.source.nodeStore fails admission
#                   (NodeStoreSourceForbidden).
#                4. Cross-namespace CSI mount and cross-class (ModelArtifact mounting a Kernel
#                   published tree) mount fail with PermissionDenied.
#              - Leg A Part 2 (2-node CPU-mock / Kind closed loop):
#                Runs ModelDeploymentForger (mode: Active, sandbox.runnerMode: Mock) against a
#                2-replica mock-sentinel SGLang ModelDeployment serving /gpustack/v1/{ready,live,
#                ack,pause,runtime_config,probe} and /metrics:
#                probe distillation -> Kueue single-card ForgeSandbox (model-forger mock -> verify ->
#                hold) -> seed node emptyDir /ingest/v1/trees (Kind: Kernel, Source: Forge) ->
#                NodeModelStore.spec.pinned watermark-GC survival -> peer node zero-Hub
#                /peer/v1/trees/{hex} Peer Sync -> read-only /var/lib/gpustack/kernels/
#                dispatch-manifest.json CSI mount inside replaced Pods -> readiness-gated canary
#                rolling replacement -> gen-2 promotion & gen-1 pruning/unpinning -> simulated
#                gen-3 canary failure (unsupported_kernel_artifact) auto-rollback to gen-2 and
#                persistent ModelKernelArtifact status.quarantined=true.
#              - Leg B (Live GPU qualification; auto-skips without real NVIDIA/AMD/Ascend GPU,
#                pre-warmed ModelArtifact, and engine/runner images):
#                Runs real 4-step /gpustack/v1/probe on vLLM / SGLang under active streaming load,
#                checks P99 ITL stability and zero /dev/shm/gpustack-probe residue, runs real GPU
#                ForgeSandbox, promotes across 2 GPU nodes via Peer Sync, and confirms
#                loaded_kernel_keys on /gpustack/v1/runtime_config.
# Environment: Any cluster with ModelKernelArtifact, ModelDeploymentForger, and ModelDeployment CRDs.
#              Leg A Part 2 requires >= 2 schedulable worker nodes, model.csi.gpustack.ai CSIDriver,
#              an entrance LocalQueue in <NS>, and either CPU Mock Device Manager or >= 3 free
#              accelerator units across the 2 workers. Leg B requires >= 3 real GPUs across >= 2
#              GPU nodes, pre-warmed ModelArtifact (E2E_C38_ARTIFACT), E2E_C38_RUNNER_IMAGE, and
#              E2E_C38_VLLM_IMAGE / E2E_C38_SGLANG_IMAGE; auto-skips cleanly otherwise.
# Inputs:      MOCKED (Leg A Part 2): lightweight pure-stdlib mock-sentinel SGLang Pod script,
#              built-in gpustack-operator model-forger mock/verify/hold subcommands, Mock
#              Device Manager presets (when active), and _model-hub.py for GC/cross-class fixtures.
#              REAL: CRD schemas, webhooks, Kueue admission, model-manager /ingest/v1/trees,
#              NodeModelStore pin union & GC, /peer/v1/trees/{hex}, model.csi.gpustack.ai mounts,
#              readiness-gated canary slot, rollback/quarantine, and (Leg B) real vLLM/SGLang.
# Expected:    All admission refusals, CSI isolation denials, 2-node mock-Forge closed-loop
#              generations (gen-1, gen-2, gen-3 rollback + quarantine), and (when real GPU inputs
#              exist) Leg B live GPU checks PASS with zero unexpected resource leaks.
# Cleanup:     EXIT trap deletes all ModelDeploymentForgers, ModelDeployments,
#              ModelKernelArtifacts, ModelArtifacts, test Pods, ConfigMaps, Secrets, hubs, and
#              second namespace, force-releases Kueue Workloads, unmounts any tmpfs cache mount
#              and restarts its plugin Pod, restores saved Settings and Mock DM ConfigMap, and
#              restores PSA labels on <NS>.
set -uo pipefail

CASE_ID=38
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_model-hub-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_mock-dm-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-38.sh <NS>" >&2
  exit 2
fi
if [ "$NS" = "$SYSTEM_NS" ]; then
  echo "[case-38] refusing the system namespace ${SYSTEM_NS}; usage: case-38.sh <consumer-NS>" >&2
  exit 2
fi

MKA_CRD="modelkernelartifacts.worker.gpustack.ai"
MDF_CRD="modeldeploymentforgers.worker.gpustack.ai"
MD_CRD="modeldeployments.worker.gpustack.ai"
MKA_RES="modelkernelartifacts.v1alpha1.worker.gpustack.ai"
MDF_RES="modeldeploymentforgers.v1alpha1.worker.gpustack.ai"
MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$"
P="e2e-c38-${SFX}"
NSB="${NS}-c38-${SFX}"
LABEL="e2e.gpustack.ai/case=38"
KERNEL_MOUNT_PATH="/var/lib/gpustack/kernels"
CACHE_ROOT="/var/lib/gpustack/models"

BOUND="${E2E_C38_BOUND:-300}"
READY_BOUND="${E2E_C38_READY_BOUND:-720}"
MOCK_IMAGE="${E2E_C38_MOCK_IMAGE:-${MH_PYTHON}}"
PROBE_IMAGE="${E2E_C38_PROBE_IMAGE:-curlimages/curl:8.10.1}"
LIVE_ARTIFACT="${E2E_C38_ARTIFACT:-qwen3-8b}"
LIVE_MODEL="${E2E_C38_MODEL:-Qwen/Qwen3-8B}"
LIVE_RUNNER_IMAGE="${E2E_C38_RUNNER_IMAGE:-}"
LIVE_VLLM_IMAGE="${E2E_C38_VLLM_IMAGE:-}"
LIVE_SGLANG_IMAGE="${E2E_C38_SGLANG_IMAGE:-}"

SCRATCH="$(mktemp -d)"
MOCK_CM_SNAPSHOT="${SCRATCH}/mock-dm-cm.yaml"
MOCK_DM_MUTATED=0
TMPFS_NODES=()
PSA_SNAPSHOT=0
ORIG_PSS_ENFORCE=""
ORIG_PSS_VERSION=""
CREATED_MDFS=()
CREATED_MDS=()
CREATED_PODS=()
SETTINGS_SAVED=0
LOAD_PID=""
CLEANED=false

ns_label() {
  kubectl get namespace "$1" -o json 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("metadata",{}).get("labels",{}).get(sys.argv[1],""))' "$2" 2>/dev/null || true
}

restore_ns_psa() {
  local labels=()
  [ "$PSA_SNAPSHOT" -eq 1 ] || return 0
  if [ -n "$ORIG_PSS_ENFORCE" ]; then
    labels+=("pod-security.kubernetes.io/enforce=${ORIG_PSS_ENFORCE}")
  else
    labels+=("pod-security.kubernetes.io/enforce-")
  fi
  if [ -n "$ORIG_PSS_VERSION" ]; then
    labels+=("pod-security.kubernetes.io/enforce-version=${ORIG_PSS_VERSION}")
  else
    labels+=("pod-security.kubernetes.io/enforce-version-")
  fi
  kubectl label namespace "$NS" --overwrite "${labels[@]}" >/dev/null 2>&1 || true
}

# shellcheck disable=SC2317 # Invoked by EXIT trap.
cleanup() {
  local item n
  if [ "$CLEANED" = true ]; then
    return 0
  fi
  CLEANED=true
  echo
  echo "[case-38] cleanup"

  if [ -n "${LOAD_PID:-}" ]; then
    kill "$LOAD_PID" 2>/dev/null || true
    wait "$LOAD_PID" 2>/dev/null || true
    LOAD_PID=""
  fi

  for item in ${CREATED_MDFS[@]+"${CREATED_MDFS[@]}"}; do
    kubectl -n "$NS" delete "$MDF_RES" "$item" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  for item in ${CREATED_MDS[@]+"${CREATED_MDS[@]}"}; do
    kubectl -n "$NS" delete "$MD_RES" "$item" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    md_force_release "$NS" "$item" || true
  done
  for item in ${CREATED_PODS[@]+"${CREATED_PODS[@]}"}; do
    kubectl -n "$NS" delete pod "$item" --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1 || true
  done
  kubectl -n "$NS" delete pods -l "$LABEL" --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MKA_RES" -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MKA_RES" -l "app.kubernetes.io/instance=${P}-mdf" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MKA_RES" -l "app.kubernetes.io/instance=${P}-live-mdf" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l "$LABEL" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete configmap -l "$LABEL" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete namespace "$NSB" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  for n in ${TMPFS_NODES[@]+"${TMPFS_NODES[@]}"}; do
    docker exec "$n" umount -l "$CACHE_ROOT" >/dev/null 2>&1 || true
    kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$n")" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done

  if [ "$MOCK_DM_MUTATED" -eq 1 ]; then
    mock_dm_restore_cm "$SYSTEM_NS" "$MOCK_DM_CM" "$MOCK_CM_SNAPSHOT" >/dev/null 2>&1 || true
  fi
  if [ "$SETTINGS_SAVED" -eq 1 ]; then
    settings_restore
  fi
  restore_ns_psa
  rm -rf "$SCRATCH"
  return 0
}
trap cleanup EXIT

# Ensure required CRDs exist before running admission checks.
for crd in "$MKA_CRD" "$MDF_CRD" "$MD_CRD"; do
  if ! kubectl get crd "$crd" >/dev/null 2>&1; then
    record SKIP "case-38 prerequisites" "missing CRD ${crd}"
    results
    exit 0
  fi
done

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1 || true

# Compute a canonical LocalKernelKey signatureHash16 matching pkg/modelkernelkey.
compute_kernel_key() {
  local engine="$1" version="$2" manu="$3" arch="$4" op="$5" hidden="$6"
  python3 - "$engine" "$version" "$manu" "$arch" "$op" "$hidden" <<'PY'
import hashlib, json, sys
engine, ver, manu, arch, op, hidden = sys.argv[1:7]
payload = json.dumps({
    "constAxes": {"hidden_size": int(hidden)},
    "constants": {},
    "inputDtypes": ["bfloat16", "bfloat16"],
    "outputDtypes": ["bfloat16"],
}, separators=(",", ":"), sort_keys=True)
sig = hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]
print(f"{engine.lower()}/{ver}/{manu.lower()}/{arch.lower()}/{op.lower()}/{sig}")
PY
}

# ==============================================================================================
# LEG A PART 1: Admission & security refusals (runs on every cluster)
# ==============================================================================================
echo "== Leg A Part 1: admission and security refusals =="

SAMPLE_KEY="$(compute_kernel_key vLLM 0.19 nvidia sm_90a rmsnorm 4096)"
SAMPLE_DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"
SAMPLE_DISPATCH="sha256:$(printf 'b%.0s' $(seq 64))"

# 1. ModelKernelArtifact with spec.source.configMap fails strict decoding.
strict_out="$(kubectl apply --dry-run=server --validate=strict -f - 2>&1 <<YAML || true
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelKernelArtifact
metadata:
  name: ${P}-bad-cm
  namespace: ${NS}
spec:
  source:
    configMap:
      name: forbidden-kernel-cm
  compatibility:
    engine: vLLM
    engineVersion: "0.19"
    manufacturer: nvidia
    architecture: sm_90a
    kernelKeys:
      - "${SAMPLE_KEY}"
YAML
)"
if printf '%s' "$strict_out" | grep -qiE 'strict decoding error|unknown field.*configMap|field not declared in schema'; then
  record PASS "Leg A1: ModelKernelArtifact spec.source.configMap fails strict decoding" \
    "$(printf '%s' "$strict_out" | tr '\n' ' ' | cut -c1-140)"
else
  record FAIL "Leg A1: ModelKernelArtifact spec.source.configMap fails strict decoding" \
    "got: $(printf '%s' "$strict_out" | tr '\n' ' ' | cut -c1-200)"
fi

# 2. ModelDeployment additionalVolumes[].configMap overlapping /var/lib/gpustack/kernels fails admission.
AT_ADMIT="${E2E_C38_ACCELERATOR_TYPE:-$(md_usable_accelerator_type || true)}"
AT_LINE=""
if [ -n "$AT_ADMIT" ]; then
  AT_LINE="    acceleratorType: ${AT_ADMIT}"
fi

if out="$(md_dryrun_refuses "ConfigMapKernelMountForbidden" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P}-bad-kvol
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: registry.k8s.io/pause:3.10
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
  - name: server
${AT_LINE}
    replicas: 1
    additionalVolumes:
    - mountPath: ${KERNEL_MOUNT_PATH}
      readOnly: true
      configMap:
        name: forbidden-kernels
YAML
)"; then
  record PASS "Leg A1: ModelDeployment ConfigMap mount at ${KERNEL_MOUNT_PATH} refused (ConfigMapKernelMountForbidden)" "$out"
else
  record FAIL "Leg A1: ModelDeployment ConfigMap mount at ${KERNEL_MOUNT_PATH} refused (ConfigMapKernelMountForbidden)" "$out"
fi

if out="$(md_dryrun_refuses "ConfigMapKernelMountForbidden" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P}-bad-kvol-sub
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: registry.k8s.io/pause:3.10
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
  - name: server
${AT_LINE}
    replicas: 1
    additionalVolumes:
    - mountPath: ${KERNEL_MOUNT_PATH}/override
      readOnly: true
      configMap:
        name: forbidden-kernels
YAML
)"; then
  record PASS "Leg A1: ModelDeployment overlapping ConfigMap mount refused (ConfigMapKernelMountForbidden)" "$out"
else
  record FAIL "Leg A1: ModelDeployment overlapping ConfigMap mount refused (ConfigMapKernelMountForbidden)" "$out"
fi

# 3. Tenant ModelKernelArtifact with spec.source.nodeStore fails admission (NodeStoreSourceForbidden).
if out="$(md_dryrun_refuses "NodeStoreSourceForbidden" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelKernelArtifact
metadata:
  name: ${P}-tenant-nodestore
  namespace: ${NS}
spec:
  source:
    nodeStore:
      manifestDigest: "${SAMPLE_DIGEST}"
      dispatchManifestDigest: "${SAMPLE_DISPATCH}"
      sizeBytes: 1024
      fileCount: 2
      seedNode: dummy-node
      forgerRef:
        name: dummy-forger
        uid: dummy-uid
  compatibility:
    engine: vLLM
    engineVersion: "0.19"
    manufacturer: nvidia
    architecture: sm_90a
    kernelKeys:
      - "${SAMPLE_KEY}"
YAML
)"; then
  record PASS "Leg A1: tenant ModelKernelArtifact spec.source.nodeStore refused (NodeStoreSourceForbidden)" "$out"
else
  record FAIL "Leg A1: tenant ModelKernelArtifact spec.source.nodeStore refused (NodeStoreSourceForbidden)" "$out"
fi

# ==============================================================================================
# LEG A PART 2: 2-node deterministic mock-Forge closed loop
# ==============================================================================================
echo "== Leg A Part 2: 2-node mock-Forge closed loop =="

run_leg_a_part2() {
  local workers_raw=() w1="" w2="" has_csi=0 has_lq=0 at_name="" at_res=""
  local total_cards=0 mock_active=0 hub_url="" repos_json=""
  local gc_art="" gc_dig="" gc_pod="" gc_released=0
  local mdf_name="${P}-mdf" md_name="${P}-md"
  local sentinel_cm="${P}-sglang" ctrl_cm="${P}-ctrl"
  local gen1_name="${mdf_name}-server-gen-1" gen2_name="${mdf_name}-server-gen-2" gen3_name="${mdf_name}-server-gen-3"
  local gen1_dig="" gen1_disp="" gen1_seed="" gen1_uid=""
  local gen2_dig="" gen2_disp=""
  local peer_node="" peer_pod="" peer_before=0 hub_before=0 peer_after=0 hub_after=0

  read -r -a workers_raw <<<"$(model_workers)"
  if [ "${#workers_raw[@]}" -lt 2 ]; then
    record SKIP "Leg A2: 2-node mock-Forge closed loop" "requires >= 2 schedulable worker nodes, found ${#workers_raw[@]}"
    return 0
  fi
  w1="${workers_raw[0]}"
  w2="${workers_raw[1]}"

  if kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
    has_csi=1
  fi
  if [ "$has_csi" -ne 1 ]; then
    record SKIP "Leg A2: 2-node mock-Forge closed loop" "CSIDriver model.csi.gpustack.ai is not installed"
    return 0
  fi

  if kubectl -n "$NS" get localqueue.kueue.x-k8s.io -o name 2>/dev/null | head -n 1 | grep -q .; then
    has_lq=1
  fi
  if [ "$has_lq" -ne 1 ]; then
    record SKIP "Leg A2: 2-node mock-Forge closed loop" "no entrance LocalQueue found in namespace ${NS}"
    return 0
  fi

  # Prefer Mock DM with 2+ cards per worker node so replicas=2 + ForgeSandbox + canary all fit.
  if mock_dm_detect_env "$SYSTEM_NS" && mock_dm_manufacturer_allowed nvidia; then
    mock_active=1
    if ! mock_dm_snapshot_cm "$SYSTEM_NS" "$MOCK_DM_CM" "$MOCK_CM_SNAPSHOT"; then
      record FAIL "Leg A2: snapshot Mock DM ConfigMap ${MOCK_DM_CM}" "could not read ConfigMap"
      return 0
    fi
    MOCK_DM_MUTATED=1
    local spec_2node
    spec_2node="$(cat <<YAML
default:
  groups: []
nodes:
  ${w1}:
    preset: nvidia-h100
    count: 2
  ${w2}:
    preset: nvidia-h100
    count: 2
YAML
)"
    mock_dm_apply_spec "$SYSTEM_NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$spec_2node"
    mock_dm_wait_devices "$SYSTEM_NS" "$w1" \
      '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length >= 2)' 90 >/dev/null || true
    mock_dm_wait_devices "$SYSTEM_NS" "$w2" \
      '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length >= 2)' 90 >/dev/null || true
    mock_dm_wait_it '.status.detail.manufacturer == "nvidia" and ((.status.accelerator.capacity // 0) >= 4)' 90 >/dev/null || true
  fi

  # Find a usable accelerated AcceleratorType whose entrance LocalQueue lives in NS.
  local candidate_at deleting inactive manu res ent
  while IFS='|' read -r candidate_at deleting inactive manu res ent; do
    [ -n "$candidate_at" ] || continue
    [ -z "$deleting" ] || continue
    [ "$inactive" = "true" ] && continue
    if [ -z "$manu" ] || [ -z "$res" ] || [ -z "$ent" ]; then
      continue
    fi
    if kubectl -n "$NS" get localqueue.kueue.x-k8s.io "$ent" >/dev/null 2>&1; then
      at_name="$candidate_at"
      at_res="$res"
      break
    fi
  done <<EOF
$(kubectl get acceleratortypes.worker.gpustack.ai \
  -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}|{.status.detail.resourceName}|{.status.entrance}{"\n"}{end}' \
  2>/dev/null || true)
EOF

  if [ -z "$at_name" ]; then
    record SKIP "Leg A2: 2-node mock-Forge closed loop" \
      "no active AcceleratorType with entrance LocalQueue in ${NS} (mock_dm=${mock_active})"
    return 0
  fi

  total_cards="$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
res, w1, w2 = sys.argv[1], sys.argv[2], sys.argv[3]
total = 0
for n in json.load(sys.stdin).get("items", []):
    name = n.get("metadata", {}).get("name")
    if name not in (w1, w2):
        continue
    val = (n.get("status", {}).get("allocatable") or {}).get(res, "0")
    try:
        total += int(float(val))
    except ValueError:
        pass
print(total)
' "$at_res" "$w1" "$w2" 2>/dev/null || echo 0)"
  if [ "${total_cards:-0}" -lt 3 ]; then
    record SKIP "Leg A2: 2-node mock-Forge closed loop" \
      "need >= 3 allocatable ${at_res} across ${w1} and ${w2}, found ${total_cards:-0}"
    return 0
  fi

  settings_save model-artifact-huggingface-endpoint model-artifact-delivery-mode \
    model-store-peer-sync model-store-high-watermark model-store-low-watermark
  SETTINGS_SAVED=1
  setting_set model-artifact-delivery-mode Node
  setting_set model-store-peer-sync true

  # Enforce restricted PSA on NSB for cross-namespace CSI denial; preserve NS PSA.
  kubectl create namespace "$NSB" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl label namespace "$NSB" --overwrite \
    pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/enforce-version=latest >/dev/null
  ORIG_PSS_ENFORCE="$(ns_label "$NS" pod-security.kubernetes.io/enforce)"
  ORIG_PSS_VERSION="$(ns_label "$NS" pod-security.kubernetes.io/enforce-version)"
  PSA_SNAPSHOT=1

  # Check Kind node docker access for tmpfs watermark-GC testing.
  if docker exec "$w1" true >/dev/null 2>&1 && docker exec "$w2" true >/dev/null 2>&1; then
    local n
    for n in "$w1" "$w2"; do
      if docker exec "$n" mount -t tmpfs -o size=100m tmpfs "$CACHE_ROOT" >/dev/null 2>&1; then
        TMPFS_NODES+=("$n")
        kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$n")" --wait=true --timeout=120s >/dev/null 2>&1 || true
      fi
    done
    if [ "${#TMPFS_NODES[@]}" -eq 2 ]; then
      for n in "$w1" "$w2"; do
        for _ in $(seq 1 40); do
          local tot
          tot="$(kubectl get nodemodelstores.worker.gpustack.ai "$n" -o jsonpath='{.status.capacity.totalBytes}' 2>/dev/null || true)"
          [ -n "$tot" ] && [ "$tot" -le $((128 << 20)) ] && break
          sleep 2
        done
      done
    fi
  fi

  # Deploy test hub for cross-class ModelArtifact check and unpinned GC victim tree.
  repos_json="$(python3 -c '
import json
mib = 1 << 20
print(json.dumps({
  "e2e/c38-gc-victim": {"files": {"config.json": {"size": 256}, "weights.bin": {"size": 25 * mib, "lfs": True}}},
}))')"
  hub_url="$(mh_deploy "$NS" "${P}-hub" "$repos_json")"
  setting_set model-artifact-huggingface-endpoint "$hub_url"
  settings_settle

  # Pre-populate an unpinned Hub tree on both workers when tmpfs GC is active so its 10m grace starts early.
  if [ "${#TMPFS_NODES[@]}" -eq 2 ]; then
    artifact "$NS" "${P}-gc-victim" e2e/c38-gc-victim "" 38
    gc_art="${P}-gc-victim"
    gc_dig="$(wait_resolved "$NS" "$gc_art" 120)"
    if [ -n "$gc_dig" ]; then
      local gc_uid
      gc_uid="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "$gc_art" -o jsonpath='{.metadata.uid}')"
      for n in "$w1" "$w2"; do
        gc_pod="${P}-gc-${n}"
        consumer "$NS" "$gc_pod" "$n" "$gc_art" "$gc_uid" "$gc_dig"
        CREATED_PODS+=("$gc_pod")
      done
      for n in "$w1" "$w2"; do
        pod_ready "$NS" "${P}-gc-${n}" 180 >/dev/null || true
      done
      for n in "$w1" "$w2"; do
        kubectl -n "$NS" delete pod "${P}-gc-${n}" --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
      done
      gc_released="$(date +%s)"
    fi
  fi

  # Deploy control ConfigMap and mock-sentinel SGLang package ConfigMap.
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${ctrl_cm}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
data:
  fail_op: ""
YAML

  cat >"${SCRATCH}/__init__.py" <<'PY'
# Mock sglang package for deterministic closed-loop kernel forging tests.
PY

  cat >"${SCRATCH}/launch_server.py" <<'PY'
import hashlib
import json
import os
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BOOT_ID = str(uuid.uuid4())
LOCK = threading.Lock()
PROBE_STATE = "idle"
PROBE_ID = ""
PROBE_PAYLOAD = None
LAST_OP_ID = ""
OBSERVED_GEN = None
APPLIED_DIGEST = ""
PAUSED = False
VALUES = {}


def parse_port(argv):
    for i, tok in enumerate(argv):
        if tok == "--port" and i + 1 < len(argv):
            try:
                return int(argv[i + 1])
            except ValueError:
                pass
        if tok.startswith("--port="):
            try:
                return int(tok.split("=", 1)[1])
            except ValueError:
                pass
    return 8000


def normalize_major_minor(ver):
    parts = ver.strip().lstrip("vV").split("+", 1)[0].split(".")
    if len(parts) >= 2:
        return f"{parts[0]}.{parts[1]}"
    return ver


def compute_key(engine, ver, manu, arch, op_family, hidden=4096):
    norm_ver = normalize_major_minor(ver)
    payload = json.dumps(
        {
            "constAxes": {"hidden_size": int(hidden)},
            "constants": {},
            "inputDtypes": ["bfloat16", "bfloat16"],
            "outputDtypes": ["bfloat16"],
        },
        separators=(",", ":"),
        sort_keys=True,
    )
    sig = hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]
    return f"{engine.lower()}/{norm_ver}/{manu.lower()}/{arch.lower()}/{op_family.lower()}/{sig}"


def read_fail_op():
    path = "/opt/mock-ctrl/fail_op"
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return fh.read().strip()
    except OSError:
        return ""


def read_mounted_kernels():
    kdir = os.environ.get("GPUSTACK_KERNEL_ARTIFACT_DIR", "")
    if not kdir:
        return [], "", ""
    mpath = os.path.join(kdir, "dispatch-manifest.json")
    if not os.path.isfile(mpath):
        return [], "", ""
    try:
        with open(mpath, "rb") as fh:
            raw = fh.read()
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        doc = json.loads(raw.decode("utf-8"))
        keys = [e.get("localKernelKey", "") for e in (doc.get("entries") or []) if e.get("localKernelKey")]
        fail_op = read_fail_op()
        if fail_op and any(f"/{fail_op}/" in k for k in keys):
            return [], digest, "unsupported_kernel_artifact"
        return keys, digest, ""
    except Exception:
        return [], "", "unsupported_kernel_artifact"


def next_target_op(loaded_keys):
    joined = ",".join(loaded_keys)
    if "/rmsnorm/" not in joined and "/rope_kv_append/" not in joined and "/activation_quant/" not in joined:
        return "rmsnorm"
    if "/rmsnorm/" in joined and "/rope_kv_append/" not in joined:
        return "rope_kv_append"
    return "activation_quant"


def build_probe_payload(req_body):
    loaded_keys, _, _ = read_mounted_kernels()
    op_family = next_target_op(loaded_keys)
    ver = req_body.get("engine_version") or "0.5"
    norm_ver = normalize_major_minor(ver)
    manu = req_body.get("manufacturer") or "nvidia"
    arch = req_body.get("architecture") or "sm_90a"
    key = compute_key("SGLang", norm_ver, manu, arch, op_family, 4096)
    gemm_key = f"sglang/{norm_ver}/{manu}/{arch}/gemm/0123456789abcdef"
    def_name = f"{op_family}_h4096"
    steps = int(req_body.get("steps") or 4)
    return {
        "schema_version": "flashinfer-trace/v1",
        "engine": "SGLang",
        "engine_version": ver,
        "manufacturer": manu,
        "architecture": arch,
        "definitions": [
            {
                "name": def_name,
                "op_type": op_family,
                "local_kernel_key": key,
                "axes": {
                    "num_tokens": {"type": "var"},
                    "hidden_size": {"type": "const", "value": 4096},
                },
                "inputs": {
                    "x": {"shape": ["num_tokens", "hidden_size"], "dtype": "bfloat16"},
                    "weight": {"shape": ["hidden_size"], "dtype": "bfloat16"},
                },
                "outputs": {
                    "out": {"shape": ["num_tokens", "hidden_size"], "dtype": "bfloat16"},
                },
                "reference": "def run(x, weight, eps=1e-6): return x * weight",
                "tags": ["sglang", op_family],
            }
        ],
        "workloads": [
            {
                "uuid": f"wl-{op_family}",
                "definition": def_name,
                "local_kernel_key": key,
                "axes": {"num_tokens": 128},
                "inputs": {"x": {"type": "random"}, "weight": {"type": "random"}},
                "observed_duration_us": 42.5,
                "call_count": 16,
            }
        ],
        "bottlenecks": [
            {
                "local_kernel_key": key,
                "op_family": op_family,
                "gpu_time_us": 680.0,
                "gpu_time_share_percent": 22.0,
                "call_count": 16,
            },
            {
                "local_kernel_key": gemm_key,
                "op_family": "gemm",
                "gpu_time_us": 2400.0,
                "gpu_time_share_percent": 65.0,
                "call_count": 16,
            },
        ],
        "diagnostics": {
            "steps_captured": steps,
            "filtered_excluded_count": 1,
            "wall_clock_seconds": 0.25,
        },
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        return

    def send_json(self, code, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def read_json(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        raw = self.rfile.read(length)
        return json.loads(raw.decode("utf-8"))

    def do_GET(self):
        loaded_keys, disp_digest, load_err = read_mounted_kernels()
        ready = (not PAUSED) and (load_err == "")
        if self.path == "/metrics":
            body = (
                "sglang:num_running_reqs 2.0\n"
                "sglang:num_queue_reqs 0.0\n"
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/gpustack/v1/live":
            self.send_json(200, {"alive": True, "boot_id": BOOT_ID, "paused": PAUSED})
            return
        if self.path == "/gpustack/v1/ready":
            resp = {
                "ready": ready,
                "boot_id": BOOT_ID,
                "observed_generation": OBSERVED_GEN,
                "applied_config_digest": APPLIED_DIGEST,
            }
            if disp_digest:
                resp["dispatch_manifest_digest"] = disp_digest
            if loaded_keys:
                resp["loaded_kernel_keys"] = loaded_keys
            if load_err:
                resp["kernel_load_error"] = load_err
            self.send_json(200 if ready else 503, resp)
            return
        if self.path == "/gpustack/v1/pause":
            self.send_json(200, {"paused": PAUSED, "boot_id": BOOT_ID})
            return
        if self.path == "/gpustack/v1/runtime_config":
            rank = {
                "rank_id": 0,
                "boot_id": BOOT_ID,
                "membership_epoch": 1,
                "applied_digest": APPLIED_DIGEST,
                "values": VALUES,
            }
            if disp_digest:
                rank["dispatch_manifest_digest"] = disp_digest
            if loaded_keys:
                rank["loaded_kernel_keys"] = loaded_keys
            if load_err:
                rank["kernel_load_error"] = load_err
            resp = {
                "engine": "sglang",
                "boot_id": BOOT_ID,
                "membership_epoch": 1,
                "observed_generation": OBSERVED_GEN,
                "applied_config_digest": APPLIED_DIGEST,
                "last_operation_id": LAST_OP_ID,
                "ready": ready,
                "paused": PAUSED,
                "supported_fields": [],
                "unsupported_fields": [],
                "boot_defaults": {},
                "ranks": [rank],
                "values": VALUES,
            }
            if disp_digest:
                resp["dispatch_manifest_digest"] = disp_digest
            if loaded_keys:
                resp["loaded_kernel_keys"] = loaded_keys
            if load_err:
                resp["kernel_load_error"] = load_err
            self.send_json(200, resp)
            return
        if self.path == "/gpustack/v1/probe":
            with LOCK:
                resp = {"boot_id": BOOT_ID, "state": PROBE_STATE}
                if PROBE_ID:
                    resp["probe_id"] = PROBE_ID
                if PROBE_STATE == "completed" and PROBE_PAYLOAD is not None:
                    resp["payload"] = PROBE_PAYLOAD
                    resp["diagnostics"] = PROBE_PAYLOAD.get("diagnostics", {})
            self.send_json(200, resp)
            return
        self.send_json(404, {"error": "not_found"})

    def do_POST(self):
        global PROBE_STATE, PROBE_ID, PROBE_PAYLOAD, LAST_OP_ID, OBSERVED_GEN, APPLIED_DIGEST, PAUSED
        req = self.read_json()
        if self.path == "/gpustack/v1/ack":
            OBSERVED_GEN = req.get("observed_generation")
            APPLIED_DIGEST = req.get("applied_config_digest", "")
            self.send_json(200, {
                "ready": True,
                "boot_id": BOOT_ID,
                "observed_generation": OBSERVED_GEN,
                "applied_config_digest": APPLIED_DIGEST,
            })
            return
        if self.path == "/gpustack/v1/pause":
            PAUSED = bool(req.get("paused", False))
            self.send_json(200, {"paused": PAUSED, "boot_id": BOOT_ID})
            return
        if self.path == "/gpustack/v1/runtime_config":
            LAST_OP_ID = req.get("operation_id", "")
            OBSERVED_GEN = req.get("generation")
            APPLIED_DIGEST = req.get("applied_config_digest", "")
            self.do_GET()
            return
        if self.path == "/gpustack/v1/probe":
            if req.get("expected_boot_id") != BOOT_ID:
                self.send_json(409, {
                    "error": "boot_id_mismatch",
                    "boot_id": BOOT_ID,
                    "expected_boot_id": req.get("expected_boot_id", ""),
                })
                return
            with LOCK:
                PROBE_ID = str(uuid.uuid4())
                PROBE_PAYLOAD = build_probe_payload(req)
                PROBE_STATE = "completed"
                resp = {
                    "boot_id": BOOT_ID,
                    "state": PROBE_STATE,
                    "probe_id": PROBE_ID,
                    "steps": int(req.get("steps") or 4),
                    "diagnostics": PROBE_PAYLOAD.get("diagnostics", {}),
                    "payload": PROBE_PAYLOAD,
                }
            self.send_json(200, resp)
            return
        self.send_json(404, {"error": "not_found"})

    def do_DELETE(self):
        global PROBE_STATE, PROBE_PAYLOAD
        if self.path == "/gpustack/v1/probe":
            with LOCK:
                PROBE_STATE = "aborted"
                PROBE_PAYLOAD = None
            self.send_json(200, {"boot_id": BOOT_ID, "state": "aborted"})
            return
        self.send_json(404, {"error": "not_found"})


if __name__ == "__main__":
    port = parse_port(sys.argv[1:])
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    server.serve_forever()
PY

  kubectl -n "$NS" create configmap "$sentinel_cm" \
    --from-file=__init__.py="${SCRATCH}/__init__.py" \
    --from-file=launch_server.py="${SCRATCH}/launch_server.py" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$NS" label configmap "$sentinel_cm" "$LABEL" --overwrite >/dev/null

  # Deploy 2-replica ModelDeployment using mock-sentinel SGLang image (no role.command override).
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${md_name}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
spec:
  engine:
    name: SGLang
    version: "0.5.22"
    image: ${MOCK_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
    - name: server
      kind: Server
      replicas: 2
      acceleratorType: ${at_name}
      image: ${MOCK_IMAGE}
      resources:
        accelerator: "1"
      env:
        - name: PYTHONPATH
          value: /opt/mock-pkg
      additionalVolumes:
        - mountPath: /opt/mock-pkg/sglang
          readOnly: true
          configMap:
            name: ${sentinel_cm}
        - mountPath: /opt/mock-ctrl
          readOnly: true
          configMap:
            name: ${ctrl_cm}
YAML
  CREATED_MDS+=("$md_name")

  local md_phase=""
  for _ in $(seq 1 $((BOUND / 3))); do
    md_phase="$(kubectl -n "$NS" get "$MD_RES" "$md_name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$md_phase" = "Ready" ] && break
    sleep 3
  done
  if [ "$md_phase" != "Ready" ]; then
    record FAIL "Leg A2: 2-replica mock-sentinel ModelDeployment reached Ready" "phase=${md_phase:-<none>}"
    return 0
  fi
  record PASS "Leg A2: 2-replica mock-sentinel ModelDeployment reached Ready" "${NS}/${md_name}"

  # Create Active Mock-mode ModelDeploymentForger (cooldown 60s, maxRetainedGenerations 1).
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentForger
metadata:
  name: ${mdf_name}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
spec:
  targetRef:
    name: ${md_name}
  mode: Active
  roles:
    - name: server
      maxKernelsPerGeneration: 4
      minSpeedupRatio: "1.05"
  probe:
    steps: 4
    cooldownSeconds: 60
    minActiveRequests: 1
  sandbox:
    runnerMode: Mock
    activeDeadlineSeconds: 600
  rollout:
    maxRetainedGenerations: 1
    failureTimeoutSeconds: 90
YAML
  CREATED_MDFS+=("$mdf_name")

  # Wait for ForgeSandbox Pod observation (or gen-1 artifact creation).
  local saw_sandbox=0 saw_forger_binding=0 gen1_created=0
  for _ in $(seq 1 $((BOUND / 2))); do
    if [ "$saw_forger_binding" -eq 0 ]; then
      if [ -n "$(kubectl -n "$NS" get "$MD_RES" "$md_name" -o jsonpath='{.metadata.annotations.modeldeployment\.gpustack\.ai/forger-binding}' 2>/dev/null || true)" ]; then
        saw_forger_binding=1
      fi
    fi
    if [ "$saw_sandbox" -eq 0 ]; then
      if kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${mdf_name},app.kubernetes.io/component=forge-sandbox" -o name 2>/dev/null | grep -q .; then
        saw_sandbox=1
      fi
    fi
    if kubectl -n "$NS" get "$MKA_RES" "$gen1_name" >/dev/null 2>&1; then
      gen1_created=1
      break
    fi
    sleep 2
  done

  if [ "$saw_forger_binding" -eq 1 ]; then
    record PASS "Leg A2: ModelDeploymentForger acquired forger-binding on target" "${NS}/${md_name}"
  else
    record FAIL "Leg A2: ModelDeploymentForger acquired forger-binding on target" "annotation absent"
  fi

  if [ "$gen1_created" -eq 1 ]; then
    record PASS "Leg A2: gen-1 ForgeSandbox (model-forger mock -> verify -> hold) ingested ModelKernelArtifact" "${NS}/${gen1_name}"
  else
    record FAIL "Leg A2: gen-1 ForgeSandbox (model-forger mock -> verify -> hold) ingested ModelKernelArtifact" \
      "saw_sandbox=${saw_sandbox}, ${gen1_name} not created within ${BOUND}s"
    return 0
  fi

  # Verify gen-1 provenance (SeedNode, Resolved=True, digestSource=Forge, NodeModelStore Source=Forge, spec.pinned).
  for _ in $(seq 1 40); do
    if [ "$(kubectl -n "$NS" get "$MKA_RES" "$gen1_name" -o jsonpath='{.status.conditions[?(@.type=="Resolved")].status}' 2>/dev/null || true)" = "True" ]; then
      break
    fi
    sleep 2
  done
  gen1_dig="$(kubectl -n "$NS" get "$MKA_RES" "$gen1_name" -o jsonpath='{.status.resolved.manifestDigest}' 2>/dev/null || true)"
  gen1_disp="$(kubectl -n "$NS" get "$MKA_RES" "$gen1_name" -o jsonpath='{.status.resolved.dispatchManifestDigest}' 2>/dev/null || true)"
  gen1_seed="$(kubectl -n "$NS" get "$MKA_RES" "$gen1_name" -o jsonpath='{.spec.source.nodeStore.seedNode}' 2>/dev/null || true)"
  gen1_uid="$(kubectl -n "$NS" get "$MKA_RES" "$gen1_name" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  local gen1_dsrc=""
  gen1_dsrc="$(kubectl -n "$NS" get "$MKA_RES" "$gen1_name" -o jsonpath='{.status.resolved.digestSource}' 2>/dev/null || true)"

  local seed_nms_entry="" seed_pinned=""
  seed_nms_entry="$(kubectl get nodemodelstores.worker.gpustack.ai "$gen1_seed" \
    -o jsonpath="{range .status.models[?(@.digest=='${gen1_dig}')]}{.state}|{.source}{end}" 2>/dev/null || true)"
  seed_pinned="$(kubectl get nodemodelstores.worker.gpustack.ai "$gen1_seed" \
    -o jsonpath='{.spec.pinned[*]}' 2>/dev/null || true)"

  if [ -n "$gen1_dig" ] && [ "$gen1_dsrc" = "Forge" ] && [ "$seed_nms_entry" = "Ready|Forge" ]; then
    record PASS "Leg A2: seed NodeModelStore published gen-1 with source=Forge" "${gen1_seed}: ${gen1_dig:0:19}..."
  else
    record FAIL "Leg A2: seed NodeModelStore published gen-1 with source=Forge" \
      "digest=${gen1_dig} dsrc=${gen1_dsrc} nms=${seed_nms_entry}"
  fi

  if printf ' %s ' "$seed_pinned" | grep -qF " ${gen1_dig} "; then
    record PASS "Leg A2: NodeModelStore.spec.pinned includes gen-1 digest" "${gen1_seed}"
  else
    record FAIL "Leg A2: NodeModelStore.spec.pinned includes gen-1 digest" "pinned=${seed_pinned}"
  fi

  # Cross-namespace & cross-class CSI mount denials against the published Kind: Kernel tree on gen1_seed.
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${P}-cross-ns
  namespace: ${NSB}
  labels:
    e2e.gpustack.ai/case: "38"
    e2e.gpustack.ai/consumer: "true"
spec:
  nodeName: ${gen1_seed}
  terminationGracePeriodSeconds: 1
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: c
      image: ${MOCK_IMAGE}
      command: ["sleep", "300"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: k
          mountPath: /kernels
          readOnly: true
  volumes:
    - name: k
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes:
          artifactKind: ModelKernelArtifact
          artifact: "${gen1_name}"
          artifactUID: "${gen1_uid}"
          manifestDigest: "${gen1_dig}"
YAML

  # ModelArtifact anchored to gen1_dig (Kernel tree) must fail with PermissionDenied on mount.
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata:
  name: ${P}-cross-class-ma
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
spec:
  expectedDigest: "${gen1_dig}"
  source:
    huggingFace:
      repository: e2e/c38-gc-victim
      revision: main
YAML
  local cc_dig="" cc_uid=""
  cc_dig="$(wait_resolved "$NS" "${P}-cross-class-ma" 60)"
  cc_uid="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-cross-class-ma" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  if [ -n "$cc_dig" ] && [ -n "$cc_uid" ]; then
    consumer "$NS" "${P}-cross-class-pod" "$gen1_seed" "${P}-cross-class-ma" "$cc_uid" "$gen1_dig"
    CREATED_PODS+=("${P}-cross-class-pod")
  fi

  sleep 20
  local cns_phase="" cns_ev=""
  cns_phase="$(kubectl -n "$NSB" get pod "${P}-cross-ns" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  cns_ev="$(pod_mount_events "$NSB" "${P}-cross-ns" | tr '\n' ' ')"
  if [ "$cns_phase" != "Running" ] && printf '%s' "$cns_ev" | grep -q "PermissionDenied" && printf '%s' "$cns_ev" | grep -q "has no such ModelKernelArtifact"; then
    record PASS "Leg A1: cross-namespace Kernel CSI mount refused with PermissionDenied" "${NSB}/${P}-cross-ns"
  else
    record FAIL "Leg A1: cross-namespace Kernel CSI mount refused with PermissionDenied" "phase=${cns_phase} ev=${cns_ev:0:180}"
  fi

  if [ -n "$cc_dig" ]; then
    local ccl_phase="" ccl_ev=""
    ccl_phase="$(kubectl -n "$NS" get pod "${P}-cross-class-pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    ccl_ev="$(pod_mount_events "$NS" "${P}-cross-class-pod" | tr '\n' ' ')"
    if [ "$ccl_phase" != "Running" ] && printf '%s' "$ccl_ev" | grep -q "PermissionDenied" && printf '%s' "$ccl_ev" | grep -q "not a model artifact"; then
      record PASS "Leg A1: cross-class ModelArtifact mounting Kernel tree refused with PermissionDenied" "${NS}/${P}-cross-class-pod"
    else
      record FAIL "Leg A1: cross-class ModelArtifact mounting Kernel tree refused with PermissionDenied" "phase=${ccl_phase} ev=${ccl_ev:0:180}"
    fi
    kubectl -n "$NS" delete pod "${P}-cross-class-pod" --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1 || true
  else
    record FAIL "Leg A1: cross-class ModelArtifact resolved expectedDigest" "${P}-cross-class-ma"
  fi
  kubectl -n "$NSB" delete pod "${P}-cross-ns" --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1 || true

  # Wait for gen-1 readiness-gated promotion onto ModelDeployment (both replicas Ready, gen-1 Promoted).
  local saw_slot=0 gen1_promoted=0
  for _ in $(seq 1 $((BOUND / 2))); do
    if kubectl -n "$NS" get "$MD_RES" "$md_name" -o jsonpath='{.metadata.annotations}' 2>/dev/null | grep -q 'replacement-slots'; then
      saw_slot=1
    fi
    local outcome=""
    outcome="$(kubectl -n "$NS" get "$MDF_RES" "$mdf_name" \
      -o jsonpath='{.status.roles[?(@.name=="server")].generations[0].outcome}' 2>/dev/null || true)"
    if [ "$outcome" = "Promoted" ]; then
      gen1_promoted=1
      break
    fi
    sleep 2
  done

  if [ "$gen1_promoted" -eq 1 ]; then
    record PASS "Leg A2: gen-1 promoted across 2-replica ModelDeployment (saw_slot=${saw_slot})" "${NS}/${gen1_name}"
  else
    record FAIL "Leg A2: gen-1 promoted across 2-replica ModelDeployment" "timed out waiting for Promoted"
    return 0
  fi

  # Verify read-only /var/lib/gpustack/kernels/dispatch-manifest.json mount and sentinel runtime_config on replaced Pods.
  local pod0="" ro_check=""
  pod0="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md_name},app.kubernetes.io/component=server" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "$pod0" ]; then
    ro_check="$(kubectl -n "$NS" exec "$pod0" -c main -- python3 - "$gen1_disp" <<'PY' 2>/dev/null || echo "EXEC_ERR"
import json, os, sys, urllib.request
want_disp = sys.argv[1]
p = "/var/lib/gpustack/kernels/dispatch-manifest.json"
assert os.path.isfile(p), "missing dispatch-manifest.json"
doc = json.load(open(p))
assert doc.get("schemaVersion") == "gpustack-forge-dispatch/v1", doc.get("schemaVersion")
try:
    open("/var/lib/gpustack/kernels/.rw-probe", "w").close()
    print("WRITABLE")
    sys.exit(0)
except OSError:
    pass
rc = json.loads(urllib.request.urlopen("http://127.0.0.1:8000/gpustack/v1/runtime_config", timeout=5).read().decode())
assert rc.get("dispatch_manifest_digest") == want_disp, (rc.get("dispatch_manifest_digest"), want_disp)
assert len(rc.get("loaded_kernel_keys") or []) > 0, "empty loaded_kernel_keys"
print("OK")
PY
)"
  fi
  if [ "$ro_check" = "OK" ]; then
    record PASS "Leg A2: replaced Pod mounts read-only ${KERNEL_MOUNT_PATH}/dispatch-manifest.json and reports loaded_kernel_keys" "${pod0}"
  else
    record FAIL "Leg A2: replaced Pod mounts read-only ${KERNEL_MOUNT_PATH}/dispatch-manifest.json and reports loaded_kernel_keys" "result=${ro_check:-empty}"
  fi

  # Determine peer node (!= gen1_seed) and verify Peer Sync (Source=Peer, zero Hub bytes).
  if [ "$gen1_seed" = "$w1" ]; then
    peer_node="$w2"
  else
    peer_node="$w1"
  fi
  local peer_nms=""
  peer_nms="$(kubectl get nodemodelstores.worker.gpustack.ai "$peer_node" \
    -o jsonpath="{range .status.models[?(@.digest=='${gen1_dig}')]}{.state}|{.source}{end}" 2>/dev/null || true)"
  if [ "$peer_nms" != "Ready|Peer" ]; then
    # Restart peer model-manager Pod to capture clean zero-Hub Peer Sync counters on a fresh mount.
    kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$peer_node")" --wait=true --timeout=120s >/dev/null 2>&1 || true
    for _ in $(seq 1 30); do
      local pp=""
      pp="$(plugin_pod "$peer_node")"
      [ -n "$pp" ] && [ "$(kubectl -n "$SYSTEM_NS" get pod "$pp" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = "true" ] && break
      sleep 3
    done
    peer_before="$(plugin_metric "$peer_node" 'gpustack_model_manager_download_bytes_total{source="peer"}')"
    hub_before="$(plugin_metric "$peer_node" 'gpustack_model_manager_download_bytes_total{source="hub"}')"
    peer_pod="${P}-peer-kernel"
    kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${peer_pod}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
    e2e.gpustack.ai/consumer: "true"
spec:
  nodeName: ${peer_node}
  terminationGracePeriodSeconds: 1
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: c
      image: ${MOCK_IMAGE}
      command: ["python3", "-c", "import os,time; assert os.path.isfile('/kernels/dispatch-manifest.json'); print('OK', flush=True); time.sleep(300)"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: k
          mountPath: /kernels
          readOnly: true
  volumes:
    - name: k
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes:
          artifactKind: ModelKernelArtifact
          artifact: "${gen1_name}"
          artifactUID: "${gen1_uid}"
          manifestDigest: "${gen1_dig}"
YAML
    CREATED_PODS+=("$peer_pod")
    pod_ready "$NS" "$peer_pod" 180 >/dev/null || true
    peer_after="$(plugin_metric "$peer_node" 'gpustack_model_manager_download_bytes_total{source="peer"}')"
    hub_after="$(plugin_metric "$peer_node" 'gpustack_model_manager_download_bytes_total{source="hub"}')"
    peer_nms="$(kubectl get nodemodelstores.worker.gpustack.ai "$peer_node" \
      -o jsonpath="{range .status.models[?(@.digest=='${gen1_dig}')]}{.state}|{.source}{end}" 2>/dev/null || true)"
    kubectl -n "$NS" delete pod "$peer_pod" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
    if [ "$peer_nms" = "Ready|Peer" ] && [ "${peer_after:-0}" -gt "${peer_before:-0}" ] && [ "${hub_after:-0}" = "${hub_before:-0}" ]; then
      record PASS "Leg A2: peer node zero-Hub /peer/v1/trees/{hex} Peer Sync preserved Kind=Kernel" \
        "${peer_node}: peer=$((peer_after - peer_before))B hub=0B"
    else
      record FAIL "Leg A2: peer node zero-Hub /peer/v1/trees/{hex} Peer Sync preserved Kind=Kernel" \
        "nms=${peer_nms} peer=${peer_before:-0}->${peer_after:-0} hub=${hub_before:-0}->${hub_after:-0}"
    fi
  else
    record PASS "Leg A2: peer node zero-Hub /peer/v1/trees/{hex} Peer Sync preserved Kind=Kernel" "${peer_node}: ${peer_nms}"
  fi

  # Watermark-GC survival test when tmpfs GC is active on Kind workers.
  if [ "${#TMPFS_NODES[@]}" -eq 2 ] && [ -n "$gc_dig" ]; then
    local left=$((gc_released + 620 - $(date +%s)))
    if [ "$left" -gt 0 ]; then
      echo "[case-38] waiting ${left}s for NodeModelStore GC grace window before watermark test"
      sleep "$left"
    fi
    setting_set model-store-low-watermark 5
    setting_set model-store-high-watermark 10
    local victim_evicted=0 kernel_kept=0
    for _ in $(seq 1 45); do
      local vs="" ks=""
      vs="$(store_state "$gen1_seed" "$gc_dig")"
      ks="$(store_state "$gen1_seed" "$gen1_dig")"
      [ -z "$vs" ] && [ "${ks%%|*}" = "Ready" ] && { victim_evicted=1; kernel_kept=1; break; }
      sleep 3
    done
    setting_unset model-store-low-watermark
    setting_unset model-store-high-watermark
    if [ "$victim_evicted" -eq 1 ] && [ "$kernel_kept" -eq 1 ]; then
      record PASS "Leg A2: watermark GC evicted unpinned Hub tree while pinned Forge kernel survived" "${gen1_seed}"
    else
      record FAIL "Leg A2: watermark GC evicted unpinned Hub tree while pinned Forge kernel survived" \
        "victim=$(store_state "$gen1_seed" "$gc_dig") kernel=$(store_state "$gen1_seed" "$gen1_dig")"
    fi
  else
    record SKIP "Leg A2: watermark-GC survival test" "requires docker access to Kind worker nodes for tmpfs mount"
  fi

  # Wait for gen-2 promotion & gen-1 pruning/unpinning (maxRetainedGenerations=1, cooldown=60s).
  local gen2_promoted=0
  for _ in $(seq 1 $((BOUND / 2))); do
    local g2_out=""
    g2_out="$(kubectl -n "$NS" get "$MDF_RES" "$mdf_name" \
      -o jsonpath='{.status.roles[?(@.name=="server")].generations[?(@.generation==2)].outcome}' 2>/dev/null || true)"
    if [ "$g2_out" = "Promoted" ]; then
      gen2_promoted=1
      break
    fi
    sleep 3
  done

  if [ "$gen2_promoted" -eq 1 ]; then
    gen2_dig="$(kubectl -n "$NS" get "$MKA_RES" "$gen2_name" -o jsonpath='{.status.resolved.manifestDigest}' 2>/dev/null || true)"
    gen2_disp="$(kubectl -n "$NS" get "$MKA_RES" "$gen2_name" -o jsonpath='{.status.resolved.dispatchManifestDigest}' 2>/dev/null || true)"
    local gen1_gone=0 gen1_unpinned=0 gen2_pinned=0
    for _ in $(seq 1 30); do
      if ! kubectl -n "$NS" get "$MKA_RES" "$gen1_name" >/dev/null 2>&1; then
        gen1_gone=1
        local pinned_now=""
        pinned_now="$(kubectl get nodemodelstores.worker.gpustack.ai "$gen1_seed" -o jsonpath='{.spec.pinned[*]}' 2>/dev/null || true)"
        if ! printf ' %s ' "$pinned_now" | grep -qF " ${gen1_dig} " && printf ' %s ' "$pinned_now" | grep -qF " ${gen2_dig} "; then
          gen1_unpinned=1
          gen2_pinned=1
          break
        fi
      fi
      sleep 2
    done
    if [ "$gen1_gone" -eq 1 ] && [ "$gen1_unpinned" -eq 1 ] && [ "$gen2_pinned" -eq 1 ]; then
      record PASS "Leg A2: gen-2 promoted, retired gen-1 pruned and unpinned from NodeModelStore" "${gen2_name} (${gen2_disp:0:19}...)"
    else
      record FAIL "Leg A2: gen-2 promoted, retired gen-1 pruned and unpinned from NodeModelStore" \
        "gen1_gone=${gen1_gone} gen1_unpinned=${gen1_unpinned} gen2_pinned=${gen2_pinned}"
    fi
  else
    record FAIL "Leg A2: gen-2 promoted and pruned gen-1" "timed out waiting for gen-2 Promoted"
    return 0
  fi

  # Arm simulated canary failure for gen-3 (activation_quant -> unsupported_kernel_artifact).
  kubectl -n "$NS" patch configmap "$ctrl_cm" --type=merge -p '{"data":{"fail_op":"activation_quant"}}' >/dev/null

  # Wait for gen-3 to promote, fail canary readiness with unsupported_kernel_artifact, auto-rollback to gen-2, and quarantine gen-3.
  local rolled_back=0
  for _ in $(seq 1 $((BOUND / 2))); do
    local g3_out=""
    g3_out="$(kubectl -n "$NS" get "$MDF_RES" "$mdf_name" \
      -o jsonpath='{.status.roles[?(@.name=="server")].generations[?(@.generation==3)].outcome}' 2>/dev/null || true)"
    if [ "$g3_out" = "RolledBack" ]; then
      rolled_back=1
      break
    fi
    sleep 3
  done

  local cur_ref="" g3_quarantined="" g3_qreason="" mdf_ready_reason=""
  cur_ref="$(kubectl -n "$NS" get "$MD_RES" "$md_name" -o jsonpath='{.spec.roles[0].kernelArtifactRef.name}' 2>/dev/null || true)"
  g3_quarantined="$(kubectl -n "$NS" get "$MKA_RES" "$gen3_name" -o jsonpath='{.status.quarantined}' 2>/dev/null || true)"
  g3_qreason="$(kubectl -n "$NS" get "$MKA_RES" "$gen3_name" -o jsonpath='{.status.quarantineReason}' 2>/dev/null || true)"
  mdf_ready_reason="$(kubectl -n "$NS" get "$MDF_RES" "$mdf_name" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)"

  if [ "$rolled_back" -eq 1 ] && [ "$cur_ref" = "$gen2_name" ] && [ "$g3_quarantined" = "true" ]; then
    record PASS "Leg A2: canary failure auto-rolled back to gen-2 and quarantined gen-3" \
      "ref=${cur_ref} quarantined=${g3_quarantined} reason=${g3_qreason} ready=${mdf_ready_reason}"
  else
    record FAIL "Leg A2: canary failure auto-rolled back to gen-2 and quarantined gen-3" \
      "rolled_back=${rolled_back} ref=${cur_ref} quarantined=${g3_quarantined} reason=${g3_qreason}"
  fi

  # Verify quarantine persists across ModelDeploymentForger deletion.
  kubectl -n "$NS" delete "$MDF_RES" "$mdf_name" --wait=true --timeout=90s >/dev/null 2>&1 || true
  local q_after=""
  q_after="$(kubectl -n "$NS" get "$MKA_RES" "$gen3_name" -o jsonpath='{.status.quarantined}' 2>/dev/null || true)"
  if [ "$q_after" = "true" ]; then
    record PASS "Leg A2: ModelKernelArtifact status.quarantined=true persists after forger deletion" "${gen3_name}"
  else
    record FAIL "Leg A2: ModelKernelArtifact status.quarantined=true persists after forger deletion" "got=${q_after:-<none>}"
  fi

  # Clean up Leg A2 workloads before Leg B.
  kubectl -n "$NS" delete "$MD_RES" "$md_name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$NS" "$md_name" || true
  for _ in $(seq 1 30); do
    kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md_name}" -o name 2>/dev/null | grep -q . || break
    sleep 2
  done
  if [ "$MOCK_DM_MUTATED" -eq 1 ]; then
    mock_dm_restore_cm "$SYSTEM_NS" "$MOCK_DM_CM" "$MOCK_CM_SNAPSHOT" >/dev/null 2>&1 || true
    MOCK_DM_MUTATED=0
  fi
}

run_leg_a_part2

# ==============================================================================================
# LEG B: Live GPU qualification (auto-skips without real NVIDIA/AMD/Ascend GPU + engine image)
# ==============================================================================================
echo "== Leg B: live GPU qualification =="

run_leg_b_live_gpu() {
  local real_at="" real_manu="" real_res="" gpu_nodes=0 gpu_total=0
  local art_rev="" engine_name="" engine_img=""
  local probe_pod="${P}-live-probe" md_live="${P}-live-md" mdf_live="${P}-live-mdf"

  # Select a non-mock accelerated AcceleratorType from nvidia, amd, or ascend.
  local mock_mfrs=""
  mock_mfrs="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null | python3 -c '
import json, sys
doc = json.load(sys.stdin)
mfrs = set()
for d in doc.get("items", []):
    if (d.get("metadata", {}).get("annotations") or {}).get("device.gpustack.ai/mock") == "true":
        for g in (d.get("spec") or {}).get("groups", []):
            if g.get("manufacturer"):
                mfrs.add(g["manufacturer"])
print(",".join(sorted(mfrs)))
' 2>/dev/null || true)"

  if [ -n "${E2E_C38_ACCELERATOR_TYPE:-}" ]; then
    real_at="${E2E_C38_ACCELERATOR_TYPE}"
    real_manu="$(kubectl get acceleratortype "$real_at" -o jsonpath='{.status.detail.manufacturer}' 2>/dev/null || true)"
    real_res="$(kubectl get acceleratortype "$real_at" -o jsonpath='{.status.detail.resourceName}' 2>/dev/null || true)"
  else
    local m
    for m in nvidia amd ascend; do
      case ",${mock_mfrs}," in *",$m,"*) continue ;; esac
      real_at="$(md_usable_type_for "$m" || true)"
      if [ -n "$real_at" ]; then
        real_manu="$m"
        real_res="$(kubectl get acceleratortype "$real_at" -o jsonpath='{.status.detail.resourceName}' 2>/dev/null || true)"
        break
      fi
    done
  fi

  if [ -z "$real_at" ] || [ -z "$real_manu" ]; then
    record SKIP "Leg B: live GPU qualification" "no real NVIDIA/AMD/Ascend AcceleratorType found"
    return 0
  fi

  local counts=""
  counts="$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
res = sys.argv[1]
nodes, total = 0, 0
for n in json.load(sys.stdin).get("items", []):
    conds = {c["type"]: c["status"] for c in n.get("status", {}).get("conditions", [])}
    if conds.get("Ready") != "True":
        continue
    val = (n.get("status", {}).get("allocatable") or {}).get(res, "0")
    try:
        v = int(float(val))
    except ValueError:
        v = 0
    if v >= 1:
        nodes += 1
        total += v
print(f"{nodes} {total}")
' "$real_res" 2>/dev/null || echo "0 0")"
  gpu_nodes="${counts%% *}"
  gpu_total="${counts##* }"

  if [ "${gpu_nodes:-0}" -lt 2 ] || [ "${gpu_total:-0}" -lt 3 ]; then
    record SKIP "Leg B: live GPU qualification" \
      "need >= 2 GPU nodes and >= 3 allocatable ${real_res}, found nodes=${gpu_nodes:-0} total=${gpu_total:-0}"
    return 0
  fi

  if ! kubectl -n "$NS" get modelartifact "$LIVE_ARTIFACT" >/dev/null 2>&1; then
    record SKIP "Leg B: live GPU qualification" "pre-warmed ModelArtifact ${NS}/${LIVE_ARTIFACT} not found"
    return 0
  fi
  art_rev="$(kubectl -n "$NS" get modelartifact "$LIVE_ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
  if [ -z "$art_rev" ]; then
    record SKIP "Leg B: live GPU qualification" "ModelArtifact ${NS}/${LIVE_ARTIFACT} has empty resolved.revision"
    return 0
  fi

  if [ -z "$LIVE_RUNNER_IMAGE" ]; then
    record SKIP "Leg B: live GPU qualification" "E2E_C38_RUNNER_IMAGE is unset"
    return 0
  fi

  if [ -n "$LIVE_VLLM_IMAGE" ]; then
    engine_name="vLLM"
    engine_img="$LIVE_VLLM_IMAGE"
  elif [ -n "$LIVE_SGLANG_IMAGE" ]; then
    engine_name="SGLang"
    engine_img="$LIVE_SGLANG_IMAGE"
  elif [ -n "${E2E_C38_ENGINE_IMAGE:-}" ]; then
    engine_name="vLLM"
    engine_img="${E2E_C38_ENGINE_IMAGE}"
  else
    record SKIP "Leg B: live GPU qualification" "neither E2E_C38_VLLM_IMAGE nor E2E_C38_SGLANG_IMAGE is set"
    return 0
  fi

  # Deploy shared probe Pod for continuous load + P99 ITL measurement.
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${probe_pod}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
      command: ["sh", "-c", "sleep 7200"]
YAML
  CREATED_PODS+=("$probe_pod")
  kubectl -n "$NS" wait --for=condition=Ready "pod/${probe_pod}" --timeout=120s >/dev/null 2>&1 || true

  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${md_live}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
spec:
  model:
    name: ${LIVE_MODEL}
    artifactRef:
      name: ${LIVE_ARTIFACT}
  engine:
    name: ${engine_name}
    image: ${engine_img}
  roles:
    - name: server
      kind: Server
      replicas: 2
      acceleratorType: ${real_at}
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len=4096
        - --gpu-memory-utilization=0.80
YAML
  CREATED_MDS+=("$md_live")

  local phase=""
  for _ in $(seq 1 $((READY_BOUND / 5))); do
    phase="$(kubectl -n "$NS" get "$MD_RES" "$md_live" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && break
    sleep 5
  done
  if [ "$phase" != "Ready" ]; then
    record FAIL "Leg B: live ${engine_name} deployment reached Ready" "phase=${phase:-<none>}"
    return 0
  fi
  record PASS "Leg B: live ${engine_name} 2-node deployment reached Ready" "${NS}/${md_live}"

  local endpoint=""
  endpoint="$(kubectl -n "$NS" get "$MD_RES" "$md_live" -o jsonpath='{.status.endpoint}' 2>/dev/null || true)"

  # Measure baseline streaming ITL P99 before attaching ModelDeploymentForger.
  local base_itl=""
  base_itl="$(kubectl -n "$NS" exec "$probe_pod" -- sh -c "
    i=0
    while [ \$i -lt 5 ]; do
      curl -fsS -o /dev/null -w '%{time_total}\n' -H 'Content-Type: application/json' \
        -d '{\"model\":\"${LIVE_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"Count 1 to 16.\"}],\"max_tokens\":16}' \
        '${endpoint}/v1/chat/completions' || echo ERR
      i=\$((i + 1))
    done
  " 2>/dev/null | awk '!/ERR/{v[++n]=$1} END{if(n>0){asort(v); printf "%.3f", v[n]*1000/16}else{print "0"}}' 2>/dev/null || echo "0")"

  # Start continuous traffic loop during 4-step probe and readiness-gated rolling replacement.
  local load_log="${SCRATCH}/live-load.log"
  (
    ok=0
    err=0
    while [ ! -f "${SCRATCH}/stop-load" ]; do
      code="$(kubectl -n "$NS" exec "$probe_pod" -- curl -sS -o /dev/null -w '%{http_code}' --max-time 30 \
        -H 'Content-Type: application/json' \
        -d "{\"model\":\"${LIVE_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply OK\"}],\"max_tokens\":16}" \
        "${endpoint}/v1/chat/completions" 2>/dev/null || echo 000)"
      if [ "$code" = "200" ]; then
        ok=$((ok + 1))
      else
        err=$((err + 1))
      fi
      sleep 1
    done
    printf '%s|%s\n' "$ok" "$err" >"$load_log"
  ) &
  LOAD_PID=$!

  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentForger
metadata:
  name: ${mdf_live}
  namespace: ${NS}
  labels:
    e2e.gpustack.ai/case: "38"
spec:
  targetRef:
    name: ${md_live}
  mode: Active
  roles:
    - name: server
      maxKernelsPerGeneration: 4
      minSpeedupRatio: "1.05"
  probe:
    steps: 4
    cooldownSeconds: 600
    minActiveRequests: 1
  sandbox:
    runnerMode: External
    runner:
      image: ${LIVE_RUNNER_IMAGE}
    activeDeadlineSeconds: 1800
  rollout:
    maxRetainedGenerations: 2
    failureTimeoutSeconds: 600
YAML
  CREATED_MDFS+=("$mdf_live")

  local live_promoted=0
  for _ in $(seq 1 $((READY_BOUND / 5))); do
    local out=""
    out="$(kubectl -n "$NS" get "$MDF_RES" "$mdf_live" \
      -o jsonpath='{.status.roles[?(@.name=="server")].generations[0].outcome}' 2>/dev/null || true)"
    if [ "$out" = "Promoted" ]; then
      live_promoted=1
      break
    fi
    sleep 5
  done

  : >"${SCRATCH}/stop-load"
  wait "$LOAD_PID" 2>/dev/null || true
  LOAD_PID=""
  local load_res=""
  load_res="$(cat "$load_log" 2>/dev/null || echo '0|0')"

  if [ "$live_promoted" -eq 1 ]; then
    record PASS "Leg B: live GPU ForgeSandbox promoted across 2-node ${engine_name} deployment" "${NS}/${mdf_live} (base_itl=${base_itl}ms)"
  else
    record FAIL "Leg B: live GPU ForgeSandbox promoted across 2-node ${engine_name} deployment" "timed out"
    return 0
  fi

  # Check zero /dev/shm/gpustack-probe residue and loaded_kernel_keys on /gpustack/v1/runtime_config.
  local shm_bad="" rc_bad="" pod pod_count=0
  for pod in $(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md_live},app.kubernetes.io/component=server" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    pod_count=$((pod_count + 1))
    local rem=""
    rem="$(kubectl -n "$NS" exec "$pod" -c main -- sh -c 'ls -A /dev/shm/gpustack-probe 2>/dev/null || true' 2>/dev/null || true)"
    [ -n "$rem" ] && shm_bad="${shm_bad}${pod}:${rem} "
    local pip=""
    pip="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.podIP}' 2>/dev/null || true)"
    local rcheck=""
    rcheck="$(kubectl -n "$NS" exec "$probe_pod" -- curl -fsS --max-time 10 "http://${pip}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
keys = d.get("loaded_kernel_keys") or []
print("OK" if len(keys) > 0 else "EMPTY")
' 2>/dev/null || echo "ERR")"
    [ "$rcheck" = "OK" ] || rc_bad="${rc_bad}${pod}:${rcheck} "
  done

  if [ "$pod_count" -eq 0 ]; then
    record FAIL "Leg B: zero /dev/shm/gpustack-probe residue on all live GPU Pods" "no server Pods found for ${NS}/${md_live}"
    record FAIL "Leg B: /gpustack/v1/runtime_config confirms loaded_kernel_keys on all Pods" "no server Pods found for ${NS}/${md_live}"
  else
    if [ -z "$shm_bad" ]; then
      record PASS "Leg B: zero /dev/shm/gpustack-probe residue on all live GPU Pods" "${NS}/${md_live} (${pod_count} pods)"
    else
      record FAIL "Leg B: zero /dev/shm/gpustack-probe residue on all live GPU Pods" "$shm_bad"
    fi
    if [ -z "$rc_bad" ]; then
      record PASS "Leg B: /gpustack/v1/runtime_config confirms loaded_kernel_keys on all Pods" "${NS}/${md_live} (${pod_count} pods)"
    else
      record FAIL "Leg B: /gpustack/v1/runtime_config confirms loaded_kernel_keys on all Pods" "$rc_bad"
    fi
  fi
  if [ "${load_res##*|}" = "0" ] && [ "${load_res%%|*}" -ge 1 ]; then
    record PASS "Leg B: zero dropped client requests during probe and readiness-gated rollout" "ok=${load_res%%|*} err=0"
  else
    record FAIL "Leg B: zero dropped client requests during probe and readiness-gated rollout" "ok=${load_res%%|*} err=${load_res##*|}"
  fi
}

run_leg_b_live_gpu

results
