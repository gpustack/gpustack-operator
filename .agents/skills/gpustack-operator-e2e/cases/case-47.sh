#!/usr/bin/env bash
#
# CASE 47 — Two deployments on one Binding share a reuse domain; an unreachable pool renders no connector
#   (MUTATING, self-cleaning: five ModelDeployments, two extra Bindings, one unreachable backend and
#    pool and one probe Pod — all removed by the shared teardown)
#
#   case-47.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_MD_INSTANCE_TYPE
# Goal:        The sharing claim the KV connector rests on, asserted from the two ends that can be
#              observed without an accelerator: what the operator RENDERED onto each replica, what
#              the store does with those values, and what it renders when the pool cannot be
#              reached.
#
# TWO HALVES, ONE FIXTURE. The first half asks whether two deployments on one Binding carry the
# same reuse domain and a third, on another Binding, carries a different one — and whether the
# store partitions on what was rendered. The second half asks what the operator renders when the
# pool behind the Binding cannot be reached: nothing at all, where a reachable pool gets the whole
# connector. Both are read off ONE Mooncake backend, ONE pool, ONE namespace and ONE Binding, so
# the two answers are comparable: the same cluster, in the same pass, under the same operator.
#
# THE ENGINE IS SGLANG IN THE FIRST HALF AND VLLM IN THE SECOND, and that is not decoration. Every
# engine is rendered the Binding's reuse domain, but it travels differently: the vLLM family reads
# the tenant from a client config projected from a Pod annotation, SGLang carries MOONCAKE_TENANT_ID
# on the container, and vLLM's connector itself travels on FIVE separate carriers — a Pod
# annotation, a downwardAPI volume, its volumeMount, a MOONCAKE_CONFIG_PATH variable and a
# --kv-transfer-config argument. The first half therefore reads one carrier and the second counts
# five, which is what makes a PARTIAL render visible: a client pointed at an address that does not
# answer looks like a cache miss from outside the Pod, while a replica that was never configured
# says so in its own spec, and refusing the first is the whole reason the resolver returns
# nothing rather than something incomplete.
#
# THE REPLICAS ARE NOT EXPECTED TO RUN. They are rendered from a client image that carries no engine,
# so no container starts -- and nothing here needs one. Every value this case reads is on the Pod
# SPEC, written by the operator at render time, and the store side is driven by the store's own
# python client. That is what makes the case answerable on a cluster with no accelerator.
#
# THE GAP THIS LEAVES, STATED RATHER THAN IMPLIED. A real replica runs an engine, not this probe, so
# "the engine reads MOONCAKE_TENANT_ID and forwards the value to the store" is a DIFFERENT claim from
# anything below. It is measured in case-53, against a real SGLang image, and that measurement
# applies to the path this case exercises because the injection webhook and this controller both
# render through the same `pkg/worker/kvcache/inject`. A case that drove the probe and
# then claimed the engine's behaviour would be asserting something it never looked at.
#
# AND THE STORE ROW MEASURES DATA ISOLATION, NOT A QUOTA LEDGER. Those are different claims and the
# difference decides how strong a security statement may lean on this case. What is asserted below is
# that a payload written under one tenant is not readable under another. It is NOT that each tenant
# carries an independent quota account -- this store's quota behaves as an eviction trigger rather
# than a hard ledger, so "one domain, one account" stays an ASSUMPTION here and is tracked on the
# kv-cache side. Nothing in this case may be read as confirming that a workload able to mint domains
# could escape a namespace ceiling; it confirms only that the domains do not see each other's data.
#
# AND `CacheAttached != True` IS NOT WORTH A ROW ON ITS OWN. That comparison would be TRUE OF EVERY
# DEPLOYMENT on this cluster: no replica becomes Ready without an engine image, no engine gives an
# account, and the condition is then Unknown/NoReplicaReady for a healthy deployment and a broken one
# alike. A row asserting only that would pass with the whole connector deleted, with the controller
# stopped, or with nothing created at all. Every row below pairs the condition it reads with
# something that is NOT true of everything.
#
# Environment: a cluster with no accelerator and a registry it can pull the Mooncake image from.
#              No GPU, no engine image.
# Needs:       a live Mooncake store (the shared fixture brings up backend, pool and Bindings) and an
#              InstanceType, for the roles to reference
#
# Inputs:      All real, nothing mocked: the fixture's pool and Bindings, one additional Binding in
#              the same namespace, one EXTERNAL backend and pool whose master does not resolve, and
#              the engine image the deployments render.
# Expected:    Deployments on the same Binding render the same reuse domain and the third
#              deployment a different one; the probe writes land under the rendered values; the
#              unreachable pool renders no connector at all while the reachable one renders all five
#              carriers, and no row reports a configured cache as an attached one.
# Cleanup:     The EXIT teardown deletes the five deployments and releases the Kueue Workloads that
#              hold their replicas, the two extra Bindings, the probe Pod and the unreachable backend
#              and pool; the shared fixture's teardown covers the rest. Idempotent, and it runs on
#              pass AND fail.
set -uo pipefail

