#!/usr/bin/env bash
#
# CASE 26 — Unified ModelDeployment admission refusal matrix: single-role, multi-role,
#           engine.image, SGLang external DP RouterRequired, EPD encoder matrix, and
#           online runtimeControl & scaler-binding coexistence
#   (MUTATING, self-cleaning: server-side dry-runs plus short-lived live deployments,
#    a ModelDeploymentScaler, and an optional CPU-vendor NodeFeature fixture; all restored
#    by the EXIT trap)
#
#   case-26.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MD_BINDING,E2E_MD_CONTROL_IMAGE,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_ACCELERATOR_TYPE,E2E_SYSTEM_NS,E2E_C26_SETTLE,E2E_C26_VENDOR,E2E_C26_NVIDIA_VENDOR
# Goal:        Verify the full ModelDeployment admission surface across all four admission layers
#              (CRD openAPIV3Schema, mutating defaulting patch, validating webhooks, and controller
#              conditions):
#                1. Single-role and multi-role rules, live stored-object update rules (size/replicas
#                   mutable, spec.model frozen without concurrency/lock wording), and multi-role
#                   replicas scaling preserving living Pod UIDs (with image-change replacement control).
#                2. Required spec.engine.image schema contract (including defaulting-patch vs no-patch
#                   wordings and role-image non-substitution) and SGLang external DP RouterRequired.
#                3. EPD encoder shape, claim-source, platform-matrix (Ascend shared-directory and
#                   NVIDIA/AMD network legs), and vllm-router --epd-config ownership rules.
#                4. Online runtimeControl admission: reserved loraAdapters, scope gates, 15-key
#                   Translator registry validation, router isolation flags, folded Elastic EP
#                   immutability/monotonicity, sentinel hardening, and scaler-binding coexistence.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an operator
#              carrying the ModelDeployment and ModelDeploymentScaler CRDs. No GPU is required;
#              no KVCachePoolBinding has to exist. jq must be on PATH. EPD platform-matrix rows
#              inject a temporary CPU-vendor NodeFeature + awareness setting if no matching
#              AcceleratorType exists, and AUTO-SKIP if NFD declines the override.
# Inputs:      REAL: CRD schema, mutating/validating webhooks, ModelDeployment and
#              ModelDeploymentScaler controllers. MOCKED (only during Phase 3 when no real
#              platform AcceleratorType exists): one NodeFeature cpu-model.vendor_id label and the
#              instance-type-aware-cpu-manufacturer setting, both restored by the EXIT trap.
# Expected:    Every baseline manifest is accepted, every invalid manifest is refused from the
#              expected layer with its distinguishing message fragment, live update/scaling rules
#              behave as specified, and scaler-binding allows pause/runtimeControl/router edits
#              while fencing replicas.
# Cleanup:     EXIT trap deletes all live ModelDeployments and ModelDeploymentScalers created by
#              this case, releases their Kueue Workloads, and tears down the CPU-vendor fixture if
#              injected. Idempotent, runs on pass and fail, safe to re-run.
set -uo pipefail

CASE_ID=26
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_epd-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-26.sh <NS>" >&2
  exit 2
fi

command -v jq >/dev/null 2>&1 || { echo "[case-26] jq is required." >&2; exit 2; }

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
BINDING="${E2E_MD_BINDING:-e2e-c26-no-such-binding}"
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
CONTROL_IMAGE="${E2E_MD_CONTROL_IMAGE:-registry.k8s.io/pause:3.9}"
if [ "$CONTROL_IMAGE" = "$IMAGE" ]; then
  echo "[case-26] E2E_MD_IMAGE and E2E_MD_CONTROL_IMAGE are the same image, so the control row" >&2
  echo "          would change nothing and report a rebuild that never happened" >&2
  exit 2
fi

VENDOR="${E2E_C26_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
VENDOR_NVIDIA="${E2E_C26_NVIDIA_VENDOR:-NVIDIA}"
SETTLE_ROUNDS="${E2E_C26_SETTLE:-60}"

MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"

MD_NOBIND="e2e-c26-nobind"
MD_REBUILD="e2e-c26-rebuild"
NF_EPD="e2e-c26-cpu-vendor"
RUN_PREFIX="e2e-c26-$$"
MD_ELASTIC="${RUN_PREFIX}-elastic"
MD_BOUND="${RUN_PREFIX}-bound"
SCALER_BOUND="${RUN_PREFIX}-scaler"

cleanup() {
  local md wl row
  kubectl -n "$NS" delete "$SCALER_RES" "$SCALER_BOUND" \
    --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
  for md in "$MD_NOBIND" "$MD_REBUILD" "$MD_ELASTIC" "$MD_BOUND"; do
    kubectl -n "$NS" delete "$MD_RES" "$md" \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  sleep 5
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    case "${row#*=}" in
      *"${MD_NOBIND}-"* | *"${MD_REBUILD}-"* | *"${MD_ELASTIC}-"* | *"${MD_BOUND}-"*)
        kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
          --ignore-not-found --wait=false >/dev/null 2>&1 || true
        ;;
    esac
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].name}{"\n"}{end}' 2>/dev/null)
EOF
  md_force_release "$NS" "$MD_ELASTIC" >/dev/null 2>&1 || true
  md_force_release "$NS" "$MD_BOUND" >/dev/null 2>&1 || true
  epd_teardown_cpu_vendor_fixture "$NS" "$NF_EPD" "$SETTLE_ROUNDS" "$SYSTEM_NS" || true
  return 0
}
trap cleanup EXIT

if [ -z "$IT" ]; then
  IT="$(md_usable_instance_types | sed -n '1p')"
fi
if [ -z "$IT" ]; then
  echo "[case-26] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

IT2="$(md_usable_instance_types | grep -v -x -F "$IT" | sed -n '1p' || true)"

# =================================================================================================
# PHASE 1: Single-role & multi-role admission surface, stored-object edits, and scale preservation
# =================================================================================================

MD_NAME="e2e-c26-probe"
ENGINE=""
KVCACHE_BLOCK=""

p1_manifest() {
  md_manifest "$MD_NAME" "$NS" "${ENGINE:-vLLM}" "$IMAGE" "${PROBE_BINDING-$BINDING}" \
    "${KVCACHE_BLOCK:-}" "$1"
}

p1_one_role() {
  md_server_manifest "$MD_NAME" "$NS" "${ENGINE:-vLLM}" "$IMAGE" "${PROBE_BINDING-$BINDING}" \
    "${KVCACHE_BLOCK:-}" "$IT" "$1"
}

p1_two_roles() {
  md_epd_manifest "$MD_NAME" "$NS" "${ENGINE:-vLLM}" "$IMAGE" "${PROBE_BINDING-$BINDING}" \
    "${KVCACHE_BLOCK:-}" "$IT"
}

