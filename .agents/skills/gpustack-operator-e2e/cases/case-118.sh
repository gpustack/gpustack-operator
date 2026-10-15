#!/usr/bin/env bash
#
# CASE 118 — A short pool starves an encoder/prefill/decode deployment WHOLE: all three groups wait
#            together
#   (MUTATING, self-recovering)
#   AUTO-SKIPS without an InstanceType that reports an admitted accelerator platform, which this
#   case builds by injecting a CPU vendor label; also skips, printing its numbers, when the pool it
#   measures cannot be shorted to a width that fits two of the three groups.
#
#   case-118.sh <NS>
#
# Goal:        An encoder/prefill/decode deployment is three pod groups, one per role, admitted as a
#              set: at initial admission, when one group cannot reserve quota, NONE of the three
#              starts. This is the same joint barrier case 50 pins for two roles, carried to the
#              shape that exists to need it — a role that is neither a prefiller nor a decoder, on a
#              queue with no accelerator anywhere in it.
#
#              THE SHORTAGE IS MEASURED, NOT COMPUTED, and the machinery is case 50's: a probe
#              larger than every flavor's quota summed asks Kueue how many replicas the pool really
#              reserves, a filler takes all but one replica of that, and the subject — three
#              one-replica groups — must then hold quota on exactly two roles while the third waits.
#              The rows assert the load-bearing half: every replica of all three roles still carries
#              the admission gate, INCLUDING the two roles that already hold quota, and the joint
#              check on a holding group names the waiting role. A deployment whose encoder started
#              alone would produce embeddings no role consumes, holding exactly the CPU the waiting
#              roles need.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose
#              pod integration is enabled, at least one Ready schedulable worker node that is not a
#              control-plane node, and the gpustack-settings Secret in the system namespace
#              (E2E_SYSTEM_NS, default gpustack-system). The queue is CPU-ONLY: the whole subject
#              reaches admission through the platform an injected CPU vendor label builds, with no
#              accelerator and no engine image — every verdict is read from Workloads, scheduling
#              gates and conditions, nothing is expected to serve.
#
#              AUTO-SKIP: the subject needs the platform InstanceType (the vendor label may not
#              take; NFD owns cpu-model.vendor_id). The probe holds up to E2E_C118_MAX_REPLICAS
#              (default 128) placeholder replicas, and the case skips, printing the bound, when the
#              pool's quota could hold more than that.
#
# Existing platform: if an InstanceType already reports the manufacturer named by E2E_C118_VENDOR
#              (an accelerator cluster, for example E2E_C118_VENDOR=Ascend), the case uses it and
#              injects nothing, so the label, the setting and the settle wait are skipped.
#
# Inputs:      MOCKED: one node label (the CPU vendor, "AMD" — the admitted row that needs no
#              shared claim) injected through a NodeFeature object, and one settings key
#              (CPU-manufacturer awareness) flipped on; both restored by the trap. REAL: three
#              ModelDeployments — a probe and a filler of plain single-role placeholders sized from
#              what Kueue reserves, and the three-role encoder subject. Each role carries an
#              explicit image because a CPU-only InstanceType has observed no accelerator and the
#              operator can synthesize no engine image (case 49's plumbing note).
#
# Expected:    With the quota free the subject is admitted as three assignments at once (the
#              baseline). With the pool shorted to one replica of room: exactly two of the three
#              roles reserve quota and one waits; every replica of all three roles still carries
#              the kueue.x-k8s.io/admission gate, the holding roles' included; a holding group's
#              joint admission check reads Pending and names the waiting role; the deployment's
#              QuotaReserved condition reports the wait naming the waiting role and neither holding
#              role. Releasing the filler admits the whole set at once.
#
# Cleanup:     A trap deletes all three deployments and releases any Workload still holding their
#              replicas, repeating the release until their Pods are gone, deletes the NodeFeature
#              object, restores the settings key to its exact original value and waits for the chain
#              to settle back: the derived type goes only once the setting cache flips and the pool
#              re-derives. Idempotent, runs on pass AND fail, safe to re-run. It creates no
#              cluster-scoped object, changes no ClusterQueue and touches no baseline: the shortage
#              is made by occupying the quota, never by editing it.
#
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-118.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
VENDOR="${E2E_C118_VENDOR:-AMD}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"

