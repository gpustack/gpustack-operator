#!/usr/bin/env bash
#
# CASE 45 — The ModelDeployment admission surface: every refusal fires from the layer that owns it,
#           for one role and for several, and a replicas change replaces nobody
#   (MUTATING, self-cleaning: the refusals are server-side dry-runs; two short-lived deployments
#    serve the controller-level checks)
#
#   case-45.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MD_BINDING,E2E_MD_CONTROL_IMAGE,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE
# Goal:        Pins the refusals a ModelDeployment owes its user, for a deployment that declares ONE
#              role and for a deployment that declares SEVERAL. The interesting part is not that each
#              bad manifest is rejected — it is WHICH layer rejects it, because three different
#              layers can produce a refusal here and they are not interchangeable:
#
#                schema   — the CRD's own openAPIV3Schema: a closed enum, required fields, strict
#                           decoding of unknown fields, and the role list's own rules;
#                webhook  — ValidateCreate, which sees a manifest the schema already accepted;
#                controller — a status condition, for anything that depends on cluster state the
#                           admission moment cannot read.
#
#              THE TRAP THIS CASE EXISTS TO AVOID. A refusal test whose sample is incomplete
#              measures the schema and never reaches the webhook, and it stays green if the webhook
#              is deleted outright. Measured on a live cluster when the role carried a template
#              tier: a role with only part of that tier came back naming a required field of the
#              schema, never the webhook's wording, and only a complete tier reached the webhook.
#              The tier is gone and its required fields with it, but the rule the measurement
#              taught stays: each webhook row here carries a COMPLETE manifest and asserts a
#              fragment of the operator's own wording, never the bare fact of a rejection.
#
#              The same reasoning drives the baseline of each surface: a manifest that must be
#              ACCEPTED. Without it every row below can pass for the wrong reason — a typo in an
#              unrelated field refuses all of them, and a table of refusals reads exactly like a
#              table of working guards.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an operator
#              image carrying the ModelDeployment CRD. No GPU is needed, and NO KVCachePoolBinding
#              has to exist.
#
#              THE ROWS THAT SCHEDULE A REPLICA NEED AN IMAGE THAT EXISTS, and that is now a schema
#              obligation rather than a controller one. `spec.engine.image` is required, so the
#              baseline manifest carries a placeholder that pulls on any platform; a role that names
#              nothing falls back to it. The one row that schedules a replica names its own image,
#              which is the ordinary per-role override, and it still requires EXACTLY 1 Pod, so a
#              convergence wrongly gated on the domain still fails it.
#
#              That the Binding may be absent is not a shortcut but
#              a property worth stating: admission never reads cluster state, so a poolRef naming a
#              Binding that was never created is ACCEPTED by both the schema and the webhook. The
#              missing Binding surfaces later, as a controller condition, which is exactly the row
#              near the end of the single-role phase. Every manifest here therefore names one
#              deliberately absent Binding, and the case leaves no KVCache object behind because it
#              creates none.
#
# Inputs:      All real, nothing mocked. Server-side dry-run (`--dry-run=server`) runs the schema
#              and the webhook and persists nothing; the rows that need a controller verdict create
#              two ModelDeployments naming a Binding that does not exist and delete them again.
#              Before the first of them is deleted that stored object is edited three times, because
#              an UPDATE rule is not exercised by a dry-run create: `size` and `replicas` must be
#              accepted, and the accepted values must be read back from storage. The `model` refusal
#              must name spec.model and state that the value describes a different deployment. It
#              must say nothing of conflict, concurrency, locks or races.
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
#              about what the unregistered domain costs, near the end of the single-role phase.
#
# Expected:    Every refusal fires from the layer the Goal names, the field and message asserted
#              per row; the created-object rows observe creation before they judge; the replicas
#              change converges without replacing the replica already running.
# Cleanup:     An EXIT trap deletes both short-lived deployments and the Kueue Workloads they left.
#              Idempotent, runs on pass AND fail, safe to re-run.
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-45.sh <NS>" >&2
  exit 2
fi

