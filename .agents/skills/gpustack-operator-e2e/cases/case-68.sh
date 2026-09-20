#!/usr/bin/env bash
#
# CASE 68 — A deployment whose roles sit on TWO InstanceTypes is admitted as a set or not at all,
#   parks when the set never assembles, and scales one role without touching anything already
#   running   (MUTATING, self-cleaning)
#
#   case-68.sh <NS>
#
# Goal:        Roles on two InstanceTypes used to be refused outright, because one Kueue Workload
#              carries one queueName and the atomicity the shape needs came entirely from Kueue's
#              intra-group rule. The replacement is one pod group per InstanceType plus an
#              AdmissionCheck that gates every group of the deployment on the whole set. Every
#              claim in that sentence is about objects the operator creates in a cluster, and none
#              of it is observable from a unit test: the Workloads are Kueue's, the quota is the
#              ClusterQueue's, and "no role is admitted" is a statement about two objects at once.
#
# Environment: A single-node cluster with NO accelerator and the operator deployed. That is the
#              intended shape rather than a limitation -- the case needs one group to be
#              INFEASIBLE, and a cluster with capacity to spare would take that away. The second
#              InstanceType is CREATED here rather than assumed: "the cluster has two pools" has to
#              be a fact the case makes, or it is a fact the case is waiting for.
#
# Inputs:      Real objects only. One CPU-only InstanceType the case creates and then marks
#              INACTIVE, which makes the reconciler hold its ClusterQueue so that group can never
#              reserve quota; one ModelDeployment with two roles; and the Binding named by
#              E2E_MD_BINDING. The runner image is a pause image; nothing here runs an engine.
#              An ACCELERATABLE type with no node behind it does NOT work as the fixture -- see the
#              comment where it is created.
#
# Expected:    Phase A -- the set assembles (both roles on the working type):
#              - each replica carries a group name of its own, so two roles of one replica each are
#                TWO groups even though they share an InstanceType. This read "one group" while a
#                role was the admission unit and one type allowed one queue name, hence one Workload;
#              - every replica reaches admitted, so the scale assertion below has Pods to compare.
#              Phase B -- two groups, one of them infeasible:
#              - the deployment's replicas carry TWO distinct group names. NOTE that this count no
#                longer separates Phase B from Phase A, which also has two: what Phase B varies is
#                that the groups land in different ClusterQueues and one of those is held, so the
#                load-bearing rows are the admission ones, not the count;
#              - Kueue composes TWO Workloads;
#              - NO role is admitted while one group cannot be placed -- and the control below is
#                what makes that mean anything;
#              - the control: the feasible role, deployed ALONE, IS admitted. Without it "no role
#                is admitted" is true whether or not the joint check exists, and the case proves
#                nothing.
#              Phase C -- the bound:
#              - with the check's own LastTransitionTime back-dated past the bound, the next
#                reconcile DEACTIVATES the workloads rather than deleting them, and the
#                deployment's QuotaReserved condition reports Parked naming what is waiting.
#              Phase D -- blast radius of a scale (runs on the Phase A shape):
#              - scaling one role leaves the other role's Pod UIDs unchanged, AND leaves the scaled
#                role's own existing replicas unchanged. The second half is what only holds once a
#                replica is the admission unit; the first held before it too.
#              Phase E -- the barrier OPENS, with the sibling arriving demonstrably later:
#              - one group RESERVES and is confirmed held (reserved=1, admitted=0 of 2);
#              - only then is the other group made placeable, and EVERY group reaches admitted --
#                including the one that had already reserved and was being held.
#
# WHY PHASE E EXISTS AT ALL, and it is not a regression row. Every phase above measures the barrier
# CLOSING: it holds, it keeps holding, and past the bound it parks. A barrier that never opens
# satisfies all of them perfectly. Opening is half the behaviour of this feature and no case
# exercised it, so the half that was covered was the half that cannot notice. It stayed invisible
# because a verdict depends on a SIBLING object's state, and in a small cluster the two groups
# reserve within one reconcile of each other -- so each group's own event arrives after the other
# has already reserved, and the set opens whether or not anything watches across the pair. That is a
# timing coincidence, not wiring.
#
# WHICH IS ALSO WHY PHASE E PINS AN ORDER RATHER THAN AN END STATE. "Everything ended up admitted"
# is produced by the coincidence just as well as by the wiring. Establishing that one group was
# already holding quota before the other could place it is what makes the held group's movement
# attributable to hearing about its sibling.
#
# Not covered: that a prefill and a decode replica land on different physical cards. That needs
#              hardware, it is T12 of the stabilization spec, and no cluster case substitutes for
#              it. Also not covered: that Kueue STAMPS LastTransitionTime correctly -- Phase C
#              back-dates that stamp, so what it exercises is the operator's comparison against it,
#              which is the half this repository owns.
#
# Why the phases are separate states rather than one: "no role is admitted" and "the other group's
# Pods are unchanged by a scale" cannot both hold at once. The second needs admitted Pods to
# compare, and the first exists only while one group cannot be placed. A case asserting both
# against one object would be asserting one of them against a state that cannot produce it. Phase E
# needs a THIRD object for the same reason: Phase C deactivates the deployment it runs on, and a
# deactivated workload is never admitted again whatever the cluster does.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-68.sh <NS>}"