p1_refuses() {
  local check="$1" want="$2" roles="$3" text
  shift 3
  if text="$(p1_manifest "$roles" | md_dryrun_refuses "$want" "$@")"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

p1_accepts() {
  local check="$1" roles="$2" text
  if text="$(p1_manifest "$roles" | md_dryrun_accepts)"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

# Row 0: single-role baseline must be accepted.
base_out="$(p1_one_role "" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
if [ -n "$base_out" ] && [ -z "${base_out##*created*}" ]; then
  record PASS "a well-formed single-role deployment is accepted" \
    "baseline passes schema and webhook even with an absent Binding"
else
  record FAIL "a well-formed single-role deployment is accepted" \
    "baseline was refused: ${base_out:0:160}"
fi

# Single-role webhook rules (manifests submitted directly via p1_one_role).
p1_one_accepts() {
  local check="$1" extra="$2" text
  if text="$(p1_one_role "$extra" | md_dryrun_accepts)"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

p1_one_refuses() {
  local check="$1" want="$2" extra="$3" text
  shift 3
  if text="$(p1_one_role "$extra" | md_dryrun_refuses "$want" "$@")"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

p1_one_accepts "a role asking for more than one Pod per instance is accepted" \
  "    size: 2"

p1_one_refuses "an owned argument in extraArgs is refused" \
  "is set by the operator for engine" \
  "    extraArgs:
    - --kv-transfer-config={}"

p1_one_refuses "an owned variable in env is refused" \
  "MOONCAKE_CONFIG_PATH" \
  "    env:
    - name: MOONCAKE_CONFIG_PATH
      value: /tmp/x.json"

p1_one_refuses "the template tier is refused as an unknown field" \
  "strict decoding error" \
  "    template:
      image: docker.io/library/busybox:1.36"

p1_one_accepts "a take-over role with an image is accepted" \
  "    image: docker.io/library/busybox:1.36
    command:
    - /bin/sh
    - -c
    - sleep 3600"

# Schema and LocalObjectReference rules.
PROBE_BINDING=""
p1_one_refuses "an empty poolRef name is refused by the webhook" \
  "must not be empty" ""
unset PROBE_BINDING

KVCACHE_BLOCK="    domain: i-declare-my-own"
p1_one_refuses "a self-declared reuse domain is refused by strict decoding" \
  "strict decoding error" ""
KVCACHE_BLOCK=""

ENGINE=vllm-ascend
p1_one_refuses "an engine outside the enum is refused by the schema" \
  "supported values" ""
ENGINE=""

pool_props="$(kubectl get crd modeldeployments.worker.gpustack.ai \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.kvCache.properties.poolRef.properties}' 2>/dev/null)"
if [ -n "$pool_props" ] && [ "${pool_props##*namespace*}" = "$pool_props" ]; then
  record PASS "a cross-namespace poolRef cannot be expressed" \
    "poolRef carries no namespace field (LocalObjectReference shape)"
else
  record FAIL "a cross-namespace poolRef cannot be expressed" \
    "poolRef properties are '${pool_props:-<absent>}'"
fi

# Live single-role object: controller condition, replica render, and stored update rules.
kubectl -n "$NS" delete "$MD_RES" "$MD_NOBIND" \
  --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
left=""
for _ in $(seq 1 20); do
  left="$(kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${MD_NOBIND}" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
  [ "${left:-0}" -eq 0 ] && break
  sleep 3
done

nobind_ready=yes
if [ "${left:-0}" -ne 0 ]; then
  nobind_ready=no
  record SKIP "controller-level rows for ${MD_NOBIND}" \
    "${left} leftover Pod(s) survived a 60s drain"
fi

MD_NAME="$MD_NOBIND"
if [ "$nobind_ready" = yes ]; then
  apply_out="$(p1_one_role "    image: docker.io/library/busybox:1.36
    engineVersion: \"0.32.0\"" \
    | kubectl apply -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$apply_out" ] || { [ -n "${apply_out##*created*}" ] && [ -n "${apply_out##*configured*}" ] &&
    [ -n "${apply_out##*unchanged*}" ]; }; then
    nobind_ready=no
    record SKIP "controller-level rows for ${MD_NOBIND}" \
      "manifest was never created: ${apply_out:-<no output>}"
  fi
fi

if [ "$nobind_ready" = yes ]; then
  conds=""
  for _ in $(seq 1 20); do
    conds="$(kubectl -n "$NS" get "$MD_RES" "$MD_NOBIND" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason};{end}' 2>/dev/null)"
    [ -n "$conds" ] && [ -z "${conds##*DomainRegistered=*}" ] && break
    sleep 3
  done
  if [ -n "$conds" ] && [ -z "${conds##*DomainRegistered=False/BindingNotFound*}" ]; then
    record PASS "a poolRef naming no Binding is refused by the controller" \
      "DomainRegistered=False/BindingNotFound"
  else
    record FAIL "a poolRef naming no Binding is refused by the controller" \
      "wanted DomainRegistered=False/BindingNotFound, got: ${conds:-<none within 60s>}"
  fi

  wl=0
  for _ in $(seq 1 40); do
    wl="$(kubectl -n "$NS" get pods \
      -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${MD_NOBIND}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
    [ "${wl:-0}" -eq 1 ] && break
    sleep 3
  done
  if [ "${wl:-0}" -eq 1 ]; then
    record PASS "replicas are rendered even though the domain is unregistered" \
      "1 replica Pod alongside DomainRegistered=False/BindingNotFound"
  elif [ "${wl:-0}" -eq 0 ]; then
    record FAIL "replicas are rendered even though the domain is unregistered" \
      "no replica Pod within 120s"
  else
    record FAIL "replicas are rendered even though the domain is unregistered" \
      "wanted 1 replica Pod, got ${wl}"
  fi
fi

record SKIP "unregistered domain costs replicas their connector and nothing else" \
  "asserted by case-27, which hosts the Ready-Binding control beside an unreachable one"

if [ "$nobind_ready" = yes ]; then
  size_out="$(kubectl -n "$NS" patch "$MD_RES" "$MD_NOBIND" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/size","value":2}]' 2>&1 | tr '\n' ' ')"
  stored_size="$(kubectl -n "$NS" get "$MD_RES" "$MD_NOBIND" \
    -o jsonpath='{.spec.roles[0].size}' 2>/dev/null)"
  if [ -n "$size_out" ] && [ -z "${size_out##*patched*}" ] && [ "$stored_size" = 2 ]; then
    record PASS "an ordinary role size edit is accepted and stored" "size=${stored_size}"
  else
    record FAIL "an ordinary role size edit is accepted and stored" \
      "wanted size=2, stored=[${stored_size:-none}], patch: ${size_out:0:160}"
  fi

  model_rc=0
  model_out="$(kubectl -n "$NS" patch "$MD_RES" "$MD_NOBIND" --type=merge \
    -p '{"spec":{"model":{"name":"e2e-c26/another-model"}}}' 2>&1)" || model_rc=$?
  model_out="$(printf '%s' "$model_out" | tr '\n' ' ')"
  if [ "$model_rc" -ne 0 ] && [ -z "${model_out##*spec.model*}" ]; then
    record PASS "an edit to spec.model is refused on a live object" "refused at spec.model"
  else
    record FAIL "an edit to spec.model is refused on a live object" \
      "rc=${model_rc}, got: ${model_out:0:160}"
  fi
  if [ "$model_rc" -eq 0 ]; then
    record SKIP "the model refusal states the rule" "edit was accepted"
    record SKIP "the model refusal states no mechanism" "edit was accepted"
  else
    if [ -z "${model_out##*describes a different deployment*}" ]; then
      record PASS "the model refusal states the rule" "describes a different deployment"
    else
      record FAIL "the model refusal states the rule" "${model_out:0:160}"
    fi
    wrong=""
    for word in conflict concurrent lock race; do
      case "$model_out" in *"$word"*) wrong="${wrong} ${word}" ;; esac
    done
    if [ -z "$wrong" ]; then
      record PASS "the model refusal states no mechanism" "none of conflict/concurrent/lock/race"
    else
      record FAIL "the model refusal states no mechanism" "carries:${wrong}"
    fi
  fi

  replicas_out="$(kubectl -n "$NS" patch "$MD_RES" "$MD_NOBIND" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1 | tr '\n' ' ')"
  if [ -n "$replicas_out" ] && [ -z "${replicas_out##*patched*}" ]; then
    record PASS "control: an edit to a role's replicas is accepted" "patched alongside size edit"
  else
    record FAIL "control: an edit to a role's replicas is accepted" "${replicas_out:0:160}"
  fi
  stored_replicas="$(kubectl -n "$NS" get "$MD_RES" "$MD_NOBIND" \
    -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null)"
  if [ "$stored_replicas" = 2 ]; then
    record PASS "the accepted replicas edit reached storage" "replicas=${stored_replicas}"
  else
    record FAIL "the accepted replicas edit reached storage" "replicas=[${stored_replicas:-none}]"
  fi
else
  record SKIP "an ordinary role size edit is accepted and stored" "subject deployment was never created"
  record SKIP "an edit to spec.model is refused on a live object" "subject deployment was never created"
fi

kubectl -n "$NS" delete "$MD_RES" "$MD_NOBIND" --wait=true --timeout=60s >/dev/null 2>&1 || true

if kubectl get crd modeldeployments.worker.gpustack.ai >/dev/null 2>&1; then
  record PASS "the worker installs its own ModelDeployment CRD" "modeldeployments CRD present"
else
  record FAIL "the worker installs its own ModelDeployment CRD" "modeldeployments CRD absent"
fi

# Multi-role admission surface.
MD_NAME="e2e-c26-multi-probe"
pdo_out="$(p1_two_roles | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
case "$pdo_out" in
  *"created (server dry run)"* | *"configured (server dry run)"*)
    record PASS "two roles are ACCEPTED" "multi-role baseline passes schema and webhook"
    ;;
  *)
    record FAIL "two roles are ACCEPTED" "baseline refused: ${pdo_out:0:220}"
    ;;
