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
#              key or object the author has to look at. The last check is one layer up: turning
#              multi-tenancy off on the backend a live pool consumes is refused, naming that pool.
#
#              SKIPS: none. This case has no conditional half - every check either runs or FAILS, and
#              a precondition it cannot meet is recorded as a failure rather than passed over. The
#              footer counts any SKIP separately from the passes, so a skipped check can never be read
#              off the PASS count.
#
#              NOT COVERED HERE: the Pod-side refusal of a pool whose ledger is gone. It used to be
#              reached by turning multi-tenancy off under the live pool; that edit is now refused at
#              backend admission (the last check), which is the rule that closed the degradation, so
#              a pool without a ledger is met only during an outage of its master. The resolver pins
#              that refusal in TestPodKVCacheResolve_QuotaLedgerGate.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              The multi-tenancy check is a server-side dry run and persists nothing, so the backend
#              is never left without a ledger - the state that once wedged a pool and its backend
#              undeletable for hours on a shared cluster. It changes no shared baseline - every
#              object it touches is one it created.
#
# The shell-wrapper check was added after the first run, in both spellings: command+args are
# concatenated by Kubernetes, so a check written against either field alone catches one and misses
# the other.
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
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

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

# A Binding that exists but is being DELETED
# The refusal this covers is the one that needs no mistake: deleting a Binding is routine operations,
# and a plain Pod is not in status.usedBy, so the finalizer protecting declared consumers cannot see
# it. Injected against a Binding whose domain is leaving the ledger, the Pod starts, is stamped, and
# fails every write with TENANT_NOT_REGISTERED. Waiting does not heal it.
#
# THE WINDOW IS HELD, NOT RACED. A test finalizer keeps the object in Deleting for exactly as long as
# this check needs. Deleting and hoping to submit inside the operator's own window would be a bet: a
# pass would be luck and a failure would be indistinguishable from a flake.
# Rejected alternative: scaling the Mooncake leader to zero so the operator's finalizer cannot
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
  quota:
    ceiling: 64Mi
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

# Multi-tenancy withdrawn under a live pool, LAST because it is the one check about the backend.
#
# This is where the degradation chain now ends. A KVCachePool over a backend with a tenant ledger
# registers its domains there, and turning the flag off afterwards used to strand them: the
# pool kept running on a master with no ledger, its Pods met the webhook's ledger refusal, and its
# finalizer could no longer release what it had registered - which wedged a pool and its backend
# undeletable on a shared cluster. Backend admission now refuses the withdrawal while anything
# consumes the backend, naming the consumers, so the check asserts that refusal.
#
# A SERVER-SIDE DRY RUN, never a real patch: it runs the same admission and persists nothing, so a
# regression that admitted the withdrawal is reported without leaving a backend in the state this
# refusal exists to prevent. It names the v1alpha1 CRD explicitly: the aggregated worker.gpustack.ai/v1
# API answers a server-side dry run without running the CRD's admission, so a dry run through it
# would read as admitted whatever the rule says.
WITHDRAW="$(kubectl patch kvcachebackends.v1alpha1.worker.gpustack.ai "$BACKEND" --dry-run=server --type=merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"multiTenancy":false}}}}}' 2>&1)" && rc=0 || rc=$?
WITHDRAW="$(echo "$WITHDRAW" | tr '\n' ' ')"
if [ "$rc" -eq 0 ]; then
  record FAIL "multi-tenancy cannot be withdrawn under a live pool" \
    "the dry run was ADMITTED, so a backend a pool consumes can lose its tenant ledger again: ${WITHDRAW:0:160}"
elif echo "$WITHDRAW" | grep -qF "multi-tenancy cannot be turned off while" \
  && echo "$WITHDRAW" | grep -qF "KVCachePool/${POOL}"; then
  record PASS "multi-tenancy cannot be withdrawn under a live pool" \
    "refused at backend admission, naming KVCachePool/${POOL} as the consumer to remove first"
else
  record FAIL "multi-tenancy cannot be withdrawn under a live pool" \
    "refused, but not by the withdrawal rule naming KVCachePool/${POOL}: ${WITHDRAW:0:200}"
fi

kvi_results "$CASE_ID"