# A platform whose encoder-cache connector carries the output through a directory (Ascend) is
# admitted only when the encode and prefill roles mount one shared claim. Name an existing
# ReadWriteMany claim in E2E_C118_EC_CLAIM; it stays empty for platforms that need none.
EC_CLAIM="${E2E_C118_EC_CLAIM:-}"
EC_VOLUMES=""
if [ -n "$EC_CLAIM" ]; then
  EC_VOLUMES="$(printf '      additionalVolumes:\n        - mountPath: /ec-cache\n          persistentVolumeClaim:\n            claimName: %s' "$EC_CLAIM")"
fi
NF="case118-cpu-vendor"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
MAX_REPLICAS="${E2E_C118_MAX_REPLICAS:-128}"
GATE="kueue.x-k8s.io/admission"
JOINT=gpustack-model-deployment-joint
SETTLE_ROUNDS="${E2E_C118_SETTLE:-60}"   # 60 x 5s for the settings cache and re-derivation

PROBE=case118-probe
FILLER=case118-filler
SUBJECT=case118-subject

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_quota-lib.sh"

# ---------------------------------------------------------------- the fixture

pick_worker_node() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("items",[]):
    cs=n.get("status",{}).get("conditions",[])
    if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in cs):
        continue
    if n.get("spec",{}).get("unschedulable"):
        continue
    # A CPU-vendor probe only reads back on a node that feeds the CPU-only catalog. An
    # acceleratable node feeds the GPU InstanceType instead, whose manufacturer is the
    # accelerator vendor, so an injected cpu-model.vendor_id never surfaces here and the
    # platform-type poll below times out. Skip those nodes, exactly as case-18 reads the
    # CPU pool (acceleratable=false) rather than the accelerator one.
    if n.get("metadata",{}).get("labels",{}).get("feature.gpustack.ai/acceleratable") == "true":
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

# One list call, because this runs from the EXIT trap on every invocation, pass or fail: Kueue holds
# a finalizer on every Pod of a serving group and releases it only when the Workload is deleted.
force_release() {
  local md="$1" row wl uids owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
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

INJECTED=false
cleanup() {
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    "$PROBE" "$FILLER" "$SUBJECT" --ignore-not-found --wait=false >/dev/null 2>&1
  # The release is a loop, not one pass: the probe holds up to MAX_REPLICAS Pods, a deletion takes
  # longer than a single round trip, and force_release deletes only the Workloads whose owners it can
  # still see. One pass finds nothing while the Pods are still going and leaves them holding the quota
  # the next case needs.
  for _ in $(seq 1 12); do
    sleep 5
    force_release "$PROBE"
    force_release "$FILLER"
    force_release "$SUBJECT"
    [ -z "$(kubectl -n "$NS" get pods \
      -l "app.kubernetes.io/instance in (${PROBE},${FILLER},${SUBJECT})" \
      --no-headers 2>/dev/null)" ] && break
  done
  if [ "$INJECTED" = true ]; then
    kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
    restore_setting
    settle_back
  fi
}
trap cleanup EXIT

IT="$(epd_platform_type)"
if [ -z "$IT" ]; then
  NODE="$(pick_worker_node)"
  if [ -z "$NODE" ]; then
    echo "[case-118] no Ready schedulable worker node; this case cannot place its vendor label" >&2
    exit 2
  fi

  echo "[case-118] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"
  INJECTED=true

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

  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    IT="$(epd_platform_type)"
    [ -n "$IT" ] && break
    sleep 5
  done
fi

if [ -z "$IT" ]; then
  record SKIP "the pool this case shorts exists" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  print_rows
  echo "[case-118] skipped: no platform InstanceType"
  exit 0
fi

# The entrance LocalQueue in this namespace routes the groups into the queue; the operator
# materializes it in every non-reserved namespace once the queue exists.
ENTRANCE="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  [ -n "$(kubectl -n "$NS" get localqueue "$ENTRANCE" -o name 2>/dev/null)" ] && break
  sleep 5
done

CQ="$(kubectl -n "$NS" get localqueue "$ENTRANCE" -o jsonpath='{.spec.clusterQueue}' 2>/dev/null)"
if [ -z "$CQ" ]; then
  echo "[case-118] the platform InstanceType names no reachable ClusterQueue through ${NS}/${ENTRANCE}" >&2
  exit 2
fi

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}' behind cluster queue ${CQ}"

