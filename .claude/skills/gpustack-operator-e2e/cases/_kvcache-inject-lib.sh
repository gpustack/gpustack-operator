#!/usr/bin/env bash
#
# _kvcache-inject-lib.sh — shared fixture and assertion helpers for the KV cache injection cases
# (53-59).
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. Each of those stays with the case that sources it, so a case
# still reads end to end on its own terms; this file only removes seven copies of the same backend,
# pool and Binding setup.
#
# A case uses it as:
#
#     set -uo pipefail
#     NS="${1:?usage: case-53.sh <NS>}"
#     CASE_ID=53
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"
#     trap kvi_teardown EXIT   # ARM FIRST - kvi_setup can fail with objects already created
#     kvi_setup                # backend + pool + namespace + Binding, all Ready
#
# The trap order is a contract, not a style: every case here was written from this block, so the
# order shown is the order they all have.
#
# Globals a case may read after kvi_setup:
#   SFX BACKEND POOL TEST_NS BINDING DOMAIN ENDPOINT
#   FAILS ROWS   (the case owns the results table; this file only appends through `record`)
#
# Environment the caller may set:
#   E2E_MOONCAKE_IMAGE   the store image                 (default docker.io/kvcacheai/mooncake:0.3.13)
#   E2E_VLLM_IMAGE       a vLLM image, for case 59       (unset => that half SKIPs, loudly)
#   E2E_SGLANG_IMAGE     an SGLang image, for case 59    (unset => that half SKIPs, loudly)
#   E2E_CLIENT_IMAGE     an image carrying the mooncake python client, for the read/write probes
#
# EXERCISED 2026-09-04 on a three-node k3s cluster: all seven completed with no failures, and case
# 54's LWS half SKIPped for a CRD that cluster does not have. Not "all passing" - a skip verifies
# nothing, which is this library's own rule.
#
# Four of the seven failed on their first real run, and not one was failing for the reason it printed:
# case 53 hit a missing prerequisite (no Binding registered the tenant its writes land on) and then
# read usage off the wrong Binding; case 56 could not have passed at all, because it compared fields
# the API server defaults onto every Pod; case 59 timed out on a 19.1GB image pull. Reading them had
# shown none of that, and three of the four were defects in the cases rather than in the webhook.

E2E_SHIM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

MOONCAKE_IMAGE="${E2E_MOONCAKE_IMAGE:-docker.io/kvcacheai/mooncake:0.3.13}"
CLIENT_IMAGE="${E2E_CLIENT_IMAGE:-$MOONCAKE_IMAGE}"

# The webhook every refusal in this family must come from. Asserting it turns "something refused" into
# "OUR webhook refused", which is a different claim once more than one plugin can sit on Pod CREATE.
KVI_WEBHOOK_NAME="mutate.gpustack-worker-kvcache.core.v1.pod"

# The pipefail the callers set makes `cmd | head -c 5 || echo $$` do something other than it reads:
# head closes after five bytes, tr dies on SIGPIPE, the pipeline reports failure, and the fallback
# APPENDS to whatever tr already produced. Measured under `set -o pipefail`: [333986], [bg33986],
# [33986] - six, seven and five characters, the split depending on when the signal landed. The `||`
# reads as "or" and behaves as "and also".
#
# Disabling pipefail for this one bounded pipeline restores the intended meaning, and the fallback is
# then applied only when nothing came out.
#
# LC_ALL=C is load-bearing, not tidiness. Under a UTF-8 locale tr exits with "Illegal byte sequence"
# on /dev/urandom, the pipeline yields nothing, and the PID fallback below runs EVERY time - which is
# what happened on macOS, where these cases are usually launched from. A PID is small and reused, so
# every fixture name became predictable and collidable on exactly the runs that looked fine.
# Measured both ways on macOS: without it, empty; with it, ouxef / cn5t6 / 5u8zf on three runs.
#
# The fallback stays, and stays a fallback: an unreadable /dev/urandom should not stop a suite that
# talks to a cluster, and with the locale fixed it is now reached only if the device itself fails.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
# $$ alone is small and reused across reboots, and these names are CLUSTER-SCOPED (the backend, the
# pool, the literal 'default'-domain Binding). Two runs that land on the same PID adopt each other's
# leftovers silently - the failure this file's own notes describe as poisoning every later run of the
# family. Adding the epoch makes a collision need the same PID in the same second.
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-i-${SFX}"
POOL="kvcp-i-${SFX}"
TEST_NS="kvc-i-${SFX}"
BINDING="bind-i"
# The Binding that registers the tenant an engine actually writes under. Named apart from BINDING
# because cases refer to that one by name when they build a Pod; nothing points a Pod at this one.
BINDING_DEFAULT="bind-i-default"
DOMAIN="dom-i-${SFX}"
ENDPOINT=""