esac

eleven=""
for i in $(seq 0 10); do
  eleven+="  - name: role-${i}
    acceleratorType: ${IT}
    replicas: 1
"
done
p1_refuses "eleven roles are refused naming this project's cap" \
  "a shape this operator does not serve" "$eleven"

p1_refuses "two roles sharing a name are refused by the schema" \
  'spec.roles[1]: Duplicate value:' \
  "  - name: worker
    acceleratorType: ${IT}
    replicas: 1
  - name: worker
    acceleratorType: ${IT}
    replicas: 1" \
  '{"name":"worker"}'

if [ -n "${IT2:-}" ]; then
  p1_accepts "two acceleratorTypes are ACCEPTED" \
    "  - name: prefill
    acceleratorType: ${IT}
    replicas: 1
  - name: decode
    acceleratorType: ${IT2}
    replicas: 1"
else
  record SKIP "two acceleratorTypes are ACCEPTED" \
    "cluster has only one usable AcceleratorType (${IT}); case-28 covers two-type joint admission by creating its second type"
fi

p1_refuses "kind: Server beside another kind is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: Server
    acceleratorType: ${IT}
    replicas: 1
  - name: prefill
    kind: Prefill
    acceleratorType: ${IT}
    replicas: 1"

ENGINE=SGLang
sglang_pd_text=""
if sglang_pd_text="$(p1_two_roles | md_dryrun_accepts)"; then
  record PASS "an SGLang prefill/decode pair is ACCEPTED" "$sglang_pd_text"
else
  record FAIL "an SGLang prefill/decode pair is ACCEPTED" "$sglang_pd_text"
fi
ENGINE=""

p1_refuses "a role name that is not a PodSetReference is refused by the schema" \
  "spec.roles[0].name" \
  "  - name: Prefill
    acceleratorType: ${IT}
    replicas: 1"

p1_refuses "a role kind outside the enum is refused by the schema" \
  "spec.roles[0].kind" \
  "  - name: prefill
    kind: router
    acceleratorType: ${IT}
    replicas: 1"

# Live multi-role convergence: replicas change preserves living UIDs; image change replaces them.
MD_NAME="$MD_REBUILD"
DECLARED_TOTAL=1

c26_totals() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${MD_REBUILD}" \
    -o jsonpath='{range .items[*]}{.metadata.annotations.kueue\.x-k8s\.io/pod-group-total-count}{"\n"}{end}' \
    2>/dev/null | sort -u | tr '\n' ' '
}

REBUILD_APPLY="$(p1_two_roles | kubectl apply -f - 2>&1)"
for _ in $(seq 1 30); do
  [ "$(c26_totals)" = "${DECLARED_TOTAL} " ] && break
  sleep 2
done
if [ "$(c26_totals)" = "${DECLARED_TOTAL} " ]; then
  record PASS "every replica declares a group total of one" "pod-group-total-count=${DECLARED_TOTAL}"
else
  record FAIL "every replica declares a group total of one" \
    "observed totals: '$(c26_totals)'; apply: ${REBUILD_APPLY:0:200}"
fi

PREFILL_BEFORE="$(md_role_uids "$NS" "$MD_REBUILD" prefill)"
DECODE_BEFORE="$(md_role_uids "$NS" "$MD_REBUILD" decode)"
TPL='"image":"'"$IMAGE"'","command":["/pause"]'
SCALE_PATCH="$(kubectl -n "$NS" patch "$MD_RES" "$MD_REBUILD" --type=merge \
  -p '{"spec":{"roles":[{"name":"prefill","kind":"Prefill","acceleratorType":"'"$IT"'","replicas":2,'"$TPL"'},{"name":"decode","kind":"Decode","acceleratorType":"'"$IT"'","replicas":1,'"$TPL"'}]}}' \
  2>&1)"

SCALED=no
BAD_TOTAL=no
for _ in $(seq 1 45); do
  t="$(c26_totals)"
  [ -n "$t" ] && [ "$t" != "${DECLARED_TOTAL} " ] && BAD_TOTAL=yes
  [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_REBUILD},app.kubernetes.io/component=prefill" \
    --no-headers 2>/dev/null | grep -c . || true)" = 2 ] && { SCALED=yes; break; }
  sleep 2
done

if [ "$SCALED" != yes ]; then
  record FAIL "a replicas change leaves every living replica alone" \
    "prefill never reached 2 replicas; totals='$(c26_totals)'; patch: ${SCALE_PATCH:0:220}"
