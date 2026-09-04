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
#              image carrying the ModelDeployment CRD. No GPU is needed: nothing here schedules a
#              replica, and NO KVCachePoolBinding has to exist. That last part is not a shortcut but
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
# Deferred:    The serving half of this case — `replicas: 4` reaching `status.roles[0].ready == 4`
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
  if [ -z "${out##*$want*}" ]; then
    record PASS "$check" "refused, quoting: ${want}"
  else
    record FAIL "$check" "wanted a refusal quoting '${want}', got: $(echo "$out" | cut -c1-160)"
  fi
}

# Row 0. Without this every row below is meaningless.
base_out="$(manifest "" "" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
if [ -z "${base_out##*created*}" ]; then
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

manifest "" "" | sed "s/case45-probe/case45-nobind/" | kubectl apply -f - >/dev/null 2>&1
conds=""
for _ in $(seq 1 20); do
  conds="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case45-nobind \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason};{end}' 2>/dev/null)"
  [ -z "${conds##*DomainRegistered=*}" ] && break
  sleep 3
done
if [ -z "${conds##*DomainRegistered=False/BindingNotFound*}" ]; then
  record PASS "a poolRef naming no Binding is refused by the controller, not by admission" \
    "DomainRegistered=False/BindingNotFound — admission cannot read cluster state, so this is a condition"
else
  record FAIL "a poolRef naming no Binding is refused by the controller, not by admission" \
    "wanted DomainRegistered=False/BindingNotFound, got: ${conds:-<no conditions>}"
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
wl="$(kubectl -n "$NS" get pods \
  -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=case45-nobind \
  -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
if [ "${wl:-0}" -eq 1 ]; then
  record PASS "the replicas are rendered even though the domain is unregistered" \
    "1 replica Pod alongside DomainRegistered=False/BindingNotFound — convergence is not gated on the domain"
else
  record FAIL "the replicas are rendered even though the domain is unregistered" \
    "wanted the 1 declared replica to be rendered anyway, got ${wl:-0} Pod(s)"
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

record SKIP "replicas: 4 reaches status.roles[0].ready == 4" \
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
