#!/usr/bin/env bash
#
# CASE 51 — Every P/D refusal fires, from the layer that owns it, and a replicas change replaces
#           nobody
#   (NON-MUTATING for the refusals, which are server-side dry-runs; one short-lived deployment for
#    the convergence rows)
#
#   case-51.sh <NS>
#
# Goal:        CASE 45 pins the single-role admission surface. This pins the rules that arrived with
#              several roles, and it is built on the same distinction: not that a bad manifest is
#              rejected, but WHICH layer rejects it and WHAT the message says.
#
#                schema   — the role name's PodSetReference pattern, the closed `kind` enum, and
#                           uniqueness, which `roles` gets from being a list-map keyed on `name`;
#                webhook  — the count, one-instanceType, and the kind combinations;
#                controller — what a replicas change does to the replicas already running, which is
#                             a convergence rather than a refusal.
#
#              THE TRAP IS CASE 45'S, AND IT IS WORSE HERE. Every manifest below carries several
#              roles, so a mistake in the SHARED part of the manifest refuses all of them — and a
#              row-per-rule table of refusals reads identically whether the rules exist or not. So
#              row 0 is a two-role deployment that must be ACCEPTED, and every refusal row asserts a
#              fragment of the operator's own wording rather than the bare fact of a rejection.
#
#              THE ACCEPTED BASELINE ALSO CARRIES THE HEADLINE. Two roles being accepted at all is
#              the rule this whole spec exists to lift: the single-role version refused them by
#              name, and case-45's own table has a row that says so.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an operator
#              image carrying the multi-role ModelDeployment. NO GPU is needed and no
#              KVCachePoolBinding has to exist: nothing here schedules a replica, and the Binding is
#              only resolved by the controller.
#
#              NO ROW READS THE CLUSTER ANY MORE. Every rule this file asserts is answered from the
#              submitted object, so no row's outcome depends on what a pool's flavors happen to pin
#              or on whether a cache has caught up.
#
# Inputs:      All real, nothing mocked. `--dry-run=server` runs the schema and the webhook and
#              persists nothing. The convergence rows create one ModelDeployment and delete it again.
#
# Deferred:    Whether each replica's group is then ADMITTED — one Workload each — is case-49's,
#              and whether a short pool leaves both roles queued is case-50's. This file stops at
#              the operator's own writes, which is what it can assert without an engine.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-51.sh <NS>" >&2
  exit 2
fi

BINDING="${E2E_MD_BINDING:-case51-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-51] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# A SECOND InstanceType is what the one-instanceType row needs, and it may not exist: a CPU-only
# cluster materializes exactly one pool. The row reports SKIP rather than inventing a name, because
# a name no InstanceType carries would be refused by the CONTROLLER for not resolving, and this row
# is about the WEBHOOK refusing two of them.
IT2="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[1].metadata.name}' 2>/dev/null)"

# Emit a manifest whose roles block is supplied whole by the caller. Unlike case-45's, the roles are
# the variable here: every rule below is about the SET of roles rather than about one role's fields.
manifest() {
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_NAME:-case51-probe}
  namespace: ${NS}
spec:
  engine:
    name: ${ENGINE:-vllm}
    version: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
${1}
YAML
}

# Two plain roles, prefill and decode, on one instanceType. The shape every row below varies from.
#
# The explicit image is plumbing, not part of any rule: a CPU-only InstanceType has observed no
# accelerator, so the operator can synthesize no engine image and the CONTROLLER would refuse the
# rebuild row for a reason none of the admission rows are about. Nothing asserted here runs.
two_roles() {
  cat <<YAML
  - name: prefill
    kind: prefill
    instanceType: ${IT}
    replicas: 1
    image: ${IMAGE}
    command: ["/pause"]
  - name: decode
    kind: decode
    instanceType: ${IT}
    replicas: 1
    image: ${IMAGE}
    command: ["/pause"]
YAML
}

# Assert that a roles block is refused AND that the refusal quotes $2. A refusal carrying somebody
# else's message is a FAIL: it means the manifest tripped a rule this row is not about.
refuses() {
  local check="$1" want="$2" roles="$3" out
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  # An EMPTY $out must not pass: deleting anything from "" leaves "", so a `-z` test on the stripped
  # string is TRUE when the command produced nothing at all. See case-45 for the same guard.
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"

    return 0
  fi
  case "$out" in
    *"$want"*) record PASS "$check" "refused naming: ${want}" ;;
    *) record FAIL "$check" "refused with the wrong message, or accepted: ${out:0:220}" ;;
  esac
}

