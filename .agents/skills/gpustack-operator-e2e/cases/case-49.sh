#!/usr/bin/env bash
#
# CASE 49 — Every replica of a multi-role ModelDeployment becomes its own Kueue Workload, deleting
#           the deployment completes, and a Service deleted out of band comes back
#           (MUTATING, self-recovering)
#
#   case-49.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MD_BINDING,E2E_MD_DELETE_BOUND,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_SVC_RECREATE_BOUND
# Goal:        Two contracts of the ModelDeployment controller, read on a live Kueue and on a live
#              Service watch.
#
#              THE FIRST IS THE PER-REPLICA UNIT. This is the mechanism a prefill/decode deployment
#              rests on, and it is the half that CANNOT be asserted anywhere but on a live Kueue:
#              the operator writes labels and annotations, and KUEUE decides what they compose
#              into. A unit test can prove the operator stamped `pod-group-name`; only this can
#              prove Kueue read it and composed one Workload PER REPLICA, each carrying a single
#              PodSet named after its role.
#
#              Two roles that differ ONLY in name render identical Pod specs, and Kueue derives a
#              role hash from a Pod spec's SHAPE when the annotation is absent -- so without it
#              every one of those replicas names its PodSet the same derived thing, and nothing can
#              tell which Workload answers for which role: per-role counting, per-role flavor
#              assignment and per-role quota all attribute to whichever role the join happens to
#              reach first, with nothing erroring. A case that only ran prefill-and-decode would
#              pass either way, because those two differ in their rendered engine arguments. So
#              this file runs both.
#
#              THE DELETION ROW IS NOT HOUSEKEEPING. The group is annotated as serving, which Kueue
#              defines as a group that never finishes, so Kueue never releases the finalizer it holds
#              on the replicas on its own -- and the Workload that would release it is owned by those
#              same replicas, without a controller reference, so garbage collection cannot reach it
#              either. A deployment whose teardown does not break that cycle sits in Deleting forever
#              with nothing erroring anywhere, which is a state every row above still passes in.
#
#              THE SECOND IS THE SERVICE WATCH. The ModelDeployment controller watches the Service
#              it reconciles, so an externally deleted Service is corrected by the next reconcile
#              rather than at a resync hours away. The code path has no observable output of its
#              own: a controller built without the watch still creates the Service on first
#              reconcile and still renders it correctly, so every existing reading about that
#              Service is identical with the watch removed.
#
#              THE FIGURE IS THE UID, AND THE ATTRIBUTION IS EVERY OTHER OBJECT THIS CONTROLLER IS
#              WOKEN BY. A row that only checked the Service exists at the end would pass if the
#              delete had silently failed, so the recreated Service must carry a DIFFERENT uid. And
#              a row that stopped there would pass with the watch removed on any cluster where some
#              other watched object happened to be written in the same window: the controller also
#              owns the replicas and the router objects, and watches the InstanceType, the
#              KVCachePoolBinding, the KVCachePool, the KVCacheBackend and the replicas' Kueue
#              Workloads -- most with no generation predicate, so a status write on any of them wakes
#              it and the Service comes back for a reason this case is not about. So all of them are
#              sampled either side of the window, and the row says only what that sample supports:
#              nothing else the controller watches moved.
#
#              EVERY COMPARISON HERE IS ALSO ASKED WHAT IT RECORDS WHEN BOTH SIDES ARE EMPTY, because
#              an absence and a quiet object read alike. With no replica Pod at all the quiescence
#              loop would declare quiescence after 9s of nothing and the attribution would compare ""
#              to "", so a replica has to be OBSERVED before either reading means anything.
#
#              A shortage, a filler and a quota edit are all deliberately absent. Whether the group
#              is admitted has nothing to do with Service reconciliation, and the quieter the
#              replicas are the sharper the attribution gets.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose pod
#              integration is enabled, and an operator image carrying the multi-role ModelDeployment.
#              The namespace must carry the pool's entrance LocalQueue -- `kubectl -n <NS> get
#              localqueue` -- which is what routes the group into the ClusterQueue.
#
#              NO GPU is needed and nothing here has to serve: a Workload is composed from the Pods as
#              they are CREATED, before any container starts, and a Service is reconciled from the
#              deployment's spec, so every assertion holds on a CPU-only node. EXITS 2 (input
#              required) when the cluster has no InstanceType.
#
# Inputs:      All real, nothing mocked. Three ModelDeployments: a prefill and a decode role of two
#              replicas each, a pair of roles of two replicas each that differ only in their names,
#              and a single-role group of one replica for the Service watch. The KVCachePoolBinding
#              is not resolved by anything asserted here, so it may be absent. Each role carries an
#              explicit `image` because the replicas are placeholders and no assertion reads anything
#              they run; override with E2E_MD_IMAGE, the InstanceType with E2E_MD_INSTANCE_TYPE, the
#              Service recreate bound with E2E_SVC_RECREATE_BOUND, the delete bound with
#              E2E_MD_DELETE_BOUND.
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
#              The Service the single-role deployment is reached through is created; after
#              `kubectl delete svc`, a Service of the same name exists again within the bound,
#              carrying a different uid; and nothing else this controller watches changed between
#              the delete and the recreate, which leaves the Service watch as what drove the
#              reconcile. SKIP, not FAIL, when something did move or when no replica was ever
#              observed: neither is a defect in anything, it just means that run cannot say which
#              watch did the work.
#
# Cleanup:     A trap deletes the three deployments and, if one is wedged past the bound, the Workload
#              that holds its replicas -- the manual form of the release the operator is asserted to
#              do. Idempotent, runs on pass AND fail, safe to re-run. It creates no cluster-scoped
#              object and changes no baseline.
#
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-49.sh <NS>" >&2
  exit 2
