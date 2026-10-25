#!/usr/bin/env bash
#
# CASE 128 — the ModelDeploymentScaler admission surface: the defaults it fills, the refusals it
#             makes, and the single-writer fence it holds on a bound deployment
#             (MUTATING, self-recovering; AUTO-SKIPS the rows that need a target on a cluster
#             without a usable InstanceType)
#
#   case-128.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C128_BOUND,E2E_MD_IMAGE
# Goal:        A scaler is a planner that owns the replica count of the deployment it names, so
#              three things have to be true of it before it may move anything. First, the values it
#              omits are filled in: the webhook judges the object as it will be stored, so a
#              default never leaves a rule reading a field that is about to change. Second, the
#              shapes that could never commit anything are refused at admission, each by the rule
#              that owns it: the schema for the bounds it states, the webhook for the signal it
#              names and the roles it plans for. Third, a deployment that carries a binding has
#              ONE writer of its replica count, and that writer is the scaler: a user may change
#              anything else about a bound deployment, but not the count and not the two
#              annotations that record who owns it.
#
#              THE BINDING FOLLOWS THE TARGET, NOT THE SCALER. The annotation on the deployment
#              names the scaler and the term it wrote under, so a target is bindable by one
#              scaler at a time, and a scaler that wants another target is refused rather than
#              moved: the old target would be left holding a binding nobody may write.
#
#              THE TERM NEVER GOES BACKWARDS. A scaler that is suspended gives the binding up and
#              takes it again afterwards, and the term it writes then is higher than the one it
#              wrote before, so a reader can tell which binding is newer.
#
# Environment: Any reachable API server with the ModelDeploymentScaler CRD installed (run case-1
#              first to materialize the scheduling chain). The defaulting rows and the refusals
#              that do not name a target run everywhere. The rows that read a target need a usable
#              InstanceType reporting an admitted platform (ascend, nvidia or amd), because the
#              webhook reads the type a role names; without one they record SKIP and say why.
#
#              AUTO-SKIP: the target-bound rows (the three refusals that read the target, and the
#              whole binding lifecycle) record SKIP when no usable InstanceType exists, when the
#              target cannot be created, or when the elastic-EP target is refused by the
#              ModelDeployment webhook on this cluster.
#
# Inputs:      MOCKED: nothing. The placeholder image (E2E_MD_IMAGE, default
#              registry.k8s.io/pause:3.10) is plumbing for a Pod spec that would otherwise be
#              refused for reasons none of these rows are about; it is never pulled. REAL: the
#              CRD schema, the defaulting and validating webhooks, the scaler controller's
#              binding, and the ModelDeployment webhook's single-writer fence.
#
# Expected:    - the defaults: mode Shadow, a 15 second interval, a 300 second pending timeout, a
#                floor of one replica per role, a scale-up step cap of 4 replicas and 100
#                percent, a 300 second scale-down window, a 300 second abort cooldown, a 120
#                second decode grace, the Opposing rebalance policy, and the token throughput
#                windows and targets;
#              - the refusals: a scaler naming no target, a ceiling above the schema's, a
#                proportional algorithm naming no metric, a role target of zero, a scale-up
#                window above the schema's, a set of bounds and ratios that no integer vector
#                satisfies, a role the target does not have, a target role that runs a command
#                of its own, and a target role on the elastic-EP profile;
#              - the lifecycle: a shadow scaler binds and observes without touching the target,
#                a scaler may not be pointed at another target, an active scaler takes the
#                binding, a second scaler on the same target is refused it, a user may not
#                write the count or the annotations of a bound deployment but may write
#                everything else, a suspended scaler gives the binding up and the count becomes
#                writable again, and the scaler that comes back writes a higher term.
#
# Cleanup:     Deletes the scalers and the deployments it created, on completion and on exit. The
#              scalers carry a finalizer, so the deletion waits for the controller to release the
#              binding first.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-128.sh <NS>" >&2
  exit 2
fi

IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

