#!/usr/bin/env bash
#
# CASE 12 -- ModelExpress Peer D2D, Shared Compile Cache and Server-Restart Fallback   (MUTATING, self-recovering)
#
#   case-12-modelexpress.sh [OUT_DIR]
#
# Goal:        Measure a ModelDeployment whose Server role loads with vLLM and
#              --load-format modelexpress in Graph Mode (no --enforce-eager: full torch.compile
#              plus CUDA Graph capture, MX_ARTIFACT_TRANSFER=1) across three load paths on a
#              two-node NVIDIA pool. Stage 1 runs a seed replica on the source node and records
#              its weight load, torch.compile and CUDA graph times plus the ModelExpress metadata
#              and compile-cache publication evidence. Stage 2 scales to two replicas so the
#              second lands cross-node on the target node, and records the peer device-to-device
#              weight transfer (RdmaStrategy) and the shared compile-cache restoration. Stage 3
#              deletes the per-deployment modelexpress metadata server pod, whose in-memory state
#              is empty by design, scales back to two replicas, and records the fallback from
#              RdmaStrategy to the model-transfer source load (GpustackLoadStrategy). Stage 4
#              writes comparison.tsv and speedup-summary.json comparing Stage 2 against
#              Stage 1 and Stage 3.
# Environment: An isolated two-node NVIDIA pool with an InstanceType covering all accelerators
#              of one node, the ModelArtifact resolved, its NodeModelStore cache Ready only on
#              the source node, and the source's model-transfer Serving generation prepared, so
#              GpustackLoadStrategy has a live source in Stage 1 and Stage 3. The target node
#              must initially lack the digest and be schedulable. The engine image must carry
#              modelexpress==0.8.0 and support the model, the parallelism and graph capture.
#              The cluster's model-artifact-remote-delivery-policy is recorded, not changed;
#              every stage needs a ready source, which Prefer and Require both admit.
# Inputs:      MX_SOURCE_NODE and MX_TARGET_NODE (distinct), MX_INSTANCE_TYPE (NVIDIA) and
#              MX_IMAGE (the pinned runner image) are required. MX_MODEL defaults to
#              Qwen/Qwen3-235B-A22B-Instruct-2507-FP8, MX_ARTIFACT to qwen3-235b-a22b-fp8,
#              MX_ACCEL to 8, MX_INTERFACE to 8 and MX_MODE to tp8_ep; MX_MODE is tp2, tp4,
#              tp4_ep, tp8 or tp8_ep and must match MX_ACCEL. MX_TP_MODE (default all) and
#              MX_MAX_WINDOW_BYTES (default 34359738368 for tp8/tp8_ep, 68719476736 for
#              tp4/tp4_ep) configure the GpustackLoadStrategy source-fallback reader in
#              Stage 1 and Stage 3. MX_ENABLE_FLASHINFER_AUTOTUNE=1 re-enables FlashInfer
#              autotune (disabled by default to avoid single-rank cuda:0 cache asymmetry across
#              TP ranks). MX_SERVER_IMAGE names the image the operator's modelexpress metadata
#              server is expected to run (the operator lever is the
#              model-deployment-modelexpress-server-image worker setting; when unset the
#              observed image is recorded only). MX_ALLOWED_RDMA_HCAS narrows the RDMA check.
#              Use a fresh OUT_DIR. The manifest is written to OUT_DIR/modelexpress-bench.yaml.
# Expected:    - the operator reconciles a <deploy>-modelexpress Deployment (one replica,
#                Recreate strategy, MX_METADATA_BACKEND=memory) and ClusterIP Service on :8001;
#              - every engine pod carries the injected ModelExpress and model-transfer env:
#                MX_SERVER_ADDRESS on :8001, MX_WORKER_HOST, MX_ARTIFACT_TRANSFER=1,
#                MX_LOAD_PLUGIN=gpustack, MX_LOAD_PLUGIN_BEFORE=server-cache,
#                MX_ARTIFACT_COMPILE_CONFIG_DIGEST, GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY and
#                GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT=modelexpress, plus the compile-cache dirs;
#              - Stage 1 reaches Ready, passes one content-checked chat completion containing
#                1591 for 37 * 43 with finish_reason=stop, mounts the requested RDMA interfaces,
#                and publishes ModelExpress metadata and compile cache artifacts;
#              - Stage 2's new replica lands on the target node cross-node from the seed, loads
#                through RdmaStrategy over peer device-to-device transfer, restores the peer's
#                compile cache, and passes the same checks in a shorter wall clock;
#              - Stage 3's scale-down keeps the seed replica on the source node; the replaced
#                metadata server comes back Ready empty; the new target-node replica falls back
#                to GpustackLoadStrategy over the model-transfer source, reaches Ready and passes
#                the same checks;
#              - OUT_DIR/comparison.tsv carries one row per stage with pod, node, strategy,
#                compile_cache_hit, weight_load_s, torch_compile_s, cuda_graph_s, ready wall
#                time, RDMA and chat checks; OUT_DIR/speedup-summary.json carries the Stage 2
#                weight-load and compile-plus-graph speedups against Stage 1 and Stage 3.
# Cleanup:     The exit trap uncordons the target node if this run cordoned it and deletes the
#              benchmark deployment. Evidence and the synchronized caches remain on disk.
#
# NOT EXECUTED YET: this script was written for the acceptance run and has never been run on a
# cluster. Every expectation above is the contract, not an observation.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/modelexpress}"
DEPLOY="${MX_DEPLOY:-mx-bench}"
MODEL="${MX_MODEL:-Qwen/Qwen3-235B-A22B-Instruct-2507-FP8}"
ARTIFACT="${MX_ARTIFACT:-qwen3-235b-a22b-fp8}"
INSTANCE_TYPE="${MX_ACCELERATOR_TYPE:-${MX_INSTANCE_TYPE:?Set MX_ACCELERATOR_TYPE or MX_INSTANCE_TYPE for the NVIDIA pool, for example nvidia-h100-80g}}"
SOURCE_NODE="${MX_SOURCE_NODE:-}"
TARGET_NODE="${MX_TARGET_NODE:-}"
IMAGE="${MX_IMAGE:?Set MX_IMAGE to the pinned runner image carrying modelexpress==0.8.0}"
SERVER_IMAGE="${MX_SERVER_IMAGE:-}"
ACCEL="${MX_ACCEL:-8}"
IFACE="${MX_INTERFACE:-8}"
MODE="${MX_MODE:-tp8_ep}"
TP_MODE="${MX_TP_MODE:-all}"
ENABLE_FLASHINFER_AUTOTUNE="${MX_ENABLE_FLASHINFER_AUTOTUNE:-0}"
ALLOWED_HCAS="${MX_ALLOWED_RDMA_HCAS:-mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_6,mlx5_7}"
READY_TIMEOUT="${READY_TIMEOUT:-3600}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-1800}"
MX_SERVER_TIMEOUT="${MX_SERVER_TIMEOUT:-600}"
CHAT_CHECKS=1
case "${INSTANCE_TYPE}" in
  *nvidia*) ;;
  *) echo "Case 12 is an NVIDIA/NIXL case: MX_ACCELERATOR_TYPE / MX_INSTANCE_TYPE must name an NVIDIA pool" >&2; exit 1 ;;
