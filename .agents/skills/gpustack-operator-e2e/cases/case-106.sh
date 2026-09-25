#!/usr/bin/env bash
#
# CASE 106 — With instance-type-derived-from-node off, a P/D deployment on one administrator-authored
#   InstanceType is still admitted as a set   (MUTATING, self-cleaning)
#
#   case-106.sh <NS>
#
# Goal:        In the mode where the administrator authors every InstanceType, the queues used to
#              reference no AdmissionCheck at all. Every replica of every role is its own Workload, so
#              a short pool admitted the role that fit and left the other waiting: a prefiller serving
#              nothing while its decoder never arrives. The joint-admission check is now referenced
#              from every operator-owned queue whatever the setting says. This case proves it on a
#              queue created while the setting is off, then reruns CASE 50's shortage on that queue.
#
#              IT RUNS CASE 50 RATHER THAN COPYING IT. Case 50 already measures a short pool the only
#              way that holds on any cluster -- a probe asks Kueue how many replicas the queue takes,
#              and a filler leaves room for exactly one -- and it names the joint check on the role
#              that fit. What this case adds is the mode and a queue born in it.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an InstanceType
#              whose general group, OS and arch select a CPU pool. NO GPU is needed. The namespace must
#              receive the new type's entrance LocalQueue, which the operator creates in every
#              namespace. EXITS 2 (input required) when the cluster has no usable InstanceType. Needs
#              case-50.sh beside this file.
#
# Inputs:      Real objects only. The instance-type-derived-from-node key of the gpustack-settings
#              Secret in E2E_SYSTEM_NS (default gpustack-system), set to false for the run and put back
#              to the value it had; one CPU-only InstanceType `case106-authored` on the working type's
#              own CPU pool, created while the setting is off so its ClusterQueue is first filled in
#              that mode; and CASE 50's three ModelDeployments on that type. Override the working
#              type with E2E_MD_INSTANCE_TYPE.
#
# Expected:    - the gpustack-model-deployment-joint AdmissionCheck is Active (precondition);
#              - the authored type's ClusterQueue references it while the setting is off;
#              - every CASE 50 row passes on the authored type -- in particular, the role that fits a
#                short pool stays gated, and its Workload's joint check is Pending naming the other
#                role, and releasing the pool admits both.
#              Against an operator without this change, the queue row and CASE 50's two gating rows
#              FAIL: the role that fit is admitted on its own, which is the defect this case pins.
#
# Cleanup:     A trap deletes the InstanceType and restores the Setting. CASE 50 cleans up its own
#              deployments. Idempotent, runs on pass AND fail, safe to re-run.
#
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"

NS="${1:?usage: case-106.sh <NS>}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-derived-from-node
AUTHORED=case106-authored
JOINT=gpustack-model-deployment-joint
SETTLE="${E2E_MD_SETTLE:-120}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

wait_for() { # wait_for <seconds> <predicate...>
  local deadline=$((SECONDS + $1))
  shift
  while [ $SECONDS -lt "$deadline" ]; do
    "$@" && return 0
    sleep 5
  done

  return 1
}

# The first InstanceType a deployment can name: not terminating, not Inactive, and not this case's own
# type left over from a previous run. See case-68 for why the first one listed is not good enough.
usable_instance_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive; do
        [ -n "$name" ] || continue
        [ "$name" = "$AUTHORED" ] && continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        echo "$name"
        break
      done
}

if [ -z "$IT" ]; then
  IT="$(usable_instance_type)"
fi
if [ -z "$IT" ]; then
  echo "[case-106] no usable InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi
read -r GROUP OS ARCH <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
  -o jsonpath='{.spec.generalGroup} {.spec.os} {.spec.arch}' 2>/dev/null)"
if [ -z "$GROUP" ] || [ -z "$OS" ] || [ -z "$ARCH" ]; then
  echo "[case-106] InstanceType ${IT} names no general group, OS and arch to author a type on" >&2
  exit 2
fi

# The Setting is read through the worker's thirty-second cache, so every change waits it out.
ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
set_derived() { # true|false
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":\"$(printf '%s' "$1" | base64)\"}}" >/dev/null
  sleep 35
}
restore_setting() {
  local v=null
  [ -n "$ORIG_SETTING" ] && v="\"${ORIG_SETTING}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"${SETTING}\":${v}}}" >/dev/null 2>&1
}

cleanup() {
  kubectl delete instancetypes.worker.gpustack.ai "$AUTHORED" --ignore-not-found --wait=false >/dev/null 2>&1
  restore_setting
}
trap cleanup EXIT

# ------------------------------------------------------------------ precondition: the check is Active.
ACTIVE="$(kubectl get admissionchecks.kueue.x-k8s.io "$JOINT" \
  -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)"