FAILS=0
SKIPS=0
ROWS=()
record() {
  ROWS+=("$1|$2|$3")
  [ "$1" = FAIL ] && FAILS=$((FAILS + 1))
  [ "$1" = SKIP ] && SKIPS=$((SKIPS + 1))
  return 0
}


# kvi_wait_for polls one jsonpath until it equals want.
kvi_wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-120}" ns_args=()
  [ -n "${6:-}" ] && ns_args=(-n "$6")
  local got="" i
  # ${ns_args[@]+...} rather than a bare "${ns_args[@]}": under set -u, bash 3.2 - macOS's stock
  # shell, which this family is usually launched from - treats expanding an EMPTY array as an unbound
  # variable and aborts the case mid-run. Every cluster-scoped wait passes no namespace, so the empty
  # case is the common one, not the edge one. Measured both ways on bash 3.2.57.
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

# kvi_setup stands up a backend with multi-tenancy on, a pool over it, and one namespace holding one
# Binding with one reuse domain. Everything the injection webhook resolves comes from here.
#
# CALLER CONTRACT: arm `trap kvi_teardown EXIT` BEFORE calling this. The cluster-scoped backend is
# created on the first line and five of the returns below come after it, so a caller that arms the
# trap on success leaks a whole fixture whenever readiness times out. The worst piece to leak is the
# "default" Binding: its domain name is claimed cluster-wide, so it makes every later run of this
# family fail in setup, on a different case, with no trace of who left it.
#
# One domain, deliberately: a second one on this backend would be the collision the Binding-side check
# does not yet prevent, and no case in this family creates one.
kvi_setup() {
  kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${MOONCAKE_IMAGE}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        multiTenancy: true
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

  if ! kvi_wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 240 >/dev/null; then
    record FAIL "backend ready" "the master did not reach Ready in 240s; nothing below can run"
    return 1
  fi

  kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${POOL}
spec:
  backends: [${BACKEND}]
  quota:
    total: 1Gi
YAML

  if ! kvi_wait_for kvcachepools.worker.gpustack.ai "$POOL" '{.status.phase}' Ready 180 >/dev/null; then
    record FAIL "pool ready" "the pool did not reach Ready in 180s; the webhook would refuse every Pod"
    return 1
  fi

  ENDPOINT="$(kubectl get kvcachepools.worker.gpustack.ai "$POOL" \
    -o jsonpath='{.status.clientEndpoint}' 2>/dev/null)"
  if [ -z "$ENDPOINT" ]; then
    record FAIL "pool published a client endpoint" \
      "status.clientEndpoint is empty, so there is no address to inject"
    return 1
  fi

  kubectl create namespace "$TEST_NS" >/dev/null 2>&1 || true
  kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: ${DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quotaCeiling: 256Mi
YAML

  if ! kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$BINDING" '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
    record FAIL "binding ready" "the Binding did not reach Ready in 180s"
    return 1
  fi

  # A SECOND Binding, whose reuse domain is the literal string "default". This is not a workaround for
  # the tests: it is the configuration the reference documents as a prerequisite, so a fixture without
  # it would be exercising a cluster we tell users not to run.
  #
  # This is a prerequisite for the engines that forward no tenant - vllm and vllm-ascend - not a
  # universal one. Their clients fall back to the store's own default name, and a multi-tenant master,
  # the only kind a pool accepts, refuses a write from a name absent from its ledger. SGLang writes
  # under its Binding's own reuse domain and needs nothing here; this fixture creates it anyway
  # because the other cases share the setup.
  #
  # Measured: with only the domain Binding above, an injected vLLM Pod starts, stays Ready, and every
  # put returns TENANT_NOT_REGISTERED (-1701); adding this one turns the same client's put into rc=0
  # with the value readable back. Registering the name is all a Binding does for it.
  kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${BINDING_DEFAULT}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: default
    blockSize: 16
    dtype: bfloat16
  quotaCeiling: 256Mi
YAML

  if ! kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$BINDING_DEFAULT" '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
    record FAIL "default-tenant binding ready" \
      "the Binding registering the 'default' tenant did not reach Ready in 180s; every injected Pod \
would start and then fail every write with TENANT_NOT_REGISTERED"
    return 1
  fi
  return 0
}

# kvi_pod_manifest prints an opted-in Pod. Args: name, engine, then any extra annotation lines.
#
# The container is kept alive by `python3 -c`, NOT by `/bin/sh -c`, and the difference is a refusal
# this webhook now makes. After a shell's -c everything further becomes a positional parameter, so
# anything appended to args reaches $0 and $1 rather than the engine - the Pod starts, records itself
# as injected, and never enables the connector. These fixtures used exactly that shape, which meant
# the fixture would have been refused the moment the check landed.
#
# python3 -c has the same keep-alive effect without the trap: argv[0] is not a shell, so the check
# does not fire, and arguments appended after the script land in sys.argv where they change nothing.
#
# Keeping a `-c` in the shape is deliberate, and makes these fixtures a test of the check itself: the
# refusal must key on argv[0] being a shell, not on the flag alone. Widen it to "any -c" and all eight
# fixtures in this family turn red at once.
#
# command is non-empty for a second reason: a container declaring neither command nor args is refused,
# because Kubernetes would then read the injected args as the whole command line and discard the
# image's CMD.
# KVI_IMAGE overrides the container image for one call. It exists so a caller that needs a different
# image does not have to rewrite the rendered manifest: the two engine cases used to pipe this through
# `sed s|image: $CLIENT_IMAGE|image: $ENGINE_IMAGE|`, and that one expression produced three separate
# defects - a metacharacter in the replacement corrupting the manifest, a metacharacter in the pattern
# silently not matching, and the pattern simply not occurring if this heredoc ever renders the line
# differently. The last two share a failure mode that is the dangerous one: the substitution no-ops,
# the Pod runs the mooncake image, and the probe's answer is still reported as the engine's verdict.
# Passing the value in removes all three at once rather than guarding each.
kvi_pod_manifest() {
  local name="$1" engine="$2"
  local image="${KVI_IMAGE:-$CLIENT_IMAGE}"
  # KVI_BINDING overrides which Binding the Pod names, for the one check that needs a Binding other
  # than the shared fixture. Passing it as an extra annotation instead would emit the key TWICE and
  # kubectl rejects a duplicate mapping key, so the override has to happen here.
  local binding="${KVI_BINDING:-$BINDING}"
  shift 2
  cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${TEST_NS}
  labels:
    kvcache.gpustack.ai/inject: "true"
  annotations:
    kvcache.gpustack.ai/binding: ${binding}
    kvcache.gpustack.ai/engine: ${engine}
$(for a in "$@"; do echo "    $a"; done)
spec:
  restartPolicy: Never
  containers:
    - name: engine
      image: ${image}
      command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML
}

# kvi_stamp reads one field out of the injection record a Pod carries.
kvi_stamp() {
  local pod="$1" field="$2"
  kubectl -n "$TEST_NS" get pod "$pod" \
    -o "jsonpath={.metadata.annotations.kvcache\\.gpustack\\.ai/injected}" 2>/dev/null \
    | python3 -c "import json,sys; d=sys.stdin.read().strip(); print(json.loads(d)['${field}'] if d else '')" 2>/dev/null
}

# kvi_env reads one environment value off the injected container.
kvi_env() {
  local pod="$1" name="$2"
  kubectl -n "$TEST_NS" get pod "$pod" \
    -o "jsonpath={.spec.containers[0].env[?(@.name=='${name}')].value}" 2>/dev/null
}

# kvi_refused applies a manifest that must be REJECTED, and reports whether the message names what it
# should. A create that succeeds is the failure: this family exists to prove the webhook refuses
# rather than injecting something that looks configured and is not.
kvi_refused() {
  local check="$1" want="$2" manifest="$3" out rc
  # The status is captured on its own line: testing $? after an assignment reads the assignment's
  # status, which is the one thing here that is never the interesting value.
  out="$(echo "$manifest" | kubectl apply -f - 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -eq 0 ]; then
    record FAIL "$check" "the API server accepted it; the webhook did not refuse"
    echo "$manifest" | kubectl delete -f - --ignore-not-found --wait=false >/dev/null 2>&1 || true
    return 1
  fi
  # WHICH webhook refused, not merely that something did. Measured 2026-09-04 against a live API
  # server: the envelope is `admission webhook "<name>" denied the request: <message>` and carries no
  # other object - not even the Pod's own name. That narrowness is what makes the short `want` strings
  # below (a bare "vllm", "args", or the namespace) satisfiable by SOMEONE ELSE's message the day a
  # second admission plugin sits on this path. Today only one webhook here refuses; that is a property
  # of the environment, not an assertion, so it is asserted.
  if ! echo "$out" | grep -qF "$KVI_WEBHOOK_NAME"; then
    record FAIL "$check" "refused, but not by ${KVI_WEBHOOK_NAME}, so the message below belongs to \
something else and the reason it names is not ours: $(echo "$out" | tr '\n' ' ' | cut -c1-160)"
    return 0
  fi
  if echo "$out" | grep -qF "$want"; then
    record PASS "$check" "refused by ${KVI_WEBHOOK_NAME}, and the message names '${want}'"
  else
    record FAIL "$check" "refused, but the message does not name '${want}': $(echo "$out" | tr '\n' ' ' | cut -c1-160)"
  fi
  return 0
}

# kvi_results prints the table every case ends with, and sets the exit status.
kvi_results() {
  local case_id="$1"
  echo
  echo "STATUS | CHECK | OBJECT"
  # Split on the delimiter `record` actually wrote, not on whitespace: a CHECK name is several words.
  # Guarded for the same bash 3.2 reason as kvi_wait_for: a setup that fails before the first record
  # leaves ROWS empty, and the banner would abort instead of printing the failure that caused it.
  for r in ${ROWS[@]+"${ROWS[@]}"}; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${case_id}] ${FAILS} check(s) FAILED"; return 1; }
  # A skipped check is not a passed one, and the footer has to say so. "all checks passed" over a
  # table of SKIPs is the shape this suite exists to avoid: a run that verified nothing, reported in
  # the same words as one that verified everything.
  if [ "$SKIPS" -gt 0 ]; then
    echo "[case-${case_id}] ${SKIPS} check(s) SKIPPED and $(( ${#ROWS[@]} - SKIPS )) passed \
- the skipped ones verified NOTHING; read the OBJECT column for what is left unverified"
    return 0
  fi
  echo "[case-${case_id}] all checks passed"
  return 0
}