esac
if [ -z "${SOURCE_NODE}" ] || [ -z "${TARGET_NODE}" ] || [ "${SOURCE_NODE}" = "${TARGET_NODE}" ]; then
  echo "Set distinct MX_SOURCE_NODE and MX_TARGET_NODE for the two-node pool" >&2
  exit 1
fi
case "${ACCEL}" in
  ""|*[!0-9]*|0) echo "MX_ACCEL must be a positive accelerator count" >&2; exit 1 ;;
esac
case "${IFACE}" in
  ""|*[!0-9]*) echo "MX_INTERFACE must be a non-negative interface count" >&2; exit 1 ;;
esac
case "${MODE}" in
  tp2)
    MODE_ACCEL=2
    DEFAULT_MAX_WINDOW_BYTES="17179869184"
    ;;
  tp4|tp4_ep)
    MODE_ACCEL=4
    DEFAULT_MAX_WINDOW_BYTES="68719476736"
    ;;
  tp8|tp8_ep)
    MODE_ACCEL=8
    DEFAULT_MAX_WINDOW_BYTES="34359738368"
    ;;
  *) echo "MX_MODE must be tp2, tp4, tp4_ep, tp8 or tp8_ep" >&2; exit 1 ;;
esac
MAX_WINDOW_BYTES="${MX_MAX_WINDOW_BYTES:-${DEFAULT_MAX_WINDOW_BYTES}}"
if [ "${ACCEL}" != "${MODE_ACCEL}" ]; then
  echo "MX_MODE=${MODE} requires MX_ACCEL=${MODE_ACCEL}, got ${ACCEL}" >&2
  exit 1
fi
if [ -n "${SERVER_IMAGE}" ]; then
  EXPECT_SERVER_IMAGE=1
else
  EXPECT_SERVER_IMAGE=0
fi

TARGET_CORDONED=0
FAILED_STAGES=0
STAGE1_POD="" STAGE1_NODE=""
STAGE2_POD="" STAGE2_NODE="" STAGE2_STRATEGY="unknown" STAGE2_CACHE="no"
STAGE3_NODE="" STAGE3_STRATEGY="unknown" STAGE3_CACHE="no"

mkdir -p "${OUT}"
[ ! -e "${OUT}/comparison.tsv" ] || { echo "Use a fresh OUT_DIR" >&2; exit 1; }
log() { echo "[$(date +%H:%M:%S)] $*"; }

capture_cluster_shape "${OUT}/cluster-shape.txt"

cleanup() {
  if [ "${TARGET_CORDONED}" = 1 ]; then
    kubectl uncordon "${TARGET_NODE}" >/dev/null 2>&1 || true
  fi
  kubectl -n "${NS}" delete modeldeployment "${DEPLOY}" --ignore-not-found --wait=true \
    --timeout=180s >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

render_extra_args() {
  printf '        - --load-format\n        - modelexpress\n'
  if [ "${ENABLE_FLASHINFER_AUTOTUNE}" != "1" ]; then
    # FlashInfer autotune caches GEMM tactics keyed only by cuda:0; across multi-rank TP runs or
    # when ModelExpress shares the compile cache, rank 0 hits the cache while ranks 1..N-1 enter
    # collective MoE autotune and deadlock.
    printf '        - --kernel-config\n        - '\''{"enable_flashinfer_autotune":false}'\''\n'
  fi
  case "${MODE}" in
    tp2)
      printf '        - --tensor-parallel-size\n        - "2"\n'
      ;;
    tp4)
      printf '        - --tensor-parallel-size\n        - "4"\n        - --max-model-len\n        - "8192"\n'
      ;;
    tp4_ep)
      printf '        - --tensor-parallel-size\n        - "4"\n        - --enable-expert-parallel\n        - --max-model-len\n        - "8192"\n'
      ;;
    tp8)
      printf '        - --tensor-parallel-size\n        - "8"\n        - --max-model-len\n        - "8192"\n'
      ;;
    tp8_ep)
      printf '        - --tensor-parallel-size\n        - "8"\n        - --enable-expert-parallel\n        - --max-model-len\n        - "8192"\n'
      ;;
  esac
}

render_role_env() {
  local entries=()
  if [ -n "${TP_MODE}" ]; then
    entries+=("        - name: GPUSTACK_MODEL_TRANSFER_TP_MODE" "          value: \"${TP_MODE}\"")
  fi
  if [ -n "${MAX_WINDOW_BYTES}" ]; then
    entries+=("        - name: GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES" "          value: \"${MAX_WINDOW_BYTES}\"")
  fi
  if [ "${#entries[@]}" -gt 0 ]; then
    printf '      env:\n'
    printf '%s\n' "${entries[@]}"
  fi
}

write_manifest() {
  local manifest_path="$1"
  local replicas="$2"
  local extra_args role_env
  extra_args="$(render_extra_args)"
  role_env="$(render_role_env)"
  cat > "${manifest_path}" <<YAML
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: ${DEPLOY}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: ${replicas}
      size: 1
      startupTimeoutSeconds: 1800
      acceleratorType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "${ACCEL}"
        interface: "${IFACE}"
      extraArgs:
${extra_args}
${role_env}
YAML
}

scale_replicas() {
  local count="$1" label="$2" stage_dir="$3" replicas
  if ! kubectl -n "${NS}" patch modeldeployment "${DEPLOY}" --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/spec/roles/0/replicas\",\"value\":${count}}]" \
    > "${stage_dir}/${label}.txt" 2>&1; then
    log "ERROR: the ${label} patch was rejected: $(cat "${stage_dir}/${label}.txt" 2>/dev/null)"
    return 1
  fi
  replicas="$(kubectl -n "${NS}" get modeldeployment "${DEPLOY}" \
    -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  if [ "${replicas:-0}" != "${count}" ]; then
    log "ERROR: the ${label} patch did not take: roles[0].replicas is ${replicas:-unknown}, not ${count}"
    return 1
  fi
  return 0
}

wait_replicas_ready() {
  local target_replicas="$1"
  local out_file="$2"
  local start_ts
  start_ts="$(date +%s)"
  local elapsed=0
  while [ "${elapsed}" -lt "${READY_TIMEOUT}" ]; do
    local ready_count
    ready_count="$(kubectl -n "${NS}" get pods \
      -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" \
      -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null \
      | grep -c '^true$' || true)"
    if [ "${ready_count}" -eq "${target_replicas}" ]; then
      local end_ts
      end_ts="$(date +%s)"
      echo "$((end_ts - start_ts))" > "${out_file}"
      log "All ${target_replicas} replicas of ${DEPLOY} Ready in $((end_ts - start_ts))s"
      return 0
    fi
    sleep 2
    elapsed="$(($(date +%s) - start_ts))"
  done
  log "ERROR: timed out after ${READY_TIMEOUT}s waiting for ${target_replicas} replicas of ${DEPLOY}"
  return 1
}