NS="${1:?usage: case-47.sh <NS>}"
CASE_ID=47

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# THE NAMES ARE SET BEFORE THE TRAP IS ARMED, so the teardown can always see them. The unreachable
# backend and pool are CLUSTER-SCOPED, so the suffix is not decoration: two runs sharing a name adopt
# each other's leftovers, which is the failure the library's own notes describe.
DEAD_BACKEND="kvcb-dead-${SFX}"
DEAD_POOL="kvcp-dead-${SFX}"
DEAD_BINDING="bind-dead-${SFX}"
DEAD_DOMAIN="dom-dead-${SFX}"
# RFC 2606 reserves `.invalid`, so this name is guaranteed never to resolve anywhere. An address that
# could accidentally belong to something running is a different experiment from an unreachable one.
DEAD_HOST="case47-nowhere.invalid"
# A SECOND REUSE DOMAIN, which is what the isolation half needs and what the shared fixture does not
# provide. The fixture's other Binding registers the literal `default` so that an engine forwarding
# no tenant can write at all; that is a different job from being a second domain.
OTHER_DOMAIN="case47-other-${SFX}"
OTHER_BINDING="case47-other-${SFX}"

# THE SHARED TEARDOWN IS WRAPPED, NOT REPLACED, AND THE REASON IS THE WARNING RATHER THAN THE
# CLEANUP. That teardown already knows it may have to force a Binding's finalizer off, says so in
# its own comments, calls it the right trade, and prints loudly -- because "a forced action that
# prints nothing reads as a clean one". All of that is correct for the workload it was written for:
# bare Pods that hold nothing, where reaching that branch means something is genuinely stuck.
#
# A ModelDeployment CLAIMS its Binding, and the Binding's finalizer refuses to release while a claim
# stands. So without this wrapper the forced branch is not the exception, it is EVERY RUN. And a
# warning that prints every time is a warning that prints never.
#
# => THE RULE, for whoever writes the next case here: what decides this is whether the workload
# CLAIMS ITS BINDING, not whether the case uses this library. A bare Pod needs nothing; anything the
# Binding's `usedBy` can name has to be removed before the fixture, or it converts that library's
# rare-and-loud path into a routine one.
#
# THAT RULE WAS NECESSARY AND NOT SUFFICIENT. The wrapper removed the deployments and the forced
# branch fired anyway, and it took two more runs to learn why, for two different reasons.
#
# The first of those runs printed the warning with NO name under it, and the diagnosis written here
# from the shape of the code alone -- the SECOND BINDING this case creates, which `kvi_teardown`
# never deletes because it deletes its own two BY NAME -- was a guess dressed as a finding. It is a
# real leak and the delete below closes it, but it was not the object being forced.
#
# => THE SHARED TEARDOWN DELETES BY NAME. Whatever this case created in that namespace, this case
# removes -- claimant or not.
#
# The second run, with that delete in, NAMED the object: `bind-i`, the fixture's own Binding, held at
# `Releasable=False(LedgerNotReleased)` -- "the master will not remove the quota of reuse domain
# ... while it still holds objects". The cause is this case's own store probe. It writes a payload to
# prove the tenant partitions, and a domain holding an object is exactly what the master will not
# release. Nothing to do with a claim, and nothing the delete above could have reached.
#
# => THE PROBE DRAINS WHAT IT WROTE, at the end of the probe below, for the same reason this wrapper
# exists at all: a warning that fires on every run stops being read.
#
# EVERY OBJECT ON THE UNREACHABLE SIDE IS FORCED, and the rule is one rule rather than three special
# cases: an object whose release path talks to the master cannot be released when the master does not
# exist. The Binding drops a tenant from the ledger, the pool reads the ledger to release its quota,
# and the backend waits for the pool -- so all three wedge, permanently, on every run. Two of the
# three are CLUSTER-SCOPED, so nothing else would ever collect them. Forcing here carries no leak to
# chase, and that is a property of THIS fixture rather than of forcing in general: nothing was ever
# registered on a master that never answered.
#
# Wrapping keeps the shared teardown's own order intact rather than reimplementing it, and keeps the
# knowledge of ModelDeployment out of a library that deliberately does not know the CR layer.

