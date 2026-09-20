#!/usr/bin/env bash
#
# CASE 49 — Every replica of a multi-role ModelDeployment becomes its own Kueue Workload, and
#           deleting the deployment completes   (MUTATING, self-recovering)
#
#   case-49.sh <NS>
#
# Goal:        This is the mechanism a prefill/decode deployment rests on, and it is the half that
#              CANNOT be asserted anywhere but on a live Kueue: the operator writes labels and
#              annotations, and KUEUE decides what they compose into. A unit test can prove the
#              operator stamped `pod-group-name`; only this can prove Kueue read it and composed one
#              Workload PER REPLICA, each carrying a single PodSet named after its role.
#
#              THE SECOND DEPLOYMENT IS THE ONE THAT MATTERS. Two roles that differ ONLY in name
#              render identical Pod specs, and Kueue derives a role hash from a Pod spec's SHAPE when
#              the annotation is absent -- so without it every one of those replicas names its PodSet
#              the same derived thing, and nothing can tell which Workload answers for which role:
#              per-role counting, per-role flavor assignment and per-role quota all attribute to
#              whichever role the join happens to reach first, with nothing erroring. A case that
#              only ran prefill-and-decode would pass either way, because those two differ in their
#              rendered engine arguments. So this file runs both.
#
#              THE DELETION ROW IS NOT HOUSEKEEPING. The group is annotated as serving, which Kueue
#              defines as a group that never finishes, so Kueue never releases the finalizer it holds
#              on the replicas on its own -- and the Workload that would release it is owned by those
#              same replicas, without a controller reference, so garbage collection cannot reach it
#              either. A deployment whose teardown does not break that cycle sits in Deleting forever
#              with nothing erroring anywhere, which is a state every row above still passes in.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose pod
#              integration is enabled, and an operator image carrying the multi-role ModelDeployment.
#              The namespace must carry the pool's entrance LocalQueue -- `kubectl -n <NS> get
#              localqueue` -- which is what routes the group into the ClusterQueue.
#
#              NO GPU is needed and nothing here has to serve: a Workload is composed from the Pods as
#              they are CREATED, before any container starts, so every assertion holds on a CPU-only
#              node. EXITS 2 (input required) when the cluster has no InstanceType.
#
# Inputs:      All real, nothing mocked. The KVCachePoolBinding is not resolved by anything asserted
#              here, so it may be absent. Each role carries an explicit `image` because a
#              CPU-only InstanceType has observed no accelerator and the operator can therefore
#              synthesize no engine image -- the replicas are placeholders, and no assertion reads
#              anything they run. Override with E2E_MD_IMAGE, the InstanceType with
#              E2E_MD_INSTANCE_TYPE.
#
# Expected:    Four replicas exist; FOUR Workloads own them, one each; every Workload carries a
#              single PodSet of one named after its role, two `prefill=1` and two `decode=1`;
#              deleting ONE replica's Workload out of band leaves a sibling's Workload reading
#              Admitted=True and its Pod holding the UID it already had -- the blast radius the
#              per-replica unit exists to bound, and the one operation that used to take the whole
#              role down with it; two roles differing only in name keep their own PodSet names
#              rather than collapsing onto one derived one; and deleting a deployment removes it,
#              its replicas and all four of its Workloads within the bound.
#
#              The RELEASED replica is split in two: the deployment returning to four
#              replica/Workload pairs is asserted, because converging on the declared count is the
#              controller's contract; which of Kueue's recoveries got it there is only reported,
#              because no reading here pins that timing.
#
# Cleanup:     A trap deletes both deployments and, if one is wedged past the bound, the Workload that
#              holds its replicas -- the manual form of the release the operator is asserted to do.
#              Idempotent, runs on pass AND fail, safe to re-run. It creates no cluster-scoped object
#              and changes no baseline.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-49.sh <NS>" >&2
  exit 2
fi

BINDING="${E2E_MD_BINDING:-case49-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