ready_server_pods() {
  kubectl -n "${NS}" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\t"}{.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null \
    | awk -F'\t' '$3 == "true" { print $1 "\t" $2 }'
}

pod_on_node() {
  # ready_server_pods output filtered to one node; prints the first match.
  local node="$1"
  ready_server_pods | awk -F'\t' -v target="${node}" '$2 == target { print $1; exit }'
}

clear_node_compile_cache() {
  local node="$1" dm_pod
  dm_pod="$(kubectl -n gpustack-system get pods \
    -l "app.kubernetes.io/component=device-manager" \
    --field-selector "spec.nodeName=${node}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "${dm_pod}" ]; then
    log "Clearing /var/lib/gpustack/cache/nvidia-vllm on ${node} via ${dm_pod}"
    kubectl -n gpustack-system exec "${dm_pod}" -- \
      rm -rf /var/lib/gpustack/cache/nvidia-vllm >/dev/null 2>&1 || true
  fi
}

ensure_source_resident() {
  local digest resident prefetch
  digest="$(kubectl -n "${NS}" get modelartifact "${ARTIFACT}" \
    -o jsonpath='{.status.resolved.manifestDigest}' 2>/dev/null || true)"
  [ -n "${digest}" ] || return 0
  resident="$(kubectl get nodemodelstore "${SOURCE_NODE}" -o json 2>/dev/null | python3 -c '
import json, sys
digest = sys.argv[1]
doc = json.load(sys.stdin)
for g in ((doc.get("status") or {}).get("modelTransfer") or {}).get("generations") or []:
    if g.get("digest") == digest and g.get("state") == "Serving":
        print(int(g.get("residentBytes") or 0))
        raise SystemExit(0)
print(-1)
' "${digest}" 2>/dev/null || echo "-1")"
  if [ "${resident}" -gt 0 ] 2>/dev/null; then
    return 0
  fi
  prefetch="$(kubectl -n "${NS}" get modelprefetch -o json 2>/dev/null | python3 -c '
import json, sys
artifact = sys.argv[1]
doc = json.load(sys.stdin)
for item in doc.get("items") or []:
    if ((item.get("spec") or {}).get("artifactRef") or {}).get("name") == artifact and (item.get("spec") or {}).get("source") is not None:
        print(item["metadata"]["name"])
        raise SystemExit(0)
' "${ARTIFACT}" 2>/dev/null || true)"
  if [ -z "${prefetch}" ]; then
    log "WARN: source generation for ${ARTIFACT} on ${SOURCE_NODE} is idle-evicted (residentBytes=${resident}), and no source ModelPrefetch was found"
    return 0
  fi
  log "Source generation for ${ARTIFACT} on ${SOURCE_NODE} is idle-evicted (residentBytes=${resident}); re-preparing via ModelPrefetch ${prefetch}"
  kubectl -n "${NS}" patch modelprefetch "${prefetch}" --type=merge -p '{"spec":{"source":null}}' >/dev/null || return 1
  local waited=0
  while [ "${waited}" -lt 60 ]; do
    if ! kubectl get nodemodelstore "${SOURCE_NODE}" -o json 2>/dev/null | python3 -c '
import json, sys
digest = sys.argv[1]
doc = json.load(sys.stdin)
gens = [g for g in (((doc.get("status") or {}).get("modelTransfer") or {}).get("generations") or []) if g.get("digest") == digest]
raise SystemExit(0 if gens else 1)
' "${digest}"; then
      break
    fi
    sleep 2
    waited=$((waited + 2))
  done
  kubectl -n "${NS}" patch modelprefetch "${prefetch}" --type=merge -p '{"spec":{"source":{}}}' >/dev/null || return 1
  waited=0
  while [ "${waited}" -lt "${READY_TIMEOUT}" ]; do
    resident="$(kubectl get nodemodelstore "${SOURCE_NODE}" -o json 2>/dev/null | python3 -c '
import json, sys
digest = sys.argv[1]
doc = json.load(sys.stdin)
for g in ((doc.get("status") or {}).get("modelTransfer") or {}).get("generations") or []:
    if g.get("digest") == digest and g.get("state") == "Serving":
        print(int(g.get("residentBytes") or 0))
        raise SystemExit(0)
print(0)
' "${digest}" 2>/dev/null || echo "0")"
    if [ "${resident}" -gt 0 ] 2>/dev/null; then
      log "Source generation for ${ARTIFACT} on ${SOURCE_NODE} resident again (${resident} bytes)"
      return 0
    fi
    sleep 3
    waited=$((waited + 3))
  done
  log "ERROR: timed out waiting for ${ARTIFACT} source generation on ${SOURCE_NODE} to refill residentBytes"
  return 1
}

check_source() {
  local evidence="$1"
  mkdir -p "${evidence}"
  ensure_source_resident || return 1
  kubectl -n "${NS}" get modelartifact "${ARTIFACT}" -o json > "${evidence}/artifact.json" || return 1
  kubectl get nodemodelstore -o json > "${evidence}/stores.json" || return 1
  python3 - "${evidence}" "${SOURCE_NODE}" "${TARGET_NODE}" <<'PY'
import json, pathlib, sys
root, source, target = pathlib.Path(sys.argv[1]), *sys.argv[2:]
artifact = json.loads((root / 'artifact.json').read_text())
digest = artifact['status']['resolved']['manifestDigest']
assert digest, 'artifact lacks a resolved manifest digest'
stores = json.loads((root / 'stores.json').read_text())['items']
ready, serving = [], []
for store in stores:
    node, status = store['metadata']['name'], store.get('status', {})
    models = [m for m in status.get('models', []) if m['digest'] == digest]
    assert node != target or not models, 'target already holds the digest; use a fresh target cache'
    if any(m['state'] == 'Ready' for m in models):
        ready.append(node)
    if any(g['digest'] == digest and g['state'] == 'Serving'
           for g in (status.get('modelTransfer') or {}).get('generations', [])):
        serving.append(node)
assert ready == [source], f'expected the disk cache only on {source}, got {ready}'
assert serving == [source], f'expected the Serving source generation only on {source}, got {serving}'
PY
}