# THE CONTEXT IS AN INPUT RATHER THAN WHATEVER IS CURRENT. Empty keeps the behaviour every other
# case has, so a runner that already selected a context is unaffected. Set it and this case stops
# depending on a value another session can change underneath it -- a check reading global mutable
# state is correct or not for reasons that are nowhere in its own output.
KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }

MD=case68-pair
MD_CONTROL=case68-control
MD_OPENS=case68-opens
IT_UNPLACEABLE=case68-nowhere
BINDING="${E2E_MD_BINDING:-case68-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

# How long a state gets to appear. Admission is a scheduling round plus a check round trip, and the
# bound exists to turn "never happens" into a FAIL rather than to measure how quick it is.
SETTLE="${E2E_MD_SETTLE:-120}"

FAILS=0
NOREADS=0
ROWS=()

# STATUS is one of PASS, FAIL, SKIP or NO-READ, and the last of those is not a shade of the others.
#
# A row whose PRECONDITION did not hold has no reading: it did not pass, it did not fail, and
# nothing was measured. Recording it as FAIL inflates the failure count with consequences of one
# cause; recording it as PASS is worse; and recording it as SKIP says it was deliberately not
# applicable, which is a different statement. This case runs every phase and collects the whole
# picture in one pass -- each iteration here costs a remote image build -- so the write-up has to be
# able to say which rows were actually exercised and which were blocked by something upstream.
record() {
  ROWS+=("$1|$2|$3")
  case "$1" in
    FAIL) FAILS=$((FAILS + 1)) ;;
    NO-READ) NOREADS=$((NOREADS + 1)) ;;
  esac

  return 0
}

# The first InstanceType A DEPLOYMENT CAN ACTUALLY NAME, which is not the same as the first one the
# API returns.
#
# THE LIST COMES BACK SORTED BY NAME AND CARRIES TYPES ON THEIR WAY OUT. This case creates its own
# `case68-nowhere` and deletes it without waiting, so a second run started straight after the first
# sees it still terminating -- and `case68-nowhere` sorts before an ordinary derived type. Naming a
# type that is being deleted is refused at admission, and the run then dies at fixture time for a
# reason that has nothing to do with what it measures. Inactive is excluded for the mirror reason: a
# deployment on one is admitted and then never scheduled, so the case waits out every timeout it has.
usable_instance_type() {
  k get instancetypes.worker.gpustack.ai \
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
  echo "[case-68] no usable InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

cleanup() {
  k -n "$NS" delete modeldeployment "$MD" "$MD_CONTROL" "$MD_OPENS" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  k delete instancetype.worker.gpustack.ai "$IT_UNPLACEABLE" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

# Every replica of one deployment, as name=<group name>, sorted so two samples compare as text.
group_names() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" \
    -o jsonpath='{range .items[*]}{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort -u
}

# PREDICATES FOR wait_for, WHICH RE-RUNS A COMMAND EACH ROUND. Passing `test "$(reading)" = n`
# instead expands the reading ONCE, before the wait even begins, and then compares that same stale
# value on every round -- a wait that can only succeed immediately or run out the clock. It reads
# like a wait and behaves like a single sample.
group_count_is() { [ "$(group_names "$1" | wc -l | tr -d ' ')" = "$2" ]; }
admitted_count_is_not() { [ "$(admitted_count)" != "$1" ]; }
md_quota_reason_is() {
  [ "$(k -n "$NS" get modeldeployment "$1" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)" = "$2" ]
}

# Pod name=UID for one role, which is what tells a Pod that STAYED from one replaced by an
# identical render. The UID is the cluster's to assign and is the reason this assertion belongs
# here rather than in a unit test: a fake client leaves it empty, and two empty strings compare
# equal exactly as two unchanged Pods do.
role_uids() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1,app.kubernetes.io/component=$2" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | sort | tr '\n' ' '
}