# How long a group gets to leave. Generous on purpose: the bound is here to turn a deadlock into a
# FAIL, not to measure how quick a delete is.
DELETE_BOUND="${E2E_MD_DELETE_BOUND:-90}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-49] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# Delete a wedged group by hand. Deleting the Workload is what releases Kueue's finalizer on the
# replicas, so this is the escape a teardown that fails the deletion row below leaves behind.
force_release() {
  # EVERY owned Workload, not the first one found. Four replicas are four Workloads, and deleting
  # one of them leaves three still holding Kueue finalizers on their Pods -- a leak that contaminates
  # every case running after this one. group_workload returns the first match and exists only to
  # wait for a Workload to appear at all; a cleanup built on it would release a quarter of the
  # deployment and report success.
  local md="$1" row wl uids u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
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

cleanup() {
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    case49-pd case49-twins --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release case49-pd
  force_release case49-twins
}
trap cleanup EXIT

# The apply's output is KEPT, in APPLY_OUT. Discarded, a schema or webhook refusal -- a very plausible
# failure for this feature -- surfaces only as a 90-second timeout whose FAIL row blames the wrong
# thing ("only N exist, so Kueue composes nothing") and never quotes the refusal that actually happened.
APPLY_OUT=""
apply_md() {
  local name="$1" roles="$2"
  APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
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
${roles}
YAML
)"
}

# One role block. The image and command are the placeholder plumbing described in the header; the
# fields that carry the verification are the name, the kind and the replica count.
role_block() {
  printf '  - name: %s\n' "$1"
  [ -n "$2" ] && printf '    kind: %s\n' "$2"
  printf '    instanceType: %s\n    replicas: %s\n' "$IT" "$3"
  printf '    image: %s\n    command: ["/pause"]\n' "$IMAGE"
}

# The Workload Kueue composed for this deployment's group, by NAME.
#
# Found through the Pods rather than by guessing the Workload's name: that name is Kueue's to choose,
# and deriving it here would mean importing its pod integration's naming. A group Workload carries a
# plain owner reference to every member Pod and NO controller reference, so the lookup matches on
# ownership rather than on control -- the same fact the operator's own status reader had to learn.
group_workload() {
  local md="$1" uids wl owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  for wl in $(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
    owners="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
      -o jsonpath='{range .metadata.ownerReferences[*]}{.uid}{"\n"}{end}' 2>/dev/null)"
    for u in $uids; do
      case "$owners" in *"$u"*) echo "$wl"; return 0 ;; esac
    done
  done
}

# Wait until the named deployment has exactly $2 Pods, which is what Kueue waits for too: it composes
# nothing for a group short of its declared total.
# Every Workload of one deployment, as "<name> <podset>=<count>,..." sorted, one line each. The
# ownerReference UID is what attaches a Workload to this deployment: Kueue names a Workload after
# the group, and a group name is a hash, so nothing about the name can be matched on.
workload_podsets() {
  local md="$1" uids
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -n "$uids" ] || return 0
  kubectl -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath="{range .items[*]}\
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
  local md="$1" want="$2" i
  for i in $(seq 1 45); do
    [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "$want" ] && return 0
    sleep 2
  done

  return 1
}

wait_workload() {
  local md="$1" i wl
  for i in $(seq 1 45); do
    wl="$(group_workload "$md")"
    [ -n "$wl" ] && { echo "$wl"; return 0; }
    sleep 2
  done

  return 1
}

# --- deployment 1: prefill and decode ---

apply_md case49-pd "$(role_block prefill prefill 2)
$(role_block decode decode 2)"

if wait_pods case49-pd 4; then
  record PASS "the group's four replicas are all created" \
    "two roles of two, in one reconcile pass"
else
  record FAIL "the group's four replicas are all created" \
    "only $(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd --no-headers 2>/dev/null | wc -l | tr -d ' ') exist, so Kueue composes nothing. apply said: ${APPLY_OUT:0:200}"
fi

