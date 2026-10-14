#!/usr/bin/env bash
#
# CASE 116 — Every encoder-role admission refusal fires, each naming what its rule refuses
#   (MUTATING for the fixture only: one settings key and one injected node label, both restored by
#    the trap; every verdict below is a server-side dry run that persists nothing)
#   AUTO-SKIPS its matrix rows without an InstanceType that reports an admitted accelerator
#   platform, which this case builds by injecting a CPU vendor label.
#
#   case-116.sh <NS>
#
# Goal:        A deployment that declares an Encode role is refused by a set of rules that a
#              deployment without one never meets, and each refusal names its own rule. This case
#              pins every rule on the encoder path and, like case 51 before it, pins WHICH rule
#              answered: a row that only checked "it was rejected" passes identically whether the
#              rule exists or not, so every row asserts a fragment of the operator's own wording
#              read from the code, and row 0 is the same shape ACCEPTED.
#
#              THE SHAPE RULES RUN BEFORE ANYTHING READS THE CLUSTER, so a role set that misses a
#              sibling kind, names a router-less shape, carries a deployment-wide cache, or
#              replaces a role's command is refused from the submitted object alone. THE MATRIX
#              RULES RUN ON THE PLATFORM THE ROLES' INSTANCE TYPES REPORT, which no CPU-only
#              cluster hands over by itself: the platform is read from each role's instance type's
#              observed manufacturer, and a CPU pool reports one only while CPU-manufacturer
#              awareness is on. So the case turns that setting on and injects a fake CPU vendor
#              label ("Ascend", lowercased to the platform the shared-directory row admits) on one
#              node, and the pool that forms behind it makes the whole matrix reachable without a
#              single accelerator: the wrong router, the wrong engine, the engine version below
#              the floor, the shared-claim rules and the mixed-platform rule all become assertable.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator and
#              no engine image: nothing here schedules or runs a replica. The gpustack-settings
#              Secret must exist in the system namespace (E2E_SYSTEM_NS, default gpustack-system).
#
#              AUTO-SKIP: the matrix rows need a derived InstanceType reporting the injected
#              platform. The label rides a NodeFeature object, and NFD owns the vendor label this
#              overrides — if this cluster's NFD declines to let an injected object override a
#              label its cpuid source reports, no such type appears, and the matrix rows SKIP
#              naming that rather than failing. The shape rows still run: they are answered from
#              the submitted object and fire before the platform is ever read.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and
#              one settings key (CPU-manufacturer awareness) flipped on — both restored by the
#              trap. REAL: every manifest goes through `kubectl apply --dry-run=server`, so the
#              schema and the webhook answer exactly as they would for a real create.
#
# Expected:    - the full encoder/prefill/decode shape on the injected platform, with the two
#                shared-claim mounts, is ACCEPTED;
#              - each rule below refuses with its own wording: a missing sibling kind (each of
#                prefill and decode), a Server role beside the encoder, no router, a deployment-
#                wide kvCache, a role command, an encoder of more than one instance, an empty
#                claim name, a second volume source on one entry, a read-only encoder mount,
#                disagreeing mount paths, an encoder or a prefiller without its claim, a router
#                that does not take an encoder role, an engine that router does not pair with, an
#                engine version below the floor, and a platform the matrix does not admit;
#              - two roles on two different platforms are refused as a mixed deployment.
#
# Cleanup:     The trap deletes the NodeFeature object, restores the settings key to its exact
#              original value (removing it if it was absent) and waits for the chain to settle back:
#              the collapsed CPU pool returns only once the setting cache flips and the pool
#              re-derives. Idempotent, runs on pass AND fail, safe to re-run. No ModelDeployment is
#              ever persisted: every apply is a server-side dry run.
#
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-116.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
# The vendor string becomes the platform after lowercasing; "Ascend" selects the shared-directory
# row, which is the one whose claim rules this case refuses against.
VENDOR="${E2E_C116_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF="case116-cpu-vendor"
MD=case116-probe
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
# 0.30.0 clears both admitted floors, so nothing below is about the version unless the row says so.
VERSION="${E2E_C116_VERSION:-0.30.0}"
SETTLE_ROUNDS="${E2E_C116_SETTLE:-60}"   # 60 x 5s = 5 min for the settings cache and re-derivation

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

