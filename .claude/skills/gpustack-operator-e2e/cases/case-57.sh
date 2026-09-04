#!/usr/bin/env bash
#
# CASE 57 — The refusals, on a live API server, each with the message its fix depends on
#   (MUTATING, self-recovering)
#
#   case-57.sh <NS>
#
# Goal:        Every refusal in this webhook exists because guessing would produce a container that
#              starts normally and does not use the cache - a result invisible from outside the Pod.
#              The unit suite proves the branches; this case proves the two things it cannot: that a
#              real API server delivers the refusal at all (a webhook that never gets called refuses
#              nothing), and that what reaches the person running kubectl NAMES what to change.
#
#              So every check here asserts a SUBSTRING of the message, never just a non-zero exit. A
#              refusal a reader cannot act on is barely better than a silent one, and "the request was
#              rejected" is exactly that.
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and one KVCachePoolBinding, so it needs no pool to pre-exist. Never auto-skips.
# Inputs:      one deliberately malformed real Pod per refusal, each breaking exactly one rule so a
#              message naming the wrong subject is a failure rather than a near miss. Nothing is
#              mocked.
# Expected:    each submission below is rejected, and each message names the annotation, container,
#              key or object the author has to look at.
#
#              SKIPS: none. This case has no conditional half - every check either runs or FAILS, and
#              a precondition it cannot meet is recorded as a failure rather than passed over. The
#              footer counts any SKIP separately from the passes, so a skipped check can never be read
#              off the PASS count.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It also RESTORES multi-tenancy on the backend before any delete, because the state the
#              last check creates deliberately - multi-tenancy turned OFF on a live backend - is the
#              state that makes the pool and the backend
#              undeletable, and it ABORTS the deletion if that restore does not converge - the objects
#              are repairable only while they are not being deleted. EXERCISED 2026-09-04: the restore
#              ran and converged ("multi-tenancy restored after ~30s; the pool is deletable again").
#              Before that it sat unexercised because it was written after a run had already wedged two
#              objects, and the cluster went to another window before it could be run - and a review
#              round found the first version of it deleting anyway when the restore failed. Worth
#              keeping: an unexercised path was already wrong before it ever ran once.
#              It changes no shared baseline - every object it touches is one it created.
#
# EXERCISED 2026-09-04 (second host) on a single-node docker-desktop cluster, arm64, k8s v1.36.1,
# against an operator built from this branch: 15 checks, all passing, and the
# restore converged. Still NOT exercised: the marker moving ahead of the destructive patch. That is
# invisible on a run which completes - it shows only on one interrupted between those two lines, and
# every run so far has completed.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster: 15 checks, all passing. Two were added after the
# first run and one after that:
#
#   shell wrapper (both spellings) - command+args are concatenated by Kubernetes, so a check written
#     against either field alone catches one and misses the other.
#   a pool whose ledger is gone   - reached by turning multiTenancy off on a live backend, since a
#     create-time invariant does not protect against a later edit. Measured here: QuotaLedgerAvailable
#     went False ~30s after the patch. That number is why the step polls instead of sleeping - any
#     plausible fixed sleep would have been too short, and the check would have reported green while
#     its precondition was unmet. THAT SAME RUN LEFT THE POOL AND THE BACKEND WEDGED for 3h51m on a
#     shared cluster, which is what the restore described under Cleanup now exists to prevent. The
#     underlying product behaviour - a finalizer that cannot finish once the ledger is gone - is
#     filed separately; it is reachable without any of this and is not a test defect.
#
# One earlier failure was this case's own: a sed keyed on the fixture's launch line stopped matching
# when that line changed, so the Pod carried no MOONCAKE_TENANT_ID at all - and the check reported
# "the value was changed", naming a mutation nobody made. It now separates absent from altered.
set -uo pipefail

NS="${1:?usage: case-57.sh <NS>}"
CASE_ID=57
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
# The specialized trap below replaces this one; until it does, multi-tenancy is still on and the
# plain teardown is the correct cleanup.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

