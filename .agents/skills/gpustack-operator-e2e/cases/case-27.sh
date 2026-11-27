#!/usr/bin/env bash
#
# CASE 27 — ModelDeployment rendered topology & ownership: KV reuse domain isolation,
#           unreachable Binding degradation, routed P/D 6-object router lifecycle &
#           KVEventsPublishing, and EPD encoder Services, picker document & shared claim
#   (MUTATING, self-cleaning: shared Mooncake fixture, unreachable external pool, routed
#    lifecycle deployments, and optional CPU-vendor fixture + hostPath PV/PVC for EPD;
#    all removed by the EXIT trap)
#
#   case-27.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_KUBE_CONTEXT,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_ACCELERATOR_TYPE,E2E_MD_SETTLE,E2E_ROUTER_SETTLE,E2E_ROUTER_STABLE,E2E_ROUTER_UNPULLABLE_IMAGE,E2E_SYSTEM_NS,E2E_C27_VENDOR,E2E_C27_SETTLE,E2E_C27_PODS,E2E_C27_STATUS,E2E_C27_WITNESS_IMAGE
# Goal:        Verify ModelDeployment rendered topology, KV isolation/degradation, router object
#              ownership, and EPD stage rendering without requiring accelerators:
#                1. KV reuse domain sharing & isolation across Bindings (SGLang MOONCAKE_TENANT_ID
#                   and live Mooncake data partitioning) and unreachable-pool degradation (vLLM
#                   renders 5/5 connector carriers on Ready Binding vs 0/5 on unreachable Binding,
#                   DomainRegistered=False/BindingNotReady, CacheAttached never falsely True).
#                2. Routed P/D 6-object router lifecycle (Deployment, ConfigMap, Service,
#                   ServiceAccount, Role, RoleBinding) with owner references, discoverability notes,
#                   prune/restore/GC, KVEventsPublishing condition states (NoRouter, Publishing,
#                   PublisherDisabled, NotApplicable, RoleUnmanaged), and live router endpoint
#                   readiness & stability (AUTO-SKIP if upstream router image cannot be pulled).
#                3. EPD encoder topology: per-role Services fronting only their own role,
#                   deployment-wide Service fronting prefill even with encode listed first,
#                   roleSummary E/P/D order, RoleKindsReady accounting, v1 ENDPOINT column,
#                   EndpointPickerConfig encode filter/profile/decider/discovery exclusion vs P/D
#                   control, shared PVC mounts, and writer/reader witness Pod round-trip.
# Environment: Any single-node cluster with a materialized scheduling chain (run case-1 first),
#              pull access to the Mooncake store image, and jq on PATH. Phase 3 additionally uses
#              pyyaml to parse epp.yaml (picker rows SKIP if pyyaml is absent) and injects a
#              temporary CPU-vendor NodeFeature when no Ascend AcceleratorType exists.
# Inputs:      REAL: Mooncake KVCacheBackend/Pool/Bindings, unreachable external backend/pool,
#              routed ModelDeployments, PV/PVC, and witness Pods. MOCKED (Phase 3 only when no
#              Ascend AcceleratorType exists): CPU-vendor NodeFeature + awareness setting.
# Expected:    All three phases pass their rendered-topology, isolation, lifecycle, condition,
#              router-picker, and shared-claim assertions; environment-gated sub-legs (unpullable
#              router image, missing pyyaml, NFD declining vendor override, unpullable witness
#              image) report SKIP explicitly rather than false PASS/FAIL.
# Cleanup:     EXIT trap deletes all ModelDeployments, releases Kueue Workloads, removes witness
#              Pods and PV/PVC, forces unreachable KVCache objects whose master never answered,
#              tears down the CPU-vendor fixture if injected, and runs kvi_teardown.
set -uo pipefail

OPERATOR_NS="${1:-}"
if [ -z "$OPERATOR_NS" ]; then
  echo "usage: case-27.sh <NS>" >&2
  exit 2
fi

CASE_ID=27
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }
# shellcheck disable=SC2034 # Read by sourced _md-lib.sh.
MD_KUBE_CONTEXT="$KCTX"

# shellcheck source=/dev/null
. "${CASE_DIR}/_kvcache-inject-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_epd-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-27] jq is required." >&2; exit 2; }
if ! k get namespace "$OPERATOR_NS" >/dev/null 2>&1; then
  echo "[case-27] operator namespace ${OPERATOR_NS} does not exist" >&2
  exit 2
fi

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE="${E2E_MD_SETTLE:-120}"
ROUTER_IMAGE="${E2E_ROUTER_UNPULLABLE_IMAGE:-invalid.invalid/gpustack/router:not-present}"
ROUTER_SETTLE="${E2E_ROUTER_SETTLE:-180}"
STABLE="${E2E_ROUTER_STABLE:-30}"

# Phase 1 names.
DEAD_BACKEND="e2e-c27-kvcb-dead-${SFX}"
DEAD_POOL="e2e-c27-kvcp-dead-${SFX}"
DEAD_BINDING="e2e-c27-bind-dead-${SFX}"
DEAD_DOMAIN="e2e-c27-dom-dead-${SFX}"
DEAD_HOST="e2e-c27-nowhere.invalid"
OTHER_DOMAIN="e2e-c27-other-${SFX}"
OTHER_BINDING="e2e-c27-other-${SFX}"

# Phase 2 names.
LIFECYCLE="e2e-c27-lifecycle"
SERVER="e2e-c27-server"
UNMANAGED="e2e-c27-unmanaged"
ROUTED_SERVER="e2e-c27-routed-server"
DISABLED="e2e-c27-publisher-disabled"
ROUTER_READY="e2e-c27-router-ready"
EXTERNAL_DP="e2e-c27-external-dp"

# Phase 3 names.
EPD_VENDOR="${E2E_C27_VENDOR:-Ascend}"
EPD_PLATFORM="$(printf '%s' "$EPD_VENDOR" | tr '[:upper:]' '[:lower:]')"
EPD_NF="e2e-c27-cpu-vendor"
EPD_MD="e2e-c27-epd"
EPD_PD="e2e-c27-pd"
EPD_CLAIM="e2e-c27-claim"
EPD_PV="e2e-c27-pv"
WITNESS_IMAGE="${E2E_C27_WITNESS_IMAGE:-busybox:1.36}"
EPD_MOUNT="/ec-cache"
EPD_SETTLE_ROUNDS="${E2E_C27_SETTLE:-60}"
EPD_POD_ROUNDS="${E2E_C27_PODS:-40}"
EPD_STATUS_ROUNDS="${E2E_C27_STATUS:-12}"

PICKER_YAML_OK=yes
python3 -c 'import yaml' >/dev/null 2>&1 || PICKER_YAML_OK=no

# Custom predicate poller (distinct name so it does not shadow _rows-lib.sh wait_for).
wait_until() {
  local deadline=$((SECONDS + $1))
  shift
  while [ "$SECONDS" -lt "$deadline" ]; do
    "$@" && return 0
    sleep 3
  done
  "$@"
}

force_delete() {
  local kind="$1" name="$2" ns_args=() left=""
  [ -n "${3:-}" ] && ns_args=(-n "$3")
  k ${ns_args[@]+"${ns_args[@]}"} delete "$kind" "$name" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 10); do
    left="$(k ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" -o name 2>/dev/null)"
    [ -z "$left" ] && return 0
    sleep 3
  done
  echo "[case-27] ${kind}/${name} did not release in 30s; forcing finalizer off."
  k ${ns_args[@]+"${ns_args[@]}"} patch "$kind" "$name" --type=merge \
    -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
}

release_deployment_in() {
  local ns="$1" md="$2"
  k -n "$ns" delete modeldeployments.worker.gpustack.ai "$md" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$ns" "$md" >/dev/null 2>&1 || true
}