# force_delete removes one object, waits for it to go, and forces its finalizer off when it will
# not. The namespace is a parameter because two of the three objects this is used on are
# CLUSTER-SCOPED, and nothing else would ever collect them.
force_delete() {
  local kind="$1" name="$2" ns_args=() left="" hint="kubectl" _
  [ -n "${3:-}" ] && ns_args=(-n "$3")
  [ -n "${3:-}" ] && hint="kubectl -n $3"
  kubectl ${ns_args[@]+"${ns_args[@]}"} delete "$kind" "$name" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 10); do
    left="$(kubectl ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" -o name 2>/dev/null)"
    [ -z "$left" ] && return 0
    sleep 3
  done
  echo "[case-47] ${kind}/${name} did not release in 30s; forcing its finalizer off."
  kubectl ${ns_args[@]+"${ns_args[@]}"} patch "$kind" "$name" --type=merge \
    -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  # THE FORCE IS VERIFIED, because `|| true` on the patch means a failure is silent. Two of the
  # three objects here are CLUSTER-SCOPED and nothing else will ever collect them, so a patch that
  # did not take leaves them Terminating forever -- and a teardown that says nothing reads as one
  # that worked. This does not retry: if the patch failed, saying so is the useful act.
  #
  # THE CHECK IS THE FINALIZER LIST, NOT THE OBJECT'S EXISTENCE. An object whose finalizers really
  # were removed still takes a few seconds to be collected, so "is it gone yet" false-positives on a
  # force that worked -- and a warning that fires when nothing is wrong is one readers learn to
  # ignore, which is the whole reason this branch prints at all.
  sleep 3
  if [ -n "$(kubectl ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" \
    -o jsonpath='{.metadata.finalizers}' 2>/dev/null)" ]; then
    echo "[case-47] ${kind}/${name} IS STILL THERE after the force. Remove it by hand:"
    echo "[case-47]   ${hint} patch $kind $name --type=merge -p '{\"metadata\":{\"finalizers\":null}}'"
  fi
}

# release_deployment deletes one ModelDeployment and releases the Kueue Workload that holds its
# replicas. Kueue holds a finalizer on every Pod of a group and releases it only when the Workload
# is deleted, and the Workload is owned by those same Pods without a controller reference, so the
# delete below is issued WITHOUT waiting and the Workload is released by hand. A regression in the
# operator's own release path then costs this case a wait rather than hanging every case that
# follows.
release_deployment() {
  local md="$1" row wl uids u
  uids="$(kubectl -n "$TEST_NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  kubectl -n "$TEST_NS" delete modeldeployments.worker.gpustack.ai "$md" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$TEST_NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
  return 0
}

# wait_gone waits for the first phase's three deployments to leave the namespace, and says so when
# they do not. A delete issued without a wait is a request, not a fact.
wait_gone() {
  local left=""
  for _ in $(seq 1 30); do
    left="$(kubectl -n "$TEST_NS" get modeldeployments.worker.gpustack.ai \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
    case " $left " in
      *" case47-a "* | *" case47-b "* | *" case47-c "*) ;;
      *) return 0 ;;
    esac
    sleep 3
  done
  echo "[case-47] the first phase's deployments are still present after 90s: ${left}"
  return 0
}