# The scaler is served by the CRD at v1alpha1 and by the aggregated API at v1. The qualified name
# names the CRD, so the resource is never ambiguous to kubectl.
SCALER_RESOURCE=modeldeploymentscalers.v1alpha1.worker.gpustack.ai

# THE DEPLOYMENT IS ALSO NAMED BY ITS CRD, and this matters for more than ambiguity. The
# aggregated v1 API serves the same kind and proxies every write to the v1alpha1 CRD through the
# operator's own client, so a write that goes through it reaches admission as the operator and the
# single-writer fence answers with the operator's rules instead of the user's. The fence rows
# below are about what a user may write, so they must carry the user's identity, and only the CRD
# carries it.
MD_RESOURCE=modeldeployments.v1alpha1.worker.gpustack.ai
BINDING_KEY='modeldeployment.gpustack.ai/scaler-binding'
FINALIZER_KEY='modeldeployment.gpustack.ai/scaler-binding'

DEFAULTS_SCALER=case128-defaults
ABSENT_TARGET=case128-absent-target
TARGET=case128-target
CMD_TARGET=case128-target-command
ELASTIC_TARGET=case128-target-elastic
S1=case128-scaler1
S2=case128-scaler2

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

cleanup() {
  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$DEFAULTS_SCALER" "$S1" "$S2" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RESOURCE" "$TARGET" "$CMD_TARGET" "$ELASTIC_TARGET" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

# ---------------------------------------------------------------- the readers

# The condition of a scaler, as "status|reason". Empty when the scaler is gone or has not
# published the condition yet.
scaler_condition() { # <scaler> <type>
  kubectl -n "$NS" get "$SCALER_RESOURCE" "$1" -o json 2>/dev/null \
    | jq -r --arg t "$2" \
      '([(.status.conditions // [])[] | select(.type == $t) | (.status + "|" + (.reason // ""))] | .[0]) // ""'
}

# Wait until a scaler reports a condition as "<status>|<reason>", or the bound expires.
wait_condition() { # <scaler> <type> <status> <reason> <seconds>
  local scaler="$1" ctype="$2" want="$3" reason="$4" bound="$5"
  local deadline value
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    value="$(scaler_condition "$scaler" "$ctype")"
    if [ "$value" = "$want|$reason" ]; then
      return 0
    fi
    sleep 3
  done
  return 1
}

# The binding annotation a target carries, as its raw JSON, or empty.
target_binding() { # <target>
  kubectl -n "$NS" get "$MD_RESOURCE" "$1" -o json 2>/dev/null \
    | jq -r --arg k "$BINDING_KEY" '.metadata.annotations[$k] // ""'
}

# The term of the binding a target carries, or empty.
target_binding_term() { # <target>
  local raw
  raw="$(target_binding "$1")"
  if [ -z "$raw" ]; then
    echo ""
    return 0
  fi
  echo "$raw" | jq -r '.term // ""'
}

# The replica count of a role of a deployment, or empty.
role_replicas() { # <target> <role>
  kubectl -n "$NS" get "$MD_RESOURCE" "$1" -o json 2>/dev/null \
    | jq -r --arg r "$2" '[(.spec.roles // [])[] | select(.name == $r) | .replicas] | .[0] // ""'
}

# The value of a jq path on the stored scaler.
stored() { # <scaler> <jq path>
  kubectl -n "$NS" get "$SCALER_RESOURCE" "$1" -o json 2>/dev/null | jq -r "$2"
}

# Assert that a reading equals a value.
expect() { # <check> <actual> <expected>
  if [ "$2" = "$3" ]; then
    record PASS "$1" "the reading is ${3}"
  else
    record FAIL "$1" "expected ${3}, got ${2:-<empty>}"
  fi
}

# Assert that a scaler manifest is REFUSED, and that the refusal names a phrase.
#
# THE PHRASE IS THE RULE'S OWN WORDING where the rule owns the wording, and the FIELD PATH where
# the schema owns it, because the schema and the webhook word the same fault differently and a
# row that pinned one word would fail on the other layer's phrasing.
#
# The instrument is the library's: it reads the manifest on stdin, prints the text of the row and
# returns the verdict. The row stays here, so the table and the verdict never leave the case that
# owns them.
refuses() { # <check> <phrase>
  local check="$1" phrase="$2" obj status=0
  obj="$(md_dryrun_refuses "$phrase")" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

# Assert that a scaler manifest is ACCEPTED, which is what makes the refusals above mean
# something: a manifest wrong in some unrelated way would refuse too.
accepts() { # <check>
  local check="$1" obj status=0
  obj="$(md_dryrun_accepts)" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

# ---------------------------------------------------------------- the usable InstanceType

# The rows that read a target need one, because the webhook reads the platform the type a role
# names reports. The type must not be deleting and must not be inactive. The lookup is the
# library's: md_usable_type_for.
IT=""
for m in ascend nvidia amd; do
  found="$(md_usable_type_for "$m")"
  if [ -n "$found" ]; then
    IT="$found"
    break
  fi
done

# ---------------------------------------------------------------- the defaults

# THE DEFAULTS ARE READ FROM A STORED OBJECT, not from a dry run. A dry run shows what the server
# would have stored; the stored object shows what a reader of the API sees afterwards, which is
# the claim that matters.
#
# THE TARGET DOES NOT EXIST ON PURPOSE. The rules that need the target are skipped when it is
# absent, so the object is judged on what the scaler itself says, and the defaults are read
# without a cluster that has a GPU in it.
kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
    - name: decode
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
EOF

if kubectl -n "$NS" get "$SCALER_RESOURCE" "$DEFAULTS_SCALER" >/dev/null 2>&1; then
  expect "the mode defaults to Shadow" "$(stored "$DEFAULTS_SCALER" '.spec.mode')" "Shadow"
  expect "the sampling interval defaults to 15 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.sampling.intervalSeconds')" "15"
  expect "the pending timeout defaults to 300 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.sampling.pendingTimeoutSeconds')" "300"
  expect "a role's floor defaults to one replica" \
    "$(stored "$DEFAULTS_SCALER" '.spec.roles[0].minReplicas')" "1"
  expect "the scale-up step is capped at 4 replicas by default" \
    "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleUp.maxStepReplicas')" "4"
  expect "the scale-up step is capped at 100 percent by default" \
    "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleUp.maxStepPercent')" "100"
  expect "the scale-up window is left unset, which is a scaler that acts at once" \
    "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleUp.stabilizationWindowSeconds // 0')" "0"
  expect "the scale-down window defaults to 300 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleDown.stabilizationWindowSeconds')" "300"
  expect "the cooldown after an aborted retirement defaults to 300 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleDown.cooldownAfterAbortSeconds')" "300"
  expect "the decode grace after a scale-up defaults to 120 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleDown.decodePostScaleUpGraceSeconds')" "120"
  expect "the rebalance policy defaults to Opposing" \
    "$(stored "$DEFAULTS_SCALER" '.spec.rebalance.policy')" "Opposing"
  expect "the stable window defaults to 180 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.stableWindowSeconds')" "180"
  expect "the panic window defaults to 60 seconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.panicWindowSeconds')" "60"
  # The threshold is a quantity, so the API server stores it in its canonical form: a whole
  # number stays whole, and the reader sees "2" rather than "2.0".
  expect "the panic threshold defaults to 2" \
    "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.panicThreshold')" "2"
  expect "the time to first token target defaults to 500 milliseconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.ttftTargetMilliseconds')" "500"
  expect "the inter-token latency target defaults to 50 milliseconds" \
    "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.itlTargetMilliseconds')" "50"
else
  record FAIL "the defaults are read from a stored scaler" \
    "the scaler ${DEFAULTS_SCALER} was refused or did not reach the API server, so no default could be read"
fi

# ---------------------------------------------------------------- the refusals

# The baseline is the same shape as the refusals below with nothing wrong with it, accepted by
# the same instrument, so a refusal is about the rule and not about the manifest.
accepts "a scaler naming a target that does not exist yet is ACCEPTED" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-baseline
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
EOF

refuses "a scaler naming no target is refused" "spec.targetRef.name" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-notarget
  namespace: ${NS}
spec:
  targetRef: {}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
EOF

refuses "a ceiling above the schema's is refused" "spec.roles[0].maxReplicas" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-ceiling
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 2000
  algorithm:
    type: TokenThroughput
EOF

refuses "a scale-up window above the schema's is refused" "spec.behavior.scaleUp.stabilizationWindowSeconds" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-window
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 4000
EOF

refuses "a sampling interval below the schema's is refused" "spec.sampling.intervalSeconds" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-interval
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
  sampling:
    intervalSeconds: 1
EOF

# THE METRIC IS NAMED, NOT ASSUMED. The proportional algorithm divides what it observed by the
# target of a role, and the signal it divides is the one the author names, so the schema requires
# it: no default fills it, because a default would pick the signal for the author.
refuses "a proportional algorithm naming no metric is refused" "spec.algorithm.proportional.metric" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-metric
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      roleTargets:
        - name: prefill
          target: "4"
EOF

refuses "a role target of zero is refused" "the target of a role is the signal one replica of it is expected to carry" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-zerotarget
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: prefill
          target: "0"
EOF

# A SET OF BOUNDS AND RATIOS THAT CANNOT HOLD AT ONCE is a scaler that could never commit
# anything, so it is refused rather than left holding a target forever. The search is over
# integers: the prefiller may hold at most two replicas and the ratio demands three of them for
# every decoder, so no integer vector inside the bounds satisfies it.
refuses "bounds and ratios that no integer vector satisfies are refused" \
  "no replica vector inside these bounds satisfies every ratio at the same time" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-infeasible
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
  ratios:
    - numeratorRole: prefill
      denominatorRole: decode
      target: "3"
      min: "3"
      max: "3"
  algorithm:
    type: TokenThroughput
EOF

# ---------------------------------------------------------------- the targets

# The rows below read a target, so they need one this cluster can hold. Without a usable
# InstanceType there is no deployment the webhook would admit, and the rows record SKIP naming
# that rather than passing on a shape nobody checked.
TARGET_REASON=""
if [ -z "$IT" ]; then
  TARGET_REASON="no usable InstanceType reports an admitted platform (ascend, nvidia, amd), so there is no deployment the webhook would admit and no target to read"
fi

if [ -n "$IT" ]; then
  out="$(kubectl apply -f - 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${TARGET}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: /models
  roles:
    - name: prefill
      kind: Prefill
      replicas: 2
      size: 1
      instanceType: ${IT}
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: 2
      size: 1
      instanceType: ${IT}
      resources:
        accelerator: "1"
EOF
)"
  case "$out" in
    *created* | *configured*) ;;
    *) TARGET_REASON="the deployment ${TARGET} was refused on this cluster: $(echo "$out" | tr '\n' ' ' | cut -c1-200)" ;;
  esac
