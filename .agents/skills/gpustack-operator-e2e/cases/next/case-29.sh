#!/usr/bin/env bash
#
# CASE 29 — ModelDeployment role scaling, multi-Pod size PodSet admission, surplus
#           retirement finalizer release, and ordinary role update whole-group replacement
#   (MUTATING, self-cleaning; Phase 2 role-shape replacement AUTO-SKIPS without a
#    suitable active NVIDIA AcceleratorType with onceMaxRequest=1)
#
#   case-29.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C29_BOUND,E2E_C29_HOLD,E2E_C29_NVIDIA_IT,E2E_C29_OTHER_IT,E2E_C29_CPU_IT,E2E_C29_UPDATE_IMAGE,E2E_C29_TIMEOUT,E2E_C29_RAW_DIR,E2E_MD_BINDING,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_ACCELERATOR_TYPE,E2E_MD_MEMBER_SETTLE,E2E_MD_SETTLE
# Goal:        Verify four core ModelDeployment lifecycle contracts:
#                1. Per-replica scaling (2 -> 3 -> 2): preserves original Pod UIDs, adds/removes
#                   one Workload, and moves ClusterQueue admittedWorkloads by +1 and back to exact
#                   baseline.
#                2. Surplus retirement finalizer release: an admitted twin nobody releases stays
#                   held by Kueue's managed finalizer until its Workload is deleted; a surplus twin
#                   on a role seat is shed along with its Workload while the survivor keeps its Pod
#                   and Workload UIDs with no replacement created; a same-group twin is removed
#                   without disturbing the seated Pod or Workload.
#                3. Multi-Pod instance (size=2) PodSet admission: composes 1 Workload with 1 PodSet
#                   of 2, shares one pod-group name without fast-admission, publishes 2 endpoints
#                   on the headless per-instance Service vs 1 leader endpoint on the role Service,
#                   resolves leader FQDN and GPUSTACK_* rank env vars inside the worker Pod, and
#                   scales to 2 instances preserving the first instance's UIDs.
#                4. Ordinary role shape edits whole-group replacement (real/mock NVIDIA): queued
#                   2->1 card reduction, size 1->2->1, command replacement, whole-card <-> logical
#                   slice edits, optional cross-pool replacement, and optional P/D sibling
#                   preservation.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), Kueue pod
#              integration enabled, and the pool's entrance LocalQueue in <NS> with capacity for at
#              least 4 placeholder Pods. Phase 2 additionally uses an active NVIDIA AcceleratorType
#              (mock or real) with onceMaxRequest=1 and >= 1 free unit (else Phase 2 SKIPs cleanly).
# Inputs:      REAL: ModelDeployments, Kueue Workloads, Services, ClusterQueue status, and
#              AcceleratorTypes. MOCKED (Phase 1b only): hand-made twin Pod copies standing in for
#              a retried create.
# Expected:    Scale trim, surplus finalizer release, multi-Pod size PodSet/Service/DNS/env, and
#              (when suitable NVIDIA type is present) whole-group replacement all PASS.
# Cleanup:     EXIT trap deletes all ModelDeployments, twin Pods, and owned Kueue Workloads, and
#              removes the temporary workspace directory.
set -uo pipefail

CASE_ID=29
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-29.sh <NS>" >&2
  exit 2
fi

MD="e2e-c29-scale"
MD_SIZE="e2e-c29-size"
ROLE="server"
SIZE=2
BINDING="${E2E_MD_BINDING:-e2e-c29-no-such-binding}"
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
MEMBER_IMAGE="${E2E_MD_IMAGE:-docker.io/library/busybox:1.36}"
SETTLE="${E2E_MD_SETTLE:-60}"
MEMBER_SETTLE="${E2E_MD_MEMBER_SETTLE:-${E2E_MD_SETTLE:-90}}"
HOLD="${E2E_C29_HOLD:-20}"
BOUND="${E2E_C29_BOUND:-60}"

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
HOLD_INSTANCE="e2e-c29-hold-${SFX}"
WORK="$(mktemp -d)"
TWINS=()
TWIN_UIDS=()

k() { kubectl "$@"; }

if [ -z "$IT" ]; then
  IT="$(md_usable_instance_type)"
fi
if [ -z "$IT" ]; then
  echo "[case-29] no usable AcceleratorType/InstanceType; run case-1 first" >&2
  exit 2
fi

pod_count() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${1}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true
}

running_count() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${1}" \
    --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | grep -c . || true
}

count_uids() {
  local n=0
  for _ in $1; do n=$((n + 1)); done
  echo "$n"
}

deployment_workloads() { md_workloads "$NS" "$1" | sort -u; }
wl_count() { deployment_workloads "$1" | grep -c . || true; }

queue_of() {
  local wl cq i=0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    wl="$(deployment_workloads "$1" | head -1)"
    if [ -n "$wl" ]; then
      cq="$(k -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
        -o jsonpath='{.status.admission.clusterQueue}' 2>/dev/null)"
      if [ -n "$cq" ]; then
        printf '%s' "$cq"
        return 0
      fi
    fi
    sleep 2
  done
  return 0
}

queue_reading() {
  local cq="$1"
  [ -n "$cq" ] || { echo "no-queue"; return 0; }
  printf 'admitted=%s usage=%s' \
    "$(k get clusterqueue "$cq" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)" \
    "$(k get clusterqueue "$cq" -o jsonpath='{range .status.flavorsUsage[*]}{range .resources[*]}{.name}:{.total}{" "}{end}{end}' 2>/dev/null)"
}

admitted_count() {
  local r="${1#admitted=}"
  r="${r%% *}"
  case "$r" in '' | *[!0-9]*) return 0 ;; esac
  printf '%s' "$r"
}

wait_queue_caught_up() {
  local cq="$1" mine i=0 seen
  mine="$(pod_count "$2")"
  [ -n "$cq" ] && [ "${mine:-0}" -gt 0 ] || return 0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    seen="$(admitted_count "$(queue_reading "$cq")")"
    [ -n "$seen" ] && [ "$seen" -ge "$mine" ] && return 0
    sleep 2
  done
  return 1
}

wait_queue_returns_to() {
  local cq="$1" want="$2" i=0
  [ -n "$cq" ] || return 0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    [ "$(queue_reading "$cq")" = "$want" ] && return 0
    sleep 2
  done
  return 1
}

