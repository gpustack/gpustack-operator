#!/usr/bin/env bash
#
# CASE 53 — Pod KV cache injection, end to end: the bytes move, the refusals say what to change,
#            and the engine's own library accepts what was written
#   (MUTATING, self-recovering, AUTO-SKIPS the LWS half without the LeaderWorkerSet CRD and each
#    engine half without its image)
#
#   case-53.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_ENGINE_READY_TIMEOUT,E2E_SGLANG_IMAGE,E2E_VLLM_IMAGE
# Goal:        One case, seven sections, one fixture. The injection webhook decides what a Pod may
#              do with a KV cache pool, and every half of that decision has to hold on a real API
#              server: an ordinary Pod can use a pool it was merely opted into; the mechanism does
#              not care what kind of workload carries the label; the record written onto the Pod
#              names the reuse domain the engine reads; a Pod that did not opt in comes back
#              untouched; every refusal says what to change; and the engine's OWN reader and
#              factory accept the rendered file and name.
#
#              The sections run in this order, and the order carries meaning:
#
#                1. a plain Deployment reads and writes the pool    - the bytes really move
#                2. a bare Pod and a LeaderWorkerSet get the same
#                   injection                                        - the label decides
#                3. the stamp and the tenant                         - what was written is recorded
#                4. a Pod that did not opt in is untouched           - the bound of the blast radius
#                5. the refusals, each naming its fix                - the messages are actionable
#                6. the engine's own reader accepts the file         - the schema is real
#                7. the engine's own factory knows the name          - the name is real
#
#              A failed precondition ends ITS OWN SECTION and not the case. The sections are
#              independent, and a run that reported one section's failure as the end of the whole
#              case would leave the other six unverified and say so nowhere.
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and two KVCachePoolBindings, plus one of its own in the refusals section, so it needs
#              no pool to pre-exist. The LWS section AUTO-SKIPS without the
#              leaderworkerset.x-k8s.io CRD, and each engine section AUTO-SKIPS without its own image
#              (E2E_VLLM_IMAGE, E2E_SGLANG_IMAGE, E2E_VLLM_ASCEND_IMAGE). Every skip names what it
#              left unverified.
# Inputs:      real Pods, a Deployment and a LeaderWorkerSet, each carrying the inject label and the
#              binding and engine annotations. Nothing is mocked: the objects under test are the
#              objects admission rewrites. The probe in section 1 is a fixture standing in for an
#              engine that consumes the injected tenant, and the launch stub every fixture runs is a
#              fixture too - no engine is asked to serve inference.
# Expected:    the master's used-bytes figure moves on the declared domain and stays at zero on the
#              "default" one; a bare Pod and an LWS Pod carry identical injected artifacts; the
#              stamp names the Binding's domain and records the tenant as an action; a Pod that did
#              not opt in is stored exactly as submitted; every malformed submission is refused with
#              a message naming the annotation, container, key or object to change; and the engine's
#              own configuration reader and connector factory accept the rendered file and name.
#
#              SKIPS: only the LWS half, the two engine halves and the vLLM-Ascend row, each on its
#              own precondition and each recorded as a SKIP naming what is left unverified - never
#              as a pass. The footer counts skips separately, so a run where a section verified
#              nothing cannot be read as a run that verified everything.
# Cleanup:     the trap removes the Pods, the Deployment, the Bindings, the namespace, the pool and
#              the backend, in that order and idempotently, on pass AND fail. The Binding is given
#              60s before its finalizer is forced: a domain still holding objects makes the master
#              refuse to drop its quota, and forcing it earlier is how a run leaves a namespace
#              Terminating forever. It changes no shared baseline - every object it touches is one it
#              created.
set -uo pipefail

NS="${1:?usage: case-53.sh <NS>}"
CASE_ID=53
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

# The volume and mount this webhook injects, by the name it uses (inject.ConfigVolumeName). A Pod that
# did not opt in must carry neither, and section 4 compares a stored Pod against a submitted one by
# exactly this name.
CONFIG_VOLUME=gpustack-kvcache-config

# The timeout the two engine sections wait for their Pods to become Ready. An engine image is
# several GB, so the default is generous.
E2E_ENGINE_READY_TIMEOUT="${E2E_ENGINE_READY_TIMEOUT:-1200}"

# A section that cannot go on - a failed precondition, or a half this cluster does not have - stops
# HERE, and the next section still runs. The sections are independent of each other, so a run that
# reported one section's stop as the end of the case would leave the other six unverified and would
# not say so anywhere.
# Each section sets SECTION_OK=1 at its start, so a guard never reads a value the section before it
# left behind.
SECTION_OK=1
section_stop() {
  SECTION_OK=0
  echo "[case-53] $1 stopped here; the checks after it in this section did not run"
}

# =============================================================================================
# Section 1 - a plain Deployment reads and writes the pool
#
# The headline: a Pod with no queue-name label, no workload CR and no owner reference from this
# operator opts into a pool with one label and two annotations, and then moves real bytes in the
# master. The read/write is done with the CLIENT LIBRARY rather than a running engine. The fixture
# reads everything from the projected file, the tenant included: it calls setup() with the seven
# positional arguments and the file's tenant_id as a keyword, which is what a vLLM-family engine that
# consumes the injected tenant does. Because the tenant comes from the file, where the bytes are
# charged is a reading of what the webhook injected.
# =============================================================================================
SECTION_OK=1
PAYLOAD="the-quick-brown-fox-${SFX}"
PROBE_LOG="/tmp/kvc-inject-53-${SFX}.log"

# A plain Deployment. No queue-name label, no InstanceType, no owner reference from this operator -
# which is the whole point of the section.
#
# The container declares the vLLM entry point and runs kvi_setup's stub behind it. That shape is not
# this section's subject; it is what admission accepts, and the reasoning is with the stub in
# _kvcache-inject-lib.sh. What matters here is that the probe below execs the image's OWN python3,
# from PATH, so it reaches the mooncake client rather than the stub.
kubectl apply -f - <<YAML >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: plain
  namespace: ${TEST_NS}
spec:
  replicas: 1
  selector:
    matchLabels: {app: plain}
  template:
    metadata:
      labels:
        app: plain
        kvcache.gpustack.ai/inject: "true"
      annotations:
        kvcache.gpustack.ai/binding: ${BINDING}
        kvcache.gpustack.ai/engine: vLLM
    spec:
      volumes:
        - name: ${LAUNCH_VOLUME}
          configMap:
            name: ${LAUNCH_CONFIGMAP}
            defaultMode: 0755
            items:
              - key: launch
                path: vllm
      containers:
        - name: engine
          image: ${CLIENT_IMAGE}
          command: ["${LAUNCH_DIR}/vllm", "serve"]
          volumeMounts:
            - name: ${LAUNCH_VOLUME}
              mountPath: ${LAUNCH_DIR}
YAML

POD=""
for _ in $(seq 1 40); do
  POD="$(kubectl -n "$TEST_NS" get pods -l app=plain -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$POD" ] && break
  sleep 3
done
if [ -z "$POD" ]; then
  record FAIL "the Deployment produced a Pod" "no Pod appeared in 120s; the webhook may have refused it"
  section_stop "Section 1"
fi

# The injection landed, and it landed on a Pod nothing else in this operator owns.
if [ "$SECTION_OK" -eq 1 ]; then
  if [ -n "$(kvi_env "$POD" MOONCAKE_CONFIG_PATH)" ]; then
    record PASS "a plain Deployment's Pod is injected" \
      "MOONCAKE_CONFIG_PATH is set on ${POD}, which carries no queue-name label and no owner from this operator"
  else
    record FAIL "a plain Deployment's Pod is injected" \
      "MOONCAKE_CONFIG_PATH is absent on ${POD}"
    section_stop "Section 1"
  fi
fi

# The wait's outcome is USED rather than discarded. A Pod that is not Running makes the exec below
# fail at once, and its empty log reaches the checks as "the probe printed no SETUP line" - a timing
# artifact wearing the costume of an injection defect. A slow image pull is a documented failure mode
# in this family: section 6 once timed out on a 19.1GB one.
if [ "$SECTION_OK" -eq 1 ]; then
  if ! kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/${POD}" --timeout=180s >/dev/null 2>&1; then
    record FAIL "the injected file is one the client accepts" \
      "the Pod never became Ready in 180s, so the probe never ran - this says NOTHING about what was \
injected. Most likely the image is still pulling; pre-pull it or raise the timeout and run again"
    section_stop "Section 1"
  fi
fi

if [ "$SECTION_OK" -eq 1 ]; then
  # The probe. It parses the file the webhook projected - not an address this case knows - and calls
  # setup() with the seven positional arguments plus the file's tenant_id. The library carries the probe
  # itself; this call writes what it printed to the log the rows below read.
  kvi_store_roundtrip_py "$POD" engine "case53-${SFX}" "$PAYLOAD" >"$PROBE_LOG" 2>&1

  if grep -q '^SETUP rc=0' "$PROBE_LOG"; then
    record PASS "the injected file is one the client accepts" \
      "setup() returned 0 from the projected file ($(grep -m1 '^TENANT' "$PROBE_LOG"))"
  else
    record FAIL "the injected file is one the client accepts" \
      "$(grep -m1 '^SETUP' "$PROBE_LOG" || echo 'the probe printed no SETUP line'); see ${PROBE_LOG}"
  fi

  if grep -q '^GET .*match=True' "$PROBE_LOG"; then
    record PASS "the Pod reads and writes the pool" \
      "a put and a get round-tripped ${#PAYLOAD} bytes through the master at ${ENDPOINT}"
  else
    record FAIL "the Pod reads and writes the pool" \
      "$(grep -m1 '^GET' "$PROBE_LOG" || echo 'the probe printed no GET line'); see ${PROBE_LOG}"
  fi

  # Usage is the master's own figure, not this section's arithmetic: it is what makes "the bytes
  # moved" a statement about the store rather than about the probe's return codes. It is read off the
  # Binding that carries the declared domain, because the probe wrote under the tenant the file
  # named: usage there is a reading of what the webhook injected, and a missing or wrong tenant_id
  # leaves it at zero.
  usage=""
  for _ in $(seq 1 20); do
    usage="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING" \
      -o jsonpath='{.status.usage}' 2>/dev/null)"
    [ -n "$usage" ] && [ "$usage" != "0" ] && break
    sleep 3
  done
  if [ -n "$usage" ] && [ "$usage" != "0" ]; then
    record PASS "the master's used-bytes figure moved on the declared domain" \
      "${BINDING} reports usage ${usage}"
  else
    record FAIL "the master's used-bytes figure moved on the declared domain" \
      "usage is '${usage:-<absent>}' on ${BINDING} 60s after the write; absent is not the same as zero. \
Zero with the round-trip above passing means the write was charged to another tenant - read the \
probe's TENANT line in ${PROBE_LOG}"
  fi

  # The paired half: the "default" Binding must report NOTHING. Without it the check above cannot
  # tell "the write carried the declared domain" from "usage moves on whichever Binding you ask".
  # Polled until the figure EXISTS, and only then required to be zero: status.usage is a pointer
  # with omitempty so that "granted zero" and "never observed" do not serialize the same way, and
  # accepting absence would pass on a cluster where the reconciler had not run at all.
  default_usage=""
  for _ in $(seq 1 20); do
    default_usage="$(kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$BINDING_DEFAULT" \
      -o jsonpath='{.status.usage}' 2>/dev/null)"
    [ -n "$default_usage" ] && break
    sleep 3
  done
  if [ -z "$default_usage" ]; then
    record FAIL "the default tenant is charged nothing" \
      "${BINDING_DEFAULT} still reports no usage figure after 60s, so its status never converged; absent \
is not zero, and this run cannot say where the bytes were charged"
  elif [ "$default_usage" = "0" ]; then
    record PASS "the default tenant is charged nothing" \
      "${BINDING_DEFAULT} reports 0 while the same bytes moved under the declared domain"
  else
    record FAIL "the default tenant is charged nothing" \
      "${BINDING_DEFAULT} reports usage ${default_usage}: the write fell back to the store's default \
tenant, so the injected file carried no usable tenant_id"
  fi