case47_teardown() {
  if [ -n "${TEST_NS:-}" ]; then
    # The deployments first and NOT waited on: each claims its Binding, and a Binding the claim
    # still names is one the shared teardown would sit on for 60s before forcing.
    for md in case47-a case47-b case47-c case47-registered case47-unreachable; do
      release_deployment "$md"
    done
    if [ -n "${OTHER_BINDING:-}" ]; then
      # Issued without waiting: the deployments above are gone by now, so nothing holds this one, and
      # kvi_teardown's own loop is already a wait over every Binding left in the namespace.
      kubectl -n "$TEST_NS" delete kvcachepoolbindings.worker.gpustack.ai "$OTHER_BINDING" \
        --ignore-not-found --wait=false >/dev/null 2>&1
    fi
    force_delete kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" "$TEST_NS"
  fi
  kvi_teardown
  # After kvi_teardown, which takes the namespace with it: these two are cluster-scoped and outlive
  # it, and the pool goes first because the backend is held behind it.
  force_delete kvcachepools.worker.gpustack.ai "$DEAD_POOL"
  force_delete kvcachebackends.worker.gpustack.ai "$DEAD_BACKEND"
}

trap case47_teardown EXIT   # ARM FIRST - kvi_setup can fail with objects already created
kvi_setup

# ONE InstanceType FOR THE WHOLE CASE, discovered once and shared by both halves. The corpus's
# usable_instance_type skips a type that is terminating or inactive, which a naive "first item"
# would happily hand over and which would then refuse every ModelDeployment that names it.
IT="${E2E_MD_INSTANCE_TYPE:-$(md_usable_instance_type)}"