# A queue references only an Active check, so the joint mechanism this case reads is either present
# and Active or nothing below can pass for a reason that has nothing to do with the barrier.
ACTIVE="$(kubectl get admissionchecks.kueue.x-k8s.io "$JOINT" \
  -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)"
if [ "$ACTIVE" = True ]; then
  record PASS "the joint check is Active" "${JOINT}"
else
  record FAIL "the joint check is Active" "${JOINT} Active=[${ACTIVE:-absent}]; a queue references only an Active check"
fi

# ---------------------------------------------------------------- the deployments

# The apply's output is KEPT: a webhook refusal turns into a timeout whose FAIL row blames Kueue
# otherwise, and never quotes the actual message (case-50's lesson).
APPLY_OUT=""
apply_plain() { # <name> <replicas> — one bulk placeholder role
  APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    version: "0.30.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
    - name: bulk
      instanceType: ${IT}
      replicas: ${2}
      image: ${IMAGE}
      command: ["/pause"]
YAML
)"
}

apply_subject() {
  APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${SUBJECT}
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
${EC_VOLUMES}
    - name: prefill
      kind: Prefill
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
${EC_VOLUMES}
    - name: decode
      kind: Decode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"
}

# ---------------------------------------------------------------- the reading instruments (case 50's)

# Every Workload owning one of this deployment's Pods, one "name=status=role" line each. Two API
# calls per invocation, not one per Workload: this is the hot path of every settle loop below.
workload_verdicts() {
  local md="$1" pods row wl rest status owners p
  pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}={.metadata.labels.app\.kubernetes\.io/component}{"\n"}{end}' \
    2>/dev/null)"
  [ -n "$pods" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    rest="${row#*=}"
    status="${rest%%=*}"
    owners="${rest#*=}"
    for p in $pods; do
      # Padded on both sides so a uid cannot match a longer one it is a prefix of.
      case " $owners " in *" ${p%%=*} "*)
        echo "${wl}=${status}=${p#*=}"
        break
        ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="QuotaReserved")].status}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

