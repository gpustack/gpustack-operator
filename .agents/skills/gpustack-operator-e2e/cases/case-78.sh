#!/usr/bin/env bash
#
# CASE 78 — Scaling a role moves its queue's admitted quota by exactly one replica in each
#           direction, and touches no replica it did not add or remove   (MUTATING, self-cleaning)
#
#   case-78.sh <NS>
#
# Goal:        THE QUOTA ARITHMETIC CANNOT BE OBSERVED ANYWHERE ELSE. A fake client has no
#              ClusterQueue, so every unit test of a scale can assert which Pods exist and none of
#              them can assert what the cluster was charged for them. This case is the only place
#              the two halves are read together: the replicas that stayed, and the quota that moved.
#
#              WHAT A SCALE USED TO COST. While a role's replicas shared one Kueue group, the
#              declared total was a number every member carried, so changing it tore the group down
#              and recomposed it -- every replica of that role was deleted and remade, losing its
#              accelerators, its loaded weights and its cache to add or remove one sibling. Each
#              replica is admitted as its own Workload now, and this case is the proof on a live
#              cluster that a scale is a trim rather than a rebuild.
#
#              THE UID IS THE ONLY OBSERVABLE THAT ANSWERS IT. A replica that stayed and one
#              replaced by an identical render agree on everything else -- same name pattern, same
#              spec, same labels, same readiness. They differ in the identity the cluster assigned,
#              which a fake client leaves empty and which is therefore unassertable below e2e.
#
#              THE DOWN LEG IS NOT THE UP LEG IN REVERSE. Scaling up can only add, so a broken
#              implementation that rebuilt everything would still end with the right count; the
#              shed direction is where a rebuild shows as the wrong replicas surviving. Both legs
#              also read the queue, because a replica whose Workload was deleted but whose quota
#              was never returned looks exactly like a correct scale from the Pod list alone.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose
#              pod integration is enabled, and an operator image carrying the per-replica admission
#              unit. The namespace must carry the pool's entrance LocalQueue. NO GPU is needed: the
#              quota this case reads is whatever its queue accounts in, and on a CPU-only pool that
#              is cpu. EXITS 2 (input required) when the cluster has no InstanceType.
#
#              THE POOL MUST HAVE ROOM FOR THREE REPLICAS. A scale-up that stays Pending for want
#              of capacity is not a failure of this feature, and the row that would report it says
#              so rather than blaming the scale.
#
# Inputs:      All real, nothing mocked. The KVCachePoolBinding is not resolved by anything asserted
#              here, so it may be absent. The role carries an explicit image because a CPU-only
#              InstanceType has observed no accelerator and the operator can synthesize none; the
#              replicas are placeholders and no assertion reads what they run. Override with
#              E2E_MD_IMAGE, the InstanceType with E2E_MD_INSTANCE_TYPE.
#
# Expected:    Two replicas admit; scaling to three keeps both original UIDs, adds one Workload and
#              raises the queue's admitted count and its usage by one replica's worth; scaling back
#              to two keeps the two oldest UIDs, removes the third's Workload, and returns the
#              queue to exactly its starting numbers.
#
# Cleanup:     A trap deletes the deployment and, if any replica is wedged past the bound, every
#              Workload still owning one -- the manual form of the release the operator performs.
#              Idempotent, runs on pass AND fail, creates no cluster-scoped object.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-78.sh <NS>" >&2
  exit 2
fi

MD=case78-scale
BINDING="${E2E_MD_BINDING:-case78-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE="${E2E_MD_SETTLE:-60}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

k() { kubectl "$@"; }

if [ -z "$IT" ]; then
  IT="$(k get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-78] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# Every Workload owning one of this deployment's Pods, by name, sorted. Matched on the ownerReference
# UID: Kueue names a Workload after the group and a group name is a hash, so nothing in the name can
# be matched on.
deployment_workloads() {
  local uids
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -n "$uids" ] || return 0
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}|{range .metadata.ownerReferences[*]}{.uid}{" "}{end}{"\n"}{end}' \
    2>/dev/null | while IFS='|' read -r name owners; do
      [ -n "$name" ] || continue
      for u in $uids; do
        case " $owners " in *" $u "*) echo "$name"; break ;; esac
      done
    done | sort -u
}

wl_count() { deployment_workloads | grep -c . || true; }

# Pod name=UID, sorted. See the header for why the UID is the observable this case rests on.
replica_uids() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort | tr '\n' ' '
}