wait_settled() {
  local md="$1" want_pods="$2" want_wls="$3" i=0
  while [ "$i" -lt "$SETTLE" ]; do
    i=$((i + 1))
    [ "$(pod_count "$md")" = "$want_pods" ] && [ "$(wl_count "$md")" = "$want_wls" ] && return 0
    sleep 2
  done
  return 1
}

wait_gone() {
  local i=0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    [ "$(pod_count "$1")" = 0 ] && return 0
    sleep 2
  done
  return 1
}

wait_instances() {
  local md="$1" instances="$2" i=0 want running
  want=$((instances * SIZE))
  while [ "$i" -lt "$MEMBER_SETTLE" ]; do
    i=$((i + 1))
    running="$(running_count "$md")"
    if [ "$(pod_count "$md")" = "$want" ] && [ "$(wl_count "$md")" = "$instances" ] && [ "$running" = "$want" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

workloads_of() {
  local uids=" $* "
  k -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.uid}|{.status.conditions[?(@.type=="Admitted")].status}|{.metadata.ownerReferences[*].uid}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name uid admitted owners; do
        [ -n "$name" ] || continue
        for o in $owners; do
          case "$uids" in *" $o "*) echo "$name $uid ${admitted:-<none>}"; break ;; esac
        done
      done
}

pod_field() { k -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null; }

wait_running_admitted() {
  local pod="$1" i=0 uid
  while [ "$i" -lt 120 ]; do
    i=$((i + 2))
    uid="$(pod_field "$pod" '{.metadata.uid}')"
    if [ "$(pod_field "$pod" '{.status.phase}')" = Running ] && [ -n "$uid" ] \
      && workloads_of "$uid" | grep -q ' True$'; then
      return 0
    fi
    sleep 2
  done
  return 1
}

gone_within() {
  local pod="$1" secs="$2" i=0
  while [ "$i" -lt "$secs" ]; do
    i=$((i + 1))
    k -n "$NS" get pod "$pod" >/dev/null 2>&1 || return 0
    sleep 1
  done
  return 1
}

group_count() { md_group_names "$NS" "$1" | grep -c . || true; }

podset_shapes() {
  local wl
  while IFS= read -r wl; do
    [ -n "$wl" ] || continue
    k -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
      -o jsonpath='{range .spec.podSets[*]}{.name}:{.count}{" "}{end}{"\n"}' 2>/dev/null
  done < <(deployment_workloads "$1")
}

endpoint_count() {
  k -n "$NS" get endpoints "$1" \
    -o jsonpath='{range .subsets[*]}{range .addresses[*]}{.ip}{"\n"}{end}{range .notReadyAddresses[*]}{.ip}{"\n"}{end}{end}' \
    2>/dev/null | grep -c . || true
}

service_names() {
  k -n "$NS" get services -l "app.kubernetes.io/instance=${1}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' | sort
}