cleanup_epd_deployments() {
  k -n "$OPERATOR_NS" delete pod e2e-c27-witness-write e2e-c27-witness-read \
    --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  for md in "$EPD_MD" "$EPD_PD"; do
    k -n "$OPERATOR_NS" delete modeldeployments.worker.gpustack.ai "$md" \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  for _ in $(seq 1 12); do
    sleep 5
    md_force_release "$OPERATOR_NS" "$EPD_MD" >/dev/null 2>&1 || true
    md_force_release "$OPERATOR_NS" "$EPD_PD" >/dev/null 2>&1 || true
    [ -z "$(k -n "$OPERATOR_NS" get pods -l "app.kubernetes.io/instance=${EPD_MD}" --no-headers 2>/dev/null)" ] &&
      [ -z "$(k -n "$OPERATOR_NS" get pods -l "app.kubernetes.io/instance=${EPD_PD}" --no-headers 2>/dev/null)" ] && break
  done
  k -n "$OPERATOR_NS" delete pvc "$EPD_CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete pv "$EPD_PV" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

case27_teardown() {
  local pod md
  if [ -n "${TEST_NS:-}" ]; then
    for pod in $(k -n "$TEST_NS" get pods -l "app.kubernetes.io/instance=${LIFECYCLE}" \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
      k -n "$TEST_NS" patch pod "$pod" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    done
    for md in e2e-c27-a e2e-c27-b e2e-c27-c e2e-c27-registered e2e-c27-unreachable \
              "$LIFECYCLE" "$SERVER" "$UNMANAGED" "$ROUTED_SERVER" "$DISABLED"; do
      release_deployment_in "$TEST_NS" "$md"
    done
    if [ -n "${OTHER_BINDING:-}" ]; then
      k -n "$TEST_NS" delete kvcachepoolbindings.worker.gpustack.ai "$OTHER_BINDING" \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    force_delete kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" "$TEST_NS"
  fi
  for pod in $(k -n "$OPERATOR_NS" get pods -l "app.kubernetes.io/instance=${ROUTER_READY}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    k -n "$OPERATOR_NS" patch pod "$pod" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  done
  release_deployment_in "$OPERATOR_NS" "$ROUTER_READY"
  release_deployment_in "$OPERATOR_NS" "$EXTERNAL_DP"
  epd_teardown_cpu_vendor_fixture "$OPERATOR_NS" "$EPD_NF" "$EPD_SETTLE_ROUNDS" "$SYSTEM_NS" cleanup_epd_deployments || true
  kvi_teardown
  force_delete kvcachepools.worker.gpustack.ai "$DEAD_POOL"
  force_delete kvcachebackends.worker.gpustack.ai "$DEAD_BACKEND"
}
trap case27_teardown EXIT

IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-$(md_usable_instance_type)}}"
if [ -z "$IT" ]; then
  echo "[case-27] no usable AcceleratorType; run case-1 first" >&2
  exit 2
fi

# Stand up the shared Mooncake fixture once for Phases 1 and 2.
FIXTURE_OK=yes
if ! kvi_setup; then
  record FAIL "Mooncake cache fixture becomes Ready" \
    "backend, pool, or Binding did not converge; Phases 1 and 2 skipped"
  FIXTURE_OK=no
fi

# =================================================================================================
# PHASE 1: KV reuse domain sharing/isolation and unreachable Binding degradation
# =================================================================================================

p1_deploy() {
  k apply -f - <<YAML 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${TEST_NS}
spec:
  engine:
    name: ${3}
    image: ${CLIENT_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${2}
  roles:
  - name: server
    acceleratorType: ${IT}
    replicas: 1
    image: ${CLIENT_IMAGE}
YAML
}

p1_pod_of() {
  local md="$1" pod=""
  for _ in $(seq 1 40); do
    pod="$(k -n "$TEST_NS" get pods \
      -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${md}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$pod" ] && break
    sleep 3
  done
  echo "$pod"
}

p1_rendered_tenant() {
  local pod
  pod="$(p1_pod_of "$1")"
  [ -n "$pod" ] || return 1
  k -n "$TEST_NS" get pod "$pod" -o \
    jsonpath='{.spec.containers[?(@.name=="main")].env[?(@.name=="MOONCAKE_TENANT_ID")].value}' 2>/dev/null
}

p1_conds_of() {
  local md="$1" want="$2" conds=""
  for _ in $(seq 1 20); do
    conds="$(k -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason};{end}' 2>/dev/null)"
    [ -n "$conds" ] && [ -z "${conds##*"${want}"*}" ] && break
    sleep 3
  done
  echo "$conds"
}

p1_markers() {
  local pod="$1" n=0 names="" v cmd
  v="$(k -n "$TEST_NS" get pod "$pod" \
    -o "jsonpath={.metadata.annotations.kvcache\\.gpustack\\.ai/client-config}" 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}annotation,"; }
  v="$(k -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.containers[?(@.name=="main")].env[?(@.name=="MOONCAKE_CONFIG_PATH")].value}' 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}env,"; }
  cmd="$(k -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.containers[?(@.name=="main")].command}' 2>/dev/null)"
  [ -n "$cmd" ] && [ -z "${cmd##*--kv-transfer-config*}" ] && { n=$((n + 1)); names="${names}arg,"; }
  v="$(k -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.volumes[?(@.name=="gpustack-kvcache-config")].name}' 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}volume,"; }
  v="$(k -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.containers[?(@.name=="main")].volumeMounts[?(@.name=="gpustack-kvcache-config")].mountPath}' 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}mount,"; }
  names="${names%,}"
  echo "${n}:${names:-none}"
}

if [ "$FIXTURE_OK" = yes ]; then
  # Subphase 1a: reuse domain sharing and tenant isolation.
  p1a_ok=yes
  k apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${OTHER_BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: ${OTHER_DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quota:
    ceiling: 256Mi
YAML
  if ! kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$OTHER_BINDING" \
    '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
    record FAIL "second reuse domain Binding is Ready" \
      "binding ${OTHER_BINDING} did not reach Ready in 180s"
    p1a_ok=no
  fi

  if [ "$p1a_ok" = yes ]; then
    deploy_out="$(p1_deploy e2e-c27-a "$BINDING" SGLang; p1_deploy e2e-c27-b "$BINDING" SGLang; p1_deploy e2e-c27-c "$OTHER_BINDING" SGLang)"
    deploy_missing=""
    for md in e2e-c27-a e2e-c27-b e2e-c27-c; do
      k -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" -o name >/dev/null 2>&1 \
        || deploy_missing="${deploy_missing} ${md}"
    done
    if [ -n "$deploy_missing" ]; then
      record FAIL "three SGLang domain-isolation deployments admitted" \
        "absent:${deploy_missing}; apply: $(printf '%s' "${deploy_out:-<none>}" | tr '\n' ' ' | cut -c1-200)"
      p1a_ok=no
    fi
  fi

  if [ "$p1a_ok" = yes ]; then
    TA="$(p1_rendered_tenant e2e-c27-a || true)"
    TB="$(p1_rendered_tenant e2e-c27-b || true)"
    TC="$(p1_rendered_tenant e2e-c27-c || true)"

    if [ -z "$TA" ] || [ -z "$TB" ]; then
      record FAIL "two deployments on one Binding render the same reuse domain" \
        "MOONCAKE_TENANT_ID empty on e2e-c27-a ('${TA}') or e2e-c27-b ('${TB}')"
    elif [ "$TA" = "$TB" ] && [ "$TA" = "$DOMAIN" ]; then
      record PASS "two deployments on one Binding render the same reuse domain" \
        "both carry MOONCAKE_TENANT_ID=${TA}"
    else
      record FAIL "two deployments on one Binding render the same reuse domain" \
        "a='${TA}' b='${TB}' expected '${DOMAIN}'"
    fi

    if [ -z "$TC" ]; then
      record FAIL "deployment on a different Binding renders a different domain" \
        "MOONCAKE_TENANT_ID empty on e2e-c27-c"
    elif [ "$TC" = "$OTHER_DOMAIN" ] && [ "$TC" != "$TA" ]; then
      record PASS "deployment on a different Binding renders a different domain" \
        "e2e-c27-c carries ${TC} vs ${TA}"
    else
      record FAIL "deployment on a different Binding renders a different domain" \
        "e2e-c27-c='${TC}' expected '${OTHER_DOMAIN}' != '${TA}'"
    fi

    if [ -z "$TA" ] || [ -z "$TC" ]; then
      record SKIP "rendered tenant partitions the Mooncake store" \
        "no tenant rendered (a='${TA}', c='${TC}')"
    else
      k apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e-c27-probe
  namespace: ${TEST_NS}
spec:
  restartPolicy: Never
  containers:
  - name: client
    image: ${CLIENT_IMAGE}
    command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML
      if ! kvi_wait_for pods e2e-c27-probe '{.status.phase}' Running 240 "$TEST_NS" >/dev/null; then
        record SKIP "rendered tenant partitions the Mooncake store" \
          "probe Pod did not reach Running in 240s"
      else
        PAYLOAD="e2e-c27-$(date -u +%s)"
        LOG="$(mktemp)"
        k -n "$TEST_NS" exec e2e-c27-probe -c client -- python3 -c "
import time
from mooncake.store import MooncakeDistributedStore as S
def cli(t):
    s = S()
    rc = s.setup('probe-%s' % t, 'P2PHANDSHAKE', 0, 128*1024*1024, 'tcp', '', '${ENDPOINT}', tenant_id=t)
    print('SETUP %s rc=%d' % (t, rc))
    return s
a = cli('${TA}')
print('PUT rc=%d' % a.put('${PAYLOAD}', b'${PAYLOAD}'))
print('SAME len=%d' % len(a.get('${PAYLOAD}') or b''))
c = cli('${TC}')
print('OTHER len=%d' % len(c.get('${PAYLOAD}') or b''))
deadline = time.time() + 60
while True:
    rc = a.remove('${PAYLOAD}')
    if rc == 0 or rc == -704:
        print('DRAIN ok rc=%d' % rc)
        break
    if rc != -706:
        print('DRAIN unexpected rc=%d' % rc)
        break
    if time.time() > deadline:
        print('DRAIN gave-up rc=-706 after 60s')
        break
    time.sleep(3)
" >"$LOG" 2>&1 || true

        same="$(grep -m1 '^SAME len=' "$LOG" | sed 's/.*len=//')"
        other="$(grep -m1 '^OTHER len=' "$LOG" | sed 's/.*len=//')"
        if [ -z "$same" ] || [ -z "$other" ]; then
          record SKIP "rendered tenant partitions the Mooncake store" \
            "client produced no SAME/OTHER line: $(tr '\n' ' ' <"$LOG" | cut -c1-200)"
        elif [ "$same" -gt 0 ] && [ "$other" -eq 0 ]; then
          record PASS "rendered tenant partitions the Mooncake store" \
            "wrote under ${TA} (${same}B), read 0B under ${TC}"
        elif [ "$same" -gt 0 ]; then
          record FAIL "rendered tenant partitions the Mooncake store" \
            "payload readable under both ${TA} and ${TC} (${other}B)"
        else
          record FAIL "rendered tenant partitions the Mooncake store" \
            "write did not round-trip under ${TA}: $(tr '\n' ' ' <"$LOG" | cut -c1-200)"
        fi
        rm -f "$LOG"
      fi
    fi
  fi

  for md in e2e-c27-a e2e-c27-b e2e-c27-c; do
    release_deployment_in "$TEST_NS" "$md"
  done
  for _ in $(seq 1 30); do
    left="$(k -n "$TEST_NS" get modeldeployments.worker.gpustack.ai \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
    case " $left " in
      *" e2e-c27-a "* | *" e2e-c27-b "* | *" e2e-c27-c "*) sleep 3 ;;
      *) break ;;
    esac
  done

  # Subphase 1b: unreachable pool degradation vs Ready Binding control.
  p1b_ok=yes
  dead_out="$(k apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${DEAD_BACKEND}