if [ "$ACTIVE" != True ]; then
  record FAIL "the joint check is Active" "${JOINT} Active=[${ACTIVE:-absent}]; a queue references only an Active check"
  print_rows
  exit 1
fi
record PASS "the joint check is Active" "$JOINT"

# ----------------------------------------------- a queue born while the administrator authors types.
#
# THE TYPE IS CREATED AFTER THE FLIP, NOT BEFORE. Flipping the setting re-reads nothing on an existing
# queue until its resource groups are refilled, so a queue filled before the flip says what the
# default mode wrote and nothing about this one.
set_derived false
kubectl delete instancetypes.worker.gpustack.ai "$AUTHORED" --ignore-not-found --wait=true >/dev/null 2>&1
if ! kubectl apply -f - >/dev/null 2>&1 <<YAML; then
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: $AUTHORED
spec:
  displayName: case-106 authored
  acceleratable: false
  generalGroup: $GROUP
  os: $OS
  arch: $ARCH
  localStorage: 1Gi
  unitResources:
    cpu: "1"
    ram: 1Gi
YAML
  record FAIL "the authored InstanceType is accepted" "applying ${AUTHORED} was refused"
  print_rows
  exit 1
fi

queue_filled() {
  [ -n "$(kubectl get clusterqueue "$AUTHORED" -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)" ]
}
queue_references_joint() {
  kubectl get clusterqueue "$AUTHORED" \
    -o jsonpath='{range .spec.admissionChecksStrategy.admissionChecks[*]}{.name}{"\n"}{end}' 2>/dev/null \
    | grep -qxF "$JOINT"
}
entrance_ready() {
  local lq
  lq="$(kubectl get instancetypes.worker.gpustack.ai "$AUTHORED" -o jsonpath='{.status.entrance}' 2>/dev/null)"
  [ -n "$lq" ] && kubectl -n "$NS" get localqueue "$lq" >/dev/null 2>&1
}

if ! wait_for "$SETTLE" queue_filled; then
  record FAIL "the authored type's queue is filled" \
    "ClusterQueue ${AUTHORED} carries no flavor after ${SETTLE}s, so nothing below can reserve quota"
  print_rows
  exit 1
fi
record PASS "the authored type's queue is filled" "$(kubectl get clusterqueue "$AUTHORED" \
  -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)"

# THE QUEUE ROW IS WHAT DISCRIMINATES THIS MODE. A queue filled while the setting is off carried no
# AdmissionCheck at all before this change, and the check is referenced only once it is Active --
# which the precondition above established -- so a missing reference here is the operator's.
if wait_for 30 queue_references_joint; then
  record PASS "the authored type's queue references the joint check" "setting off, ${JOINT} referenced"
else
  record FAIL "the authored type's queue references the joint check" \
    "references [$(kubectl get clusterqueue "$AUTHORED" \
      -o jsonpath='{.spec.admissionChecksStrategy.admissionChecks[*].name}' 2>/dev/null)] with the setting off"
fi

if ! wait_for "$SETTLE" entrance_ready; then
  record FAIL "the authored type's entrance LocalQueue reaches ${NS}" "no LocalQueue after ${SETTLE}s"
  print_rows
  exit 1
fi

# --------------------------------------------------------------------- CASE 50 on the authored type.
#
# A SKIP IN CASE 50 EXITS 0, and a skipped shortage is exactly the run in which nothing was gated.
# So its table is read as well as its exit code, and a skipped row makes this row a SKIP rather than
# a PASS: the claim was not measured.
echo "[case-106] running case-50 on ${AUTHORED} with ${SETTING}=false"
C50_OUT="$(mktemp)"
E2E_MD_INSTANCE_TYPE="$AUTHORED" bash "${CASES_DIR}/case-50.sh" "$NS" 2>&1 | tee "$C50_OUT"
C50_RC=${PIPESTATUS[0]}
C50_SKIPS="$(grep -c '^SKIP | ' "$C50_OUT" || true)"
rm -f "$C50_OUT"
if [ "$C50_RC" -ne 0 ]; then
  record FAIL "case-50 passes on the authored type" "case-50 exited ${C50_RC}; see its table above"
elif [ "${C50_SKIPS:-0}" -gt 0 ]; then
  record SKIP "case-50 passes on the authored type" \
    "case-50 skipped ${C50_SKIPS} row(s), so the shortage was not measured; see its table above"
else
  record PASS "case-50 passes on the authored type" "every row of its table above passed"
fi

echo
echo "== case-106: authored-mode P/D admitted as a set =="
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-106] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-106] all checks passed"