make_twin() {
  local name="$1" group="$2" instance="$3" owned="$4"
  k -n "$NS" get pod "$SEATED" -o json | python3 -c '
import json, sys
name, group, instance, owned = sys.argv[1:5]
p = json.load(sys.stdin); m = p["metadata"]
labels = dict(m["labels"]); labels["kueue.x-k8s.io/pod-group-name"] = group
labels["app.kubernetes.io/instance"] = instance
ann = {k: v for k, v in m.get("annotations", {}).items() if k != "kueue.x-k8s.io/workload"}
meta = {"name": name, "namespace": m["namespace"], "labels": labels, "annotations": ann}
if owned == "owned":
    meta["ownerReferences"] = m["ownerReferences"]
spec = p["spec"]; spec.pop("nodeName", None); spec.pop("schedulingGates", None)
print(json.dumps({"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec}))
' "$name" "$group" "$instance" "$owned" >"${WORK}/${name}.json" \
    && k create -f "${WORK}/${name}.json" >/dev/null
}

release_twins() {
  local t w
  for t in ${TWINS[@]+"${TWINS[@]}"}; do
    k -n "$NS" delete pod "$t" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  for w in $(workloads_of ${TWIN_UIDS[@]+"${TWIN_UIDS[@]}"} | awk '{print $1}'); do
    k -n "$NS" delete workloads.kueue.x-k8s.io "$w" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}

cleanup() {
  local md pod row wl uids u
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  uids="${uids} $(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SIZE}" -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  for pod in ${TWINS[@]+"${TWINS[@]}"}; do
    uids="${uids} $(k -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  done
  for md in "$MD" "$MD_SIZE"; do
    k -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  release_twins
  sleep 5
  if [ -n "${uids// /}" ]; then
    while IFS= read -r row; do
      [ -n "$row" ] || continue
      wl="${row%%=*}"
      for u in $uids; do
        case " ${row#*=} " in
          *" $u "*)
            k -n "$NS" delete workloads.kueue.x-k8s.io "$wl" --ignore-not-found --wait=false >/dev/null 2>&1 || true
            break
            ;;
        esac
      done
    done <<EOF
$(k -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# =================================================================================================
# PHASE 1A: Scale trim (2 -> 3 -> 2) with UID preservation and ClusterQueue quota accounting
# =================================================================================================

APPLY_OUT="$(cat <<YAML | k apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
  - name: server
    kind: Server
    acceleratorType: ${IT}
    replicas: 2
    image: ${IMAGE}
    command: ["/pause"]
YAML
)"

SCALE_SETTLED=""
CQ=""
BASE_UIDS=""
BASE_Q=""
if wait_settled "$MD" 2 2; then
  SCALE_SETTLED=yes
  CQ="$(queue_of "$MD")"
  BASE_UIDS="$(md_pod_uids "$NS" "$MD")"
  wait_queue_caught_up "$CQ" "$MD" || true
  BASE_Q="$(queue_reading "$CQ")"
  record PASS "two replicas admit with one Workload each" "queue ${CQ:-<none>}: ${BASE_Q}"
else
  record FAIL "two replicas admit with one Workload each" \
    "got $(pod_count "$MD") Pods, $(wl_count "$MD") Workloads; apply: ${APPLY_OUT:0:200}"
fi

if [ "$SCALE_SETTLED" = yes ]; then
  if UP_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1)"; then
    record PASS "scaling up 2 -> 3 accepted" "patched"
  else
    record FAIL "scaling up 2 -> 3 accepted" "${UP_PATCH:0:200}"
  fi

  if wait_settled "$MD" 3 3; then
    record PASS "added replica gets a Workload of its own" "3 replicas, 3 Workloads"
    kept="$(md_surviving_uids "$NS" "$MD" "$BASE_UIDS")"
    want="$(count_uids "$BASE_UIDS")"
    if [ "$kept" = "$want" ]; then
      record PASS "scaling up preserves both original replica UIDs" "${kept}/${want} preserved"
    else
      record FAIL "scaling up preserves both original replica UIDs" "only ${kept}/${want} preserved"
    fi
    wait_queue_caught_up "$CQ" "$MD" || true
    UP_Q="$(queue_reading "$CQ")"
    BASE_ADM="$(admitted_count "$BASE_Q")"
    UP_ADM="$(admitted_count "$UP_Q")"
    if [ -n "$BASE_ADM" ] && [ -n "$UP_ADM" ] && [ "$UP_ADM" -eq "$((BASE_ADM + 1))" ]; then
      record PASS "ClusterQueue charged for exactly one more replica" "admitted ${BASE_ADM} -> ${UP_ADM}"
    else
      record FAIL "ClusterQueue charged for exactly one more replica" "before [${BASE_Q}] after [${UP_Q}]"
    fi
  else
    record FAIL "added replica gets a Workload of its own" "got $(pod_count "$MD") Pods, $(wl_count "$MD") Workloads"
  fi

  if DOWN_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1)"; then
    record PASS "scaling down 3 -> 2 accepted" "patched"
  else
    record FAIL "scaling down 3 -> 2 accepted" "${DOWN_PATCH:0:200}"
  fi

  if wait_settled "$MD" 2 2; then
    record PASS "shed replica removes its Workload" "2 replicas, 2 Workloads"
    kept_down="$(md_surviving_uids "$NS" "$MD" "$BASE_UIDS")"
    want_down="$(count_uids "$BASE_UIDS")"
    if [ "$kept_down" = "$want_down" ]; then
      record PASS "scaling down preserves original two replica UIDs" "${kept_down}/${want_down} preserved"
    else
      record FAIL "scaling down preserves original two replica UIDs" "only ${kept_down}/${want_down} preserved"
    fi
    wait_queue_returns_to "$CQ" "$BASE_Q" || true
    DOWN_Q="$(queue_reading "$CQ")"
    if [ -n "$(admitted_count "$BASE_Q")" ] && [ "$DOWN_Q" = "$BASE_Q" ]; then
      record PASS "ClusterQueue returns to exact starting numbers" "[${DOWN_Q}]"
    else
      record FAIL "ClusterQueue returns to exact starting numbers" "started [${BASE_Q}], ended [${DOWN_Q}]"
    fi
  else
    record FAIL "shed replica removes its Workload" "got $(pod_count "$MD") Pods, $(wl_count "$MD") Workloads"
  fi
fi

# =================================================================================================
# PHASE 1B: Surplus retirement finalizer release & same-group twin isolation
# =================================================================================================

SEAT_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":1}]' 2>&1 || true)"

SEATED=""
SEATED_UID=""
SEATED_WL=""
SEATED_ADMITTED=""
if wait_settled "$MD" 1 1; then
  for ((i = 0; i < 90; i += 2)); do
    SEATED="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$SEATED" ] && break
    sleep 2
  done
  if [ -n "$SEATED" ] && wait_running_admitted "$SEATED"; then
    SEATED_ADMITTED=yes
    SEATED_UID="$(pod_field "$SEATED" '{.metadata.uid}')"
    read -r SEATED_WL _ _ <<<"$(workloads_of "$SEATED_UID" | head -1)"
  fi
fi