# The Workloads Kueue composed for one deployment's replicas, by name.
deployment_workloads() {
  local uids
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -z "$uids" ] && return 0
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{range .metadata.ownerReferences[*]}{.uid}{","}{end}{"\n"}{end}' 2>/dev/null \
    | while IFS='|' read -r name owners; do
        for u in $uids; do
          case "$owners" in *"$u"*) echo "$name"; break ;; esac
        done
      done | sort -u
}

admitted_count() {
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null \
    | grep -c '^True$'
}

# How many of ONE deployment's workloads report Admitted, as "<admitted>/<total>". The whole-cluster
# count above cannot answer Phase E: another deployment being admitted would satisfy it, and the
# claim there is about every group of one deployment.
deployment_admitted() { # deployment_admitted <md>
  local wl total=0 ok=0
  for wl in $(deployment_workloads "$1"); do
    total=$((total + 1))
    if [ "$(k -n "$NS" get workload "$wl" \
        -o jsonpath='{.status.conditions[?(@.type=="Admitted")].status}' 2>/dev/null)" = True ]; then
      ok=$((ok + 1))
    fi
  done
  echo "$ok/$total"
}

# "<reserved>/<admitted>/<total>" over one deployment's workloads.
#
# PHASE E NEEDS ALL THREE NUMBERS AND NOT JUST THE ADMITTED ONE. "Nothing is admitted" is also true
# of a deployment where NOTHING HAS RESERVED YET, and from that state releasing the held type lets
# both groups reserve in the same scheduling round -- which is the coincidence that hid the missing
# watch in the first place. Reserved=1 with admitted=0 is the state where one group is demonstrably
# holding quota and waiting on the other, and it is the only starting point from which "the held
# group moved" can mean "it heard about its sibling".
deployment_quota() { # deployment_quota <md>
  local wl total=0 res=0 adm=0 conds
  for wl in $(deployment_workloads "$1"); do
    total=$((total + 1))
    conds="$(k -n "$NS" get workload "$wl" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status};{end}' 2>/dev/null)"
    case "$conds" in *"QuotaReserved=True;"*) res=$((res + 1)) ;; esac
    case "$conds" in *"Admitted=True;"*) adm=$((adm + 1)) ;; esac
  done
  echo "$res/$adm/$total"
}

wait_for() { # wait_for <seconds> <predicate...>
  local deadline=$((SECONDS + $1)); shift
  while [ $SECONDS -lt "$deadline" ]; do
    "$@" && return 0
    sleep 5
  done

  return 1
}

apply_deployment() { # apply_deployment <name> <role yaml block>
  k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: $1
  namespace: $NS
spec:
  model:
    name: case68/model
  engine:
    name: vllm
    version: "0.25.1"
  kvCache:
    poolRef:
      name: $BINDING
  roles:
$2
YAML
}

one_role() {
  cat <<YAML
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      image: $IMAGE
      command: ["/pause"]
YAML
}

two_roles_one_type() {
  cat <<YAML
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      image: $IMAGE
      command: ["/pause"]
    - name: beta
      kind: server
      replicas: 1
      instanceType: $IT
      image: $IMAGE
      command: ["/pause"]
YAML
}

two_roles_two_types() {
  cat <<YAML
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      image: $IMAGE
      command: ["/pause"]
    - name: beta
      kind: server
      replicas: 1
      instanceType: $IT_UNPLACEABLE
      # NO resources block. The type is not acceleratable, so nothing defaults a card count here --
      # and a card count is what the per-unit ceiling rule reads. Declaring one would put this role
      # back in front of that rule.
      image: $IMAGE
      command: ["/pause"]
YAML
}

# ---------------------------------------------------------------- Phase A: one group, admitted.
apply_deployment "$MD" "$(two_roles_one_type)"