# ---------------------------------------------------------------- the fixture, and its restore

# One Ready, schedulable worker node (not a control-plane one): the vendor label lands there and
# that node's CPU becomes the pool whose manufacturer the case controls.
pick_worker_node() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("items",[]):
    cs=n.get("status",{}).get("conditions",[])
    if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in cs):
        continue
    if n.get("spec",{}).get("unschedulable"):
        continue
    ts=n.get("spec",{}).get("taints",[])
    if any(str(t.get("key","")).startswith("node-role.kubernetes.io/") for t in ts):
        continue
    print(n["metadata"]["name"])
    break
'
}

# The InstanceType that reports the injected platform: derived from the pool the labeled node
# feeds, which is the whole fixture. Not terminating and not Inactive, for the reasons case-51
# gives: a departing type is refused at admission for a reason no row here is about.
epd_platform_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ "$manufacturer" = "$PLATFORM" ]; then
          echo "$name"
          break
        fi
      done
}

# Any InstanceType a deployment can name, for the shape rows: they fire before the platform is
# read, so the type's own platform does not matter to them. Same exclusions as case-51.
usable_instance_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.spec.acceleratable}|{.status.accelerator.capacity}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive acceleratable capacity; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ "$acceleratable" = true ]; then
          case "$capacity" in "" | 0) continue ;; esac
        fi
        echo "$name"
        break
      done
}

# The Setting is read through the worker's thirty-second cache, so the flip and the restore are
# both absorbed by the type poll below rather than by a blind sleep. The value rides the Secret
# base64-encoded; the original is kept encoded and patched back verbatim (case-106's mechanic).
ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
set_awareness() { # true|false
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":\"$(printf '%s' "$1" | base64)\"}}" >/dev/null 2>&1
}
restore_setting() {
  local v=null
  [ -n "$ORIG_SETTING" ] && v="\"${ORIG_SETTING}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":${v}}}" >/dev/null 2>&1
}