if [ "$SEATED_ADMITTED" = yes ]; then
  record PASS "seated replica runs admitted for surplus test" "${SEATED} in ${SEATED_WL}"

  BASE="${MD}-base-${SFX}"
  TWINS+=("$BASE")
  if make_twin "$BASE" "e2e-c29-base-${SFX}" "$HOLD_INSTANCE" unowned && wait_running_admitted "$BASE"; then
    TWIN_UIDS+=("$(pod_field "$BASE" '{.metadata.uid}')")
    k -n "$NS" delete pod "$BASE" --wait=false >/dev/null 2>&1 || true
    sleep "$HOLD"
    BASE_FIN="$(pod_field "$BASE" '{.metadata.finalizers}')"
    if printf '%s' "$BASE_FIN" | grep -q 'kueue.x-k8s.io/managed'; then
      record PASS "unreleased admitted Pod stays held by Kueue finalizer" "${BASE} finalizers=${BASE_FIN}"
    else
      record FAIL "unreleased admitted Pod stays held by Kueue finalizer" "finalizers='${BASE_FIN:-<gone>}'"
    fi
    for w in $(workloads_of ${TWIN_UIDS[@]+"${TWIN_UIDS[@]}"} | awk '{print $1}'); do
      k -n "$NS" delete workloads.kueue.x-k8s.io "$w" --wait=false >/dev/null 2>&1 || true
    done
    if gone_within "$BASE" "$BOUND"; then
      record PASS "deleting Workload releases held Pod" "${BASE} gone"
    else
      record FAIL "deleting Workload releases held Pod" "${BASE} still present after ${BOUND}s"
    fi
  else
    record FAIL "unreleased admitted Pod stays held by Kueue finalizer" "baseline twin never ran admitted"
  fi

  TWIN="${MD}-surplus-${SFX}"
  TWINS+=("$TWIN")
  if make_twin "$TWIN" "e2e-c29-surplus-${SFX}" "$HOLD_INSTANCE" unowned && wait_running_admitted "$TWIN"; then
    TWIN_UID="$(pod_field "$TWIN" '{.metadata.uid}')"
    TWIN_UIDS+=("$TWIN_UID")
    read -r TWIN_WL _ _ <<<"$(workloads_of "$TWIN_UID" | head -1)"
    OWNERS="$(pod_field "$SEATED" '{.metadata.ownerReferences}')"
    k -n "$NS" patch pod "$TWIN" --type=merge \
      -p "{\"metadata\":{\"labels\":{\"app.kubernetes.io/instance\":\"${MD}\"},\"ownerReferences\":${OWNERS}}}" >/dev/null
    LEFT=""
    i=0
    while [ "$i" -lt "$BOUND" ]; do
      i=$((i + 1))
      s=0; t=0
      k -n "$NS" get pod "$SEATED" >/dev/null 2>&1 && s=1
      k -n "$NS" get pod "$TWIN" >/dev/null 2>&1 && t=1
      if [ $((s + t)) -eq 1 ]; then
        [ "$s" -eq 0 ] && LEFT="$SEATED" || LEFT="$TWIN"
        break
      fi
      sleep 1
    done
    if [ -z "$LEFT" ]; then
      record FAIL "shed surplus releases Kueue finalizer" "both ${SEATED} and ${TWIN} remained after ${BOUND}s"
    else
      if [ "$LEFT" = "$TWIN" ]; then
        KEPT="$SEATED"; KEPT_UID="$SEATED_UID"; KEPT_WL="$SEATED_WL"; LEFT_UID="$TWIN_UID"; LEFT_WL="$TWIN_WL"
      else
        KEPT="$TWIN"; KEPT_UID="$TWIN_UID"; KEPT_WL="$TWIN_WL"; LEFT_UID="$SEATED_UID"; LEFT_WL="$SEATED_WL"
      fi
      record PASS "shed surplus releases Kueue finalizer" "${LEFT} left in ${i}s"
      if [ -z "$(workloads_of "$LEFT_UID")" ] && ! k -n "$NS" get workloads.kueue.x-k8s.io "$LEFT_WL" >/dev/null 2>&1; then
        record PASS "shed surplus takes its Workload with it" "${LEFT_WL} gone"
      else
        record FAIL "shed surplus takes its Workload with it" "${LEFT_WL} still exists"
      fi
      sleep 5
      NOW_UID="$(pod_field "$KEPT" '{.metadata.uid}')"
      KEPT_ROW="$(workloads_of "$KEPT_UID" | head -1)"
      if [ "$NOW_UID" = "$KEPT_UID" ] && [ "$(echo "$KEPT_ROW" | awk '{print $1}')" = "$KEPT_WL" ] \
        && [ "$(echo "$KEPT_ROW" | awk '{print $3}')" = True ]; then
        record PASS "surviving replica keeps its Pod and Workload" "${KEPT} in ${KEPT_WL} (Admitted=True)"
      else
        record FAIL "surviving replica keeps its Pod and Workload" "uid=${NOW_UID:-gone} row='${KEPT_ROW:-none}'"
      fi
      if [ "$(pod_count "$MD")" = 1 ]; then
        record PASS "no replacement created for shed surplus" "1 Pod for 1 declared replica"
      else
        record FAIL "no replacement created for shed surplus" "$(pod_count "$MD") Pods"
      fi
      SEATED="$KEPT"; SEATED_UID="$KEPT_UID"; SEATED_WL="$KEPT_WL"
    fi
  else
    record FAIL "shed surplus releases Kueue finalizer" "surplus twin never ran admitted"
  fi

  SAME="${MD}-samegroup-${SFX}"
  TWINS+=("$SAME")
  SEATED_GROUP="$(pod_field "$SEATED" '{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}')"
  SEATED_WL_UID_NOW="$(k -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  if make_twin "$SAME" "$SEATED_GROUP" "$MD" owned; then
    TWIN_UIDS+=("$(pod_field "$SAME" '{.metadata.uid}')")
    if gone_within "$SAME" "$BOUND"; then
      record PASS "same-group twin is removed" "${SAME} gone"
    else
      record FAIL "same-group twin is removed" "${SAME} remained after ${BOUND}s"
    fi
    sleep 5
    if [ "$(pod_field "$SEATED" '{.metadata.uid}')" = "$SEATED_UID" ] \
      && [ "$(k -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)" = "$SEATED_WL_UID_NOW" ]; then
      record PASS "seated replica survives same-group twin removal" "${SEATED} and ${SEATED_WL} UIDs intact"
    else
      record FAIL "seated replica survives same-group twin removal" "Pod or Workload UID changed"
    fi
  else
    record FAIL "same-group twin is removed" "could not create same-group twin"
  fi
else
  record FAIL "seated replica runs admitted for surplus test" "pod '${SEATED:-<none>}' not admitted (${SEAT_PATCH:0:120})"
fi

# Release Phase 1A/1B resources before Phase 1C.
k -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
release_twins
wait_gone "$MD" || true

# =================================================================================================
# PHASE 1C: Multi-Pod instance (size=2) PodSet admission, headless Service, DNS/env, and scale
# =================================================================================================

APPLY_SIZE_OUT="$(cat <<YAML | k apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_SIZE}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${MEMBER_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
  - name: ${ROLE}
    kind: Server
    acceleratorType: ${IT}
    replicas: 1
    size: ${SIZE}
    image: ${MEMBER_IMAGE}
    command: ["sh", "-c", "sleep 100000"]
YAML
)"