else
  kept_p="$(md_surviving_uids "$NS" "$MD_REBUILD" "$PREFILL_BEFORE" prefill)"
  kept_d="$(md_surviving_uids "$NS" "$MD_REBUILD" "$DECODE_BEFORE" decode)"
  want_p="$(wc -w <<<"$PREFILL_BEFORE" | tr -d ' ')"
  want_d="$(wc -w <<<"$DECODE_BEFORE" | tr -d ' ')"
  if [ "$kept_p" = "$want_p" ] && [ "$kept_d" = "$want_d" ]; then
    record PASS "a replicas change leaves every living replica alone" \
      "all ${want_p} prefill and ${want_d} decode replicas kept their UIDs"
  else
    record FAIL "a replicas change leaves every living replica alone" \
      "prefill kept ${kept_p}/${want_p}, decode kept ${kept_d}/${want_d}"
  fi
fi

if [ "$BAD_TOTAL" = no ]; then
  record PASS "no replica ever declared a pod-group total other than one" \
    "polled every 2s across the scale"
else
  record FAIL "no replica ever declared a pod-group total other than one" \
    "sampled a total other than ${DECLARED_TOTAL}"
fi

CONTROL_BEFORE="$(md_role_uids "$NS" "$MD_REBUILD" decode)"
CONTROL_PATCH="$(kubectl -n "$NS" patch "$MD_RES" "$MD_REBUILD" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/1/image","value":"'"$CONTROL_IMAGE"'"}]' 2>&1)"

CONTROL_DONE=no
for _ in $(seq 1 90); do
  [ "$(md_surviving_uids "$NS" "$MD_REBUILD" "$CONTROL_BEFORE" decode)" = 0 ] && { CONTROL_DONE=yes; break; }
  sleep 2
done
if [ "$CONTROL_DONE" = yes ]; then
  record PASS "control: an image change replaces that role's replicas" \
    "all old decode UIDs replaced"
else
  record FAIL "control: an image change replaces that role's replicas" \
    "$(md_surviving_uids "$NS" "$MD_REBUILD" "$CONTROL_BEFORE" decode) old decode UID(s) survived; patch: ${CONTROL_PATCH:0:200}"
fi

kubectl -n "$NS" delete "$MD_RES" "$MD_REBUILD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
md_force_release "$NS" "$MD_REBUILD" >/dev/null 2>&1 || true

# =================================================================================================
# PHASE 2: spec.engine.image schema contract and SGLang external DP RouterRequired
# =================================================================================================

P2_MD="e2e-c26-img-probe"
P2_IT=""
P2_MFG=""
for m in ascend nvidia amd; do
  found="$(md_usable_type_for "$m")"
  if [ -n "$found" ]; then
    P2_IT="$found"
    P2_MFG="$m"
    break
  fi
done
SGLANG_IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-${IT}}}"

VLLM_VER_LINE="$(md_engine_version_line vLLM "$IMAGE")"
SGLANG_VER_LINE="$(md_engine_version_line SGLang "$IMAGE")"

ENGINE_OK="    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}"
ENGINE_NO_IMAGE="    name: vLLM"
ENGINE_EMPTY="    name: vLLM
    image: \"\""
ENGINE_SGLANG="    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}"

ROLE_PLAIN="  - name: decode
    kind: Decode
    acceleratorType: ${P2_IT:-e2e-c26-any}"
ROLE_WITH_IMAGE="  - name: decode
    kind: Decode
    acceleratorType: ${P2_IT:-e2e-c26-any}
    image: ${IMAGE}"
ROLE_PINNED="  - name: decode
    kind: Decode
    acceleratorType: ${P2_IT:-e2e-c26-any}
    resources:
      accelerator: \"1\""

RUST_ENV='    env:
      - name: SGLANG_RUST_SERVER
        value: "1"'

p2_sglang_role() {
  cat <<ROLES
  - name: server
    kind: Server
    replicas: 1
    size: 2
    acceleratorType: ${SGLANG_IT}
    image: ${IMAGE}
    extraArgs:
      - --tp-size
      - "2"
      - --attn-dp-size
      - "$1"
      - --nnodes
      - "$2"
      - --node-rank
      - \$(GPUSTACK_MEMBER_INDEX)
      - --dist-init-addr
      - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
$3
ROLES
}

p2_manifest() {
  local engine="$1" roles="$2" router="${3:-}"
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P2_MD}
  namespace: ${NS}
spec:
  engine:
${engine}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
$(if [ -n "$router" ]; then echo "  router:"; echo "    name: ${router}"; fi)
  roles:
${roles}
YAML
}

p2_dry_run() {
  p2_manifest "$1" "$2" "${3:-}" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' '
}

p2_accepts() {
  local check="$1" engine="$2" roles="$3" router="${4:-}" obj status=0
  obj="$(md_dryrun_accepts < <(p2_manifest "$engine" "$roles" "$router"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

p2_refuses() {
  local check="$1" engine="$2" roles="$3" router="$4" want="" obj status=0
  shift 4
  if [ "$#" -gt 0 ]; then
    want="$1"
    shift
  fi
  obj="$(md_dryrun_refuses "$want" "$@" < <(p2_manifest "$engine" "$roles" "$router"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

p2_refuses_one_of() {
  local check="$1" engine="$2" roles="$3" router="$4" first="$5" second="$6" out w
  out="$(p2_dry_run "$engine" "$roles" "$router")"
  if [ -z "$out" ]; then
    record FAIL "$check" "apply produced no output, so nothing was refused"
    return 0
  fi
  for w in "$first" "$second"; do
    case "$out" in
      *"$w"*)
        record PASS "$check" "refused with: ${w}"
        return 0
        ;;
    esac
  done
  record FAIL "$check" "not refused with '${first}' or '${second}' (got: ${out:0:200})"
}

WORKER_DEPLOY="$(kubectl -n "$SYSTEM_NS" get deploy gpustack-operator-worker -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
if [ -n "$WORKER_DEPLOY" ]; then
  record PASS "deployed operator build recorded" "worker image ${WORKER_DEPLOY}"
else
  record SKIP "deployed operator build recorded" "no gpustack-operator-worker Deployment in ${SYSTEM_NS}"
fi

if [ -n "$P2_IT" ]; then
  p2_accepts "engine.image baseline with a role set is ACCEPTED (${P2_MFG})" "$ENGINE_OK" "$ROLE_WITH_IMAGE"
else
  record SKIP "engine.image baseline with a role set is ACCEPTED" \
    "no usable AcceleratorType reports an admitted platform (ascend, nvidia, amd)"
fi

p2_refuses_one_of "engine with no image key is refused by the schema" \
  "$ENGINE_NO_IMAGE" "$ROLE_PLAIN" "" \
  "spec.engine.image: Required value" 'spec.engine.image: Invalid value: ""'

empty_out="$(p2_dry_run "$ENGINE_EMPTY" "$ROLE_WITH_IMAGE")"
if [ -z "$empty_out" ]; then
  record SKIP "empty engine.image behavior recorded" "apply produced no output"
else
  case "$empty_out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "empty engine.image behavior recorded" "ACCEPTED (no minLength): ${empty_out:0:160}"
      ;;
    *)
      record PASS "empty engine.image behavior recorded" "refused: ${empty_out:0:160}"
      ;;
  esac
fi

p2_refuses_one_of "engine with no image key is refused even when every role names an image" \
  "$ENGINE_NO_IMAGE" "$ROLE_WITH_IMAGE" "" \
  "spec.engine.image: Required value" 'spec.engine.image: Invalid value: ""'