fi

BINDING="${E2E_MD_BINDING:-case49-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

# The deployment the Service watch is read on: one role, one replica. The rows about the Service
# are about what the controller reconciles, not about how a group composes.
MD=case49-svc

# How long a group gets to leave. Generous on purpose: the bound is here to turn a deadlock into a
# FAIL, not to measure how quick a delete is.
DELETE_BOUND="${E2E_MD_DELETE_BOUND:-90}"

# How long the correction gets. The informer cache resyncs every hour by default
# (--informer-cache-resync-period), so anything inside this bound separates a watch from a resync by
# a wide margin; the bound exists to turn a missing watch into a FAIL rather than to measure how
# quick the watch is.
RECREATE_BOUND="${E2E_SVC_RECREATE_BOUND:-90}"

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# The first InstanceType A DEPLOYMENT CAN ACTUALLY NAME, which is not the same as the first one the
# API returns.
#
# THE LIST COMES BACK SORTED BY NAME AND CARRIES TYPES ON THEIR WAY OUT. Case 68 creates its own
# `case68-held` and deletes it without waiting, and that name sorts before an ordinary derived
# type -- so a case running straight after it picks a type that is already terminating. Naming one
# is refused at admission, and the run then dies at fixture time for a reason that has nothing to do
# with what it measures. Inactive is excluded for the mirror reason: a deployment on one is admitted
# and then never scheduled, so the case waits out every timeout it has.
if [ -z "$IT" ]; then
  IT="$(md_usable_instance_type)"
fi
if [ -z "$IT" ]; then
  echo "[case-49] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# Delete a wedged group by hand. Deleting the Workload is what releases Kueue's finalizer on the
# replicas, so this is the escape a teardown that fails the deletion row below leaves behind.
# md_force_release deletes EVERY owned Workload, not the first one found: four replicas are four
# Workloads, and deleting one of them leaves three still holding Kueue finalizers on their Pods.

# Every Pod of a group, as name=resourceVersion, sorted so two samples compare as text. Used for
# the quiescence loop, where the replicas are the only churn source worth waiting out, and by the
# cleanup, where it says whether the group is gone.
replica_versions() {
  local md="$1"
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion}{"\n"}{end}' 2>/dev/null \
    | sort | tr '\n' ' '
}