spec:
  type: Mooncake
  connection:
    external:
      endpoints:
        - name: Client
          address: ${DEAD_HOST}:50051
        - name: Admin
          address: ${DEAD_HOST}:9003
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${DEAD_POOL}
spec:
  backends: [${DEAD_BACKEND}]
  quota:
    total: 1Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${DEAD_BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${DEAD_POOL}
  domain:
    name: ${DEAD_DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quota:
    ceiling: 256Mi
YAML
)"
  dead_missing=""
  k get kvcachebackends.worker.gpustack.ai "$DEAD_BACKEND" -o name >/dev/null 2>&1 || dead_missing="${dead_missing} backend"
  k get kvcachepools.worker.gpustack.ai "$DEAD_POOL" -o name >/dev/null 2>&1 || dead_missing="${dead_missing} pool"
  k -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" -o name >/dev/null 2>&1 || dead_missing="${dead_missing} binding"
  if [ -n "$dead_missing" ]; then
    record FAIL "unreachable pool objects admitted" \
      "absent:${dead_missing}; apply: $(printf '%s' "$dead_out" | tr '\n' ' ' | cut -c1-200)"
    p1b_ok=no
  fi

  if [ "$p1b_ok" = yes ]; then
    dead_ready=no
    DEAD_PHASE="$(kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" \
      '{.status.phase}' Ready 90 "$TEST_NS")" && dead_ready=yes
    if [ "$dead_ready" = yes ]; then
      record FAIL "unreachable pool Binding never reports Ready" \
        "${DEAD_BINDING} reached Ready against ${DEAD_HOST}"
    else
      record PASS "unreachable pool Binding never reports Ready" \
        "${DEAD_BINDING} is '${DEAD_PHASE:-<none>}' after 90s"
    fi

    u_out="$(p1_deploy e2e-c27-unreachable "$DEAD_BINDING" vLLM)"
    r_out="$(p1_deploy e2e-c27-registered "$BINDING" vLLM)"
    md_missing=""
    for md in e2e-c27-unreachable e2e-c27-registered; do
      k -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" -o name >/dev/null 2>&1 || md_missing="${md_missing} ${md}"
    done
    if [ -n "$md_missing" ]; then
      record FAIL "unreachable and registered vLLM deployments admitted" \
        "absent:${md_missing}; u=${u_out:0:100} / r=${r_out:0:100}"
      p1b_ok=no
    fi
  fi

  if [ "$p1b_ok" = yes ]; then
    U_CONDS="$(p1_conds_of e2e-c27-unreachable 'DomainRegistered=')"
    if [ -n "$U_CONDS" ] && [ -z "${U_CONDS##*DomainRegistered=False/BindingNotReady*}" ]; then
      record PASS "unreachable pool reports DomainRegistered=False/BindingNotReady" \
        "DomainRegistered=False/BindingNotReady"
    else
      record FAIL "unreachable pool reports DomainRegistered=False/BindingNotReady" \
        "got: ${U_CONDS:-<none within 60s>}"
    fi

    POD_U="$(p1_pod_of e2e-c27-unreachable)"
    POD_R="$(p1_pod_of e2e-c27-registered)"
    if [ -z "$POD_U" ] || [ -z "$POD_R" ]; then
      record FAIL "unreachable pool renders no connector while Ready Binding renders all five carriers" \
        "missing replica within 120s (u='${POD_U}', r='${POD_R}')"
    else
      MU="$(p1_markers "$POD_U")"
      MR="$(p1_markers "$POD_R")"
      if [ -z "${MR%%5:*}" ] && [ -z "${MU%%0:*}" ]; then
        record PASS "unreachable pool renders no connector while Ready Binding renders all five carriers" \
          "registered=${MR}, unreachable=${MU}"
      else
        record FAIL "unreachable pool renders no connector while Ready Binding renders all five carriers" \
          "registered=${MR}, unreachable=${MU} (wanted 5:* and 0:*)"
      fi
    fi

    R_CONDS="$(p1_conds_of e2e-c27-registered 'CacheAttached=')"
    R_CA=""
    if [ -n "$R_CONDS" ] && [ -z "${R_CONDS##*CacheAttached=*}" ]; then
      R_CA="${R_CONDS##*CacheAttached=}"
      R_CA="${R_CA%%;*}"
    fi
    if [ -z "$R_CA" ]; then
      record FAIL "rendered connector is not reported as an attached cache" \
        "no CacheAttached condition within 60s (${R_CONDS:-<none>})"
    elif [ -z "${R_CONDS##*DomainRegistered=True/Registered*}" ] && [ -z "${R_CA##Unknown/*}" ]; then
      record PASS "rendered connector is not reported as an attached cache" \
        "DomainRegistered=True/Registered alongside CacheAttached=${R_CA}"
    else
      record FAIL "rendered connector is not reported as an attached cache" \
        "wanted DomainRegistered=True/Registered + CacheAttached=Unknown/*, got: ${R_CONDS}"
    fi

    record SKIP "live engine init behavior when connector cannot initialize" \
      "requires a real serving engine image on an accelerator"
  fi

  release_deployment_in "$TEST_NS" e2e-c27-unreachable
  release_deployment_in "$TEST_NS" e2e-c27-registered
fi

# =================================================================================================
# PHASE 2: Routed P/D 6-object router lifecycle, KVEventsPublishing, and live router endpoint
# =================================================================================================

p2_role() {
  local name="$1" kind="$2" managed="$3"
  printf '  - name: %s\n    kind: %s\n    acceleratorType: %s\n    replicas: 1\n' "$name" "$kind" "$IT"
  printf '    image: %s\n' "$IMAGE"
  [ "$managed" = yes ] || printf '    command: ["/pause"]\n'
}

