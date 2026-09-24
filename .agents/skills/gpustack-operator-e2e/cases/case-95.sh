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
#              when the Pods move during the window, or when Kueue rewrites the marker or reserves the
#              chosen replica before the window ends: each of those leaves the reading unattributable. The pool may have any
#              number of flavors; the case SKIPS, printing the bound, when its quota could hold more
#              than E2E_C95_MAX_REPLICAS (default 128) replicas.
#
# Inputs:      All real, nothing mocked. One single-role ModelDeployment sized to one replica more
#              than the quota of every flavor of the pool's queue summed could hold, so at least one
#              replica's Workload waits; one replica's cost is what Kueue charged its Workload, as
#              _quota-lib.sh explains. Each replica runs a placeholder image (E2E_MD_IMAGE) because
#              a CPU-only InstanceType has observed no accelerator; nothing here reads what it runs.
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
MAX_REPLICAS="${E2E_C95_MAX_REPLICAS:-128}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_quota-lib.sh"

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

# A Workload's QuotaReserved condition as "status|message". The status is read beside the message
# because a replica judged short can still be reserved later -- Kueue works through a long queue a
# head at a time, and on a pool of several flavors a later pass finds room on another one -- and the
# status patch below replaces the message of whatever condition is there. A marker read back from a
# reserved Workload is one the deployment is right not to quote.
wl_reading() {
  kubectl -n "$NS" get workloads.kueue.x-k8s.io "$1" \
    -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.message}{end}' 2>/dev/null
}

pods_rv() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}:{.metadata.resourceVersion} {end}' 2>/dev/null
}

# --- size the deployment one replica past every flavor's quota ---

# One replica's cost is read off its own Workload once Kueue has reserved for it, which is the unit
# the pool's quota is written in. A bound from the first flavor alone lets the extra replicas land
# on a flavor nobody counted, and then nothing waits.
apply_md 1
CHARGE=""
for _ in $(seq 1 30); do
  ONE="$(deployment_workloads | sed -n 's/=True$//p' | head -n 1)"
  [ -n "$ONE" ] && CHARGE="$(workload_charge "$NS" "$ONE")"
  [ -n "$CHARGE" ] && [ "$CHARGE" != "{}" ] && break
  sleep 3
done
IFS='|' read -r BOUND _ BOUND_DETAIL <<<"$(pool_replica_bound "$CQ" "$CHARGE")"
BOUND="${BOUND:-0}"

WL=""
if [ "$BOUND" -le 0 ]; then
  record SKIP "a replica of the deployment waits for quota" \
    "could not bound the pool from cluster queue '${CQ}': ${BOUND_DETAIL:-the queue could not be read}; one replica is charged ${CHARGE:-nothing readable}. apply said: ${APPLY_OUT:0:200}"
elif [ $((BOUND + 1)) -gt "$MAX_REPLICAS" ]; then
  record SKIP "a replica of the deployment waits for quota" \
    "cluster queue '${CQ}' could hold ${BOUND} replicas (${BOUND_DETAIL}), so one past it exceeds E2E_C95_MAX_REPLICAS=${MAX_REPLICAS}"
else
  REPLICAS=$((BOUND + 1))
  apply_md "$REPLICAS"

  # WAITING AND QUOTED, both: a Workload that has not been judged yet carries no message, and one the
  # deployment has not quoted yet would let the first pass after the marker pass for the watch.
  #
  # AND STILL WAITING ON THE NEXT READING, with the reservation count unchanged. A replica judged
  # short while Kueue is still reserving for the others can be reserved a moment later, and a marker
  # written onto it measures nothing: the deployment rightly stops quoting it.
  PREV=""
  for _ in $(seq 1 60); do
    VERDICTS_NOW="$(deployment_workloads)"
    PICK="$(printf '%s\n' "$VERDICTS_NOW" | sed -n 's/=False$//p' | head -n 1)"
    WL=""
    if [ -n "$PICK" ]; then
      case "$(md_quota)" in *"\"${PICK}\""*) WL="$PICK" ;; esac
    fi
    NOW="${WL}|$(printf '%s\n' "$VERDICTS_NOW" | grep -c '=True$' || true)"
    [ -n "$WL" ] && [ "$NOW" = "$PREV" ] && break
    PREV="$NOW"
    WL=""
    sleep 3
  done
  if [ -z "$WL" ]; then
    record SKIP "a replica of the deployment waits for quota" \
      "no Workload of ${REPLICAS} replicas past a bound of ${BOUND_DETAIL} waited with its message quoted; md reads: $(md_quota | cut -c1-200)"
  else
    record PASS "a replica of the deployment waits for quota" \
      "${REPLICAS} replicas past a bound of ${BOUND_DETAIL}; ${WL} is waiting and quoted"
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

  # The quiet wait above is long enough for Kueue to reserve a replica it judged short, so the pick is
  # checked again right before the marker is written onto it.
  case "$(wl_reading "$WL")" in False\|*) ;; *) RESERVED_BEFORE=yes ;; esac

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
    case "$(wl_reading "$WL")" in False\|*"$MARK"*) ;; *) KEPT=no ;; esac
    if [ -z "$SEEN" ]; then
      case "$(md_quota)" in *"$MARK"*) SEEN=$(($(date +%s) - START)) ;; esac
    fi
    sleep 2
  done
  RV2="$(pods_rv)"

  if [ "${RESERVED_BEFORE:-no}" = yes ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "Kueue reserved quota for ${WL} while the Pods settled, so no waiting replica was left to write the marker on"
  elif [ -z "$IDX" ] || ! printf '%s' "$PATCH_OUT" | grep -q patched; then
    record FAIL "the Workload carries the case's marker" \
      "the status patch did not land (condition index '${IDX}'): ${PATCH_OUT:0:200}"
  elif [ "$KEPT" = no ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "Kueue rewrote ${WL}'s message or reserved quota for it inside the window, so a waiting marker was not there to be read throughout"
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

  # ONLY WHILE THE MARKER STILL SITS ON A WAITING REPLICA. The deployment quotes waiting Workloads
  # alone, so once Kueue has reserved the chosen one or rewritten its message there is nothing the
  # status should quote, and a miss here would blame the status for the race the headline already
  # skipped on.
  POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o name 2>/dev/null | head -n 1)"
  STILL=no
  case "$(wl_reading "$WL")" in False\|*"$MARK"*) STILL=yes ;; esac
  [ "$STILL" = yes ] && kubectl -n "$NS" annotate "$POD" case95-wake="$MARK" --overwrite >/dev/null 2>&1
  CTRL=no
  for _ in $(seq 1 10); do
    [ "$STILL" = yes ] || break
    case "$(md_quota)" in *"$MARK"*) CTRL=yes; break ;; esac
    sleep 1
  done
  if [ "$STILL" = no ]; then
    record SKIP "after a Pod event the deployment quotes the marker" \
      "${WL} no longer waits with ${MARK}, so there is nothing the status should quote"
  elif [ "$CTRL" = yes ]; then
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
