#!/usr/bin/env bash
#
# CASE 45 — The ModelDeployment admission surface: every refusal fires, and each one fires from the
#           layer that is supposed to own it
#   (NON-MUTATING for the refusals, which are server-side dry-runs; one short-lived object for the
#    controller-level check)
#
#   case-45.sh <NS>
#
# Goal:        Pins the refusals a ModelDeployment owes its user. The interesting part is not that
#              each bad manifest is rejected — it is WHICH layer rejects it, because three different
#              layers can produce a refusal here and they are not interchangeable:
#
#                schema   — the CRD's own openAPIV3Schema: a closed enum, required fields, and
#                           strict decoding of unknown fields;
#                webhook  — ValidateCreate, which sees a manifest the schema already accepted;
#                controller — a status condition, for anything that depends on cluster state the
#                           admission moment cannot read.
#
#              THE TRAP THIS CASE EXISTS TO AVOID. A refusal test whose sample is incomplete
#              measures the schema and never reaches the webhook, and it stays green if the webhook
#              is deleted outright. Measured on a live cluster: a role carrying only
#              `template.resources.cpu` comes back `resources.ram: Required value` — the schema —
#              and adding cpu+ram+localStorage then yields `template.image: Required value`, still
#              the schema. Only a template complete enough to satisfy every required field reaches
#              the webhook and produces the message this case asserts. So each webhook row here
#              carries a COMPLETE manifest and asserts a fragment of the operator's own wording,
#              never the bare fact of a rejection.
#
#              The same reasoning drives row 0: a baseline that must be ACCEPTED. Without it every
#              row below can pass for the wrong reason — a typo in an unrelated field refuses all
#              seven manifests, and seven refusals read exactly like seven working guards.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an operator
#              image carrying the ModelDeployment CRD. No GPU is needed, and NO KVCachePoolBinding
#              has to exist.
#
#              ONE ROW PAYS FOR THE "NO GPU" PART EXPLICITLY, and it did not always. The
#              replica-rendering row gives its role a `template.image`, because a role naming no
#              image has one SYNTHESIZED from the accelerator backend its InstanceType observed —
#              and a CPU-only InstanceType has observed none. Measured on a single-node CPU-only
#              cluster: without the explicit image that row polls out at 0 Pods, and the operator
#              log says "the instance type has not observed its hardware yet". That is a correct
#              refusal (an empty image would surface later as an ImagePullBackOff naming a tag
#              nobody typed) and it has its own coverage in the unit tier, so the row drops the
#              dependency rather than the assertion: it still requires EXACTLY 1 Pod, so a
#              convergence wrongly gated on the domain still fails it.
#
#              The lesson is in the header rather than at the row: this line read "No GPU is needed:
#              nothing here schedules a replica" while a row that needed one was added below it, and
#              the sentence never stopped being true OF THE CASE IT DESCRIBED. An added assertion
#              can raise a case's environment requirement without contradicting anything already
#              written down.
#
#              That the Binding may be absent is not a shortcut but
#              a property worth stating: admission never reads cluster state, so a poolRef naming a
#              Binding that was never created is ACCEPTED by both the schema and the webhook. The
#              missing Binding surfaces later, as a controller condition, which is exactly the row
#              near the end of this case. Every manifest here therefore names one deliberately
#              absent Binding, and the case leaves no KVCache object behind because it creates none.
#
# Inputs:      All real, nothing mocked. Server-side dry-run (`--dry-run=server`) runs the schema
#              and the webhook and persists nothing; the one row that needs a controller verdict
#              creates a ModelDeployment naming a Binding that does not exist and deletes it again.
#
# Deferred:    The serving half of this case — `replicas: 2` reaching `status.roles[0].ready == 2`
#              and `status.endpoint` answering an inference request — is NOT here. It needs the
#              synthesized connector to actually be wired into the replicas, which is a separate
#              task; until then a passing run of this file means the admission surface holds, not
#              that the deployment serves. Those rows report SKIP so the verdict table says so out
#              loud rather than by omission.
#
#              A third row is deferred for a different and more interesting reason: it is not that
#              the behaviour is missing, but that asserting it would not DISCRIMINATE. See the row
#              about what the unregistered domain costs, near the end.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-45.sh <NS>" >&2
  exit 2