p2_apply_md() {
  local name="$1" router="$2" roles="$3" engine="${4:-vLLM}"
  {
    cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${TEST_NS}
spec:
  engine:
    name: ${engine}
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
YAML
    if [ "$router" = yes ]; then
      cat <<YAML
  router:
    name: llm-d-router
    image: ${ROUTER_IMAGE}
YAML
    fi
    printf '  roles:\n%s\n' "$roles"
  } | k apply -f -
}

p2_cond_is() {
  local md="$1" status="$2" reason="$3" got
  got="$(k -n "$TEST_NS" get modeldeployment "$md" \
    -o jsonpath='{.status.conditions[?(@.type=="KVEventsPublishing")].status}{"|"}{.status.conditions[?(@.type=="KVEventsPublishing")].reason}' \
    2>/dev/null)"
  [ "$got" = "$status|$reason" ]
}

p2_assert_cond() {
  local md="$1" status="$2" reason="$3" label="$4" got
  if wait_until "$SETTLE" p2_cond_is "$md" "$status" "$reason"; then
    record PASS "$label" "${status}/${reason}"
  else
    got="$(k -n "$TEST_NS" get modeldeployment "$md" \
      -o jsonpath='{.status.conditions[?(@.type=="KVEventsPublishing")].status}{"/"}{.status.conditions[?(@.type=="KVEventsPublishing")].reason}' 2>/dev/null)"
    record FAIL "$label" "wanted ${status}/${reason}, got ${got:-none}"
  fi
}

p2_router_count_is() {
  local expected="$1" total=0 kind
  for kind in deployment configmap service serviceaccount role rolebinding; do
    k -n "$TEST_NS" get "$kind" "${LIFECYCLE}-router" >/dev/null 2>&1 && total=$((total + 1))
  done
  [ "$total" = "$expected" ]
}

p2_router_objects_valid() {
  local uid kind row
  uid="$(k -n "$TEST_NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ -n "$uid" ] || return 1
  for kind in deployment configmap service serviceaccount role rolebinding; do
    row="$(k -n "$TEST_NS" get "$kind" "${LIFECYCLE}-router" \
      -o jsonpath='{.metadata.ownerReferences[0].uid}{"|"}{.metadata.labels.resource\.gpustack\.ai/type}{"|"}{.metadata.annotations.note\.gpustack\.ai/router}' 2>/dev/null)"
    [ "$row" = "$uid|modeldeployments|llm-d-router" ] || return 1
  done
}

p2_starting_without_ready() {
  local phase ready
  phase="$(k -n "$TEST_NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{.status.phase}' 2>/dev/null)"
  ready="$(k -n "$TEST_NS" get modeldeployment "$LIFECYCLE" \
    -o jsonpath='{range .status.roles[*]}{.ready}{" "}{end}' 2>/dev/null)"
  [ "$phase" = Starting ] && ! echo "$ready" | grep -Eq '(^| )1( |$)'
}

p2_producer_publishes_events() {
  local pod command ports
  pod="$(k -n "$TEST_NS" get pods \
    -l "app.kubernetes.io/instance=${LIFECYCLE},app.kubernetes.io/component=prefill" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$pod" ] || return 1
  command="$(k -n "$TEST_NS" get pod "$pod" -o jsonpath='{.spec.containers[0].command}' 2>/dev/null)"
  ports="$(k -n "$TEST_NS" get pod "$pod" \
    -o jsonpath='{range .spec.containers[0].ports[*]}{.containerPort}{" "}{end}' 2>/dev/null)"
  [[ "$command" == *--kv-events-config* ]] && [[ " $ports " == *" 5557 "* ]] && [[ " $ports " == *" 5558 "* ]]
}

p2_lifecycle_gone() {
  ! k -n "$TEST_NS" get modeldeployment "$LIFECYCLE" >/dev/null 2>&1 && p2_router_count_is 0
}

p2_epp_present() {
  local ns="$1" md="$2" raw=""
  raw="$(k -n "$ns" get configmap "${md}-router" -o jsonpath='{.data.epp\.yaml}' 2>/dev/null)"
  [ -n "$raw" ]
}

p2_epp_json() {
  local ns="$1" md="$2" raw=""
  raw="$(k -n "$ns" get configmap "${md}-router" -o jsonpath='{.data.epp\.yaml}' 2>/dev/null)"
  [ -n "$raw" ] || return 1
  printf '%s' "$raw" | python3 -c '
import json, sys, yaml
d = yaml.safe_load(sys.stdin.read())
print(json.dumps(d) if d is not None else "{}")
'
}

p2_assert_health_filter() {
  local ns="$1" md="$2" want_poll="$3" label="$4" doc_json="" params="" placement=""
  if [ "$PICKER_YAML_OK" != yes ]; then
    record SKIP "$label" "pyyaml is absent in the runner"
    return 0
  fi
  if ! wait_until "$SETTLE" p2_epp_present "$ns" "$md"; then
    record FAIL "$label" "configmap/${md}-router epp.yaml absent within ${SETTLE}s"
    return 0
  fi
  doc_json="$(p2_epp_json "$ns" "$md")"
  params="$(printf '%s' "$doc_json" | jq -c '[.plugins[]? | select(.type == "endpoint-health-filter") | .parameters] | first // empty')"
  if [ -z "$params" ]; then
    record FAIL "$label" "endpoint-health-filter plugin absent in configmap/${md}-router"
    return 0
  fi
  if ! printf '%s' "$params" | jq -e --argjson poll "$want_poll" '
      .pollFailureThreshold == $poll
      and .responseFailureThreshold == 5
      and .responseFailureCodes == [502, 503]
      and .maxEjectionPercent == 50
    ' >/dev/null 2>&1; then
    record FAIL "$label" "unexpected endpoint-health-filter parameters: ${params}"
    return 0
  fi
  placement="$(printf '%s' "$doc_json" | jq -r '
    [ .schedulingProfiles[]?
      | select(.name == "server" or .name == "prefill" or .name == "decode")
      | select((.plugins[0].pluginRef != (.name + "-pods")) or (.plugins[1].pluginRef != "endpoint-health-filter"))
      | "\(.name):[\((.plugins // []) | map(.pluginRef) | join(","))]"
    ] | join(" ")
  ')"
  if [ -n "$placement" ]; then
    record FAIL "$label" "endpoint-health-filter not immediately after role filter: ${placement}"
  else
    record PASS "$label" "pollFailureThreshold=${want_poll}, responseFailureThreshold=5, codes=[502,503], maxEjectionPercent=50"
  fi
}

if [ "$FIXTURE_OK" = yes ]; then
  p2_pair="$(p2_role prefill Prefill yes)
$(p2_role decode Decode yes)"

  p2_apply_md "$LIFECYCLE" no "$p2_pair" >/dev/null
  p2_assert_cond "$LIFECYCLE" False NoRouter "unrouted P/D pair reports KVEventsPublishing=False/NoRouter"

  k -n "$TEST_NS" patch modeldeployment "$LIFECYCLE" --type=merge \
    -p '{"spec":{"router":{"name":"llm-d-router","image":"'"$ROUTER_IMAGE"'"}}}' >/dev/null
  if wait_until "$SETTLE" p2_router_count_is 6 && p2_router_objects_valid; then
    record PASS "adding spec.router converges all six owned router objects" "6/6 owned and discoverable"
  else
    record FAIL "adding spec.router converges all six owned router objects" "missing object, ownerRef, or router note"
  fi

  if wait_until "$SETTLE" p2_starting_without_ready; then
    ready="$(k -n "$TEST_NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{range .status.roles[*]}{.ready}{" "}{end}' 2>/dev/null)"
    record PASS "no-accelerator routed deployment stays Starting" "phase=Starting, ready=[${ready:-none}]"
  else
    phase="$(k -n "$TEST_NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{.status.phase}' 2>/dev/null)"
    record FAIL "no-accelerator routed deployment stays Starting" "phase=${phase:-none}"
  fi

  p2_assert_cond "$LIFECYCLE" True Publishing "managed routed P/D pair reports KVEventsPublishing=True/Publishing"

  if wait_until "$SETTLE" p2_producer_publishes_events; then
    record PASS "prefill producer Pod carries --kv-events-config and ports 5557/5558" "argument and both event ports rendered"
  else
    record FAIL "prefill producer Pod carries --kv-events-config and ports 5557/5558" "argument or event port absent"
  fi

  p2_apply_md "$DISABLED" yes "$(p2_role server Server yes)" SGLang >/dev/null
  p2_assert_cond "$DISABLED" False PublisherDisabled "routed SGLang server reports KVEventsPublishing=False/PublisherDisabled"

  k -n "$TEST_NS" patch modeldeployment "$LIFECYCLE" --type=json \
    -p='[{"op":"remove","path":"/spec/router"}]' >/dev/null
  if wait_until "$SETTLE" p2_router_count_is 0; then
    record PASS "removing spec.router prunes all six router objects" "0/6 remaining"
  else
    record FAIL "removing spec.router prunes all six router objects" "one or more router objects remain"
  fi

  k -n "$TEST_NS" patch modeldeployment "$LIFECYCLE" --type=merge \
    -p '{"spec":{"router":{"name":"llm-d-router","image":"'"$ROUTER_IMAGE"'"}}}' >/dev/null
  if wait_until "$SETTLE" p2_router_count_is 6; then
    record PASS "re-adding spec.router restores all six router objects" "6/6 present"
  else
    record FAIL "re-adding spec.router restores all six router objects" "one or more router objects absent"
  fi

  p2_apply_md "$SERVER" no "$(p2_role server Server no)" >/dev/null
  p2_assert_cond "$SERVER" True NotApplicable "unrouted server-only deployment reports KVEventsPublishing=True/NotApplicable"

  p2_apply_md "$UNMANAGED" yes "$(p2_role prefill Prefill no)