p2_refuses_one_of "engine with no image key is refused even when role pins accelerator" \
  "$ENGINE_NO_IMAGE" "$ROLE_PINNED" "" \
  "spec.engine.image: Required value" 'spec.engine.image: Invalid value: ""'

# SGLang external DP RouterRequired controls and rules.
p2_accepts "control: SGLang attention width 1 is ACCEPTED without a router" \
  "$ENGINE_SGLANG" "$(p2_sglang_role 1 2 "$RUST_ENV")" ""
p2_accepts "control: SGLang single node is ACCEPTED without a router" \
  "$ENGINE_SGLANG" "$(p2_sglang_role 2 1 "$RUST_ENV")" ""
p2_accepts "control: SGLang Rust server off is ACCEPTED without a router" \
  "$ENGINE_SGLANG" "$(p2_sglang_role 2 2 "")" ""
p2_refuses "external SGLang shape with no router is REFUSED naming RouterRequired" \
  "$ENGINE_SGLANG" "$(p2_sglang_role 2 2 "$RUST_ENV")" "" \
  "RouterRequired" "spec.router"
p2_accepts "external SGLang shape with sglang-gateway is ACCEPTED" \
  "$ENGINE_SGLANG" "$(p2_sglang_role 2 2 "$RUST_ENV")" "sglang-gateway"
p2_accepts "external SGLang shape with llm-d-router is ACCEPTED" \
  "$ENGINE_SGLANG" "$(p2_sglang_role 2 2 "$RUST_ENV")" "llm-d-router"

# =================================================================================================
# PHASE 3: EPD encoder admission and platform matrix
# =================================================================================================

P3_MD="e2e-c26-epd-probe"
P3_ROUTER="llm-d-router"
P3_ENGINE="vLLM"
P3_ROUTER_EXTRA_ARGS=""
P3_KVCACHE_BLOCK=""
P3_IT="$IT"

p3_manifest() {
  local ver_line
  ver_line="$(md_engine_version_line "${P3_ENGINE:-vLLM}" "$IMAGE")"
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P3_MD}
  namespace: ${NS}
spec:
  engine:
    name: ${P3_ENGINE:-vLLM}
    image: ${IMAGE}${ver_line:+
${ver_line}}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${P3_ROUTER:-llm-d-router}
${P3_ROUTER_EXTRA_ARGS:-}
${P3_KVCACHE_BLOCK:-}
  roles:
${1}
YAML
}

p3_encode_role() {
  printf '  - name: encode\n    kind: Encode\n    acceleratorType: %s\n    image: %s\n' "$P3_IT" "$IMAGE"
  [ -n "${1:-}" ] || return 0
  printf '    additionalVolumes:\n      - mountPath: %s\n' "${2:-/ec-cache}"
  [ -n "${3:-}" ] && printf '        readOnly: %s\n' "$3"
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$1"
  return 0
}

p3_encode_role_sized() {
  printf '  - name: encode\n    kind: Encode\n    acceleratorType: %s\n    image: %s\n    size: %s\n' \
    "$P3_IT" "$IMAGE" "$1"
  printf '    additionalVolumes:\n      - mountPath: /ec-cache\n'
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$2"
}

p3_prefill_role() {
  printf '  - name: prefill\n    kind: Prefill\n    acceleratorType: %s\n    image: %s\n' "$P3_IT" "$IMAGE"
  [ -n "${1:-}" ] || return 0
  printf '    additionalVolumes:\n      - mountPath: %s\n' "${2:-/ec-cache}"
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$1"
  return 0
}

p3_decode_role() {
  printf '  - name: decode\n    kind: Decode\n    acceleratorType: %s\n    image: %s\n' "$P3_IT" "$IMAGE"
  [ -n "${1:-}" ] && printf '    command: ["/pause"]\n'
  return 0
}

p3_shared_claim_roles() {
  p3_encode_role e2e-c26-shared /ec-cache
  p3_prefill_role e2e-c26-shared /ec-cache
  p3_decode_role
}

p3_platform_baseline_roles() {
  local it="${1:-${P3_IT:-unset}}" mfg="${2:-${PLATFORM:-ascend}}" claim=""
  if [ "$mfg" = ascend ]; then
    claim="    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: e2e-c26-shared"
  fi
  cat <<ROLES
  - name: encode
    kind: Encode
    acceleratorType: ${it}
    image: ${IMAGE}
${claim}
  - name: prefill
    kind: Prefill
    acceleratorType: ${it}
    image: ${IMAGE}
${claim}
  - name: decode
    kind: Decode
    acceleratorType: ${it}
    image: ${IMAGE}
ROLES
}

p3_network_roles() {
  cat <<ROLES
  - name: encode
    kind: Encode
    acceleratorType: ${1:-${P3_IT:-unset}}
    image: ${IMAGE}
  - name: prefill
    kind: Prefill
    acceleratorType: ${1:-${P3_IT:-unset}}
    image: ${IMAGE}
  - name: decode
    kind: Decode
    acceleratorType: ${1:-${P3_IT:-unset}}
    image: ${IMAGE}
ROLES
}

p3_pair_roles() {
  cat <<ROLES
  - name: prefill
    kind: Prefill
    acceleratorType: ${1:-${P3_IT:-unset}}
    image: ${IMAGE}
  - name: decode
    kind: Decode
    acceleratorType: ${1:-${P3_IT:-unset}}
    image: ${IMAGE}
ROLES
}