fi

# =============================================================================================
# Section 2 - a bare Pod and a LeaderWorkerSet get the identical injection
#
# The mechanism is not shaped for Deployments: the webhook triggers on a LABEL and reads
# ANNOTATIONS, so anything that can put those on a Pod template - a third-party operator's CR, an
# LWS, a hand-written Pod - gets the same result. The assertion is the strong form: the two Pods'
# injected artifacts are compared to each other FIELD BY FIELD, not each checked against a list. A
# per-workload difference would show up as an inequality without anyone having to predict which
# field it would be.
#
# The LWS half SKIPs loudly when the CRD is absent, rather than passing quietly: an absent CRD
# proves nothing about workload independence, and reporting it as a pass is the failure this suite
# exists to avoid.
# =============================================================================================
SECTION_OK=1

# injected_shape prints the parts of a Pod this webhook is allowed to write, sorted so two Pods can be
# compared directly. The Pod name is deliberately absent from it: it is the one thing that must differ.
injected_shape() {
  local pod="$1"
  {
    # Filtered to the variables this webhook writes, for the same reason the volume lines below are:
    # a controller above the Pod adds its own. LWS gives every container LWS_LEADER_ADDRESS,
    # LWS_GROUP_SIZE and LWS_WORKER_INDEX, so an unfiltered list makes the two shapes differ on keys
    # this webhook never touched - and the LWS half would fail on its first real run, having only
    # ever SKIPled. MOONCAKE_ and MC_ are every prefix injectPod can write: the renderers' keys and
    # the two observability defaults.
    # WHAT THIS GIVES UP: a variable this webhook should NOT have written goes unseen here. That is
    # section 4's question - submitted shape versus stored shape - and this section does not ask it.
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].env[*]}ENV {.name}={.value}{"\n"}{end}' 2>/dev/null \
      | grep -E '^ENV (MOONCAKE_|MC_)' || true
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].args[*]}ARG {@}{"\n"}{end}' 2>/dev/null
    # Filtered to the volume this webhook injects, for the same reason section 4 filters: the API
    # server projects a service-account token volume onto every Pod under a RANDOM name, so listing
    # all of them would make two Pods differ on a field neither this webhook nor this section
    # controls. On a cluster where the LWS half actually runs, that would fail the comparison for no
    # real reason.
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.containers[0].volumeMounts[?(@.name=='${CONFIG_VOLUME}')]}MOUNT {.name}@{.mountPath} ro={.readOnly}{'\n'}{end}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.volumes[?(@.name=='${CONFIG_VOLUME}')]}VOL {.name}{'\n'}{end}" 2>/dev/null
    echo "STAMP engine=$(kvi_stamp "$pod" engine) vehicle=$(kvi_stamp "$pod" vehicle) tenantInjected=$(kvi_stamp "$pod" tenantInjected)"
  } | sort
}

# A bare Pod: no controller of any kind above it.
kvi_pod_manifest bare vLLM | kubectl apply -f - >/dev/null 2>&1
if ! kvi_wait_for pods bare '{.metadata.name}' bare 60 "$TEST_NS" >/dev/null; then
  record FAIL "a bare Pod is admitted" "the Pod never appeared; the webhook may have refused it"
  section_stop "Section 2"
fi

if [ "$SECTION_OK" -eq 1 ]; then
  BARE_SHAPE="$(injected_shape bare)"
  if echo "$BARE_SHAPE" | grep -q 'ENV MOONCAKE_CONFIG_PATH='; then
    record PASS "a bare Pod is injected" "no controller above it, and it carries the full artifact set"
  else
    record FAIL "a bare Pod is injected" "MOONCAKE_CONFIG_PATH is absent"
    section_stop "Section 2"
  fi
fi

# The LWS half. An absent CRD is a SKIP that says so, never a pass.
if [ "$SECTION_OK" -eq 1 ]; then
  if ! kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io >/dev/null 2>&1; then
    record SKIP "an LWS Pod gets the identical injection" \
      "leaderworkerset.x-k8s.io is not installed on this cluster; workload independence is UNVERIFIED \
against LWS, and this case asserts it only for the bare Pod"
    section_stop "Section 2"
  fi
fi

if [ "$SECTION_OK" -eq 1 ]; then
  kubectl apply -f - <<YAML >/dev/null
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: lws
  namespace: ${TEST_NS}
spec:
  replicas: 1
  leaderWorkerTemplate:
    size: 1
    leaderTemplate:
      metadata:
        labels:
          kvcache.gpustack.ai/inject: "true"
        annotations:
          kvcache.gpustack.ai/binding: ${BINDING}
          kvcache.gpustack.ai/engine: vLLM
      spec:
        volumes:
          - name: ${LAUNCH_VOLUME}
            configMap:
              name: ${LAUNCH_CONFIGMAP}
              defaultMode: 0755
              items:
                - key: launch
                  path: vllm
        containers:
          - name: engine
            image: ${CLIENT_IMAGE}
            command: ["${LAUNCH_DIR}/vllm", "serve"]
            volumeMounts:
              - name: ${LAUNCH_VOLUME}
                mountPath: ${LAUNCH_DIR}
    # The worker carries no inject label, so this webhook never sees it and it needs no admissible
    # launch. It is left as a plain keep-alive on purpose: the pair is what shows the label decides.
    workerTemplate:
      spec:
        containers:
          - name: worker
            image: ${CLIENT_IMAGE}
            command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML

  LWS_POD=""
  for _ in $(seq 1 40); do
    LWS_POD="$(kubectl -n "$TEST_NS" get pods -l leaderworkerset.sigs.k8s.io/name=lws \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$LWS_POD" ] && break
    sleep 3
  done
  if [ -z "$LWS_POD" ]; then
    record FAIL "an LWS Pod gets the identical injection" "no LWS Pod appeared in 120s"
    section_stop "Section 2"
  fi

  if [ "$SECTION_OK" -eq 1 ]; then
    LWS_SHAPE="$(injected_shape "$LWS_POD")"
    if [ "$BARE_SHAPE" = "$LWS_SHAPE" ]; then
      record PASS "an LWS Pod gets the identical injection" \
        "env, args, mounts, volumes and stamp are byte-identical to the bare Pod's"
    else
      record FAIL "an LWS Pod gets the identical injection" \
        "the two differ: $(diff <(echo "$BARE_SHAPE") <(echo "$LWS_SHAPE") | tr '\n' ' ' | cut -c1-200)"
    fi
  fi
fi

# =============================================================================================
# Section 3 - the stamp that replaces isolation, and the control that makes it mean something
#
# This is the honesty gate. A Binding declares a reuse domain, and the webhook writes that domain
# into every engine it configures - the vLLM family reads it as `tenant_id` from its projected
# client config, SGLang as MOONCAKE_TENANT_ID from its environment. The webhook never refuses over
# whether the engine BUILD will honour it - refusing would mean one namespace's Binding stops
# another namespace's Pods, and a Pod's author caused none of it - so the injection record is where
# the decision is visible.
#
# What the record says is deliberately an ACTION and not an outcome: `tenantInjected`, never
# "isolated". Whether an injected tenant takes effect depends on the engine BUILD, and admission
# never inspects the image, so no engine version is stamped anywhere - a record naming one would
# claim a fact nothing measured. A Pod handed a variable its build predates would otherwise be
# stamped as isolated while sharing a cache. Over-claiming in that direction is the failure this
# section exists to prevent.
#
# The control is the ARTIFACT, per engine. Every engine now receives the tenant, so a stamp reading
# "injected" everywhere is the correct answer rather than a suspicious one; what separates it from a
# stamp hard-coded that way is that each engine's own vehicle carries the Binding's domain, and the
# OTHER engine's vehicle does not. A webhook that stamped the action without writing it, or wrote it
# where the engine does not read it, fails on one side or the other.
#
# NOT COVERED HERE: the other answer, tenantInjected=false. It is reached only through a pool whose
# master holds no tenant ledger (QuotaLedgerAvailable False for MultiTenancyDisabled), and this
# fixture builds a multi-tenant one; the renderer and the resolver pin it in
# TestRender_TenantOmittedForAnEmptyDomain and TestPodKVCacheResolve_QuotaLedgerGate.
#
# ONE SKIP, and it is MEASURED rather than declared: the vLLM-Ascend row of the control loop. That
# value is no longer one the engine annotation takes, and the spelling that replaces it needs an
# ascend-transport pool this fixture does not build - so the row is unreachable rather than unlucky.
# The loop does not take that on trust: it submits the row as a server-side dry run and skips only
# on an actual refusal, printing what the server said, so the skip retires itself if either refusal
# is ever lifted. A dry run that fails for any OTHER reason is a FAILURE, not a skip. The count the
# loop is held to is derived from what was skipped for the same reason, and a count that does not add
# up is recorded rather than passed over. An engine whose Pod never appears is still a FAILURE.
# =============================================================================================
SECTION_OK=1

kvi_pod_manifest stamped vLLM | kubectl apply -f - >/dev/null 2>&1
if ! kvi_wait_for pods stamped '{.metadata.name}' stamped 60 "$TEST_NS" >/dev/null; then
  record FAIL "a domain-carrying Binding is injected, not refused" \
    "the Pod never appeared - if the webhook refused it, the isolation gap has become a denial of \
service for a Pod whose author did not create the domain"
  section_stop "Section 3"
fi