verify_rdma_mount() {
  local pod="$1"
  local stage_dir="$2"
  if [ "${IFACE}" = "0" ]; then
    echo "n/a" > "${stage_dir}/rdma-check.txt"
    return 0
  fi
  # shellcheck disable=SC2016 # container variables expand inside the remote container shell
  if kubectl -n "${NS}" exec "${pod}" -- python3 -c '
import glob, os, sys
allowed = set(filter(None, sys.argv[1].split(",")))
uverbs = sorted(glob.glob("/dev/infiniband/uverbs*"))
rdma_devs = [d.strip() for d in os.environ.get("GPUSTACK_RDMA_DEVICES", "").split(",") if d.strip()]
nccl_hcas = [d.strip().split(":")[0] for d in os.environ.get("NCCL_IB_HCA", "").lstrip("^=").split(",") if d.strip()]
ucx_net = os.environ.get("UCX_NET_DEVICES", "") or ",".join(f"{d}:1" for d in rdma_devs)
print("UVERBS=" + ",".join(uverbs))
print("GPUSTACK_RDMA_DEVICES=" + ",".join(rdma_devs))
print("NCCL_IB_HCA=" + os.environ.get("NCCL_IB_HCA", ""))
print("UCX_NET_DEVICES=" + ucx_net)
assert uverbs, "missing /dev/infiniband/uverbs*"
assert rdma_devs, "empty GPUSTACK_RDMA_DEVICES"
assert nccl_hcas, "empty NCCL_IB_HCA"
if allowed:
    assert set(rdma_devs).issubset(allowed), f"disallowed HCA in GPUSTACK_RDMA_DEVICES: {rdma_devs} vs {allowed}"
    assert set(nccl_hcas).issubset(allowed), f"disallowed HCA in NCCL_IB_HCA: {nccl_hcas} vs {allowed}"
' "${ALLOWED_HCAS}" > "${stage_dir}/rdma-mount-${pod}.txt" 2>&1; then
    echo "PASS" > "${stage_dir}/rdma-check.txt"
    log "Verified RDMA mount and env vars inside ${pod}"
    return 0
  fi
  echo "FAIL" > "${stage_dir}/rdma-check.txt"
  log "WARN: RDMA mount verification failed inside ${pod}"
  return 1
}

verify_chat_completions() {
  local pod="$1"
  local stage_dir="$2"
  local ok=0
  local idx
  for idx in $(seq 1 "${CHAT_CHECKS}"); do
    local prompt expected
    case "${idx}" in
      1)
        prompt="What is 37 * 43? Reply with the number only."
        expected="1591"
        ;;
      *)
        prompt="What is the capital of France? Reply in one word."
        expected="Paris"
        ;;
    esac
    local out_file="${stage_dir}/chat-check-${idx}.json"
    if kubectl -n "${NS}" exec "${pod}" -- python3 -c "
import json, urllib.request
expected = '''${expected}'''
req = urllib.request.Request(
    'http://127.0.0.1:8000/v1/chat/completions',
    data=json.dumps({
        'model': '${MODEL}',
        'messages': [{'role': 'user', 'content': '''${prompt}'''}],
        'max_tokens': 64,
        'temperature': 0.0,
        'chat_template_kwargs': {'enable_thinking': False},
    }).encode('utf-8'),
    headers={'Content-Type': 'application/json'},
)
with urllib.request.urlopen(req, timeout=120) as resp:
    body = resp.read().decode('utf-8')
    data = json.loads(body)
    assert data.get('choices'), 'missing choices'
    content = data['choices'][0].get('message', {}).get('content', '') or ''
    assert data['choices'][0].get('finish_reason') == 'stop', 'incomplete response'
    assert expected.lower() in content.lower(), f'expected {expected!r} in {content!r}'
    print(body)
" > "${out_file}" 2>&1; then
      ok=$((ok + 1))
    fi
  done
  echo "${ok}/${CHAT_CHECKS}" > "${stage_dir}/chat-checks.txt"
  log "Verified ${ok}/${CHAT_CHECKS} /v1/chat/completions checks on ${pod}"
  [ "${ok}" -eq "${CHAT_CHECKS}" ]
}

assert_modelexpress_objects() {
  local stage_dir="$1"
  kubectl -n "${NS}" get deployment "${DEPLOY}-modelexpress" -o json \
    > "${stage_dir}/modelexpress-deployment.json" 2>/dev/null || return 1
  kubectl -n "${NS}" get service "${DEPLOY}-modelexpress" -o json \
    > "${stage_dir}/modelexpress-service.json" 2>/dev/null || return 1
  python3 - "${stage_dir}" "${DEPLOY}" "${SERVER_IMAGE}" "${EXPECT_SERVER_IMAGE}" <<'PY'
import json, pathlib, sys
stage, deploy, expected_image, expect = pathlib.Path(sys.argv[1]), *sys.argv[2:]
expect = expect == "1"
deployment = json.loads((stage / "modelexpress-deployment.json").read_text())
service = json.loads((stage / "modelexpress-service.json").read_text())
spec = deployment["spec"]
assert spec.get("replicas") == 1, f"modelexpress Deployment replicas is {spec.get('replicas')}, not 1"
strategy = spec.get("strategy", {}).get("type")
assert strategy == "Recreate", f"modelexpress Deployment strategy is {strategy}, not Recreate"
containers = spec["template"]["spec"]["containers"]
assert len(containers) == 1, "modelexpress Deployment does not carry exactly one container"
main = containers[0]
assert main["name"] == "modelexpress-server", f"container is {main['name']}"
env = {e["name"]: e.get("value") for e in main.get("env", [])}
assert env.get("MX_METADATA_BACKEND") == "memory", f"MX_METADATA_BACKEND is {env.get('MX_METADATA_BACKEND')}"
assert env.get("MODEL_EXPRESS_SERVER_PORT") == "8001", "MODEL_EXPRESS_SERVER_PORT is not 8001"
ports = main.get("ports", [])
assert any(p.get("containerPort") == 8001 for p in ports), "the metadata server does not listen on 8001"
observed_image = main.get("image", "")
(stage / "modelexpress-server-image.txt").write_text(observed_image + "\n")
if expect:
    assert observed_image == expected_image, (
        f"metadata server image is {observed_image}, expected {expected_image}; "
        "set the model-deployment-modelexpress-server-image worker setting"
    )
assert service["spec"].get("type") == "ClusterIP", "the metadata Service is not ClusterIP"
service_ports = service["spec"].get("ports", [])
assert any(p.get("port") == 8001 for p in service_ports), "the metadata Service does not expose 8001"
PY
}

wait_modelexpress_server_ready() {
  # Prints the Ready metadata server pod name; the caller logs the failure, because this
  # function runs inside a command substitution that would swallow a stdout log line.
  local waited=0 mx_pod
  while [ "${waited}" -lt "${MX_SERVER_TIMEOUT}" ]; do
    mx_pod="$(kubectl -n "${NS}" get pods \
      -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=modelexpress-server" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -n 1)"
    if [ -n "${mx_pod}" ] && kubectl -n "${NS}" wait --for=condition=ready \
      "pod/${mx_pod}" --timeout=30s >/dev/null 2>&1; then
      printf '%s\n' "${mx_pod}"
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  return 1
}

