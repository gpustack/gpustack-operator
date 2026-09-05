#!/usr/bin/env bash
#
# CASE 49 — A multi-role ModelDeployment becomes ONE Kueue Workload with one PodSet per role, and
#           deleting it completes   (MUTATING, self-recovering)
#
#   case-49.sh <NS>
#
# Goal:        This is the mechanism a prefill/decode deployment rests on, and it is the half that
#              CANNOT be asserted anywhere but on a live Kueue: the operator writes labels and
#              annotations, and KUEUE decides what they compose into. A unit test can prove the
#              operator stamped `pod-group-name`; only this can prove Kueue read it and built one
#              Workload whose PodSets are the roles.
#
#              THE SECOND DEPLOYMENT IS THE ONE THAT MATTERS. Two roles that differ ONLY in name
#              render identical Pod specs, and Kueue derives a role hash from a Pod spec's SHAPE when
#              the annotation is absent -- so without it those two roles collapse into ONE PodSet
#              holding both their replicas, and per-role counting, per-role flavor assignment and
#              per-role status all disappear with nothing erroring. A case that only ran
#              prefill-and-decode would pass either way, because those two differ in their rendered
#              engine arguments. So this file runs both.
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
#              here, so it may be absent. Each role carries an explicit `template.image` because a
#              CPU-only InstanceType has observed no accelerator and the operator can therefore
#              synthesize no engine image -- the replicas are placeholders, and no assertion reads
#              anything they run. Override with E2E_MD_IMAGE, the InstanceType with
#              E2E_MD_INSTANCE_TYPE.
#
# Expected:    Four replicas exist; exactly ONE Workload owns them; its PodSets are `prefill=2` and
#              `decode=2`; two roles differing only in name stay two PodSets; and deleting a group
#              removes the deployment, its replicas and its Workload within the bound.
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
  local md="$1" wl
  wl="$(group_workload "$md")"
  [ -n "$wl" ] && kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
    --ignore-not-found --wait=false >/dev/null 2>&1

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
  engine: vllm
  engineVersion: "0.11.0"
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
  printf '    template:\n      image: %s\n      command: ["/pause"]\n' "$IMAGE"
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
  record FAIL "Kueue composes ONE Workload for the group" \
    "no Workload in ${NS} owns any of this deployment's Pods"
else
  # ONE, not "at least one": four replicas each becoming their own Workload is exactly the pre-group
  # behaviour this design replaces, and it would satisfy an "a Workload exists" check. Counted over
  # the deployment's OWN Pods rather than over the namespace, which may hold a neighbour's.
  n=0
  for w in $(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
    owners="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$w" \
      -o jsonpath='{range .metadata.ownerReferences[*]}{.name}{"\n"}{end}' 2>/dev/null)"
    case "$owners" in *case49-pd-*) n=$((n + 1)) ;; esac
  done
  if [ "$n" = 1 ]; then
    record PASS "Kueue composes ONE Workload for the group" \
      "${WL}, owning the replicas with no controller reference"
  else
    record FAIL "Kueue composes ONE Workload for the group" \
      "${n} Workloads own this deployment's Pods; four independent ones is the pre-group shape"
  fi

  sets="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$WL" \
    -o jsonpath='{range .spec.podSets[*]}{.name}={.count}{" "}{end}' 2>/dev/null)"
  if [ "${sets#*prefill=2}" != "$sets" ] && [ "${sets#*decode=2}" != "$sets" ]; then
    record PASS "its PodSets are the ROLES, counted per role" "podSets: ${sets}"
  else
    record FAIL "its PodSets are the ROLES, counted per role" "podSets: ${sets}"
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
# and the match happens here, which is the shape case-50's group_workload already uses.
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
    "the deployment, its replicas and its Workload are all gone within ${DELETE_BOUND}s"
else
  record FAIL "deleting the group completes" \
    "still present after ${DELETE_BOUND}s: phase '$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case49-pd -o jsonpath='{.status.phase}' 2>/dev/null)', $(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case49-pd --no-headers 2>/dev/null | wc -l | tr -d ' ') replica(s) held by Kueue's finalizer, $(orphan_workloads) Workload(s) still owned by them"
fi
force_release case49-pd

# --- deployment 2: two roles that differ ONLY in name ---

apply_md case49-twins "$(role_block left '' 2)
$(role_block right '' 2)"

if ! wait_pods case49-twins 4; then
  record FAIL "two identically-shaped roles stay two PodSets" \
    "the group never reached four replicas, so Kueue composed nothing to inspect. apply said: ${APPLY_OUT:0:200}"
else
  WL2="$(wait_workload case49-twins)"
  if [ -z "$WL2" ]; then
    record FAIL "two identically-shaped roles stay two PodSets" \
      "no Workload owns this deployment's Pods"
  else
    sets2="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$WL2" \
      -o jsonpath='{range .spec.podSets[*]}{.name}={.count}{" "}{end}' 2>/dev/null)"
    count2="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$WL2" \
      -o jsonpath='{.spec.podSets[*].name}' 2>/dev/null | wc -w | tr -d ' ')"
    # THE FAILING SHAPE IS ONE PodSet OF FOUR, and it is a legal Workload. Asserting the count is what
    # tells it from two of two; asserting only that the names are there would pass against a single
    # PodSet named after whichever role Kueue saw first.
    if [ "$count2" = 2 ] && [ "${sets2#*left=2}" != "$sets2" ] && [ "${sets2#*right=2}" != "$sets2" ]; then
      record PASS "two identically-shaped roles stay two PodSets" \
        "podSets: ${sets2} - the role-hash annotation is what prevents the collapse"
    else
      record FAIL "two identically-shaped roles stay two PodSets" \
        "podSets: ${sets2} - one PodSet of four is the collapse this design exists to prevent"
    fi
  fi
fi

# --- deferred: the serving half ---

# THESE TWO ROWS ARE A LEDGER ENTRY, so each states what does NOT close it. A gap recorded as only
# "deferred" gets closed by the first thing that looks like coverage.
record SKIP "both roles reach status.roles[].ready == replicas" \
  "needs an engine that serves, which needs an accelerator. NOT closed by: rerunning this case on a CPU-only cluster; a unit test, since the claim is a container starting; or passing E2E_MD_IMAGE a real engine image, since the engine needs the hardware it was built for"
record SKIP "status.endpoint answers an inference request" \
  "same requirement, and the same three things do not close it. status.roles[].assignedFlavor is unreachable here for a third reason: it reports the flavor of a role's ACCELERATOR credits, and a CPU-only pool quotes one for cpu only"

# Results.
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-49] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-49] all checks passed (the serving half is deferred; see the SKIP rows)"
