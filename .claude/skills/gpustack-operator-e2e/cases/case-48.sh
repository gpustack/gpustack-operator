#!/usr/bin/env bash
#
# CASE 48 — The deliberate break: what a deployment looks like when the cache cannot work
#   (MUTATING: one external KVCacheBackend and one KVCachePool, both CLUSTER-SCOPED, plus one
#    KVCachePoolBinding and two ModelDeployments in the fixture's namespace — all removed here)
#
#   case-48.sh <NS>
#
# Goal:        The failure side of the connector, from the two ends that can be observed without an
#              accelerator: what the operator does NOT render when the pool cannot be reached, and
#              what it renders anyway when the image cannot possibly use it.
#
# WHAT `CacheAttached != True` IS WORTH HERE, SAID BEFORE ANY ROW LEANS ON IT. That comparison is the
# acceptance this case was written from, and on this cluster it is TRUE OF EVERY DEPLOYMENT there is.
# No replica becomes Ready without an engine image, no engine gives an account, and the condition is
# then Unknown/NoReplicaReady for a healthy deployment and a broken one alike. A row asserting only
# that would pass with the whole connector deleted, with the controller stopped, or with nothing
# created at all.
#   => So every row below pairs it with something that is NOT true of everything: the value
# DomainRegistered carries on the same object, or the connector's own carriers on the Pod spec. The
# acceptance is met by asserting more than it asks, not less.
#
# THE ENGINE IS VLLM, and that is the other half of the pair case-47 began. vLLM's connector travels
# on FIVE separate carriers -- a Pod annotation, a downwardAPI volume, its volumeMount, a
# MOONCAKE_CONFIG_PATH variable, and a --kv-transfer-config argument -- where SGLang's is environment
# only. "No connector was rendered" is therefore five independent absences here rather than one, and
# that is what makes a PARTIAL render visible: a client pointed at an address that does not answer
# looks like a cache miss from outside the Pod, while a replica that was never configured says so in
# its own spec, and refusing the first is the whole reason resolveModelDeploymentConnection returns
# nothing rather than something incomplete. Using vllm also puts the second branch of the engine
# mapping under an e2e; case-47 covers sglang.
#
# THE FIRST VEHICLE IS SPLIT, BECAUSE ONLY ONE HALF OF IT IS ANSWERABLE HERE. "An engine image
# without the matching per-vendor mooncake-transfer-engine wheel" is two claims, not one:
#
#   1. the operator renders the connector regardless -- it reads no release matrix and inspects no
#      image, so a break of this kind is invisible to it and only the engine can report it. Asserted
#      below, on a deployment whose image carries no engine at all.
#   2. the engine then does one of two things: abort at init, or serve on without the cache. That
#      needs an engine that runs. This cluster has none, and what is observed instead is a container
#      that never came up -- a THIRD shape, and not either of the two. Reported as SKIP, naming which
#      of the three was seen, because calling it one of the other two would be the substitution this
#      suite exists to refuse.
#
# Environment: as CASE 47 — a cluster with no accelerator and a registry it can pull the Mooncake
#              image from. No GPU, no engine image.
# Needs:       a live Mooncake store (the shared fixture brings up backend, pool and Bindings) and an
#              InstanceType, for the role to reference
#
set -uo pipefail

NS="${1:?usage: case-48.sh <NS>}"
CASE_ID=48

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# The unreachable pool, named before the trap is armed so the teardown can always see the names.
# CLUSTER-SCOPED, so the suffix is not decoration: two runs sharing a name adopt each other's
# leftovers, which is the failure the library's own notes describe.
DEAD_BACKEND="kvcb-dead-${SFX}"
DEAD_POOL="kvcp-dead-${SFX}"
DEAD_BINDING="bind-dead-${SFX}"
DEAD_DOMAIN="dom-dead-${SFX}"
# RFC 2606 reserves `.invalid`, so this name is guaranteed never to resolve anywhere. An address that
# could accidentally belong to something running is a different experiment from an unreachable one.
DEAD_HOST="case48-nowhere.invalid"

