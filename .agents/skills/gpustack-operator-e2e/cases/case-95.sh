#!/usr/bin/env bash
#
# CASE 95 — A change on a waiting replica's Workload alone reaches the deployment's status, with no
#   Pod event to carry it   (MUTATING, self-recovering)
#
#   case-95.sh <NS>
#
# Goal:        The deployment controller watches its replicas' Kueue Workloads, so a verdict Kueue
#              writes on a Workload wakes the deployment even while nothing touches its Pods. Kueue
#              does not touch a waiting replica's Pods while it decides -- the Pods sit gated from
#              creation until admission -- so without that watch the deployment's QuotaReserved
#              condition goes on quoting a Workload message that has since changed, until some
#              unrelated Pod event happens to wake it.
#
#              THE CHANGE IS WRITTEN BY THE CASE, NOT WAITED FOR. Kueue rewording a pending message
#              is not reliable enough to measure: under topology-aware scheduling a retry can write
#              the same text again. So the case writes a marker of its own into the waiting
#              Workload's QuotaReserved message through the status subresource, touches no Pod, and
#              reads the deployment's condition for a bounded window. The deployment quotes every
#              waiting Workload's message, so the marker reaching it is the watch's doing.
#
#              THE CONTROL IS WHAT MAKES A MISS A VERDICT ABOUT THE WATCH. After the window one Pod
#              is annotated, which wakes the deployment through the Pod watch; the marker then has to
#              appear. If it does not, the status never quotes the message at all, and a miss in the
#              window says nothing about what woke whom.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and a Kueue whose
#              pod integration is enabled. The namespace must carry the pool's entrance LocalQueue --
#              `kubectl -n <NS> get localqueue`. No GPU; nothing here serves. EXITS 2 (input
#              required) when the cluster has no usable InstanceType or the namespace reaches no
#              ClusterQueue. SKIPS the headline, printing why, when no replica can be made to wait,
#              when the Pods move during the window, or when Kueue rewrites the marker before the
#              window ends: each of those leaves the reading unattributable.
#
# Inputs:      All real, nothing mocked. One single-role ModelDeployment sized from the pool's own
#              numbers to one replica more than its CPU quota holds, so at least one replica's
#              Workload waits. Each replica runs a placeholder image (E2E_MD_IMAGE) because a
#              CPU-only InstanceType has observed no accelerator; nothing here reads what it runs.
#              Override the InstanceType with E2E_MD_INSTANCE_TYPE and the window (seconds) with
#              E2E_C95_WINDOW.
#
# Expected:    A waiting replica's Workload keeps the case's marker for the whole window, no Pod of
#              the deployment changes resourceVersion in it, and the deployment's QuotaReserved
#              condition quotes the marker within the window. After a Pod annotation the condition
#              quotes the marker (the control).
#
# Cleanup:     A trap deletes the deployment and releases any Workload still holding its replicas.
#              Idempotent, runs on pass AND fail, safe to re-run. It changes no ClusterQueue and no
#              baseline; the marker lives on a Workload the trap deletes.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-95.sh <NS>" >&2
  exit 2
fi

MD=case95-subject
BINDING="${E2E_MD_BINDING:-case95-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
WINDOW="${E2E_C95_WINDOW:-30}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

# The first InstanceType a deployment can name: not terminating and not inactive, for the reasons
# case-50 gives.
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
  echo "[case-95] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

CQ="$(kubectl -n "$NS" get localqueue \
  "$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
    -o jsonpath='{.status.entrance}' 2>/dev/null)" \
  -o jsonpath='{.spec.clusterQueue}' 2>/dev/null)"
if [ -z "$CQ" ]; then
  echo "[case-95] InstanceType ${IT} names no reachable ClusterQueue in ${NS}; run case-1 first, and" >&2
  echo "          check that ${NS} carries the pool's entrance LocalQueue" >&2
  exit 2
fi

# Millicores from a plain decimal CPU quantity with an optional `m`, or 0 for anything else; the
# shape is matched first because awk takes a numeric prefix of a value it does not understand.
millis() {
  case "$1" in
    *[!0-9.m]* | "" | m) echo 0 ;;
    *m) printf '%s' "${1%m}" | awk '{printf "%d", $0+0}' ;;
    *) printf '%s' "$1" | awk '{printf "%d", ($0+0)*1000}' ;;
  esac
}

pod_uids() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null
}

# Every Workload owning one of the deployment's Pods, as "name=quotaReservedStatus" lines. One list
# call, matched locally, because this runs in poll loops and from the trap.
deployment_workloads() {
  local uids row wl rest status owners u
  uids="$(pod_uids)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    rest="${row#*=}"
    status="${rest%%=*}"
    owners="${rest#*=}"
    for u in $uids; do
      # Padded on both sides so a uid cannot match a longer one it is a prefix of.
      case " $owners " in *" $u "*) echo "${wl}=${status}"; break ;; esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="QuotaReserved")].status}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

cleanup() {
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  local row
  for row in $(deployment_workloads); do
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "${row%%=*}" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done

  return 0
}
trap cleanup EXIT

APPLY_OUT=""
apply_md() {
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
      name: ${BINDING}
  roles:
  - name: server
    instanceType: ${IT}
    replicas: $1
    image: ${IMAGE}
    command: ["/pause"]
YAML
)"
}

md_quota() {
  kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" \
    -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null
}