# Restoring the value is not the end of the restore: the worker reads it through a thirty-second
# cache, and the type derived from it outlives the patch by that cache plus one reconcile. Wait for
# the derived type to go, so the next case in a serial run does not list an aware type this run left
# behind (case-18's wait_generic, on this same fixture). A cluster that started with the setting
# already on keeps its derived type, so there is nothing to wait for and the wait is skipped.
settle_back() {
  [ "$(printf '%s' "$ORIG_SETTING" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    [ -z "$(epd_platform_type)" ] && return 0
    sleep 5
  done
  return 0
}

NODE="$(pick_worker_node)"
if [ -z "$NODE" ]; then
  echo "[case-116] no Ready schedulable worker node; this case cannot place its vendor label" >&2
  exit 2
fi

cleanup() {
  kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
  restore_setting
  settle_back
}
trap cleanup EXIT

echo "[case-116] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"

# The label rides a NodeFeature object (case-4's mechanism): the object stays and keeps re-asserting
# the label, where a bare `kubectl label` is a cpuid-owned value NFD reverts on its next pass.
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
spec:
  labels:
    feature.node.kubernetes.io/cpu-model.vendor_id: "${VENDOR}"
  features: {}
YAML
set_awareness true

# The pool needs the settings cache to expire, the node flavor to re-derive and the type to be
# authored, so this poll IS the wait. A timeout is a skip, not a failure: NFD may decline to let an
# injected object override a label its cpuid source reports, and that is an environment answer.
PLATFORM_TYPE=""
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  PLATFORM_TYPE="$(epd_platform_type)"
  [ -n "$PLATFORM_TYPE" ] && break
  sleep 5
done

FALLBACK_TYPE="$(usable_instance_type)"
if [ -z "$PLATFORM_TYPE" ] && [ -z "$FALLBACK_TYPE" ]; then
  echo "[case-116] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# The shape rows embed whichever type exists: their rules fire before the platform is read. The
# matrix rows below name the platform type directly and are skipped without it.
IT="${PLATFORM_TYPE:-$FALLBACK_TYPE}"

if [ -n "$PLATFORM_TYPE" ]; then
  record PASS "a CPU-only pool reports an admitted platform" \
    "instancetype ${PLATFORM_TYPE} reports manufacturer '${PLATFORM}' from the injected vendor label"
else
  record SKIP "a CPU-only pool reports an admitted platform" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so every matrix row below skips: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived. The shape rows still run: they are refused before the platform is read"
fi

# ---------------------------------------------------------------- the manifests and the rows

# ENGINE, VERSION and ROUTER_NAME are the one-field dials the matrix rows turn; each is reset
# right after the row that turns it, because an assignment prefixed to a function call persists in
# bash after the call returns and would silently move every later row off the baseline.
manifest() { # roles-block
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: ${ENGINE:-vLLM}
    version: "${VERSION}"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${ROUTER_NAME:-llm-d-router}
${KVCACHE_BLOCK:-}
  roles:
${1}
YAML
}

encode_role() { # [claimName mountPath readOnly]
  printf '  - name: encode\n    kind: Encode\n    instanceType: %s\n    image: %s\n' "$IT" "$IMAGE"
  [ -n "${1:-}" ] || return 0
  printf '    additionalVolumes:\n      - mountPath: %s\n' "${2:-/ec-cache}"
  [ -n "${3:-}" ] && printf '        readOnly: %s\n' "$3"
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$1"
  return 0
}

encode_role_sized() { # size claimName
  printf '  - name: encode\n    kind: Encode\n    instanceType: %s\n    image: %s\n    size: %s\n' \
    "$IT" "$IMAGE" "$1"
  printf '    additionalVolumes:\n      - mountPath: /ec-cache\n'
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$2"
}

prefill_role() { # [claimName mountPath]
  printf '  - name: prefill\n    kind: Prefill\n    instanceType: %s\n    image: %s\n' "$IT" "$IMAGE"
  [ -n "${1:-}" ] || return 0
  printf '    additionalVolumes:\n      - mountPath: %s\n' "${2:-/ec-cache}"
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$1"
  return 0
}

decode_role() { # [command]
  printf '  - name: decode\n    kind: Decode\n    instanceType: %s\n    image: %s\n' "$IT" "$IMAGE"
  [ -n "${1:-}" ] && printf '    command: ["/pause"]\n'
  return 0
}

# The accepted baseline: encode and prefill mount one claim at one path, the platform is the
# injected one, the version clears the floor. Every refusal below varies exactly one thing from it.
baseline_roles() {
  encode_role case116-shared /ec-cache
  prefill_role case116-shared /ec-cache
  decode_role
}

# Assert that a roles block is refused AND that the refusal quotes every fragment after the roles
# block. An empty apply output is a FAIL on its own: deleting anything from "" leaves "". A
# refusal carrying other messages too passes — several rules aggregate into one response — but a
# MISSING fragment means the manifest tripped a rule this row is not about.
refuses() {
  local check="$1" want="$2" roles="$3" out w
  shift 3
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"
    return 0
  fi
  for w in "$want" "$@"; do
    case "$out" in
      *"$w"*) ;;
      *) record FAIL "$check" "refused with the wrong message, or accepted (missing '${w}'): ${out:0:220}"; return 0 ;;
    esac
  done
  record PASS "$check" "refused naming: ${want}${*:+ + $*}"
  return 0
}

# The positive side of the same instrument, and it carries the headline: three roles being accepted
# at all is the shape this whole rule set exists to guard, so the baseline row proves the refusals
# below are each their own rule rather than one blanket rejection.
accepts() {
  local check="$1" roles="$2" out
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "$check" "accepted by both the schema and the webhook" ;;
    *) record FAIL "$check" "refused: ${out:0:220}" ;;
  esac
  return 0
}