# THE SHARED TEARDOWN IS WRAPPED, by case-47's rule and by the one its re-run added:
#
#   - anything the Binding's `usedBy` can name is removed before the fixture, or the library's
#     forced-finalizer path -- rare and loud by design -- becomes routine and stops being read;
#   - and the shared teardown deletes BY NAME, so whatever this case created in that namespace, this
#     case removes, claimant or not.
#
# EVERY OBJECT ON THE UNREACHABLE SIDE IS FORCED, and the rule is one rule rather than three special
# cases: an object whose release path talks to the master cannot be released when the master does not
# exist. The Binding drops a tenant from the ledger, the pool reads the ledger to release its quota,
# and the backend waits for the pool -- so all three wedge, permanently, on every run.
#
# Measured on the first run of this case, which forced only the Binding: the namespace and the
# Binding went, and `kvcp-dead-*` stayed Terminating on its `gpustack.ai/controlled` finalizer with
# `Error/reading the tenant ledger failed: ... no such host`, holding `kvcb-dead-*` behind it at
# `Deleting/in use by KVCachePool`. Both are CLUSTER-SCOPED, so nothing else would ever collect them.
#
# Forcing here carries no leak to chase, and that is a property of THIS fixture rather than of
# forcing in general: nothing was ever registered on a master that never answered. The shared
# library's own forced branch is a different statement -- it acts on a REACHABLE master, where a
# stranded tenant is real.
case48_force_delete() {
  local kind="$1" name="$2" ns_args=() left="" _
  [ -n "${3:-}" ] && ns_args=(-n "$3")
  kubectl ${ns_args[@]+"${ns_args[@]}"} delete "$kind" "$name" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 10); do
    left="$(kubectl ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" -o name 2>/dev/null)"
    [ -z "$left" ] && return 0
    sleep 3
  done
  echo "[case-48] ${kind}/${name} did not release in 30s; forcing its finalizer off."
  kubectl ${ns_args[@]+"${ns_args[@]}"} patch "$kind" "$name" --type=merge \
    -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  # THE FORCE IS VERIFIED, because `|| true` on the patch means a failure is silent. Two of the
  # three objects here are CLUSTER-SCOPED and nothing else will ever collect them, so a patch that
  # did not take leaves them Terminating forever -- and a teardown that says nothing reads as one
  # that worked. This does not retry: if the patch failed, saying so is the useful act.
  sleep 3
  if [ -n "$(kubectl ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" -o name 2>/dev/null)" ]; then
    echo "[case-48] ${kind}/${name} IS STILL THERE after the force. Remove it by hand:"
    echo "[case-48]   kubectl ${ns_args[@]+"${ns_args[@]}"} patch $kind $name --type=merge -p '{\"metadata\":{\"finalizers\":null}}'"
  fi
}
case48_teardown() {
  if [ -n "${TEST_NS:-}" ]; then
    # The deployments first and WAITED: each claims its Binding, and a Binding the claim still names
    # is one the shared teardown would sit on for 60s before forcing.
    kubectl -n "$TEST_NS" delete modeldeployments.worker.gpustack.ai \
      case48-registered case48-unreachable --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1
    case48_force_delete kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" "$TEST_NS"
  fi
  kvi_teardown
  # After kvi_teardown, which takes the namespace with it: these two are cluster-scoped and outlive
  # it, and the pool goes first because the backend is held behind it.
  case48_force_delete kvcachepools.worker.gpustack.ai "$DEAD_POOL"
  case48_force_delete kvcachebackends.worker.gpustack.ai "$DEAD_BACKEND"
}

trap case48_teardown EXIT   # ARM FIRST - kvi_setup can fail with objects already created
kvi_setup

IT="${E2E_MD_INSTANCE_TYPE:-$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)}"
if [ -z "$IT" ]; then
  record FAIL "an instance type exists" "no InstanceType in the cluster; run case-1 first"
  kvi_results "$CASE_ID"
  exit 1
fi