WL="$(wait_workload case49-pd)"
if [ -z "$WL" ]; then
  record FAIL "Kueue composes one Workload per replica" \
    "no Workload in ${NS} owns any of this deployment's Pods"
else
  PD_WLS="$(workload_podsets case49-pd)"
  n_wl="$(printf '%s\n' "$PD_WLS" | grep -c . || true)"
  # FOUR, not "at least one" and not one: a single Workload covering all four replicas is exactly the
  # per-role shape this design replaces, and it would satisfy an "a Workload exists" check. The count
  # is over the deployment's OWN Pods rather than the namespace, which may hold a neighbour's.
  if [ "$n_wl" = 4 ]; then
    record PASS "Kueue composes one Workload per replica" \
      "4 Workloads for 4 replicas, each owning its own with no controller reference"
  else
    record FAIL "Kueue composes one Workload per replica" \
      "${n_wl} Workload(s) own this deployment's Pods, expected 4: [${PD_WLS//$'\n'/; }]"
  fi

  # EACH carries ONE PodSet OF ONE. The count is the half that matters: a Workload whose PodSet
  # declares 2 is a group that waits for a sibling that will never join it, and it would still read
  # as "named after the role" to a check that only looked at names.
  wrong="$(printf '%s\n' "$PD_WLS" | awk 'NF && $2 != "prefill=1" && $2 != "decode=1"')"
  n_prefill="$(printf '%s\n' "$PD_WLS" | grep -c ' prefill=1$' || true)"
  n_decode="$(printf '%s\n' "$PD_WLS" | grep -c ' decode=1$' || true)"
  if [ -z "$wrong" ] && [ "$n_prefill" = 2 ] && [ "$n_decode" = 2 ]; then
    record PASS "each Workload carries one PodSet of one, named for its role" \
      "2 prefill=1 and 2 decode=1 — the role-hash annotation is what names them"
  else
    record FAIL "each Workload carries one PodSet of one, named for its role" \
      "prefill=1 x${n_prefill}, decode=1 x${n_decode}, unexpected: [${wrong//$'\n'/; }]"
  fi
fi

# --- one replica's Workload, deleted out of band ---

# THE OPERATION THAT USED TO TAKE THE WHOLE ROLE WITH IT. While a role shared one Workload, that
# Workload WAS the role: deleting it evicted every replica behind it at once. Each replica owns one
# now, and the claim is that the blast radius of deleting one is one replica.
#
# ADMITTED, NOT MERELY PRESENT. A sibling left standing but evicted back to pending has lost exactly
# what the per-replica unit exists to protect, and a check that reads the Pod alone calls it a
# survivor. The UID is read for the same reason it is read elsewhere here: a replica rebuilt to the
# same name agrees with a survivor on everything a spec comparison can see.

# Each replica as "<pod-name> <pod-uid> <workload-name>". All three are needed together: the name to
# say which replica was picked, the UID to tell a survivor from a replacement, the Workload because
# that is the object deleted.
replica_rows() {
  local md="$1" pods
  pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -n "$pods" ] || return 0
  kubectl -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath="{range .items[*]}{.metadata.name}|\
{range .metadata.ownerReferences[*]}{.uid}{\" \"}{end}{\"\n\"}{end}" 2>/dev/null \
    | while IFS='|' read -r wl owners; do
        [ -n "$wl" ] || continue
        printf '%s\n' "$pods" | while IFS='=' read -r pname puid; do
          case " $owners " in *" $puid "*) echo "$pname $puid $wl" ;; esac
        done
      done | sort
}

wl_admitted() {
  kubectl -n "$NS" get workloads.kueue.x-k8s.io "$1" \
    -o jsonpath='{range .status.conditions[?(@.type=="Admitted")]}{.status}{end}' 2>/dev/null
}