# Deliberately absent: see the Environment note. Admission does not resolve it, and the controller
# row in the single-role phase asserts that the controller does.
BINDING="${E2E_MD_BINDING:-case45-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
# The deployment-wide engine image. No row here runs a container, so what it names does not matter --
# only that it exists, because the field is required and a role that names none falls back to it.
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
# The second image exists only to be DIFFERENT from the first: the control row in the several-role
# phase changes it to move the rendered Pod, and never runs it long enough to care what it does. It
# must still be a real image, because a replica that cannot pull is a replica that never replaces
# its predecessor.
CONTROL_IMAGE="${E2E_MD_CONTROL_IMAGE:-registry.k8s.io/pause:3.9}"
if [ "$CONTROL_IMAGE" = "$IMAGE" ]; then
  echo "[case-45] E2E_MD_IMAGE and E2E_MD_CONTROL_IMAGE are the same image, so the control row" >&2
  echo "          would change nothing and report a rebuild that never happened" >&2
  exit 2
fi
# The name the manifests carry. Each phase sets the name it needs: a dry-run name is never
# persisted, and the two live names are the objects the trap deletes.
MD_NAME=case45-probe
# The dials the rows turn. Each is reset right after the row that turns it, because an assignment
# prefixed to a function call persists in bash after the call returns and would silently move every
# later row off the baseline.
ENGINE=""
KVCACHE_BLOCK=""

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# Every InstanceType A DEPLOYMENT CAN ACTUALLY NAME, in the API's order, which is not the same as
# every one the API returns. The list and the exclusions it applies live in _md-lib.sh: a type on
# its way out and an inactive type are both skipped, and so is an accelerated type whose pool has
# no whole card left.
usable_instance_types() {
  md_usable_instance_types
}

if [ -z "$IT" ]; then
  IT="$(usable_instance_types | sed -n '1p')"
fi
if [ -z "$IT" ]; then
  echo "[case-45] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# A SECOND InstanceType is what the two-instanceType row needs, and it may not exist: a CPU-only
# cluster materializes exactly one pool. The row reports SKIP rather than inventing a name, because
# a name no InstanceType carries would be refused for not resolving. It is chosen by the same rule
# as the first -- a type a deployment can actually name -- rather than by list position, which is how
# this row once picked a zero-card mock type and reported the webhook refusing two types.
IT2="$(usable_instance_types | grep -v -x -F "$IT" | sed -n '1p')"

# Emit a manifest whose roles block is supplied whole by the caller. Unlike the two-role surface
# below, the roles are the variable in every refusal here: one rule is about the SET of roles, the
# other is about one role's fields.
#
# The poolRef name expands with a bare dash and NOT with a colon-dash, and the difference is the
# entire point of the row that uses it: the colon form substitutes the default for an EMPTY value as
# well as an unset one, so it would quietly rewrite the empty name the empty-poolRef row is trying to
# submit -- and that row would then pass against a manifest carrying a perfectly good name.
#
# THIS EXPLANATION LIVES OUT HERE RATHER THAN AS A YAML COMMENT INSIDE THE HEREDOC, and that is not
# taste. The heredoc is unquoted -- it has to be, every field in it is a variable -- so the shell
# expands its contents, backticks included. A YAML comment is inert to YAML and is NOT inert to the
# shell: written inside, this paragraph produced ":-: command not found" on stderr twice per call,
# because the backticks around the two expansion forms were read as command substitution.
# `poolRef` is a LocalObjectReference by design, which is why this case has no cross-namespace row:
# see the assertion on the schema below.
manifest() {
  md_manifest "$MD_NAME" "$NS" "${ENGINE:-vLLM}" "$IMAGE" "${PROBE_BINDING-$BINDING}" \
    "${KVCACHE_BLOCK:-}" "$1"
}

# The ONE role the single-role surface varies from. Every row below changes one field of it and
# leaves the rest alone, which is what makes a refusal there attributable to the field that changed.
one_role() {
  md_server_manifest "$MD_NAME" "$NS" "${ENGINE:-vLLM}" "$IMAGE" "${PROBE_BINDING-$BINDING}" \
    "${KVCACHE_BLOCK:-}" "$IT" "$1"
}

