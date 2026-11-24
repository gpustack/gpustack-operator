#!/usr/bin/env bash
#
# CASE 28 — Multi-role & P/D/EPD Kueue joint admission: per-replica Workloads, Service watch
#           recreation, short-pool whole-set starvation, Workload watch, administrator-authored
#           InstanceType (instance-type-derived-from-node=false), two-InstanceType barrier
#           closing/parking/opening, and three-role EPD starvation
#   (MUTATING, self-recovering)
#
#   case-28.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_KUBE_CONTEXT,E2E_MD_BINDING,E2E_MD_DELETE_BOUND,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_ACCELERATOR_TYPE,E2E_MD_SETTLE,E2E_SVC_RECREATE_BOUND,E2E_SYSTEM_NS,E2E_C28_MAX_REPLICAS,E2E_C28_EPD_VENDOR,E2E_C28_EPD_EC_CLAIM,E2E_C28_EPD_SETTLE
# Goal:        Verify the full Kueue multi-role and joint-admission contract on CPU-only clusters:
#                1. Per-replica Workloads (4 replicas -> 4 Workloads with role-named PodSets),
#                   single-Workload deletion blast radius, clean group deletion, identically-shaped
#                   twin roles keeping distinct PodSet names, and out-of-band Service deletion
#                   watch recreation with watched-object quiescence attribution.
#                2. Short-pool whole-set starvation on a 2-role P/D deployment (1 role holds quota,
#                   both roles remain scheduling-gated by gpustack-model-deployment-joint, status
#                   names only the waiting role), Workload-only status watch propagation, filler
#                   release admitting the set, and identical behavior on an administrator-authored
#                   InstanceType with instance-type-derived-from-node=false.
#                3. Two-InstanceType joint admission using an Inactive second AcceleratorType:
#                   same-type scale blast radius, two-type hold with feasible single-role control,
#                   back-dated LastTransitionTime parking (spec.active=false, reason=Parked), and
#                   delayed-sibling barrier opening (1/0/2 -> 2/2).
#                4. Three-role EPD (Encode/Prefill/Decode) short-pool whole-set starvation:
#                   2 of 3 roles reserve while all 3 remain gated until filler release admits all 3.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), Kueue pod
#              integration enabled, and the pool's entrance LocalQueue present in <NS>. No GPU is
#              required; all placeholder replicas use pause images.
# Inputs:      REAL: ModelDeployments, Kueue Workloads, Services, temporary InstanceType /
#              AcceleratorType fixtures, and gpustack-settings secret toggles. MOCKED (Phase 4
#              only when no AMD AcceleratorType exists): CPU-vendor NodeFeature + awareness setting.
# Expected:    All four phases pass their per-replica Workload, Service watch, joint-admission
#              hold/park/open, authored-type, and EPD starvation assertions; capacity-gated or
#              race-sensitive attribution checks report SKIP rather than false PASS/FAIL.
# Cleanup:     EXIT trap deletes all ModelDeployments, releases Kueue Workloads, removes authored
#              and held InstanceType/AcceleratorType objects, restores gpustack-settings, tears
#              down the Phase 4 CPU-vendor fixture if injected, and deletes temporary files.
set -uo pipefail

CASE_ID=28
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-28.sh <NS>" >&2
  exit 2
fi

KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }
# shellcheck disable=SC2034 # Read by sourced _md-lib.sh.
MD_KUBE_CONTEXT="$KCTX"

# shellcheck source=/dev/null
. "${CASE_DIR}/_quota-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_epd-lib.sh"

BINDING="${E2E_MD_BINDING:-e2e-c28-no-such-binding}"
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
DELETE_BOUND="${E2E_MD_DELETE_BOUND:-90}"
RECREATE_BOUND="${E2E_SVC_RECREATE_BOUND:-90}"
MAX_REPLICAS="${E2E_C28_MAX_REPLICAS:-128}"
GATE="kueue.x-k8s.io/admission"
WINDOW="${POOL_WINDOW:-30}"
# shellcheck disable=SC2034 # Read by _rows-lib.sh settings helpers.
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING="instance-type-derived-from-node"
JOINT="gpustack-model-deployment-joint"
SETTLE="${E2E_MD_SETTLE:-120}"

# Object names per phase.
MD_PD="e2e-c28-pd"
MD_TWINS="e2e-c28-twins"
MD_SVC="e2e-c28-svc"
AUTHORED="e2e-c28-authored"
MD_PAIR="e2e-c28-pair"
MD_CONTROL="e2e-c28-control"
MD_OPENS="e2e-c28-opens"
IT_HELD="e2e-c28-held"
EPD_VENDOR="${E2E_C28_EPD_VENDOR:-AMD}"
EPD_PLATFORM="$(printf '%s' "$EPD_VENDOR" | tr '[:upper:]' '[:lower:]')"
EPD_EC_CLAIM="${E2E_C28_EPD_EC_CLAIM:-}"
EPD_NF="e2e-c28-cpu-vendor"
EPD_SETTLE_ROUNDS="${E2E_C28_EPD_SETTLE:-60}"
EPD_PROBE="e2e-c28-epd-probe"
EPD_FILLER="e2e-c28-epd-filler"
EPD_SUBJECT="e2e-c28-epd-subject"

SAMPLE_ERR="$(mktemp 2>/dev/null || true)"

if [ -z "$IT" ]; then
  IT="$(md_usable_instance_type "$AUTHORED" "$IT_HELD")"
fi
if [ -z "$IT" ]; then
  echo "[case-28] no usable AcceleratorType/InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

CQ="$(md_cq_of_it "$NS" "$IT")"
if [ -z "$CQ" ]; then
  echo "[case-28] InstanceType ${IT} names no reachable ClusterQueue in ${NS}" >&2
  exit 2
fi

GROUP=""
OS=""
ARCH=""
read -r GROUP OS ARCH <<<"$(k get acceleratortypes.worker.gpustack.ai "$IT" \
  -o jsonpath='{.spec.generalGroup} {.spec.os} {.spec.arch}' 2>/dev/null)"
if [ -z "$GROUP" ] || [ -z "$OS" ] || [ -z "$ARCH" ]; then
  read -r GROUP OS ARCH <<<"$(k get instancetypes.worker.gpustack.ai "$IT" \
    -o jsonpath='{.spec.generalGroup} {.spec.os} {.spec.arch}' 2>/dev/null)"
fi

settings_save "$SETTING"

wait_until() {
  local deadline=$((SECONDS + $1))
  shift
  while [ "$SECONDS" -lt "$deadline" ]; do
    "$@" && return 0
    sleep 5
  done
  return 1
}

release_deployment() {
  local md="$1" wls wl
  wls="$(md_workload_verdicts "$NS" "$md" | cut -d= -f1)"
  k -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    [ "$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
    sleep 2
  done
  md_force_release "$NS" "$md" >/dev/null 2>&1 || true
  for wl in $wls; do
    k -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}

cleanup_epd_deployments() {
  k -n "$NS" delete modeldeployments.worker.gpustack.ai \
    "$EPD_PROBE" "$EPD_FILLER" "$EPD_SUBJECT" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 12); do
    sleep 5
    md_force_release "$NS" "$EPD_PROBE" >/dev/null 2>&1 || true
    md_force_release "$NS" "$EPD_FILLER" >/dev/null 2>&1 || true
    md_force_release "$NS" "$EPD_SUBJECT" >/dev/null 2>&1 || true
    [ -z "$(k -n "$NS" get pods \
      -l "app.kubernetes.io/instance in (${EPD_PROBE},${EPD_FILLER},${EPD_SUBJECT})" \
      --no-headers 2>/dev/null)" ] && break
  done
}

