#!/usr/bin/env bash
#
# CASE 93 — A replica shed as surplus while it runs admitted releases Kueue's finalizer, and the
#   replica it sat beside keeps its Pod and its Workload   (MUTATING, self-recovering)
#
#   case-93.sh <NS>
#
# Goal:        A role can hold more live Pods than it declares with every one of them on a seat the
#              spec keeps -- two Pods on one ordinal, left by a create whose response was lost and
#              retried. The deployment controller sheds the excess and creates no replacement,
#              because the role is over its count rather than under it. Kueue holds a finalizer on
#              every Pod of a serving group and releases it only when that group's Workload is
#              deleted; a serving group never finishes on its own. So a shed Pod whose Workload
#              nobody deletes stays terminating forever, holding its quota, with nothing erroring.
#              The controller deletes the shed Pod's Workload with it. This case proves that on a
#              live Kueue, which is the only place it can be proved: a fake client runs no Kueue
#              controller, and there a shed Pod simply disappears whether or not anything released
#              it.
#
#              THE BASELINE ROW IS WHAT MAKES THE MAIN ONE A VERDICT. A running, admitted Pod that
#              is deleted by hand and whose Workload is left alone must still be there, finalizer in
#              place, after the hold window. If it is not, Kueue released it on its own on this
#              version, and "the shed Pod left" would prove nothing about the controller.
#
#              THE SURPLUS IS BUILT, not waited for. A twin of the seated Pod is created outside the
#              deployment (a different instance label, no owner), so the controller leaves it alone
#              while Kueue admits it into a group of its own; one patch then gives it the
#              deployment's label and owner, which is the declared-plus-one state with both Pods
#              running and admitted. Created with the deployment's label from the start, the twin
#              is shed while still gated, which is a different and easier question.
#
#              A TWIN IN THE SAME GROUP is the other shape the lost-response retry produces, and it
#              is answered as an outcome only: Kueue treats a group member beyond the group's total
#              as excess and deletes it itself, racing the controller's shed, and either is correct
#              so long as the seated Pod and its Workload are untouched.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose pod
#              integration is enabled, and room for three single-replica placeholder Pods on the
#              chosen InstanceType. The namespace must carry the pool's entrance LocalQueue --
#              `kubectl -n <NS> get localqueue`. No GPU; nothing here serves. EXITS 2 (input
#              required) when the cluster has no InstanceType.
#
# Inputs:      One ModelDeployment of one role and one replica, running a placeholder image
#              (E2E_MD_IMAGE, default pause) on the first usable InstanceType (E2E_MD_INSTANCE_TYPE).
#              MOCKED: the twin Pods are hand-made copies of the seated Pod -- the fixture that
#              stands in for a retried create. Real: Kueue's admission, finalizer and Workloads, and
#              the controller's shed.
#
# Expected:    - the seated Pod runs and its Workload reads Admitted=True;
#              - BASELINE: a hand-deleted, admitted twin nobody releases still exists with Kueue's
#                finalizer after the hold window (E2E_C93_HOLD, default 20s), and goes once its
#                Workload is deleted by hand;
#              - an admitted twin joined to the deployment on the seated Pod's ordinal: exactly one
#                of the two leaves within the bound (E2E_C93_BOUND, default 60s), its Workload is
#                deleted, and the one that stays keeps its UID and its Workload's UID, still
#                Admitted=True; no third Pod is created;
#              - a twin in the seated Pod's own group is gone within the bound, and the seated Pod
#                and its Workload keep their UIDs.
#
# Cleanup:     Trap deletes the deployment and every twin, then every Workload one of them owned
#              -- the manual form of the release the controller is asserted to do. Idempotent, runs
#              on pass AND fail, safe to re-run. It creates no cluster-scoped object and changes no
#              baseline.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-93.sh <NS>" >&2
  exit 2
fi
CASE_ID=93
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
HOLD="${E2E_C93_HOLD:-20}"
BOUND="${E2E_C93_BOUND:-60}"

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
MD="case93-${SFX}"
HOLD_INSTANCE="case93-hold-${SFX}"
WORK="$(mktemp -d)"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed"
  return 0
}

# The first InstanceType a deployment can name: not terminating, not inactive. See case-49 for why
# the first one listed is not good enough.
usable_instance_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive; do
        [ -n "$name" ] || continue
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
  echo "[case-93] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# Every Workload owning a Pod whose UID is listed, as "<name> <uid> <admitted>" lines. Found through
# the owner references because a Workload's name is Kueue's to choose.
workloads_of() {
  local uids=" $* "
  kubectl -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.uid}|{.status.conditions[?(@.type=="Admitted")].status}|{.metadata.ownerReferences[*].uid}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name uid admitted owners; do
        [ -n "$name" ] || continue
        for o in $owners; do
          case "$uids" in *" $o "*) echo "$name $uid ${admitted:-<none>}"; break ;; esac
        done
      done
}

pod_field() {
  kubectl -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null
}