# Two plain roles, prefill and decode, on one instanceType. The shape every row of the several-role
# surface varies from.
#
# The explicit image is plumbing, not part of any rule: the placeholder roles name one so the
# manifest is complete, and nothing asserted here runs a container.
two_roles() {
  md_epd_manifest "$MD_NAME" "$NS" "${ENGINE:-vLLM}" "$IMAGE" "${PROBE_BINDING-$BINDING}" \
    "${KVCACHE_BLOCK:-}" "$IT"
}

# Assert that a manifest is refused AND that the refusal quotes $2 -- and every further fragment
# passed after the roles block. A refusal carrying somebody else's message is a FAIL: it means the
# manifest tripped a rule this row is not about.
#
# AN EMPTY $out MUST NOT PASS. The refusal is read out of the apply's output, and an output that is
# empty at all means nothing was refused: the row would report a refusal it never saw. A check that
# passes when nothing happened is measuring nothing, and this one would do it for every row at once.
refuses() {
  local check="$1" want="$2" roles="$3" text
  shift 3
  if text="$(manifest "$roles" | md_dryrun_refuses "$want" "$@")"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

# The mirror of `refuses`, for a row whose point is that a value is NOT rejected. It carries the same
# empty-output trap for the same reason: "created" must be observed, because a row that passes on no
# output at all reports an acceptance nothing produced.
accepts() {
  local check="$1" roles="$2" text
  if text="$(manifest "$roles" | md_dryrun_accepts)"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

# ---------------------------------------------------------------- the single-role surface
#
# One role, and the rules that answer for a deployment of one.

# Row 0. Without this every row below is meaningless.
base_out="$(manifest "$(one_role "")" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
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

# A SECOND ROLE IS NO LONGER REFUSED, so the row that asserted it is gone rather than inverted. What
# replaced it is the several-role surface below: its row 0 applies a two-role deployment and requires
# it to be ACCEPTED, which is the same fact asserted from the side that now holds. Keeping a refusal
# row here would have it fail against a correct operator, and keeping an acceptance row here would
# duplicate a baseline the surface below already licenses its whole table on.

# THE SIZE CAP IS GONE, AND THE ROW IS INVERTED RATHER THAN DELETED. It used to assert a refusal
# worded as a missing capability -- deliberately, so that the message would stop being true at the
# same moment the limit did. That day came: a role's instance may span several Pods, so the value
# this row sends is now the ordinary case and the assertion is that nothing refuses it.
accepts "a role asking for more than one Pod per instance is accepted" \
  "$(one_role "    size: 2")"

refuses "an owned argument in extraArgs is refused" \
  "is set by the operator for engine" \
  "$(one_role "    extraArgs:
    - --kv-transfer-config={}")"

refuses "an owned variable in env is refused" \
  "MOONCAKE_CONFIG_PATH" \
  "$(one_role "    env:
    - name: MOONCAKE_CONFIG_PATH
      value: /tmp/x.json")"

# The template tier no longer exists, and strict decoding is the refusal that says so: a field kept
# only to improve one error message was a promise the schema made and nothing kept. This row pins
# the disappearance itself, which is what replaced the old resources rule -- an unknown-field error
# naming `template` rather than a webhook message about where the request belongs.
refuses "the template tier is refused as an unknown field" \
  "strict decoding error" \
  "$(one_role "    template:
      image: docker.io/library/busybox:1.36")"

# The take-over tier: a role that replaces the whole command line. It asserts an ACCEPTANCE, because
# owning the argv and naming an image are independent choices -- the operator runs whatever the role
# says, and the command it would otherwise build is simply not built.
#
# THE IMAGE IS NOW REQUIRED, and that is why this row carries one. It used to assert the OPPOSITE:
# that a take-over role could omit `image`, because the operator synthesized one from the
# InstanceType. That capability is gone, so the row's old subject no longer exists. What survives is
# the question underneath it -- does taking over the argv still admit -- and the row keeps asking it.
accepts "a take-over role with an image is accepted" \
  "$(one_role "    image: docker.io/library/busybox:1.36
    command:
    - /bin/sh
    - -c
    - sleep 3600")"

# --- schema rows: these never reach the webhook, and that is the design ---

# THE ONLY RULE HERE THAT IS IN THE WEBHOOK BECAUSE OF A TYPE RATHER THAN BY CHOICE, which is why it
# earns a row of its own. Every other bound in this spec gets its lower bound from a schema marker --
# earlier, cheaper, and applied to every client. This field's type is upstream's
# `core.LocalObjectReference`, and a marker cannot be attached to a struct this API does not own, so
# the bound has to live in the webhook. `required` makes the KEY present, not the VALUE non-empty:
# `poolRef: {name: ""}` satisfies the schema completely.
#
# PROBE_BINDING is this case's own variable, unlike ENGINE above, so clearing it afterwards restores
# the default rather than discarding something a caller set.
PROBE_BINDING=""
refuses "an empty poolRef name is refused by the webhook, which is where the type forces it" \
  "must not be empty" "$(one_role "")"
unset PROBE_BINDING

KVCACHE_BLOCK="    domain: i-declare-my-own"
refuses "a self-declared reuse domain is refused by strict decoding" \
  "strict decoding error" "$(one_role "")"
KVCACHE_BLOCK=""

ENGINE=vllm-ascend
refuses "an engine outside the enum is refused by the schema" \
  "supported values" "$(one_role "")"
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

# ---------------------------------------------------------------- the live object and its edits

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

# THE TRAP RIDES HERE, and it covers BOTH short-lived objects of this case: the one the rows below
# edit and the one the several-role phase creates later. It is ARMED BEFORE THE FIRST APPLY, not
# after it. Interrupted between the apply and the delete at the end of this phase, the case leaks a
# ModelDeployment whose finalizer holds it, and the next run either collides on the name or counts
# the leftover replica into the row below.
#
# The Workloads go with the deployments: Kueue keeps a finalizer on every Pod of a serving group and
# drops it only when that Workload goes, so a cleanup that left them behind would hold the quota the
# next run needs.
cleanup() {
  local md wl row
  for md in case45-nobind case51-rebuild; do
    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done
  sleep 5
  # One list call: this trap runs on every exit, pass or fail, so a get per Workload adds latency to
  # every invocation of the case on a busy namespace.
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    case "${row#*=}" in
      *case45-nobind-*|*case51-rebuild-*)
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

# The role names its own image here, which overrides the engine-wide one the baseline manifest
# carries. That is the ordinary way to pin one role to another build, and it is worth exercising on
# the one row that actually schedules a replica: it proves the override reaches the Pod spec, not
# only the schema. The row still demands EXACTLY 1 Pod.
#
# THE APPLY IS READ, NOT DISCARDED. `>/dev/null 2>&1` here made a rejected manifest look like a
# controller that never reconciled: both rows below would go red naming a missing condition and a
# Pod count of 0, while the object had never been created at all. That is the same substitution the
# whole case is built to refuse, one layer lower -- and it is the shape a stale CRD schema produces,
# which is precisely the environment this row is most often run in.
MD_NAME=case45-nobind
if [ "$nobind_ready" = yes ]; then
  apply_out="$(manifest "$(one_role "    image: docker.io/library/busybox:1.36")" \
    | kubectl apply -f - 2>&1 | tr '\n' ' ')"
  # `created` OR `configured` OR `unchanged`: the delete at the top of this block passes
  # --timeout=60s, and a finalizer that outlasts it leaves the object in place, so a re-apply
  # reports the object it found rather than one it made. All three mean the same thing to the rows
  # below -- the object exists -- while only "created" would turn a slow finalizer into a FAIL
  # blaming the controller.
  if [ -z "$apply_out" ] || { [ -n "${apply_out##*created*}" ] && [ -n "${apply_out##*configured*}" ] &&
    [ -n "${apply_out##*unchanged*}" ]; }; then
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

# The other half of that rule, that the unregistered domain costs the replicas their connector and
# NOTHING else, is not asserted here. A Ready Binding now renders a client-config annotation, a
# downwardAPI projection of it, a mount and the engine's config-path variable, so "carries no
# connector" is a discriminating claim, but only beside a registered deployment to compare with. This
# case deliberately names a Binding that does not exist, so every deployment in it is unregistered
# and the assertion would pass without ever seeing a registered one. case 47 asserts it with that
# control: a Ready Binding over a live Mooncake backend beside an unreachable one.
record SKIP "the unregistered domain costs the replicas their connector and nothing else" \
  "asserted by case 47, which hosts the Ready-Binding control this case cannot: every deployment here is unregistered"

# --- the two rows that need a STORED object, because they are about an edit ---
#
# Every refusal above is a create refused by `--dry-run=server`, which has nothing to compare
# against. Freezing a field is a rule about the difference between two versions of one object, so it
# cannot fire on a create at all and no dry run reaches it. These two patch the deployment that is
# still standing.
#
# Ordinary size and replica-count edits are accepted. Model and engine identity remain frozen.
if [ "$nobind_ready" = yes ]; then
  size_out="$(kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai case45-nobind --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/size","value":2}]' 2>&1 | tr '\n' ' ')"
  stored_size="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case45-nobind \
    -o jsonpath='{.spec.roles[0].size}' 2>/dev/null)"
  if [ -n "$size_out" ] && [ -z "${size_out##*patched*}" ] && [ "$stored_size" = 2 ]; then
    record PASS "an ordinary role size edit is accepted and stored" "size=${stored_size}"
  else
    record FAIL "an ordinary role size edit is accepted and stored" \
      "wanted size=2, stored=[${stored_size:-none}], patch: $(echo "$size_out" | cut -c1-160)"
  fi

  # THE MODEL SAYS WHICH DEPLOYMENT THIS IS, so it is frozen too, and its refusal is read for its
  # wording. An earlier framing of this freeze was "shrink the surface two writers can disagree on";
  # a message carrying it sends an operator looking for a locking problem that does not exist.
  model_out="$(kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai case45-nobind --type=merge \
    -p '{"spec":{"model":{"name":"case45/another-model"}}}' 2>&1)"
  model_rc=$?
  model_out="$(echo "$model_out" | tr '\n' ' ')"
  if [ "$model_rc" -ne 0 ] && [ -z "${model_out##*spec.model*}" ]; then
    record PASS "an edit to the model is refused on a live object, naming the field" "refused at spec.model"
  else
    record FAIL "an edit to the model is refused on a live object, naming the field" \
      "rc=${model_rc}, wanted a refusal naming spec.model, got: $(echo "$model_out" | cut -c1-160)"
  fi
  if [ "$model_rc" -eq 0 ]; then
    # No refusal to read: these two rows are about its wording, so they are unmeasured, not failed.
    record SKIP "the model refusal states the rule" "the edit was accepted; there is no refusal to read"
    record SKIP "the model refusal states no mechanism" "the edit was accepted; there is no refusal to read"
  else
    if [ -z "${model_out##*describes a different deployment*}" ]; then
      record PASS "the model refusal states the rule" "a different value describes a different deployment"
    else
      record FAIL "the model refusal states the rule" "$(echo "$model_out" | cut -c1-160)"
    fi
    wrong=""
    for word in conflict concurrent lock race; do
      case "$model_out" in *"$word"*) wrong="$wrong $word" ;; esac
    done
    if [ -z "$wrong" ]; then
      record PASS "the model refusal states no mechanism" "none of conflict/concurrent/lock/race"
    else
      record FAIL "the model refusal states no mechanism" "carries:${wrong}"
    fi
  fi

  replicas_out="$(kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai case45-nobind --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1 | tr '\n' ' ')"
  if [ -n "$replicas_out" ] && [ -z "${replicas_out##*patched*}" ]; then
    record PASS "the control: an edit to a role's replicas is accepted" \
      "patched alongside the accepted size edit"
  else
    record FAIL "the control: an edit to a role's replicas is accepted" \
      "wanted the patch to be accepted, got: $(echo "$replicas_out" | cut -c1-160)"
  fi
  stored_replicas="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case45-nobind \
    -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null)"
  if [ "$stored_replicas" = 2 ]; then
    record PASS "the accepted replicas edit reached storage" "replicas=${stored_replicas}"
  else
    record FAIL "the accepted replicas edit reached storage" "replicas=[${stored_replicas:-none}]"
  fi