cleanup() {
  local md
  [ -n "${SAMPLE_ERR:-}" ] && rm -f "$SAMPLE_ERR"
  k -n "$NS" delete modeldeployments.worker.gpustack.ai \
    "$MD_PD" "$MD_TWINS" "$MD_SVC" \
    e2e-c28-p2-probe e2e-c28-p2-filler e2e-c28-p2-subject \
    e2e-c28-p2a-probe e2e-c28-p2a-filler e2e-c28-p2a-subject \
    "$MD_PAIR" "$MD_CONTROL" "$MD_OPENS" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  sleep 5
  for md in "$MD_PD" "$MD_TWINS" "$MD_SVC" \
            e2e-c28-p2-probe e2e-c28-p2-filler e2e-c28-p2-subject \
            e2e-c28-p2a-probe e2e-c28-p2a-filler e2e-c28-p2a-subject \
            "$MD_PAIR" "$MD_CONTROL" "$MD_OPENS"; do
    md_force_release "$NS" "$md" >/dev/null 2>&1 || true
  done
  k delete instancetypes.worker.gpustack.ai "$AUTHORED" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete acceleratortypes.worker.gpustack.ai "$IT_HELD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  epd_teardown_cpu_vendor_fixture "$NS" "$EPD_NF" "$EPD_SETTLE_ROUNDS" "$SYSTEM_NS" cleanup_epd_deployments || true
  settings_restore || true
}
trap cleanup EXIT

# =================================================================================================
# Shared helpers for Phases 1-4
# =================================================================================================

replica_versions() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${1}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion}{"\n"}{end}' 2>/dev/null \
    | sort | tr '\n' ' '
}

sample_kind() {
  local label="$1" out
  shift
  if out="$(k "$@" 2>"${SAMPLE_ERR:-/dev/null}")"; then
    [ -n "$out" ] && printf '%s\n' "$out"
  elif [ -n "$SAMPLE_ERR" ] && command grep -qi 'not found' "$SAMPLE_ERR"; then
    printf '%s=NOT-FOUND\n' "$label"
  else
    printf '%s=READ-FAILED\n' "$label"
  fi
}

watched_versions() {
  local md="$1"
  {
    sample_kind pods -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      -o jsonpath="{range .items[*]}pod/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample_kind routerobjects -n "$NS" get deployments,configmaps,serviceaccounts,roles,rolebindings \
      -l "app.kubernetes.io/instance=${md}" \
      -o jsonpath="{range .items[*]}{.kind}/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample_kind workloads -n "$NS" get workloads.kueue.x-k8s.io \
      -o jsonpath="{range .items[*]}workload/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample_kind instancetype get instancetypes.worker.gpustack.ai "$IT" \
      -o jsonpath="instancetype/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}"
    sample_kind modeldeployment -n "$NS" get modeldeployments.worker.gpustack.ai "$md" \
      -o jsonpath="modeldeployment/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}"
    sample_kind kvcachepoolbindings get kvcachepoolbindings.worker.gpustack.ai -A \
      -o jsonpath="{range .items[*]}kvcachepoolbinding/{.metadata.namespace}/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample_kind kvcachepools get kvcachepools.worker.gpustack.ai \
      -o jsonpath="{range .items[*]}kvcachepool/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample_kind kvcachebackends get kvcachebackends.worker.gpustack.ai \
      -o jsonpath="{range .items[*]}kvcachebackend/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
  } | sort | tr '\n' ' '
}

workload_podsets() {
  local md="$1" uids
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -n "$uids" ] || return 0
  k -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath="{range .items[*]}\
{.metadata.name}|{range .spec.podSets[*]}{.name}={.count}{\",\"}{end}|\
{range .metadata.ownerReferences[*]}{.uid}{\" \"}{end}{\"\n\"}{end}" 2>/dev/null \
    | while IFS='|' read -r name sets owners; do
        [ -n "$name" ] || continue
        for u in $uids; do
          case " $owners " in *" $u "*) echo "$name ${sets%,}"; break ;; esac
        done
      done | sort
}

wait_pods() {
  local md="$1" want="$2"
  for _ in $(seq 1 45); do
    [ "$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "$want" ] && return 0
    sleep 2
  done
  return 1
}

wait_any_workload() {
  local md="$1" wl
  for _ in $(seq 1 45); do
    wl="$(md_workloads "$NS" "$md" | head -n 1)"
    [ -n "$wl" ] && { echo "$wl"; return 0; }
    sleep 2
  done
  return 1
}

replica_rows() {
  local md="$1" pods
  pods="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -n "$pods" ] || return 0
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath="{range .items[*]}{.metadata.name}|{range .metadata.ownerReferences[*]}{.uid}{\" \"}{end}{\"\n\"}{end}" 2>/dev/null \
    | while IFS='|' read -r wl owners; do
        [ -n "$wl" ] || continue
        printf '%s\n' "$pods" | while IFS='=' read -r pname puid; do
          case " $owners " in *" $puid "*) echo "$pname $puid $wl" ;; esac
        done
      done | sort
}

orphan_workloads() {
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].name}{"\n"}{end}' \
    2>/dev/null | grep -c "${1}-" || true
}

settle_verdicts() {
  local md="$1" want="$2" prev="" cur n r=0 w=0
  for _ in $(seq 1 60); do
    cur="$(md_workload_verdicts "$NS" "$md")"
    n="$(printf '%s' "$cur" | grep -c . || true)"
    r="$(printf '%s\n' "$cur" | grep -c '^[^=]*=True=' || true)"
    w="$(printf '%s\n' "$cur" | grep -c '^[^=]*=False=' || true)"
    if [ "$n" -eq "$want" ] && [ "$r" -eq "$want" ]; then
      echo "$r 0"
      return 0
    fi
    if [ "$n" -eq "$want" ] && [ "$w" -ge 1 ]; then
      [ "$prev" = "$r" ] && { echo "$r $((want - r))"; return 0; }
      prev="$r"
    else
      prev=""
    fi
    sleep 5
  done
  echo "$r $((want - r))"
  return 1
}

assigned_sets() {
  local wl
  for wl in $(md_workloads "$NS" "$1"); do
    k -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
      -o jsonpath='{range .status.admission.podSetAssignments[*]}{.name}{" "}{end}' 2>/dev/null
  done
}

wait_admitted() {
  local md="$1" want="$2" got
  for _ in $(seq 1 40); do
    got="$(assigned_sets "$md")"
    [ "$(echo "$got" | wc -w | tr -d ' ')" -ge "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  assigned_sets "$md"
  return 1
}

md_quota() {
  k -n "$NS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null
}

wl_reading() {
  k -n "$NS" get workloads.kueue.x-k8s.io "$1" \
    -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.message}{end}' 2>/dev/null
}

wl_waits() {
  case "$(wl_reading "$1")" in
    False\|*"${2:-}"*) return 0 ;;
    *) return 1 ;;
  esac
}

md_quotes() {
  case "$(md_quota "$1")" in
    *"\"$2\""*) return 0 ;;
    *) return 1 ;;
  esac
}

pods_rv() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${1}" \
    -o jsonpath='{range .items[*]}{.metadata.name}:{.metadata.resourceVersion} {end}' 2>/dev/null
}

# =================================================================================================
# PHASE 1: Per-replica Workloads, blast radius, twin PodSet names, and Service watch recreation
# =================================================================================================

md_apply "$MD_PD" "$NS" "$BINDING" "$(md_role_block prefill Prefill "$IT" 2 "$IMAGE")
$(md_role_block decode Decode "$IT" 2 "$IMAGE")" vLLM "$IMAGE"

if wait_pods "$MD_PD" 4; then
  record PASS "multi-role group creates all four replica Pods" "2 prefill + 2 decode"
else
  record FAIL "multi-role group creates all four replica Pods" \
    "got $(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PD}" --no-headers 2>/dev/null | wc -l | tr -d ' '); apply: ${MD_APPLY_OUT:0:200}"
fi

WL="$(wait_any_workload "$MD_PD" || true)"
if [ -z "$WL" ]; then
  record FAIL "Kueue composes one Workload per replica" "no Workload owns ${MD_PD} Pods"