if wait_instances "$MD_SIZE" 1; then
  record PASS "size=2 instance admits as one Workload" "2 Running members behind 1 Workload"

  GROUP_COUNT="$(group_count "$MD_SIZE")"
  if [ "$GROUP_COUNT" = 1 ]; then
    record PASS "members of one instance share one pod-group" "1 group name across 2 members"
  else
    record FAIL "members of one instance share one pod-group" "got ${GROUP_COUNT} group names"
  fi

  SHAPES="$(podset_shapes "$MD_SIZE" | tr -d '\n' | sed 's/ *$//')"
  if [ "$SHAPES" = "${ROLE}:${SIZE}" ]; then
    record PASS "Kueue composes one PodSet of size=2 named for the role" "[${SHAPES}]"
  else
    record FAIL "Kueue composes one PodSet of size=2 named for the role" "got [${SHAPES}], want [${ROLE}:${SIZE}]"
  fi

  FAST="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SIZE}" -o json 2>/dev/null | grep -c 'pod-group-fast-admission' || true)"
  if [ "$FAST" = 0 ]; then
    record PASS "no member carries pod-group-fast-admission annotation" "0 members"
  else
    record FAIL "no member carries pod-group-fast-admission annotation" "${FAST} member(s) carry it"
  fi

  RSVC="${MD_SIZE}-${ROLE}-r0"
  SVCS="$(service_names "$MD_SIZE" | tr '\n' ' ')"
  case " $SVCS " in
    *" $RSVC "*) record PASS "instance owns per-instance headless Service" "${RSVC}" ;;
    *) record FAIL "instance owns per-instance headless Service" "services=[${SVCS}]" ;;
  esac

  CLUSTER_IP="$(k -n "$NS" get service "$RSVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
  if [ "$CLUSTER_IP" = "None" ]; then
    record PASS "per-instance Service is headless" "clusterIP=None"
  else
    record FAIL "per-instance Service is headless" "clusterIP=${CLUSTER_IP:-<none>}"
  fi

  RSVC_EP="$(endpoint_count "$RSVC")"
  ROLE_EP="$(endpoint_count "${MD_SIZE}-${ROLE}")"
  if [ "$RSVC_EP" = "$SIZE" ] && [ "$ROLE_EP" = 1 ]; then
    record PASS "headless Service publishes both members while role Service fronts leader only" "${RSVC}=${RSVC_EP}, role=${ROLE_EP}"
  else
    record FAIL "headless Service publishes both members while role Service fronts leader only" "${RSVC}=${RSVC_EP} (want ${SIZE}), role=${ROLE_EP} (want 1)"
  fi

  LEADER="${MD_SIZE}-${ROLE}-r0-m0"
  WORKER="${MD_SIZE}-${ROLE}-r0-m1"
  LEADER_FQDN="${LEADER}.${RSVC}.${NS}.svc.cluster.local"

  if k -n "$NS" exec "$WORKER" -- sh -c 'command -v nslookup' >/dev/null 2>&1; then
    RESOLVED="$(k -n "$NS" exec "$WORKER" -- nslookup "$LEADER_FQDN" 2>&1 || true)"
    LEADER_IP="$(k -n "$NS" get pod "$LEADER" -o jsonpath='{.status.podIP}' 2>/dev/null)"
    if [ -n "$LEADER_IP" ] && [ -n "$RESOLVED" ] && [ -z "${RESOLVED##*"$LEADER_IP"*}" ]; then
      record PASS "worker resolves leader FQDN via headless Service" "${LEADER_FQDN} -> ${LEADER_IP}"
    else
      record FAIL "worker resolves leader FQDN via headless Service" "wanted ${LEADER_IP}, got: ${RESOLVED:0:160}"
    fi
  else
    record SKIP "worker resolves leader FQDN via headless Service" "image ${MEMBER_IMAGE} lacks nslookup"
  fi

  if k -n "$NS" exec "$WORKER" -- sh -c 'command -v env' >/dev/null 2>&1; then
    RANK="$(k -n "$NS" exec "$WORKER" -- env 2>/dev/null | grep '^GPUSTACK_' | sort | tr '\n' ' ')"
    WANT_LEADER="GPUSTACK_REPLICA_LEADER_ADDRESS=${LEADER}.${RSVC}"
    if [[ " $RANK " == *" $WANT_LEADER "* ]] && [[ " $RANK " == *" GPUSTACK_REPLICA_SIZE=${SIZE} "* ]] && [[ " $RANK " == *" GPUSTACK_MEMBER_INDEX=1 "* ]]; then
      record PASS "worker resolves GPUSTACK_* rank env vars via downward API" "[${RANK}]"
    else
      record FAIL "worker resolves GPUSTACK_* rank env vars via downward API" "got [${RANK:-<none>}]"
    fi
  else
    record SKIP "worker resolves GPUSTACK_* rank env vars via downward API" "image ${MEMBER_IMAGE} lacks env"
  fi

  BEFORE_UIDS="$(md_pod_uids "$NS" "$MD_SIZE")"
  SCALE_OUT="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD_SIZE" --type=merge \
    -p '{"spec":{"roles":[{"name":"'"$ROLE"'","kind":"Server","acceleratorType":"'"$IT"'","replicas":2,"size":'"$SIZE"',"image":"'"$MEMBER_IMAGE"'","command":["sh","-c","sleep 100000"]}]}}' \
    2>&1 || true)"

  if wait_instances "$MD_SIZE" 2; then
    KEPT="$(md_surviving_uids "$NS" "$MD_SIZE" "$BEFORE_UIDS")"
    if [ "$KEPT" = "$SIZE" ]; then
      record PASS "adding second size=2 instance preserves first instance UIDs" "${KEPT}/${SIZE} preserved"
    else
      record FAIL "adding second size=2 instance preserves first instance UIDs" "${KEPT}/${SIZE} preserved"
    fi
    if [ "$(group_count "$MD_SIZE")" = 2 ]; then
      record PASS "second size=2 instance has its own pod group" "2 distinct groups"
    else
      record FAIL "second size=2 instance has its own pod group" "got $(group_count "$MD_SIZE")"
    fi
    SVCS2="$(service_names "$MD_SIZE" | tr '\n' ' ')"
    case " $SVCS2 " in
      *" ${MD_SIZE}-${ROLE}-r1 "*) record PASS "second size=2 instance brings its own headless Service" "${MD_SIZE}-${ROLE}-r1" ;;
      *) record FAIL "second size=2 instance brings its own headless Service" "services=[${SVCS2}]" ;;
    esac
    ROLE_EP2="$(endpoint_count "${MD_SIZE}-${ROLE}")"
    if [ "$ROLE_EP2" = 2 ]; then
      record PASS "role Service grows to 2 leader endpoints" "2 endpoints for 2 instances of ${SIZE}"
    else
      record FAIL "role Service grows to 2 leader endpoints" "got ${ROLE_EP2}"
    fi
  else
    record FAIL "adding second size=2 instance preserves first instance UIDs" "never reached 2 instances; patch: ${SCALE_OUT:0:180}"
  fi
else
  record FAIL "size=2 instance admits as one Workload" "got $(pod_count "$MD_SIZE") Pods, $(wl_count "$MD_SIZE") Workloads; apply: ${APPLY_SIZE_OUT:0:200}"
fi

k -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD_SIZE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
md_force_release "$NS" "$MD_SIZE" >/dev/null 2>&1 || true
wait_gone "$MD_SIZE" || true