# --- the unreachable pool ---
#
# EXTERNAL rather than managed, and that is what makes it cheap and honest at once: the reconciler
# renders nothing for an external backend, it only observes, so this fixture starts no workload and
# occupies no memory. What it describes is a master somebody else runs at an address that does not
# exist -- which is exactly the shape of a pool that has gone away, without having to break a working
# one.
#
# EVERY APPLY BELOW IS READ. Discarding the output makes a REFUSED manifest look like a controller
# that never reconciled, and every row after it then goes red naming a missing condition on an object
# that was never created.
dead_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${DEAD_BACKEND}
spec:
  type: Mooncake
  connection:
    external:
      endpoints:
        - name: Client
          address: ${DEAD_HOST}:50051
        - name: Admin
          address: ${DEAD_HOST}:9003
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: ${DEAD_POOL}
spec:
  backends: [${DEAD_BACKEND}]
  quota:
    total: 1Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${DEAD_BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${DEAD_POOL}
  domain:
    name: ${DEAD_DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quotaCeiling: 256Mi
YAML
)"
# THE CHECK COUNTS, IT DOES NOT MATCH A SUBSTRING, and over a multi-document manifest that is the
# difference between a guard and a decoration. `kubectl apply` reports the three objects in ONE
# stream, so a run where the backend was created and the pool was REFUSED still contains the word
# "created" -- and every row below would then read a missing condition on an object nobody made,
# which is the exact substitution the comment above says this guard prevents. Three objects, three
# `created` lines, or nothing here has a subject.
#   A count also carries the empty case for free: no output at all counts zero.
dead_created="$(printf '%s\n' "$dead_out" | grep -c ' created$' || true)"
if [ "${dead_created:-0}" -ne 3 ]; then
  record SKIP "the unreachable pool is admitted" \
    "${dead_created:-0} of 3 objects were created, so nothing below has a broken subject to \
observe: $(echo "${dead_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  kvi_results "$CASE_ID"
  exit $?
fi

# The precondition, asserted rather than assumed: a Binding whose master does not exist must not
# report itself usable. Ready here would be the finding -- the operator would be telling every
# deployment on it that a reuse domain is registered on a master it never reached, and the connector
# below would then be rendered pointing at an address that answers nothing.
#
# The wait is the discrimination: 90 seconds separates "broken" from "not observed yet", and the
# fixture's own Binding above reaches Ready well inside it.
dead_ready=no
DEAD_PHASE="$(kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" \
  '{.status.phase}' Ready 90 "$TEST_NS")" && dead_ready=yes
if [ "$dead_ready" = yes ]; then
  record FAIL "a Binding whose pool cannot be reached never reports itself ready" \
    "${DEAD_BINDING} reached Ready against a master at ${DEAD_HOST} that does not resolve, so \
DomainRegistered would go True and a connector would be rendered for an address nothing answers"
else
  record PASS "a Binding whose pool cannot be reached never reports itself ready" \
    "${DEAD_BINDING} is '${DEAD_PHASE:-<no phase observed>}' after 90s against a master at \
${DEAD_HOST} that does not resolve"
fi

# --- two deployments: one on the dead Binding, one on the fixture's Ready Binding ---
#
# Both name an image explicitly. A role without one gets an image synthesized from the accelerator
# backend its InstanceType observed, and a CPU-only InstanceType has observed none; that refusal is
# correct and is pinned in the unit tier, so naming an image drops the dependency rather than the
# assertion.
#
# The image carries the Mooncake client and no engine, which is what makes case48-registered the
# subject of the first vehicle as well as the control for the second: the operator renders a full
# vLLM connector onto a container that could never run vLLM, because it inspects no image.
deploy() {
  kubectl apply -f - <<YAML 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${TEST_NS}
spec:
  engine: vllm
  # The newest vLLM the runner project publishes. Inert here -- the role names its image explicitly,
  # so nothing is synthesized from this -- but a version no runner ships would read as the version
  # under test, and this case tests nothing about an engine.
  engineVersion: "0.27.1"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${2}
  roles:
  - name: server
    instanceType: ${IT}
    replicas: 1
    template:
      image: ${CLIENT_IMAGE}
YAML
}

# Read one at a time. A combined capture would find "created" from whichever one succeeded and let
# the other's refusal through as a missing condition on an object nobody made.
u_out="$(deploy case48-unreachable "$DEAD_BINDING")"
r_out="$(deploy case48-registered "$BINDING")"
if [ -z "$u_out" ] || [ -n "${u_out##*created*}" ] || [ -z "$r_out" ] || [ -n "${r_out##*created*}" ]; then
  record SKIP "both deployments are admitted" \
    "a ModelDeployment was not created, so every row below would report an absence produced by the \
manifest rather than by the operator — case48-unreachable: ${u_out:-<no output>} / case48-registered: \
${r_out:-<no output>}"
  kvi_results "$CASE_ID"
  exit $?
fi

# conds reads one object's conditions as `Type=Status/Reason;`. Every test on it guards for empty
# first: `${conds##*pat*}` deletes the longest matching prefix, and deleting anything from "" leaves
# "", so `-z` is TRUE on an empty value -- which is what a cluster with no ModelDeployment controller
# produces, and a row that passes on nothing asserts nothing.
conds_of() {
  local md="$1" want="$2" conds="" _
  for _ in $(seq 1 20); do
    conds="$(kubectl -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason};{end}' 2>/dev/null)"
    [ -n "$conds" ] && [ -z "${conds##*${want}*}" ] && break
    sleep 3
  done
  echo "$conds"
}

U_CONDS="$(conds_of case48-unreachable 'DomainRegistered=')"
if [ -n "$U_CONDS" ] && [ -z "${U_CONDS##*DomainRegistered=False/BindingNotReady*}" ]; then
  record PASS "an unreachable pool is reported as a Binding that is not ready, not as one missing" \
    "DomainRegistered=False/BindingNotReady — the Binding exists, so BindingNotFound would send an \
admin to create an object that is already there"
else
  record FAIL "an unreachable pool is reported as a Binding that is not ready, not as one missing" \
    "wanted DomainRegistered=False/BindingNotReady, got: ${U_CONDS:-<no conditions at all within 60s>}"
fi

# --- what was rendered onto each replica ---
#
# Read off the Pod SPEC, which the operator writes at render time. A Pod that never starts carries
# everything below, and the replicas ARE built on both sides: convergence is not gated on
# DomainRegistered, so a deployment whose pool is unreachable still gets its replica.
pod_of() {
  local md="$1" pod="" _
  for _ in $(seq 1 40); do
    pod="$(kubectl -n "$TEST_NS" get pods \
      -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance="$md" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$pod" ] && break
    sleep 3
  done
  echo "$pod"
}