# The F4b step at the end turns multi-tenancy OFF on a live backend, and THAT STATE IS WHAT MAKES BOTH
# OBJECTS UNDELETABLE: the pool's finalizer teardown reads the tenant ledger, the master no longer
# holds one, and the backend cannot go while the pool is still there. Observed on the shared cluster
# after the first run of this case: a pool and its backend wedged for 3h51m with identical
# deletionTimestamps, until a finalizer was removed by hand.
#
# Patching the flag back once deletion has started does NOT help. The backend is already Deleting, so
# its controller stops reconciling children: the leader Deployment kept its old args (gen=2,
# observed=2), the leader pod never restarted, and 120s of polling showed no movement. It is an
# irreversible transition, not slow convergence.
#
# So the flag is restored BEFORE anything is deleted, and the restore waits for the CONDITION to come
# back rather than for the patch to be accepted - the patch was accepted in the wedged case too.
CASE57_TENANCY_OFF=0

case57_restore_multi_tenancy() {
  [ "$CASE57_TENANCY_OFF" -eq 1 ] || return 0
  kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type=merge \
    -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":true}}}}}' >/dev/null 2>&1
  local i
  for i in $(seq 1 40); do
    if [ "$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
        -o 'jsonpath={.status.conditions[?(@.type=="QuotaLedgerAvailable")].status}' 2>/dev/null)" = True ]; then
      echo "[case-57] multi-tenancy restored after ~$((i * 3))s; the pool is deletable again"
      return 0
    fi
    sleep 3
  done
  echo "[case-57] WARNING: QuotaLedgerAvailable did not return to True within 120s."
  return 1
}

# Deletion is the step that makes the damage permanent, so a failed restore STOPS it.
#
# The first version of this teardown called the restore and then deleted regardless. That is the
# same mistake the restore exists to prevent: while the objects are merely misconfigured they can
# still be repaired by hand, and once they are Deleting they cannot - the controller stops
# reconciling children, so putting multi-tenancy back no longer reaches the running master.
#
# So a failed restore leaves everything in place and says what to do. Objects left behind on a shared
# cluster are a cost; objects left behind that NOBODY can remove are a different thing entirely.
case57_teardown() {
  local rc=$?
  if ! case57_restore_multi_tenancy; then
    echo "[case-57] NOT DELETING ANYTHING. multi-tenancy is still off on ${BACKEND}, and deleting \
now is what makes the pool and the backend unremovable. They are repairable exactly while they are \
not being deleted. To finish by hand: patch the backend's \
spec.connection.managed.leader.multiTenancy back to true, wait for KVCachePool ${POOL} to report \
QuotaLedgerAvailable=True, and then delete. Left in place: backend ${BACKEND}, pool ${POOL}, \
namespace ${TEST_NS}."
    # `return` from an EXIT trap does not change the script's status - bash keeps the one that
    # triggered the trap - so a run whose assertions all passed would report success while leaving
    # the cluster misconfigured. exit is what actually sets it.
    exit 1
  fi
  kvi_teardown
  return "$rc"
}
# Replaces the plain kvi_teardown armed above, now that the run is about to reach the step that can
# turn multi-tenancy off.
trap case57_teardown EXIT

# A Pod that is legal except for one thing. Each check below breaks exactly one, so a message that
# names the wrong subject is a failure rather than a near miss.
pod_without() {
  local name="$1" drop="$2" extra="${3:-}"
  {
    echo "apiVersion: v1"
    echo "kind: Pod"
    echo "metadata:"
    echo "  name: ${name}"
    echo "  namespace: ${TEST_NS}"
    echo "  labels:"
    echo '    kvcache.gpustack.ai/inject: "true"'
    echo "  annotations:"
    [ "$drop" != binding ] && echo "    kvcache.gpustack.ai/binding: ${BINDING}"
    [ "$drop" != engine ] && echo "    kvcache.gpustack.ai/engine: vllm"
    [ -n "$extra" ] && echo "$extra"
    echo "spec:"
    echo "  restartPolicy: Never"
    echo "  containers:"
    echo "    - name: engine"
    echo "      image: ${CLIENT_IMAGE}"
    echo '      command: ["python3", "-c", "import time; time.sleep(3600)"]'
  }
}

kvi_refused "the engine annotation is required" \
  "kvcache.gpustack.ai/engine" "$(pod_without r-noengine engine)"

kvi_refused "an unknown engine names the accepted values" \
  "vllm" "$(pod_without r-badengine engine '    kvcache.gpustack.ai/engine: tensorrt')"