# "RESERVED WAITING" once every replica has a Workload and the counts stop moving. Waiting is what
# did NOT reserve, not what Kueue judged short: once one replica is turned away, the identical ones
# behind it carry no condition at all.
settle_verdicts() {
  local md="$1" want="$2" prev="" cur n r=0 w=0
  for _ in $(seq 1 60); do
    cur="$(workload_verdicts "$md")"
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

deployment_workloads() {
  local md="$1" uids row wl owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in *" $u "*) echo "$wl"; break ;; esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

# Every PodSet this deployment holds an assignment for, across ALL of its Workloads: three words is
# the whole set, and reading a single Workload yields one and looks exactly like a starved role.
assigned_sets() {
  local wl
  for wl in $(deployment_workloads "$1"); do
    kubectl -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
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
  echo "$(assigned_sets "$md")"

  return 1
}

# Deletes a deployment and waits for its Pods to go, then deletes any Workload that outlived them.
# The Workloads are named BEFORE the delete: force_release finds them through the Pods.
release_deployment() {
  local md="$1" wls wl
  wls="$(workload_verdicts "$md" | cut -d= -f1)"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 60); do
    [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
    sleep 2
  done
  force_release "$md"
  for wl in $wls; do
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done

  return 0
}

# ---------------------------------------------------------------- row 0: the baseline, on a free pool

apply_subject
BASE="$(wait_admitted "$SUBJECT" 3)"
if [ "$(echo "$BASE" | wc -w | tr -d ' ')" = 3 ]; then
  record PASS "with the pool free, the three-role group is admitted" \
    "podSetAssignments: ${BASE}"
else
  record FAIL "with the pool free, the three-role group is admitted" \
    "assignments: '${BASE}' - the baseline did not admit, so every row below would starve for the wrong reason. apply said: ${APPLY_OUT:0:200}"
fi

# What Kueue charged ONE replica, read off a baseline Workload while it still holds its reservation:
# the unit the pool's quota is written in, whatever the InstanceType's own numbers say.
CHARGE="$(workload_charge "$NS" \
  "$(workload_verdicts "$SUBJECT" | sed -n 's/=True=.*$//p' | head -n 1)")"

release_deployment "$SUBJECT"

# ---------------------------------------------------------------- bound and measure the pool

PROBE_N=0
IFS='|' read -r BOUND FLAVORS BOUND_DETAIL <<<"$(pool_replica_bound "$CQ" "$CHARGE")"
BOUND="${BOUND:-0}"
if [ "$BOUND" -le 0 ]; then
  record SKIP "the pool's capacity could be measured" \
    "could not bound a probe from cluster queue '${CQ}': ${BOUND_DETAIL:-the queue could not be read}; one replica is charged ${CHARGE:-nothing readable}"
elif [ $((BOUND + 1)) -gt "$MAX_REPLICAS" ]; then
  record SKIP "the pool's capacity could be measured" \
    "cluster queue '${CQ}' could hold ${BOUND} replicas (${BOUND_DETAIL}), so a probe past it exceeds E2E_C118_MAX_REPLICAS=${MAX_REPLICAS}"
else
  PROBE_N=$((BOUND + 1))
fi

# Borrowing is the one thing that can still break the experiment: a queue in a cohort draws on
# another queue's unused quota. Both cohort spellings are read because the bundled CRD serves
# v1beta1 and v1beta2, and the field moved between them (case-50's note).
if [ "$PROBE_N" -ge 1 ]; then
  PRE_RAW="$(kubectl get clusterqueue "$CQ" \
    -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}' 2>/dev/null)"
  PRE_VER="${PRE_RAW%%|*}"
  PRE_COHORT="${PRE_RAW#*|}"

  if [ -z "$PRE_RAW" ]; then
    record SKIP "the shortage cannot be relieved by borrowing" "could not read cluster queue '${CQ}'"
    PROBE_N=0
  elif [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; then
    record SKIP "the shortage cannot be relieved by borrowing" \
      "cluster queue '${CQ}' is served as '${PRE_VER}', whose cohort field this case does not know how to read"
    PROBE_N=0
  elif [ -n "$PRE_COHORT" ]; then
    record SKIP "the shortage cannot be relieved by borrowing" \
      "cluster queue '${CQ}' is in cohort '${PRE_COHORT}', so occupying it does not make the pool short"
    PROBE_N=0
  else
    record PASS "the shortage cannot be relieved by borrowing" \
      "cluster queue '${CQ}' (${PRE_VER}) is in no cohort; its ${FLAVORS} flavor(s) are measured together below"
  fi
fi

# THE CAPACITY IS KUEUE'S ANSWER, NOT THIS SCRIPT'S ARITHMETIC: the bound ignores what other
# workloads hold and the node room topology-aware scheduling checks before reserving.
FILL=0
if [ "$PROBE_N" -ge 1 ]; then
  apply_plain "$PROBE" "$PROBE_N"
  PROBED="$(settle_verdicts "$PROBE" "$PROBE_N")"
  PROBE_RC=$?
  CAP="${PROBED%% *}"
  PROBE_WAIT="${PROBED##* }"

  if [ "$PROBE_RC" -ne 0 ]; then
    record FAIL "the pool's capacity could be measured" \
      "Kueue did not settle a verdict on all ${PROBE_N} probe replicas: ${CAP} reserved, ${PROBE_WAIT} waiting. apply said: ${APPLY_OUT:0:200}"
  elif [ "$PROBE_WAIT" -lt 1 ]; then
    record SKIP "the pool's capacity could be measured" \
      "all ${PROBE_N} probe replicas reserved quota past a bound of ${BOUND_DETAIL}, so the capacity was not reached and one replica of room cannot be left"
  elif [ "$CAP" -lt 3 ]; then
    record SKIP "the pool's capacity could be measured" \
      "the pool reserves ${CAP} replica(s), so no filler can leave it short enough to fit two of the three groups and starve the third"
  else
    FILL=$((CAP - 1))
    record PASS "the pool's capacity could be measured" \
      "Kueue reserved ${CAP} of ${PROBE_N} probe replicas and left ${PROBE_WAIT} waiting; the quota bound was ${BOUND_DETAIL}"
  fi

  release_deployment "$PROBE"
fi

if [ "$FILL" -ge 1 ]; then
  apply_plain "$FILLER" "$FILL"
  FILLED="$(wait_admitted "$FILLER" "$FILL")"
  if [ "$(echo "$FILLED" | wc -w | tr -d ' ')" -lt "$FILL" ]; then
    record FAIL "the filler occupies the pool" \
      "$(echo "$FILLED" | wc -w | tr -d ' ') of ${FILL} filler replicas reserved quota where the probe reserved ${CAP}, so the pool is not one replica short and nothing below is under test. apply said: ${APPLY_OUT:0:200}"
    FILL=0
  else
    record PASS "the filler occupies the pool" \
      "${FILL} replica(s) hold all but one replica of the ${CAP} the pool reserves"
  fi
fi

# ---------------------------------------------------------------- the headline: all three wait together

if [ "$FILL" -ge 1 ]; then
  apply_subject

  # The width is read off the subject itself, not trusted from the probe: with one replica of room,
  # exactly two of the three one-replica groups hold and one waits. Neither role holding means the
  # room closed before the subject arrived; all three holding means the pool was not short. Both
  # stop the rows below rather than passing them vacuously.
  SUBJ="$(settle_verdicts "$SUBJECT" 3)"
  SUBJ_RC=$?
  VERDICTS="$(workload_verdicts "$SUBJECT")"
  HOLDING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=True=//p' | sort -u | tr '\n' ' ')"
  WAITING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=False=//p' | sort -u | tr '\n' ' ')"
  WIDE=no
  if [ "$SUBJ_RC" -ne 0 ]; then
    record FAIL "the short pool reserves quota for exactly two of the three roles" \
      "Kueue did not settle a verdict on all three of the subject's replicas: '${VERDICTS}'. apply said: ${APPLY_OUT:0:200}"
  elif [ "${SUBJ%% *}" = 2 ]; then
    WIDE=yes
    record PASS "the short pool reserves quota for exactly two of the three roles" \
      "${HOLDING}hold quota and ${WAITING}waits, beside ${FILL} filler replica(s) of the ${CAP} the pool reserves"
  elif [ "${SUBJ%% *}" = 0 ]; then
    record SKIP "the short pool reserves quota for exactly two of the three roles" \
      "no role reserved quota, so the pool had no room left and the rows below would pass with the barrier doing nothing"
  else
    record SKIP "the short pool reserves quota for exactly two of the three roles" \
      "${SUBJ%% *} of three roles reserved quota, so the pool was not short the width this run built"
  fi
fi

if [ "${WIDE:-no}" = yes ]; then
  # THE ROW THIS FILE EXISTS FOR, counted over the replicas of EACH role so a reading of "some
  # gated" cannot pass for "all gated": the two roles that already hold quota are gated exactly
  # like the one that waits, because nothing in Kueue's own atomicity holds them once their
  # reservations are placed — the joint check this operator references from the queue is what does.
  UNGATED=""
  SEEN=0
  for role in encode prefill decode; do
    gates="$(kubectl -n "$NS" get pods \
      -l "app.kubernetes.io/instance=${SUBJECT},app.kubernetes.io/component=${role}" \
      -o jsonpath="{range .items[*]}{.metadata.name}={.spec.schedulingGates[?(@.name=='${GATE}')].name}{\"\n\"}{end}" \
      2>/dev/null)"
    seen="$(printf '%s' "$gates" | grep -c . || true)"
    SEEN=$((SEEN + seen))
    n="$(printf '%s' "$gates" | grep -c -v "=${GATE}$" || true)"
    [ "${n:-0}" -gt 0 ] && UNGATED="${UNGATED}${role}:${n} "
  done

  if [ "$SEEN" -lt 3 ]; then
    record FAIL "the roles that would have fit are gated too" \
      "only ${SEEN} replica(s) exist to read a gate from, so the check had nothing to discriminate"
  elif [ -z "$UNGATED" ]; then
    record PASS "the roles that would have fit are gated too" \
      "all ${SEEN} replicas across all three roles still carry ${GATE}, the two holding quota included"
  else
    record FAIL "the roles that would have fit are gated too" \
      "ungated replicas by role: ${UNGATED}- this is the per-role admission the group exists to prevent"
  fi

  # What holds the groups that fit, read off one of their own Workloads: the joint check, its
  # Pending state, and the role it is waiting for — the answer an operator reading the Workload gets.
  HOLD_WL="$(printf '%s\n' "$VERDICTS" | sed -n 's/=True=.*$//p' | head -n 1)"
  WAIT_ROLE="$(printf '%s' "${WAITING% }" | tr ' ' '\n' | sed -n 1p)"
  JC=""
  for _ in $(seq 1 10); do
    JC="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$HOLD_WL" \
      -o jsonpath='{range .status.admissionChecks[?(@.name=="'"$JOINT"'")]}{.state}|{.message}{end}' \
      2>/dev/null)"
    case "$JC" in Pending\|*"roles ${WAIT_ROLE}"*) break ;; esac
    sleep 3
  done
  case "$JC" in
    Pending\|*"roles ${WAIT_ROLE}"*)
      record PASS "the joint check holds the groups that fit, naming the waiting role" \
        "${HOLD_WL}: Pending, waiting for ${WAIT_ROLE}"
      ;;
    "")
      record FAIL "the joint check holds the groups that fit, naming the waiting role" \
        "${HOLD_WL} carries no ${JOINT} check, so its queue does not reference it"
      ;;
    *)
      record FAIL "the joint check holds the groups that fit, naming the waiting role" \
        "${HOLD_WL}: expected Pending naming ${WAIT_ROLE}, got: ${JC}"
      ;;
  esac

  # The operator's own account of the shortage, which names the WAITING role and neither holding
  # one: a message naming a holding role would send an operator to free quota for a replica that
  # already has it.
  QR=""
  QR_ROLES=""
  OK=no
  for _ in $(seq 1 10); do
    QR="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$SUBJECT" \
      -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null)"
    QR_MSG="${QR#*|*|}"
    QR_ROLES="$(printf '%s' "$QR_MSG" | sed -n 's/.*the replicas of roles \([^.]*\)\..*/\1/p')"
    QR_WORDS=" $(printf '%s' "$QR_ROLES" | tr ',' ' ' | tr -s ' ') "
    OK=yes
    case "$QR_WORDS" in *" ${WAIT_ROLE} "*) ;; *) OK=no ;; esac
    for h in $HOLDING; do
      case "$QR_WORDS" in *" ${h% } "*) OK=no ;; esac
    done
    case "$QR" in False\|Pending\|*) [ "$OK" = yes ] && break ;; esac
    sleep 3
  done
  case "$QR" in
    False\|Pending\|*)
      if [ "$OK" = yes ]; then
        record PASS "the deployment reports the wait, naming the waiting role only" \
          "QuotaReserved=False reason=Pending naming roles ${QR_ROLES}, not the holding roles [${HOLDING}]"
      else
        record FAIL "the deployment reports the wait, naming the waiting role only" \
          "QuotaReserved=False reason=Pending but the waiting roles read '${QR_ROLES}' where ${WAIT_ROLE} waits and [${HOLDING}] hold: ${QR_MSG}"
      fi
      ;;
    "")
      record FAIL "the deployment reports the wait, naming the waiting role only" \
        "no QuotaReserved condition was written at all while the group sat unadmitted"
      ;;
    *)
      record FAIL "the deployment reports the wait, naming the waiting role only" \
        "expected False/Pending while the pool is short, got: ${QR}"
      ;;
  esac
fi

if [ "$FILL" -ge 1 ]; then

  # --- releasing the pool admits the WHOLE group ---

  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$FILLER" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  force_release "$FILLER"

  AFTER="$(wait_admitted "$SUBJECT" 3)"
  if [ "$(echo "$AFTER" | wc -w | tr -d ' ')" = 3 ]; then
    record PASS "releasing the pool admits all three roles at once" \
      "podSetAssignments: ${AFTER}"
  else
    record FAIL "releasing the pool admits all three roles at once" \
      "assignments: '${AFTER}' - the group was starved by something other than the shortage"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-118] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-118] all checks passed"