# How many of a recorded UID set are still present.
surviving() {
  local before="$1" now n=0 e
  now="$(replica_uids)"
  for e in $before; do
    case " $now " in *" $e "*) n=$((n + 1)) ;; esac
  done
  echo "$n"
}

how_many_admitted() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true
}

# THE QUEUE IS FOUND THROUGH AN ADMITTED WORKLOAD, not derived from the InstanceType's name. The
# derivation exists in the operator and repeating it here would make this case pass whenever both
# copies are wrong in the same way.
#
# IT WAITS FOR THE ADMISSION, because the settle this runs after does not. That settle returns as
# soon as the Pods and their Workloads EXIST, and a Workload Kueue has composed but not yet admitted
# carries no .status.admission at all -- so a single read lands on an empty string.
#
# MEASURED: the whole case once ran in six seconds and every queue reading below said "no-queue".
# The damage is not one wrong row. An empty queue name makes wait_queue_caught_up return without
# waiting, turns the up leg's comparison into a FAIL that blames the queue for having no count, and
# leaves the down leg comparing "no-queue" against "no-queue" -- which PASSES. A lost race thus
# reports as one product failure and one product success, neither of which happened.
#
# Timing out still yields the empty string rather than a guess: the rows below are written to report
# what they found, and a queue that never admits is a finding rather than something to wait out.
queue_of() {
  local wl cq i
  for i in $(seq 1 30); do
    wl="$(deployment_workloads | head -1)"
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

# The queue's admitted count and its total usage of every resource it covers, as one string. Read
# together because they move together: a Workload admitted without its request being charged, or a
# charge left behind by a Workload that went, are both states the count alone reads as correct.
queue_reading() {
  local cq="$1"
  [ -n "$cq" ] || { echo "no-queue"; return 0; }
  printf 'admitted=%s usage=%s' \
    "$(k get clusterqueue "$cq" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)" \
    "$(k get clusterqueue "$cq" \
      -o jsonpath='{range .status.flavorsUsage[*]}{range .resources[*]}{.name}:{.total}{" "}{end}{end}' 2>/dev/null)"
}

# The admitted count out of a reading, or empty when it carries none. Split out because the count is
# the one component of that string an equation can be written against: the usage half names whatever
# resources the pool accounts in, which differ per pool, so it is reported rather than predicted.
admitted_count() {
  local r="${1#admitted=}"
  r="${r%% *}"
  case "$r" in
    '' | *[!0-9]*) return 0 ;;
  esac
  printf '%s' "$r"
}

# THE QUEUE'S STATUS LAGS THE WORKLOADS IT ACCOUNTS FOR, and every reading below has to wait for it.
# A Workload reads Admitted the moment Kueue admits it; the ClusterQueue republishes its counters on
# a pass of its own. Reading the two in one breath samples a queue that has not caught up.
#
# MEASURED, ON THE FIRST RUN OF THIS CASE: the baseline read `admitted=0` while both replicas were
# already admitted and holding their Workloads. Every later comparison was then against a number
# from before the case began -- the up leg read "0 -> 0" and reported that a scale which had
# actually happened had not been charged for.
#
# IT WAITS FOR "AT LEAST OUR OWN", NOT FOR AN EXACT NUMBER. The queue is shared, so a neighbour
# admitting into it inflates the count; an equality check would then run out the clock on a reading
# that was correct. This is the same limit the up-leg row states: the count belongs to the queue,
# not to this deployment.
wait_queue_caught_up() {
  local cq="$1" mine i seen
  mine="$(how_many_admitted)"
  [ -n "$cq" ] && [ "${mine:-0}" -gt 0 ] || return 0
  for i in $(seq 1 30); do
    seen="$(admitted_count "$(queue_reading "$cq")")"
    [ -n "$seen" ] && [ "$seen" -ge "$mine" ] && return 0
    sleep 2
  done

  return 1
}

# THE DOWN LEG NEEDS THE OPPOSITE WAIT, and the difference is not a detail: the queue has to RELEASE
# a reservation, and "accounts for at least our own" is ALREADY TRUE while the shed replica's quota
# is still held. Waiting on that predicate would return immediately and read the pre-release number.
#
# It waits for the reading to reach the value the row is about to compare against, then gives up and
# lets the row report what it actually found. Waiting cannot turn a wrong number into a right one --
# a queue that never returns the quota times out here and FAILS below, printing both readings, which
# is the outcome that row exists to produce.
wait_queue_returns_to() {
  local cq="$1" want="$2" i
  [ -n "$cq" ] || return 0
  for i in $(seq 1 30); do
    [ "$(queue_reading "$cq")" = "$want" ] && return 0
    sleep 2
  done

  return 1
}