fi

# The deployment whose role runs a command of its own. A scaler cannot tell what that command
# serves, so it cannot tell when the role is busy.
CMD_REASON=""
if [ -n "$IT" ] && [ -z "$TARGET_REASON" ]; then
  out="$(kubectl apply -f - 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${CMD_TARGET}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: /models
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      instanceType: ${IT}
      command:
        - /bin/sh
        - -c
        - sleep 3600
      resources:
        accelerator: "1"
EOF
)"
  case "$out" in
    *created* | *configured*) ;;
    *) CMD_REASON="the deployment ${CMD_TARGET} was refused on this cluster: $(echo "$out" | tr '\n' ' ' | cut -c1-200)" ;;
  esac
fi

# The deployment whose role runs the elastic-EP profile. Another replica of it would add no
# capacity, because the width of one replica is what sizes the collective.
ELASTIC_REASON=""
if [ -n "$IT" ] && [ -z "$TARGET_REASON" ]; then
  out="$(kubectl apply -f - 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${ELASTIC_TARGET}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: /models
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      instanceType: ${IT}
      elasticEp:
        width: 2
      resources:
        accelerator: "1"
EOF
)"
  case "$out" in
    *created* | *configured*) ;;
    *) ELASTIC_REASON="the deployment ${ELASTIC_TARGET} was refused on this cluster: $(echo "$out" | tr '\n' ' ' | cut -c1-200)" ;;
  esac