$(p2_role decode Decode no)" >/dev/null
  p2_assert_cond "$UNMANAGED" Unknown RoleUnmanaged "unmanaged P/D pair reports KVEventsPublishing=Unknown/RoleUnmanaged"
  p2_assert_health_filter "$TEST_NS" "$UNMANAGED" 0 \
    "unmanaged role epp.yaml disables active health polling (pollFailureThreshold=0)"

  p2_apply_md "$ROUTED_SERVER" yes "$(p2_role server Server yes)" >/dev/null
  p2_assert_cond "$ROUTED_SERVER" True Publishing "routed server reports KVEventsPublishing=True/Publishing"
  p2_assert_health_filter "$TEST_NS" "$ROUTED_SERVER" 3 \
    "routed server epp.yaml attaches endpoint-health-filter after server-pods"

  k -n "$TEST_NS" delete modeldeployment "$LIFECYCLE" --wait=false >/dev/null
  if wait_until "$SETTLE" p2_lifecycle_gone; then
    record PASS "deleting ModelDeployment garbage-collects all six router objects" "owner and 6 dependents absent"
  else
    record FAIL "deleting ModelDeployment garbage-collects all six router objects" "owner or router dependent remains"
  fi

  for md in "$SERVER" "$UNMANAGED" "$ROUTED_SERVER" "$DISABLED"; do
    release_deployment_in "$TEST_NS" "$md"
  done
fi

# Live router readiness and published endpoint (in OPERATOR_NS with default router image).
k apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${ROUTER_READY}
  namespace: ${OPERATOR_NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: e2e-c27-no-such-binding
  router:
    name: llm-d-router
  roles:
  - name: prefill
    kind: Prefill
    acceleratorType: ${IT}
    replicas: 1
    image: ${IMAGE}
  - name: decode
    kind: Decode
    acceleratorType: ${IT}
    replicas: 1
    image: ${IMAGE}
YAML

router_ready=no
for _ in $(seq 1 "$((ROUTER_SETTLE / 3))"); do
  if [ "$(k -n "$OPERATOR_NS" get deployment "${ROUTER_READY}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = 1 ]; then
    router_ready=yes
    break
  fi
  sleep 3
done

epp_args="$(k -n "$OPERATOR_NS" get deployment "${ROUTER_READY}-router" \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="epp")].args[*]}' 2>/dev/null)"
case " $epp_args " in
  *" --allow-experimental-plugins=true "*)
    record PASS "llm-d-router epp container args include --allow-experimental-plugins=true" \
      "present on deployment/${ROUTER_READY}-router"
    ;;
  *)
    record FAIL "llm-d-router epp container args include --allow-experimental-plugins=true" \
      "args=[${epp_args:-none}]"
    ;;
esac

p2_assert_health_filter "$OPERATOR_NS" "$ROUTER_READY" 3 \
  "llm-d-router epp.yaml configures endpoint-health-filter after role filter on prefill/decode"

if [ "$router_ready" != yes ]; then
  waiting="$(k -n "$OPERATOR_NS" get pods -l "modeldeployment.gpustack.ai/router=llm-d-router,app.kubernetes.io/instance=${ROUTER_READY}" \
    -o jsonpath='{range .items[*].status.containerStatuses[*]}{.name}{"="}{.state.waiting.reason}{" "}{end}' 2>/dev/null)"
  case "$waiting" in
    *ErrImagePull* | *ImagePullBackOff* | *InvalidImageName*)
      record SKIP "router readiness and published endpoint" "upstream router image could not be pulled: ${waiting}"
      ;;
    *)
      record FAIL "router readiness and published endpoint" "router not Ready in ${ROUTER_SETTLE}s; waiting=[${waiting:-none}]"
      ;;
  esac
else
  want_ep="http://${ROUTER_READY}-router.${OPERATOR_NS}.svc:8000"
  got_ep=""
  for _ in $(seq 1 40); do
    got_ep="$(k -n "$OPERATOR_NS" get modeldeployment "$ROUTER_READY" -o jsonpath='{.status.endpoint}' 2>/dev/null)"
    [ "$got_ep" = "$want_ep" ] && break
    sleep 3
  done
  if [ "$got_ep" = "$want_ep" ]; then
    record PASS "ready router becomes the published status.endpoint" "${got_ep}"
    rpod="$(k -n "$OPERATOR_NS" get pods \
      -l "modeldeployment.gpustack.ai/router=llm-d-router,app.kubernetes.io/instance=${ROUTER_READY}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    obs_rc=0
    obs_body="$(k get --raw "/api/v1/namespaces/${OPERATOR_NS}/pods/http:${rpod}:9090/proxy/observer/endpoints" 2>&1)" || obs_rc=$?
    if [ "$obs_rc" -eq 0 ] && printf '%s' "$obs_body" | jq -e '(.workers | type == "array") and ([.workers[]? | select(.selection.circuit_open == true)] | length == 0)' >/dev/null 2>&1; then
      obs_count="$(printf '%s' "$obs_body" | jq -r '.workers | length')"
      record PASS "llm-d-router :9090/observer/endpoints responds 200 with conservative circuit_open=false" \
        "workers=${obs_count}, circuit_open_true=0"
    else
      record FAIL "llm-d-router :9090/observer/endpoints responds 200 with conservative circuit_open=false" \
        "rc=${obs_rc} body=$(printf '%s' "$obs_body" | tr '\n' ' ' | cut -c1-180)"
    fi
    envoy_r0="$(k -n "$OPERATOR_NS" get pod "$rpod" -o jsonpath='{.status.containerStatuses[?(@.name=="envoy")].restartCount}' 2>/dev/null)"
    epp_r0="$(k -n "$OPERATOR_NS" get pod "$rpod" -o jsonpath='{.status.containerStatuses[?(@.name=="epp")].restartCount}' 2>/dev/null)"
    stable=yes
    for _ in $(seq 1 "$((STABLE / 3))"); do
      sleep 3
      cur_ready="$(k -n "$OPERATOR_NS" get deployment "${ROUTER_READY}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
      cur_envoy="$(k -n "$OPERATOR_NS" get pod "$rpod" -o jsonpath='{.status.containerStatuses[?(@.name=="envoy")].restartCount}' 2>/dev/null)"
      cur_epp="$(k -n "$OPERATOR_NS" get pod "$rpod" -o jsonpath='{.status.containerStatuses[?(@.name=="epp")].restartCount}' 2>/dev/null)"
      if [ "$cur_ready" != 1 ] || [ "$cur_envoy" != "$envoy_r0" ] || [ "$cur_epp" != "$epp_r0" ]; then
        stable=no
        break
      fi
    done
    if [ "$stable" = yes ]; then
      record PASS "router readiness remains stable" "${STABLE}s with zero container restarts"
    else
      record FAIL "router readiness remains stable" "ready=${cur_ready:-none} envoy=${envoy_r0}->${cur_envoy:-none} epp=${epp_r0}->${cur_epp:-none}"
    fi
  else
    record FAIL "ready router becomes the published status.endpoint" "wanted ${want_ep}, got ${got_ep:-none}"
  fi