assert_role_pod_env() {
  local pod="$1"
  local stage_dir="$2"
  kubectl -n "${NS}" get pod "${pod}" -o json > "${stage_dir}/server-pod.json" 2>/dev/null || return 1
  python3 - "${stage_dir}" > "${stage_dir}/env-pod.txt" <<'PY'
import json, pathlib, sys
stage = pathlib.Path(sys.argv[1])
pod = json.loads((stage / "server-pod.json").read_text())
main = [c for c in pod["spec"]["containers"] if c["name"] == "main"][0]
env = {e["name"]: e.get("value", "") for e in main.get("env", [])}
required = {
    "MX_SERVER_ADDRESS": None,
    "MX_WORKER_HOST": None,
    "MX_ARTIFACT_TRANSFER": "1",
    "MX_LOAD_PLUGIN": "gpustack",
    "MX_LOAD_PLUGIN_BEFORE": "server-cache",
    "MX_ARTIFACT_COMPILE_CONFIG_DIGEST": None,
    "MX_ARTIFACT_READY_URL": None,
    "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY": None,
    "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "modelexpress",
    "VLLM_CACHE_ROOT": None,
    "TORCHINDUCTOR_CACHE_DIR": None,
    "TRITON_CACHE_DIR": None,
    "HF_HUB_CACHE": None,
}
missing = [n for n, v in required.items() if n not in env]
for name, value in sorted(required.items()):
    if name in env:
        print(f"{name}={env[name]}")
if missing:
    raise AssertionError(f"server pod lacks the injected env: {', '.join(missing)}")
address = env["MX_SERVER_ADDRESS"]
assert address.endswith(":8001"), f"MX_SERVER_ADDRESS does not point at :8001: {address}"
assert "-modelexpress" in address, f"MX_SERVER_ADDRESS does not name the metadata Service: {address}"
recorded = {
    n: env[n] for n in sorted(env)
    if n.startswith("GPUSTACK_MODEL_TRANSFER_") or n in ("MODEL_EXPRESS_URL", "HF_HUB_OFFLINE",
                                                        "MX_COMPILED_CACHE_DIR", "CUDA_CACHE_PATH")
}
for name, value in recorded.items():
    print(f"recorded {name}={value}")
PY
}

collect_runtime_env() {
  # The upstream launcher names (MODEL_EXPRESS_URL, MX_COMPILED_CACHE_DIR, MX_RANK_ID) and the
  # engine runtime names (HF_HUB_OFFLINE, CUDA_CACHE_PATH, CUDA_VISIBLE_DEVICES) live in the
  # engine process environment, not on the rendered pod spec, so they are recorded from PID 1
  # and a missing name is a warning, not a stage failure.
  local pod="$1"
  local stage_dir="$2"
  kubectl -n "${NS}" exec "${pod}" -- sh -c 'tr "\0" "\n" < /proc/1/environ' \
    > "${stage_dir}/env-runtime.txt" 2>&1 || true
  local name missing=""
  for name in MODEL_EXPRESS_URL MX_COMPILED_CACHE_DIR MX_RANK_ID HF_HUB_OFFLINE CUDA_CACHE_PATH CUDA_VISIBLE_DEVICES; do
    if ! grep -q "^${name}=" "${stage_dir}/env-runtime.txt" 2>/dev/null; then
      missing="${missing} ${name}"
    fi
  done
  if [ -n "${missing}" ]; then
    log "WARN: engine runtime env of ${pod} lacks:${missing}"
  fi
}

log_seconds() {
  # LOG_FILE PATTERN REDUCE prints the largest seconds figure the pattern matched, or nothing.
  local log_file="$1" pattern="$2" reduce="$3"
  grep -Eo "${pattern}" "${log_file}" 2>/dev/null | sed -E "${reduce}" | sort -nr | head -n 1
}

gather_stage_timings() {
  local stage_dir="$1"
  local log_file="$2"
  local weight transferred compile graph
  weight="$(log_seconds "${log_file}" \
    'Model loading took [0-9.]+ GiB memory and [0-9.]+ seconds' 's/.* ([0-9.]+) seconds/\1/')"
  transferred="$(log_seconds "${log_file}" \
    'Transferred [0-9.]+ [A-Za-z]+ in [0-9.]+s' 's/.* in ([0-9.]+)s/\1/')"
  compile="$(log_seconds "${log_file}" \
    'torch\.compile (takes|took) [0-9.]+ s' 's/.* (takes|took) ([0-9.]+) s/\2/')"
  graph="$(log_seconds "${log_file}" \
    'Graph capturing finished in [0-9.]+ secs' 's/.* in ([0-9.]+) secs/\1/')"
  {
    printf 'weight_load_model_s=%s\n' "${weight:-}"
    printf 'weight_load_transferred_s=%s\n' "${transferred:-}"
    printf 'torch_compile_s=%s\n' "${compile:-}"
    printf 'cuda_graph_s=%s\n' "${graph:-}"
  } > "${stage_dir}/timings.env"
  grep -E 'RdmaStrategy|GpustackLoadStrategy|ServerCacheStrategy|DefaultStrategy|Strategy rdma failed|TARGET_BUFFER_MEASURE|MxModelLoader starting' \
    "${log_file}" > "${stage_dir}/strategy-evidence.txt" 2>/dev/null || true
  grep -E 'Published ModelExpress metadata|Artifact cache published|Published P2P metadata to MX server|Source published successfully|vLLM artifact publish complete' \
    "${log_file}" > "${stage_dir}/publication-evidence.txt" 2>/dev/null || true
  # The strong restoration line is the evidence a compile cache was restored; a bare
  # MX_ARTIFACT_TRANSFER mention is recorded beside it but proves nothing on its own,
  # because the env dump of the launcher carries the same name.
  grep -E 'Artifact cache restored|vLLM artifact install complete|which ModelExpress installed' \
    "${log_file}" > "${stage_dir}/restore-evidence.txt" 2>/dev/null || true
  grep -E 'MX_ARTIFACT_TRANSFER' \
    "${log_file}" >> "${stage_dir}/restore-evidence.txt" 2>/dev/null || true
  grep -cE 'Artifact cache restored|vLLM artifact install complete|which ModelExpress installed' \
    "${log_file}" > "${stage_dir}/restore-strong-count.txt" 2>/dev/null || true
  if [ -n "${transferred}" ]; then
    printf '%s\n' "${transferred}" > "${stage_dir}/weight-load-seconds.txt"
  elif [ -n "${weight}" ]; then
    printf '%s\n' "${weight}" > "${stage_dir}/weight-load-seconds.txt"
  fi
  [ -n "${compile}" ] && printf '%s\n' "${compile}" > "${stage_dir}/torch-compile-seconds.txt"
  [ -n "${graph}" ] && printf '%s\n' "${graph}" > "${stage_dir}/cuda-graph-seconds.txt"
  return 0
}