fi

# ---------------------------------------------------------------- the refusals that read a target

if [ -n "$TARGET_REASON" ]; then
  record SKIP "a scaler naming a role the target does not have is refused" "$TARGET_REASON"
  record SKIP "a scaler naming a target role that runs a command of its own is refused" "$TARGET_REASON"
  record SKIP "a scaler naming a target role on the elastic-EP profile is refused" "$TARGET_REASON"
  record SKIP "a shadow scaler binds the target and changes nothing" "$TARGET_REASON"
  record SKIP "a scaler may not be pointed at another target" "$TARGET_REASON"
  record SKIP "an active scaler takes the binding on the target" "$TARGET_REASON"
  record SKIP "a second scaler on the same target is refused the binding" "$TARGET_REASON"
  record SKIP "a user may not write the replica count of a bound deployment" "$TARGET_REASON"
  record SKIP "a user may not write the annotations of a bound deployment" "$TARGET_REASON"
  record SKIP "a user may write everything else of a bound deployment" "$TARGET_REASON"
  record SKIP "a suspended scaler gives the binding up and the count becomes writable" "$TARGET_REASON"
  record SKIP "a scaler that comes back writes a higher term" "$TARGET_REASON"
  record SKIP "deleting a scaler releases the binding and the finalizer" "$TARGET_REASON"