# THE POSITIVE SIDE OF THE SAME INSTRUMENT, and it exists because one rule this case asserted was
# REMOVED rather than reworded. A deleted refusal must be replaced by the acceptance that took its
# place instead of simply dropped: dropping it leaves nothing at all reporting the day the refusal
# returns, and a rule that comes back silently is exactly what a refusal suite is for.
accepts() {
  local check="$1" roles="$2" out
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "$check" "accepted by both the schema and the webhook" ;;
    *) record FAIL "$check" "refused: ${out:0:220}" ;;
  esac
}

# --- row 0: the baseline, and the headline ---

out="$(manifest "$(two_roles)" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
case "$out" in
  *"created (server dry run)"*|*"configured (server dry run)"*)
    record PASS "two roles are ACCEPTED" \
      "the single-role bound is lifted; every refusal below is therefore its own rule" ;;
  *)
    record FAIL "two roles are ACCEPTED" \
      "the baseline was refused, so every row below passes for the wrong reason: ${out:0:220}" ;;
esac

# --- the webhook's rules ---

eleven=""
for i in $(seq 0 10); do
  eleven+="  - name: role-${i}
    instanceType: ${IT}
    replicas: 1
"
done
# THE CAP IS THIS PROJECT'S, and the refusal has to say whose it is. It used to be Kueue's --
# Workload.spec.podSets maxItems, binding while every role was one PodSet of a single Workload --
# and a message still naming Kueue would send a reader to look at a Workload their roles no longer
# become, since each replica now carries its own.
refuses "eleven roles are refused naming THIS PROJECT's cap" \
  "a shape this operator does not serve" "$eleven"

# BY THE SCHEMA, and the attribution is the finding rather than a detail. `roles` is a list-map keyed
# on `name`, so the API server rejects the duplicate during validation and the webhook's own rule --
# which names the merge, and reads far better -- never runs. Asserting that better wording here would
# fail against a correct operator.
refuses "two roles sharing a name are refused BY THE SCHEMA" \
  'Duplicate value: {"name"' \
  "  - name: worker
    instanceType: ${IT}
    replicas: 1
  - name: worker
    instanceType: ${IT}
    replicas: 1"

# THIS ROW WAS A REFUSAL AND IS NOW AN ACCEPTANCE, because the rule behind it was deleted rather
# than reworded. While a role was the admission unit, one Workload carried one queue name and roles
# on two InstanceTypes had nowhere to be admitted together, so the webhook refused the shape up
# front. A replica is the unit now: each becomes its own group, and the joint barrier admits or
# holds the whole set across however many types it spans. Several types in one deployment is the
# premise of that barrier rather than a state anything rejects.
#
# A SECOND REAL InstanceType IS REQUIRED NOW, WHERE THE REFUSAL NEEDED NONE. The deleted rule was
# answered from the submitted object alone and ran before anything read the cluster, so a synthetic
# name exercised it. With that rule gone the object reaches the rules that DO read the cluster, and
# a name resolving to nothing is refused for not existing -- which would pass a careless row for
# entirely the wrong reason. So this row skips rather than substituting a fabricated name.
if [ -n "${IT2:-}" ]; then
  accepts "two instanceTypes are ACCEPTED, which is the joint barrier's whole premise" \
    "  - name: prefill
    instanceType: ${IT}
    replicas: 1
  - name: decode
    instanceType: ${IT2}
    replicas: 1"
else
  record SKIP "two instanceTypes are ACCEPTED, which is the joint barrier's whole premise" \
    "needs a second real InstanceType and this cluster materialized one. NOT closed by a synthetic name, which is now refused for not existing; CASE 68 covers the same premise by CREATING its second type"
fi

refuses "kind: server beside another kind is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: server
    instanceType: ${IT}
    replicas: 1
  - name: prefill
    kind: prefill
    instanceType: ${IT}
    replicas: 1"

ENGINE=sglang refuses "a kind the engine cannot be told is refused NAMING THE ENGINE" \
  "has no rendering term for kind" "$(two_roles)"

# --- the schema's rules, which run BEFORE the webhook and must not be confused with it ---

refuses "a role name that is not a PodSetReference is refused BY THE SCHEMA" \
  "spec.roles[0].name" \
  "  - name: Prefill
    instanceType: ${IT}
    replicas: 1"

refuses "a kind outside the enum is refused BY THE SCHEMA" \
  "spec.roles[0].kind" \
  "  - name: prefill
    kind: router
    instanceType: ${IT}
    replicas: 1"

# --- the controller: a replicas change replaces nobody, a role rename replaces everybody ---

REBUILD_MD=case51-rebuild