fi

# Deliberately absent: see the Environment note. Admission does not resolve it, and the controller
# row at the end asserts that the controller does.
BINDING="case45-no-such-binding"
IT="${E2E_MD_INSTANCE_TYPE:-}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# Pick an InstanceType if the caller did not name one. Any of them will do: no row here schedules a
# replica, so the accelerator the type describes is never asked for.
if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-45] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# Emit a manifest. $1 is spliced in under the single role, $2 under spec.kvCache; both may be empty.
# `poolRef` is a LocalObjectReference by design, which is why this case has no cross-namespace row:
# see the assertion on the schema below.
manifest() {
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: case45-probe
  namespace: ${NS}
spec:
  engine: ${ENGINE:-vllm}
  engineVersion: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
${2}
  roles:
  - name: server
    instanceType: ${IT}
    replicas: 1
${1}
YAML
}

# Assert that a manifest is refused AND that the refusal quotes $2, which is what ties the row to the
# layer that owns it. A refusal carrying someone else's message is a FAIL, not a pass.
refuses() {
  local check="$1" want="$2" role_extra="$3" kv_extra="$4" out
  out="$(manifest "$role_extra" "$kv_extra" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  # AN EMPTY $out MUST NOT PASS. `${out##*$want*}` deletes the longest matching prefix, and deleting
  # anything from "" leaves "" -- so `-z` is TRUE when the command produced nothing at all, and the
  # row would report a refusal it never saw. A check that passes when nothing happened is measuring
  # nothing, and this one would do it for every row at once.
  if [ -n "$out" ] && [ -z "${out##*$want*}" ]; then
    record PASS "$check" "refused, quoting: ${want}"
  else
    record FAIL "$check" "wanted a refusal quoting '${want}', got: $(echo "$out" | cut -c1-160)"
  fi
}

# Row 0. Without this every row below is meaningless.
base_out="$(manifest "" "" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
# The same empty-output trap as in `refuses`, and it matters most here: this row is what licenses
# every refusal below, so if it can pass on no output at all, the case's whole discriminating power
# rests on something that was never observed.
if [ -n "$base_out" ] && [ -z "${base_out##*created*}" ]; then
  record PASS "a well-formed deployment is accepted" \
    "baseline passes schema and webhook even with an absent Binding, so a refusal below is about the thing being tested"
else
  record FAIL "a well-formed deployment is accepted" \
    "baseline was refused, so no refusal below proves anything: $(echo "$base_out" | cut -c1-160)"
fi

# --- webhook rows: the manifest is schema-complete, so the webhook is what answers ---

refuses "two roles are refused, naming the spec that introduces them" \
  "multiple roles are not supported by this version" \
  "  - name: decode
    instanceType: ${IT}
    replicas: 1" ""

refuses "an owned argument in extraArgs is refused" \
  "is set by the operator for engine" \
  "    extraArgs:
    - --kv-transfer-config={}" ""

refuses "an owned variable in env is refused" \
  "MOONCAKE_CONFIG_PATH" \
  "    env:
    - name: MOONCAKE_CONFIG_PATH
      value: /tmp/x.json" ""

# The overlay tier is a SECOND path to the same key, and it was the one the rule missed: the
# renderer merges template.env together with env, so an owned key here passed admission and was
# dropped silently at render time.
#
# The wanted fragment is `template.env` rather than the variable name, because the append-tier
# refusal above already quotes the variable -- a case that asserted only the name would pass with
# the overlay rule deleted. It is also not the fully indexed path, so it does not depend on how the
# API server renders a field index.
refuses "an owned variable in the template overlay is refused, naming that tier" \
  "template.env" \
  "    template:
      image: docker.io/library/busybox:1.36
      env:
      - name: MOONCAKE_CONFIG_PATH
        value: /tmp/x.json" ""