else
  record SKIP "an ordinary role size edit is accepted and stored" \
    "the subject deployment was never created, so there is no stored object to edit"
  record SKIP "an edit to the model is refused on a live object, naming the field" \
    "the subject deployment was never created, so there is no stored object to edit"
fi

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

# Neither row waits on the connector being wired into the replicas, which it now is: a replica
# reaching Ready needs a container that serves, and this case runs on a CPU-only cluster against
# busybox. Naming the real blocker matters more than the deferral itself, since a stale reason sends
# the next reader to look for work that is already done.
record SKIP "replicas: 2 reaches status.roles[0].ready == 2" \
  "deferred: needs an engine image that actually serves, which needs an accelerator; this case is CPU-only by design"
record SKIP "status.endpoint serves inference" \
  "deferred: needs a serving replica behind the Service, so it carries the same requirement as the row above"

# ---------------------------------------------------------------- the several-role surface
#
# The rules that arrived with several roles. The trap is the single-role surface's, and it is worse
# here: every manifest below carries several roles, so a mistake in the SHARED part of the manifest
# refuses all of them — and a row-per-rule table of refusals reads identically whether the rules
# exist or not. So row 0 is a two-role deployment that must be ACCEPTED, and every refusal row
# asserts a fragment of the operator's own wording rather than the bare fact of a rejection.
#
# THE ACCEPTED BASELINE ALSO CARRIES THE HEADLINE. Two roles being accepted at all is the rule this
# whole spec exists to lift: the single-role version refused them by name, and the single-role
# surface above says so.
#
# NO ROW READS THE CLUSTER ANY MORE. Every rule this surface asserts is answered from the submitted
# object, so no row's outcome depends on what a pool's flavors happen to pin or on whether a cache
# has caught up.