if [ "$SECTION_OK" -eq 1 ]; then
  record PASS "a domain-carrying Binding is injected, not refused" \
    "the Pod was admitted; the gap is reported rather than pushed onto its author"

  stamped_domain="$(kvi_stamp stamped domain)"

  # There is deliberately no assertion here that the domain "is" or "is not" enforced. The stamp used
  # to carry that, and it could not be honest: whether an engine honours an injected tenant depends
  # on its BUILD, and admission never looks at the image. A Pod handed a variable its build predates
  # would have been stamped as isolated while sharing a cache. What is asserted instead is the
  # action taken, below. The same honesty rule is why no engine version is read off the stamp either:
  # the record carries what admission did, never what it measured nothing about.

  if [ "$stamped_domain" = "$DOMAIN" ]; then
    record PASS "the stamp names the Binding's domain" "domain=${stamped_domain}"
  else
    record FAIL "the stamp names the Binding's domain" \
      "domain='${stamped_domain:-<absent>}', expected '${DOMAIN}'"
  fi

  # The tenant as each vehicle carries it, "config=<tenant_id in the projected client config>
  # env=<the container's MOONCAKE_TENANT_ID>", either side empty when absent. Both are read for
  # every engine, because the control below is that each engine's own vehicle carries the domain AND
  # the other one does not: a tenant written where the engine does not read it is decoration that
  # reads as a guarantee.
  tenant_vehicles() {
    local pod="$1" cfg env
    cfg="$(kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={.metadata.annotations.kvcache\.gpustack\.ai/client-config}" 2>/dev/null \
      | python3 -c "import json,sys; d=sys.stdin.read().strip(); print(json.loads(d).get('tenant_id','') if d else '')" 2>/dev/null)"
    env="$(kvi_env "$pod" MOONCAKE_TENANT_ID)"
    echo "config=${cfg} env=${env}"
  }

  # What was DONE about the tenant. The webhook writes a non-empty domain into every engine it
  # configures and leaves compatibility with the image to whoever chose it; an engine build that
  # reads no tenant writes under the store's own "default" name, which is why a pool serving one
  # needs a Binding registering that name. The stamp records the write, never whether isolation
  # resulted.
  injected="$(kvi_stamp stamped tenantInjected)"
  if [ "$injected" = True ] || [ "$injected" = true ]; then
    record PASS "the stamp records the tenant ACTION, and for vLLM it is 'written'" \
      "tenantInjected=${injected}: the domain was written into this engine's client config. The field \
says what was DONE, never whether isolation resulted - that depends on the engine build, which is not \
checked here"
  else
    record FAIL "the stamp records the tenant ACTION, and for vLLM it is 'written'" \
      "tenantInjected='${injected:-<absent>}' on a vLLM Pod against a domain-carrying Binding; every \
engine is given the tenant"
  fi

  # The vLLM family's vehicle is the projected client config, and only that: MOONCAKE_TENANT_ID is
  # SGLang's, and this engine does not read it.
  got="$(tenant_vehicles stamped)"
  if [ "$got" = "config=${DOMAIN} env=" ]; then
    record PASS "the reuse domain reaches a vLLM container through its client config" \
      "tenant_id=${DOMAIN} in the projected config, and no MOONCAKE_TENANT_ID - that variable is \
SGLang's vehicle and this engine does not read it"
  else
    record FAIL "the reuse domain reaches a vLLM container through its client config" \
      "read ${got}; expected config=${DOMAIN} and no environment variable"
  fi

  # The paired half, on its own Pod because the engine is fixed per Pod.
  kvi_pod_manifest sg-stamped SGLang | kubectl apply -f - >/dev/null 2>&1
  if ! kvi_wait_for pods sg-stamped '{.metadata.name}' sg-stamped 60 "$TEST_NS" >/dev/null; then
    record FAIL "the reuse domain reaches an SGLang container as its tenant" \
      "the Pod was never stored, so the paired half did not run and the vLLM half above is unpaired: \
nothing here shows the webhook routes the tenant by engine at all"
  else
    sg_tenant="$(kvi_env sg-stamped MOONCAKE_TENANT_ID)"
    if [ "$sg_tenant" = "$DOMAIN" ]; then
      record PASS "the reuse domain reaches an SGLang container as its tenant" \
        "MOONCAKE_TENANT_ID=${sg_tenant}, which is the Binding's own reuse domain"
    else
      record FAIL "the reuse domain reaches an SGLang container as its tenant" \
        "MOONCAKE_TENANT_ID='${sg_tenant:-<absent>}', expected '${DOMAIN}'"
    fi
  fi

  # THE CONTROL. It used to require both stamp answers to appear, because the compiled table then
  # held both - some engines were given a tenant and some were not. The tenant is unconditional now,
  # so "injected" on every engine is the correct reading and the table has one answer; requiring two
  # would fail against a correct webhook. What still separates a real decision from a hard-coded one
  # is the artifact: each engine carries the domain in its OWN vehicle and not in the other's, so the
  # loop requires the stamp and the artifact to agree per engine, and requires BOTH vehicles to have
  # been observed across the engines it reached.
  ok=1
  checked=0
  skipped=0
  seen_config=0
  seen_env=0
  for engine in vLLM vllm-ascend SGLang; do
    pod="ctl-${engine//-/}"
    pod="${pod,,}"
    # vLLM-Ascend is not reachable from this fixture, and TWO refusals stand in front of it - neither
    # of them about the launch:
    #
    #  1. The engine annotation does not take the value. `vllm_ascend` was ruled the package the
    #     runner installs when the accelerator backend is CANN rather than an engine anybody names, so
    #     ParseEngine refuses it and names `engine: vLLM` + `manufacturer: ascend` as the spelling.
    #  2. Spelled that way it is refused one layer down: that runtime accepts only the `ascend`
    #     transport, and this family's pool is built on a TCP backend.
    #
    # THE SKIP IS ASKED FOR, NOT ASSERTED. Both refusals live in the webhook, which is the moving part
    # here - so this submits the row's own manifest as a server-side dry run and skips only on an
    # actual refusal. Hard-coding it would have written a claim about admission into this file, where
    # it would go stale silently: the row would keep skipping, `checked` would keep adding up, and the
    # case would stay green while covering one engine less than it says. Asked this way the skip
    # retires itself the day either refusal is lifted, and the reason it prints is the server's own.
    #
    # It is asked before the expectation below is consulted, because the expectation is not what stops
    # the row - and it is a SKIP rather than a FAIL naming a Pod that never appeared, which is what a
    # reader would otherwise go and chase.
    #
    # The Go side pins the answer where the engine IS reachable: TestRender_TenantGoesToEveryEngineThatReadsOne
    # renders every engine, vLLM-Ascend included. What no test replaces is the cluster half.
    if [ "$engine" = vllm-ascend ]; then
      why="$(kvi_admission_refuses "$(kvi_pod_manifest "$pod" "$engine")")" && rc=0 || rc=$?
      case "$rc" in
        0)
          skipped=$((skipped + 1))
          record SKIP "the stamp and the artifact agree per engine (${engine})" \
            "admission refuses this row on this fixture, so its tenant is UNVERIFIED on a cluster - it \
is pinned only in the inject package's own test (TestRender_TenantGoesToEveryEngineThatReadsOne). The \
server said: ${why}"
          continue
          ;;
        2)
          ok=0
          record FAIL "the stamp and the artifact agree per engine" \
            "engine ${engine}: the dry run failed for something other than this webhook's refusal, so \
whether the row is reachable was never established and it was neither run nor skipped: ${why}"
          continue
          ;;
      esac
      # rc=1: admission accepts it now, so the row RUNS like any other - which is the whole point of
      # asking rather than deciding here.
    fi
    # The vehicle this engine reads the tenant from; the other one must stay empty.
    case "$engine" in
      SGLang) want="config= env=${DOMAIN}" ;;
      *)      want="config=${DOMAIN} env=" ;;
    esac
    kvi_pod_manifest "$pod" "$engine" | kubectl apply -f - >/dev/null 2>&1
    if ! kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null; then
      # A control that did not run is recorded, never passed over. Skipping it silently would leave a
      # smaller set of engines checked while the summary below still claimed all of them.
      ok=0
      record FAIL "the stamp and the artifact agree per engine" \
        "engine ${engine} never produced a Pod, so its answer was not observed at all"
      continue
    fi
    checked=$((checked + 1))
    stamp="$(kvi_stamp "$pod" tenantInjected)"
    got="$(tenant_vehicles "$pod")"
    case "$stamp" in
      True | true) ;;
      *) ok=0; record FAIL "the stamp and the artifact agree per engine" \
           "engine ${engine} stamped tenantInjected='${stamp:-<absent>}' against a domain-carrying Binding"
         continue ;;
    esac
    if [ "$got" != "$want" ]; then
      ok=0
      record FAIL "the stamp and the artifact agree per engine" \
        "engine ${engine} stamped the write, but its vehicles read ${got}; expected ${want}"
      continue
    fi
    case "$engine" in
      SGLang) seen_env=1 ;;
      *)      seen_config=1 ;;
    esac
  done
  # The count the loop is held to is DERIVED from what was skipped, never written as a constant. The
  # skip above is asked of admission and can stop firing, and a hard-coded 2 would then leave a third
  # row running with nothing holding the loop to it - no PASS, no FAIL, the case green by omission.
  if [ "$ok" -eq 1 ] && [ "$checked" -eq $((3 - skipped)) ]; then
    if [ "$seen_config" -eq 1 ] && [ "$seen_env" -eq 1 ]; then
      record PASS "the stamp and the artifact agree per engine" \
        "${checked} of 3 engines observed and ${skipped} skipped as unreachable; each stamped the write \
and carried the domain in its own vehicle only, and BOTH vehicles appeared - which is what separates a \
per-engine write from a stamp hard-coded to 'injected'. A skipped engine is NOT part of this claim"
    else
      record FAIL "the stamp and the artifact agree per engine" \
        "all ${checked} observed but only one vehicle appeared, so this run cannot tell a per-engine \
write from one written the same way everywhere"
    fi
  elif [ "$ok" -eq 1 ]; then
    # Counts that do not add up, with no FAIL from the loop to explain them. Recorded rather than
    # passed over, because the loop's own rule is that a control which did not run is never dropped in
    # silence - and this is the one arm where that could otherwise happen.
    record FAIL "the stamp and the artifact agree per engine" \
      "observed ${checked} engines and skipped ${skipped} of 3, which do not add up: the reachable set \
changed under the loop and no row reported it"
  fi
fi

# =============================================================================================
# Section 4 - a Pod that did not opt in comes back unchanged BY THIS WEBHOOK
#
# A mutating webhook with failurePolicy Fail on every Pod CREATE in the cluster is a blast radius,
# and the only thing bounding it is the objectSelector. This section asserts the bound from OUTSIDE
# the operator: submit Pods that did not opt in, read back what the API server stored, and diff it
# against what was submitted. Nothing here inspects the webhook's own decision - that is the point,
# since a webhook that believed it had skipped a Pod and mutated it anyway would report the same
# thing either way.
#
# Three shapes, because they fail differently: no label at all (the ordinary Pod, which the selector
# never routes here), the label at "false" (the documented opt-out), and the label at a third value
# (which the selector's In [true] must not match either).
#
# EXERCISED 2026-09-04 on a three-node k3s cluster. The first run failed all three checks, and the
# fault was this section's: it compared every volume, mount and annotation, so the API server's own
# service-account projection counted as a change admission made. It could not have passed whatever
# the webhook did. Now four checks pass, the fourth being the control that proves the comparison can
# see an injection at all.
# =============================================================================================
SECTION_OK=1