else
  PD_WLS="$(workload_podsets "$MD_PD")"
  n_wl="$(printf '%s\n' "$PD_WLS" | grep -c . || true)"
  if [ "$n_wl" = 4 ]; then
    record PASS "Kueue composes one Workload per replica" "4 Workloads for 4 replicas"
  else
    record FAIL "Kueue composes one Workload per replica" "got ${n_wl}: [${PD_WLS//$'\n'/; }]"
  fi

  wrong="$(printf '%s\n' "$PD_WLS" | awk 'NF && $2 != "prefill=1" && $2 != "decode=1"')"
  n_prefill="$(printf '%s\n' "$PD_WLS" | grep -c ' prefill=1$' || true)"
  n_decode="$(printf '%s\n' "$PD_WLS" | grep -c ' decode=1$' || true)"
  if [ -z "$wrong" ] && [ "$n_prefill" = 2 ] && [ "$n_decode" = 2 ]; then
    record PASS "each Workload carries one PodSet of 1 named for its role" "2 prefill=1 and 2 decode=1"
  else
    record FAIL "each Workload carries one PodSet of 1 named for its role" \
      "prefill=${n_prefill}, decode=${n_decode}, unexpected=[${wrong//$'\n'/; }]"
  fi
fi

REPLICA_ROWS="$(replica_rows "$MD_PD")"
VICTIM="$(printf '%s\n' "$REPLICA_ROWS" | sed -n '1p')"
SIBLING="$(printf '%s\n' "$REPLICA_ROWS" | sed -n '2p')"
V_POD="${VICTIM%% *}"
V_WL="${VICTIM##* }"
S_POD="${SIBLING%% *}"
S_WL="${SIBLING##* }"
S_UID="$(printf '%s\n' "$SIBLING" | awk '{print $2}')"

if [ -z "$VICTIM" ] || [ -z "$SIBLING" ] || [ "$V_WL" = "$S_WL" ]; then
  record SKIP "deleting one replica's Workload leaves a sibling admitted" \
    "needs two replicas with distinct Workloads"
else
  k -n "$NS" delete workloads.kueue.x-k8s.io "$V_WL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  sleep 15
  S_UID_NOW="$(k -n "$NS" get pod "$S_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  S_ADM="$(k -n "$NS" get workloads.kueue.x-k8s.io "$S_WL" -o jsonpath='{range .status.conditions[?(@.type=="Admitted")]}{.status}{end}' 2>/dev/null)"
  if [ "$S_UID_NOW" = "$S_UID" ] && [ "$S_ADM" = True ]; then
    record PASS "deleting one replica's Workload leaves a sibling admitted" \
      "deleted ${V_WL}; ${S_POD} kept UID and ${S_WL} stayed Admitted=True"
  else
    record FAIL "deleting one replica's Workload leaves a sibling admitted" \
      "${S_POD} uid=${S_UID_NOW:-gone} (was ${S_UID}), ${S_WL} Admitted=${S_ADM:-absent}"
  fi

  V_PAIRS=0
  for _ in $(seq 1 20); do
    V_PAIRS="$(replica_rows "$MD_PD" | grep -c . || true)"
    [ "$V_PAIRS" = 4 ] && break
    sleep 3
  done
  if [ "$V_PAIRS" = 4 ]; then
    record PASS "deployment converges back to four replica/Workload pairs" "4 pairs after ${V_POD} recovery"
  else
    record FAIL "deployment converges back to four replica/Workload pairs" "settled at ${V_PAIRS} pairs"
  fi
fi

k -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD_PD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
GONE=no
for _ in $(seq 1 "$((DELETE_BOUND / 3))"); do
  if [ "$(k -n "$NS" get modeldeployments.worker.gpustack.ai "$MD_PD" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] \
    && [ "$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PD}" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] \
    && [ "$(orphan_workloads "$MD_PD")" = 0 ]; then
    GONE=yes
    break
  fi
  sleep 3
done
if [ "$GONE" = yes ]; then
  record PASS "deleting multi-role deployment removes Pods and Workloads" "gone within ${DELETE_BOUND}s"
else
  record FAIL "deleting multi-role deployment removes Pods and Workloads" \
    "orphans remaining after ${DELETE_BOUND}s"
fi
md_force_release "$NS" "$MD_PD" >/dev/null 2>&1 || true

# Twin identically-shaped roles keep distinct PodSet names.
md_apply "$MD_TWINS" "$NS" "$BINDING" "$(md_role_block left '' "$IT" 2 "$IMAGE")
$(md_role_block right '' "$IT" 2 "$IMAGE")" vLLM "$IMAGE"

if ! wait_pods "$MD_TWINS" 4; then
  record FAIL "identically-shaped twin roles keep distinct PodSet names" \
    "never reached 4 Pods; apply: ${MD_APPLY_OUT:0:200}"
else
  if [ -z "$(wait_any_workload "$MD_TWINS" || true)" ]; then
    record FAIL "identically-shaped twin roles keep distinct PodSet names" "no Workload composed"
  else
    TW_WLS="$(workload_podsets "$MD_TWINS")"
    n_left="$(printf '%s\n' "$TW_WLS" | grep -c ' left=1$' || true)"
    n_right="$(printf '%s\n' "$TW_WLS" | grep -c ' right=1$' || true)"
    if [ "$n_left" = 2 ] && [ "$n_right" = 2 ]; then
      record PASS "identically-shaped twin roles keep distinct PodSet names" "2 left=1 and 2 right=1"
    else
      record FAIL "identically-shaped twin roles keep distinct PodSet names" \
        "left=${n_left}, right=${n_right}: [${TW_WLS//$'\n'/; }]"
    fi
  fi
fi

release_deployment "$MD_TWINS"

# Service watch recreation on out-of-band Service deletion.
md_apply "$MD_SVC" "$NS" "$BINDING" "$(md_role_block server '' "$IT" 1 "$IMAGE")" vLLM "$IMAGE"
if ! k -n "$NS" get modeldeployments.worker.gpustack.ai "$MD_SVC" -o name >/dev/null 2>&1; then
  record FAIL "Service watch deployment created" "absent after apply: ${MD_APPLY_OUT:0:200}"
else
  first_uid=""
  for _ in $(seq 1 30); do
    first_uid="$(k -n "$NS" get svc "$MD_SVC" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    [ -n "$first_uid" ] && break
    sleep 3
  done
  if [ -z "$first_uid" ]; then
    record FAIL "deployment Service is created" "no svc/${MD_SVC} within 90s"
  else
    record PASS "deployment Service is created" "svc/${MD_SVC} uid=${first_uid}"
    replicas_seen=false
    for _ in $(seq 1 40); do
      [ -n "$(replica_versions "$MD_SVC")" ] && { replicas_seen=true; break; }
      sleep 3
    done
    quiet_for=0
    last="$(replica_versions "$MD_SVC")"
    if [ "$replicas_seen" = true ]; then
      for _ in $(seq 1 40); do
        sleep 3
        now="$(replica_versions "$MD_SVC")"
        if [ "$now" = "$last" ]; then
          quiet_for=$((quiet_for + 3))
          [ "$quiet_for" -ge 9 ] && break
        else
          quiet_for=0
        fi
        last="$now"
      done
    fi
    before="$(watched_versions "$MD_SVC")"
    k -n "$NS" delete svc "$MD_SVC" >/dev/null 2>&1 || true
    started=$SECONDS
    new_uid=""
    elapsed=0
    for _ in $(seq 1 "$RECREATE_BOUND"); do
      new_uid="$(k -n "$NS" get svc "$MD_SVC" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
      if [ -n "$new_uid" ] && [ "$new_uid" != "$first_uid" ]; then
        elapsed=$((SECONDS - started))
        break
      fi
      new_uid=""
      sleep 1
    done
    after="$(watched_versions "$MD_SVC")"
    if [ -n "$new_uid" ]; then
      record PASS "out-of-band deleted Service is recreated with a new UID" \
        "uid ${first_uid} -> ${new_uid} in ${elapsed}s"
    else
      record FAIL "out-of-band deleted Service is recreated with a new UID" \
        "svc/${MD_SVC} did not get a new UID within ${RECREATE_BOUND}s"
    fi
    if printf '%s\n%s\n' "$before" "$after" | command grep -Eq '=(NOT-FOUND|READ-FAILED)( |$)'; then
      record SKIP "Service recreation attributed solely to Service watch" "unobservable watched kind"
    elif [ "$replicas_seen" != true ] || [ "$quiet_for" -lt 9 ]; then
      record SKIP "Service recreation attributed solely to Service watch" "replicas not quiet (${quiet_for}s)"
    elif [ "$before" = "$after" ]; then
      record PASS "Service recreation attributed solely to Service watch" "no other watched object changed"
    else
      record SKIP "Service recreation attributed solely to Service watch" "another watched object moved in window"
    fi
  fi