# ---------------------------------------------------------------- row 0: the accepted baseline

if [ -n "$PLATFORM_TYPE" ]; then
  accepts "the full encoder shape on the admitted platform is ACCEPTED" "$(baseline_roles)"
else
  record SKIP "the full encoder shape on the admitted platform is ACCEPTED" \
    "no platform InstanceType exists, so there is no shape to accept and every refusal below would pass for the wrong reason"
fi

# ---------------------------------------------------------------- the shape rules: object-only

refuses "an encoder with no decoder is refused naming the missing role" \
  'needs a "Decode" role beside it' \
  "$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /ec-cache)"

refuses "an encoder with no prefill is refused naming the missing role" \
  'needs a "Prefill" role beside it' \
  "$(encode_role case116-shared /ec-cache)
$(decode_role)"

refuses "a Server role beside the encoder is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: Server
    instanceType: ${IT}
    image: ${IMAGE}
$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

# The no-router row needs a manifest with the router block absent, which the manifest builder
# cannot spell through ROUTER_NAME: an empty name is a different refusal. Build it inline.
out="$(cat <<YAML | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' '
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    version: "${VERSION}"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
$(baseline_roles)
YAML
)"
case "$out" in
  *"an Encode role requires a router"*)
    record PASS "an encoder with no router is refused" "refused naming: an Encode role requires a router" ;;
  "")
    record FAIL "an encoder with no router is refused" "the apply produced no output at all, so nothing was refused" ;;
  *)
    record FAIL "an encoder with no router is refused" "refused with the wrong message, or accepted: ${out:0:220}" ;;
esac

KVCACHE_BLOCK="$(printf '  kvCache:\n    poolRef:\n      name: case116-no-such-binding\n')" \
  refuses "a deployment-wide kvCache is refused on the encoder shape" \
  "spec.kvCache is refused" \
  "$(baseline_roles)"
KVCACHE_BLOCK=""

refuses "a role command is refused on the encoder shape" \
  "replaces the managed arguments and the routing sidecar" \
  "$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /ec-cache)
$(decode_role command)"

# An encoder runs one instance per Pod, so its replica size is fixed at 1. This is a shape rule like
# the rows above it: it is answered from the submitted object, before the platform is read, so it
# needs no matrix row and does not skip when the injected platform never derived.
refuses "an encoder of more than one instance is refused" \
  "replica size is 1" \
  "$(encode_role_sized 2 case116-shared)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

# ---------------------------------------------------------------- the claim-source rules: object-only

# An empty claim name is refused wherever a claim entry appears, encoder role or not, and before
# anything reads the cluster. The entry itself must exist: a role with NO claim entry is a
# different row's refusal, so the claimName is written empty rather than the block dropped.
refuses "an empty claim name is refused" \
  "claim name must not be empty" \
  "  - name: encode
    kind: Encode
    instanceType: ${IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: \"\"
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

# One entry mounts one volume: a configMap beside the claim leaves the renderer a choice the API
# never makes.
out="$(manifest "  - name: encode
    kind: Encode
    instanceType: ${IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
        configMap:
          name: case116-anything
$(prefill_role case116-shared /ec-cache)
$(decode_role)" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
case "$out" in
  *"set beside persistentVolumeClaim on the same entry"*)
    record PASS "a second source on one claim entry is refused" "refused naming: set beside persistentVolumeClaim on the same entry" ;;
  "")
    record FAIL "a second source on one claim entry is refused" "the apply produced no output at all, so nothing was refused" ;;
  *)
    record FAIL "a second source on one claim entry is refused" "refused with the wrong message, or accepted: ${out:0:220}" ;;
esac

# ---------------------------------------------------------------- the matrix rules: platform-bound