# ONE DEPLOYMENT SHAPE FOR BOTH HALVES, and the engine is the only thing that differs between
# them. The output is READ, not discarded: a refused manifest -- a bad InstanceType name, a
# validation error, a typo -- is otherwise discovered 120 seconds later as an empty tenant or an
# absent connector, and the FAIL that follows guesses among causes that have nothing to do with the
# real one.
deploy() {
  kubectl apply -f - <<YAML 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${TEST_NS}
spec:
  engine:
    name: ${3}
    # Inert here -- the role names its own image, which overrides this one -- but the field is
    # required, and this case tests nothing about an engine, so the same placeholder the role uses
    # stands in for the deployment-wide default too.
    image: ${CLIENT_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${2}
  roles:
  - name: server
    instanceType: ${IT}
    replicas: 1
    image: ${CLIENT_IMAGE}
YAML
}

# EVERY GATE BELOW IS THE OBJECTS' EXISTENCE, NOT THE WORDS THE APPLY PRINTED. Counting ` created`
# lines makes a gate depend on how the objects came to be there rather than on whether they are
# there, and two ordinary situations then fire it with everything present and correct: a re-run
# over a surviving TEST_NS, and the retrying kubectl shim resending an apply whose response was
# lost -- both report `configured`/`unchanged`. Widening the accepted words would keep the gate
# reading the apply's narration, and would also accept a refusal whose text happens to contain one
# of them.
#
# The apply's output is captured at every gate anyway, because a refusal's text is the only
# diagnosis of why one is missing.

# pod_of names the replica a deployment rendered, or nothing. Read off the Pod SPEC, which the
# operator writes at render time -- a Pod that never starts still carries everything below.
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

# rendered_tenant reads the tenant the operator rendered onto one deployment's replica. SGLang
# carries it in one variable on the container; the value is one read straight off the Pod spec, so
# this case stays about the Binding rather than about decoding a config.
rendered_tenant() {
  local pod
  pod="$(pod_of "$1")"
  [ -n "$pod" ] || return 1

  kubectl -n "$TEST_NS" get pod "$pod" -o \
    jsonpath='{.spec.containers[?(@.name=="main")].env[?(@.name=="MOONCAKE_TENANT_ID")].value}' 2>/dev/null
}

# conds reads one object's conditions as `Type=Status/Reason;`. Every test on it guards for empty
# first: `${conds##*pat*}` deletes the longest matching prefix, and deleting anything from "" leaves
# "", so `-z` is TRUE on an empty value -- which is what a cluster with no ModelDeployment controller
# produces, and a row that passes on nothing asserts nothing.
conds_of() {
  local md="$1" want="$2" conds="" _
  for _ in $(seq 1 20); do
    conds="$(kubectl -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason};{end}' 2>/dev/null)"
    [ -n "$conds" ] && [ -z "${conds##*"${want}"*}" ] && break
    sleep 3
  done
  echo "$conds"
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

# =================================================================================================
# PHASE ONE -- the sharing claim: two deployments on one Binding, one on another
# =================================================================================================

# The precondition is asserted ONCE PER PHASE, and the InstanceType behind it is discovered ONCE.
# The two halves each carried their own copy of this row, each gating its own half, and both copies
# are kept here: one shared discovery, two per-phase gates.
phase1=yes
if [ -z "$IT" ]; then
  record FAIL "an instance type exists" "no InstanceType in the cluster; run case-1 first"
  phase1=no
fi

if [ "$phase1" = yes ]; then
  kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${OTHER_BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: ${OTHER_DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quota:
    ceiling: 256Mi
YAML
  if ! kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$OTHER_BINDING" \
    '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
    record FAIL "the second reuse domain is registered" \
      "binding ${OTHER_BINDING} did not reach Ready in 180s, so the isolation half has no second tenant to test"
    phase1=no
  fi
fi

if [ "$phase1" = yes ]; then
  deploy_out="$(deploy case47-a "$BINDING" SGLang; deploy case47-b "$BINDING" SGLang; deploy case47-c "$OTHER_BINDING" SGLang)"
  deploy_missing=""
  for md in case47-a case47-b case47-c; do
    kubectl -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" -o name >/dev/null 2>&1 \
      || deploy_missing="${deploy_missing} ${TEST_NS}/modeldeployment/${md}"
  done
  # A refusal here comes from this operator's own admission (a stale fixture or a regression), not from
  # the environment, so it is a FAIL and this half stops: every row below would read an absence.
  if [ -n "$deploy_missing" ]; then
    record FAIL "the three deployments are admitted" \
      "absent after the apply:${deploy_missing} — the rows below are not run, since each would report \
an absence produced by the refusal. The apply said: \
$(printf '%s' "${deploy_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
    phase1=no
  fi
fi

if [ "$phase1" = yes ]; then
  TA="$(rendered_tenant case47-a)"
  TB="$(rendered_tenant case47-b)"
  TC="$(rendered_tenant case47-c)"

  # EVERY ASSERTION BELOW GUARDS FOR EMPTY FIRST. An engine that stopped forwarding a tenant, a
  # renderer that stopped emitting one, or a Pod that was never created all produce an empty string --
  # and "" equals "" would report the sharing half as PASS while nothing had been rendered at all.
  if [ -z "$TA" ] || [ -z "$TB" ]; then
    record FAIL "the operator renders a tenant for an engine that reads one" \
      "MOONCAKE_TENANT_ID is empty on case47-a ('${TA}') or case47-b ('${TB}'); either the renderer \
stopped emitting it, SGLang stopped carrying the tenant in that variable, or no replica was rendered"
  elif [ "$TA" = "$TB" ] && [ "$TA" = "$DOMAIN" ]; then
    record PASS "two deployments on one Binding are rendered the same reuse domain" \
      "both carry MOONCAKE_TENANT_ID=${TA}, which is the Binding's own domain — sharing is a rendered \
consequence of the Binding rather than a coincidence of defaults"
  else
    record FAIL "two deployments on one Binding are rendered the same reuse domain" \
      "case47-a='${TA}' case47-b='${TB}' expected both to be '${DOMAIN}'"
  fi

  if [ -z "$TC" ]; then
    record FAIL "a deployment on a different Binding is rendered a different domain" \
      "MOONCAKE_TENANT_ID is empty on case47-c, so this row has no value to compare"
  elif [ "$TC" = "$OTHER_DOMAIN" ] && [ "$TC" != "$TA" ]; then
    record PASS "a deployment on a different Binding is rendered a different domain" \
      "case47-c carries ${TC} against ${TA} on the pair above — the domain follows the Binding, so the \
API's own semantics are visible on the Pod before any engine runs"
  else
    record FAIL "a deployment on a different Binding is rendered a different domain" \
      "case47-c='${TC}' expected '${OTHER_DOMAIN}' and expected it to differ from '${TA}'"
  fi
fi

if [ "$phase1" = yes ]; then
  # --- the store side: does the tenant the operator rendered actually partition anything ---
  #
  # GUARDED ON THE VALUES IT IS ABOUT TO USE. With an empty TA or TC the probe below would call setup()
  # with an empty tenant id, and the client either errors -- reported as "nothing was measured" -- or
  # succeeds under the store's own default and produces a FAIL row about ISOLATION that is really the
  # render failure above wearing a different name. Two failures conflated into one row is worse than
  # one row that says it did not run.
  if [ -z "$TA" ] || [ -z "$TC" ]; then
    record SKIP "the rendered tenant partitions the store" \
      "no tenant was rendered (case47-a='${TA}', case47-c='${TC}'), so the store side has nothing to \
partition on — this is the row above failing, not an isolation result"
  else
    kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: case47-probe
  namespace: ${TEST_NS}
spec:
  restartPolicy: Never
  containers:
  - name: client
    image: ${CLIENT_IMAGE}
    command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML
    if ! kvi_wait_for pods case47-probe '{.status.phase}' Running 240 "$TEST_NS" >/dev/null; then
      record SKIP "the rendered tenant partitions the store" \
        "the probe Pod did not reach Running in 240s, so the store side was never exercised — this is \
an environment failure and NOT evidence either way about partitioning"
    else
      # Written under the tenant the operator rendered for the PAIR, then read back under that same
      # tenant and again under the tenant it rendered for the third deployment. The second read is the
      # discriminating one: a store that ignores tenants returns the payload both times.
      PAYLOAD="case47-$(date -u +%s)"
      LOG="$(mktemp)"
      kubectl -n "$TEST_NS" exec case47-probe -c client -- python3 -c "
from mooncake.store import MooncakeDistributedStore as S
def cli(t):
    s = S()
    rc = s.setup('probe-%s' % t, 'P2PHANDSHAKE', 0, 128*1024*1024, 'tcp', '', '${ENDPOINT}', tenant_id=t)
    print('SETUP %s rc=%d' % (t, rc))
    return s
a = cli('${TA}')
print('PUT rc=%d' % a.put('${PAYLOAD}', b'${PAYLOAD}'))
print('SAME len=%d' % len(a.get('${PAYLOAD}') or b''))
c = cli('${TC}')
print('OTHER len=%d' % len(c.get('${PAYLOAD}') or b''))
# The drain, and it belongs to the teardown rather than to any assertion above: a reuse domain that
# still holds an object is one the master refuses to release the quota of, which is what makes the
# shared teardown force a finalizer on every run of this case. Retried on -706 (the write's lease has
# not expired) rather than slept through, because that TTL is a master startup parameter; -704 is
# already-absent and is done, not a failure.
# Every exit from this loop says which one it was. A silent break makes a connection error, an
# unexpected error code and a successful drain look identical -- and the one that matters is
# invisible: the object stays, the shared teardown goes back to forcing a finalizer on every run,
# and nothing in the log says why.
import time
deadline = time.time() + 60
while True:
    rc = a.remove('${PAYLOAD}')
    if rc == 0 or rc == -704:
        print('DRAIN ok rc=%d' % rc)
        break
    if rc != -706:
        print('DRAIN unexpected rc=%d' % rc)
        break
    if time.time() > deadline:
        print('DRAIN gave-up rc=-706 after 60s; the write lease had not expired')
        break
    time.sleep(3)
" >"$LOG" 2>&1

      same="$(grep -m1 '^SAME len=' "$LOG" | sed 's/.*len=//')"
      other="$(grep -m1 '^OTHER len=' "$LOG" | sed 's/.*len=//')"
      if [ -z "$same" ] || [ -z "$other" ]; then
        record SKIP "the rendered tenant partitions the store" \
          "the client produced no SAME/OTHER line, so nothing was measured: $(tr '\n' ' ' <"$LOG" | cut -c1-220)"
      elif [ "$same" -gt 0 ] && [ "$other" -eq 0 ]; then
        record PASS "the rendered tenant partitions the store" \
          "a payload written under ${TA} read back ${same} bytes under that tenant and 0 under ${TC} — \
the value the operator rendered is what the store partitions on"
      elif [ "$same" -gt 0 ]; then
        record FAIL "the rendered tenant partitions the store" \
          "the payload was readable under BOTH ${TA} and ${TC} (${other} bytes), so a second Binding is \
not an isolation boundary at this store version — the API semantics hold and the enforcement does not"
      else
        record FAIL "the rendered tenant partitions the store" \
          "the write did not round-trip under its own tenant (${same} bytes), so this run says nothing \
about isolation: $(tr '\n' ' ' <"$LOG" | cut -c1-220)"
      fi
      rm -f "$LOG"
    fi
  fi
fi

# THE FIRST HALF'S DEPLOYMENTS GO BEFORE THE SECOND HALF'S. Left in place they hold CPU quota and
# claim the fixture's own Binding while the second half's deployments are admitted, and a deployment
# that cannot be admitted renders no replica for the rows below to read. So they are deleted and
# their Kueue Workloads released by hand BEFORE the second half applies its own.
for md in case47-a case47-b case47-c; do
  release_deployment "$md"
done
wait_gone

# =================================================================================================
# PHASE TWO -- the deliberate break: what a deployment looks like when the cache cannot work
# =================================================================================================

phase2=yes
if [ -z "$IT" ]; then
  record FAIL "an instance type exists" "no InstanceType in the cluster; run case-1 first"
  phase2=no
fi

if [ "$phase2" = yes ]; then
  # EXTERNAL rather than managed, and that is what makes it cheap and honest at once: the reconciler
  # renders nothing for an external backend, it only observes, so this fixture starts no workload and
  # occupies no memory. What it describes is a master somebody else runs at an address that does not
  # exist -- which is exactly the shape of a pool that has gone away, without having to break a
  # working one.
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
  quota:
    ceiling: 256Mi
YAML
)"
  # THREE DOCUMENTS, AND ALL THREE ARE READ BACK. `kubectl apply` reports them in ONE stream, so a run
  # where the backend was created and the pool was REFUSED still contains the word "created" -- and
  # every row below would then read a missing condition on an object nobody made, which is the exact
  # substitution this guard exists to prevent. Two of the three are CLUSTER-SCOPED, so each is asked
  # for where it actually lives.
  dead_missing=""
  kubectl get kvcachebackends.worker.gpustack.ai "$DEAD_BACKEND" -o name >/dev/null 2>&1 \
    || dead_missing="${dead_missing} kvcachebackend/${DEAD_BACKEND}"
  kubectl get kvcachepools.worker.gpustack.ai "$DEAD_POOL" -o name >/dev/null 2>&1 \
    || dead_missing="${dead_missing} kvcachepool/${DEAD_POOL}"
  kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$DEAD_BINDING" -o name >/dev/null 2>&1 \
    || dead_missing="${dead_missing} ${TEST_NS}/kvcachepoolbinding/${DEAD_BINDING}"
  # A refusal here comes from this operator's own admission (a stale fixture or a regression), not from
  # the environment, so it is a FAIL and this half stops: nothing below would have a subject.
  if [ -n "$dead_missing" ]; then
    record FAIL "the unreachable pool is admitted" \
      "absent after the apply:${dead_missing} — the rows below are not run, since nothing would have a broken subject to observe. \
The apply said: $(printf '%s' "${dead_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
    phase2=no
  fi
fi

if [ "$phase2" = yes ]; then
  # The precondition, asserted rather than assumed: a Binding whose master does not exist must not
  # report itself usable. Ready here would be the finding -- the operator would be telling every
  # deployment on it that a reuse domain is registered on a master it never reached, and the connector
  # below would then be rendered pointing at an address that answers nothing.
  #
  # The wait is the discrimination: 90 seconds separates "broken" from "not observed yet", and the
  # fixture's own Binding reaches Ready well inside it.
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
fi

if [ "$phase2" = yes ]; then
  # Both name an image explicitly, which is the engine-wide default every role here falls back to.
  #
  # The image carries the Mooncake client and no engine, which is what makes the registered deployment
  # the subject of the first vehicle as well as the control for the second: the operator renders a full
  # vLLM connector onto a container that could never run vLLM, because it inspects no image.
  #
  # The two applies are still captured SEPARATELY rather than together, because the diagnosis is what
  # each capture is for: one stream would attribute a refusal to whichever name the reader guessed.
  u_out="$(deploy case47-unreachable "$DEAD_BINDING" vLLM)"
  r_out="$(deploy case47-registered "$BINDING" vLLM)"
  md_missing=""
  for md in case47-unreachable case47-registered; do
    kubectl -n "$TEST_NS" get modeldeployments.worker.gpustack.ai "$md" -o name >/dev/null 2>&1 \
      || md_missing="${md_missing} ${TEST_NS}/modeldeployment/${md}"
  done
  # Same reasoning as the gate above: an absent deployment was refused by this operator.
  if [ -n "$md_missing" ]; then
    record FAIL "both deployments are admitted" \
      "absent after the apply:${md_missing} — the rows below are not run, since each would report an \
absence produced by the refusal. case47-unreachable said: ${u_out:-<no output>} / \
case47-registered said: ${r_out:-<no output>}"
    phase2=no
  fi
fi

if [ "$phase2" = yes ]; then
  U_CONDS="$(conds_of case47-unreachable 'DomainRegistered=')"
  if [ -n "$U_CONDS" ] && [ -z "${U_CONDS##*DomainRegistered=False/BindingNotReady*}" ]; then
    record PASS "an unreachable pool is reported as a Binding that is not ready, not as one missing" \
      "DomainRegistered=False/BindingNotReady — the Binding exists, so BindingNotFound would send an \
admin to create an object that is already there"
  else
    record FAIL "an unreachable pool is reported as a Binding that is not ready, not as one missing" \
      "wanted DomainRegistered=False/BindingNotReady, got: ${U_CONDS:-<no conditions at all within 60s>}"
  fi
fi

if [ "$phase2" = yes ]; then
  # The replicas ARE built on both sides: convergence is not gated on DomainRegistered, so a
  # deployment whose pool is unreachable still gets its replica.
  POD_U="$(pod_of case47-unreachable)"
  POD_R="$(pod_of case47-registered)"
  if [ -z "$POD_U" ] || [ -z "$POD_R" ]; then
    record FAIL "a deployment on an unreachable pool renders no connector at all" \
      "no replica appeared within 120s for case47-unreachable ('${POD_U}') or case47-registered \
('${POD_R}'), so there is no rendered spec to read"
  else
    MU="$(markers "$POD_U")"
    MR="$(markers "$POD_R")"
    if [ -z "${MR%%5:*}" ] && [ -z "${MU%%0:*}" ]; then
      record PASS "a deployment on an unreachable pool renders no connector at all" \
        "case47-registered carries all five carriers (${MR#*:}) and case47-unreachable carries none — \
not a partial one, which is the outcome that would look like a cache miss from outside the Pod \
instead of like a replica that was never configured"
    elif [ -n "${MR%%5:*}" ]; then
      record FAIL "a deployment on an unreachable pool renders no connector at all" \
        "the READY-Binding side rendered ${MR} instead of all five carriers, so the absence on the \
other side is not evidence: a renderer that emits nothing anywhere produces it too"
    else
      record FAIL "a deployment on an unreachable pool renders no connector at all" \
        "case47-unreachable rendered ${MU} against ${MR} on the control — a connector pointed at a \
pool that answers nothing is the one outcome the resolver returns nil to avoid"
    fi
  fi
fi

if [ "$phase2" = yes ]; then
  # --- configured is not attached ---
  #
  # The pair on ONE object is what makes this an observation rather than a restatement of the cluster's
  # shape. DomainRegistered=True says the operator did every part of its job; CacheAttached says
  # nothing observed a cache, on the same deployment, in the same pass. An implementation that raised
  # CacheAttached from a successful render would go red here, and only here -- `CacheAttached != True`
  # on its own is satisfied by every deployment on this cluster.
  R_CONDS="$(conds_of case47-registered 'CacheAttached=')"
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
      "DomainRegistered=True/Registered and CacheAttached=${R_CA} on case47-registered — the operator \
rendered the whole connector onto an image that carries no engine, and claims nothing about a cache \
no replica ever reported on"
  else
    record FAIL "a rendered connector is not reported as an attached cache" \
      "wanted DomainRegistered=True/Registered beside CacheAttached=Unknown/*, got: ${R_CONDS}"
  fi
fi

if [ "$phase2" = yes ]; then
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
fi

kvi_results "$CASE_ID"