# submitted_shape and stored_shape read the same fields off the two sides of admission. Only the
# fields this webhook is allowed to write are compared: the API server defaults plenty of others
# (serviceAccount, tolerations, the default token mount), and a diff of the whole object would drown
# the signal in them.
stored_shape() {
  local pod="$1"
  {
    # env and args are listed WHOLE: the submitted container declares no env and exactly two args, and
    # the API server adds neither, so any line here that is not in WANT was put there by admission.
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].env[*]}ENV {.name}{"\n"}{end}' 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].args[*]}ARG {@}{"\n"}{end}' 2>/dev/null
    # Volumes, mounts and annotations are NOT listed whole, and that is not laziness. The API server
    # projects a service-account token volume and its mount onto every Pod, and the two opt-in
    # annotations the Pod was submitted with are annotations too. Listing all of them made this check
    # fail for reasons that have nothing to do with the webhook - it could not pass no matter how the
    # webhook behaved, and a check that cannot pass reports as little as one that cannot fail. So the
    # names this webhook would write are matched exactly, and everything else is left to the API
    # server. A webhook injecting under some OTHER name is not caught here; it is caught by section 1,
    # which asserts the container has what it needs rather than what it lacks.
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.containers[0].volumeMounts[?(@.name=='${CONFIG_VOLUME}')]}MOUNT {.name}{'\n'}{end}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.volumes[?(@.name=='${CONFIG_VOLUME}')]}VOL {.name}{'\n'}{end}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath=STAMP={.metadata.annotations.kvcache\.gpustack\.ai/injected}{'\n'}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath=CONFIG={.metadata.annotations.kvcache\.gpustack\.ai/client-config}{'\n'}" 2>/dev/null
  } | sort
}

# The submitted shape, written out once. Every check below submits exactly this container, so anything
# that appears in the stored shape and not here was added by admission.
# No ARG rows: the fixture carries its whole launch line in command, so an untouched Pod has no args
# at all. That makes the ARG listing above a pure detector - any ARG row in the stored shape was put
# there by admission.
WANT="$(printf 'CONFIG=\nSTAMP=\n' | sort)"

# The container declares the vLLM entry point and runs kvi_setup's stub behind it, because the
# opted-in Pod below has to be ADMITTED for the positive control to mean anything. The three
# untouched checks would pass with any launch at all - the webhook never looks at a Pod without the
# label - so the shape is decided by the one submission that does reach admission, and all four use
# it because the comparison's whole premise is that they submit the same container.
submit() {
  local pod="$1" label_line="$2"
  kubectl apply -f - <<YAML >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: ${TEST_NS}
${label_line}
  annotations:
    kvcache.gpustack.ai/binding: ${BINDING}
    kvcache.gpustack.ai/engine: vLLM
spec:
  restartPolicy: Never
  volumes:
    - name: ${LAUNCH_VOLUME}
      configMap:
        name: ${LAUNCH_CONFIGMAP}
        defaultMode: 0755
        items:
          - key: launch
            path: vllm
  containers:
    - name: engine
      image: ${CLIENT_IMAGE}
      command: ["${LAUNCH_DIR}/vllm", "serve"]
      volumeMounts:
        - name: ${LAUNCH_VOLUME}
          mountPath: ${LAUNCH_DIR}
YAML
}

check() {
  local pod="$1" what="$2"
  if ! kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null; then
    record FAIL "$what" "the Pod was never stored; a Pod that did not opt in must not even be refused"
    return
  fi
  local got
  got="$(stored_shape "$pod")"
  if [ "$got" = "$WANT" ]; then
    record PASS "$what" "stored exactly as submitted: no env, no volume, no mount, no record"
  else
    record FAIL "$what" "admission changed it: $(diff <(echo "$WANT") <(echo "$got") | tr '\n' ' ' | cut -c1-200)"
  fi
}

# The annotations are present in all three, deliberately: they would be enough to inject if the label
# were, so this proves the LABEL is what decides and the annotations alone do nothing.
submit no-label ""
check no-label "a Pod with no inject label is untouched"

submit label-false '  labels:
    kvcache.gpustack.ai/inject: "false"'
check label-false "a Pod opting out with false is untouched"

submit label-other '  labels:
    kvcache.gpustack.ai/inject: "yes"'
check label-other "a Pod whose label is a third value is untouched"

# Positive control. The three checks above are all NEGATIVE - each passes by observing nothing - and a
# stored_shape that had gone blind would pass all three for the wrong reason. This submits the one
# label value that MUST be injected and requires the same comparison to see it. Its expected value
# (changed, naming the config path) differs from the failure value (unchanged), so it discriminates.
submit opted-in '  labels:
    kvcache.gpustack.ai/inject: "true"'
if ! kvi_wait_for pods opted-in '{.metadata.name}' opted-in 60 "$TEST_NS" >/dev/null; then
  record FAIL "the same comparison sees an injection when there is one" \
    "the opted-in Pod was never stored, so this control did not run and the three checks above are \
unverified: nothing here proves the comparison can see anything at all"
else
  ctl="$(stored_shape opted-in)"
  if [ "$ctl" = "$WANT" ]; then
    record FAIL "the same comparison sees an injection when there is one" \
      "an opted-in Pod compared EQUAL to the submitted shape - the comparison is blind, and the three \
untouched checks above passed for that reason rather than because nothing was injected"
  elif echo "$ctl" | grep -q '^ENV MOONCAKE_CONFIG_PATH$'; then
    record PASS "the same comparison sees an injection when there is one" \
      "the opted-in Pod differs and the difference names MOONCAKE_CONFIG_PATH, so the three untouched \
checks above passed against a comparison that is looking"
  else
    record FAIL "the same comparison sees an injection when there is one" \
      "the opted-in Pod differs, but not by MOONCAKE_CONFIG_PATH: $(echo "$ctl" | tr '\n' ' ' | cut -c1-160)"
  fi
fi

# =============================================================================================
# Section 5 - the refusals, on a live API server, each with the message its fix depends on
#
# Every refusal in this webhook exists because guessing would produce a container that starts
# normally and does not use the cache - a result invisible from outside the Pod. The unit suite
# proves the branches; this section proves the two things it cannot: that a real API server
# delivers the refusal at all (a webhook that never gets called refuses nothing), and that what
# reaches the person running kubectl NAMES what to change.
#
# So every check here asserts a SUBSTRING of the message, never just a non-zero exit. A refusal a
# reader cannot act on is barely better than a silent one, and "the request was rejected" is
# exactly that.
#
# NOT COVERED HERE: the Pod-side refusal of a pool whose ledger is gone. It used to be reached by
# turning multi-tenancy off under the live pool; that edit is now refused at backend admission (the
# last check), which is the rule that closed the degradation, so a pool without a ledger is met only
# during an outage of its master. The resolver pins that refusal in
# TestPodKVCacheResolve_QuotaLedgerGate.
#
# The multi-tenancy check is a server-side dry run and persists nothing, so the backend is never
# left without a ledger - the state that once wedged a pool and its backend undeletable for hours on
# a shared cluster.
#
# The shell-wrapper check was added after the first run, in both spellings: command+args are
# concatenated by Kubernetes, so a check written against either field alone catches one and misses
# the other.
#
# One earlier failure was this section's own: a sed keyed on the fixture's launch line stopped
# matching when that line changed, so the Pod carried no MOONCAKE_TENANT_ID at all - and the check
# reported "the value was changed", naming a mutation nobody made. It now separates absent from
# altered.
# =============================================================================================
SECTION_OK=1

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
    [ "$drop" != engine ] && echo "    kvcache.gpustack.ai/engine: vLLM"
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
  "vLLM" "$(pod_without r-badengine engine '    kvcache.gpustack.ai/engine: tensorrt')"

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
    kvcache.gpustack.ai/engine: vLLM
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
    kvcache.gpustack.ai/engine: vLLM
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
    kvcache.gpustack.ai/engine: vLLM
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
    kvcache.gpustack.ai/engine: vLLM
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
kvi_pod_manifest ok-tenant vLLM | \
  sed 's|^      image: .*|&\n      env:\n        - name: MOONCAKE_TENANT_ID\n          value: mine|' | \
  kubectl apply -f - >/dev/null 2>&1
if kvi_wait_for pods ok-tenant '{.metadata.name}' ok-tenant 60 "$TEST_NS" >/dev/null; then
  got_tenant="$(kvi_env ok-tenant MOONCAKE_TENANT_ID)"
  if [ "$got_tenant" = "mine" ]; then
    record PASS "a user-set tenant id is not a conflict" \
      "admitted, and the value is left exactly as its author wrote it"
  elif [ -z "$got_tenant" ]; then
    # Absent and altered call for opposite investigations - one is this section failing to set it up,
    # the other is the webhook overwriting a user's value - so they are never reported as one.
    record FAIL "a user-set tenant id is not a conflict" \
      "the Pod carries no MOONCAKE_TENANT_ID at all, so this case never set one up; that says \
nothing about whether the webhook would have preserved it"
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
      "$(KVI_BINDING="$TERM_BINDING" kvi_pod_manifest term-probe vLLM)"
  fi
  # Release it whatever happened above, and before the teardown runs: a held Binding blocks the
  # namespace, and this one is held by a finalizer only this file knows about.
  kubectl -n "$TEST_NS" get kvcachepoolbindings.worker.gpustack.ai "$TERM_BINDING" -o json 2>/dev/null \
    | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
f=[x for x in d['metadata'].get('finalizers',[]) if x != '${TERM_HOLD}']
print(json.dumps({'metadata':{'finalizers':f}}))" > "/tmp/kvc-term-release-${SFX}.json" 2>/dev/null
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