read_value() {
  local file="$1"
  [ -s "${file}" ] && cat "${file}" || printf '%s' ""
}

stage_row() {
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9" "${10}" "${11}" >> "${OUT}/comparison.tsv"
}

stage1_seed_replica() {
  local stage_name="stage1-seed-replica"
  local stage_dir="${OUT}/${stage_name}"
  mkdir -p "${stage_dir}"
  log "Stage 1: seeding one replica on ${SOURCE_NODE}"
  if ! check_source "${OUT}/checks"; then
    log "ERROR: the source is not the sole disk cache and Serving generation for ${ARTIFACT}"
    return 1
  fi
  if [ "$(kubectl get node "${TARGET_NODE}" -o jsonpath='{.spec.unschedulable}')" = "true" ]; then
    log "ERROR: ${TARGET_NODE} is already cordoned"
    return 1
  fi
  kubectl cordon "${TARGET_NODE}" >/dev/null || return 1
  TARGET_CORDONED=1
  clear_node_compile_cache "${SOURCE_NODE}"
  clear_node_compile_cache "${TARGET_NODE}"
  write_manifest "${OUT}/modelexpress-bench.yaml" 1
  kubectl -n "${NS}" apply -f "${OUT}/modelexpress-bench.yaml" >/dev/null || return 1
  if ! assert_modelexpress_objects "${stage_dir}"; then
    log "ERROR: the ${DEPLOY}-modelexpress Deployment and Service did not reconcile as expected"
    kubectl -n "${NS}" describe modeldeployment "${DEPLOY}" > "${stage_dir}/describe.txt" 2>&1 || true
    return 1
  fi
  local mx_pod
  mx_pod="$(wait_modelexpress_server_ready)" || {
    log "ERROR: the ${DEPLOY}-modelexpress metadata server did not become Ready within ${MX_SERVER_TIMEOUT}s"
    return 1
  }
  log "Metadata server pod ${mx_pod} is Ready"
  if ! wait_replicas_ready 1 "${stage_dir}/ready-wall-seconds.txt"; then
    kubectl -n "${NS}" describe modeldeployment "${DEPLOY}" > "${stage_dir}/describe.txt" 2>&1 || true
    kubectl -n "${NS}" get pods -o wide > "${stage_dir}/pods.txt" 2>&1 || true
    return 1
  fi
  local pod
  pod="$(pod_on_node "${SOURCE_NODE}")"
  if [ -z "${pod}" ]; then
    log "ERROR: no ready replica landed on ${SOURCE_NODE}"
    ready_server_pods > "${stage_dir}/pods-ready.txt" 2>/dev/null || true
    return 1
  fi
  STAGE1_POD="${pod}"
  STAGE1_NODE="${SOURCE_NODE}"
  kubectl -n "${NS}" logs "${pod}" > "${stage_dir}/${pod}.log" 2>&1 || true
  if ! assert_role_pod_env "${pod}" "${stage_dir}"; then
    log "ERROR: ${pod} lacks the injected ModelExpress env"
    return 1
  fi
  collect_runtime_env "${pod}" "${stage_dir}"
  verify_rdma_mount "${pod}" "${stage_dir}"
  verify_chat_completions "${pod}" "${stage_dir}"
  gather_stage_timings "${stage_dir}" "${stage_dir}/${pod}.log"
  # The seed has no peer to read, so the chain runs past RdmaStrategy: name the first
  # strategy below it that the log shows, as a diagnostic rather than a gate.
  local seed_strategy="unknown"
  if grep -qE 'GpustackLoadStrategy|TARGET_BUFFER_MEASURE' "${stage_dir}/strategy-evidence.txt"; then
    seed_strategy="GpustackLoadStrategy"
  elif grep -q 'ServerCacheStrategy' "${stage_dir}/strategy-evidence.txt"; then
    seed_strategy="ServerCacheStrategy"
  elif grep -q 'DefaultStrategy' "${stage_dir}/strategy-evidence.txt"; then
    seed_strategy="DefaultStrategy"
  fi
  local publication_ok=0
  if [ -s "${stage_dir}/publication-evidence.txt" ]; then
    publication_ok=1
  else
    log "WARN: ${pod} logged no ModelExpress metadata or compile-cache publication line"
  fi
  local rdma_ok chat_ok
  rdma_ok="$(read_value "${stage_dir}/rdma-check.txt")"
  chat_ok="$(read_value "${stage_dir}/chat-checks.txt")"
  [ "${rdma_ok}" = "FAIL" ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  [ "${chat_ok}" != "${CHAT_CHECKS}/${CHAT_CHECKS}" ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  [ "${publication_ok}" = 0 ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  stage_row "${stage_name}" "${pod}" "${STAGE1_NODE}" "${seed_strategy}" \
    "no" \
    "$(read_value "${stage_dir}/weight-load-seconds.txt")" \
    "$(read_value "${stage_dir}/torch-compile-seconds.txt")" \
    "$(read_value "${stage_dir}/cuda-graph-seconds.txt")" \
    "$(read_value "${stage_dir}/ready-wall-seconds.txt")" \
    "${rdma_ok:-n/a}" "${chat_ok:-0/${CHAT_CHECKS}}"
  return 0
}

stage2_peer_d2d_replica() {
  local stage_name="stage2-peer-d2d-replica"
  local stage_dir="${OUT}/${stage_name}"
  mkdir -p "${stage_dir}"
  log "Stage 2: scaling to two replicas so the peer lands on ${TARGET_NODE}"
  clear_node_compile_cache "${TARGET_NODE}"
  kubectl uncordon "${TARGET_NODE}" >/dev/null || return 1
  TARGET_CORDONED=0
  if ! scale_replicas 2 "scale-up-patch" "${stage_dir}"; then
    return 1
  fi
  if ! wait_replicas_ready 2 "${stage_dir}/ready-wall-seconds.txt"; then
    kubectl -n "${NS}" describe modeldeployment "${DEPLOY}" > "${stage_dir}/describe.txt" 2>&1 || true
    kubectl -n "${NS}" get pods -o wide > "${stage_dir}/pods.txt" 2>&1 || true
    return 1
  fi
  local seed_pod peer_pod
  seed_pod="$(pod_on_node "${SOURCE_NODE}")"
  peer_pod="$(pod_on_node "${TARGET_NODE}")"
  if [ -z "${seed_pod}" ] || [ -z "${peer_pod}" ]; then
    log "ERROR: expected one ready replica on each node"
    ready_server_pods > "${stage_dir}/pods-ready.txt" 2>/dev/null || true
    return 1
  fi
  if [ "${seed_pod}" != "${STAGE1_POD}" ]; then
    log "WARN: the Stage 1 seed pod ${STAGE1_POD} is no longer the ready pod on ${SOURCE_NODE} (now ${seed_pod})"
  fi
  STAGE2_POD="${peer_pod}"
  STAGE2_NODE="${TARGET_NODE}"
  kubectl -n "${NS}" logs "${peer_pod}" > "${stage_dir}/${peer_pod}.log" 2>&1 || true
  verify_rdma_mount "${peer_pod}" "${stage_dir}"
  verify_chat_completions "${peer_pod}" "${stage_dir}"
  gather_stage_timings "${stage_dir}" "${stage_dir}/${peer_pod}.log"
  if { grep -q 'RdmaStrategy' "${stage_dir}/strategy-evidence.txt" \
    || { grep -q 'MxModelLoader starting' "${stage_dir}/strategy-evidence.txt" \
      && ! grep -qE 'Strategy rdma failed|TARGET_BUFFER_MEASURE' "${stage_dir}/strategy-evidence.txt"; }; } \
    && [ -n "$(read_value "${stage_dir}/weight-load-seconds.txt")" ]; then
    STAGE2_STRATEGY="RdmaStrategy"
  else
    log "ERROR: ${peer_pod} shows no RdmaStrategy peer device-to-device load"
    STAGE2_STRATEGY="unknown"
    FAILED_STAGES=$((FAILED_STAGES + 1))
  fi
  if [ "$(read_value "${stage_dir}/restore-strong-count.txt")" -gt 0 ]; then
    STAGE2_CACHE="yes"
  elif [ -s "${stage_dir}/restore-evidence.txt" ]; then
    log "WARN: ${peer_pod} shows MX_ARTIFACT_TRANSFER but no strong compile-cache restoration line"
    STAGE2_CACHE="unproven"
  else
    log "ERROR: ${peer_pod} logged no shared compile-cache restoration"
    STAGE2_CACHE="no"
    FAILED_STAGES=$((FAILED_STAGES + 1))
  fi
  local rdma_ok chat_ok
  rdma_ok="$(read_value "${stage_dir}/rdma-check.txt")"
  chat_ok="$(read_value "${stage_dir}/chat-checks.txt")"
  [ "${rdma_ok}" = "FAIL" ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  [ "${chat_ok}" != "${CHAT_CHECKS}/${CHAT_CHECKS}" ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  stage_row "${stage_name}" "${peer_pod}" "${STAGE2_NODE}" "${STAGE2_STRATEGY}" \
    "${STAGE2_CACHE}" \
    "$(read_value "${stage_dir}/weight-load-seconds.txt")" \
    "$(read_value "${stage_dir}/torch-compile-seconds.txt")" \
    "$(read_value "${stage_dir}/cuda-graph-seconds.txt")" \
    "$(read_value "${stage_dir}/ready-wall-seconds.txt")" \
    "${rdma_ok:-n/a}" "${chat_ok:-0/${CHAT_CHECKS}}"
  return 0
}

stage3_server_restart_fallback() {
  local stage_name="stage3-server-restart-fallback"
  local stage_dir="${OUT}/${stage_name}"
  mkdir -p "${stage_dir}"
  log "Stage 3: restarting the metadata server, then falling back to the source load"
  if ! scale_replicas 1 "scale-down-patch" "${stage_dir}"; then
    return 1
  fi
  # Unrouted Server deployments hold retirement in observeModelDeploymentRetirementResidual when
  # spec.router is nil; delete the Stage 2 peer pod directly and clear the aborted retirement
  # reservation so the subsequent scale-up can recreate ordinal 1.
  if [ -n "${STAGE2_POD}" ]; then
    kubectl -n "${NS}" delete pod "${STAGE2_POD}" --ignore-not-found --wait=true --timeout=180s \
      > "${stage_dir}/scale-down-pod-delete.txt" 2>&1 || true
    kubectl -n "${NS}" patch modeldeployment "${DEPLOY}" --subresource=status --type=merge \
      -p '{"status":{"retirement":null}}' >/dev/null 2>&1 || true
  fi
  local waited=0 survivor
  while [ "${waited}" -lt "${SCALE_TIMEOUT}" ]; do
    survivor="$(ready_server_pods | awk -F'\t' 'NR==1 { print $1 }')"
    local count
    count="$(ready_server_pods | grep -c . || true)"
    if [ "${count}" = "1" ] && [ -n "${survivor}" ]; then
      break
    fi
    sleep 5
    waited=$((waited + 5))
  done
  if [ -z "${survivor}" ]; then
    log "ERROR: no replica survived the scale-down"
    return 1
  fi
  local survivor_node
  survivor_node="$(ready_server_pods | awk -F'\t' -v pod="${survivor}" '$1 == pod { print $2 }')"
  if [ "${survivor_node}" != "${SOURCE_NODE}" ]; then
    log "ERROR: the scale-down kept ${survivor} on ${survivor_node:-unknown}, not the seed on ${SOURCE_NODE}"
    FAILED_STAGES=$((FAILED_STAGES + 1))
    return 1
  fi
  ensure_source_resident || return 1
  clear_node_compile_cache "${TARGET_NODE}"
  if ! scale_replicas 2 "scale-up-patch" "${stage_dir}"; then
    return 1
  fi
  # Wait 25s for the new replica container to reach engine initialization before force-restarting
  # modelexpress-server, so the surviving seed replica's 30s background heartbeat cannot re-register
  # a stale source entry before the new replica queries RdmaStrategy.
  sleep 25
  local old_mx_pod old_mx_uid restart_start restart_end
  old_mx_pod="$(kubectl -n "${NS}" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=modelexpress-server" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  old_mx_uid="$(kubectl -n "${NS}" get pod "${old_mx_pod}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  restart_start="$(date +%s)"
  kubectl -n "${NS}" delete pod "${old_mx_pod}" --grace-period=0 --force --wait=false \
    > "${stage_dir}/server-restart-delete.txt" 2>&1 || true
  local new_mx_pod=""
  waited=0
  while [ "${waited}" -lt "${MX_SERVER_TIMEOUT}" ]; do
    new_mx_pod="$(kubectl -n "${NS}" get pods \
      -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=modelexpress-server" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -vx "${old_mx_pod}" | head -n 1 || true)"
    if [ -n "${new_mx_pod}" ] \
      && kubectl -n "${NS}" wait --for=condition=ready "pod/${new_mx_pod}" --timeout=30s >/dev/null 2>&1; then
      break
    fi
    sleep 2
    waited=$((waited + 2))
  done
  if [ -z "${new_mx_pod}" ] || [ "${new_mx_pod}" = "${old_mx_pod}" ]; then
    log "ERROR: the ${DEPLOY}-modelexpress metadata server did not come back Ready"
    return 1
  fi
  restart_end="$(date +%s)"
  {
    echo "old_pod=${old_mx_pod}"
    echo "old_uid=${old_mx_uid}"
    echo "new_pod=${new_mx_pod}"
    echo "new_uid=$(kubectl -n "${NS}" get pod "${new_mx_pod}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    echo "restart_seconds=$((restart_end - restart_start))"
    echo "in_memory_state=wiped by design: the replacement server starts empty"
  } > "${stage_dir}/server-restart.txt"
  log "Metadata server replaced: ${old_mx_pod} to ${new_mx_pod} in $((restart_end - restart_start))s"
  if ! wait_replicas_ready 2 "${stage_dir}/ready-wall-seconds.txt"; then
    kubectl -n "${NS}" describe modeldeployment "${DEPLOY}" > "${stage_dir}/describe.txt" 2>&1 || true
    kubectl -n "${NS}" get pods -o wide > "${stage_dir}/pods.txt" 2>&1 || true
    return 1
  fi
  local fallback_pod
  fallback_pod="$(pod_on_node "${TARGET_NODE}")"
  if [ -z "${fallback_pod}" ] || [ "${fallback_pod}" = "${STAGE2_POD}" ]; then
    log "ERROR: expected a new ready replica on ${TARGET_NODE}, got ${fallback_pod:-none}"
    ready_server_pods > "${stage_dir}/pods-ready.txt" 2>/dev/null || true
    FAILED_STAGES=$((FAILED_STAGES + 1))
    return 1
  fi
  STAGE3_NODE="${TARGET_NODE}"
  kubectl -n "${NS}" logs "${fallback_pod}" > "${stage_dir}/${fallback_pod}.log" 2>&1 || true
  verify_rdma_mount "${fallback_pod}" "${stage_dir}"
  verify_chat_completions "${fallback_pod}" "${stage_dir}"
  gather_stage_timings "${stage_dir}" "${stage_dir}/${fallback_pod}.log"
  if grep -qE 'GpustackLoadStrategy|TARGET_BUFFER_MEASURE' "${stage_dir}/strategy-evidence.txt"; then
    STAGE3_STRATEGY="GpustackLoadStrategy"
  else
    log "ERROR: ${fallback_pod} shows no GpustackLoadStrategy source fallback"
    STAGE3_STRATEGY="unknown"
    FAILED_STAGES=$((FAILED_STAGES + 1))
  fi
  if [ "$(read_value "${stage_dir}/restore-strong-count.txt")" -gt 0 ]; then
    STAGE3_CACHE="yes"
  fi
  local rdma_ok chat_ok
  rdma_ok="$(read_value "${stage_dir}/rdma-check.txt")"
  chat_ok="$(read_value "${stage_dir}/chat-checks.txt")"
  [ "${rdma_ok}" = "FAIL" ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  [ "${chat_ok}" != "${CHAT_CHECKS}/${CHAT_CHECKS}" ] && FAILED_STAGES=$((FAILED_STAGES + 1))
  stage_row "${stage_name}" "${fallback_pod}" "${STAGE3_NODE}" "${STAGE3_STRATEGY}" \
    "${STAGE3_CACHE}" \
    "$(read_value "${stage_dir}/weight-load-seconds.txt")" \
    "$(read_value "${stage_dir}/torch-compile-seconds.txt")" \
    "$(read_value "${stage_dir}/cuda-graph-seconds.txt")" \
    "$(read_value "${stage_dir}/ready-wall-seconds.txt")" \
    "${rdma_ok:-n/a}" "${chat_ok:-0/${CHAT_CHECKS}}"
  return 0
}

stage4_summaries() {
  python3 - "${OUT}" <<'PY'
import csv, json, pathlib, sys
out = pathlib.Path(sys.argv[1])
rows = []
with (out / "comparison.tsv").open() as handle:
    for row in csv.DictReader(handle, delimiter="\t"):
        rows.append(row)
def number(value):
    try:
        result = float(value)
    except (TypeError, ValueError):
        return None
    return result if result > 0 else None
def stage_row(name):
    for row in rows:
        if row["stage"] == name:
            return row
    return None
def metric(row, key):
    if row is None:
        return None
    return number(row.get(key))
def speedup(faster, slower):
    if faster is None or slower is None or faster <= 0 or slower <= 0:
        return None
    return round(slower / faster, 4)
def combined(row):
    compile_s = metric(row, "torch_compile_s")
    graph_s = metric(row, "cuda_graph_s")
    if compile_s is None or graph_s is None:
        return None
    return compile_s + graph_s
seed, peer, fallback = (stage_row("stage1-seed-replica"), stage_row("stage2-peer-d2d-replica"),
                        stage_row("stage3-server-restart-fallback"))
summary = {
    "inputs": {
        "weight_load_s": {
            "stage1": metric(seed, "weight_load_s"),
            "stage2": metric(peer, "weight_load_s"),
            "stage3": metric(fallback, "weight_load_s"),
        },
        "compile_plus_graph_s": {
            "stage1": combined(seed),
            "stage2": combined(peer),
            "stage3": combined(fallback),
        },
    },
    "stage2_vs_stage1": {
        "weight_load_speedup": speedup(metric(peer, "weight_load_s"), metric(seed, "weight_load_s")),
        "compile_plus_graph_speedup": speedup(combined(peer), combined(seed)),
    },
    "stage2_vs_stage3": {
        "weight_load_speedup": speedup(metric(peer, "weight_load_s"), metric(fallback, "weight_load_s")),
        "compile_plus_graph_speedup": speedup(combined(peer), combined(fallback)),
    },
    "timer": "engine-reported seconds: weight load or peer transfer, torch.compile, CUDA graph capture",
}
(out / "speedup-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
PY
  log "Comparison written to ${OUT}/comparison.tsv"
  cat "${OUT}/comparison.tsv"
  log "Speedup summary written to ${OUT}/speedup-summary.json"
}

{
  printf 'stage\tpod\tnode\tstrategy\tcompile_cache_hit\tweight_load_s\ttorch_compile_s\tcuda_graph_s\tready_wall_s\trdma_check\tchat_checks\n'
} > "${OUT}/comparison.tsv"

log "Starting Case 12 (ModelExpress peer D2D, shared compile cache, restart fallback)"
if ! stage1_seed_replica; then
  FAILED_STAGES=$((FAILED_STAGES + 1))
  log "ERROR: Stage 1 failed; evidence retained in ${OUT}/stage1-seed-replica"
  exit 1
fi
if ! stage2_peer_d2d_replica; then
  FAILED_STAGES=$((FAILED_STAGES + 1))
  log "ERROR: Stage 2 failed; evidence retained in ${OUT}/stage2-peer-d2d-replica"
  exit 1
fi
if ! stage3_server_restart_fallback; then
  FAILED_STAGES=$((FAILED_STAGES + 1))
  log "ERROR: Stage 3 failed; evidence retained in ${OUT}/stage3-server-restart-fallback"
  exit 1
fi
stage4_summaries
if [ "${FAILED_STAGES}" -gt 0 ]; then
  log "ERROR: ${FAILED_STAGES} check(s) failed verification"
  exit 1
fi
log "Case 12 complete: peer D2D and restart-fallback evidence retained in ${OUT}"