# THE DECLARED TOTAL IS ONE AND STAYS ONE, whatever the replica counts are. Each replica is a Kueue
# group of its own, so the number a Pod declares is a property of the group it is alone in rather
# than of the deployment it belongs to -- which is precisely what lets a replicas change leave every
# living replica untouched. A reading of anything else here means a Pod joined a group expecting a
# sibling that will never arrive, and Kueue composes nothing for it.
DECLARED_TOTAL=1
# Deleted without waiting, then any Workload still holding the replicas is released by hand. Kueue
# keeps a finalizer on every Pod of a serving group and drops it only when that Workload goes, so a
# cleanup that blocks on the deployment would block for as long as the caller allows if the operator
# ever stopped deleting it -- turning somebody else's regression into this case's timeout.
cleanup() {
  local wl
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$REBUILD_MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  # One list call: this trap runs on every exit, pass or fail, so a get per Workload adds latency to
  # every invocation of the case on a busy namespace.
  local row
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    case "${row#*=}" in
      *"${REBUILD_MD}-"*)
        kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
          --ignore-not-found --wait=false >/dev/null 2>&1
        ;;
    esac
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].name}{"\n"}{end}' 2>/dev/null)
EOF
}
trap cleanup EXIT

# THE TOTAL IS OBSERVED ALONGSIDE THE UIDS, not instead of them. A Pod declaring anything but one
# has joined a group waiting for a sibling that will never come, and Kueue composes no Workload for
# it -- a state that looks like nothing at all from the outside, and one that counting Pods or
# reading UIDs would both pass straight through.
totals() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${REBUILD_MD}" \
    -o jsonpath='{range .items[*]}{.metadata.annotations.kueue\.x-k8s\.io/pod-group-total-count}{"\n"}{end}' \
    2>/dev/null | sort -u | tr '\n' ' '
}

# Pod name=UID for one role, sorted. THE UID IS THE OBSERVABLE THIS HALF OF THE CASE RESTS ON: a
# replica that stayed and one replaced by an identical render are the same Pod to every other
# reading -- same name pattern, same spec, same labels -- and differ only in the identity the
# cluster assigned. A fake client leaves it empty, which is why this cannot be asserted below e2e.
role_uids() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${REBUILD_MD},app.kubernetes.io/component=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort | tr '\n' ' '
}

# The UIDs of one role that are STILL PRESENT out of a recorded set, as a count.
surviving() {
  local before="$1" role="$2" now n=0 e
  now="$(role_uids "$role")"
  for e in $before; do
    case " $now " in *" $e "*) n=$((n + 1)) ;; esac
  done
  echo "$n"
}

# KEPT, unlike a dry-run row's: every rebuild row below depends on this object existing, so a refusal
# here would otherwise surface as three timeouts with three wrong diagnoses instead of one message.
REBUILD_APPLY="$(MD_NAME="$REBUILD_MD" manifest "$(two_roles)" | kubectl apply -f - 2>&1)"

for _ in $(seq 1 30); do
  [ "$(totals)" = "${DECLARED_TOTAL} " ] && break
  sleep 2
done
if [ "$(totals)" = "${DECLARED_TOTAL} " ]; then
  record PASS "every replica declares a group total of one" \
    "both roles' replicas declare ${DECLARED_TOTAL}: each is a group of itself"
else
  record FAIL "every replica declares a group total of one" \
    "observed totals: '$(totals)'; apply said: ${REBUILD_APPLY:0:200}"
fi

# Recorded BEFORE the patch, which is the only moment they can be recorded: the whole question is
# whether these exact identities survive it.
PREFILL_BEFORE="$(role_uids prefill)"
DECODE_BEFORE="$(role_uids decode)"

# The role object's closing brace belongs to the caller below, NOT here. Carrying one in this
# fragment too produced "command":["/pause"]}} in every patch this case has ever sent, which the API
# server rejects while DECODING -- before any webhook or controller sees it. The row that depends on
# the patch therefore could not pass, and had never passed.
TPL='"image":"'"$IMAGE"'","command":["/pause"]'
# CAPTURED, NOT DISCARDED. This patch used to send both streams to /dev/null, and a REFUSED patch
# then produced exactly what a slow scale produces: nothing happens, the poll below runs out, and
# the row reports "prefill never reached 2 replicas" -- a symptom, with its cause thrown away at the
# only moment it was available. Capturing it is what turned that symptom into the decoding error
# above, which is the whole reason the brace was findable at all.
SCALE_PATCH="$(kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai "$REBUILD_MD" --type=merge \
  -p '{"spec":{"roles":[{"name":"prefill","kind":"prefill","instanceType":"'"$IT"'","replicas":2,'"$TPL"'},{"name":"decode","kind":"decode","instanceType":"'"$IT"'","replicas":1,'"$TPL"'}]}}' \
  2>&1)"