kvi_refused "the binding annotation is required" \
  "kvcache.gpustack.ai/binding" "$(pod_without r-nobinding binding)"

kvi_refused "a namespaced binding value is refused" \
  "cross-namespace" "$(pod_without r-nsbinding binding '    kvcache.gpustack.ai/binding: other/chat')"

kvi_refused "a binding absent from the namespace names the namespace" \
  "$TEST_NS" "$(pod_without r-nobind2 binding '    kvcache.gpustack.ai/binding: does-not-exist')"

kvi_refused "the domain annotation is refused rather than ignored" \
  "kvcache.gpustack.ai/domain" "$(pod_without r-domain none '    kvcache.gpustack.ai/domain: someone-elses')"

kvi_refused "a typo under this webhook's prefix is refused" \
  "not one this webhook accepts" "$(pod_without r-typo none '    kvcache.gpustack.ai/bindng: chat')"

kvi_refused "the injection record may not be supplied" \
  "written by this webhook" "$(pod_without r-forged none '    kvcache.gpustack.ai/injected: "{}"')"

# Several containers and none named. The message must list the candidates: without them the author
# has to go and read the manifest they just submitted to find out what to type.
kvi_refused "several containers and none named lists the candidates" \
  "never picks the first" "$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: r-multi
  namespace: ${TEST_NS}
  labels:
    kvcache.gpustack.ai/inject: "true"
  annotations:
    kvcache.gpustack.ai/binding: ${BINDING}
    kvcache.gpustack.ai/engine: vllm
spec:
  restartPolicy: Never
  containers:
    - name: engine
      image: ${CLIENT_IMAGE}
      command: ["python3", "-c", "import time; time.sleep(3600)"]
    - name: logs
      image: ${CLIENT_IMAGE}
      command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML
)"

# A container with neither command nor args. Kubernetes would read the injected args as the whole
# command line and discard the image's CMD, so the message has to name ARGS as the field to fill -
# command is the wrong one, and putting the launch line there overrides the ENTRYPOINT as well.
kvi_refused "a container with neither command nor args names args as the fix" \
  "args" "$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: r-bare
  namespace: ${TEST_NS}
  labels:
    kvcache.gpustack.ai/inject: "true"
  annotations:
    kvcache.gpustack.ai/binding: ${BINDING}
    kvcache.gpustack.ai/engine: vllm
spec:
  restartPolicy: Never
  containers:
    - name: engine
      image: ${CLIENT_IMAGE}
YAML
)"

# A key that selects the mechanism, already set. Merging would leave two sources for one setting.
kvi_refused "an already-configured connector is refused, not merged" \
  "MOONCAKE_CONFIG_PATH" "$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: r-conflict
  namespace: ${TEST_NS}
  labels:
    kvcache.gpustack.ai/inject: "true"
  annotations:
    kvcache.gpustack.ai/binding: ${BINDING}
    kvcache.gpustack.ai/engine: vllm
spec:
  restartPolicy: Never
  containers:
    - name: engine
      image: ${CLIENT_IMAGE}
      command: ["python3", "-c", "import time; time.sleep(3600)"]
      env:
        - name: MOONCAKE_CONFIG_PATH
          value: /mine.json
YAML
)"

# A shell wrapper. Both spellings are submitted, because Kubernetes concatenates command and args and
# they produce an identical process - a check written against either field alone would catch one and
# miss the other, and the one it missed is the shape this suite's own fixtures used.
for spelling in in-command in-args; do
  case "$spelling" in
    in-command) launch='      command: ["/bin/sh", "-c"]
      args: ["vllm serve --model x"]' ;;
    in-args)    launch='      command: ["/bin/sh"]
      args: ["-c", "vllm serve --model x"]' ;;
  esac
  kvi_refused "a shell wrapper is refused (${spelling})" \
    "positional parameters" "$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: r-shell-${spelling}
  namespace: ${TEST_NS}
  labels:
    kvcache.gpustack.ai/inject: "true"
  annotations:
    kvcache.gpustack.ai/binding: ${BINDING}
    kvcache.gpustack.ai/engine: vllm
spec:
  restartPolicy: Never
  containers:
    - name: engine
      image: ${CLIENT_IMAGE}