# NOT NAMED ROWS: that is the results accumulator, and a scalar assignment to an array name writes
# element zero -- so this line replaced every row recorded before it with a list of pod triples.
# Only the count survived, because FAILS is tallied separately: a case failing here would have
# printed "1 check(s) FAILED" and no failing row to say which.
REPLICA_ROWS="$(replica_rows case49-pd)"
VICTIM="$(printf '%s\n' "$REPLICA_ROWS" | sed -n '1p')"
SIBLING="$(printf '%s\n' "$REPLICA_ROWS" | sed -n '2p')"
V_POD="${VICTIM%% *}"
V_WL="${VICTIM##* }"
S_POD="${SIBLING%% *}"
S_WL="${SIBLING##* }"
S_UID="$(printf '%s\n' "$SIBLING" | awk '{print $2}')"

# Two DISTINCT Workloads or the deletion proves nothing: with the group still on one Workload, the
# victim and the sibling name the same object and "the sibling survived" cannot be asked.
if [ -z "$VICTIM" ] || [ -z "$SIBLING" ] || [ "$V_WL" = "$S_WL" ]; then
  record SKIP "deleting one replica's Workload leaves a sibling admitted" \
    "needs two replicas holding two different Workloads; the rows above say the group never reached that shape"
else
  kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$V_WL" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 15

  S_UID_NOW="$(kubectl -n "$NS" get pod "$S_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  S_PHASE="$(kubectl -n "$NS" get pod "$S_POD" -o jsonpath='{.status.phase}' 2>/dev/null)"
  S_ADM="$(wl_admitted "$S_WL")"

  if [ "$S_UID_NOW" = "$S_UID" ] && [ "$S_ADM" = True ]; then
    record PASS "deleting one replica's Workload leaves a sibling admitted" \
      "deleted ${V_WL}, the Workload of ${V_POD}; ${S_POD} kept its UID and ${S_WL} still reads Admitted=True, phase ${S_PHASE}"
  else
    record FAIL "deleting one replica's Workload leaves a sibling admitted" \
      "deleted ${V_WL}, the Workload of ${V_POD}, and the sibling did not come through: ${S_POD} uid ${S_UID_NOW:-gone} was ${S_UID}, ${S_WL} Admitted=${S_ADM:-absent}, phase ${S_PHASE:-none}"
  fi

  # THE VICTIM'S OWN HALF, SPLIT BY WHAT IS ACTUALLY KNOWN. Measured on the first run that carried
  # this row: the released Pod was GONE and the deployment was back at four pairs. Converging on the
  # declared replica count is the controller's own contract, so that half is asserted. WHICH of
  # Kueue's recoveries produced it -- a new Workload over the surviving Pod, or the Pod leaving and a
  # replacement arriving -- is a timing inside Kueue that nothing here pins, so it stays a reading.
  # Asserting the mechanism from one observation would be asserting a guess that happened to be seen
  # once. NOT closed by the sibling row above, which says nothing about the replica that was hit.
  V_PAIRS=0
  for _ in $(seq 1 20); do
    V_PAIRS="$(replica_rows case49-pd | grep -c . || true)"
    [ "$V_PAIRS" = 4 ] && break
    sleep 3
  done
  V_UID_NOW="$(kubectl -n "$NS" get pod "$V_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  V_FATE="$([ -n "$V_UID_NOW" ] && echo "still present" || echo "gone")"
  if [ "$V_PAIRS" = 4 ]; then
    record PASS "the deployment returns to four replica/Workload pairs" \
      "${V_POD} is ${V_FATE}; whichever way it recovered, the controller converged back on the declared replicas"
  else
    record FAIL "the deployment returns to four replica/Workload pairs" \
      "settled at ${V_PAIRS} pair(s), not 4 — deleting one replica's Workload cost the deployment a replica it declares (${V_POD} is ${V_FATE})"
  fi
fi

# --- the deletion, which the rows above all pass without ---

kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai case49-pd \
  --ignore-not-found --wait=false >/dev/null 2>&1