# =============================================================================================
# Section 6 - each rendered artifact is one its own engine accepts: both vehicles, each gated
#
# Everything upstream of this section proves we render what we BELIEVE is right. Only this one proves
# an engine accepts it. The two halves ask the same question of different artifacts, which is why
# they are one section rather than two:
#
#                vLLM   - feed the projected file to MooncakeStoreConfig.from_file and assert
#                         __post_init__ does not raise. The mode/global_segment_size pair is what is
#                         being proven: getting it wrong aborts the engine at startup, and it is a
#                         pairing no unit test of ours can check, because the rule lives in the
#                         engine. Then the tenant, as its own row and by the same two steps as
#                         SGLang below: the reader holds the Binding's reuse domain, and it differs
#                         from the reader's default, the only kind the store setup forwards. The
#                         file is also read raw, so the key's JSON type - a string - is asserted,
#                         not inferred.
#                SGLang - call the engine's own _load_config and assert on the config it returns,
#                         never on os.environ. Reading the environment directly would prove only that
#                         we set the variables we meant to set; what has to be proven is WHICH BRANCH
#                         SGLang takes, and a fixture reading os.environ would be green even if the
#                         file branch had swallowed the injection whole. Then three things about that
#                         config: local_hostname is the Pod's IP rather than "localhost" (the value
#                         that decided the vehicle), tenant_id is the Binding's own reuse domain, and
#                         that tenant differs from the engine default - because the store call only
#                         forwards a non-default one, so a build that read our value and then dropped
#                         it would satisfy the first check and fail this one.
#
# Each half is gated on its own image and SKIPS INDEPENDENTLY. One image present must not let the
# other half report green by omission, and a SKIP says the schema is unverified against that engine
# rather than passing quietly.
#
# Images, and why these rather than the newest. The reason outlives the tag: a tag gets bumped by
# whoever needs a newer engine, and then only the reason says whether the bump was safe.
#
#                E2E_VLLM_IMAGE=gpustack/runner:cuda12.9-vllm0.29.0
#                  0.29.0 because that is the version the engine facts table is READ at, and the
#                  lowest version the model-deployment docs support. This section proves an engine
#                  accepts what we render, and what we render was decided from that version's
#                  source; testing a different one silently changes the question. There is also a
#                  hard FLOOR of 0.21.1: vLLM's Mooncake store connector - the module holding
#                  MooncakeStoreConfig, from_file and load_from_config - was added 2026-05-13 and
#                  first shipped in v0.21.1rc0. On anything older this half cannot fail meaningfully,
#                  it can only ImportError, and the injected file has no reader at all. Bumping this
#                  tag means re-reading the facts table at the new version, not just changing a string.
#
#                E2E_SGLANG_IMAGE=gpustack/runner:cuda12.9-sglang0.5.18
#                  0.5.18 because that is now the version the engine facts table is read at, and the
#                  two must match for the same reason the vLLM half must. They did NOT match before:
#                  the table recorded a tag this project neither deploys nor tests, and this header
#                  used to say so as an accepted gap. It was not a gap, it was a wrong entry - that
#                  build does forward a tenant - and the mismatch is what hid it. A version named
#                  here that nothing runs is worse than no version at all.
#
# Neither image is used for inference: no GPU, no weights, no model. Both halves exec one
# python3 -c against the engine's own configuration reader, which is why cuda12.9 is picked over
# cuda13.0 - a wider driver floor for a container that only imports.
#
# PARKED ON A MACHINE WITH NO ACCELERATOR, AND WHAT DOES NOT COUNT AS FILLING IT. The engine half of
# this suite is not being pursued there, because the two questions it asks separate cleanly and only
# one of them is answerable:
#   - "is the name we render in that engine's registry" - ANSWERED, and answered more cleanly
#     without a cluster: one `docker run --rm` per image, listing the whole registry rather than
#     probing for one name. Re-running it through a Pod adds no information.
#   - "does the engine actually run with the configuration we inject" - NOT ANSWERABLE HERE,
#     structurally. Measured: vllm-ascend aborts on `torch_npu` before any vLLM module loads, and
#     this section's own message for that outcome says it reports NOTHING about the name we render,
#     in either direction.
# => So the remaining gap is the SECOND question only, and these do NOT close it: a green run on a
#    machine with no accelerator; `docker run` finding the name in the registry; the Pod reaching
#    Running - `sleep 3600` reaches Running too; and setting an environment variable so some backend
#    stops loading - measured: TORCH_DEVICE_BACKEND_AUTOLOAD=0 only moves the error to
#    `ImportError: libascend_hal.so`. A change that replaces one error message with another reads
#    like a fix because the first error really is gone.
# => WHAT WOULD: a real accelerator, the engine starting AND serving requests with the injected
#    configuration, and the KV traffic demonstrably going through mooncake.
#
# DISK, AND WHY THIS IS A STEP COUNT RATHER THAN A PARAMETER. Measured from the arm64 manifests,
# scaled by a ratio measured on a third image (6.0GB of layers -> 22.7GB resident, 3.8x): vLLM
# 13.6GB of layers / ~52GB resident, SGLang 18.1GB / ~69GB, vllm-ascend 6.0GB / 22.7GB.
# `docker image inspect .Size` reports the FIRST column and `docker system df -v` the second, both
# in GB, so a plan built on the wrong one is off by nearly 4x. NO TWO OF THESE IMAGES FIT AT ONCE
# on an ordinary workstation.
# => Set ONE image variable per run and remove the image before the next run. A run with two set
#    does not fail on an assertion - it fails on the pull, and an ImagePullBackOff looks exactly the
#    same when the image is genuinely broken, so the red points at the wrong subsystem. The
#    independent SKIPs make one-at-a-time a supported way to run this rather than a degraded one.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster, both halves passing - AND THAT RUN IS NOT EVIDENCE
# FOR WHAT THIS FILE NOW ASSERTS. It exercised an earlier revision whose SGLang verdict keyed on
# HOSTNAME alone: the probe printed five branch booleans that no pass condition ever read, so the
# section was green while the facts underneath it were wrong. An external review found the wrong fact,
# not the section.
# =============================================================================================
SECTION_OK=1

# run_in returns 0 when the probe ran, 1 when the Pod never appeared, 2 when it appeared but never
# became Ready, and 4 when the image reference was refused before anything was submitted.
# The Ready case is separated because an engine image is several GB: a pull that has not
# finished leaves a Pod that is stored but not running, the exec then fails, and its empty log is
# indistinguishable from a probe that ran and printed nothing. Those two call for opposite actions -
# wait longer, versus go read why the module produced no output - so the caller is told which it is.
# The exact values this webhook renders, so the vLLM half matches the whole line rather than its
# prefix. Under a prefix match any PARSED output passes - including one where every injected key was
# ignored and the reader returned its own defaults, which is the outcome this half exists to rule out.
# ALL THREE differ from this reader's defaults, which is what makes the whole-line match decisive:
# the reader defaults to mode=embedded and 4 GiB for BOTH sizes, while we render standalone-store,
# segment 0 (an engine container contributes no memory to the pool) and a 128 MiB buffer. Read the
# constants below as the authority: they decide the verdict, and this comment is only what a reader
# trusts afterwards.
VLLM_WANT_MODE=standalone-store
VLLM_WANT_SEGMENT=0
VLLM_WANT_BUFFER=134217728

run_in() {
  local pod="$1" image="$2" engine="$3" script="$4" log="$5"
  # The image is interpolated into the rendered YAML. `|` and `&` open a block scalar and an anchor
  # when they lead a value, so an unusual reference would produce a manifest nobody wrote; refusing it
  # keeps a quoting bug from being read as an engine verdict.
  case "$image" in
    *[!A-Za-z0-9./:_@-]* | "") return 4 ;;
  esac
  KVI_IMAGE="$image" kvi_pod_manifest "$pod" "$engine" \
    | kubectl apply -f - >/dev/null 2>&1
  kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null || return 1
  if ! kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/${pod}" \
      --timeout="${E2E_ENGINE_READY_TIMEOUT}s" >/dev/null 2>&1; then
    return 2
  fi
  kubectl -n "$TEST_NS" exec "$pod" -c engine -- python3 -c "$script" >"$log" 2>&1 || true
  return 0
}