# markers echoes `<count>:<names>` over vLLM's five carriers. Counting them separately is the point:
# a renderer that emitted the argument and dropped the annotation would leave the volume projecting
# an empty file, and one number that says "some connector" would hide exactly that.
#
# THE MOUNT IS COUNTED APART FROM THE VOLUME, and they are not one carrier. A render that emits the
# volume and drops the volumeMount produces a Pod where the configuration exists and no process can
# read it -- a partial render, which is the thing this case exists to catch, scoring full marks if
# the pair were counted as one.
markers() {
  local pod="$1" n=0 names="" v cmd
  v="$(kubectl -n "$TEST_NS" get pod "$pod" \
    -o "jsonpath={.metadata.annotations.kvcache\\.gpustack\\.ai/client-config}" 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}annotation,"; }
  v="$(kubectl -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.containers[?(@.name=="main")].env[?(@.name=="MOONCAKE_CONFIG_PATH")].value}' 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}env,"; }
  cmd="$(kubectl -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.containers[?(@.name=="main")].command}' 2>/dev/null)"
  [ -n "$cmd" ] && [ -z "${cmd##*--kv-transfer-config*}" ] && { n=$((n + 1)); names="${names}arg,"; }
  v="$(kubectl -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.volumes[?(@.name=="gpustack-kvcache-config")].name}' 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}volume,"; }
  v="$(kubectl -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={.spec.containers[?(@.name=="main")].volumeMounts[?(@.name=="gpustack-kvcache-config")].mountPath}' 2>/dev/null)"
  [ -n "$v" ] && { n=$((n + 1)); names="${names}mount,"; }
  names="${names%,}"
  echo "${n}:${names:-none}"
}