# THE WORKLOAD IS CHECKED TOO, because the deployment and the Pods leaving does not imply it did.
# It is nobody's dependent -- its owners are the Pods, without a controller reference -- so a
# regression that stopped deleting it would leave an orphan holding quota while every other reading
# here says the group is gone. Named by the Workloads whose owner names carry this deployment's
# prefix, since by then there are no Pods left to trace ownership from.
# ONE query per call, not one per Workload. This runs on every iteration of the delete-wait loop,
# against an API server that is busy tearing the group down; names and owner names come back together
# and the match happens here.
orphan_workloads() {
  kubectl -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].name}{"\n"}{end}' \
    2>/dev/null | grep -c 'case49-pd-' || true
}

GONE=no
for _ in $(seq 1 "$((DELETE_BOUND / 3))"); do
  if [ "$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case49-pd \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] \
    && [ "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] \
    && [ "$(orphan_workloads)" = 0 ]; then
    GONE=yes
    break
  fi
  sleep 3
done

if [ "$GONE" = yes ]; then
  record PASS "deleting the group completes" \
    "the deployment, its replicas and every one of their Workloads are gone within ${DELETE_BOUND}s"
else
  record FAIL "deleting the group completes" \
    "still present after ${DELETE_BOUND}s: phase '$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case49-pd -o jsonpath='{.status.phase}' 2>/dev/null)', $(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd --no-headers 2>/dev/null | wc -l | tr -d ' ') replica(s) held by Kueue's finalizer, $(orphan_workloads) Workload(s) still owned by them"
fi
force_release case49-pd

# --- deployment 2: two roles that differ ONLY in name ---

apply_md case49-twins "$(role_block left '' 2)
$(role_block right '' 2)"

if ! wait_pods case49-twins 4; then
  record FAIL "two identically-shaped roles keep their own PodSet names" \
    "the group never reached four replicas, so Kueue composed nothing to inspect. apply said: ${APPLY_OUT:0:200}"
else
  WL2="$(wait_workload case49-twins)"
  if [ -z "$WL2" ]; then
    record FAIL "two identically-shaped roles keep their own PodSet names" \
      "no Workload owns this deployment's Pods"
  else
    TW_WLS="$(workload_podsets case49-twins)"
    n_left="$(printf '%s\n' "$TW_WLS" | grep -c ' left=1$' || true)"
    n_right="$(printf '%s\n' "$TW_WLS" | grep -c ' right=1$' || true)"
    # THE FAILING SHAPE IS FOUR WORKLOADS WHOSE PodSets ALL CARRY ONE DERIVED NAME, and every one of
    # them is a legal Workload. Nothing about the Workload count tells it from the right answer --
    # these roles are separate groups either way, because a group name is derived from the role --
    # so what has to be asserted is the NAMES, split two and two. Asserting only that both names
    # appear would pass against three of one and one of the other.
    if [ "$n_left" = 2 ] && [ "$n_right" = 2 ]; then
      record PASS "two identically-shaped roles keep their own PodSet names" \
        "2 left=1 and 2 right=1 — the role-hash annotation is what keeps the names apart"
    else
      record FAIL "two identically-shaped roles keep their own PodSet names" \
        "left=1 x${n_left}, right=1 x${n_right}: [${TW_WLS//$'\n'/; }] — one derived name across all four is the collapse this design exists to prevent"
    fi
  fi
fi

# --- deferred: the serving half ---

# THESE TWO ROWS ARE A LEDGER ENTRY, so each states what does NOT close it. A gap recorded as only
# "deferred" gets closed by the first thing that looks like coverage.
record SKIP "both roles reach status.roles[].ready == replicas" \
  "needs an engine that serves, which needs an accelerator. NOT closed by: rerunning this case on a CPU-only cluster; a unit test, since the claim is a container starting; or passing E2E_MD_IMAGE a real engine image, since the engine needs the hardware it was built for"
record SKIP "status.endpoint answers an inference request" \
  "same requirement, and the same three things do not close it. status.roles[].assignedFlavors is unreachable here for a third reason: it reports the flavors of a role's ACCELERATOR credits, and a CPU-only pool quotes one for cpu only"

# Results.
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-49] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-49] all checks passed (the serving half is deferred; see the SKIP rows)"