MD_NAME=case51-probe

# --- row 0: the baseline, and the headline ---

pdo_out="$(manifest "$(two_roles)" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
case "$pdo_out" in
  *"created (server dry run)"*|*"configured (server dry run)"*)
    record PASS "two roles are ACCEPTED" \
      "the single-role bound is lifted; every refusal below is therefore its own rule" ;;
  *)
    record FAIL "two roles are ACCEPTED" \
      "the baseline was refused, so every row below passes for the wrong reason: ${pdo_out:0:220}" ;;
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
#
# TWO FRAGMENTS, because the API server prints the duplicate key in a version-dependent form -- one
# release writes `{"name":"worker"}`, a later one `map[string]interface {}{"name":"worker"}`. What
# both keep is the field path with the schema's own verdict, and the key itself.
refuses "two roles sharing a name are refused BY THE SCHEMA" \
  'spec.roles[1]: Duplicate value:' \
  "  - name: worker
    instanceType: ${IT}
    replicas: 1
  - name: worker
    instanceType: ${IT}
    replicas: 1" \
  '{"name":"worker"}'

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
    "needs a second InstanceType a deployment can name, and this cluster has none besides ${IT}. NOT closed by a synthetic name, which is now refused for not existing; CASE 68 covers the same premise by CREATING its second type"