fi

release_deployment_in "$OPERATOR_NS" "$ROUTER_READY"

# Lightweight External DP Server epp.yaml render check (no GPU required).
k apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${EXTERNAL_DP}
  namespace: ${OPERATOR_NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
    version: "0.19.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: llm-d-router
    image: ${ROUTER_IMAGE}
  roles:
  - name: server
    kind: Server
    acceleratorType: ${IT}
    replicas: 1
    size: 2
    image: ${IMAGE}
    engineVersion: "0.19.0"
    extraArgs:
      - --data-parallel-size
      - "2"
      - --data-parallel-rank
      - \$(GPUSTACK_MEMBER_INDEX)
YAML

if [ "$PICKER_YAML_OK" != yes ]; then
  record SKIP "external DP server epp.yaml omits prefix-cache-scorer and retains health/load scorers" \
    "pyyaml is absent in the runner"
elif ! wait_until "$SETTLE" p2_epp_present "$OPERATOR_NS" "$EXTERNAL_DP"; then
  record FAIL "external DP server epp.yaml omits prefix-cache-scorer and retains health/load scorers" \
    "configmap/${EXTERNAL_DP}-router epp.yaml absent within ${SETTLE}s"
else
  ext_dp_json="$(p2_epp_json "$OPERATOR_NS" "$EXTERNAL_DP")"
  ext_dp_refs="$(printf '%s' "$ext_dp_json" | jq -c '[.schedulingProfiles[]? | select(.name == "server") | .plugins[]? | .pluginRef]')"
  if [ "$ext_dp_refs" = '["server-pods","endpoint-health-filter","queue-scorer","kv-cache-utilization-scorer","max-score-picker"]' ]; then
    record PASS "external DP server epp.yaml omits prefix-cache-scorer and retains health/load scorers" \
      "server plugins=${ext_dp_refs}"
  else
    record FAIL "external DP server epp.yaml omits prefix-cache-scorer and retains health/load scorers" \
      "got server plugins=${ext_dp_refs:-none}"
  fi
fi

release_deployment_in "$OPERATOR_NS" "$EXTERNAL_DP"

# =================================================================================================
# PHASE 3: EPD encoder Services, router picker document, and shared-claim filesystem round-trip
# =================================================================================================

EPD_IT="$(epd_platform_type "$EPD_PLATFORM")"
EPD_NODE=""
if [ -z "$EPD_IT" ]; then
  EPD_NODE="$(epd_pick_worker_node)"
  if [ -n "$EPD_NODE" ]; then
    echo "[case-27] injecting CPU vendor '${EPD_VENDOR}' on node ${EPD_NODE} for EPD topology checks"
    epd_setup_cpu_vendor_fixture "$EPD_VENDOR" "$OPERATOR_NS" "$EPD_NF" "$EPD_SETTLE_ROUNDS" "$SYSTEM_NS" "$EPD_NODE"
    EPD_IT="$EPD_FIXTURE_TYPE"
  fi
fi

if [ -z "$EPD_IT" ]; then
  record SKIP "Phase 3 EPD encoder Services, router picker, and shared-claim checks" \
    "no AcceleratorType reported manufacturer '${EPD_PLATFORM}' within $((EPD_SETTLE_ROUNDS * 5))s"
else
  [ -n "$EPD_NODE" ] || EPD_NODE="$(k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -n 1)"

  ENTRANCE="$(k get acceleratortypes.worker.gpustack.ai "$EPD_IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
  [ -n "$ENTRANCE" ] || ENTRANCE="$(k get instancetypes.worker.gpustack.ai "$EPD_IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
  ENTRANCE_LQ=""
  for _ in $(seq 1 "$EPD_SETTLE_ROUNDS"); do
    ENTRANCE_LQ="$(k -n "$OPERATOR_NS" get localqueue "${ENTRANCE:-}" -o name 2>/dev/null)"
    [ -n "$ENTRANCE_LQ" ] && break
    sleep 5
  done

  if [ -z "$ENTRANCE_LQ" ]; then
    record SKIP "Phase 3 EPD encoder topology checks" \
      "AcceleratorType ${EPD_IT} entrance LocalQueue '${ENTRANCE:-<none>}' not found in ${OPERATOR_NS}"
  else
    record PASS "EPD platform pool and entrance LocalQueue ready" "${EPD_IT} (${EPD_PLATFORM}) -> ${ENTRANCE_LQ##*/}"

    k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${EPD_PV}
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
    path: /tmp/gpustack-e2e-c27
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${EPD_CLAIM}
  namespace: ${OPERATOR_NS}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: ${EPD_PV}
YAML

    EPD_APPLY_OUT="$(k apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${EPD_MD}
  namespace: ${OPERATOR_NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
      additionalVolumes:
        - mountPath: ${EPD_MOUNT}
          persistentVolumeClaim:
            claimName: ${EPD_CLAIM}
    - name: prefill
      kind: Prefill
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
      additionalVolumes:
        - mountPath: ${EPD_MOUNT}
          persistentVolumeClaim:
            claimName: ${EPD_CLAIM}
    - name: decode
      kind: Decode
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"

    if ! k -n "$OPERATOR_NS" get modeldeployments.worker.gpustack.ai "$EPD_MD" -o name >/dev/null 2>&1; then
      record FAIL "EPD encoder deployment admitted and created" \
        "absent after apply: $(printf '%s' "$EPD_APPLY_OUT" | tr '\n' ' ' | cut -c1-200)"
    else
      record PASS "EPD encoder deployment admitted and created" "${EPD_MD} on ${EPD_IT}"

      EPD_PODS=0
      for _ in $(seq 1 "$EPD_POD_ROUNDS"); do
        EPD_PODS="$(k -n "$OPERATOR_NS" get pods -l "app.kubernetes.io/instance=${EPD_MD}" --no-headers 2>/dev/null | grep -c . || true)"
        [ "${EPD_PODS:-0}" -ge 3 ] && break
        sleep 5
      done
      if [ "${EPD_PODS:-0}" -ge 3 ]; then
        record PASS "all three EPD role Pods rendered" "${EPD_PODS} Pods"
      else
        record FAIL "all three EPD role Pods rendered" "only ${EPD_PODS:-0}/3 Pods within $((EPD_POD_ROUNDS * 5))s"
      fi

      p3_selector_string() {
        # shellcheck disable=SC2016 # Go template variables must not expand in shell.
        k -n "$OPERATOR_NS" get svc "$1" -o go-template='{{range $k,$v := .spec.selector}}{{$k}}={{$v}},{{end}}' 2>/dev/null \
          | sed 's/,$//'
      }

      p3_selected_components() {
        [ -n "$1" ] || return 0
        k -n "$OPERATOR_NS" get pods -l "$1" \
          -o jsonpath='{range .items[*]}{.metadata.labels.app\.kubernetes\.io/component}{"\n"}{end}' 2>/dev/null \
          | sort -u | tr '\n' ' '
      }

      p3_selector_has() {
        case ",$1," in
          *",$2=$3,"*) return 0 ;;
        esac
        return 1
      }

      p3_selector_fronts_only() {
        local sel="$1" comp="$2" comps
        SELECT_REASON=""
        if [ -z "$sel" ]; then
          SELECT_REASON="carries no selector"
          return 1
        fi
        if ! p3_selector_has "$sel" app.kubernetes.io/component "$comp"; then
          SELECT_REASON="missing component=${comp}"
          return 1
        fi
        if ! p3_selector_has "$sel" app.kubernetes.io/instance "$EPD_MD" ||
          ! p3_selector_has "$sel" app.kubernetes.io/name model-deployment; then
          SELECT_REASON="does not pin instance and name labels"
          return 1
        fi
        comps="$(p3_selected_components "$sel")"
        case "$comps" in
          "$comp ")
            SELECT_REASON="resolves to ${comp} Pod(s) alone"
            return 0
            ;;
          "")
            if p3_selector_has "$sel" modeldeployment.gpustack.ai/endpoint-eligible true; then
              SELECT_REASON="resolves to no Pod due to designed endpoint-eligible withdrawal"
              return 0
            fi
            SELECT_REASON="resolves to no Pod without endpoint-eligible term"
            return 1
            ;;
          *)
            SELECT_REASON="resolves to [${comps}], not ${comp} alone"
            return 1
            ;;
        esac
      }

      p3_role_svc_check() {
        if [ -z "$(k -n "$OPERATOR_NS" get svc "$1" -o name 2>/dev/null)" ]; then
          record FAIL "$3" "no svc/${1} exists"
          return 0
        fi
        if p3_selector_fronts_only "$(p3_selector_string "$1")" "$2"; then
          record PASS "$3" "svc/${1} ${SELECT_REASON}"
        else
          record FAIL "$3" "svc/${1} selector ${SELECT_REASON}"
        fi
      }

      p3_role_svc_check "${EPD_MD}-encode" encode "encoder Service exists and fronts only encode"
      p3_role_svc_check "${EPD_MD}-prefill" prefill "prefiller Service exists and fronts only prefill"
      p3_role_svc_check "${EPD_MD}-decode" decode "decoder Service exists and fronts only decode"

      if p3_selector_fronts_only "$(p3_selector_string "${EPD_MD}-prefill")" encode; then
        record FAIL "control: Service check refuses another role's selector" "prefill selector accepted as encode"
      else
        record PASS "control: Service check refuses another role's selector" "refused: ${SELECT_REASON}"
      fi

      DANGLING="app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${EPD_MD},app.kubernetes.io/component=encode,e2e-c27.gpustack.ai/no-such-label=true"
      if p3_selector_fronts_only "$DANGLING" encode; then
        record FAIL "control: Service check refuses unexplained empty selector" "accepted dangling selector"
      else
        record PASS "control: Service check refuses unexplained empty selector" "refused: ${SELECT_REASON}"
      fi

      if [ -z "$(k -n "$OPERATOR_NS" get svc "$EPD_MD" -o name 2>/dev/null)" ]; then
        record FAIL "deployment-wide Service fronts prefill, never encode" "no svc/${EPD_MD}"
      else
        FRONT_COMPS="$(p3_selected_components "$(p3_selector_string "$EPD_MD")")"
        case "$FRONT_COMPS" in
          *encode*) record FAIL "deployment-wide Service fronts prefill, never encode" "resolves to [${FRONT_COMPS}]" ;;
          *prefill*) record PASS "deployment-wide Service fronts prefill, never encode" "svc/${EPD_MD} resolves to [${FRONT_COMPS}] with encode first" ;;
          *) record FAIL "deployment-wide Service fronts prefill, never encode" "resolves to [${FRONT_COMPS:-none}]" ;;
        esac
      fi

      RS=""
      for _ in $(seq 1 "$EPD_STATUS_ROUNDS"); do
        RS="$(k -n "$OPERATOR_NS" get modeldeployments.worker.gpustack.ai "$EPD_MD" -o jsonpath='{.status.roleSummary}' 2>/dev/null)"
        printf '%s' "$RS" | command grep -Eq '^[0-9]+R[0-9]+E[0-9]+P[0-9]+D$' && break
        sleep 5
      done
      if printf '%s' "$RS" | command grep -Eq '^[0-9]+R[0-9]+E[0-9]+P[0-9]+D$'; then
        record PASS "roleSummary reports fixed order R/E/P/D" "${RS}"
      else
        record FAIL "roleSummary reports fixed order R/E/P/D" "got '${RS:-<unset>}'"
      fi

      RKR=""
      for _ in $(seq 1 "$EPD_STATUS_ROUNDS"); do
        RKR="$(k -n "$OPERATOR_NS" get modeldeployments.worker.gpustack.ai "$EPD_MD" \
          -o jsonpath='{.status.conditions[?(@.type=="RoleKindsReady")].reason}' 2>/dev/null)"
        case "$RKR" in KindsNotReady | AllKindsReady) break ;; esac
        sleep 5
      done
      case "$RKR" in
        KindsNotReady | AllKindsReady) record PASS "RoleKindsReady accounts the encoder role" "reason=${RKR}" ;;
        *) record FAIL "RoleKindsReady accounts the encoder role" "reason='${RKR:-<none>}'" ;;
      esac

      EP="$(k -n "$OPERATOR_NS" get modeldeployments.worker.gpustack.ai "$EPD_MD" -o jsonpath='{.status.endpoint}' 2>/dev/null)"
      if [ -z "$EP" ]; then
        record SKIP "status.endpoint never names the encoder" "no endpoint published yet (router not Ready)"
      else
        case "$EP" in
          *encode*) record FAIL "status.endpoint never names the encoder" "endpoint '${EP}' names encode" ;;
          *) record PASS "status.endpoint never names the encoder" "endpoint '${EP}'" ;;
        esac
      fi

      HDR="$(k -n "$OPERATOR_NS" get modeldeployments.v1.worker.gpustack.ai "$EPD_MD" 2>/dev/null | head -n 1)"
      case "$HDR" in
        *ENGINE*ROLES*PHASE*ENDPOINT*) record PASS "v1 table carries ENDPOINT column" "${HDR}" ;;
        *) record FAIL "v1 table carries ENDPOINT column" "missing ENDPOINT in '${HDR:-<empty>}'" ;;
      esac

      # Control P/D deployment (no Encode role) + EndpointPickerConfig verification.
      PD_APPLY_OUT="$(k apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${EPD_PD}
  namespace: ${OPERATOR_NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
    - name: decode
      kind: Decode
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"
      pd_count=0
      for _ in $(seq 1 "$EPD_POD_ROUNDS"); do
        pd_count="$(k -n "$OPERATOR_NS" get pods -l "app.kubernetes.io/instance=${EPD_PD}" --no-headers 2>/dev/null | grep -c . || true)"
        [ "${pd_count:-0}" -ge 2 ] && break
        sleep 5
      done
      if [ "${pd_count:-0}" -ge 2 ]; then
        record PASS "control P/D deployment rendered its role Pods" "${pd_count} Pods"
      else
        record FAIL "control P/D deployment rendered its role Pods" \
          "only ${pd_count:-0}/2 Pods; apply: $(printf '%s' "$PD_APPLY_OUT" | tr '\n' ' ' | cut -c1-160)"
      fi

      p3_picker_yaml() {
        k -n "$OPERATOR_NS" get configmap "${1}-router" -o jsonpath='{.data.epp\.yaml}' 2>/dev/null
      }

      p3_json_of() {
        python3 -c '
import json, sys, yaml
d = yaml.safe_load(sys.stdin.read())
print(json.dumps(d) if d is not None else "{}")
'
      }

      if [ "$PICKER_YAML_OK" != yes ]; then
        record SKIP "EPD EndpointPickerConfig router-stage checks" "pyyaml is absent in the runner"
      else
        DOC="$(p3_picker_yaml "$EPD_MD")"
        if [ -z "$DOC" ]; then
          record FAIL "router ConfigMap carries EndpointPickerConfig" "configmap/${EPD_MD}-router epp.yaml absent"
        else
          DOC_JSON="$(printf '%s' "$DOC" | p3_json_of)"
          PD_RAW="$(p3_picker_yaml "$EPD_PD")"
          PD_JSON=""
          [ -n "$PD_RAW" ] && PD_JSON="$(printf '%s' "$PD_RAW" | p3_json_of)"

          if printf '%s' "$DOC_JSON" | jq -e '.kind == "EndpointPickerConfig" and (.plugins | type == "array")' >/dev/null 2>&1; then
            record PASS "router ConfigMap carries EndpointPickerConfig" \
              "$(printf '%s' "$DOC_JSON" | jq -r '.plugins | length') plugins"
          else
            record FAIL "router ConfigMap carries EndpointPickerConfig" "invalid EndpointPickerConfig"
          fi

          FILTER_OK="$(printf '%s' "$DOC_JSON" | jq -r '[.plugins[]? | select(.type == "label-selector-filter") | select(.parameters.matchLabels["modeldeployment.gpustack.ai/role-kind"] == "encode")] | length')"
          if [ "${FILTER_OK:-0}" -ge 1 ]; then
            record PASS "encode filter selects role-kind=encode" "filter present"
          else
            record FAIL "encode filter selects role-kind=encode" "missing encode label-selector-filter"
          fi

          ENCODE_PLUGINS="$(printf '%s' "$DOC_JSON" | jq -c '[.schedulingProfiles[]? | select(.name == "encode") | .plugins[]? | .pluginRef] | unique')"
          if [ "$ENCODE_PLUGINS" = '["encode-pods"]' ]; then
            record PASS "encode profile uses only encode-pods filter" "${ENCODE_PLUGINS}"
          else
            record FAIL "encode profile uses only encode-pods filter" "got ${ENCODE_PLUGINS}"
          fi

          DECIDER="$(printf '%s' "$DOC_JSON" | jq -r '[.plugins[]? | select(.type == "always-disagg-multimodal-decider")] | length')"
          if [ "${DECIDER:-0}" -ge 1 ]; then
            record PASS "always-disagg-multimodal-decider is present" "decider count=${DECIDER}"
          else
            record FAIL "always-disagg-multimodal-decider is present" "decider absent"
          fi

          HANDLER="$(printf '%s' "$DOC_JSON" | jq -c '[.plugins[]? | select(.type == "disagg-profile-handler") | .parameters] | first // {}')"
          H_ENC="$(printf '%s' "$HANDLER" | jq -r '.profiles.encode // ""')"
          H_PRE="$(printf '%s' "$HANDLER" | jq -r '.profiles.prefill // ""')"
          H_DEC="$(printf '%s' "$HANDLER" | jq -r '.deciders.encode // ""')"
          if [ "$H_ENC" = encode ] && [ "$H_PRE" = prefill ] && [ "$H_DEC" = always-disagg-multimodal-decider ]; then
            record PASS "disagg-profile-handler routes encode stage to multimodal decider" "encode+prefill wired"
          else
            record FAIL "disagg-profile-handler routes encode stage to multimodal decider" "encode=${H_ENC} prefill=${H_PRE} decider=${H_DEC}"
          fi

          DISCOVERY="$(printf '%s' "$DOC_JSON" | jq -r '[.plugins[]? | select(.type == "precise-prefix-cache-producer") | .parameters.kvEventsConfig.podDiscoveryConfig.podLabelSelector // ""] | first // ""')"
          case "$DISCOVERY" in
            *role-kind!=decode*role-kind!=encode* | *role-kind!=encode*role-kind!=decode*)
              record PASS "prefix-cache producer excludes decode and encode kinds" "${DISCOVERY}"
              ;;
            "")
              APPROX="$(printf '%s' "$DOC_JSON" | jq -r '[.plugins[]? | select(.type == "approx-prefix-cache-producer")] | length')"
              if [ "${APPROX:-0}" -ge 1 ]; then
                record PASS "prefix-cache producer excludes decode and encode kinds" "approx-prefix-cache-producer rendered"
              else
                record FAIL "prefix-cache producer excludes decode and encode kinds" "no prefix-cache producer present"
              fi
              ;;
            *)
              record FAIL "prefix-cache producer excludes decode and encode kinds" "selector '${DISCOVERY}' missing exclusions"
              ;;
          esac

          if [ -z "$PD_JSON" ]; then
            record SKIP "control P/D deployment renders no encoder stage" "configmap/${EPD_PD}-router epp.yaml absent"
          else
            C_DEC="$(printf '%s' "$PD_JSON" | jq -r '[.plugins[]? | select(.type == "always-disagg-multimodal-decider")] | length')"
            C_PRO="$(printf '%s' "$PD_JSON" | jq -r '[.schedulingProfiles[]? | select(.name == "encode")] | length')"
            C_FIL="$(printf '%s' "$PD_JSON" | jq -r '[.plugins[]? | select(.type == "label-selector-filter") | select(.parameters.matchLabels["modeldeployment.gpustack.ai/role-kind"] == "encode")] | length')"
            if [ "$C_DEC" -eq 0 ] && [ "$C_PRO" -eq 0 ] && [ "$C_FIL" -eq 0 ]; then
              record PASS "control P/D deployment renders no encoder stage" "decider=0 profile=0 filter=0"
            else
              record FAIL "control P/D deployment renders no encoder stage" "decider=${C_DEC} profile=${C_PRO} filter=${C_FIL}"
            fi
          fi
        fi
      fi

      # Shared PVC mounts and witness filesystem round-trip.
      ENCODE_CLAIM="$(epd_role_pod_claim "$OPERATOR_NS" "$EPD_MD" encode)"
      PREFILL_CLAIM="$(epd_role_pod_claim "$OPERATOR_NS" "$EPD_MD" prefill)"
      DECODE_CLAIM="$(epd_role_pod_claim "$OPERATOR_NS" "$EPD_MD" decode)"

      case "$ENCODE_CLAIM" in
        "${EPD_CLAIM}|${EPD_MOUNT}|false") record PASS "encoder Pod mounts shared claim writable" "${ENCODE_CLAIM}" ;;
        *) record FAIL "encoder Pod mounts shared claim writable" "got '${ENCODE_CLAIM:-none}'" ;;
      esac
      case "$PREFILL_CLAIM" in
        "${EPD_CLAIM}|${EPD_MOUNT}|"*) record PASS "prefiller Pod mounts same claim and path" "${PREFILL_CLAIM}" ;;
        *) record FAIL "prefiller Pod mounts same claim and path" "got '${PREFILL_CLAIM:-none}'" ;;
      esac
      if [ -z "$DECODE_CLAIM" ]; then
        record PASS "decoder Pod mounts no claim" "no PVC mount"
      else
        record FAIL "decoder Pod mounts no claim" "unexpected mount '${DECODE_CLAIM}'"
      fi

      PVC_PHASE="$(k -n "$OPERATOR_NS" get pvc "$EPD_CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null)"
      if [ "$PVC_PHASE" = Bound ]; then
        record PASS "shared PVC is Bound" "pvc/${EPD_CLAIM} -> pv/${EPD_PV}"
        TOKEN="e2e-c27-$(date +%s)"
        k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: e2e-c27-witness-write
  namespace: ${OPERATOR_NS}