# TWO ROLES ON ONE TYPE ARE TWO GROUPS, AND THAT IS THE POINT RATHER THAN A REGRESSION. This row
# read `= 1` while a ROLE was the admission unit: two roles on one InstanceType shared the single
# Workload that type's one queue name allowed. A replica is the unit now, and a group is named from
# the deployment, the role and the replica's ordinal -- so two roles of one replica each are two
# groups whatever they sit on.
#
# WHICH COSTS PHASE B ITS OLD DISCRIMINATOR, and saying so here is what stops the next reader from
# trusting a number that no longer separates anything: Phase B's shape is ALSO two groups, so the
# count cannot tell the one-type case from the two-type one. What Phase B varies is whether the two
# groups can be PLACED -- they land in different ClusterQueues and one of them is held -- and its
# load-bearing rows are the admission ones below, not the count.
if wait_for "$SETTLE" group_count_is "$MD" 2; then
  record PASS "roles sharing one type still get a group each" "$(group_names "$MD" | tr '\n' ' ')"
else
  record FAIL "roles sharing one type still get a group each" "saw [$(group_names "$MD" | tr '\n' ' ')]"
fi

# ------------------------------------------------- Phase D (on the Phase A shape): blast radius.
before_beta="$(role_uids "$MD" beta)"
before_alpha="$(role_uids "$MD" alpha)"
if [ -z "$before_beta" ] || [ -z "$before_alpha" ]; then
  record SKIP "a scale leaves every living replica alone" "no replicas to compare; earlier phase failed"
else
  # A JSON PATCH ON ONE FIELD, NOT A MERGE PATCH ON THE LIST. A merge patch replaces `roles`
  # wholesale, so a role restated without its `command` sets command to null -- and that
  # is a FROZEN field, so the identity rule refuses the edit. Measured before the field moved
  # onto the role: the refusal named the command path, and the scale below silently never
  # happened, which turned this row into a PASS that compared two unchanged samples.
  if ! k -n "$NS" patch modeldeployment "$MD" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' >/dev/null 2>&1; then
    record FAIL "scale one role" "the replicas edit was refused; nothing below measured a scale"
  fi
  sleep 20
  after_beta="$(role_uids "$MD" beta)"
  after_alpha="$(role_uids "$MD" alpha)"

  # THE DIRECTION IS ASSERTED NOW, AND IT DELIBERATELY WAS NOT BEFORE. While a whole deployment was
  # one group, a replicas change rebuilt all of it and beta moving was as correct as beta staying --
  # so this row recorded whichever value it saw and PASSED either way, which made it blind to the
  # behaviour it looks like it is about. Every replica is its own group now: a scale of alpha is not
  # beta's business, and it is not alpha's own survivors' business either.
  #
  # ALPHA'S SURVIVORS ARE THE HALF THAT IS NEW HERE. A reading of beta alone passed before this spec
  # and passes after it, so it cannot tell the two apart; what only holds now is that the replica
  # alpha already had keeps its identity while a second is added beside it.
  kept_alpha=0
  for e in $before_alpha; do
    case " $after_alpha " in *" $e "*) kept_alpha=$((kept_alpha + 1)) ;; esac
  done
  want_alpha="$(printf '%s\n' $before_alpha | grep -c . || true)"
  if [ "$before_beta" = "$after_beta" ] && [ "$kept_alpha" = "$want_alpha" ]; then
    record PASS "a scale leaves every living replica alone" \
      "beta untouched, and all ${want_alpha} of alpha's own replicas kept their UIDs: only the added one is new"
  else
    record FAIL "a scale leaves every living replica alone" \
      "beta before/after [${before_beta}] / [${after_beta}]; alpha kept ${kept_alpha}/${want_alpha} of its UIDs"
  fi
fi

k -n "$NS" delete modeldeployment "$MD" --ignore-not-found --wait=true >/dev/null 2>&1