# The complete template is the point: see the trap in the header. cpu/ram/localStorage and image are
# all required by the schema, so a shorter sample never reaches this webhook.
refuses "a resource-bearing template is refused, pointing at the two fields that do own it" \
  "the accelerator request belongs in" \
  "    template:
      image: docker.io/library/busybox:1.36
      resources:
        cpu: \"1\"
        ram: \"1Gi\"
        localStorage: \"1Gi\"" ""

# The take-over tier, with NO image. This row asserts an ACCEPTANCE, and it is the one that pins the
# fix for the defect where `roles[].template` inherited a required `image`: every override tier lives
# under `template`, so requiring an image there forced anyone using an overlay to give up the
# synthesized image -- two advertised capabilities excluding each other.
#
# WHAT DOES NOT PIN IT: the same manifest WITH an image. That one is accepted before and after the
# fix, so it discriminates nothing. The absence of `image` is the entire assertion.
takeover_out="$(manifest "    template:
      command:
      - /bin/sh
      - -c
      - sleep 3600" "" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
if [ -n "$takeover_out" ] && [ -z "${takeover_out##*created*}" ]; then
  record PASS "a take-over template without an image is accepted" \
    "template.command with no template.image passes schema and webhook — the overlay tiers do not cost the synthesized image"
else
  record FAIL "a take-over template without an image is accepted" \
    "wanted acceptance, got: $(echo "$takeover_out" | cut -c1-160)"
fi

# --- schema rows: these never reach the webhook, and that is the design ---

refuses "a self-declared reuse domain is refused by strict decoding" \
  "strict decoding error" "" "    domain: i-declare-my-own"

ENGINE=vllm-ascend \
  refuses "an engine outside the enum is refused by the schema" \
  "supported values" "" ""
unset ENGINE

# The cross-namespace refusal is structural rather than enforced: poolRef is a LocalObjectReference,
# so a namespace cannot be written down in the first place. Assert the shape, because an added
# `namespace` field would silently turn a design guarantee into an unwritten rule.
pool_props="$(kubectl get crd modeldeployments.worker.gpustack.ai \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.kvCache.properties.poolRef.properties}' 2>/dev/null)"
if [ -n "$pool_props" ] && [ "${pool_props##*namespace*}" = "$pool_props" ]; then
  record PASS "a cross-namespace poolRef cannot be expressed" \
    "poolRef carries no namespace field, so reaching another namespace is not a rule but a type"
else
  record FAIL "a cross-namespace poolRef cannot be expressed" \
    "poolRef properties are '${pool_props:-<absent>}', which is not the LocalObjectReference shape"
fi

# --- controller row: needs cluster state, so it is a condition rather than a refusal ---

# Clear a leftover from an interrupted run FIRST. The replica row below asserts an exact count, and
# a Pod left behind by an earlier pass carries the same instance label, so it would be counted and
# the row would fail about the leak rather than about the rule.
#
# The alternative -- relaxing that row to `>= 1` -- was rejected: it would also pass on 2, which is
# exactly what the leak produces. A row that tolerates any positive number survives both the leak
# and the race by no longer being able to see either.
kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai case45-nobind \
  --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1
left=""
for _ in $(seq 1 20); do
  left="$(kubectl -n "$NS" get pods \
    -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=case45-nobind \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
  [ "${left:-0}" -eq 0 ] && break
  sleep 3
done

# A DRAIN THAT DID NOT FINISH MUST STOP THE ROWS BELOW RATHER THAN FEED THEM. Falling through leaves
# the replica row counting a Pod from an earlier pass and reporting "got 2 Pod(s)" -- a message about
# the rule, produced by a leftover.
#
# And it must not be recorded as a FAIL either: FAIL claims the rule was violated, while what
# actually happened is that the row's precondition was never established. Those are different
# findings and only one of them is about this operator.
nobind_ready=yes
if [ "${left:-0}" -ne 0 ]; then
  nobind_ready=no
  record SKIP "the controller-level rows for case45-nobind" \
    "$left leftover Pod(s) from an earlier pass survived a 60s drain; every row below would have counted them"