# A running Pod whose Workload is admitted -- the state every row below starts from.
wait_running_admitted() {
  local pod="$1" i uid
  for ((i = 0; i < 120; i += 2)); do
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
  local pod="$1" secs="$2" i
  for ((i = 0; i < secs; i++)); do
    kubectl -n "$NS" get pod "$pod" >/dev/null 2>&1 || return 0
    sleep 1
  done
  return 1
}

# A copy of the seated Pod: same spec, same seat labels, same fingerprint -- so the controller
# cannot tell the two apart by anything but the name -- with the group, the instance label and the
# owner chosen by the row that makes it. The node binding and the scheduling gates are dropped so
# Kueue gates and admits it afresh.
make_twin() {
  local name="$1" group="$2" instance="$3" owned="$4"
  kubectl -n "$NS" get pod "$SEATED" -o json | python3 -c '
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
' "$name" "$group" "$instance" "$owned" >"$WORK/${name}.json" \
    && kubectl create -f "$WORK/${name}.json" >/dev/null
}

TWINS=()
TWIN_UIDS=()
cleanup() {
  echo
  echo "[case-93] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --ignore-not-found --wait=false >/dev/null 2>&1
  local t w
  for t in ${TWINS[@]+"${TWINS[@]}"}; do
    kubectl -n "$NS" delete pod "$t" --ignore-not-found --wait=false >/dev/null 2>&1
  done
  sleep 5
  for w in $(workloads_of ${TWIN_UIDS[@]+"${TWIN_UIDS[@]}"} ${SEATED_UID:-} | awk '{print $1}'); do
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$w" --ignore-not-found --wait=false >/dev/null 2>&1
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# ---------------------------------------------------------------- the seated replica

APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vllm
    version: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: case93-no-such-binding
  roles:
  - name: server
    instanceType: ${IT}
    replicas: 1
    image: ${IMAGE}
    command: ["/pause"]
YAML
)"

SEATED=""
for ((i = 0; i < 90; i += 2)); do
  SEATED="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$SEATED" ] && break
  sleep 2
done
if [ -z "$SEATED" ] || ! wait_running_admitted "$SEATED"; then
  record FAIL "the seated replica runs and is admitted" \
    "pod '${SEATED:-<none>}' never ran admitted; apply said: $(echo "$APPLY_OUT" | tr '\n' ' ' | cut -c1-200)"
  results; exit 1
fi
SEATED_UID="$(pod_field "$SEATED" '{.metadata.uid}')"
read -r SEATED_WL _ _ <<<"$(workloads_of "$SEATED_UID" | head -1)"
record PASS "the seated replica runs and is admitted" "${SEATED} in Workload ${SEATED_WL} (Admitted=True)"

# ---------------------------------------------------------------- baseline: nobody releases it

BASE="${MD}-base"
TWINS+=("$BASE")
if make_twin "$BASE" "case93-base-${SFX}" "$HOLD_INSTANCE" unowned && wait_running_admitted "$BASE"; then
  TWIN_UIDS+=("$(pod_field "$BASE" '{.metadata.uid}')")
  kubectl -n "$NS" delete pod "$BASE" --wait=false >/dev/null 2>&1
  sleep "$HOLD"
  BASE_FIN="$(pod_field "$BASE" '{.metadata.finalizers}')"
  if echo "$BASE_FIN" | grep -q 'kueue.x-k8s.io/managed'; then
    record PASS "an admitted Pod nobody releases stays held" \
      "${BASE} still present ${HOLD}s after its delete, finalizers=${BASE_FIN}"
  else
    record FAIL "an admitted Pod nobody releases stays held" \
      "${BASE} was released without its Workload being deleted (finalizers='${BASE_FIN:-<pod gone>}'); \
the surplus row below cannot tell the controller's release from Kueue's on this version"
  fi
  for w in $(workloads_of "${TWIN_UIDS[@]}" | awk '{print $1}'); do
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$w" --wait=false >/dev/null 2>&1
  done
  if gone_within "$BASE" "$BOUND"; then
    record PASS "deleting its Workload releases it" "${BASE} gone once its Workload was deleted"
  else
    record FAIL "deleting its Workload releases it" "${BASE} still present ${BOUND}s after its Workload was deleted"
  fi
else
  record FAIL "an admitted Pod nobody releases stays held" "the baseline twin ${BASE} never ran admitted"
fi

# ---------------------------------------------------------------- a running surplus on the seat