SCALED=no
BAD_TOTAL=no
for _ in $(seq 1 45); do
  t="$(totals)"
  # ANY total but one is the failure this poll exists to catch, and it is caught by OBSERVING rather
  # than by reasoning: a Pod waiting for a sibling that will never join composes no Workload, and it
  # looks like nothing at all from the outside.
  [ -n "$t" ] && [ "$t" != "${DECLARED_TOTAL} " ] && BAD_TOTAL=yes
  [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${REBUILD_MD},app.kubernetes.io/component=prefill" \
    --no-headers 2>/dev/null | grep -c . || true)" = 2 ] && { SCALED=yes; break; }
  sleep 2
done

# THE INVERTED ROW. Until this spec a replicas change moved a number every member of the deployment
# carried, so the group had to be torn down and recomposed and every replica went with it. The
# assertion is now the opposite one, and it is measured on UIDs because nothing else can tell a
# replica that STAYED from one replaced by an identical render.
if [ "$SCALED" != yes ]; then
  record FAIL "a replicas change leaves every living replica alone" \
    "prefill never reached 2 replicas; totals read '$(totals)'; the patch said: ${SCALE_PATCH:0:220}"
else
  kept_p="$(surviving "$PREFILL_BEFORE" prefill)"
  kept_d="$(surviving "$DECODE_BEFORE" decode)"
  want_p="$(printf '%s\n' $PREFILL_BEFORE | grep -c . || true)"
  want_d="$(printf '%s\n' $DECODE_BEFORE | grep -c . || true)"
  # THE SIBLING ROLE IS HALF THE ROW. A scale that rebuilt only the role it names would still be a
  # regression, and a check reading prefill alone would call it a pass.
  if [ "$kept_p" = "$want_p" ] && [ "$kept_d" = "$want_d" ]; then
    record PASS "a replicas change leaves every living replica alone" \
      "all ${want_p} prefill and ${want_d} decode replicas kept their UIDs across the scale; the new replica is created beside them"
  else
    record FAIL "a replicas change leaves every living replica alone" \
      "prefill kept ${kept_p}/${want_p} UIDs, decode kept ${kept_d}/${want_d}: a scale rebuilt replicas it was not asked to touch"
  fi
fi

if [ "$BAD_TOTAL" = no ]; then
  record PASS "no replica ever declared a total other than one" \
    "polled every 2s across the scale; a window shorter than the interval is not visible to this row"
else
  record FAIL "no replica ever declared a total other than one" \
    "sampled a total other than ${DECLARED_TOTAL}, which is a group waiting for a member that will never arrive"
fi

# THE CONTROL, AND WITHOUT IT THE ROW ABOVE PROVES NOTHING. "The UIDs did not change" is also what a
# broken observation reports -- a role_uids that silently returned the same string twice, a patch
# that never applied -- so the same instrument has to be shown reporting the other value. Renaming a
# role derives a new group name for every one of its ordinals, so every replica of it IS replaced,
# while the sibling role is still expected to sit untouched.
RENAMED_BEFORE="$(role_uids decode)"
kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai "$REBUILD_MD" --type=merge \
  -p '{"spec":{"roles":[{"name":"prefill","kind":"prefill","instanceType":"'"$IT"'","replicas":2,'"$TPL"'},{"name":"decoder","kind":"decode","instanceType":"'"$IT"'","replicas":1,'"$TPL"'}]}}' \
  >/dev/null 2>&1

RENAME_DONE=no
for _ in $(seq 1 45); do
  [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${REBUILD_MD},app.kubernetes.io/component=decoder" \
    --no-headers 2>/dev/null | grep -c . || true)" = 1 ] && { RENAME_DONE=yes; break; }
  sleep 2
done

if [ "$RENAME_DONE" != yes ]; then
  record SKIP "the control: a role rename DOES replace that role's replicas" \
    "the renamed role never reached its replica, so the instrument was never shown reporting a change"
else
  still="$(surviving "$RENAMED_BEFORE" decode)"
  if [ "$still" = 0 ]; then
    record PASS "the control: a role rename DOES replace that role's replicas" \
      "none of the old decode UIDs survived, so the reading above is the instrument answering rather than failing to look"
  else
    record FAIL "the control: a role rename DOES replace that role's replicas" \
      "${still} old decode UID(s) still present: this instrument cannot tell a replacement from a survivor, so the row above is unproven"
  fi
fi

# Results.
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-51] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-51] all checks passed"