POD_U="$(pod_of case48-unreachable)"
POD_R="$(pod_of case48-registered)"
if [ -z "$POD_U" ] || [ -z "$POD_R" ]; then
  record FAIL "a deployment on an unreachable pool renders no connector at all" \
    "no replica appeared within 120s for case48-unreachable ('${POD_U}') or case48-registered \
('${POD_R}'), so there is no rendered spec to read"
else
  MU="$(markers "$POD_U")"
  MR="$(markers "$POD_R")"
  if [ -z "${MR%%5:*}" ] && [ -z "${MU%%0:*}" ]; then
    record PASS "a deployment on an unreachable pool renders no connector at all" \
      "case48-registered carries all five carriers (${MR#*:}) and case48-unreachable carries none — \
not a partial one, which is the outcome that would look like a cache miss from outside the Pod \
instead of like a replica that was never configured"
  elif [ -n "${MR%%5:*}" ]; then
    record FAIL "a deployment on an unreachable pool renders no connector at all" \
      "the READY-Binding side rendered ${MR} instead of all five carriers, so the absence on the \
other side is not evidence: a renderer that emits nothing anywhere produces it too"
  else
    record FAIL "a deployment on an unreachable pool renders no connector at all" \
      "case48-unreachable rendered ${MU} against ${MR} on the control — a connector pointed at a \
pool that answers nothing is the one outcome the resolver returns nil to avoid"
  fi
fi

# --- configured is not attached ---
#
# The pair on ONE object is what makes this an observation rather than a restatement of the cluster's
# shape. DomainRegistered=True says the operator did every part of its job; CacheAttached says
# nothing observed a cache, on the same deployment, in the same pass. An implementation that raised
# CacheAttached from a successful render would go red here, and only here -- `CacheAttached != True`
# on its own is satisfied by every deployment on this cluster.
R_CONDS="$(conds_of case48-registered 'CacheAttached=')"
# Extracted only after the condition is known to be there. `${x##*CacheAttached=}` on a string
# without it returns the string unchanged, and the `%%;*` below would then quietly hand this row the
# FIRST condition's value under CacheAttached's name.
R_CA=""
if [ -n "$R_CONDS" ] && [ -z "${R_CONDS##*CacheAttached=*}" ]; then
  R_CA="${R_CONDS##*CacheAttached=}"
  R_CA="${R_CA%%;*}"
fi
if [ -z "$R_CA" ]; then
  record FAIL "a rendered connector is not reported as an attached cache" \
    "no CacheAttached condition within 60s, so the pair this row rests on was never read: \
${R_CONDS:-<no conditions at all>}"
elif [ -z "${R_CONDS##*DomainRegistered=True/Registered*}" ] && [ -z "${R_CA##Unknown/*}" ]; then
  record PASS "a rendered connector is not reported as an attached cache" \
    "DomainRegistered=True/Registered and CacheAttached=${R_CA} on case48-registered — the operator \
rendered the whole connector onto an image that carries no engine, and claims nothing about a cache \
no replica ever reported on"
else
  record FAIL "a rendered connector is not reported as an attached cache" \
    "wanted DomainRegistered=True/Registered beside CacheAttached=Unknown/*, got: ${R_CONDS}"
fi

# --- the shape the engine took, which this cluster cannot answer ---
#
# THE THREE SHAPES GIVE THE SAME NOTHING, and only two of them are properties of an engine:
#   a. the container never came up  -- a property of the IMAGE, which is this cluster's answer;
#   b. the engine came up and aborted when the connector could not initialize;
#   c. the engine came up and served on without the cache.
# (b) and (c) are what the spec asks this case to record, they differ per engine and per version, and
# both need an image that actually serves. Recording (a) as either would be the substitution this
# suite exists to refuse, so the observed state is printed and the row verifies nothing.
if [ -n "$POD_R" ]; then
  R_STATE="$(kubectl -n "$TEST_NS" get pod "$POD_R" -o \
    'jsonpath={.status.phase}/{.status.containerStatuses[0].state.waiting.reason}{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null)"
else
  R_STATE=""
fi
record SKIP "which shape the engine takes when the connector cannot initialize" \
  "shape (a): the replica is '${R_STATE:-<no pod state>}' because its image carries no engine at all. \
Aborting at init and serving on without the cache are the two the spec asks for; both need an image \
that serves, which is the accelerator gate this case was split away from"

kvi_results "$CASE_ID"