spec:
  restartPolicy: Never
  tolerations:
    - operator: Exists
  nodeSelector:
    kubernetes.io/hostname: ${EPD_NODE}
  containers:
    - name: write
      image: ${WITNESS_IMAGE}
      command: ["sh", "-c", "printf '%s' '${TOKEN}' > /shared/written-by-other-mount"]
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: ${EPD_CLAIM}
YAML
        W_OK=no
        for _ in $(seq 1 "$EPD_POD_ROUNDS"); do
          case "$(k -n "$OPERATOR_NS" get pod e2e-c27-witness-write -o jsonpath='{.status.phase}' 2>/dev/null)" in
            Succeeded) W_OK=yes; break ;;
            Failed) break ;;
          esac
          case "$(epd_pod_pull_reason "$OPERATOR_NS" e2e-c27-witness-write)" in ImagePullBackOff | ErrImagePull) break ;; esac
          sleep 5
        done
        if [ "$W_OK" != yes ]; then
          case "$(epd_pod_pull_reason "$OPERATOR_NS" e2e-c27-witness-write)" in
            ImagePullBackOff | ErrImagePull)
              record SKIP "shared claim witness round-trip" "witness image '${WITNESS_IMAGE}' could not be pulled"
              ;;
            *)
              record FAIL "shared claim witness round-trip" "writer witness did not succeed"
              ;;
          esac
        else
          k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: e2e-c27-witness-read
  namespace: ${OPERATOR_NS}