# ------------------------------------------- Phase B: two groups, exactly one of them infeasible.
#
# THE SECOND TYPE IS CPU-ONLY AND THEN MARKED INACTIVE, and both halves of that were learned the
# hard way.
#
# An ACCELERATABLE type with no node behind it does not work: its status carries an accelerator
# ceiling of zero, the defaulter fills the role's card count with one, and the admission rule that
# checks a request against that ceiling refuses the deployment outright. The shape then never
# reaches the scheduler at all, so nothing is "infeasible" -- it is rejected, which is a different
# thing and makes every row below unmeasurable. Measured: "instance type ... hands out at most 0
# accelerator(s) at once".
#
# CORRECTED: THAT WAS NOT THE FIXTURE'S PROBLEM, IT WAS THE RULE'S. The paragraph above is kept
# because its mechanism and its measured reading are both right, but it names the wrong culprit, and
# a reader who takes it at face value routes around a product defect instead of reporting it. The
# rule was applying a WHOLE-CARD ceiling to a request that was not for whole cards; the whole-card
# view counts free unpartitioned cards and reads zero on a pool that has carved all of its own. That
# is fixed, and the card count it rejected was one the webhook's own defaulting had written.
#
# THE FIXTURE BELOW STAYS AS IT IS, and not because of that refusal. `inactive` makes a queue refuse
# to admit through a documented state the reconciler maintains, while the accelerated type's
# infeasibility would now rest on an admission rule that has just changed. A fixture whose property
# rests on a rule is one the next change to that rule can silently empty out.
#
# A CPU-ONLY type is admitted (no card count is defaulted, so no ceiling applies), and `inactive`
# is what makes its queue refuse to admit: the InstanceType reconciler holds the backing
# ClusterQueue, which reports Active=False with a Hold stop policy. That is a supported, documented
# state rather than a broken fixture -- the group can never reserve quota, and the sibling can.
#
# It is created and THEN patched rather than created inactive, because create-then-hold is the
# sequence this was measured on.
k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: $IT_UNPLACEABLE
spec:
  displayName: case-68 held
  acceleratable: false
  generalGroup: case68-held
  os: linux
  arch: amd64
  localStorage: 1Gi
  unitResources:
    cpu: "1"
    ram: 1Gi
YAML
sleep 10
k patch instancetype "$IT_UNPLACEABLE" --type=merge -p '{"spec":{"inactive":true}}' >/dev/null 2>&1
sleep 10

apply_deployment "$MD" "$(two_roles_two_types)"

two_groups=no
if wait_for "$SETTLE" group_count_is "$MD" 2; then
  two_groups=yes
  record PASS "two types are two groups" "$(group_names "$MD" | wc -l | tr -d ' ') distinct group names"
else
  record FAIL "two types are two groups" "saw [$(group_names "$MD" | tr '\n' ' ')]"
fi

# THE TWO ROWS BELOW MEASURE NOTHING IF THE SHAPE ABOVE DID NOT FORM, and saying so is the point of
# the NO-READ status. "Two groups compose two workloads" against a deployment that produced one
# group is a reading about a different situation, and counting it as a second failure would report
# one cause twice.
wls=""
if [ "$two_groups" != yes ]; then
  record NO-READ "two groups compose two workloads" "the two-group shape never formed"
  record NO-READ "no role admitted while one group cannot be placed" "the two-group shape never formed"
  record NO-READ "the deployment does not claim quota it half has" "the two-group shape never formed"
else
  wls="$(deployment_workloads "$MD" | tr '\n' ' ')"
  n_wl="$(echo "$wls" | wc -w | tr -d ' ')"
  if [ "$n_wl" = 2 ]; then
    record PASS "two groups compose two workloads" "$wls"
  else
    record FAIL "two groups compose two workloads" "saw $n_wl: [$wls]"
  fi

  # NO ROLE ADMITTED -- and the control below is what makes this mean anything.
  sleep 30
  if [ "$(admitted_count)" = 0 ]; then
    record PASS "no role admitted while one group cannot be placed" "0 admitted workloads"
  else
    record FAIL "no role admitted while one group cannot be placed" "$(admitted_count) admitted"
  fi

  # AND THE DEPLOYMENT MUST NOT CLAIM IT HOLDS QUOTA. This row exists because this cluster case
  # found the opposite: with one group's queue held, the condition read Reserved with a message
  # naming the deployment's whole replica count -- wrong in both halves at once. The cause was
  # reading ONE Workload for a deployment that has one per group, and half a deployment holding
  # quota is not the deployment holding quota.
  reason_now="$(k -n "$NS" get modeldeployment "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)"
  if [ "$reason_now" = Reserved ]; then
    record FAIL "the deployment does not claim quota it half has" \
      "reason=Reserved while one group's queue is held"
  else
    record PASS "the deployment does not claim quota it half has" "reason=${reason_now:-none}"
  fi