cleanup() {
  local row wl uids u
  k -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          k -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(k -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF

  return 0
}
trap cleanup EXIT

# Wait until the deployment holds exactly $1 replicas AND a Workload for each. Both, because the
# Pods appear first: a poll that stopped at the Pod count would read the queue mid-admission and
# report a shortfall that resolves a second later.
wait_settled() {
  local want="$1" i
  for i in $(seq 1 "$SETTLE"); do
    if [ "$(how_many_admitted)" = "$want" ] && [ "$(wl_count)" = "$want" ]; then
      return 0
    fi
    sleep 2
  done

  return 1
}

# --- the deployment: one role, two replicas ---

APPLY_OUT="$(cat <<YAML | k apply -f - 2>&1
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
    kind: server
    instanceType: ${IT}
    replicas: 2
    image: ${IMAGE}
    command: ["/pause"]
YAML
)"

if ! wait_settled 2; then
  record FAIL "two replicas admit, one Workload each" \
    "reached $(how_many_admitted) replica(s) and $(wl_count) Workload(s); apply said: ${APPLY_OUT:0:200}"
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  echo "[case-78] ${FAILS} check(s) FAILED"
  exit 1
fi

CQ="$(queue_of)"
BASE_UIDS="$(replica_uids)"
wait_queue_caught_up "$CQ"
BASE_Q="$(queue_reading "$CQ")"
record PASS "two replicas admit, one Workload each" \
  "queue ${CQ:-<none>} reads: ${BASE_Q}"

# --- up: 2 -> 3 ---

# The patch output is KEPT rather than discarded: a refused edit and a slow one produce the same
# silence downstream, and the message saying which is only available here.
if ! UP_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1)"; then
  record FAIL "scaling up is accepted" \
    "the replicas edit was refused, so nothing below measured a scale: ${UP_PATCH:0:220}"
else
  record PASS "scaling up is accepted" "replicas 2 -> 3"
fi

if ! wait_settled 3; then
  record FAIL "the added replica gets a Workload of its own" \
    "reached $(how_many_admitted) replica(s) and $(wl_count) Workload(s) — if the pool is full, this is capacity rather than the scale"
  record SKIP "scaling up leaves both original replicas untouched" "the scale never settled"
  record SKIP "the queue is charged for exactly one more replica" "the scale never settled"
else
  record PASS "the added replica gets a Workload of its own" \
    "3 replicas, 3 Workloads: the new one was admitted on its own rather than with the others"

  kept="$(surviving "$BASE_UIDS")"
  want="$(printf '%s\n' $BASE_UIDS | grep -c . || true)"
  # THE ROW THIS CASE EXISTS FOR. Before the per-replica unit this was FALSE by construction: the
  # group's declared total moved, so every replica of the role was rebuilt to carry the new one.
  if [ "$kept" = "$want" ]; then
    record PASS "scaling up leaves both original replicas untouched" \
      "all ${want} kept their UIDs; the third was created beside them"
  else
    record FAIL "scaling up leaves both original replicas untouched" \
      "only ${kept}/${want} kept their UIDs — a scale rebuilt replicas it was asked only to add beside"
  fi

  wait_queue_caught_up "$CQ"
  UP_Q="$(queue_reading "$CQ")"
  # ONE MORE, NOT MERELY DIFFERENT. A count that moved the wrong way, or by two, is a different
  # defect from one that did not move at all, and a check on inequality alone reports all three as
  # the same pass. The down leg already states its expectation as an equation; this is the same
  # discipline on the leg where the expected value has to be computed rather than remembered.
  #
  # WHAT THIS READING CANNOT SEPARATE: the count belongs to the whole ClusterQueue, which is the
  # only place the charge is visible at all. Another deployment admitting into the same queue inside
  # this window is counted here too. Nothing in this case can tell the two apart, so an off-by-more
  # reading is worth checking against the queue's other tenants before it is read as a scale defect.
  BASE_ADM="$(admitted_count "$BASE_Q")"
  UP_ADM="$(admitted_count "$UP_Q")"
  if [ -z "$BASE_ADM" ] || [ -z "$UP_ADM" ]; then
    record FAIL "the queue is charged for exactly one more replica" \
      "no admitted count to compare: before [${BASE_Q}] after [${UP_Q}]"
  elif [ "$UP_ADM" -eq "$((BASE_ADM + 1))" ]; then
    record PASS "the queue is charged for exactly one more replica" \
      "admitted ${BASE_ADM} -> ${UP_ADM}; usage before [${BASE_Q}] after [${UP_Q}]"
  else
    record FAIL "the queue is charged for exactly one more replica" \
      "admitted ${BASE_ADM} -> ${UP_ADM}, wanted $((BASE_ADM + 1)): before [${BASE_Q}] after [${UP_Q}]"
  fi