# One kind's resourceVersions, or a loud sentinel line when they could not be observed.
#
# THE SENTINEL IS NOT A VALUE THE ATTRIBUTION MAY COMPARE, and the guard below enforces that. This
# helper exists because discarding a failed read would make an unreadable kind indistinguishable from
# an empty one, so it would drop silently out of both samples while the row went on claiming it --
# but emitting a sentinel only moves the problem unless the consumer refuses to compare it, because
# two sentinels are equal to each other. See the attribution row.
#
# Two failures are told apart, because only one of them is a fault:
#
#   NOT-FOUND    the object is gone. A named `kubectl get` exits nonzero with empty stdout for this
#                exactly as it does for an unreadable kind, so the error text is what separates them.
#   READ-FAILED  the kind itself could not be read -- CRD absent, RBAC, or a transport failure the
#                retrying shim ran out of attempts on.
#
# A LIST call over a kind with no objects is neither: it prints nothing and exits 0, which is an
# observation of zero objects and is left as such.
#
# STDERR IS KEPT SEPARATE RATHER THAN FOLDED IN WITH `2>&1`, which would be the way to avoid a
# scratch file entirely. The retrying kubectl shim writes to stderr on a call that eventually
# succeeds -- `kubectl-shim: transport failure, retrying ...` -- so folding it in would put that
# notice into the sample as a line pretending to be a resourceVersion.
#
# One scratch file for the whole case, not one per call: this runs on every quiescence poll and twice
# more for the attribution, so a per-call mktemp both multiplies its own failure path and leaks a
# file per in-flight call if the runner kills the case. An empty SAMPLE_ERR means mktemp failed; the
# redirect then goes to /dev/null and every failure classifies as READ-FAILED, which is the
# conservative sentinel rather than a wrong one.
SAMPLE_ERR="$(mktemp 2>/dev/null || true)"

sample() {
  local label="$1" out
  shift
  if out="$(kubectl "$@" 2>"${SAMPLE_ERR:-/dev/null}")"; then
    # Command substitution strips the trailing newline, so it is put back here rather than left to
    # glue this kind's last line onto the next kind's first one. A kind with no objects prints
    # nothing at all, which is why the empty case is skipped instead of emitting a blank line.
    if [ -n "$out" ]; then
      printf '%s\n' "$out"
    fi
  elif [ -n "$SAMPLE_ERR" ] && command grep -qi 'not found' "$SAMPLE_ERR"; then
    printf '%s=NOT-FOUND\n' "$label"
  else
    printf '%s=READ-FAILED\n' "$label"
  fi

  return 0
}

# Everything this controller is woken by EXCEPT the Service under test, as
# kind/name=resourceVersion. This list must match the Owns/Watches clauses of the ModelDeployment
# controller's SetupWithManager one for one: a kind missing here is a wake-up the attribution cannot
# see. The controller Owns the replica Pods, the Service and the router's Deployment, ConfigMap,
# ServiceAccount, Role and RoleBinding, and Watches the KVCachePoolBinding, the InstanceType, the
# KVCachePool, the KVCacheBackend and the replicas' Kueue Workloads; its own spec is filtered by a
# GenerationChangedPredicate and nothing here edits it, but it is sampled anyway so the list needs no
# argument about that. This deployment has no router, so the router kinds are expected to be empty on
# both sides; they are sampled so that a router object appearing in the window is seen. Workloads are
# sampled for the whole namespace, which can only disqualify the row more often, never pass it wrongly.
watched_versions() {
  local md="$1"
  {
    sample pods -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      -o jsonpath="{range .items[*]}pod/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample routerobjects -n "$NS" get deployments,configmaps,serviceaccounts,roles,rolebindings \
      -l "app.kubernetes.io/instance=${md}" \
      -o jsonpath="{range .items[*]}{.kind}/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample workloads -n "$NS" get workloads.kueue.x-k8s.io \
      -o jsonpath="{range .items[*]}workload/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample instancetype get instancetypes.worker.gpustack.ai "$IT" \
      -o jsonpath="instancetype/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}"
    sample modeldeployment -n "$NS" get modeldeployments.worker.gpustack.ai "$md" \
      -o jsonpath="modeldeployment/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}"
    sample kvcachepoolbindings get kvcachepoolbindings.worker.gpustack.ai -A \
      -o jsonpath="{range .items[*]}kvcachepoolbinding/{.metadata.namespace}/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample kvcachepools get kvcachepools.worker.gpustack.ai \
      -o jsonpath="{range .items[*]}kvcachepool/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample kvcachebackends get kvcachebackends.worker.gpustack.ai \
      -o jsonpath="{range .items[*]}kvcachebackend/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
  } | sort | tr '\n' ' '
}