${launch}
YAML
)"
done

# The counterpart, and it is the one most likely to regress: a key this webhook does NOT write must
# not be a conflict. Refusing over MOONCAKE_TENANT_ID would block the single workaround available to
# somebody running a patched engine that does forward a tenant.
# The env block is appended to the manifest rather than patched into a line of it. A sed keyed on the
# fixture's launch line silently stopped matching when that line changed, and the Pod then carried no
# such variable at all - which the check below reported as "the value was changed", naming a mutation
# nobody had made.
kvi_pod_manifest ok-tenant vllm | \
  sed 's|^      image: .*|&\n      env:\n        - name: MOONCAKE_TENANT_ID\n          value: mine|' | \
  kubectl apply -f - >/dev/null 2>&1
if kvi_wait_for pods ok-tenant '{.metadata.name}' ok-tenant 60 "$TEST_NS" >/dev/null; then
  got_tenant="$(kvi_env ok-tenant MOONCAKE_TENANT_ID)"
  if [ "$got_tenant" = "mine" ]; then
    record PASS "a user-set tenant id is not a conflict" \
      "admitted, and the value is left exactly as its author wrote it"
  elif [ -z "$got_tenant" ]; then
    # Absent and altered call for opposite investigations - one is this case failing to set it up,
    # the other is the webhook overwriting a user's value - so they are never reported as one.
    record FAIL "a user-set tenant id is not a conflict" \
      "the Pod carries no MOONCAKE_TENANT_ID at all, so this case never set one up; that says nothing \
about whether the webhook would have preserved it"
  else
    record FAIL "a user-set tenant id is not a conflict" \
      "admitted, but the value was changed to '${got_tenant}'"
  fi
else
  record FAIL "a user-set tenant id is not a conflict" \
    "the Pod was refused; this webhook does not write that key, so refusing over it blocks the one \
workaround a patched-engine operator has"
fi

# F4b, LAST because it takes the shared pool out of service.
#
# This refusal looked unreachable at first: a KVCachePool is refused at creation when its backend has
# no tenant ledger, so how does a Pod ever meet a pool whose ledger is gone? By the backend being
# changed afterwards - multiTenancy can be flipped to false on a live backend and nothing rejects it.
# A CREATE-TIME INVARIANT DOES NOT PROTECT AGAINST A LATER EDIT, and F4b is the last link of that
# degradation chain rather than a duplicate of the pool's own check.
#
# ── A Binding that exists but is being DELETED ────────────────────────────────────────────────────
# The refusal this covers is the one that needs no mistake: deleting a Binding is routine operations,
# and a plain Pod is not in status.usedBy, so the finalizer protecting declared consumers cannot see
# it. Injected against a Binding whose domain is leaving the ledger, the Pod starts, is stamped, and
# fails every write with TENANT_NOT_REGISTERED. Waiting does not heal it.
#
# THE WINDOW IS HELD, NOT RACED. A test finalizer keeps the object in Deleting for exactly as long as
# this check needs. Deleting and hoping to submit inside the operator's own window would be a bet: a
# pass would be luck and a failure would be indistinguishable from a flake.
# ⛔ Rejected alternative: scaling the Mooncake leader to zero so the operator's finalizer cannot
# converge. The leader is a Deployment this operator reconciles, so it comes straight back - the hold
# would be fighting the controller rather than holding anything.
#
# WHAT THIS PROVES AND WHAT IT DOES NOT. It proves the webhook refuses when Get returns a Binding
# whose deletionTimestamp is set, on a real API server - which is the production condition. It does
# NOT measure how long the operator's own finalizer holds one; that is a property of the pool
# reconciler (releaseKVCachePoolBinding deletes the master entry BEFORE dropping the finalizer, which
# is why the window exists at all) and not of this webhook.
#
# Its OWN Binding, not the shared one. A check that consumes the fixture its neighbours read would
# make them depend on running first.
TERM_BINDING="bind-term"
TERM_HOLD="e2e.gpustack.ai/kvc-terminating-hold"
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${TERM_BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: ${DOMAIN}-term
    blockSize: 16
    dtype: bfloat16
  quotaCeiling: 64Mi
YAML
if ! kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
  record FAIL "a Binding that is being deleted is refused" \
    "the second Binding never reached Ready in 180s, so the terminating state was never entered and \
this check did not run - it says nothing about the refusal in either direction"
else
  # Append rather than replace: the operator's own finalizer must stay, or removing ours would let the
  # object vanish while the reconciler still believes it owns an entry on the master.
  kubectl -n "$TEST_NS" patch kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/metadata/finalizers/-\",\"value\":\"${TERM_HOLD}\"}]" \
    >/dev/null 2>&1
  kubectl -n "$TEST_NS" delete kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" \
    --wait=false >/dev/null 2>&1
  TERM_TS="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" \
    -o 'jsonpath={.metadata.deletionTimestamp}' 2>/dev/null)"
  if [ -z "$TERM_TS" ]; then
    record FAIL "a Binding that is being deleted is refused" \
      "the Binding carries no deletionTimestamp after the delete, so the state this check needs was \
never reached; the hold finalizer did not take"
  else
    kvi_refused "a Binding that is being deleted is refused" "which is being deleted" \
      "$(KVI_BINDING="$TERM_BINDING" kvi_pod_manifest term-probe vllm)"
  fi
  # Release it whatever happened above, and before the teardown runs: a held Binding blocks the
  # namespace, and this one is held by a finalizer only this file knows about.
  kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" -o json 2>/dev/null \
    | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