# =================================================================================================
# PHASE 2: Ordinary role shape edits whole-group replacement (real/mock NVIDIA AcceleratorType)
# =================================================================================================

NVIDIA_IT="${E2E_C29_NVIDIA_IT:-$(md_usable_type_for nvidia || true)}"
export E2E_C29_NAMESPACE="$NS"
export E2E_C29_RESOLVED_IT="$NVIDIA_IT"
export E2E_C29_SOURCE_ROOT
E2E_C29_SOURCE_ROOT="$(cd "${CASE_DIR}/../../../.." && pwd)"

PY_ROWS="${WORK}/phase2-rows.txt"
python3 - "$PY_ROWS" <<'PY' || true
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import uuid
from decimal import Decimal

ROWS_OUT = pathlib.Path(sys.argv[1])
NS = os.environ['E2E_C29_NAMESPACE']
IT = os.environ.get('E2E_C29_RESOLVED_IT', '')
OTHER = os.environ.get('E2E_C29_OTHER_IT', '')
CPU_IT = os.environ.get('E2E_C29_CPU_IT', '')
NAME = 'e2e-c29-upd-' + uuid.uuid4().hex[:8]
TIMEOUT = int(os.environ.get('E2E_C29_TIMEOUT', '180'))
RAW = pathlib.Path(os.environ['E2E_C29_RAW_DIR']) if os.environ.get('E2E_C29_RAW_DIR') else None
IMAGE = os.environ.get('E2E_C29_UPDATE_IMAGE', 'docker.io/library/busybox:1.36')
COMMAND = ['sh', '-c', 'sleep 86400']
ROWS = []
WORKLOADS = set()
MD_UID = None
OWNERSHIP_LABEL = 'gpustack.ai/e2e-case-29-token'
OWNERSHIP_TOKEN = 'e2e-c29-' + uuid.uuid4().hex
SOURCE = pathlib.Path(os.environ['E2E_C29_SOURCE_ROOT']) / 'pkg/nodefeature/knowns.go'
MAX_UNITS = int(re.search(r'ResourceMaxUnits\s*=\s*([0-9_]+)', SOURCE.read_text()).group(1).replace('_', ''))


def kubectl(*args, obj=None, allow_missing=False):
    result = subprocess.run(
        ['kubectl', '--request-timeout=30s', *args],
        input=json.dumps(obj) if obj is not None else None,
        capture_output=True, text=True, timeout=45,
    )
    if result.returncode:
        if allow_missing and 'NotFound' in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip() or result.stdout.strip())
    return json.loads(result.stdout) if result.stdout.lstrip().startswith('{') else result.stdout


def rec(status, check, detail):
    ROWS.append((status, check, str(detail).replace('\n', ' ')))


def discover_owned_deployment():
    global MD_UID
    current = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json', allow_missing=True)
    if current is None:
        return None
    if current.get('metadata', {}).get('labels', {}).get(OWNERSHIP_LABEL) != OWNERSHIP_TOKEN:
        return None
    MD_UID = current['metadata']['uid']
    return current


def create_owned_deployment(obj):
    global MD_UID
    try:
        created = kubectl('create', '-f', '-', '-o', 'json', obj=obj)
    except Exception:
        discover_owned_deployment()
        raise
    MD_UID = created['metadata']['uid']
    return created