spec:
  restartPolicy: Never
  tolerations:
    - operator: Exists
  nodeSelector:
    kubernetes.io/hostname: ${EPD_NODE}
  containers:
    - name: read
      image: ${WITNESS_IMAGE}
      command: ["sh", "-c", "cat /shared/written-by-other-mount"]
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: ${EPD_CLAIM}
YAML
          R_OK=no
          for _ in $(seq 1 "$EPD_POD_ROUNDS"); do
            case "$(k -n "$OPERATOR_NS" get pod e2e-c27-witness-read -o jsonpath='{.status.phase}' 2>/dev/null)" in
              Succeeded | Failed) R_OK=yes; break ;;
            esac
            case "$(epd_pod_pull_reason "$OPERATOR_NS" e2e-c27-witness-read)" in ImagePullBackOff | ErrImagePull) break ;; esac
            sleep 5
          done
          READ_BACK=""
          [ "$R_OK" = yes ] && READ_BACK="$(k -n "$OPERATOR_NS" logs e2e-c27-witness-read -c read 2>/dev/null)"
          if [ "$READ_BACK" = "$TOKEN" ]; then
            record PASS "shared claim witness round-trip" "exchanged '${READ_BACK}' through pvc/${EPD_CLAIM}"
          else
            record FAIL "shared claim witness round-trip" "read '${READ_BACK:-<nothing>}', expected '${TOKEN}'"
          fi
        fi
      else
        record FAIL "shared PVC is Bound" "phase='${PVC_PHASE:-<none>}'"
      fi
    fi
  fi
fi

kvi_results "$CASE_ID"