fi

# The role names an image EXPLICITLY here, unlike every row above, and the header says why. A role
# without one gets an image synthesized from the accelerator backend its InstanceType observed, and
# a CPU-only InstanceType has observed none. That refusal is correct and is asserted in the unit
# tier -- model_deployment_image_test.go and model_deployment_render_test.go both pin its message --
# so this row drops the dependency rather than the assertion. It still demands EXACTLY 1 Pod.
#
# THE APPLY IS READ, NOT DISCARDED. `>/dev/null 2>&1` here made a rejected manifest look like a
# controller that never reconciled: both rows below would go red naming a missing condition and a
# Pod count of 0, while the object had never been created at all. That is the same substitution the
# whole case is built to refuse, one layer lower -- and it is the shape a stale CRD schema produces,
# which is precisely the environment this row is most often run in.
if [ "$nobind_ready" = yes ]; then
  apply_out="$(manifest "    template:
      image: docker.io/library/busybox:1.36" "" \
    | sed "s/case45-probe/case45-nobind/" | kubectl apply -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$apply_out" ] || [ -n "${apply_out##*created*}" ]; then
    nobind_ready=no
    record SKIP "the controller-level rows for case45-nobind" \
      "the manifest was never created, so nothing below has a subject: ${apply_out:-<no output at all>}"
  fi
fi

if [ "$nobind_ready" = yes ]; then
  # BOTH TESTS ON $conds GUARD FOR EMPTY FIRST, and each was wrong in a different way without it.
  # `${conds##*pat*}` deletes the longest matching prefix, and deleting anything from "" leaves "",
  # so `-z` is TRUE for an empty $conds -- the value a cluster with no ModelDeployment controller
  # produces. Measured on exactly that cluster: the loop broke on its FIRST iteration, so the 60s
  # window never elapsed, and the row then reported PASS naming a condition that was never read.
  # A poll whose exit test passes on nothing polls once, and a row whose assertion passes on nothing
  # asserts nothing -- one missing guard bought both.
  #
  # Three other tests in this file already carry this guard, added when a reviewer pointed at one of
  # them. These two were not pointed at, and that is the whole lesson: the named site was a sample.
  conds=""
  for _ in $(seq 1 20); do
    conds="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case45-nobind \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason};{end}' 2>/dev/null)"
    [ -n "$conds" ] && [ -z "${conds##*DomainRegistered=*}" ] && break
    sleep 3
  done
  if [ -n "$conds" ] && [ -z "${conds##*DomainRegistered=False/BindingNotFound*}" ]; then
    record PASS "a poolRef naming no Binding is refused by the controller, not by admission" \
      "DomainRegistered=False/BindingNotFound — admission cannot read cluster state, so this is a condition"
  else
    record FAIL "a poolRef naming no Binding is refused by the controller, not by admission" \
      "wanted DomainRegistered=False/BindingNotFound, got: ${conds:-<no conditions at all within 60s>}"
  fi
fi