if [ -n "$PLATFORM_TYPE" ]; then
  # The wrong router is refused before its engine is even consulted, and the message names both.
  ROUTER_NAME=vllm-router refuses "a router that takes no encoder role is refused" \
    'router "vllm-router" is not admitted for an encoder role' \
    "$(baseline_roles)"
  ROUTER_NAME=""

  ENGINE=SGLang refuses "an engine this router does not pair with the encoder is refused" \
    'is not admitted with engine "SGLang"' \
    "$(baseline_roles)"
  ENGINE=""

  # 0.10.0 is below both floors, so exactly the floor rule answers.
  VERSION=0.10.0 refuses "an engine version below the floor is refused" \
    "needs that version or later" \
    "$(baseline_roles)"
  VERSION="${E2E_C116_VERSION:-0.30.0}"

  # The shared-claim rules run only on the row whose connector is a mounted directory, which is the
  # platform this case injects. Each row varies exactly one mount from the baseline.
  refuses "a read-only encoder mount is refused" \
    "must not be read-only" \
    "$(encode_role case116-shared /ec-cache true)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

  refuses "disagreeing mount paths are refused" \
    "must name the same mountPath" \
    "$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /other-cache)
$(decode_role)"

  refuses "an encoder that mounts no claim is refused" \
    "attach the shared claim the prefiller mounts" \
    "$(encode_role)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

  refuses "a prefiller that mounts no claim is refused" \
    "mounts nothing" \
    "$(encode_role case116-shared /ec-cache)
$(prefill_role)
$(decode_role)"
else
  for row in \
    "a router that takes no encoder role is refused" \
    "an engine this router does not pair with the encoder is refused" \
    "an engine version below the floor is refused" \
    "a read-only encoder mount is refused" \
    "disagreeing mount paths are refused" \
    "an encoder that mounts no claim is refused" \
    "a prefiller that mounts no claim is refused"; do
    record SKIP "$row" "needs the platform InstanceType this run did not build; see the first row"
  done
fi

# ---------------------------------------------------------------- the platform rules

# A platform no admitted row covers. With awareness on, the cluster's own CPU types report their
# real vendor, so any type whose manufacturer is neither of the two admitted platforms serves;
# an all-AMD or all-unknown cluster may have none, and the row says so rather than inventing one.
if [ -n "$PLATFORM_TYPE" ]; then
  OTHER_IT="$(kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ -n "$manufacturer" ] && [ "$manufacturer" != "$PLATFORM" ] \
          && [ "$manufacturer" != amd ]; then
          echo "$name"
          break
        fi
      done)"

  if [ -n "$OTHER_IT" ]; then
    # The whole deployment on the unadmitted type: the matrix names the platform, the router and
    # the engine together.
    refuses "a platform the matrix does not admit is refused, all three named" \
      "is not admitted for encoder roles under router" \
      "  - name: encode
    kind: Encode
    instanceType: ${OTHER_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
  - name: prefill
    kind: Prefill
    instanceType: ${OTHER_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
  - name: decode
    kind: Decode
    instanceType: ${OTHER_IT}
    image: ${IMAGE}"

    # Two platforms in one deployment, both claim mounts well formed so only the mixing answers.
    refuses "two platforms in one encoder deployment are refused" \
      "more than one accelerator platform" \
      "$(encode_role case116-shared /ec-cache)
  - name: prefill
    kind: Prefill
    instanceType: ${OTHER_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
$(decode_role)"
  else
    record SKIP "a platform the matrix does not admit is refused, all three named" \
      "every usable InstanceType reports an admitted platform or none, so there is no unadmitted platform to name"
    record SKIP "two platforms in one encoder deployment are refused" \
      "no second manufacturer exists to mix with the injected one"
  fi
else
  record SKIP "a platform the matrix does not admit is refused, all three named" \
    "needs the platform InstanceType this run did not build; see the first row"
  record SKIP "two platforms in one encoder deployment are refused" \
    "needs the platform InstanceType this run did not build; see the first row"
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-116] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-116] all checks passed"