# not_ready_reason reports what the kubelet says about a Pod that never became Ready, so a timeout
# names the pull rather than leaving the reader to guess it.
not_ready_reason() {
  local pod="$1"
  kubectl -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={range .status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{end}' 2>/dev/null \
    | cut -c1-120
}

# vLLM: the file, through the engine's own reader.
if [ -z "${E2E_VLLM_IMAGE:-}" ]; then
  record SKIP "vLLM's own reader accepts the projected file" \
    "E2E_VLLM_IMAGE is unset. The file schema is UNVERIFIED against the real engine: the Go tests \
prove we render what we believe is right, and only this half proves vLLM accepts it"
else
  LOG_V="/tmp/kvc-inject-59-vllm-${SFX}.log"
  # No `if !` around the call: the negation would be what $? then reports, collapsing run_in's three
  # outcomes into two and losing exactly the one that had to be told apart.
  if run_in vllm-probe "$E2E_VLLM_IMAGE" vLLM '
import json, os, sys
# The import is a step this section OBSERVES, not a precondition it assumes. The dataclass shares a
# worker-side module with transfer-thread and lookup-server scaffolding, so importing it can pull up
# heavy vLLM initialisation. An import failure and a schema rejection call for opposite actions - one
# says nothing at all about what we render - so they are reported as different outcomes.
try:
    from vllm.distributed.kv_transfer.kv_connector.v1.mooncake.store.worker import MooncakeStoreConfig
except BaseException as e:
    print("IMPORT_RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("IMPORT_OK")
try:
    cfg = MooncakeStoreConfig.from_file(os.environ["MOONCAKE_CONFIG_PATH"])
except Exception as e:
    print("RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("PARSED mode=%s segment=%s buffer=%s" % (cfg.mode, cfg.global_segment_size, cfg.local_buffer_size))
# The tenant, three ways. TENANT_RAW is the key as we wrote it, with its JSON type: the reader
# normalizes (strips, maps null and blank to "default"), so its value alone cannot show what the
# file holds. TENANT and TENANT_FORWARDED are the same two steps the SGLang half prints - the store
# setup only forwards a tenant that differs from "default". A reader older than v0.28.0 has no
# tenant_id field at all, and says so rather than raising.
with open(os.environ["MOONCAKE_CONFIG_PATH"]) as f:
    raw = json.load(f)
if "tenant_id" in raw:
    print("TENANT_RAW=%s:%s" % (type(raw["tenant_id"]).__name__, raw["tenant_id"]))
else:
    print("TENANT_RAW=absent")
tenant = getattr(cfg, "tenant_id", "<reader has no tenant_id field>")
print("TENANT=%s" % tenant)
print("TENANT_FORWARDED=%s" % (tenant not in ("default", "<reader has no tenant_id field>")))
' "$LOG_V"; rc_v=$?; [ "$rc_v" -ne 0 ]; then
    if [ "$rc_v" -eq 2 ]; then
      record FAIL "vLLM's own reader accepts the projected file" \
        "the Pod never became Ready in ${E2E_ENGINE_READY_TIMEOUT}s: $(not_ready_reason vllm-probe). \
An engine image is several GB, so this is most likely still pulling; it says NOTHING about the file \
we render. Raise E2E_ENGINE_READY_TIMEOUT or pre-pull the image and run again"
    elif [ "$rc_v" -eq 4 ]; then
      record FAIL "vLLM's own reader accepts the projected file" \
        "E2E_VLLM_IMAGE ('${E2E_VLLM_IMAGE}') carries a character this case will not interpolate \
into a sed replacement. Nothing was submitted, so this says NOTHING about the file we render"
    else
      record FAIL "vLLM's own reader accepts the projected file" "the probe Pod never appeared"
    fi
  elif grep -q '^IMPORT_RAISED ' "$LOG_V"; then
    record FAIL "vLLM's own reader accepts the projected file" \
      "$(grep -m1 '^IMPORT_RAISED ' "$LOG_V") - the module would not import, so this run says NOTHING \
about the file we render, in either direction; see ${LOG_V}"
  elif grep -qxF "PARSED mode=${VLLM_WANT_MODE} segment=${VLLM_WANT_SEGMENT} buffer=${VLLM_WANT_BUFFER}" "$LOG_V"; then
    record PASS "vLLM's own reader accepts the projected file" \
      "$(grep -m1 '^PARSED ' "$LOG_V") - every value is the one we rendered, and ALL THREE differ from \
this reader's own defaults (mode=embedded, and 4GiB for both sizes), so the file was read rather than \
fallen back from. __post_init__ did not raise, so the mode/segment pair agrees"
  else
    record FAIL "vLLM's own reader accepts the projected file" \
      "$(grep -m1 '^RAISED ' "$LOG_V" || grep -m1 '^PARSED ' "$LOG_V" || \
echo 'the probe printed neither PARSED nor RAISED'); expected exactly \
'PARSED mode=${VLLM_WANT_MODE} segment=${VLLM_WANT_SEGMENT} buffer=${VLLM_WANT_BUFFER}'. A PARSED line \
with other values means the reader fell back to its own defaults rather than reading what we \
projected, which is a pass under a prefix match and a failure under this one; see ${LOG_V}"
  fi
  # The tenant is its own row, asserted as the SGLang half asserts it: the Binding's reuse domain,
  # held by the reader and different from its default. TENANT_RAW adds the file's own view, a JSON
  # string - the reader raises TypeError on any other type, which the row above reports as RAISED.
  # This fixture always resolves a domain, so an absent key is a FAILURE here; that an unset domain
  # writes no key is pinned by inject_test.go instead. Gated on PARSED: without it the row above
  # already failed, and this one would only repeat that the reader never returned.
  if [ "$rc_v" -eq 0 ] && grep -q '^PARSED ' "$LOG_V"; then
    missing_v=""
    for expect in "TENANT_RAW=str:${DOMAIN}" "TENANT=${DOMAIN}" "TENANT_FORWARDED=True"; do
      grep -qxF "$expect" "$LOG_V" || missing_v="${missing_v} ${expect}"
    done
    if [ -z "$missing_v" ]; then
      record PASS "vLLM's own reader holds the injected tenant" \
        "TENANT=${DOMAIN}, written as a JSON string and different from the reader's default, so \
the store setup will forward it"
    else
      record FAIL "vLLM's own reader holds the injected tenant" \
        "these expected values were absent from the probe output:${missing_v}. Observed: $(grep -E \
'^TENANT' "$LOG_V" | tr '\n' ' ' | cut -c1-200); see ${LOG_V}"
    fi
  fi
fi

# SGLang: the environment, through the engine's own selection.
if [ -z "${E2E_SGLANG_IMAGE:-}" ]; then
  record SKIP "SGLang takes the env branch and resolves the Pod's IP" \
    "E2E_SGLANG_IMAGE is unset. The environment vehicle is UNVERIFIED against the real engine, and it \
is the half where a wrong answer is silent: the file branch would return localhost and still start"
else
  LOG_S="/tmp/kvc-inject-59-sglang-${SFX}.log"
  # As above: no `if !`, so run_in's three outcomes survive into $?.
  if run_in sglang-probe "$E2E_SGLANG_IMAGE" SGLang '
import io, logging, sys
from sglang.srt.mem_cache.storage.mooncake_store.mooncake_store import MooncakeBaseStore
from sglang.srt import environ as envs

# _load_config is the SELECTOR, and calling it is the whole point: load_from_env is a staticmethod,
# so calling that one IS choosing the env branch, and a probe built on it proves only that the env
# branch parses - never that the engine would have picked it. Picking it is what this webhook bet on.
#
# MooncakeBaseStore rather than MooncakeStore: __init__ there assigns two Nones and nothing else, so
# constructing one needs no GPU and, like the module top level, never imports the mooncake package.
buf = io.StringIO()
logging.getLogger().addHandler(logging.StreamHandler(buf))
logging.getLogger().setLevel(logging.INFO)

print("CONFIG_PATH_SET=%s" % envs.envs.SGLANG_HICACHE_MOONCAKE_CONFIG_PATH.is_set())
try:
    cfg = MooncakeBaseStore()._load_config(None)
except Exception as e:
    print("RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
# Each branch announces itself with its own line. All three are asserted, not just the wanted one: an
# implementation that logged all three would pass a test that only looked for the one we want.
log = buf.getvalue()
print("BRANCH_ENV=%s"   % ("loaded from env successfully" in log))
print("BRANCH_FILE=%s"  % ("loaded from file successfully" in log))
print("BRANCH_EXTRA=%s" % ("loaded from extra_config successfully" in log))
print("HOSTNAME=%s" % cfg.local_hostname)
print("MASTER=%s SEGMENT=%s PROTOCOL=%s" % (cfg.master_server_address, cfg.global_segment_size, cfg.protocol))
# The tenant the engine ended up holding, and then whether it would REACH the store. Those are two
# steps: the config carrying a tenant proves the variable was read, while the store call only forwards
# it when it differs from the engine default - so a build that read our value and then dropped it
# would satisfy the first and fail the second.
print("TENANT=%s" % cfg.tenant_id)
print("TENANT_FORWARDED=%s" % (cfg.tenant_id != "default"))

# Positive control. Without it the extra_config branch is never reached by this section, and the
# design rule it guards - that we must NOT occupy extra_config - reads as an unrelated caution rather
# than a decision with teeth. Its expected value (extra_config) differs from its failure value (env),
# so it discriminates. The branch is chosen on master_server_address alone; the rest falls back to
# defaults.
buf.truncate(0); buf.seek(0)
class _Populated:
    extra_config = {"master_server_address": "control.invalid:50051"}
try:
    MooncakeBaseStore()._load_config(_Populated())
    log2 = buf.getvalue()
    print("CONTROL_EXTRA=%s" % ("loaded from extra_config successfully" in log2))
    print("CONTROL_ENV=%s"   % ("loaded from env successfully" in log2))
except Exception as e:
    print("CONTROL_RAISED %s: %s" % (type(e).__name__, e))
' "$LOG_S"; rc_s=$?; [ "$rc_s" -ne 0 ]; then
    if [ "$rc_s" -eq 2 ]; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "the Pod never became Ready in ${E2E_ENGINE_READY_TIMEOUT}s: $(not_ready_reason sglang-probe). \
An engine image is several GB, so this is most likely still pulling; it says NOTHING about which \
branch the engine takes. Raise E2E_ENGINE_READY_TIMEOUT or pre-pull the image and run again"
    elif [ "$rc_s" -eq 4 ]; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "E2E_SGLANG_IMAGE ('${E2E_SGLANG_IMAGE}') carries a character this case will not interpolate \
into a sed replacement. Nothing was submitted, so this says NOTHING about which branch the engine takes"
    else
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" "the probe Pod never appeared"
    fi
  else
    POD_IP="$(kubectl -n "$TEST_NS" get pod sglang-probe -o jsonpath='{.status.podIP}' 2>/dev/null)"
    # An empty POD_IP would be embedded into the expected line as "HOSTNAME=", every grep would miss,
    # and the FAIL would list all values as absent - reading as an injection defect when the real
    # cause is that the Pod's IP was never obtained. Named here so the two stay distinguishable.
    if [ -z "$POD_IP" ]; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "the Pod's IP could not be read back, so the expected hostname could not be built; this is \
about reading the Pod, not about what was injected"
      section_stop "Section 6"
    fi
    if [ "$SECTION_OK" -eq 1 ]; then
      if ! grep -q '^CONFIG_PATH_SET=' "$LOG_S"; then
        # An empty log and a config path that IS set both fail the next test, and they call for opposite
        # actions - one is a broken probe, the other a real defect in the injection. Separating them here
        # keeps a FAIL from naming the wrong cause, which is the failure mode a refusal message has.
        record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
          "the probe printed nothing: the exec did not run, or the image has no sglang module. This says \
nothing about the injection either way; see ${LOG_S}"
      elif ! grep -q '^CONFIG_PATH_SET=False' "$LOG_S"; then
        record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
          "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH is set in the container, so _load_config would take the \
file branch and the injected variables would be inert"
      else
        # EVERY value the probe prints is checked, and the list is written out so a value that is
        # printed but not asserted becomes visible as a missing row rather than as nothing at all.
        # An earlier revision printed the five branch booleans and then keyed its verdict on HOSTNAME
        # alone; the section passed while the branch facts underneath it were wrong, and it took an
        # external review to notice. Absence of a row is the failure mode being guarded here.
        #
        # The three BRANCH_ values are asserted together, positives AND negatives: an implementation
        # that announced every branch would satisfy "took the env branch" on its own.
        # MASTER/SEGMENT/PROTOCOL are here because the probe prints them, and the comment above claims
        # every printed value is checked. It did not: the revision that fixed "printed but not asserted"
        # for the five booleans left three more behind - while asserting in prose that it had not.
        want="BRANCH_ENV=True BRANCH_FILE=False BRANCH_EXTRA=False HOSTNAME=${POD_IP} \
TENANT=${DOMAIN} TENANT_FORWARDED=True CONTROL_EXTRA=True CONTROL_ENV=False"
        want_line="MASTER=${ENDPOINT} SEGMENT=0 PROTOCOL=tcp"
        missing=""
        for expect in $want; do
          grep -qxF "$expect" "$LOG_S" || missing="${missing} ${expect}"
        done
        grep -qxF "$want_line" "$LOG_S" || missing="${missing} [${want_line}]"
        if [ -z "$missing" ]; then
          record PASS "SGLang reads the env branch, the Pod's IP and the injected tenant" \
            "all 9 asserted values matched, including TENANT=${DOMAIN} (the Binding's own reuse domain, \
so the engine will forward it) and local_hostname=${POD_IP} rather than localhost"
        else
          record FAIL "SGLang reads the env branch, the Pod's IP and the injected tenant" \
            "these expected values were absent from the probe output:${missing}. Observed: $(grep -E \
'^(BRANCH_|HOSTNAME|TENANT|CONTROL_)' "$LOG_S" | tr '\n' ' ' | cut -c1-200); see ${LOG_S}"
        fi
      fi
    fi
  fi
fi

# =============================================================================================
# Section 7 - the connector name we render is one that engine's own factory has registered
#
# This is the first section that checks the connector NAME. Everything upstream proves we render
# what we believe is right; section 6 proves an engine's config reader accepts the file. Neither
# touches the factory - and the factory is where an engine turns `kv_connector` into a class, and
# where a name it does not know raises `ValueError: Unsupported connector type` before any config
# file is opened.
#
# TWO ROWS, ONE ASSERTION. This is not "a section for vLLM-Ascend": the vLLM row had never been
# checked either. We had verified that vLLM's reader accepts our file, never that its factory
# accepts the name we pair with that file. That row happened to be correct. A dimension nothing
# covered is the finding here; one engine failing it was the symptom.
#
# ONE OF THE TWO NO LONGER RUNS, and the assertion is smaller for it. The vLLM-Ascend row cannot be
# submitted from this fixture at all - the engine annotation refuses that value, and the spelling
# admission points at instead needs an ascend-transport pool while this family's is TCP. The
# section does not take that on trust: it submits the row's own manifest as a server-side dry run
# and skips only on an actual refusal, printing what the server said, so the skip retires itself
# the day either refusal is lifted instead of outliving its reason. The loop body carries both in
# full. What is lost while it stands is this section's own finding: the row that caught
# MooncakeStoreConnector being a name that factory did not know, at the release read at the time, is
# the one going unrun, leaving the rendered name pinned by a Go test and by the off-cluster registry
# reading recorded in section 6, neither of which is a factory answering on a cluster.
#
# WHY THE WRONG NAME READ AS RIGHT, which is why the FAIL below prints the whole registry rather than
# only the verdict. THE NAMES BELOW ARE A SNAPSHOT OF ONE READING, AND NOTHING THIS SECTION ASSERTS
# DEPENDS ON THEM: the verdict is taken from the registry the engine holds at run time, so this
# snapshot can go stale without the assertion going wrong. That gap is the reason to say so here
# rather than in a note - a reader arrives at this header BECAUSE the section failed, so they are
# reading the narrative while the lookup that just ran is newer than it is.
#
# In the reading recorded in section 6 - vllm-ascend v0.19.1rc1, read 2026-09-04 - that factory held
# four names beginning with Mooncake: MooncakeConnector, MooncakeConnectorV1,
# MooncakeConnectorStoreV1 and MooncakeLayerwiseConnector. The name we rendered then,
# MooncakeStoreConnector, was not one of them, and was not a typo of any of the four: it was a
# legal-looking combination of their parts that named nothing. Surrounded by four real neighbours a
# name like that survives every review that reads it, so the check has to be a lookup rather than a
# reading, and a reader who sees the failure needs the neighbours in front of them to pick the right
# one.
#
# The name is NOT hard-coded. It is read back out of the Pod the webhook just mutated and handed to
# that engine's factory. A hard-coded expectation would split this into two independent assertions -
# "we render X" and "the engine knows X" - and the defect lives exactly between them: both halves
# can be individually right about different X.
#
# WHY THIS IS NOT A THIRD HALF OF SECTION 6. That half has a hard floor of vLLM 0.21.1, because it
# imports `...v1.mooncake.store.worker`, added 2026-05-13. vLLM-Ascend v0.19.1rc1 pins vLLM v0.19.1,
# where that module does not exist at all - `v1/mooncake/` there holds only `__init__`,
# `mooncake_connector` and `mooncake_utils`. Hanging this on that half would gate it behind a floor
# its subject can never meet, and the red would read as "the ascend image is broken". The two APIs
# this section needs both exist at v0.19.1, so its gate is a different gate.
#
# Neither image runs inference: no GPU, no NPU, no weights, no model. Each execs one python3 -c that
# imports a registry and looks up a string.
#
# PRECONDITION, AND THE FAILURE CONDITION OF THIS SECTION: we do not render
# `kv_connector_module_path` (vllm.go renders `kv_connector` alone). The two releases this section
# reads order the two lookups OPPOSITELY. At v0.25.1 and v0.29.0 the module path comes first, and
# while it is set the registry is never consulted. At v0.19.1, the vLLM that vllm-ascend pins, the
# registry comes first and the module path is only the fallback for a miss. Both raise `Unsupported
# connector type` on a registry miss with the key unset - so while we never render it, registry
# membership and "the engine starts" are the same question on both. On the day we do render it, this
# assertion stops measuring the engine: at v0.19.1 it turns into a false negative, reporting missing
# what the fallback would resolve, and at v0.29.0 it can be wrong in either direction, because the
# registry is not asked at all. It must move to the version-specific entry point then
# (`_get_connector_class_with_compat` at v0.19.1, `get_connector_class` at v0.29.0; the names differ,
# which is why the registry is read directly while that is equivalent).
#
# EXERCISED 2026-09-04, first run ever, single-node docker-desktop (arm64, no accelerator):
#   vllm row        PASS - LOADED MooncakeStoreConnector. That row had never been executed either;
#                   until this run it rested on source read at v0.25.1.
#   vllm-ascend row FAIL at step 1, and NOT about the name: FACTORY_IMPORT_RAISED RuntimeError,
#                   "Failed to load the backend extension: torch_npu".
#
# WHY THAT ROW CANNOT PASS HERE, measured rather than guessed - and it is this webhook's own subject
# matter. The vllm-ascend image prepares its environment in the image ENTRYPOINT, which sources three
# Ascend set_env.sh scripts before exec'ing anything. TWO things bypass it: a Pod that sets `command`
# discards the ENTRYPOINT entirely (the exact Kubernetes rule this webhook refuses over), and
# `kubectl exec` starts a process that never runs it. So torch's device-backend autoload fails before
# any vLLM module is imported.
#   Measured three ways on the same image: through the ENTRYPOINT (`docker run <img> python3 -`) the
#   import SUCCEEDS; bypassing it (`--entrypoint python3`) it fails exactly as it does here; and
#   TORCH_DEVICE_BACKEND_AUTOLOAD=0 does NOT fix it - the failure only moves to
#   `ImportError: libascend_hal.so`, because vLLM activates the ascend PLATFORM plugin, which imports
#   torch_npu regardless of the connector registry this section is about.
# => The fix is to give the probe the environment the ENTRYPOINT builds, not to disable a backend.
#   Left undone deliberately: a fixture that runs the probe as the Pod's `args` would keep the
#   ENTRYPOINT, but the name this section asserts has to be read off the mutated Pod BEFORE the probe
#   runs, and passing it at create time is the hard-coding this section exists to avoid. That tension
#   is real and is a design decision, not an edit to make during a run.
#
# What that run does NOT leave unverified: the registry contents themselves. Measured the same day by
# `docker run --rm quay.io/ascend/vllm-ascend:v0.19.1rc1` (ENTRYPOINT honored, no accelerator):
# after load_general_plugins() the factory holds 19 names, AscendStoreConnector among them and
# MooncakeStoreConnector absent. That is this row's subject; what is missing is only its delivery
# through the webhook and kubectl exec.
#
# What HAS been executed, 2026-09-04, outside this file - one `docker run --rm` on a host with no
# accelerator of any kind, against quay.io/ascend/vllm-ascend:v0.19.1rc1 (linux/arm64):
#   - `import vllm` and `load_general_plugins()` both succeed with no accelerator present;
#   - the registry then holds 19 names; `AscendStoreConnector` is among them and
#     `MooncakeStoreConnector` is NOT, which is what this section's vllm-ascend row asserts;
#   - the class behind the name also loaded, so step 4 below is not hypothetical either.
# That retires the earlier note that plugin loading without an accelerator was untested. What is
# still unexercised here is this FILE: the webhook injecting, the name being read back off the
# mutated Pod, and the probe running through kubectl exec.
# =============================================================================================
SECTION_OK=1

# Creating the Pod and running the probe are two steps rather than one, because the name this
# section asserts has to be read off the SAME Pod the probe runs in. Doing it in one step forced a
# create, a read, a delete and a second create - and the second create is a second trip through the
# mutating webhook, so the name that was validated belonged to a Pod that no longer existed.
#
# start_pod returns 0 when the Pod is Ready, 1 when it never appeared, 2 when it appeared but never
# became Ready, and 4 when the image reference was refused before anything was submitted.
# The Ready case is separate because an engine image is several GB: a pull that has not
# finished leaves a Pod that is stored but not running, and that is a different thing to report.
start_pod() {
  local pod="$1" image="$2" engine="$3"
  # The image is interpolated into the rendered YAML. `|` and `&` open a block scalar and an anchor
  # when they lead a value, so an unusual reference would produce a manifest nobody wrote; refusing it
  # keeps a quoting bug from being read as an engine verdict.
  case "$image" in
    *[!A-Za-z0-9./:_@-]* | "") return 4 ;;
  esac
  KVI_IMAGE="$image" kvi_pod_manifest "$pod" "$engine" \
    | kubectl apply -f - >/dev/null 2>&1
  kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null || return 1
  kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/${pod}" \
    --timeout="${E2E_ENGINE_READY_TIMEOUT}s" >/dev/null 2>&1 || return 2
  return 0
}

# exec_probe runs one python3 -c in a Ready Pod and returns 3 when the exec itself did not complete.
# Without that, an exec that never ran - container OOM-killed, runtime gone, no python3 in the image -
# leaves an empty log and reaches the verdicts as "the probe printed no verdict line", which points
# the reader at the probe's output when the probe never produced any.
exec_probe() {
  local pod="$1" script="$2" log="$3" rc=0
  kubectl -n "$TEST_NS" exec "$pod" -c engine -- python3 -c "$script" >"$log" 2>&1 || rc=$?
  [ "$rc" -eq 0 ] || return 3
  return 0
}

# rendered_connector reads the connector name the webhook actually wrote, out of the mutated Pod.
# It reads command and args together because the flag is appended to args while a workload's own
# launcher may sit in command, and the pair is one argv.
rendered_connector() {
  local pod="$1"
  kubectl -n "$TEST_NS" get pod "$pod" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    ctr = json.load(sys.stdin)["spec"]["containers"][0]
except Exception:
    sys.exit(0)
argv = ctr.get("command", []) + ctr.get("args", [])
for i, a in enumerate(argv):
    if a == "--kv-transfer-config" and i + 1 < len(argv):
        try:
            print(json.loads(argv[i + 1]).get("kv_connector", ""))
        except Exception:
            pass
        break
' 2>/dev/null
}

# The probe. NAME is emitted ahead of the body rather than interpolated into it, and the body is a
# QUOTED heredoc so the shell expands nothing inside it. Unquoted, every backtick pair in the prose
# below runs as a command substitution: the comment naming ValueError did exactly that, printing
# "ValueError:: command not found" on the section's own output and deleting the line it sat in. The
# value still cannot carry quoting, because the shell-side guard below refuses anything that is not
# a bare identifier. NAME is passed to exec_probe as part of the script rather than as an argument
# because that helper runs `python3 -c` with no argv after it.
probe_script() {
  printf 'NAME = "%s"\n' "$1"
  cat <<'PY'
import sys

# 1. The factory module. Reported on its own: an import failure says nothing about the name we
# render, and calls for a different action than a name that does not resolve.
try:
    from vllm.distributed.kv_transfer.kv_connector.factory import KVConnectorFactory
except BaseException as e:
    print("FACTORY_IMPORT_RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("FACTORY_IMPORT_OK")

# 2. Registration. An engine's own connectors are registered by entry points under
# vllm.general_plugins, not by importing the factory. This goes through vLLM's own loader rather
# than calling the plugin's register function directly: the direct call would assert "the name is
# in the registry because I just put it there", and would stay green if the entry point declaration
# were broken - which is a real failure that only shows up in a packaged image.
try:
    from vllm.plugins import load_general_plugins
    load_general_plugins()
except BaseException as e:
    print("PLUGINS_RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("PLUGINS_OK")

# 3. THE VERDICT: is the rendered name in this engine's registry. That membership is the exact
# property deciding whether a workload starts while kv_connector_module_path is unset, which is
# every Pod we render: with that key unset both releases this section reads resolve the name from the
# registry alone and raise `ValueError: Unsupported connector type` on a miss. The order of the two
# lookups differs by release when the key IS set - see Expected in the header. The whole registry is
# printed, not just the answer: a failure that also lists the names the engine DOES know says what
# to render instead, and listing the full set does not depend on having guessed which other name to
# probe.
names = sorted(KVConnectorFactory._registry)
print("REGISTRY %s" % ",".join(names))
if NAME not in KVConnectorFactory._registry:
    print("NOT_IN_REGISTRY %s" % NAME); sys.exit(0)
print("IN_REGISTRY %s" % NAME)

# 4. EVIDENCE, not a verdict. Resolving the class runs the registry's lazy loader, and that loader
# imports the implementation module - torch, zmq and the engine's own worker code. Whether that
# import succeeds is a property of THIS IMAGE's dependencies, not of the name we render, so the two
# have opposite fixes and must not share an outcome. Reported either way, because a load failure is
# still the one place a missing dependency becomes visible from here.
try:
    cls = KVConnectorFactory.get_connector_class_by_name(NAME)
except BaseException as e:
    print("LOAD_FAILED %s %s: %s" % (NAME, type(e).__name__, e)); sys.exit(0)
print("LOADED %s -> %s" % (NAME, getattr(cls, "__name__", "<unnamed>")))
PY
}

# The engine table. Two rows, one assertion. Adding an engine here is adding a row, which is the
# shape this section is built for - the previous gap was a dimension with no rows at all.
#
# ONE OF THE TWO IS CURRENTLY DEAD: the vllm-ascend row SKIPS unconditionally, for the two reasons
# spelled out at the top of the loop body. It stays in the list rather than being deleted, because the
# row is the unit this section is built from and its image variable is still the one to set when the
# fixture can carry it; the skip is a statement about the fixture, not about the row.
resolved_rows=0
for row in "vLLM:E2E_VLLM_IMAGE" "vllm-ascend:E2E_VLLM_ASCEND_IMAGE"; do
  engine="${row%%:*}"
  var="${row##*:}"
  image="$(eval "printf '%s' \"\${${var}:-}\"")"
  check="the ${engine} factory has the connector name we render in its registry"
  pod="conn-${engine//-/}"
  pod="${pod,,}"

  # vLLM-Ascend is not reachable from this fixture, and the question is ASKED of admission rather
  # than answered here. Two refusals stand in front of the row:
  #
  #  1. `kvcache.gpustack.ai/engine: vllm-ascend` is refused outright. That value was ruled the
  #     package the runner installs when the accelerator backend is CANN rather than an engine
  #     anybody names, so admission points at `engine: vLLM` + `manufacturer: ascend` instead.
  #  2. Spelled that way the Pod is refused one layer down: that runtime accepts only the `ascend`
  #     transport, and this family's pool is built on a TCP backend.
  #
  # Both live in the webhook, which is the moving part - so the row's own manifest goes through a
  # server-side dry run and skips only on an actual refusal. Written as a fixed branch instead, the
  # skip would outlive its reason in silence: the row would keep skipping after the refusal was
  # lifted and this section would keep reporting the same thing. Asked this way it retires itself.
  #
  # It is asked BEFORE the image, because the image is not what stops it - admission never looks at
  # one, which is also why the dry run does not need this row's own image to be set.
  #
  # THE COST IS THIS SECTION'S OWN FINDING: the row that caught MooncakeStoreConnector being a name
  # vLLM-Ascend's factory did not know, at the release read at the time, is the one going unrun.
  # What is left of that guard is TestRender_VLLMFamilyVehicleIsAFile pinning AscendStoreConnector as
  # the name we render, plus the off-cluster reading in section 6 - one `docker run` against the
  # image listing the whole registry, recorded there with its date. Neither is this file asserting
  # it, and the summary below already refuses to claim the cross-engine property on one row.
  if [ "$engine" = vllm-ascend ]; then
    why="$(kvi_admission_refuses "$(kvi_pod_manifest "$pod" "$engine")")" && rc=0 || rc=$?
    case "$rc" in
      0)
        record SKIP "$check" \
          "admission refuses this row on this fixture, so the name we render for ${engine} is \
UNVERIFIED against its factory - the dimension this case was written for, and the one its original \
finding came from. The server said: ${why}"
        continue
        ;;
      2)
        record FAIL "$check" \
          "the dry run failed for something other than this webhook's refusal, so whether this row \
is reachable was never established and it was neither run nor skipped: ${why}"
        continue
        ;;
    esac
    # rc=1: admission accepts it now, so the row goes on to its image check and runs.
  fi

  if [ -z "$image" ]; then
    record SKIP "$check" \
      "${var} is unset. The name we render for ${engine} is UNVERIFIED against a real factory: the \
Go tests prove which name we write, and only this row proves that engine knows it"
    continue
  fi

  LOG="/tmp/kvc-inject-60-${engine}-${SFX}.log"
  # One create for the whole row. The name asserted below and the registry the probe queries have to
  # come from the SAME Pod: a second create would be a second pass through the mutating webhook, and
  # the validated name would describe a Pod that no longer exists.
  start_pod "$pod" "$image" "$engine"; rc=$?
  if [ "$rc" -eq 2 ]; then
    record FAIL "$check" \
      "the Pod never became Ready in ${E2E_ENGINE_READY_TIMEOUT}s. An engine image is several GB, so \
this is most likely still pulling; it says NOTHING about the name we render. Raise \
E2E_ENGINE_READY_TIMEOUT or pre-pull the image and run again"
    continue
  elif [ "$rc" -eq 4 ]; then
    record FAIL "$check" \
      "the image reference '${image}' carries a character this case will not interpolate into a sed \
replacement. Nothing was submitted, so this says NOTHING about the name we render; fix the variable"
    continue
  elif [ "$rc" -ne 0 ]; then
    record FAIL "$check" "the Pod never appeared, so the webhook's output was never observed"
    continue
  fi

  # Read what was rendered BEFORE running anything: if this is empty the injection itself did not
  # happen, which is a different failure from a name the engine rejects, and reporting it as the
  # latter would send the reader to the engine.
  name="$(rendered_connector "$pod")"
  if [ -z "$name" ]; then
    record FAIL "$check" \
      "no --kv-transfer-config carrying a kv_connector was found on the mutated Pod, so the webhook \
injected nothing for ${engine}; the factory was never asked"
    continue
  fi
  case "$name" in
    *[!A-Za-z0-9_]* | "")
      record FAIL "$check" \
        "the rendered connector name '${name}' is not a bare identifier; refusing to interpolate it \
into the probe rather than shipping a quoting bug"
      continue
      ;;
  esac

  exec_probe "$pod" "$(probe_script "$name")" "$LOG"; rc=$?

  if [ "$rc" -eq 3 ]; then
    record FAIL "$check" \
      "the probe exec did not complete, so the log below is empty for a reason that has nothing to do \
with ${name}: the container may have been OOM-killed, the runtime may be gone, or the image may not \
carry python3. Observed: $(tr '\n' ' ' < "$LOG" | cut -c1-160); see ${LOG}"
  elif grep -q '^FACTORY_IMPORT_RAISED ' "$LOG"; then
    record FAIL "$check" \
      "$(grep -m1 '^FACTORY_IMPORT_RAISED ' "$LOG") - the factory would not import, so this run says \
NOTHING about the name we render, in either direction; see ${LOG}"
  elif grep -q '^PLUGINS_RAISED ' "$LOG"; then
    record FAIL "$check" \
      "$(grep -m1 '^PLUGINS_RAISED ' "$LOG") - plugin registration failed, so the registry this \
lookup would consult was never populated; that is about the image, not about ${name}; see ${LOG}"
  elif grep -q "^NOT_IN_REGISTRY ${name}$" "$LOG"; then
    record FAIL "$check" \
      "${engine}'s factory does not know '${name}', so a real workload aborts at startup with \
'Unsupported connector type' before the projected file is ever opened. The names it DOES know: \
$(grep -m1 '^REGISTRY ' "$LOG" | cut -d' ' -f2- | cut -c1-300); see ${LOG}"
  elif grep -q "^IN_REGISTRY ${name}$" "$LOG" && grep -q "^LOADED ${name} -> " "$LOG"; then
    resolved_rows=$((resolved_rows + 1))
    record PASS "$check" \
      "$(grep -m1 '^LOADED ' "$LOG") - the name came off the mutated Pod, not from this script, so \
what was looked up is what the webhook wrote, and the class behind it loaded in this image too"
  elif grep -q "^IN_REGISTRY ${name}$" "$LOG"; then
    # The verdict is membership, so this is a pass; the load failure rides in the same row rather
    # than becoming a row of its own, because kvi_results counts every non-SKIP row as a pass and an
    # extra INFO row would inflate that count. It is spelled out rather than dropped: a downgraded
    # output that does not carry the reason it was downgraded is just a quieter way of losing it.
    resolved_rows=$((resolved_rows + 1))
    record PASS "$check" \
      "'${name}' IS in ${engine}'s registry, which is the property deciding whether a workload \
starts, so the name we render is right. The class behind it did NOT load in this image - \
$(grep -m1 '^LOAD_FAILED ' "$LOG" | cut -c1-140) - and that is about the image's dependencies, not \
about the name, which is why it is not a failure here. It is not nothing either: a genuinely broken \
engine build would look the same from this side, so read ${LOG} before trusting the image"
  else
    record FAIL "$check" \
      "the probe printed no verdict line. Observed: $(tr '\n' ' ' < "$LOG" | cut -c1-200); see ${LOG}"
  fi
done

# A summary line, and it deliberately does not claim more than the rows produced. Two rows skipped is
# not a pass: it is the state this section was written to end.
if [ "$resolved_rows" -eq 2 ]; then
  record PASS "both rendered connector names are in their own engine's registry" \
    "vllm and vllm-ascend each know the name written for it, which is what tells a per-engine \
decision from a constant that happens to suit one of them"
else
  record SKIP "both rendered connector names are in their own engine's registry" \
    "${resolved_rows} of 2 rows confirmed; the cross-engine claim needs both, and a row that skipped \
or failed above is reported there rather than being folded into this line"
fi

kvi_results "$CASE_ID"