else
  refuses "a scaler naming a role the target does not have is refused" \
    "the target does not have a role with this name, so there is nothing to scale" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-unknownrole
  namespace: ${NS}
spec:
  targetRef:
    name: ${TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
    - name: encoder
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
EOF

  if [ -n "$CMD_REASON" ]; then
    record SKIP "a scaler naming a target role that runs a command of its own is refused" "$CMD_REASON"
  else
    refuses "a scaler naming a target role that runs a command of its own is refused" \
      "this role runs a command of its own, and a scaler cannot tell what that command serves" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-command
  namespace: ${NS}
spec:
  targetRef:
    name: ${CMD_TARGET}
  roles:
    - name: server
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
EOF
  fi

  if [ -n "$ELASTIC_REASON" ]; then
    record SKIP "a scaler naming a target role on the elastic-EP profile is refused" "$ELASTIC_REASON"
  else
    refuses "a scaler naming a target role on the elastic-EP profile is refused" \
      "this role runs the elastic-EP profile" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-elastic
  namespace: ${NS}
spec:
  targetRef:
    name: ${ELASTIC_TARGET}
  roles:
    - name: server
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
EOF
  fi

  # ---------------------------------------------------------------- the lifecycle

  # A SHADOW SCALER OBSERVES WITHOUT TOUCHING THE TARGET. It binds the deployment it names and
  # publishes what it would have done, and the count on the deployment is the count its owner
  # wrote.
  kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${S1}
  namespace: ${NS}
spec:
  targetRef:
    name: ${TARGET}
  mode: Shadow
  roles:
    - name: prefill
      minReplicas: 2
      maxReplicas: 4
    - name: decode
      minReplicas: 2
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: prefill
          target: "4"
        - name: decode
          target: "4"
EOF

  if wait_condition "$S1" Bound True Bound "${E2E_C128_BOUND:-120}"; then
    record PASS "a shadow scaler binds the target and changes nothing" \
      "the scaler reports Bound=True and the count of the target is still $(role_replicas "$TARGET" prefill)"
  else
    record FAIL "a shadow scaler binds the target and changes nothing" \
      "the scaler never reported Bound=True (it reports $(scaler_condition "$S1" Bound)), so the shadow path was not reached"
  fi

  expect "a shadow scaler leaves the count of the target alone" \
    "$(role_replicas "$TARGET" prefill)" "2"
  expect "a shadow scaler does not take the binding" "$(target_binding "$TARGET")" ""

  # THE TARGET OF A SCALER IS FIXED. The binding follows the target, so a scaler pointed at
  # another one would leave the old target holding a binding nobody may write.
  out="$(kubectl -n "$NS" patch "$SCALER_RESOURCE" "$S1" --type=merge \
    -p '{"spec":{"targetRef":{"name":"case128-somewhere-else"}}}' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"the target of a scaler is fixed once it is set"*)
      record PASS "a scaler may not be pointed at another target" "refused with: ${out:0:200}" ;;
    *)
      record FAIL "a scaler may not be pointed at another target" "the patch was not refused as it should be: ${out:0:200}" ;;
  esac

  # THE ACTIVE SCALER TAKES THE BINDING. The annotation on the deployment names the scaler and
  # the term it wrote under.
  kubectl -n "$NS" patch "$SCALER_RESOURCE" "$S1" --type=merge -p '{"spec":{"mode":"Active"}}' >/dev/null 2>&1
  term_one=""
  deadline=$((SECONDS + ${E2E_C128_BOUND:-120}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    term_one="$(target_binding_term "$TARGET")"
    [ -n "$term_one" ] && break
    sleep 3
  done
  if [ -n "$term_one" ]; then
    record PASS "an active scaler takes the binding on the target" \
      "the target carries the binding under term ${term_one}"
  else
    record FAIL "an active scaler takes the binding on the target" \
      "the target carries no binding ${term_one} after the scaler was made active"
  fi
  expect "the active scaler reports Bound" "$(scaler_condition "$S1" Bound)" "True|Bound"
  expect "the active scaler reports Active" "$(scaler_condition "$S1" Active)" "True|Active"

  # THE SINGLE-WRITER FENCE. A user may change anything about a bound deployment except the count
  # and the two annotations that record who owns it.
  out="$(kubectl -n "$NS" patch "$MD_RESOURCE" "$TARGET" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"the replica count of a bound deployment belongs to the scaler that owns it"*)
      record PASS "a user may not write the replica count of a bound deployment" "refused with: ${out:0:200}" ;;
    *)
      record FAIL "a user may not write the replica count of a bound deployment" \
        "the patch was not refused as it should be: ${out:0:200}" ;;
  esac

  out="$(kubectl -n "$NS" patch "$MD_RESOURCE" "$TARGET" --type=merge \
    -p '{"metadata":{"annotations":{"modeldeployment.gpustack.ai/scaler-binding":"{\"scalerName\":\"case128-impostor\",\"scalerUID\":\"00000000-0000-0000-0000-000000000000\",\"term\":99}"}}}' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"the binding and decision annotations of a bound deployment belong to the scaler that owns it"*)
      record PASS "a user may not write the annotations of a bound deployment" "refused with: ${out:0:200}" ;;
    *)
      record FAIL "a user may not write the annotations of a bound deployment" \
        "the patch was not refused as it should be: ${out:0:200}" ;;
  esac

  out="$(kubectl -n "$NS" patch "$MD_RESOURCE" "$TARGET" --type=merge \
    -p '{"metadata":{"labels":{"case128.gpustack.ai/user-edit":"yes"}}}' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *patched* | *configured*)
      record PASS "a user may write everything else of a bound deployment" "the label was written: ${out:0:160}" ;;
    *)
      record FAIL "a user may write everything else of a bound deployment" \
        "the label was not written: ${out:0:200}" ;;
  esac

  # A SECOND SCALER ON THE SAME TARGET IS REFUSED THE BINDING. The binding names one scaler, so
  # a target that carries one belongs to that scaler alone.
  kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${S2}
  namespace: ${NS}