fi

refuses "kind: Server beside another kind is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: Server
    instanceType: ${IT}
    replicas: 1
  - name: prefill
    kind: Prefill
    instanceType: ${IT}
    replicas: 1"

# THIS ROW WAS A REFUSAL AND IS NOW AN ACCEPTANCE, for the reason the rule's own comment gives: both
# engines render all three kinds, so no engine this API accepts is refused by it today. The rule stays
# for an engine added to the renderer without a table entry, which an API-valid manifest cannot
# name. SGLang prefill/decode is the pair that rule once refused, so its acceptance is the row that
# reports the refusal coming back.
ENGINE=SGLang accepts "an sglang prefill/decode pair is ACCEPTED: every engine renders every kind" \
  "$(two_roles)"

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

# ---------------------------------------------------------------- the live convergence
#
# What a replicas change does to the replicas already running, which is a convergence rather than a
# refusal. The deployment here is the SEVERAL-ROLE one, because the row is about the SIBLING role:
# a scale that rebuilt only the role it names would still be a regression, and a check reading one
# role alone would call it a pass.
#
# THE OBJECT IS ITS OWN, and not the single-role one the phase above edits: that one declares ONE
# role and its replica row demands EXACTLY 1 Pod, while this one's rows count a group per role and
# compare UIDs across a scale. One deployment cannot answer both, so the case creates one for each
# and deletes the first before the second is made -- the CPU quota the second needs is the first's
# until the first is gone.