cleanup() {
  echo
  echo "[case-49] cleanup"
  [ -n "${SAMPLE_ERR:-}" ] && rm -f "$SAMPLE_ERR"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    case49-pd case49-twins "$MD" --ignore-not-found --wait=false >/dev/null 2>&1
  # Retried rather than done once. md_force_release finds the Workload through the Pods it owns, so a
  # single shot right after the delete finds nothing when admission is still in flight or when the
  # Pod list races the deletion -- and the wedged finalizers it exists to clear then survive, which
  # is exactly the contamination it exists to prevent. Each round is cheap and the loop ends as soon
  # as the Pods are gone.
  for _ in $(seq 1 12); do
    sleep 5
    md_force_release "$NS" case49-pd
    md_force_release "$NS" case49-twins
    md_force_release "$NS" "$MD"
    if [ -z "$(replica_versions case49-pd)" ] \
      && [ -z "$(replica_versions case49-twins)" ] \
      && [ -z "$(replica_versions "$MD")" ]; then
      break
    fi
  done

  return 0
}
trap cleanup EXIT

# The apply's output is KEPT, in MD_APPLY_OUT. Discarded, a schema or webhook refusal -- a very
# plausible failure for this feature -- surfaces only as a 90-second timeout whose FAIL row blames
# the wrong thing ("only N exist, so Kueue composes nothing") and never quotes the refusal that
# actually happened. md_apply writes it there; md_role_block builds the roles it carries.

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

md_apply case49-pd "$NS" "$BINDING" "$(md_role_block prefill Prefill "$IT" 2 "$IMAGE")
$(md_role_block decode Decode "$IT" 2 "$IMAGE")" vLLM "$IMAGE"

if wait_pods case49-pd 4; then
  record PASS "the group's four replicas are all created" \
    "two roles of two, in one reconcile pass"
else
  record FAIL "the group's four replicas are all created" \
    "only $(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd --no-headers 2>/dev/null | wc -l | tr -d ' ') exist, so Kueue composes nothing. apply said: ${MD_APPLY_OUT:0:200}"
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
  local md="$1"
  kubectl -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].name}{"\n"}{end}' \
    2>/dev/null | grep -c "${md}-" || true
}

GONE=no
for _ in $(seq 1 "$((DELETE_BOUND / 3))"); do
  if [ "$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case49-pd \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] \
    && [ "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] \
    && [ "$(orphan_workloads case49-pd)" = 0 ]; then
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
    "still present after ${DELETE_BOUND}s: phase '$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case49-pd -o jsonpath='{.status.phase}' 2>/dev/null)', $(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd --no-headers 2>/dev/null | wc -l | tr -d ' ') replica(s) held by Kueue's finalizer, $(orphan_workloads case49-pd) Workload(s) still owned by them"
fi
md_force_release "$NS" case49-pd

# --- deployment 2: two roles that differ ONLY in name ---

md_apply case49-twins "$NS" "$BINDING" "$(md_role_block left '' "$IT" 2 "$IMAGE")
$(md_role_block right '' "$IT" 2 "$IMAGE")" vLLM "$IMAGE"

if ! wait_pods case49-twins 4; then
  record FAIL "two identically-shaped roles keep their own PodSet names" \
    "the group never reached four replicas, so Kueue composed nothing to inspect. apply said: ${MD_APPLY_OUT:0:200}"
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

# The twins group leaves before the Service watch below runs, and its Workloads go with it: the
# attribution samples every Workload in the namespace, and one that moves inside the window is what
# turns that row into a SKIP. md_force_release is the release the trap uses too, so a group wedged
# behind Kueue's finalizer does not hold the namespace open.
kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai case49-twins \
  --ignore-not-found --wait=false >/dev/null 2>&1
for _ in $(seq 1 20); do
  TW_PODS="$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-twins \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$TW_PODS" = 0 ] && [ "$(orphan_workloads case49-twins)" = 0 ]; then
    break
  fi
  sleep 3
  md_force_release "$NS" case49-twins
done

# --- deferred: the serving half ---