spec:
  targetRef:
    name: ${TARGET}
  mode: Active
  roles:
    - name: prefill
      minReplicas: 2
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: prefill
          target: "4"
EOF

  if wait_condition "$S2" Bound False AlreadyBound "${E2E_C128_BOUND:-120}"; then
    record PASS "a second scaler on the same target is refused the binding" \
      "the second scaler reports Bound=False with the reason AlreadyBound"
  else
    record FAIL "a second scaler on the same target is refused the binding" \
      "the second scaler reports Bound=$(scaler_condition "$S2" Bound), so the binding was not refused to it"
  fi

  # THE SECOND SCALER GOES AWAY BEFORE THE FIRST ONE IS SUSPENDED. A scaler that is refused the
  # binding is refused only for as long as the other one holds it, so the moment the first one
  # gives the binding up, the second one takes it, and the rows below are about a target that
  # nothing owns. Leaving it there would make the binding a race between the two of them.
  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$S2" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1

  # THE SUSPENDED SCALER GIVES THE BINDING UP, and the count becomes writable again.
  kubectl -n "$NS" patch "$SCALER_RESOURCE" "$S1" --type=merge -p '{"spec":{"suspend":true}}' >/dev/null 2>&1
  deadline=$((SECONDS + ${E2E_C128_BOUND:-120}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -z "$(target_binding "$TARGET")" ] && break
    sleep 3
  done
  expect "a suspended scaler gives the binding up" "$(target_binding "$TARGET")" ""
  expect "a suspended scaler reports Suspended" "$(scaler_condition "$S1" Active)" "False|Suspended"

  out="$(kubectl -n "$NS" patch "$MD_RESOURCE" "$TARGET" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *patched* | *configured*)
      record PASS "a suspended scaler gives the binding up and the count becomes writable" "the count was written: ${out:0:160}" ;;
    *)
      record FAIL "a suspended scaler gives the binding up and the count becomes writable" \
        "the count was not written: ${out:0:200}" ;;
  esac

  # THE TERM NEVER GOES BACKWARDS. The scaler that comes back writes a higher term, so a reader
  # can tell which binding is newer.
  kubectl -n "$NS" patch "$SCALER_RESOURCE" "$S1" --type=merge -p '{"spec":{"suspend":false}}' >/dev/null 2>&1
  term_two=""
  deadline=$((SECONDS + ${E2E_C128_BOUND:-120}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    term_two="$(target_binding_term "$TARGET")"
    if [ -n "$term_two" ] && [ -n "$term_one" ] && [ "$term_two" -gt "$term_one" ]; then
      break
    fi
    sleep 3
  done
  if [ -n "$term_two" ] && [ -n "$term_one" ] && [ "$term_two" -gt "$term_one" ]; then
    record PASS "a scaler that comes back writes a higher term" \
      "the binding was written under term ${term_one} and again under term ${term_two}"
  else
    record FAIL "a scaler that comes back writes a higher term" \
      "the first term was ${term_one:-<none>} and the second was ${term_two:-<none>}, so the term did not go forward"
  fi

  # DELETING A SCALER RELEASES THE BINDING AND THE FINALIZER. The finalizer is what lets a
  # deleted scaler remove its binding before it disappears, so the key is read off the object
  # rather than assumed. The second scaler is already gone, so the first one is the only scaler
  # left that could hold the binding.
  finalizers="$(kubectl -n "$NS" get "$SCALER_RESOURCE" "$S1" -o json 2>/dev/null \
    | jq -r '(.metadata.finalizers // []) | join(",")')"
  case "$finalizers" in
    *"$FINALIZER_KEY"*)
      record PASS "a scaler holds the finalizer that lets it release its binding" \
        "the finalizers of the scaler are ${finalizers}" ;;
    *)
      record FAIL "a scaler holds the finalizer that lets it release its binding" \
        "the finalizers of the scaler are ${finalizers:-<none>}, so the binding could not be released on deletion" ;;
  esac

  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$S1" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
  if kubectl -n "$NS" get "$SCALER_RESOURCE" "$S1" >/dev/null 2>&1; then
    record FAIL "deleting a scaler releases the binding and the finalizer" \
      "the scaler ${S1} is still there, so its finalizer was not removed"
  else
    record PASS "deleting a scaler releases the binding and the finalizer" "the scaler ${S1} is gone"
  fi
  expect "deleting a scaler leaves the target unbound" "$(target_binding "$TARGET")" ""
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-128] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-128] all checks passed"