p3_refuses() {
  local check="$1" want="$2" roles="$3" obj status=0
  shift 3
  obj="$(md_dryrun_refuses "$want" "$@" < <(p3_manifest "$roles"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

p3_accepts() {
  local check="$1" roles="$2" obj status=0
  obj="$(md_dryrun_accepts < <(p3_manifest "$roles"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

# Subphase 3a: read the unmodified cluster platforms first.
UNADMITTED_PLATFORMS="cambricon hygon iluvatar metax mthreads thead"
OTHER_IT=""
OTHER_MANUFACTURER=""
for m in $UNADMITTED_PLATFORMS; do
  OTHER_IT="$(epd_platform_type "$m")"
  if [ -n "$OTHER_IT" ]; then
    OTHER_MANUFACTURER="$m"
    break
  fi
done

ADMITTED_IT=""
ADMITTED_MFG=""
for m in ascend nvidia amd; do
  ADMITTED_IT="$(epd_platform_type "$m")"
  if [ -n "$ADMITTED_IT" ]; then
    ADMITTED_MFG="$m"
    break
  fi
done

SEEN="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.detail.manufacturer}{" "}{end}' 2>/dev/null)"

if [ -z "$OTHER_IT" ] && [ -z "$ADMITTED_IT" ]; then
  record SKIP "unmodified cluster EPD platform matrix" \
    "no usable AcceleratorType reported a manufacturer (cluster holds: ${SEEN:-<none>})"
else
  if [ -n "$ADMITTED_IT" ]; then
    p3_accepts "an admitted cluster EPD combination is ACCEPTED" \
      "$(p3_platform_baseline_roles "$ADMITTED_IT" "$ADMITTED_MFG")"
  else
    record SKIP "an admitted cluster EPD combination is ACCEPTED" \
      "no usable AcceleratorType reports an admitted platform (${SEEN:-<none>})"
  fi

  if [ -n "$OTHER_IT" ]; then
    p3_refuses "an unadmitted cluster platform is refused naming router, engine, and admitted rows" \
      "is not admitted for encoder roles under router" \
      "$(p3_platform_baseline_roles "$OTHER_IT" "$OTHER_MANUFACTURER")" \
      'and engine' \
      "the admitted rows are"

    P3_ENGINE=SGLang p3_refuses "an unadmitted cluster platform is refused naming SGLang" \
      'is not admitted with engine "SGLang"' \
      "$(p3_platform_baseline_roles "$OTHER_IT" "$OTHER_MANUFACTURER")"
    P3_ENGINE=vLLM

    if [ -n "$ADMITTED_IT" ]; then
      p3_refuses "two platforms in one encoder deployment are refused by the mixing rule" \
        "more than one accelerator platform" \
        "$(p3_platform_baseline_roles "$ADMITTED_IT" "$ADMITTED_MFG")
  - name: decode
    kind: Decode
    acceleratorType: ${OTHER_IT}
    image: ${IMAGE}"
    else
      record SKIP "two platforms in one encoder deployment are refused by the mixing rule" \
        "no admitted platform beside ${OTHER_MANUFACTURER} to mix with"
    fi
  else
    record SKIP "unadmitted cluster platform EPD refusal rows" \
      "no usable AcceleratorType reports an unadmitted manufacturer (${SEEN:-<none>})"
  fi
fi

# Subphase 3b: shared-directory leg (Ascend) via existing type or CPU-vendor fixture.
NODE=""
PLATFORM_TYPE="$(epd_platform_type "$PLATFORM")"
if [ -z "$PLATFORM_TYPE" ]; then
  NODE="$(epd_pick_worker_node)"
  if [ -n "$NODE" ]; then
    echo "[case-26] injecting CPU vendor '${VENDOR}' on node ${NODE} for EPD matrix checks"
    epd_setup_cpu_vendor_fixture "$VENDOR" "$NS" "$NF_EPD" "$SETTLE_ROUNDS" "$SYSTEM_NS" "$NODE"
    PLATFORM_TYPE="$EPD_FIXTURE_TYPE"
  fi
fi

P3_IT="${PLATFORM_TYPE:-$IT}"
if [ -n "$PLATFORM_TYPE" ]; then
  record PASS "an active pool reports the EPD target platform" \
    "AcceleratorType ${PLATFORM_TYPE} reports manufacturer '${PLATFORM}'"
  p3_accepts "the full shared-claim encoder shape on ${PLATFORM} is ACCEPTED" "$(p3_shared_claim_roles)"
else
  record SKIP "an active pool reports the EPD target platform" \
    "no AcceleratorType reported '${PLATFORM}'; object-only shape rows still run"
fi

# Object-only EPD shape and claim-source rules.
p3_refuses "an encoder with no decoder is refused naming the missing role" \
  'needs a "Decode" role beside it' \
  "$(p3_encode_role e2e-c26-shared /ec-cache)
$(p3_prefill_role e2e-c26-shared /ec-cache)"

p3_refuses "an encoder with no prefill is refused naming the missing role" \
  'needs a "Prefill" role beside it' \
  "$(p3_encode_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

p3_refuses "a Server role beside an encoder is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: Server
    acceleratorType: ${P3_IT}
    image: ${IMAGE}
$(p3_encode_role e2e-c26-shared /ec-cache)
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

no_router_out="$(cat <<YAML | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' '
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P3_MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
$(p3_shared_claim_roles)
YAML
)"
case "$no_router_out" in
  *"an Encode role requires a router"*)
    record PASS "an encoder with no router is refused" "refused naming: an Encode role requires a router"
    ;;
  *)
    record FAIL "an encoder with no router is refused" "unexpected output: ${no_router_out:0:220}"
    ;;
esac

P3_KVCACHE_BLOCK="$(printf '  kvCache:\n    poolRef:\n      name: e2e-c26-no-such-binding\n')"
p3_refuses "a deployment-wide kvCache is refused on the encoder shape" \
  "spec.kvCache is refused" \
  "$(p3_shared_claim_roles)"
P3_KVCACHE_BLOCK=""

p3_refuses "a role command is refused on the encoder shape" \
  "replaces the managed arguments and the routing sidecar" \
  "$(p3_encode_role e2e-c26-shared /ec-cache)
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role command)"

p3_refuses "an encoder of size > 1 is refused" \
  "replica size is 1" \
  "$(p3_encode_role_sized 2 e2e-c26-shared)
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