f=[x for x in d['metadata'].get('finalizers',[]) if x != '${TERM_HOLD}']
print(json.dumps({'metadata':{'finalizers':f}}))" > /tmp/kvc-term-release-${SFX}.json 2>/dev/null
  if [ -s "/tmp/kvc-term-release-${SFX}.json" ]; then
    kubectl -n "$TEST_NS" patch kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" \
      --type=merge -p "$(cat "/tmp/kvc-term-release-${SFX}.json")" >/dev/null 2>&1 || true
  fi
  rm -f "/tmp/kvc-term-release-${SFX}.json"
fi

# Two cheaper routes were rejected first: a pool pointing at a nonexistent backend cannot be created
# at all (its reference is validated at admission), and holding a pool with a finalizer risks leaving
# one wedged on a shared cluster.
#
# This paragraph used to end "flipping the flag leaves both objects ordinary, so the trap removes them
# normally". A run disproved it: flipping the flag IS what wedges them, for the reasons recorded at
# the teardown above. The route is still the right one - it is the only way to reach F4b at all - but
# it has a cleanup cost, and believing it had none is what left objects on a shared cluster.
echo "[case-57] turning multi-tenancy off on the backend to reach the last check"
# The restore marker is armed BEFORE the patch, never after. An interruption in the window between
# the two - SIGINT, a kill, a set -u error - runs the EXIT trap with the flag still 0, skips the
# restore, and leaves the pool and the backend undeletable: the exact state this case's own teardown
# notes record as having already stranded two objects on a shared cluster for 3h51m. The asymmetry
# decides the order - a restore attempted on a backend that was never patched is a no-op, while a
# restore skipped on one that was patched needs manual finalizer surgery to undo.
CASE57_TENANCY_OFF=1
kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type=merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":false}}}}}' >/dev/null 2>&1

# Polled, never slept on, and a timeout FAILS rather than SKIPs: a check whose precondition silently
# went unmet would otherwise report green while asserting nothing. The elapsed time is printed because
# nothing had measured it before - how long a backend edit takes to reach the pool's condition.
ledger_off=0
for i in $(seq 1 40); do
  if [ "$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
      -o 'jsonpath={.status.conditions[?(@.type=="QuotaLedgerAvailable")].status}' 2>/dev/null)" = False ]; then
    ledger_off=$((i * 3))
    break
  fi
  sleep 3
done

if [ "$ledger_off" -eq 0 ]; then
  record FAIL "a pool whose ledger is gone refuses the Pod" \
    "QuotaLedgerAvailable never went False within 120s of turning multi-tenancy off, so this refusal \
was never reached - the check did not run rather than passed"
else
  echo "[case-57] QuotaLedgerAvailable went False after ~${ledger_off}s"
  kvi_refused "a pool whose ledger is gone refuses the Pod" \
    "$POOL" "$(pod_without r-noledger none)"
fi

kvi_results "$CASE_ID"