fi

release_deployment "$MD_SVC"

# =================================================================================================
# PHASE 2: Short-pool whole-set starvation, Workload watch, and authored InstanceType mode
# =================================================================================================

run_shortage_suite() {
  local it="$1" cq="$2" prefix="$3"
  local probe="${prefix}-probe" filler="${prefix}-filler" subject="${prefix}-subject"
  PROBE_N=0
  FILL=0
  SUITE_SUBJECT="$subject"
  SUITE_WAIT_WL=""

  md_apply "$subject" "$NS" "$BINDING" "$(md_role_block prefill Prefill "$it" 1 "$IMAGE")
$(md_role_block decode Decode "$it" 1 "$IMAGE")" vLLM "$IMAGE"
  BASE="$(wait_admitted "$subject" 2 || true)"
  if [ "$(echo "$BASE" | wc -w | tr -d ' ')" = 2 ]; then
    record PASS "[${prefix}] free pool admits both roles" "podSetAssignments: ${BASE}"
  else
    record FAIL "[${prefix}] free pool admits both roles" "assignments='${BASE}'; apply: ${MD_APPLY_OUT:0:180}"
  fi

  CHARGE="$(workload_charge "$NS" "$(md_workload_verdicts "$NS" "$subject" | sed -n 's/=True=.*$//p' | head -n 1)")"
  release_deployment "$subject"

  BOUND=0
  FLAVORS=""
  BOUND_DETAIL=""
  IFS='|' read -r BOUND FLAVORS BOUND_DETAIL <<<"$(pool_replica_bound "$cq" "$CHARGE")"
  BOUND="${BOUND:-0}"
  if [ "$BOUND" -le 0 ] || [ $((BOUND + 1)) -gt "$MAX_REPLICAS" ]; then
    record SKIP "[${prefix}] pool capacity measurable within bound" \
      "cq=${cq} bound=${BOUND} max=${MAX_REPLICAS} (${BOUND_DETAIL:-unreadable})"
    return 0
  fi
  PROBE_N=$((BOUND + 1))

  PRE_RAW="$(k get clusterqueue "$cq" -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}' 2>/dev/null)"
  PRE_VER="${PRE_RAW%%|*}"
  PRE_COHORT="${PRE_RAW#*|}"
  if [ -z "$PRE_RAW" ] || { [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; } || [ -n "$PRE_COHORT" ]; then
    record SKIP "[${prefix}] pool has no borrowing cohort" "cq=${cq} ver=${PRE_VER:-?} cohort=${PRE_COHORT:-<none>}"
    PROBE_N=0
    return 0
  fi
  record PASS "[${prefix}] pool has no borrowing cohort" "${cq} (${PRE_VER}), ${FLAVORS} flavor(s)"

  md_apply "$probe" "$NS" "$BINDING" "$(md_role_block bulk '' "$it" "$PROBE_N" "$IMAGE")" vLLM "$IMAGE"
  PROBE_RC=0
  PROBED="$(settle_verdicts "$probe" "$PROBE_N")" || PROBE_RC=$?
  CAP="${PROBED%% *}"
  PROBE_WAIT="${PROBED##* }"
  release_deployment "$probe"
  if [ "$PROBE_RC" -ne 0 ]; then
    record FAIL "[${prefix}] pool capacity measured" "verdicts unsettled (${CAP}/${PROBE_N})"
    return 0
  elif [ "$PROBE_WAIT" -lt 1 ] || [ "$CAP" -lt 2 ]; then
    record SKIP "[${prefix}] pool capacity measured" "cap=${CAP}, waiting=${PROBE_WAIT} (need cap>=2 and wait>=1)"
    return 0
  fi
  FILL=$((CAP - 1))
  record PASS "[${prefix}] pool capacity measured" "cap=${CAP} of ${PROBE_N}, filler=${FILL}"

  md_apply "$filler" "$NS" "$BINDING" "$(md_role_block bulk '' "$it" "$FILL" "$IMAGE")" vLLM "$IMAGE"
  FILLED="$(wait_admitted "$filler" "$FILL" || true)"
  if [ "$(echo "$FILLED" | wc -w | tr -d ' ')" -lt "$FILL" ]; then
    record FAIL "[${prefix}] filler occupies all but one replica" "got $(echo "$FILLED" | wc -w | tr -d ' ')/${FILL}"
    FILL=0
    return 0
  fi
  record PASS "[${prefix}] filler occupies all but one replica" "${FILL}/${CAP} held"

  md_apply "$subject" "$NS" "$BINDING" "$(md_role_block prefill Prefill "$it" 1 "$IMAGE")
$(md_role_block decode Decode "$it" 1 "$IMAGE")" vLLM "$IMAGE"
  SUBJ_RC=0
  SUBJ="$(settle_verdicts "$subject" 2)" || SUBJ_RC=$?
  VERDICTS="$(md_workload_verdicts "$NS" "$subject")"
  HOLDING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=True=//p' | sort -u | tr '\n' ' ')"
  WAITING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=False=//p' | sort -u | tr '\n' ' ')"
  if [ "$SUBJ_RC" -ne 0 ]; then
    record FAIL "[${prefix}] short pool reserves quota for exactly one role" "unsettled: ${VERDICTS}"
    return 0
  elif [ "${SUBJ%% *}" != 1 ]; then
    record SKIP "[${prefix}] short pool reserves quota for exactly one role" "reserved=${SUBJ%% *} of 2"
    return 0
  fi
  HOLD_ROLE="${HOLDING% }"
  WAIT_ROLE="${WAITING% }"
  SUITE_WAIT_WL="$(printf '%s\n' "$VERDICTS" | sed -n 's/=False=.*$//p' | head -n 1)"
  record PASS "[${prefix}] short pool reserves quota for exactly one role" "${HOLD_ROLE} holds, ${WAIT_ROLE} waits"

  UNGATED=""
  SEEN=0
  for role in prefill decode; do
    gates="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${subject},app.kubernetes.io/component=${role}" \
      -o jsonpath="{range .items[*]}{.metadata.name}={.spec.schedulingGates[?(@.name=='${GATE}')].name}{\"\n\"}{end}" 2>/dev/null)"
    seen="$(printf '%s' "$gates" | grep -c . || true)"
    SEEN=$((SEEN + seen))
    n="$(printf '%s' "$gates" | grep -c -v "=${GATE}$" || true)"
    [ "${n:-0}" -gt 0 ] && UNGATED="${UNGATED}${role}:${n} "
  done
  if [ "$SEEN" -ge 2 ] && [ -z "$UNGATED" ]; then
    record PASS "[${prefix}] affordable role stays gated too" "all ${SEEN} replicas carry ${GATE}"
  else
    record FAIL "[${prefix}] affordable role stays gated too" "seen=${SEEN}, ungated=[${UNGATED:-none}]"
  fi

  HOLD_WL="$(printf '%s\n' "$VERDICTS" | sed -n 's/=True=.*$//p' | head -n 1)"
  JC=""
  for _ in $(seq 1 10); do
    JC="$(k -n "$NS" get workloads.kueue.x-k8s.io "$HOLD_WL" \
      -o jsonpath='{range .status.admissionChecks[?(@.name=="'"$JOINT"'")]}{.state}|{.message}{end}' 2>/dev/null)"
    case "$JC" in Pending\|*"roles ${WAIT_ROLE}"*) break ;; esac
    sleep 3
  done
  case "$JC" in
    Pending\|*"roles ${WAIT_ROLE}"*)
      record PASS "[${prefix}] joint check holds reserving role naming waiting role" "${HOLD_WL}: Pending (${WAIT_ROLE})"
      ;;
    *)
      record FAIL "[${prefix}] joint check holds reserving role naming waiting role" "got '${JC:-<none>}'"
      ;;
  esac

  QR=""
  QR_ROLES=""
  HAS_W=no
  HAS_H=no
  for _ in $(seq 1 10); do
    QR="$(md_quota "$subject")"
    QR_MSG="${QR#*|*|}"
    QR_ROLES="$(printf '%s' "$QR_MSG" | sed -n 's/.*the replicas of roles \([^.]*\)\..*/\1/p')"
    QR_WORDS=" $(printf '%s' "$QR_ROLES" | tr ',' ' ' | tr -s ' ') "
    case "$QR_WORDS" in *" ${WAIT_ROLE} "*) HAS_W=yes ;; *) HAS_W=no ;; esac
    case "$QR_WORDS" in *" ${HOLD_ROLE} "*) HAS_H=yes ;; *) HAS_H=no ;; esac
    case "$QR" in False\|Pending\|*) [ "$HAS_W" = yes ] && [ "$HAS_H" = no ] && break ;; esac
    sleep 3
  done
  case "$QR" in
    False\|Pending\|*)
      if [ "$HAS_W" = yes ] && [ "$HAS_H" = no ]; then
        record PASS "[${prefix}] QuotaReserved reports waiting role only" "roles=${QR_ROLES} (not ${HOLD_ROLE})"
      else
        record FAIL "[${prefix}] QuotaReserved reports waiting role only" "got roles='${QR_ROLES}' (${QR_MSG:0:120})"
      fi
      ;;
    *)
      record FAIL "[${prefix}] QuotaReserved reports waiting role only" "got '${QR:-<none>}'"
      ;;
  esac
}