p3_refuses "an empty claim name on additionalVolumes is refused" \
  "claim name must not be empty" \
  "  - name: encode
    kind: Encode
    acceleratorType: ${P3_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: \"\"
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

p3_refuses "a second volume source beside persistentVolumeClaim is refused" \
  "set beside persistentVolumeClaim on the same entry" \
  "  - name: encode
    kind: Encode
    acceleratorType: ${P3_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: e2e-c26-shared
        configMap:
          name: e2e-c26-anything
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

# Platform-bound EPD matrix rules on Ascend.
if [ -n "$PLATFORM_TYPE" ]; then
  P3_ROUTER=vllm-router p3_refuses "vllm-router on ascend for an encoder role is refused" \
    'platform "ascend" is not admitted for encoder roles under router "vllm-router" and engine "vLLM"' \
    "$(p3_shared_claim_roles)"
  P3_ROUTER=llm-d-router

  P3_ROUTER=sglang-gateway P3_ENGINE=vLLM p3_refuses "sglang-gateway paired with vLLM on EPD is refused" \
    'router "sglang-gateway" is not admitted with engine "vLLM"' \
    "$(p3_shared_claim_roles)" \
    'the admitted rows are'
  P3_ROUTER=llm-d-router
  P3_ENGINE=vLLM

  p3_refuses "a read-only encoder claim mount is refused" \
    "must not be read-only" \
    "$(p3_encode_role e2e-c26-shared /ec-cache true)
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

  p3_refuses "disagreeing encoder/prefill claim mount paths are refused" \
    "must name the same mountPath" \
    "$(p3_encode_role e2e-c26-shared /ec-cache)
$(p3_prefill_role e2e-c26-shared /other-cache)
$(p3_decode_role)"

  p3_refuses "an encoder mounting no shared claim on ascend is refused" \
    "attach the shared claim the prefiller mounts" \
    "$(p3_encode_role)
$(p3_prefill_role e2e-c26-shared /ec-cache)
$(p3_decode_role)"

  p3_refuses "a prefiller mounting no shared claim on ascend is refused" \
    "mounts nothing" \
    "$(p3_encode_role e2e-c26-shared /ec-cache)
$(p3_prefill_role)
$(p3_decode_role)"

  UNADMITTED_IT="$(kubectl get acceleratortypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ -n "$manufacturer" ] && [ "$manufacturer" != "$PLATFORM" ] \
          && [ "$manufacturer" != amd ] && [ "$manufacturer" != nvidia ]; then
          echo "$name"
          break
        fi
      done)"
  SECOND_IT="$(kubectl get acceleratortypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ -n "$manufacturer" ] && [ "$manufacturer" != "$PLATFORM" ]; then
          echo "$name"
          break
        fi
      done)"

  if [ -n "$UNADMITTED_IT" ]; then
    p3_refuses "an unadmitted platform is refused under EPD matrix" \
      "is not admitted for encoder roles under router" \
      "$(p3_platform_baseline_roles "$UNADMITTED_IT" "$PLATFORM")"
  else
    record SKIP "an unadmitted platform is refused under EPD matrix" \
      "no usable AcceleratorType reports an unadmitted manufacturer"
  fi

  if [ -n "$SECOND_IT" ]; then
    p3_refuses "two platforms in one encoder deployment are refused" \
      "more than one accelerator platform" \
      "$(p3_encode_role e2e-c26-shared /ec-cache)
  - name: prefill
    kind: Prefill
    acceleratorType: ${SECOND_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: e2e-c26-shared
$(p3_decode_role)"
  else
    record SKIP "two platforms in one encoder deployment are refused" \
      "no second manufacturer exists to mix with ${PLATFORM}"
  fi

  P3_ENGINE=SGLang p3_accepts "llm-d-router with SGLang EPD is ACCEPTED" \
    "$(p3_network_roles "$P3_IT")"
  P3_ENGINE=SGLang P3_ROUTER=sglang-gateway p3_accepts "sglang-gateway with SGLang EPD is ACCEPTED" \
    "$(p3_network_roles "$P3_IT")"
  P3_ROUTER=llm-d-router
  P3_ENGINE=vLLM
else
  record SKIP "platform-bound Ascend EPD matrix rows" \
    "no ${PLATFORM} AcceleratorType derived"
fi

# Subphase 3c: network leg (NVIDIA/AMD).
PHASE3_IT=""
PHASE3_PLATFORM=""
for m in nvidia amd; do
  PHASE3_IT="$(epd_platform_type "$m")"
  if [ -n "$PHASE3_IT" ]; then
    PHASE3_PLATFORM="$m"
    break
  fi
done

if [ -z "$PHASE3_IT" ]; then
  [ -n "$NODE" ] || NODE="$(epd_pick_worker_node)"
  if [ -n "$NODE" ]; then
    echo "[case-26] carrying CPU vendor '${VENDOR_NVIDIA}' on node ${NODE} for network-leg EPD rows"
    epd_setup_cpu_vendor_fixture "$VENDOR_NVIDIA" "$NS" "$NF_EPD" "$SETTLE_ROUNDS" "$SYSTEM_NS" "$NODE" nvidia
    PHASE3_IT="$EPD_FIXTURE_TYPE"
    [ -n "$PHASE3_IT" ] && PHASE3_PLATFORM=nvidia
  fi
fi

if [ -n "$PHASE3_IT" ]; then
  P3_ROUTER=vllm-router p3_accepts "vllm-router + vLLM with Encode role is ACCEPTED on ${PHASE3_PLATFORM}" \
    "$(p3_network_roles "$PHASE3_IT")"
  P3_ROUTER=vllm-router P3_ENGINE=SGLang p3_refuses "vllm-router paired with SGLang EPD is refused" \
    'router "vllm-router" is not admitted with engine "SGLang"' \
    "$(p3_network_roles "$PHASE3_IT")" \
    'the admitted rows are'
  P3_ENGINE=vLLM
  P3_ROUTER_EXTRA_ARGS="$(printf '    extraArgs:\n      - --epd-config={"encoder_urls":[]}\n')"
  P3_ROUTER=vllm-router p3_refuses "user-supplied --epd-config on vllm-router is refused" \
    '"--epd-config" is set by the operator for router "vllm-router"' \
    "$(p3_network_roles "$PHASE3_IT")"
  P3_ROUTER_EXTRA_ARGS=""
  P3_ROUTER=vllm-router p3_accepts "vllm-router with no Encode role is admitted" \
    "$(p3_pair_roles "$PHASE3_IT")"
  P3_ROUTER=llm-d-router
else
  record SKIP "network-leg vllm-router EPD matrix rows" \
    "no nvidia or amd AcceleratorType available or derivable"
fi

# Restore the CPU vendor fixture before Phase 4 so Phase 4 sees the normal cluster catalog.
epd_teardown_cpu_vendor_fixture "$NS" "$NF_EPD" "$SETTLE_ROUNDS" "$SYSTEM_NS" || true

# =================================================================================================
# PHASE 4: Online runtimeControl admission, 15-key Translator registry, Elastic EP, & scaler-binding
# =================================================================================================

rt_dry_refuse() {
  local label="$1" want="$2" msg
  shift 2
  if msg="$(md_dryrun_refuses "$want" "$@")"; then
    record PASS "$label" "$msg"
  else
    record FAIL "$label" "$msg"
  fi
}

rt_dry_accept() {
  local label="$1" msg
  if msg="$(md_dryrun_accepts)"; then
    record PASS "$label" "$msg"
  else
    record FAIL "$label" "$msg"
  fi
}

if ! kubectl get crd modeldeploymentscalers.worker.gpustack.ai >/dev/null 2>&1; then
  record SKIP "Phase 4 runtimeControl and scaler-binding admission rows" \
    "ModelDeploymentScaler CRD missing"
else
  RT_AT="${E2E_MD_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
  [ -n "$RT_AT" ] || RT_AT="$(md_usable_accelerator_type "$NS" || true)"
  if [ -z "$RT_AT" ]; then
    record SKIP "Phase 4 runtimeControl and scaler-binding admission rows" \
      "no usable AcceleratorType available"
  else
    # 1. Reserved spec.model.loraAdapters is refused.
    rt_dry_refuse "reserved spec.model.loraAdapters is refused" "loraAdapters" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-lora
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
    loraAdapters:
      - name: adapter-a
        artifactRef:
          name: lora-a
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
EOF

    # 2. Scope gates: command, CPU-only, and SGLang Ray refuse runtimeControl; pause is unrestricted.
    rt_dry_refuse "runtimeControl refused on unmanaged command role" "command" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-scope-cmd
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      command: ["/bin/sh", "-c", "sleep 3600"]
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "16"
EOF

    rt_dry_refuse "runtimeControl refused on CPU-only role" "CPU" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-scope-cpu
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "16"
EOF

    rt_dry_refuse "runtimeControl refused on SGLang Ray role" "Ray" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-scope-sglang-ray
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      extraArgs:
        - --distributed-executor-backend=ray
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "16"
EOF

    rt_dry_accept "role.pause admitted on CPU-only and unmanaged command roles" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-pause-unrestricted
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      pause: true
      command: ["/bin/sh", "-c", "sleep 3600"]
EOF

    # 3. 15-key Translator parameter validation.
    rt_dry_refuse "unknown runtimeControl parameter key is refused" "unknown" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-unknown
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.nonExistentKey: "10"
EOF

    rt_dry_refuse "SGLang-only parameter refused on vLLM" "speculative.acceptThresholdSingle" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-engine
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          speculative.acceptThresholdSingle: "0.5"
EOF

    rt_dry_refuse "scheduling.maxActiveRequests exceeding --max-num-seqs is refused" "maxActiveRequests" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-ceiling
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      extraArgs:
        - --max-num-seqs=32
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "64"
EOF

    rt_dry_refuse "speculative.draftBudgetFraction=0 (half-open lower bound) is refused" "draftBudgetFraction" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-halfopen
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          speculative.draftBudgetFraction: "0"
EOF

    rt_dry_refuse "scheduling.ppMaxMicroBatchSize without ppSize>1 is refused" "ppMaxMicroBatchSize" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-pp
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.ppMaxMicroBatchSize: "2"
EOF

    rt_dry_refuse "scheduling.maxInflightKVLoads without KV connector is refused" "maxInflightKVLoads" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-kv
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxInflightKVLoads: "4"
EOF

    rt_dry_accept "valid vLLM runtimeControl.parameters and extraArgs ceilings admitted" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-valid-vllm
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  router:
    name: vllm-router
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      extraArgs:
        - --max-num-seqs=64
        - --max-num-batched-tokens=8192
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "32"
          scheduling.maxBatchedTokens: "4096"
          scheduling.longPrefillTokenThreshold: "2048"
          scheduling.cacheAwareWindow: "64"
          scheduling.cacheAwareThreshold: "0.5"
EOF

    rt_dry_accept "valid SGLang runtimeControl.parameters with pp=2 admitted" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-valid-sglang
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}
  router:
    name: sglang-gateway
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      extraArgs:
        - --max-running-requests=64
        - --chunked-prefill-size=8192
        - --pp-size=2
      resources:
        accelerator: "2"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "32"
          scheduling.maxBatchedTokens: "4096"
          scheduling.prefillMaxRequests: "16"
          scheduling.scheduleConservativeness: "1.2"
          scheduling.prefillDecodeInterval: "4"
          scheduling.ppMaxMicroBatchSize: "8"
          speculative.acceptThresholdSingle: "0.7"
          speculative.acceptThresholdAcc: "0.8"
          speculative.draftBudgetFraction: "0.5"