REBUILD_MD=case51-rebuild
MD_NAME="$REBUILD_MD"

# THE DECLARED TOTAL IS ONE AND STAYS ONE, whatever the replica counts are. Each replica is a Kueue
# group of its own, so the number a Pod declares is a property of the group it is alone in rather
# than of the deployment it belongs to -- which is precisely what lets a replicas change leave every
# living replica untouched. A reading of anything else here means a Pod joined a group expecting a
# sibling that will never arrive, and Kueue composes nothing for it.
DECLARED_TOTAL=1

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
  md_role_uids "$NS" "$REBUILD_MD" "$1"
}

# The UIDs of one role that are STILL PRESENT out of a recorded set, as a count.
surviving() {
  md_surviving_uids "$NS" "$REBUILD_MD" "$1" "$2"
}

# KEPT, unlike a dry-run row's: every rebuild row below depends on this object existing, so a refusal
# here would otherwise surface as three timeouts with three wrong diagnoses instead of one message.
REBUILD_APPLY="$(manifest "$(two_roles)" | kubectl apply -f - 2>&1)"

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
  -p '{"spec":{"roles":[{"name":"prefill","kind":"Prefill","instanceType":"'"$IT"'","replicas":2,'"$TPL"'},{"name":"decode","kind":"Decode","instanceType":"'"$IT"'","replicas":1,'"$TPL"'}]}}' \
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
  want_p="$(grep -c . <<<"$PREFILL_BEFORE" || true)"
  want_d="$(grep -c . <<<"$DECODE_BEFORE" || true)"
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
# that never applied -- so the same instrument has to be shown reporting the other value.
#
# THE CONTROL IS AN IMAGE CHANGE, NOT A ROLE RENAME. A rename would replace every replica of the
# renamed role, which is the reading this row wants, but no rename ever reaches the converger: the
# role set is part of a deployment's identity and admission refuses any edit to it, in either
# direction. A control the API rejects is a control that never runs, and a row that records SKIP
# leaves the assertion above resting on an instrument nobody watched answer twice.
#
# An image change is refused by nothing, moves the rendered Pod, and therefore rolls every replica
# of the role it names -- one at a time, which is why this waits for the whole role to turn over
# rather than for a single Pod.
CONTROL_BEFORE="$(role_uids decode)"
CONTROL_PATCH="$(kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai "$REBUILD_MD" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/1/image","value":"'"$CONTROL_IMAGE"'"}]' 2>&1)"

CONTROL_DONE=no
for _ in $(seq 1 90); do
  [ "$(surviving "$CONTROL_BEFORE" decode)" = 0 ] && { CONTROL_DONE=yes; break; }
  sleep 2
done

if [ "$CONTROL_DONE" = yes ]; then
  record PASS "the control: an image change DOES replace that role's replicas" \
    "none of the old decode UIDs survived the image change, so the reading above is the instrument answering rather than failing to look"
else
  record FAIL "the control: an image change DOES replace that role's replicas" \
    "$(surviving "$CONTROL_BEFORE" decode) of the old decode UID(s) outlived an image change, so this instrument cannot tell a replacement from a survivor and the row above is unproven. patch said: ${CONTROL_PATCH:0:200}"
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-45] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-45] all checks passed (the serving half is deferred; see the SKIP rows)"