fi

# THE CONTROL. Without it the row above is true whether or not the joint check exists: both groups
# being unplaceable would satisfy it, and so would an operator that admits nothing at all.
apply_deployment "$MD_CONTROL" "$(one_role)"
if wait_for "$SETTLE" admitted_count_is_not 0; then
  record PASS "control: the feasible role alone IS admitted" "the refusal above is the barrier's"
else
  record FAIL "control: the feasible role alone IS admitted" \
    "nothing is admitted even alone, so the row above says nothing about the barrier"
fi
k -n "$NS" delete modeldeployment "$MD_CONTROL" --ignore-not-found --wait=true >/dev/null 2>&1

# ------------------------------------------------------------------------- Phase C: the bound.
#
# BACK-DATING THE STAMP RATHER THAN WAITING THE BOUND OUT. The bound is measured from the check's
# own LastTransitionTime, which is durable on the Workload, so moving that stamp is the same input
# the clock would have produced half an hour later. What this exercises is the operator's
# comparison against the stamp; that Kueue stamps it correctly is Kueue's and is not asserted here.
back="$(date -u -v-2H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '2 hours ago' '+%Y-%m-%dT%H:%M:%SZ')"
parked_any=no
for wl in $wls; do
  idx=0
  for name in $(k -n "$NS" get workload "$wl" \
      -o jsonpath='{range .status.admissionChecks[*]}{.name}{"\n"}{end}' 2>/dev/null); do
    case "$name" in
      gpustack-model-deployment-joint)
        k -n "$NS" patch workload "$wl" --subresource=status --type=json \
          -p '[{"op":"replace","path":"/status/admissionChecks/'"$idx"'/lastTransitionTime","value":"'"$back"'"}]' \
          >/dev/null 2>&1 && parked_any=yes
        ;;
    esac
    idx=$((idx + 1))
  done
done

if [ "$two_groups" != yes ]; then
  # Upstream, not a property of the bound: with no two-group shape there is nothing for the barrier
  # to hold and therefore nothing for the bound to fire on.
  record NO-READ "the bound parks the set" "the two-group shape never formed"
  record NO-READ "the deployment reports it is parked" "the two-group shape never formed"
  record NO-READ "the message names what clears it" "the two-group shape never formed"
elif [ "$parked_any" != yes ]; then
  # This one IS about the barrier: the workloads exist and this controller's check is not on them,
  # which is a real observation rather than a missing precondition.
  record FAIL "the bound parks the set" \
    "this controller's check is on none of [$wls] -- the queue does not reference it"
  record NO-READ "the deployment reports it is parked" "no check to back-date"
  record NO-READ "the message names what clears it" "no check to back-date"
else
  # The verdict is spec.active going false, NOT the Workload disappearing: a deleted Workload is
  # composed again by Kueue from the Pods that are still there, so a delete removes one turn of the
  # loop and nothing else.
  any_deactivated() {
    local wl a
    for wl in $wls; do
      a="$(k -n "$NS" get workload "$wl" -o jsonpath='{.spec.active}' 2>/dev/null)"
      [ "$a" = false ] && return 0
    done

    return 1
  }

  if wait_for "$SETTLE" any_deactivated; then
    record PASS "the bound parks rather than deletes" "a workload reports spec.active=false"
  else
    record FAIL "the bound parks rather than deletes" "no workload was deactivated within ${SETTLE}s"
  fi

  # THE FLIP AND THE REPORT COME FROM DIFFERENT CONTROLLERS, so the wait above does not cover this
  # one. spec.active is patched by the joint-admission controller; the deployment's condition is
  # computed a ModelDeployment reconcile later, from that same flag plus the marker the barrier left
  # on the check. Reading it the instant the Workload flips samples a status the deciding pass has
  # not written yet -- measured, it returns the Pending the barrier had published before parking,
  # which reads exactly like a barrier that never parked at all.
  wait_for "$SETTLE" md_quota_reason_is "$MD" Parked

  msg="$(k -n "$NS" get modeldeployment "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].message}' 2>/dev/null)"
  reason="$(k -n "$NS" get modeldeployment "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)"
  if [ "$reason" = Parked ] && [ -n "$msg" ]; then
    record PASS "the deployment reports it is parked" "reason=Parked"
  else
    record FAIL "the deployment reports it is parked" "reason=[${reason:-none}] message=[${msg:-none}]"
  fi
  case "$msg" in
    *re-apply*) record PASS "the message names what clears it" "an identical re-apply is called out" ;;
    *) record FAIL "the message names what clears it" "[${msg:-none}]" ;;
  esac