wl_message() {
  kubectl -n "$NS" get workloads.kueue.x-k8s.io "$1" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].message}' 2>/dev/null
}

pods_rv() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}:{.metadata.resourceVersion} {end}' 2>/dev/null
}

# --- size the deployment one replica past the pool's CPU quota ---

apply_md 1
REQ_RAW=""
for _ in $(seq 1 30); do
  REQ_RAW="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}' 2>/dev/null)"
  [ -n "$REQ_RAW" ] && break
  sleep 2
done
REQ="$(millis "$REQ_RAW")"
QUOTA_RAW="$(kubectl get clusterqueue "$CQ" \
  -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="cpu")].nominalQuota}' 2>/dev/null)"
QUOTA="$(millis "$QUOTA_RAW")"

WL=""
if [ "$REQ" -le 0 ] || [ "$QUOTA" -le 0 ]; then
  record SKIP "a replica of the deployment waits for quota" \
    "could not read the numbers: request='${REQ_RAW}' quota='${QUOTA_RAW}'. apply said: ${APPLY_OUT:0:200}"
else
  REPLICAS=$((QUOTA / REQ + 1))
  apply_md "$REPLICAS"

  # WAITING AND QUOTED, both: a Workload that has not been judged yet carries no message, and one the
  # deployment has not quoted yet would let the first pass after the marker pass for the watch.
  for _ in $(seq 1 60); do
    WL="$(deployment_workloads | sed -n 's/=False$//p' | head -n 1)"
    [ -n "$WL" ] && case "$(md_quota)" in *"\"${WL}\""*) break ;; esac
    WL=""
    sleep 3
  done
  if [ -z "$WL" ]; then
    record SKIP "a replica of the deployment waits for quota" \
      "no Workload of ${REPLICAS} replicas of ${REQ}m against ${QUOTA}m waited with its message quoted; md reads: $(md_quota | cut -c1-200)"
  else
    record PASS "a replica of the deployment waits for quota" \
      "${REPLICAS} replicas of ${REQ}m against ${QUOTA}m; ${WL} is waiting and quoted"
  fi
fi

# --- the headline: a Workload-only change reaches the deployment ---

if [ -n "$WL" ]; then
  # QUIET FIRST. Admitted replicas start and their Pods move for a while; a Pod event inside the
  # window would wake the deployment through the Pod watch and pass for the Workload watch.
  RV0="$(pods_rv)"
  for _ in $(seq 1 24); do
    sleep 5
    RV1="$(pods_rv)"
    [ "$RV0" = "$RV1" ] && break
    RV0="$RV1"
  done

  MARK="case95-marker-$(date +%s)"
  IDX="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$WL" \
    -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}' 2>/dev/null \
    | awk '$0 == "QuotaReserved" {print NR - 1; exit}')"
  PATCH_OUT="$(kubectl -n "$NS" patch workloads.kueue.x-k8s.io "$WL" --subresource=status --type=json \
    -p "[{\"op\":\"test\",\"path\":\"/status/conditions/${IDX}/type\",\"value\":\"QuotaReserved\"},{\"op\":\"replace\",\"path\":\"/status/conditions/${IDX}/message\",\"value\":\"${MARK}\"}]" 2>&1)"

  SEEN=""
  KEPT=yes
  START=$(date +%s)
  while [ $(($(date +%s) - START)) -lt "$WINDOW" ]; do
    case "$(wl_message "$WL")" in *"$MARK"*) ;; *) KEPT=no ;; esac
    if [ -z "$SEEN" ]; then
      case "$(md_quota)" in *"$MARK"*) SEEN=$(($(date +%s) - START)) ;; esac
    fi
    sleep 2
  done
  RV2="$(pods_rv)"

  if [ -z "$IDX" ] || ! printf '%s' "$PATCH_OUT" | grep -q patched; then
    record FAIL "the Workload carries the case's marker" \
      "the status patch did not land (condition index '${IDX}'): ${PATCH_OUT:0:200}"
  elif [ "$KEPT" = no ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "Kueue rewrote ${WL}'s message inside the window, so the marker was not there to be read throughout"
  elif [ "$RV0" != "$RV2" ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "a Pod of the deployment moved inside the window, so a wake cannot be told from the Pod watch's"
  elif [ -n "$SEEN" ]; then
    record PASS "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "QuotaReserved quoted ${MARK} after ${SEEN}s; ${WL} kept it and no Pod moved"
  else
    record FAIL "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "QuotaReserved never quoted ${MARK} while ${WL} carried it and no Pod moved: nothing woke the deployment on the Workload"
  fi

  # --- the control: a Pod event wakes it, and the status then quotes the marker ---

  POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o name 2>/dev/null | head -n 1)"
  kubectl -n "$NS" annotate "$POD" case95-wake="$MARK" --overwrite >/dev/null 2>&1
  CTRL=no
  for _ in $(seq 1 10); do
    case "$(md_quota)" in *"$MARK"*) CTRL=yes; break ;; esac
    sleep 1
  done
  if [ "$CTRL" = yes ]; then
    record PASS "after a Pod event the deployment quotes the marker" \
      "annotating ${POD#pod/} woke it and QuotaReserved quotes ${MARK}"
  else
    record FAIL "after a Pod event the deployment quotes the marker" \
      "the status does not quote the Workload's message at all, so the headline row measured nothing: $(md_quota | cut -c1-200)"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-95] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-95] all checks passed"