finish_shortage_suite() {
  local prefix="$1"
  local filler="${prefix}-filler" subject="${prefix}-subject"
  if [ "$FILL" -ge 1 ]; then
    release_deployment "$filler"
    AFTER="$(wait_admitted "$subject" 2 || true)"
    if [ "$(echo "$AFTER" | wc -w | tr -d ' ')" = 2 ]; then
      record PASS "[${prefix}] releasing filler admits both roles" "podSetAssignments: ${AFTER}"
    else
      record FAIL "[${prefix}] releasing filler admits both roles" "assignments='${AFTER}'"
    fi
  fi
  release_deployment "$subject"
}

run_shortage_suite "$IT" "$CQ" e2e-c28-p2

# Workload-only watch check on the waiting replica from the shorted subject.
WAIT_WL="$SUITE_WAIT_WL"
if [ -n "$WAIT_WL" ] && md_quotes "$SUITE_SUBJECT" "$WAIT_WL"; then
  record PASS "shorted deployment quotes its waiting Workload" "${WAIT_WL}"
  RV0="$(pods_rv "$SUITE_SUBJECT")"
  for _ in $(seq 1 12); do
    sleep 5
    RV1="$(pods_rv "$SUITE_SUBJECT")"
    [ "$RV0" = "$RV1" ] && break
    RV0="$RV1"
  done
  RESERVED_BEFORE=no
  wl_waits "$WAIT_WL" || RESERVED_BEFORE=yes
  MARK="e2e-c28-marker-$(date +%s)"
  IDX="$(k -n "$NS" get workloads.kueue.x-k8s.io "$WAIT_WL" \
    -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}' 2>/dev/null \
    | awk '$0 == "QuotaReserved" {print NR - 1; exit}')"
  PATCH_OUT="$(k -n "$NS" patch workloads.kueue.x-k8s.io "$WAIT_WL" --subresource=status --type=json \
    -p "[{\"op\":\"test\",\"path\":\"/status/conditions/${IDX}/type\",\"value\":\"QuotaReserved\"},{\"op\":\"replace\",\"path\":\"/status/conditions/${IDX}/message\",\"value\":\"${MARK}\"}]" 2>&1 || true)"

  MARK_SEEN=""
  KEPT=yes
  START=$(date +%s)
  while [ $(($(date +%s) - START)) -lt "$WINDOW" ]; do
    wl_waits "$WAIT_WL" "$MARK" || KEPT=no
    if [ -z "$MARK_SEEN" ]; then
      case "$(md_quota "$SUITE_SUBJECT")" in *"$MARK"*) MARK_SEEN=$(($(date +%s) - START)) ;; esac
    fi
    sleep 2
  done
  RV2="$(pods_rv "$SUITE_SUBJECT")"

  if [ "$RESERVED_BEFORE" = yes ] || [ "$KEPT" = no ] || [ "$RV0" != "$RV2" ]; then
    record SKIP "Workload-only change reaches deployment within ${WINDOW}s" \
      "race in window (reserved=${RESERVED_BEFORE} kept=${KEPT} pods_moved=$([ "$RV0" != "$RV2" ] && echo yes || echo no))"
  elif [ -z "$IDX" ] || ! printf '%s' "$PATCH_OUT" | grep -q patched; then
    record FAIL "Workload-only change reaches deployment within ${WINDOW}s" "status patch failed: ${PATCH_OUT:0:160}"
  elif [ -n "$MARK_SEEN" ]; then
    record PASS "Workload-only change reaches deployment within ${WINDOW}s" "quoted ${MARK} in ${MARK_SEEN}s with zero Pod churn"
  else
    record FAIL "Workload-only change reaches deployment within ${WINDOW}s" "marker never quoted within ${WINDOW}s"
  fi

  POD="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${SUITE_SUBJECT}" -o name 2>/dev/null | head -n 1)"
  if wl_waits "$WAIT_WL" "$MARK" && [ -n "$POD" ]; then
    k -n "$NS" annotate "$POD" e2e-c28-wake="$MARK" --overwrite >/dev/null 2>&1 || true
    CTRL=no
    for _ in $(seq 1 10); do
      case "$(md_quota "$SUITE_SUBJECT")" in *"$MARK"*) CTRL=yes; break ;; esac
      sleep 1
    done
    if [ "$CTRL" = yes ]; then
      record PASS "control: Pod event wakes deployment to quote marker" "annotated ${POD#pod/}"
    else
      record FAIL "control: Pod event wakes deployment to quote marker" "marker not quoted after Pod annotation"
    fi
  else
    record SKIP "control: Pod event wakes deployment to quote marker" "waiting Workload no longer holds marker"
  fi
else
  record SKIP "Workload-only watch propagation" "no waiting Workload quoted by shorted subject"
fi

finish_shortage_suite e2e-c28-p2

# Administrator-authored InstanceType with instance-type-derived-from-node=false.
if [ -n "$GROUP" ] && [ -n "$OS" ] && [ -n "$ARCH" ]; then
  ACTIVE="$(k get admissionchecks.kueue.x-k8s.io "$JOINT" \
    -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)"
  if [ "$ACTIVE" != True ]; then
    record FAIL "joint AdmissionCheck is Active" "${JOINT} Active=${ACTIVE:-absent}"
  else
    record PASS "joint AdmissionCheck is Active" "$JOINT"
    setting_set "$SETTING" false
    settings_settle
    k delete instancetypes.worker.gpustack.ai "$AUTHORED" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    if k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${AUTHORED}