fi

# --- down: 3 -> 2 ---

if ! DOWN_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1)"; then
  record FAIL "scaling down is accepted" "the replicas edit was refused: ${DOWN_PATCH:0:220}"
else
  record PASS "scaling down is accepted" "replicas 3 -> 2"
fi

if ! wait_settled 2; then
  record FAIL "the shed replica takes its Workload with it" \
    "settled at $(how_many_admitted) replica(s) and $(wl_count) Workload(s): a Workload left behind holds quota for a replica that is gone"
  record SKIP "the two survivors are the two that were already there" "the scale-down never settled"
  record SKIP "the queue returns to exactly its starting numbers" "the scale-down never settled"
else
  record PASS "the shed replica takes its Workload with it" \
    "2 replicas, 2 Workloads: the removed replica's Workload was deleted rather than stranded"

  # THE SHED DIRECTION IS WHERE A REBUILD SHOWS. Scaling up can only add, so an implementation that
  # rebuilt everything still ends with the right count; here it would end with two replicas that are
  # both new. Which two survive is also stated: a scale-down sheds the HIGHEST ordinals, so the two
  # the deployment started with are the two that must remain.
  kept_down="$(surviving "$BASE_UIDS")"
  want_down="$(printf '%s\n' $BASE_UIDS | grep -c . || true)"
  if [ "$kept_down" = "$want_down" ]; then
    record PASS "the two survivors are the two that were already there" \
      "both original UIDs outlived a scale up and back down; the replica shed is the one added"
  else
    record FAIL "the two survivors are the two that were already there" \
      "only ${kept_down}/${want_down} of the original UIDs survived — the scale-down shed a replica that predated it"
  fi

  wait_queue_returns_to "$CQ" "$BASE_Q"
  DOWN_Q="$(queue_reading "$CQ")"
  # EXACTLY THE STARTING NUMBERS, not merely "lower". A queue that returned some of the charge and
  # kept the rest reads as a correct scale to any check that only asserts a decrease, and the
  # leaked remainder is quota no later deployment can use and nothing reports.
  #
  # A READING WITH NO COUNT IN IT IS REFUSED FIRST, on the same grounds the up leg refuses one: two
  # readings that both say "no queue was ever found" are equal, so the comparison below would pass
  # without having compared any quota at all -- and that pass is indistinguishable from the one this
  # row exists to produce.
  if [ -z "$(admitted_count "$BASE_Q")" ] || [ -z "$(admitted_count "$DOWN_Q")" ]; then
    record FAIL "the queue returns to exactly its starting numbers" \
      "no admitted count to compare: before [${BASE_Q}] after [${DOWN_Q}]"
  elif [ "$DOWN_Q" = "$BASE_Q" ]; then
    record PASS "the queue returns to exactly its starting numbers" \
      "[${DOWN_Q}] — the shed replica's reservation was returned in full"
  else
    record FAIL "the queue returns to exactly its starting numbers" \
      "started [${BASE_Q}], ended [${DOWN_Q}] — the difference is quota held for a replica that no longer exists"
  fi
fi

# --- deferred: the serving half ---

# A LEDGER ENTRY, stating what does NOT close it. A gap recorded as only "deferred" gets closed by
# the first thing that looks like coverage.
record SKIP "the surviving replicas keep serving across both scales" \
  "needs an engine that serves, which needs an accelerator. NOT closed by: the UID rows above, which prove the Pod object survived rather than that its engine never stopped answering; nor by a readiness probe on a pause image, which answers for a container that serves nothing"

# Results.
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-78] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-78] all checks passed (the serving half is deferred; see the SKIP row)"