def delete_owned_deployment(uid):
    options = {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': uid}}
    path = '/apis/worker.gpustack.ai/v1/namespaces/' + NS + '/modeldeployments/' + NAME
    kubectl('delete', '--raw', path, '-f', '-', obj=options)


def has_condition(obj, name):
    return any(c['type'] == name and c['status'] == 'True' for c in obj.get('status', {}).get('conditions', []))


def owns_workload(workload):
    return any(o.get('kind') == 'Pod' and o.get('name', '').startswith(NAME + '-')
               for o in workload['metadata'].get('ownerReferences', []))


def snapshot(label):
    md = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json')
    pods = kubectl('-n', NS, 'get', 'pods', '-l', 'app.kubernetes.io/instance=' + NAME, '-o', 'json')['items']
    pods = [p for p in pods if any(o['uid'] == md['metadata']['uid'] for o in p['metadata'].get('ownerReferences', []))]
    uids = {p['metadata']['uid'] for p in pods}
    workloads = kubectl('-n', NS, 'get', 'workloads.kueue.x-k8s.io', '-o', 'json')['items']
    workloads = [w for w in workloads if owns_workload(w) or any(o['uid'] in uids for o in w['metadata'].get('ownerReferences', []))]
    WORKLOADS.update(w['metadata']['name'] for w in workloads)
    result = {'deployment': md, 'pods': pods, 'workloads': workloads}
    if RAW:
        RAW.mkdir(parents=True, exist_ok=True)
        with (RAW / (NAME + '-' + label.replace('/', '-') + '.jsonl')).open('a') as stream:
            stream.write(json.dumps(result) + '\n')
    return result


def wait(label, predicate):
    deadline = time.monotonic() + TIMEOUT
    while True:
        state = snapshot(label)
        if predicate(state):
            return state
        if time.monotonic() >= deadline:
            raise RuntimeError(label + ' did not converge')
        time.sleep(2)


def main_container(pod):
    return next(c for c in pod['spec']['containers'] if c['name'] == 'main')


def accelerator_matches(container, role, inst_type):
    accelerated = inst_type['spec'].get('acceleratable', False)
    resources = role.get('resources', {})
    memory = resources.get('acceleratorSlicedMemoryPercentage', 0)
    cores = resources.get('acceleratorSlicedCoresPercentage', 0)
    for kind in ('requests', 'limits'):
        actual = container['resources'].get(kind, {})
        if memory or cores:
            expected_memory, expected_cores = memory or cores, cores or memory
            expected = {'nvidia.com/gpu.sliced': str(resources['accelerator']),
                        'nvidia.com/gpu.sliced.memory-percentage': str(expected_memory),
                        'nvidia.com/gpu.sliced.cores-percentage': str(expected_cores)}
            if not accelerated or any(actual.get(key) != value for key, value in expected.items()):
                return False
            units = actual.get('nvidia.com/gpu.sliced.units', '')
            if not units:
                return False
            scale = {'k': 1000, 'M': 1000000, 'G': 1000000000}.get(units[-1], 1)
            value = Decimal(units[:-1] if scale != 1 else units) * scale
            if value != expected_memory * MAX_UNITS // 100 or 'nvidia.com/gpu' in actual:
                return False
        else:
            expected = str(resources['accelerator']) if accelerated else None
            if actual.get('nvidia.com/gpu') != expected or 'nvidia.com/gpu.sliced' in actual:
                return False
    return True


def converged(state, old=None, changed='server'):
    md = state['deployment']
    roles = md['spec']['roles']
    pods, workloads = state['pods'], state['workloads']
    queued_roles = [r for r in roles if r.get('acceleratorType') or r.get('instanceType')]
    if md['metadata']['uid'] != MD_UID or len(pods) != sum(r['size'] for r in roles) or len(workloads) != len(queued_roles):
        return False
    if md['metadata'].get('annotations', {}).get('modeldeployment.gpustack.ai/replacement-slots'):
        return False
    if not all(has_condition(p, 'Ready') and not p['metadata'].get('deletionTimestamp') for p in pods):
        return False
    for role in roles:
        members = [p for p in pods if p['metadata']['labels']['app.kubernetes.io/component'] == role['name']]
        if len(members) != role['size']:
            return False
        member_uids = {p['metadata']['uid'] for p in members}
        owned = [w for w in workloads if any(o['uid'] in member_uids for o in w['metadata'].get('ownerReferences', []))]
        acc_type_name = role.get('acceleratorType') or role.get('instanceType')
        if not acc_type_name:
            if owned:
                return False
            for pod in members:
                if main_container(pod).get('command') != role['command'] or pod['metadata']['labels'].get('kueue.x-k8s.io/queue-name'):
                    return False
            continue
        if len(owned) != 1:
            return False
        workload = owned[0]
        if not has_condition(workload, 'Admitted') or not workload.get('status', {}).get('admission'):
            return False
        pod_set = workload['spec']['podSets']
        if len(pod_set) != 1 or pod_set[0]['name'] != role['name'] or pod_set[0]['count'] != role['size']:
            return False
        owners = {o['uid'] for o in workload['metadata'].get('ownerReferences', []) if o['kind'] == 'Pod'}
        if owners != member_uids:
            return False
        try:
            inst_type = kubectl('get', 'acceleratortypes.worker.gpustack.ai', acc_type_name, '-o', 'json')
        except Exception:
            inst_type = kubectl('get', 'instancetypes.worker.gpustack.ai', acc_type_name, '-o', 'json')
        entrance = inst_type['status']['entrance']
        if workload['spec']['queueName'] != entrance:
            return False
        for pod in members:
            container = main_container(pod)
            if container.get('command') != role['command'] or pod['metadata']['labels'].get('kueue.x-k8s.io/queue-name') != entrance:
                return False
            if not accelerator_matches(container, role, inst_type):
                return False
            if main_container(pod_set[0]['template']).get('resources') != container.get('resources'):
                return False
            if main_container(pod_set[0]['template']).get('command') != container.get('command'):
                return False
    if old:
        changed_pods = [p for p in old['pods'] if p['metadata']['labels']['app.kubernetes.io/component'] == changed]
        old_pods = {p['metadata']['uid'] for p in changed_pods}
        if old_pods.intersection(p['metadata']['uid'] for p in pods):
            return False
        for pod_before in changed_pods:
            current = kubectl('-n', NS, 'get', 'pods', pod_before['metadata']['name'], '-o', 'json', allow_missing=True)
            if current and current['metadata']['uid'] == pod_before['metadata']['uid']:
                return False
        for workload_before in old['workloads']:
            if not any(o['uid'] in old_pods for o in workload_before['metadata'].get('ownerReferences', [])):
                continue
            current = kubectl('-n', NS, 'get', 'workloads.kueue.x-k8s.io', workload_before['metadata']['name'], '-o', 'json', allow_missing=True)
            if current and current['metadata']['uid'] == workload_before['metadata']['uid']:
                return False
        for role in roles:
            if role['name'] == changed:
                continue
            def identity(view):
                uids = {p['metadata']['uid'] for p in view['pods'] if p['metadata']['labels']['app.kubernetes.io/component'] == role['name']}
                work = {w['metadata']['uid'] for w in view['workloads'] if any(o['uid'] in uids for o in w['metadata'].get('ownerReferences', []))}
                return uids, work
            if identity(state) != identity(old):
                return False
    return True


def edit(label, fields):
    old = snapshot(label + '-before')
    patch = [{'op': 'add', 'path': '/spec/roles/0/' + key, 'value': value} for key, value in fields.items()]
    kubectl('-n', NS, 'patch', 'modeldeployments.worker.gpustack.ai', NAME, '--type=json', '-p', json.dumps(patch))
    changed = old['deployment']['spec']['roles'][0]['name']
    wait(label, lambda state: converged(state, old, changed))
    rec('PASS', label, 'same deployment; fresh complete group and Admitted Workload')


def cleanup_py():
    if MD_UID is None:
        discover_owned_deployment()
    if MD_UID is None:
        return
    current = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json', allow_missing=True)
    if current is None:
        return
    if current.get('metadata', {}).get('labels', {}).get(OWNERSHIP_LABEL) != OWNERSHIP_TOKEN or current['metadata']['uid'] != MD_UID:
        return
    delete_owned_deployment(MD_UID)
    for name in sorted(WORKLOADS):
        kubectl('-n', NS, 'delete', 'workloads.kueue.x-k8s.io', name, '--ignore-not-found', '--wait=false')
    deadline = time.monotonic() + TIMEOUT
    while time.monotonic() < deadline:
        pods = kubectl('-n', NS, 'get', 'pods', '-l', 'app.kubernetes.io/instance=' + NAME, '-o', 'json')['items']
        md = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json', allow_missing=True)
        workloads = kubectl('-n', NS, 'get', 'workloads.kueue.x-k8s.io', '-o', 'json')['items']
        discovered = {w['metadata']['name'] for w in workloads if owns_workload(w)}
        for name in discovered - WORKLOADS:
            kubectl('-n', NS, 'delete', 'workloads.kueue.x-k8s.io', name, '--ignore-not-found', '--wait=false')
        WORKLOADS.update(discovered)
        remaining = [w for w in workloads if w['metadata']['name'] in WORKLOADS]
        if not pods and md is None and not remaining:
            return
        time.sleep(2)


try:
    if not IT:
        rec('SKIP', 'Phase 2 ordinary role shape whole-group replacement', 'no NVIDIA AcceleratorType available (set E2E_C29_NVIDIA_IT)')
    else:
        try:
            inst_type = kubectl('get', 'acceleratortypes.worker.gpustack.ai', IT, '-o', 'json')
        except Exception:
            inst_type = kubectl('get', 'instancetypes.worker.gpustack.ai', IT, '-o', 'json')
        resource = inst_type.get('status', {}).get('accelerator', {})
        suitable = (inst_type.get('status', {}).get('phase') == 'Active'
                    and inst_type.get('status', {}).get('detail', {}).get('manufacturer') == 'nvidia'
                    and resource.get('onceMaxRequest') == '1'
                    and int(resource.get('remaining', 0)) >= 1)
        if not suitable:
            rec('SKIP', 'Phase 2 ordinary role shape whole-group replacement',
                'needs Active NVIDIA AcceleratorType with onceMaxRequest=1 and >=1 free unit')
        else:
            obj = {'apiVersion': 'worker.gpustack.ai/v1', 'kind': 'ModelDeployment',
                   'metadata': {'name': NAME, 'namespace': NS, 'labels': {OWNERSHIP_LABEL: OWNERSHIP_TOKEN}},
                   'spec': {
                       'model': {'name': 'Qwen/Qwen2.5-0.5B-Instruct'},
                       'engine': {'name': 'vLLM', 'image': IMAGE},
                       'roles': [{
                           'name': 'server', 'kind': 'Server', 'replicas': 1, 'size': 1,
                           'acceleratorType': IT, 'image': IMAGE,
                           'command': COMMAND, 'resources': {'accelerator': '2' if int(resource['capacity']) >= 2 else '1'},
                           'terminationGracePeriodSeconds': 15}]}}
            create_owned_deployment(obj)
            def initially_queued(state):
                if len(state['pods']) != 1 or len(state['workloads']) != 1:
                    return False
                pod, workload = state['pods'][0], state['workloads'][0]
                return (not pod['spec'].get('nodeName') and not has_condition(workload, 'Admitted')
                        and any(g['name'] == 'kueue.x-k8s.io/admission' for g in pod['spec'].get('schedulingGates', []))
                        and not state['deployment']['metadata'].get('annotations', {}).get('modeldeployment.gpustack.ai/replacement-slots'))
            if int(resource['capacity']) >= 2:
                wait('initially queued without a slot', initially_queued)
                rec('PASS', 'initially queued group has no replacement slot', 'gated Pod and unadmitted Workload')
                edit('queued resources two to one', {'resources': {'accelerator': '1'}})
            else:
                rec('SKIP', 'queued resources two to one', 'one-card pool refuses initial 2-card request')
                wait('one-card baseline', converged)
            if int(resource['remaining']) >= 2:
                edit('role size one to two whole-group replacement', {'size': 2})
                edit('role size two to one whole-group replacement', {'size': 1})
            else:
                rec('SKIP', 'role size one to two and back', 'fewer than two free units')
            edit('takeover command whole-group replacement', {'command': ['sh', '-c', 'true; sleep 86400']})
            if int(inst_type['status'].get('acceleratorSliced', {}).get('capacity') or 0) > 0:
                edit('whole card to half slice replacement', {'resources': {'accelerator': '1',
                     'acceleratorSlicedMemoryPercentage': 50, 'acceleratorSlicedCoresPercentage': 50}})
                edit('slice fifty to forty percent replacement', {'resources': {'accelerator': '1',
                     'acceleratorSlicedMemoryPercentage': 40, 'acceleratorSlicedCoresPercentage': 40}})
                edit('slice back to whole card replacement', {'resources': {'accelerator': '1'}})
            else:
                rec('SKIP', 'logical slice resource replacements', 'no logical slice capacity')
            if OTHER:
                try:
                    second = kubectl('get', 'acceleratortypes.worker.gpustack.ai', OTHER, '-o', 'json')
                except Exception:
                    second = kubectl('get', 'instancetypes.worker.gpustack.ai', OTHER, '-o', 'json')
                if second['spec']['acceleratorGroup'] == inst_type['spec']['acceleratorGroup']:
                    raise RuntimeError('E2E_C29_OTHER_IT names the same accelerator group')
                edit('AcceleratorType cross-pool replacement', {'acceleratorType': OTHER})
            else:
                rec('SKIP', 'AcceleratorType cross-pool replacement', 'E2E_C29_OTHER_IT unset')
            if CPU_IT:
                cleanup_py()
                MD_UID = None
                WORKLOADS.clear()
                NAME += '-pd'
                obj['metadata']['name'] = NAME
                obj['spec']['roles'] = [{
                    'name': kind.lower(), 'kind': kind, 'replicas': 1, 'size': 1,
                    'image': IMAGE, 'command': COMMAND, 'terminationGracePeriodSeconds': 15,
                } for kind in ('Prefill', 'Decode')]
                create_owned_deployment(obj)
                wait('P/D baseline', converged)
                edit('prefill size 1->2 preserves decode', {'size': 2})
                edit('prefill size 2->1 preserves decode', {'size': 1})
                edit('prefill command replacement preserves decode', {'command': ['sh', '-c', 'true; sleep 86400']})
            else:
                rec('SKIP', 'P/D sibling preservation on role shape edit', 'E2E_C29_CPU_IT unset')
except Exception as error:
    rec('FAIL', 'Phase 2 ordinary role shape whole-group replacement', str(error))
finally:
    try:
        cleanup_py()
    except Exception as error:
        rec('FAIL', 'Phase 2 cleanup', str(error))

with ROWS_OUT.open('w') as f:
    for st, chk, det in ROWS:
        f.write(f'{st}|{chk}|{det}\n')
PY

if [ -f "$PY_ROWS" ]; then
  while IFS='|' read -r st chk det; do
    [ -n "$st" ] || continue
    record "$st" "$chk" "$det"
  done < "$PY_ROWS"
fi

results