TWIN="${MD}-surplus"
TWINS+=("$TWIN")
if make_twin "$TWIN" "case93-surplus-${SFX}" "$HOLD_INSTANCE" unowned && wait_running_admitted "$TWIN"; then
  TWIN_UID="$(pod_field "$TWIN" '{.metadata.uid}')"
  TWIN_UIDS+=("$TWIN_UID")
  read -r TWIN_WL _ _ <<<"$(workloads_of "$TWIN_UID" | head -1)"
  OWNERS="$(pod_field "$SEATED" '{.metadata.ownerReferences}')"
  kubectl -n "$NS" patch pod "$TWIN" --type=merge \
    -p "{\"metadata\":{\"labels\":{\"app.kubernetes.io/instance\":\"${MD}\"},\"ownerReferences\":${OWNERS}}}" >/dev/null

  # Whichever of the two the controller keeps, exactly one leaves; which one is its tie-break on
  # the name, and nothing here depends on it.
  LEFT=""
  for ((i = 0; i < BOUND; i++)); do
    s=0; t=0
    kubectl -n "$NS" get pod "$SEATED" >/dev/null 2>&1 && s=1
    kubectl -n "$NS" get pod "$TWIN" >/dev/null 2>&1 && t=1
    if [ $((s + t)) -eq 1 ]; then
      [ "$s" -eq 0 ] && LEFT="$SEATED" || LEFT="$TWIN"
      break
    fi
    sleep 1
  done
  if [ -z "$LEFT" ]; then
    record FAIL "the shed surplus releases Kueue's finalizer" \
      "both ${SEATED} and ${TWIN} still present ${BOUND}s after the twin joined; seated finalizers=$(pod_field "$SEATED" '{.metadata.finalizers}') deletion=$(pod_field "$SEATED" '{.metadata.deletionTimestamp}'); twin finalizers=$(pod_field "$TWIN" '{.metadata.finalizers}') deletion=$(pod_field "$TWIN" '{.metadata.deletionTimestamp}')"
  else
    if [ "$LEFT" = "$TWIN" ]; then
      KEPT="$SEATED"; KEPT_UID="$SEATED_UID"; KEPT_WL="$SEATED_WL"; LEFT_UID="$TWIN_UID"; LEFT_WL="$TWIN_WL"
    else
      KEPT="$TWIN"; KEPT_UID="$TWIN_UID"; KEPT_WL="$TWIN_WL"; LEFT_UID="$SEATED_UID"; LEFT_WL="$SEATED_WL"
    fi
    record PASS "the shed surplus releases Kueue's finalizer" "${LEFT} left within ${i}s of the twin joining"
    if [ -z "$(workloads_of "$LEFT_UID")" ] \
      && ! kubectl -n "$NS" get workloads.kueue.x-k8s.io "$LEFT_WL" >/dev/null 2>&1; then
      record PASS "the shed surplus takes its Workload with it" "Workload ${LEFT_WL} is gone"
    else
      record FAIL "the shed surplus takes its Workload with it" "Workload ${LEFT_WL} still exists"
    fi
    sleep 5
    NOW_UID="$(pod_field "$KEPT" '{.metadata.uid}')"
    KEPT_ROW="$(workloads_of "$KEPT_UID" | head -1)"
    if [ "$NOW_UID" = "$KEPT_UID" ] && [ "$(echo "$KEPT_ROW" | awk '{print $1}')" = "$KEPT_WL" ] \
      && [ "$(echo "$KEPT_ROW" | awk '{print $3}')" = True ]; then
      record PASS "the replica left beside it keeps its Pod and its Workload" \
        "${KEPT} (${KEPT_UID}) in ${KEPT_WL}, Admitted=True"
    else
      record FAIL "the replica left beside it keeps its Pod and its Workload" \
        "${KEPT} uid now '${NOW_UID:-<gone>}' (was ${KEPT_UID}); its Workload row '${KEPT_ROW:-<none>}' (was ${KEPT_WL} Admitted=True)"
    fi
    N="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [ "$N" = 1 ]; then
      record PASS "no replacement is created for a surplus" "1 Pod for 1 declared replica"
    else
      record FAIL "no replacement is created for a surplus" "${N} Pods for 1 declared replica"
    fi
    SEATED="$KEPT"; SEATED_UID="$KEPT_UID"; SEATED_WL="$KEPT_WL"
  fi
else
  record FAIL "the shed surplus releases Kueue's finalizer" "the surplus twin ${TWIN} never ran admitted"
fi

# ---------------------------------------------------------------- a twin in the seated Pod's group

SAME="${MD}-samegroup"
TWINS+=("$SAME")
SEATED_GROUP="$(pod_field "$SEATED" '{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}')"
SEATED_WL_UID_NOW="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
if make_twin "$SAME" "$SEATED_GROUP" "$MD" owned; then
  if gone_within "$SAME" "$BOUND"; then
    record PASS "a twin in the seated group is removed" "${SAME} gone within the bound"
  else
    record FAIL "a twin in the seated group is removed" \
      "${SAME} still present ${BOUND}s later: finalizers=$(pod_field "$SAME" '{.metadata.finalizers}') gates=$(pod_field "$SAME" '{.spec.schedulingGates}')"
  fi
  sleep 5
  if [ "$(pod_field "$SEATED" '{.metadata.uid}')" = "$SEATED_UID" ] \
    && [ "$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)" = "$SEATED_WL_UID_NOW" ]; then
    record PASS "the seated replica survives a twin in its own group" "${SEATED} and ${SEATED_WL} keep their UIDs"
  else
    record FAIL "the seated replica survives a twin in its own group" \
      "${SEATED} uid now '$(pod_field "$SEATED" '{.metadata.uid}')' (was ${SEATED_UID}); ${SEATED_WL} uid now '$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)' (was ${SEATED_WL_UID_NOW})"
  fi
else
  record FAIL "a twin in the seated group is removed" "the same-group twin could not be created"
fi

results