# The replicas ARE built while the Binding is missing, and the spec requires exactly that:
# convergence is never gated on `DomainRegistered`, because a Binding created a second after the
# deployment is indistinguishable from one that is never coming. Gating would also turn a store
# leader restart — an ordinary operation, measured at 3.5-32 seconds — into an outage of every
# deployment on the pool.
#
# BOTH HALVES OF THIS QUERY WERE WRONG IN THE FIRST VERSION, and it passed anyway. It asked for
# `deployments.apps` carrying `gpustack.ai/model-deployment=<name>`: this kind renders Pods DIRECTLY,
# so no Deployment ever exists, and that label does not exist either. A query for the wrong kind
# filtered by an invented label returns 0 forever, which was exactly the answer the row wanted back
# when it asserted that nothing gets built — so the check could never fail. Both halves were settled
# by rendering one deployment on a live cluster and reading the object back: the Pod is
# `<md>-<role>-<ordinal>` and carries app.kubernetes.io/name=model-deployment plus
# app.kubernetes.io/instance=<md>. Measured side by side, the invented selector counted 0 and this
# one counted 1 against the same running Pod.
# POLLED, NOT SAMPLED ONCE. The loop above exits on the CONDITION, and the condition and the Pod are
# two separate writes; whichever order the reconcile makes them in, a single read taken the instant
# the condition appears can land before the Pod exists and report a FAIL about timing rather than
# about the rule.
#
# The wait is for EXACTLY 1, and the row still fails on any other number, so the poll buys tolerance
# for the race without buying tolerance for a wrong count.
#
# THE TWO WAYS THIS ROW CAN GO RED ARE REPORTED DIFFERENTLY, because they were indistinguishable and
# one of them is not about this operator: a window too short for a loaded cluster produced the very
# same "got 0 Pod(s)" as a render that never happens. Zero now says the window ran out and says that
# the two readings are not separated; any other number says the count is wrong. The window is 120s
# rather than 60s for the same reason -- image pull and scheduling are on this path.
if [ "$nobind_ready" = yes ]; then
  wl=0
  for _ in $(seq 1 40); do
    wl="$(kubectl -n "$NS" get pods \
      -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=case45-nobind \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
    [ "${wl:-0}" -eq 1 ] && break
    sleep 3
  done
  if [ "${wl:-0}" -eq 1 ]; then
    record PASS "the replicas are rendered even though the domain is unregistered" \
      "1 replica Pod alongside DomainRegistered=False/BindingNotFound — convergence is not gated on the domain"
  elif [ "${wl:-0}" -eq 0 ]; then
    record FAIL "the replicas are rendered even though the domain is unregistered" \
      "no replica Pod within 120s — a render that never happens and a render slower than this window look identical here, and this row does not separate them"
  else
    record FAIL "the replicas are rendered even though the domain is unregistered" \
      "wanted exactly the 1 declared replica, got $wl Pod(s)"
  fi
fi

# The other half of that rule — the unregistered domain costs the replicas their connector and
# NOTHING else — cannot be asserted yet, and the reason is worth writing down rather than leaving as
# a bare `deferred`. Measured side by side on a live cluster: a deployment whose Binding is READY
# renders a Pod with 0 env vars, 0 volumes and argv `[vllm serve <model>]` — byte-identical to the
# Pod this case just counted. The connector is not wired into any replica until T14, so "carries no
# connector" is true of every deployment on the cluster and discriminates nothing. Asserting it here
# would pass for a reason that has nothing to do with the domain, which is the failure this whole
# case is built to avoid.
record SKIP "the unregistered domain costs the replicas their connector and nothing else" \
  "deferred: needs T14. Until a connector is wired into any replica at all, a Ready Binding renders the same Pod, so this would hold for the wrong reason"

kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai case45-nobind \
  --wait=true --timeout=60s >/dev/null 2>&1

# --- the chart guard rides here ---

if kubectl get crd modeldeployments.worker.gpustack.ai >/dev/null 2>&1; then
  record PASS "the worker installs its own CRD" \
    "modeldeployments CRD is present without any chart manifest declaring it"
else
  record FAIL "the worker installs its own CRD" \
    "modeldeployments CRD is absent, so either the worker did not install it or the image predates it"
fi

# --- deferred: the serving half ---

record SKIP "replicas: 2 reaches status.roles[0].ready == 2" \
  "deferred: needs the synthesized connector wired into the replicas"
record SKIP "status.endpoint serves inference" \
  "deferred: needs the synthesized connector wired into the replicas"

# Results.
echo
echo "STATUS | CHECK | OBJECT"
# Split on the delimiter `record` actually wrote, not on whitespace — see case-43 for what the
# whitespace split did to multi-word CHECK names.
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-45] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-45] all checks passed (the serving half is deferred; see the SKIP rows)"