# kvi_teardown removes everything kvi_setup made, in the order that lets the finalizers clear.
#
# Safe over a HALF-BUILT fixture, which is what makes the trap arm before setup: every delete passes
# --ignore-not-found, and the binding wait loop reads an empty list as done.
#
# The Binding goes first and is given time: a domain still holding objects makes the master refuse to
# drop its quota, and the operator's finalizer is right to wait. Forcing it before that is how a run
# leaves a namespace Terminating forever.
kvi_teardown() {
  echo
  echo "[kvcache-inject] cleanup"
  kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$TEST_NS" delete deployments --all --ignore-not-found --wait=false >/dev/null 2>&1 || true

  kubectl -n "$TEST_NS" delete kvcachepoolbindings.worker.gpustack.ai "$BINDING" "$BINDING_DEFAULT" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  local left="" _
  for _ in $(seq 1 20); do
    left="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai -o name 2>/dev/null)"
    [ -z "$left" ] && break
    sleep 3
  done
  if [ -n "${left:-}" ]; then
    echo "[kvcache-inject] a binding is still held after 60s; what the operator says about it:"
    kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai \
      -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[*]}{.type}={.status}({.reason}) {.message}{"\n"}{end}{end}' 2>/dev/null \
      | sed 's/^/    /'
    # Forcing the finalizer off bypasses the operator's own release path, which removes it only once
    # the master's ledger entry for that tenant is gone (kv_cache_pool.go, releaseKVCachePoolBinding).
    # So this can leave a tenant registered in a master with no Binding owning it. It is still the
    # right trade here - a Binding left held forever poisons every later run of this family, and these
    # names are cluster-scoped - but it is not clean, and a forced action that prints nothing reads as
    # a clean one.
    echo "[kvcache-inject] forcing the finalizer off; a tenant may remain registered in the master."
    echo "[kvcache-inject] the pool and backend are deleted below, which normally takes it with them;"
    echo "[kvcache-inject] if either is also stuck, check the master's tenant list by hand."
    kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai -o name 2>/dev/null | while read -r b; do
      kubectl -n "$TEST_NS" patch "$b" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    done
  fi

  kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