EOF

    # 4. Router isolation forbidden args and router owned args.
    rt_dry_refuse "router isolation rejects --enable-igw in spec.router.extraArgs" "enable-igw" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-router-igw
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}
  router:
    name: sglang-gateway
    extraArgs:
      - --enable-igw
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
EOF

    rt_dry_refuse "llm-d-router owned args reject --allow-experimental-plugins in spec.router.extraArgs" \
      '"--allow-experimental-plugins" is set by the operator for router "llm-d-router"' <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-router-exp-flag
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  router:
    name: llm-d-router
    extraArgs:
      - --allow-experimental-plugins
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
EOF

    rt_dry_refuse "llm-d-router owned args reject --allow-experimental-plugins=false in spec.router.extraArgs" \
      '"--allow-experimental-plugins" is set by the operator for router "llm-d-router"' <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-router-exp-false
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  router:
    name: llm-d-router
    extraArgs:
      - --allow-experimental-plugins=false
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
EOF

    # 5. Folded elastic.dataParallelWidth create/update immutability and monotonicity.
    if out="$(kubectl apply -f - 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_ELASTIC}
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${RT_AT}
      extraArgs:
        - --elastic-ep-max-dp-size=4
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "2"
EOF
)"; then
      record PASS "folded elastic.dataParallelWidth deployment created" "$MD_ELASTIC"

      if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
        -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"4"}]' 2>&1)"; then
        record PASS "elastic.dataParallelWidth monotonic scale-up 2->4 accepted" "$msg"
      else
        record FAIL "elastic.dataParallelWidth monotonic scale-up 2->4 accepted" "$msg"
      fi

      if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
        -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"6"}]' 2>&1)"; then
        record FAIL "elastic.dataParallelWidth above --elastic-ep-max-dp-size refused" "unexpectedly accepted: $msg"
      else
        record PASS "elastic.dataParallelWidth above --elastic-ep-max-dp-size refused" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
      fi

      if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
        -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth"}]' 2>&1)"; then
        record FAIL "removing elastic.dataParallelWidth after creation refused" "unexpectedly accepted: $msg"
      else
        record PASS "removing elastic.dataParallelWidth after creation refused" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
      fi

      if kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --type=json \
        -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"3"}]' >/dev/null 2>&1; then
        if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
          -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"2"}]' 2>&1)"; then
          record FAIL "elastic.dataParallelWidth scale-down 3->2 refused" "unexpectedly accepted: $msg"
        else
          record PASS "elastic.dataParallelWidth scale-down 3->2 refused" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
        fi
      else
        record FAIL "elastic.dataParallelWidth live patch 2->3 succeeded" "failed to patch width to 3"
      fi
    else
      record SKIP "folded elastic.dataParallelWidth live update checks" \
        "cluster refused elastic creation: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
    fi

    # 6. Sentinel admission hardening.
    rt_dry_refuse "pause-only sentinel role refuses GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK" "GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-pause-env
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      pause: true
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      env:
        - name: GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK
          value: "0"
EOF

    rt_dry_refuse "sentinel role refuses additionalVolumes overlapping engine-sentinel path" "engine sentinel" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-sentinel-vol
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      additionalVolumes:
        - mountPath: /var/lib/gpustack/operator/lib/engine-sentinel
          configMap:
            name: dummy-cm
EOF

    rt_dry_refuse "decimal parameter refuses huge scientific exponent" "must be a valid decimal quantity" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-huge-exp
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}${SGLANG_VER_LINE:+
${SGLANG_VER_LINE}}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.scheduleConservativeness: "1e100000000"
EOF

    # 7. Scaler-binding coexistence: pause, runtimeControl, mutable router allowed; replicas fenced.
    if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_BOUND}
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}${VLLM_VER_LINE:+
${VLLM_VER_LINE}}
  router:
    name: vllm-router
    requestTimeoutSeconds: 60
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${RT_AT}
      resources:
        accelerator: "1"
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SCALER_BOUND}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_BOUND}
  mode: Active
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: server
          target: "8"
  roles:
    - name: server
      minReplicas: 1
      maxReplicas: 2
EOF
    then
      got_bind=""
      for _ in $(seq 1 30); do
        got_bind="$(kubectl -n "$NS" get "$MD_RES" "$MD_BOUND" -o jsonpath='{.metadata.annotations.modeldeployment\.gpustack\.ai/scaler-binding}' 2>/dev/null || true)"
        [ -n "$got_bind" ] && break
        sleep 2
      done
      if [ -n "$got_bind" ]; then
        record PASS "ModelDeploymentScaler bound target deployment" "$got_bind"

        if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_BOUND" --type=merge \
          -p '{"spec":{"router":{"requestTimeoutSeconds":90},"roles":[{"name":"server","kind":"Server","replicas":1,"pause":true,"acceleratorType":"'"${RT_AT}"'","resources":{"accelerator":"1"},"runtimeControl":{"parameters":{"scheduling.maxActiveRequests":"16"}}}]}}' 2>&1)"; then
          record PASS "scaler-bound deployment admits pause, runtimeControl, and router edits" "$msg"
        else
          record FAIL "scaler-bound deployment admits pause, runtimeControl, and router edits" "$msg"
        fi

        if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_BOUND" --dry-run=server --type=json \
          -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1)"; then
          record FAIL "scaler-bound deployment still fences replicas edits" "unexpectedly admitted: $msg"
        else
          record PASS "scaler-bound deployment still fences replicas edits" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
        fi
      else
        record FAIL "ModelDeploymentScaler bound target deployment" "scaler-binding annotation never appeared"
      fi
    else
      record FAIL "create scaler-bound ModelDeployment fixture" "apply failed"
    fi
  fi
fi

results