spec:
  displayName: case-28 authored
  acceleratable: false
  generalGroup: ${GROUP}
  os: ${OS}
  arch: ${ARCH}
  localStorage: 1Gi
  unitResources:
    cpu: "1"
    ram: 1Gi
YAML
    then
      authored_cq() { cq_of_instancetype "$NS" "$AUTHORED"; }
      authored_queue_filled() {
        local cq
        cq="$(authored_cq)"
        [ -n "$(k get clusterqueue "$cq" -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)" ]
      }
      authored_queue_joint() {
        local cq
        cq="$(authored_cq)"
        k get clusterqueue "$cq" -o jsonpath='{range .spec.admissionChecksStrategy.admissionChecks[*]}{.name}{"\n"}{end}' 2>/dev/null \
          | grep -qxF "$JOINT"
      }
      authored_entrance_ready() {
        local lq
        lq="$(k get instancetypes.worker.gpustack.ai "$AUTHORED" -o jsonpath='{.status.entrance}' 2>/dev/null)"
        [ -n "$lq" ] && k -n "$NS" get localqueue "$lq" >/dev/null 2>&1
      }

      if wait_until "$SETTLE" authored_queue_filled; then
        record PASS "authored InstanceType queue is filled (derived=false)" "$(authored_cq)"
        if wait_until 30 authored_queue_joint; then
          record PASS "authored InstanceType queue references joint check" "${JOINT}"
        else
          record FAIL "authored InstanceType queue references joint check" "missing ${JOINT} on $(authored_cq)"
        fi
        if wait_until "$SETTLE" authored_entrance_ready; then
          run_shortage_suite "$AUTHORED" "$(authored_cq)" e2e-c28-p2a
          finish_shortage_suite e2e-c28-p2a
        else
          record FAIL "authored InstanceType entrance LocalQueue reaches ${NS}" "timed out"
        fi
      else
        record FAIL "authored InstanceType queue is filled (derived=false)" "no flavors on $(authored_cq)"
      fi
    else
      record FAIL "authored InstanceType accepted" "kubectl apply failed"
    fi
    k delete instancetypes.worker.gpustack.ai "$AUTHORED" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    settings_restore
  fi
else
  record SKIP "administrator-authored InstanceType shortage" "working type lacks generalGroup/os/arch"
fi

# =================================================================================================
# PHASE 3: Two-InstanceType joint admission (hold, feasible control, park, and delayed open)
# =================================================================================================

p3_apply() {
  k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${NS}
spec:
  model:
    name: e2e-c28/model
  engine:
    name: vLLM
    image: ${IMAGE}
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
${2}
YAML
}

p3_group_count_is() { [ "$(md_group_names "$NS" "$1" | wc -l | tr -d ' ')" = "$2" ]; }
p3_role_count_is() {
  [ "$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${1},app.kubernetes.io/component=${2}" -o name 2>/dev/null | grep -c . || true)" = "$3" ]
}
p3_deployment_admitted() {
  local wl total=0 ok=0
  for wl in $(md_workloads "$NS" "$1" | sort -u); do
    total=$((total + 1))
    [ "$(k -n "$NS" get workload "$wl" -o jsonpath='{.status.conditions[?(@.type=="Admitted")].status}' 2>/dev/null)" = True ] && ok=$((ok + 1))
  done
  echo "${ok}/${total}"
}
p3_deployment_quota() {
  local wl total=0 res=0 adm=0 conds
  for wl in $(md_workloads "$NS" "$1" | sort -u); do
    total=$((total + 1))
    conds="$(k -n "$NS" get workload "$wl" -o jsonpath='{range .status.conditions[*]}{.type}={.status};{end}' 2>/dev/null)"
    case "$conds" in *"QuotaReserved=True;"*) res=$((res + 1)) ;; esac
    case "$conds" in *"Admitted=True;"*) adm=$((adm + 1)) ;; esac
  done
  echo "${res}/${adm}/${total}"
}
p3_md_admitted_is() { [ "$(p3_deployment_admitted "$1")" = "$2" ]; }
p3_md_quota_reason_is() {
  [ "$(k -n "$NS" get modeldeployment "$1" -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)" = "$2" ]
}