fi

# ------------------------------------------------------------- Phase E: the barrier OPENS.
#
# EVERY PHASE ABOVE MEASURES THE BARRIER CLOSING, and a barrier that never opens passes all of them.
# Opening is the other half of this feature, and it is the half whose trigger lives on a DIFFERENT
# object: a held group stays Pending until its SIBLING reserves quota, and that reservation is
# written to the sibling's workload. The held group is judged again only if something watches across
# the pair.
#
# IT NEEDS ITS OWN DEPLOYMENT. Phase C deactivated the one above, and a deactivated workload is never
# admitted again whatever the cluster does -- running this on it would measure the park, not the
# barrier.
#
# THE ORDER IS THE ASSERTION, AND THE ORDER HAS TO BE ESTABLISHED RATHER THAN ASSUMED. The sibling
# must arrive DEMONSTRABLY LATER: one group reserves and is confirmed to be sitting held, and only
# then does the other become placeable. An assertion that merely reads the end state cannot tell a
# working watch from two groups reserving in the same scheduling round, which is exactly how the
# missing watch survived a cluster run. So the precondition below is reserved=1 admitted=0 and not
# "nothing is admitted": the latter is also true before anything has reserved at all, and from there
# releasing the type reproduces the coincidence instead of ruling it out.
k -n "$NS" delete modeldeployment "$MD" --ignore-not-found --wait=true >/dev/null 2>&1

apply_deployment "$MD_OPENS" "$(two_roles_two_types)"

opens_held=no
one_reserved_none_admitted() { [ "$(deployment_quota "$MD_OPENS")" = "1/0/2" ]; }
if wait_for "$SETTLE" one_reserved_none_admitted; then
  opens_held=yes
  record PASS "one group reserves and is held while the other cannot place" "reserved=1 admitted=0 of 2"
else
  saw="$(deployment_quota "$MD_OPENS")"
  record FAIL "one group reserves and is held while the other cannot place" \
    "saw reserved/admitted/total = $saw, so the later arrival is not established and the row below would read the end state only"
fi

if [ "$opens_held" != yes ]; then
  # Nothing was ever held, so nothing can be observed opening. Recording a FAIL here would report an
  # upstream cause a second time.
  record NO-READ "the barrier opens when the set becomes feasible" "the set was never held"
else
  # RELEASING THE TYPE IS THE ONLY CHANGE. The held group's own workload is untouched; what moves is
  # its SIBLING's ability to reserve, which is exactly the event the held group has to hear about.
  k patch instancetype "$IT_UNPLACEABLE" --type=merge -p '{"spec":{"inactive":false}}' >/dev/null 2>&1

  both_admitted() { [ "$(deployment_admitted "$MD_OPENS")" = "2/2" ]; }
  if wait_for "$SETTLE" both_admitted; then
    record PASS "the barrier opens when the set becomes feasible" "2/2 admitted"
  else
    saw="$(deployment_admitted "$MD_OPENS")"
    record FAIL "the barrier opens when the set becomes feasible" \
      "$saw after ${SETTLE}s: the group that had already reserved was never judged again, so the barrier closed and stayed closed"
  fi
fi

echo
echo "== case-68: two groups, admitted as a set =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

# THE NO-READ COUNT IS STATED WHETHER OR NOT ANYTHING FAILED. A run that passed every row it could
# measure, while several rows measured nothing, is not the same result as a clean run -- and the two
# are indistinguishable from an exit code.
if [ "$NOREADS" -ne 0 ]; then
  echo
  echo "${NOREADS} row(s) had NO READING: a precondition did not hold, so they neither passed nor"
  echo "failed. Treat them as unmeasured, not as covered."
fi

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n ${NS} get modeldeployment ${MD} -o yaml"
  echo "  kubectl -n ${NS} get workloads.kueue.x-k8s.io -o wide"
  exit 1
fi

if [ "$NOREADS" -ne 0 ]; then
  echo "case-68: every row that could be measured PASSED, but ${NOREADS} were not measured"
  exit 0
fi
echo "all case-68 checks PASS"