# THESE TWO ROWS ARE A LEDGER ENTRY, so each states what does NOT close it. A gap recorded as only
# "deferred" gets closed by the first thing that looks like coverage.
record SKIP "both roles reach status.roles[].ready == replicas" \
  "needs an engine that serves, which needs an accelerator. NOT closed by: rerunning this case on a CPU-only cluster; a unit test, since the claim is a container starting; or passing E2E_MD_IMAGE a real engine image, since the engine needs the hardware it was built for"
record SKIP "status.endpoint answers an inference request" \
  "same requirement, and the same three things do not close it. status.roles[].assignedFlavors is unreachable here for a third reason: it reports the flavors of a role's ACCELERATOR credits, and a CPU-only pool quotes one for cpu only"

# --- the Service watch ---

echo "== 1. a single-role deployment, and the Service it is reached through =="

# The apply's output is KEPT. Discarded, a schema or webhook refusal surfaces only as a timeout
# whose FAIL row blames the watch for an object that was never created.
md_apply "$MD" "$NS" "$BINDING" "$(md_role_block server '' "$IT" 1 "$IMAGE")" vLLM "$IMAGE"

# The gate is that the object is THERE, not that the apply printed one of three words. A substring
# match would take a refusal whose text happens to contain "created" for success, and matching the
# exact word instead would make the gate depend on whether this run is the first over a surviving
# namespace. Reading the object back answers the question the checks below actually need.
SVC_PHASE=run
if ! kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o name >/dev/null 2>&1; then
  record FAIL "the deployment's Service is created" \
    "no modeldeployment/${MD} after the apply, so nothing below has a subject. The apply said: $(printf '%s' "${MD_APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  SVC_PHASE=skip
fi

if [ "$SVC_PHASE" = run ]; then
  first_uid=""
  for _ in $(seq 1 30); do
    first_uid="$(kubectl -n "$NS" get svc "$MD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    [ -n "$first_uid" ] && break
    sleep 3
  done
  if [ -z "$first_uid" ]; then
    record FAIL "the deployment's Service is created" \
      "no svc/${MD} within 90s, so there is nothing to delete and nothing to correct"
  else
    record PASS "the deployment's Service is created" "svc/${MD} uid=${first_uid}"
  fi
fi

if [ "$SVC_PHASE" = run ] && [ -n "${first_uid:-}" ]; then
  echo "== 2. wait for a replica to appear, then for the replicas to go quiet =="

  # A replica has to be OBSERVED before "unchanged" carries any information. With no Pod at all
  # replica_versions returns the empty string forever, the loop below compares "" to "" and declares
  # quiescence after 9s of nothing, and the attribution row would then compare two empty samples and
  # pass. Whether Kueue admits this group is not this case's subject and not something it arranges, so
  # the absence is recorded and carried rather than treated as a failure.
  replicas_seen=false
  for _ in $(seq 1 40); do
    if [ -n "$(replica_versions "$MD")" ]; then
      replicas_seen=true
      break
    fi
    sleep 3
  done

  # Quiescence is a precondition of the attribution, not a nicety. While the replicas are still being
  # written the controller is being woken by its Pod watch several times a second, and a Service
  # deleted in that window comes back for a reason this case is not about.
  #
  # Skipped outright when no replica was ever observed: with nothing to sample this loop would compare
  # "" to "" for its full 40 rounds and then declare quiescence, spending two minutes to produce a
  # reading whose value is already decided. The attribution is disqualified either way below.
  quiet_for=0
  last="$(replica_versions "$MD")"
  if [ "$replicas_seen" = true ]; then
    for _ in $(seq 1 40); do
      sleep 3
      now="$(replica_versions "$MD")"
      if [ "$now" = "$last" ]; then
        quiet_for=$((quiet_for + 3))
        [ "$quiet_for" -ge 9 ] && break
      else
        quiet_for=0
      fi
      last="$now"
    done
  fi
  echo "[case-49] replica(s) observed: ${replicas_seen}; unchanged for ${quiet_for}s: ${last:-<none>}"

  # Whether the attribution row can say anything, decided BEFORE the window and from the preconditions
  # rather than from the reading. Exhausting the loop without ever reaching 9 quiet seconds used to
  # fall through here silently: the sample was taken mid-churn, the row would SKIP or - worse - pass on
  # two samples that merely happened to match, and nothing said the precondition itself had failed.
  attributable=true
  attribution_note=""
  if [ "$replicas_seen" != true ]; then
    attributable=false
    attribution_note="no replica Pod was ever observed, so an unchanged sample would be a reading about an absence rather than about a watch"
  elif [ "$quiet_for" -lt 9 ]; then
    attributable=false
    attribution_note="the replicas never went 9s without a write (${quiet_for}s at best), so the controller was being woken throughout the window"
  fi

  before="$(watched_versions "$MD")"

  echo "== 3. delete the Service out of band =="
  del_out="$(kubectl -n "$NS" delete svc "$MD" 2>&1)"
  echo "$del_out"
  started=$SECONDS

  new_uid=""
  elapsed=0
  for _ in $(seq 1 "$RECREATE_BOUND"); do
    new_uid="$(kubectl -n "$NS" get svc "$MD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    if [ -n "$new_uid" ] && [ "$new_uid" != "$first_uid" ]; then
      elapsed=$((SECONDS - started))
      break
    fi
    new_uid=""
    sleep 1
  done

  after="$(watched_versions "$MD")"

  echo "== 4. the readings =="
  if [ -n "$new_uid" ]; then
    record PASS "a Service of the same name exists again, with a different uid" \
      "uid ${first_uid} -> ${new_uid} in ${elapsed}s; a same-uid Service would have meant the delete never took effect"
  else
    # Named apart from a rendering failure: the Service may be absent, or present under the ORIGINAL
    # uid, and only the first is a missing correction.
    still="$(kubectl -n "$NS" get svc "$MD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    case "$still" in
      "$first_uid") record FAIL "a Service of the same name exists again, with a different uid" \
        "svc/${MD} still carries uid ${first_uid} after ${RECREATE_BOUND}s, so the delete never took effect and this case measured nothing" ;;
      "") record FAIL "a Service of the same name exists again, with a different uid" \
        "no svc/${MD} ${RECREATE_BOUND}s after the delete; the controller was not woken, which is what a missing Service watch looks like" ;;
      *) record FAIL "a Service of the same name exists again, with a different uid" \
        "svc/${MD} carries uid ${still}, which is neither the original nor one this poll observed" ;;
    esac
  fi

  # The attribution, and it claims only what the sample supports: no OTHER object this controller
  # watches moved, which leaves the Service watch. Every failure mode is a SKIP rather than a FAIL --
  # a watched object being written in the window is not a defect in anything, it just means this run
  # cannot say which watch did the work, and the uid row above stands on its own either way.
  # A SENTINEL IN EITHER SAMPLE DISQUALIFIES THE COMPARISON, and this guard is the point of the
  # sentinel existing. `sample` emits NOT-FOUND or READ-FAILED for a kind it could not observe, so that
  # such a kind cannot drop silently out of the sample -- but a kind that is unobservable for the whole
  # window emits the SAME sentinel on both sides, and `before = after` is then true. The row would
  # record PASS while naming a kind it never saw, which is the exact false pass the sentinel was added
  # to prevent, reintroduced one layer up. An equality test is always true when both sides are the
  # marker for "no data", so the marker has to be excluded before the test rather than compared by it.
  if printf '%s\n%s\n' "$before" "$after" | command grep -Eq '=(NOT-FOUND|READ-FAILED)( |$)'; then
    record SKIP "no other object this controller watches changed in the window" \
      "at least one watched kind could not be observed across the window, so an unchanged sample would not mean it did not move: before=[${before}] after=[${after}]"
  elif [ "$attributable" != true ]; then
    record SKIP "no other object this controller watches changed in the window" \
      "the precondition failed, so the row above passes unattributed: ${attribution_note}"
  elif [ "$before" = "$after" ]; then
    record PASS "no other object this controller watches changed in the window" \
      "identical either side of the delete, so no Pod, router object, Workload, InstanceType, Binding, pool or backend event could have driven the reconcile: ${after}"
  else
    record SKIP "no other object this controller watches changed in the window" \
      "something moved, so the row above passes unattributed: before=[${before}] after=[${after}]"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-49] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-49] all checks passed (the serving half is deferred; see the SKIP rows)"