if [ -n "$GROUP" ] && [ -n "$OS" ] && [ -n "$ARCH" ]; then
  # Phase 3a: same-type two roles = 2 groups + scale preserves existing UIDs.
  p3_apply "$MD_PAIR" "    - name: alpha
      kind: Server
      replicas: 1
      acceleratorType: ${IT}
      image: ${IMAGE}
      command: [\"/pause\"]
    - name: beta
      kind: Server
      replicas: 1
      acceleratorType: ${IT}
      image: ${IMAGE}
      command: [\"/pause\"]"

  if wait_until "$SETTLE" p3_group_count_is "$MD_PAIR" 2; then
    record PASS "two roles on one type compose two distinct pod groups" "$(md_group_names "$NS" "$MD_PAIR" | tr '\n' ' ')"
  else
    record FAIL "two roles on one type compose two distinct pod groups" "saw [$(md_group_names "$NS" "$MD_PAIR" | tr '\n' ' ')]"
  fi

  before_beta="$(md_role_uids "$NS" "$MD_PAIR" beta)"
  before_alpha="$(md_role_uids "$NS" "$MD_PAIR" alpha)"
  if [ -n "$before_beta" ] && [ -n "$before_alpha" ]; then
    want_alpha="$(wc -w <<<"$before_alpha" | tr -d ' ')"
    if k -n "$NS" patch modeldeployment "$MD_PAIR" --type=json \
      -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' >/dev/null 2>&1 \
      && wait_until "$SETTLE" p3_role_count_is "$MD_PAIR" alpha "$((want_alpha + 1))"; then
      after_beta="$(md_role_uids "$NS" "$MD_PAIR" beta)"
      kept_alpha="$(md_surviving_uids "$NS" "$MD_PAIR" "$before_alpha" alpha)"
      if [ "$before_beta" = "$after_beta" ] && [ "$kept_alpha" = "$want_alpha" ]; then
        record PASS "scaling alpha preserves beta and existing alpha UIDs" "kept ${kept_alpha}/${want_alpha} alpha, beta untouched"
      else
        record FAIL "scaling alpha preserves beta and existing alpha UIDs" "kept ${kept_alpha}/${want_alpha} alpha"
      fi
    else
      record FAIL "scaling alpha preserves beta and existing alpha UIDs" "scale patch did not converge"
    fi
  fi
  release_deployment "$MD_PAIR"

  # Phase 3b: create Inactive second AcceleratorType + two-type hold + feasible control.
  k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: AcceleratorType
metadata:
  name: ${IT_HELD}
spec:
  displayName: case-28 held
  generalGroup: ${GROUP}
  acceleratorGroup: nvidia-e2e-c28-held
  os: ${OS}
  arch: ${ARCH}
  inactive: true
YAML

  held_queue_has_capacity() {
    local cq
    cq="$(md_cq_of_at "$NS" "$IT_HELD")"
    cq="${cq:-$IT_HELD}"
    [ -n "$(k get clusterqueue "$cq" -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)" ]
  }
  held_capacity=no
  wait_until "$SETTLE" held_queue_has_capacity && held_capacity=yes

  p3_two_types="    - name: alpha
      kind: Server
      replicas: 1
      acceleratorType: ${IT}
      image: ${IMAGE}
      command: [\"/pause\"]
    - name: beta
      kind: Server
      replicas: 1
      acceleratorType: ${IT_HELD}
      image: ${IMAGE}
      command: [\"/pause\"]"

  p3_apply "$MD_PAIR" "$p3_two_types"
  if wait_until "$SETTLE" p3_group_count_is "$MD_PAIR" 2; then
    wls="$(md_workloads "$NS" "$MD_PAIR" | sort -u | tr '\n' ' ')"
    n_wl="$(wc -w <<<"$wls" | tr -d ' ')"
    if [ "$n_wl" = 2 ]; then
      record PASS "two-InstanceType deployment composes two Workloads" "${wls}"
    else
      record FAIL "two-InstanceType deployment composes two Workloads" "got ${n_wl}: [${wls}]"
    fi
    sleep 30
    adm_counts="$(p3_deployment_admitted "$MD_PAIR")"
    if [ "${adm_counts%%/*}" = 0 ]; then
      record PASS "no role admitted while second AcceleratorType is Inactive" "${adm_counts} admitted"
    else
      record FAIL "no role admitted while second AcceleratorType is Inactive" "${adm_counts} admitted"
    fi
    reason_now="$(k -n "$NS" get modeldeployment "$MD_PAIR" -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)"
    if [ "$reason_now" = Reserved ]; then
      record FAIL "deployment does not claim Reserved while one type is held" "reason=Reserved"
    else
      record PASS "deployment does not claim Reserved while one type is held" "reason=${reason_now:-none}"
    fi

    # Control: feasible role alone IS admitted.
    p3_apply "$MD_CONTROL" "    - name: alpha
      kind: Server
      replicas: 1
      acceleratorType: ${IT}
      image: ${IMAGE}
      command: [\"/pause\"]"
    if wait_until "$SETTLE" p3_md_admitted_is "$MD_CONTROL" 1/1; then
      record PASS "control: feasible role alone is admitted" "1/1 admitted"
    else
      record FAIL "control: feasible role alone is admitted" "$(p3_deployment_admitted "$MD_CONTROL")"
    fi
    release_deployment "$MD_CONTROL"

    # Phase 3c: back-date LastTransitionTime -> park rather than delete.
    back="$(date -u -v-2H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '2 hours ago' '+%Y-%m-%dT%H:%M:%SZ')"
    parked_any=no
    for wl in $wls; do
      idx=0
      for cname in $(k -n "$NS" get workload "$wl" -o jsonpath='{range .status.admissionChecks[*]}{.name}{"\n"}{end}' 2>/dev/null); do
        if [ "$cname" = "$JOINT" ]; then
          k -n "$NS" patch workload "$wl" --subresource=status --type=json \
            -p '[{"op":"replace","path":"/status/admissionChecks/'"$idx"'/lastTransitionTime","value":"'"$back"'"}]' \
            >/dev/null 2>&1 && parked_any=yes
        fi
        idx=$((idx + 1))
      done
    done

    if [ "$parked_any" != yes ]; then
      record FAIL "back-dated joint check parks Workload" "joint check absent on [${wls}]"
    else
      any_deactivated() {
        local wl a
        for wl in $wls; do
          a="$(k -n "$NS" get workload "$wl" -o jsonpath='{.spec.active}' 2>/dev/null)"
          [ "$a" = false ] && return 0
        done
        return 1
      }
      if wait_until "$SETTLE" any_deactivated; then
        record PASS "back-dated joint check parks Workload (spec.active=false)" "deactivated"
      else
        record FAIL "back-dated joint check parks Workload (spec.active=false)" "not deactivated within ${SETTLE}s"
      fi
      wait_until "$SETTLE" p3_md_quota_reason_is "$MD_PAIR" Parked || true
      p_reason="$(k -n "$NS" get modeldeployment "$MD_PAIR" -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)"
      p_msg="$(k -n "$NS" get modeldeployment "$MD_PAIR" -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].message}' 2>/dev/null)"
      if [ "$p_reason" = Parked ] && [ -n "$p_msg" ]; then
        record PASS "deployment reports QuotaReserved reason=Parked" "reason=Parked"
      else
        record FAIL "deployment reports QuotaReserved reason=Parked" "reason=${p_reason:-none}"
      fi
      case "$p_msg" in
        *re-apply*) record PASS "parked message names re-apply recovery" "mentions re-apply" ;;
        *) record FAIL "parked message names re-apply recovery" "msg=${p_msg:0:160}" ;;
      esac
    fi
  else
    record FAIL "two-InstanceType deployment composes two groups" "never reached 2 groups"
  fi
  release_deployment "$MD_PAIR"

  # Phase 3d: delayed sibling opens the barrier (1/0/2 -> 2/2).
  p3_apply "$MD_OPENS" "$p3_two_types"
  one_reserved_none_admitted() { [ "$(p3_deployment_quota "$MD_OPENS")" = "1/0/2" ]; }
  if wait_until "$SETTLE" one_reserved_none_admitted; then
    record PASS "one group reserves and holds while second type is Inactive" "reserved/admitted/total=1/0/2"
    if [ "$held_capacity" != yes ]; then
      record SKIP "barrier opens when second type becomes Active" "held ClusterQueue never received a flavor"
    else
      k patch acceleratortype "$IT_HELD" --type=merge -p '{"spec":{"inactive":false}}' >/dev/null 2>&1 || true
      both_admitted() { [ "$(p3_deployment_admitted "$MD_OPENS")" = "2/2" ]; }
      if wait_until "$SETTLE" both_admitted; then
        record PASS "barrier opens when second type becomes Active" "2/2 admitted"
      else
        record FAIL "barrier opens when second type becomes Active" "got $(p3_deployment_admitted "$MD_OPENS")"
      fi
    fi
  else
    record FAIL "one group reserves and holds while second type is Inactive" "got $(p3_deployment_quota "$MD_OPENS")"
  fi
  release_deployment "$MD_OPENS"
  k delete acceleratortype.worker.gpustack.ai "$IT_HELD" --ignore-not-found --wait=true >/dev/null 2>&1 || true
else
  record SKIP "Phase 3 two-InstanceType joint admission checks" "working type lacks generalGroup/os/arch"
fi

# =================================================================================================
# PHASE 4: Three-role EPD (Encode/Prefill/Decode) short-pool whole-set starvation
# =================================================================================================

EPD_EC_VOLUMES=""
if [ -n "$EPD_EC_CLAIM" ]; then
  EPD_EC_VOLUMES="$(printf '      additionalVolumes:\n        - mountPath: /ec-cache\n          persistentVolumeClaim:\n            claimName: %s' "$EPD_EC_CLAIM")"
fi

EPD_IT="$(epd_platform_type "$EPD_PLATFORM")"
if [ -z "$EPD_IT" ]; then
  EPD_NODE="$(epd_pick_worker_node)"
  if [ -n "$EPD_NODE" ]; then
    echo "[case-28] injecting CPU vendor '${EPD_VENDOR}' on node ${EPD_NODE} for EPD starvation checks"
    epd_setup_cpu_vendor_fixture "$EPD_VENDOR" "$NS" "$EPD_NF" "$EPD_SETTLE_ROUNDS" "$SYSTEM_NS" "$EPD_NODE"
    EPD_IT="$EPD_FIXTURE_TYPE"
  fi
fi

if [ -z "$EPD_IT" ]; then
  record SKIP "Phase 4 three-role EPD short-pool starvation" \
    "no AcceleratorType reported manufacturer '${EPD_PLATFORM}'"
else
  EPD_ENTRANCE="$(k get acceleratortypes.worker.gpustack.ai "$EPD_IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
  [ -n "$EPD_ENTRANCE" ] || EPD_ENTRANCE="$(k get instancetypes.worker.gpustack.ai "$EPD_IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
  for _ in $(seq 1 "$EPD_SETTLE_ROUNDS"); do
    [ -n "$(k -n "$NS" get localqueue "${EPD_ENTRANCE:-}" -o name 2>/dev/null)" ] && break
    sleep 5
  done
  EPD_CQ="$(md_cq_of_it "$NS" "$EPD_IT")"
  if [ -z "$EPD_CQ" ]; then
    record SKIP "Phase 4 three-role EPD short-pool starvation" "no reachable ClusterQueue for ${EPD_IT}"
  else
    epd_apply_plain() {
      k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
    - name: bulk
      acceleratorType: ${EPD_IT}
      replicas: ${2}
      image: ${IMAGE}
      command: ["/pause"]
YAML
    }
    epd_apply_subject() {
      k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${EPD_SUBJECT}
  namespace: ${NS}
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
${EPD_EC_VOLUMES}
    - name: prefill
      kind: Prefill
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
${EPD_EC_VOLUMES}
    - name: decode
      kind: Decode
      acceleratorType: ${EPD_IT}
      replicas: 1
      image: ${IMAGE}
YAML
    }

    epd_apply_subject
    EPD_BASE="$(wait_admitted "$EPD_SUBJECT" 3 || true)"
    if [ "$(echo "$EPD_BASE" | wc -w | tr -d ' ')" = 3 ]; then
      record PASS "[epd] free pool admits all three roles" "podSetAssignments: ${EPD_BASE}"
    else
      record FAIL "[epd] free pool admits all three roles" "assignments='${EPD_BASE}'"
    fi
    EPD_CHARGE="$(workload_charge "$NS" "$(md_workload_verdicts "$NS" "$EPD_SUBJECT" | sed -n 's/=True=.*$//p' | head -n 1)")"
    release_deployment "$EPD_SUBJECT"

    EPD_BOUND=0
    EPD_DETAIL=""
    IFS='|' read -r EPD_BOUND _ EPD_DETAIL <<<"$(pool_replica_bound "$EPD_CQ" "$EPD_CHARGE")"
    EPD_BOUND="${EPD_BOUND:-0}"
    EPD_COHORT="$(k get clusterqueue "$EPD_CQ" -o jsonpath='{.spec.cohort}{.spec.cohortName}' 2>/dev/null)"
    if [ "$EPD_BOUND" -le 0 ] || [ $((EPD_BOUND + 1)) -gt "$MAX_REPLICAS" ] || [ -n "$EPD_COHORT" ]; then
      record SKIP "[epd] pool capacity measurable and unborrowed" "cq=${EPD_CQ} bound=${EPD_BOUND} cohort=${EPD_COHORT:-<none>} (${EPD_DETAIL:-?})"
    else
      EPD_PROBE_N=$((EPD_BOUND + 1))
      epd_apply_plain "$EPD_PROBE" "$EPD_PROBE_N"
      EPD_PROBE_RC=0
      EPD_PROBED="$(settle_verdicts "$EPD_PROBE" "$EPD_PROBE_N")" || EPD_PROBE_RC=$?
      EPD_CAP="${EPD_PROBED%% *}"
      EPD_WAIT="${EPD_PROBED##* }"
      release_deployment "$EPD_PROBE"

      if [ "$EPD_PROBE_RC" -ne 0 ]; then
        record FAIL "[epd] pool capacity measured" "unsettled (${EPD_CAP}/${EPD_PROBE_N})"
      elif [ "$EPD_WAIT" -lt 1 ] || [ "$EPD_CAP" -lt 3 ]; then
        record SKIP "[epd] pool capacity measured" "cap=${EPD_CAP} wait=${EPD_WAIT} (need cap>=3 and wait>=1)"
      else
        EPD_FILL=$((EPD_CAP - 1))
        epd_apply_plain "$EPD_FILLER" "$EPD_FILL"
        EPD_FILLED="$(wait_admitted "$EPD_FILLER" "$EPD_FILL" || true)"
        if [ "$(echo "$EPD_FILLED" | wc -w | tr -d ' ')" -lt "$EPD_FILL" ]; then
          record FAIL "[epd] filler occupies pool" "got $(echo "$EPD_FILLED" | wc -w | tr -d ' ')/${EPD_FILL}"
        else
          epd_apply_subject
          EPD_SUBJ_RC=0
          EPD_SUBJ="$(settle_verdicts "$EPD_SUBJECT" 3)" || EPD_SUBJ_RC=$?
          EPD_VERDICTS="$(md_workload_verdicts "$NS" "$EPD_SUBJECT")"
          EPD_HOLDING="$(printf '%s\n' "$EPD_VERDICTS" | sed -n 's/^[^=]*=True=//p' | sort -u | tr '\n' ' ')"
          EPD_WAITING="$(printf '%s\n' "$EPD_VERDICTS" | sed -n 's/^[^=]*=False=//p' | sort -u | tr '\n' ' ')"
          if [ "$EPD_SUBJ_RC" -eq 0 ] && [ "${EPD_SUBJ%% *}" = 2 ]; then
            record PASS "[epd] short pool reserves exactly two of three roles" "${EPD_HOLDING}hold, ${EPD_WAITING}waits"
            EPD_UNGATED=""
            EPD_SEEN=0
            for role in encode prefill decode; do
              gates="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${EPD_SUBJECT},app.kubernetes.io/component=${role}" \
                -o jsonpath="{range .items[*]}{.metadata.name}={.spec.schedulingGates[?(@.name=='${GATE}')].name}{\"\n\"}{end}" 2>/dev/null)"
              seen="$(printf '%s' "$gates" | grep -c . || true)"
              EPD_SEEN=$((EPD_SEEN + seen))
              n="$(printf '%s' "$gates" | grep -c -v "=${GATE}$" || true)"
              [ "${n:-0}" -gt 0 ] && EPD_UNGATED="${EPD_UNGATED}${role}:${n} "
            done
            if [ "$EPD_SEEN" -ge 3 ] && [ -z "$EPD_UNGATED" ]; then
              record PASS "[epd] all three roles stay gated including the two holding quota" "${EPD_SEEN}/3 gated"
            else
              record FAIL "[epd] all three roles stay gated including the two holding quota" "seen=${EPD_SEEN} ungated=[${EPD_UNGATED:-none}]"
            fi

            EPD_HOLD_WL="$(printf '%s\n' "$EPD_VERDICTS" | sed -n 's/=True=.*$//p' | head -n 1)"
            EPD_WAIT_ROLE="$(printf '%s' "${EPD_WAITING% }" | tr ' ' '\n' | sed -n 1p)"
            EPD_JC=""
            for _ in $(seq 1 10); do
              EPD_JC="$(k -n "$NS" get workloads.kueue.x-k8s.io "$EPD_HOLD_WL" \
                -o jsonpath='{range .status.admissionChecks[?(@.name=="'"$JOINT"'")]}{.state}|{.message}{end}' 2>/dev/null)"
              case "$EPD_JC" in Pending\|*"roles ${EPD_WAIT_ROLE}"*) break ;; esac
              sleep 3
            done
            case "$EPD_JC" in
              Pending\|*"roles ${EPD_WAIT_ROLE}"*) record PASS "[epd] joint check holds reserving group naming waiting role" "${EPD_HOLD_WL}: Pending (${EPD_WAIT_ROLE})" ;;
              *) record FAIL "[epd] joint check holds reserving group naming waiting role" "got '${EPD_JC:-<none>}'" ;;
            esac

            release_deployment "$EPD_FILLER"
            EPD_AFTER="$(wait_admitted "$EPD_SUBJECT" 3 || true)"
            if [ "$(echo "$EPD_AFTER" | wc -w | tr -d ' ')" = 3 ]; then
              record PASS "[epd] releasing filler admits all three roles at once" "${EPD_AFTER}"
            else
              record FAIL "[epd] releasing filler admits all three roles at once" "assignments='${EPD_AFTER}'"
            fi
          elif [ "$EPD_SUBJ_RC" -ne 0 ]; then
            record FAIL "[epd] short pool reserves exactly two of three roles" "unsettled: ${EPD_VERDICTS}"
          else
            record SKIP "[epd] short pool reserves exactly two of three roles" "reserved=${EPD_SUBJ%% *} of 3"
          fi
        fi
        release_deployment "$EPD_FILLER"
        release_deployment "$EPD_SUBJECT"
      fi
    fi
  fi
fi

results
